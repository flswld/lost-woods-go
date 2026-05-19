package game

import (
	"strconv"

	"hk4e/common/constant"
	"hk4e/gdconf"
	"hk4e/gs/model"
	"hk4e/pkg/object"
	"hk4e/protocol/cmd"
	"hk4e/protocol/proto"

	"github.com/flswld/halo/logger"
	pb "google.golang.org/protobuf/proto"
)

// 角色养成 模块
//
// 本文件承载玩家角色（Avatar）的全部养成逻辑：
//   - 升级（AvatarUpgradeReq）：经验书喂角色 升等级
//   - 突破（AvatarPromoteReq）：达到等级上限后突破解锁更高等级（消耗突破材料+摩拉）
//   - 突破奖励（AvatarPromoteGetRewardReq）：每突破到指定档位领取一次性原石/天赋之冠等
//   - 风之翼（AvatarWearFlycloakReq）+ 装扮（AvatarChangeCostumeReq）
//   - 天赋升级（AvatarSkillUpgradeReq）：普攻/E/Q 三个技能各自升级（消耗天赋之冠+摩拉）
//   - 命之座（UnlockAvatarTalentReq）：消耗命星解锁命之座（**激活后实际效果不生效——依赖 ability 系统**）
//   - HP/能量管理（AddPlayerAvatarHp/SubPlayerAvatarHp/AddPlayerAvatarEnergy/CostPlayerAvatarEnergy）
//   - 元素切换（ChangePlayerAvatarSkillDepot）：主角七国元素切换（旅行者特有）
//   - 复活（RevivePlayerAvatar）：死亡角色复活
//   - 战斗属性更新（UpdatePlayerAvatarFightProp）：基础属性 + 突破属性 + 词条属性 + 天赋加成
//
// 关键约束：
//   - 角色 GUID 是雪花 ID，所有玩家请求都通过 GameObjectGuidMap 反查 Avatar 对象
//   - 升级摩拉 = 经验书数量 / 5（虽然这是固定比例 但和官服一致）
//   - 武器精炼/命之座/天赋等的实际效果**没有实现**（详见 CLAUDE.md "Ability 系统现状"）
//   - 队伍角色升级后会广播 EntityFightPropUpdateNotify 给场景内其他玩家

/************************************************** 接口请求 **************************************************/

// AvatarUpgradeReq 角色升级请求（经验书喂角色）
//
// 处理流程：
//  1. 校验角色存在 + 经验书物品配置 + 摩拉是否足够（摩拉 = 经验数 / 5）
//  2. 校验角色等级未达突破限制（每个突破阶段有等级上限 如未突破→20级 1突→40级）
//  3. 一次性扣经验书 + 摩拉
//  4. 调 AddPlayerAvatarExp 给角色加经验（内部会按等级表升级 + 更新战斗属性）
func (g *Game) AvatarUpgradeReq(player *model.Player, payloadMsg pb.Message) {
	req := payloadMsg.(*proto.AvatarUpgradeReq)
	// 是否拥有角色
	avatar, ok := player.GameObjectGuidMap[req.AvatarGuid].(*model.Avatar)
	if !ok {
		logger.Error("avatar error, avatarGuid: %v", req.AvatarGuid)
		g.SendError(cmd.AvatarUpgradeRsp, player, &proto.AvatarUpgradeRsp{}, proto.Retcode_RET_CAN_NOT_FIND_AVATAR)
		return
	}
	// 获取经验书物品配置表
	itemDataConfig := gdconf.GetItemDataById(int32(req.ItemId))
	if itemDataConfig == nil {
		logger.Error("item data config error, itemId: %v", constant.ITEM_ID_SCOIN)
		g.SendError(cmd.AvatarUpgradeRsp, player, &proto.AvatarUpgradeRsp{}, proto.Retcode_RET_ITEM_NOT_EXIST)
		return
	}
	// 经验书将给予的经验数
	itemParam, err := strconv.Atoi(itemDataConfig.Use1Param1)
	if err != nil {
		logger.Error("parse item param error: %v", err)
		g.SendError(cmd.AvatarUpgradeRsp, player, &proto.AvatarUpgradeRsp{})
		return
	}
	// 角色获得的经验
	expCount := uint32(itemParam) * req.Count
	// 摩拉数量是否足够
	if g.GetPlayerItemCount(player.PlayerId, constant.ITEM_ID_SCOIN) < expCount/5 {
		logger.Error("item count not enough, itemId: %v", constant.ITEM_ID_SCOIN)
		g.SendError(cmd.AvatarUpgradeRsp, player, &proto.AvatarUpgradeRsp{}, proto.Retcode_RET_SCOIN_NOT_ENOUGH)
		return
	}
	// 获取角色配置表
	avatarDataConfig := gdconf.GetAvatarDataById(int32(avatar.AvatarId))
	if avatarDataConfig == nil {
		logger.Error("avatar config error, avatarId: %v", avatar.AvatarId)
		g.SendError(cmd.AvatarUpgradeRsp, player, &proto.AvatarUpgradeRsp{})
		return
	}
	// 获取角色突破配置表
	avatarPromoteConfig := gdconf.GetAvatarPromoteDataByIdAndLevel(avatarDataConfig.PromoteId, int32(avatar.Promote))
	if avatarPromoteConfig == nil {
		logger.Error("avatar promote config error, promoteLevel: %v", avatar.Promote)
		g.SendError(cmd.AvatarUpgradeRsp, player, &proto.AvatarUpgradeRsp{})
		return
	}
	// 角色等级是否达到限制
	if avatar.Level >= uint8(avatarPromoteConfig.LevelLimit) {
		logger.Error("avatar level ge level limit, level: %v", avatar.Level)
		g.SendError(cmd.AvatarUpgradeRsp, player, &proto.AvatarUpgradeRsp{}, proto.Retcode_RET_AVATAR_BREAK_LEVEL_LESS_THAN)
		return
	}
	// 消耗升级材料以及摩拉
	ok = g.CostPlayerItem(player.PlayerId, []*ChangeItem{
		{ItemId: req.ItemId, ChangeCount: req.Count},
		{ItemId: constant.ITEM_ID_SCOIN, ChangeCount: expCount / 5},
	})
	if !ok {
		logger.Error("item count not enough, uid: %v", player.PlayerId)
		g.SendError(cmd.AvatarUpgradeRsp, player, &proto.AvatarUpgradeRsp{}, proto.Retcode_RET_ITEM_COUNT_NOT_ENOUGH)
		return
	}
	// 角色升级前的信息
	oldLevel := avatar.Level
	oldFightPropMap := make(map[uint32]float32, len(avatar.FightPropMap))
	for propType, propValue := range avatar.FightPropMap {
		oldFightPropMap[propType] = propValue
	}

	// 角色添加经验
	g.AddPlayerAvatarExp(player.PlayerId, avatar.AvatarId, expCount)

	avatarUpgradeRsp := &proto.AvatarUpgradeRsp{
		CurLevel:        uint32(avatar.Level),
		OldLevel:        uint32(oldLevel),
		OldFightPropMap: oldFightPropMap,
		CurFightPropMap: avatar.FightPropMap,
		AvatarGuid:      req.AvatarGuid,
	}
	g.SendMsg(cmd.AvatarUpgradeRsp, player.PlayerId, player.ClientSeq, avatarUpgradeRsp)
}

