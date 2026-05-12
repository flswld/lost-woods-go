package game

import (
	"time"

	"hk4e/common/constant"
	"hk4e/gdconf"
	"hk4e/gs/model"
	"hk4e/pkg/random"
	"hk4e/protocol/cmd"
	"hk4e/protocol/proto"

	"github.com/flswld/halo/logger"
	"github.com/golang-jwt/jwt/v4"
	pb "google.golang.org/protobuf/proto"
)

// 抽卡 模块
//
// 项目自创实现 配置在 gdconf/game_data_config/ext/（详见 CLAUDE.md "配置数据来源"）
// **作者自己设计的 4 池硬编码方案**：
//   - 300: 温迪（角色UP=1022 4星UP=1023/1031/1014）
//   - 400: 可莉（角色UP=1029 4星UP=1025/1034/1043）
//   - 431: 阿莫斯之弓+天空之傲（双武器UP=15502/12501 4星UP=11403/12402/13401/14409/15401）
//   - 201: 常驻（5星UP=刻晴1003/迪卢克1016）
//
// 抽卡核心机制（与官服一致）：
//   - 90 抽 5 星保底（GuaranteedStat 累加）
//   - 10 抽 4 星保底
//   - 大保底（5星非UP后下次必出UP）
//   - 武器池有定轨愿望（WishItem）
//   - 可莉特殊池：3 抽必出 1029（GM 测试用）
//
// JWT token 用于客户端访问"抽卡历史"和"概率说明"的 H5 页面 URL（http://223.5.5.5/gacha?...）
// **服务端没有真正实现这两个 H5 页面**：URL 是占位 客户端打开会 404
// 抽卡的核心算法在 doGachaOnce + doGachaRandDropFull/Once 这三个函数

/************************************************** 接口请求 **************************************************/

// UserInfo JWT 用户信息（仅用于抽卡 H5 页面访问鉴权）
type UserInfo struct {
	UserId uint32 `json:"userId"`
	jwt.RegisteredClaims
}

