package game

import (
	"hk4e/common/constant"
	"hk4e/gdconf"
	"hk4e/gs/model"
	"hk4e/pkg/alg"
	"hk4e/pkg/object"
	"hk4e/protocol/cmd"
	"hk4e/protocol/proto"

	"github.com/flswld/halo/logger"
	lua "github.com/yuin/gopher-lua"
)

// Lua 桥接 模块
//
// 项目场景 Lua 是从原神官服 dump 出来的原版配置（不是项目作者写的）
// 描述大世界场景里有什么 + 局部交互逻辑（如"打死守卫怪→解锁宝箱"）
//
// Go ↔ Lua 双向调用：
//   - Go → Lua: CallSceneLuaFunc / CallGadgetLuaFunc 调用 Lua 函数（trigger condition/action 等）
//   - Lua → Go: ScriptLib 暴露给 Lua 的几十个 API（SetGadgetState/CreateMonster/AddQuestProgress 等）
//
// LuaCtx + LuaEvt 是两个 Lua table 包装结构 通过 SetField 转换为 Lua 表传给 Lua 函数
// Lua 函数通过 ScriptLib 的 GetContextPlayer/GetContextGroup 反向取出这些上下文信息
//
// EndlessLoopCheck 防止 Lua 触发器递归（如 Lua A 调 Go → Go 触发 Lua B → Lua B 又调 Go → ...）
// 死循环保护阈值是 EndlessLoopCheckTypeCallLuaFunc 在 game.go 中定义

// LuaCtx Lua 调用上下文 表示"谁在操作什么实体"（玩家uid/世界主uid/源/目标实体/group）
type LuaCtx struct {
	uid            uint32 // 触发玩家 uid
	ownerUid       uint32 // 世界主 uid（多人世界用）
	sourceEntityId uint32 // 源实体（如发动技能的实体）
	targetEntityId uint32 // 目标实体（如被击中的实体）
	groupId        uint32 // 当前操作所在的 group
}

type LuaEvt struct {
	param1         int32
	param2         int32
	param3         int32
	param4         int32
	paramStr1      string
	evtType        int32
	uid            uint32
	sourceName     string
	sourceEntityId uint32
	targetEntityId uint32
}

// CallSceneLuaFunc Go 调用场景 Lua 函数（trigger condition/action 入口）
//
// 把 luaCtx + luaEvt 包装成两个 Lua table 调用 Lua 全局函数 luaFuncName
// 函数返回值约定：bool（true=匹配/执行）或 number（0/正数/-1 等→约定的状态码）
// 异常：捕获后只记录日志返回 false 不会让 Lua 错误冒到 Go 层
//
// 调用方：lua_trigger.go 的 Trigger 检测函数（如 SceneRegionTriggerCheck/MonsterDieTriggerCheck）
func CallSceneLuaFunc(luaState *lua.LState, luaFuncName string, luaCtx *LuaCtx, luaEvt *LuaEvt) bool {
	GAME.EndlessLoopCheck(EndlessLoopCheckTypeCallLuaFunc)
	ctx := luaState.NewTable()
	luaState.SetField(ctx, "uid", lua.LNumber(luaCtx.uid))
	luaState.SetField(ctx, "owner_uid", lua.LNumber(luaCtx.ownerUid))
	luaState.SetField(ctx, "source_entity_id", lua.LNumber(luaCtx.sourceEntityId))
	luaState.SetField(ctx, "target_entity_id", lua.LNumber(luaCtx.targetEntityId))
	luaState.SetField(ctx, "groupId", lua.LNumber(luaCtx.groupId))
	evt := luaState.NewTable()
	luaState.SetField(evt, "param1", lua.LNumber(luaEvt.param1))
	luaState.SetField(evt, "param2", lua.LNumber(luaEvt.param2))
	luaState.SetField(evt, "param3", lua.LNumber(luaEvt.param3))
	luaState.SetField(evt, "param4", lua.LNumber(luaEvt.param4))
	luaState.SetField(evt, "param_str1", lua.LString(luaEvt.paramStr1))
	luaState.SetField(evt, "type", lua.LNumber(luaEvt.evtType))
	luaState.SetField(evt, "uid", lua.LNumber(luaEvt.uid))
	luaState.SetField(evt, "source_name", lua.LString(luaEvt.sourceName))
	luaState.SetField(evt, "source_eid", lua.LNumber(luaEvt.sourceEntityId))
	luaState.SetField(evt, "target_eid", lua.LNumber(luaEvt.targetEntityId))
	err := luaState.CallByParam(lua.P{
		Fn:      luaState.GetGlobal(luaFuncName),
		NRet:    1,
		Protect: true,
	}, ctx, evt)
	if err != nil {
		logger.Error("call scene lua error, groupId: %v, func: %v, error: %v", luaCtx.groupId, luaFuncName, err)
		return false
	}
	luaRet := luaState.Get(-1)
	luaState.Pop(1)
	switch luaRet.(type) {
	case lua.LBool:
		return bool(luaRet.(lua.LBool))
	case lua.LNumber:
		return object.ConvRetCodeToBool(int64(luaRet.(lua.LNumber)))
	default:
		return false
	}
}

