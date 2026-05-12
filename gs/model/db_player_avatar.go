package model

// 玩家角色子模型 - 持久化在玩家档（Db* 前缀表示持久化字段）
//
// **关键约定**：
//   - `bson:"-" msgpack:"-"` 标签字段是"在线态"运行时数据 不持久化
//     上线时由 InitAvatar/InitDbAvatar 重建（从 SkillLevelMap/Promote 等持久字段计算）
//   - `Guid` 用雪花 id 生成（uint64 全服唯一）每次上线重新分配 不在存档里
//   - `FightPropMap` 战斗属性 在线计算（基础值 + 武器加成 + 圣遗物加成）不存档
//   - **CurrHP/CurrEnergy 在持久层** 这样下线后再上来不会满血复活
//
// 角色养成相关字段：
//   - Level + Exp + Promote: 等级 + 经验 + 突破阶段（突破解锁等级上限）
//   - SkillLevelMap: 三种技能（普攻/E/Q）等级 PromoteRewardMap: 突破奖励领取状态
//   - TalentIdList: 已解锁命之座 ID 列表（最多 6 命 itemId+100 是命星道具）
//   - FetterList + FetterLevel + FetterExp: 好感度系统（每聊天/任务后涨）
//   - FlyCloak/Costume: 当前装配的风之翼/衣装（账号级 全角色共享 见 DbAvatar.FlyCloakList）
//
// 装备关系（在线态 EquipGuidMap）：
//   - 武器 1 把（EquipWeapon）+ 圣遗物 5 件（EquipReliquaryMap[slot]）
//   - 通过 Weapon/Reliquary.WearAvatarId 反向索引保证唯一装配
//
// **效果不生效现状**：命之座/天赋升级数据存档完整 但 Ability 系统未实现
// 详见 CLAUDE.md "Ability 系统现状"

import (
	"time"

	"hk4e/common/constant"
	"hk4e/gdconf"

	"github.com/flswld/halo/logger"
)

// DbAvatar 角色模块根模型 挂在 Player.DbAvatar
type DbAvatar struct {
	AvatarMap        map[uint32]*Avatar // 玩家拥有的角色 key:avatarId（如 10000007=荧）
	MainCharAvatarId uint32             // 主角 id（只能是 10000005 空 / 10000007 荧 出生时选定）
	FlyCloakList     []uint32           // 已解锁的风之翼 ID 列表（账号级 全角色共享）
	CostumeList      []uint32           // 已解锁的角色衣装 ID 列表（账号级）
}

func (p *Player) GetDbAvatar() *DbAvatar {
	if p.DbAvatar == nil {
		p.DbAvatar = new(DbAvatar)
	}
	if p.DbAvatar.AvatarMap == nil {
		p.DbAvatar.AvatarMap = make(map[uint32]*Avatar)
	}
	if p.DbAvatar.MainCharAvatarId == 0 {
		p.DbAvatar.MainCharAvatarId = 0
	}
	if p.DbAvatar.FlyCloakList == nil {
		p.DbAvatar.FlyCloakList = make([]uint32, 0)
	}
	if p.DbAvatar.CostumeList == nil {
		p.DbAvatar.CostumeList = make([]uint32, 0)
	}
	return p.DbAvatar
}

type Avatar struct {
	AvatarId          uint32               // 角色id
	LifeState         uint16               // 存活状态
	Level             uint8                // 等级
	Exp               uint32               // 经验值
	Promote           uint8                // 突破等阶
	Satiation         uint32               // 饱食度
	SatiationPenalty  uint32               // 饱食度溢出
	CurrHP            float64              // 当前生命值
	CurrEnergy        float64              // 当前元素能量值
	FetterList        []uint32             // 资料解锁条目
	SkillLevelMap     map[uint32]uint32    // 技能等级数据
	TalentIdList      []uint32             // 命座数据
	SkillDepotId      uint32               // 技能库id
	FlyCloak          uint32               // 当前风之翼
	Costume           uint32               // 当前衣装
	BornTime          int64                // 获得时间
	FetterLevel       uint8                // 好感度等级
	FetterExp         uint32               // 好感度经验
	PromoteRewardMap  map[uint32]bool      // 突破奖励 map[突破等级]是否已被领取
	Guid              uint64               `bson:"-" msgpack:"-"`
	EquipGuidMap      map[uint64]uint64    `bson:"-" msgpack:"-"`
	EquipWeapon       *Weapon              `bson:"-" msgpack:"-"`
	EquipReliquaryMap map[uint8]*Reliquary `bson:"-" msgpack:"-"`
	FightPropMap      map[uint32]float32   `bson:"-" msgpack:"-"`
}

