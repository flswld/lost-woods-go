package gdconf

import (
	"github.com/flswld/halo/logger"
)

// PUBG 世界物件配置 - **作者自创扩展配置**（不在原版 3.2 数据中）
//
// 用途：PUBG 玩法中地图上的散点物件
//   - 增攻物件：玩家拾取后角色基础攻击力 +N
//   - 恢复 HP 物件：玩家拾取后角色当前 HP 增加 N
//
// 配置文件：game_data_config/ext/PubgWorldGadgetData.csv（与原版 3.2 配置分离）
// 加载用 readExtCsv（专门读 ext/ 目录的工具函数）
//
// PluginPubg.StartPubg 时按 Probability 概率把这些物件撒到地图上
// 玩家碰到调 EventGadgetInteract 给角色加属性

// pubg世界物件

const (
	PubgWorldGadgetTypeIncAtk = 1 // 增加攻击力 参数1:攻击力增量
	PubgWorldGadgetTypeIncHp  = 2 // 恢复生命值 参数1:生命值增量
)

type PubgWorldGadgetData struct {
	WorldGadgetId int32    `csv:"WorldGadgetId"`
	GadgetId      int32    `csv:"GadgetId"`
	X             float32  `csv:"X"`
	Y             float32  `csv:"Y"`
	Z             float32  `csv:"Z"`
	Probability   int32    `csv:"Probability"`
	Type          int32    `csv:"Type"`
	Param         IntArray `csv:"Param"`
}

func (g *GameDataConfig) loadPubgWorldGadgetData() {
	g.PubgWorldGadgetDataMap = make(map[int32]*PubgWorldGadgetData)
	pubgWorldGadgetDataList := make([]*PubgWorldGadgetData, 0)
	readExtCsv[PubgWorldGadgetData](g.extPrefix+"PubgWorldGadgetData.csv", &pubgWorldGadgetDataList)
	for _, pubgWorldGadgetData := range pubgWorldGadgetDataList {
		g.PubgWorldGadgetDataMap[pubgWorldGadgetData.WorldGadgetId] = pubgWorldGadgetData
	}
	logger.Info("PubgWorldGadgetData Count: %v", len(g.PubgWorldGadgetDataMap))
}

func GetPubgWorldGadgetDataById(worldGadgetId int32) *PubgWorldGadgetData {
	if CONF.PubgWorldGadgetDataMap == nil {
		return nil
	}
	return CONF.PubgWorldGadgetDataMap[worldGadgetId]
}

func GetPubgWorldGadgetDataMap() map[int32]*PubgWorldGadgetData {
	return CONF.PubgWorldGadgetDataMap
}
