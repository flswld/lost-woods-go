package game

import (
	"encoding/base64"

	"hk4e/common/constant"
	"hk4e/gdconf"
	"hk4e/gs/model"
	"hk4e/pkg/random"
	"hk4e/protocol/cmd"
	"hk4e/protocol/proto"

	"github.com/flswld/halo/logger"
	"google.golang.org/protobuf/encoding/protojson"
)

// GMCmd GM 函数集合（系统函数 GM + HTTP 后台 + 客户端 GmTalk @@ 格式 三种入口共用）
//
// 与 game_command_controller.go 的玩家聊天命令的关系：
//   - controller 是给玩家用的（通过私聊"小可爱"输入"item add 1234 5"等）
//     底层调 GMCmd.GMAddItem 等方法
//   - GMCmd 是给开发者/运维用的 通过反射调用（CallGMCmd）
//     方法名约定 GMxxx 参数只能是基本类型（int/uint8/float64/bool/string）
//
// 命名规则：
//   - GMxxx：常规 GM 命令（操作玩家档/场景）
//   - 无前缀：特殊用途（如 ChangePlayerCmdPerm/ReloadGameDataConfig/CreateRobotInAiWorld 等）
//
// 调用示例：
//   - HTTP: POST /gm/cmd { funcName: "GMAddItem", paramList: ["100000001", "1234", "5"] }
//   - 客户端 GmTalk: "@@GMAddItem(100000001,1234,5)"
//
// **GMClearItem 等危险操作**：会调 LogoutPlayer 强制玩家下线（避免数据不一致）
//   清掉 DbItem 后强制重登 让玩家重新加载干净的存档

// GM函数模块
// GM函数只支持基本类型的简单参数传入

type GMCmd struct {
}

// 玩家通用GM指令

// GMTeleportPlayer 传送玩家
func (g *GMCmd) GMTeleportPlayer(userId, sceneId uint32, posX, posY, posZ float64) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	GAME.TeleportPlayer(
		player,
		proto.EnterReason_ENTER_REASON_GM,
		sceneId,
		&model.Vector{X: posX, Y: posY, Z: posZ},
		new(model.Vector),
		0,
		0,
	)
}

// GMAddItem 添加道具
func (g *GMCmd) GMAddItem(userId, itemId, itemCount uint32) {
	GAME.AddPlayerItem(userId, []*ChangeItem{{ItemId: itemId, ChangeCount: itemCount}}, proto.ActionReasonType_ACTION_REASON_GM)
}

// GMAddAllItem 添加所有道具
func (g *GMCmd) GMAddAllItem(userId uint32, itemCount uint32) {
	GAME.LogoutPlayer(userId)
	itemList := make([]*ChangeItem, 0)
	for itemId := range GAME.GetAllItemDataConfig() {
		itemList = append(itemList, &ChangeItem{
			ItemId:      uint32(itemId),
			ChangeCount: itemCount,
		})
	}
	GAME.AddPlayerItem(userId, itemList, proto.ActionReasonType_ACTION_REASON_GM)
}

// GMCostItem 消耗道具
func (g *GMCmd) GMCostItem(userId, itemId, itemCount uint32) {
	GAME.CostPlayerItem(userId, []*ChangeItem{{ItemId: itemId, ChangeCount: itemCount}})
}

// GMClearItem 清除全部道具
func (g *GMCmd) GMClearItem(userId uint32) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	player.DbItem = nil
	GAME.LogoutPlayer(userId)
}

// GMAddWeapon 添加武器
func (g *GMCmd) GMAddWeapon(userId, itemId, itemCount uint32, level, promote, refinement uint8) {
	// 武器数量
	for i := uint32(0); i < itemCount; i++ {
		// 添加武器
		weaponId := GAME.AddPlayerWeapon(userId, itemId)
		// 获取玩家
		player := USER_MANAGER.GetOnlineUser(userId)
		if player == nil {
			logger.Error("player is nil, uid: %v", userId)
			return
		}
		// 获取武器
		weapon := player.GetDbWeapon().GetWeapon(weaponId)
		if weapon == nil {
			logger.Error("weapon is nil, weaponId: %v", weaponId)
			return
		}
		// 设置武器的突破等级
		weapon.Promote = promote
		// 设置武器等级
		weapon.Level = level
		weapon.Exp = 0
		// 设置武器精炼
		weapon.Refinement = refinement
		// 道具背包更新
		GAME.SendMsg(cmd.StoreItemChangeNotify, player.PlayerId, player.ClientSeq, GAME.PacketStoreItemChangeNotifyByWeapon(weapon))
	}
}

// GMAddAllWeapon 添加所有武器
func (g *GMCmd) GMAddAllWeapon(userId, itemCount uint32, level, promote, refinement uint8) {
	for itemId := range GAME.GetAllWeaponDataConfig() {
		g.GMAddWeapon(userId, uint32(itemId), itemCount, level, promote, refinement)
	}
}

