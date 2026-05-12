package game

import (
	"hk4e/common/constant"
	"hk4e/gdconf"
	"hk4e/gs/model"
	"hk4e/pkg/endec"
	"hk4e/protocol/cmd"
	"hk4e/protocol/proto"

	"github.com/flswld/halo/logger"
	pb "google.golang.org/protobuf/proto"
)

// 队伍管理 模块
//
// 玩家共有 4 个常驻队伍（4 套队伍配置 可独立切换）+ 多人世界队伍（专门用于联机）
// 每个队伍最多 4 人 玩家可任意排列已拥有的角色
// 全局有"当前出战队"概念 决定大世界中实际带的角色
//
// 主要请求：
//   - ChangeAvatarReq: 切换出战角色（队内切人 不改队伍构成）
//   - SetUpAvatarTeamReq: 编辑指定队伍的角色构成（添加/移除/排序）
//   - ChooseCurAvatarTeamReq: 切换出战队伍（如从 1 队切到 2 队）
//   - ChangeMpTeamAvatarReq: 多人世界队伍调整（不影响单人队伍配置）
//   - AvatarDieAnimationEndReq: 角色死亡动画结束 → 自动切换到下一个活角色或全队死亡
//   - WorldPlayerReviveReq: 世界复活（七天神像/复苏材料）
//
// AI 世界限制：ChangeMpTeamAvatarReq 在 AI 世界被拒绝（PUBG 玩法不让玩家随意改队伍）

/************************************************** 接口请求 **************************************************/

// ChangeAvatarReq 切换出战角色请求（队内切人 如 1队 钟离切到香菱）
// 与 SetUpAvatarTeamReq 的区别：不改队伍构成 只改 dbTeam.CurrAvatarIndex 出战索引
func (g *Game) ChangeAvatarReq(player *model.Player, payloadMsg pb.Message) {
	req := payloadMsg.(*proto.ChangeAvatarReq)
	targetAvatar, ok := player.GameObjectGuidMap[req.Guid].(*model.Avatar)
	if !ok {
		logger.Error("target avatar error, avatarGuid: %v", req.Guid)
		g.SendError(cmd.ChangeAvatarRsp, player, &proto.ChangeAvatarRsp{})
		return
	}

	g.ChangeAvatar(player, targetAvatar.AvatarId)

	rsp := &proto.ChangeAvatarRsp{
		CurGuid: req.Guid,
	}
	g.SendMsg(cmd.ChangeAvatarRsp, player.PlayerId, player.ClientSeq, rsp)
}

