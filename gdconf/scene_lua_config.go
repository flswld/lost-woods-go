package gdconf

import (
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/flswld/halo/logger"
	lua "github.com/yuin/gopher-lua"
)

// 场景 Lua 配置 - 项目最复杂的配置加载器（详见 CLAUDE.md "Lua 虚拟机 LRU 缓存"）
//
// 数据规模：
//   - 场景 group 数量极大（数万个）每个 group 一个独立 Lua VM
//   - 全部常驻会爆内存（每个 VM 几百 KB → 数万 GB）
//   - 解决：LRU 缓存机制 仅保留最近使用的 1000 个
//
// 三层结构：
//   - SceneLuaConfig: 场景级（如蒙德/璃月）含 sceneConfig + blockMap
//   - Block: 区块级 含 groupMap 玩家存档以 block 为单位
//   - Group: 最小组织单元 含怪物/物件/NPC/触发器/变量/Lua VM
//
// LRU 内存控制（onTickMinute 触发）：
//   - LuaStateLruKeepNum=1000: 常驻上限
//   - 超出时清理最旧的 group VM
//   - 删掉后下次访问该 group 需要重新解析 Lua 文件 → 重建 VM
//   - **state 会重置 但 group 的存档变量在 model.SceneGroup.VariableMap 里持久化所以不丢**
//
// 加载并发：SceneGroupLoaderLimit=4 平衡内存峰值
//   每个 group 加载需占用一个 Lua VM 解析配置后立即释放
//   并发数过高会同时持有大量 VM 导致 OOM

// 场景详情配置数据

const (
	SceneGroupLoaderLimit = 4    // 加载文件的并发数 此操作很耗内存 调大之前请确保你的机器内存足够
	LuaStateLruKeepNum    = 1000 // 内存常驻LUA虚拟机实例数量上限
)

type SceneLuaConfig struct {
	Id          int32
	SceneConfig *SceneConfig     // 地图配置
	BlockMap    map[int32]*Block // 所有的区块
}

type Vector struct {
	X float32 `json:"x"`
	Y float32 `json:"y"`
	Z float32 `json:"z"`
}

type SceneConfig struct {
	BeginPos     *Vector `json:"begin_pos"`
	Size         *Vector `json:"size"`
	BornPos      *Vector `json:"born_pos"`
	BornRot      *Vector `json:"born_rot"`
	DieY         float32 `json:"die_y"`
	VisionAnchor *Vector `json:"vision_anchor"`
}

type Block struct {
	Id               int32
	BlockRange       *BlockRange      // 区块范围坐标
	GroupMap         map[int32]*Group // 所有的group
	groupMapLoadLock sync.Mutex
}

type BlockRange struct {
	Min *Vector `json:"min"`
	Max *Vector `json:"max"`
}

// Group 场景组（最小组织单元）
//
// 一个 group 通常表示场景里一处小角落 例如：
//   - 一处守卫怪 + 1 个宝箱 + 触发器（"打死怪→宝箱出现"）= 1 个 group
//   - 一组 NPC + 关联区域 = 1 个 group
//
// 字段：
//   - 静态数据（来自 group_xxx.lua 加载）：
//     · MonsterMap/NpcMap/GadgetMap: 实体配置
//     · RegionMap: 触发区域（不可见 玩家进入触发 Lua）
//     · TriggerMap: 触发器（条件 + 动作）
//     · VariableMap: group 状态变量（玩家档持久化）
//     · SuiteMap: 不同状态的实体集合（初始/打完怪/拿完宝箱等不同状态）
//   - 运行时（LuaStr / LuaState）：LRU 控制 数万 group VM 不能全常驻
//   - LuaStr 留着是为了 LRU 清掉后能快速重建 VM（不用再读文件）
type Group struct {
	Id              int32                `json:"id"`
	RefreshId       int32                `json:"refresh_id"`
	Area            int32                `json:"area"`
	Pos             *Vector              `json:"pos"`
	DynamicLoad     bool                 `json:"dynamic_load"`
	IsReplaceable   *Replaceable         `json:"is_replaceable"`
	MonsterMap      map[int32]*Monster   `json:"-"` // 怪物
	NpcMap          map[int32]*Npc       `json:"-"` // NPC
	GadgetMap       map[int32]*Gadget    `json:"-"` // 物件
	RegionMap       map[int32]*Region    `json:"-"` // 区域（触发器形状）
	TriggerMap      map[string]*Trigger  `json:"-"` // Lua 触发器（条件+动作）
	VariableMap     map[string]*Variable `json:"-"` // group 状态变量（持久化）
	GroupInitConfig *GroupInitConfig     `json:"-"` // 初始化配置（默认 suite）
	SuiteMap        map[int32]*Suite     `json:"-"` // 小组配置（实体状态切换）
	LuaStr          string               `json:"-"` // Lua 原始字符串（LRU 重建用）
	LuaState        *lua.LState          `json:"-"` // Lua VM 实例（受 LRU 控制）
	BlockId         int32                `json:"-"`
}