// CallGadgetLuaFunc Go 调用物件 Lua 脚本函数
//
// 物件 Lua 脚本是物件级的本地脚本（每个 Gadget 类型有自己的脚本）
// 与场景 Lua 不同：物件 Lua 用专用入口名 + 专用参数列表（不走 LuaEvt 通用包装）
//
// 已知钩子：
//   - OnClientExecuteReq: 客户端发起物件请求（如机关交互）
//   - OnBeHurt: 物件被攻击（用于响应元素反应/状态机切换）
//   - OnDie: 物件死亡（如打破破坏物 触发奖励）
func CallGadgetLuaFunc(luaState *lua.LState, luaFuncName string, luaCtx *LuaCtx, param ...any) bool {
	GAME.EndlessLoopCheck(EndlessLoopCheckTypeCallLuaFunc)
	ctx := luaState.NewTable()
	luaState.SetField(ctx, "uid", lua.LNumber(luaCtx.uid))
	luaState.SetField(ctx, "owner_uid", lua.LNumber(luaCtx.ownerUid))
	luaState.SetField(ctx, "source_entity_id", lua.LNumber(luaCtx.sourceEntityId))
	luaState.SetField(ctx, "target_entity_id", lua.LNumber(luaCtx.targetEntityId))
	luaState.SetField(ctx, "groupId", lua.LNumber(luaCtx.groupId))
	luaParamList := make([]lua.LValue, 0)
	luaParamList = append(luaParamList, ctx)
	switch luaFuncName {
	case "OnClientExecuteReq":
		luaParamList = append(luaParamList, lua.LNumber(param[0].(int32)))
		luaParamList = append(luaParamList, lua.LNumber(param[1].(int32)))
		luaParamList = append(luaParamList, lua.LNumber(param[2].(int32)))
	case "OnBeHurt":
		luaParamList = append(luaParamList, lua.LNumber(param[0].(uint32)))
		luaParamList = append(luaParamList, lua.LNumber(param[1].(int)))
		luaParamList = append(luaParamList, lua.LBool(param[2].(bool)))
	case "OnDie":
		luaParamList = append(luaParamList, lua.LNumber(param[0].(int)))
		luaParamList = append(luaParamList, lua.LNumber(param[1].(int)))
	}
	err := luaState.CallByParam(lua.P{
		Fn:      luaState.GetGlobal(luaFuncName),
		NRet:    1,
		Protect: true,
	}, luaParamList...)
	if err != nil {
		logger.Error("call gadget lua error, func: %v, error: %v", luaFuncName, err)
		return false
	}
	luaRet := luaState.Get(-1)
	luaState.Pop(1)
	switch luaRet.(type) {
	case lua.LBool:
		return bool(luaRet.(lua.LBool))
	case lua.LNumber:
		return object.ConvRetCodeToBool(int64(luaRet.(lua.LNumber)))
	default:
		return false
	}
}

// GetContextPlayer 获取上下文中的玩家对象
func GetContextPlayer(ctx *lua.LTable, luaState *lua.LState) *model.Player {
	uid, ok := luaState.GetField(ctx, "uid").(lua.LNumber)
	if !ok {
		return nil
	}
	player := USER_MANAGER.GetOnlineUser(uint32(uid))
	return player
}

// GetContextGroup 获取上下文中的场景组对象
func GetContextGroup(player *model.Player, ctx *lua.LTable, luaState *lua.LState) *Group {
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		return nil
	}
	groupId, ok := luaState.GetField(ctx, "groupId").(lua.LNumber)
	if !ok {
		return nil
	}
	scene := world.GetSceneById(player.GetSceneId())
	group := scene.GetGroupById(uint32(groupId))
	if group == nil {
		return nil
	}
	return group
}

// GetContextSceneGroup 获取上下文中的场景组存档对象
func GetContextSceneGroup(player *model.Player, groupId uint32) *model.SceneGroup {
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		return nil
	}
	owner := world.GetOwner()
	sceneGroup := owner.GetSceneGroupById(groupId)
	return sceneGroup
}