// SetUpAvatarTeamReq 编辑队伍构成请求
//
// 校验：teamId 范围 1-4 + 角色数量 1-4 + 不能编辑当前出战队为空
// v4.0+ 客户端：编辑当前出战队时如果出战角色变了 还要发 VISION_REPLACE 让其他玩家看到角色切换
//
//	v4.0 之前用 VISION_BORN/VISION_MISS 一对 这里特殊处理是因为新版本动画连贯性要求
func (g *Game) SetUpAvatarTeamReq(player *model.Player, payloadMsg pb.Message) {
	req := payloadMsg.(*proto.SetUpAvatarTeamReq)
	teamId := req.TeamId
	avatarIdList := make([]uint32, 0)
	for _, avatarGuid := range req.AvatarTeamGuidList {
		avatar, ok := player.GameObjectGuidMap[avatarGuid].(*model.Avatar)
		if !ok {
			g.SendError(cmd.SetUpAvatarTeamRsp, player, &proto.SetUpAvatarTeamRsp{})
			return
		}
		avatarIdList = append(avatarIdList, avatar.AvatarId)
	}
	currAvatar, ok := player.GameObjectGuidMap[req.CurAvatarGuid].(*model.Avatar)
	if !ok {
		logger.Error("get curr avatar error, avatarGuid: %v", req.CurAvatarGuid)
		g.SendError(cmd.SetUpAvatarTeamRsp, player, &proto.SetUpAvatarTeamRsp{})
		return
	}

	if teamId <= 0 || teamId >= 5 {
		g.SendError(cmd.SetUpAvatarTeamRsp, player, &proto.SetUpAvatarTeamRsp{})
		return
	}
	dbTeam := player.GetDbTeam()
	if (teamId == uint32(dbTeam.GetActiveTeamId()) && len(avatarIdList) == 0) || len(avatarIdList) > 4 {
		g.SendError(cmd.SetUpAvatarTeamRsp, player, &proto.SetUpAvatarTeamRsp{})
		return
	}

	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		logger.Error("get world is nil, worldId: %v, uid: %v", player.WorldId, player.PlayerId)
		g.SendError(cmd.SetUpAvatarTeamRsp, player, &proto.SetUpAvatarTeamRsp{})
		return
	}
	scene := world.GetSceneById(player.GetSceneId())

	oldAvatarEntity := world.GetPlayerActiveAvatarEntity(player)
	g.ChangeTeam(player, teamId, avatarIdList, currAvatar.AvatarId)
	newAvatarEntity := world.GetPlayerActiveAvatarEntity(player)

	rsp := &proto.SetUpAvatarTeamRsp{
		TeamId:             req.TeamId,
		CurAvatarGuid:      req.CurAvatarGuid,
		AvatarTeamGuidList: req.AvatarTeamGuidList,
	}
	g.SendMsg(cmd.SetUpAvatarTeamRsp, player.PlayerId, player.ClientSeq, rsp)

	if player.ClientVersion >= 400 && oldAvatarEntity.GetId() != newAvatarEntity.GetId() {
		sceneEntityDisappearNotify := &proto.SceneEntityDisappearNotify{
			DisappearType: proto.VisionType_VISION_REPLACE,
			EntityList:    []uint32{oldAvatarEntity.GetId()},
		}
		g.SendToSceneA(scene, cmd.SceneEntityDisappearNotify, player.ClientSeq, sceneEntityDisappearNotify, 0)

		sceneEntityInfo := g.PacketSceneEntityInfoAvatar(scene, player, newAvatarEntity.(*AvatarEntity).GetAvatarId())
		sceneEntityAppearNotify := &proto.SceneEntityAppearNotify{
			AppearType: proto.VisionType_VISION_REPLACE,
			Param:      oldAvatarEntity.GetId(),
			EntityList: []*proto.SceneEntityInfo{sceneEntityInfo},
		}
		g.SendToSceneA(scene, cmd.SceneEntityAppearNotify, player.ClientSeq, sceneEntityAppearNotify, 0)
	}
}

