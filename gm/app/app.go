package app

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"hk4e/common/config"
	"hk4e/common/mq"
	"hk4e/common/rpc"
	"hk4e/gm/controller"
	"hk4e/node/api"

	"github.com/flswld/halo/logger"
)

// gm 服启动入口
//
// gm 是运维 HTTP 后台 与其他服务不同：
//   - **不向 Node 注册自己**（gm 是控制面 不接受游戏流量）
//   - 通过 natsrpc 连 Node + 直接调 GMService 操作 GS
//   - MQ AppId 写死 "gm"
//   - 监听 HTTP 端口 9001 提供 GM 命令/停服管理/白名单管理 等接口
//
// 主要服务：详见 controller.go 的路由注册
func Run(ctx context.Context) error {
	if !config.GetConfig().Hk4e.StandaloneModeEnable {
		logger.InitLogger(&logger.Config{
			AppName:      "gm",
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
	logger.Warn("gm start")
	defer func() {
		logger.Warn("gm exit")
	}()

	// natsrpc client
	discoveryClient, err := rpc.NewDiscoveryClient()
	if err != nil {
		return err
	}

	messageQueue := mq.NewMessageQueue(api.GM, "gm", nil)
	defer messageQueue.Close()

	http, err := controller.NewController(discoveryClient, messageQueue)
	if err != nil {
		return err
	}
	defer http.Close()

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