type GroupInitConfig struct {
	Suite     int32 `json:"suite"`
	EndSuite  int32 `json:"end_suite"`
	RandSuite bool  `json:"rand_suite"`
}

type Replaceable struct {
	Value      bool  `json:"value"`
	Version    int32 `json:"version"`
	NewBinOnly bool  `json:"new_bin_only"`
}

type Monster struct {
	ConfigId      int32   `json:"config_id"`
	MonsterId     int32   `json:"monster_id"`
	Pos           *Vector `json:"pos"`
	Rot           *Vector `json:"rot"`
	Level         int32   `json:"level"`
	AreaId        int32   `json:"area_id"`
	DropTag       string  `json:"drop_tag"` // 关联MonsterDropData表
	DropId        int32   `json:"drop_id"`
	IsOneOff      bool    `json:"isOneoff"`
	VisionLevel   int32   `json:"vision_level"`
	TitleId       int32   `json:"title_id"`
	SpecialNameId int32   `json:"special_name_id"`
}

type Npc struct {
	ConfigId int32   `json:"config_id"`
	NpcId    int32   `json:"npc_id"`
	Pos      *Vector `json:"pos"`
	Rot      *Vector `json:"rot"`
	AreaId   int32   `json:"area_id"`
}

type Gadget struct {
	ConfigId    int32   `json:"config_id"`
	GadgetId    int32   `json:"gadget_id"`
	Pos         *Vector `json:"pos"`
	Rot         *Vector `json:"rot"`
	Level       int32   `json:"level"`
	AreaId      int32   `json:"area_id"`
	PointType   int32   `json:"point_type"` // 关联GatherData表
	State       int32   `json:"state"`
	VisionLevel int32   `json:"vision_level"`
	DropTag     string  `json:"drop_tag"`
	IsOneOff    bool    `json:"isOneoff"`
	ChestDropId int32   `json:"chest_drop_id"`
}

type Region struct {
	ConfigId   int32     `json:"config_id"`
	Shape      int32     `json:"shape"`
	Radius     float32   `json:"radius"`
	Size       *Vector   `json:"size"`
	Pos        *Vector   `json:"pos"`
	Height     float32   `json:"height"`
	PointArray []*Vector `json:"point_array"`
	AreaId     int32     `json:"area_id"`
}

type Trigger struct {
	ConfigId     int32  `json:"config_id"`
	Name         string `json:"name"`
	Event        int32  `json:"event"`
	Source       string `json:"source"`
	Condition    string `json:"condition"`
	Action       string `json:"action"`
	TriggerCount int32  `json:"trigger_count"`
}

type Variable struct {
	ConfigId  int32  `json:"config_id"`
	Name      string `json:"name"`
	Value     int32  `json:"value"`
	NoRefresh bool   `json:"no_refresh"`
}

