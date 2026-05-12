package game

import (
	"encoding/base64"
	"fmt"
	"math"
	"time"

	"hk4e/common/constant"
	"hk4e/gdconf"
	"hk4e/gs/model"
	"hk4e/pkg/endec"
	"hk4e/pkg/random"
	"hk4e/protocol/cmd"
	"hk4e/protocol/proto"

	"github.com/flswld/halo/logger"
	pb "google.golang.org/protobuf/proto"
)

// PUBG 玩法插件 - 项目自创玩法的典型实现
//
// 核心机制（详见 CLAUDE.md "AI 世界" 章节）：
//   - 跑在 AI 世界容器里（uid<PlayerBaseUid 不受 4 人上限约束 可同时容纳几十/上百玩家）
//   - 类似经典吃鸡：缩圈毒区 + 全图武器拾取 + 弓箭对射
//   - 多种弓箭子弹通过物理引擎模拟轨迹+命中判定（gs/game/game_physics_engine.go）
//   - 每次玩家进入 AI 世界 PluginPubg 给玩家注入隐藏小图标 + 开 GM 按钮的 Lua bytecode
//
// 4 阶段（PUBG_PHASE_*）：
//   - WAIT (-1): 等待玩家进入 显示倒计时
//   - START (0): 游戏开始 第 1 圈缩圈倒计时启动
//   - II (2): 第 2 圈起 进入持续缩圈阶段（每分钟更新一次）
//   - END (16): 全图缩到中心点 决出胜者
//
// 关键参数：
//   - 第 1 圈倒计时 PUBG_FIRST_AREA_REDUCE_TIME = 5 分钟
//   - 单局无敌时间（防伞兵）PUBG_PHASE_INV_TIME = 3 分钟
//   - 玩家攻击 PUBG_ATK + 血量 PUBG_HP（独立于原版面板）
//   - 弓箭伤害倍率 PUBG_BOW_ATTACK_ATK_RATIO = 2.0
//   - 普通攻击距离 PUBG_NORMAL_ATTACK_DISTANCE = 3.0 米

const (
	PUBG_PHASE_WAIT  = -1 // 等待开始
	PUBG_PHASE_START = 0  // 游戏开始（第1圈缩圈中）
	PUBG_PHASE_II    = 2  // 第2圈以上缩圈
	PUBG_PHASE_END   = 16 // 终局（中心点死斗）
)

const (
	PUBG_PHASE_INV_TIME         = 180.0
	PUBG_FIRST_AREA_REDUCE_TIME = 300.0
)

const (
	PUBG_ATK                         = 100.0
	PUBG_HP                          = 1000.0
	PUBG_HP_LOST                     = 10.0
	PUBG_BOW_ATTACK_ATK_RATIO        = 2.0
	PUBG_NORMAL_ATTACK_DISTANCE      = 3.0
	PUBG_NORMAL_ATTACK_INTERVAL_TIME = 500
	PUBG_NORMAL_ATTACK_ATK_RATIO     = 5.0
)

// PluginPubg pubg游戏插件
type PluginPubg struct {
	*Plugin
	seq                      uint32
	world                    *World                // 世界对象
	blueAreaCenterPos        *model.Vector         // 蓝区中心点
	blueAreaRadius           float64               // 蓝区半径
	safeAreaCenterPos        *model.Vector         // 安全区中心点
	safeAreaRadius           float64               // 安全区半径
	phase                    int                   // 阶段
	areaReduceRadiusSpeed    float64               // 缩圈半径速度
	areaReduceXSpeed         float64               // 缩圈X速度
	areaReduceZSpeed         float64               // 缩圈Z速度
	areaPointList            []*proto.MapMarkPoint // 客户端区域地图坐标列表
	entityIdWorldGadgetIdMap map[uint32]int32      // 实体id世界物件id映射集合
	playerHitTimeMap         map[uint32]int64      // 玩家攻击命中时间集合
}

func NewPluginPubg() *PluginPubg {
	p := &PluginPubg{
		Plugin:                   NewPlugin(),
		seq:                      0,
		world:                    nil,
		blueAreaCenterPos:        &model.Vector{X: 0.0, Y: 0.0, Z: 0.0},
		blueAreaRadius:           0.0,
		safeAreaCenterPos:        &model.Vector{X: 0.0, Y: 0.0, Z: 0.0},
		safeAreaRadius:           0.0,
		phase:                    PUBG_PHASE_WAIT,
		areaReduceRadiusSpeed:    0.0,
		areaReduceXSpeed:         0.0,
		areaReduceZSpeed:         0.0,
		areaPointList:            make([]*proto.MapMarkPoint, 0),
		entityIdWorldGadgetIdMap: make(map[uint32]int32),
		playerHitTimeMap:         make(map[uint32]int64),
	}
	return p
}

