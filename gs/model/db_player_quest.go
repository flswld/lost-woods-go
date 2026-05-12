package model

// 玩家任务子模型 - 任务进度持久化
//
// **任务两层结构**（与官服一致）：
//   - ParentQuest（父任务/主线任务）：一条主线（如"风起地"）由多个子任务串起来
//     · 父任务有自己的 State（进行中/完成）+ QuestVar（10 个 int32 变量供 Lua 用）
//   - Quest（子任务/具体步骤）：玩家当前实际推进的步骤（"前往蒙德城"/"对话琴"等）
//     · 同时只有一个 Quest 在 进行中 状态 完成后自动激活下一个子任务
//
// **State 状态值**（constant.QUEST_STATE_*）：
//   NONE=0 未启用 / UNSTARTED=1 已接取未开始 / UNFINISHED=2 进行中 / FINISHED=3 完成 / FAILED=4 失败
//   完成的任务会保留在 QuestMap 客户端用来判断历史完成情况
//
// **FinishCountList**：与 questData.FinishCondList 顺序一致 记录每个条件的当前进度计数
//   如"杀 10 只丘丘人"任务 FinishCountList[0] 从 0 累加到 10
//   全部条件达到 questData 配置的 Count 后调 CheckQuestFinish 触发完成
//
// **QuestVar 用途**（父任务 10 个 int32 槽位）：
//   - 由 Lua 通过 ScriptLib.SetQuestVar / GetQuestVar 操作（如果实现的话 未确认）
//   - 用于跨子任务保存状态（如"玩家是否选了 A 选项"影响后续分支）
//
// 详见 player_quest.go AcceptQuest/StartQuest/TriggerQuest 的处理逻辑

import (
	"time"

	"hk4e/common/constant"
	"hk4e/gdconf"

	"github.com/flswld/halo/logger"
)

// DbQuest 玩家任务模块根模型
type DbQuest struct {
	QuestMap       map[uint32]*Quest       // 子任务列表 key:questId
	ParentQuestMap map[uint32]*ParentQuest // 父任务列表 key:parentQuestId
}

// Quest 子任务进度
type Quest struct {
	QuestId         uint32   // 子任务 id（在 gdconf.QuestData 中查配置）
	State           uint8    // QUEST_STATE_*
	AcceptTime      uint32   // 接取时间（玩家接到任务的时间戳）
	StartTime       uint32   // 开始执行时间（任务进入活跃状态的时间）
	FinishCountList []uint32 // 完成条件进度计数 索引与 questData.FinishCondList 一一对应
}

// ParentQuest 父任务/主线进度
type ParentQuest struct {
	ParentQuestId uint32    // 父任务 id（一条主线 含多个子任务）
	State         uint8     // QUEST_STATE_*
	QuestVar      [10]int32 // 任务变量（10 个 int32 槽位 跨子任务保存状态 给 Lua 用）
}

func (p *Player) GetDbQuest() *DbQuest {
	if p.DbQuest == nil {
		p.DbQuest = new(DbQuest)
	}
	if p.DbQuest.QuestMap == nil {
		p.DbQuest.QuestMap = make(map[uint32]*Quest)
	}
	if p.DbQuest.ParentQuestMap == nil {
		p.DbQuest.ParentQuestMap = make(map[uint32]*ParentQuest)
	}
	return p.DbQuest
}

// GetQuestMap 获取全部任务
func (q *DbQuest) GetQuestMap() map[uint32]*Quest {
	return q.QuestMap
}

// GetQuestById 获取一个任务
func (q *DbQuest) GetQuestById(questId uint32) *Quest {
	return q.QuestMap[questId]
}

// AddQuest 添加一个任务
func (q *DbQuest) AddQuest(questId uint32) {
	_, exist := q.QuestMap[questId]
	if exist {
		logger.Error("quest is already exist, questId: %v", questId)
		return
	}
	questDataConfig := gdconf.GetQuestDataById(int32(questId))
	if questDataConfig == nil {
		logger.Error("get quest data config is nil, questId: %v", questId)
		return
	}
	q.QuestMap[questId] = &Quest{
		QuestId:         uint32(questDataConfig.QuestId),
		State:           constant.QUEST_STATE_UNSTARTED,
		AcceptTime:      uint32(time.Now().Unix()),
		StartTime:       0,
		FinishCountList: make([]uint32, len(questDataConfig.FinishCondList)),
	}
	q.AddParentQuest(uint32(questDataConfig.ParentQuestId))
}

