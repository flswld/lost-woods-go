package game

import (
	"strconv"
	"strings"

	"hk4e/common/constant"
	"hk4e/gdconf"
	"hk4e/gs/model"
	"hk4e/protocol/cmd"
	"hk4e/protocol/proto"

	"github.com/flswld/halo/logger"
	pb "google.golang.org/protobuf/proto"
)

// 任务系统 模块（接入有限 仅做了一小部分）
//
// 任务三大入口：
//   - 玩家行为驱动：AcceptQuest（接取检查）+ TriggerQuest（玩家行为推进任务）
//   - Lua 脚本驱动：lua_func.go 的 ScriptLib.AddQuestProgress → TriggerQuest(LUA_NOTIFY)
//   - 客户端主动：AddQuestContentProgressReq（任务系统的客户端预留入口）
//
// 任务生命周期：
//   AcceptQuest（接取条件检查）→ StartQuest（任务开始 + 执行 StartExecList）
//      → TriggerQuest（玩家行为推进 finishCond 累加）→ CheckQuestFinish
//      → FinishQuest（执行 FinishExecList 给奖励）+ DeleteQuest from active
//   或 → FailQuest（任务失败 执行 FailExecList）
//
// **现状（参见 CLAUDE.md "任务系统协作"）**：
//   - AcceptCond 仅支持 STATE_EQUAL / STATE_NOT_EQUAL 两种条件类型
//   - AcceptCondCompose 6 种逻辑都已实现：NONE/AND, OR, A_AND_ETCOR, A_AND_B_AND_ETCOR, A_OR_ETCAND, A_OR_B_OR_ETCAND
//   - ExecType 12+ 种已实现（NOTIFY_GROUP_LUA / REFRESH_GROUP_SUITE / SET_OPEN_STATE 等）
//   - **主线/支线/世界任务/邀约任务的完整流程未实现** 仅做了任务的"骨架机制" 不能完整跑通官服剧情

/************************************************** 接口请求 **************************************************/

// AddQuestContentProgressReq 客户端主动推进任务进度请求
// 一些任务的完成条件由客户端检测（如对话选项/特殊互动）由客户端发请求让服务端推进
// 处理后立即调 AcceptQuest 检查是否解锁了新任务（任务接取条件可能依赖另一任务的完成状态）
func (g *Game) AddQuestContentProgressReq(player *model.Player, payloadMsg pb.Message) {
	req := payloadMsg.(*proto.AddQuestContentProgressReq)

	g.TriggerQuest(player, int32(req.ContentType), "", int32(req.Param))

	rsp := &proto.AddQuestContentProgressRsp{
		ContentType: req.ContentType,
	}
	g.SendMsg(cmd.AddQuestContentProgressRsp, player.PlayerId, player.ClientSeq, rsp)

	g.AcceptQuest(player, true)
}

func (g *Game) QuestCreateEntityReq(player *model.Player, payloadMsg pb.Message) {
	req := payloadMsg.(*proto.QuestCreateEntityReq)
	itemDataConfig := gdconf.GetItemDataById(int32(req.Entity.GetItemId()))
	if itemDataConfig == nil {
		g.SendError(cmd.QuestCreateEntityRsp, player, &proto.QuestCreateEntityRsp{})
		return
	}
	pos := &model.Vector{X: float64(req.Entity.Pos.X), Y: float64(req.Entity.Pos.Y), Z: float64(req.Entity.Pos.Z)}
	entityId := g.CreateDropGadget(player, pos, uint32(itemDataConfig.GadgetId), req.Entity.GetItemId(), 1)
	if entityId == 0 {
		g.SendError(cmd.QuestCreateEntityRsp, player, &proto.QuestCreateEntityRsp{})
		return
	}
	rsp := &proto.QuestCreateEntityRsp{
		QuestId:       req.QuestId,
		EntityId:      entityId,
		Entity:        req.Entity,
		ParentQuestId: req.ParentQuestId,
		IsRewind:      req.IsRewind,
	}
	g.SendMsg(cmd.QuestCreateEntityRsp, player.PlayerId, player.ClientSeq, rsp)
}

func (g *Game) QuestDestroyEntityReq(player *model.Player, payloadMsg pb.Message) {
	req := payloadMsg.(*proto.QuestDestroyEntityReq)
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		g.SendError(cmd.QuestDestroyEntityRsp, player, &proto.QuestDestroyEntityRsp{})
		return
	}
	scene := world.GetSceneById(req.SceneId)
	entity := scene.GetEntity(req.EntityId)
	if entity == nil {
		g.SendError(cmd.QuestDestroyEntityRsp, player, &proto.QuestDestroyEntityRsp{})
		return
	}
	scene.DestroyEntity(req.EntityId)
	g.RemoveSceneEntityNotifyBroadcast(scene, proto.VisionType_VISION_MISS, []uint32{req.EntityId}, 0)
	rsp := &proto.QuestDestroyEntityRsp{
		QuestId:  req.QuestId,
		SceneId:  req.SceneId,
		EntityId: req.EntityId,
	}
	g.SendMsg(cmd.QuestDestroyEntityRsp, player.PlayerId, player.ClientSeq, rsp)
}