// RegLuaScriptLibFunc 注册 ScriptLib（Lua 侧通过 ScriptLib.XXX 调用的 Go 方法）
//
// 在 gdconf 加载完场景 Lua 后被调用一次 把 Go 函数挂到所有 Lua VM 的 ScriptLib 表里
// 注册的 API 分两类：
//   - 场景 LUA API: 30+ 个（实体查询/创建/group变量/区域/天气/操作台等）
//   - 物件 LUA API: 5 个（SetGadgetState/GetGadgetState/GetContextGadgetConfigId/GetContextGroupId/DropSubfield）
//
// Lua 调用示例：ScriptLib.SetGroupVariableValue(context, "var_name", value)
// 客户端见到的"宝箱开启"的服务端逻辑就是 Lua trigger 调 ScriptLib.SetGadgetState 实现
// RegLuaScriptLibFunc 启动时注册 ScriptLib 全套 API（共 38 个，gdconf 加载完场景 Lua 后调一次）
//
// 注册的 Go 函数全部挂到 Lua VM 的 `ScriptLib` 全局表中
// Lua 侧调用方式：`ScriptLib.SetGadgetState(context, configId, state)` 等
//
// **38 个 API 分类**（按业务域分组 按行号顺序）：
//
//	【上下文 / 调试】
//	  GetEntityType                          解析 entityId 取实体类型（按客户端版本位移量不同）
//	  GetQuestState                          查询玩家任务状态
//	  PrintLog / PrintContextLog             Lua 调试日志（带玩家 uid）
//
//	【相机】
//	  BeginCameraSceneLook                   CG 镜头锁定（**临时屏蔽** 触发客户端 bug）
//
//	【实体计数】
//	  GetGroupMonsterCount                   当前 group 怪物数（trigger 用：全打完才出宝箱）
//	  GetGroupMonsterCountByGroupId          按 groupId 查（跨 group 联动）
//	  CheckRemainGadgetCountByGroupId        按 groupId 查物件剩余数
//	  GetRegionEntityCount                   region 内实体数
//
//	【实体创建/销毁】
//	  CreateMonster                          创建怪物（支持 delay 延迟创建）
//	  CreateGadget                           创建物件
//	  KillEntityByConfigId / KillGroupEntity 按 configId / group 杀实体
//
//	【物件状态】
//	  GetGadgetStateByConfigId / SetGadgetStateByConfigId   按 configId 操作（场景 LUA 用）
//	  ChangeGroupGadget                                     批量改 group 内物件
//	  GetGadgetState / SetGadgetState                       按 ctx 操作（物件 LUA 用）
//	  GetContextGadgetConfigId / GetContextGroupId          取上下文物件信息
//
//	【group 变量】（持久化在 model.SceneGroup.VariableMap 通常是任务进度/计数器）
//	  GetGroupVariableValue / GetGroupVariableValueByGroup
//	  SetGroupVariableValue / SetGroupVariableValueByGroup
//	  ChangeGroupVariableValue / ChangeGroupVariableValueByGroup    （Set 是赋值 Change 是 +=）
//
//	【group suite 切换】（一个 group 不同状态：怪在/怪死/宝箱拿走）
//	  RefreshGroup                          重新加载 group（按当前 variable 决定 suite）
//	  AddExtraGroupSuite / RemoveExtraGroupSuite   叠加/移除 suite
//
//	【任务】
//	  MarkPlayerAction                      标记玩家行为（用于任务条件）
//	  AddQuestProgress                      推进任务进度（关键 API 走 TriggerQuest(LUA_NOTIFY)）
//
//	【其他】
//	  ShowReminder                          客户端右上角提示
//	  CreateGroupTimerEvent                 延迟 N 秒触发 TIMER_EVENT trigger
//	  EnterWeatherArea / SetWeatherAreaState  天气区域控制
//	  SetWorktopOptions / SetWorktopOptionsByGroupId        操作台选项设置
//	  DelWorktopOption / DelWorktopOptionByGroupId          操作台选项移除
//
//	【物件 LUA 专用】（在物件本地 Lua 脚本里调用 GadgetLuaConfig.LuaState）
//	  SetGadgetState / GetGadgetState
//	  GetContextGadgetConfigId / GetContextGroupId
//	  DropSubfield                          掉落子物品（如打破破坏物的掉落）
//
// **调用约定**：所有 ScriptLib 函数都是单参数 `*lua.LState` 返回 `int`（压栈数量）
//
//	入参第 1 个通常是 `context table`（含 uid/groupId 等）
//	出参用 luaState.Push(LValue) 失败约定返回 -1
func RegLuaScriptLibFunc() {
	// 调用场景LUA方法
	gdconf.RegScriptLibFunc("GetEntityType", GetEntityType)
	gdconf.RegScriptLibFunc("GetQuestState", GetQuestState)
	gdconf.RegScriptLibFunc("PrintLog", PrintLog)
	gdconf.RegScriptLibFunc("PrintContextLog", PrintContextLog)
	gdconf.RegScriptLibFunc("BeginCameraSceneLook", BeginCameraSceneLook)
	gdconf.RegScriptLibFunc("GetGroupMonsterCount", GetGroupMonsterCount)
	gdconf.RegScriptLibFunc("GetGroupMonsterCountByGroupId", GetGroupMonsterCountByGroupId)
	gdconf.RegScriptLibFunc("CheckRemainGadgetCountByGroupId", CheckRemainGadgetCountByGroupId)
	gdconf.RegScriptLibFunc("ChangeGroupGadget", ChangeGroupGadget)
	gdconf.RegScriptLibFunc("GetGadgetStateByConfigId", GetGadgetStateByConfigId)
	gdconf.RegScriptLibFunc("SetGadgetStateByConfigId", SetGadgetStateByConfigId)
	gdconf.RegScriptLibFunc("MarkPlayerAction", MarkPlayerAction)
	gdconf.RegScriptLibFunc("AddQuestProgress", AddQuestProgress)
	gdconf.RegScriptLibFunc("CreateMonster", CreateMonster)
	gdconf.RegScriptLibFunc("CreateGadget", CreateGadget)
	gdconf.RegScriptLibFunc("KillEntityByConfigId", KillEntityByConfigId)
	gdconf.RegScriptLibFunc("AddExtraGroupSuite", AddExtraGroupSuite)
	gdconf.RegScriptLibFunc("GetGroupVariableValue", GetGroupVariableValue)
	gdconf.RegScriptLibFunc("GetGroupVariableValueByGroup", GetGroupVariableValueByGroup)
	gdconf.RegScriptLibFunc("SetGroupVariableValue", SetGroupVariableValue)
	gdconf.RegScriptLibFunc("SetGroupVariableValueByGroup", SetGroupVariableValueByGroup)
	gdconf.RegScriptLibFunc("ChangeGroupVariableValue", ChangeGroupVariableValue)
	gdconf.RegScriptLibFunc("ChangeGroupVariableValueByGroup", ChangeGroupVariableValueByGroup)
	gdconf.RegScriptLibFunc("GetRegionEntityCount", GetRegionEntityCount)
	gdconf.RegScriptLibFunc("CreateGroupTimerEvent", CreateGroupTimerEvent)
	gdconf.RegScriptLibFunc("EnterWeatherArea", EnterWeatherArea)
	gdconf.RegScriptLibFunc("SetWeatherAreaState", SetWeatherAreaState)
	gdconf.RegScriptLibFunc("RefreshGroup", RefreshGroup)
	gdconf.RegScriptLibFunc("RemoveExtraGroupSuite", RemoveExtraGroupSuite)
	gdconf.RegScriptLibFunc("ShowReminder", ShowReminder)
	gdconf.RegScriptLibFunc("KillGroupEntity", KillGroupEntity)
	gdconf.RegScriptLibFunc("SetWorktopOptions", SetWorktopOptions)
	gdconf.RegScriptLibFunc("SetWorktopOptionsByGroupId", SetWorktopOptionsByGroupId)
	gdconf.RegScriptLibFunc("DelWorktopOption", DelWorktopOption)
	gdconf.RegScriptLibFunc("DelWorktopOptionByGroupId", DelWorktopOptionByGroupId)
	// 调用物件LUA方法
	gdconf.RegScriptLibFunc("SetGadgetState", SetGadgetState)
	gdconf.RegScriptLibFunc("GetGadgetState", GetGadgetState)
	gdconf.RegScriptLibFunc("GetContextGadgetConfigId", GetContextGadgetConfigId)
	gdconf.RegScriptLibFunc("GetContextGroupId", GetContextGroupId)
	gdconf.RegScriptLibFunc("DropSubfield", DropSubfield)
}