// StartQuest 开始执行一个任务
func (q *DbQuest) StartQuest(questId uint32) {
	quest, exist := q.QuestMap[questId]
	if !exist {
		logger.Error("get quest is nil, questId: %v", questId)
		return
	}
	if quest.State != constant.QUEST_STATE_UNSTARTED {
		logger.Error("invalid quest state, questId: %v, state: %v", questId, quest.State)
		return
	}
	quest.State = constant.QUEST_STATE_UNFINISHED
	quest.StartTime = uint32(time.Now().Unix())
}

// DeleteQuest 删除一个任务
func (q *DbQuest) DeleteQuest(questId uint32) {
	_, exist := q.QuestMap[questId]
	if !exist {
		logger.Error("quest is not exist, questId: %v", questId)
		return
	}
	delete(q.QuestMap, questId)
}

// AddQuestFinishCount 添加一个任务的完成进度（按 count 件数累加）
func (q *DbQuest) AddQuestFinishCount(questId uint32, index int, count uint32) {
	quest, exist := q.QuestMap[questId]
	if !exist {
		logger.Error("get quest is nil, questId: %v", questId)
		return
	}
	if quest.State != constant.QUEST_STATE_UNFINISHED {
		return
	}
	if index >= len(quest.FinishCountList) {
		logger.Error("invalid quest cond index, questId: %v, index: %v", questId, index)
		return
	}
	quest.FinishCountList[index] += count
}

// CheckQuestFinish 检查任务是否完成
func (q *DbQuest) CheckQuestFinish(questId uint32) {
	quest, exist := q.QuestMap[questId]
	if !exist {
		logger.Error("get quest is nil, questId: %v", questId)
		return
	}
	if quest.State != constant.QUEST_STATE_UNFINISHED {
		return
	}
	questDataConfig := gdconf.GetQuestDataById(int32(questId))
	if questDataConfig == nil {
		logger.Error("get quest data config is nil, questId: %v", questId)
		return
	}
	resultList := make([]bool, 0)
	for index, finishCond := range questDataConfig.FinishCondList {
		result := false
		finishCount := finishCond.Count
		if finishCount == 0 {
			finishCount = 1
		}
		if quest.FinishCountList[index] >= uint32(finishCount) {
			result = true
		}
		resultList = append(resultList, result)
	}
	finish := false
	switch questDataConfig.FinishCondCompose {
	case constant.QUEST_LOGIC_TYPE_NONE:
		fallthrough
	case constant.QUEST_LOGIC_TYPE_AND:
		finish = true
		for _, result := range resultList {
			if !result {
				finish = false
				break
			}
		}
	case constant.QUEST_LOGIC_TYPE_OR:
		finish = false
		for _, result := range resultList {
			if result {
				finish = true
				break
			}
		}
	case constant.QUEST_LOGIC_TYPE_A_AND_ETCOR:
		if len(resultList) < 2 {
			finish = false
			break
		}
		finishA := resultList[0]
		finishEtc := false
		for _, result := range resultList[1:] {
			if result {
				finishEtc = true
				break
			}
		}
		finish = finishA && finishEtc
	case constant.QUEST_LOGIC_TYPE_A_AND_B_AND_ETCOR:
		if len(resultList) < 3 {
			finish = false
			break
		}
		finishA := resultList[0]
		finishB := resultList[1]
		finishEtc := false
		for _, result := range resultList[2:] {
			if result {
				finishEtc = true
				break
			}
		}
		finish = finishA && finishB && finishEtc
	case constant.QUEST_LOGIC_TYPE_A_OR_ETCAND:
		if len(resultList) < 2 {
			finish = false
			break
		}
		finishA := resultList[0]
		finishEtc := true
		for _, result := range resultList[1:] {
			if !result {
				finishEtc = false
				break
			}
		}
		finish = finishA || finishEtc
	case constant.QUEST_LOGIC_TYPE_A_OR_B_OR_ETCAND:
		if len(resultList) < 3 {
			finish = false
			break
		}
		finishA := resultList[0]
		finishB := resultList[1]
		finishEtc := true
		for _, result := range resultList[2:] {
			if !result {
				finishEtc = false
				break
			}
		}
		finish = finishA || finishB || finishEtc
	default:
		logger.Error("not support quest finish cond logic type: %v, questId: %v", questDataConfig.FinishCondCompose, questId)
	}
	if finish {
		quest.State = constant.QUEST_STATE_FINISHED
		q.CheckParentQuestFinish(uint32(questDataConfig.ParentQuestId))
	}
}

