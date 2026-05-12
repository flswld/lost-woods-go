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

// 背包物品 模块
//
// 物品分两大类：
//   - 实体物品（普通材料/食物/任务道具）：存在 dbItem.itemMap 里 有 ChangeCount 数量
//   - 虚拟物品（原石/摩拉/经验/树脂）：存 PropMap 不创建 Item 对象 通过 VIRTUAL_ITEM_PROP 映射
//
// 使用入口：
//   - UseItemReq: 客户端使用物品请求（背包里点"使用"）
//   - UseItem: 内部 API（任务奖励/邮件领取等都走这个）
//
// 已实现的 ItemUseOption（共 8 种）：
//   - GAIN_AVATAR: 角色试用券 → 解锁角色（已拥有则给命之座道具）
//   - RELIVE_AVATAR: 复活材料 → 复活指定角色
//   - ADD_SERVER_BUFF: 服务端 buff（"草泥马回血要走ability" 注释暴露 ability 系统未实现）
//   - GAIN_FLYCLOAK: 风之翼
//   - GAIN_NAME_CARD: 名片
//   - GAIN_COSTUME: 衣装/时装
//   - ADD_ELEM_ENERGY: 加元素能量（同元素全量 异元素半量 后排 60%）
//   - ADD_ALL_ENERGY: 全队加能量
//
// AddPlayerItem/CostPlayerItem 是物品增减的统一入口 涉及 PropMap 修改 + 通知客户端

/************************************************** 接口请求 **************************************************/

// UseItemReq 使用物品请求（背包→使用）
//
// 处理：
//  1. 校验玩家有这个物品
//  2. 一次性扣 req.Count 个物品
//  3. 循环调 UseItem 每次使用一个（按物品 ItemUseList 配置触发对应效果）
//
// targetGuid 仅复活材料/弓箭专用券等需要选择目标的物品才用
func (g *Game) UseItemReq(player *model.Player, payloadMsg pb.Message) {
	req := payloadMsg.(*proto.UseItemReq)
	// 是否拥有物品
	item, exist := player.GameObjectGuidMap[req.Guid].(*model.Item)
	if !exist {
		logger.Error("item not exist, weaponGuid: %v", req.Guid)
		g.SendError(cmd.UseItemRsp, player, &proto.UseItemRsp{}, proto.Retcode_RET_ITEM_NOT_EXIST)
		return
	}
	// 消耗物品
	ok := g.CostPlayerItem(player.PlayerId, []*ChangeItem{{ItemId: item.ItemId, ChangeCount: req.Count}})
	if !ok {
		logger.Error("item count not enough, uid: %v", player.PlayerId)
		g.SendError(cmd.UseItemRsp, player, &proto.UseItemRsp{}, proto.Retcode_RET_ITEM_COUNT_NOT_ENOUGH)
		return
	}

	for count := uint32(0); count < req.Count; count++ {
		g.UseItem(player.PlayerId, item.ItemId, req.TargetGuid)
	}

	rsp := &proto.UseItemRsp{
		Guid:       req.Guid,
		TargetGuid: req.TargetGuid,
		ItemId:     item.ItemId,
		OptionIdx:  req.OptionIdx,
	}
	g.SendMsg(cmd.UseItemRsp, player.PlayerId, player.ClientSeq, rsp)
}

/************************************************** 游戏功能 **************************************************/

