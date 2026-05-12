package game

import (
	"math"
	"strings"

	"hk4e/common/constant"
	"hk4e/gdconf"
	"hk4e/gs/model"
	"hk4e/pkg/alg"
	"hk4e/pkg/reflection"
	"hk4e/protocol/cmd"
	"hk4e/protocol/proto"

	"github.com/flswld/halo/logger"
	pb "google.golang.org/protobuf/proto"
)

// 战斗同步 模块
//
// 原神战斗采用 Frame Sync（动作同步）模型 而非状态同步：
//   - 客户端把 ability/combat 操作打包成 InvokeEntry 走 UnionCmdNotify 发服务端
//   - 服务端按 ForwardType 分流转发给世界内其他玩家 让其他客户端同步执行相同动作
//   - 服务端自身**不做伤害计算和反作弊校验**（因为 Ability 系统大部分未实现）
//
// 主要职责：
//   - UnionCmdNotify: 解包客户端聚合包 拆出多个 InvokeEntry 分别走 DoForward
//   - CombatInvocationsNotify: 战斗事件通知（被击中/移动/动画状态等）
//   - AbilityInvocationsNotify: 能力事件通知（技能/buff等）
//   - ClientAbilityInitFinishNotify/ClientAbilityChangeNotify: 能力初始化/变更
//   - 一系列Evt*Notify: 子弹/聚焦/创建物件等子事件通知
//   - SceneBlockAoiPlayerMove/AiWorldAoiPlayerMove: 玩家移动后的 AOI 更新
//
// 关键工具：DoForward 泛型函数 按 ForwardType 把 InvokeEntry 分流到对应广播范围

var cmdProtoMap *cmd.CmdProtoMap = nil

// DoForward 战斗事件分流转发（核心广播逻辑）
//
// 客户端的每个 InvokeEntry 带 ForwardType 字段 表示要发给谁：
//   - FORWARD_TO_ALL: 场景内所有玩家（含自己） → SendToSceneA(exceptUid=0)
//   - FORWARD_TO_ALL_EXCEPT_CUR: 场景内除自己外所有玩家 → SendToSceneA(exceptUid=自己)
//   - FORWARD_TO_HOST: 仅发给世界房主 → SendToWorldH
//
// AI世界特殊：用 SendToSceneACV 限定客户端版本（PUBG玩法的子弹模拟需要）+ AOI格子过滤
// 此外修复一个 AOI bug：发送前清掉已不在世界但残留在 AOI 格子里的玩家引用
func DoForward[IET model.InvokeEntryType](player *model.Player, invokeHandler *model.InvokeHandler[IET],
	cmdId uint16, newNtf pb.Message, forwardField string,
	srcNtf pb.Message, copyFieldList []string) {
	if cmdProtoMap == nil {
		cmdProtoMap = cmd.NewCmdProtoMap()
	}
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		return
	}
	scene := world.GetSceneById(player.GetSceneId())
	if srcNtf != nil && copyFieldList != nil {
		for _, fieldName := range copyFieldList {
			reflection.CopyStructField(newNtf, srcNtf, fieldName)
		}
	}
	if invokeHandler.AllLen() == 0 && invokeHandler.AllExceptCurLen() == 0 && invokeHandler.HostLen() == 0 {
		return
	}
	if WORLD_MANAGER.IsAiWorld(world) {
		aiWorldAoi := world.GetAiWorldAoi()
		pos := GAME.GetPlayerPos(player)
		gid := aiWorldAoi.GetGidByPos(float32(pos.X), float32(pos.Y), float32(pos.Z))
		if gid == math.MaxUint32 {
			return
		}
		gridList := aiWorldAoi.GetSurrGridListByGid(gid, 1)
		for _, grid := range gridList {
			objectList := grid.GetObjectList()
			for uid, wa := range objectList {
				playerMap := world.GetAllPlayer()
				_, exist := playerMap[uint32(uid)]
				if !exist {
					logger.Error("remove not in world player cause by aoi bug, niw uid: %v, niw wa: %+v, uid: %v", uid, wa, player.PlayerId)
					delete(objectList, uid)
				}
			}
		}
	}
	if WORLD_MANAGER.IsAiWorld(world) && cmdId != cmd.CombatInvocationsNotify {
		if invokeHandler.AllLen() > 0 {
			reflection.SetStructFieldValue(newNtf, forwardField, invokeHandler.EntryListForwardAll)
			GAME.SendToSceneACV(scene, cmdId, player.ClientSeq, newNtf, 0, player.ClientVersion)
		}
		if invokeHandler.AllExceptCurLen() > 0 {
			reflection.SetStructFieldValue(newNtf, forwardField, invokeHandler.EntryListForwardAllExceptCur)
			GAME.SendToSceneACV(scene, cmdId, player.ClientSeq, newNtf, player.PlayerId, player.ClientVersion)
		}
		if invokeHandler.HostLen() > 0 {
			reflection.SetStructFieldValue(newNtf, forwardField, invokeHandler.EntryListForwardHost)
			GAME.SendToWorldH(world, cmdId, player.ClientSeq, newNtf)
		}
		return
	}
	if invokeHandler.AllLen() > 0 {
		reflection.SetStructFieldValue(newNtf, forwardField, invokeHandler.EntryListForwardAll)
		GAME.SendToSceneA(scene, cmdId, player.ClientSeq, newNtf, 0)
	}
	if invokeHandler.AllExceptCurLen() > 0 {
		reflection.SetStructFieldValue(newNtf, forwardField, invokeHandler.EntryListForwardAllExceptCur)
		GAME.SendToSceneA(scene, cmdId, player.ClientSeq, newNtf, player.PlayerId)
	}
	if invokeHandler.HostLen() > 0 {
		reflection.SetStructFieldValue(newNtf, forwardField, invokeHandler.EntryListForwardHost)
		GAME.SendToWorldH(world, cmdId, player.ClientSeq, newNtf)
	}
}

// UnionCmdNotify 聚合消息处理
//
// 客户端为减少网络开销 会把多个高频消息（移动/动画/ability）打包成 UnionCmdNotify 批量发送
// 实际是个空壳：服务端接收时上层 RouteManager 已经把内层消息单独 Decode 转发到对应 handler
// 这里仅在玩家进场完成后 把累积在 InvokeHandler 里的 entry 批量转发给其他玩家然后清空
func (g *Game) UnionCmdNotify(player *model.Player, payloadMsg pb.Message) {
	ntf := payloadMsg.(*proto.UnionCmdNotify)
	_ = ntf
	if player.SceneLoadState != model.SceneEnterDone {
		return
	}
	DoForward[proto.CombatInvokeEntry](player, player.CombatInvokeHandler,
		cmd.CombatInvocationsNotify, new(proto.CombatInvocationsNotify), "InvokeList",
		nil, nil)
	DoForward[proto.AbilityInvokeEntry](player, player.AbilityInvokeHandler,
		cmd.AbilityInvocationsNotify, new(proto.AbilityInvocationsNotify), "Invokes",
		nil, nil)
	player.CombatInvokeHandler.Clear()
	player.AbilityInvokeHandler.Clear()
}

