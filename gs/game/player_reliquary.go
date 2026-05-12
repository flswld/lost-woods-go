package game

import (
	"strconv"

	"hk4e/common/constant"
	"hk4e/gdconf"
	"hk4e/gs/model"
	"hk4e/pkg/random"
	"hk4e/protocol/cmd"
	"hk4e/protocol/proto"

	"github.com/flswld/halo/logger"
	pb "google.golang.org/protobuf/proto"
)

// 圣遗物养成 模块
//
// 主要养成：
//   - 升级（ReliquaryUpgradeReq）：精通经验之冠/初阶/高阶 + 低星圣遗物作素材
//   - 装备（player_equip.go 的 WearPlayerAvatarReliquary）：5 个槽位 - 花/羽/沙/杯/冠
//   - 追加词条：每升 4 级（4/8/12/16/20）有几率刷新一个新副词条 5 星最多 4 个副词条
//
// **现状**：
//   - 升级/词条计算完整 经验暴击概率与官方一致（1% × 5 倍 / 8% × 2 倍 / 91% × 1 倍）
//   - 主属性/副词条参与战斗属性计算（FightProp 计算包含圣遗物词条）
//   - **2/4 件套套装效果不生效**：套装效果（如"风套2件套带4件套加暴伤"）依赖 Ability 系统
//   - ReliquaryPromoteReq 直接返回空响应（突破到 21 级是消耗大圣遗物经验之冠 项目未实现）

const (
	RELIQUARY_APPEND_PROP_MAX = 4   // 副词条最大数量（5 星上限）
	RELIQUARY_CONV_EXP        = 0.8 // 圣遗物作素材的经验转换率（80% 自身经验返还）
)

/************************************************** 接口请求 **************************************************/

