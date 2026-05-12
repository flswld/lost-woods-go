package service

import (
	"hk4e/common/mq"
	"hk4e/node/api"
	"hk4e/node/dao"

	"github.com/byebyebruce/natsrpc"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/encoders/protobuf"
)

// Service Node 服务封装（natsrpc Server 容器）
//
// 注册的 RPC 接口仅 1 个：DiscoveryNATSRPCServer
// 暴露的 RPC 方法在 discovery.go 详见 DiscoveryService 的方法
//
// natsrpc 序列化用 protobuf 编码（PROTOBUF_ENCODER）
// 接口定义文件：node/api/api.proto → 生成 api.natsrpc.pb.go + api.pb.go

type Service struct {
	db               *dao.Dao
	discoveryService *DiscoveryService
}

// NewService Node natsrpc Server 启动
//
// 处理：
//  1. 用 protobuf encoder 包装 NATS 连接
//  2. 创建 natsrpc.Server
//  3. 创建 DiscoveryService（含 DB / Region / 后台 goroutine）
//  4. 注册到 natsrpc Server（RegisterDiscoveryNATSRPCServer）
//
// 之后其他服务通过 DiscoveryClient 调用 RegisterServer/KeepaliveServer 等方法
func NewService(db *dao.Dao, conn *nats.Conn, messageQueue *mq.MessageQueue) (*Service, error) {
	enc, err := nats.NewEncodedConn(conn, protobuf.PROTOBUF_ENCODER)
	if err != nil {
		return nil, err
	}
	svr, err := natsrpc.NewServer(enc)
	if err != nil {
		return nil, err
	}
	discoveryService, err := NewDiscoveryService(db, messageQueue)
	if err != nil {
		return nil, err
	}
	_, err = api.RegisterDiscoveryNATSRPCServer(svr, discoveryService)
	if err != nil {
		return nil, err
	}
	s := &Service{
		db:               db,
		discoveryService: discoveryService,
	}
	return s, nil
}

func (s *Service) Close() {
	s.discoveryService.close()
}