// CombatInvocationsNotify 战斗事件通知（核心战斗入口）
//
// 客户端的战斗事件按 ArgumentType 分类：
//   - COMBAT_EVT_BEING_HIT: 实体被击中（最关键 → handleEvtBeingHit 扣血）
//   - ENTITY_MOVE: 实体移动（玩家/怪物/物件位置同步 → handleEntityMove 触发AOI更新+天气区域检查）
//   - ANIMATOR_PARAMETER_CHANGED / ANIMATOR_STATE_CHANGED: 动画状态变化（仅解析不处理）
//
// 所有 entry 都会被加入 player.CombatInvokeHandler 等 UnionCmdNotify 批量转发
// 注释里那段"众里寻他千百度"是开发者自嘲调试动画问题的发现：
//
//	只要服务端转发了 MOTION_NOTIFY 或 MOTION_FIGHT 中任意一个 客户端动画就会被打断
func (g *Game) CombatInvocationsNotify(player *model.Player, payloadMsg pb.Message) {
	ntf := payloadMsg.(*proto.CombatInvocationsNotify)
	if player.SceneLoadState != model.SceneEnterDone {
		return
	}
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		return
	}
	scene := world.GetSceneById(player.GetSceneId())
	for _, entry := range ntf.InvokeList {
		switch entry.ArgumentType {
		case proto.CombatTypeArgument_COMBAT_EVT_BEING_HIT:
			evtBeingHitInfo := new(proto.EvtBeingHitInfo)
			err := pb.Unmarshal(entry.CombatData, evtBeingHitInfo)
			if err != nil {
				logger.Error("parse EvtBeingHitInfo error: %v", err)
				break
			}
			g.handleEvtBeingHit(player, scene, evtBeingHitInfo)
		case proto.CombatTypeArgument_ENTITY_MOVE:
			entityMoveInfo := new(proto.EntityMoveInfo)
			err := pb.Unmarshal(entry.CombatData, entityMoveInfo)
			if err != nil {
				logger.Error("parse EntityMoveInfo error: %v", err)
				break
			}
			motionInfo := entityMoveInfo.MotionInfo
			if motionInfo == nil || motionInfo.Pos == nil || motionInfo.Rot == nil {
				break
			}
			g.handleEntityMove(
				player, world, scene, entityMoveInfo.EntityId,
				&model.Vector{X: float64(motionInfo.Pos.X), Y: float64(motionInfo.Pos.Y), Z: float64(motionInfo.Pos.Z)},
				&model.Vector{X: float64(motionInfo.Rot.X), Y: float64(motionInfo.Rot.Y), Z: float64(motionInfo.Rot.Z)},
				false, entityMoveInfo,
			)
			// 众里寻他千百度 蓦然回首 那人却在灯火阑珊处
			if motionInfo.State == proto.MotionState_MOTION_NOTIFY || motionInfo.State == proto.MotionState_MOTION_FIGHT {
				// 只要转发了这两个包的其中之一 客户端的动画就会被打断
				continue
			}
		case proto.CombatTypeArgument_COMBAT_ANIMATOR_PARAMETER_CHANGED:
			evtAnimatorParameterInfo := new(proto.EvtAnimatorParameterInfo)
			err := pb.Unmarshal(entry.CombatData, evtAnimatorParameterInfo)
			if err != nil {
				logger.Error("parse EvtAnimatorParameterInfo error: %v", err)
				break
			}
		case proto.CombatTypeArgument_COMBAT_ANIMATOR_STATE_CHANGED:
			evtAnimatorStateChangedInfo := new(proto.EvtAnimatorStateChangedInfo)
			err := pb.Unmarshal(entry.CombatData, evtAnimatorStateChangedInfo)
			if err != nil {
				logger.Error("parse EvtAnimatorStateChangedInfo error: %v", err)
				break
			}
		}
		player.CombatInvokeHandler.AddEntry(entry.ForwardType, entry)
	}
}

func (g *Game) handleEvtBeingHit(player *model.Player, scene *Scene, hitInfo *proto.EvtBeingHitInfo) {
	// 触发事件
	if PLUGIN_MANAGER.TriggerEvent(PluginEventIdEvtBeingHit, &PluginEventEvtBeingHit{
		PluginEvent: NewPluginEvent(),
		Player:      player,
		HitInfo:     hitInfo,
	}) {
		return
	}

	attackResult := hitInfo.AttackResult
	if attackResult == nil {
		return
	}
	defEntity := scene.GetEntity(attackResult.DefenseId)
	if defEntity == nil {
		return
	}
	var changHpReason proto.ChangHpReason
	atkEntity := scene.GetEntity(attackResult.AttackerId)
	if atkEntity != nil {
		switch atkEntity.GetEntityType() {
		case constant.ENTITY_TYPE_AVATAR:
			changHpReason = proto.ChangHpReason_CHANGE_HP_SUB_AVATAR
		case constant.ENTITY_TYPE_MONSTER:
			changHpReason = proto.ChangHpReason_CHANGE_HP_SUB_MONSTER
		case constant.ENTITY_TYPE_GADGET:
			changHpReason = proto.ChangHpReason_CHANGE_HP_SUB_GEAR
		}
	}
	switch defEntity.(type) {
	case *AvatarEntity:
		avatarEntity := defEntity.(*AvatarEntity)
		g.SubPlayerAvatarHp(player.PlayerId, avatarEntity.GetAvatarId(), attackResult.Damage, 0.0, changHpReason)
	case *MonsterEntity:
		g.SubEntityHp(player, scene, defEntity.GetId(), attackResult.Damage, 0.0, changHpReason)
	case IGadgetEntity:
		iGadgetEntity := defEntity.(IGadgetEntity)
		gadgetDataConfig := gdconf.GetGadgetDataById(int32(iGadgetEntity.GetGadgetId()))
		if gadgetDataConfig == nil {
			logger.Error("get gadget data config is nil, gadgetId: %v", iGadgetEntity.GetGadgetId())
			return
		}
		logger.Debug("[EvtBeingHit] GadgetData: %+v, entityId: %v, uid: %v", gadgetDataConfig, defEntity.GetId(), player.PlayerId)
		if gadgetDataConfig.ServerLuaScript != "" {
			gadgetLuaConfig := gdconf.GetGadgetLuaConfigByName(gadgetDataConfig.ServerLuaScript)
			if gadgetLuaConfig == nil {
				logger.Error("get gadget lua config is nil, name: %v", gadgetDataConfig.ServerLuaScript)
				return
			}
			isHost := player.PlayerId == scene.GetWorld().GetOwner().PlayerId
			CallGadgetLuaFunc(gadgetLuaConfig.LuaState, "OnBeHurt",
				&LuaCtx{uid: player.PlayerId, targetEntityId: defEntity.GetId(), groupId: defEntity.GetGroupId()},
				attackResult.ElementType, 0, isHost)
		}
	}
}