// GMClearWeapon 清除全部武器
func (g *GMCmd) GMClearWeapon(userId uint32) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	dbWeapon := player.GetDbWeapon()
	for _, weapon := range dbWeapon.GetWeaponMap() {
		if weapon.AvatarId == 0 {
			dbWeapon.CostWeapon(player, weapon.WeaponId)
		}
	}
	GAME.LogoutPlayer(userId)
}

// GMAddReliquary 添加圣遗物
func (g *GMCmd) GMAddReliquary(userId, itemId, itemCount, mainPropId uint32, appendPropIdList []uint32) {
	// 圣遗物数量
	for i := uint32(0); i < itemCount; i++ {
		// 添加圣遗物
		GAME.AddPlayerReliquary(userId, itemId, mainPropId, appendPropIdList)
	}
}

// GMAddAllReliquary 添加所有圣遗物
func (g *GMCmd) GMAddAllReliquary(userId, itemCount uint32) {
	GAME.LogoutPlayer(userId)
	for itemId := range GAME.GetAllReliquaryDataConfig() {
		g.GMAddReliquary(userId, uint32(itemId), itemCount, 0, nil)
	}
}

// GMClearReliquary 清除全部圣遗物
func (g *GMCmd) GMClearReliquary(userId uint32) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	dbReliquary := player.GetDbReliquary()
	for _, reliquary := range dbReliquary.GetReliquaryMap() {
		if reliquary.AvatarId == 0 {
			dbReliquary.CostReliquary(player, reliquary.ReliquaryId)
		}
	}
	GAME.LogoutPlayer(userId)
}

// GMAddAvatar 添加角色
func (g *GMCmd) GMAddAvatar(userId, avatarId uint32, level, promote uint8) {
	// 添加角色
	GAME.AddPlayerAvatar(userId, avatarId)
	// 获取玩家
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	// 获取角色
	avatar := player.GetDbAvatar().GetAvatarById(avatarId)
	if avatar == nil {
		logger.Error("avatar not exist, avatarId: %v", avatarId)
		return
	}
	// 修正角色属性
	avatar.Level = level
	avatar.Promote = promote
	GAME.AddPlayerAvatarHp(player.PlayerId, avatarId, 0.0, 1.0, proto.ChangHpReason_CHANGE_HP_ADD_GM)
	// 角色更新面板
	GAME.UpdatePlayerAvatarFightProp(player.PlayerId, avatar.AvatarId)
	// 角色属性表更新通知
	GAME.SendMsg(cmd.AvatarPropNotify, player.PlayerId, player.ClientSeq, GAME.PacketAvatarPropNotify(avatar))
}

// GMAddAllAvatar 添加所有角色
func (g *GMCmd) GMAddAllAvatar(userId uint32, level, promote uint8) {
	for avatarId := range GAME.GetAllAvatarDataConfig() {
		g.GMAddAvatar(userId, uint32(avatarId), level, promote)
	}
}

// GMDelAvatar 删除角色
func (g *GMCmd) GMDelAvatar(userId, avatarId uint32) {
	GAME.DelPlayerAvatar(userId, avatarId)
}

// GMDelAllAvatar 删除所有角色
func (g *GMCmd) GMDelAllAvatar(userId uint32) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	dbAvatar := player.GetDbAvatar()
	for _, avatar := range dbAvatar.GetAvatarMap() {
		g.GMDelAvatar(userId, avatar.AvatarId)
	}
}

// GMKillSelf 杀死自己
func (g *GMCmd) GMKillSelf(userId uint32) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		logger.Error("world is nil, worldId: %v, uid: %v", player.WorldId, player.PlayerId)
		return
	}
	// 杀死当前活跃角色
	activeAvatarId := world.GetPlayerActiveAvatarId(player)
	GAME.SubPlayerAvatarHp(player.PlayerId, activeAvatarId, 0.0, 1.0, proto.ChangHpReason_CHANGE_HP_SUB_GM)
}

// GMKillMonster 杀死指定怪物
func (g *GMCmd) GMKillMonster(userId uint32, entityId uint32) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		logger.Error("world is nil, worldId: %v, uid: %v", player.WorldId, player.PlayerId)
		return
	}
	scene := world.GetSceneById(player.GetSceneId())
	if scene == nil {
		logger.Error("scene is nil, sceneId: %v, uid: %v", player.GetSceneId(), player.PlayerId)
		return
	}
	// 获取实体
	entity := scene.GetEntity(entityId)
	if entity == nil {
		logger.Error("entity is nil, entityId: %v, uid: %v", entityId, player.PlayerId)
		return
	}
	// 确保为怪物
	_, ok := entity.(*MonsterEntity)
	if !ok {
		return
	}
	// 杀死怪物
	GAME.SubEntityHp(player, scene, entity.GetId(), 0.0, 1.0, proto.ChangHpReason_CHANGE_HP_SUB_GM)
}