// OnEnable 插件启用生命周期（注册 8 个事件监听 + 2 个全局 tick + 1 个 GM 命令控制器）
//
// 8 个事件覆盖玩法核心流程：
//   - MarkMap: 拦截标点请求 改用 PUBG 区域标点
//   - AvatarDieAnimationEnd: 死亡时广播淘汰消息（仅 PUBG 内）
//   - GadgetInteract: 拾取物件 → 加攻击/恢复HP（PUBG 物件配置）
//   - PostEnterScene: 玩家进 AI 世界 → 注入隐藏小图标的 Lua
//   - EvtDoSkillSucc: 角色技能命中 → 转换为 PUBG 伤害判定
//   - EvtBeingHit: 实体受击 → PUBG 内自定义伤害结算
//   - EvtCreateGadget: 创建物件（弓箭子弹）→ 送物理引擎模拟
//   - EvtBulletHit: 子弹命中（弓箭专用 物理引擎判定结果）
func (p *PluginPubg) OnEnable() {
	// 监听事件
	p.ListenEvent(PluginEventIdMarkMap, PluginEventPriorityNormal, p.EventMarkMap)
	p.ListenEvent(PluginEventIdAvatarDieAnimationEnd, PluginEventPriorityNormal, p.EventAvatarDieAnimationEnd)
	p.ListenEvent(PluginEventIdGadgetInteract, PluginEventPriorityNormal, p.EventGadgetInteract)
	p.ListenEvent(PluginEventIdPostEnterScene, PluginEventPriorityNormal, p.EventPostEnterScene)
	p.ListenEvent(PluginEventIdEvtDoSkillSucc, PluginEventPriorityNormal, p.EventEvtDoSkillSucc)
	p.ListenEvent(PluginEventIdEvtBeingHit, PluginEventPriorityNormal, p.EventEvtBeingHit)
	p.ListenEvent(PluginEventIdEvtCreateGadget, PluginEventPriorityNormal, p.EvtCreateGadget)
	p.ListenEvent(PluginEventIdEvtBulletHit, PluginEventPriorityNormal, p.EvtBulletHit)
	// 添加全局定时器
	p.AddGlobalTick(PluginGlobalTickSecond, p.GlobalTickPubg)
	p.AddGlobalTick(PluginGlobalTickMinuteChange, p.GlobalTickMinuteChange)
	// 注册命令
	p.RegCommandController(p.NewPubgCommandController())
}

/************************************************** 事件监听 **************************************************/

// EventMarkMap 地图标点事件
func (p *PluginPubg) EventMarkMap(iEvent IPluginEvent) {
	event := iEvent.(*PluginEventMarkMap)
	player := event.Player
	// 确保游戏开启
	if !p.IsStartPubg() || p.world.GetId() != player.WorldId {
		return
	}
	GAME.SendMsg(cmd.MarkMapRsp, player.PlayerId, player.ClientSeq, &proto.MarkMapRsp{MarkList: p.GetAreaPointList()})
	event.Cancel()
}

// EventAvatarDieAnimationEnd 角色死亡动画结束事件
func (p *PluginPubg) EventAvatarDieAnimationEnd(iEvent IPluginEvent) {
	event := iEvent.(*PluginEventAvatarDieAnimationEnd)
	player := event.Player
	// 确保游戏开启
	if !p.IsStartPubg() || p.world.GetId() != player.WorldId {
		return
	}
	alivePlayerNum := len(p.GetAlivePlayerList())
	info := fmt.Sprintf("『%v』死亡了，剩余%v位存活玩家。", player.NickName, alivePlayerNum)
	GAME.PlayerChatReq(p.world.GetOwner(), &proto.PlayerChatReq{ChatInfo: &proto.ChatInfo{Content: &proto.ChatInfo_Text{Text: info}}})
	GAME.SendMsg(cmd.AvatarDieAnimationEndRsp, player.PlayerId, player.ClientSeq, &proto.AvatarDieAnimationEndRsp{SkillId: event.Req.SkillId, DieGuid: event.Req.DieGuid})
	event.Cancel()
}

// EventGadgetInteract gadget交互事件
func (p *PluginPubg) EventGadgetInteract(iEvent IPluginEvent) {
	event := iEvent.(*PluginEventGadgetInteract)
	player := event.Player
	// 确保游戏开启
	if !p.IsStartPubg() || p.world.GetId() != player.WorldId {
		return
	}
	req := event.Req
	worldGadgetId, exist := p.entityIdWorldGadgetIdMap[req.GadgetEntityId]
	if exist {
		dbAvatar := player.GetDbAvatar()
		avatarId := p.world.GetPlayerActiveAvatarId(player)
		avatar := dbAvatar.GetAvatarById(avatarId)
		pubgWorldGadgetDataConfig := gdconf.GetPubgWorldGadgetDataById(worldGadgetId)
		switch pubgWorldGadgetDataConfig.Type {
		case gdconf.PubgWorldGadgetTypeIncAtk:
			avatar.FightPropMap[constant.FIGHT_PROP_BASE_ATTACK] += float32(pubgWorldGadgetDataConfig.Param[0])
			avatar.FightPropMap[constant.FIGHT_PROP_CUR_ATTACK] += float32(pubgWorldGadgetDataConfig.Param[0])
		case gdconf.PubgWorldGadgetTypeIncHp:
			avatar.FightPropMap[constant.FIGHT_PROP_CUR_HP] += float32(pubgWorldGadgetDataConfig.Param[0])
			if avatar.FightPropMap[constant.FIGHT_PROP_CUR_HP] > avatar.FightPropMap[constant.FIGHT_PROP_MAX_HP] {
				avatar.FightPropMap[constant.FIGHT_PROP_CUR_HP] = avatar.FightPropMap[constant.FIGHT_PROP_MAX_HP]
			}
		}
		GAME.SendMsg(cmd.AvatarFightPropUpdateNotify, player.PlayerId, player.ClientSeq, &proto.AvatarFightPropUpdateNotify{
			AvatarGuid:   avatar.Guid,
			FightPropMap: avatar.FightPropMap,
		})
	}
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		logger.Error("get world is nil, worldId: %v, uid: %v", player.WorldId, player.PlayerId)
		return
	}
	scene := world.GetSceneById(player.GetSceneId())
	GAME.KillEntity(player, scene, req.GadgetEntityId, proto.PlayerDieType_PLAYER_DIE_NONE)
	rsp := &proto.GadgetInteractRsp{
		GadgetEntityId: req.GadgetEntityId,
		GadgetId:       req.GadgetId,
		OpType:         req.OpType,
		InteractType:   proto.InteractType_INTERACT_GATHER,
	}
	GAME.SendMsg(cmd.GadgetInteractRsp, player.PlayerId, player.ClientSeq, rsp)
	event.Cancel()
}