// handleEntityMove 实体移动处理（玩家移动是高频消息 处理逻辑很重）
//
// 玩家角色实体移动时：
//  1. 同步队伍中所有 worldAvatar 实体（包括非出战的）+ 武器实体的位置
//  2. 单人/多人世界：异步加载新block + 调 SceneBlockAoiPlayerMove 处理 AOI 变化
//     · 跨越block格子时才触发 LoadSceneBlockAsync 期间不冻结玩家移动
//  3. AI 世界：调 AiWorldAoiPlayerMove 处理 AOI 变化（PUBG格子 120×12×120）
//  4. 触发 SceneWeatherAreaCheck 检查天气区域是否变化
//  5. 安全位置（state=STANDBY/WALK/RUN/DASH 等）才更新 player.Pos（防止飞天/卡墙后传送）
//  6. 体力消耗（ImmediateStamina）+ 坠落撞击扣血（速度差>20视为坠落）
//
// 物件实体移动时：仅载具会处理体力（划船耗体力）+ 销毁请求
func (g *Game) handleEntityMove(player *model.Player, world *World, scene *Scene, entityId uint32, pos, rot *model.Vector, force bool, moveInfo *proto.EntityMoveInfo) {
	entity := scene.GetEntity(entityId)
	if entity == nil {
		return
	}
	avatarEntity, ok := entity.(*AvatarEntity)
	if ok {
		// 玩家实体在移动
		if avatarEntity.GetUid() != player.PlayerId {
			return
		}
		oldPos := g.GetPlayerPos(player)
		// 更新玩家角色实体的位置信息
		for _, worldAvatar := range world.GetPlayerWorldAvatarList(player) {
			worldAvatarEntityId := worldAvatar.GetAvatarEntityId()
			worldAvatarEntity := scene.GetEntity(worldAvatarEntityId)
			if worldAvatarEntity == nil {
				logger.Error("world avatar entity is nil, worldAvatar: %+v, uid: %v", worldAvatar, player.PlayerId)
				continue
			}
			worldAvatarEntity.SetPos(pos)
			worldAvatarEntity.SetRot(rot)
			worldWeaponEntityId := worldAvatar.GetWeaponEntityId()
			worldWeaponEntity := scene.GetEntity(worldWeaponEntityId)
			if worldWeaponEntity == nil {
				logger.Error("world weapon entity is nil, worldAvatar: %+v, uid: %v", worldAvatar, player.PlayerId)
				continue
			}
			worldWeaponEntity.SetPos(pos)
			worldWeaponEntity.SetRot(rot)
		}
		if !WORLD_MANAGER.IsAiWorld(world) {
			if !world.IsValidScenePos(scene.GetId(), float32(pos.X), 0.0, float32(pos.Z)) {
				return
			}
			wait := g.LoadSceneBlockAsync(player, scene, scene, oldPos, pos, "SceneBlockAoiPlayerMove", &SceneBlockLoadInfoCtx{
				World:          world,
				Scene:          scene,
				OldPos:         oldPos,
				NewPos:         pos,
				AvatarEntityId: entity.GetId(),
			})
			if wait {
				return
			}
			g.SceneBlockAoiPlayerMove(player, world, scene, oldPos, pos, entity.GetId())
		} else {
			if !world.IsValidAiWorldPos(scene.GetId(), float32(pos.X), float32(pos.Y), float32(pos.Z)) {
				return
			}
			g.AiWorldAoiPlayerMove(player, world, scene, oldPos, pos)
		}
		// 场景天气区域变更检测
		g.SceneWeatherAreaCheck(player, oldPos, pos)
	}
	// 更新场景实体的位置信息
	entity.SetPos(pos)
	entity.SetRot(rot)
	if !force {
		motionInfo := moveInfo.MotionInfo
		switch entity.(type) {
		case *AvatarEntity:
			switch motionInfo.State {
			case proto.MotionState_MOTION_STANDBY, proto.MotionState_MOTION_WALK, proto.MotionState_MOTION_RUN, proto.MotionState_MOTION_DASH,
				58, 59, 60, 64, 65, 66:
				// 更新玩家安全位置
				player.SetPos(pos)
				player.SetRot(rot)
			}
			// 处理耐力消耗
			g.ImmediateStamina(player, motionInfo.State)
			// 坠落撞击扣血
			if player.Speed == nil {
				player.Speed = &model.Vector{X: float64(motionInfo.Speed.X), Y: float64(motionInfo.Speed.Y), Z: float64(motionInfo.Speed.Z)}
			}
			if motionInfo.State == proto.MotionState_MOTION_FALL_ON_GROUND || motionInfo.State == proto.MotionState_MOTION_FIGHT {
				oldSpeed := &alg.Vector3{X: float32(player.Speed.X), Y: float32(player.Speed.Y), Z: float32(player.Speed.Z)}
				newSpeed := &alg.Vector3{X: motionInfo.Speed.X, Y: motionInfo.Speed.Y, Z: motionInfo.Speed.Z}
				deltaSpeed := alg.Vector3Sub(oldSpeed, newSpeed)
				deltaSpeedMag := alg.Vector3Magnitude(deltaSpeed)
				if deltaSpeedMag > 20.0 {
					logger.Debug("player fall on ground, deltaSpeed: %+v, deltaSpeedMag: %v, uid: %v", deltaSpeed, deltaSpeedMag, player.PlayerId)
					// 速度矢量20-30部分线性映射到最大生命值百分比扣血
					rate := deltaSpeedMag - 20.0
					if rate > 10.0 {
						rate = 10.0
					}
					rate /= 10.0
					// 下落攻击最大生命值百分比扣血上限为40
					if motionInfo.State == proto.MotionState_MOTION_FIGHT {
						if rate > 0.4 {
							rate = 0.4
						}
					}
					fightProp := entity.GetFightProp()
					maxHp := fightProp[constant.FIGHT_PROP_MAX_HP]
					g.SubPlayerAvatarHp(player.PlayerId, avatarEntity.GetAvatarId(), maxHp*rate, 0.0, proto.ChangHpReason_CHANGE_HP_SUB_FALL)
				}
			}
			player.Speed = &model.Vector{X: float64(motionInfo.Speed.X), Y: float64(motionInfo.Speed.Y), Z: float64(motionInfo.Speed.Z)}
		case IGadgetEntity:
			_, ok := entity.(*GadgetVehicleEntity)
			if ok {
				// 处理耐力消耗
				g.ImmediateStamina(player, motionInfo.State)
				// 处理载具销毁请求
				g.VehicleDestroyMotion(player, entity, motionInfo.State)
			}
		}
		entity.SetMoveState(uint16(motionInfo.State))
		entity.SetLastMoveSceneTimeMs(moveInfo.SceneTime)
		entity.SetLastMoveReliableSeq(moveInfo.ReliableSeq)
	}
}

