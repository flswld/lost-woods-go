package game

import (
	"time"

	"hk4e/common/mq"
	"hk4e/gs/model"
	"hk4e/protocol/cmd"
	"hk4e/protocol/proto"

	"github.com/flswld/halo/logger"
	pb "google.golang.org/protobuf/proto"
)

// 聊天 模块（私聊 + 多人世界世界频道）
//
// 主要功能：
//   - 私聊（PrivateChatReq）：玩家间一对一文本/图标消息 跨服走 MQ
//   - 世界聊天（PlayerChatReq）：多人世界内的频道聊天
//   - 拉取历史（PullRecentChatReq/PullPrivateChatReq）：进入聊天界面时加载消息
//   - 已读标记（ReadPrivateChatReq）：客户端进入会话时标记已读
//
// 数据存储（详见 CLAUDE.md "聊天记录细节"）：
//   - **聊天记录只走 DB 不放 Redis**（不属于热数据）
//   - 内存：player.ChatMsgMap[targetUid] → []ChatMsg（每对私聊最多 100 条）
//   - DB：通过 USER_MANAGER.AsyncWriteDb 异步写（不阻塞主循环）
//   - Sequence 从 101 开始（1~100 留作系统消息/小可爱命令回执等特殊用途）
//
// 跨服私聊：
//   - 目标在本 GS：直接写双方内存 + 在线推送 PrivateChatNotify
//   - 目标在远程 GS：MQ 走 ServerChatMsgNotify 转发到目标 GS
//   - 目标离线：仅写发送方记录 + 写 DB 等目标上线时再拉
//
// 私聊 + GM 命令：玩家私聊"小可爱"AI 时 PrivateChatReq 走 COMMAND_MANAGER.PlayerInputCommand
//   AI 把消息当 GM 命令解析（详见 CLAUDE.md "GM 命令系统" 主入口）

/************************************************** 接口请求 **************************************************/

const (
	MaxMsgListLen = 100 // 与某人的最大聊天记录条数（防止恶意刷屏 + 控制内存占用）
)

// PullRecentChatReq 拉取最近未读聊天记录请求（进入聊天主界面时调用）
//
// 注释里 line 23-24 是作者吐槽：
//
//	原神官服只返回最新 5 条未读消息（PullNum=5）→ 玩家未读消息超过 5 条会"消息丢失"
//	作者表示项目不做这个限制 全量返回未读消息（"反手就是一个遍历"）
//
// 多人世界还会推送 world.ChatList 里最近 10 条世界频道聊天
func (g *Game) PullRecentChatReq(player *model.Player, payloadMsg pb.Message) {
	req := payloadMsg.(*proto.PullRecentChatReq)
	// 经研究发现 原神现网环境 客户端仅拉取最新的5条未读聊天消息 所以人太多的话小姐姐不回你消息是有原因的
	// 因此 阿米你这样做真的合适吗 不过现在代码到了我手上我想怎么写就怎么写 我才不会重蹈覆辙
	_ = req.PullNum

	retMsgList := make([]*proto.ChatInfo, 0)
	for _, msgList := range player.ChatMsgMap {
		for _, chatMsg := range msgList {
			// 反手就是一个遍历
			if chatMsg.IsRead {
				continue
			}
			retMsgList = append(retMsgList, g.ConvChatMsgToChatInfo(chatMsg))
		}
	}

	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		logger.Error("get world is nil, worldId: %v, uid: %v", player.WorldId, player.PlayerId)
		return
	}
	if world.IsMultiplayerWorld() {
		chatList := world.GetChatList()
		count := len(chatList)
		if count > 10 {
			count = 10
		}
		for i := len(chatList) - count; i < len(chatList); i++ {
			playerChatNotify := &proto.PlayerChatNotify{
				ChannelId: 0,
				ChatInfo:  chatList[i],
			}
			g.SendMsg(cmd.PlayerChatNotify, player.PlayerId, player.ClientSeq, playerChatNotify)
		}
	}

	pullRecentChatRsp := &proto.PullRecentChatRsp{
		ChatInfo: retMsgList,
	}
	g.SendMsg(cmd.PullRecentChatRsp, player.PlayerId, player.ClientSeq, pullRecentChatRsp)
}

