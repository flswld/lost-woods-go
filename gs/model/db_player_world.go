package model

// 玩家世界子模型 - 持久化大世界进度
//
// **本模型只存"全局/账号级"进度**，单个场景的格子细节（怪物状态/宝箱开关等）在 SceneBlock 表中
//
// 字段语义：
//   - SceneMap[sceneId]: 每个场景的解锁状态（传送点 / 区域 / 场景标签）
//   - MapMarkList: 玩家在地图上手动标记的点（最多通常 N 个 客户端 MarkMapReq 控制）
//   - GameTime: **房主玩家累计游戏时间**（秒 一直累加 离线不计时 不循环 初始 1029）
//     · 客户端拿这个秒数 mod 86400 算当天某个时刻 驱动昼夜变化/天气/NPC 对话分支
//     · 多人世界使用房主的时间 不是同步——你进朋友世界看到的是朋友的时间
//   - WidgetSlotMap: 小工具栏（如夜兰回血 派蒙的"心愿之歌"等）
//
// DbScene 单场景解锁项（懒加载 GetSceneById 时自动创建）：
//   - UnlockPointMap: 已解锁的传送锚点 ID（七天神像 + 普通传送点）
//   - UnHidePointMap: 已揭开的隐藏锚点 ID（如风龙废墟内的传送）
//   - UnlockAreaMap: 已解锁的地图区域 ID（蒙德城/望风山地等）
//   - SceneTagMap: 场景标签（季节性活动 / 任务进度相关的场景状态）

import (
	"hk4e/gdconf"
)

// DbWorld 大世界模块根模型
type DbWorld struct {
	SceneMap      map[uint32]*DbScene // 每场景独立的解锁状态
	MapMarkList   []*MapMark          // 玩家手动地图标记
	GameTime      uint32              // 玩家累计游戏时间（秒）每秒+1 离线不计时 不循环 初始 1029（与官服新号开场一致）
	WidgetSlotMap map[uint8]*Widget   // 小工具栏配置
}

// DbScene 单场景解锁状态 GetSceneById 时懒加载创建
type DbScene struct {
	SceneId        uint32
	UnlockPointMap map[uint32]bool // 已解锁的传送锚点
	UnHidePointMap map[uint32]bool // 已揭开的隐藏锚点
	UnlockAreaMap  map[uint32]bool // 已解锁的地图区域
	SceneTagMap    map[uint32]bool // 场景标签（活动/任务进度相关）
}

// MapMark 玩家手动地图标记
type MapMark struct {
	SceneId   uint32
	Pos       *Vector
	PointType uint32 // 标记类型（任务/玩家/...）
	Name      string
}

// Widget 小工具栏槽位
type Widget struct {
	Tag        uint8  // 槽位类型
	MaterialId uint32 // 小工具物品 id
}

func (p *Player) GetDbWorld() *DbWorld {
	if p.DbWorld == nil {
		p.DbWorld = new(DbWorld)
		p.DbWorld.GameTime = 1029 // 与官服新号开场一致 之后每秒+1 离线不计时
	}
	if p.DbWorld.SceneMap == nil {
		p.DbWorld.SceneMap = make(map[uint32]*DbScene)
	}
	if p.DbWorld.MapMarkList == nil {
		p.DbWorld.MapMarkList = make([]*MapMark, 0)
	}
	if p.DbWorld.WidgetSlotMap == nil {
		p.DbWorld.WidgetSlotMap = make(map[uint8]*Widget)
	}
	return p.DbWorld
}

func (w *DbWorld) GetSceneById(sceneId uint32) *DbScene {
	scene, exist := w.SceneMap[sceneId]
	// 不存在自动创建场景
	if !exist {
		// 拒绝创建配置表中不存在的非法场景
		sceneDataConfig := gdconf.GetSceneDataById(int32(sceneId))
		if sceneDataConfig == nil {
			return nil
		}
		scene = new(DbScene)
		w.SceneMap[sceneId] = scene
	}
	if scene.SceneId == 0 {
		scene.SceneId = sceneId
	}
	if scene.UnlockPointMap == nil {
		scene.UnlockPointMap = make(map[uint32]bool)
	}
	if scene.UnHidePointMap == nil {
		scene.UnHidePointMap = make(map[uint32]bool)
	}
	if scene.UnlockAreaMap == nil {
		scene.UnlockAreaMap = make(map[uint32]bool)
	}
	if scene.SceneTagMap == nil {
		scene.SceneTagMap = make(map[uint32]bool)
	}
	return scene
}

func (s *DbScene) GetUnHidePointList() []uint32 {
	unHidePointList := make([]uint32, 0, len(s.UnHidePointMap))
	for pointId := range s.UnHidePointMap {
		unHidePointList = append(unHidePointList, pointId)
	}
	return unHidePointList
}

func (s *DbScene) GetUnlockPointList() []uint32 {
	unlockPointList := make([]uint32, 0, len(s.UnlockPointMap))
	for pointId := range s.UnlockPointMap {
		unlockPointList = append(unlockPointList, pointId)
	}
	return unlockPointList
}

func (s *DbScene) UnlockPoint(pointId uint32) {
	pointDataConfig := gdconf.GetScenePointBySceneIdAndPointId(int32(s.SceneId), int32(pointId))
	if pointDataConfig == nil {
		return
	}
	s.UnlockPointMap[pointId] = true
	// 隐藏锚点取消隐藏
	if pointDataConfig.IsModelHidden {
		s.UnHidePointMap[pointId] = true
	}
}

func (s *DbScene) CheckPointUnlock(pointId uint32) bool {
	_, exist := s.UnlockPointMap[pointId]
	return exist
}

func (s *DbScene) GetUnlockAreaList() []uint32 {
	unlockAreaList := make([]uint32, 0, len(s.UnlockAreaMap))
	for areaId := range s.UnlockAreaMap {
		unlockAreaList = append(unlockAreaList, areaId)
	}
	return unlockAreaList
}

func (s *DbScene) UnlockArea(areaId uint32) {
	exist := false
	for _, worldAreaData := range gdconf.GetWorldAreaDataMap() {
		if uint32(worldAreaData.SceneId) == s.SceneId && uint32(worldAreaData.AreaId1) == areaId {
			exist = true
			break
		}
	}
	if !exist {
		return
	}
	s.UnlockAreaMap[areaId] = true
}

func (s *DbScene) CheckAreaUnlock(areaId uint32) bool {
	_, exist := s.UnlockAreaMap[areaId]
	return exist
}

func (s *DbScene) GetSceneTagList() []uint32 {
	sceneTagList := make([]uint32, 0, len(s.SceneTagMap))
	for sceneTag := range s.SceneTagMap {
		sceneTagList = append(sceneTagList, sceneTag)
	}
	return sceneTagList
}

func (s *DbScene) AddSceneTag(sceneTag uint32) {
	sceneTagDataConfig := gdconf.GetSceneTagDataById(int32(sceneTag))
	if sceneTagDataConfig == nil {
		return
	}
	if uint32(sceneTagDataConfig.SceneId) != s.SceneId {
		return
	}
	s.SceneTagMap[sceneTag] = true
}

func (s *DbScene) DelSceneTag(sceneTag uint32) {
	delete(s.SceneTagMap, sceneTag)
}