// GMKillAllMonster 杀死所有怪物
func (g *GMCmd) GMKillAllMonster(userId uint32) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		logger.Error("world is nil, worldId: %v, uid: %v", player.WorldId, player.PlayerId)
		return
	}
	scene := world.GetSceneById(player.GetSceneId())
	if scene == nil {
		logger.Error("scene is nil, sceneId: %v, uid: %v", player.GetSceneId(), player.PlayerId)
		return
	}
	// 杀死视野内所有怪物实体
	for _, entity := range GAME.GetVisionEntity(scene, GAME.GetPlayerPos(player)) {
		// 确保为怪物
		_, ok := entity.(*MonsterEntity)
		if !ok {
			continue
		}
		// 杀死怪物
		GAME.SubEntityHp(player, scene, entity.GetId(), 0.0, 1.0, proto.ChangHpReason_CHANGE_HP_SUB_GM)
	}
}

// GMAddQuest 添加任务
func (g *GMCmd) GMAddQuest(userId uint32, questId uint32) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	dbQuest := player.GetDbQuest()
	dbQuest.AddQuest(questId)
	dbQuest.StartQuest(questId)
	ntf := &proto.QuestListUpdateNotify{
		QuestList: make([]*proto.Quest, 0),
	}
	ntf.QuestList = append(ntf.QuestList, GAME.PacketQuest(player, questId))
	GAME.SendMsg(cmd.QuestListUpdateNotify, player.PlayerId, player.ClientSeq, ntf)
}

// GMFinishQuest 完成任务
func (g *GMCmd) GMFinishQuest(userId uint32, questId uint32) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	dbQuest := player.GetDbQuest()
	dbQuest.ForceFinishQuest(questId)
	ntf := &proto.QuestListUpdateNotify{
		QuestList: make([]*proto.Quest, 0),
	}
	ntf.QuestList = append(ntf.QuestList, GAME.PacketQuest(player, questId))
	GAME.SendMsg(cmd.QuestListUpdateNotify, player.PlayerId, player.ClientSeq, ntf)
	GAME.AcceptQuest(player, true)
}

// GMForceFinishAllQuest 强制完成当前所有任务
func (g *GMCmd) GMForceFinishAllQuest(userId uint32) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	dbQuest := player.GetDbQuest()
	ntf := &proto.QuestListUpdateNotify{
		QuestList: make([]*proto.Quest, 0),
	}
	for _, quest := range dbQuest.GetQuestMap() {
		dbQuest.ForceFinishQuest(quest.QuestId)
		pbQuest := GAME.PacketQuest(player, quest.QuestId)
		if pbQuest == nil {
			continue
		}
		ntf.QuestList = append(ntf.QuestList, pbQuest)
	}
	GAME.SendMsg(cmd.QuestListUpdateNotify, player.PlayerId, player.ClientSeq, ntf)
}

// GMClearQuest 清除全部任务
func (g *GMCmd) GMClearQuest(userId uint32) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	player.DbQuest = nil
	player.SceneId = 3
	player.Pos = &model.Vector{X: 2747, Y: 194, Z: -1719}
	player.Rot = &model.Vector{X: 0, Y: 307, Z: 0}
	GAME.AcceptQuest(player, false)
	GAME.LogoutPlayer(userId)
}

// GMUnlockPoint 解锁场景锚点
func (g *GMCmd) GMUnlockPoint(userId uint32, sceneId uint32, pointId uint32) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	GAME.UnlockPlayerScenePoint(player, sceneId, pointId)
}

// GMUnlockAllPoint 解锁场景全部锚点
func (g *GMCmd) GMUnlockAllPoint(userId uint32, sceneId uint32) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	dbWorld := player.GetDbWorld()
	dbScene := dbWorld.GetSceneById(sceneId)
	if dbScene == nil {
		logger.Error("db scene is nil, sceneId: %v, uid: %v", sceneId, userId)
		return
	}
	scenePointMapConfig := gdconf.GetScenePointMapBySceneId(int32(sceneId))
	if scenePointMapConfig == nil {
		logger.Error("scene point config is nil, sceneId: %v", sceneId)
		return
	}
	for _, pointData := range scenePointMapConfig {
		dbScene.UnlockPoint(uint32(pointData.Id))
	}
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		return
	}
	scene := world.GetSceneById(player.SceneId)
	GAME.SendToSceneA(scene, cmd.ScenePointUnlockNotify, player.ClientSeq, &proto.ScenePointUnlockNotify{
		SceneId:   sceneId,
		PointList: dbScene.GetUnlockPointList(),
	}, 0)
}

// GMUnlockArea 解锁场景区域
func (g *GMCmd) GMUnlockArea(userId uint32, sceneId uint32, areaId uint32) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	GAME.UnlockPlayerSceneArea(player, sceneId, areaId)
}

