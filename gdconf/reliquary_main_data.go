package gdconf

import (
	"github.com/flswld/halo/logger"
)

// ReliquaryMainData 圣遗物主属性配置表
type ReliquaryMainData struct {
	MainPropId      int32 `csv:"主属性ID"`
	MainPropDepotId int32 `csv:"主属性库ID,omitempty"`
	PropType        int32 `csv:"属性类别,omitempty"`
	RandomWeight    int32 `csv:"随机权重,omitempty"`
}

func (g *GameDataConfig) loadReliquaryMainData() {
	g.ReliquaryMainDataDepotMap = make(map[int32]map[int32]*ReliquaryMainData)
	g.ReliquaryMainDataMap = make(map[int32]*ReliquaryMainData)
	reliquaryMainDataList := make([]*ReliquaryMainData, 0)
	readTable[ReliquaryMainData](g.txtPrefix+"ReliquaryMainData.txt", &reliquaryMainDataList)
	for _, reliquaryMainData := range reliquaryMainDataList {
		// 通过主属性库ID找到
		_, ok := g.ReliquaryMainDataDepotMap[reliquaryMainData.MainPropDepotId]
		if !ok {
			g.ReliquaryMainDataDepotMap[reliquaryMainData.MainPropDepotId] = make(map[int32]*ReliquaryMainData)
		}
		g.ReliquaryMainDataDepotMap[reliquaryMainData.MainPropDepotId][reliquaryMainData.MainPropId] = reliquaryMainData
		g.ReliquaryMainDataMap[reliquaryMainData.MainPropId] = reliquaryMainData
	}
	logger.Info("ReliquaryMainData Count: %v", len(g.ReliquaryMainDataDepotMap))
}

func GetReliquaryMainDataByPropId(mainPropId int32) *ReliquaryMainData {
	return CONF.ReliquaryMainDataMap[mainPropId]
}

func GetReliquaryMainDataMapByDepotId(mainPropDepotId int32) map[int32]*ReliquaryMainData {
	return CONF.ReliquaryMainDataDepotMap[mainPropDepotId]
}
