package game

import (
	"hk4e/common/constant"
	"hk4e/gdconf"
	"hk4e/gs/model"
	"hk4e/protocol/cmd"
	"hk4e/protocol/proto"

	"github.com/flswld/halo/logger"
	pb "google.golang.org/protobuf/proto"
)

// 装备穿戴 模块
//
// 装备分两类：武器（Weapon）+ 圣遗物（Reliquary）
// 角色的装备槽位：1 把武器 + 5 件圣遗物（花/羽/沙/杯/冠）
//
// 三个核心请求：
//   - SetEquipLockStateReq: 上锁/解锁（防止误销毁/误作精炼素材）
//   - TakeoffEquipReq: 卸下圣遗物（武器不能空槽 必须用 WearEquip 替换）
//   - WearEquipReq: 穿戴武器/圣遗物
//
// 装备交换的复杂逻辑：
//   - 武器在另一个角色身上 → 双向交换（A 装备 B 的武器 同时把 A 原武器给 B）
//   - 圣遗物在另一个角色身上 → 同样双向交换 但圣遗物可以被卸下放回背包

/************************************************** 接口请求 **************************************************/

// SetEquipLockStateReq 装备上锁/解锁请求（武器和圣遗物通用）
// 上锁后该装备不能被销毁/作精炼素材/作圣遗物升级素材 防止玩家误操作
func (g *Game) SetEquipLockStateReq(player *model.Player, payloadMsg pb.Message) {
	req := payloadMsg.(*proto.SetEquipLockStateReq)

	// 获取目标装备
	equipGameObj, ok := player.GameObjectGuidMap[req.TargetEquipGuid]
	if !ok {
		logger.Error("equip error, equipGuid: %v", req.TargetEquipGuid)
		g.SendError(cmd.SetEquipLockStateRsp, player, &proto.SetEquipLockStateRsp{}, proto.Retcode_RET_ITEM_NOT_EXIST)
		return
	}
	switch equipGameObj.(type) {
	case *model.Weapon:
		weapon := equipGameObj.(*model.Weapon)
		weapon.Lock = req.IsLocked
		// 更新武器的物品数据
		g.SendMsg(cmd.StoreItemChangeNotify, player.PlayerId, player.ClientSeq, g.PacketStoreItemChangeNotifyByWeapon(weapon))
	case *model.Reliquary:
		reliquary := equipGameObj.(*model.Reliquary)
		reliquary.Lock = req.IsLocked
		// 更新圣遗物的物品数据
		g.SendMsg(cmd.StoreItemChangeNotify, player.PlayerId, player.ClientSeq, g.PacketStoreItemChangeNotifyByReliquary(reliquary))
	default:
		logger.Error("equip type error, equipGuid: %v", req.TargetEquipGuid)
		g.SendError(cmd.SetEquipLockStateRsp, player, &proto.SetEquipLockStateRsp{})
		return
	}

	setEquipLockStateRsp := &proto.SetEquipLockStateRsp{
		TargetEquipGuid: req.TargetEquipGuid,
		IsLocked:        req.IsLocked,
	}
	g.SendMsg(cmd.SetEquipLockStateRsp, player.PlayerId, player.ClientSeq, setEquipLockStateRsp)
}

// TakeoffEquipReq 卸下圣遗物请求
// 武器不能卸下（角色必须有武器才能战斗）→ 客户端不会发武器的 TakeoffEquipReq
// 卸下后调 UpdatePlayerAvatarFightProp 重算面板（少了圣遗物的副词条）
func (g *Game) TakeoffEquipReq(player *model.Player, payloadMsg pb.Message) {
	req := payloadMsg.(*proto.TakeoffEquipReq)

	// 获取目标角色
	avatar, ok := player.GameObjectGuidMap[req.AvatarGuid].(*model.Avatar)
	if !ok {
		logger.Error("avatar error, avatarGuid: %v", req.AvatarGuid)
		g.SendError(cmd.TakeoffEquipRsp, player, &proto.TakeoffEquipRsp{}, proto.Retcode_RET_CAN_NOT_FIND_AVATAR)
		return
	}
	// 确保角色已装备指定位置的圣遗物
	reliquary, ok := avatar.EquipReliquaryMap[uint8(req.Slot)]
	if !ok {
		logger.Error("avatar not wear reliquary, slot: %v", req.Slot)
		g.SendError(cmd.TakeoffEquipRsp, player, &proto.TakeoffEquipRsp{})
		return
	}
	// 卸下圣遗物
	dbAvatar := player.GetDbAvatar()
	dbAvatar.TakeOffReliquary(avatar.AvatarId, reliquary)
	// 角色更新面板
	g.UpdatePlayerAvatarFightProp(player.PlayerId, avatar.AvatarId)
	// 更新玩家装备
	avatarEquipChangeNotify := g.PacketAvatarEquipChangeNotifyByReliquary(avatar, uint8(req.Slot))
	g.SendMsg(cmd.AvatarEquipChangeNotify, player.PlayerId, player.ClientSeq, avatarEquipChangeNotify)

	takeoffEquipRsp := &proto.TakeoffEquipRsp{
		AvatarGuid: req.AvatarGuid,
		Slot:       req.Slot,
	}
	g.SendMsg(cmd.TakeoffEquipRsp, player.PlayerId, player.ClientSeq, takeoffEquipRsp)
}