type CommonLuaTableParam struct {
	ConfigId     int32  `json:"config_id"`
	DelayTime    int32  `json:"delay_time"`
	RegionEid    int32  `json:"region_eid"`
	EntityType   int32  `json:"entity_type"`
	GroupId      int32  `json:"group_id"`
	Suite        int32  `json:"suite"`
	KillPolicy   int32  `json:"kill_policy"`
	SubfieldName string `json:"subfield_name"`
}

// GetEntityType ScriptLib API: 解析 entityId 取实体类型（按客户端版本不同位移量不同）
//
// EntityId 编码：高位是 entityType 低位是计数器（详见 game_world_manager.go:GetNextWorldEntityId）
//   - v6.5+: entityType 占 11 bit (>>21)
//   - v6.0+: entityType 占 10 bit (>>22)
//   - <6.0:  entityType 占 8 bit  (>>24)
//
// 通过 SELF.ClientVersion 决定位移量 这就是为什么 Lua 必须在主循环单线程内调用（依赖全局 SELF）
func GetEntityType(luaState *lua.LState) int {
	entityId := luaState.ToInt(1)
	if SELF.ClientVersion >= 650 {
		luaState.Push(lua.LNumber(entityId >> 21))
	} else if SELF.ClientVersion >= 600 {
		luaState.Push(lua.LNumber(entityId >> 22))
	} else {
		luaState.Push(lua.LNumber(entityId >> 24))
	}
	return 1
}

func GetQuestState(luaState *lua.LState) int {
	ctx, ok := luaState.Get(1).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(constant.QUEST_STATE_NONE))
		return 1
	}
	player := GetContextPlayer(ctx, luaState)
	if player == nil {
		luaState.Push(lua.LNumber(constant.QUEST_STATE_NONE))
		return 1
	}
	entityId := luaState.ToInt(2)
	_ = entityId
	questId := luaState.ToInt(3)
	dbQuest := player.GetDbQuest()
	quest := dbQuest.GetQuestById(uint32(questId))
	if quest == nil {
		luaState.Push(lua.LNumber(constant.QUEST_STATE_NONE))
		return 1
	}
	luaState.Push(lua.LNumber(quest.State))
	return 1
}

func PrintLog(luaState *lua.LState) int {
	logInfo := luaState.ToString(1)
	logger.Info("[LUA LOG] %v", logInfo)
	return 0
}

func PrintContextLog(luaState *lua.LState) int {
	ctx, ok := luaState.Get(1).(*lua.LTable)
	if !ok {
		return 0
	}
	uid, ok := luaState.GetField(ctx, "uid").(lua.LNumber)
	if !ok {
		return 0
	}
	logInfo := luaState.ToString(2)
	logger.Info("[LUA CTX LOG] %v [UID: %v]", logInfo, uid)
	return 0
}

// BeginCameraSceneLook ScriptLib API: 开始相机过场（CG 镜头锁定）
// **临时屏蔽**：解锁风之翼任务调这个会触发未知客户端 bug 作者用 luaState.Push(0) 提前返回绕过
// 后续代码不可达 但保留备用（可能未来修复后启用）
func BeginCameraSceneLook(luaState *lua.LState) int {
	// TODO 由于解锁风之翼任务相关原因暂时屏蔽
	luaState.Push(lua.LNumber(0))
	return 1
	ctx, ok := luaState.Get(1).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	player := GetContextPlayer(ctx, luaState)
	if player == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	cameraLockInfo, ok := luaState.Get(2).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	ntf := new(proto.BeginCameraSceneLookNotify)
	gdconf.ParseLuaTableToObject(cameraLockInfo, ntf)
	GAME.SendMsg(cmd.BeginCameraSceneLookNotify, player.PlayerId, player.ClientSeq, ntf)
	logger.Debug("BeginCameraSceneLook, ntf: %v, uid: %v", ntf, player.PlayerId)
	luaState.Push(lua.LNumber(0))
	return 1
}

func GetGroupMonsterCount(luaState *lua.LState) int {
	ctx, ok := luaState.Get(1).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	player := GetContextPlayer(ctx, luaState)
	if player == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	group := GetContextGroup(player, ctx, luaState)
	if group == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	monsterCount := 0
	for _, entity := range group.GetAllEntity() {
		_, ok := entity.(*MonsterEntity)
		if ok {
			monsterCount++
		}
	}
	luaState.Push(lua.LNumber(monsterCount))
	return 1
}

func GetGroupMonsterCountByGroupId(luaState *lua.LState) int {
	ctx, ok := luaState.Get(1).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	player := GetContextPlayer(ctx, luaState)
	if player == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	scene := world.GetSceneById(player.GetSceneId())
	groupId := luaState.ToInt(2)
	group := scene.GetGroupById(uint32(groupId))
	if group == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	monsterCount := 0
	for _, entity := range group.GetAllEntity() {
		_, ok := entity.(*MonsterEntity)
		if ok {
			monsterCount++
		}
	}
	luaState.Push(lua.LNumber(monsterCount))
	return 1
}

func CheckRemainGadgetCountByGroupId(luaState *lua.LState) int {
	ctx, ok := luaState.Get(1).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	player := GetContextPlayer(ctx, luaState)
	if player == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	scene := world.GetSceneById(player.GetSceneId())
	luaTable, ok := luaState.Get(2).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	luaTableParam := new(CommonLuaTableParam)
	gdconf.ParseLuaTableToObject[*CommonLuaTableParam](luaTable, luaTableParam)
	group := scene.GetGroupById(uint32(luaTableParam.GroupId))
	if group == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	gadgetCount := 0
	for _, entity := range group.GetAllEntity() {
		_, ok := entity.(IGadgetEntity)
		if ok {
			gadgetCount++
		}
	}
	luaState.Push(lua.LNumber(gadgetCount))
	return 1
}