// ForceFinishQuest 强制完成一个任务
func (q *DbQuest) ForceFinishQuest(questId uint32) {
	quest, exist := q.QuestMap[questId]
	if !exist {
		logger.Error("get quest is nil, questId: %v", questId)
		return
	}
	quest.State = constant.QUEST_STATE_FINISHED
	questDataConfig := gdconf.GetQuestDataById(int32(questId))
	if questDataConfig == nil {
		logger.Error("get quest data config is nil, questId: %v", questId)
		return
	}
	q.CheckParentQuestFinish(uint32(questDataConfig.ParentQuestId))
}

// FailQuest 失败一个任务
func (q *DbQuest) FailQuest(questId uint32) {
	quest, exist := q.QuestMap[questId]
	if !exist {
		logger.Error("get quest is nil, questId: %v", questId)
		return
	}
	if quest.State != constant.QUEST_STATE_UNFINISHED {
		return
	}
	quest.State = constant.QUEST_STATE_FAILED
	questDataConfig := gdconf.GetQuestDataById(int32(questId))
	if questDataConfig == nil {
		logger.Error("get quest data config is nil, questId: %v", questId)
		return
	}
	quest.FinishCountList = make([]uint32, len(questDataConfig.FinishCondList))
}

// GetParentQuestMap 获取全部父任务
func (q *DbQuest) GetParentQuestMap() map[uint32]*ParentQuest {
	return q.ParentQuestMap
}

// GetParentQuestById 获取一个父任务
func (q *DbQuest) GetParentQuestById(parentQuestId uint32) *ParentQuest {
	return q.ParentQuestMap[parentQuestId]
}

// AddParentQuest 添加一个父任务
func (q *DbQuest) AddParentQuest(parentQuestId uint32) {
	_, exist := q.ParentQuestMap[parentQuestId]
	if exist {
		return
	}
	q.ParentQuestMap[parentQuestId] = &ParentQuest{
		ParentQuestId: parentQuestId,
		State:         constant.PARENT_QUEST_STATE_NONE,
		QuestVar:      [10]int32{0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
	}
}

// CheckParentQuestFinish 检查父任务是否完成
func (q *DbQuest) CheckParentQuestFinish(parentQuestId uint32) {
	parentQuest, exist := q.ParentQuestMap[parentQuestId]
	if !exist {
		logger.Error("get parent quest is nil, parentQuestId: %v", parentQuestId)
		return
	}
	finish := true
	questDataMap := gdconf.GetQuestDataMapByParentQuestId(int32(parentQuestId))
	for _, questData := range questDataMap {
		quest, exist := q.QuestMap[uint32(questData.QuestId)]
		if !exist {
			finish = false
			break
		}
		if quest.State != constant.QUEST_STATE_FINISHED {
			finish = false
			break
		}
	}
	if finish {
		parentQuest.State = constant.PARENT_QUEST_STATE_FINISHED
	}
}

// ForceFinishParentQuest 强制完成一个父任务
func (q *DbQuest) ForceFinishParentQuest(parentQuestId uint32) {
	parentQuest, exist := q.ParentQuestMap[parentQuestId]
	if !exist {
		logger.Error("get parent quest is nil, parentQuestId: %v", parentQuestId)
		return
	}
	parentQuest.State = constant.PARENT_QUEST_STATE_FINISHED
}