// EventPostEnterScene 玩家进入 AI 世界后注入 Lua bytecode
//
// 注入的 Lua（Lua 源代码注释里有）作用：
//  1. 让客户端 GM 按钮可见（CS.UnityEngine.GameObject.Find("/Canvas/.../BtnGm").SetActive(true)）
//  2. 隐藏多人世界小地图玩家位置标记 Layer3（吃鸡玩法不能让玩家直接看见对手位置）
//
// 走 PlayerLuaShellNotify 把魔改 Lua bytecode 发给客户端 远程在 XLua 解释器中执行
// **慎用**：客户端 XLua 权限很大 能直接操作 UI 树 之前有人滥用此能力做坏事 详见 CLAUDE.md
func (p *PluginPubg) EventPostEnterScene(iEvent IPluginEvent) {
	event := iEvent.(*PluginEventPostEnterScene)
	player := event.Player
	// 确保游戏开启
	if !p.IsStartPubg() || p.world.GetId() != player.WorldId {
		return
	}
	// 开启GM按钮 隐藏多人世界玩家位置地图标记
	// local btnGm = CS.UnityEngine.GameObject.Find("/Canvas/Pages/InLevelMainPage/GrpMainPage/GrpMainBtn/GrpMainToggle/GrpTopPanel/BtnGm")
	// btnGm:SetActive(true)
	// local miniMapMarkLayer3 = CS.UnityEngine.GameObject.Find("/Canvas/Pages/InLevelMainPage/GrpMainPage/MapInfo/GrpMiniMap/GrpMap/MarkContainer/Layer3")
	// miniMapMarkLayer3:SetActive(false)
	// local mapMarkLayer3 = CS.UnityEngine.GameObject.Find("/Canvas/Pages/InLevelMapPage/GrpMap/MarkContainer/Layer3")
	// mapMarkLayer3:SetActive(false)
	luac, err := base64.StdEncoding.DecodeString("G0x1YVMBGZMNChoKBAQICHhWAAAAAAAAAAAAAAAod0ABDkBhaV93b3JsZC5sdWEAAAAAAAAAAAABBhwAAAAkAEAAKUBAACmAQAApwEAAVgABACyAAAFdQEEA2ACAAGxAgAFkAEAAaUDAAGmAwABpwMAAloABAGyAAAGdQMEAGAEAAKxAgAGkAEAAqUBAAamAQAGpwEAB1sABAKyAAAHdQEEBWAEAAOxAgAEZAIAACAAAAAQDQ1MEDFVuaXR5RW5naW5lBAtHYW1lT2JqZWN0BAVGaW5kFFUvQ2FudmFzL1BhZ2VzL0luTGV2ZWxNYWluUGFnZS9HcnBNYWluUGFnZS9HcnBNYWluQnRuL0dycE1haW5Ub2dnbGUvR3JwVG9wUGFuZWwvQnRuR20EClNldEFjdGl2ZRRZL0NhbnZhcy9QYWdlcy9JbkxldmVsTWFpblBhZ2UvR3JwTWFpblBhZ2UvTWFwSW5mby9HcnBNaW5pTWFwL0dycE1hcC9NYXJrQ29udGFpbmVyL0xheWVyMxQ5L0NhbnZhcy9QYWdlcy9JbkxldmVsTWFwUGFnZS9HcnBNYXAvTWFya0NvbnRhaW5lci9MYXllcjMBAAAAAQAAAAAAHAAAAAEAAAABAAAAAQAAAAEAAAABAAAAAQAAAAIAAAACAAAAAgAAAAMAAAADAAAAAwAAAAMAAAADAAAAAwAAAAQAAAAEAAAABAAAAAUAAAAFAAAABQAAAAUAAAAFAAAABQAAAAYAAAAGAAAABgAAAAYAAAADAAAABmJ0bkdtBgAAABwAAAASbWluaU1hcE1hcmtMYXllcjMPAAAAHAAAAA5tYXBNYXJrTGF5ZXIzGAAAABwAAAABAAAABV9FTlY=")
	if err != nil {
		logger.Error("decode luac error: %v", err)
		return
	}
	GAME.SendMsg(cmd.PlayerLuaShellNotify, player.PlayerId, 0, &proto.PlayerLuaShellNotify{
		ShellType: proto.LuaShellType_LUASHELL_NORMAL,
		Id:        1,
		LuaShell:  luac,
		UseType:   1,
	})
}

// EventEvtDoSkillSucc 使用技能事件
func (p *PluginPubg) EventEvtDoSkillSucc(iEvent IPluginEvent) {
	event := iEvent.(*PluginEventEvtDoSkillSucc)
	player := event.Player
	// 确保游戏开启
	if !p.IsStartPubg() || p.world.GetId() != player.WorldId {
		return
	}
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		logger.Error("get world is nil, worldId: %v, uid: %v", player.WorldId, player.PlayerId)
		return
	}
	ntf := event.Ntf
	worldAvatar := world.GetWorldAvatarByEntityId(ntf.CasterId)
	if worldAvatar == nil {
		return
	}
	avatarDataConfig := gdconf.GetAvatarDataById(int32(worldAvatar.GetAvatarId()))
	if avatarDataConfig == nil {
		return
	}
	logger.Debug("avatar normal attack, avatarId: %v, weaponType: %v, uid: %v", avatarDataConfig.AvatarId, avatarDataConfig.WeaponType, player.PlayerId)
	switch avatarDataConfig.WeaponType {
	case constant.WEAPON_TYPE_SWORD_ONE_HAND, constant.WEAPON_TYPE_CLAYMORE, constant.WEAPON_TYPE_POLE, constant.WEAPON_TYPE_CATALYST:
		scene := world.GetSceneById(player.GetSceneId())
		avatarEntity := scene.GetEntity(worldAvatar.GetAvatarEntityId())
		for _, entity := range scene.GetAllEntity() {
			if entity.GetId() == avatarEntity.GetId() {
				continue
			}
			_, ok := entity.(*AvatarEntity)
			if !ok {
				continue
			}
			distance3D := math.Sqrt(
				(avatarEntity.GetPos().X-entity.GetPos().X)*(avatarEntity.GetPos().X-entity.GetPos().X) +
					(avatarEntity.GetPos().Y-entity.GetPos().Y)*(avatarEntity.GetPos().Y-entity.GetPos().Y) +
					(avatarEntity.GetPos().Z-entity.GetPos().Z)*(avatarEntity.GetPos().Z-entity.GetPos().Z),
			)
			if distance3D > PUBG_NORMAL_ATTACK_DISTANCE {
				continue
			}
			p.PubgHit(scene, entity.GetId(), avatarEntity.GetId(), false)
		}
	default:
	}
}