// WearEquipReq 穿戴装备请求（武器和圣遗物通用）
//
// 武器特殊校验：weaponConfig.EquipType 必须等于 avatarConfig.WeaponType
//
//	防止单手剑角色装上双手剑（客户端理论上也不会发但服务端兜底校验）
//
// 圣遗物自动按 ReliquaryType（1=花/2=羽/3=沙/4=杯/5=冠）放到对应槽位
// 装备完成后必须调 UpdatePlayerAvatarFightProp 重算面板
func (g *Game) WearEquipReq(player *model.Player, payloadMsg pb.Message) {
	req := payloadMsg.(*proto.WearEquipReq)

	// 获取目标角色
	avatar, ok := player.GameObjectGuidMap[req.AvatarGuid].(*model.Avatar)
	if !ok {
		logger.Error("avatar error, avatarGuid: %v", req.AvatarGuid)
		g.SendError(cmd.WearEquipRsp, player, &proto.WearEquipRsp{}, proto.Retcode_RET_CAN_NOT_FIND_AVATAR)
		return
	}
	// 获取角色配置表
	avatarConfig := gdconf.GetAvatarDataById(int32(avatar.AvatarId))
	if avatarConfig == nil {
		logger.Error("avatar config error, avatarId: %v", avatar.AvatarId)
		return
	}
	// 获取目标装备
	equipGameObj, ok := player.GameObjectGuidMap[req.EquipGuid]
	if !ok {
		logger.Error("equip error, equipGuid: %v", req.EquipGuid)
		g.SendError(cmd.WearEquipRsp, player, &proto.WearEquipRsp{}, proto.Retcode_RET_ITEM_NOT_EXIST)
		return
	}
	switch equipGameObj.(type) {
	case *model.Weapon:
		weapon := equipGameObj.(*model.Weapon)
		// 获取武器配置表
		weaponConfig := gdconf.GetItemDataById(int32(weapon.ItemId))
		if weaponConfig == nil {
			logger.Error("weapon config error, itemId: %v", weapon.ItemId)
			return
		}
		// 校验装备的武器类型是否匹配
		if weaponConfig.EquipType != avatarConfig.WeaponType {
			logger.Error("weapon type error, weaponType: %v", weaponConfig.EquipType)
			g.SendError(cmd.WearEquipRsp, player, &proto.WearEquipRsp{})
			return
		}
		g.WearPlayerAvatarWeapon(player.PlayerId, avatar.AvatarId, weapon.WeaponId)
	case *model.Reliquary:
		reliquary := equipGameObj.(*model.Reliquary)
		g.WearPlayerAvatarReliquary(player.PlayerId, avatar.AvatarId, reliquary.ReliquaryId)
	default:
		logger.Error("equip type error, equipGuid: %v", req.EquipGuid)
		g.SendError(cmd.WearEquipRsp, player, &proto.WearEquipRsp{})
		return
	}
	// 角色更新面板
	g.UpdatePlayerAvatarFightProp(player.PlayerId, avatar.AvatarId)

	wearEquipRsp := &proto.WearEquipRsp{
		AvatarGuid: req.AvatarGuid,
		EquipGuid:  req.EquipGuid,
	}
	g.SendMsg(cmd.WearEquipRsp, player.PlayerId, player.ClientSeq, wearEquipRsp)
}

/************************************************** 游戏功能 **************************************************/

