package game

import (
	"hk4e/common/mq"
	"hk4e/gs/model"
	"hk4e/node/api"
	"hk4e/protocol/cmd"

	"github.com/flswld/halo/logger"
	"github.com/flswld/halo/protocol/kcp"
	pb "google.golang.org/protobuf/proto"
)

// 接口路由管理器
//
// 职责：把来自客户端（经由Gate转发）的cmd消息和来自其他服务器的内部消息分发给对应的Game方法。
// 路由表 handlerFuncRouteMap 在 initRoute 中静态注册，每个cmd_id对应一个 HandlerFunc。
// 所有路由处理都在Game主循环单线程内执行（见game.go的gameMainLoop），handler内部访问玩家数据无需加锁。

// initRoute 注册所有客户端cmd到对应Game方法的路由映射
// 新增客户端协议处理时，在此追加 cmd.XxxReq -> GAME.XxxReq 一行
func (r *RouteManager) initRoute() {
	r.handlerFuncRouteMap = map[uint16]HandlerFunc{
		cmd.PingReq:                           GAME.PingReq,
		cmd.SetPlayerBornDataReq:              GAME.SetPlayerBornDataReq,
		cmd.QueryPathReq:                      GAME.QueryPathReq,
		cmd.UnionCmdNotify:                    GAME.UnionCmdNotify,
		cmd.MassiveEntityElementOpBatchNotify: GAME.MassiveEntityElementOpBatchNotify,
		cmd.ToTheMoonEnterSceneReq:            GAME.ToTheMoonEnterSceneReq,
		cmd.PlayerSetPauseReq:                 GAME.PlayerSetPauseReq,
		cmd.EnterSceneReadyReq:                GAME.EnterSceneReadyReq,
		cmd.PathfindingEnterSceneReq:          GAME.PathfindingEnterSceneReq,
		cmd.GetScenePointReq:                  GAME.GetScenePointReq,
		cmd.GetSceneAreaReq:                   GAME.GetSceneAreaReq,
		cmd.SceneInitFinishReq:                GAME.SceneInitFinishReq,
		cmd.EnterSceneDoneReq:                 GAME.EnterSceneDoneReq,
		cmd.EnterWorldAreaReq:                 GAME.EnterWorldAreaReq,
		cmd.PostEnterSceneReq:                 GAME.PostEnterSceneReq,
		cmd.TowerAllDataReq:                   GAME.TowerAllDataReq,
		cmd.SceneTransToPointReq:              GAME.SceneTransToPointReq,
		cmd.UnlockTransPointReq:               GAME.UnlockTransPointReq,
		cmd.MarkMapReq:                        GAME.MarkMapReq,
		cmd.ChangeAvatarReq:                   GAME.ChangeAvatarReq,
		cmd.SetUpAvatarTeamReq:                GAME.SetUpAvatarTeamReq,
		cmd.ChooseCurAvatarTeamReq:            GAME.ChooseCurAvatarTeamReq,
		cmd.GetGachaInfoReq:                   GAME.GetGachaInfoReq,
		cmd.DoGachaReq:                        GAME.DoGachaReq,
		cmd.CombatInvocationsNotify:           GAME.CombatInvocationsNotify,
		cmd.AbilityInvocationsNotify:          GAME.AbilityInvocationsNotify,
		cmd.ClientAbilityInitFinishNotify:     GAME.ClientAbilityInitFinishNotify,
		cmd.EvtDoSkillSuccNotify:              GAME.EvtDoSkillSuccNotify,
		cmd.ClientAbilityChangeNotify:         GAME.ClientAbilityChangeNotify,
		cmd.EntityAiSyncNotify:                GAME.EntityAiSyncNotify,
		cmd.WearEquipReq:                      GAME.WearEquipReq,
		cmd.ChangeGameTimeReq:                 GAME.ChangeGameTimeReq,
		cmd.GetPlayerSocialDetailReq:          GAME.GetPlayerSocialDetailReq,
		cmd.SetPlayerBirthdayReq:              GAME.SetPlayerBirthdayReq,
		cmd.SetNameCardReq:                    GAME.SetNameCardReq,
		cmd.SetPlayerSignatureReq:             GAME.SetPlayerSignatureReq,
		cmd.SetPlayerNameReq:                  GAME.SetPlayerNameReq,
		cmd.SetPlayerHeadImageReq:             GAME.SetPlayerHeadImageReq,
		cmd.GetAllUnlockNameCardReq:           GAME.GetAllUnlockNameCardReq,
		cmd.GetPlayerFriendListReq:            GAME.GetPlayerFriendListReq,
		cmd.GetPlayerAskFriendListReq:         GAME.GetPlayerAskFriendListReq,
		cmd.AskAddFriendReq:                   GAME.AskAddFriendReq,
		cmd.DealAddFriendReq:                  GAME.DealAddFriendReq,
		cmd.GetOnlinePlayerListReq:            GAME.GetOnlinePlayerListReq,
		cmd.PlayerApplyEnterMpReq:             GAME.PlayerApplyEnterMpReq,
		cmd.PlayerApplyEnterMpResultReq:       GAME.PlayerApplyEnterMpResultReq,
		cmd.PlayerGetForceQuitBanInfoReq:      GAME.PlayerGetForceQuitBanInfoReq,
		cmd.GetShopmallDataReq:                GAME.GetShopmallDataReq,
		cmd.GetShopReq:                        GAME.GetShopReq,
		cmd.BuyGoodsReq:                       GAME.BuyGoodsReq,
		cmd.McoinExchangeHcoinReq:             GAME.McoinExchangeHcoinReq,
		cmd.AvatarChangeCostumeReq:            GAME.AvatarChangeCostumeReq,
		cmd.AvatarWearFlycloakReq:             GAME.AvatarWearFlycloakReq,
		cmd.PullRecentChatReq:                 GAME.PullRecentChatReq,
		cmd.PullPrivateChatReq:                GAME.PullPrivateChatReq,
		cmd.PrivateChatReq:                    GAME.PrivateChatReq,
		cmd.ReadPrivateChatReq:                GAME.ReadPrivateChatReq,
		cmd.PlayerChatReq:                     GAME.PlayerChatReq,
		cmd.BackMyWorldReq:                    GAME.BackMyWorldReq,
		cmd.ChangeWorldToSingleModeReq:        GAME.ChangeWorldToSingleModeReq,
		cmd.SceneKickPlayerReq:                GAME.SceneKickPlayerReq,
		cmd.ChangeMpTeamAvatarReq:             GAME.ChangeMpTeamAvatarReq,
		cmd.SceneAvatarStaminaStepReq:         GAME.SceneAvatarStaminaStepReq,
		cmd.JoinPlayerSceneReq:                GAME.JoinPlayerSceneReq,
		cmd.EvtAvatarEnterFocusNotify:         GAME.EvtAvatarEnterFocusNotify,
		cmd.EvtAvatarUpdateFocusNotify:        GAME.EvtAvatarUpdateFocusNotify,
		cmd.EvtAvatarExitFocusNotify:          GAME.EvtAvatarExitFocusNotify,
		cmd.EvtEntityRenderersChangedNotify:   GAME.EvtEntityRenderersChangedNotify,
		cmd.EvtBulletDeactiveNotify:           GAME.EvtBulletDeactiveNotify,
		cmd.EvtBulletHitNotify:                GAME.EvtBulletHitNotify,
		cmd.EvtBulletMoveNotify:               GAME.EvtBulletMoveNotify,
		cmd.EvtCreateGadgetNotify:             GAME.EvtCreateGadgetNotify,
		cmd.EvtDestroyGadgetNotify:            GAME.EvtDestroyGadgetNotify,
		cmd.CreateVehicleReq:                  GAME.CreateVehicleReq,
		cmd.VehicleInteractReq:                GAME.VehicleInteractReq,
		cmd.SceneEntityDrownReq:               GAME.SceneEntityDrownReq,
		cmd.GetOnlinePlayerInfoReq:            GAME.GetOnlinePlayerInfoReq,
		cmd.GCGAskDuelReq:                     GAME.GCGAskDuelReq,
		cmd.GCGInitFinishReq:                  GAME.GCGInitFinishReq,
		cmd.GCGOperationReq:                   GAME.GCGOperationReq,
		cmd.ObstacleModifyNotify:              GAME.ObstacleModifyNotify,
		cmd.AvatarUpgradeReq:                  GAME.AvatarUpgradeReq,
		cmd.AvatarPromoteReq:                  GAME.AvatarPromoteReq,
		cmd.CalcWeaponUpgradeReturnItemsReq:   GAME.CalcWeaponUpgradeReturnItemsReq,
		cmd.WeaponUpgradeReq:                  GAME.WeaponUpgradeReq,
		cmd.WeaponPromoteReq:                  GAME.WeaponPromoteReq,
		cmd.WeaponAwakenReq:                   GAME.WeaponAwakenReq,
		cmd.AvatarPromoteGetRewardReq:         GAME.AvatarPromoteGetRewardReq,
		cmd.SetEquipLockStateReq:              GAME.SetEquipLockStateReq,
		cmd.TakeoffEquipReq:                   GAME.TakeoffEquipReq,
		cmd.AddQuestContentProgressReq:        GAME.AddQuestContentProgressReq,
		cmd.NpcTalkReq:                        GAME.NpcTalkReq,
		cmd.EvtAiSyncSkillCdNotify:            GAME.EvtAiSyncSkillCdNotify,
		cmd.EvtAiSyncCombatThreatInfoNotify:   GAME.EvtAiSyncCombatThreatInfoNotify,
		cmd.EntityConfigHashNotify:            GAME.EntityConfigHashNotify,
		cmd.MonsterAIConfigHashNotify:         GAME.MonsterAIConfigHashNotify,
		cmd.DungeonEntryInfoReq:               GAME.DungeonEntryInfoReq,
		cmd.PlayerEnterDungeonReq:             GAME.PlayerEnterDungeonReq,
		cmd.PlayerQuitDungeonReq:              GAME.PlayerQuitDungeonReq,
		cmd.GadgetInteractReq:                 GAME.GadgetInteractReq,
		cmd.GmTalkReq:                         GAME.GmTalkReq,
		cmd.SetEntityClientDataNotify:         GAME.SetEntityClientDataNotify,
		cmd.EntityForceSyncReq:                GAME.EntityForceSyncReq,
		cmd.AvatarDieAnimationEndReq:          GAME.AvatarDieAnimationEndReq,
		cmd.WorldPlayerReviveReq:              GAME.WorldPlayerReviveReq,
		cmd.UseItemReq:                        GAME.UseItemReq,
		cmd.EnterTransPointRegionNotify:       GAME.EnterTransPointRegionNotify,
		cmd.ExitTransPointRegionNotify:        GAME.ExitTransPointRegionNotify,
		cmd.GetPlayerBlacklistReq:             GAME.GetPlayerBlacklistReq,
		cmd.GetChatEmojiCollectionReq:         GAME.GetChatEmojiCollectionReq,
		cmd.SetPlayerPropReq:                  GAME.SetPlayerPropReq,
		cmd.SetOpenStateReq:                   GAME.SetOpenStateReq,
		cmd.PlayerStartMatchReq:               GAME.PlayerStartMatchReq,
		cmd.PlayerCancelMatchReq:              GAME.PlayerCancelMatchReq,
		cmd.PlayerConfirmMatchReq:             GAME.PlayerConfirmMatchReq,
		cmd.QuestCreateEntityReq:              GAME.QuestCreateEntityReq,
		cmd.QuestDestroyEntityReq:             GAME.QuestDestroyEntityReq,
		cmd.QuestDestroyNpcReq:                GAME.QuestDestroyNpcReq,
		cmd.AvatarSkillUpgradeReq:             GAME.AvatarSkillUpgradeReq,
		cmd.UnlockAvatarTalentReq:             GAME.UnlockAvatarTalentReq,
		cmd.ReliquaryUpgradeReq:               GAME.ReliquaryUpgradeReq,
		cmd.ReliquaryPromoteReq:               GAME.ReliquaryPromoteReq,
		cmd.GetAllMailReq:                     GAME.GetAllMailReq,
		cmd.GetAllMailNotify:                  GAME.GetAllMailNotify,
		cmd.DelMailReq:                        GAME.DelMailReq,
		cmd.GetMailItemReq:                    GAME.GetMailItemReq,
		cmd.ReadMailNotify:                    GAME.ReadMailNotify,
		cmd.ChangeMailStarNotify:              GAME.ChangeMailStarNotify,
		cmd.SelectWorktopOptionReq:            GAME.SelectWorktopOptionReq,
		cmd.GetWidgetSlotReq:                  GAME.GetWidgetSlotReq,
		cmd.SetWidgetSlotReq:                  GAME.SetWidgetSlotReq,
		cmd.QuickUseWidgetReq:                 GAME.QuickUseWidgetReq,
		cmd.SceneAudioNotify:                  GAME.SceneAudioNotify,
		cmd.WidgetDoBagReq:                    GAME.WidgetDoBagReq,
		cmd.PersonalSceneJumpReq:              GAME.PersonalSceneJumpReq,
		cmd.GetParentQuestVideoKeyReq:         GAME.GetParentQuestVideoKeyReq,
	}
}