func (g *Game) QuestDestroyNpcReq(player *model.Player, payloadMsg pb.Message) {
	req := payloadMsg.(*proto.QuestDestroyNpcReq)
	logger.Debug("quest destroy npc, npcId: %v, parentQuestId: %v, uid: %v", req.NpcId, req.ParentQuestId, player.PlayerId)
	rsp := &proto.QuestDestroyNpcRsp{
		NpcId:         req.NpcId,
		ParentQuestId: req.ParentQuestId,
	}
	g.SendMsg(cmd.QuestDestroyNpcRsp, player.PlayerId, player.ClientSeq, rsp)
}

/************************************************** 游戏功能 **************************************************/

const (
	QuestExecTypeFinish = iota
	QuestExecTypeFail
	QuestExecTypeStart
)

// 通用参数匹配
func matchParamEqual(param1 []int32, param2 []int32, num int) bool {
	if len(param1) != num || len(param2) != num {
		return false
	}
	for i := 0; i < num; i++ {
		if param1[i] != param2[i] {
			return false
		}
	}
	return true
}

// AcceptQuest 接取任务（核心入口 全表扫描所有 quest 检查接取条件）
//
// 处理：
//  1. 遍历 gdconf.GetQuestDataMap() 全部 quest（数千个）
//  2. 跳过玩家已有的 quest
//  3. 检查每个 AcceptCond（仅支持 STATE_EQUAL/STATE_NOT_EQUAL）
//  4. 按 AcceptCondCompose 组合多个条件结果（AND/OR/A_AND_ETCOR/A_OR_ETCAND/...）
//  5. 满足条件 → dbQuest.AddQuest 加入玩家档 → StartQuest
//
// **特殊hack**（line 240）：风龙任务 35722 一接受就立即 finish
//
//	原因：进风龙秘境客户端会无限重连 作者绕过此问题的暴力方案
//
// 性能：每次玩家行为都可能触发 AcceptQuest 全表扫描（任务条件依赖其他任务状态）
//
//	EndlessLoopCheck 防止递归（任务A接受→触发任务B接受→触发任务C接受→...）
func (g *Game) AcceptQuest(player *model.Player, notify bool) {
	g.EndlessLoopCheck(EndlessLoopCheckTypeAcceptQuest)
	dbQuest := player.GetDbQuest()
	addQuestIdList := make([]uint32, 0)
	for _, questData := range gdconf.GetQuestDataMap() {
		if dbQuest.GetQuestById(uint32(questData.QuestId)) != nil {
			continue
		}
		resultList := make([]bool, 0)
		for _, acceptCond := range questData.AcceptCondList {
			result := false
			switch acceptCond.Type {
			case constant.QUEST_ACCEPT_COND_TYPE_STATE_EQUAL:
				// 某个任务状态等于 参数1:任务id 参数2:任务状态
				if len(acceptCond.Param) != 2 {
					break
				}
				quest := dbQuest.GetQuestById(uint32(acceptCond.Param[0]))
				if quest == nil {
					break
				}
				if quest.State != uint8(acceptCond.Param[1]) {
					break
				}
				result = true
			case constant.QUEST_ACCEPT_COND_TYPE_STATE_NOT_EQUAL:
				// 某个任务状态不等于 参数1:任务id 参数2:任务状态
				if len(acceptCond.Param) != 2 {
					break
				}
				quest := dbQuest.GetQuestById(uint32(acceptCond.Param[0]))
				if quest == nil {
					break
				}
				if quest.State == uint8(acceptCond.Param[1]) {
					break
				}
				result = true
			default:
				// logger.Error("not support quest accept cond type: %v, questId: %v, uid: %v", acceptCond.Type, questData.QuestId, player.PlayerId)
				continue
			}
			resultList = append(resultList, result)
		}
		if len(resultList) != len(questData.AcceptCondList) {
			continue
		}
		accept := false
		switch questData.AcceptCondCompose {
		case constant.QUEST_LOGIC_TYPE_NONE:
			fallthrough
		case constant.QUEST_LOGIC_TYPE_AND:
			accept = true
			for _, result := range resultList {
				if !result {
					accept = false
					break
				}
			}
		case constant.QUEST_LOGIC_TYPE_OR:
			accept = false
			for _, result := range resultList {
				if result {
					accept = true
					break
				}
			}
		case constant.QUEST_LOGIC_TYPE_A_AND_ETCOR:
			if len(resultList) < 2 {
				accept = false
				break
			}
			acceptA := resultList[0]
			acceptEtc := false
			for _, result := range resultList[1:] {
				if result {
					acceptEtc = true
					break
				}
			}
			accept = acceptA && acceptEtc
		case constant.QUEST_LOGIC_TYPE_A_AND_B_AND_ETCOR:
			if len(resultList) < 3 {
				accept = false
				break
			}
			acceptA := resultList[0]
			acceptB := resultList[1]
			acceptEtc := false
			for _, result := range resultList[2:] {
				if result {
					acceptEtc = true
					break
				}
			}
			accept = acceptA && acceptB && acceptEtc
		case constant.QUEST_LOGIC_TYPE_A_OR_ETCAND:
			if len(resultList) < 2 {
				accept = false
				break
			}
			acceptA := resultList[0]
			acceptEtc := true
			for _, result := range resultList[1:] {
				if !result {
					acceptEtc = false
					break
				}
			}
			accept = acceptA || acceptEtc
		case constant.QUEST_LOGIC_TYPE_A_OR_B_OR_ETCAND:
			if len(resultList) < 3 {
				accept = false
				break
			}
			acceptA := resultList[0]
			acceptB := resultList[1]
			acceptEtc := true
			for _, result := range resultList[2:] {
				if !result {
					acceptEtc = false
					break
				}
			}
			accept = acceptA || acceptB || acceptEtc
		default:
			logger.Error("not support quest accept cond logic type: %v, questId: %v, uid: %v", questData.AcceptCondCompose, questData.QuestId, player.PlayerId)
		}
		if accept {
			if questData.QuestId == 35722 {
				// TODO 由于风龙任务进入秘境客户端会无限重连相关原因暂时屏蔽
				COMMAND_MANAGER.PlayerInputCommand(player, COMMAND_MANAGER.system.PlayerId, "quest finish 35722")
			}
			dbQuest.AddQuest(uint32(questData.QuestId))
			addQuestIdList = append(addQuestIdList, uint32(questData.QuestId))
		}
	}
	if notify {
		ntf := &proto.QuestListUpdateNotify{
			QuestList: make([]*proto.Quest, 0),
		}
		for _, questId := range addQuestIdList {
			pbQuest := g.PacketQuest(player, questId)
			if pbQuest == nil {
				continue
			}
			ntf.QuestList = append(ntf.QuestList, pbQuest)
		}
		g.SendMsg(cmd.QuestListUpdateNotify, player.PlayerId, player.ClientSeq, ntf)
	}
	for _, questId := range addQuestIdList {
		g.StartQuest(player, questId, notify)
	}
}

