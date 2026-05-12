package rpc

import (
	"hk4e/common/config"
	gsapi "hk4e/gs/api"
	nodeapi "hk4e/node/api"

	"github.com/byebyebruce/natsrpc"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/encoders/protobuf"
)

// natsrpc 客户端封装 - 项目目前仅有 2 个 RPC 服务
//
// 与 mq.MessageQueue 的区别：
//   - MQ：异步广播 + 转发（数据平面/控制平面）
//   - natsrpc：同步 RPC（请求-响应模式 等结果返回）
//
// 当前 RPC 接口：
//   - DiscoveryClient: 调 Node 的 DiscoveryService（注册 / 心跳 / 服务发现 / UID 分配）
//   - GMClient: 调 GS 的 GMService（GM 后台 → GS 执行命令 同步等结果）
//
// 接口定义在 .proto 文件中 用 protoc-gen-natsrpc 生成存根代码
//   - node/api/api.natsrpc.pb.go ← node/api/api.proto
//   - gs/api/api.natsrpc.pb.go ← gs/api/api.proto

// DiscoveryClient Node 服务发现 RPC 客户端（每个服务都需要持有）
// 嵌入 nodeapi.DiscoveryNATSRPCClient 直接获得所有 RPC 方法
type DiscoveryClient struct {
	nodeapi.DiscoveryNATSRPCClient
}

func NewDiscoveryClient() (*DiscoveryClient, error) {
	conn, err := nats.Connect(config.GetConfig().MQ.NatsUrl)
	if err != nil {
		return nil, err
	}
	discoveryClient, err := newDiscoveryClient(conn)
	if err != nil {
		return nil, err
	}
	return discoveryClient, nil
}

func newDiscoveryClient(conn *nats.Conn) (*DiscoveryClient, error) {
	enc, err := nats.NewEncodedConn(conn, protobuf.PROTOBUF_ENCODER)
	if err != nil {
		return nil, err
	}
	cli, err := nodeapi.NewDiscoveryNATSRPCClient(enc)
	if err != nil {
		return nil, err
	}
	return &DiscoveryClient{
		DiscoveryNATSRPCClient: cli,
	}, nil
}

// GMClient GS GM 服务 RPC 客户端
//
// 用于 GM 后台向特定 GS 同步发送 GM 命令并等结果（10 秒超时）
// 通过 natsrpc.WithClientID(gsId) 路由到指定 GS 实例（按 GsId 1~MaxGsId）
//
// gm/controller/gm_controller.go 缓存 GMClient 实例避免每次重建连接
type GMClient struct {
	gsapi.GMNATSRPCClient
}

func NewGMClient(gsId uint32) (*GMClient, error) {
	conn, err := nats.Connect(config.GetConfig().MQ.NatsUrl)
	if err != nil {
		return nil, err
	}
	gmClient, err := newGmClient(conn, gsId)
	if err != nil {
		return nil, err
	}
	return gmClient, nil
}

func newGmClient(conn *nats.Conn, gsId uint32) (*GMClient, error) {
	enc, err := nats.NewEncodedConn(conn, protobuf.PROTOBUF_ENCODER)
	if err != nil {
		return nil, err
	}
	cli, err := gsapi.NewGMNATSRPCClient(enc, natsrpc.WithClientID(gsId))
	if err != nil {
		return nil, err
	}
	return &GMClient{
		GMNATSRPCClient: cli,
	}, nil
}