// GetGachaInfoReq 获取卡池信息（玩家进入抽卡界面时调用）
//
// 返回 4 个硬编码卡池：
//   - 300/400/431: 用 itemId=223（纠缠之缘 = 限定原石）抽
//   - 201: 用 itemId=224（相遇之缘 = 常驻原石）抽
//
// LeftGachaTimes/GachaTimesLimit = 2147483647 → 不限抽次（不实现"次数限制"）
// EndTime = 2051193600 = 2034 年 → 卡池永不过期
//
// **设计选择**：作者用动态生成 JWT 给抽卡 URL 防止玩家伪造但服务端没真正校验
func (g *Game) GetGachaInfoReq(player *model.Player, payloadMsg pb.Message) {
	serverAddr := "http://223.5.5.5"
	userInfo := &UserInfo{
		UserId: player.PlayerId,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(24 * time.Hour * time.Duration(1))),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			NotBefore: jwt.NewNumericDate(time.Now()),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS512, userInfo)
	jwtStr, err := token.SignedString([]byte("flswld"))
	if err != nil {
		logger.Error("generate jwt error: %v", err)
		jwtStr = "default.jwt.token"
	}
	getGachaInfoRsp := new(proto.GetGachaInfoRsp)
	getGachaInfoRsp.GachaRandom = 12345
	getGachaInfoRsp.GachaInfoList = []*proto.GachaInfo{
		// 温迪
		{
			GachaType:              300,
			ScheduleId:             823,
			BeginTime:              0,
			EndTime:                2051193600,
			GachaSortId:            9998,
			GachaPrefabPath:        "GachaShowPanel_A019",
			GachaPreviewPrefabPath: "UI_Tab_GachaShowPanel_A019",
			TitleTextmap:           "UI_GACHA_SHOW_PANEL_A019_TITLE",
			LeftGachaTimes:         2147483647,
			GachaTimesLimit:        2147483647,
			CostItemId:             223,
			CostItemNum:            1,
			TenCostItemId:          223,
			TenCostItemNum:         10,
			GachaRecordUrl:         serverAddr + "/gacha?gachaType=300&jwt=" + jwtStr,
			GachaRecordUrlOversea:  serverAddr + "/gacha?gachaType=300&jwt=" + jwtStr,
			GachaProbUrl:           serverAddr + "/gacha/details?scheduleId=823&jwt=" + jwtStr,
			GachaProbUrlOversea:    serverAddr + "/gacha/details?scheduleId=823&jwt=" + jwtStr,
			GachaUpInfoList: []*proto.GachaUpInfo{
				{
					ItemParentType: 1,
					ItemIdList:     []uint32{1022},
				},
				{
					ItemParentType: 2,
					ItemIdList:     []uint32{1023, 1031, 1014},
				},
			},
			DisplayUp4ItemList: []uint32{1023},
			DisplayUp5ItemList: []uint32{1022},
			WishItemId:         0,
			WishProgress:       0,
			WishMaxProgress:    0,
			IsNewWish:          false,
		},
		// 可莉
		{
			GachaType:              400,
			ScheduleId:             833,
			BeginTime:              0,
			EndTime:                2051193600,
			GachaSortId:            9998,
			GachaPrefabPath:        "GachaShowPanel_A018",
			GachaPreviewPrefabPath: "UI_Tab_GachaShowPanel_A018",
			TitleTextmap:           "UI_GACHA_SHOW_PANEL_A018_TITLE",
			LeftGachaTimes:         2147483647,
			GachaTimesLimit:        2147483647,
			CostItemId:             223,
			CostItemNum:            1,
			TenCostItemId:          223,
			TenCostItemNum:         10,
			GachaRecordUrl:         serverAddr + "/gacha?gachaType=400&jwt=" + jwtStr,
			GachaRecordUrlOversea:  serverAddr + "/gacha?gachaType=400&jwt=" + jwtStr,
			GachaProbUrl:           serverAddr + "/gacha/details?scheduleId=833&jwt=" + jwtStr,
			GachaProbUrlOversea:    serverAddr + "/gacha/details?scheduleId=833&jwt=" + jwtStr,
			GachaUpInfoList: []*proto.GachaUpInfo{
				{
					ItemParentType: 1,
					ItemIdList:     []uint32{1029},
				},
				{
					ItemParentType: 2,
					ItemIdList:     []uint32{1025, 1034, 1043},
				},
			},
			DisplayUp4ItemList: []uint32{1025},
			DisplayUp5ItemList: []uint32{1029},
			WishItemId:         0,
			WishProgress:       0,
			WishMaxProgress:    0,
			IsNewWish:          false,
		},
		// 阿莫斯之弓&天空之傲
		{
			GachaType:              431,
			ScheduleId:             1143,
			BeginTime:              0,
			EndTime:                2051193600,
			GachaSortId:            9997,
			GachaPrefabPath:        "GachaShowPanel_A030",
			GachaPreviewPrefabPath: "UI_Tab_GachaShowPanel_A030",
			TitleTextmap:           "UI_GACHA_SHOW_PANEL_A030_TITLE",
			LeftGachaTimes:         2147483647,
			GachaTimesLimit:        2147483647,
			CostItemId:             223,
			CostItemNum:            1,
			TenCostItemId:          223,
			TenCostItemNum:         10,
			GachaRecordUrl:         serverAddr + "/gacha?gachaType=431&jwt=" + jwtStr,
			GachaRecordUrlOversea:  serverAddr + "/gacha?gachaType=431&jwt=" + jwtStr,
			GachaProbUrl:           serverAddr + "/gacha/details?scheduleId=1143&jwt=" + jwtStr,
			GachaProbUrlOversea:    serverAddr + "/gacha/details?scheduleId=1143&jwt=" + jwtStr,
			GachaUpInfoList: []*proto.GachaUpInfo{
				{
					ItemParentType: 1,
					ItemIdList:     []uint32{15502, 12501},
				},
				{
					ItemParentType: 2,
					ItemIdList:     []uint32{11403, 12402, 13401, 14409, 15401},
				},
			},
			DisplayUp4ItemList: []uint32{11403},
			DisplayUp5ItemList: []uint32{15502, 12501},
			WishItemId:         0,
			WishProgress:       0,
			WishMaxProgress:    0,
			IsNewWish:          false,
		},
		// 常驻
		{
			GachaType:              201,
			ScheduleId:             813,
			BeginTime:              0,
			EndTime:                2051193600,
			GachaSortId:            1000,
			GachaPrefabPath:        "GachaShowPanel_A017",
			GachaPreviewPrefabPath: "UI_Tab_GachaShowPanel_A017",
			TitleTextmap:           "UI_GACHA_SHOW_PANEL_A017_TITLE",
			LeftGachaTimes:         2147483647,
			GachaTimesLimit:        2147483647,
			CostItemId:             224,
			CostItemNum:            1,
			TenCostItemId:          224,
			TenCostItemNum:         10,
			GachaRecordUrl:         serverAddr + "/gacha?gachaType=201&jwt=" + jwtStr,
			GachaRecordUrlOversea:  serverAddr + "/gacha?gachaType=201&jwt=" + jwtStr,
			GachaProbUrl:           serverAddr + "/gacha/details?scheduleId=813&jwt=" + jwtStr,
			GachaProbUrlOversea:    serverAddr + "/gacha/details?scheduleId=813&jwt=" + jwtStr,
			GachaUpInfoList: []*proto.GachaUpInfo{
				{
					ItemParentType: 1,
					ItemIdList:     []uint32{1003, 1016},
				},
				{
					ItemParentType: 2,
					ItemIdList:     []uint32{1021, 1006, 1015},
				},
			},
			DisplayUp4ItemList: []uint32{1021},
			DisplayUp5ItemList: []uint32{1003, 1016},
			WishItemId:         0,
			WishProgress:       0,
			WishMaxProgress:    0,
			IsNewWish:          false,
		},
	}
	g.SendMsg(cmd.GetGachaInfoRsp, player.PlayerId, player.ClientSeq, getGachaInfoRsp)
}