// ReliquaryUpgradeReq 圣遗物升级请求
//
// 处理：
//  1. 经验来源两类：
//     · 经验材料（精通经验之冠 4001/初阶 4002/低阶 4003）：从 ItemUseList 取 ADD_RELIQUARY_EXP
//     · 圣遗物作素材：基础经验 BaseConvExp + 已有经验 × 0.8（有"经验返还"机制）
//  2. 经验暴击：1% 概率 × 5 倍 / 8% 概率 × 2 倍 / 91% 概率 × 1 倍（与官服一致）
//  3. 摩拉成本 = 总经验数（1:1 比例）
//  4. 按等级表 reliquaryLevelConfig.Exp 升级 多余经验保留到下次
//  5. 追加副词条：每升 4 级（PropAddLevel = 4/8/12/16/20）调用 AppendReliquaryProp 追加 1 词条
//  6. 装备时 → UpdatePlayerAvatarFightProp 重算面板
func (g *Game) ReliquaryUpgradeReq(player *model.Player, payloadMsg pb.Message) {
	req := payloadMsg.(*proto.ReliquaryUpgradeReq)
	reliquary, ok := player.GameObjectGuidMap[req.TargetReliquaryGuid].(*model.Reliquary)
	if !ok {
		logger.Error("reliquary guid not exist, targetReliquaryGuid: %v, uid: %v", req.TargetReliquaryGuid, player.PlayerId)
		g.SendError(cmd.ReliquaryUpgradeRsp, player, &proto.ReliquaryUpgradeRsp{})
		return
	}
	// 计算总经验
	totalAddExp := uint32(0)
	totalCostSCoin := uint32(0)
	if len(req.ItemParamList) != 0 {
		// 经验材料
		itemList := make([]*ChangeItem, 0)
		for _, itemParam := range req.ItemParamList {
			itemList = append(itemList, &ChangeItem{
				ItemId:      itemParam.ItemId,
				ChangeCount: itemParam.Count,
			})
			itemConfig := gdconf.GetItemDataById(int32(itemParam.ItemId))
			if itemConfig == nil {
				logger.Error("itemConfig is nil, itemId: %v", itemParam.ItemId)
				g.SendError(cmd.ReliquaryUpgradeRsp, player, &proto.ReliquaryUpgradeRsp{}, proto.Retcode_RET_NOT_FOUND_CONFIG)
				return
			}
			for _, itemUse := range itemConfig.ItemUseList {
				if itemUse.UseOption == constant.ITEM_USE_ADD_RELIQUARY_EXP {
					exp, err := strconv.Atoi(itemUse.UseParam[0])
					if err != nil {
						logger.Error("item use param format error: %v", err)
						g.SendError(cmd.ReliquaryUpgradeRsp, player, &proto.ReliquaryUpgradeRsp{}, proto.Retcode_RET_NOT_FOUND_CONFIG)
						return
					}
					totalAddExp += uint32(exp)
					totalCostSCoin += uint32(exp)
				}
			}
		}
		ok := g.CostPlayerItem(player.PlayerId, itemList)
		if !ok {
			logger.Error("item count not enough, itemList: %v, uid: %v", itemList, player.PlayerId)
			g.SendError(cmd.ReliquaryUpgradeRsp, player, &proto.ReliquaryUpgradeRsp{}, proto.Retcode_RET_ITEM_COUNT_NOT_ENOUGH)
			return
		}
	}
	if len(req.FoodReliquaryGuidList) != 0 {
		// 其他圣遗物
		reliquaryIdList := make([]uint64, 0)
		for _, foodReliquaryGuid := range req.FoodReliquaryGuidList {
			foodReliquary, ok := player.GameObjectGuidMap[foodReliquaryGuid].(*model.Reliquary)
			if !ok {
				logger.Error("food reliquary guid not exist, foodReliquaryGuid: %v, uid: %v", foodReliquaryGuid, player.PlayerId)
				g.SendError(cmd.ReliquaryUpgradeRsp, player, &proto.ReliquaryUpgradeRsp{})
				return
			}
			foodReliquaryConfig := gdconf.GetItemDataById(int32(foodReliquary.ItemId))
			if foodReliquaryConfig == nil {
				logger.Error("foodReliquaryConfig is nil, itemId: %v", foodReliquary.ItemId)
				g.SendError(cmd.ReliquaryUpgradeRsp, player, &proto.ReliquaryUpgradeRsp{}, proto.Retcode_RET_NOT_FOUND_CONFIG)
				return
			}
			totalAddExp += uint32(foodReliquaryConfig.BaseConvExp)
			totalCostSCoin += uint32(foodReliquaryConfig.BaseConvExp)
			foodExp := uint32(0)
			for level := uint8(1); level < foodReliquary.Level; level++ {
				reliquaryLevelConfig := gdconf.GetReliquaryLevelDataByStageAndLevel(foodReliquaryConfig.Stage, int32(level))
				if reliquaryLevelConfig == nil {
					logger.Error("reliquaryLevelConfig is nil, stage: %v, level: %v", foodReliquaryConfig.Stage, level)
					g.SendError(cmd.ReliquaryUpgradeRsp, player, &proto.ReliquaryUpgradeRsp{}, proto.Retcode_RET_NOT_FOUND_CONFIG)
					return
				}
				foodExp += uint32(reliquaryLevelConfig.Exp)
			}
			foodExp += foodReliquary.Exp
			totalAddExp += uint32(float32(foodExp) * RELIQUARY_CONV_EXP)
			reliquaryIdList = append(reliquaryIdList, foodReliquary.ReliquaryId)
		}
		g.CostPlayerReliquary(player.PlayerId, reliquaryIdList)
	}
	// 消耗金币
	ok = g.CostPlayerItem(player.PlayerId, []*ChangeItem{{ItemId: constant.ITEM_ID_SCOIN, ChangeCount: totalCostSCoin}})
	if !ok {
		logger.Error("item count not enough, totalCostSCoin: %v, uid: %v", totalCostSCoin, player.PlayerId)
		g.SendError(cmd.ReliquaryUpgradeRsp, player, &proto.ReliquaryUpgradeRsp{}, proto.Retcode_RET_ITEM_COUNT_NOT_ENOUGH)
		return
	}
	// 经验暴击
	powerUpRate := uint32(1)
	rn := random.GetRandomFloat32(0.0, 100.0)
	if rn < 1.0 {
		powerUpRate = 5
	} else if rn < 9.0 {
		powerUpRate = 2
	}
	totalAddExp *= powerUpRate
	// 升级前数据
	oldLevel := reliquary.Level
	oldAppendPropList := make([]uint32, 0)
	for _, appendPropId := range reliquary.AppendPropIdList {
		oldAppendPropList = append(oldAppendPropList, appendPropId)
	}
	// 升级
	reliquaryConfig := gdconf.GetItemDataById(int32(reliquary.ItemId))
	if reliquaryConfig == nil {
		logger.Error("reliquaryConfig is nil, itemId: %v", reliquary.ItemId)
		g.SendError(cmd.ReliquaryUpgradeRsp, player, &proto.ReliquaryUpgradeRsp{}, proto.Retcode_RET_NOT_FOUND_CONFIG)
		return
	}
	reliquary.Exp += totalAddExp
	for i := 0; i < 1000; i++ {
		reliquaryLevelConfig := gdconf.GetReliquaryLevelDataByStageAndLevel(reliquaryConfig.Stage, int32(reliquary.Level))
		if reliquaryLevelConfig == nil {
			logger.Error("reliquaryLevelConfig is nil, stage: %v, level: %v", reliquaryConfig.Stage, reliquary.Level)
			g.SendError(cmd.ReliquaryUpgradeRsp, player, &proto.ReliquaryUpgradeRsp{}, proto.Retcode_RET_NOT_FOUND_CONFIG)
			return
		}
		if reliquary.Exp < uint32(reliquaryLevelConfig.Exp) {
			break
		}
		reliquary.Exp -= uint32(reliquaryLevelConfig.Exp)
		reliquary.Level++
	}
	// 追加词条
	for level := oldLevel + 1; level <= reliquary.Level; level++ {
		for _, propAddLevel := range reliquaryConfig.PropAddLevel {
			if int32(level) == propAddLevel {
				g.AppendReliquaryProp(reliquary, 1)
			}
		}
	}
	g.SendMsg(cmd.StoreItemChangeNotify, player.PlayerId, player.ClientSeq, g.PacketStoreItemChangeNotifyByReliquary(reliquary))
	// 获取持有该圣遗物的角色
	dbAvatar := player.GetDbAvatar()
	avatar := dbAvatar.GetAvatarById(reliquary.AvatarId)
	// 圣遗物可能没被任何角色装备 仅在被装备时更新面板
	if avatar != nil {
		// 角色更新面板
		g.UpdatePlayerAvatarFightProp(player.PlayerId, avatar.AvatarId)
	}
	rsp := &proto.ReliquaryUpgradeRsp{
		OldLevel:            uint32(oldLevel),
		CurLevel:            uint32(reliquary.Level),
		TargetReliquaryGuid: req.TargetReliquaryGuid,
		CurAppendPropList:   reliquary.AppendPropIdList,
		PowerUpRate:         powerUpRate,
		OldAppendPropList:   oldAppendPropList,
	}
	g.SendMsg(cmd.ReliquaryUpgradeRsp, player.PlayerId, player.ClientSeq, rsp)
}

