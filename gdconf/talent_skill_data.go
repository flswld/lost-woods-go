package gdconf

import (
	"github.com/flswld/halo/logger"
)

// TalentSkillData 命座配置表
type TalentSkillData struct {
	TalentId      int32  `csv:"天赋ID"`
	TalentConfig  string `csv:"开启天赋配置,omitempty"`
	CostItemId    int32  `csv:"激活消耗主材料ID,omitempty"`
	CostItemCount int32  `csv:"激活消耗主材料数量,omitempty"`

	// 增加属性
	AddProp1Type  int32   `csv:"[增加属性]1类型,omitempty"`
	AddProp1Value float32 `csv:"[增加属性]1值,omitempty"`
	AddProp2Type  int32   `csv:"[增加属性]2类型,omitempty"`
	AddProp2Value float32 `csv:"[增加属性]2值,omitempty"`

	// 参数
	Param1 float32 `csv:"参数1,omitempty"`
	Param2 float32 `csv:"参数2,omitempty"`
	Param3 float32 `csv:"参数3,omitempty"`
	Param4 float32 `csv:"参数4,omitempty"`
	Param5 float32 `csv:"参数5,omitempty"`
	Param6 float32 `csv:"参数6,omitempty"`
	Param7 float32 `csv:"参数7,omitempty"`

	AddPropList []*AddProp // 增加属性列表
	ParamList   []float32  // 参数列表
}

func (g *GameDataConfig) loadTalentSkillData() {
	g.TalentSkillDataMap = make(map[int32]*TalentSkillData)
	talentSkillDataList := make([]*TalentSkillData, 0)
	readTable[TalentSkillData](g.txtPrefix+"TalentSkillData.txt", &talentSkillDataList)
	for _, talentSkillData := range talentSkillDataList {
		// 增加属性列表
		addPropList := make([]*AddProp, 0)
		if talentSkillData.AddProp1Type != 0 {
			addPropList = append(addPropList, &AddProp{
				Type:  talentSkillData.AddProp1Type,
				Value: talentSkillData.AddProp1Value,
			})
		}
		if talentSkillData.AddProp2Type != 0 {
			addPropList = append(addPropList, &AddProp{
				Type:  talentSkillData.AddProp2Type,
				Value: talentSkillData.AddProp2Value,
			})
		}
		talentSkillData.AddPropList = addPropList
		// 参数列表
		paramList := make([]float32, 0)
		if talentSkillData.Param1 != 0.0 {
			paramList = append(paramList, talentSkillData.Param1)
		}
		if talentSkillData.Param2 != 0.0 {
			paramList = append(paramList, talentSkillData.Param2)
		}
		if talentSkillData.Param3 != 0.0 {
			paramList = append(paramList, talentSkillData.Param3)
		}
		if talentSkillData.Param4 != 0.0 {
			paramList = append(paramList, talentSkillData.Param4)
		}
		if talentSkillData.Param5 != 0.0 {
			paramList = append(paramList, talentSkillData.Param5)
		}
		if talentSkillData.Param6 != 0.0 {
			paramList = append(paramList, talentSkillData.Param6)
		}
		if talentSkillData.Param7 != 0.0 {
			paramList = append(paramList, talentSkillData.Param7)
		}
		talentSkillData.ParamList = paramList
		g.TalentSkillDataMap[talentSkillData.TalentId] = talentSkillData
	}
	logger.Info("TalentSkillData Count: %v", len(g.TalentSkillDataMap))
}

func GetTalentSkillDataById(talentId int32) *TalentSkillData {
	return CONF.TalentSkillDataMap[talentId]
}

func GetTalentSkillDataMap() map[int32]*TalentSkillData {
	return CONF.TalentSkillDataMap
}