// ChooseCurAvatarTeamReq 切换出战队伍请求（如从 1 队切到 2 队）
//
// 多人世界禁止切队伍（多人世界用专门的多人队伍 通过 ChangeMpTeamAvatarReq）
// 切队伍后：dbTeam.CurrTeamIndex 改变 + 出战角色重置为新队第 1 个 + 重新生成 worldAvatar 实体
func (g *Game) ChooseCurAvatarTeamReq(player *model.Player, payloadMsg pb.Message) {
	req := payloadMsg.(*proto.ChooseCurAvatarTeamReq)
	teamId := req.TeamId
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		logger.Error("get world is nil, worldId: %v, uid: %v", player.WorldId, player.PlayerId)
		g.SendError(cmd.ChooseCurAvatarTeamRsp, player, &proto.ChooseCurAvatarTeamRsp{})
		return
	}
	scene := world.GetSceneById(player.GetSceneId())
	if world.IsMultiplayerWorld() {
		g.SendError(cmd.ChooseCurAvatarTeamRsp, player, &proto.ChooseCurAvatarTeamRsp{})
		return
	}

	oldAvatarEntity := world.GetPlayerActiveAvatarEntity(player)

	dbTeam := player.GetDbTeam()
	team := dbTeam.GetTeamByIndex(uint8(teamId) - 1)
	if team == nil || len(team.GetAvatarIdList()) == 0 {
		g.SendError(cmd.ChooseCurAvatarTeamRsp, player, &proto.ChooseCurAvatarTeamRsp{})
		return
	}
	dbTeam.CurrTeamIndex = uint8(teamId) - 1
	dbTeam.CurrAvatarIndex = 0

	world.SetPlayerLocalTeam(player, team.GetAvatarIdList())
	world.SetPlayerActiveAvatarId(player, dbTeam.GetActiveAvatarId())
	world.UpdateMultiplayerTeam()
	world.UpdatePlayerWorldAvatar(player)

	sceneTeamUpdateNotify := g.PacketSceneTeamUpdateNotify(world, player)
	g.SendMsg(cmd.SceneTeamUpdateNotify, player.PlayerId, player.ClientSeq, sceneTeamUpdateNotify)

	newAvatarEntity := world.GetPlayerActiveAvatarEntity(player)

	chooseCurAvatarTeamRsp := &proto.ChooseCurAvatarTeamRsp{
		CurTeamId: teamId,
	}
	g.SendMsg(cmd.ChooseCurAvatarTeamRsp, player.PlayerId, player.ClientSeq, chooseCurAvatarTeamRsp)

	if player.ClientVersion >= 400 && oldAvatarEntity.GetId() != newAvatarEntity.GetId() {
		sceneEntityDisappearNotify := &proto.SceneEntityDisappearNotify{
			DisappearType: proto.VisionType_VISION_REPLACE,
			EntityList:    []uint32{oldAvatarEntity.GetId()},
		}
		g.SendToSceneA(scene, cmd.SceneEntityDisappearNotify, player.ClientSeq, sceneEntityDisappearNotify, 0)

		sceneEntityInfo := g.PacketSceneEntityInfoAvatar(scene, player, newAvatarEntity.(*AvatarEntity).GetAvatarId())
		sceneEntityAppearNotify := &proto.SceneEntityAppearNotify{
			AppearType: proto.VisionType_VISION_REPLACE,
			Param:      oldAvatarEntity.GetId(),
			EntityList: []*proto.SceneEntityInfo{sceneEntityInfo},
		}
		g.SendToSceneA(scene, cmd.SceneEntityAppearNotify, player.ClientSeq, sceneEntityAppearNotify, 0)
	}
}

// ChangeMpTeamAvatarReq 多人世界队伍调整请求
//
// 多人世界 4 人共享一个"多人队伍" 每人最多 4 个角色 总人数受限于队伍可用槽位
// 多人队伍配置不持久化（不写入 dbTeam）只影响当前多人世界
// AI 世界（PUBG）禁用此请求 → AI 世界队伍配置由系统固定（每人只有 1 个角色）
func (g *Game) ChangeMpTeamAvatarReq(player *model.Player, payloadMsg pb.Message) {
	req := payloadMsg.(*proto.ChangeMpTeamAvatarReq)
	avatarIdList := make([]uint32, 0)
	for _, avatarGuid := range req.AvatarGuidList {
		avatar, ok := player.GameObjectGuidMap[avatarGuid].(*model.Avatar)
		if !ok {
			logger.Error("avatar error, avatarGuid: %v", avatarGuid)
			return
		}
		avatarId := avatar.AvatarId
		avatarIdList = append(avatarIdList, avatarId)
	}
	currAvatar, ok := player.GameObjectGuidMap[req.CurAvatarGuid].(*model.Avatar)
	if !ok {
		logger.Error("avatar error, avatarGuid: %v", req.CurAvatarGuid)
		return
	}
	currAvatarId := currAvatar.AvatarId

	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		logger.Error("get world is nil, worldId: %v, uid: %v", player.WorldId, player.PlayerId)
		return
	}
	scene := world.GetSceneById(player.GetSceneId())

	if WORLD_MANAGER.IsAiWorld(world) {
		g.SendError(cmd.ChangeMpTeamAvatarRsp, player, &proto.ChangeMpTeamAvatarRsp{})
		return
	}

	if !world.IsMultiplayerWorld() || len(avatarIdList) == 0 || len(avatarIdList) > 4 {
		g.SendError(cmd.ChangeMpTeamAvatarRsp, player, &proto.ChangeMpTeamAvatarRsp{})
		return
	}

	oldAvatarEntity := world.GetPlayerActiveAvatarEntity(player)

	world.SetPlayerLocalTeam(player, avatarIdList)
	world.SetPlayerActiveAvatarId(player, currAvatarId)
	world.UpdateMultiplayerTeam()
	world.UpdatePlayerWorldAvatar(player)

	sceneTeamUpdateNotify := g.PacketSceneTeamUpdateNotify(world, player)
	g.SendToWorldA(world, cmd.SceneTeamUpdateNotify, player.ClientSeq, sceneTeamUpdateNotify, 0)

	newAvatarEntity := world.GetPlayerActiveAvatarEntity(player)

	changeMpTeamAvatarRsp := &proto.ChangeMpTeamAvatarRsp{
		CurAvatarGuid:  req.CurAvatarGuid,
		AvatarGuidList: req.AvatarGuidList,
	}
	g.SendMsg(cmd.ChangeMpTeamAvatarRsp, player.PlayerId, player.ClientSeq, changeMpTeamAvatarRsp)

	if player.ClientVersion >= 400 && oldAvatarEntity.GetId() != newAvatarEntity.GetId() {
		sceneEntityDisappearNotify := &proto.SceneEntityDisappearNotify{
			DisappearType: proto.VisionType_VISION_REPLACE,
			EntityList:    []uint32{oldAvatarEntity.GetId()},
		}
		g.SendToSceneA(scene, cmd.SceneEntityDisappearNotify, player.ClientSeq, sceneEntityDisappearNotify, 0)

		sceneEntityInfo := g.PacketSceneEntityInfoAvatar(scene, player, newAvatarEntity.(*AvatarEntity).GetAvatarId())
		sceneEntityAppearNotify := &proto.SceneEntityAppearNotify{
			AppearType: proto.VisionType_VISION_REPLACE,
			Param:      oldAvatarEntity.GetId(),
			EntityList: []*proto.SceneEntityInfo{sceneEntityInfo},
		}
		g.SendToSceneA(scene, cmd.SceneEntityAppearNotify, player.ClientSeq, sceneEntityAppearNotify, 0)
	}
}