// AvatarPromoteReq 角色突破请求（达到等级上限后解锁更高级数）
//
// 处理流程：
//  1. 校验角色等级达到当前突破阶段的上限（如未突破→20级才能1突）
//  2. 校验冒险等级达到 MinPlayerLevel（突破有冒险等级要求）
//  3. 校验突破材料 + 摩拉数量
//  4. 一次性扣材料 + 摩拉
//  5. avatar.Promote++ 突破阶段加一 等级保持不变
func (g *Game) AvatarPromoteReq(player *model.Player, payloadMsg pb.Message) {
	req := payloadMsg.(*proto.AvatarPromoteReq)
	// 是否拥有角色
	avatar, ok := player.GameObjectGuidMap[req.Guid].(*model.Avatar)
	if !ok {
		logger.Error("avatar error, avatarGuid: %v", req.Guid)
		g.SendError(cmd.AvatarPromoteRsp, player, &proto.AvatarPromoteRsp{}, proto.Retcode_RET_CAN_NOT_FIND_AVATAR)
		return
	}
	// 获取角色配置表
	avatarDataConfig := gdconf.GetAvatarDataById(int32(avatar.AvatarId))
	if avatarDataConfig == nil {
		logger.Error("avatar config error, avatarId: %v", avatar.AvatarId)
		g.SendError(cmd.AvatarPromoteRsp, player, &proto.AvatarPromoteRsp{})
		return
	}
	// 获取角色突破配置表
	avatarPromoteConfig := gdconf.GetAvatarPromoteDataByIdAndLevel(avatarDataConfig.PromoteId, int32(avatar.Promote))
	if avatarPromoteConfig == nil {
		logger.Error("avatar promote config error, promoteLevel: %v", avatar.Promote)
		g.SendError(cmd.AvatarPromoteRsp, player, &proto.AvatarPromoteRsp{})
		return
	}
	// 角色等级是否达到限制
	if avatar.Level < uint8(avatarPromoteConfig.LevelLimit) {
		logger.Error("avatar level le level limit, level: %v", avatar.Level)
		g.SendError(cmd.AvatarPromoteRsp, player, &proto.AvatarPromoteRsp{}, proto.Retcode_RET_AVATAR_LEVEL_LESS_THAN)
		return
	}
	// 获取角色突破下一级的配置表
	avatarPromoteConfig = gdconf.GetAvatarPromoteDataByIdAndLevel(avatarDataConfig.PromoteId, int32(avatar.Promote+1))
	if avatarPromoteConfig == nil {
		logger.Error("avatar promote config error, next promoteLevel: %v", avatar.Promote+1)
		g.SendError(cmd.AvatarPromoteRsp, player, &proto.AvatarPromoteRsp{}, proto.Retcode_RET_AVATAR_ON_MAX_BREAK_LEVEL)
		return
	}
	// 将被消耗的物品列表
	costItemList := make([]*ChangeItem, 0, len(avatarPromoteConfig.CostItemMap)+1)
	// 突破材料是否足够并添加到消耗物品列表
	for itemId, count := range avatarPromoteConfig.CostItemMap {
		costItemList = append(costItemList, &ChangeItem{
			ItemId:      itemId,
			ChangeCount: count,
		})
	}
	// 消耗列表添加摩拉的消耗
	costItemList = append(costItemList, &ChangeItem{
		ItemId:      constant.ITEM_ID_SCOIN,
		ChangeCount: uint32(avatarPromoteConfig.CostCoin),
	})
	// 突破材料以及摩拉是否足够
	for _, item := range costItemList {
		if g.GetPlayerItemCount(player.PlayerId, item.ItemId) < item.ChangeCount {
			logger.Error("item count not enough, itemId: %v", item.ItemId)
			// 摩拉的错误提示与材料不同
			if item.ItemId == constant.ITEM_ID_SCOIN {
				g.SendError(cmd.AvatarPromoteRsp, player, &proto.AvatarPromoteRsp{}, proto.Retcode_RET_SCOIN_NOT_ENOUGH)
			}
			g.SendError(cmd.AvatarPromoteRsp, player, &proto.AvatarPromoteRsp{}, proto.Retcode_RET_ITEM_COUNT_NOT_ENOUGH)
			return
		}
	}
	// 冒险等级是否符合要求
	if player.PropMap[constant.PLAYER_PROP_PLAYER_LEVEL] < uint32(avatarPromoteConfig.MinPlayerLevel) {
		logger.Error("player level not enough, level: %v", player.PropMap[constant.PLAYER_PROP_PLAYER_LEVEL])
		g.SendError(cmd.AvatarPromoteRsp, player, &proto.AvatarPromoteRsp{}, proto.Retcode_RET_PLAYER_LEVEL_LESS_THAN)
		return
	}
	// 消耗突破材料和摩拉
	ok = g.CostPlayerItem(player.PlayerId, costItemList)
	if !ok {
		logger.Error("item count not enough, uid: %v", player.PlayerId)
		g.SendError(cmd.AvatarPromoteRsp, player, &proto.AvatarPromoteRsp{}, proto.Retcode_RET_ITEM_COUNT_NOT_ENOUGH)
		return
	}

	// 角色突破等级+1
	avatar.Promote++
	g.SetPlayerAvatarLevelExpPromote(player.PlayerId, avatar.AvatarId, 0, 0, avatar.Promote)

	avatarPromoteRsp := &proto.AvatarPromoteRsp{
		Guid: req.Guid,
	}
	g.SendMsg(cmd.AvatarPromoteRsp, player.PlayerId, player.ClientSeq, avatarPromoteRsp)
}

// AvatarPromoteGetRewardReq 角色突破获取奖励请求
func (g *Game) AvatarPromoteGetRewardReq(player *model.Player, payloadMsg pb.Message) {
	req := payloadMsg.(*proto.AvatarPromoteGetRewardReq)
	// 是否拥有角色
	avatar, ok := player.GameObjectGuidMap[req.AvatarGuid].(*model.Avatar)
	if !ok {
		logger.Error("avatar error, avatarGuid: %v", req.AvatarGuid)
		g.SendError(cmd.AvatarPromoteGetRewardRsp, player, &proto.AvatarPromoteGetRewardRsp{}, proto.Retcode_RET_CAN_NOT_FIND_AVATAR)
		return
	}
	// 获取角色配置表
	avatarDataConfig := gdconf.GetAvatarDataById(int32(avatar.AvatarId))
	if avatarDataConfig == nil {
		logger.Error("avatar config error, avatarId: %v", avatar.AvatarId)
		g.SendError(cmd.AvatarPromoteGetRewardRsp, player, &proto.AvatarPromoteGetRewardRsp{})
		return
	}
	// 角色是否获取过该突破等级的奖励
	if avatar.PromoteRewardMap[req.PromoteLevel] {
		logger.Error("avatar config error, avatarId: %v", avatar.AvatarId)
		g.SendError(cmd.AvatarPromoteGetRewardRsp, player, &proto.AvatarPromoteGetRewardRsp{}, proto.Retcode_RET_REWARD_HAS_TAKEN)
		return
	}
	// 发放奖励
	rewardId := avatarDataConfig.PromoteRewardMap[req.PromoteLevel]
	ok = g.RewardItem(player.PlayerId, rewardId, proto.ActionReasonType_ACTION_REASON_AVATAR_PROMOTE)
	if !ok {
		g.SendError(cmd.AvatarPromoteGetRewardRsp, player, &proto.AvatarPromoteGetRewardRsp{})
		return
	}
	// 设置该奖励为已被获取状态
	avatar.PromoteRewardMap[req.PromoteLevel] = true
	avatarPromoteGetRewardRsp := &proto.AvatarPromoteGetRewardRsp{
		RewardId:     rewardId,
		AvatarGuid:   req.AvatarGuid,
		PromoteLevel: req.PromoteLevel,
	}
	g.SendMsg(cmd.AvatarPromoteGetRewardRsp, player.PlayerId, player.ClientSeq, avatarPromoteGetRewardRsp)
}