// StartQuest 开始任务（任务接取后立即调用）
//
// 处理：
//  1. dbQuest.StartQuest 改任务状态为进行中
//  2. ExecQuest(QuestExecTypeStart) 执行 StartExecList（解锁场景物件/创建临时怪等）
//  3. QuestStartTriggerCheck 通知场景内的 Lua trigger（QUEST_START 类型）
//  4. 通知客户端 QuestListUpdateNotify
//  5. 章节起始任务 → 发 ChapterStateNotify(BEGIN) 让客户端展示"开始新章节"动画
//  6. 主动 TriggerQuest 每个 finishCond 类型 → 立即检查是否任务一接就完成
func (g *Game) StartQuest(player *model.Player, questId uint32, notify bool) {
	g.EndlessLoopCheck(EndlessLoopCheckTypeStartQuest)
	dbQuest := player.GetDbQuest()
	dbQuest.StartQuest(questId)

	g.ExecQuest(player, questId, QuestExecTypeStart)
	g.QuestStartTriggerCheck(player, questId)

	if notify {
		g.SendMsg(cmd.QuestListUpdateNotify, player.PlayerId, player.ClientSeq, &proto.QuestListUpdateNotify{
			QuestList: []*proto.Quest{g.PacketQuest(player, questId)},
		})
		for _, chapterDataConfig := range gdconf.GetChapterDataMap() {
			if uint32(chapterDataConfig.StartQuestId) == questId {
				g.SendMsg(cmd.ChapterStateNotify, player.PlayerId, player.ClientSeq, &proto.ChapterStateNotify{
					ChapterState: proto.ChapterState_CHAPTER_STATE_BEGIN,
					ChapterId:    uint32(chapterDataConfig.ChapterId),
				})
			}
		}
	}

	// 任务开始时主动触发检测是否完成
	questDataConfig := gdconf.GetQuestDataById(int32(questId))
	if questDataConfig == nil {
		logger.Error("get quest data config is nil, questId: %v", questId)
		return
	}
	for _, finishCond := range questDataConfig.FinishCondList {
		g.TriggerQuest(player, finishCond.Type, "")
	}
}