// EventEvtBeingHit 实体受击事件
func (p *PluginPubg) EventEvtBeingHit(iEvent IPluginEvent) {
	event := iEvent.(*PluginEventEvtBeingHit)
	player := event.Player
	// 确保游戏开启
	if !p.IsStartPubg() || p.world.GetId() != player.WorldId {
		return
	}
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		logger.Error("get world is nil, worldId: %v, uid: %v", player.WorldId, player.PlayerId)
		return
	}
	scene := world.GetSceneById(player.GetSceneId())
	hitInfo := event.HitInfo
	attackResult := hitInfo.AttackResult
	if attackResult == nil {
		return
	}
	defEntity := scene.GetEntity(attackResult.DefenseId)
	if defEntity == nil {
		return
	}
	defAvatarEntity, ok := defEntity.(*AvatarEntity)
	if !ok {
		return
	}
	fightProp := defEntity.GetFightProp()
	currHp := fightProp[constant.FIGHT_PROP_CUR_HP]
	if currHp-attackResult.Damage > 0.0 {
		return
	}
	defPlayer := USER_MANAGER.GetOnlineUser(defAvatarEntity.GetUid())
	if defPlayer == nil {
		return
	}
	atkEntity := scene.GetEntity(attackResult.AttackerId)
	if atkEntity == nil {
		return
	}
	atkAvatarEntity, ok := atkEntity.(*AvatarEntity)
	if !ok {
		return
	}
	atkPlayer := USER_MANAGER.GetOnlineUser(atkAvatarEntity.GetUid())
	if atkPlayer == nil {
		return
	}
	info := fmt.Sprintf("『%v』击败了『%v』。", atkPlayer.NickName, defPlayer.NickName)
	GAME.PlayerChatReq(world.GetOwner(), &proto.PlayerChatReq{ChatInfo: &proto.ChatInfo{Content: &proto.ChatInfo_Text{Text: info}}})
	p.CreateUserTimer(defPlayer.PlayerId, 10, p.UserTimerPubgDieExit, p.world.GetId())
}

// EvtCreateGadget 创建物件事件 - 拦截弓箭子弹送物理引擎模拟
//
// 仅识别两种弓箭蓄力子弹：
//   - Bullet_ArrowAiming: 普通弓箭蓄力
//   - Bullet_Venti_ArrowAiming: 温迪专属（六命增伤）
//
// pitchAngle 转换：原神客户端的 X 欧拉角是 0~360 角度（俯仰）需要转成 -90~90 范围
//   - 0~90: 仰角（上）→ -原值
//   - 270~360: 俯角（下）→ 360-原值
//
// 提交给 PhysicsEngine 后 子弹按抛物线运动 在每个 tick 检测命中
func (p *PluginPubg) EvtCreateGadget(iEvent IPluginEvent) {
	event := iEvent.(*PluginEventEvtCreateGadget)
	player := event.Player
	// 确保游戏开启
	if !p.IsStartPubg() || p.world.GetId() != player.WorldId {
		return
	}
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		logger.Error("get world is nil, worldId: %v, uid: %v", player.WorldId, player.PlayerId)
		return
	}
	ntf := event.Ntf
	gadgetDataConfig := gdconf.GetGadgetDataById(int32(ntf.ConfigId))
	if gadgetDataConfig == nil {
		logger.Error("gadget data config is nil, gadgetId: %v", ntf.ConfigId)
		return
	}
	// 蓄力箭
	if gadgetDataConfig.PrefabPath != "ART/Others/Bullet/Bullet_ArrowAiming" &&
		gadgetDataConfig.PrefabPath != "ART/Others/Bullet/Bullet_Venti_ArrowAiming" {
		return
	}
	pitchAngleRaw := ntf.InitEulerAngles.X
	pitchAngle := float32(0.0)
	if pitchAngleRaw < 90.0 {
		pitchAngle = -pitchAngleRaw
	} else if pitchAngleRaw > 270.0 {
		pitchAngle = 360.0 - pitchAngleRaw
	} else {
		logger.Error("invalid raw pitch angle: %v, uid: %v", pitchAngleRaw, player.PlayerId)
		return
	}
	yawAngle := ntf.InitEulerAngles.Y
	bulletPhysicsEngine := world.GetBulletPhysicsEngine()
	bulletPhysicsEngine.CreateRigidBody(
		ntf.EntityId,
		world.GetPlayerActiveAvatarEntity(player).GetId(),
		player.GetSceneId(),
		ntf.InitPos.X, ntf.InitPos.Y, ntf.InitPos.Z,
		pitchAngle, yawAngle,
	)
}

// EvtBulletHit 子弹命中事件
func (p *PluginPubg) EvtBulletHit(iEvent IPluginEvent) {
	event := iEvent.(*PluginEventEvtBulletHit)
	player := event.Player
	// 确保游戏开启
	if !p.IsStartPubg() || p.world.GetId() != player.WorldId {
		return
	}
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		logger.Error("get world is nil, worldId: %v, uid: %v", player.WorldId, player.PlayerId)
		return
	}
	ntf := event.Ntf
	bulletPhysicsEngine := world.GetBulletPhysicsEngine()
	if bulletPhysicsEngine.IsRigidBody(ntf.EntityId) {
		bulletPhysicsEngine.DestroyRigidBody(ntf.EntityId)
		_ = ntf.HitPoint
	}
}