// AvatarWearFlycloakReq 角色装备风之翼请求
func (g *Game) AvatarWearFlycloakReq(player *model.Player, payloadMsg pb.Message) {
	req := payloadMsg.(*proto.AvatarWearFlycloakReq)

	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		logger.Error("get world is nil, worldId: %v, uid: %v", player.WorldId, player.PlayerId)
		g.SendError(cmd.AvatarWearFlycloakRsp, player, &proto.AvatarWearFlycloakRsp{})
		return
	}
	scene := world.GetSceneById(player.GetSceneId())

	// 确保角色存在
	avatar, ok := player.GameObjectGuidMap[req.AvatarGuid].(*model.Avatar)
	if !ok {
		logger.Error("avatar error, avatarGuid: %v", req.AvatarGuid)
		g.SendError(cmd.AvatarWearFlycloakRsp, player, &proto.AvatarWearFlycloakRsp{}, proto.Retcode_RET_CAN_NOT_FIND_AVATAR)
		return
	}

	// 确保要更换的风之翼已获得
	exist := false
	dbAvatar := player.GetDbAvatar()
	for _, v := range dbAvatar.FlyCloakList {
		if v == req.FlycloakId {
			exist = true
		}
	}
	if !exist {
		logger.Error("flycloak not exist, flycloakId: %v", req.FlycloakId)
		g.SendError(cmd.AvatarWearFlycloakRsp, player, &proto.AvatarWearFlycloakRsp{}, proto.Retcode_RET_NOT_HAS_FLYCLOAK)
		return
	}

	// 设置角色风之翼
	avatar.FlyCloak = req.FlycloakId

	ntf := &proto.AvatarFlycloakChangeNotify{
		AvatarGuid: req.AvatarGuid,
		FlycloakId: req.FlycloakId,
	}
	g.SendToSceneA(scene, cmd.AvatarFlycloakChangeNotify, player.ClientSeq, ntf, 0)

	rsp := &proto.AvatarWearFlycloakRsp{
		AvatarGuid: req.AvatarGuid,
		FlycloakId: req.FlycloakId,
	}
	g.SendMsg(cmd.AvatarWearFlycloakRsp, player.PlayerId, player.ClientSeq, rsp)
}

// AvatarChangeCostumeReq 角色更换时装请求
func (g *Game) AvatarChangeCostumeReq(player *model.Player, payloadMsg pb.Message) {
	req := payloadMsg.(*proto.AvatarChangeCostumeReq)

	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		logger.Error("get world is nil, worldId: %v, uid: %v", player.WorldId, player.PlayerId)
		g.SendError(cmd.AvatarChangeCostumeRsp, player, &proto.AvatarChangeCostumeRsp{})
		return
	}
	scene := world.GetSceneById(player.GetSceneId())

	// 确保角色存在
	avatar, ok := player.GameObjectGuidMap[req.AvatarGuid].(*model.Avatar)
	if !ok {
		logger.Error("avatar error, avatarGuid: %v", req.AvatarGuid)
		g.SendError(cmd.AvatarChangeCostumeRsp, player, &proto.AvatarChangeCostumeRsp{}, proto.Retcode_RET_COSTUME_AVATAR_ERROR)
		return
	}

	// 确保要更换的时装已获得
	exist := false
	dbAvatar := player.GetDbAvatar()
	for _, v := range dbAvatar.CostumeList {
		if v == req.CostumeId {
			exist = true
		}
	}
	if req.CostumeId == 0 {
		exist = true
	}
	if !exist {
		logger.Error("costume not exist, costumeId: %v", req.CostumeId)
		g.SendError(cmd.AvatarChangeCostumeRsp, player, &proto.AvatarChangeCostumeRsp{}, proto.Retcode_RET_NOT_HAS_COSTUME)
		return
	}

	// 设置角色时装
	avatar.Costume = req.CostumeId

	// 角色更换时装通知
	ntf := new(proto.AvatarChangeCostumeNotify)
	// 要更换时装的角色实体不存在代表更换的是仓库内的角色
	if world.GetPlayerWorldAvatar(player, avatar.AvatarId) == nil {
		ntf.EntityInfo = &proto.SceneEntityInfo{
			Entity: &proto.SceneEntityInfo_Avatar{
				Avatar: g.PacketSceneAvatarInfo(scene, player, avatar.AvatarId),
			},
		}
	} else {
		ntf.EntityInfo = g.PacketSceneEntityInfoAvatar(scene, player, avatar.AvatarId)
	}
	g.SendToSceneA(scene, cmd.AvatarChangeCostumeNotify, player.ClientSeq, ntf, 0)

	rsp := &proto.AvatarChangeCostumeRsp{
		AvatarGuid: req.AvatarGuid,
		CostumeId:  req.CostumeId,
	}
	g.SendMsg(cmd.AvatarChangeCostumeRsp, player.PlayerId, player.ClientSeq, rsp)
}

// AvatarSkillUpgradeReq 角色技能升级请求（普攻/E/Q 三个天赋各自升级）
//
// 处理：消耗天赋之冠 + 摩拉 → SkillLevelMap[skillId]++ → 广播 AvatarSkillChangeNotify
// **效果不生效**：技能升级带来的伤害提升通过 ProudSkillData 的 ParamList 表达
// 但项目 Ability 系统只实现了 6 种 AbilityAction 不解析这些参数 所以技能升级实际不影响伤害
func (g *Game) AvatarSkillUpgradeReq(player *model.Player, payloadMsg pb.Message) {
	req := payloadMsg.(*proto.AvatarSkillUpgradeReq)
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		logger.Error("get world is nil, worldId: %v, uid: %v", player.WorldId, player.PlayerId)
		g.SendError(cmd.AvatarSkillUpgradeRsp, player, &proto.AvatarSkillUpgradeRsp{})
		return
	}
	avatar, ok := player.GameObjectGuidMap[req.AvatarGuid].(*model.Avatar)
	if !ok {
		g.SendError(cmd.AvatarSkillUpgradeRsp, player, &proto.AvatarSkillUpgradeRsp{})
		return
	}
	skillLevel, exist := avatar.SkillLevelMap[req.AvatarSkillId]
	if !exist {
		g.SendError(cmd.AvatarSkillUpgradeRsp, player, &proto.AvatarSkillUpgradeRsp{})
		return
	}
	avatarSkillDataConfig := gdconf.GetAvatarSkillDataById(int32(req.AvatarSkillId))
	if avatarSkillDataConfig == nil {
		g.SendError(cmd.AvatarSkillUpgradeRsp, player, &proto.AvatarSkillUpgradeRsp{})
		return
	}
	proudSkillDataConfig := gdconf.GetProudSkillDataByGroupIdAndLevel(avatarSkillDataConfig.UpgradeSkillGroupId, int32(skillLevel))
	if proudSkillDataConfig == nil {
		g.SendError(cmd.AvatarSkillUpgradeRsp, player, &proto.AvatarSkillUpgradeRsp{})
		return
	}

	// 消耗物品列表
	costItemList := make([]*ChangeItem, 0)
	for _, costItem := range proudSkillDataConfig.CostItemList {
		costItemList = append(costItemList, &ChangeItem{
			ItemId:      uint32(costItem.ItemId),
			ChangeCount: uint32(costItem.ItemCount),
		})
	}
	costItemList = append(costItemList, &ChangeItem{
		ItemId:      constant.ITEM_ID_SCOIN,
		ChangeCount: uint32(proudSkillDataConfig.CostSCoin),
	})
	ok = g.CostPlayerItem(player.PlayerId, costItemList)
	if !ok {
		g.SendError(cmd.AvatarSkillUpgradeRsp, player, &proto.AvatarSkillUpgradeRsp{})
		return
	}

	skillLevel++
	avatar.SkillLevelMap[req.AvatarSkillId] = skillLevel

	entityId := world.GetPlayerWorldAvatarEntityId(player, avatar.AvatarId)
	ntf := &proto.AvatarSkillChangeNotify{
		CurLevel:      skillLevel,
		AvatarGuid:    req.AvatarGuid,
		EntityId:      entityId,
		SkillDepotId:  avatar.SkillDepotId,
		OldLevel:      req.OldLevel,
		AvatarSkillId: req.AvatarSkillId,
	}
	g.SendMsg(cmd.AvatarSkillChangeNotify, player.PlayerId, player.ClientSeq, ntf)

	rsp := &proto.AvatarSkillUpgradeRsp{
		AvatarGuid:    req.AvatarGuid,
		CurLevel:      skillLevel,
		AvatarSkillId: req.AvatarSkillId,
		OldLevel:      req.OldLevel,
	}
	g.SendMsg(cmd.AvatarSkillUpgradeRsp, player.PlayerId, player.ClientSeq, rsp)
}