// ExecQuest 执行任务的副作用列表（开始/失败/完成时的 hooks）
//
// 三种 ExecType 对应不同的执行列表：
//   - QuestExecTypeStart  → StartExecList（任务开始时执行 如解锁场景物件）
//   - QuestExecTypeFail   → FailExecList（任务失败时执行 如清理临时怪）
//   - QuestExecTypeFinish → ExecList（任务完成时执行 如发奖励/解锁后续）
//
// 已实现的 12+ 种 ExecType（与 CLAUDE.md 任务系统章节一致）：
//   - NOTIFY_GROUP_LUA: 通知场景 Lua（实际是空操作 仅占位）
//   - REFRESH_GROUP_SUITE: 刷新场景小组（如打完守卫怪 宝箱出现）
//   - SET_OPEN_STATE: 设置游戏功能开放状态（解锁角色卡池/秘境/活动等）
//   - UNLOCK_POINT/UNLOCK_AREA: 解锁传送点/区域
//   - CHANGE_AVATAR_ELEMET: 改主角元素（七国元素切换 任务驱动）
//   - SET_IS_FLYABLE/SET_IS_WEATHER_LOCKED/SET_IS_GAME_TIME_LOCKED/SET_IS_TRANSFERABLE: 各种状态锁
//   - SET_GAME_TIME: 设置游戏时间（强制白天/晚上）
//   - ROLLBACK_QUEST: 回滚任务（失败任务恢复到未开始）
//   - ADD_CUR_AVATAR_ENERGY: 给当前角色加满能量（剧情大招过场前用）
func (g *Game) ExecQuest(player *model.Player, questId uint32, questExecType int) {
	g.EndlessLoopCheck(EndlessLoopCheckTypeExecQuest)
	questDataConfig := gdconf.GetQuestDataById(int32(questId))
	if questDataConfig == nil {
		return
	}
	var questExecList []*gdconf.QuestExec = nil
	switch questExecType {
	case QuestExecTypeFinish:
		questExecList = questDataConfig.ExecList
	case QuestExecTypeFail:
		questExecList = questDataConfig.FailExecList
	case QuestExecTypeStart:
		questExecList = questDataConfig.StartExecList
	default:
		return
	}
	for _, questExec := range questExecList {
		switch questExec.Type {
		case constant.QUEST_EXEC_TYPE_NOTIFY_GROUP_LUA:
			// 通知LUA侧
		case constant.QUEST_EXEC_TYPE_REFRESH_GROUP_SUITE:
			// 刷新场景小组
			if len(questExec.Param) != 2 {
				continue
			}
			split := strings.Split(questExec.Param[1], ",")
			if len(split) != 2 {
				continue
			}
			groupId, err := strconv.Atoi(split[0])
			if err != nil {
				continue
			}
			suiteId, err := strconv.Atoi(split[1])
			if err != nil {
				continue
			}
			g.RefreshSceneGroupSuite(player, uint32(groupId), uint8(suiteId))
		case constant.QUEST_EXEC_TYPE_SET_OPEN_STATE:
			// 设置游戏功能开放状态
			if len(questExec.Param) != 2 {
				continue
			}
			key, err := strconv.Atoi(questExec.Param[0])
			if err != nil {
				continue
			}
			value, err := strconv.Atoi(questExec.Param[1])
			if err != nil {
				continue
			}
			g.ChangePlayerOpenState(player.PlayerId, uint32(key), uint32(value))
		case constant.QUEST_EXEC_TYPE_UNLOCK_POINT:
			// 解锁传送点
			if len(questExec.Param) != 2 {
				continue
			}
			sceneId, err := strconv.Atoi(questExec.Param[0])
			if err != nil {
				continue
			}
			pointId, err := strconv.Atoi(questExec.Param[1])
			if err != nil {
				continue
			}
			g.UnlockPlayerScenePoint(player, uint32(sceneId), uint32(pointId))
		case constant.QUEST_EXEC_TYPE_UNLOCK_AREA:
			// 解锁场景区域
			if len(questExec.Param) != 2 {
				continue
			}
			sceneId, err := strconv.Atoi(questExec.Param[0])
			if err != nil {
				continue
			}
			areaId, err := strconv.Atoi(questExec.Param[1])
			if err != nil {
				continue
			}
			g.UnlockPlayerSceneArea(player, uint32(sceneId), uint32(areaId))
		case constant.QUEST_EXEC_TYPE_CHANGE_AVATAR_ELEMET:
			// 改变主角元素类型
			if len(questExec.Param) != 1 {
				continue
			}
			elementType, err := strconv.Atoi(questExec.Param[0])
			if err != nil {
				continue
			}
			dbAvatar := player.GetDbAvatar()
			g.ChangePlayerAvatarSkillDepot(player.PlayerId, dbAvatar.MainCharAvatarId, 0, elementType)
		case constant.QUEST_EXEC_TYPE_SET_IS_FLYABLE:
			// 设置允许飞行状态
			if len(questExec.Param) != 1 {
				continue
			}
			value, err := strconv.Atoi(questExec.Param[0])
			if err != nil {
				continue
			}
			player.PropMap[constant.PLAYER_PROP_IS_FLYABLE] = uint32(value)
			g.SendMsg(cmd.PlayerPropNotify, player.PlayerId, player.ClientSeq, g.PacketPlayerPropNotify(player, constant.PLAYER_PROP_IS_FLYABLE))
		case constant.QUEST_EXEC_TYPE_SET_IS_WEATHER_LOCKED:
			// 设置天气锁定状态
			if len(questExec.Param) != 1 {
				continue
			}
			value, err := strconv.Atoi(questExec.Param[0])
			if err != nil {
				continue
			}
			player.PropMap[constant.PLAYER_PROP_IS_WEATHER_LOCKED] = uint32(value)
			g.SendMsg(cmd.PlayerPropNotify, player.PlayerId, player.ClientSeq, g.PacketPlayerPropNotify(player, constant.PLAYER_PROP_IS_WEATHER_LOCKED))
		case constant.QUEST_EXEC_TYPE_SET_IS_GAME_TIME_LOCKED:
			// 设置游戏时间锁定状态
			if len(questExec.Param) != 1 {
				continue
			}
			value, err := strconv.Atoi(questExec.Param[0])
			if err != nil {
				continue
			}
			player.PropMap[constant.PLAYER_PROP_IS_GAME_TIME_LOCKED] = uint32(value)
			g.SendMsg(cmd.PlayerPropNotify, player.PlayerId, player.ClientSeq, g.PacketPlayerPropNotify(player, constant.PLAYER_PROP_IS_GAME_TIME_LOCKED))
		case constant.QUEST_EXEC_TYPE_SET_IS_TRANSFERABLE:
			// 设置允许传送状态
			if len(questExec.Param) != 1 {
				continue
			}
			value, err := strconv.Atoi(questExec.Param[0])
			if err != nil {
				continue
			}
			player.PropMap[constant.PLAYER_PROP_IS_TRANSFERABLE] = uint32(value)
			g.SendMsg(cmd.PlayerPropNotify, player.PlayerId, player.ClientSeq, g.PacketPlayerPropNotify(player, constant.PLAYER_PROP_IS_TRANSFERABLE))
		case constant.QUEST_EXEC_TYPE_SET_GAME_TIME:
			// 设置游戏时间到某时刻（剧情常用 跳到下一个目标时刻 保持累计语义）
			// 参数格式："HH" 或 "HH.MM"（小数点分隔 分钟前导 0 省略 如 "17.4"=17:04 "17.45"=17:45）
			if len(questExec.Param) != 1 {
				continue
			}
			var hour, minute int
			var err error
			parts := strings.Split(questExec.Param[0], ".")
			hour, err = strconv.Atoi(parts[0])
			if err != nil {
				continue
			}
			if len(parts) == 2 {
				minute, err = strconv.Atoi(parts[1])
				if err != nil {
					continue
				}
			}
			world := WORLD_MANAGER.GetWorldById(player.WorldId)
			if world == nil {
				logger.Error("get world is nil, worldId: %v, uid: %v", player.WorldId, player.PlayerId)
				continue
			}
			scene := world.GetSceneById(player.GetSceneId())
			if scene == nil {
				logger.Error("scene is nil, sceneId: %v, uid: %v", player.GetSceneId(), player.PlayerId)
				continue
			}
			// 累计秒数 mod 86400 得当天已过秒数 跳到下一个目标时刻（已过则跳到明天）
			curSec := world.GetGameTime()
			secOfDay := curSec % 86400
			targetSecOfDay := uint32(hour*3600 + minute*60)
			var newSec uint32
			if targetSecOfDay >= secOfDay {
				newSec = curSec - secOfDay + targetSecOfDay
			} else {
				newSec = curSec - secOfDay + targetSecOfDay + 86400
			}
			g.ChangeGameTime(world, newSec)
		case constant.QUEST_EXEC_TYPE_ROLLBACK_QUEST:
			// 回滚任务
			if len(questExec.Param) != 1 {
				continue
			}
			rollbackQuestId, err := strconv.Atoi(questExec.Param[0])
			if err != nil {
				continue
			}
			dbQuest := player.GetDbQuest()
			rollbackQuest := dbQuest.GetQuestById(uint32(rollbackQuestId))
			if rollbackQuest.State != constant.QUEST_STATE_FAILED {
				continue
			}
			rollbackQuest.State = constant.QUEST_STATE_UNSTARTED
			g.StartQuest(player, rollbackQuest.QuestId, true)
		case constant.QUEST_EXEC_TYPE_ADD_CUR_AVATAR_ENERGY:
			// 添加当前角色元素能量
			world := WORLD_MANAGER.GetWorldById(player.WorldId)
			if world == nil {
				logger.Error("get world is nil, worldId: %v, uid: %v", player.WorldId, player.PlayerId)
				continue
			}
			activeAvatarId := world.GetPlayerActiveAvatarId(player)
			g.AddPlayerAvatarEnergy(player.PlayerId, activeAvatarId, 0.0, true)
		default:
			logger.Error("not support quest exec type: %v, questId: %v, uid: %v", questExec.Type, questId, player.PlayerId)
		}
	}
}

