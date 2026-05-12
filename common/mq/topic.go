package mq

import (
	"hk4e/node/api"
)

// MQ 路由 Topic 命名规范 + 6 种 SendTo 便利方法
//
// Topic 格式：{ServerType}_{AppId}_HK4E
//   例：GATE_3a4f2c1b_HK4E（向某具体 Gate 发）
//   广播 topic 固定 ALL_SERVER_HK4E（订阅方都能收到）
//
// 6 种 SendTo* 方法：分别对应 6 种服务类型 + 全局广播
//   每个方法填入 Topic / ServerType / AppId / 来源信息 → 投递到 netMsgInput

// getOriginServer 取本服务的标识（用于消息来源标记）
func (m *MessageQueue) getOriginServer() (originServerType string, originServerAppId string) {
	originServerType = m.serverType
	originServerAppId = m.appId
	return originServerType, originServerAppId
}

func (m *MessageQueue) getTopic(serverType string, appId string) string {
	topic := serverType + "_" + appId + "_" + "HK4E"
	return topic
}

func (m *MessageQueue) SendToGate(appId string, netMsg *NetMsg) {
	netMsg.Topic = m.getTopic(api.GATE, appId)
	netMsg.ServerType = api.GATE
	netMsg.AppId = appId
	originServerType, originServerAppId := m.getOriginServer()
	netMsg.OriginServerType = originServerType
	netMsg.OriginServerAppId = originServerAppId
	m.netMsgInput <- netMsg
}

func (m *MessageQueue) SendToGs(appId string, netMsg *NetMsg) {
	netMsg.Topic = m.getTopic(api.GS, appId)
	netMsg.ServerType = api.GS
	netMsg.AppId = appId
	originServerType, originServerAppId := m.getOriginServer()
	netMsg.OriginServerType = originServerType
	netMsg.OriginServerAppId = originServerAppId
	m.netMsgInput <- netMsg
}

func (m *MessageQueue) SendToMulti(appId string, netMsg *NetMsg) {
	netMsg.Topic = m.getTopic(api.MULTI, appId)
	netMsg.ServerType = api.MULTI
	netMsg.AppId = appId
	originServerType, originServerAppId := m.getOriginServer()
	netMsg.OriginServerType = originServerType
	netMsg.OriginServerAppId = originServerAppId
	m.netMsgInput <- netMsg
}

func (m *MessageQueue) SendToRobot(appId string, netMsg *NetMsg) {
	netMsg.Topic = m.getTopic(api.ROBOT, appId)
	netMsg.ServerType = api.ROBOT
	netMsg.AppId = appId
	originServerType, originServerAppId := m.getOriginServer()
	netMsg.OriginServerType = originServerType
	netMsg.OriginServerAppId = originServerAppId
	m.netMsgInput <- netMsg
}

func (m *MessageQueue) SendToDispatch(appId string, netMsg *NetMsg) {
	netMsg.Topic = m.getTopic(api.DISPATCH, appId)
	netMsg.ServerType = api.DISPATCH
	netMsg.AppId = appId
	originServerType, originServerAppId := m.getOriginServer()
	netMsg.OriginServerType = originServerType
	netMsg.OriginServerAppId = originServerAppId
	m.netMsgInput <- netMsg
}

// SendToAll 全服广播（必走 NATS 不能走 TCP 直连）
// 用例：停服通知 / 全服 GM 命令 / 全服公告
// 自己也会收到 但 natsMsgRecvHandler 会过滤掉自己发的（OriginServerType+AppId 比对）
func (m *MessageQueue) SendToAll(netMsg *NetMsg) {
	netMsg.Topic = "ALL_SERVER_HK4E"
	netMsg.ServerType = "ALL_SERVER_HK4E"
	originServerType, originServerAppId := m.getOriginServer()
	netMsg.OriginServerType = originServerType
	netMsg.OriginServerAppId = originServerAppId
	m.netMsgInput <- netMsg
}