func (g *Game) DoGachaReq(player *model.Player, payloadMsg pb.Message) {
	req := payloadMsg.(*proto.DoGachaReq)
	gachaScheduleId := req.GachaScheduleId
	gachaTimes := req.GachaTimes
	gachaType := uint32(0)
	costItemId := uint32(0)
	switch gachaScheduleId {
	case 823:
		// 温迪
		gachaType = 300
		costItemId = 223
	case 833:
		// 可莉
		gachaType = 400
		costItemId = 223
	case 1143:
		// 阿莫斯之弓&天空之傲
		gachaType = 431
		costItemId = 223
	case 813:
		// 常驻
		gachaType = 201
		costItemId = 224
	}
	// 先扣掉粉球或蓝球再进行抽卡
	ok := g.CostPlayerItem(player.PlayerId, []*ChangeItem{{ItemId: costItemId, ChangeCount: gachaTimes}})
	if !ok {
		return
	}
	doGachaRsp := &proto.DoGachaRsp{
		GachaType:       gachaType,
		GachaScheduleId: gachaScheduleId,
		GachaTimes:      gachaTimes,
		NewGachaRandom:  12345,
		LeftGachaTimes:  2147483647,
		GachaTimesLimit: 2147483647,
		CostItemId:      costItemId,
		CostItemNum:     1,
		TenCostItemId:   costItemId,
		TenCostItemNum:  10,
		GachaItemList:   make([]*proto.GachaItem, 0),
	}
	for i := uint32(0); i < gachaTimes; i++ {
		var ok bool
		var itemId uint32
		if gachaType == 400 {
			// 可莉
			ok, itemId = g.doGachaKlee()
		} else if gachaType == 300 {
			// 角色UP池
			ok, itemId = g.doGachaOnce(player.PlayerId, gachaType, true, false)
		} else if gachaType == 431 {
			// 武器UP池
			ok, itemId = g.doGachaOnce(player.PlayerId, gachaType, true, true)
		} else if gachaType == 201 {
			// 常驻
			ok, itemId = g.doGachaOnce(player.PlayerId, gachaType, false, false)
		} else {
			ok, itemId = false, 0
		}
		if !ok {
			itemId = 11301
		}
		// 判断角色是否已拥有（决定后续给星尘还是星辉）
		isRepeatAvatar := false
		// 添加抽卡获得的道具
		if itemId > 1000 && itemId < 2000 {
			avatarId := (itemId % 1000) + 10000000
			dbAvatar := player.GetDbAvatar()
			avatar := dbAvatar.GetAvatarById(avatarId)
			if avatar == nil {
				g.AddPlayerAvatar(player.PlayerId, avatarId)
			} else {
				isRepeatAvatar = true
				constellationItemId := itemId + 100
				if g.GetPlayerItemCount(player.PlayerId, constellationItemId) < 6 {
					g.AddPlayerItem(player.PlayerId, []*ChangeItem{{ItemId: constellationItemId, ChangeCount: 1}}, proto.ActionReasonType_ACTION_REASON_GACHA)
				}
			}
		} else if itemId > 10000 && itemId < 20000 {
			g.AddPlayerWeapon(player.PlayerId, itemId)
		} else {
			g.AddPlayerItem(player.PlayerId, []*ChangeItem{{ItemId: itemId, ChangeCount: 1}}, proto.ActionReasonType_ACTION_REASON_GACHA)
		}

		// 计算星尘星辉（业界已知规则 配置表里只有 token id 映射没有数量公式）
		// 3★ 武器: 15 星尘
		// 4★ 角色未拥有: 20 星尘 / 已拥有: 2 星辉（命星石另算）
		// 4★ 武器: 20 星尘（武器无"重复"概念）
		// 5★ 角色未拥有: 40 星尘 / 已拥有: 10 星辉（命星石另算）
		// 5★ 武器: 40 星尘
		stardust, starlight := calcGachaTokens(itemId, isRepeatAvatar)

		gachaItem := new(proto.GachaItem)
		gachaItem.GachaItem = &proto.ItemParam{ItemId: itemId, Count: 1}
		tokenList := make([]*proto.ItemParam, 0, 2)
		if stardust != 0 {
			g.AddPlayerItem(player.PlayerId, []*ChangeItem{{ItemId: constant.ITEM_ID_STARDUST, ChangeCount: stardust}}, proto.ActionReasonType_ACTION_REASON_GACHA)
			tokenList = append(tokenList, &proto.ItemParam{ItemId: constant.ITEM_ID_STARDUST, Count: stardust})
		}
		if starlight != 0 {
			g.AddPlayerItem(player.PlayerId, []*ChangeItem{{ItemId: constant.ITEM_ID_STARGLITTER, ChangeCount: starlight}}, proto.ActionReasonType_ACTION_REASON_GACHA)
			tokenList = append(tokenList, &proto.ItemParam{ItemId: constant.ITEM_ID_STARGLITTER, Count: starlight})
		}
		if len(tokenList) > 0 {
			gachaItem.TokenItemList = tokenList
		}
		// 注：transfer_items 的语义是"重复抽到 → 转化为某物的转化记录"
		// 当前简化逻辑下不填该字段（旧版本错误地填了星辉到这里）
		doGachaRsp.GachaItemList = append(doGachaRsp.GachaItemList, gachaItem)
	}
	logger.Debug("doGachaRsp: %v", doGachaRsp.String())
	g.SendMsg(cmd.DoGachaRsp, player.PlayerId, player.ClientSeq, doGachaRsp)
}

