package handle

import (
	"math"
	"time"

	"hk4e/common/constant"
	"hk4e/gdconf"
	"hk4e/protocol/proto"

	"github.com/flswld/halo/logger"
	pb "google.golang.org/protobuf/proto"
)

// Multi 反作弊核心 - 服务端被动检测
//
// 设计：multi 服订阅 GS 广播的关键事件 在独立服务上做行为分析
//   - 不影响 GS 主循环性能（GS 单线程时间敏感 不能阻塞做反作弊）
//   - 多 GS 共享 multi（一个 multi 服务多个 GS 但项目当前部署是 1:1）
//
// 检测项：
//   1. 玩家瞬移：1 秒内移动 > JumpDistance(500m) 且不在传送点附近 → 判定瞬移
//   2. 玩家超速：10 个采样点平均速度 > MaxMoveSpeed(100m/s) → 判定超速（飞天/穿墙）
//   3. 攻击频率：单实体被攻击 > AttackCountLimitEntitySec(10次/秒) → 判定连发外挂
//
// **注意**：
//   - 检测仅记录日志 KickCheatPlayer=false 不主动踢人（防止误杀）
//   - 仅检测 sceneId==3 这一个场景（应该是某个测试场景的特殊判断）
//   - 项目作者把反作弊做成"被动监测" 没做服务端主动校验（详见 CLAUDE.md "Ability 系统现状"）

const (
	MoveVectorCacheNum        = 10    // 移动采样数（10 个点算平均速度）
	MaxMoveSpeed              = 100.0 // 最大允许平均速度（米/秒）
	JumpDistance              = 500.0 // 单次瞬移距离阈值（米）超过判定瞬移
	PointDistance             = 10.0  // 传送点附近判定距离（米）
	AttackCountLimitEntitySec = 10    // 单实体每秒最多被攻击次数
	KickCheatPlayer           = false // 是否真的踢作弊玩家（默认 false 仅记录日志）
)

type MoveVector struct {
	pos  *proto.Vector
	time int64
}

type AttackEntity struct {
	attackStartTime uint64
	attackCount     uint32
}

type AnticheatContext struct {
	sceneId         uint32
	moveVectorList  []*MoveVector
	attackEntityMap map[uint32]*AttackEntity
}

// Move 移动检测 - 检测玩家是否瞬移
//
// 处理：
//  1. 与上次移动间隔 < 1 秒 → 跳过（采样间隔太短不准）
//  2. 距离 > JumpDistance(500m)：可能瞬移
//     · 检查是否在某传送点附近（PointDistance=10m）
//     · 是 → 视为合法传送 重置移动历史
//     · 否 → 返回 false（瞬移作弊判定）
//  3. 加入移动历史 保留最近 10 个采样点
func (a *AnticheatContext) Move(pos *proto.Vector) bool {
	now := time.Now().UnixMilli()
	if len(a.moveVectorList) > 0 {
		lastMoveVector := a.moveVectorList[len(a.moveVectorList)-1]
		if now-lastMoveVector.time < 1000 {
			return true
		}
		distance := GetDistance(pos, lastMoveVector.pos)
		if distance > JumpDistance {
			// 瞬时变化太大 判断是否为传送
			scenePointMap := gdconf.GetScenePointMapBySceneId(int32(a.sceneId))
			if scenePointMap == nil {
				return true
			}
			isJump := true
			for _, pointData := range scenePointMap {
				d := GetDistance(pos, &proto.Vector{
					X: float32(pointData.TranPos.X),
					Y: float32(pointData.TranPos.Y),
					Z: float32(pointData.TranPos.Z),
				})
				if d < PointDistance {
					isJump = false
					break
				}
			}
			if isJump {
				return false
			} else {
				a.moveVectorList = make([]*MoveVector, 0)
			}
		}
	}
	a.moveVectorList = append(a.moveVectorList, &MoveVector{
		pos:  pos,
		time: now,
	})
	if len(a.moveVectorList) > MoveVectorCacheNum {
		a.moveVectorList = a.moveVectorList[len(a.moveVectorList)-MoveVectorCacheNum:]
	}
	return true
}