// UnlockAvatarTalentReq 角色命之座解锁请求（消耗命星）
//
// 命之座加成（如"E技能附带额外伤害"、"Q释放后回能"）通过 TalentSkillData 的 buff 表达
// 同样依赖 Ability 系统支持 但项目未实现 → **解锁后实际效果不生效**
// 仅做了：扣命星 + 加进 avatar.TalentIdList + 广播 AvatarUnlockTalentNotify（让客户端显示已解锁）
func (g *Game) UnlockAvatarTalentReq(player *model.Player, payloadMsg pb.Message) {
	req := payloadMsg.(*proto.UnlockAvatarTalentReq)
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		logger.Error("get world is nil, worldId: %v, uid: %v", player.WorldId, player.PlayerId)
		g.SendError(cmd.UnlockAvatarTalentRsp, player, &proto.UnlockAvatarTalentRsp{})
		return
	}
	avatar, ok := player.GameObjectGuidMap[req.AvatarGuid].(*model.Avatar)
	if !ok {
		g.SendError(cmd.UnlockAvatarTalentRsp, player, &proto.UnlockAvatarTalentRsp{})
		return
	}

	talentSkillDataConfig := gdconf.GetTalentSkillDataById(int32(req.TalentId))
	if talentSkillDataConfig == nil {
		logger.Error("get talentSkillDataConfig is nil, talentId: %v", req.TalentId)
		g.SendError(cmd.UnlockAvatarTalentRsp, player, &proto.UnlockAvatarTalentRsp{})
		return
	}
	ok = g.CostPlayerItem(player.PlayerId, []*ChangeItem{{
		ItemId:      uint32(talentSkillDataConfig.CostItemId),
		ChangeCount: uint32(talentSkillDataConfig.CostItemCount)}})
	if !ok {
		g.SendError(cmd.UnlockAvatarTalentRsp, player, &proto.UnlockAvatarTalentRsp{})
		return
	}

	avatar.TalentIdList = append(avatar.TalentIdList, req.TalentId)

	entityId := world.GetPlayerWorldAvatarEntityId(player, avatar.AvatarId)
	ntf := &proto.AvatarUnlockTalentNotify{
		EntityId:     entityId,
		AvatarGuid:   req.AvatarGuid,
		TalentId:     req.TalentId,
		SkillDepotId: avatar.SkillDepotId,
	}
	g.SendMsg(cmd.AvatarUnlockTalentNotify, player.PlayerId, player.ClientSeq, ntf)

	rsp := &proto.UnlockAvatarTalentRsp{
		TalentId:   req.TalentId,
		AvatarGuid: req.AvatarGuid,
	}
	g.SendMsg(cmd.UnlockAvatarTalentRsp, player.PlayerId, player.ClientSeq, rsp)
}

/************************************************** 游戏功能 **************************************************/

// GetAllAvatarDataConfig 获取所有可发放的角色数据配置（GM "all_avatar" 用）
// 跳过 ID <= 10000001（无效）+ ID >= 11000000（无效）+ 主角 10000005/10000007（特殊处理）
func (g *Game) GetAllAvatarDataConfig() map[int32]*gdconf.AvatarData {
	allAvatarDataConfig := make(map[int32]*gdconf.AvatarData)
	for avatarId, avatarData := range gdconf.GetAvatarDataMap() {
		if avatarId <= 10000001 || avatarId >= 11000000 {
			// 跳过无效角色
			continue
		}
		if avatarId == 10000005 || avatarId == 10000007 {
			// 跳过主角
			continue
		}
		allAvatarDataConfig[avatarId] = avatarData
	}
	return allAvatarDataConfig
}

// AddPlayerAvatar 添加玩家角色（GM 命令/抽卡获取/任务奖励都走这里）
//
// 处理：
//  1. 已拥有则直接返回（不重复添加）
//  2. dbAvatar.AddAvatar 在玩家档中创建 Avatar 对象（默认 1 级 0 突破）
//  3. 自动给角色装备 InitialWeapon（角色配置里的初始武器）
//  4. UpdatePlayerAvatarFightProp 根据等级/突破/装备计算战斗属性
//  5. 通知客户端 AvatarAddNotify
//  6. 当前队伍未满 4 人时 自动加入队伍
func (g *Game) AddPlayerAvatar(userId uint32, avatarId uint32) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	// 角色是否已经存在
	dbAvatar := player.GetDbAvatar()
	avatar := dbAvatar.GetAvatarById(avatarId)
	if avatar != nil {
		return
	}
	// 添加角色
	dbAvatar.AddAvatar(player, avatarId)
	// 添加初始武器
	avatarDataConfig := gdconf.GetAvatarDataById(int32(avatarId))
	if avatarDataConfig == nil {
		logger.Error("config is nil, itemId: %v", avatarId)
		return
	}
	weaponId := g.AddPlayerWeapon(player.PlayerId, uint32(avatarDataConfig.InitialWeapon))
	// 角色装上初始武器
	g.WearPlayerAvatarWeapon(player.PlayerId, avatarId, weaponId)
	g.UpdatePlayerAvatarFightProp(player.PlayerId, avatarId)
	// 通知客户端
	ntf := &proto.AvatarAddNotify{
		Avatar:   g.PacketAvatarInfo(dbAvatar.GetAvatarById(avatarId)),
		IsInTeam: false,
	}
	g.SendMsg(cmd.AvatarAddNotify, userId, player.ClientSeq, ntf)
	// 加入队伍
	dbTeam := player.GetDbTeam()
	if len(dbTeam.GetActiveTeam().GetAvatarIdList()) >= 4 {
		return
	}
	activeTeam := dbTeam.GetActiveTeam()
	g.ChangeTeam(player, uint32(dbTeam.GetActiveTeamId()), append(activeTeam.GetAvatarIdList(), avatarId), dbTeam.GetActiveAvatarId())
}

// DelPlayerAvatar 删除玩家角色
func (g *Game) DelPlayerAvatar(userId uint32, avatarId uint32) {
	if avatarId == 10000005 || avatarId == 10000007 {
		// 主角不能删除
		return
	}
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	// 角色是否存在
	dbAvatar := player.GetDbAvatar()
	avatar := dbAvatar.GetAvatarById(avatarId)
	if avatar == nil {
		return
	}
	// 队伍中的角色不能删除
	dbTeam := player.GetDbTeam()
	for _, team := range dbTeam.GetTeamList() {
		for _, v := range team.GetAvatarIdList() {
			if avatarId == v {
				return
			}
		}
	}
	// 删除角色
	dbAvatar.DelAvatar(player, avatarId)
	// 通知客户端
	ntf := &proto.AvatarDelNotify{
		AvatarGuidList: []uint64{avatar.Guid},
	}
	g.SendMsg(cmd.AvatarDelNotify, userId, player.ClientSeq, ntf)
}