/************************************************** 全局定时器 **************************************************/

// GlobalTickPubg PUBG 主定时器（每秒执行一次）
//
// 处理：
//  1. UpdateArea 更新缩圈进度 + 通知客户端
//  2. 玩家在蓝区外 → 调 handleEvtBeingHit 扣 PUBG_HP_LOST 血量（毒区伤害）
//  3. 检查存活玩家：
//     · 1 人 → 吃鸡 大吉大利消息 + StopPubg
//     · 0 人 → 直接停止
func (p *PluginPubg) GlobalTickPubg() {
	world := p.world
	if world == nil {
		return
	}
	// 确保游戏开启
	if !p.IsStartPubg() {
		return
	}
	p.UpdateArea()
	scene := world.GetSceneById(world.GetOwner().GetSceneId())
	for _, scenePlayer := range scene.GetAllPlayer() {
		pos := GAME.GetPlayerPos(scenePlayer)
		if !p.IsInBlueArea(pos) {
			GAME.handleEvtBeingHit(scenePlayer, scene, &proto.EvtBeingHitInfo{
				AttackResult: &proto.AttackResult{
					AttackerId: 0,
					DefenseId:  world.GetPlayerActiveAvatarEntity(scenePlayer).GetId(),
					Damage:     PUBG_HP_LOST,
				},
			})
		}
	}
	alivePlayerList := p.GetAlivePlayerList()
	if len(alivePlayerList) <= 1 {
		if len(alivePlayerList) == 1 {
			info := fmt.Sprintf("『%v』大吉大利，今晚吃鸡。", alivePlayerList[0].NickName)
			GAME.PlayerChatReq(world.GetOwner(), &proto.PlayerChatReq{ChatInfo: &proto.ChatInfo{Content: &proto.ChatInfo_Text{Text: info}}})
		}
		p.StopPubg()
	}
}

// GlobalTickMinuteChange 每分钟变更时检查是否到自动开局时间
// 算法：每个 GS 分配不同 startMinute（按 GsId 取模） 每 60 分钟自动开 6 局
// 例：GsId=1 → 0/10/20/30/40/50 分钟开局；GsId=2 → 仍然 6 个时间点但都是 10 分钟周期
//
// 这意味着如果配置 6 个 GS 实例 集群每 10 分钟有一局自动开始
func (p *PluginPubg) GlobalTickMinuteChange() {
	minute := time.Now().Minute()
	roomNumber := GAME.GetGsId() - 1
	startMinute := roomNumber % 6 * 10
	if uint32(minute) == startMinute {
		p.StartPubg()
	}
}

/************************************************** 用户定时器 **************************************************/

// UserTimerPubgEnd pubg游戏结束后执行定时器
func (p *PluginPubg) UserTimerPubgEnd(player *model.Player, data []any) {
	logger.Debug("UserTimerPubgEnd, seq: %v", p.seq)
	oldWorld := WORLD_MANAGER.GetWorldById(player.WorldId)
	if oldWorld == nil {
		return
	}
	GAME.WorldRemovePlayer(oldWorld, player)
	newWorld := WORLD_MANAGER.CreateWorld(player)
	GAME.WorldAddPlayer(newWorld, player)
	GAME.HostEnterMpWorld(player)
	GAME.EnterSceneReadyReq(player, &proto.EnterSceneReadyReq{
		EnterSceneToken: newWorld.GetEnterSceneToken(),
	})
	GAME.SceneInitFinishReq(player, &proto.SceneInitFinishReq{
		EnterSceneToken: newWorld.GetEnterSceneToken(),
	})
	GAME.EnterSceneDoneReq(player, &proto.EnterSceneDoneReq{
		EnterSceneToken: newWorld.GetEnterSceneToken(),
	})
	GAME.PostEnterSceneReq(player, &proto.PostEnterSceneReq{
		EnterSceneToken: newWorld.GetEnterSceneToken(),
	})
	WORLD_MANAGER.InitAiWorld(player)
	roomNumber := GAME.GetGsId() - 1
	startMinute := roomNumber % 6 * 10
	info := fmt.Sprintf("下一次游戏开启时间：%02d:%02d。", time.Now().Add(time.Hour).Hour(), startMinute)
	GAME.PlayerChatReq(player, &proto.PlayerChatReq{ChatInfo: &proto.ChatInfo{Content: &proto.ChatInfo_Text{Text: info}}})
}

// UserTimerPubgUpdateArea 更新游戏区域
func (p *PluginPubg) UserTimerPubgUpdateArea(player *model.Player, data []any) {
	logger.Debug("UserTimerPubgUpdateArea, seq: %v", p.seq)
	if !p.IsStartPubg() {
		return
	}
	p.phase++
	p.RefreshArea()
}

// UserTimerPubgDieExit pubg死亡离开
func (p *PluginPubg) UserTimerPubgDieExit(player *model.Player, data []any) {
	logger.Debug("UserTimerPubgDieExit, seq: %v", p.seq)
	pubgWorldId := data[0].(uint64)
	if player.WorldId != pubgWorldId {
		return
	}
	GAME.ReLoginPlayer(player.PlayerId, true)
}

/************************************************** 命令控制器 **************************************************/

// pubg游戏命令

func (p *PluginPubg) NewPubgCommandController() *CommandController {
	return &CommandController{
		Name:        "PUBG游戏",
		AliasList:   []string{"pubg"},
		Description: "<color=#FFFFCC>{alias}</color> <color=#FFCC99>测试pubg游戏</color>",
		UsageList: []string{
			"{alias} <start/stop> 开始或关闭pubg游戏",
		},
		Perm: CommandPermGM,
		Func: p.PubgCommand,
	}
}