// AvatarDieAnimationEndReq 角色死亡动画结束请求
//
// 触发时机：角色血量归零→播放死亡动画→动画结束 客户端发此请求
//
// 处理分支：
//  1. 溺水死亡（DIE_DRAWN）：耐力扣半 → TeleportPlayer 复活原位置
//  2. 其他死亡：找队内还活着的下一个角色 → ChangeAvatar 切换出战
//  3. 全队死亡：发 WorldPlayerDieNotify 客户端弹复活界面（让玩家选七天神像/付费复活）
//
// 用 PluginEventIdAvatarDieAnimationEnd 事件让 PUBG 插件介入死亡处理（吃鸡淘汰逻辑）
func (g *Game) AvatarDieAnimationEndReq(player *model.Player, payloadMsg pb.Message) {
	req := payloadMsg.(*proto.AvatarDieAnimationEndReq)

	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		return
	}
	scene := world.GetSceneById(player.GetSceneId())

	// 触发事件
	if PLUGIN_MANAGER.TriggerEvent(PluginEventIdAvatarDieAnimationEnd, &PluginEventAvatarDieAnimationEnd{
		PluginEvent: NewPluginEvent(),
		Player:      player,
		Req:         req,
	}) {
		return
	}

	entity := scene.GetEntity(uint32(req.DieGuid))
	if entity.GetLastDieType() == int32(proto.PlayerDieType_PLAYER_DIE_DRAWN) {
		maxStamina := player.PropMap[constant.PLAYER_PROP_MAX_STAMINA]
		// 设置玩家耐力为一半
		g.SetPlayerStamina(player, maxStamina/2)
		// 传送玩家至安全位置
		g.TeleportPlayer(
			player,
			proto.EnterReason_ENTER_REASON_REVIVAL,
			player.GetSceneId(),
			player.GetPos(),
			player.GetRot(),
			0,
			0,
		)
	} else {
		targetAvatarId := uint32(0)
		for _, worldAvatar := range world.GetPlayerWorldAvatarList(player) {
			dbAvatar := player.GetDbAvatar()
			avatar := dbAvatar.GetAvatarById(worldAvatar.GetAvatarId())
			if avatar == nil {
				logger.Error("get avatar is nil, avatarId: %v", worldAvatar.GetAvatarId())
				continue
			}
			if avatar.LifeState != constant.LIFE_STATE_ALIVE {
				continue
			}
			targetAvatarId = worldAvatar.GetAvatarId()
		}
		if targetAvatarId == 0 {
			g.SendMsg(cmd.WorldPlayerDieNotify, player.PlayerId, player.ClientSeq, &proto.WorldPlayerDieNotify{
				DieType: proto.PlayerDieType(entity.GetLastDieType()),
			})
		} else {
			g.ChangeAvatar(player, targetAvatarId)
		}
	}

	g.SendMsg(cmd.AvatarDieAnimationEndRsp, player.PlayerId, player.ClientSeq, &proto.AvatarDieAnimationEndRsp{SkillId: req.SkillId, DieGuid: req.DieGuid})
}