// PullPrivateChatReq 拉取与某玩家的历史消息（玩家点开某好友的聊天框时调用）
// 按 fromSequence 偏移和 pullNum 数量分页（增量拉取避免一次性传太多）
// Sequence 从 101 开始（详见模块说明）所以 fromSequence=0 实际是从最早的消息开始
func (g *Game) PullPrivateChatReq(player *model.Player, payloadMsg pb.Message) {
	req := payloadMsg.(*proto.PullPrivateChatReq)
	targetUid := req.TargetUid
	pullNum := req.PullNum
	fromSequence := req.FromSequence

	msgList, exist := player.ChatMsgMap[targetUid]
	if !exist {
		return
	}
	if pullNum+fromSequence > uint32(len(msgList)) {
		pullNum = uint32(len(msgList)) - fromSequence
	}
	recentMsgList := msgList[fromSequence : fromSequence+pullNum]
	retMsgList := make([]*proto.ChatInfo, 0)
	for _, chatMsg := range recentMsgList {
		retMsgList = append(retMsgList, g.ConvChatMsgToChatInfo(chatMsg))
	}

	pullPrivateChatRsp := &proto.PullPrivateChatRsp{
		ChatInfo: retMsgList,
	}
	g.SendMsg(cmd.PullPrivateChatRsp, player.PlayerId, player.ClientSeq, pullPrivateChatRsp)
}

// PrivateChatReq 发送私聊消息请求
//
// 处理：
//  1. 文本消息：80 字限制（防刷屏）
//  2. 调 SendPrivateChat 写双方记录 + 推送 + 持久化
//  3. **GM 命令入口**：text 消息额外调 COMMAND_MANAGER.PlayerInputCommand
//     · 玩家私聊"小可爱"AI 时 文本作为 GM 命令解析（如 "item add 1234 5" 不带 / 前缀 格式与原神官方内部 GM 一致）
//     · AI 自动回复执行结果
//
// 图标消息（Icon）：玩家发的表情包/系统图标 不会被识别为命令
func (g *Game) PrivateChatReq(player *model.Player, payloadMsg pb.Message) {
	req := payloadMsg.(*proto.PrivateChatReq)
	targetUid := req.TargetUid
	content := req.Content

	// 根据发送的类型发送消息
	switch content.(type) {
	case *proto.PrivateChatReq_Text:
		text := content.(*proto.PrivateChatReq_Text).Text
		if len(text) == 0 || len(text) > 80 {
			g.SendError(cmd.PrivateChatRsp, player, &proto.PrivateChatRsp{}, proto.Retcode_RET_PRIVATE_CHAT_CONTENT_TOO_LONG)
			return
		}
		// 发送私聊文本消息
		g.SendPrivateChat(player, targetUid, text)
		// 输入命令 会检测是否为命令的
		COMMAND_MANAGER.PlayerInputCommand(player, targetUid, text)
	case *proto.PrivateChatReq_Icon:
		icon := content.(*proto.PrivateChatReq_Icon).Icon
		// 发送私聊图标消息
		g.SendPrivateChat(player, targetUid, icon)
	default:
		return
	}

	g.SendMsg(cmd.PrivateChatRsp, player.PlayerId, player.ClientSeq, new(proto.PrivateChatRsp))
}