// TriggerQuest 触发任务推进（核心入口 玩家行为 + Lua trigger 都最终走这里）
//
// 调用方式：TriggerQuest(player, COND_TYPE_KILL_MONSTER, "", monsterId)
//   - cond: 条件类型（QUEST_FINISH_COND_TYPE_*）
//   - complexParam: 复杂字符串参数（如时间范围 "8,18"）
//   - param: 整数参数列表（最多 2 个 用 matchParamEqual 比对）
//
// 处理：
//  1. 遍历玩家所有进行中任务 检查每个 quest 的 FailCondList + FinishCondList
//  2. 条件匹配 → AddQuestFinishCount（计数器+1） 或 FailQuest
//  3. CheckQuestFinish 判断任务是否完成（计数器到达阈值）
//  4. 通知客户端 + 触发 FinishQuest/FailQuest 钩子
//  5. 链式触发 AcceptQuest（完成任务可能解锁后续任务）+ TriggerOpenState（解锁功能）
//
// 已实现的 14+ 种 finishCond.Type 见 case 列表（COMPLETE_TALK / KILL_MONSTER / OBTAIN_ITEM / 进场景 / 时间窗口 等）
// 注释里 line 653 的吐槽体现作者对配置不确定性的无奈
func (g *Game) TriggerQuest(player *model.Player, cond int32, complexParam string, param ...int32) {
	g.EndlessLoopCheck(EndlessLoopCheckTypeTriggerQuest)
	dbQuest := player.GetDbQuest()
	updateQuestIdMap := make(map[uint32]struct{})
	for _, quest := range dbQuest.GetQuestMap() {
		if quest.State != constant.QUEST_STATE_UNFINISHED {
			continue
		}
		questDataConfig := gdconf.GetQuestDataById(int32(quest.QuestId))
		if questDataConfig == nil {
			continue
		}
		for _, failCond := range questDataConfig.FailCondList {
			if failCond.Type != cond {
				continue
			}
			switch cond {
			case constant.QUEST_FINISH_COND_TYPE_LUA_NOTIFY:
				// LUA侧通知 复杂参数
				if failCond.ComplexParam != complexParam {
					continue
				}
				dbQuest.FailQuest(quest.QuestId)
			case constant.QUEST_FINISH_COND_TYPE_COMPLETE_TALK:
				// 与NPC对话 参数1:对话id
				ok := matchParamEqual(failCond.Param, param, 1)
				if !ok {
					continue
				}
				dbQuest.FailQuest(quest.QuestId)
			default:
				logger.Error("not support quest fail cond type: %v, questId: %v, uid: %v", cond, quest.QuestId, player.PlayerId)
			}
			updateQuestIdMap[quest.QuestId] = struct{}{}
		}
		for index, finishCond := range questDataConfig.FinishCondList {
			if finishCond.Type != cond {
				continue
			}
			switch cond {
			case constant.QUEST_FINISH_COND_TYPE_FINISH_PLOT:
				// 完成剧情 参数1:剧情id
				ok := matchParamEqual(finishCond.Param, param, 1)
				if !ok {
					continue
				}
				dbQuest.AddQuestFinishCount(quest.QuestId, index, 1)
			case constant.QUEST_FINISH_COND_TYPE_TRIGGER_FIRE:
				// 场景触发器跳了 参数1:触发器id
				ok := matchParamEqual(finishCond.Param, param, 1)
				if !ok {
					continue
				}
				dbQuest.AddQuestFinishCount(quest.QuestId, index, 1)
			case constant.QUEST_FINISH_COND_TYPE_UNLOCK_TRANS_POINT:
				// 解锁传送锚点 参数1:场景id 参数2:传送锚点id
				dbWorld := player.GetDbWorld()
				dbScene := dbWorld.GetSceneById(uint32(finishCond.Param[0]))
				if dbScene == nil {
					continue
				}
				unlock := dbScene.CheckPointUnlock(uint32(finishCond.Param[1]))
				if !unlock {
					continue
				}
				dbQuest.AddQuestFinishCount(quest.QuestId, index, 1)
			case constant.QUEST_FINISH_COND_TYPE_COMPLETE_TALK:
				// 与NPC对话 参数1:对话id
				ok := matchParamEqual(finishCond.Param, param, 1)
				if !ok {
					continue
				}
				dbQuest.AddQuestFinishCount(quest.QuestId, index, 1)
			case constant.QUEST_FINISH_COND_TYPE_LUA_NOTIFY:
				// LUA侧通知 复杂参数
				if finishCond.ComplexParam != complexParam {
					continue
				}
				dbQuest.AddQuestFinishCount(quest.QuestId, index, 1)
			case constant.QUEST_FINISH_COND_TYPE_SKILL:
				// 使用技能 参数1:技能id
				ok := matchParamEqual(finishCond.Param, param, 1)
				if !ok {
					continue
				}
				dbQuest.AddQuestFinishCount(quest.QuestId, index, 1)
			case constant.QUEST_FINISH_COND_TYPE_OBTAIN_ITEM:
				// 获得道具 参数1:道具id 调用方在 param[1] 传入实际获得件数
				ok := matchParamEqual(finishCond.Param, param, 1)
				if !ok {
					continue
				}
				addCount := uint32(1)
				if len(param) >= 2 && param[1] > 0 {
					addCount = uint32(param[1])
				}
				dbQuest.AddQuestFinishCount(quest.QuestId, index, addCount)
			case constant.QUEST_FINISH_COND_TYPE_UNLOCK_AREA:
				// 解锁场景区域 参数1:场景id 参数2:场景区域id
				dbWorld := player.GetDbWorld()
				dbScene := dbWorld.GetSceneById(uint32(finishCond.Param[0]))
				if dbScene == nil {
					continue
				}
				unlock := dbScene.CheckAreaUnlock(uint32(finishCond.Param[1]))
				if !unlock {
					continue
				}
				dbQuest.AddQuestFinishCount(quest.QuestId, index, 1)
			case constant.QUEST_FINISH_COND_TYPE_ADD_QUEST_PROGRESS:
				// TODO 这你妈到底是加父任务的进度还是子任务的进度
				continue
			case constant.QUEST_FINISH_COND_TYPE_ENTER_DUNGEON:
				// 进入地牢 参数1:地牢id 参数2:传送锚点id
				ok := matchParamEqual(finishCond.Param, param, 2)
				if !ok {
					continue
				}
				dbQuest.AddQuestFinishCount(quest.QuestId, index, 1)
			case constant.QUEST_FINISH_COND_TYPE_ENTER_MY_WORLD:
				// 进入世界 参数1:场景id
				ok := matchParamEqual(finishCond.Param, param, 1)
				if !ok {
					continue
				}
				dbQuest.AddQuestFinishCount(quest.QuestId, index, 1)
			case constant.QUEST_FINISH_COND_TYPE_ENTER_ROOM:
				// 进入房间 参数1:场景id
				ok := matchParamEqual(finishCond.Param, param, 1)
				if !ok {
					continue
				}
				dbQuest.AddQuestFinishCount(quest.QuestId, index, 1)
			case constant.QUEST_FINISH_COND_TYPE_GAME_TIME_TICK:
				// 游戏时间
				split := strings.Split(finishCond.ComplexParam, ",")
				if len(split) != 2 {
					continue
				}
				split0, err := strconv.Atoi(split[0])
				if err != nil {
					continue
				}
				split1, err := strconv.Atoi(split[1])
				if err != nil {
					continue
				}
				startGameTimeHour := uint32(split0)
				endGameTimeHour := uint32(split1)
				world := WORLD_MANAGER.GetWorldById(player.WorldId)
				if world == nil {
					continue
				}
				gameTime := world.GetGameTime()
				// game_time 是累计秒数 mod 86400 算当天小时
				gameTimeHour := (gameTime % 86400) / 3600
				// 跨午夜区间如 "22,6" 表示 22 点至次日 6 点
				var inRange bool
				if startGameTimeHour <= endGameTimeHour {
					inRange = gameTimeHour >= startGameTimeHour && gameTimeHour <= endGameTimeHour
				} else {
					inRange = gameTimeHour >= startGameTimeHour || gameTimeHour <= endGameTimeHour
				}
				if !inRange {
					continue
				}
				dbQuest.AddQuestFinishCount(quest.QuestId, index, 1)
			default:
				logger.Error("not support quest finish cond type: %v, questId: %v, uid: %v", cond, quest.QuestId, player.PlayerId)
			}
			updateQuestIdMap[quest.QuestId] = struct{}{}
		}
		dbQuest.CheckQuestFinish(quest.QuestId)
	}
	if len(updateQuestIdMap) > 0 {
		// 更新客户端任务列表
		questList := make([]*proto.Quest, 0)
		for questId := range updateQuestIdMap {
			pbQuest := g.PacketQuest(player, questId)
			if pbQuest == nil {
				continue
			}
			questList = append(questList, pbQuest)
		}
		g.SendMsg(cmd.QuestListUpdateNotify, player.PlayerId, player.ClientSeq, &proto.QuestListUpdateNotify{
			QuestList: questList,
		})
		// 任务事件
		for questId := range updateQuestIdMap {
			quest := dbQuest.GetQuestById(questId)
			if quest.State == constant.QUEST_STATE_FINISHED {
				g.FinishQuest(player, quest.QuestId)
			} else if quest.State == constant.QUEST_STATE_FAILED {
				g.FailQuest(player, quest.QuestId)
			}
		}
		g.AcceptQuest(player, true)
		g.TriggerOpenState(player.PlayerId)
	}
}