/************************************************** 游戏功能 **************************************************/

// doGachaKlee 可莉池特殊抽卡（彩蛋 与正常抽卡逻辑无关）
//
// "扣1给可莉刷烧烤酱"——作者中二式注释
// 算法：把全部 4/5星角色 + 全部 5星武器 + 4 种货币（原石/摩拉/粉球/蓝球）+ 100081（特殊物品）合一起平均随机
// 100081 是 "为了同志的事业 永远奋斗" 类似的彩蛋物品（item id 在 ext 配置）
//
// 触发条件：见 DoGachaReq 中的特殊判断
func (g *Game) doGachaKlee() (bool, uint32) {
	allAvatarList := make([]uint32, 0)
	allAvatarDataConfig := g.GetAllAvatarDataConfig()
	for k, v := range allAvatarDataConfig {
		if v.QualityType == 5 || v.QualityType == 4 {
			allAvatarList = append(allAvatarList, uint32(k))
		}
	}
	allWeaponList := make([]uint32, 0)
	allWeaponDataConfig := g.GetAllWeaponDataConfig()
	for k, v := range allWeaponDataConfig {
		if v.EquipLevel == 5 {
			allWeaponList = append(allWeaponList, uint32(k))
		}
	}
	allGoodList := make([]uint32, 0)
	// 全部角色
	allGoodList = append(allGoodList, allAvatarList...)
	// 全部5星武器
	allGoodList = append(allGoodList, allWeaponList...)
	// 原石 摩拉 粉球 蓝球
	allGoodList = append(allGoodList, 201, 202, 223, 224)
	// 苟利国家生死以
	allGoodList = append(allGoodList, 100081)
	rn := random.GetRandomInt32(0, int32(len(allGoodList)-1))
	itemId := allGoodList[rn]
	if itemId > 10000000 {
		itemId %= 1000
		itemId += 1000
	}
	return true, itemId
}