type SuiteLuaTable struct {
	MonsterConfigIdList any   `json:"monsters"` // 怪物
	GadgetConfigIdList  any   `json:"gadgets"`  // 物件
	RegionConfigIdList  any   `json:"regions"`  // 区域
	TriggerNameList     any   `json:"triggers"` // 触发器
	RandWeight          int32 `json:"rand_weight"`
}

type Suite struct {
	MonsterConfigIdList []int32
	GadgetConfigIdList  []int32
	RegionConfigIdList  []int32
	TriggerNameList     []string
	RandWeight          int32
}

func (g *GameDataConfig) loadGroup(group *Group, block *Block, sceneId int32, blockId int32) {
	sceneLuaPrefix := g.luaPrefix + "scene/"
	sceneIdStr := strconv.Itoa(int(sceneId))
	groupId := group.Id
	groupIdStr := strconv.Itoa(int(groupId))
	fileData, err := os.ReadFile(sceneLuaPrefix + sceneIdStr + "/scene" + sceneIdStr + "_group" + groupIdStr + ".lua")
	if err != nil {
		logger.Error("open file error: %v", err)
		return
	}
	if fileData[0] == 0xEF && fileData[1] == 0xBB && fileData[2] == 0xBF {
		fileData = fileData[3:]
	}
	group.LuaStr = string(fileData)
	luaState, err := newLuaState(group.LuaStr)
	if err != nil {
		logger.Error("lua parse error: %v, groupId: %v", err, groupId)
	}
	// init_config
	group.GroupInitConfig = new(GroupInitConfig)
	ok := getSceneLuaConfigTable[*GroupInitConfig](luaState, "init_config", group.GroupInitConfig)
	if !ok {
		logger.Error("get init_config object error, sceneId: %v, blockId: %v, groupId: %v", sceneId, blockId, groupId)
		luaState.Close()
		return
	}
	// monsters
	monsterList := make([]*Monster, 0)
	ok = getSceneLuaConfigTable[*[]*Monster](luaState, "monsters", &monsterList)
	if !ok {
		logger.Error("get monsters object error, sceneId: %v, blockId: %v, groupId: %v", sceneId, blockId, groupId)
		luaState.Close()
		return
	}
	group.MonsterMap = make(map[int32]*Monster)
	for _, monster := range monsterList {
		group.MonsterMap[monster.ConfigId] = monster
	}
	// npcs
	npcList := make([]*Npc, 0)
	ok = getSceneLuaConfigTable[*[]*Npc](luaState, "npcs", &npcList)
	if !ok {
		logger.Error("get npcs object error, sceneId: %v, blockId: %v, groupId: %v", sceneId, blockId, groupId)
		luaState.Close()
		return
	}
	group.NpcMap = make(map[int32]*Npc)
	for _, npc := range npcList {
		group.NpcMap[npc.ConfigId] = npc
	}
	// gadgets
	gadgetList := make([]*Gadget, 0)
	ok = getSceneLuaConfigTable[*[]*Gadget](luaState, "gadgets", &gadgetList)
	if !ok {
		logger.Error("get gadgets object error, sceneId: %v, blockId: %v, groupId: %v", sceneId, blockId, groupId)
		luaState.Close()
		return
	}
	group.GadgetMap = make(map[int32]*Gadget)
	for _, gadget := range gadgetList {
		group.GadgetMap[gadget.ConfigId] = gadget
	}
	// regions
	regionList := make([]*Region, 0)
	ok = getSceneLuaConfigTable[*[]*Region](luaState, "regions", &regionList)
	if !ok {
		logger.Error("get regions object error, sceneId: %v, blockId: %v, groupId: %v", sceneId, blockId, groupId)
		luaState.Close()
		return
	}
	group.RegionMap = make(map[int32]*Region)
	for _, region := range regionList {
		group.RegionMap[region.ConfigId] = region
	}
	// triggers
	triggerList := make([]*Trigger, 0)
	ok = getSceneLuaConfigTable[*[]*Trigger](luaState, "triggers", &triggerList)
	if !ok {
		logger.Error("get triggers object error, sceneId: %v, blockId: %v, groupId: %v", sceneId, blockId, groupId)
		luaState.Close()
		return
	}
	group.TriggerMap = make(map[string]*Trigger)
	for _, trigger := range triggerList {
		group.TriggerMap[trigger.Name] = trigger
	}
	// variables
	variableList := make([]*Variable, 0)
	ok = getSceneLuaConfigTable[*[]*Variable](luaState, "variables", &variableList)
	if !ok {
		logger.Error("get variables object error, sceneId: %v, blockId: %v, groupId: %v", sceneId, blockId, groupId)
		luaState.Close()
		return
	}
	group.VariableMap = make(map[string]*Variable)
	for _, variable := range variableList {
		group.VariableMap[variable.Name] = variable
	}
	// suites
	suiteLuaTableList := make([]*SuiteLuaTable, 0)
	ok = getSceneLuaConfigTable[*[]*SuiteLuaTable](luaState, "suites", &suiteLuaTableList)
	if !ok {
		logger.Error("get suites object error, sceneId: %v, blockId: %v, groupId: %v", sceneId, blockId, groupId)
		luaState.Close()
		return
	}
	if len(suiteLuaTableList) == 0 {
		// logger.Debug("get suites object is nil, sceneId: %v, blockId: %v, groupId: %v", sceneId, blockId, groupId)
	}
	group.SuiteMap = make(map[int32]*Suite)
	for index, suiteLuaTable := range suiteLuaTableList {
		suite := &Suite{
			MonsterConfigIdList: make([]int32, 0),
			GadgetConfigIdList:  make([]int32, 0),
			RegionConfigIdList:  make([]int32, 0),
			TriggerNameList:     make([]string, 0),
			RandWeight:          suiteLuaTable.RandWeight,
		}
		idAnyList, ok := suiteLuaTable.MonsterConfigIdList.([]any)
		if ok {
			for _, idAny := range idAnyList {
				suite.MonsterConfigIdList = append(suite.MonsterConfigIdList, int32(idAny.(float64)))
			}
		}
		idAnyList, ok = suiteLuaTable.GadgetConfigIdList.([]any)
		if ok {
			for _, idAny := range idAnyList {
				suite.GadgetConfigIdList = append(suite.GadgetConfigIdList, int32(idAny.(float64)))
			}
		}
		idAnyList, ok = suiteLuaTable.RegionConfigIdList.([]any)
		if ok {
			for _, idAny := range idAnyList {
				suite.RegionConfigIdList = append(suite.RegionConfigIdList, int32(idAny.(float64)))
			}
		}
		nameAnyList, ok := suiteLuaTable.TriggerNameList.([]any)
		if ok {
			for _, nameAny := range nameAnyList {
				suite.TriggerNameList = append(suite.TriggerNameList, nameAny.(string))
			}
		}
		group.SuiteMap[int32(len(suiteLuaTableList)-index)] = suite
	}
	luaState.Close()
	group.BlockId = blockId
	block.groupMapLoadLock.Lock()
	block.GroupMap[group.Id] = group
	block.groupMapLoadLock.Unlock()
}

