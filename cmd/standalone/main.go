package main

// standalone 单进程模式入口 - 开发/小规模部署推荐
//
// 一个进程内串行启动 7 个服务（顺序敏感，每两个之间间隔 1 秒）：
//
//	1. NATS Server   ← 内嵌 NATS（所有服务依赖的消息总线）
//	2. Node          ← 服务注册中心（必须先启动 否则后面服务拿不到 AppId）
//	3. Dispatch      ← HTTP 调度（一/二级 dispatch + SDK 登录）
//	4. Gate          ← KCP/TCP 客户端接入
//	5. GS            ← 游戏业务（gdconf 加载耗时约 5-10 秒）
//	6. GM + Multi    ← 同时启动（无 sleep 间隔）
//
// 1 秒间隔是为了让前一个服务的 RegisterServer/启动 KCP 监听等动作完成
// 例：Gate 启动时 GS 已注册到 Node，doGateLogin 拿到 minLoadGsServerAppId 才能成功
//
// 关闭流程：监听 SIGTERM/SIGINT/SIGQUIT → cancelMulti() → 反向传播 cancel
// 各服务 defer 中 CancelServer + Close 资源 NATS 最后退出（stopChan 同步）
//
// statsviz 在 0.0.0.0:4567（注意：standalone 模式与 GS 共用同端口 集群模式 Gate=3456）

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"hk4e/cmd/nats"
	cfg "hk4e/common/config"
	dispatchapp "hk4e/dispatch/app"
	gateapp "hk4e/gate/app"
	gmapp "hk4e/gm/app"
	gsapp "hk4e/gs/app"
	multiapp "hk4e/multi/app"
	nodeapp "hk4e/node/app"
	"hk4e/pkg/statsviz_serve"

	"github.com/flswld/halo/logger"
)

var (
	config = flag.String("config", "application.toml", "config file")
)

func main() {
	flag.Parse()
	go func() {
		_ = statsviz_serve.Serve("0.0.0.0:4567")
	}()
	cfg.InitConfig(*config)

	logger.InitLogger(&logger.Config{
		AppName:      "standalone",
		Level:        logger.ParseLevel(cfg.GetConfig().Logger.Level),
		TrackLine:    cfg.GetConfig().Logger.TrackLine,
		TrackThread:  cfg.GetConfig().Logger.TrackThread,
		EnableFile:   cfg.GetConfig().Logger.EnableFile,
		DisableColor: cfg.GetConfig().Logger.DisableColor,
		EnableJson:   cfg.GetConfig().Logger.EnableJson,
	})
	logger.Warn("standalone start")
	defer func() {
		logger.Warn("standalone exit")
		logger.CloseLogger()
	}()

	stopChan := make(chan struct{})

	// 每个服务一个 context 关闭信号反向传播：Multi 退出 → cancelGm → ... → cancelNats
	// 这样保证关闭顺序与启动顺序相反（先停业务后停基础设施）NATS 最后退出
	ctxNats, cancelNats := context.WithCancel(context.Background())
	ctxNode, cancelNode := context.WithCancel(context.Background())
	ctxDispatch, cancelDispatch := context.WithCancel(context.Background())
	ctxGate, cancelGate := context.WithCancel(context.Background())
	ctxGs, cancelGs := context.WithCancel(context.Background())
	ctxGm, cancelGm := context.WithCancel(context.Background())
	ctxMulti, cancelMulti := context.WithCancel(context.Background())

	// 1. NATS Server（所有服务的消息总线 必须最先启动）
	go func() {
		err := nats.RunNatsServer(ctxNats)
		if err != nil {
			panic(err)
		}
		stopChan <- struct{}{}
	}()

	time.Sleep(time.Second)

	// 2. Node 服务注册中心（其他服务需要 RegisterServer 获取 AppId）
	go func() {
		err := nodeapp.Run(ctxNode)
		if err != nil {
			panic(err)
		}
		cancelNats()
	}()

	time.Sleep(time.Second)

	// 3. Dispatch（HTTP 一/二级 dispatch + SDK 登录 + Gate Token 验证）
	go func() {
		err := dispatchapp.Run(ctxDispatch)
		if err != nil {
			panic(err)
		}
		cancelNode()
	}()

	time.Sleep(time.Second)

	// 4. Gate（KCP/TCP 接入 + 客户端协议代理）
	go func() {
		err := gateapp.Run(ctxGate)
		if err != nil {
			panic(err)
		}
		cancelDispatch()
	}()

	time.Sleep(time.Second)

	// 5. GS（游戏业务核心 启动时加载 gdconf 耗时 5-10 秒）
	go func() {
		err := gsapp.Run(ctxGs)
		if err != nil {
			panic(err)
		}
		cancelGate()
	}()

	time.Sleep(time.Second)

	// 6. GM 后台 HTTP + 7. Multi 反作弊（同时启动 无 sleep 间隔）
	go func() {
		err := gmapp.Run(ctxGm)
		if err != nil {
			panic(err)
		}
		cancelGs()
	}()

	go func() {
		err := multiapp.Run(ctxMulti)
		if err != nil {
			panic(err)
		}
		cancelGm()
	}()

	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGINT)
	for {
		select {
		case s := <-c:
			logger.Warn("get a signal %s", s.String())
			switch s {
			case syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGINT:
				cancelMulti()
				<-stopChan
				return
			case syscall.SIGHUP:
			default:
				return
			}
		}
	}
}