// UseItem 使用物品（核心实现 按 ItemUseList 配置分支处理 8 种 UseOption）
//
// 同一物品可能有多个 ItemUse（如食物可能同时回血+加buff）
// EndlessLoopCheck 防止递归（用了 A 物品给 B 物品 → B 又触发 A 等情况）
//
// **特殊**：行 94 注释 "草泥马回血要走ability" 暴露问题——
//
//	食物回血在原版通过 ability buff 实现 但项目 ability 系统不完整 → 食物无法回血
//	作者用粗口注释表达对这个限制的不满
func (g *Game) UseItem(userId uint32, itemId uint32, targetParam ...uint64) {
	g.EndlessLoopCheck(EndlessLoopCheckTypeUseItem)
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	itemDataConfig := gdconf.GetItemDataById(int32(itemId))
	if itemDataConfig == nil {
		logger.Error("item data config is nil, itemId: %v", itemId)
		return
	}
	for _, itemUse := range itemDataConfig.ItemUseList {
		switch itemUse.UseOption {
		case constant.ITEM_USE_GAIN_AVATAR:
			// 获得角色
			if len(itemUse.UseParam) != 1 {
				continue
			}
			avatarId, err := strconv.Atoi(itemUse.UseParam[0])
			if err != nil {
				continue
			}
			dbAvatar := player.GetDbAvatar()
			avatar := dbAvatar.GetAvatarById(uint32(avatarId))
			if avatar == nil {
				g.AddPlayerAvatar(userId, uint32(avatarId))
			} else {
				g.AddPlayerItem(userId, []*ChangeItem{{ItemId: itemId + 100, ChangeCount: 1}}, proto.ActionReasonType_ACTION_REASON_SUBFIELD_DROP)
			}
		case constant.ITEM_USE_RELIVE_AVATAR:
			// 复活角色
			if len(targetParam) != 1 {
				continue
			}
			avatar, exist := player.GameObjectGuidMap[targetParam[0]].(*model.Avatar)
			if !exist {
				logger.Error("avatar not exist, avatarGuid: %v", targetParam[0])
				continue
			}
			g.RevivePlayerAvatar(player, avatar.AvatarId)
		case constant.ITEM_USE_ADD_SERVER_BUFF:
			// 草泥马回血要走ability
		case constant.ITEM_USE_GAIN_FLYCLOAK:
			// 获得风之翼
			if len(itemUse.UseParam) != 1 {
				continue
			}
			flyCloakId, err := strconv.Atoi(itemUse.UseParam[0])
			if err != nil {
				continue
			}
			g.AddPlayerFlycloak(userId, uint32(flyCloakId))
		case constant.ITEM_USE_GAIN_NAME_CARD:
			// 获得名片
			if len(itemUse.UseParam) != 0 {
				continue
			}
			g.AddPlayerNameCard(userId, itemId)
		case constant.ITEM_USE_GAIN_COSTUME:
			// 获得衣装
			if len(itemUse.UseParam) != 1 {
				continue
			}
			costumeId, err := strconv.Atoi(itemUse.UseParam[0])
			if err != nil {
				continue
			}
			g.AddPlayerCostume(userId, uint32(costumeId))
		case constant.ITEM_USE_ADD_ELEM_ENERGY:
			// 添加元素能量
			if len(itemUse.UseParam) != 3 {
				continue
			}
			elementType, err := strconv.Atoi(itemUse.UseParam[0])
			if err != nil {
				continue
			}
			sameEnergy, err := strconv.Atoi(itemUse.UseParam[1])
			if err != nil {
				continue
			}
			otherEnergy, err := strconv.Atoi(itemUse.UseParam[2])
			if err != nil {
				continue
			}
			world := WORLD_MANAGER.GetWorldById(player.WorldId)
			if world == nil {
				continue
			}
			dbAvatar := player.GetDbAvatar()
			activeAvatarId := world.GetPlayerActiveAvatarId(player)
			for _, worldAvatar := range world.GetPlayerWorldAvatarList(player) {
				addEnergy := float32(0.0)
				if dbAvatar.GetAvatarElementType(worldAvatar.GetAvatarId()) == elementType {
					addEnergy = float32(sameEnergy)
				} else {
					addEnergy = float32(otherEnergy)
				}
				if worldAvatar.GetAvatarId() != activeAvatarId {
					addEnergy *= 0.6
				}
				g.AddPlayerAvatarEnergy(player.PlayerId, worldAvatar.GetAvatarId(), addEnergy, false)
			}
		case constant.ITEM_USE_ADD_ALL_ENERGY:
			// 添加全体元素能量
			if len(itemUse.UseParam) != 1 {
				continue
			}
			addEnergy, err := strconv.Atoi(itemUse.UseParam[0])
			if err != nil {
				continue
			}
			world := WORLD_MANAGER.GetWorldById(player.WorldId)
			if world == nil {
				continue
			}
			for _, worldAvatar := range world.GetPlayerWorldAvatarList(player) {
				g.AddPlayerAvatarEnergy(player.PlayerId, worldAvatar.GetAvatarId(), float32(addEnergy), false)
			}
		default:
			// logger.Error("use option not support, useOption: %v, uid: %v", itemUse.UseOption, userId)
		}
	}
}

