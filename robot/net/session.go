package net

import (
	"sync"
	"sync/atomic"
	"time"

	"hk4e/common/config"
	hk4egatenet "hk4e/gate/net"
	"hk4e/protocol/cmd"
	"hk4e/protocol/proto"

	"github.com/flswld/halo/logger"
	"github.com/flswld/halo/protocol/kcp"
	pb "google.golang.org/protobuf/proto"
)

// Robot 模拟客户端的 KCP Session
//
// 与 gate/net/session.go 是镜像关系：
//   - gate Session 是服务端视角（accept 入站连接）
//   - robot Session 是客户端视角（dial 出站连接）
//
// 两边共用 hk4egatenet 包的协议编解码（KCP 编解码 + ClientProtoProxy）
//
// 字段说明：
//   - Conn: KCP 客户端会话
//   - XorKey: 通信加密密钥（初始用 dispatchKey 握手后切换为 SecretKey）
//   - SendChan/RecvChan: 收发通道 sendHandle/recvHandle 协程读写
//   - ServerCmdProtoMap: 服务端 3.2 cmd ↔ proto 映射（与 gate 一致）
//   - ClientProtoProxy: 客户端协议代理（多版本部署时启用 与 gate 一致）
//   - ClientVersionRandomKey: GetPlayerTokenRsp 给的版本校验种子
//   - SecurityCmdBuffer: 反作弊回包缓存（项目不实际校验 但客户端要带这个字段）
//   - Uid: 玩家 uid（PlayerLoginRsp 后写入）
type Session struct {
	Conn                   *kcp.UDPSession
	XorKey                 []byte
	SendChan               chan *hk4egatenet.ProtoMsg
	RecvChan               chan *hk4egatenet.ProtoMsg
	ServerCmdProtoMap      *cmd.CmdProtoMap
	ClientProtoProxy       *hk4egatenet.ClientProtoProxy
	ClientSeq              uint32
	DeadEvent              chan struct{}
	ClientVersionRandomKey string
	SecurityCmdBuffer      []byte
	Uid                    uint32
	CloseOnce              sync.Once
}

// NewSession 创建 KCP 客户端会话连到 Gate
//
// KCP 参数与 gate 端一致：
//   - SetACKNoDelay(true): 立即 ACK（实时性优先）
//   - SetWriteDelay(false): 不延迟发送
//   - SetWindowSize(256, 256): 收发窗口 256 包
//   - SetMtu(1200): 单包最大 1200 字节（避免 IP 分片）
//
// 启动 recvHandle/sendHandle 双 goroutine 处理收发
func NewSession(gateAddr string, dispatchKey []byte) (*Session, error) {
	conn, err := kcp.DialKCP(gateAddr)
	if err != nil {
		logger.Error("kcp client conn to server error: %v", err)
		return nil, err
	}
	kcp.SetByteCheckMode(int(config.GetConfig().Hk4e.ByteCheckMode))
	conn.SetACKNoDelay(true)
	conn.SetWriteDelay(false)
	conn.SetWindowSize(256, 256)
	conn.SetMtu(1200)
	r := &Session{
		Conn:                   conn,
		XorKey:                 dispatchKey,
		SendChan:               make(chan *hk4egatenet.ProtoMsg, 1000),
		RecvChan:               make(chan *hk4egatenet.ProtoMsg, 1000),
		ServerCmdProtoMap:      cmd.NewCmdProtoMap(),
		ClientSeq:              0,
		DeadEvent:              make(chan struct{}),
		ClientVersionRandomKey: "",
		SecurityCmdBuffer:      nil,
		Uid:                    0,
	}
	if config.GetConfig().Hk4e.ClientProtoProxyEnable {
		r.ClientProtoProxy = hk4egatenet.NewClientProtoProxy(config.GetConfig().Hk4e.ClientProtoDir)
	}
	go r.recvHandle()
	go r.sendHandle()
	return r, nil
}

func (s *Session) SendMsg(cmdId uint16, msg pb.Message) {
	s.SendChan <- &hk4egatenet.ProtoMsg{
		SessionId: 0,
		CmdId:     cmdId,
		HeadMessage: &proto.PacketHead{
			ClientSequenceId: atomic.AddUint32(&s.ClientSeq, 1),
			SentMs:           uint64(time.Now().UnixMilli()),
			EnetIsReliable:   1,
		},
		PayloadMessage: msg,
	}
}

// Close 关闭会话（CloseOnce 防重入）
// 关闭 KCP 连接 + 关闭 DeadEvent 让 client.Logic 退出
func (s *Session) Close() {
	s.CloseOnce.Do(func() {
		_ = s.Conn.Close()
		close(s.DeadEvent)
	})
}

// recvHandle 客户端接收循环
// 与 gate 的 recvHandle 镜像：读取 KCP 数据 → DecodeBinToPayload 拆包 → ProtoDecode 解析
// 解析后塞入 RecvChan 让 client.Logic 处理
func (s *Session) recvHandle() {
	logger.Info("recv handle start")
	conn := s.Conn
	convId := conn.GetConv()
	recvBuf := make([]byte, hk4egatenet.PacketMaxLen)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(time.Second * hk4egatenet.ConnRecvTimeout))
		recvLen, err := conn.Read(recvBuf)
		if err != nil {
			logger.Error("exit recv loop, conn read err: %v, convId: %v", err, convId)
			s.Close()
			break
		}
		recvData := recvBuf[:recvLen]
		kcpMsgList := make([]*hk4egatenet.KcpMsg, 0)
		hk4egatenet.DecodeBinToPayload(recvData, convId, &kcpMsgList, s.XorKey)
		for _, v := range kcpMsgList {
			protoMsgList := hk4egatenet.ProtoDecode(v, s.ServerCmdProtoMap, s.ClientProtoProxy)
			for _, vv := range protoMsgList {
				s.RecvChan <- vv
			}
		}
	}
}

// sendHandle 客户端发送循环
// 从 SendChan 读 ProtoMsg → ProtoEncode 编码 → EncodePayloadToBin XOR 加密 → KCP Write
func (s *Session) sendHandle() {
	logger.Info("send handle start")
	conn := s.Conn
	convId := conn.GetConv()
	for {
		protoMsg, ok := <-s.SendChan
		if !ok {
			logger.Error("exit send loop, send chan close, convId: %v", convId)
			s.Close()
			break
		}
		kcpMsg := hk4egatenet.ProtoEncode(protoMsg, s.ServerCmdProtoMap, s.ClientProtoProxy)
		if kcpMsg == nil {
			logger.Error("decode kcp msg is nil, convId: %v", convId)
			continue
		}
		bin := hk4egatenet.EncodePayloadToBin(kcpMsg, s.XorKey)
		_ = conn.SetWriteDeadline(time.Now().Add(time.Second * hk4egatenet.ConnSendTimeout))
		_, err := conn.Write(bin)
		if err != nil {
			logger.Error("exit send loop, conn write err: %v, convId: %v", err, convId)
			s.Close()
			break
		}
	}
}