func (g *GameDataConfig) loadSceneLuaConfig(loadSceneLua bool) {
	g.SceneLuaConfigMap = make(map[int32]*SceneLuaConfig)
	g.SceneLuaGroupMap = make(map[int32]*Group)
	g.SceneLuaStateLruMap = make(map[int32]*LuaStateLru)
	if !loadSceneLua {
		return
	}
	sceneLuaPrefix := g.luaPrefix + "scene/"
	for _, sceneData := range g.SceneDataMap {
		sceneId := sceneData.SceneId
		sceneIdStr := strconv.Itoa(int(sceneId))
		filePath := sceneLuaPrefix + sceneIdStr + "/scene" + sceneIdStr + ".lua"
		mainLuaData, err := os.ReadFile(filePath)
		if err != nil {
			continue
		}
		luaState, err := newLuaState(string(mainLuaData))
		if err != nil {
			logger.Error("lua parse error: %v, path: %v", err, filePath)
		}
		sceneLuaConfig := new(SceneLuaConfig)
		sceneLuaConfig.Id = sceneId
		// scene_config
		sceneLuaConfig.SceneConfig = new(SceneConfig)
		ok := getSceneLuaConfigTable[*SceneConfig](luaState, "scene_config", sceneLuaConfig.SceneConfig)
		if !ok {
			logger.Error("get scene_config object error, sceneId: %v", sceneId)
			luaState.Close()
			continue
		}
		sceneLuaConfig.BlockMap = make(map[int32]*Block)
		// blocks
		blockIdList := make([]int32, 0)
		ok = getSceneLuaConfigTable[*[]int32](luaState, "blocks", &blockIdList)
		if !ok {
			logger.Error("get blocks object error, sceneId: %v", sceneId)
			luaState.Close()
			continue
		}
		// block_rects
		blockRectList := make([]*BlockRange, 0)
		ok = getSceneLuaConfigTable[*[]*BlockRange](luaState, "block_rects", &blockRectList)
		luaState.Close()
		if !ok {
			logger.Error("get block_rects object error, sceneId: %v", sceneId)
			continue
		}
		for index, blockId := range blockIdList {
			block := new(Block)
			block.Id = blockId
			if index >= len(blockRectList) {
				continue
			}
			block.BlockRange = blockRectList[index]
			blockIdStr := strconv.Itoa(int(block.Id))
			filePath := sceneLuaPrefix + sceneIdStr + "/scene" + sceneIdStr + "_block" + blockIdStr + ".lua"
			blockLuaData, err := os.ReadFile(filePath)
			if err != nil {
				logger.Error("open file error: %v", err)
				continue
			}
			luaState, err := newLuaState(string(blockLuaData))
			if err != nil {
				logger.Error("lua parse error: %v, path: %v", err, filePath)
			}
			// groups
			block.GroupMap = make(map[int32]*Group)
			groupList := make([]*Group, 0)
			ok = getSceneLuaConfigTable[*[]*Group](luaState, "groups", &groupList)
			luaState.Close()
			if !ok {
				logger.Error("get groups object error, sceneId: %v, blockId: %v", sceneId, blockId)
				continue
			}
			// 因为group文件实在是太多了 有好几万个 所以这里并发同时加载
			wc := make(chan bool, SceneGroupLoaderLimit)
			wg := sync.WaitGroup{}
			for i := 0; i < len(groupList); i++ {
				group := groupList[i]
				wc <- true
				wg.Add(1)
				go func() {
					g.loadGroup(group, block, sceneId, blockId)
					<-wc
					wg.Done()
				}()
				g.SceneLuaGroupMap[group.Id] = group
			}
			wg.Wait()
			sceneLuaConfig.BlockMap[block.Id] = block
		}
		g.SceneLuaConfigMap[sceneId] = sceneLuaConfig
	}
	sceneCount := 0
	blockCount := 0
	groupCount := 0
	monsterCount := 0
	npcCount := 0
	gadgetCount := 0
	for _, scene := range g.SceneLuaConfigMap {
		for _, block := range scene.BlockMap {
			for _, group := range block.GroupMap {
				monsterCount += len(group.MonsterMap)
				npcCount += len(group.NpcMap)
				gadgetCount += len(group.GadgetMap)
				groupCount++
			}
			blockCount++
		}
		sceneCount++
	}
	logger.Info("Scene Count: %v, Block Count: %v, Group Count: %v, Monster Count: %v, Npc Count: %v, Gadget Count: %v",
		sceneCount, blockCount, groupCount, monsterCount, npcCount, gadgetCount)
}

