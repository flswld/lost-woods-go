package mq

import (
	"context"
	"encoding/binary"
	"net"
	"strconv"
	"strings"
	"time"

	"hk4e/common/config"
	"hk4e/common/rpc"
	"hk4e/node/api"
	"hk4e/protocol/cmd"

	"github.com/flswld/halo/logger"
	"github.com/nats-io/nats.go"
	"github.com/vmihailenco/msgpack/v5"
	pb "google.golang.org/protobuf/proto"
)

// 服务间消息队列 - 数据平面 + 控制平面通信
//
// **双通道架构**（详见 CLAUDE.md "TCP 直连优先 NATS 兜底"）：
//   - TCP 直连：GS/Multi/Robot ↔ Gate 的高频消息（玩家移动/技能/聊天等）
//     · GS 启动时建立到所有 Gate 的 TCP 长连接 每分钟同步一次 Gate 列表
//     · 单一长连接 多路复用 减少连接开销 + 极低延迟
//   - NATS：广播消息 + TCP 不通时的降级路径
//     · 广播只能走 NATS（topic ALL_SERVER_HK4E）
//
// Topic 格式：{serverType}_{appId}_HK4E（详见 topic.go）
//
// **不要用这个来做 RPC**！异步回调会让代码很难维护
//   要 RPC 用专门的 NATSRPC（gs/api/* 中定义的接口）
//
// 消息序列化：msgpack（紧凑高效）NetMsg 整体编码后通过 NATS/TCP 传输

type MessageQueue struct {
	natsConn               *nats.Conn
	natsMsgChan            chan *nats.Msg
	netMsgInput            chan *NetMsg
	netMsgOutput           chan *NetMsg
	cmdProtoMap            *cmd.CmdProtoMap
	serverType             string
	appId                  string
	gateTcpMqEventChan     chan *GateTcpMqEvent
	gateTcpMqDeadEventChan chan string
	discoveryClient        *rpc.DiscoveryClient
}

// NewMessageQueue 创建消息队列（每个服务启动时调一次）
//
// 启动流程：
//  1. 连 NATS（必备 失败返回 nil 服务无法启动）
//  2. 订阅自己的 topic（{serverType}_{appId}_HK4E）
//  3. 订阅广播 topic（ALL_SERVER_HK4E）
//  4. 启动 TCP 直连：
//     · GATE 启动 server（监听其他服连接进来）
//     · GS/MULTI/ROBOT 启动 client（连到所有 Gate）
//     · DISPATCH/NODE/GM 不需要 TCP 直连（不走数据平面）
//  5. 启动 natsMsgRecvHandler / sendHandler 两个 goroutine
//
// netMsgInput: 业务代码调 SendToXxx 把消息放进来
// netMsgOutput: 业务代码调 GetNetMsg() 从这个 chan 读到来的消息
func NewMessageQueue(serverType string, appId string, discoveryClient *rpc.DiscoveryClient) (r *MessageQueue) {
	r = new(MessageQueue)
	conn, err := nats.Connect(config.GetConfig().MQ.NatsUrl)
	if err != nil {
		logger.Error("connect nats error: %v", err)
		return nil
	}
	r.natsConn = conn
	r.natsMsgChan = make(chan *nats.Msg, 1000)
	_, err = r.natsConn.ChanSubscribe(r.getTopic(serverType, appId), r.natsMsgChan)
	if err != nil {
		logger.Error("nats subscribe error: %v", err)
		return nil
	}
	_, err = r.natsConn.ChanSubscribe("ALL_SERVER_HK4E", r.natsMsgChan)
	if err != nil {
		logger.Error("nats subscribe error: %v", err)
		return nil
	}
	r.netMsgInput = make(chan *NetMsg, 1000)
	r.netMsgOutput = make(chan *NetMsg, 1000)
	r.cmdProtoMap = cmd.NewCmdProtoMap()
	r.serverType = serverType
	r.appId = appId
	r.gateTcpMqEventChan = make(chan *GateTcpMqEvent, 1000)
	r.gateTcpMqDeadEventChan = make(chan string, 1000)
	r.discoveryClient = discoveryClient
	if serverType == api.GATE {
		go r.runGateTcpMqServer()
	} else if serverType == api.GS || serverType == api.MULTI || serverType == api.ROBOT {
		go r.runGateTcpMqClient()
	}
	go r.natsMsgRecvHandler()
	go r.sendHandler()
	return r
}

func (m *MessageQueue) Close() {
	// 等待所有待发送的消息发送完毕
	for {
		if len(m.netMsgInput) == 0 {
			time.Sleep(time.Millisecond * 100)
			break
		}
		time.Sleep(time.Millisecond * 100)
	}
	m.natsConn.Close()
}