func (g *Game) ReadPrivateChatReq(player *model.Player, payloadMsg pb.Message) {
	req := payloadMsg.(*proto.ReadPrivateChatReq)
	targetUid := req.TargetUid

	msgList, exist := player.ChatMsgMap[targetUid]
	if !exist {
		return
	}
	for _, chatMsg := range msgList {
		chatMsg.IsRead = true
	}

	// 更新db
	USER_MANAGER.AsyncWriteDb(func(u *UserManager) {
		u.ReadUserChatMsgToDbSync(player.PlayerId, targetUid)
	})

	g.SendMsg(cmd.ReadPrivateChatRsp, player.PlayerId, player.ClientSeq, new(proto.ReadPrivateChatRsp))
}

// PlayerChatReq 多人世界世界聊天请求
//
// 仅多人世界的"世界频道"聊天（按 ChannelId 区分） 不持久化到 DB（仅内存 world.ChatList）
// 通过 SendToWorldA 广播给世界内所有玩家
//
// 异步写 DB：仅 chatMsg.Uid 和 ToUid 都是真玩家时才写（跳过 AI 玩家间的聊天 减小 DB 压力）
func (g *Game) PlayerChatReq(player *model.Player, payloadMsg pb.Message) {
	req := payloadMsg.(*proto.PlayerChatReq)
	channelId := req.ChannelId
	chatInfo := req.ChatInfo

	sendChatInfo := &proto.ChatInfo{
		Time:    uint32(time.Now().Unix()),
		Uid:     player.PlayerId,
		Content: nil,
	}
	switch chatInfo.Content.(type) {
	case *proto.ChatInfo_Text:
		text := chatInfo.Content.(*proto.ChatInfo_Text).Text
		if len(text) == 0 {
			return
		}
		sendChatInfo.Content = &proto.ChatInfo_Text{
			Text: text,
		}
	case *proto.ChatInfo_Icon:
		icon := chatInfo.Content.(*proto.ChatInfo_Icon).Icon
		sendChatInfo.Content = &proto.ChatInfo_Icon{
			Icon: icon,
		}
	default:
		return
	}

	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		logger.Error("get world is nil, worldId: %v, uid: %v", player.WorldId, player.PlayerId)
		return
	}
	world.AddChat(sendChatInfo)
	chatMsg := g.ConvChatInfoToChatMsg(chatInfo)
	chatMsg.IsDelete = true

	ntf := &proto.PlayerChatNotify{
		ChannelId: channelId,
		ChatInfo:  sendChatInfo,
	}
	g.SendToWorldA(world, cmd.PlayerChatNotify, player.ClientSeq, ntf, 0)

	g.SendMsg(cmd.PlayerChatRsp, player.PlayerId, player.ClientSeq, new(proto.PlayerChatRsp))

	USER_MANAGER.AsyncWriteDb(func(u *UserManager) {
		if chatMsg.Uid < PlayerBaseUid || chatMsg.ToUid < PlayerBaseUid {
			return
		}
		u.SaveUserChatMsgToDbSync(chatMsg)
	})
}

func (g *Game) GetChatEmojiCollectionReq(player *model.Player, payloadMsg pb.Message) {
	req := payloadMsg.(*proto.GetChatEmojiCollectionReq)
	_ = req
	g.SendMsg(cmd.GetChatEmojiCollectionRsp, player.PlayerId, player.ClientSeq, new(proto.GetChatEmojiCollectionRsp))
}

/************************************************** 游戏功能 **************************************************/

