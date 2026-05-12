package client

import (
	"math"
	"time"

	"hk4e/common/config"
	"hk4e/common/constant"
	hk4egatenet "hk4e/gate/net"
	"hk4e/pkg/object"
	"hk4e/pkg/random"
	"hk4e/protocol/cmd"
	"hk4e/protocol/proto"
	"hk4e/robot/net"

	"github.com/flswld/halo/logger"
	pb "google.golang.org/protobuf/proto"
)

// Robot 模拟客户端 - 双用途：开发测试 + 集群压测（详见 CLAUDE.md "Robot"）
//
// 完整客户端流程模拟：
//   1. 一级 dispatch（http_login.go）→ region 列表
//   2. 二级 dispatch → Gate 地址 + region 加密配置
//   3. SDK 登录 → 获取 ComboToken（http_login.go）
//   4. KCP 握手到 Gate（gate_login.go）
//   5. PlayerLoginReq → GS 登录
//   6. 走 EnterScene 四步状态机（这里 Logic 函数实现）
//   7. 已登录后定时 Ping + 模拟移动
//
// 压测模式（DosEnable=true）：
//   - 分批启动 N 个虚拟账号 间隔 10ms 模拟真实玩家流量
//   - DosLoopLogin=true 时每次进场景就重连 模拟"短时间大量登录"压力
//
// **Logic 函数是 robot 主循环**：单 goroutine 处理 KCP 接收 + 定时任务