// ReliquaryPromoteReq 圣遗物突破请求（**未实现** 直接返回空响应）
// 圣遗物突破=升满 20 级后再上一级（21级）需要消耗大圣遗物经验 项目作者没做这部分
func (g *Game) ReliquaryPromoteReq(player *model.Player, payloadMsg pb.Message) {
	req := payloadMsg.(*proto.ReliquaryPromoteReq)
	logger.Debug("ReliquaryPromoteReq: %+v", req)
	g.SendMsg(cmd.ReliquaryPromoteRsp, player.PlayerId, player.ClientSeq, new(proto.ReliquaryPromoteRsp))
}

/************************************************** 游戏功能 **************************************************/

func (g *Game) GetAllReliquaryDataConfig() map[int32]*gdconf.ItemData {
	allReliquaryDataConfig := make(map[int32]*gdconf.ItemData)
	for itemId, itemData := range gdconf.GetItemDataMap() {
		if itemData.Type != constant.ITEM_TYPE_RELIQUARY {
			continue
		}
		allReliquaryDataConfig[itemId] = itemData
	}
	return allReliquaryDataConfig
}

// GetReliquaryMainDataRandomByDepotId 按主属性词库 ID 随机一个主属性
// 走 RWS（按权重轮盘抽签）：花/羽固定 HP/ATK 一种 沙/杯/冠的权重各异
func (g *Game) GetReliquaryMainDataRandomByDepotId(mainPropDepotId int32) *gdconf.ReliquaryMainData {
	mainPropMap := gdconf.GetReliquaryMainDataMapByDepotId(mainPropDepotId)
	if mainPropMap == nil {
		return nil
	}
	weightAll := int32(0)
	mainPropList := make([]*gdconf.ReliquaryMainData, 0)
	for _, data := range mainPropMap {
		weightAll += data.RandomWeight
		mainPropList = append(mainPropList, data)
	}
	randNum := random.GetRandomInt32(0, weightAll-1)
	sumWeight := int32(0)
	// RWS随机
	for _, data := range mainPropList {
		sumWeight += data.RandomWeight
		if sumWeight > randNum {
			return data
		}
	}
	return nil
}