// WearPlayerAvatarReliquary 玩家角色装备圣遗物（核心交换逻辑）
//
// 三种情况：
//  1. 圣遗物在别的角色身上 + 当前角色对应槽位有装备：
//     · 卸下当前角色已装备的圣遗物
//     · 把对方角色卸下目标圣遗物
//     · 把"当前角色卸下的圣遗物"装到对方角色对应槽位（避免对方空一格）
//     · 把目标圣遗物装到当前角色
//  2. 圣遗物在别的角色身上 + 当前角色对应槽位无装备：
//     · 直接从对方角色卸下 → 装到当前角色（对方留空）
//  3. 圣遗物在背包 + 当前角色对应槽位有装备：
//     · 卸下当前角色的圣遗物（自动放回背包）→ 装上目标圣遗物
//
// 注意：通知会同时发给"装备调整后的两个角色"双方
func (g *Game) WearPlayerAvatarReliquary(userId uint32, avatarId uint32, reliquaryId uint64) {
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
	dbReliquary := player.GetDbReliquary()
	reliquary := dbReliquary.GetReliquaryById(reliquaryId)
	if reliquary == nil {
		logger.Error("get reliquary is nil, reliquaryId: %v", reliquaryId)
		return
	}
	// 获取圣遗物配置表
	reliquaryConfig := gdconf.GetItemDataById(int32(reliquary.ItemId))
	if reliquaryConfig == nil {
		logger.Error("reliquary config error, itemId: %v", reliquary.ItemId)
		return
	}
	// 角色已装备的圣遗物
	avatarCurReliquary := avatar.EquipReliquaryMap[uint8(reliquaryConfig.ReliquaryType)]
	if reliquary.AvatarId != 0 {
		// 圣遗物在别的角色身上
		targetReliquaryAvatar := dbAvatar.GetAvatarById(reliquary.AvatarId)
		if targetReliquaryAvatar == nil {
			logger.Error("get avatar is nil, avatarId: %v", reliquary.AvatarId)
			return
		}
		// 确保目前角色已装备圣遗物
		if avatarCurReliquary != nil {
			// 卸下角色已装备的圣遗物
			dbAvatar.TakeOffReliquary(avatarId, avatarCurReliquary)
			// 将目标圣遗物的角色装备当前角色曾装备的圣遗物
			dbAvatar.WearReliquary(targetReliquaryAvatar.AvatarId, avatarCurReliquary)
		}
		// 将目标圣遗物的角色卸下圣遗物
		dbAvatar.TakeOffReliquary(targetReliquaryAvatar.AvatarId, reliquary)

		// 更新目标圣遗物角色的装备
		avatarEquipChangeNotify := g.PacketAvatarEquipChangeNotifyByReliquary(targetReliquaryAvatar, uint8(reliquaryConfig.ReliquaryType))
		g.SendMsg(cmd.AvatarEquipChangeNotify, userId, player.ClientSeq, avatarEquipChangeNotify)
	} else if avatarCurReliquary != nil {
		// 角色当前有圣遗物则卸下
		dbAvatar.TakeOffReliquary(avatarId, avatarCurReliquary)
	}
	// 角色装备圣遗物
	dbAvatar.WearReliquary(avatarId, reliquary)

	// 更新角色装备
	avatarEquipChangeNotify := g.PacketAvatarEquipChangeNotifyByReliquary(avatar, uint8(reliquaryConfig.ReliquaryType))
	g.SendMsg(cmd.AvatarEquipChangeNotify, userId, player.ClientSeq, avatarEquipChangeNotify)
}

