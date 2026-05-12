package app

import (
	"context"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"syscall"

	"hk4e/common/config"
	"hk4e/common/mq"
	"hk4e/node/api"
	"hk4e/node/dao"
	"hk4e/node/service"

	"github.com/flswld/halo/logger"
	"github.com/nats-io/nats.go"
)

// node 服启动入口（必须**最先**启动 其他所有服务依赖它）
//
// 启动顺序：
//  1. 直接连 NATS（不走 DiscoveryClient 因为自己就是被发现的目标）
//  2. 启动 MessageQueue（仅用来监听全服广播）
//  3. 连 DB（持久化 Region 表 含 Ec2b/NextUid/StopServerInfo）
//  4. 启动 service.NewService（注册 natsrpc Server + 启动 DiscoveryService）
//
// 与其他服务不同：
//   - 不向 Node 注册自己（自己就是注册中心）
//   - 不需要 KeepaliveServer
//   - MQ AppId 固定为 "node"
//
// **单点故障**：Node 是集群唯一注册中心 挂了所有服务都无法新建连接
// 已注册的连接还能维持工作 但路由变更会失效
func Run(ctx context.Context) error {
	if !config.GetConfig().Hk4e.StandaloneModeEnable {
		logger.InitLogger(&logger.Config{
			AppName:      "node",
			Level:        logger.ParseLevel(config.GetConfig().Logger.Level),
			TrackLine:    config.GetConfig().Logger.TrackLine,
			TrackThread:  config.GetConfig().Logger.TrackThread,
			EnableFile:   config.GetConfig().Logger.EnableFile,
			DisableColor: config.GetConfig().Logger.DisableColor,
			EnableJson:   config.GetConfig().Logger.EnableJson,
		})
		defer func() {
			logger.CloseLogger()
		}()
	}
	logger.Warn("node start")
	defer func() {
		logger.Warn("node exit")
	}()

	// natsrpc server
	conn, err := nats.Connect(config.GetConfig().MQ.NatsUrl)
	if err != nil {
		logger.Error("connect nats error: %v", err)
		return err
	}
	defer conn.Close()

	// 只用来监听全服广播
	messageQueue := mq.NewMessageQueue(api.NODE, "node", nil)
	defer messageQueue.Close()

	db, err := dao.NewDao()
	if err != nil {
		return err
	}
	defer db.CloseDao()

	s, err := service.NewService(db, conn, messageQueue)
	if err != nil {
		return err
	}
	defer s.Close()

	c := make(chan os.Signal, 1)
	if !config.GetConfig().Hk4e.StandaloneModeEnable {
		signal.Notify(c, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGINT)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case s := <-c:
			logger.Warn("get a signal %s", s.String())
			switch s {
			case syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGINT:
				return nil
			case syscall.SIGHUP:
			default:
				return nil
			}
		}
	}
}