// GetAllItemDataConfig 获取所有物品数据配置表
func (g *Game) GetAllItemDataConfig() map[int32]*gdconf.ItemData {
	allItemDataConfig := make(map[int32]*gdconf.ItemData)
	for itemId, itemDataConfig := range gdconf.GetItemDataMap() {
		if itemDataConfig.Type != constant.ITEM_TYPE_VIRTUAL &&
			itemDataConfig.Type != constant.ITEM_TYPE_MATERIAL &&
			itemDataConfig.Type != constant.ITEM_TYPE_FURNITURE {
			continue
		}
		allItemDataConfig[itemId] = itemDataConfig
	}
	return allItemDataConfig
}

// GetPlayerItemCount 获取玩家某物品数量（虚拟物品走 PropMap 实体物品走 dbItem.itemMap）
// 虚拟物品如：原石(itemId=201)→PROP_PLAYER_HCOIN, 摩拉(202)→PROP_PLAYER_SCOIN, 树脂(106)→PROP_PLAYER_RESIN
func (g *Game) GetPlayerItemCount(userId uint32, itemId uint32) uint32 {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return 0
	}
	prop, ok := constant.VIRTUAL_ITEM_PROP[itemId]
	if ok {
		value := player.PropMap[prop]
		return value
	} else {
		dbItem := player.GetDbItem()
		value := dbItem.GetItemCount(itemId)
		return value
	}
}

type ChangeItem struct {
	ItemId      uint32
	ChangeCount uint32
}