// WearPlayerAvatarWeapon 玩家角色装备武器（与圣遗物略有不同）
//
// 武器关键约束：**武器不能空** 角色必须有武器才能战斗
// 所以武器交换时必须确保两边都有武器才能交换：
//  1. 武器在别的角色身上 + 当前角色有武器：双向交换两人各自的武器
//  2. 武器在背包 + 当前角色有武器：卸下当前武器（放回背包）→ 装上目标武器
//  3. 角色没有武器（如 AddPlayerAvatar 的初始状态）：直接装备目标武器
//
// 装备后场景中的武器实体也要更新 → 走 PacketAvatarEquipChangeNotifyByWeapon 带 entityId
func (g *Game) WearPlayerAvatarWeapon(userId uint32, avatarId uint32, weaponId uint64) {
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
	dbWeapon := player.GetDbWeapon()
	weapon := dbWeapon.GetWeaponById(weaponId)
	if weapon == nil {
		logger.Error("get weapon is nil, weaponId: %v", weaponId)
		return
	}
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		logger.Error("get world is nil, worldId: %v, uid: %v", player.WorldId, player.PlayerId)
		return
	}
	// 角色已装备的武器
	avatarCurWeapon := avatar.EquipWeapon
	// 武器需要确保双方都装备才能替换不然会出问题
	if avatarCurWeapon != nil {
		if weapon.AvatarId != 0 {
			// 武器在别的角色身上
			targetWeaponAvatar := dbAvatar.GetAvatarById(weapon.AvatarId)
			if targetWeaponAvatar == nil {
				logger.Error("get avatar is nil, avatarId: %v", weapon.AvatarId)
				return
			}
			// 卸下角色已装备的武器
			dbAvatar.TakeOffWeapon(avatarId, avatarCurWeapon)

			// 将目标武器的角色卸下武器
			dbAvatar.TakeOffWeapon(targetWeaponAvatar.AvatarId, weapon)
			// 将目标武器的角色装备当前角色曾装备的武器
			dbAvatar.WearWeapon(targetWeaponAvatar.AvatarId, avatarCurWeapon)

			// 更新目标武器角色的装备
			weaponEntityId := uint32(0)
			worldAvatar := world.GetPlayerWorldAvatar(player, targetWeaponAvatar.AvatarId)
			if worldAvatar != nil {
				weaponEntityId = worldAvatar.GetWeaponEntityId()
			}
			avatarEquipChangeNotify := g.PacketAvatarEquipChangeNotifyByWeapon(targetWeaponAvatar, targetWeaponAvatar.EquipWeapon, weaponEntityId)
			g.SendMsg(cmd.AvatarEquipChangeNotify, userId, player.ClientSeq, avatarEquipChangeNotify)
		} else {
			// 角色当前有武器则卸下
			dbAvatar.TakeOffWeapon(avatarId, avatarCurWeapon)
		}
	}
	// 角色装备武器
	dbAvatar.WearWeapon(avatarId, weapon)

	// 更新角色装备
	weaponEntityId := uint32(0)
	worldAvatar := world.GetPlayerWorldAvatar(player, avatarId)
	if worldAvatar != nil {
		weaponEntityId = worldAvatar.GetWeaponEntityId()
	}
	avatarEquipChangeNotify := g.PacketAvatarEquipChangeNotifyByWeapon(avatar, weapon, weaponEntityId)
	g.SendMsg(cmd.AvatarEquipChangeNotify, userId, player.ClientSeq, avatarEquipChangeNotify)
}

/************************************************** 打包封装 **************************************************/

func (g *Game) PacketAvatarEquipChangeNotifyByReliquary(avatar *model.Avatar, slot uint8) *proto.AvatarEquipChangeNotify {
	// 获取角色对应位置的圣遗物
	reliquary, ok := avatar.EquipReliquaryMap[slot]
	if !ok {
		// 没有则代表卸下
		avatarEquipChangeNotify := &proto.AvatarEquipChangeNotify{
			AvatarGuid: avatar.Guid,
			EquipType:  uint32(slot),
		}
		return avatarEquipChangeNotify
	}
	reliquaryConfig := gdconf.GetItemDataById(int32(reliquary.ItemId))
	if reliquaryConfig == nil {
		logger.Error("reliquary config error, itemId: %v", reliquary.ItemId)
		return new(proto.AvatarEquipChangeNotify)
	}
	avatarEquipChangeNotify := &proto.AvatarEquipChangeNotify{
		AvatarGuid: avatar.Guid,
		ItemId:     reliquary.ItemId,
		EquipGuid:  reliquary.Guid,
		EquipType:  uint32(reliquaryConfig.ReliquaryType),
		Reliquary: &proto.SceneReliquaryInfo{
			ItemId:       reliquary.ItemId,
			Guid:         reliquary.Guid,
			Level:        uint32(reliquary.Level),
			PromoteLevel: uint32(reliquary.Promote),
		},
	}
	return avatarEquipChangeNotify
}

func (g *Game) PacketAvatarEquipChangeNotifyByWeapon(avatar *model.Avatar, weapon *model.Weapon, entityId uint32) *proto.AvatarEquipChangeNotify {
	weaponConfig := gdconf.GetItemDataById(int32(weapon.ItemId))
	if weaponConfig == nil {
		logger.Error("weapon config error, itemId: %v", weapon.ItemId)
		return new(proto.AvatarEquipChangeNotify)
	}
	affixMap := make(map[uint32]uint32)
	for _, affixId := range weapon.AffixIdList {
		affixMap[affixId] = uint32(weapon.Refinement)
	}
	avatarEquipChangeNotify := &proto.AvatarEquipChangeNotify{
		AvatarGuid: avatar.Guid,
		ItemId:     weapon.ItemId,
		EquipGuid:  weapon.Guid,
		EquipType:  constant.EQUIP_TYPE_WEAPON,
		Weapon: &proto.SceneWeaponInfo{
			EntityId:     entityId,
			GadgetId:     uint32(weaponConfig.GadgetId),
			ItemId:       weapon.ItemId,
			Guid:         weapon.Guid,
			Level:        uint32(weapon.Level),
			PromoteLevel: uint32(weapon.Promote),
			AbilityInfo:  new(proto.AbilitySyncStateInfo),
			AffixMap:     affixMap,
		},
	}
	return avatarEquipChangeNotify
}