// SendPrivateChat 发送私聊消息（核心实现 双方记录+持久化+跨服转发）
//
// 处理：
//  1. 创建 ChatMsg 对象 按 content 类型分（string=文本 uint32=图标）
//  2. 写发送方记录：分配 Sequence（首条 101 后续递增）+ 满 100 条丢弃最旧
//  3. 推送 PrivateChatNotify 给发送方（让客户端立即看到自己的消息）
//  4. 异步写 DB（仅真玩家间的消息 跳过 AI 玩家）
//  5. 接收方处理：
//     · 在本 GS：写接收方记录 + 在线则立即推送
//     · 在远程 GS：MQ 走 ServerChatMsgNotify 转发
//     · 全服离线：仅写发送方记录（接收方上线时 PullPrivateChat 拉取）
func (g *Game) SendPrivateChat(player *model.Player, targetUid uint32, content any) {
	chatMsg := &model.ChatMsg{
		Sequence: 0,
		Time:     uint32(time.Now().Unix()),
		ToUid:    targetUid,
		Uid:      player.PlayerId,
		IsRead:   false,
		IsDelete: false,
	}
	// 根据传入的值判断消息类型
	switch content.(type) {
	case string:
		// 文本消息
		chatMsg.MsgType = model.ChatMsgTypeText
		chatMsg.Text = content.(string)
	case uint32:
		// 图标消息
		chatMsg.MsgType = model.ChatMsgTypeIcon
		chatMsg.Icon = content.(uint32)
	}

	// 消息加入自己的队列
	msgList, exist := player.ChatMsgMap[targetUid]
	// 处理序号
	if !exist {
		msgList = make([]*model.ChatMsg, 0)
		chatMsg.Sequence = 101
	} else {
		chatMsg.Sequence = uint32(len(msgList)) + 101
	}
	if len(msgList) > MaxMsgListLen {
		msgList = msgList[1:]
	}
	msgList = append(msgList, chatMsg)
	player.ChatMsgMap[targetUid] = msgList

	chatInfo := g.ConvChatMsgToChatInfo(chatMsg)

	privateChatNotify := &proto.PrivateChatNotify{
		ChatInfo: chatInfo,
	}
	g.SendMsg(cmd.PrivateChatNotify, player.PlayerId, player.ClientSeq, privateChatNotify)

	USER_MANAGER.AsyncWriteDb(func(u *UserManager) {
		if chatMsg.Uid < PlayerBaseUid || chatMsg.ToUid < PlayerBaseUid {
			return
		}
		u.SaveUserChatMsgToDbSync(chatMsg)
	})

	targetPlayer := USER_MANAGER.GetOnlineUser(targetUid)
	if targetPlayer == nil {
		if USER_MANAGER.GetRemoteUserOnlineState(targetUid) {
			// 目标玩家在别的服在线
			gsAppId := USER_MANAGER.GetRemoteUserGsAppId(targetUid)
			g.messageQueue.SendToGs(gsAppId, &mq.NetMsg{
				MsgType: mq.MsgTypeServer,
				EventId: mq.ServerChatMsgNotify,
				ServerMsg: &mq.ServerMsg{
					ChatMsgInfo: &mq.ChatMsgInfo{
						Time:     chatMsg.Time,
						ToUid:    chatMsg.ToUid,
						Uid:      chatMsg.Uid,
						IsRead:   chatMsg.IsRead,
						MsgType:  chatMsg.MsgType,
						Text:     chatMsg.Text,
						Icon:     chatMsg.Icon,
						IsDelete: chatMsg.IsDelete,
					},
				},
			})
		}
		return
	}

	// 消息加入目标玩家的队列
	msgList, exist = targetPlayer.ChatMsgMap[player.PlayerId]
	if !exist {
		msgList = make([]*model.ChatMsg, 0)
	}
	if len(msgList) > MaxMsgListLen {
		msgList = msgList[1:]
	}
	msgList = append(msgList, chatMsg)
	targetPlayer.ChatMsgMap[player.PlayerId] = msgList

	// 如果目标玩家在线发送消息
	if targetPlayer.Online {
		privateChatNotify := &proto.PrivateChatNotify{
			ChatInfo: chatInfo,
		}
		g.SendMsg(cmd.PrivateChatNotify, targetPlayer.PlayerId, targetPlayer.ClientSeq, privateChatNotify)
	}
}