// GMUnlockAllArea 解锁场景全部区域
func (g *GMCmd) GMUnlockAllArea(userId uint32, sceneId uint32) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	dbWorld := player.GetDbWorld()
	dbScene := dbWorld.GetSceneById(sceneId)
	if dbScene == nil {
		logger.Error("db scene is nil, sceneId: %v, uid: %v", sceneId, userId)
		return
	}
	for _, worldAreaDataConfig := range gdconf.GetWorldAreaDataMap() {
		dbScene.UnlockArea(uint32(worldAreaDataConfig.AreaId1))
	}
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		return
	}
	scene := world.GetSceneById(player.SceneId)
	GAME.SendToSceneA(scene, cmd.SceneAreaUnlockNotify, player.ClientSeq, &proto.SceneAreaUnlockNotify{
		SceneId:  sceneId,
		AreaList: dbScene.GetUnlockAreaList(),
	}, 0)
}

// GMSetWeather 设置天气
func (g *GMCmd) GMSetWeather(userId uint32, climateType uint32) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	GAME.SetPlayerWeather(player, player.WeatherInfo.WeatherAreaId, climateType, true)
}

// GMCreateMonster 在玩家附近创建怪物
func (g *GMCmd) GMCreateMonster(userId uint32, monsterId uint32, posX, posY, posZ float64, count uint32, level uint8) {
	if monsterId == 0 {
		for _, monsterData := range gdconf.GetMonsterDataMap() {
			monsterId = uint32(monsterData.MonsterId)
			break
		}
	}
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		logger.Error("world is nil, worldId: %v, uid: %v", player.WorldId, player.PlayerId)
		return
	}
	scene := world.GetSceneById(player.GetSceneId())
	if scene == nil {
		logger.Error("scene is nil, sceneId: %v, uid: %v", player.GetSceneId(), player.PlayerId)
		return
	}
	if count > 100 {
		logger.Error("monster count too large, uid: %v", userId)
		return
	}
	for i := 0; i < int(count); i++ {
		GAME.CreateMonster(player, &model.Vector{
			X: posX,
			Y: posY,
			Z: posZ,
		}, monsterId, level)
	}
}

// GMCreateGadget 在玩家附近创建物件
func (g *GMCmd) GMCreateGadget(userId uint32, gadgetId uint32, count uint32) {
	if gadgetId == 0 {
		for _, gadgetData := range gdconf.GetGadgetDataMap() {
			gadgetId = uint32(gadgetData.GadgetId)
			break
		}
	}
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	if count > 100 {
		logger.Error("gadget count too large, uid: %v", userId)
		return
	}
	for i := 0; i < int(count); i++ {
		GAME.CreateGadget(player, nil, gadgetId)
	}
}

// GMClearPlayer 清除账号数据
func (g *GMCmd) GMClearPlayer(userId uint32) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	player.OfflineClear = true
	GAME.LogoutPlayer(userId)
}

// GMClearWorld 清除大世界数据
func (g *GMCmd) GMClearWorld(userId uint32) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	player.DbWorld = nil
	GAME.LogoutPlayer(userId)
}

// GMNotSave 离线回档
func (g *GMCmd) GMNotSave(userId uint32) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	player.NotSave = true
}

// GMSetOpenState 设置功能开放状态
func (g *GMCmd) GMSetOpenState(userId uint32, openStateId uint32, value uint32) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	player.OpenStateMap[openStateId] = value
}

// GMSetAllOpenState 设置全部功能开放状态
func (g *GMCmd) GMSetAllOpenState(userId uint32, value uint32) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	for _, openStateData := range gdconf.GetOpenStateDataMap() {
		player.OpenStateMap[uint32(openStateData.OpenStateId)] = value
	}
	GAME.LogoutPlayer(userId)
}

// GMAddAllSceneTag 解锁全部场景标签
func (g *GMCmd) GMAddAllSceneTag(userId uint32, sceneId uint32) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	dbWorld := player.GetDbWorld()
	dbScene := dbWorld.GetSceneById(sceneId)
	if dbScene == nil {
		logger.Error("db scene is nil, sceneId: %v, uid: %v", sceneId, userId)
		return
	}
	for _, sceneTagDataConfig := range gdconf.GetSceneTagDataMap() {
		if uint32(sceneTagDataConfig.SceneId) == sceneId {
			dbScene.AddSceneTag(uint32(sceneTagDataConfig.SceneTagId))
		}
	}
	if sceneId == player.GetSceneId() {
		GAME.SendMsg(cmd.SceneDataNotify, player.PlayerId, player.ClientSeq, &proto.SceneDataNotify{
			LevelConfigNameList: nil,
			SceneTagIdList:      dbScene.GetSceneTagList(),
		})
	}
}