// AddPlayerItem 添加玩家物品（最常用的物品入口 任务奖励/秘境/邮件/掉落都走这里）
//
// 处理：
//  1. 合并 itemList 中相同 itemId（如同时给 100+200 摩拉 合并成 300）
//  2. 按 ItemType 分支：
//     · WEAPON: 调 AddPlayerWeapon 创建武器（每件单独 GUID）
//     · RELIQUARY: 调 AddPlayerReliquary 创建圣遗物（每件单独 GUID）
//     · VIRTUAL: 走 PropMap +=（如原石/摩拉直接加属性）
//     · MATERIAL/FURNITURE: 走 dbItem.AddItem 计数+1（同 itemId 共享一个 GUID）
//  3. AutoUse=1 的物品（如战令经验书）→ 自动调 UseItem 立即使用
//  4. 触发 QUEST_FINISH_COND_TYPE_OBTAIN_ITEM 任务推进（如"获得 5 个甜甜花"任务）
//  5. 特殊物品：玩家经验 → AddPlayerExp 升冒险等级；角色经验 → 给当前出战角色加经验
//  6. 通知客户端：StoreItemChangeNotify（背包变化）+ ItemAddHintNotify（屏幕飘字提示）
func (g *Game) AddPlayerItem(userId uint32, itemList []*ChangeItem, hintReason proto.ActionReasonType) bool {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return false
	}
	itemMap := make(map[uint32]uint32)
	for _, changeItem := range itemList {
		itemMap[changeItem.ItemId] += changeItem.ChangeCount
	}
	dbItem := player.GetDbItem()
	propList := make([]uint32, 0)
	changeNtf := &proto.StoreItemChangeNotify{
		StoreType: proto.StoreType_STORE_PACK,
		ItemList:  make([]*proto.Item, 0),
	}
	addHintNtf := &proto.ItemAddHintNotify{
		Reason:   uint32(hintReason),
		ItemList: make([]*proto.ItemHint, 0),
	}
	for itemId, addCount := range itemMap {
		itemDataConfig := gdconf.GetItemDataById(int32(itemId))
		if itemDataConfig == nil {
			continue
		}
		if object.ConvInt64ToBool(int64(itemDataConfig.AutoUse)) {
			continue
		}
		pbItemHint := &proto.ItemHint{
			ItemId: itemId,
			Count:  addCount,
			Guid:   dbItem.GetItemGuid(itemId),
		}
		addHintNtf.ItemList = append(addHintNtf.ItemList, pbItemHint)
		switch itemDataConfig.Type {
		case constant.ITEM_TYPE_WEAPON:
			g.AddPlayerWeapon(player.PlayerId, itemId)
		case constant.ITEM_TYPE_RELIQUARY:
			g.AddPlayerReliquary(player.PlayerId, itemId, 0, nil)
		case constant.ITEM_TYPE_VIRTUAL, constant.ITEM_TYPE_MATERIAL, constant.ITEM_TYPE_FURNITURE:
			prop, exist := constant.VIRTUAL_ITEM_PROP[itemId]
			if exist {
				// 物品为虚拟物品 角色属性物品数量增加
				player.PropMap[prop] += addCount
				propList = append(propList, prop)
			} else {
				// 物品为普通物品 直接进背包
				// 校验背包物品容量 目前物品包括材料和家具
				if dbItem.GetItemMapLen() > constant.STORE_PACK_LIMIT_MATERIAL+constant.STORE_PACK_LIMIT_FURNITURE {
					return false
				}
				dbItem.AddItem(player, itemId, addCount)
			}
			pbItem := &proto.Item{
				ItemId: itemId,
				Guid:   dbItem.GetItemGuid(itemId),
				Detail: &proto.Item_Material{
					Material: &proto.Material{
						Count: dbItem.GetItemCount(itemId),
					},
				},
			}
			changeNtf.ItemList = append(changeNtf.ItemList, pbItem)
		}
	}
	if len(propList) > 0 {
		g.SendMsg(cmd.PlayerPropNotify, userId, player.ClientSeq, g.PacketPlayerPropNotify(player, propList...))
	}
	if len(changeNtf.ItemList) > 0 {
		g.SendMsg(cmd.StoreItemChangeNotify, userId, player.ClientSeq, changeNtf)
	}
	if len(addHintNtf.ItemList) > 0 {
		if hintReason != proto.ActionReasonType_ACTION_REASON_NONE {
			g.SendMsg(cmd.ItemAddHintNotify, userId, player.ClientSeq, addHintNtf)
		}
	}
	for itemId, addCount := range itemMap {
		g.TriggerQuest(player, constant.QUEST_FINISH_COND_TYPE_OBTAIN_ITEM, "", int32(itemId), int32(addCount))
		itemDataConfig := gdconf.GetItemDataById(int32(itemId))
		if itemDataConfig == nil {
			continue
		}
		if object.ConvInt64ToBool(int64(itemDataConfig.AutoUse)) {
			for count := uint32(0); count < addCount; count++ {
				g.UseItem(userId, itemId)
			}
		}
		// 特殊属性变化处理函数
		switch itemId {
		case constant.ITEM_ID_PLAYER_EXP:
			// 冒险阅历
			g.AddPlayerExp(userId)
		case constant.ITEM_ID_AVATAR_EXP:
			// 角色经验
			world := WORLD_MANAGER.GetWorldById(player.WorldId)
			if world != nil {
				activeAvatarId := world.GetPlayerActiveAvatarId(player)
				g.AddPlayerAvatarExp(player.PlayerId, activeAvatarId, addCount)
			}
		}
	}
	return true
}