// FinishQuest 完成任务
func (g *Game) FinishQuest(player *model.Player, questId uint32) {
	// 任务完成执行
	g.ExecQuest(player, questId, QuestExecTypeFinish)
	// 任务完成发奖
	questDataConfig := gdconf.GetQuestDataById(int32(questId))
	if questDataConfig == nil {
		return
	}
	if len(questDataConfig.ItemIdList) != 0 {
		for index, itemId := range questDataConfig.ItemIdList {
			questItem := []*ChangeItem{{ItemId: uint32(itemId), ChangeCount: uint32(questDataConfig.ItemCountList[index])}}
			g.AddPlayerItem(player.PlayerId, questItem, proto.ActionReasonType_ACTION_REASON_QUEST_ITEM)
		}
	}
	dbQuest := player.GetDbQuest()
	parentQuest := dbQuest.GetParentQuestById(uint32(questDataConfig.ParentQuestId))
	if parentQuest == nil {
		return
	}
	if parentQuest.State == constant.PARENT_QUEST_STATE_FINISHED {
		// 父任务完成发奖
		mainQuestDataConfig := gdconf.GetMainQuestDataById(questDataConfig.ParentQuestId)
		if mainQuestDataConfig == nil {
			return
		}
		for _, rewardId := range mainQuestDataConfig.RewardIdList {
			g.RewardItem(player.PlayerId, uint32(rewardId), proto.ActionReasonType_ACTION_REASON_QUEST_ITEM)
		}
		// 更新客户端父任务
		g.SendMsg(cmd.FinishedParentQuestUpdateNotify, player.PlayerId, player.ClientSeq, &proto.FinishedParentQuestUpdateNotify{
			ParentQuestList: g.PacketParentQuestList(player, []uint32{parentQuest.ParentQuestId}),
		})
	}
	for _, chapterDataConfig := range gdconf.GetChapterDataMap() {
		if uint32(chapterDataConfig.EndQuestId) == questId {
			g.SendMsg(cmd.ChapterStateNotify, player.PlayerId, player.ClientSeq, &proto.ChapterStateNotify{
				ChapterState: proto.ChapterState_CHAPTER_STATE_END,
				ChapterId:    uint32(chapterDataConfig.ChapterId),
			})
		}
	}
}