// WorldPlayerReviveReq 世界复活请求（七天神像/复苏材料/付费复活）
//
// AI 世界特殊：调 ReLoginPlayer 让玩家重连重生（PUBG 玩法吃鸡淘汰后重开一局）
// 普通世界：TeleportPlayer 原地传送一次（触发场景四步状态机里的"复活"分支 → 全队回血）
func (g *Game) WorldPlayerReviveReq(player *model.Player, payloadMsg pb.Message) {
	req := payloadMsg.(*proto.WorldPlayerReviveReq)
	_ = req
	world := WORLD_MANAGER.GetWorldById(player.WorldId)

	if WORLD_MANAGER.IsAiWorld(world) {
		GAME.ReLoginPlayer(player.PlayerId, true)
		return
	}

	g.TeleportPlayer(
		player,
		proto.EnterReason_ENTER_REASON_REVIVAL,
		player.GetSceneId(),
		player.GetPos(),
		player.GetRot(),
		0,
		0,
	)
	g.SendMsg(cmd.WorldPlayerReviveRsp, player.PlayerId, player.ClientSeq, new(proto.WorldPlayerReviveRsp))
}

/************************************************** 游戏功能 **************************************************/

// ChangeAvatar 切换队内出战角色（核心实现）
//
// 处理：
//  1. 校验目标角色不能是当前出战角色（避免重复切人）+ 必须在队伍中
//  2. dbTeam.CurrAvatarIndex 切到新角色（仅单人世界 多人世界由 worldTeam 管理）
//  3. 老角色实体设为 STANDBY 状态
//  4. 广播 SceneEntityDisappearNotify(VISION_REPLACE) + SceneEntityAppearNotify(VISION_REPLACE)
//     用 VISION_REPLACE 是为了让客户端做平滑切换动画（不是单纯的死亡+出生）
func (g *Game) ChangeAvatar(player *model.Player, targetAvatarId uint32) {
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		logger.Error("get world is nil, worldId: %v, uid: %v", player.WorldId, player.PlayerId)
		return
	}
	scene := world.GetSceneById(player.GetSceneId())
	oldAvatarId := world.GetPlayerActiveAvatarId(player)
	if targetAvatarId == oldAvatarId {
		logger.Error("can not change to the same avatar, uid: %v, oldAvatarId: %v, targetAvatarId: %v", player.PlayerId, oldAvatarId, targetAvatarId)
		return
	}
	newAvatarIndex := world.GetPlayerAvatarIndexByAvatarId(player, targetAvatarId)
	if newAvatarIndex == -1 {
		logger.Error("can not find the target avatar in team, uid: %v, targetAvatarId: %v", player.PlayerId, targetAvatarId)
		return
	}
	if !world.IsMultiplayerWorld() {
		dbTeam := player.GetDbTeam()
		dbTeam.CurrAvatarIndex = uint8(newAvatarIndex)
	}
	world.SetPlayerActiveAvatarId(player, targetAvatarId)
	oldAvatarEntityId := world.GetPlayerWorldAvatarEntityId(player, oldAvatarId)
	oldAvatarEntity := scene.GetEntity(oldAvatarEntityId)
	if oldAvatarEntity == nil {
		logger.Error("can not find old avatar entity, entity id: %v", oldAvatarEntityId)
		return
	}
	oldAvatarEntity.SetMoveState(uint16(proto.MotionState_MOTION_STANDBY))

	sceneEntityDisappearNotify := &proto.SceneEntityDisappearNotify{
		DisappearType: proto.VisionType_VISION_REPLACE,
		EntityList:    []uint32{oldAvatarEntity.GetId()},
	}
	g.SendToSceneA(scene, cmd.SceneEntityDisappearNotify, player.ClientSeq, sceneEntityDisappearNotify, 0)

	newAvatarEntity := g.PacketSceneEntityInfoAvatar(scene, player, targetAvatarId)
	sceneEntityAppearNotify := &proto.SceneEntityAppearNotify{
		AppearType: proto.VisionType_VISION_REPLACE,
		Param:      oldAvatarEntity.GetId(),
		EntityList: []*proto.SceneEntityInfo{newAvatarEntity},
	}
	g.SendToSceneA(scene, cmd.SceneEntityAppearNotify, player.ClientSeq, sceneEntityAppearNotify, 0)
}

