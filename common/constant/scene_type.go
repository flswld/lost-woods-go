// 场景类型常量
//
// 6 种场景：
//   - WORLD: 大世界（蒙德/璃月/须弥/枫丹...）SceneId 通常 1~50
//   - DUNGEON: 副本（**项目内未实现** 进副本是空场景）
//   - ROOM: 房间（小型场景 如剧情/家园内部）
//   - HOME_WORLD/HOME_ROOM: 尘歌壶（**项目未实现**）
//   - ACTIVITY: 活动场景（**项目未实现**）
//
// 用于 player_scene.go PostEnterSceneReq 按场景类型触发不同任务条件：
//   WORLD → QUEST_FINISH_COND_TYPE_ENTER_MY_WORLD
//   DUNGEON → QUEST_FINISH_COND_TYPE_ENTER_DUNGEON
//   ROOM → QUEST_FINISH_COND_TYPE_ENTER_ROOM

package constant

const (
	SCENE_TYPE_NONE       = 0
	SCENE_TYPE_WORLD      = 1
	SCENE_TYPE_DUNGEON    = 2
	SCENE_TYPE_ROOM       = 3
	SCENE_TYPE_HOME_WORLD = 4
	SCENE_TYPE_HOME_ROOM  = 5
	SCENE_TYPE_ACTIVITY   = 6
)