// AddPlayerFlycloak 给予玩家风之翼
func (g *Game) AddPlayerFlycloak(userId uint32, flyCloakId uint32) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	// 验证玩家是否已拥有该风之翼
	dbAvatar := player.GetDbAvatar()
	for _, flycloak := range dbAvatar.FlyCloakList {
		if flycloak == flyCloakId {
			return
		}
	}
	dbAvatar.FlyCloakList = append(dbAvatar.FlyCloakList, flyCloakId)

	avatarGainFlycloakNotify := &proto.AvatarGainFlycloakNotify{
		FlycloakId: flyCloakId,
	}
	g.SendMsg(cmd.AvatarGainFlycloakNotify, userId, player.ClientSeq, avatarGainFlycloakNotify)
}

// AddPlayerCostume 给予玩家时装
func (g *Game) AddPlayerCostume(userId uint32, costumeId uint32) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	// 验证玩家是否已拥有该时装
	dbAvatar := player.GetDbAvatar()
	for _, costume := range dbAvatar.CostumeList {
		if costume == costumeId {
			return
		}
	}
	dbAvatar.CostumeList = append(dbAvatar.CostumeList, costumeId)

	avatarGainCostumeNotify := &proto.AvatarGainCostumeNotify{
		CostumeId: costumeId,
	}
	g.SendMsg(cmd.AvatarGainCostumeNotify, userId, player.ClientSeq, avatarGainCostumeNotify)
}

// AddPlayerAvatarExp 增加玩家角色经验
func (g *Game) AddPlayerAvatarExp(userId uint32, avatarId uint32, exp uint32) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	dbAvatar := player.GetDbAvatar()
	avatar := dbAvatar.GetAvatarById(avatarId)
	// 获取角色配置表
	avatarDataConfig := gdconf.GetAvatarDataById(int32(avatar.AvatarId))
	if avatarDataConfig == nil {
		logger.Error("avatar config error, avatarId: %v", avatar.AvatarId)
		return
	}
	// 获取角色突破配置表
	avatarPromoteConfig := gdconf.GetAvatarPromoteDataByIdAndLevel(avatarDataConfig.PromoteId, int32(avatar.Promote))
	if avatarPromoteConfig == nil {
		logger.Error("avatar promote config error, promoteLevel: %v", avatar.Promote)
		return
	}
	// 角色增加经验
	newExp := avatar.Exp
	newLevel := avatar.Level
	newExp += exp
	// 角色升级
	for i := 0; i < 1000; i++ {
		// 获取角色等级配置表
		avatarLevelConfig := gdconf.GetAvatarLevelDataByLevel(int32(newLevel))
		if avatarLevelConfig == nil {
			// 获取不到代表已经到达最大等级
			break
		}
		// 角色当前等级未突破则跳出循环
		if newLevel >= uint8(avatarPromoteConfig.LevelLimit) {
			// 角色未突破溢出的经验处理
			newExp = 0
			break
		}
		// 角色经验小于升级所需的经验则跳出循环
		if newExp < uint32(avatarLevelConfig.Exp) {
			break
		}
		// 角色等级提升
		newExp -= uint32(avatarLevelConfig.Exp)
		newLevel++
	}
	g.SetPlayerAvatarLevelExpPromote(player.PlayerId, avatar.AvatarId, newLevel, newExp)
}

// SetPlayerAvatarLevelExpPromote 设置玩家角色等级经验突破
func (g *Game) SetPlayerAvatarLevelExpPromote(userId uint32, avatarId uint32, level uint8, exp uint32, promote ...uint8) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	dbAvatar := player.GetDbAvatar()
	avatar := dbAvatar.GetAvatarById(avatarId)
	avatarDataConfig := gdconf.GetAvatarDataById(int32(avatar.AvatarId))
	if avatarDataConfig == nil {
		logger.Error("avatar config error, avatarId: %v", avatar.AvatarId)
		return
	}
	if len(promote) == 0 {
		avatar.Level = level
		avatar.Exp = exp
		promoteLevel := int32(0)
		for i := 0; i < 1000; i++ {
			avatarPromoteConfig := gdconf.GetAvatarPromoteDataByIdAndLevel(avatarDataConfig.PromoteId, promoteLevel)
			if avatarPromoteConfig == nil {
				break
			}
			avatar.Promote = uint8(avatarPromoteConfig.PromoteLevel)
			if avatar.Level <= uint8(avatarPromoteConfig.LevelLimit) {
				break
			}
			promoteLevel++
		}
	} else {
		avatar.Promote = promote[0]
		avatarPromoteConfig := gdconf.GetAvatarPromoteDataByIdAndLevel(avatarDataConfig.PromoteId, int32(avatar.Promote))
		if avatarPromoteConfig == nil {
			logger.Error("avatar promote config error, promoteLevel: %v", avatar.Promote)
			return
		}
		avatar.Level = uint8(avatarPromoteConfig.LevelLimit)
		avatar.Exp = 0
	}
	// 角色更新面板
	g.UpdatePlayerAvatarFightProp(player.PlayerId, avatar.AvatarId)
	// 角色属性表更新通知
	g.SendMsg(cmd.AvatarPropNotify, player.PlayerId, player.ClientSeq, g.PacketAvatarPropNotify(avatar))
	g.AddPlayerAvatarHp(player.PlayerId, avatar.AvatarId, 0.0, 1.0, proto.ChangHpReason_CHANGE_HP_ADD_UPGRADE)
}

// UpdatePlayerAvatarFightProp 重新计算并下发玩家角色战斗属性
//
// 调用时机：升级/突破/穿装备/换天赋/换主角元素 等所有影响角色面板属性的事件
// 流程：
//  1. dbAvatar.UpdateAvatarFightProp 计算 = 基础属性（角色等级表）+ 突破属性 + 武器主词条 + 圣遗物词条
//     注意：天赋/命之座/精炼/套装效果 等需要 Ability 系统支持的部分**不在此计算**
//  2. NoCd 标志（GM "nocd" 命令）：技能冷却减免 100%（实战测试用）
//  3. 计算属性变化 diff 通过 EntityFightPropUpdateNotify 广播给场景内所有玩家
//
// AI 世界跳过：PUBG 玩法不需要面板属性 简化为固定值
func (g *Game) UpdatePlayerAvatarFightProp(userId uint32, avatarId uint32) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil || WORLD_MANAGER.IsAiWorld(world) {
		return
	}
	dbAvatar := player.GetDbAvatar()
	avatar := dbAvatar.GetAvatarById(avatarId)
	if avatar == nil {
		logger.Error("get avatar is nil, avatarId: %v", avatarId)
		return
	}
	oldFightPropMap := make(map[uint32]float32)
	for propType, propValue := range avatar.FightPropMap {
		oldFightPropMap[propType] = propValue
	}
	dbAvatar.UpdateAvatarFightProp(avatar)
	if player.NoCd {
		avatar.FightPropMap[constant.FIGHT_PROP_SKILL_CD_MINUS_RATIO] = float32(1.0)
	}
	updateFightPropMap := make(map[uint32]float32)
	for propType, newPropValue := range avatar.FightPropMap {
		oldPropValue := oldFightPropMap[propType]
		if newPropValue != oldPropValue {
			updateFightPropMap[propType] = newPropValue
		}
	}
	entityId := world.GetPlayerWorldAvatarEntityId(player, avatar.AvatarId)
	if entityId == 0 {
		return
	}
	scene := world.GetSceneById(player.GetSceneId())
	entity := scene.GetEntity(entityId)
	if entity == nil {
		logger.Error("get entity is nil, entityId: %v", entityId)
		return
	}
	entity.SetFightProp(avatar.FightPropMap)
	g.EntityFightPropUpdateNotifyBroadcast(scene, entity, updateFightPropMap)
}