func (p *PluginPubg) PubgCommand(c *CommandContent) bool {
	var mode string // 模式

	return c.Must("string", func(param any) bool {
		// 模式
		mode = param.(string)
		return true
	}).Execute(func() bool {
		switch mode {
		case "start":
			// 开始游戏
			p.StartPubg()
			c.SendSuccMessage(c.Executor, "已开始PUBG游戏。")
		case "stop":
			// 结束游戏
			p.StopPubg()
			c.SendSuccMessage(c.Executor, "已结束PUBG游戏。")
		default:
			return false
		}
		return true
	})
}

/************************************************** 插件功能 **************************************************/

// StartPubg 开始 PUBG 游戏（GM 命令 /pubg start 或 自动开局触发）
//
// 处理：
//  1. 把所有 PUBG 物件按概率撒到地图（增攻/恢复HP 散点配置在 gdconf/game_data_config/ext/）
//  2. phase = START 走 RefreshArea 生成第 1 圈安全区
//  3. 重置每个玩家的 fightProp（清零原版属性 用 PUBG_HP/PUBG_ATK 标准化值）
//  4. 关闭无敌/无限能量/无cd（保留无限体力 让玩家可全图跑动）
//
// 关键设计：所有玩家用统一 1000 HP + 100 ATK 玩法公平 不带原版养成数据进 PUBG
func (p *PluginPubg) StartPubg() {
	if p.IsStartPubg() {
		return
	}
	p.seq++
	logger.Debug("StartPubg, seq: %v", p.seq)
	world := WORLD_MANAGER.GetAiWorld()
	p.world = world
	info := "游戏开始。"
	GAME.PlayerChatReq(p.world.GetOwner(), &proto.PlayerChatReq{ChatInfo: &proto.ChatInfo{Content: &proto.ChatInfo_Text{Text: info}}})
	for _, pubgWorldGadgetDataConfig := range gdconf.GetPubgWorldGadgetDataMap() {
		rn := random.GetRandomInt32(1, 100)
		if rn > pubgWorldGadgetDataConfig.Probability {
			continue
		}
		entityId := GAME.CreateGadget(
			p.world.GetOwner(),
			&model.Vector{X: float64(pubgWorldGadgetDataConfig.X), Y: float64(pubgWorldGadgetDataConfig.Y), Z: float64(pubgWorldGadgetDataConfig.Z)},
			uint32(pubgWorldGadgetDataConfig.GadgetId),
		)
		p.entityIdWorldGadgetIdMap[entityId] = pubgWorldGadgetDataConfig.WorldGadgetId
	}
	p.phase = PUBG_PHASE_START
	p.RefreshArea()
	for _, player := range world.GetAllPlayer() {
		dbAvatar := player.GetDbAvatar()
		avatarId := p.world.GetPlayerActiveAvatarId(player)
		avatar := dbAvatar.GetAvatarById(avatarId)
		for k := range avatar.FightPropMap {
			avatar.FightPropMap[k] = 0.0
		}
		avatar.FightPropMap[constant.FIGHT_PROP_BASE_HP] = PUBG_HP
		avatar.FightPropMap[constant.FIGHT_PROP_MAX_HP] = PUBG_HP
		avatar.FightPropMap[constant.FIGHT_PROP_CUR_HP] = PUBG_HP
		avatar.FightPropMap[constant.FIGHT_PROP_BASE_ATTACK] = PUBG_ATK
		avatar.FightPropMap[constant.FIGHT_PROP_CUR_ATTACK] = PUBG_ATK
		GAME.SendMsg(cmd.AvatarFightPropUpdateNotify, player.PlayerId, player.ClientSeq, &proto.AvatarFightPropUpdateNotify{
			AvatarGuid:   avatar.Guid,
			FightPropMap: avatar.FightPropMap,
		})
		p.playerHitTimeMap[player.PlayerId] = 0
		player.WuDi = false
		player.EnergyInf = false
		player.StaminaInf = true
		player.NoCd = false
	}
}

// StopPubg 结束pubg游戏
func (p *PluginPubg) StopPubg() {
	if !p.IsStartPubg() {
		return
	}
	logger.Debug("StopPubg, seq: %v", p.seq)
	owner := p.world.GetOwner()
	for eid := range p.entityIdWorldGadgetIdMap {
		GAME.KillEntity(owner, p.world.GetSceneById(owner.GetSceneId()), eid, proto.PlayerDieType_PLAYER_DIE_GM)
	}
	p.world = nil
	p.blueAreaCenterPos = &model.Vector{X: 0.0, Y: 0.0, Z: 0.0}
	p.blueAreaRadius = 0.0
	p.safeAreaCenterPos = &model.Vector{X: 0.0, Y: 0.0, Z: 0.0}
	p.safeAreaRadius = 0.0
	p.phase = PUBG_PHASE_WAIT
	p.areaReduceRadiusSpeed = 0.0
	p.areaReduceXSpeed = 0.0
	p.areaReduceZSpeed = 0.0
	p.areaPointList = make([]*proto.MapMarkPoint, 0)
	p.entityIdWorldGadgetIdMap = make(map[uint32]int32)
	p.playerHitTimeMap = make(map[uint32]int64)
	p.CreateUserTimer(owner.PlayerId, 60, p.UserTimerPubgEnd)
	info := "游戏结束。"
	GAME.PlayerChatReq(owner, &proto.PlayerChatReq{ChatInfo: &proto.ChatInfo{Content: &proto.ChatInfo_Text{Text: info}}})
}

// IsStartPubg pubg游戏是否开启
func (p *PluginPubg) IsStartPubg() bool {
	return p.phase != PUBG_PHASE_WAIT
}

// GetAreaPointList 获取游戏区域标点列表
func (p *PluginPubg) GetAreaPointList() []*proto.MapMarkPoint {
	return p.areaPointList
}