func ChangeGroupGadget(luaState *lua.LState) int {
	ctx, ok := luaState.Get(1).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	player := GetContextPlayer(ctx, luaState)
	if player == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	group := GetContextGroup(player, ctx, luaState)
	if group == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	gadgetInfo, ok := luaState.Get(2).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	gadgetStateInfo := new(gdconf.Gadget)
	gdconf.ParseLuaTableToObject(gadgetInfo, gadgetStateInfo)
	entity := group.GetEntityByConfigId(uint32(gadgetStateInfo.ConfigId))
	if entity == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	GAME.ChangeGadgetState(player, entity.GetId(), uint32(gadgetStateInfo.State))
	luaState.Push(lua.LNumber(0))
	return 1
}

func GetGadgetStateByConfigId(luaState *lua.LState) int {
	ctx, ok := luaState.Get(1).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	player := GetContextPlayer(ctx, luaState)
	if player == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	groupId := luaState.ToInt(2)
	configId := luaState.ToInt(3)
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	scene := world.GetSceneById(player.GetSceneId())
	group := scene.GetGroupById(uint32(groupId))
	if group == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	entity := group.GetEntityByConfigId(uint32(configId))
	if entity == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	iGadgetEntity, ok := entity.(IGadgetEntity)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	luaState.Push(lua.LNumber(iGadgetEntity.GetGadgetState()))
	return 1
}

func SetGadgetStateByConfigId(luaState *lua.LState) int {
	ctx, ok := luaState.Get(1).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	player := GetContextPlayer(ctx, luaState)
	if player == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	group := GetContextGroup(player, ctx, luaState)
	if group == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	configId := luaState.ToInt(2)
	state := luaState.ToInt(3)
	entity := group.GetEntityByConfigId(uint32(configId))
	if entity == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	GAME.ChangeGadgetState(player, entity.GetId(), uint32(state))
	luaState.Push(lua.LNumber(0))
	return 1
}

func MarkPlayerAction(luaState *lua.LState) int {
	ctx, ok := luaState.Get(1).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	player := GetContextPlayer(ctx, luaState)
	if player == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	param1 := luaState.ToInt(2)
	param2 := luaState.ToInt(3)
	param3 := luaState.ToInt(4)
	logger.Debug("[MarkPlayerAction] [%v %v %v] [UID: %v]", param1, param2, param3, player.PlayerId)
	luaState.Push(lua.LNumber(0))
	return 1
}

// AddQuestProgress ScriptLib API: Lua 侧推进任务进度
// Lua 通过 ScriptLib.AddQuestProgress(context, "complexParam") 触发 LUA_NOTIFY 任务条件
// 任务配置 finishCond.ComplexParam 与 Lua 传的字符串相等 → 推进任务进度
// 这是 Lua trigger 与 quest 系统的桥梁（如打死任务怪 → Lua trigger 调此函数 → quest 进度推进）
func AddQuestProgress(luaState *lua.LState) int {
	ctx, ok := luaState.Get(1).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	player := GetContextPlayer(ctx, luaState)
	if player == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	complexParam := luaState.ToString(2)
	GAME.TriggerQuest(player, constant.QUEST_FINISH_COND_TYPE_LUA_NOTIFY, complexParam)
	luaState.Push(lua.LNumber(0))
	return 1
}

func CreateMonster(luaState *lua.LState) int {
	ctx, ok := luaState.Get(1).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	player := GetContextPlayer(ctx, luaState)
	if player == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	groupId, ok := luaState.GetField(ctx, "groupId").(lua.LNumber)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	luaTable, ok := luaState.Get(2).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	luaTableParam := new(CommonLuaTableParam)
	gdconf.ParseLuaTableToObject[*CommonLuaTableParam](luaTable, luaTableParam)
	TICK_MANAGER.CreateUserTimer(player.PlayerId, UserTimerActionLuaCreateMonster, uint32(luaTableParam.DelayTime),
		uint32(groupId), uint32(luaTableParam.ConfigId))
	luaState.Push(lua.LNumber(0))
	return 1
}

func CreateGadget(luaState *lua.LState) int {
	ctx, ok := luaState.Get(1).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	player := GetContextPlayer(ctx, luaState)
	if player == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	groupId, ok := luaState.GetField(ctx, "groupId").(lua.LNumber)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	luaTable, ok := luaState.Get(2).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	luaTableParam := new(CommonLuaTableParam)
	gdconf.ParseLuaTableToObject[*CommonLuaTableParam](luaTable, luaTableParam)
	GAME.SceneGroupCreateEntity(player, uint32(groupId), uint32(luaTableParam.ConfigId), constant.ENTITY_TYPE_GADGET)
	luaState.Push(lua.LNumber(0))
	return 1
}

func KillEntityByConfigId(luaState *lua.LState) int {
	ctx, ok := luaState.Get(1).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	player := GetContextPlayer(ctx, luaState)
	if player == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	groupId, ok := luaState.GetField(ctx, "groupId").(lua.LNumber)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	luaTable, ok := luaState.Get(2).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	luaTableParam := new(CommonLuaTableParam)
	gdconf.ParseLuaTableToObject[*CommonLuaTableParam](luaTable, luaTableParam)
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	scene := world.GetSceneById(player.GetSceneId())
	group := scene.GetGroupById(uint32(groupId))
	if group == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	entity := group.GetEntityByConfigId(uint32(luaTableParam.ConfigId))
	if entity == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	GAME.KillEntity(player, scene, entity.GetId(), proto.PlayerDieType_PLAYER_DIE_NONE)
	luaState.Push(lua.LNumber(0))
	return 1
}

func AddExtraGroupSuite(luaState *lua.LState) int {
	ctx, ok := luaState.Get(1).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	player := GetContextPlayer(ctx, luaState)
	if player == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	groupId := luaState.ToInt(2)
	suiteId := luaState.ToInt(3)
	GAME.AddSceneGroupSuite(player, uint32(groupId), uint8(suiteId))
	luaState.Push(lua.LNumber(0))
	return 1
}

