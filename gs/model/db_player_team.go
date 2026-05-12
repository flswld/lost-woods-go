package model

// 玩家队伍子模型 - 多个队伍 + 当前出战角色（与官服一致）
//
// **队伍机制**：
//   - 4 个常驻队伍（TeamList 长度固定 4）每个队伍最多 4 个角色（AvatarIdList 长度固定 4）
//   - CurrTeamIndex: 当前激活的队伍索引（0-3）
//   - CurrAvatarIndex: 当前队伍内的活跃角色索引（0-3）即玩家当前操控的角色
//
// **AvatarIdList[i] = 0 表示空槽位**（如队伍 1 只有 3 个角色 第 4 位是 0）
//   GetAvatarIdList 过滤零值返回真实角色列表 SetAvatarIdList 自动 padding 到 4 个槽位
//
// **多人世界队伍**：
//   - 单人模式：CurrTeamIndex 选定的 TeamList 决定队伍
//   - 多人模式：World.multiplayerTeam 整合所有玩家本地队伍成 worldTeam（详见 game_world_manager.go UpdateMultiplayerTeam）
//   - AI 世界（PUBG）：仅取每个玩家的 1 个出战角色（不展开整个队伍 详见 World.AddPlayer）
//
// **元素共鸣**（TeamResonances）：在线计算 不持久化
//   - 4 个相同元素 → 元素共鸣加成（如全火队 +25% 攻击）
//   - 2+2 元素 → 双共鸣
//   - 通过 TeamResonancesConfig 决定生效的共鸣 ID 影响 FightProp 计算
//   - **当前可能仅做了数据流不实际生效**（Ability 系统未完整 详见 CLAUDE.md）

type Team struct {
	Name         string   // 队伍名（玩家自定义）
	AvatarIdList []uint32 // 4 个槽位 0 表示空 GetAvatarIdList 自动过滤
}

func (t *Team) GetAvatarIdList() []uint32 {
	avatarIdList := make([]uint32, 0)
	for _, avatarId := range t.AvatarIdList {
		if avatarId == 0 {
			continue
		}
		avatarIdList = append(avatarIdList, avatarId)
	}
	return avatarIdList
}

func (t *Team) SetAvatarIdList(avatarIdList []uint32) {
	t.AvatarIdList = make([]uint32, 4)
	for index := range t.AvatarIdList {
		if index >= len(avatarIdList) {
			break
		}
		t.AvatarIdList[index] = avatarIdList[index]
	}
}

// DbTeam 队伍模块根模型
type DbTeam struct {
	TeamList             []*Team         // 固定 4 个常驻队伍
	CurrTeamIndex        uint8           // 当前队伍索引 0-3
	CurrAvatarIndex      uint8           // 队伍内当前出战角色索引 0-3
	TeamResonances       map[uint16]bool `bson:"-" msgpack:"-"` // 在线态：元素共鸣 ID 集合
	TeamResonancesConfig map[int32]bool  `bson:"-" msgpack:"-"` // 在线态：元素共鸣配置生效标志
}

func (p *Player) GetDbTeam() *DbTeam {
	if p.DbTeam == nil {
		p.DbTeam = new(DbTeam)
	}
	if p.DbTeam.TeamList == nil {
		p.DbTeam.TeamList = []*Team{
			{Name: "", AvatarIdList: make([]uint32, 4)},
			{Name: "", AvatarIdList: make([]uint32, 4)},
			{Name: "", AvatarIdList: make([]uint32, 4)},
			{Name: "", AvatarIdList: make([]uint32, 4)},
		}
	}
	if p.DbTeam.CurrTeamIndex == 0 {
		p.DbTeam.CurrTeamIndex = 0
	}
	if p.DbTeam.CurrAvatarIndex == 0 {
		p.DbTeam.CurrAvatarIndex = 0
	}
	return p.DbTeam
}

func (t *DbTeam) GetActiveTeamId() uint8 {
	return t.CurrTeamIndex + 1
}

func (t *DbTeam) GetTeamByIndex(teamIndex uint8) *Team {
	if t.TeamList == nil {
		return nil
	}
	if teamIndex >= uint8(len(t.TeamList)) {
		return nil
	}
	activeTeam := t.TeamList[teamIndex]
	return activeTeam
}

func (t *DbTeam) GetActiveTeam() *Team {
	return t.GetTeamByIndex(t.CurrTeamIndex)
}

func (t *DbTeam) GetActiveAvatarId() uint32 {
	team := t.GetActiveTeam()
	if team == nil {
		return 0
	}
	return team.AvatarIdList[t.CurrAvatarIndex]
}

func (t *DbTeam) GetTeamList() []*Team {
	return t.TeamList
}