// GetMoveSpeed 计算最近 10 个采样点的平均移动速度
// 算法：相邻两点的距离 / 时间差 = 瞬时速度 求所有相邻对的平均
// 不足 10 个点返回 0（数据不够 不判定）
func (a *AnticheatContext) GetMoveSpeed() float32 {
	avgMoveSpeed := float32(0.0)
	if len(a.moveVectorList) < MoveVectorCacheNum {
		return avgMoveSpeed
	}
	for index := range a.moveVectorList {
		if index+1 >= len(a.moveVectorList) {
			break
		}
		nextMoveVector := a.moveVectorList[index+1]
		beforeMoveVector := a.moveVectorList[index]
		dx := GetDistance(nextMoveVector.pos, beforeMoveVector.pos)
		dt := float32(nextMoveVector.time-beforeMoveVector.time) / 1000.0
		avgMoveSpeed += dx / dt
	}
	avgMoveSpeed /= float32(len(a.moveVectorList))
	return avgMoveSpeed
}

// Attack 攻击频率检测 - 检测连发外挂
//
// 算法：滑动窗口计数
//   - 每个被攻击实体维护 attackStartTime + attackCount
//   - 1 秒内累计 > 10 次 → 返回 false（频率超限）
//   - 1 秒后重置计数器（新窗口）
//
// 检测连发外挂的原理：人类合法攻击节奏不会超过 10 次/秒
func (a *AnticheatContext) Attack(defEntityId uint32) bool {
	now := uint64(time.Now().UnixMilli())
	attackEntity, exist := a.attackEntityMap[defEntityId]
	if !exist {
		attackEntity = &AttackEntity{
			attackStartTime: now,
			attackCount:     0,
		}
		a.attackEntityMap[defEntityId] = attackEntity
	}
	attackEntity.attackCount++
	if attackEntity.attackCount > AttackCountLimitEntitySec {
		if now-attackEntity.attackStartTime < 1000 {
			return false
		} else {
			attackEntity.attackStartTime = now
			attackEntity.attackCount = 0
		}
	}
	return true
}

func NewAnticheatContext() *AnticheatContext {
	r := &AnticheatContext{
		sceneId:         0,
		moveVectorList:  make([]*MoveVector, 0),
		attackEntityMap: make(map[uint32]*AttackEntity),
	}
	return r
}

func (h *Handle) AddPlayerAcCtx(userId uint32) {
	h.playerAcCtxMap[userId] = NewAnticheatContext()
}

func (h *Handle) DelPlayerAcCtx(userId uint32) {
	delete(h.playerAcCtxMap, userId)
}

func (h *Handle) GetPlayerAcCtx(userId uint32) *AnticheatContext {
	return h.playerAcCtxMap[userId]
}