func GetGroupVariableValue(luaState *lua.LState) int {
	ctx, ok := luaState.Get(1).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	player := GetContextPlayer(ctx, luaState)
	if player == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	groupId, ok := luaState.GetField(ctx, "groupId").(lua.LNumber)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	name := luaState.ToString(2)
	sceneGroup := GetContextSceneGroup(player, uint32(groupId))
	if sceneGroup == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	value := sceneGroup.GetVariableByName(name)
	luaState.Push(lua.LNumber(value))
	return 1
}

func GetGroupVariableValueByGroup(luaState *lua.LState) int {
	ctx, ok := luaState.Get(1).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	player := GetContextPlayer(ctx, luaState)
	if player == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	name := luaState.ToString(2)
	groupId := luaState.ToInt(3)
	sceneGroup := GetContextSceneGroup(player, uint32(groupId))
	if sceneGroup == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	value := sceneGroup.GetVariableByName(name)
	luaState.Push(lua.LNumber(value))
	return 1
}

func SetGroupVariableValue(luaState *lua.LState) int {
	ctx, ok := luaState.Get(1).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	player := GetContextPlayer(ctx, luaState)
	if player == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	groupId, ok := luaState.GetField(ctx, "groupId").(lua.LNumber)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	name := luaState.ToString(2)
	value := luaState.ToInt(3)
	sceneGroup := GetContextSceneGroup(player, uint32(groupId))
	if sceneGroup == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	sceneGroup.SetVariable(name, int32(value))
	luaState.Push(lua.LNumber(0))
	return 1
}

func SetGroupVariableValueByGroup(luaState *lua.LState) int {
	ctx, ok := luaState.Get(1).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	player := GetContextPlayer(ctx, luaState)
	if player == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	name := luaState.ToString(2)
	value := luaState.ToInt(3)
	groupId := luaState.ToInt(4)
	sceneGroup := GetContextSceneGroup(player, uint32(groupId))
	if sceneGroup == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	sceneGroup.SetVariable(name, int32(value))
	luaState.Push(lua.LNumber(0))
	return 1
}

func ChangeGroupVariableValue(luaState *lua.LState) int {
	ctx, ok := luaState.Get(1).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	player := GetContextPlayer(ctx, luaState)
	if player == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	groupId, ok := luaState.GetField(ctx, "groupId").(lua.LNumber)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	name := luaState.ToString(2)
	change := luaState.ToInt(3)
	sceneGroup := GetContextSceneGroup(player, uint32(groupId))
	if sceneGroup == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	value := sceneGroup.GetVariableByName(name)
	sceneGroup.SetVariable(name, value+int32(change))
	luaState.Push(lua.LNumber(0))
	return 1
}

func ChangeGroupVariableValueByGroup(luaState *lua.LState) int {
	ctx, ok := luaState.Get(1).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	player := GetContextPlayer(ctx, luaState)
	if player == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	name := luaState.ToString(2)
	change := luaState.ToInt(3)
	groupId := luaState.ToInt(4)
	sceneGroup := GetContextSceneGroup(player, uint32(groupId))
	if sceneGroup == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	value := sceneGroup.GetVariableByName(name)
	sceneGroup.SetVariable(name, value+int32(change))
	luaState.Push(lua.LNumber(0))
	return 1
}

func GetRegionEntityCount(luaState *lua.LState) int {
	ctx, ok := luaState.Get(1).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	player := GetContextPlayer(ctx, luaState)
	if player == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	groupId, ok := luaState.GetField(ctx, "groupId").(lua.LNumber)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	luaTable, ok := luaState.Get(2).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	luaTableParam := new(CommonLuaTableParam)
	gdconf.ParseLuaTableToObject[*CommonLuaTableParam](luaTable, luaTableParam)
	groupConfig := gdconf.GetSceneGroup(int32(groupId))
	if groupConfig == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	regionConfig := groupConfig.RegionMap[luaTableParam.RegionEid]
	if regionConfig == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	shape := alg.NewShape()
	switch uint8(regionConfig.Shape) {
	case constant.REGION_SHAPE_SPHERE:
		shape.NewSphere(&alg.Vector3{X: regionConfig.Pos.X, Y: regionConfig.Pos.Y, Z: regionConfig.Pos.Z}, regionConfig.Radius)
	case constant.REGION_SHAPE_CUBIC:
		shape.NewCubic(&alg.Vector3{X: regionConfig.Pos.X, Y: regionConfig.Pos.Y, Z: regionConfig.Pos.Z},
			&alg.Vector3{X: regionConfig.Size.X, Y: regionConfig.Size.Y, Z: regionConfig.Size.Z})
	case constant.REGION_SHAPE_CYLINDER:
		shape.NewCylinder(&alg.Vector3{X: regionConfig.Pos.X, Y: regionConfig.Pos.Y, Z: regionConfig.Pos.Z},
			regionConfig.Radius, regionConfig.Height)
	case constant.REGION_SHAPE_POLYGON:
		vector2PointArray := make([]*alg.Vector2, 0)
		for _, vector := range regionConfig.PointArray {
			// z就是y
			vector2PointArray = append(vector2PointArray, &alg.Vector2{X: vector.X, Z: vector.Y})
		}
		shape.NewPolygon(&alg.Vector3{X: regionConfig.Pos.X, Y: regionConfig.Pos.Y, Z: regionConfig.Pos.Z},
			vector2PointArray, regionConfig.Height)
	}
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	scene := world.GetSceneById(player.GetSceneId())
	count := 0
	for _, entity := range scene.GetAllEntity() {
		contain := shape.Contain(&alg.Vector3{X: float32(entity.GetPos().X), Y: float32(entity.GetPos().Y), Z: float32(entity.GetPos().Z)})
		if !contain {
			continue
		}
		if entity.GetEntityType() != uint8(luaTableParam.EntityType) {
			continue
		}
		count++
	}
	luaState.Push(lua.LNumber(count))
	return 1
}

