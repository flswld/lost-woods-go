package mq

import (
	pb "google.golang.org/protobuf/proto"
)

// 服务间消息体定义 - 跨服务通信的统一封装
//
// 三层结构：
//   外层 NetMsg 含 MsgType + EventId + 三种消息体之一（GameMsg/ConnCtrlMsg/ServerMsg）
//   内层是具体业务数据（玩家消息/连接控制/跨服业务）
//
// 序列化：msgpack 编码（高效紧凑 比 JSON 快几倍）
// 双通道传输：NATS（广播）+ TCP 直连（高吞吐）详见 nats.go
//
// 三大 MsgType：
//   - MsgTypeGame: 客户端 ↔ GS 的游戏业务消息（gate 转发用）
//   - MsgTypeConnCtrl: 连接控制（踢人/RTT 上报/玩家下线）
//   - MsgTypeServer: 跨服业务（多人世界/聊天/好友/停服/GM 等）
//
// EventId 在不同 MsgType 下含义不同（详见各 const 组定义）

const (
	MsgTypeGame     = iota // 来自客户端的游戏消息（GameMsg）
	MsgTypeConnCtrl        // GATE客户端连接信息消息（ConnCtrlMsg）
	MsgTypeServer          // 服务器之间转发的消息（ServerMsg）
)

// NetMsg 跨服务消息体（外层封装）
//
// 标记 msgpack:"-" 的字段不参与序列化（仅本地路由用）：
//   - ServerType/AppId/Topic: 发送时填 接收方不需要
//
// OriginServerType/OriginServerAppId: 序列化字段 接收方据此知道消息来源
//
//	用于：GS 收到消息后回包时知道发回哪个 Gate；多 GS 部署时统计来源等
type NetMsg struct {
	MsgType           uint8
	EventId           uint16
	ServerType        string `msgpack:"-"` // 目标服务类型（仅发送方用）
	AppId             string `msgpack:"-"` // 目标 AppId（仅发送方用）
	Topic             string `msgpack:"-"` // NATS topic（仅发送方用）
	GameMsg           *GameMsg
	ConnCtrlMsg       *ConnCtrlMsg
	ServerMsg         *ServerMsg
	OriginServerType  string // 来源服务类型
	OriginServerAppId string // 来源 AppId
}

const (
	NormalMsg = iota // 正常的游戏消息
)

// GameMsg 客户端游戏业务消息（gate ↔ GS 转发用）
//
// 序列化策略：
//   - PayloadMessage 是 protobuf 对象（msgpack 不能直接序列化 标记 -）
//   - 实际传输前 gate 会先调 pb.Marshal 把对象序列化为 PayloadMessageData
//   - GS 收到后调 pb.Unmarshal 反序列化回对象
//
// 这样隔离的原因：避免 PayloadMessage 内部指针被并发访问（详见 gs/game/game.go:381）
//
// NotParse: 是否跳过解析（某些情况只转发不解析 提升性能）
type GameMsg struct {
	UserId             uint32
	CmdId              uint16
	ClientSeq          uint32
	PayloadMessage     pb.Message `msgpack:"-"` // proto 对象 仅本地用
	PayloadMessageData []byte     // 序列化后的字节 跨服务传输用
	NotParse           bool
}

const (
	ClientRttNotify   = iota // 客户端网络时延上报
	KickPlayerNotify         // 通知GATE剔除玩家
	UserOfflineNotify        // 玩家离线通知GS
)

type ConnCtrlMsg struct {
	UserId     uint32
	ClientRtt  uint32
	KickUserId uint32
	KickReason uint32
}

// MsgTypeServer 的 EventId（跨服业务事件）
//
// 这些事件支撑了项目的核心跨服功能：
//   - 跨服玩家迁移（多人世界）：GsChangeNotify + PlayerMpReq/Rsp
//   - 跨服聊天：ChatMsgNotify
//   - 跨服好友：AddFriendNotify
//   - 全服在线状态：OnlineStateChangeNotify（广播 由 Node 维护 globalGsOnlineMap）
//   - 全服管理：StopNotify / DispatchCancelNotify / GmCmdNotify（广播）
const (
	ServerAppidBindNotify             = iota // gate 通知 GS 玩家所在的 multi 服 appid（让 GS 知道反作弊路由地址）
	ServerUserOnlineStateChangeNotify        // GS 广播玩家上线/离线 Node 维护全局在线表
	ServerUserGsChangeNotify                 // 跨服迁移：让 Gate 把会话切到目标 GS（详见 CLAUDE.md "跨服无感迁移"）
	ServerPlayerMpReq                        // 跨服多人世界请求（A 给 B 敲门 / B 同意 / A 加入）
	ServerPlayerMpRsp                        // 跨服多人世界响应
	ServerChatMsgNotify                      // 跨服私聊（详见 CLAUDE.md "聊天记录细节"）
	ServerAddFriendNotify                    // 跨服好友申请/同意
	ServerStopNotify                         // Node 通知所有服务停服（GM 后台触发）
	ServerDispatchCancelNotify               // Node 通知所有服务取消指定版本调度（滚动升级用）
	ServerGmCmdNotify                        // GM 后台广播命令到指定 GS / 全服
)

type ServerMsg struct {
	MultiServerAppId string
	UserId           uint32
	IsOnline         bool
	GameServerAppId  string
	JoinHostUserId   uint32
	PlayerMpInfo     *PlayerMpInfo
	ChatMsgInfo      *ChatMsgInfo
	AddFriendInfo    *AddFriendInfo
	AppVersion       string
	GmCmdFuncName    string
	GmCmdParamList   []string
}

type OriginInfo struct {
	CmdName string
	UserId  uint32
}

type PlayerBaseInfo struct {
	UserId         uint32
	Nickname       string
	PlayerLevel    uint32
	MpSettingType  uint8
	NameCardId     uint32
	Signature      string
	HeadImageId    uint32
	WorldPlayerNum uint32
	WorldLevel     uint32
}

type PlayerMpInfo struct {
	OriginInfo            *OriginInfo
	HostUserId            uint32
	ApplyUserId           uint32
	ApplyPlayerOnlineInfo *PlayerBaseInfo
	ApplyOk               bool
	Agreed                bool
	Reason                int32
	HostNickname          string
}

type ChatMsgInfo struct {
	Time     uint32
	ToUid    uint32
	Uid      uint32
	IsRead   bool
	MsgType  uint8
	Text     string
	Icon     uint32
	IsDelete bool
}

type AddFriendInfo struct {
	OriginInfo            *OriginInfo
	TargetUserId          uint32
	ApplyPlayerOnlineInfo *PlayerBaseInfo
}
