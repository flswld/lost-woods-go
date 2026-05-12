package game

import (
	"hk4e/common/constant"
	"hk4e/gdconf"
	"hk4e/gs/model"
	"hk4e/pkg/alg"

	"github.com/flswld/halo/logger"
)

// Lua 触发器 模块
//
// Lua trigger 是场景内"事件 → 检查条件 → 执行动作"的机制 由原神官服 Lua 配置定义
// 每个 trigger 监听一种事件（9 种），匹配时调 Lua 函数执行 condition + action
//
// 9 种事件类型（与 CLAUDE.md 任务系统一致）：
//   - ENTER_REGION / LEAVE_REGION: 玩家进入/离开 region（4 种形状：球/方/柱/多边形）
//   - QUEST_START: 任务开始时
//   - ANY_MONSTER_LIVE / ANY_MONSTER_DIE: 怪物创建/死亡
//   - GADGET_CREATE / GADGET_STATE_CHANGE / ANY_GADGET_DIE: 物件相关
//   - GROUP_LOAD: 场景组加载时
//   - TIMER_EVENT: Lua 自己创建的定时器到期
//   - SELECT_OPTION: 操作台选项选中
//
// 调用流程：玩家行为（移动/打怪等）→ TriggerCheck 函数 → 遍历相关 group 的 trigger
//   → 调 Lua condition 检查 → 满足则调 Lua action 执行 → 必要时推进任务进度

// forEachPlayerSceneGroup 遍历玩家场景内所有 group 的所有 suite（性能敏感 trigger 检测都靠这个）
func forEachPlayerSceneGroup(player *model.Player, handleFunc func(suiteConfig *gdconf.Suite, groupConfig *gdconf.Group)) {
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		return
	}
	scene := world.GetSceneById(player.GetSceneId())
	for groupId, group := range scene.GetAllGroup() {
		groupConfig := gdconf.GetSceneGroup(int32(groupId))
		if groupConfig == nil {
			logger.Error("get group config is nil, groupId: %v, uid: %v", groupId, player.PlayerId)
			continue
		}
		for suiteId := range group.GetAllSuite() {
			suiteConfig := groupConfig.SuiteMap[int32(suiteId)]
			handleFunc(suiteConfig, groupConfig)
		}
	}
}

func forEachPlayerSceneGroupTrigger(player *model.Player, handleFunc func(triggerConfig *gdconf.Trigger, groupConfig *gdconf.Group)) {
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		return
	}
	scene := world.GetSceneById(player.GetSceneId())
	for groupId, group := range scene.GetAllGroup() {
		groupConfig := gdconf.GetSceneGroup(int32(groupId))
		if groupConfig == nil {
			logger.Error("get group config is nil, groupId: %v, uid: %v", groupId, player.PlayerId)
			continue
		}
		for suiteId := range group.GetAllSuite() {
			suiteConfig := groupConfig.SuiteMap[int32(suiteId)]
			for _, triggerName := range suiteConfig.TriggerNameList {
				triggerConfig := groupConfig.TriggerMap[triggerName]
				handleFunc(triggerConfig, groupConfig)
			}
		}
	}
}

func forEachGroupTrigger(player *model.Player, group *Group, handleFunc func(triggerConfig *gdconf.Trigger, groupConfig *gdconf.Group)) {
	groupConfig := gdconf.GetSceneGroup(int32(group.GetId()))
	if groupConfig == nil {
		logger.Error("get group config is nil, groupId: %v, uid: %v", group.GetId(), player.PlayerId)
		return
	}
	for suiteId := range group.GetAllSuite() {
		suiteConfig := groupConfig.SuiteMap[int32(suiteId)]
		for _, triggerName := range suiteConfig.TriggerNameList {
			triggerConfig := groupConfig.TriggerMap[triggerName]
			handleFunc(triggerConfig, groupConfig)
		}
	}
}

