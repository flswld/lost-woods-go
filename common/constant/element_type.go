// 元素类型常量 - 原神七大元素
//
// 7 种元素（与官服 ElementType 一致）：
//   - FIRE(1)   火     - 钟离/胡桃/可莉
//   - WATER(2)  水     - 神里绫人/夜兰/达达利亚
//   - GRASS(3)  草     - 提纳里/纳西妲（3.0+ 加入）
//   - ELEC(4)   雷     - 雷电将军/八重神子
//   - ICE(5)    冰     - 神里绫华/七七/甘雨
//   - 注意：跳过了 6（项目作者也不知道为什么 米哈游协议留空）
//   - WIND(7)   风     - 温迪/枫原万叶
//   - ROCK(8)   岩     - 钟离/凯亚岩主角
//
// **岩元素特殊**：注释里说"凯亚岩主角"是误注释——主角才能切换七国元素
//
// ELEMENT_TYPE_FIGHT_PROP_ENERGY_MAP 把元素类型映射到对应的能量条 fightProp
//   每个元素有独立的 CurEnergy/MaxEnergy 用于角色 Q 大招冷却

package constant

const (
	ELEMENT_TYPE_NONE  = 0
	ELEMENT_TYPE_FIRE  = 1
	ELEMENT_TYPE_WATER = 2
	ELEMENT_TYPE_GRASS = 3
	ELEMENT_TYPE_ELEC  = 4
	ELEMENT_TYPE_ICE   = 5
	ELEMENT_TYPE_WIND  = 7
	ELEMENT_TYPE_ROCK  = 8
)

type FightPropEnergy struct {
	CurEnergy int
	MaxEnergy int
}

var ELEMENT_TYPE_FIGHT_PROP_ENERGY_MAP map[int]*FightPropEnergy

func init() {
	ELEMENT_TYPE_FIGHT_PROP_ENERGY_MAP = make(map[int]*FightPropEnergy)
	ELEMENT_TYPE_FIGHT_PROP_ENERGY_MAP[ELEMENT_TYPE_FIRE] = &FightPropEnergy{
		CurEnergy: FIGHT_PROP_CUR_FIRE_ENERGY,
		MaxEnergy: FIGHT_PROP_MAX_FIRE_ENERGY,
	}
	ELEMENT_TYPE_FIGHT_PROP_ENERGY_MAP[ELEMENT_TYPE_WATER] = &FightPropEnergy{
		CurEnergy: FIGHT_PROP_CUR_WATER_ENERGY,
		MaxEnergy: FIGHT_PROP_MAX_WATER_ENERGY,
	}
	ELEMENT_TYPE_FIGHT_PROP_ENERGY_MAP[ELEMENT_TYPE_GRASS] = &FightPropEnergy{
		CurEnergy: FIGHT_PROP_CUR_GRASS_ENERGY,
		MaxEnergy: FIGHT_PROP_MAX_GRASS_ENERGY,
	}
	ELEMENT_TYPE_FIGHT_PROP_ENERGY_MAP[ELEMENT_TYPE_ELEC] = &FightPropEnergy{
		CurEnergy: FIGHT_PROP_CUR_ELEC_ENERGY,
		MaxEnergy: FIGHT_PROP_MAX_ELEC_ENERGY,
	}
	ELEMENT_TYPE_FIGHT_PROP_ENERGY_MAP[ELEMENT_TYPE_ICE] = &FightPropEnergy{
		CurEnergy: FIGHT_PROP_CUR_ICE_ENERGY,
		MaxEnergy: FIGHT_PROP_MAX_ICE_ENERGY,
	}
	ELEMENT_TYPE_FIGHT_PROP_ENERGY_MAP[ELEMENT_TYPE_WIND] = &FightPropEnergy{
		CurEnergy: FIGHT_PROP_CUR_WIND_ENERGY,
		MaxEnergy: FIGHT_PROP_MAX_WIND_ENERGY,
	}
	ELEMENT_TYPE_FIGHT_PROP_ENERGY_MAP[ELEMENT_TYPE_ROCK] = &FightPropEnergy{
		CurEnergy: FIGHT_PROP_CUR_ROCK_ENERGY,
		MaxEnergy: FIGHT_PROP_MAX_ROCK_ENERGY,
	}
}