// SceneBlockAoiPlayerMove 普通世界玩家跨格子的 AOI 处理（不涉及 AI 世界）
//
// 计算并处理 4 类变化：
//  1. 卸载 group：旧位置邻接但新位置不邻接的 group → RemoveSceneGroup
//     · 多人世界还要确认其他玩家也都离开了这个 group 才能卸
//  2. 加载 group：新位置邻接但旧位置不邻接的 group → AddSceneGroup
//  3. 实体消失：旧视野有但新视野没有的实体 → 给自己发 VISION_MISS
//     · 角色实体特殊：双向消失（自己看不到对方 + 对方也看不到自己）
//  4. 实体出现：新视野有但旧视野没有的实体 → 给自己发 VISION_MEET
//     · 角色实体特殊：双向出现
//
// 最后调 SceneRegionTriggerCheck 检测玩家是否进入/离开 Lua 触发区域
func (g *Game) SceneBlockAoiPlayerMove(player *model.Player, world *World, scene *Scene, oldPos *model.Vector, newPos *model.Vector, avatarEntityId uint32) {
	// 加载和卸载的group
	oldNeighborGroupMap := g.GetNeighborGroup(player.GetSceneId(), oldPos)
	newNeighborGroupMap := g.GetNeighborGroup(player.GetSceneId(), newPos)
	otherPlayerNeighborGroupMap := make(map[uint32]*gdconf.Group)
	for _, otherPlayer := range scene.GetAllPlayer() {
		otherPlayerPos := g.GetPlayerPos(otherPlayer)
		for k, v := range g.GetNeighborGroup(player.GetSceneId(), otherPlayerPos) {
			otherPlayerNeighborGroupMap[k] = v
		}
	}
	for groupId, groupConfig := range oldNeighborGroupMap {
		_, exist := newNeighborGroupMap[groupId]
		if exist {
			continue
		}
		// 旧有新没有的group即为卸载的
		if !world.IsMultiplayerWorld() {
			// 单人世界直接卸载group
			g.RemoveSceneGroup(player, scene, groupConfig)
		} else {
			// 多人世界group附近没有任何玩家则卸载
			_, exist = otherPlayerNeighborGroupMap[uint32(groupConfig.Id)]
			if !exist {
				g.RemoveSceneGroup(player, scene, groupConfig)
			}
		}
	}
	for groupId, groupConfig := range newNeighborGroupMap {
		_, exist := oldNeighborGroupMap[groupId]
		if exist {
			continue
		}
		// 新有旧没有的group即为加载的
		g.AddSceneGroup(player, scene, groupConfig)
	}
	// 消失和出现的场景实体
	oldVisionEntityMap := g.GetVisionEntity(scene, oldPos)
	newVisionEntityMap := g.GetVisionEntity(scene, newPos)
	delEntityIdList := make([]uint32, 0)
	for entityId := range oldVisionEntityMap {
		_, exist := newVisionEntityMap[entityId]
		if exist {
			continue
		}
		// 旧有新没有的实体即为消失的
		delEntityIdList = append(delEntityIdList, entityId)
	}
	addEntityIdList := make([]uint32, 0)
	for entityId := range newVisionEntityMap {
		_, exist := oldVisionEntityMap[entityId]
		if exist {
			continue
		}
		// 新有旧没有的实体即为出现的
		addEntityIdList = append(addEntityIdList, entityId)
	}
	// 同步客户端消失和出现的场景实体
	if len(delEntityIdList) > 0 {
		g.RemoveSceneEntityNotifyToPlayer(player, proto.VisionType_VISION_MISS, delEntityIdList)
		for _, delEntityId := range delEntityIdList {
			entity := scene.GetEntity(delEntityId)
			avatarEntity, ok := entity.(*AvatarEntity)
			if !ok {
				continue
			}
			otherPlayer := USER_MANAGER.GetOnlineUser(avatarEntity.GetUid())
			if otherPlayer == nil {
				logger.Error("get player is nil, target uid: %v, uid: %v", avatarEntity.GetUid(), player.PlayerId)
				continue
			}
			g.RemoveSceneEntityNotifyToPlayer(otherPlayer, proto.VisionType_VISION_MISS, []uint32{avatarEntityId})
		}
	}
	if len(addEntityIdList) > 0 {
		g.AddSceneEntityNotify(player, proto.VisionType_VISION_MEET, addEntityIdList, false, false)
		for _, addEntityId := range addEntityIdList {
			entity := scene.GetEntity(addEntityId)
			avatarEntity, ok := entity.(*AvatarEntity)
			if !ok {
				continue
			}
			otherPlayer := USER_MANAGER.GetOnlineUser(avatarEntity.GetUid())
			if otherPlayer == nil {
				logger.Error("get player is nil, target uid: %v, uid: %v", avatarEntity.GetUid(), player.PlayerId)
				continue
			}
			sceneEntityInfoAvatar := g.PacketSceneEntityInfoAvatar(scene, player, scene.GetEntity(avatarEntityId).(*AvatarEntity).GetAvatarId())
			g.AddSceneEntityNotifyToPlayer(otherPlayer, proto.VisionType_VISION_MEET, []*proto.SceneEntityInfo{sceneEntityInfoAvatar})
		}
	}
	// 场景区域触发器检测
	g.SceneRegionTriggerCheck(player, oldPos, newPos, avatarEntityId)
}