func (m *MessageQueue) GetNetMsg() chan *NetMsg {
	return m.netMsgOutput
}

// natsMsgRecvHandler NATS 消息接收 goroutine
// 把 NATS 收到的消息反序列化后塞入 netMsgOutput 让业务代码读
// 自己发出的广播消息会被收到 这里通过 OriginServerType+AppId 比对自己 跳过
func (m *MessageQueue) natsMsgRecvHandler() {
	for {
		natsMsg := <-m.natsMsgChan
		rawData := natsMsg.Data
		netMsg := m.parseNetMsg(rawData)
		if netMsg == nil {
			continue
		}
		// 忽略自己发出的广播消息
		if netMsg.OriginServerType == m.serverType && netMsg.OriginServerAppId == m.appId {
			continue
		}
		m.netMsgOutput <- netMsg
	}
}

func (m *MessageQueue) buildNetMsg(netMsg *NetMsg) []byte {
	switch netMsg.MsgType {
	case MsgTypeGame:
		gameMsg := netMsg.GameMsg
		if gameMsg == nil {
			logger.Error("send game msg is nil")
			return nil
		}
		if gameMsg.PayloadMessageData == nil {
			// protobuf PayloadMessage
			payloadMessageData, err := pb.Marshal(gameMsg.PayloadMessage)
			if err != nil {
				logger.Error("parse payload msg to bin error: %v", err)
				return nil
			}
			gameMsg.PayloadMessageData = payloadMessageData
		}
	default:
	}
	// msgpack NetMsg
	rawData, err := msgpack.Marshal(netMsg)
	if err != nil {
		logger.Error("parse net msg to bin error: %v", err)
		return nil
	}
	return rawData
}

func (m *MessageQueue) parseNetMsg(rawData []byte) *NetMsg {
	// msgpack NetMsg
	netMsg := new(NetMsg)
	err := msgpack.Unmarshal(rawData, netMsg)
	if err != nil {
		logger.Error("parse bin to net msg error: %v", err)
		return nil
	}
	switch netMsg.MsgType {
	case MsgTypeGame:
		gameMsg := netMsg.GameMsg
		if gameMsg == nil {
			logger.Error("recv game msg is nil")
			return nil
		}
		if netMsg.EventId == NormalMsg {
			if !gameMsg.NotParse {
				// protobuf PayloadMessage
				payloadMessage := m.cmdProtoMap.GetProtoObjFastNewByCmdId(gameMsg.CmdId)
				if payloadMessage == nil {
					logger.Error("get protobuf obj by cmd id error: %v", err)
					return nil
				}
				err = pb.Unmarshal(gameMsg.PayloadMessageData, payloadMessage)
				if err != nil {
					logger.Error("parse bin to payload msg error: %v", err)
					return nil
				}
				gameMsg.PayloadMessage = payloadMessage
			} else {
				payloadMessageData := make([]byte, len(gameMsg.PayloadMessageData))
				copy(payloadMessageData, gameMsg.PayloadMessageData)
				gameMsg.PayloadMessageData = payloadMessageData
			}
		}
	default:
	}
	return netMsg
}