// GMFreeMode 自由探索模式（一键开放全图）
//
// 开启的内容：
//   - 允许飞行（PROP_IS_FLYABLE=1）
//   - 允许传送（PROP_IS_TRANSFERABLE=1）
//   - 解除天气/时间锁
//   - 允许潜水 + 满潜水耐力
//   - 开启多人模式（PROP_IS_MP_MODE_AVAILABLE=1）
//   - 解锁鲜肉/全球区域 + 多人世界 OpenState
//   - 解锁场景 ID=3（须弥/枫丹等？）的全部传送点 + 区域
//
// 用于"我只想四处看看不打主线"的场景 一键解锁所有探索能力
func (g *GMCmd) GMFreeMode(userId uint32) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}

	player.PropMap[constant.PLAYER_PROP_IS_FLYABLE] = 1
	player.PropMap[constant.PLAYER_PROP_IS_TRANSFERABLE] = 1
	player.PropMap[constant.PLAYER_PROP_IS_WEATHER_LOCKED] = 0
	player.PropMap[constant.PLAYER_PROP_IS_GAME_TIME_LOCKED] = 0
	player.PropMap[constant.PLAYER_PROP_PLAYER_CAN_DIVE] = 1
	player.PropMap[constant.PLAYER_PROP_DIVE_MAX_STAMINA] = 10000
	player.PropMap[constant.PLAYER_PROP_DIVE_CUR_STAMINA] = 10000
	player.PropMap[constant.PLAYER_PROP_IS_MP_MODE_AVAILABLE] = 1
	GAME.SendMsg(cmd.PlayerPropNotify, userId, player.ClientSeq, GAME.PacketPlayerPropNotify(player))

	GAME.ChangePlayerOpenState(player.PlayerId, constant.OPEN_STATE_LIMIT_REGION_FRESHMEAT, 1)
	GAME.ChangePlayerOpenState(player.PlayerId, constant.OPEN_STATE_LIMIT_REGION_GLOBAL, 1)
	GAME.ChangePlayerOpenState(player.PlayerId, constant.OPEN_STATE_MULTIPLAYER, 1)

	g.GMUnlockAllArea(userId, 3)
	g.GMUnlockAllPoint(userId, 3)
}

// GMChangeSkillDepot 切换当前角色技能库
func (g *GMCmd) GMChangeSkillDepot(userId uint32, skillDepotId uint32) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		logger.Error("world is nil, worldId: %v, uid: %v", player.WorldId, player.PlayerId)
		return
	}
	GAME.ChangePlayerAvatarSkillDepot(player.PlayerId, world.GetPlayerActiveAvatarId(player), skillDepotId, 0)
}

// GMSetPlayerWuDi 开启关闭角色无敌
func (g *GMCmd) GMSetPlayerWuDi(userId uint32, open bool) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	player.WuDi = open
}

// GMSetMonsterWudi 开启关闭场景内怪物无敌
func (g *GMCmd) GMSetMonsterWudi(userId uint32, open bool) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		return
	}
	scene := world.GetSceneById(player.GetSceneId())
	scene.SetMonsterWudi(open)
}

// GMSetPlayerEnergyInf 开启关闭角色无限能量
func (g *GMCmd) GMSetPlayerEnergyInf(userId uint32, open bool) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	player.EnergyInf = open
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		logger.Error("world is nil, worldId: %v, uid: %v", player.WorldId, player.PlayerId)
		return
	}
	for _, worldAvatar := range world.GetPlayerWorldAvatarList(player) {
		GAME.AddPlayerAvatarEnergy(player.PlayerId, worldAvatar.GetAvatarId(), 0.0, true)
	}
}

// GMSetPlayerStaminaInf 开启关闭角色无限耐力
func (g *GMCmd) GMSetPlayerStaminaInf(userId uint32, open bool) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	player.StaminaInf = open
}

// GMSetPlayerNoCd 开启关闭角色无冷却
func (g *GMCmd) GMSetPlayerNoCd(userId uint32, open bool) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	player.NoCd = open
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		logger.Error("world is nil, worldId: %v, uid: %v", player.WorldId, player.PlayerId)
		return
	}
	for _, worldAvatar := range world.GetPlayerWorldAvatarList(player) {
		GAME.UpdatePlayerAvatarFightProp(player.PlayerId, worldAvatar.GetAvatarId())
	}
}

