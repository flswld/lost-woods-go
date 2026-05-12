package game

import (
	"time"

	"hk4e/common/constant"
	"hk4e/gs/model"
	"hk4e/protocol/cmd"
	"hk4e/protocol/proto"

	pb "google.golang.org/protobuf/proto"
)

// 商店 模块（占位实现）
//
// **现状**：商店仅作"客户端不报错"的占位 没有真正的商品体系
//   - GetShopmallDataReq 返回 5 个 ShopType（900/1052/902/1001/903）但只有 1001 真有商品
//   - GetShopReq 仅 ShopType=1001 走分支 其他类型返回空响应
//   - 商品永远固定 2 件：itemId=223（纠缠之缘 160原石）+ itemId=224（相遇之缘 160原石）
//   - BuyGoodsReq 仅识别这 2 个商品 其他买不了
//
// 实际可用功能：
//   - BuyGoodsReq: 用 201 原石换 223/224 缘
//   - McoinExchangeHcoinReq: 创世结晶（203）兑换原石（201）1:1
//
// 这些缘可以拿去抽卡（DoGachaReq 用 223/224 作为消耗物品）
// 项目实际给玩家原石/缘都是通过 GM 命令 商店占位让客户端不会显示空白页面

/************************************************** 接口请求 **************************************************/

// GetShopmallDataReq 商城首页（返回 5 个商店类型）
// 仅 1001 类型有商品 其他类型客户端点进去会看到空商店
func (g *Game) GetShopmallDataReq(player *model.Player, payloadMsg pb.Message) {
	getShopmallDataRsp := &proto.GetShopmallDataRsp{
		ShopTypeList: []uint32{900, 1052, 902, 1001, 903},
	}
	g.SendMsg(cmd.GetShopmallDataRsp, player.PlayerId, player.ClientSeq, getShopmallDataRsp)
}

// GetShopReq 获取商店商品列表
// 仅 ShopType=1001 处理 返回硬编码 2 件商品（223/224 缘 各 160 原石）
// 时间窗口：BeginTime=2019-12 EndTime=2034 → 永久不过期
// 下次刷新时间：30 天后（实际不会真的刷新 玩家无限购）
func (g *Game) GetShopReq(player *model.Player, payloadMsg pb.Message) {
	req := payloadMsg.(*proto.GetShopReq)
	shopType := req.ShopType

	if shopType != 1001 {
		return
	}

	nextRefreshTime := uint32(time.Now().Add(time.Hour * 24 * 30).Unix())

	getShopRsp := &proto.GetShopRsp{
		Shop: &proto.Shop{
			GoodsList: []*proto.ShopGoods{
				{
					MinLevel:        1,
					EndTime:         2051193600,
					Hcoin:           160,
					GoodsId:         102001,
					NextRefreshTime: nextRefreshTime,
					MaxLevel:        99,
					BeginTime:       1575129600,
					GoodsItem: &proto.ItemParam{
						ItemId: constant.ITEM_ID_INTERTWINED_FATE,
						Count:  1,
					},
				},
				{
					MinLevel:        1,
					EndTime:         2051193600,
					Hcoin:           160,
					GoodsId:         102002,
					NextRefreshTime: nextRefreshTime,
					MaxLevel:        99,
					BeginTime:       1575129600,
					GoodsItem: &proto.ItemParam{
						ItemId: constant.ITEM_ID_ACQUAINT_FATE,
						Count:  1,
					},
				},
			},
			NextRefreshTime: nextRefreshTime,
			ShopType:        1001,
		},
	}
	g.SendMsg(cmd.GetShopRsp, player.PlayerId, player.ClientSeq, getShopRsp)
}

// shopGoodsTable 占位商店的服务端硬编码价格表 不信任客户端 req.Goods.Hcoin 防止白嫖
// 项目占位的 ShopType=1001 在 ShopGoodsData.txt 配置里不存在 故走硬编码
var shopGoodsTable = map[uint32]struct {
	shopType uint32 // 必须与 req.ShopType 匹配防止串店
	itemId   uint32 // 商品本体 ID
	count    uint32 // 单次购买给的件数
	hcoin    uint32 // 单次原石价
}{
	102001: {shopType: 1001, itemId: constant.ITEM_ID_INTERTWINED_FATE, count: 1, hcoin: 160}, // 纠缠之缘
	102002: {shopType: 1001, itemId: constant.ITEM_ID_ACQUAINT_FATE, count: 1, hcoin: 160},    // 相遇之缘
}

func (g *Game) BuyGoodsReq(player *model.Player, payloadMsg pb.Message) {
	req := payloadMsg.(*proto.BuyGoodsReq)
	buyCount := req.BuyCount
	if req.Goods == nil || buyCount == 0 {
		return
	}

	// 按 GoodsId 查服务端硬编码价格表 不信任客户端 req.Goods.Hcoin
	goodsConfig, ok := shopGoodsTable[req.Goods.GoodsId]
	if !ok || goodsConfig.shopType != req.ShopType {
		return
	}
	costHcoinCount := goodsConfig.hcoin * buyCount
	if g.GetPlayerItemCount(player.PlayerId, constant.ITEM_ID_HCOIN) < costHcoinCount {
		return
	}
	if !g.CostPlayerItem(player.PlayerId, []*ChangeItem{{ItemId: constant.ITEM_ID_HCOIN, ChangeCount: costHcoinCount}}) {
		return
	}

	g.AddPlayerItem(player.PlayerId, []*ChangeItem{{ItemId: goodsConfig.itemId, ChangeCount: buyCount * goodsConfig.count}}, proto.ActionReasonType_ACTION_REASON_SHOP)
	// BoughtNum 是"刷新窗口内已购买次数" 占位商店不限购未做计数 保持 0

	buyGoodsRsp := &proto.BuyGoodsRsp{
		ShopType:  req.ShopType,
		BuyCount:  req.BuyCount,
		GoodsList: []*proto.ShopGoods{req.Goods},
	}
	g.SendMsg(cmd.BuyGoodsRsp, player.PlayerId, player.ClientSeq, buyGoodsRsp)
}

// McoinExchangeHcoinReq 创世结晶兑换原石（1:1 比例）
// itemId=203（创世结晶）→ itemId=201（原石）
// 创世结晶是充值得来的 在原版是付费道具 项目里通过 GM 命令获得
func (g *Game) McoinExchangeHcoinReq(player *model.Player, payloadMsg pb.Message) {
	req := payloadMsg.(*proto.McoinExchangeHcoinReq)
	if req.Hcoin != req.McoinCost {
		return
	}
	count := req.Hcoin

	if g.GetPlayerItemCount(player.PlayerId, constant.ITEM_ID_MCOIN) < count {
		return
	}
	ok := g.CostPlayerItem(player.PlayerId, []*ChangeItem{{ItemId: constant.ITEM_ID_MCOIN, ChangeCount: count}})
	if !ok {
		return
	}

	g.AddPlayerItem(player.PlayerId, []*ChangeItem{{ItemId: constant.ITEM_ID_HCOIN, ChangeCount: count}}, proto.ActionReasonType_ACTION_REASON_SHOP)

	mcoinExchangeHcoinRsp := &proto.McoinExchangeHcoinRsp{
		Hcoin:     req.Hcoin,
		McoinCost: req.McoinCost,
	}
	g.SendMsg(cmd.McoinExchangeHcoinRsp, player.PlayerId, player.ClientSeq, mcoinExchangeHcoinRsp)
}

/************************************************** 游戏功能 **************************************************/

/************************************************** 打包封装 **************************************************/