// sendHandler 消息发送 goroutine（核心调度逻辑 含 TCP 直连 vs NATS 降级）
//
// 处理路径：
//  1. 广播消息（ServerType=ALL_SERVER_HK4E）：必走 NATS
//  2. 有目标 AppId 的消息：
//     · 优先查 gateTcpMqInstMap 找 TCP 直连
//     · TCP 写成功 → 直接走 TCP（高吞吐 低延迟）
//     · TCP 写失败 → 关闭连接 + fallback 到 NATS
//     · 没找到 TCP → 直接走 NATS
//
// TCP 包格式：4 字节大端长度前缀 + msgpack 数据
// gateTcpMqEventChan 处理 TCP 连接的建立/断开事件 维护 instMap
func (m *MessageQueue) sendHandler() {
	// 网关tcp连接消息收发快速通道 key1:服务器类型 key2:服务器appid value:连接实例
	gateTcpMqInstMap := map[string]map[string]*GateTcpMqInst{
		api.GATE:  make(map[string]*GateTcpMqInst),
		api.GS:    make(map[string]*GateTcpMqInst),
		api.MULTI: make(map[string]*GateTcpMqInst),
		api.ROBOT: make(map[string]*GateTcpMqInst),
	}
	for {
		select {
		case netMsg := <-m.netMsgInput:
			rawData := m.buildNetMsg(netMsg)
			if rawData == nil {
				continue
			}
			fallbackNatsMqSend := func() {
				// 找不到tcp快速通道就fallback回nats
				natsMsg := nats.NewMsg(netMsg.Topic)
				natsMsg.Data = rawData
				err := m.natsConn.PublishMsg(natsMsg)
				if err != nil {
					logger.Error("nats publish msg error: %v", err)
					return
				}
			}
			// 广播消息只能走nats
			if netMsg.ServerType == "ALL_SERVER_HK4E" {
				fallbackNatsMqSend()
				continue
			}
			// 有tcp快速通道就走快速通道
			instMap, exist := gateTcpMqInstMap[netMsg.ServerType]
			if !exist {
				logger.Error("unknown server type: %v", netMsg.ServerType)
				fallbackNatsMqSend()
				continue
			}
			inst, exist := instMap[netMsg.AppId]
			if !exist {
				fallbackNatsMqSend()
				continue
			}
			// 前4个字节为消息的载荷部分长度
			data := make([]byte, 4+len(rawData))
			binary.BigEndian.PutUint32(data[0:4], uint32(len(rawData)))
			copy(data[4:], rawData)
			err := inst.conn.SetWriteDeadline(time.Now().Add(time.Second))
			if err != nil {
				fallbackNatsMqSend()
				continue
			}
			_, err = inst.conn.Write(data)
			if err != nil {
				// 发送失败关闭连接fallback回nats
				logger.Error("gate tcp mq send error: %v", err)
				_ = inst.conn.Close()
				m.gateTcpMqEventChan <- &GateTcpMqEvent{
					event: EventDisconnect,
					inst:  inst,
				}
				fallbackNatsMqSend()
				continue
			}
		case gateTcpMqEvent := <-m.gateTcpMqEventChan:
			inst := gateTcpMqEvent.inst
			switch gateTcpMqEvent.event {
			case EventConnect:
				logger.Warn("gate tcp mq connect, addr: %v, server type: %v, appid: %v", inst.conn.RemoteAddr().String(), inst.serverType, inst.appId)
				gateTcpMqInstMap[inst.serverType][inst.appId] = inst
			case EventDisconnect:
				logger.Warn("gate tcp mq disconnect, addr: %v, server type: %v, appid: %v", inst.conn.RemoteAddr().String(), inst.serverType, inst.appId)
				delete(gateTcpMqInstMap[inst.serverType], inst.appId)
				m.gateTcpMqDeadEventChan <- inst.conn.RemoteAddr().String()
			}
		}
	}
}

type GateTcpMqInst struct {
	conn       *net.TCPConn
	serverType string
	appId      string
}

const (
	EventConnect = iota
	EventDisconnect
)

type GateTcpMqEvent struct {
	event int
	inst  *GateTcpMqInst
}

// runGateTcpMqServer GATE 启动 TCP MQ 服务端（监听其他服连接进来）
// 监听端口 GateTcpMqPort（默认 33333）
// 接受连接 → 握手（确认对方身份和 appid）→ 注册到 instMap → 启动接收 goroutine
func (m *MessageQueue) runGateTcpMqServer() {
	addr, err := net.ResolveTCPAddr("tcp4", "0.0.0.0:"+strconv.Itoa(int(config.GetConfig().Hk4e.GateTcpMqPort)))
	if err != nil {
		logger.Error("gate tcp mq parse port error: %v", err)
		return
	}
	listener, err := net.ListenTCP("tcp4", addr)
	if err != nil {
		logger.Error("gate tcp mq listen error: %v", err)
		return
	}
	for {
		conn, err := listener.AcceptTCP()
		if err != nil {
			logger.Error("gate tcp mq accept error: %v", err)
			return
		}
		_ = conn.SetNoDelay(true)
		logger.Info("accept gate tcp mq, server addr: %v", conn.RemoteAddr().String())
		go m.gateTcpMqHandshake(conn)
	}
}

func (m *MessageQueue) gateTcpMqHandshake(conn *net.TCPConn) {
	recvBuf := make([]byte, 1500)
	recvLen, err := conn.Read(recvBuf)
	if err != nil {
		logger.Error("handshake packet recv error: %v", err)
		return
	}
	recvBuf = recvBuf[:recvLen]
	serverMetaData := string(recvBuf)
	// 握手包格式 服务器类型@appid
	split := strings.Split(serverMetaData, "@")
	if len(split) != 2 {
		logger.Error("handshake packet format error")
		return
	}
	inst := &GateTcpMqInst{
		conn:       conn,
		serverType: "",
		appId:      "",
	}
	switch split[0] {
	case api.GATE:
		inst.serverType = api.GATE
	case api.GS:
		inst.serverType = api.GS
	case api.MULTI:
		inst.serverType = api.MULTI
	case api.ROBOT:
		inst.serverType = api.ROBOT
	default:
		logger.Error("invalid server type")
		return
	}
	if len(split[1]) != 8 {
		logger.Error("invalid appid")
		return
	}
	inst.appId = split[1]
	go m.gateTcpMqRecvHandle(inst)
	m.gateTcpMqEventChan <- &GateTcpMqEvent{
		event: EventConnect,
		inst:  inst,
	}
}