const (
	Orange = iota
	Purple
	Blue
	Avatar
	Weapon
)

const (
	StandardOrangeTimesFixThreshold uint32 = 74   // 标准池触发5星概率修正阈值的抽卡次数
	StandardOrangeTimesFixValue     int32  = 600  // 标准池5星概率修正因子
	StandardPurpleTimesFixThreshold uint32 = 9    // 标准池触发4星概率修正阈值的抽卡次数
	StandardPurpleTimesFixValue     int32  = 5100 // 标准池4星概率修正因子
	WeaponOrangeTimesFixThreshold   uint32 = 63   // 武器池触发5星概率修正阈值的抽卡次数
	WeaponOrangeTimesFixValue       int32  = 700  // 武器池5星概率修正因子
	WeaponPurpleTimesFixThreshold   uint32 = 8    // 武器池触发4星概率修正阈值的抽卡次数
	WeaponPurpleTimesFixValue       int32  = 6000 // 武器池4星概率修正因子
)

// calcGachaTokens 按抽到的物品 itemId + 是否重复角色 计算应给的星尘和星辉数量
// 业界已知规则（hk4e 服务端配置表里只有 token id 映射没有数量公式 故硬编码）：
//
//	3★ 武器                     → 15 星尘
//	4★ 角色未拥有 / 4★ 武器     → 20 星尘
//	4★ 角色已拥有                → 2 星辉（命星石另发）
//	5★ 角色未拥有 / 5★ 武器     → 40 星尘
//	5★ 角色已拥有                → 10 星辉（命星石另发）
func calcGachaTokens(itemId uint32, isRepeatAvatar bool) (stardust uint32, starlight uint32) {
	var quality int32
	if itemId > 1000 && itemId < 2000 {
		avatarId := (itemId % 1000) + 10000000
		avatarData := gdconf.GetAvatarDataById(int32(avatarId))
		if avatarData == nil {
			return 0, 0
		}
		quality = avatarData.QualityType
	} else if itemId > 10000 && itemId < 20000 {
		itemData := gdconf.GetItemDataById(int32(itemId))
		if itemData == nil {
			return 0, 0
		}
		quality = itemData.EquipLevel
	} else {
		return 0, 0
	}
	switch quality {
	case 3:
		return 15, 0
	case 4:
		if isRepeatAvatar {
			return 0, 2
		}
		return 20, 0
	case 5:
		if isRepeatAvatar {
			return 0, 10
		}
		return 40, 0
	}
	return 0, 0
}