// AddPlayerReliquary 给予玩家圣遗物（GM 命令/秘境奖励/合成）
//
// 参数：
//   - mainPropId=0 时按 ItemData.MainPropDepotId 随机抽主属性
//   - appendPropIdList=nil 时按 AppendPropCount 配置随机生成初始副词条数量
//
// 流程：扣容量 → 创建 Reliquary 对象 → 调 AppendReliquaryProp 生成初始词条 → 通知客户端
func (g *Game) AddPlayerReliquary(userId uint32, itemId uint32, mainPropId uint32, appendPropIdList []uint32) uint64 {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return 0
	}
	reliquaryConfig := gdconf.GetItemDataById(int32(itemId))
	if reliquaryConfig == nil {
		logger.Error("reliquary config error, itemId: %v", itemId)
		return 0
	}
	reliquaryId := uint64(g.snowflake.GenId())
	// 圣遗物主属性
	if mainPropId == 0 {
		reliquaryMainConfig := g.GetReliquaryMainDataRandomByDepotId(reliquaryConfig.MainPropDepotId)
		if reliquaryMainConfig == nil {
			logger.Error("reliquary main config error, mainPropDepotId: %v", reliquaryConfig.MainPropDepotId)
			return 0
		}
		mainPropId = uint32(reliquaryMainConfig.MainPropId)
	}
	// 玩家添加圣遗物
	dbReliquary := player.GetDbReliquary()
	// 校验背包圣遗物容量
	if dbReliquary.GetReliquaryMapLen() > constant.STORE_PACK_LIMIT_RELIQUARY {
		return 0
	}
	dbReliquary.AddReliquary(player, itemId, reliquaryId, mainPropId)
	reliquary := dbReliquary.GetReliquary(reliquaryId)
	if reliquary == nil {
		logger.Error("reliquary is nil, itemId: %v, reliquaryId: %v", itemId, reliquaryId)
		return 0
	}
	// 设置圣遗物初始词条
	if len(appendPropIdList) == 0 {
		g.AppendReliquaryProp(reliquary, reliquaryConfig.AppendPropCount)
	} else {
		reliquary.AppendPropIdList = appendPropIdList
	}
	g.SendMsg(cmd.StoreItemChangeNotify, userId, player.ClientSeq, g.PacketStoreItemChangeNotifyByReliquary(reliquary))
	return reliquaryId
}

func (g *Game) GetReliquaryAffixDataRandomByDepotId(appendPropDepotId int32, excludeMainPropType uint32, excludeAppendPropIdList []uint32) *gdconf.ReliquaryAffixData {
	appendPropMap := gdconf.GetReliquaryAffixDataMapByDepotId(appendPropDepotId)
	if appendPropMap == nil {
		return nil
	}
	weightAll := int32(0)
	appendPropList := make([]*gdconf.ReliquaryAffixData, 0)
	for _, data := range appendPropMap {
		// 排除列表中的属性类型是否相同
		if excludeMainPropType == uint32(data.PropType) {
			continue
		}
		exist := false
		for _, excludeAppendPropId := range excludeAppendPropIdList {
			if uint32(data.AppendPropId) == excludeAppendPropId {
				exist = true
				break
			}
		}
		if exist {
			continue
		}
		weightAll += data.RandomWeight
		appendPropList = append(appendPropList, data)
	}
	randNum := random.GetRandomInt32(0, weightAll-1)
	sumWeight := int32(0)
	// RWS随机
	for _, data := range appendPropList {
		sumWeight += data.RandomWeight
		if sumWeight > randNum {
			return data
		}
	}
	return nil
}

