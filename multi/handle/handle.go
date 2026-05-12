package handle

import (
	"hk4e/common/mq"
	"hk4e/node/api"
	"hk4e/protocol/cmd"

	"github.com/flswld/halo/logger"
	"github.com/flswld/halo/protocol/kcp"
	pb "google.golang.org/protobuf/proto"
)

// Multi 主处理器（消息分发 + 反作弊上下文 + 寻路服务）
//
// multi 服订阅来自 gate 的玩家消息（CombatInvocationsNotify 等）+ GS 的玩家上下线广播
// 单 goroutine 串行处理（与 GS 主循环一样的设计）保证状态一致性
//
// 三个职责：
//  1. 反作弊（playerAcCtxMap）：玩家上线创建上下文 → 检测 → 下线销毁
//  2. NavMesh 寻路（worldStatic）：QueryPathReq 计算路径返回客户端（怪物寻路）
//  3. 障碍物管理（ObstacleModifyNotify）：动态添加/删除障碍物 carving
type Handle struct {
	messageQueue   *mq.MessageQueue             // MQ 双通道
	playerAcCtxMap map[uint32]*AnticheatContext // 玩家反作弊上下文
	worldStatic    *WorldStatic                 // 静态世界（NavMesh + 障碍物）
}

func NewHandle(messageQueue *mq.MessageQueue) (r *Handle) {
	r = new(Handle)
	r.messageQueue = messageQueue
	r.playerAcCtxMap = make(map[uint32]*AnticheatContext)
	r.worldStatic = NewWorldStatic()
	r.worldStatic.InitTerrain()
	go r.run()
	return r
}

// run multi 服主循环（单 goroutine 串行处理）
//
// 订阅消息分两类：
//   - MsgTypeGame: 来自 gate 的玩家消息（仅 NormalMsg）
//     · CombatInvocationsNotify: 反作弊检测
//     · ToTheMoonEnterSceneReq: 玩家场景切换
//     · QueryPathReq: 怪物寻路请求 计算后返回客户端
//     · ObstacleModifyNotify: 动态障碍物增删
//   - MsgTypeServer: 来自 GS 的状态广播
//     · ServerUserOnlineStateChangeNotify: 上线创建反作弊上下文 / 下线销毁
func (h *Handle) run() {
	logger.Info("start handle")
	for {
		netMsg := <-h.messageQueue.GetNetMsg()
		switch netMsg.MsgType {
		case mq.MsgTypeGame:
			if netMsg.OriginServerType != api.GATE {
				continue
			}
			if netMsg.EventId != mq.NormalMsg {
				continue
			}
			gameMsg := netMsg.GameMsg
			switch gameMsg.CmdId {
			case cmd.CombatInvocationsNotify:
				h.CombatInvocationsNotify(gameMsg.UserId, netMsg.OriginServerAppId, gameMsg.PayloadMessage)
			case cmd.ToTheMoonEnterSceneReq:
				h.ToTheMoonEnterSceneReq(gameMsg.UserId, netMsg.OriginServerAppId, gameMsg.PayloadMessage)
			case cmd.QueryPathReq:
				h.QueryPath(gameMsg.UserId, netMsg.OriginServerAppId, gameMsg.PayloadMessage)
			case cmd.ObstacleModifyNotify:
				h.ObstacleModifyNotify(gameMsg.UserId, netMsg.OriginServerAppId, gameMsg.PayloadMessage)
			}
		case mq.MsgTypeServer:
			serverMsg := netMsg.ServerMsg
			switch netMsg.EventId {
			case mq.ServerUserOnlineStateChangeNotify:
				logger.Info("player online state change, state: %v, uid: %v", serverMsg.IsOnline, serverMsg.UserId)
				if serverMsg.IsOnline {
					h.AddPlayerAcCtx(serverMsg.UserId)
				} else {
					h.DelPlayerAcCtx(serverMsg.UserId)
				}
			default:
			}
		default:
		}
	}
}

// KickPlayer multi 踢人入口（被反作弊检测调用）
//
// **默认不踢人**：KickCheatPlayer=false 时直接 return 仅记录日志
// 这是项目默认开关 防止反作弊误杀（毕竟规则简单容易误判）
// 真正部署时如果反作弊规则可靠 可以打开 KickCheatPlayer
//
// 真踢人时：通过 ConnCtrlMsg 发到 gate 让 gate 关 KCP 会话
//
//	reason=EnetServerKillClient 客户端会显示对应错误码
func (h *Handle) KickPlayer(userId uint32, gateAppId string) {
	if !KickCheatPlayer {
		return
	}
	h.messageQueue.SendToGate(gateAppId, &mq.NetMsg{
		MsgType: mq.MsgTypeConnCtrl,
		EventId: mq.KickPlayerNotify,
		ConnCtrlMsg: &mq.ConnCtrlMsg{
			KickUserId: userId,
			KickReason: kcp.EnetServerKillClient,
		},
	})
}

// SendMsg 发送消息给客户端
func (h *Handle) SendMsg(cmdId uint16, userId uint32, gateAppId string, payloadMsg pb.Message) {
	if payloadMsg == nil {
		return
	}
	gameMsg := new(mq.GameMsg)
	gameMsg.UserId = userId
	gameMsg.CmdId = cmdId
	gameMsg.ClientSeq = 0
	// 在这里直接序列化成二进制数据 防止发送的消息内包含各种游戏数据指针 而造成并发读写的问题
	payloadMessageData, err := pb.Marshal(payloadMsg)
	if err != nil {
		logger.Error("parse payload msg to bin error: %v", err)
		return
	}
	gameMsg.PayloadMessageData = payloadMessageData
	h.messageQueue.SendToGate(gateAppId, &mq.NetMsg{
		MsgType: mq.MsgTypeGame,
		EventId: mq.NormalMsg,
		GameMsg: gameMsg,
	})
}