// ChangePlayerAvatarSkillDepot 切换角色的 SkillDepot（技能库 决定使用哪种元素）
//
// 主要用于**主角七国元素切换**：旅行者每到一个国家可切换为对应元素
//   - 蒙德 → 风元素 → SkillDepot 504
//   - 璃月 → 岩元素 → SkillDepot 506
//   - 稻妻 → 雷元素 → SkillDepot 507
//   - ... 须弥/枫丹/纳塔/至冬
//
// 处理：
//  1. changeSkillDepotId=0 时按 elementType 自动查找对应的 SkillDepot
//  2. 修改 avatar.SkillDepotId + 重置元素能量条（按新元素的 MaxEnergy）
//  3. 广播 AvatarSkillDepotChangeNotify + AbilityChangeNotify（重新初始化能力）
func (g *Game) ChangePlayerAvatarSkillDepot(userId uint32, avatarId uint32, changeSkillDepotId uint32, elementType int) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		return
	}
	if changeSkillDepotId == 0 {
		avatarDataConfig := gdconf.GetAvatarDataById(int32(avatarId))
		if avatarDataConfig == nil {
			return
		}
		for _, skillDepotId := range avatarDataConfig.SkillDepotIdList {
			avatarSkillDepotDataConfig := gdconf.GetAvatarSkillDepotDataById(skillDepotId)
			if avatarSkillDepotDataConfig == nil {
				continue
			}
			avatarSkillDataConfig := gdconf.GetAvatarSkillDataById(avatarSkillDepotDataConfig.EnergySkill)
			if avatarSkillDataConfig == nil {
				continue
			}
			if avatarSkillDataConfig.CostElemType != int32(elementType) {
				continue
			}
			changeSkillDepotId = uint32(skillDepotId)
			break
		}
	}
	dbAvatar := player.GetDbAvatar()
	avatar := dbAvatar.GetAvatarById(avatarId)
	if avatar == nil {
		logger.Error("get avatar is nil, avatarId: %v", avatarId)
		return
	}

	dbAvatar.ChangeSkillDepot(avatarId, changeSkillDepotId)
	entityId := world.GetPlayerWorldAvatarEntityId(player, avatarId)

	g.SendMsg(cmd.AvatarSkillDepotChangeNotify, player.PlayerId, player.ClientSeq, &proto.AvatarSkillDepotChangeNotify{
		EntityId:      entityId,
		AvatarGuid:    avatar.Guid,
		SkillDepotId:  changeSkillDepotId,
		SkillLevelMap: avatar.SkillLevelMap,
	})

	g.SendMsg(cmd.AbilityChangeNotify, player.PlayerId, player.ClientSeq, &proto.AbilityChangeNotify{
		EntityId:            entityId,
		AbilityControlBlock: g.PacketAvatarAbilityControlBlock(avatar.AvatarId, changeSkillDepotId),
	})

	avatarSkillDataConfig := gdconf.GetAvatarEnergySkillConfig(avatar.SkillDepotId)
	if avatarSkillDataConfig == nil {
		return
	}
	fightPropEnergy := constant.ELEMENT_TYPE_FIGHT_PROP_ENERGY_MAP[int(avatarSkillDataConfig.CostElemType)]
	avatar.FightPropMap[uint32(fightPropEnergy.MaxEnergy)] = float32(avatarSkillDataConfig.CostElemVal)
	avatar.FightPropMap[uint32(fightPropEnergy.CurEnergy)] = 0.0

	scene := world.GetSceneById(player.GetSceneId())
	entity := scene.GetEntity(entityId)
	if entity == nil {
		logger.Error("get entity is nil, entityId: %v", entityId)
		return
	}
	g.EntityFightPropUpdateNotifyBroadcast(scene, entity, map[uint32]float32{
		uint32(fightPropEnergy.MaxEnergy): avatar.FightPropMap[uint32(fightPropEnergy.MaxEnergy)],
		uint32(fightPropEnergy.CurEnergy): avatar.FightPropMap[uint32(fightPropEnergy.CurEnergy)],
	})
}

// AddPlayerAvatarHp 角色加血 value=固定值 percent=按最大HP百分比（二选一）
// 与 SubEntityHp 的角色分支一致 单独写这个函数是因为加血逻辑稍有差异（无需触发HpDrop）
// 上限不超过 MAX_HP；变更后通过 EntityFightPropUpdateNotify 广播
func (g *Game) AddPlayerAvatarHp(userId uint32, avatarId uint32, value float32, percent float32, reason proto.ChangHpReason) {
	if value == 0.0 && percent == 0.0 {
		return
	}
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	dbAvatar := player.GetDbAvatar()
	avatar := dbAvatar.GetAvatarById(avatarId)
	if avatar == nil {
		logger.Error("get avatar is nil, avatarId: %v", avatarId)
		return
	}
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		logger.Error("get world is nil, worldId: %v, uid: %v", player.WorldId, player.PlayerId)
		return
	}
	entityId := world.GetPlayerWorldAvatarEntityId(player, avatarId)
	if entityId == 0 {
		return
	}
	scene := world.GetSceneById(player.GetSceneId())
	entity := scene.GetEntity(entityId)
	if entity == nil {
		logger.Error("get entity is nil, entityId: %v", entityId)
		return
	}
	fightProp := entity.GetFightProp()
	currHp := fightProp[constant.FIGHT_PROP_CUR_HP]
	maxHp := fightProp[constant.FIGHT_PROP_MAX_HP]
	lastHp := currHp
	if percent != 0.0 {
		currHp += maxHp * percent
	} else {
		currHp += value
	}
	if currHp > maxHp {
		currHp = maxHp
	}
	fightProp[constant.FIGHT_PROP_CUR_HP] = currHp
	g.EntityFightPropUpdateNotifyBroadcast(scene, entity, map[uint32]float32{
		constant.FIGHT_PROP_CUR_HP: fightProp[constant.FIGHT_PROP_CUR_HP],
	})
	g.SendMsg(cmd.EntityFightPropChangeReasonNotify, player.PlayerId, player.ClientSeq, &proto.EntityFightPropChangeReasonNotify{
		PropDelta:      currHp - lastHp,
		ChangeHpReason: reason,
		EntityId:       entity.GetId(),
		PropType:       constant.FIGHT_PROP_CUR_HP,
	})
}

// SubPlayerAvatarHp 角色扣血
func (g *Game) SubPlayerAvatarHp(userId uint32, avatarId uint32, value float32, percent float32, reason proto.ChangHpReason) {
	if value == 0.0 && percent == 0.0 {
		return
	}
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	if player.WuDi {
		return
	}
	dbAvatar := player.GetDbAvatar()
	avatar := dbAvatar.GetAvatarById(avatarId)
	if avatar == nil {
		logger.Error("get avatar is nil, avatarId: %v", avatarId)
		return
	}
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		logger.Error("get world is nil, worldId: %v, uid: %v", player.WorldId, player.PlayerId)
		return
	}
	scene := world.GetSceneById(player.GetSceneId())
	entityId := world.GetPlayerWorldAvatarEntityId(player, avatarId)
	if entityId == 0 {
		return
	}
	entity := scene.GetEntity(entityId)
	fightProp := entity.GetFightProp()
	currHp := fightProp[constant.FIGHT_PROP_CUR_HP]
	maxHp := fightProp[constant.FIGHT_PROP_MAX_HP]
	lastHp := currHp
	if percent != 0.0 {
		currHp -= maxHp * percent
	} else {
		currHp -= value
	}
	if currHp < 0.0 {
		currHp = 0.0
	}
	fightProp[constant.FIGHT_PROP_CUR_HP] = currHp
	g.EntityFightPropUpdateNotifyBroadcast(scene, entity, map[uint32]float32{
		constant.FIGHT_PROP_CUR_HP: fightProp[constant.FIGHT_PROP_CUR_HP],
	})
	g.SendMsg(cmd.EntityFightPropChangeReasonNotify, player.PlayerId, player.ClientSeq, &proto.EntityFightPropChangeReasonNotify{
		PropDelta:      currHp - lastHp,
		ChangeHpReason: reason,
		EntityId:       entity.GetId(),
		PropType:       constant.FIGHT_PROP_CUR_HP,
	})
	if currHp == 0.0 {
		dieType := g.GetPlayerDieTypeByChangHpReason(reason)
		g.KillEntity(player, scene, entity.GetId(), dieType)
	}
}