// SceneRegionTriggerCheck 场景区域触发器检测（玩家移动时调用 检测进入/离开 region）
//
// 4 种 region 形状（pkg/alg/shape.go）：
//   - SPHERE: 球形（中心+半径）
//   - CUBIC: 立方体（中心+三向尺寸）
//   - CYLINDER: 圆柱（中心+半径+高度）
//   - POLYGON: 多边形柱体（中心+底面顶点+高度）
//
// 检测逻辑：oldPos 不在 region + newPos 在 region → 触发 ENTER_REGION
//
//	oldPos 在 region + newPos 不在 region → 触发 LEAVE_REGION
//
// 调用方：handleEntityMove 玩家角色实体移动时调用一次
// 注意：这个检测会遍历所有附近 group 的所有 region 性能敏感 但每秒数次玩家移动是可接受的
func (g *Game) SceneRegionTriggerCheck(player *model.Player, oldPos *model.Vector, newPos *model.Vector, entityId uint32) {
	forEachPlayerSceneGroup(player, func(suiteConfig *gdconf.Suite, groupConfig *gdconf.Group) {
		for _, regionConfigId := range suiteConfig.RegionConfigIdList {
			regionConfig := groupConfig.RegionMap[regionConfigId]
			if regionConfig == nil {
				continue
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
			oldPosInRegion := shape.Contain(&alg.Vector3{X: float32(oldPos.X), Y: float32(oldPos.Y), Z: float32(oldPos.Z)})
			newPosInRegion := shape.Contain(&alg.Vector3{X: float32(newPos.X), Y: float32(newPos.Y), Z: float32(newPos.Z)})
			if !oldPosInRegion && newPosInRegion {
				logger.Debug("player enter region: %v, uid: %v", regionConfig, player.PlayerId)
				for _, triggerName := range suiteConfig.TriggerNameList {
					triggerConfig := groupConfig.TriggerMap[triggerName]
					if triggerConfig.Event != constant.LUA_EVENT_ENTER_REGION {
						continue
					}
					if triggerConfig.Condition != "" {
						cond := CallSceneLuaFunc(groupConfig.GetLuaState(), triggerConfig.Condition,
							&LuaCtx{uid: player.PlayerId, groupId: uint32(groupConfig.Id)},
							&LuaEvt{param1: regionConfig.ConfigId, targetEntityId: entityId, sourceEntityId: uint32(regionConfig.ConfigId)})
						if !cond {
							continue
						}
					}
					logger.Debug("scene group trigger fire, trigger: %+v, uid: %v", triggerConfig, player.PlayerId)
					if triggerConfig.Action != "" {
						logger.Debug("scene group trigger do action, trigger: %+v, uid: %v", triggerConfig, player.PlayerId)
						ok := CallSceneLuaFunc(groupConfig.GetLuaState(), triggerConfig.Action,
							&LuaCtx{uid: player.PlayerId, groupId: uint32(groupConfig.Id)},
							&LuaEvt{})
						if !ok {
							logger.Error("trigger action fail, trigger: %+v, uid: %v", triggerConfig, player.PlayerId)
						}
					}
					for _, triggerDataConfig := range gdconf.GetTriggerDataMap() {
						if triggerDataConfig.TriggerName == triggerConfig.Name {
							g.TriggerQuest(player, constant.QUEST_FINISH_COND_TYPE_TRIGGER_FIRE, "", triggerDataConfig.TriggerId)
						}
					}
				}
			} else if oldPosInRegion && !newPosInRegion {
				logger.Debug("player leave region: %v, uid: %v", regionConfig, player.PlayerId)
				for _, triggerName := range suiteConfig.TriggerNameList {
					triggerConfig := groupConfig.TriggerMap[triggerName]
					if triggerConfig.Event != constant.LUA_EVENT_LEAVE_REGION {
						continue
					}
					if triggerConfig.Condition != "" {
						cond := CallSceneLuaFunc(groupConfig.GetLuaState(), triggerConfig.Condition,
							&LuaCtx{uid: player.PlayerId, groupId: uint32(groupConfig.Id)},
							&LuaEvt{param1: regionConfig.ConfigId, targetEntityId: entityId, sourceEntityId: uint32(regionConfig.ConfigId)})
						if !cond {
							continue
						}
					}
					logger.Debug("scene group trigger fire, trigger: %+v, uid: %v", triggerConfig, player.PlayerId)
					if triggerConfig.Action != "" {
						logger.Debug("scene group trigger do action, trigger: %+v, uid: %v", triggerConfig, player.PlayerId)
						ok := CallSceneLuaFunc(groupConfig.GetLuaState(), triggerConfig.Action,
							&LuaCtx{uid: player.PlayerId, groupId: uint32(groupConfig.Id)},
							&LuaEvt{})
						if !ok {
							logger.Error("trigger action fail, trigger: %+v, uid: %v", triggerConfig, player.PlayerId)
						}
					}
				}
			}
		}
	})
}

// QuestStartTriggerCheck 任务开始触发器检测（任务接受时调用）
// 调用方：StartQuest 在任务开始时调用 让 Lua trigger 知道某任务开始了
// Lua 可以在 condition 里检查 evt.param1（questId）匹配自己关心的任务
func (g *Game) QuestStartTriggerCheck(player *model.Player, questId uint32) {
	forEachPlayerSceneGroupTrigger(player, func(triggerConfig *gdconf.Trigger, groupConfig *gdconf.Group) {
		if triggerConfig.Event != constant.LUA_EVENT_QUEST_START {
			return
		}
		if triggerConfig.Condition != "" {
			cond := CallSceneLuaFunc(groupConfig.GetLuaState(), triggerConfig.Condition,
				&LuaCtx{uid: player.PlayerId, groupId: uint32(groupConfig.Id)},
				&LuaEvt{param1: int32(questId)})
			if !cond {
				return
			}
		}
		if triggerConfig.Action != "" {
			logger.Debug("scene group trigger do action, trigger: %+v, uid: %v", triggerConfig, player.PlayerId)
			ok := CallSceneLuaFunc(groupConfig.GetLuaState(), triggerConfig.Action,
				&LuaCtx{uid: player.PlayerId, groupId: uint32(groupConfig.Id)},
				&LuaEvt{})
			if !ok {
				logger.Error("trigger action fail, trigger: %+v, uid: %v", triggerConfig, player.PlayerId)
			}
		}
	})
}

// MonsterCreateTriggerCheck 怪物创建触发器检测（仅在 group 内创建怪物时调用）
// 调用方：SceneGroupCreateEntity 创建怪物时调用
// 用于"刷新场景组让怪生成 → 让另一个 group 同步切 suite"这类联动
func (g *Game) MonsterCreateTriggerCheck(player *model.Player, group *Group, entity IEntity) {
	forEachGroupTrigger(player, group, func(triggerConfig *gdconf.Trigger, groupConfig *gdconf.Group) {
		if triggerConfig.Event != constant.LUA_EVENT_ANY_MONSTER_LIVE {
			return
		}
		if triggerConfig.Condition != "" {
			cond := CallSceneLuaFunc(groupConfig.GetLuaState(), triggerConfig.Condition,
				&LuaCtx{uid: player.PlayerId, groupId: uint32(groupConfig.Id)},
				&LuaEvt{param1: int32(entity.GetId())})
			if !cond {
				return
			}
		}
		if triggerConfig.Action != "" {
			logger.Debug("scene group trigger do action, trigger: %+v, uid: %v", triggerConfig, player.PlayerId)
			ok := CallSceneLuaFunc(groupConfig.GetLuaState(), triggerConfig.Action,
				&LuaCtx{uid: player.PlayerId, groupId: uint32(groupConfig.Id), targetEntityId: entity.GetId()},
				&LuaEvt{})
			if !ok {
				logger.Error("trigger action fail, trigger: %+v, uid: %v", triggerConfig, player.PlayerId)
			}
		}
	})
}

// MonsterDieTriggerCheck 怪物死亡触发器检测（最常用 各种"打怪 → 解锁宝箱"用这个）
// 调用方：KillEntity 怪物死亡时调用
// Lua action 里通常调 ScriptLib.SetGroupVariableValue + ScriptLib.GetGroupMonsterCount 计数 + 全杀完后切 suite
func (g *Game) MonsterDieTriggerCheck(player *model.Player, group *Group, entity IEntity) {
	forEachGroupTrigger(player, group, func(triggerConfig *gdconf.Trigger, groupConfig *gdconf.Group) {
		if triggerConfig.Event != constant.LUA_EVENT_ANY_MONSTER_DIE {
			return
		}
		if triggerConfig.Condition != "" {
			cond := CallSceneLuaFunc(groupConfig.GetLuaState(), triggerConfig.Condition,
				&LuaCtx{uid: player.PlayerId, groupId: uint32(groupConfig.Id)},
				&LuaEvt{})
			if !cond {
				return
			}
		}
		if triggerConfig.Action != "" {
			logger.Debug("scene group trigger do action, trigger: %+v, uid: %v", triggerConfig, player.PlayerId)
			ok := CallSceneLuaFunc(groupConfig.GetLuaState(), triggerConfig.Action,
				&LuaCtx{uid: player.PlayerId, groupId: uint32(groupConfig.Id), targetEntityId: entity.GetId()},
				&LuaEvt{})
			if !ok {
				logger.Error("trigger action fail, trigger: %+v, uid: %v", triggerConfig, player.PlayerId)
			}
		}
	})
}

// GadgetCreateTriggerCheck 物件创建触发器检测
// 调用方：SceneGroupCreateEntity 创建物件时调用
// 用于"物件出现 → 触发关联事件"如某宝箱出现就播放音效
func (g *Game) GadgetCreateTriggerCheck(player *model.Player, group *Group, entity IEntity) {
	forEachGroupTrigger(player, group, func(triggerConfig *gdconf.Trigger, groupConfig *gdconf.Group) {
		if triggerConfig.Event != constant.LUA_EVENT_GADGET_CREATE {
			return
		}
		if triggerConfig.Condition != "" {
			cond := CallSceneLuaFunc(groupConfig.GetLuaState(), triggerConfig.Condition,
				&LuaCtx{uid: player.PlayerId, groupId: uint32(groupConfig.Id)},
				&LuaEvt{param1: int32(entity.GetConfigId())})
			if !cond {
				return
			}
		}
		if triggerConfig.Action != "" {
			logger.Debug("scene group trigger do action, trigger: %+v, uid: %v", triggerConfig, player.PlayerId)
			ok := CallSceneLuaFunc(groupConfig.GetLuaState(), triggerConfig.Action,
				&LuaCtx{uid: player.PlayerId, groupId: uint32(groupConfig.Id), targetEntityId: entity.GetId()},
				&LuaEvt{})
			if !ok {
				logger.Error("trigger action fail, trigger: %+v, uid: %v", triggerConfig, player.PlayerId)
			}
		}
	})
}

// GadgetStateChangeTriggerCheck 物件状态变更触发器检测（如机关激活/宝箱开启）
// 调用方：ChangeGadgetState 物件状态变更时调用
// 常用于"按下按钮（操作台）→ 触发某事件"这类机关解谜
func (g *Game) GadgetStateChangeTriggerCheck(player *model.Player, group *Group, entity IEntity, state uint32) {
	forEachGroupTrigger(player, group, func(triggerConfig *gdconf.Trigger, groupConfig *gdconf.Group) {
		if triggerConfig.Event != constant.LUA_EVENT_GADGET_STATE_CHANGE {
			return
		}
		if triggerConfig.Condition != "" {
			cond := CallSceneLuaFunc(groupConfig.GetLuaState(), triggerConfig.Condition,
				&LuaCtx{uid: player.PlayerId, groupId: uint32(groupConfig.Id)},
				&LuaEvt{param1: int32(state), param2: int32(entity.GetConfigId())})
			if !cond {
				return
			}
		}
		if triggerConfig.Action != "" {
			logger.Debug("scene group trigger do action, trigger: %+v, uid: %v", triggerConfig, player.PlayerId)
			ok := CallSceneLuaFunc(groupConfig.GetLuaState(), triggerConfig.Action,
				&LuaCtx{uid: player.PlayerId, groupId: uint32(groupConfig.Id), targetEntityId: entity.GetId()},
				&LuaEvt{})
			if !ok {
				logger.Error("trigger action fail, trigger: %+v, uid: %v", triggerConfig, player.PlayerId)
			}
		}
	})
}

// GadgetDieTriggerCheck 物件死亡/销毁触发器检测
// 调用方：KillEntity 物件被销毁时调用（如打破破坏物 / 宝箱拿完后销毁）
func (g *Game) GadgetDieTriggerCheck(player *model.Player, group *Group, entity IEntity) {
	forEachGroupTrigger(player, group, func(triggerConfig *gdconf.Trigger, groupConfig *gdconf.Group) {
		if triggerConfig.Event != constant.LUA_EVENT_ANY_GADGET_DIE {
			return
		}
		if triggerConfig.Condition != "" {
			cond := CallSceneLuaFunc(groupConfig.GetLuaState(), triggerConfig.Condition,
				&LuaCtx{uid: player.PlayerId, groupId: uint32(groupConfig.Id)},
				&LuaEvt{param1: int32(entity.GetConfigId())})
			if !cond {
				return
			}
		}
		if triggerConfig.Action != "" {
			logger.Debug("scene group trigger do action, trigger: %+v, uid: %v", triggerConfig, player.PlayerId)
			ok := CallSceneLuaFunc(groupConfig.GetLuaState(), triggerConfig.Action,
				&LuaCtx{uid: player.PlayerId, groupId: uint32(groupConfig.Id), targetEntityId: entity.GetId()},
				&LuaEvt{})
			if !ok {
				logger.Error("trigger action fail, trigger: %+v, uid: %v", triggerConfig, player.PlayerId)
			}
		}
	})
}

// GroupLoadTriggerCheck 场景组加载触发器检测（玩家进入 group 范围首次加载时调用）
// 用途：group 加载时初始化（如根据存档变量决定 group 出哪批怪 / 物件什么状态）
func (g *Game) GroupLoadTriggerCheck(player *model.Player, group *Group) {
	forEachGroupTrigger(player, group, func(triggerConfig *gdconf.Trigger, groupConfig *gdconf.Group) {
		if triggerConfig.Event != constant.LUA_EVENT_GROUP_LOAD {
			return
		}
		if triggerConfig.Condition != "" {
			cond := CallSceneLuaFunc(groupConfig.GetLuaState(), triggerConfig.Condition,
				&LuaCtx{uid: player.PlayerId, groupId: uint32(groupConfig.Id)},
				&LuaEvt{})
			if !cond {
				return
			}
		}
		if triggerConfig.Action != "" {
			logger.Debug("scene group trigger do action, trigger: %+v, uid: %v", triggerConfig, player.PlayerId)
			ok := CallSceneLuaFunc(groupConfig.GetLuaState(), triggerConfig.Action,
				&LuaCtx{uid: player.PlayerId, groupId: uint32(groupConfig.Id)},
				&LuaEvt{})
			if !ok {
				logger.Error("trigger action fail, trigger: %+v, uid: %v", triggerConfig, player.PlayerId)
			}
		}
	})
}

// TimerEventTriggerCheck Lua 定时器事件触发器检测
//
// Lua 通过 ScriptLib.CreateGroupTimerEvent 创建定时器 N 秒后触发对应 trigger
// 本函数由 TICK_MANAGER 在定时器到期时调用
// triggerConfig.Source 可指定定时器名（同 group 可有多个定时器） 不匹配会跳过
func (g *Game) TimerEventTriggerCheck(player *model.Player, group *Group, source string) {
	forEachGroupTrigger(player, group, func(triggerConfig *gdconf.Trigger, groupConfig *gdconf.Group) {
		if triggerConfig.Event != constant.LUA_EVENT_TIMER_EVENT {
			return
		}
		if triggerConfig.Source != "" {
			if triggerConfig.Source != source {
				return
			}
		}
		if triggerConfig.Condition != "" {
			cond := CallSceneLuaFunc(groupConfig.GetLuaState(), triggerConfig.Condition,
				&LuaCtx{uid: player.PlayerId, groupId: uint32(groupConfig.Id)},
				&LuaEvt{sourceName: source})
			if !cond {
				return
			}
		}
		if triggerConfig.Action != "" {
			logger.Debug("scene group trigger do action, trigger: %+v, uid: %v", triggerConfig, player.PlayerId)
			ok := CallSceneLuaFunc(groupConfig.GetLuaState(), triggerConfig.Action,
				&LuaCtx{uid: player.PlayerId, groupId: uint32(groupConfig.Id)},
				&LuaEvt{})
			if !ok {
				logger.Error("trigger action fail, trigger: %+v, uid: %v", triggerConfig, player.PlayerId)
			}
		}
	})
}

// SelectOptionTriggerCheck 操作台选项选中触发器检测
// 操作台 (Worktop) 可有多个选项 玩家通过 SelectWorktopOptionReq 选中后调用
// trigger 通过 evt.param1 区分玩家选了哪个选项（如"是/否"）
func (g *Game) SelectOptionTriggerCheck(player *model.Player, group *Group, entity IEntity, option uint32) {
	forEachGroupTrigger(player, group, func(triggerConfig *gdconf.Trigger, groupConfig *gdconf.Group) {
		if triggerConfig.Event != constant.LUA_EVENT_SELECT_OPTION {
			return
		}
		if triggerConfig.Condition != "" {
			cond := CallSceneLuaFunc(groupConfig.GetLuaState(), triggerConfig.Condition,
				&LuaCtx{uid: player.PlayerId, groupId: uint32(groupConfig.Id)},
				&LuaEvt{param1: int32(entity.GetConfigId()), param2: int32(option)})
			if !cond {
				return
			}
		}
		if triggerConfig.Action != "" {
			logger.Debug("scene group trigger do action, trigger: %+v, uid: %v", triggerConfig, player.PlayerId)
			ok := CallSceneLuaFunc(groupConfig.GetLuaState(), triggerConfig.Action,
				&LuaCtx{uid: player.PlayerId, groupId: uint32(groupConfig.Id), targetEntityId: entity.GetId()},
				&LuaEvt{})
			if !ok {
				logger.Error("trigger action fail, trigger: %+v, uid: %v", triggerConfig, player.PlayerId)
			}
		}
	})
}