func (a *DbAvatar) GetAvatarById(avatarId uint32) *Avatar {
	return a.AvatarMap[avatarId]
}

func (a *DbAvatar) GetAvatarMap() map[uint32]*Avatar {
	return a.AvatarMap
}

func (a *DbAvatar) InitDbAvatar(player *Player) {
	for _, avatar := range a.AvatarMap {
		a.InitAvatar(player, avatar)
	}
}

func (a *DbAvatar) LoadOfflineFightProp(avatar *Avatar) {
	// 当前血量
	avatar.FightPropMap[constant.FIGHT_PROP_CUR_HP] = float32(avatar.CurrHP)
	// 当前元素能量
	avatarSkillDataConfig := gdconf.GetAvatarEnergySkillConfig(avatar.SkillDepotId)
	if avatarSkillDataConfig != nil {
		fightPropEnergy := constant.ELEMENT_TYPE_FIGHT_PROP_ENERGY_MAP[int(avatarSkillDataConfig.CostElemType)]
		avatar.FightPropMap[uint32(fightPropEnergy.MaxEnergy)] = float32(avatarSkillDataConfig.CostElemVal)
		avatar.FightPropMap[uint32(fightPropEnergy.CurEnergy)] = float32(avatar.CurrEnergy)
	}
}

func (a *DbAvatar) SaveOfflineFightProp(avatar *Avatar) {
	// 当前血量
	avatar.CurrHP = float64(avatar.FightPropMap[constant.FIGHT_PROP_CUR_HP])
	// 当前元素能量
	avatarSkillDataConfig := gdconf.GetAvatarEnergySkillConfig(avatar.SkillDepotId)
	if avatarSkillDataConfig != nil {
		fightPropEnergy := constant.ELEMENT_TYPE_FIGHT_PROP_ENERGY_MAP[int(avatarSkillDataConfig.CostElemType)]
		avatar.CurrEnergy = float64(avatar.FightPropMap[uint32(fightPropEnergy.CurEnergy)])
	}
}

func (a *DbAvatar) InitAvatar(player *Player, avatar *Avatar) {
	// 角色战斗属性
	avatar.FightPropMap = make(map[uint32]float32)
	a.LoadOfflineFightProp(avatar)
	// guid
	avatar.Guid = player.GetNextGameObjectGuid()
	player.GameObjectGuidMap[avatar.Guid] = GameObject(avatar)
	avatar.EquipGuidMap = make(map[uint64]uint64)
	avatar.EquipReliquaryMap = make(map[uint8]*Reliquary)
	a.AvatarMap[avatar.AvatarId] = avatar
	return
}

func (a *DbAvatar) UpdateAllAvatarFightProp() {
	for _, avatar := range a.AvatarMap {
		a.UpdateAvatarFightProp(avatar)
	}
}