// ChangeTeam 修改指定队伍的角色构成（仅单人世界）
//
// 处理：
//  1. 写入 dbTeam.TeamList[teamId-1].AvatarIdList
//  2. 全量发送 AvatarTeamUpdateNotify 给客户端（包含所有 4 队的最新构成）
//  3. 如果改动的是当前出战队 → 同步更新 World 中的 LocalTeam + 重新生成实体 + 发 SceneTeamUpdateNotify
//
// 多人世界禁止改队：多人世界用专门的多人队伍（ChangeMpTeamAvatarReq）
func (g *Game) ChangeTeam(player *model.Player, teamId uint32, avatarIdList []uint32, currAvatarId uint32) {
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		logger.Error("get world is nil, worldId: %v, uid: %v", player.WorldId, player.PlayerId)
		return
	}
	if world.IsMultiplayerWorld() {
		return
	}

	dbTeam := player.GetDbTeam()
	dbTeam.GetTeamByIndex(uint8(teamId - 1)).SetAvatarIdList(avatarIdList)

	avatarTeamUpdateNotify := &proto.AvatarTeamUpdateNotify{
		AvatarTeamMap: make(map[uint32]*proto.AvatarTeam),
	}
	dbAvatar := player.GetDbAvatar()
	for teamIndex, team := range dbTeam.TeamList {
		avatarTeam := &proto.AvatarTeam{
			TeamName:       team.Name,
			AvatarGuidList: make([]uint64, 0),
		}
		for _, avatarId := range team.GetAvatarIdList() {
			avatarTeam.AvatarGuidList = append(avatarTeam.AvatarGuidList, dbAvatar.GetAvatarById(avatarId).Guid)
		}
		avatarTeamUpdateNotify.AvatarTeamMap[uint32(teamIndex)+1] = avatarTeam
	}
	g.SendMsg(cmd.AvatarTeamUpdateNotify, player.PlayerId, player.ClientSeq, avatarTeamUpdateNotify)

	if teamId == uint32(dbTeam.GetActiveTeamId()) {
		world.SetPlayerLocalTeam(player, avatarIdList)
		world.SetPlayerActiveAvatarId(player, currAvatarId)
		world.UpdateMultiplayerTeam()
		world.UpdatePlayerWorldAvatar(player)

		currAvatarIndex := world.GetPlayerAvatarIndexByAvatarId(player, currAvatarId)
		dbTeam.CurrAvatarIndex = uint8(currAvatarIndex)

		sceneTeamUpdateNotify := g.PacketSceneTeamUpdateNotify(world, player)
		g.SendMsg(cmd.SceneTeamUpdateNotify, player.PlayerId, player.ClientSeq, sceneTeamUpdateNotify)
	}
}

/************************************************** 打包封装 **************************************************/

