package gdconf

import (
	"github.com/flswld/halo/logger"
)

// 抽卡掉落组配置 - **作者自创扩展配置**（不在原版 3.2 数据中）
//
// 原因：3.2 那次泄漏没带完整抽卡配置 作者自己设计了一套
// 配置文件（game_data_config/ext/）：
//   - GachaDropAvatarUp.csv: 角色 UP 池（温迪/可莉等）
//   - GachaDropWeaponUp.csv: 武器 UP 池（阿莫斯+天空之傲）
//   - GachaDropNormal.csv: 常驻池
//
// 树形结构：
//   - DropId: 掉落组 ID（多个 GachaDrop 共享同一 DropId 形成轮盘）
//   - Weight: 该项的权重
//   - Result: 命中后的结果（叶节点是物品 ID 内部节点是子掉落组 ID）
//   - IsEnd: 是否为叶节点（true 返回 false 递归到 Result 对应的 DropId）
//
// 抽卡算法（详见 player_gacha.go doGachaOnce/doGachaRandDropFull）：
//   一次抽卡 = 多次 RWS 随机直到 IsEnd
//   例：GachaType=300 → 抽到 5星组(=3001) → 抽到 UP5星组(=30012) → 抽到 1022(温迪) → IsEnd

// 当初写卡池算法的时候临时建立的表 以后再做迁移吧

type GachaDrop struct {
	DropId int32 `csv:"DropId"`
	Weight int32 `csv:"Weight"`
	Result int32 `csv:"Result"`
	IsEnd  bool  `csv:"IsEnd"`
}

type GachaDropGroupData struct {
	DropId     int32
	WeightAll  int32
	DropConfig []*GachaDrop
}

func (g *GameDataConfig) loadGachaDropGroupData() {
	g.GachaDropGroupDataMap = make(map[int32]*GachaDropGroupData)
	fileNameList := []string{"GachaDropAvatarUp.csv", "GachaDropWeaponUp.csv", "GachaDropNormal.csv"}
	for _, fileName := range fileNameList {
		gachaDropList := make([]*GachaDrop, 0)
		readExtCsv[GachaDrop](g.extPrefix+fileName, &gachaDropList)
		for _, gachaDrop := range gachaDropList {
			gachaDropGroupData, exist := g.GachaDropGroupDataMap[gachaDrop.DropId]
			if !exist {
				gachaDropGroupData = new(GachaDropGroupData)
				gachaDropGroupData.DropId = gachaDrop.DropId
				gachaDropGroupData.WeightAll = 0
				gachaDropGroupData.DropConfig = make([]*GachaDrop, 0)
				g.GachaDropGroupDataMap[gachaDrop.DropId] = gachaDropGroupData
			}
			gachaDropGroupData.WeightAll += gachaDrop.Weight
			gachaDropGroupData.DropConfig = append(gachaDropGroupData.DropConfig, gachaDrop)
		}
	}
	logger.Info("GachaDropGroupData Count: %v", len(g.GachaDropGroupDataMap))
}

func GetGachaDropGroupDataByDropId(dropId int32) *GachaDropGroupData {
	if CONF.GachaDropGroupDataMap == nil {
		return nil
	}
	return CONF.GachaDropGroupDataMap[dropId]
}