// AiWorldAoiPlayerMove AI世界（PUBG玩法）玩家跨格子的 AOI 处理
//
// 与普通世界 SceneBlockAoiPlayerMove 区别：
//   - AI世界用 3D 格子（120×12×120）而非 2D（普通世界 1024×1024）
//   - 不卸载/加载 group（因为 AI 世界没用 group 系统 实体是动态创建的）
//   - 重点是处理玩家间互相可见性：跨格子时通知双方对方角色实体的出现/消失
//   - 同时把玩家本身从老格子的 AOI 对象列表中移除 加到新格子
//
// 这套机制让 AI 世界（无 4 人上限）能支持几十/上百人同屏 仅渲染附近格子的玩家
func (g *Game) AiWorldAoiPlayerMove(player *model.Player, world *World, scene *Scene, oldPos *model.Vector, newPos *model.Vector) {
	aiWorldAoi := world.GetAiWorldAoi()
	oldGid := aiWorldAoi.GetGidByPos(float32(oldPos.X), float32(oldPos.Y), float32(oldPos.Z))
	newGid := aiWorldAoi.GetGidByPos(float32(newPos.X), float32(newPos.Y), float32(newPos.Z))
	if oldGid != newGid {
		// 玩家跨越了格子
		logger.Debug("player cross ai world aoi grid, oldGid: %v, oldPos: %+v, newGid: %v, newPos: %+v, uid: %v",
			oldGid, oldPos, newGid, newPos, player.PlayerId)
		// 找出本次移动所带来的消失和出现的格子
		oldGridList := aiWorldAoi.GetSurrGridListByGid(oldGid, 1)
		newGridList := aiWorldAoi.GetSurrGridListByGid(newGid, 1)
		delGridIdList := make([]uint32, 0)
		for _, oldGrid := range oldGridList {
			exist := false
			for _, newGrid := range newGridList {
				if oldGrid.GetGid() == newGrid.GetGid() {
					exist = true
					break
				}
			}
			if exist {
				continue
			}
			delGridIdList = append(delGridIdList, oldGrid.GetGid())
		}
		addGridIdList := make([]uint32, 0)
		for _, newGrid := range newGridList {
			exist := false
			for _, oldGrid := range oldGridList {
				if newGrid.GetGid() == oldGrid.GetGid() {
					exist = true
					break
				}
			}
			if exist {
				continue
			}
			addGridIdList = append(addGridIdList, newGrid.GetGid())
		}
		activeAvatarId := world.GetPlayerActiveAvatarId(player)
		activeWorldAvatar := world.GetPlayerWorldAvatar(player, activeAvatarId)
		// 老格子移除玩家
		logger.Debug("ai world aoi remove player, oldPos: %+v, uid: %v", oldPos, player.PlayerId)
		ok := aiWorldAoi.RemoveObjectFromGridByPos(int64(player.PlayerId), float32(oldPos.X), float32(oldPos.Y), float32(oldPos.Z))
		if !ok {
			logger.Error("ai world aoi remove player fail, uid: %v, pos: %+v", player.PlayerId, g.GetPlayerPos(player))
		}
		// 处理消失的格子
		for _, delGridId := range delGridIdList {
			// 通知自己 老格子里的其它玩家消失
			oldOtherWorldAvatarMap := aiWorldAoi.GetObjectListByGid(delGridId)
			delEntityIdList := make([]uint32, 0)
			for _, otherWorldAvatarAny := range oldOtherWorldAvatarMap {
				otherWorldAvatar := otherWorldAvatarAny.(*WorldAvatar)
				delEntityIdList = append(delEntityIdList, otherWorldAvatar.GetAvatarEntityId())
			}
			if len(delEntityIdList) > 0 {
				g.RemoveSceneEntityNotifyToPlayer(player, proto.VisionType_VISION_MISS, delEntityIdList)
			}
			// 通知老格子里的其它玩家 自己消失
			for otherPlayerId := range oldOtherWorldAvatarMap {
				otherPlayer := USER_MANAGER.GetOnlineUser(uint32(otherPlayerId))
				if otherPlayer == nil {
					logger.Error("get player is nil, target uid: %v, uid: %v", otherPlayerId, player.PlayerId)
					continue
				}
				g.RemoveSceneEntityNotifyToPlayer(otherPlayer, proto.VisionType_VISION_MISS, []uint32{activeWorldAvatar.GetAvatarEntityId()})
			}
		}
		// 处理出现的格子
		for _, addGridId := range addGridIdList {
			// 通知自己 新格子里的其他玩家出现
			newOtherWorldAvatarMap := aiWorldAoi.GetObjectListByGid(addGridId)
			addEntityIdList := make([]uint32, 0)
			for _, otherWorldAvatarAny := range newOtherWorldAvatarMap {
				otherWorldAvatar := otherWorldAvatarAny.(*WorldAvatar)
				addEntityIdList = append(addEntityIdList, otherWorldAvatar.GetAvatarEntityId())
			}
			if len(addEntityIdList) > 0 {
				g.AddSceneEntityNotify(player, proto.VisionType_VISION_MEET, addEntityIdList, false, false)
			}
			// 通知新格子里的其他玩家 自己出现
			for otherPlayerId := range newOtherWorldAvatarMap {
				otherPlayer := USER_MANAGER.GetOnlineUser(uint32(otherPlayerId))
				if otherPlayer == nil {
					logger.Error("get player is nil, target uid: %v, uid: %v", otherPlayerId, player.PlayerId)
					continue
				}
				sceneEntityInfoAvatar := g.PacketSceneEntityInfoAvatar(scene, player, activeAvatarId)
				g.AddSceneEntityNotifyToPlayer(otherPlayer, proto.VisionType_VISION_MEET, []*proto.SceneEntityInfo{sceneEntityInfoAvatar})
			}
		}
		// 新格子添加玩家
		logger.Debug("ai world aoi add player, newPos: %+v, uid: %v", newPos, player.PlayerId)
		ok = aiWorldAoi.AddObjectToGridByPos(int64(player.PlayerId), activeWorldAvatar, float32(newPos.X), float32(newPos.Y), float32(newPos.Z))
		if !ok {
			logger.Error("ai world aoi add player fail, uid: %v, pos: %+v", player.PlayerId, g.GetPlayerPos(player))
		}
	}
	// 消失和出现的场景实体
	oldVisionEntityMap := g.GetVisionEntity(scene, oldPos)
	newVisionEntityMap := g.GetVisionEntity(scene, newPos)
	delEntityIdList := make([]uint32, 0)
	for entityId, entity := range oldVisionEntityMap {
		_, ok := entity.(*AvatarEntity)
		if ok {
			continue
		}
		_, exist := newVisionEntityMap[entityId]
		if exist {
			continue
		}
		// 旧有新没有的实体即为消失的
		delEntityIdList = append(delEntityIdList, entityId)
	}
	addEntityIdList := make([]uint32, 0)
	for entityId, entity := range newVisionEntityMap {
		_, ok := entity.(*AvatarEntity)
		if ok {
			continue
		}
		_, exist := oldVisionEntityMap[entityId]
		if exist {
			continue
		}
		// 新有旧没有的实体即为出现的
		addEntityIdList = append(addEntityIdList, entityId)
	}
	// 同步客户端消失和出现的场景实体
	if len(delEntityIdList) > 0 {
		g.RemoveSceneEntityNotifyToPlayer(player, proto.VisionType_VISION_MISS, delEntityIdList)
	}
	if len(addEntityIdList) > 0 {
		g.AddSceneEntityNotify(player, proto.VisionType_VISION_MEET, addEntityIdList, false, false)
	}
}

func (g *Game) AbilityInvocationsNotify(player *model.Player, payloadMsg pb.Message) {
	ntf := payloadMsg.(*proto.AbilityInvocationsNotify)
	if player.SceneLoadState != model.SceneEnterDone {
		return
	}
	for _, entry := range ntf.Invokes {
		player.AbilityInvokeHandler.AddEntry(entry.ForwardType, entry)
	}
	for _, entry := range ntf.Invokes {
		g.handleAbilityInvoke(player, entry)
	}
}

func (g *Game) ClientAbilityInitFinishNotify(player *model.Player, payloadMsg pb.Message) {
	ntf := payloadMsg.(*proto.ClientAbilityInitFinishNotify)
	if player.SceneLoadState != model.SceneEnterDone {
		return
	}
	invokeHandler := model.NewInvokeHandler[proto.AbilityInvokeEntry]()
	for _, entry := range ntf.Invokes {
		invokeHandler.AddEntry(entry.ForwardType, entry)
	}
	DoForward[proto.AbilityInvokeEntry](player, invokeHandler,
		cmd.ClientAbilityInitFinishNotify, new(proto.ClientAbilityInitFinishNotify), "Invokes",
		ntf, []string{"EntityId"})
	for _, entry := range ntf.Invokes {
		g.handleAbilityInvoke(player, entry)
	}
}

func (g *Game) ClientAbilityChangeNotify(player *model.Player, payloadMsg pb.Message) {
	ntf := payloadMsg.(*proto.ClientAbilityChangeNotify)
	if player.SceneLoadState != model.SceneEnterDone {
		return
	}
	invokeHandler := model.NewInvokeHandler[proto.AbilityInvokeEntry]()
	for _, entry := range ntf.Invokes {
		invokeHandler.AddEntry(entry.ForwardType, entry)
	}
	DoForward[proto.AbilityInvokeEntry](player, invokeHandler,
		cmd.ClientAbilityChangeNotify, new(proto.ClientAbilityChangeNotify), "Invokes",
		ntf, []string{"IsInitHash", "EntityId"})
	for _, entry := range ntf.Invokes {
		g.handleAbilityInvoke(player, entry)
		world := WORLD_MANAGER.GetWorldById(player.WorldId)
		if world == nil {
			continue
		}
		worldAvatar := world.GetWorldAvatarByEntityId(entry.EntityId)
		if worldAvatar == nil {
			continue
		}
		switch entry.ArgumentType {
		case proto.AbilityInvokeArgument_ABILITY_META_ADD_NEW_ABILITY:
			abilityMetaAddAbility := new(proto.AbilityMetaAddAbility)
			err := pb.Unmarshal(entry.AbilityData, abilityMetaAddAbility)
			if err != nil {
				logger.Error("parse AbilityMetaAddAbility error: %v", err)
				continue
			}
			if abilityMetaAddAbility.Ability == nil {
				continue
			}
			worldAvatar.AddAbility(abilityMetaAddAbility.Ability)
		case proto.AbilityInvokeArgument_ABILITY_META_MODIFIER_CHANGE:
			abilityMetaModifierChange := new(proto.AbilityMetaModifierChange)
			err := pb.Unmarshal(entry.AbilityData, abilityMetaModifierChange)
			if err != nil {
				logger.Error("parse AbilityMetaModifierChange error: %v", err)
				continue
			}
			abilityAppliedModifier := &proto.AbilityAppliedModifier{
				ModifierLocalId:           abilityMetaModifierChange.ModifierLocalId,
				ParentAbilityName:         abilityMetaModifierChange.ParentAbilityName,
				ParentAbilityOverride:     abilityMetaModifierChange.ParentAbilityOverride,
				InstancedAbilityId:        entry.Head.InstancedAbilityId,
				InstancedModifierId:       entry.Head.InstancedModifierId,
				AttachedInstancedModifier: abilityMetaModifierChange.AttachedInstancedModifier,
				ApplyEntityId:             abilityMetaModifierChange.ApplyEntityId,
				IsAttachedParentAbility:   abilityMetaModifierChange.IsAttachedParentAbility,
				IsServerbuffModifier:      entry.Head.IsServerbuffModifier,
			}
			worldAvatar.AddModifier(abilityAppliedModifier)
		}
	}
}