// UpdateArea 更新游戏区域
func (p *PluginPubg) UpdateArea() {
	if p.areaReduceRadiusSpeed > 0.0 && p.blueAreaRadius > p.safeAreaRadius {
		p.blueAreaRadius -= p.areaReduceRadiusSpeed
		p.blueAreaCenterPos.X += p.areaReduceXSpeed
		p.blueAreaCenterPos.Z += p.areaReduceZSpeed
		p.SyncMapMarkArea()
	}
}

// IsInBlueArea 是否在蓝圈内
func (p *PluginPubg) IsInBlueArea(pos *model.Vector) bool {
	distance2D := math.Sqrt(
		(p.blueAreaCenterPos.X-pos.X)*(p.blueAreaCenterPos.X-pos.X) +
			(p.blueAreaCenterPos.Z-pos.Z)*(p.blueAreaCenterPos.Z-pos.Z),
	)
	return distance2D < p.blueAreaRadius
}

// RefreshArea 推进游戏阶段 + 重新计算安全区/缩圈速度
//
// 阶段循环（phase % 3）：
//   - START(0): 第一圈大蓝区（半径 2000） 等待玩家落地（3 分钟无敌时间）
//   - phase%3==1: 生成新安全区（在当前蓝区内随机偏移 半径减半）+ 等 PUBG_PHASE_INV_TIME 倒计时
//   - phase%3==2: 蓝区开始缩向新安全区（持续 PUBG_FIRST_AREA_REDUCE_TIME 或 PUBG_PHASE_INV_TIME）
//   - phase%3==0: 缩圈完毕 蓝区与安全区重合 等待下次刷新
//   - END(16): 安全区消失 全图变蓝区 决出胜者
//
// 通过 SyncMapMarkArea 把蓝区/安全区圆周转成 72 个标点（每 5 度一个）发给客户端显示
// 客户端在小地图上看到的圆圈就是这些点连起来的
func (p *PluginPubg) RefreshArea() {
	info := ""
	if p.phase == PUBG_PHASE_START {
		info = fmt.Sprintf("安全区已生成，当前%v位存活玩家。", len(p.GetAlivePlayerList()))
		p.blueAreaCenterPos = &model.Vector{X: 500.0, Y: 0.0, Z: -500.0}
		p.blueAreaRadius = 2000.0
		p.safeAreaCenterPos = &model.Vector{X: 0.0, Y: 0.0, Z: 0.0}
		p.safeAreaRadius = 0.0
		p.CreateUserTimer(p.world.GetOwner().PlayerId, PUBG_PHASE_INV_TIME, p.UserTimerPubgUpdateArea)
	} else if p.phase == PUBG_PHASE_END {
		info = "安全区已消失。"
		p.blueAreaRadius = 0.0
		p.safeAreaRadius = 0.0
	} else {
		switch p.phase % 3 {
		case 1:
			info = fmt.Sprintf("新的安全区已出现，进度%.1f%%。", float64(p.phase)/PUBG_PHASE_END*100.0)
			p.safeAreaCenterPos = &model.Vector{
				X: p.blueAreaCenterPos.X + random.GetRandomFloat64(-(p.blueAreaRadius*0.7/2.0), p.blueAreaRadius*0.7/2.0),
				Y: 0.0,
				Z: p.blueAreaCenterPos.Z + random.GetRandomFloat64(-(p.blueAreaRadius*0.7/2.0), p.blueAreaRadius*0.7/2.0),
			}
			p.safeAreaRadius = p.blueAreaRadius / 2.0
			p.areaReduceRadiusSpeed = 0.0
			p.CreateUserTimer(p.world.GetOwner().PlayerId, PUBG_PHASE_INV_TIME, p.UserTimerPubgUpdateArea)
		case 2:
			info = fmt.Sprintf("安全区正在缩小，进度%.1f%%。", float64(p.phase)/PUBG_PHASE_END*100.0)
			invTime := 0.0
			if p.phase == PUBG_PHASE_II {
				invTime = PUBG_FIRST_AREA_REDUCE_TIME
			} else {
				invTime = PUBG_PHASE_INV_TIME
			}
			p.areaReduceRadiusSpeed = (p.blueAreaRadius - p.safeAreaRadius) / invTime
			p.areaReduceXSpeed = (p.safeAreaCenterPos.X - p.blueAreaCenterPos.X) / invTime
			p.areaReduceZSpeed = (p.safeAreaCenterPos.Z - p.blueAreaCenterPos.Z) / invTime
			p.CreateUserTimer(p.world.GetOwner().PlayerId, uint32(invTime), p.UserTimerPubgUpdateArea)
		case 0:
			info = fmt.Sprintf("安全区缩小完毕，进度%.1f%%。", float64(p.phase)/PUBG_PHASE_END*100.0)
			p.CreateUserTimer(p.world.GetOwner().PlayerId, PUBG_PHASE_INV_TIME, p.UserTimerPubgUpdateArea)
		}
	}
	p.SyncMapMarkArea()
	GAME.PlayerChatReq(p.world.GetOwner(), &proto.PlayerChatReq{ChatInfo: &proto.ChatInfo{Content: &proto.ChatInfo_Text{Text: info}}})
}

