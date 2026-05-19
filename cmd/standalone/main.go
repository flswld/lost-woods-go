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
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	natsserver "hk4e/cmd/nats"
	cfg "hk4e/common/config"
	dispatchapp "hk4e/dispatch/app"
	gateapp "hk4e/gate/app"
	gmapp "hk4e/gm/app"
	gsapp "hk4e/gs/app"
	multiapp "hk4e/multi/app"
	"hk4e/node/api"
	nodeapp "hk4e/node/app"
	"hk4e/pkg/statsviz_serve"

	"github.com/flswld/halo/logger"
	natsclient "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/encoders/protobuf"
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
	serviceErrChan := make(chan error, 8)

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
		err := natsserver.RunNatsServer(ctxNats)
		if err != nil {
			serviceErrChan <- fmt.Errorf("nats start error: %w", err)
			return
		}
		stopChan <- struct{}{}
	}()

	time.Sleep(time.Second)

	// 2. Node 服务注册中心（其他服务需要 RegisterServer 获取 AppId）
	go func() {
		err := nodeapp.Run(ctxNode)
		if err != nil {
			serviceErrChan <- fmt.Errorf("node start error: %w", err)
			return
		}
		cancelNats()
	}()

	err := waitNodeReady(ctxNode, serviceErrChan)
	if err != nil {
		logger.Error("standalone start error: %v", err)
		panic(err)
	}

	// 3. Dispatch（HTTP 一/二级 dispatch + SDK 登录 + Gate Token 验证）
	go func() {
		err := dispatchapp.Run(ctxDispatch)
		if err != nil {
			serviceErrChan <- fmt.Errorf("dispatch start error: %w", err)
			return
		}
		cancelNode()
	}()

	time.Sleep(time.Second)

	// 4. Gate（KCP/TCP 接入 + 客户端协议代理）
	go func() {
		err := gateapp.Run(ctxGate)
		if err != nil {
			serviceErrChan <- fmt.Errorf("gate start error: %w", err)
			return
		}
		cancelDispatch()
	}()

	time.Sleep(time.Second)

	// 5. GS（游戏业务核心 启动时加载 gdconf 耗时 5-10 秒）
	go func() {
		err := gsapp.Run(ctxGs)
		if err != nil {
			serviceErrChan <- fmt.Errorf("gs start error: %w", err)
			return
		}
		cancelGate()
	}()

	time.Sleep(time.Second)

	// 6. GM 后台 HTTP + 7. Multi 反作弊（同时启动 无 sleep 间隔）
	go func() {
		err := gmapp.Run(ctxGm)
		if err != nil {
			serviceErrChan <- fmt.Errorf("gm start error: %w", err)
			return
		}
		cancelGs()
	}()

	go func() {
		err := multiapp.Run(ctxMulti)
		if err != nil {
			serviceErrChan <- fmt.Errorf("multi start error: %w", err)
			return
		}
		cancelGm()
	}()

	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGINT)
	for {
		select {
		case err := <-serviceErrChan:
			logger.Error("standalone service error: %v", err)
			panic(err)
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

func waitNodeReady(ctx context.Context, serviceErrChan <-chan error) error {
	logger.Warn("standalone wait node ready")
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	retryCount := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-serviceErrChan:
			return err
		case <-ticker.C:
			ok, err := checkNodeReady()
			if ok {
				logger.Warn("node ready")
				return nil
			}
			retryCount++
			if retryCount%5 == 0 {
				logger.Warn("node not ready yet, wait node db and natsrpc init finish, last error: %v", err)
			}
		}
	}
}

func checkNodeReady() (bool, error) {
	conn, err := natsclient.Connect(cfg.GetConfig().MQ.NatsUrl)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	enc, err := natsclient.NewEncodedConn(conn, protobuf.PROTOBUF_ENCODER)
	if err != nil {
		return false, err
	}
	defer enc.Close()
	discoveryClient, err := api.NewDiscoveryNATSRPCClient(enc)
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = discoveryClient.GetStopServerInfo(ctx, &api.NullMsg{})
	if err != nil {
		return false, err
	}
	return true, nil
}