// UpdateAvatarFightProp 更新角色面板
func (a *DbAvatar) UpdateAvatarFightProp(avatar *Avatar) {
	// 清空动态计算的战斗属性
	a.SaveOfflineFightProp(avatar)
	avatar.FightPropMap = make(map[uint32]float32)
	a.LoadOfflineFightProp(avatar)
	// 角色基础属性
	avatarDataConfig := gdconf.GetAvatarDataById(int32(avatar.AvatarId))
	if avatarDataConfig == nil {
		logger.Error("avatarDataConfig error, avatarId: %v", avatar.AvatarId)
		return
	}
	avatar.FightPropMap[constant.FIGHT_PROP_NONE] = 0.0
	for k, v := range gdconf.GetAvatarFightPropMap(avatar.AvatarId, avatar.Level, avatar.Promote) {
		avatar.FightPropMap[k] = v
	}
	// 武器基础属性加成
	weaponItemConfig := gdconf.GetItemDataById(int32(avatar.EquipWeapon.ItemId))
	if weaponItemConfig == nil {
		logger.Error("weaponItemConfig is nil, itemId: %v", avatar.EquipWeapon.ItemId)
		return
	}
	for _, prop := range weaponItemConfig.PropList {
		curveConfig := gdconf.GetWeaponCurveByLevelAndType(int32(avatar.EquipWeapon.Level), prop.Curve)
		if curveConfig == nil {
			logger.Error("curveConfig is nil, level: %v, curve: %v", avatar.EquipWeapon.Level, prop.Curve)
			return
		}
		avatar.FightPropMap[uint32(prop.Type)] += prop.Value * curveConfig.Value
	}
	// 圣遗物属性加成
	for _, reliquary := range avatar.EquipReliquaryMap {
		// 主词条
		reliquaryItemConfig := gdconf.GetItemDataById(int32(reliquary.ItemId))
		if reliquaryItemConfig == nil {
			logger.Error("reliquaryItemConfig is nil, itemId: %v", reliquary.ItemId)
			return
		}
		reliquaryMainConfig := gdconf.GetReliquaryMainDataByPropId(int32(reliquary.MainPropId))
		if reliquaryMainConfig == nil {
			logger.Error("reliquaryMainConfig is nil, mainPropDepotId: %v, mainPropId: %v", reliquaryItemConfig.MainPropDepotId, reliquary.MainPropId)
			return
		}
		reliquaryLevelConfig := gdconf.GetReliquaryLevelDataByStageAndLevel(reliquaryItemConfig.Stage, int32(reliquary.Level))
		if reliquaryLevelConfig == nil {
			logger.Error("reliquaryLevelConfig is nil, stage: %v, level: %v", reliquaryItemConfig.Stage, reliquary.Level)
			return
		}
		addProp := reliquaryLevelConfig.AddPropMap[reliquaryMainConfig.PropType]
		avatar.FightPropMap[uint32(addProp.Type)] += addProp.Value
		// 副词条
		for _, appendPropId := range reliquary.AppendPropIdList {
			reliquaryAffixConfig := gdconf.GetReliquaryAffixDataByPropId(int32(appendPropId))
			if reliquaryAffixConfig == nil {
				logger.Error("reliquaryAffixConfig is nil, appendPropDepotId: %v, appendPropId: %v", reliquaryItemConfig.AppendPropDepotId, appendPropId)
				return
			}
			avatar.FightPropMap[uint32(reliquaryAffixConfig.PropType)] += reliquaryAffixConfig.AppendPropValue
		}
	}
	// 攻防血绿字计算
	fpm := avatar.FightPropMap
	fpm[constant.FIGHT_PROP_CUR_ATTACK] = fpm[constant.FIGHT_PROP_BASE_ATTACK]*(1.0+fpm[constant.FIGHT_PROP_ATTACK_PERCENT]) + fpm[constant.FIGHT_PROP_ATTACK]
	fpm[constant.FIGHT_PROP_CUR_DEFENSE] = fpm[constant.FIGHT_PROP_BASE_DEFENSE]*(1.0+fpm[constant.FIGHT_PROP_DEFENSE_PERCENT]) + fpm[constant.FIGHT_PROP_DEFENSE]
	fpm[constant.FIGHT_PROP_MAX_HP] = fpm[constant.FIGHT_PROP_BASE_HP]*(1.0+fpm[constant.FIGHT_PROP_HP_PERCENT]) + fpm[constant.FIGHT_PROP_HP]
}

func (a *DbAvatar) AddAvatar(player *Player, avatarId uint32) {
	avatarDataConfig := gdconf.GetAvatarDataById(int32(avatarId))
	if avatarDataConfig == nil {
		logger.Error("avatar data config is nil, avatarId: %v", avatarId)
		return
	}
	avatar := &Avatar{
		AvatarId:          avatarId,
		LifeState:         constant.LIFE_STATE_ALIVE,
		Level:             1,
		Exp:               0,
		Promote:           0,
		Satiation:         0,
		SatiationPenalty:  0,
		CurrHP:            0,
		CurrEnergy:        0,
		FetterList:        make([]uint32, 0),
		SkillLevelMap:     make(map[uint32]uint32),
		TalentIdList:      make([]uint32, 0),
		SkillDepotId:      0,
		FlyCloak:          140001,
		Costume:           0,
		BornTime:          time.Now().Unix(),
		FetterLevel:       1,
		FetterExp:         0,
		Guid:              0,
		EquipGuidMap:      nil,
		EquipWeapon:       nil,
		EquipReliquaryMap: nil,
		FightPropMap:      nil,
		PromoteRewardMap:  make(map[uint32]bool, len(avatarDataConfig.PromoteRewardMap)),
	}

	avatar.CurrHP = float64(gdconf.GetAvatarFightPropMap(avatar.AvatarId, avatar.Level, avatar.Promote)[constant.FIGHT_PROP_BASE_HP])
	// 角色突破奖励领取状态
	for promoteLevel := range avatarDataConfig.PromoteRewardMap {
		avatar.PromoteRewardMap[promoteLevel] = false
	}

	a.AvatarMap[avatarId] = avatar
	a.ChangeSkillDepot(avatarId, uint32(avatarDataConfig.SkillDepotId))
	a.InitAvatar(player, avatar)
}