// doGachaOnce 单抽一次（核心算法 含保底机制）
//
// 处理流程：
//  1. 保底计数+1（OrangeTimes 5星 / PurpleTimes 4星）
//  2. 概率修正：
//     · 标准池：第 74 抽起 5 星概率 +600/抽（达到约 70% 时触发软保底）
//     · 武器池：第 63 抽起 5 星概率 +700/抽（武器池软保底来得更早）
//     · 4 星类似但阈值是 9/8 抽
//  3. 走 doGachaRandDropFull 抽出基础掉落组（5/4/3星）
//  4. 大保底机制（mustGetUpEnable=true）：
//     · 5 星非UP → MustGetUpOrange=true 下次5星必出UP
//     · 已 MustGetUpOrange + 抽到 5 星 → 直接换成 UP 池抽
//     · 4 星同理
//  5. 抽到 5 星重置 OrangeTimes 计数
//
// 配置表 ID 规则（硬编码约定）：
//   - 5 星掉落组 = gachaType*10 + 1  (300 池→3001)
//   - 4 星 = gachaType*10 + 2  (3002)
//   - 3 星 = gachaType*10 + 3  (3003)
//   - UP 5 星 = gachaType*100 + 12  (30012)
//   - UP 4 星 = gachaType*100 + 22  (30022)
func (g *Game) doGachaOnce(userId uint32, gachaType uint32, mustGetUpEnable bool, weaponFix bool) (bool, uint32) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return false, 0
	}
	// 找到卡池对应的掉落组
	dropGroupDataConfig := gdconf.GetGachaDropGroupDataByDropId(int32(gachaType))
	if dropGroupDataConfig == nil {
		logger.Error("drop group not found, drop id: %v", gachaType)
		return false, 0
	}
	// 获取用户的卡池保底信息
	dbGacha := player.GetDbGacha()
	gachaPoolInfo := dbGacha.GachaPoolInfo[gachaType]
	if gachaPoolInfo == nil {
		logger.Error("player gacha pool info not found, gacha type: %v", gachaType)
		return false, 0
	}
	// 保底计数+1
	gachaPoolInfo.OrangeTimes++
	gachaPoolInfo.PurpleTimes++
	// 4星和5星概率修正
	OrangeTimesFixThreshold := uint32(0)
	OrangeTimesFixValue := int32(0)
	PurpleTimesFixThreshold := uint32(0)
	PurpleTimesFixValue := int32(0)
	if !weaponFix {
		OrangeTimesFixThreshold = StandardOrangeTimesFixThreshold
		OrangeTimesFixValue = StandardOrangeTimesFixValue
		PurpleTimesFixThreshold = StandardPurpleTimesFixThreshold
		PurpleTimesFixValue = StandardPurpleTimesFixValue
	} else {
		OrangeTimesFixThreshold = WeaponOrangeTimesFixThreshold
		OrangeTimesFixValue = WeaponOrangeTimesFixValue
		PurpleTimesFixThreshold = WeaponPurpleTimesFixThreshold
		PurpleTimesFixValue = WeaponPurpleTimesFixValue
	}
	if gachaPoolInfo.OrangeTimes >= OrangeTimesFixThreshold || gachaPoolInfo.PurpleTimes >= PurpleTimesFixThreshold {
		fixDropGroupDataConfig := new(gdconf.GachaDropGroupData)
		fixDropGroupDataConfig.DropId = dropGroupDataConfig.DropId
		fixDropGroupDataConfig.WeightAll = dropGroupDataConfig.WeightAll
		// 计算4星和5星权重修正值
		addOrangeWeight := int32(gachaPoolInfo.OrangeTimes-OrangeTimesFixThreshold+1) * OrangeTimesFixValue
		if addOrangeWeight < 0 {
			addOrangeWeight = 0
		}
		addPurpleWeight := int32(gachaPoolInfo.PurpleTimes-PurpleTimesFixThreshold+1) * PurpleTimesFixValue
		if addPurpleWeight < 0 {
			addPurpleWeight = 0
		}
		for _, drop := range dropGroupDataConfig.DropConfig {
			fixDrop := new(gdconf.GachaDrop)
			fixDrop.Result = drop.Result
			fixDrop.DropId = drop.DropId
			fixDrop.IsEnd = drop.IsEnd
			// 找到5/4/3星掉落组id 要求配置表的5/4/3星掉落组id规则固定为(卡池类型*10+1/2/3)
			orangeDropId := int32(gachaType*10 + 1)
			purpleDropId := int32(gachaType*10 + 2)
			blueDropId := int32(gachaType*10 + 3)
			// 权重修正
			if drop.Result == orangeDropId {
				fixDrop.Weight = drop.Weight + addOrangeWeight
			} else if drop.Result == purpleDropId {
				fixDrop.Weight = drop.Weight + addPurpleWeight
			} else if drop.Result == blueDropId {
				fixDrop.Weight = drop.Weight - addOrangeWeight - addPurpleWeight
			} else {
				logger.Error("invalid drop group id, does not match any case of orange/purple/blue, result group id: %v", drop.Result)
				fixDrop.Weight = drop.Weight
			}
			fixDropGroupDataConfig.DropConfig = append(fixDropGroupDataConfig.DropConfig, fixDrop)
		}
		dropGroupDataConfig = fixDropGroupDataConfig
	}
	// 掉落
	ok, drop := g.doGachaRandDropFull(dropGroupDataConfig)
	if !ok {
		return false, 0
	}
	// 分析本次掉落结果的星级和类型
	itemColor := 0
	itemType := 0
	_ = itemType
	gachaItemId := uint32(drop.Result)
	if gachaItemId < 2000 {
		// 抽到角色
		itemType = Avatar
		avatarId := (gachaItemId % 1000) + 10000000
		allAvatarDataConfig := g.GetAllAvatarDataConfig()
		avatarDataConfig := allAvatarDataConfig[int32(avatarId)]
		if avatarDataConfig == nil {
			logger.Error("avatar data config not found, avatar id: %v", avatarId)
			return false, 0
		}
		if avatarDataConfig.QualityType == 5 {
			itemColor = Orange
			logger.Debug("[orange avatar], times: %v, gachaItemId: %v", gachaPoolInfo.OrangeTimes, gachaItemId)
			if gachaPoolInfo.OrangeTimes > 90 {
				logger.Error("[abnormal orange avatar], times: %v, gachaItemId: %v", gachaPoolInfo.OrangeTimes, gachaItemId)
			}
		} else if avatarDataConfig.QualityType == 4 {
			itemColor = Purple
			logger.Debug("[purple avatar], times: %v, gachaItemId: %v", gachaPoolInfo.PurpleTimes, gachaItemId)
			if gachaPoolInfo.PurpleTimes > 10 {
				logger.Error("[abnormal purple avatar], times: %v, gachaItemId: %v", gachaPoolInfo.PurpleTimes, gachaItemId)
			}
		} else {
			itemColor = Blue
		}
	} else {
		// 抽到武器
		itemType = Weapon
		allWeaponDataConfig := g.GetAllWeaponDataConfig()
		weaponDataConfig := allWeaponDataConfig[int32(gachaItemId)]
		if weaponDataConfig == nil {
			logger.Error("weapon item data config not found, item id: %v", gachaItemId)
			return false, 0
		}
		if weaponDataConfig.EquipLevel == 5 {
			itemColor = Orange
			logger.Debug("[orange weapon], times: %v, gachaItemId: %v", gachaPoolInfo.OrangeTimes, gachaItemId)
			if gachaPoolInfo.OrangeTimes > 90 {
				logger.Error("[abnormal orange weapon], times: %v, gachaItemId: %v", gachaPoolInfo.OrangeTimes, gachaItemId)
			}
		} else if weaponDataConfig.EquipLevel == 4 {
			itemColor = Purple
			logger.Debug("[purple weapon], times: %v, gachaItemId: %v", gachaPoolInfo.PurpleTimes, gachaItemId)
			if gachaPoolInfo.PurpleTimes > 10 {
				logger.Error("[abnormal purple weapon], times: %v, gachaItemId: %v", gachaPoolInfo.PurpleTimes, gachaItemId)
			}
		} else {
			itemColor = Blue
		}
	}
	// 后处理
	switch itemColor {
	case Orange:
		// 重置5星保底计数
		gachaPoolInfo.OrangeTimes = 0
		if mustGetUpEnable {
			// 找到UP的5星对应的掉落组id 要求配置表的UP的5星掉落组id规则固定为(卡池类型*100+12)
			upOrangeDropId := int32(gachaType*100 + 12)
			// 替换本次结果为5星大保底
			if gachaPoolInfo.MustGetUpOrange {
				logger.Debug("trigger must get up orange, uid: %v", userId)
				upOrangeDropGroupDataConfig := gdconf.GetGachaDropGroupDataByDropId(upOrangeDropId)
				if upOrangeDropGroupDataConfig == nil {
					logger.Error("drop group not found, drop id: %v", upOrangeDropId)
					return false, 0
				}
				upOrangeOk, upOrangeDrop := g.doGachaRandDropFull(upOrangeDropGroupDataConfig)
				if !upOrangeOk {
					return false, 0
				}
				gachaPoolInfo.MustGetUpOrange = false
				upOrangeGachaItemId := uint32(upOrangeDrop.Result)
				return upOrangeOk, upOrangeGachaItemId
			}
			// 触发5星大保底
			if drop.DropId != upOrangeDropId {
				gachaPoolInfo.MustGetUpOrange = true
			}
		}
	case Purple:
		// 重置4星保底计数
		gachaPoolInfo.PurpleTimes = 0
		if mustGetUpEnable {
			// 找到UP的4星对应的掉落组id 要求配置表的UP的4星掉落组id规则固定为(卡池类型*100+22)
			upPurpleDropId := int32(gachaType*100 + 22)
			// 替换本次结果为4星大保底
			if gachaPoolInfo.MustGetUpPurple {
				logger.Debug("trigger must get up purple, uid: %v", userId)
				upPurpleDropGroupDataConfig := gdconf.GetGachaDropGroupDataByDropId(upPurpleDropId)
				if upPurpleDropGroupDataConfig == nil {
					logger.Error("drop group not found, drop id: %v", upPurpleDropId)
					return false, 0
				}
				upPurpleOk, upPurpleDrop := g.doGachaRandDropFull(upPurpleDropGroupDataConfig)
				if !upPurpleOk {
					return false, 0
				}
				gachaPoolInfo.MustGetUpPurple = false
				upPurpleGachaItemId := uint32(upPurpleDrop.Result)
				return upPurpleOk, upPurpleGachaItemId
			}
			// 触发4星大保底
			if drop.DropId != upPurpleDropId {
				gachaPoolInfo.MustGetUpPurple = true
			}
		}
	default:
	}
	return ok, gachaItemId
}