func (g *Game) PacketSceneTeamUpdateNotify(world *World, player *model.Player) *proto.SceneTeamUpdateNotify {
	sceneTeamUpdateNotify := &proto.SceneTeamUpdateNotify{
		IsInMp: world.IsMultiplayerWorld(),
	}
	empty := new(proto.AbilitySyncStateInfo)
	for _, worldAvatar := range world.GetWorldAvatarList() {
		if WORLD_MANAGER.IsAiWorld(world) && worldAvatar.GetUid() != player.PlayerId {
			continue
		}

		worldPlayer := USER_MANAGER.GetOnlineUser(worldAvatar.GetUid())
		if worldPlayer == nil {
			logger.Error("player is nil, uid: %v", worldAvatar.GetUid())
			continue
		}
		worldPlayerScene := world.GetSceneById(worldPlayer.GetSceneId())
		worldPlayerDbAvatar := worldPlayer.GetDbAvatar()
		worldPlayerAvatar := worldPlayerDbAvatar.GetAvatarById(worldAvatar.GetAvatarId())
		equipIdList := make([]uint32, 0)
		weapon := worldPlayerAvatar.EquipWeapon
		equipIdList = append(equipIdList, weapon.ItemId)
		for _, reliquary := range worldPlayerAvatar.EquipReliquaryMap {
			equipIdList = append(equipIdList, reliquary.ItemId)
		}
		sceneTeamAvatar := &proto.SceneTeamAvatar{
			PlayerUid:         worldPlayer.PlayerId,
			AvatarGuid:        worldPlayerAvatar.Guid,
			SceneId:           worldPlayer.GetSceneId(),
			EntityId:          world.GetPlayerWorldAvatarEntityId(worldPlayer, worldAvatar.GetAvatarId()),
			SceneEntityInfo:   g.PacketSceneEntityInfoAvatar(worldPlayerScene, worldPlayer, worldAvatar.GetAvatarId()),
			WeaponGuid:        worldPlayerAvatar.EquipWeapon.Guid,
			WeaponEntityId:    world.GetPlayerWorldAvatarWeaponEntityId(worldPlayer, worldAvatar.GetAvatarId()),
			IsPlayerCurAvatar: world.IsPlayerActiveAvatarEntity(worldPlayer, worldAvatar.GetAvatarEntityId()),
			IsOnScene:         world.IsPlayerActiveAvatarEntity(worldPlayer, worldAvatar.GetAvatarEntityId()),
			AvatarAbilityInfo: &proto.AbilitySyncStateInfo{
				IsInited:           len(worldAvatar.PacketAbilityList()) != 0,
				DynamicValueMap:    nil,
				AppliedAbilities:   worldAvatar.PacketAbilityList(),
				AppliedModifiers:   worldAvatar.PacketModifierList(),
				MixinRecoverInfos:  nil,
				SgvDynamicValueMap: nil,
			},
			WeaponAbilityInfo:   empty,
			AbilityControlBlock: g.PacketAvatarAbilityControlBlock(worldAvatar.GetAvatarId(), worldPlayerAvatar.SkillDepotId),
		}
		if world.IsMultiplayerWorld() {
			sceneTeamAvatar.AvatarInfo = g.PacketAvatarInfo(worldPlayerAvatar)
			sceneTeamAvatar.SceneAvatarInfo = g.PacketSceneAvatarInfo(worldPlayerScene, worldPlayer, worldAvatar.GetAvatarId())
		}
		sceneTeamUpdateNotify.SceneTeamAvatarList = append(sceneTeamUpdateNotify.SceneTeamAvatarList, sceneTeamAvatar)
	}
	return sceneTeamUpdateNotify
}