// SyncMapMarkArea 同步地图标点区域
func (p *PluginPubg) SyncMapMarkArea() {
	p.areaPointList = make([]*proto.MapMarkPoint, 0)
	if p.blueAreaRadius > 0.0 {
		for angleStep := 0; angleStep < 360; angleStep += 5 {
			x := p.blueAreaRadius*math.Cos(float64(angleStep)/360.0*2*math.Pi) + p.blueAreaCenterPos.X
			z := p.blueAreaRadius*math.Sin(float64(angleStep)/360.0*2*math.Pi) + p.blueAreaCenterPos.Z
			p.areaPointList = append(p.areaPointList, &proto.MapMarkPoint{
				SceneId:   p.world.GetOwner().GetSceneId(),
				Name:      "",
				Pos:       &proto.Vector{X: float32(x), Y: 0, Z: float32(z)},
				PointType: proto.MapMarkPointType_SPECIAL,
			})
		}
	}
	if p.safeAreaRadius > 0.0 {
		for angleStep := 0; angleStep < 360; angleStep += 5 {
			x := p.safeAreaRadius*math.Cos(float64(angleStep)/360.0*2*math.Pi) + p.safeAreaCenterPos.X
			z := p.safeAreaRadius*math.Sin(float64(angleStep)/360.0*2*math.Pi) + p.safeAreaCenterPos.Z
			p.areaPointList = append(p.areaPointList, &proto.MapMarkPoint{
				SceneId:   p.world.GetOwner().GetSceneId(),
				Name:      "",
				Pos:       &proto.Vector{X: float32(x), Y: 0, Z: float32(z)},
				PointType: proto.MapMarkPointType_COLLECTION,
			})
		}
	}
	for _, player := range p.world.GetAllPlayer() {
		GAME.SendMsg(cmd.AllMarkPointNotify, player.PlayerId, player.ClientSeq, &proto.AllMarkPointNotify{MarkList: p.areaPointList})
	}
}

// GetAlivePlayerList 获取存活玩家列表
func (p *PluginPubg) GetAlivePlayerList() []*model.Player {
	scene := p.world.GetSceneById(p.world.GetOwner().GetSceneId())
	alivePlayerList := make([]*model.Player, 0)
	for _, scenePlayer := range scene.GetAllPlayer() {
		if scenePlayer.PlayerId == p.world.GetOwner().PlayerId {
			continue
		}
		entity := p.world.GetPlayerActiveAvatarEntity(scenePlayer)
		if entity.GetFightProp()[constant.FIGHT_PROP_CUR_HP] <= 0.0 {
			continue
		}
		alivePlayerList = append(alivePlayerList, scenePlayer)
	}
	return alivePlayerList
}

// PubgHit PUBG 击中处理（弓箭命中或近战命中都走这里）
//
// 处理：
//  1. 攻击间隔保护：同一攻击者 500ms 内不能重复触发命中（PUBG_NORMAL_ATTACK_INTERVAL_TIME）
//     防止客户端同一帧重复发命中包导致瞬秒
//  2. 伤害计算：
//     · 弓箭命中：dmg = ATK / 2.0（即弓箭伤害是攻击力的一半 调平攻击节奏）
//     · 近战命中：dmg = ATK / 5.0（近战伤害更低 但攻击间隔短）
//  3. 调 handleEvtBeingHit 走原版伤害结算流程（扣血）
//  4. 构造 EvtBeingHitInfo + CombatInvocationsNotify 广播给场景内其他玩家
//     让其他玩家客户端也看到命中特效（因为命中由服务端判定 不是客户端原生事件）
func (p *PluginPubg) PubgHit(scene *Scene, defAvatarEntityId uint32, atkAvatarEntityId uint32, isBow bool) {
	defEntity := scene.GetEntity(defAvatarEntityId)
	if defEntity == nil {
		return
	}
	defPlayer := USER_MANAGER.GetOnlineUser(defEntity.(*AvatarEntity).GetUid())
	if defPlayer == nil {
		return
	}
	atkEntity := scene.GetEntity(atkAvatarEntityId)
	if atkEntity == nil {
		return
	}
	atkPlayer := USER_MANAGER.GetOnlineUser(atkEntity.(*AvatarEntity).GetUid())
	if atkPlayer == nil {
		return
	}
	now := time.Now().UnixMilli()
	lastHitTime := p.playerHitTimeMap[atkPlayer.PlayerId]
	if now-lastHitTime < PUBG_NORMAL_ATTACK_INTERVAL_TIME {
		return
	}
	p.playerHitTimeMap[atkPlayer.PlayerId] = now
	atk := atkEntity.GetFightProp()[constant.FIGHT_PROP_CUR_ATTACK]
	dmg := float32(0.0)
	if isBow {
		dmg = atk / PUBG_BOW_ATTACK_ATK_RATIO
	} else {
		dmg = atk / PUBG_NORMAL_ATTACK_ATK_RATIO
	}
	GAME.handleEvtBeingHit(defPlayer, scene, &proto.EvtBeingHitInfo{
		AttackResult: &proto.AttackResult{
			AttackerId:   atkEntity.GetId(),
			DefenseId:    defEntity.GetId(),
			Damage:       dmg,
			DamageShield: dmg,
		},
	})
	pos := GAME.GetPlayerPos(defPlayer)
	evtBeingHitInfo := &proto.EvtBeingHitInfo{
		AttackResult: &proto.AttackResult{
			AttackerId:        atkEntity.GetId(),
			DefenseId:         defEntity.GetId(),
			Damage:            dmg,
			DamageShield:      dmg,
			HitEffResult:      &proto.AttackHitEffectResult{},
			ResolvedDir:       &proto.Vector{},
			HashedAnimEventId: uint32(endec.Hk4eAbilityHashCode("ATK01")),
			HitCollision: &proto.HitCollision{
				HitPoint: &proto.Vector{X: float32(pos.X), Y: float32(pos.Y), Z: float32(pos.Z)},
				HitDir:   &proto.Vector{},
			},
		},
	}
	combatData, err := pb.Marshal(evtBeingHitInfo)
	if err != nil {
		return
	}
	GAME.SendToSceneA(scene, cmd.CombatInvocationsNotify, 0, &proto.CombatInvocationsNotify{
		InvokeList: []*proto.CombatInvokeEntry{{
			CombatData:   combatData,
			ForwardType:  proto.ForwardType_FORWARD_TO_ALL,
			ArgumentType: proto.CombatTypeArgument_COMBAT_EVT_BEING_HIT,
		}},
	}, 0)
}