// CostPlayerItem 消耗玩家物品（统一物品扣减入口）
//
// 处理：
//  1. 检查所有物品数量足够（不足返回 false 不扣任何物品）
//  2. 数量足够时执行扣减：
//     · 虚拟物品：PropMap -=
//     · 实体物品：dbItem.CostItem 扣计数 数量为 0 时移除
//  3. 通知客户端：PlayerPropNotify（虚拟物品变化）+ StoreItemChangeNotify（实体物品变化）+ StoreItemDelNotify（数量为 0 的物品）
//
// 与 AddPlayerItem 配套使用 升级/突破/合成的"扣材料"都走这里
func (g *Game) CostPlayerItem(userId uint32, itemList []*ChangeItem) bool {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return false
	}
	itemMap := make(map[uint32]uint32)
	for _, changeItem := range itemList {
		itemMap[changeItem.ItemId] += changeItem.ChangeCount
	}
	dbItem := player.GetDbItem()
	propList := make([]uint32, 0)
	changeNtf := &proto.StoreItemChangeNotify{
		StoreType: proto.StoreType_STORE_PACK,
		ItemList:  make([]*proto.Item, 0),
	}
	delNtf := &proto.StoreItemDelNotify{
		StoreType: proto.StoreType_STORE_PACK,
		GuidList:  make([]uint64, 0),
	}
	for itemId, costCount := range itemMap {
		// 检查剩余道具数量
		count := g.GetPlayerItemCount(player.PlayerId, itemId)
		if count < costCount {
			return false
		} else if count == costCount {
			delNtf.GuidList = append(delNtf.GuidList, dbItem.GetItemGuid(itemId))
		}
		prop, exist := constant.VIRTUAL_ITEM_PROP[itemId]
		if exist {
			// 物品为虚拟物品 角色属性物品数量减少
			player.PropMap[prop] -= costCount
			propList = append(propList, prop)
		} else {
			// 物品为普通物品 直接扣除
			dbItem.CostItem(player, itemId, costCount)
		}
		count = g.GetPlayerItemCount(player.PlayerId, itemId)
		pbItem := &proto.Item{
			ItemId: itemId,
			Guid:   dbItem.GetItemGuid(itemId),
			Detail: &proto.Item_Material{
				Material: &proto.Material{
					Count: count,
				},
			},
		}
		changeNtf.ItemList = append(changeNtf.ItemList, pbItem)
	}
	if len(propList) > 0 {
		g.SendMsg(cmd.PlayerPropNotify, userId, player.ClientSeq, g.PacketPlayerPropNotify(player, propList...))
	}
	if len(changeNtf.ItemList) > 0 {
		g.SendMsg(cmd.StoreItemChangeNotify, userId, player.ClientSeq, changeNtf)
	}
	if len(delNtf.GuidList) > 0 {
		g.SendMsg(cmd.StoreItemDelNotify, userId, player.ClientSeq, delNtf)
	}
	return true
}

// RewardItem 奖励玩家物品
func (g *Game) RewardItem(userId uint32, rewardId uint32, hintReason proto.ActionReasonType) bool {
	rewardConfig := gdconf.GetRewardDataById(int32(rewardId))
	if rewardConfig == nil {
		logger.Error("reward config error, rewardId: %v", rewardId)
		return false
	}
	rewardItemList := make([]*ChangeItem, 0, len(rewardConfig.RewardItemMap))
	for itemId, count := range rewardConfig.RewardItemMap {
		rewardItemList = append(rewardItemList, &ChangeItem{
			ItemId:      itemId,
			ChangeCount: count,
		})
	}
	g.AddPlayerItem(userId, rewardItemList, hintReason)
	return true
}

/************************************************** 打包封装 **************************************************/

// PacketStoreWeightLimitNotify 背包容量限制通知
func (g *Game) PacketStoreWeightLimitNotify() *proto.StoreWeightLimitNotify {
	storeWeightLimitNotify := &proto.StoreWeightLimitNotify{
		StoreType: proto.StoreType_STORE_PACK,
		// 背包容量限制
		WeightLimit:         constant.STORE_PACK_LIMIT_WEIGHT,
		WeaponCountLimit:    constant.STORE_PACK_LIMIT_WEAPON,
		ReliquaryCountLimit: constant.STORE_PACK_LIMIT_RELIQUARY,
		MaterialCountLimit:  constant.STORE_PACK_LIMIT_MATERIAL,
		FurnitureCountLimit: constant.STORE_PACK_LIMIT_FURNITURE,
	}
	return storeWeightLimitNotify
}