func GetSceneLuaConfigById(sceneId int32) *SceneLuaConfig {
	return CONF.SceneLuaConfigMap[sceneId]
}

func GetSceneLuaConfigMap() map[int32]*SceneLuaConfig {
	return CONF.SceneLuaConfigMap
}

func GetSceneGroup(groupId int32) *Group {
	groupConfig, exist := CONF.SceneLuaGroupMap[groupId]
	if !exist {
		return nil
	}
	return groupConfig
}

// LuaStateLru LRU 表条目 按 AccessTime 排序决定淘汰顺序
// GroupId 关联到具体 group AccessTime 每次 GetLuaState 时更新
type LuaStateLru struct {
	GroupId    int32
	AccessTime int64
}

type LuaStateLruList []*LuaStateLru

func (l LuaStateLruList) Len() int {
	return len(l)
}

func (l LuaStateLruList) Less(i, j int) bool {
	return l[i].AccessTime < l[j].AccessTime
}

func (l LuaStateLruList) Swap(i, j int) {
	l[i], l[j] = l[j], l[i]
}

// LuaStateLruRemove 淘汰最旧的 Lua VM 释放内存（每分钟调用一次 见 game_tick_manager.go:onTickMinute）
//
// 淘汰流程：
//  1. 当前 LRU 表条目数超过 LuaStateLruKeepNum=1000 才触发（否则直接 return）
//  2. 按 AccessTime 升序排序 取最旧的 N 个
//  3. 把每个 group.LuaState 置 nil（Lua VM 失去引用 GC 自动回收）
//  4. 从 LRU 表里删掉对应条目
//
// **淘汰后下次访问该 group 时 GetLuaState 会重建 VM**（newLuaState + 重新解析 g.LuaStr）
// VM 内部状态会重置 但**group 存档变量在 model.SceneGroup.VariableMap 持久化**所以不丢
func LuaStateLruRemove() {
	removeNum := len(CONF.SceneLuaStateLruMap) - LuaStateLruKeepNum
	if removeNum <= 0 {
		return
	}
	luaStateLruList := make(LuaStateLruList, 0)
	for _, luaStateLru := range CONF.SceneLuaStateLruMap {
		luaStateLruList = append(luaStateLruList, luaStateLru)
	}
	sort.Stable(luaStateLruList)
	for i := 0; i < removeNum; i++ {
		luaStateLru := luaStateLruList[i]
		group := GetSceneGroup(luaStateLru.GroupId)
		group.LuaState = nil
		delete(CONF.SceneLuaStateLruMap, luaStateLru.GroupId)
	}
	logger.Info("lua state lru remove finish, remove num: %v", removeNum)
}