func (g *Game) GetPlayerDieTypeByChangHpReason(reason proto.ChangHpReason) proto.PlayerDieType {
	var dieType proto.PlayerDieType
	switch reason {
	case proto.ChangHpReason_CHANGE_HP_SUB_MONSTER:
		dieType = proto.PlayerDieType_PLAYER_DIE_KILL_BY_MONSTER
	case proto.ChangHpReason_CHANGE_HP_SUB_GEAR:
		dieType = proto.PlayerDieType_PLAYER_DIE_KILL_BY_GEAR
	case proto.ChangHpReason_CHANGE_HP_SUB_FALL:
		dieType = proto.PlayerDieType_PLAYER_DIE_FALL
	case proto.ChangHpReason_CHANGE_HP_SUB_DRAWN:
		dieType = proto.PlayerDieType_PLAYER_DIE_DRAWN
	case proto.ChangHpReason_CHANGE_HP_SUB_ABYSS:
		dieType = proto.PlayerDieType_PLAYER_DIE_ABYSS
	case proto.ChangHpReason_CHANGE_HP_SUB_GM:
		dieType = proto.PlayerDieType_PLAYER_DIE_GM
	case proto.ChangHpReason_CHANGE_HP_SUB_CLIMATE_COLD:
		dieType = proto.PlayerDieType_PLAYER_DIE_CLIMATE_COLD
	case proto.ChangHpReason_CHANGE_HP_SUB_STORM_LIGHTNING:
		dieType = proto.PlayerDieType_PLAYER_DIE_STORM_LIGHTING
	default:
		dieType = proto.PlayerDieType_PLAYER_DIE_GM
	}
	return dieType
}

// AddPlayerAvatarEnergy 角色恢复元素能量（攻击/拾取元素球时调用）
// max=true 直接充满；max=false 加 value（不超过 MaxEnergy）
// 元素类型决定 EnergyProp 索引（火/水/雷/冰/风/岩/草 各对应不同的 FIGHT_PROP_*_ENERGY）
func (g *Game) AddPlayerAvatarEnergy(userId uint32, avatarId uint32, value float32, max bool) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	dbAvatar := player.GetDbAvatar()
	avatar := dbAvatar.GetAvatarById(avatarId)
	if avatar == nil {
		logger.Error("get avatar is nil, avatarId: %v", avatarId)
		return
	}
	avatarSkillDataConfig := gdconf.GetAvatarEnergySkillConfig(avatar.SkillDepotId)
	if avatarSkillDataConfig == nil {
		logger.Error("get avatar energy skill is nil, skillDepotId: %v", avatar.SkillDepotId)
		return
	}
	fightPropEnergy := constant.ELEMENT_TYPE_FIGHT_PROP_ENERGY_MAP[int(avatarSkillDataConfig.CostElemType)]
	if max {
		avatar.FightPropMap[uint32(fightPropEnergy.CurEnergy)] = float32(avatarSkillDataConfig.CostElemVal)
	} else {
		avatar.FightPropMap[uint32(fightPropEnergy.CurEnergy)] += value
		if avatar.FightPropMap[uint32(fightPropEnergy.CurEnergy)] > float32(avatarSkillDataConfig.CostElemVal) {
			avatar.FightPropMap[uint32(fightPropEnergy.CurEnergy)] = float32(avatarSkillDataConfig.CostElemVal)
		}
	}

	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		logger.Error("get world is nil, worldId: %v, uid: %v", player.WorldId, player.PlayerId)
		return
	}
	scene := world.GetSceneById(player.SceneId)
	entityId := world.GetPlayerWorldAvatarEntityId(player, avatarId)
	entity := scene.GetEntity(entityId)
	if entity == nil {
		logger.Error("get entity is nil, entityId: %v", entityId)
		return
	}
	g.EntityFightPropUpdateNotifyBroadcast(scene, entity, map[uint32]float32{
		uint32(fightPropEnergy.CurEnergy): avatar.FightPropMap[uint32(fightPropEnergy.CurEnergy)],
	})
}

// CostPlayerAvatarEnergy 角色消耗元素能量（释放Q大招时调用）
// EnergyInf（GM "energyinf" 命令）：无限能量模式 直接跳过扣能
// max=true 清空能量（释放完整大招）；max=false 扣 value
func (g *Game) CostPlayerAvatarEnergy(userId uint32, avatarId uint32, value float32, max bool) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	if player.EnergyInf {
		return
	}
	dbAvatar := player.GetDbAvatar()
	avatar := dbAvatar.GetAvatarById(avatarId)
	if avatar == nil {
		logger.Error("get avatar is nil, avatarId: %v", avatarId)
		return
	}
	avatarSkillDataConfig := gdconf.GetAvatarEnergySkillConfig(avatar.SkillDepotId)
	if avatarSkillDataConfig == nil {
		logger.Error("get avatar energy skill is nil, skillDepotId: %v", avatar.SkillDepotId)
		return
	}
	fightPropEnergy := constant.ELEMENT_TYPE_FIGHT_PROP_ENERGY_MAP[int(avatarSkillDataConfig.CostElemType)]
	if max {
		avatar.FightPropMap[uint32(fightPropEnergy.CurEnergy)] = 0.0
	} else {
		avatar.FightPropMap[uint32(fightPropEnergy.CurEnergy)] -= value
		if avatar.FightPropMap[uint32(fightPropEnergy.CurEnergy)] < 0.0 {
			avatar.FightPropMap[uint32(fightPropEnergy.CurEnergy)] = 0.0
		}
	}

	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		logger.Error("get world is nil, worldId: %v, uid: %v", player.WorldId, player.PlayerId)
		return
	}
	scene := world.GetSceneById(player.SceneId)
	entityId := world.GetPlayerWorldAvatarEntityId(player, avatarId)
	entity := scene.GetEntity(entityId)
	if entity == nil {
		logger.Error("get entity is nil, entityId: %v", entityId)
		return
	}
	g.EntityFightPropUpdateNotifyBroadcast(scene, entity, map[uint32]float32{
		uint32(fightPropEnergy.CurEnergy): avatar.FightPropMap[uint32(fightPropEnergy.CurEnergy)],
	})
}