// GMSetTalentUnlock 解锁锁定角色命座
func (g *GMCmd) GMSetTalentUnlock(userId uint32, talentId uint32, unlock bool) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		logger.Error("world is nil, worldId: %v, uid: %v", player.WorldId, player.PlayerId)
		return
	}
	activeAvatarId := world.GetPlayerActiveAvatarId(player)
	dbAvatar := player.GetDbAvatar()
	avatar := dbAvatar.GetAvatarById(activeAvatarId)
	if talentId == 0 {
		if unlock {
			avatarSkillDepotDataConfig := gdconf.GetAvatarSkillDepotDataById(int32(avatar.SkillDepotId))
			if avatarSkillDepotDataConfig == nil {
				logger.Error("avatar skill depot data config is nil, skillDepotId: %v", avatar.SkillDepotId)
				return
			}
			avatar.TalentIdList = make([]uint32, 0)
			entityId := world.GetPlayerWorldAvatarEntityId(player, avatar.AvatarId)
			for _, v := range avatarSkillDepotDataConfig.Talents {
				avatar.TalentIdList = append(avatar.TalentIdList, uint32(v))
				ntf := &proto.AvatarUnlockTalentNotify{
					EntityId:     entityId,
					AvatarGuid:   avatar.Guid,
					TalentId:     uint32(v),
					SkillDepotId: avatar.SkillDepotId,
				}
				GAME.SendMsg(cmd.AvatarUnlockTalentNotify, player.PlayerId, player.ClientSeq, ntf)
			}
		} else {
			avatar.TalentIdList = make([]uint32, 0)
			GAME.LogoutPlayer(userId)
		}
	} else {
		if unlock {
			for _, v := range avatar.TalentIdList {
				if v == talentId {
					return
				}
			}
			entityId := world.GetPlayerWorldAvatarEntityId(player, avatar.AvatarId)
			avatar.TalentIdList = append(avatar.TalentIdList, talentId)
			ntf := &proto.AvatarUnlockTalentNotify{
				EntityId:     entityId,
				AvatarGuid:   avatar.Guid,
				TalentId:     talentId,
				SkillDepotId: avatar.SkillDepotId,
			}
			GAME.SendMsg(cmd.AvatarUnlockTalentNotify, player.PlayerId, player.ClientSeq, ntf)
		} else {
			newTalentIdList := make([]uint32, 0)
			for _, v := range avatar.TalentIdList {
				if v == talentId {
					continue
				}
				newTalentIdList = append(newTalentIdList, v)
			}
			avatar.TalentIdList = newTalentIdList
			GAME.LogoutPlayer(userId)
		}
	}
}

// GMSetPlayerLevelExp 设置玩家冒险等级与经验
func (g *GMCmd) GMSetPlayerLevelExp(userId uint32, level uint32, exp uint32) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	GAME.SetPlayerLevelExp(userId, level, exp)
}

// GMSetPlayerAvatarLevelExp 设置玩家当前角色等级经验
func (g *GMCmd) GMSetPlayerAvatarLevelExp(userId uint32, level uint8, exp uint32) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		logger.Error("world is nil, worldId: %v, uid: %v", player.WorldId, player.PlayerId)
		return
	}
	activeAvatarId := world.GetPlayerActiveAvatarId(player)
	GAME.SetPlayerAvatarLevelExpPromote(userId, activeAvatarId, level, exp)
}

// GMSetPlayerAvatarPromote 设置玩家当前角色突破
func (g *GMCmd) GMSetPlayerAvatarPromote(userId uint32, promote uint8) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		logger.Error("world is nil, worldId: %v, uid: %v", player.WorldId, player.PlayerId)
		return
	}
	activeAvatarId := world.GetPlayerActiveAvatarId(player)
	GAME.SetPlayerAvatarLevelExpPromote(userId, activeAvatarId, 0, 0, promote)
}

// 系统级GM指令

// ChangePlayerCmdPerm 修改玩家命令权限等级（让普通玩家成为 GM）
// 使用场景：服主想给某玩家临时 GM 权限做测试 调一次后该玩家就能用 GM 命令了
// 权限级别：CommandPermNormal(0) → CommandPermGM(1)
func (g *GMCmd) ChangePlayerCmdPerm(userId uint32, cmdPerm uint8) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	player.CmdPerm = cmdPerm
}

// ReloadGameDataConfig 热重载游戏数据配置（不停服更新配置）
// 通过 LocalEvent 异步处理：goroutine 加载新配置到 CONF_RELOAD → 主循环原子替换
// reloadSceneLua=true 同时重载场景 Lua（耗时较长 数万 group lua 文件全部重新解析）
func (g *GMCmd) ReloadGameDataConfig(reloadSceneLua bool) {
	LOCAL_EVENT_MANAGER.GetLocalEventChan() <- &LocalEvent{
		EventId: ReloadGameDataConfig,
		Msg:     reloadSceneLua,
	}
}