// GetLuaState 获取 group 的 Lua VM（懒加载 + LRU 访问时间更新）
//
// 每次调用都更新 LRU 表的 AccessTime（让活跃 group 不被淘汰）
// 首次访问或 VM 被淘汰后：
//  1. newLuaState 重新解析 g.LuaStr 创建新 VM
//  2. 注册 ScriptLib 全局表（暴露给 Lua 调用的 Go 函数集合）
//  3. SCRIPT_LIB_FUNC_LIST 在 gs/game/lua_func.go RegLuaScriptLibFunc 中填充
//     注意：本文件不知道具体注册了什么函数 业务侧通过 RegScriptLibFunc 动态追加
func (g *Group) GetLuaState() *lua.LState {
	CONF.SceneLuaStateLruMap[g.Id] = &LuaStateLru{
		GroupId:    g.Id,
		AccessTime: time.Now().UnixMilli(),
	}
	if g.LuaState == nil {
		luaState, err := newLuaState(g.LuaStr)
		if err != nil {
			logger.Error("lua parse error: %v, groupId: %v", err, g.Id)
		}
		scriptLib := luaState.NewTable()
		luaState.SetGlobal("ScriptLib", scriptLib)
		for _, scriptLibFunc := range SCRIPT_LIB_FUNC_LIST {
			luaState.SetField(scriptLib, scriptLibFunc.fnName, luaState.NewFunction(scriptLibFunc.fn))
		}
		g.LuaState = luaState
	}
	return g.LuaState
}