// RevivePlayerAvatar 复活玩家死亡角色（消耗复苏材料/七天神像复活/秘境失败复活等）
// 仅在 LifeState=DEAD 时执行：恢复 LifeState=ALIVE + 广播状态变更 + 回 35% 最大HP
// AI 世界（PUBG）的死亡复活走 ReLoginPlayer 不走这个函数（重新进 AI 世界等于重生）
func (g *Game) RevivePlayerAvatar(player *model.Player, avatarId uint32) {
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		return
	}
	scene := world.GetSceneById(player.GetSceneId())

	dbAvatar := player.GetDbAvatar()
	avatar := dbAvatar.GetAvatarById(avatarId)
	if avatar == nil {
		logger.Error("get avatar is nil, avatarId: %v", avatarId)
		return
	}

	if avatar.LifeState != constant.LIFE_STATE_DEAD {
		return
	}
	avatar.LifeState = constant.LIFE_STATE_ALIVE
	entityId := world.GetPlayerWorldAvatarEntityId(player, avatarId)
	entity := scene.GetEntity(entityId)
	if entity != nil {
		entity.SetLifeState(constant.LIFE_STATE_ALIVE)
	}

	ntf := &proto.AvatarLifeStateChangeNotify{
		AvatarGuid:      avatar.Guid,
		LifeState:       uint32(avatar.LifeState),
		DieType:         proto.PlayerDieType_PLAYER_DIE_NONE,
		MoveReliableSeq: 0,
	}
	g.SendToWorldA(world, cmd.AvatarLifeStateChangeNotify, 0, ntf, 0)

	g.AddPlayerAvatarHp(player.PlayerId, avatarId, 0.0, 0.35, proto.ChangHpReason_CHANGE_HP_ADD_REVIVE)
}

/************************************************** 打包封装 **************************************************/

// PacketAvatarInfo 打包角色信息
func (g *Game) PacketAvatarInfo(avatar *model.Avatar) *proto.AvatarInfo {
	pbAvatar := &proto.AvatarInfo{
		IsFocus:  false,
		AvatarId: avatar.AvatarId,
		Guid:     avatar.Guid,
		PropMap: map[uint32]*proto.PropValue{
			uint32(constant.PLAYER_PROP_LEVEL): {
				Type:  uint32(constant.PLAYER_PROP_LEVEL),
				Val:   int64(avatar.Level),
				Value: &proto.PropValue_Ival{Ival: int64(avatar.Level)},
			},
			uint32(constant.PLAYER_PROP_EXP): {
				Type:  uint32(constant.PLAYER_PROP_EXP),
				Val:   int64(avatar.Exp),
				Value: &proto.PropValue_Ival{Ival: int64(avatar.Exp)},
			},
			uint32(constant.PLAYER_PROP_BREAK_LEVEL): {
				Type:  uint32(constant.PLAYER_PROP_BREAK_LEVEL),
				Val:   int64(avatar.Promote),
				Value: &proto.PropValue_Ival{Ival: int64(avatar.Promote)},
			},
			uint32(constant.PLAYER_PROP_SATIATION_VAL): {
				Type:  uint32(constant.PLAYER_PROP_SATIATION_VAL),
				Val:   int64(avatar.Satiation),
				Value: &proto.PropValue_Ival{Ival: int64(avatar.Satiation)},
			},
			uint32(constant.PLAYER_PROP_SATIATION_PENALTY_TIME): {
				Type:  uint32(constant.PLAYER_PROP_SATIATION_PENALTY_TIME),
				Val:   int64(avatar.SatiationPenalty),
				Value: &proto.PropValue_Ival{Ival: int64(avatar.SatiationPenalty)},
			},
		},
		LifeState:     uint32(avatar.LifeState),
		EquipGuidList: object.ConvMapValueToList[uint64, uint64](avatar.EquipGuidMap),
		FightPropMap:  avatar.FightPropMap,
		SkillDepotId:  avatar.SkillDepotId,
		FetterInfo: &proto.AvatarFetterInfo{
			ExpLevel:                uint32(avatar.FetterLevel),
			ExpNumber:               avatar.FetterExp,
			FetterList:              nil,
			RewardedFetterLevelList: []uint32{10},
		},
		SkillLevelMap:            avatar.SkillLevelMap,
		TalentIdList:             avatar.TalentIdList,
		InherentProudSkillList:   gdconf.GetAvatarInherentProudSkillList(avatar.SkillDepotId, avatar.Promote),
		AvatarType:               1,
		WearingFlycloakId:        avatar.FlyCloak,
		CostumeId:                avatar.Costume,
		BornTime:                 uint32(avatar.BornTime),
		PendingPromoteRewardList: make([]uint32, 0, len(avatar.PromoteRewardMap)),
	}
	for _, v := range avatar.FetterList {
		pbAvatar.FetterInfo.FetterList = append(pbAvatar.FetterInfo.FetterList, &proto.FetterData{
			FetterId:    v,
			FetterState: constant.FETTER_STATE_FINISH,
		})
	}
	// 解锁全部资料
	for _, v := range gdconf.GetFetterIdListByAvatarId(int32(avatar.AvatarId)) {
		pbAvatar.FetterInfo.FetterList = append(pbAvatar.FetterInfo.FetterList, &proto.FetterData{
			FetterId:    uint32(v),
			FetterState: constant.FETTER_STATE_FINISH,
		})
	}
	// 突破等级奖励
	for promoteLevel, isTaken := range avatar.PromoteRewardMap {
		if !isTaken {
			pbAvatar.PendingPromoteRewardList = append(pbAvatar.PendingPromoteRewardList, promoteLevel)
		}
	}
	return pbAvatar
}

// PacketAvatarPropNotify 角色属性表更新通知
func (g *Game) PacketAvatarPropNotify(avatar *model.Avatar) *proto.AvatarPropNotify {
	avatarPropNotify := &proto.AvatarPropNotify{
		PropMap:    make(map[uint32]int64, 5),
		AvatarGuid: avatar.Guid,
	}
	// 角色等级
	avatarPropNotify.PropMap[uint32(constant.PLAYER_PROP_LEVEL)] = int64(avatar.Level)
	// 角色经验
	avatarPropNotify.PropMap[uint32(constant.PLAYER_PROP_EXP)] = int64(avatar.Exp)
	// 角色突破等级
	avatarPropNotify.PropMap[uint32(constant.PLAYER_PROP_BREAK_LEVEL)] = int64(avatar.Promote)
	// 角色饱食度
	avatarPropNotify.PropMap[uint32(constant.PLAYER_PROP_SATIATION_VAL)] = int64(avatar.Satiation)
	// 角色饱食度溢出
	avatarPropNotify.PropMap[uint32(constant.PLAYER_PROP_SATIATION_PENALTY_TIME)] = int64(avatar.SatiationPenalty)

	return avatarPropNotify
}

// PacketAvatarDataNotify 角色数据通知
func (g *Game) PacketAvatarDataNotify(player *model.Player) *proto.AvatarDataNotify {
	dbAvatar := player.GetDbAvatar()
	dbTeam := player.GetDbTeam()
	avatarDataNotify := &proto.AvatarDataNotify{
		CurAvatarTeamId:   uint32(dbTeam.GetActiveTeamId()),
		ChooseAvatarGuid:  dbAvatar.GetAvatarById(dbAvatar.MainCharAvatarId).Guid,
		OwnedFlycloakList: dbAvatar.FlyCloakList,
		// 角色衣装
		OwnedCostumeList: dbAvatar.CostumeList,
		AvatarList:       make([]*proto.AvatarInfo, 0),
		AvatarTeamMap:    make(map[uint32]*proto.AvatarTeam),
	}
	for _, avatar := range dbAvatar.GetAvatarMap() {
		pbAvatar := g.PacketAvatarInfo(avatar)
		avatarDataNotify.AvatarList = append(avatarDataNotify.AvatarList, pbAvatar)
	}
	for teamIndex, team := range dbTeam.TeamList {
		var teamAvatarGuidList []uint64 = nil
		for _, avatarId := range team.GetAvatarIdList() {
			teamAvatarGuidList = append(teamAvatarGuidList, dbAvatar.GetAvatarById(avatarId).Guid)
		}
		avatarDataNotify.AvatarTeamMap[uint32(teamIndex)+1] = &proto.AvatarTeam{
			AvatarGuidList: teamAvatarGuidList,
			TeamName:       team.Name,
		}
	}
	return avatarDataNotify
}