// handleAbilityInvoke 处理单个 AbilityInvokeEntry（按 ArgumentType 分支）
//
// ArgumentType 字符串大致分 4 类：
//   - 含 "ACTION": 能力动作（释放技能、扣体力等）→ entity.AbilityAction(action)
//     · 当前仅实现 6 种 AbilityAction（详见 game_world_scene.go AvatarSkillStart/HealHP 等）
//   - 含 "MIXIN": 持续性能力混入 → entity.AbilityMixin(mixin)
//     · 当前仅实现 1 种 CostStaminaMixin（持续扣体力 如长按E）
//   - 含 "META": 元能力操作（添加能力/添加modifier/覆盖参数等）→ 维护实体的 abilityMap/modifierMap
//     · ABILITY_META_ADD_NEW_ABILITY: 给实体加新能力
//     · ABILITY_META_MODIFIER_CHANGE: modifier 增删
//     · ABILITY_META_REINIT_OVERRIDEMAP / OVERRIDE_PARAM: 覆盖能力参数（dynamicFloat 求值用）
//     · ABILITY_META_GLOBAL_FLOAT_VALUE: 全局动态值（如角色等级影响的伤害系数）
//
// **重要**：项目 Ability 系统大部分未实现 这里只是骨架 实际伤害计算/护盾/元素反应都没有
func (g *Game) handleAbilityInvoke(player *model.Player, entry *proto.AbilityInvokeEntry) {
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		return
	}
	scene := world.GetSceneById(player.GetSceneId())
	entity := scene.GetEntity(entry.EntityId)
	if entity == nil {
		return
	}
	// logger.Debug("[LocalAbilityInvoke] type: %v, localId: %v, entityId: %v, uid: %v", entry.ArgumentType, entry.Head.LocalId, entity.GetId(), player.PlayerId)
	if strings.Contains(entry.ArgumentType.String(), "ACTION") || entry.ArgumentType == proto.AbilityInvokeArgument_ABILITY_NONE {
		ability := entity.GetAbility(entry.Head.InstancedAbilityId)
		if ability == nil {
			logger.Error("get ability is nil, instancedAbilityId: %v, entityId: %v, uid: %v", entry.Head.InstancedAbilityId, entity.GetId(), player.PlayerId)
			return
		}
		abilityDataConfig := gdconf.GetAbilityDataByName(ability.abilityName)
		if abilityDataConfig == nil {
			logger.Error("get ability data config is nil, abilityName: %v", ability.abilityName)
			return
		}
		actionDataConfig := abilityDataConfig.GetActionDataByLocalId(entry.Head.LocalId)
		if actionDataConfig == nil {
			logger.Error("get action data config is nil, abilityName: %v, localId: %v", ability.abilityName, entry.Head.LocalId)
			return
		}
		entity.AbilityAction(ability, actionDataConfig, entity)
	} else if strings.Contains(entry.ArgumentType.String(), "MIXIN") {
		ability := entity.GetAbility(entry.Head.InstancedAbilityId)
		if ability == nil {
			logger.Error("get ability is nil, instancedAbilityId: %v, entityId: %v, uid: %v", entry.Head.InstancedAbilityId, entity.GetId(), player.PlayerId)
			return
		}
		abilityDataConfig := gdconf.GetAbilityDataByName(ability.abilityName)
		if abilityDataConfig == nil {
			logger.Error("get ability data config is nil, abilityName: %v", ability.abilityName)
			return
		}
		mixinDataConfig := abilityDataConfig.GetMixinDataByLocalId(entry.Head.LocalId)
		if mixinDataConfig == nil {
			logger.Error("get mixin data config is nil, abilityName: %v, localId: %v", ability.abilityName, entry.Head.LocalId)
			return
		}
		entity.AbilityMixin(ability, mixinDataConfig, entity)
	} else if strings.Contains(entry.ArgumentType.String(), "META") {
		switch entry.ArgumentType {
		case proto.AbilityInvokeArgument_ABILITY_META_ADD_NEW_ABILITY:
			addAbility := new(proto.AbilityMetaAddAbility)
			err := pb.Unmarshal(entry.AbilityData, addAbility)
			if err != nil {
				logger.Error("parse AbilityMetaAddAbility error: %v", err)
				return
			}
			abilityNameHash := addAbility.Ability.AbilityName.GetHash()
			abilityDataConfig := gdconf.GetAbilityDataByHash(abilityNameHash)
			if abilityDataConfig == nil {
				logger.Error("get abilityDataConfig is nil, abilityNameHash: %v, instancedAbilityId: %v, entityId: %v", abilityNameHash, addAbility.Ability.InstancedAbilityId, entity.GetId())
				return
			}
			entity.AddAbility(abilityDataConfig.AbilityName, addAbility.Ability.InstancedAbilityId)
		case proto.AbilityInvokeArgument_ABILITY_META_MODIFIER_CHANGE:
			modifierChange := new(proto.AbilityMetaModifierChange)
			err := pb.Unmarshal(entry.AbilityData, modifierChange)
			if err != nil {
				logger.Error("parse AbilityMetaModifierChange error: %v", err)
				return
			}
			if modifierChange.Action == proto.ModifierAction_ADDED {
				ability := entity.GetAbility(entry.Head.InstancedAbilityId)
				if ability == nil {
					logger.Error("get ability is nil, instancedAbilityId: %v, entityId: %v, uid: %v", entry.Head.InstancedAbilityId, entity.GetId(), player.PlayerId)
					return
				}
				entity.AddModifier(ability, entry.Head.InstancedModifierId, uint32(entry.Head.ModifierConfigLocalId))
			} else if modifierChange.Action == proto.ModifierAction_REMOVED {
				entity.RemoveModifier(entry.Head.InstancedModifierId)
			}
		case proto.AbilityInvokeArgument_ABILITY_META_REINIT_OVERRIDEMAP:
			reInitOverrideMap := new(proto.AbilityMetaReInitOverrideMap)
			err := pb.Unmarshal(entry.AbilityData, reInitOverrideMap)
			if err != nil {
				logger.Error("parse AbilityMetaReInitOverrideMap error: %v", err)
				return
			}
			ability := entity.GetAbility(entry.Head.InstancedAbilityId)
			if ability == nil {
				logger.Error("get ability is nil, instancedAbilityId: %v, entityId: %v, uid: %v", entry.Head.InstancedAbilityId, entity.GetId(), player.PlayerId)
				return
			}
			for _, abilityScalarValueEntry := range reInitOverrideMap.OverrideMap {
				if abilityScalarValueEntry.ValueType != proto.AbilityScalarType_ABILITY_SCALAR_TYPE_FLOAT {
					logger.Error("param type not support, type: %v, uid: %v", abilityScalarValueEntry.ValueType, player.PlayerId)
					return
				}
				key := abilityScalarValueEntry.Key.GetHash()
				value := abilityScalarValueEntry.Value.(*proto.AbilityScalarValueEntry_FloatValue).FloatValue
				ability.abilitySpecialOverrideMap[key] = value
			}
		case proto.AbilityInvokeArgument_ABILITY_META_OVERRIDE_PARAM:
			abilityScalarValueEntry := new(proto.AbilityScalarValueEntry)
			err := pb.Unmarshal(entry.AbilityData, abilityScalarValueEntry)
			if err != nil {
				logger.Error("parse AbilityScalarValueEntry error: %v", err)
				return
			}
			ability := entity.GetAbility(entry.Head.InstancedAbilityId)
			if ability == nil {
				logger.Error("get ability is nil, instancedAbilityId: %v, entityId: %v, uid: %v", entry.Head.InstancedAbilityId, entity.GetId(), player.PlayerId)
				return
			}
			if abilityScalarValueEntry.ValueType != proto.AbilityScalarType_ABILITY_SCALAR_TYPE_FLOAT {
				logger.Error("param type not support, type: %v, uid: %v", abilityScalarValueEntry.ValueType, player.PlayerId)
				return
			}
			key := abilityScalarValueEntry.Key.GetHash()
			value := abilityScalarValueEntry.Value.(*proto.AbilityScalarValueEntry_FloatValue).FloatValue
			ability.abilitySpecialOverrideMap[key] = value
		case proto.AbilityInvokeArgument_ABILITY_META_GLOBAL_FLOAT_VALUE:
			abilityScalarValueEntry := new(proto.AbilityScalarValueEntry)
			err := pb.Unmarshal(entry.AbilityData, abilityScalarValueEntry)
			if err != nil {
				logger.Error("parse AbilityScalarValueEntry error: %v", err)
				return
			}
			if abilityScalarValueEntry.ValueType != proto.AbilityScalarType_ABILITY_SCALAR_TYPE_FLOAT {
				// logger.Error("param type not support, type: %v, uid: %v", abilityScalarValueEntry.ValueType, player.PlayerId)
				return
			}
			key := abilityScalarValueEntry.Key.GetHash()
			value := abilityScalarValueEntry.Value.(*proto.AbilityScalarValueEntry_FloatValue).FloatValue
			dynamicValueMap := entity.GetDynamicValueMap()
			dynamicValueMap[key] = value
		}
	} else {
		logger.Error("???")
	}
}

