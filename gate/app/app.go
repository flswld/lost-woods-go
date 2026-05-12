package app

import (
	"context"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"hk4e/common/config"
	"hk4e/common/mq"
	"hk4e/common/rpc"
	"hk4e/gate/dao"
	"hk4e/gate/net"
	"hk4e/node/api"

	"github.com/flswld/halo/logger"
)

// gate 服务启动入口
//
// 启动顺序：
//  1. 连 Node 服务发现 RPC
//  2. 向 Node 注册（拿到 AppId 后才能服务）
//     · GateServerAddr 包含 KCP 地址 + MQ 地址 让其他服可以连过来
//     · GameVersionList 来自 Hk4e.Version 配置 用于客户端版本筛选
//  3. 每 15s 发 keepalive（带 LoadCount=当前在线连接数 让 Node 算最小负载 GS）
//  4. 重新初始化日志（带 AppId 后缀方便集群定位）
//  5. 启动 MQ（NATS + TCP 直连双通道）
//  6. 启动 Account DB（OpenId ↔ uid 映射）
//  7. 启动 ConnManager 进入工作状态
//  8. 等 SIGTERM/SIGINT 优雅退出（CancelServer 让 Node 移除自己）

var APPID string      // 注册到 Node 后获得的 AppId（每个 gate 实例唯一）
var APPVERSION string // 编译时注入的版本号（make build VERSION=x.x.x）

// Run gate 主入口（cmd/gate/main.go 调用）
// ctx 取消时会触发 select 退出 让 defer 清理资源
func Run(ctx context.Context) error {
	// natsrpc client
	discoveryClient, err := rpc.NewDiscoveryClient()
	if err != nil {
		return err
	}

	// 注册到节点服务器
	rsp, err := discoveryClient.RegisterServer(context.TODO(), &api.RegisterServerReq{
		ServerType: api.GATE,
		AppVersion: APPVERSION,
		GateServerAddr: &api.GateServerAddr{
			KcpAddr: config.GetConfig().Hk4e.KcpAddr,
			KcpPort: uint32(config.GetConfig().Hk4e.KcpPort),
			MqAddr:  config.GetConfig().Hk4e.GateTcpMqAddr,
			MqPort:  uint32(config.GetConfig().Hk4e.GateTcpMqPort),
		},
		GameVersionList: strings.Split(config.GetConfig().Hk4e.Version, ","),
	})
	if err != nil {
		return err
	}
	APPID = rsp.GetAppId()
	go func() {
		ticker := time.NewTicker(time.Second * 15)
		for {
			<-ticker.C
			_, err := discoveryClient.KeepaliveServer(context.TODO(), &api.KeepaliveServerReq{
				ServerType: api.GATE,
				AppId:      APPID,
				LoadCount:  uint32(atomic.LoadInt32(&net.CLIENT_CONN_NUM)),
			})
			if err != nil {
				logger.Error("keepalive error: %v", err)
			}
		}
	}()
	defer func() {
		_, _ = discoveryClient.CancelServer(context.TODO(), &api.CancelServerReq{
			ServerType: api.GATE,
			AppId:      APPID,
		})
	}()

	if !config.GetConfig().Hk4e.StandaloneModeEnable {
		logger.InitLogger(&logger.Config{
			AppName:      "gate_" + APPID,
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
	logger.Warn("gate start, appid: %v", APPID)
	defer func() {
		logger.Warn("gate exit, appid: %v", APPID)
	}()

	messageQueue := mq.NewMessageQueue(api.GATE, APPID, nil)
	defer messageQueue.Close()

	db, err := dao.NewDao()
	if err != nil {
		return err
	}
	defer db.CloseDao()

	connManager, err := net.NewConnManager(db, messageQueue, discoveryClient)
	if err != nil {
		return err
	}
	defer connManager.Close()

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