// PacketPlayerStoreNotify 玩家背包内容通知
func (g *Game) PacketPlayerStoreNotify(player *model.Player) *proto.PlayerStoreNotify {
	dbItem := player.GetDbItem()
	dbWeapon := player.GetDbWeapon()
	dbReliquary := player.GetDbReliquary()
	playerStoreNotify := &proto.PlayerStoreNotify{
		StoreType:   proto.StoreType_STORE_PACK,
		WeightLimit: constant.STORE_PACK_LIMIT_WEIGHT,
		ItemList:    make([]*proto.Item, 0, dbItem.GetItemMapLen()+dbWeapon.GetWeaponMapLen()+dbReliquary.GetReliquaryMapLen()),
	}
	for _, weapon := range dbWeapon.GetWeaponMap() {
		itemDataConfig := gdconf.GetItemDataById(int32(weapon.ItemId))
		if itemDataConfig == nil {
			logger.Error("get item data config is nil, itemId: %v", weapon.ItemId)
			continue
		}
		if itemDataConfig.Type != constant.ITEM_TYPE_WEAPON {
			continue
		}
		affixMap := make(map[uint32]uint32)
		for _, affixId := range weapon.AffixIdList {
			affixMap[affixId] = uint32(weapon.Refinement)
		}
		pbItem := &proto.Item{
			ItemId: weapon.ItemId,
			Guid:   weapon.Guid,
			Detail: &proto.Item_Equip{
				Equip: &proto.Equip{
					Detail: &proto.Equip_Weapon{
						Weapon: &proto.Weapon{
							Level:        uint32(weapon.Level),
							Exp:          weapon.Exp,
							PromoteLevel: uint32(weapon.Promote),
							AffixMap:     affixMap,
						},
					},
					IsLocked: weapon.Lock,
				},
			},
		}
		playerStoreNotify.ItemList = append(playerStoreNotify.ItemList, pbItem)
	}
	for _, reliquary := range dbReliquary.GetReliquaryMap() {
		itemDataConfig := gdconf.GetItemDataById(int32(reliquary.ItemId))
		if itemDataConfig == nil {
			logger.Error("get item data config is nil, itemId: %v", reliquary.ItemId)
			continue
		}
		if itemDataConfig.Type != constant.ITEM_TYPE_RELIQUARY {
			continue
		}
		pbItem := &proto.Item{
			ItemId: reliquary.ItemId,
			Guid:   reliquary.Guid,
			Detail: &proto.Item_Equip{
				Equip: &proto.Equip{
					Detail: &proto.Equip_Reliquary{
						Reliquary: &proto.Reliquary{
							Level:            uint32(reliquary.Level),
							Exp:              reliquary.Exp,
							PromoteLevel:     uint32(reliquary.Promote),
							MainPropId:       reliquary.MainPropId,
							AppendPropIdList: reliquary.AppendPropIdList,
						},
					},
					IsLocked: reliquary.Lock,
				},
			},
		}
		playerStoreNotify.ItemList = append(playerStoreNotify.ItemList, pbItem)
	}
	for _, item := range dbItem.GetItemMap() {
		itemDataConfig := gdconf.GetItemDataById(int32(item.ItemId))
		if itemDataConfig == nil {
			logger.Error("get item data config is nil, itemId: %v", item.ItemId)
			continue
		}
		pbItem := &proto.Item{
			ItemId: item.ItemId,
			Guid:   item.Guid,
			Detail: nil,
		}
		if itemDataConfig != nil && itemDataConfig.Type == constant.ITEM_TYPE_FURNITURE {
			pbItem.Detail = &proto.Item_Furniture{
				Furniture: &proto.Furniture{
					Count: item.Count,
				},
			}
		} else {
			pbItem.Detail = &proto.Item_Material{
				Material: &proto.Material{
					Count:      item.Count,
					DeleteInfo: nil,
				},
			}
		}
		playerStoreNotify.ItemList = append(playerStoreNotify.ItemList, pbItem)
	}
	return playerStoreNotify
}