func (g *Game) MassiveEntityElementOpBatchNotify(player *model.Player, payloadMsg pb.Message) {
	ntf := payloadMsg.(*proto.MassiveEntityElementOpBatchNotify)
	if player.SceneLoadState != model.SceneEnterDone {
		return
	}
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		return
	}
	scene := world.GetSceneById(player.GetSceneId())
	ntf.OpIdx = scene.GetMeeoIndex()
	scene.SetMeeoIndex(scene.GetMeeoIndex() + 1)
	g.SendToSceneA(scene, cmd.MassiveEntityElementOpBatchNotify, player.ClientSeq, ntf, 0)
}

func (g *Game) EvtDoSkillSuccNotify(player *model.Player, payloadMsg pb.Message) {
	ntf := payloadMsg.(*proto.EvtDoSkillSuccNotify)
	if player.SceneLoadState != model.SceneEnterDone {
		return
	}

	// 触发事件
	if PLUGIN_MANAGER.TriggerEvent(PluginEventIdEvtDoSkillSucc, &PluginEventEvtDoSkillSucc{
		PluginEvent: NewPluginEvent(),
		Player:      player,
		Ntf:         ntf,
	}) {
		return
	}

	// logger.Debug("EvtDoSkillSuccNotify: %+v", ntf)
	// 触发任务
	g.TriggerQuest(player, constant.QUEST_FINISH_COND_TYPE_SKILL, "", int32(ntf.SkillId))
	// 消耗元素能量
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		return
	}
	activeAvatarId := world.GetPlayerActiveAvatarId(player)
	dbAvatar := player.GetDbAvatar()
	avatar := dbAvatar.GetAvatarById(activeAvatarId)
	if avatar == nil {
		return
	}
	avatarSkillDataConfig := gdconf.GetAvatarEnergySkillConfig(avatar.SkillDepotId)
	if avatarSkillDataConfig == nil {
		return
	}
	if int32(ntf.SkillId) == avatarSkillDataConfig.AvatarSkillId {
		g.CostPlayerAvatarEnergy(player.PlayerId, activeAvatarId, 0, true)
	}
}

func (g *Game) EvtAvatarEnterFocusNotify(player *model.Player, payloadMsg pb.Message) {
	ntf := payloadMsg.(*proto.EvtAvatarEnterFocusNotify)
	if player.SceneLoadState != model.SceneEnterDone {
		return
	}
	// logger.Debug("EvtAvatarEnterFocusNotify: %+v", ntf)
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		return
	}
	scene := world.GetSceneById(player.GetSceneId())
	g.SendToSceneA(scene, cmd.EvtAvatarEnterFocusNotify, player.ClientSeq, ntf, 0)
}

func (g *Game) EvtAvatarUpdateFocusNotify(player *model.Player, payloadMsg pb.Message) {
	ntf := payloadMsg.(*proto.EvtAvatarUpdateFocusNotify)
	if player.SceneLoadState != model.SceneEnterDone {
		return
	}
	// logger.Debug("EvtAvatarUpdateFocusNotify: %+v", ntf)
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		return
	}
	scene := world.GetSceneById(player.GetSceneId())
	g.SendToSceneA(scene, cmd.EvtAvatarUpdateFocusNotify, player.ClientSeq, ntf, 0)
}

func (g *Game) EvtAvatarExitFocusNotify(player *model.Player, payloadMsg pb.Message) {
	ntf := payloadMsg.(*proto.EvtAvatarExitFocusNotify)
	if player.SceneLoadState != model.SceneEnterDone {
		return
	}
	// logger.Debug("EvtAvatarExitFocusNotify: %+v", ntf)
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		return
	}
	scene := world.GetSceneById(player.GetSceneId())
	g.SendToSceneA(scene, cmd.EvtAvatarExitFocusNotify, player.ClientSeq, ntf, 0)
}

func (g *Game) EvtEntityRenderersChangedNotify(player *model.Player, payloadMsg pb.Message) {
	ntf := payloadMsg.(*proto.EvtEntityRenderersChangedNotify)
	if player.SceneLoadState != model.SceneEnterDone {
		return
	}
	// logger.Debug("EvtEntityRenderersChangedNotify: %+v", ntf)
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		return
	}
	scene := world.GetSceneById(player.GetSceneId())
	g.SendToSceneA(scene, cmd.EvtEntityRenderersChangedNotify, player.ClientSeq, ntf, 0)
}

func (g *Game) EvtBulletDeactiveNotify(player *model.Player, payloadMsg pb.Message) {
	ntf := payloadMsg.(*proto.EvtBulletDeactiveNotify)
	if player.SceneLoadState != model.SceneEnterDone {
		return
	}
	// logger.Debug("EvtBulletDeactiveNotify: %+v", ntf)
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		return
	}
	scene := world.GetSceneById(player.GetSceneId())
	g.SendToSceneA(scene, cmd.EvtBulletDeactiveNotify, player.ClientSeq, ntf, 0)
}