// HandlerFunc 客户端cmd处理函数签名 player为消息所属玩家 payloadMsg为protobuf反序列化后的请求体
type HandlerFunc func(player *model.Player, payloadMsg pb.Message)

// RouteManager 路由管理器 维护cmd_id到handler的映射表
type RouteManager struct {
	// 路由表 key:cmdId value:HandlerFunc 启动时一次性注册不变
	handlerFuncRouteMap map[uint16]HandlerFunc
}

func NewRouteManager() (r *RouteManager) {
	r = new(RouteManager)
	r.initRoute()
	return r
}

// doRoute 客户端正常游戏消息的路由分发
// 校验玩家在线状态后切换全局SELF指针并调用handler
// 注意：SELF会在handler执行前后置换 这是单线程模式下传递"当前处理玩家"上下文的隐式约定
// handler内部以及其调用链允许直接读取SELF 但绝不能持有到异步goroutine中
func (r *RouteManager) doRoute(cmdId uint16, userId uint32, clientSeq uint32, payloadMsg pb.Message) {
	handlerFunc, ok := r.handlerFuncRouteMap[cmdId]
	if !ok {
		logger.Error("no route for msg, cmdId: %v", cmdId)
		return
	}
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		// 玩家在本服找不到 视为非法连接 让Gate踢掉对应的KCP会话
		logger.Error("player is nil, uid: %v", userId)
		GAME.KickPlayer(userId, kcp.EnetNotFoundSession)
		return
	}
	if !player.Online {
		logger.Error("player not online, uid: %v", userId)
		return
	}
	// NetFreeze在跨服迁移、踢人、重连等过程中置位 避免向中间态玩家继续下发消息
	if player.NetFreeze {
		return
	}
	player.ClientSeq = clientSeq
	SELF = player
	handlerFunc(player, payloadMsg)
	SELF = nil
}