func CreateGroupTimerEvent(luaState *lua.LState) int {
	ctx, ok := luaState.Get(1).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	player := GetContextPlayer(ctx, luaState)
	if player == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	groupId := luaState.ToInt(2)
	source := luaState.ToString(3)
	delay := luaState.ToInt(4)
	TICK_MANAGER.CreateUserTimer(player.PlayerId, UserTimerActionLuaGroupTimerEvent, uint32(delay),
		uint32(groupId), source)
	luaState.Push(lua.LNumber(0))
	return 1
}

func EnterWeatherArea(luaState *lua.LState) int {
	ctx, ok := luaState.Get(1).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	player := GetContextPlayer(ctx, luaState)
	if player == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	weatherAreaId := luaState.ToInt(2)
	// 设置玩家天气
	climateType := GAME.GetWeatherAreaClimate(uint32(weatherAreaId))
	GAME.SetPlayerWeather(player, uint32(weatherAreaId), climateType, true)
	luaState.Push(lua.LNumber(0))
	return 1
}

func SetWeatherAreaState(luaState *lua.LState) int {
	ctx, ok := luaState.Get(1).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	player := GetContextPlayer(ctx, luaState)
	if player == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	weatherAreaId := luaState.ToInt(2)
	climateType := luaState.ToInt(3)
	GAME.SetPlayerWeather(player, uint32(weatherAreaId), uint32(climateType), true)
	luaState.Push(lua.LNumber(0))
	return 1
}

func RefreshGroup(luaState *lua.LState) int {
	ctx, ok := luaState.Get(1).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	player := GetContextPlayer(ctx, luaState)
	if player == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	luaTable, ok := luaState.Get(2).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	luaTableParam := new(CommonLuaTableParam)
	gdconf.ParseLuaTableToObject[*CommonLuaTableParam](luaTable, luaTableParam)
	GAME.RefreshSceneGroupSuite(player, uint32(luaTableParam.GroupId), uint8(luaTableParam.Suite))
	luaState.Push(lua.LNumber(0))
	return 1
}

func RemoveExtraGroupSuite(luaState *lua.LState) int {
	ctx, ok := luaState.Get(1).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	player := GetContextPlayer(ctx, luaState)
	if player == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	groupId := luaState.ToInt(2)
	suiteId := luaState.ToInt(3)
	GAME.RemoveSceneGroupSuite(player, uint32(groupId), uint8(suiteId))
	luaState.Push(lua.LNumber(0))
	return 1
}

func ShowReminder(luaState *lua.LState) int {
	luaState.Push(lua.LNumber(0))
	return 1
}

func KillGroupEntity(luaState *lua.LState) int {
	ctx, ok := luaState.Get(1).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	player := GetContextPlayer(ctx, luaState)
	if player == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	groupId, ok := luaState.GetField(ctx, "groupId").(lua.LNumber)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	luaTable, ok := luaState.Get(2).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	luaTableParam := new(CommonLuaTableParam)
	gdconf.ParseLuaTableToObject[*CommonLuaTableParam](luaTable, luaTableParam)
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	scene := world.GetSceneById(player.GetSceneId())
	group := scene.GetGroupById(uint32(groupId))
	if group == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	entityMap := group.GetAllEntity()
	switch luaTableParam.KillPolicy {
	case constant.GROUP_KILL_ALL:
		for _, entity := range entityMap {
			GAME.KillEntity(player, scene, entity.GetId(), proto.PlayerDieType_PLAYER_DIE_NONE)
		}
	case constant.GROUP_KILL_MONSTER:
		for _, entity := range entityMap {
			_, ok := entity.(*MonsterEntity)
			if !ok {
				continue
			}
			GAME.KillEntity(player, scene, entity.GetId(), proto.PlayerDieType_PLAYER_DIE_NONE)
		}
	case constant.GROUP_KILL_NPC:
		for _, entity := range entityMap {
			_, ok := entity.(*NpcEntity)
			if !ok {
				continue
			}
			GAME.KillEntity(player, scene, entity.GetId(), proto.PlayerDieType_PLAYER_DIE_NONE)
		}
	case constant.GROUP_KILL_GADGET:
		for _, entity := range entityMap {
			_, ok := entity.(IGadgetEntity)
			if !ok {
				continue
			}
			GAME.KillEntity(player, scene, entity.GetId(), proto.PlayerDieType_PLAYER_DIE_NONE)
		}
	}
	luaState.Push(lua.LNumber(0))
	return 1
}

func SetWorktopOptions(luaState *lua.LState) int {
	ctx, ok := luaState.Get(1).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	player := GetContextPlayer(ctx, luaState)
	if player == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	targetEntityId, ok := luaState.GetField(ctx, "target_entity_id").(lua.LNumber)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	luaTable, ok := luaState.Get(2).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	var optionList []uint32 = nil
	gdconf.ParseLuaTableToObject[*[]uint32](luaTable, &optionList)
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	scene := world.GetSceneById(player.SceneId)
	entity := scene.GetEntity(uint32(targetEntityId))
	if entity == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	gadgetWorktopEntity, ok := entity.(*GadgetWorktopEntity)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	optionMap := gadgetWorktopEntity.GetOptionMap()
	for _, option := range optionList {
		optionMap[option] = struct{}{}
	}
	GAME.SendToSceneA(scene, cmd.WorktopOptionNotify, player.ClientSeq, &proto.WorktopOptionNotify{
		GadgetEntityId: gadgetWorktopEntity.GetId(),
		OptionList:     object.ConvMapKeyToList[uint32, struct{}](gadgetWorktopEntity.GetOptionMap()),
	}, 0)
	luaState.Push(lua.LNumber(0))
	return 1
}