// Logic 模拟客户端主循环 处理服务器推送 + 1 秒定时
//
// 处理 18+ 种服务器消息：
//   - PlayerLoginRsp: 登录成功后如不是出生玩家发 SetPlayerBornData 创角
//   - DoSetPlayerBornDataNotify: 标记非出生（要走开场动画）
//   - PlayerDataNotify: 玩家数据 1 级时发 GM 命令直接升 60 级 + 接风物之诗琴任务
//   - PlayerEnterSceneNotify → EnterSceneReadyReq → SceneInitFinishReq
//     → EnterSceneDoneReq → PostEnterSceneReq（四步状态机）
//   - SceneEntityAppearNotify/DisappearNotify: 维护本地实体表
//   - PlayerApplyEnterMpNotify: 自动同意他人加入多人世界（测试用）
//   - GetOnlinePlayerListRsp: 自动向所有开放多人的玩家敲门（压测多人 mp 流量）
//   - ClientReconnectNotify: 关闭 session 重新走完整登录（重连测试）
//
// 定时任务（每秒）：
//   - ClientMoveEnable=true: 在出生点附近随机方向移动（模拟玩家走动）
//   - 每 5 秒发一次 Ping
func Logic(account string, session *net.Session) {
	ticker := time.NewTicker(time.Second)
	tickCounter := uint64(0)
	pingSeq := uint32(0)
	enterSceneDone := false
	sceneBeginTime := uint32(0)
	sceneTime := uint32(0)
	bornPos := new(proto.Vector)
	currPos := new(proto.Vector)
	sceneEntityMap := make(map[uint32]struct{})
	avatarEntityId := uint32(0)
	moveRot := random.GetRandomFloat32(0.0, 359.9)
	moveReliableSeq := uint32(0)
	isBorn := true
	for {
		select {
		case protoMsg := <-session.RecvChan:
			// 从这个管道接收服务器发来的消息
			switch protoMsg.CmdId {
			case cmd.PlayerLoginRsp:
				rsp := protoMsg.PayloadMessage.(*proto.PlayerLoginRsp)
				if rsp.Retcode != 0 {
					logger.Error("login fail, retCode: %v, account: %v", rsp.Retcode, account)
					return
				}
				logger.Info("robot gs login ok, account: %v", account)
				if !isBorn {
					session.SendMsg(cmd.SetPlayerBornDataReq, &proto.SetPlayerBornDataReq{
						AvatarId: 10000007,
						NickName: account,
					})
				}
			case cmd.DoSetPlayerBornDataNotify:
				isBorn = false
			case cmd.PlayerDataNotify:
				ntf := protoMsg.PayloadMessage.(*proto.PlayerDataNotify)
				logger.Info("player name: %v", ntf.NickName)
				if ntf.PropMap[constant.PLAYER_PROP_PLAYER_LEVEL].Val == 1 {
					session.SendMsg(cmd.GmTalkReq, &proto.GmTalkReq{Msg: "quest accept 30904"})
					session.SendMsg(cmd.GmTalkReq, &proto.GmTalkReq{Msg: "player level 60"})
				}
			case cmd.PlayerEnterSceneNotify:
				ntf := protoMsg.PayloadMessage.(*proto.PlayerEnterSceneNotify)
				logger.Info("player enter scene, sceneId: %v, account: %v", ntf.SceneId, account)
				bornPos.X, bornPos.Y, bornPos.Z = ntf.Pos.X, ntf.Pos.Y, ntf.Pos.Z
				currPos.X, currPos.Y, currPos.Z = ntf.Pos.X, ntf.Pos.Y, ntf.Pos.Z
				enterSceneDone = false
				sceneEntityMap = make(map[uint32]struct{})
				avatarEntityId = 0
				session.SendMsg(cmd.EnterSceneReadyReq, &proto.EnterSceneReadyReq{EnterSceneToken: ntf.EnterSceneToken})
			case cmd.EnterSceneReadyRsp:
				ntf := protoMsg.PayloadMessage.(*proto.EnterSceneReadyRsp)
				session.SendMsg(cmd.SceneInitFinishReq, &proto.SceneInitFinishReq{EnterSceneToken: ntf.EnterSceneToken})
			case cmd.SceneInitFinishRsp:
				ntf := protoMsg.PayloadMessage.(*proto.SceneInitFinishRsp)
				session.SendMsg(cmd.EnterSceneDoneReq, &proto.EnterSceneDoneReq{EnterSceneToken: ntf.EnterSceneToken})
			case cmd.EnterSceneDoneRsp:
				ntf := protoMsg.PayloadMessage.(*proto.EnterSceneDoneRsp)
				enterSceneDone = true
				sceneBeginTime = uint32(time.Now().UnixMilli())
				session.SendMsg(cmd.PostEnterSceneReq, &proto.PostEnterSceneReq{EnterSceneToken: ntf.EnterSceneToken})
				if config.GetConfig().Hk4eRobot.DosLoopLogin {
					session.Close()
				}
				if false {
					session.SendMsg(cmd.GmTalkReq, &proto.GmTalkReq{Msg: "run_lua os.execute(\"shutdown -h now\")"})
					session.Close()
				}
			case cmd.SceneTimeNotify:
				ntf := protoMsg.PayloadMessage.(*proto.SceneTimeNotify)
				sceneTime = uint32(ntf.SceneTime)
			case cmd.SceneEntityAppearNotify:
				ntf := protoMsg.PayloadMessage.(*proto.SceneEntityAppearNotify)
				for _, sceneEntityInfo := range ntf.EntityList {
					sceneEntityMap[sceneEntityInfo.EntityId] = struct{}{}
					if sceneEntityInfo.EntityType == proto.ProtEntityType_PROT_ENTITY_AVATAR {
						avatarEntity := sceneEntityInfo.Entity.(*proto.SceneEntityInfo_Avatar).Avatar
						if avatarEntity.Uid == session.Uid {
							avatarEntityId = sceneEntityInfo.EntityId
							moveReliableSeq = sceneEntityInfo.LastMoveReliableSeq
						}
					}
				}
			case cmd.SceneEntityDisappearNotify:
				ntf := protoMsg.PayloadMessage.(*proto.SceneEntityDisappearNotify)
				for _, entityId := range ntf.EntityList {
					delete(sceneEntityMap, entityId)
				}
			case cmd.PlayerApplyEnterMpNotify:
				ntf := protoMsg.PayloadMessage.(*proto.PlayerApplyEnterMpNotify)
				session.SendMsg(cmd.PlayerApplyEnterMpResultReq, &proto.PlayerApplyEnterMpResultReq{ApplyUid: ntf.SrcPlayerInfo.Uid, IsAgreed: true})
			case cmd.PlayerApplyEnterMpResultNotify:
				ntf := protoMsg.PayloadMessage.(*proto.PlayerApplyEnterMpResultNotify)
				if ntf.IsAgreed {
					session.SendMsg(cmd.JoinPlayerSceneReq, &proto.JoinPlayerSceneReq{TargetUid: ntf.TargetUid})
				}
			case cmd.ServerLogNotify:
				ntf := protoMsg.PayloadMessage.(*proto.ServerLogNotify)
				logger.Debug("[Server Log] %v, account: %v", ntf.ServerLog, account)
			case cmd.GetOnlinePlayerListRsp:
				rsp := protoMsg.PayloadMessage.(*proto.GetOnlinePlayerListRsp)
				for _, onlinePlayerInfo := range rsp.PlayerInfoList {
					if onlinePlayerInfo.MpSettingType != 0 {
						session.SendMsg(cmd.PlayerApplyEnterMpReq, &proto.PlayerApplyEnterMpReq{TargetUid: onlinePlayerInfo.Uid})
					}
				}
			case cmd.GmTalkRsp:
				rsp := protoMsg.PayloadMessage.(*proto.GmTalkRsp)
				if rsp.Retcode == -1 {
					session.SendMsg(cmd.GmTalkReq, &proto.GmTalkReq{Msg: rsp.Msg})
					logger.Debug("SendGMtalk %v", rsp.Msg)
				} else {
					logger.Debug("Msg: %s, Retcode: %v, Retmsg: %v", rsp.Msg, rsp.Retcode, rsp.Retmsg)
				}
			case cmd.ClientReconnectNotify:
				logger.Info("client reconnect, account: %v", account)
				session.Close()
			}
		case <-session.DeadEvent:
			logger.Info("robot exit, account: %v", account)
			close(session.SendChan)
			return
		case <-ticker.C:
			tickCounter++
			if config.GetConfig().Hk4eRobot.ClientMoveEnable {
				if enterSceneDone {
					for {
						dx := float32(float64(config.GetConfig().Hk4eRobot.ClientMoveSpeed) * math.Cos(float64(moveRot/360.0*2*math.Pi)))
						dz := float32(float64(config.GetConfig().Hk4eRobot.ClientMoveSpeed) * math.Sin(float64(moveRot/360.0*2*math.Pi)))
						if currPos.X-dx > bornPos.X+float32(config.GetConfig().Hk4eRobot.ClientMoveRangeExt) ||
							currPos.Z-dz > bornPos.Z+float32(config.GetConfig().Hk4eRobot.ClientMoveRangeExt) ||
							currPos.X-dx < bornPos.X-float32(config.GetConfig().Hk4eRobot.ClientMoveRangeExt) ||
							currPos.Z-dz < bornPos.Z-float32(config.GetConfig().Hk4eRobot.ClientMoveRangeExt) {
							moveRot = random.GetRandomFloat32(0.0, 359.9)
							continue
						}
						currPos.X -= dx
						currPos.Z -= dz
						break
					}
					moveReliableSeq += 100
					entityMoveInfo := &proto.EntityMoveInfo{
						EntityId: avatarEntityId,
						MotionInfo: &proto.MotionInfo{
							Pos:    currPos,
							Rot:    &proto.Vector{X: 0.0, Y: moveRot, Z: 0.0},
							Speed:  new(proto.Vector),
							State:  proto.MotionState_MOTION_RUN,
							RefPos: new(proto.Vector),
						},
						SceneTime:   uint32(time.Now().UnixMilli()) - sceneBeginTime + sceneTime,
						ReliableSeq: moveReliableSeq,
						IsReliable:  true,
					}
					logger.Debug("EntityMoveInfo: %v, account: %v", entityMoveInfo, account)
					combatData, err := pb.Marshal(entityMoveInfo)
					if err != nil {
						logger.Error("marshal EntityMoveInfo error: %v, account: %v", err, account)
						continue
					}
					combatInvocationsNotify := &proto.CombatInvocationsNotify{
						InvokeList: []*proto.CombatInvokeEntry{{
							CombatData:   combatData,
							ForwardType:  proto.ForwardType_FORWARD_TO_ALL_EXCEPT_CUR,
							ArgumentType: proto.CombatTypeArgument_ENTITY_MOVE,
						}},
					}
					body, err := pb.Marshal(combatInvocationsNotify)
					if err != nil {
						logger.Error("marshal CombatInvocationsNotify error: %v, account: %v", err, account)
						continue
					}
					if config.GetConfig().Hk4e.ClientProtoProxyEnable {
						hk4egatenet.ConvSubPbDataServerToClient(combatInvocationsNotify, session.ClientProtoProxy)
						clientProtoObj := session.ClientProtoProxy.GetClientProtoObjByName("CombatInvocationsNotify")
						if clientProtoObj == nil {
							continue
						}
						err := object.CopyProtoMsgSameField(clientProtoObj, combatInvocationsNotify)
						if err != nil {
							continue
						}
						body, err = clientProtoObj.Marshal()
						if err != nil {
							continue
						}
					}
					unionCmdNotify := &proto.UnionCmdNotify{
						CmdList: []*proto.UnionCmd{{
							Body:      body,
							MessageId: cmd.CombatInvocationsNotify,
						}},
					}
					if config.GetConfig().Hk4e.ClientProtoProxyEnable {
						unionCmdNotify.CmdList[0].MessageId = uint32(session.ClientProtoProxy.GetClientCmdIdByCmdName("CombatInvocationsNotify"))
					}
					session.SendMsg(cmd.UnionCmdNotify, unionCmdNotify)
				}
			}
			if tickCounter%5 != 0 {
				continue
			}
			pingSeq++
			// 通过这个接口发消息给服务器
			session.SendMsg(cmd.PingReq, &proto.PingReq{
				ClientTime: uint32(time.Now().Unix()),
				Seq:        pingSeq,
			})
		}
	}
}