func (g *Game) ConvChatInfoToChatMsg(chatInfo *proto.ChatInfo) (chatMsg *model.ChatMsg) {
	chatMsg = &model.ChatMsg{
		Sequence: chatInfo.Sequence,
		Time:     chatInfo.Time,
		ToUid:    chatInfo.ToUid,
		Uid:      chatInfo.Uid,
		IsRead:   chatInfo.IsRead,
		MsgType:  0,
		Text:     "",
		Icon:     0,
		IsDelete: false,
	}
	switch chatInfo.Content.(type) {
	case *proto.ChatInfo_Text:
		chatMsg.MsgType = model.ChatMsgTypeText
		chatMsg.Text = chatInfo.Content.(*proto.ChatInfo_Text).Text
	case *proto.ChatInfo_Icon:
		chatMsg.MsgType = model.ChatMsgTypeIcon
		chatMsg.Icon = chatInfo.Content.(*proto.ChatInfo_Icon).Icon
	default:
		logger.Error("chat info content type error, contentType: %T", chatInfo.Content)
	}
	return chatMsg
}

func (g *Game) ConvChatMsgToChatInfo(chatMsg *model.ChatMsg) (chatInfo *proto.ChatInfo) {
	chatInfo = &proto.ChatInfo{
		Time:     chatMsg.Time,
		Sequence: chatMsg.Sequence,
		ToUid:    chatMsg.ToUid,
		Uid:      chatMsg.Uid,
		IsRead:   chatMsg.IsRead,
		Content:  nil,
	}
	switch chatMsg.MsgType {
	case model.ChatMsgTypeText:
		chatInfo.Content = &proto.ChatInfo_Text{
			Text: chatMsg.Text,
		}
	case model.ChatMsgTypeIcon:
		chatInfo.Content = &proto.ChatInfo_Icon{
			Icon: chatMsg.Icon,
		}
	default:
		logger.Error("chat info content type error, msgType: %v", chatMsg.MsgType)
	}
	return chatInfo
}

// 跨服玩家聊天通知

// ServerChatMsgNotify 跨服私聊接收（接收来自其他 GS 的 MQ 消息 转给本 GS 在线玩家）
// 等同于 SendPrivateChat 的"接收方在本 GS"分支 写本 GS 接收方的内存 + 在线推送
// 跨服消息的 DB 写入由发送方所在 GS 负责 这里只更新内存
func (g *Game) ServerChatMsgNotify(chatMsgInfo *mq.ChatMsgInfo) {
	targetPlayer := USER_MANAGER.GetOnlineUser(chatMsgInfo.ToUid)
	if targetPlayer == nil {
		logger.Error("player is nil, uid: %v", chatMsgInfo.ToUid)
		return
	}
	chatMsg := &model.ChatMsg{
		Time:     chatMsgInfo.Time,
		ToUid:    chatMsgInfo.ToUid,
		Uid:      chatMsgInfo.Uid,
		IsRead:   chatMsgInfo.IsRead,
		MsgType:  chatMsgInfo.MsgType,
		Text:     chatMsgInfo.Text,
		Icon:     chatMsgInfo.Icon,
		IsDelete: chatMsgInfo.IsDelete,
	}
	// 消息加入目标玩家的队列
	msgList, exist := targetPlayer.ChatMsgMap[chatMsgInfo.Uid]
	if !exist {
		msgList = make([]*model.ChatMsg, 0)
	}
	if len(msgList) > MaxMsgListLen {
		msgList = msgList[1:]
	}
	msgList = append(msgList, chatMsg)
	targetPlayer.ChatMsgMap[chatMsgInfo.Uid] = msgList

	// 如果目标玩家在线发送消息
	if targetPlayer.Online {
		privateChatNotify := &proto.PrivateChatNotify{
			ChatInfo: g.ConvChatMsgToChatInfo(chatMsg),
		}
		g.SendMsg(cmd.PrivateChatNotify, targetPlayer.PlayerId, targetPlayer.ClientSeq, privateChatNotify)
	}
}

/************************************************** 打包封装 **************************************************/