// runGateTcpMqClient GS/MULTI/ROBOT 启动 TCP MQ 客户端
//
// 处理：
//  1. 每分钟从 Node 拉取所有 Gate 的 MqAddr/MqPort
//  2. 对每个 Gate 发起 TCP 连接 + 握手
//  3. 维护一个 gateServerConnAddrMap 防重复连同一 Gate
//  4. 死连接事件（gateTcpMqDeadEventChan）触发后从 map 中移除 等下次扫描重连
func (m *MessageQueue) runGateTcpMqClient() {
	// 已存在的GATE连接列表
	gateServerConnAddrMap := make(map[string]bool)
	m.gateTcpMqConn(gateServerConnAddrMap)
	ticker := time.NewTicker(time.Minute)
	for {
		select {
		case addr := <-m.gateTcpMqDeadEventChan:
			// GATE连接断开
			delete(gateServerConnAddrMap, addr)
		case <-ticker.C:
			// 定时获取全部GATE实例地址并建立连接
			m.gateTcpMqConn(gateServerConnAddrMap)
		}
	}
}

func (m *MessageQueue) gateTcpMqConn(gateServerConnAddrMap map[string]bool) {
	rsp, err := m.discoveryClient.GetAllGateServerInfoList(context.TODO(), new(api.NullMsg))
	if err != nil {
		logger.Error("gate tcp mq get gate list error: %v", err)
		return
	}
	for _, gateServerInfo := range rsp.GateServerInfoList {
		gateServerAddr := gateServerInfo.MqAddr + ":" + strconv.Itoa(int(gateServerInfo.MqPort))
		_, exist := gateServerConnAddrMap[gateServerAddr]
		// GATE连接已存在
		if exist {
			logger.Info("gate tcp mq conn already exist addr: %v", gateServerAddr)
			continue
		}
		addr, err := net.ResolveTCPAddr("tcp4", gateServerAddr)
		if err != nil {
			logger.Error("gate tcp mq parse addr error: %v", err)
			return
		}
		conn, err := net.DialTCP("tcp4", nil, addr)
		if err != nil {
			logger.Error("gate tcp mq conn error: %v", err)
			return
		}
		_ = conn.SetNoDelay(true)
		_, err = conn.Write([]byte(m.serverType + "@" + m.appId))
		if err != nil {
			logger.Error("gate tcp mq handshake send error: %v", err)
			return
		}
		inst := &GateTcpMqInst{
			conn:       conn,
			serverType: api.GATE,
			appId:      gateServerInfo.AppId,
		}
		m.gateTcpMqEventChan <- &GateTcpMqEvent{
			event: EventConnect,
			inst:  inst,
		}
		gateServerConnAddrMap[gateServerAddr] = true
		logger.Info("connect gate tcp mq, gate addr: %v", conn.RemoteAddr().String())
		go m.gateTcpMqRecvHandle(inst)
	}
}

func (m *MessageQueue) gateTcpMqRecvHandle(inst *GateTcpMqInst) {
	header := make([]byte, 4)
	payload := make([]byte, 1024)
	for {
		// 读取头部的消息长度
		recvLen := 0
		for recvLen < 4 {
			n, err := inst.conn.Read(header[recvLen:])
			if err != nil {
				logger.Error("gate tcp mq recv error: %v", err)
				m.gateTcpMqEventChan <- &GateTcpMqEvent{
					event: EventDisconnect,
					inst:  inst,
				}
				_ = inst.conn.Close()
				return
			}
			recvLen += n
		}
		msgLen := binary.BigEndian.Uint32(header)
		// 读取消息体
		if len(payload) < int(msgLen) {
			payload = make([]byte, msgLen)
		}
		recvLen = 0
		for recvLen < int(msgLen) {
			n, err := inst.conn.Read(payload[recvLen:msgLen])
			if err != nil {
				logger.Error("gate tcp mq recv error: %v", err)
				m.gateTcpMqEventChan <- &GateTcpMqEvent{
					event: EventDisconnect,
					inst:  inst,
				}
				_ = inst.conn.Close()
				return
			}
			recvLen += n
		}
		netMsg := m.parseNetMsg(payload[:msgLen])
		if netMsg != nil {
			m.netMsgOutput <- netMsg
		}
	}
}