// RouteHandle 主循环消息总入口 按消息类型分发到三类处理路径
// 由 Game.gameMainLoop 在 select 接收到 mq 消息后直接调用
func (r *RouteManager) RouteHandle(netMsg *mq.NetMsg) {
	switch netMsg.MsgType {
	case mq.MsgTypeGame:
		// 来自客户端的游戏消息（经Gate转发）
		if netMsg.OriginServerType != api.GATE {
			return
		}
		gameMsg := netMsg.GameMsg
		switch netMsg.EventId {
		case mq.NormalMsg:
			// PlayerLoginReq需要特殊处理：玩家此时还未在内存 不能走doRoute的在线校验
			if gameMsg.CmdId == cmd.PlayerLoginReq {
				GAME.PlayerLoginReq(gameMsg.UserId, gameMsg.ClientSeq, netMsg.OriginServerAppId, gameMsg.PayloadMessage)
				return
			}
			r.doRoute(gameMsg.CmdId, gameMsg.UserId, gameMsg.ClientSeq, gameMsg.PayloadMessage)
		}
	case mq.MsgTypeConnCtrl:
		// 来自Gate的连接控制消息（RTT上报、玩家离线通知等）
		if netMsg.OriginServerType != api.GATE {
			return
		}
		connCtrlMsg := netMsg.ConnCtrlMsg
		switch netMsg.EventId {
		case mq.ClientRttNotify:
			GAME.ClientRttNotify(connCtrlMsg.UserId, connCtrlMsg.ClientRtt)
		case mq.UserOfflineNotify:
			// 玩家断线 走非"跨服切换"分支 即正常下线
			GAME.OnOffline(connCtrlMsg.UserId, netMsg.OriginServerAppId, &ChangeGsInfo{
				IsChangeGs: false,
			})
		default:
		}
	case mq.MsgTypeServer:
		// 服务器之间转发的业务消息（跨服多人/聊天/好友/停服/GM等）
		serverMsg := netMsg.ServerMsg
		switch netMsg.EventId {
		case mq.ServerUserOnlineStateChangeNotify:
			// 远程玩家上线/离线状态变化 由其他GS广播过来
			logger.Debug("remote user online state change, uid: %v, online: %v", serverMsg.UserId, serverMsg.IsOnline)
			USER_MANAGER.SetRemoteUserOnlineState(serverMsg.UserId, serverMsg.IsOnline, netMsg.OriginServerAppId)
		case mq.ServerAppidBindNotify:
			// Multi服告知本玩家绑定到的Multi appid
			GAME.ServerAppidBindNotify(serverMsg.UserId, serverMsg.MultiServerAppId)
		case mq.ServerPlayerMpReq:
			// 跨服多人世界请求（敲门/确认）由对端GS转发过来
			GAME.ServerPlayerMpReq(serverMsg.PlayerMpInfo, netMsg.OriginServerAppId)
		case mq.ServerPlayerMpRsp:
			// 跨服多人世界响应
			GAME.ServerPlayerMpRsp(serverMsg.PlayerMpInfo)
		case mq.ServerChatMsgNotify:
			// 跨服私聊消息
			GAME.ServerChatMsgNotify(serverMsg.ChatMsgInfo)
		case mq.ServerAddFriendNotify:
			// 跨服添加好友通知
			GAME.ServerAddFriendNotify(serverMsg.AddFriendInfo)
		case mq.ServerStopNotify:
			// 全服停服通知
			GAME.ServerStopNotify()
		case mq.ServerDispatchCancelNotify:
			// 服务器调度取消（不再接受新玩家登录）
			GAME.ServerDispatchCancelNotify(serverMsg.AppVersion)
		case mq.ServerGmCmdNotify:
			// 跨服GM命令转发到本服 投递到主循环命令通道执行
			commandTextInput := COMMAND_MANAGER.GetCommandMessageInput()
			commandTextInput <- &CommandMessage{
				GMType:     SystemFuncGM,
				FuncName:   serverMsg.GmCmdFuncName,
				ParamList:  serverMsg.GmCmdParamList,
				ResultChan: nil,
			}
		default:
		}
	}
}
