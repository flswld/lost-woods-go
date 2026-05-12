package model

// 抽卡子模型 - 保底计数器持久化（详见 player_gacha.go 抽卡逻辑）
//
// **本项目抽卡是作者自己设计的扩展实现**（3.2 泄漏没带完整抽卡配置 详见 CLAUDE.md "抽卡"）
//
// 保底机制：
//   - OrangeTimes < 74（标准池）/ 63（武器池）→ 每抽 5 星概率正常
//     ≥ 74 / 63 → 概率每抽 +600（线性提升 到 89 抽必出 5 星）
//   - PurpleTimes 类似但 4 星阈值 9 / 8
//   - MustGetUpOrange/Purple: 大保底标记
//     · 上一次 5 星非 UP 时置 true 下一次 5 星必出 UP
//     · 抽到 UP 后清回 false（重置大保底）
//
// 4 个固定池 GachaType: 300（温迪）/400（可莉）/431（阿莫斯+天空之傲）/201（常驻）
//   每个池独立计数（5 星不互通保底）

// DbGacha 抽卡模块根模型
type DbGacha struct {
	GachaPoolInfo map[uint32]*GachaPoolInfo // key: GachaType (300/400/431/201)
}

// GachaPoolInfo 单个卡池的保底状态
type GachaPoolInfo struct {
	GachaType       uint32 // 卡池类型 与 4 个硬编码卡池对应
	OrangeTimes     uint32 // 5 星保底计数（距上次 5 星的抽数）
	PurpleTimes     uint32 // 4 星保底计数
	MustGetUpOrange bool   // 5 星大保底（上次 5 星非 UP → 下次 5 星必出 UP）
	MustGetUpPurple bool   // 4 星大保底
}

func (p *Player) GetDbGacha() *DbGacha {
	if p.DbGacha == nil {
		p.DbGacha = &DbGacha{
			GachaPoolInfo: map[uint32]*GachaPoolInfo{
				300: {
					// 温迪
					GachaType:       300,
					OrangeTimes:     0,
					PurpleTimes:     0,
					MustGetUpOrange: false,
					MustGetUpPurple: false,
				},
				400: {
					// 可莉
					GachaType:       400,
					OrangeTimes:     0,
					PurpleTimes:     0,
					MustGetUpOrange: false,
					MustGetUpPurple: false,
				},
				431: {
					// 阿莫斯之弓&天空之傲
					GachaType:       431,
					OrangeTimes:     0,
					PurpleTimes:     0,
					MustGetUpOrange: false,
					MustGetUpPurple: false,
				},
				201: {
					// 常驻
					GachaType:       201,
					OrangeTimes:     0,
					PurpleTimes:     0,
					MustGetUpOrange: false,
					MustGetUpPurple: false,
				},
			},
		}
	}
	return p.DbGacha
}
