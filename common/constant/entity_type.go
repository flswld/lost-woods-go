// 实体类型常量
//
// EntityId 高位编码 entityType（详见 game_world_manager.go GetNextWorldEntityId）
// 通过 GetEntityType(entityId) 解析回类型枚举
//
// 9 种实体使用最多：
//   - AVATAR: 玩家角色（每队 4 个）
//   - MONSTER: 怪物
//   - NPC: 任务/对话 NPC
//   - GADGET: 物件（宝箱/机关/采集物/载具/子弹/角色武器实体）
//   - REGION: 触发区域（不可见 玩家进入触发 Lua）
//   - WEAPON: 武器实体（角色装备的武器作为子实体）
//   - TEAM: 队伍实体（多人世界出战队的逻辑实体）
//   - MASSIVE_ENTITY: 大量实体（如风元素染色 / 群怪）
//   - MP_LEVEL: 多人世界等级实体

package constant

const (
	ENTITY_TYPE_NONE             = 0
	ENTITY_TYPE_AVATAR           = 1
	ENTITY_TYPE_MONSTER          = 2
	ENTITY_TYPE_NPC              = 3
	ENTITY_TYPE_GADGET           = 4
	ENTITY_TYPE_REGION           = 5
	ENTITY_TYPE_WEAPON           = 6
	ENTITY_TYPE_WEATHER          = 7
	ENTITY_TYPE_SCENE            = 8
	ENTITY_TYPE_TEAM             = 9
	ENTITY_TYPE_MASSIVE_ENTITY   = 10
	ENTITY_TYPE_MP_LEVEL         = 11
	ENTITY_TYPE_PLAY_TEAM_ENTITY = 12
	ENTITY_TYPE_EYE_POINT        = 13
	ENTITY_TYPE_MAX              = 14
)