func SetWorktopOptionsByGroupId(luaState *lua.LState) int {
	ctx, ok := luaState.Get(1).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	player := GetContextPlayer(ctx, luaState)
	if player == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	groupId := luaState.ToInt(2)
	configId := luaState.ToInt(3)
	luaTable, ok := luaState.Get(4).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	var optionList []uint32 = nil
	gdconf.ParseLuaTableToObject[*[]uint32](luaTable, &optionList)
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	scene := world.GetSceneById(player.SceneId)
	group := scene.GetGroupById(uint32(groupId))
	if group == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	entity := group.GetEntityByConfigId(uint32(configId))
	if entity == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	gadgetWorktopEntity, ok := entity.(*GadgetWorktopEntity)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	optionMap := gadgetWorktopEntity.GetOptionMap()
	for _, option := range optionList {
		optionMap[option] = struct{}{}
	}
	GAME.SendToSceneA(scene, cmd.WorktopOptionNotify, player.ClientSeq, &proto.WorktopOptionNotify{
		GadgetEntityId: gadgetWorktopEntity.GetId(),
		OptionList:     object.ConvMapKeyToList[uint32, struct{}](gadgetWorktopEntity.GetOptionMap()),
	}, 0)
	luaState.Push(lua.LNumber(0))
	return 1
}

func DelWorktopOption(luaState *lua.LState) int {
	ctx, ok := luaState.Get(1).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	player := GetContextPlayer(ctx, luaState)
	if player == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	targetEntityId, ok := luaState.GetField(ctx, "target_entity_id").(lua.LNumber)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	option := luaState.ToInt(2)
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	scene := world.GetSceneById(player.SceneId)
	entity := scene.GetEntity(uint32(targetEntityId))
	if entity == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	gadgetWorktopEntity, ok := entity.(*GadgetWorktopEntity)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	optionMap := gadgetWorktopEntity.GetOptionMap()
	delete(optionMap, uint32(option))
	GAME.SendToSceneA(scene, cmd.WorktopOptionNotify, player.ClientSeq, &proto.WorktopOptionNotify{
		GadgetEntityId: gadgetWorktopEntity.GetId(),
		OptionList:     object.ConvMapKeyToList[uint32, struct{}](gadgetWorktopEntity.GetOptionMap()),
	}, 0)
	luaState.Push(lua.LNumber(0))
	return 1
}

func DelWorktopOptionByGroupId(luaState *lua.LState) int {
	ctx, ok := luaState.Get(1).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	player := GetContextPlayer(ctx, luaState)
	if player == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	groupId := luaState.ToInt(2)
	configId := luaState.ToInt(3)
	option := luaState.ToInt(4)
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	scene := world.GetSceneById(player.SceneId)
	group := scene.GetGroupById(uint32(groupId))
	if group == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	entity := group.GetEntityByConfigId(uint32(configId))
	if entity == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	gadgetWorktopEntity, ok := entity.(*GadgetWorktopEntity)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	optionMap := gadgetWorktopEntity.GetOptionMap()
	delete(optionMap, uint32(option))
	GAME.SendToSceneA(scene, cmd.WorktopOptionNotify, player.ClientSeq, &proto.WorktopOptionNotify{
		GadgetEntityId: gadgetWorktopEntity.GetId(),
		OptionList:     object.ConvMapKeyToList[uint32, struct{}](gadgetWorktopEntity.GetOptionMap()),
	}, 0)
	luaState.Push(lua.LNumber(0))
	return 1
}

func SetGadgetState(luaState *lua.LState) int {
	ctx, ok := luaState.Get(1).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	player := GetContextPlayer(ctx, luaState)
	if player == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	targetEntityId, ok := luaState.GetField(ctx, "target_entity_id").(lua.LNumber)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	state := luaState.ToInt(2)
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	scene := world.GetSceneById(player.SceneId)
	entity := scene.GetEntity(uint32(targetEntityId))
	if entity == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	GAME.ChangeGadgetState(player, entity.GetId(), uint32(state))
	luaState.Push(lua.LNumber(0))
	return 1
}

func GetGadgetState(luaState *lua.LState) int {
	ctx, ok := luaState.Get(1).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	player := GetContextPlayer(ctx, luaState)
	if player == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	targetEntityId, ok := luaState.GetField(ctx, "target_entity_id").(lua.LNumber)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	scene := world.GetSceneById(player.GetSceneId())
	entity := scene.GetEntity(uint32(targetEntityId))
	if entity == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	iGadgetEntity, ok := entity.(IGadgetEntity)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	luaState.Push(lua.LNumber(iGadgetEntity.GetGadgetState()))
	return 1
}

func GetContextGadgetConfigId(luaState *lua.LState) int {
	ctx, ok := luaState.Get(1).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	player := GetContextPlayer(ctx, luaState)
	if player == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	targetEntityId, ok := luaState.GetField(ctx, "target_entity_id").(lua.LNumber)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	scene := world.GetSceneById(player.GetSceneId())
	entity := scene.GetEntity(uint32(targetEntityId))
	if entity == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	luaState.Push(lua.LNumber(entity.GetConfigId()))
	return 1
}

func GetContextGroupId(luaState *lua.LState) int {
	ctx, ok := luaState.Get(1).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	groupId, ok := luaState.GetField(ctx, "groupId").(lua.LNumber)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	luaState.Push(groupId)
	return 1
}

func DropSubfield(luaState *lua.LState) int {
	ctx, ok := luaState.Get(1).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	player := GetContextPlayer(ctx, luaState)
	if player == nil {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	luaTable, ok := luaState.Get(2).(*lua.LTable)
	if !ok {
		luaState.Push(lua.LNumber(-1))
		return 1
	}
	luaTableParam := new(CommonLuaTableParam)
	gdconf.ParseLuaTableToObject[*CommonLuaTableParam](luaTable, luaTableParam)
	logger.Debug("Lua DropSubfield SubfieldName: %v", luaTableParam.SubfieldName)
	luaState.Push(lua.LNumber(0))
	return 1
}