// FailQuest 失败任务
func (g *Game) FailQuest(player *model.Player, questId uint32) {
	// 任务失败执行
	g.ExecQuest(player, questId, QuestExecTypeFail)
}

/************************************************** 打包封装 **************************************************/

// PacketQuest 打包一个任务
func (g *Game) PacketQuest(player *model.Player, questId uint32) *proto.Quest {
	dbQuest := player.GetDbQuest()
	questDataConfig := gdconf.GetQuestDataById(int32(questId))
	if questDataConfig == nil {
		logger.Error("get quest data config is nil, questId: %v", questId)
		return nil
	}
	quest := dbQuest.GetQuestById(questId)
	if quest == nil {
		logger.Error("get quest is nil, questId: %v", questId)
		return nil
	}
	pbQuest := &proto.Quest{
		QuestId:            quest.QuestId,
		State:              uint32(quest.State),
		StartTime:          quest.StartTime,
		ParentQuestId:      uint32(questDataConfig.ParentQuestId),
		StartGameTime:      0,
		AcceptTime:         quest.AcceptTime,
		FinishProgressList: quest.FinishCountList,
	}
	return pbQuest
}

// PacketQuestListNotify 打包任务列表通知
func (g *Game) PacketQuestListNotify(player *model.Player) *proto.QuestListNotify {
	ntf := &proto.QuestListNotify{
		QuestList: make([]*proto.Quest, 0),
	}
	dbQuest := player.GetDbQuest()
	for _, quest := range dbQuest.GetQuestMap() {
		pbQuest := g.PacketQuest(player, quest.QuestId)
		if pbQuest == nil {
			continue
		}
		ntf.QuestList = append(ntf.QuestList, pbQuest)
	}
	return ntf
}

