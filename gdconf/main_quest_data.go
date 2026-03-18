package gdconf

import (
	"github.com/flswld/halo/logger"
)

// MainQuestData 主线任务配置表
type MainQuestData struct {
	ParentQuestId int32    `csv:"父任务ID"`
	RewardIdList  IntArray `csv:"任务奖励RewardID,omitempty"`
	VideoKey      uint64   `csv:"VideoKey,omitempty"`
}

func (g *GameDataConfig) loadMainQuestData() {
	g.MainQuestDataMap = make(map[int32]*MainQuestData)
	fileNameList := []string{
		"MainQuestData.txt",
		"MainQuestData_Exported.txt",
	}
	for _, fileName := range fileNameList {
		mainQuestDataList := make([]*MainQuestData, 0)
		readTable[MainQuestData](g.txtPrefix+fileName, &mainQuestDataList)
		for _, mainQuestData := range mainQuestDataList {
			g.MainQuestDataMap[mainQuestData.ParentQuestId] = mainQuestData
		}
	}
	logger.Info("MainQuestData Count: %v", len(g.MainQuestDataMap))
}

func GetMainQuestDataById(parentQuestId int32) *MainQuestData {
	return CONF.MainQuestDataMap[parentQuestId]
}

func GetMainQuestDataMap() map[int32]*MainQuestData {
	return CONF.MainQuestDataMap
}