func (a *DbAvatar) DelAvatar(player *Player, avatarId uint32) {
	avatar := a.AvatarMap[avatarId]
	a.TakeOffWeapon(avatarId, avatar.EquipWeapon)
	for _, reliquary := range avatar.EquipReliquaryMap {
		a.TakeOffReliquary(avatarId, reliquary)
	}
	delete(a.AvatarMap, avatarId)
	delete(player.GameObjectGuidMap, avatar.Guid)
}

func (a *DbAvatar) ChangeSkillDepot(avatarId uint32, skillDepotId uint32) {
	avatar, exist := a.AvatarMap[avatarId]
	if !exist {
		logger.Error("avatar not exist, avatarId: %v", avatarId)
		return
	}
	avatarSkillDepotDataConfig := gdconf.GetAvatarSkillDepotDataById(int32(skillDepotId))
	if avatarSkillDepotDataConfig == nil {
		logger.Error("avatar skill depot data config is nil, skillDepotId: %v", skillDepotId)
		return
	}
	avatar.SkillDepotId = skillDepotId
	// 元素爆发
	_, exist = avatar.SkillLevelMap[uint32(avatarSkillDepotDataConfig.EnergySkill)]
	if !exist {
		avatar.SkillLevelMap[uint32(avatarSkillDepotDataConfig.EnergySkill)] = 1
	}
	for _, skillId := range avatarSkillDepotDataConfig.Skills {
		// 小技能
		_, exist = avatar.SkillLevelMap[uint32(skillId)]
		if !exist {
			avatar.SkillLevelMap[uint32(skillId)] = 1
		}
	}
}

func (a *DbAvatar) WearReliquary(avatarId uint32, reliquary *Reliquary) {
	avatar := a.AvatarMap[avatarId]
	reliquaryConfig := gdconf.GetItemDataById(int32(reliquary.ItemId))
	if reliquaryConfig == nil {
		logger.Error("reliquary config error, itemId: %v", reliquary.ItemId)
		return
	}
	avatar.EquipReliquaryMap[uint8(reliquaryConfig.ReliquaryType)] = reliquary
	reliquary.AvatarId = avatarId
	avatar.EquipGuidMap[reliquary.Guid] = reliquary.Guid
}

func (a *DbAvatar) TakeOffReliquary(avatarId uint32, reliquary *Reliquary) {
	avatar := a.AvatarMap[avatarId]
	reliquaryConfig := gdconf.GetItemDataById(int32(reliquary.ItemId))
	if reliquaryConfig == nil {
		logger.Error("reliquary config error, itemId: %v", reliquary.ItemId)
		return
	}
	delete(avatar.EquipReliquaryMap, uint8(reliquaryConfig.ReliquaryType))
	reliquary.AvatarId = 0
	delete(avatar.EquipGuidMap, reliquary.Guid)
}

func (a *DbAvatar) WearWeapon(avatarId uint32, weapon *Weapon) {
	avatar := a.AvatarMap[avatarId]
	avatar.EquipWeapon = weapon
	weapon.AvatarId = avatarId
	avatar.EquipGuidMap[weapon.Guid] = weapon.Guid
}

func (a *DbAvatar) TakeOffWeapon(avatarId uint32, weapon *Weapon) {
	avatar := a.AvatarMap[avatarId]
	avatar.EquipWeapon = nil
	weapon.AvatarId = 0
	delete(avatar.EquipGuidMap, weapon.Guid)
}

func (a *DbAvatar) GetAvatarElementType(avatarId uint32) int {
	avatar := a.AvatarMap[avatarId]
	avatarSkillDepotDataConfig := gdconf.GetAvatarSkillDepotDataById(int32(avatar.SkillDepotId))
	if avatarSkillDepotDataConfig == nil {
		return 0
	}
	avatarSkillDataConfig := gdconf.GetAvatarSkillDataById(avatarSkillDepotDataConfig.EnergySkill)
	if avatarSkillDataConfig == nil {
		return 0
	}
	return int(avatarSkillDataConfig.CostElemType)
}