// EvtBulletHitNotify 子弹命中通知
// 客户端报告"我的子弹打到了某个实体" 服务端转发给场景内其他玩家
// 触发 PluginEventIdEvtBulletHit 让 PUBG 插件介入做物理引擎命中判定+伤害结算
func (g *Game) EvtBulletHitNotify(player *model.Player, payloadMsg pb.Message) {
	ntf := payloadMsg.(*proto.EvtBulletHitNotify)
	if player.SceneLoadState != model.SceneEnterDone {
		return
	}
	// logger.Debug("EvtBulletHitNotify: %+v", ntf)
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		return
	}
	scene := world.GetSceneById(player.GetSceneId())
	g.SendToSceneA(scene, cmd.EvtBulletHitNotify, player.ClientSeq, ntf, 0)

	// 触发事件
	if PLUGIN_MANAGER.TriggerEvent(PluginEventIdEvtBulletHit, &PluginEventEvtBulletHit{
		PluginEvent: NewPluginEvent(),
		Player:      player,
		Ntf:         ntf,
	}) {
		return
	}
}

func (g *Game) EvtBulletMoveNotify(player *model.Player, payloadMsg pb.Message) {
	ntf := payloadMsg.(*proto.EvtBulletMoveNotify)
	if player.SceneLoadState != model.SceneEnterDone {
		return
	}
	// logger.Debug("EvtBulletMoveNotify: %+v", ntf)
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		return
	}
	scene := world.GetSceneById(player.GetSceneId())
	g.SendToSceneA(scene, cmd.EvtBulletMoveNotify, player.ClientSeq, ntf, 0)
}

// EvtCreateGadgetNotify 客户端创建物件通知（如玩家拉弓射出箭矢、放风元素技能产生的元素球）
// 创建 GadgetClientEntity（客户端发起的物件）+ 广播 SceneEntityAppearNotify 给场景内玩家
// 触发 PluginEventIdEvtCreateGadget 让 PUBG 插件接管：判断是否箭矢子弹 → 送物理引擎模拟轨迹+命中判定
func (g *Game) EvtCreateGadgetNotify(player *model.Player, payloadMsg pb.Message) {
	ntf := payloadMsg.(*proto.EvtCreateGadgetNotify)
	if player.SceneLoadState != model.SceneEnterDone {
		return
	}
	// logger.Debug("EvtCreateGadgetNotify: %+v", ntf)
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		return
	}
	scene := world.GetSceneById(player.GetSceneId())
	if ntf.InitPos == nil {
		return
	}
	gadgetClientEntity := scene.CreateEntityGadgetClient(
		ntf.EntityId,
		&model.Vector{X: float64(ntf.InitPos.X), Y: float64(ntf.InitPos.Y), Z: float64(ntf.InitPos.Z)},
		&model.Vector{X: float64(ntf.InitEulerAngles.X), Y: float64(ntf.InitEulerAngles.Y), Z: float64(ntf.InitEulerAngles.Z)},
		ntf.ConfigId,
	)
	gadgetClientEntity.CreateGadgetClientEntity(ntf.CampId, ntf.CampType, ntf.OwnerEntityId, ntf.TargetEntityId, ntf.PropOwnerEntityId)
	scene.CreateEntity(gadgetClientEntity)
	g.AddSceneEntityNotify(player, proto.VisionType_VISION_BORN, []uint32{ntf.EntityId}, true, true)

	// 触发事件
	if PLUGIN_MANAGER.TriggerEvent(PluginEventIdEvtCreateGadget, &PluginEventEvtCreateGadget{
		PluginEvent: NewPluginEvent(),
		Player:      player,
		Ntf:         ntf,
	}) {
		return
	}
}

func (g *Game) EvtDestroyGadgetNotify(player *model.Player, payloadMsg pb.Message) {
	ntf := payloadMsg.(*proto.EvtDestroyGadgetNotify)
	if player.SceneLoadState != model.SceneEnterDone {
		return
	}
	// logger.Debug("EvtDestroyGadgetNotify: %+v", ntf)
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		return
	}
	scene := world.GetSceneById(player.GetSceneId())
	scene.DestroyEntity(ntf.EntityId)
	g.RemoveSceneEntityNotifyBroadcast(scene, proto.VisionType_VISION_MISS, []uint32{ntf.EntityId}, 0)
}

func (g *Game) EvtAiSyncSkillCdNotify(player *model.Player, payloadMsg pb.Message) {
	ntf := payloadMsg.(*proto.EvtAiSyncSkillCdNotify)
	if player.SceneLoadState != model.SceneEnterDone {
		return
	}
	// logger.Debug("EvtAiSyncSkillCdNotify: %+v", ntf)
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		return
	}
	scene := world.GetSceneById(player.GetSceneId())
	g.SendToSceneA(scene, cmd.EvtAiSyncSkillCdNotify, player.ClientSeq, ntf, 0)
}

func (g *Game) EvtAiSyncCombatThreatInfoNotify(player *model.Player, payloadMsg pb.Message) {
	ntf := payloadMsg.(*proto.EvtAiSyncCombatThreatInfoNotify)
	if player.SceneLoadState != model.SceneEnterDone {
		return
	}
	// logger.Debug("EvtAiSyncCombatThreatInfoNotify: %+v", ntf)
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		return
	}
	scene := world.GetSceneById(player.GetSceneId())
	g.SendToSceneA(scene, cmd.EvtAiSyncCombatThreatInfoNotify, player.ClientSeq, ntf, 0)
}

func (g *Game) EntityConfigHashNotify(player *model.Player, payloadMsg pb.Message) {
	ntf := payloadMsg.(*proto.EntityConfigHashNotify)
	_ = ntf
}

func (g *Game) MonsterAIConfigHashNotify(player *model.Player, payloadMsg pb.Message) {
	ntf := payloadMsg.(*proto.MonsterAIConfigHashNotify)
	_ = ntf
}

func (g *Game) SetEntityClientDataNotify(player *model.Player, payloadMsg pb.Message) {
	ntf := payloadMsg.(*proto.SetEntityClientDataNotify)
	g.SendMsg(cmd.SetEntityClientDataNotify, player.PlayerId, player.ClientSeq, ntf)
}

func (g *Game) EntityAiSyncNotify(player *model.Player, payloadMsg pb.Message) {
	ntf := payloadMsg.(*proto.EntityAiSyncNotify)
	entityAiSyncNotify := &proto.EntityAiSyncNotify{
		InfoList: make([]*proto.AiSyncInfo, 0),
	}
	for _, monsterId := range ntf.LocalAvatarAlertedMonsterList {
		entityAiSyncNotify.InfoList = append(entityAiSyncNotify.InfoList, &proto.AiSyncInfo{
			EntityId:        monsterId,
			HasPathToTarget: true,
			IsSelfKilling:   false,
		})
	}
	g.SendMsg(cmd.EntityAiSyncNotify, player.PlayerId, player.ClientSeq, entityAiSyncNotify)
}

func (g *Game) SceneAudioNotify(player *model.Player, payloadMsg pb.Message) {
	ntf := payloadMsg.(*proto.SceneAudioNotify)
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		return
	}
	scene := world.GetSceneById(player.GetSceneId())
	g.SendToSceneA(scene, cmd.SceneAudioNotify, player.ClientSeq, ntf, 0)
}
