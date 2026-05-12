// 实体视野等级 - 控制实体在不同距离下是否可见
//
// 每个实体有一个 visionLevel 决定其有效视野范围（VisionRange 米）
// gs/game/player_scene.go IsInVision 用此判断实体是否在玩家视野内
//
// 6 个等级（按视野从小到大）：
//   - SUPER_NEARBY (5):   20m  - 极近距离（小物件如果实/草）
//   - NEARBY (4):         40m  - 近距离（采集物/小怪）
//   - NORMAL (0):         80m  - 普通（大部分怪物/物件）
//   - LITTLE_REMOTE (1): 160m  - 略远（首领怪/大型物件）
//   - REMOTE (2):       1000m  - 远距离（地标性物件）
//   - SUPER (3):        4000m  - 超远（场景级 always 可见 如世界地标）
//
// GridWidth 对应 AOI 格子大小 不同视野等级用不同粒度的 AOI 索引

package constant

const (
	VISION_LEVEL_NORMAL        = 0
	VISION_LEVEL_LITTLE_REMOTE = 1
	VISION_LEVEL_REMOTE        = 2
	VISION_LEVEL_SUPER         = 3
	VISION_LEVEL_NEARBY        = 4
	VISION_LEVEL_SUPER_NEARBY  = 5
)

type Vision struct {
	VisionRange uint32
	GridWidth   uint32
}

var VISION_LEVEL map[int]*Vision

func init() {
	VISION_LEVEL = map[int]*Vision{
		VISION_LEVEL_NORMAL:        {VisionRange: 80, GridWidth: 20},
		VISION_LEVEL_LITTLE_REMOTE: {VisionRange: 160, GridWidth: 40},
		VISION_LEVEL_REMOTE:        {VisionRange: 1000, GridWidth: 250},
		VISION_LEVEL_SUPER:         {VisionRange: 4000, GridWidth: 1000},
		VISION_LEVEL_NEARBY:        {VisionRange: 40, GridWidth: 20},
		VISION_LEVEL_SUPER_NEARBY:  {VisionRange: 20, GridWidth: 10},
	}
}