// CombatInvocationsNotify 战斗事件订阅入口（GS 广播 multi 收 副本检测）
//
// 拆 InvokeList 按 ArgumentType 分发：
//   - ENTITY_MOVE: 玩家角色实体移动 → 走 Move + GetMoveSpeed 检测
//     · 仅角色实体（GetEntityType=AVATAR）才检测
//     · 仅 sceneId=3 才检测（特定测试场景）
//   - COMBAT_EVT_BEING_HIT: 实体被击中 → 攻击者方向不知 仅按"被攻击实体"做频率检测
//     · 仅怪物（GetEntityType=MONSTER）作为目标才检测
//
// 检测命中后：调 KickPlayer 通知 GS 踢人（实际行为受 KickCheatPlayer 配置控制）
func (h *Handle) CombatInvocationsNotify(userId uint32, gateAppId string, payloadMsg pb.Message) {
	req := payloadMsg.(*proto.CombatInvocationsNotify)
	ctx := h.GetPlayerAcCtx(userId)
	if ctx == nil {
		logger.Error("get player anticheat context is nil, uid: %v", userId)
		return
	}
	for _, entry := range req.InvokeList {
		switch entry.ArgumentType {
		case proto.CombatTypeArgument_ENTITY_MOVE:
			entityMoveInfo := new(proto.EntityMoveInfo)
			err := pb.Unmarshal(entry.CombatData, entityMoveInfo)
			if err != nil {
				logger.Error("parse EntityMoveInfo error: %v, uid: %v", err, userId)
				continue
			}
			if GetEntityType(entityMoveInfo.EntityId) != constant.ENTITY_TYPE_AVATAR {
				continue
			}
			if entityMoveInfo.MotionInfo == nil {
				continue
			}
			motionInfo := entityMoveInfo.MotionInfo
			if motionInfo.Pos == nil {
				continue
			}
			// 玩家超速移动检测
			if ctx.sceneId != 3 {
				continue
			}
			ok := ctx.Move(motionInfo.Pos)
			if !ok {
				logger.Warn("player move jump, pos: %v, uid: %v", motionInfo.Pos, userId)
				h.KickPlayer(userId, gateAppId)
				continue
			}
			moveSpeed := ctx.GetMoveSpeed()
			if moveSpeed > MaxMoveSpeed {
				logger.Warn("player move overspeed, speed: %v, uid: %v", moveSpeed, userId)
				h.KickPlayer(userId, gateAppId)
				continue
			}
		case proto.CombatTypeArgument_COMBAT_EVT_BEING_HIT:
			evtBeingHitInfo := new(proto.EvtBeingHitInfo)
			err := pb.Unmarshal(entry.CombatData, evtBeingHitInfo)
			if err != nil {
				logger.Error("parse EvtBeingHitInfo error: %v, uid: %v", err, userId)
				continue
			}
			attackResult := evtBeingHitInfo.AttackResult
			if attackResult == nil {
				continue
			}
			if GetEntityType(attackResult.DefenseId) != constant.ENTITY_TYPE_MONSTER {
				continue
			}
			ok := ctx.Attack(attackResult.DefenseId)
			if !ok {
				logger.Warn("player attack monster feq too high, uid: %v", userId)
				h.KickPlayer(userId, gateAppId)
				continue
			}
		}
	}
}

// ToTheMoonEnterSceneReq 玩家进入场景通知（GS → multi 同步当前场景）
// multi 用 ctx.sceneId 决定是否启用反作弊检测（仅 sceneId=3 启用）
func (h *Handle) ToTheMoonEnterSceneReq(userId uint32, gateAppId string, payloadMsg pb.Message) {
	req := payloadMsg.(*proto.ToTheMoonEnterSceneReq)
	ctx := h.GetPlayerAcCtx(userId)
	if ctx == nil {
		logger.Error("get player anticheat context is nil, uid: %v", userId)
		return
	}
	ctx.sceneId = req.SceneId
	logger.Info("player enter scene: %v, uid: %v", req.SceneId, userId)
}

// GetEntityType 解析 entityId 取实体类型（写死 >> 24）
//
// **注意**：multi 这里是写死 8bit 实体类型移位 没有像 gs/lua_func.go 那样按客户端版本切换
// 因为 multi 不直接服务客户端 GS 已经做了版本兼容
// 但如果未来 GS 升级 multi 也要同步改这里 否则反作弊检测可能失效
func GetEntityType(entityId uint32) int {
	return int(entityId >> 24)
}

func GetDistance(v1 *proto.Vector, v2 *proto.Vector) float32 {
	return float32(math.Sqrt(float64((v1.X-v2.X)*(v1.X-v2.X)) + float64((v1.Y-v2.Y)*(v1.Y-v2.Y)) + float64((v1.Z-v2.Z)*(v1.Z-v2.Z))))
}