// AppendReliquaryProp 圣遗物追加副词条
//
// 两种逻辑：
//   - 副词条数 < 4: 解锁新词条 排除已有词条 + 主属性词条
//   - 副词条数 = 4: 强化现有词条之一 从 4 个已有词条中随机选 1 个加强（最多 6 次强化机会）
//
// 走 RWS 随机：每个副词条配 RandomWeight 不同词条出现概率不同
// 注意：与主属性同类型的词条会被排除（如花的主属性是HP固定 不会再出HP副词条）
func (g *Game) AppendReliquaryProp(reliquary *model.Reliquary, count int32) {
	// 获取圣遗物配置表
	reliquaryConfig := gdconf.GetItemDataById(int32(reliquary.ItemId))
	if reliquaryConfig == nil {
		logger.Error("reliquary config error, itemId: %v", reliquary.ItemId)
		return
	}
	// 主属性配置表
	reliquaryMainConfig := gdconf.GetReliquaryMainDataByPropId(int32(reliquary.MainPropId))
	if reliquaryMainConfig == nil {
		logger.Error("reliquary main config error, mainPropDepotId: %v, propId: %v", reliquaryConfig.MainPropDepotId, reliquary.MainPropId)
		return
	}
	// 圣遗物追加属性的次数
	for i := 0; i < int(count); i++ {
		// 排除追加的属性
		excludeAppendPropIdList := make([]uint32, 0)
		if len(reliquary.AppendPropIdList) < RELIQUARY_APPEND_PROP_MAX {
			// 解锁新的不重复词条
			for _, appendPropId := range reliquary.AppendPropIdList {
				excludeAppendPropIdList = append(excludeAppendPropIdList, appendPropId)
			}
		} else {
			// 强化现有词条
			for _, reliquaryAffixConfig := range gdconf.GetReliquaryAffixDataMapByDepotId(reliquaryConfig.AppendPropDepotId) {
				exist := false
				for _, appendPropId := range reliquary.AppendPropIdList {
					if uint32(reliquaryAffixConfig.AppendPropId) == appendPropId {
						exist = true
						break
					}
				}
				if !exist {
					excludeAppendPropIdList = append(excludeAppendPropIdList, uint32(reliquaryAffixConfig.AppendPropId))
				}
			}
		}
		// 将要添加的属性
		appendAffixConfig := g.GetReliquaryAffixDataRandomByDepotId(reliquaryConfig.AppendPropDepotId, uint32(reliquaryMainConfig.PropType), excludeAppendPropIdList)
		if appendAffixConfig == nil {
			logger.Error("append affix config error, appendPropDepotId: %v", reliquaryConfig.AppendPropDepotId)
			return
		}
		// 圣遗物添加词条
		reliquary.AppendPropIdList = append(reliquary.AppendPropIdList, uint32(appendAffixConfig.AppendPropId))
	}
}

func (g *Game) CostPlayerReliquary(userId uint32, reliquaryIdList []uint64) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	storeItemDelNotify := &proto.StoreItemDelNotify{
		GuidList:  make([]uint64, 0, len(reliquaryIdList)),
		StoreType: proto.StoreType_STORE_PACK,
	}
	dbReliquary := player.GetDbReliquary()
	for _, reliquaryId := range reliquaryIdList {
		reliquaryGuid := dbReliquary.CostReliquary(player, reliquaryId)
		if reliquaryGuid == 0 {
			logger.Error("reliquary cost error, reliquaryId: %v", reliquaryId)
			return
		}
		storeItemDelNotify.GuidList = append(storeItemDelNotify.GuidList, reliquaryGuid)
	}
	g.SendMsg(cmd.StoreItemDelNotify, userId, player.ClientSeq, storeItemDelNotify)
}

/************************************************** 打包封装 **************************************************/

func (g *Game) PacketStoreItemChangeNotifyByReliquary(reliquary *model.Reliquary) *proto.StoreItemChangeNotify {
	storeItemChangeNotify := &proto.StoreItemChangeNotify{
		StoreType: proto.StoreType_STORE_PACK,
		ItemList:  make([]*proto.Item, 0),
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
	storeItemChangeNotify.ItemList = append(storeItemChangeNotify.ItemList, pbItem)
	return storeItemChangeNotify
}