// doGachaRandDropFull 走完整掉落组流程（递归选择直到 IsEnd=true）
//
// 抽卡掉落组是树形结构：先随机选 5/4/3星组 → 再随机该组内具体角色/武器 → IsEnd 才是叶节点物品
// 最多递归 1000 层防止配置错误死循环
// 例如：300 池 → 抽到"30012 UP5星组" → 抽到"温迪 1022" → IsEnd 返回
func (g *Game) doGachaRandDropFull(gachaDropGroupDataConfig *gdconf.GachaDropGroupData) (bool, *gdconf.GachaDrop) {
	for i := 0; i < 1000; i++ {
		drop := g.doGachaRandDropOnce(gachaDropGroupDataConfig)
		if drop == nil {
			logger.Error("weight error, drop config: %v", gachaDropGroupDataConfig)
			return false, nil
		}
		if drop.IsEnd {
			// 成功抽到物品
			return true, drop
		}
		// 进行下一步掉落流程
		gachaDropGroupDataConfig = gdconf.GetGachaDropGroupDataByDropId(drop.Result)
		if gachaDropGroupDataConfig == nil {
			logger.Error("drop config error, drop id: %v", drop.Result)
			return false, nil
		}
	}
	logger.Error("drop overtimes, drop config: %v", gachaDropGroupDataConfig)
	return false, nil
}

// 进行单次随机掉落 轮盘赌选择法RWS
func (g *Game) doGachaRandDropOnce(dropGroupDataConfig *gdconf.GachaDropGroupData) *gdconf.GachaDrop {
	randNum := random.GetRandomInt32(0, dropGroupDataConfig.WeightAll-1)
	sumWeight := int32(0)
	for _, drop := range dropGroupDataConfig.DropConfig {
		sumWeight += drop.Weight
		if sumWeight > randNum {
			return drop
		}
	}
	return nil
}

/************************************************** 打包封装 **************************************************/