// PacketAvatarAbilityControlBlock 打包角色的 Ability 控制块（进场景时下发）
//
// 客户端进场景前需要预加载所有 ability 资源 控制块就是告诉客户端要加载哪些 ability：
//   - 默认 ability 列表（gdconf.GetDefaultAbilityNameList）：所有角色通用
//   - v4.0+ 枫丹潜水 ability：ActivityAbility_Absorb_Shoot（4.0 加的潜水玩法）
//   - 角色专属 ability（avatarDataConfig.ConfigAbility）：角色普攻/E/Q 的 ability
//   - 技能库 ability（skillDepot.ConfigAbility.TargetAbilities）：元素相关 ability
//
// 注释里 "TODO 队伍 ability" + "TODO 装备 ability"：圣遗物套装效果/武器精炼效果都属于这部分 项目未实现
// 这就是为什么"装备/精炼/套装效果不生效"的根本原因——客户端根本没收到要加载这些 ability 的指令
func (g *Game) PacketAvatarAbilityControlBlock(avatarId uint32, skillDepotId uint32) *proto.AbilityControlBlock {
	acb := &proto.AbilityControlBlock{
		AbilityEmbryoList: make([]*proto.AbilityEmbryo, 0),
	}
	abilityId := 0
	// 默认ability
	for _, defaultAbilityName := range gdconf.GetDefaultAbilityNameList() {
		abilityId++
		ae := &proto.AbilityEmbryo{
			AbilityId:               uint32(abilityId),
			AbilityNameHash:         uint32(endec.Hk4eAbilityHashCode(defaultAbilityName)),
			AbilityOverrideNameHash: uint32(endec.Hk4eAbilityHashCode("Default")),
		}
		acb.AbilityEmbryoList = append(acb.AbilityEmbryoList, ae)
	}
	// 枫丹潜水ability
	if SELF != nil && SELF.ClientVersion >= 400 {
		defaultAbilityNameList := []string{"ActivityAbility_Absorb_Shoot"}
		for _, defaultAbilityName := range defaultAbilityNameList {
			abilityId++
			ae := &proto.AbilityEmbryo{
				AbilityId:               uint32(abilityId),
				AbilityNameHash:         uint32(endec.Hk4eAbilityHashCode(defaultAbilityName)),
				AbilityOverrideNameHash: uint32(endec.Hk4eAbilityHashCode("Default")),
			}
			acb.AbilityEmbryoList = append(acb.AbilityEmbryoList, ae)
		}
	}
	// 角色ability
	avatarDataConfig := gdconf.GetAvatarDataById(int32(avatarId))
	if avatarDataConfig != nil && avatarDataConfig.ConfigAbility != nil {
		for _, configAbility := range avatarDataConfig.ConfigAbility.Abilities {
			abilityId++
			ae := &proto.AbilityEmbryo{
				AbilityId:               uint32(abilityId),
				AbilityNameHash:         uint32(endec.Hk4eAbilityHashCode(configAbility.AbilityName)),
				AbilityOverrideNameHash: uint32(endec.Hk4eAbilityHashCode("Default")),
			}
			acb.AbilityEmbryoList = append(acb.AbilityEmbryoList, ae)
		}
	}
	// 技能库ability
	avatarSkillDepotDataConfig := gdconf.GetAvatarSkillDepotDataById(int32(skillDepotId))
	if avatarSkillDepotDataConfig != nil && avatarSkillDepotDataConfig.ConfigAbility != nil {
		for _, configAbility := range avatarSkillDepotDataConfig.ConfigAbility.TargetAbilities {
			abilityId++
			ae := &proto.AbilityEmbryo{
				AbilityId:               uint32(abilityId),
				AbilityNameHash:         uint32(endec.Hk4eAbilityHashCode(configAbility.AbilityName)),
				AbilityOverrideNameHash: uint32(endec.Hk4eAbilityHashCode("Default")),
			}
			acb.AbilityEmbryoList = append(acb.AbilityEmbryoList, ae)
		}
	}
	// TODO 队伍ability
	// TODO 装备ability
	return acb
}

func (g *Game) PacketTeamAbilityControlBlock() *proto.AbilityControlBlock {
	acb := &proto.AbilityControlBlock{
		AbilityEmbryoList: make([]*proto.AbilityEmbryo, 0),
	}
	abilityId := 0
	// 枫丹潜水ability
	if SELF != nil && SELF.ClientVersion >= 400 {
		defaultTeamAbilityNameList := []string{"Ability_Avatar_Dive_Team"}
		for _, defaultTeamAbilityName := range defaultTeamAbilityNameList {
			abilityId++
			ae := &proto.AbilityEmbryo{
				AbilityId:               uint32(abilityId),
				AbilityNameHash:         uint32(endec.Hk4eAbilityHashCode(defaultTeamAbilityName)),
				AbilityOverrideNameHash: uint32(endec.Hk4eAbilityHashCode("Default")),
			}
			acb.AbilityEmbryoList = append(acb.AbilityEmbryoList, ae)
		}
	}
	return acb
}