// PacketFinishedParentQuestNotify 打包已完成父任务列表通知
func (g *Game) PacketFinishedParentQuestNotify(player *model.Player) *proto.FinishedParentQuestNotify {
	dbQuest := player.GetDbQuest()
	parentQuestIdList := make([]uint32, len(dbQuest.GetParentQuestMap()))
	for _, parentQuest := range dbQuest.GetParentQuestMap() {
		if parentQuest.State != constant.PARENT_QUEST_STATE_FINISHED {
			continue
		}
		parentQuestIdList = append(parentQuestIdList, parentQuest.ParentQuestId)
	}
	ntf := &proto.FinishedParentQuestNotify{
		ParentQuestList: g.PacketParentQuestList(player, parentQuestIdList),
	}
	return ntf
}

func (g *Game) PacketParentQuestList(player *model.Player, parentQuestIdList []uint32) []*proto.ParentQuest {
	parentQuestList := make([]*proto.ParentQuest, 0)
	dbQuest := player.GetDbQuest()
	for _, parentQuestId := range parentQuestIdList {
		parentQuest := dbQuest.GetParentQuestById(parentQuestId)
		if parentQuest == nil {
			continue
		}
		childQuestList := make([]*proto.ChildQuest, 0)
		questDataMap := gdconf.GetQuestDataMapByParentQuestId(int32(parentQuestId))
		for _, questDataConfig := range questDataMap {
			quest := dbQuest.GetQuestById(uint32(questDataConfig.QuestId))
			if quest == nil {
				continue
			}
			childQuestList = append(childQuestList, &proto.ChildQuest{
				State:   uint32(quest.State),
				QuestId: quest.QuestId,
			})
		}
		parentQuestList = append(parentQuestList, &proto.ParentQuest{
			ParentQuestId:    parentQuest.ParentQuestId,
			ParentQuestState: uint32(parentQuest.State),
			IsFinished:       parentQuest.State == constant.PARENT_QUEST_STATE_FINISHED,
			ChildQuestList:   childQuestList,
			QuestVar:         parentQuest.QuestVar[:],
		})
	}
	return parentQuestList
}