// XLuaDebug 玩家客户端远程执行 Lua bytecode（详见 CLAUDE.md "客户端 Lua 远程执行"）
//
// **危险能力**：客户端 XLua 权限非常高 能直接操作 UI 树/调用 C# API
// 玩家必须主动开启 player.XLuaDebug = true 才允许执行（避免被滥用）
// luacBase64 必须是用 docs/luac.exe.win 编译的魔改 bytecode（标准 Lua 不能用）
//
// 用途：调试 PUBG 玩法 UI / 远程修复客户端 bug
// 注释提到"之前有人拿这个干坏事"——这是个 hack 性质的能力 慎用
func (g *GMCmd) XLuaDebug(userId uint32, luacBase64 string) {
	logger.Debug("xlua debug, uid: %v, luac: %v", userId, luacBase64)
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	// 只有在线玩家主动开启之后才能发送
	if !player.XLuaDebug {
		logger.Error("player xlua debug not enable, uid: %v", userId)
		return
	}
	luac, err := base64.StdEncoding.DecodeString(luacBase64)
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

func (g *GMCmd) AvPlayAudio(fileDataBase64 string) {
	fileData, err := base64.StdEncoding.DecodeString(fileDataBase64)
	if err != nil {
		logger.Error("file data base64 format error: %v", err)
		return
	}
	PlayAudio(fileData)
}

func (g *GMCmd) AvStartMidiInputDev() {
	err := StartMidiInputDev()
	if err != nil {
		logger.Error("start midi input dev error: %v", err)
	}
	logger.Info("start midi input dev ok")
}

func (g *GMCmd) AvStopMidiInputDev() {
	StopMidiInputDev()
	logger.Info("stop midi input dev ok")
}

// AvUpdateFrame JPEG 像素屏渲染（详见 CLAUDE.md "玩法实现状态" 中的 JPEG 像素屏）
//
// 80×80 像素 用 7 色 gadget 在 AI 世界场景 3 摆出彩色图片
// 默认坐标（2700, 200, -1800）是作者预留的空地坐标
// 玩具性质 但说明项目可以做出"用游戏世界做显示器"这种创意玩法
func (g *GMCmd) AvUpdateFrame(fileDataBase64 string, rgb bool, posX, posY, posZ float64) {
	fileData, err := base64.StdEncoding.DecodeString(fileDataBase64)
	if err != nil {
		logger.Error("file data base64 format error: %v", err)
		return
	}
	basePos := &model.Vector{X: posX, Y: posY, Z: posZ}
	if basePos.X == 0.0 && basePos.Y == 0.0 && basePos.Z == 0.0 {
		basePos = &model.Vector{X: 2700, Y: 200, Z: -1800}
	}
	UpdateFrame(fileData, basePos, rgb)
}

// CreateRobotInAiWorld 在 AI 世界中创建机器人玩家（**空壳实现**）
//
// 创建一个假玩家 + 加入 AI 世界 + 走完整四步状态机
// **没有 AI 行为**：机器人创建后不会移动/打怪/吃鸡 仅占一个位置
// 详见 CLAUDE.md "玩法实现状态" 表中"机器人玩家"行——未来扩展空间大
//
// 参数都可选：name 不传时随机 8 位字符串 avatarId 不传时取第一个角色
// 用于测试 PUBG 多人对战体验（凑人数）但实际只能当人形靶子用
func (g *GMCmd) CreateRobotInAiWorld(uid uint32, name string, avatarId uint32, posX, posY, posZ float64) {
	if uid == 0 {
		return
	}
	if name == "" {
		name = random.GetRandomStr(8)
	}
	if avatarId == 0 {
		for _, avatarData := range gdconf.GetAvatarDataMap() {
			avatarId = uint32(avatarData.AvatarId)
			break
		}
	}
	aiWorld := WORLD_MANAGER.GetAiWorld()
	robot := GAME.CreateRobot(uid, name, name)
	GAME.AddPlayerAvatar(uid, avatarId)
	dbAvatar := robot.GetDbAvatar()
	GAME.SetUpAvatarTeamReq(robot, &proto.SetUpAvatarTeamReq{
		TeamId:             1,
		AvatarTeamGuidList: []uint64{dbAvatar.GetAvatarById(avatarId).Guid},
		CurAvatarGuid:      dbAvatar.GetAvatarById(avatarId).Guid,
	})
	GAME.SetPlayerHeadImageReq(robot, &proto.SetPlayerHeadImageReq{
		AvatarId: avatarId,
	})
	GAME.JoinPlayerSceneReq(robot, &proto.JoinPlayerSceneReq{
		TargetUid: aiWorld.GetOwner().PlayerId,
	})
	GAME.EnterSceneReadyReq(robot, &proto.EnterSceneReadyReq{
		EnterSceneToken: aiWorld.GetEnterSceneToken(),
	})
	GAME.SceneInitFinishReq(robot, &proto.SceneInitFinishReq{
		EnterSceneToken: aiWorld.GetEnterSceneToken(),
	})
	GAME.EnterSceneDoneReq(robot, &proto.EnterSceneDoneReq{
		EnterSceneToken: aiWorld.GetEnterSceneToken(),
	})
	GAME.PostEnterSceneReq(robot, &proto.PostEnterSceneReq{
		EnterSceneToken: aiWorld.GetEnterSceneToken(),
	})
	GAME.EntityForceSyncReq(robot, &proto.EntityForceSyncReq{
		MotionInfo: &proto.MotionInfo{
			Pos: &proto.Vector{X: float32(posX), Y: float32(posY), Z: float32(posZ)},
			Rot: new(proto.Vector),
		},
		EntityId: aiWorld.GetPlayerActiveAvatarEntity(robot).GetId(),
	})
	robot.SetPos(&model.Vector{X: posX, Y: posY, Z: posZ})
}

// ServerAnnounce 服务器公告（全服弹窗）
// isRevoke=false 发布公告 / true 撤销已发布的公告
// announceId 唯一标识 撤销时按 ID 查找
func (g *GMCmd) ServerAnnounce(announceId uint32, announceMsg string, isRevoke bool) {
	if !isRevoke {
		GAME.ServerAnnounceNotify(announceId, announceMsg)
	} else {
		GAME.ServerAnnounceRevokeNotify(announceId)
	}
}

// SendMsgToPlayer 给玩家发任意 cmd 消息（运维调试神器）
//
// 通过 cmdName + JSON 字符串构造任意协议消息发给客户端
// 用例：调试新协议 / 触发客户端特定行为 / 展示活动公告等
//
// **安全限制**：禁止发 WindSeedClientNotify 和 PlayerLuaShellNotify
//
//	这两个是高危协议（前者控制客户端反作弊种子 后者远程执行 Lua）
//	"what are you doing ???" 这条 Error 日志是作者拦截滥用尝试的吐槽
func (g *GMCmd) SendMsgToPlayer(cmdName string, userId uint32, msgJson string) {
	if cmdProtoMap == nil {
		cmdProtoMap = cmd.NewCmdProtoMap()
	}
	cmdId := cmdProtoMap.GetCmdIdByCmdName(cmdName)
	if cmdId == 0 {
		logger.Error("cmd name not found")
		return
	}
	if cmdId == cmd.WindSeedClientNotify || cmdId == cmd.PlayerLuaShellNotify {
		logger.Error("what are you doing ???")
		return
	}
	msg := cmdProtoMap.GetProtoObjByCmdId(cmdId)
	err := protojson.Unmarshal([]byte(msgJson), msg)
	if err != nil {
		logger.Error("parse msg error: %v", err)
		return
	}
	GAME.SendMsg(cmdId, userId, 0, msg)
}

func (g *GMCmd) StartPubg() {
	iPlugin, err := PLUGIN_MANAGER.GetPlugin(&PluginPubg{})
	if err != nil {
		logger.Error("get plugin pubg error: %v", err)
		return
	}
	pluginPubg := iPlugin.(*PluginPubg)
	pluginPubg.StartPubg()
}

func (g *GMCmd) StopPubg() {
	iPlugin, err := PLUGIN_MANAGER.GetPlugin(&PluginPubg{})
	if err != nil {
		logger.Error("get plugin pubg error: %v", err)
		return
	}
	pluginPubg := iPlugin.(*PluginPubg)
	pluginPubg.StopPubg()
}

func (g *GMCmd) SetPhysicsEngineParam(pathTracing bool) {
	world := WORLD_MANAGER.GetAiWorld()
	engine := world.GetBulletPhysicsEngine()
	engine.SetPhysicsEngineParam(pathTracing)
}

func (g *GMCmd) ShowAvatarCollider() {
	world := WORLD_MANAGER.GetAiWorld()
	engine := world.GetBulletPhysicsEngine()
	engine.ShowAvatarCollider()
}

// AiWorldAoiDebug AI 世界 AOI 调试输出（运维诊断 PUBG 玩家可见性问题）
//
// 遍历 AI 世界所有非空 AOI 格子 打印每个格子里有哪些玩家及其位置
// 用于排查"为什么玩家 A 看不到玩家 B"这类 AOI 视野同步 bug
// 输出走 logger.Debug 大量日志 仅在 debug 级别可见
func (g *GMCmd) AiWorldAoiDebug() {
	aiWorld := WORLD_MANAGER.GetAiWorld()
	if aiWorld == nil {
		return
	}
	scene := aiWorld.GetSceneById(aiWorld.GetOwner().GetSceneId())
	aiWorldAoi := aiWorld.GetAiWorldAoi()
	gridMap := aiWorldAoi.Debug()
	logger.Debug("total grid num: %v", len(gridMap))
	for _, grid := range gridMap {
		objectMap := grid.GetObjectList()
		if len(objectMap) == 0 {
			continue
		}
		logger.Debug("================================================== GRID gid:%v ==================================================", grid.GetGid())
		for objectId, object := range objectMap {
			wa := object.(*WorldAvatar)
			var pos *model.Vector = nil
			entity := scene.GetEntity(wa.GetAvatarEntityId())
			if entity != nil {
				pos = entity.GetPos()
			}
			logger.Debug("uid: %v, wa.uid: %v, wa.avatarId: %v, wa.entityId: %v, pos: %+v", objectId, wa.GetUid(), wa.GetAvatarId(), wa.GetAvatarEntityId(), pos)
		}
	}
}

func (g *GMCmd) GetPlayerData(userId uint32) *model.Player {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return nil
	}
	return player
}

func (g *GMCmd) GetPlayerPos(userId uint32) (*model.Vector, *model.Vector) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return nil, nil
	}
	return GAME.GetPlayerPos(player), player.GetPos()
}

func (g *GMCmd) SendMail(userId uint32, title string, content string) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	GAME.AddPlayerMail(userId, title, content)
}

func (g *GMCmd) SetPlayerClientVersion(userId uint32, version int) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	player.ClientVersion = version
}
