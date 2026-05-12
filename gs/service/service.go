package service

import (
	"hk4e/gs/api"

	"github.com/byebyebruce/natsrpc"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/encoders/protobuf"
)

// GS 对外 RPC 服务（natsrpc 实现 走 NATS 通道）
//
// 当前仅 1 个 RPC 服务：
//   - GMService: 接受来自 GM 后台 / 其他 GS 的 GM 命令调用
//
// 注册方式：通过 api.RegisterGMNATSRPCServer 把 *GMService 注册到 natsrpc 服务器
// natsrpc.WithServiceID(gsId) 让每个 GS 实例的 RPC 服务有独立 ID 调用方按 ID 路由

type Service struct{}

// NewService 启动 GS 的 natsrpc 服务（在 gs/app/app.go 启动时调用）
//
// 处理：
//  1. 用 protobuf encoder 包装 NATS 连接
//  2. 创建 natsrpc.Server
//  3. 把 GMService 注册到 server（其他服务通过 GMClient 调用）
func NewService(conn *nats.Conn, gsId uint32) (*Service, error) {
	enc, err := nats.NewEncodedConn(conn, protobuf.PROTOBUF_ENCODER)
	if err != nil {
		return nil, err
	}
	svr, err := natsrpc.NewServer(enc)
	if err != nil {
		return nil, err
	}
	gs := &GMService{}
	_, err = api.RegisterGMNATSRPCServer(svr, gs, natsrpc.WithServiceID(gsId))
	if err != nil {
		return nil, err
	}
	return &Service{}, nil
}

// Close 关闭
func (s *Service) Close() {
	// TODO
}
