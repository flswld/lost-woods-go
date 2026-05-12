package app

import (
	"context"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"syscall"
	"time"

	"hk4e/common/config"
	"hk4e/common/mq"
	"hk4e/common/rpc"
	"hk4e/dispatch/controller"
	"hk4e/dispatch/dao"
	"hk4e/node/api"

	"github.com/flswld/halo/logger"
)

// dispatch 服务启动入口
//
// dispatch 是客户端的"登录入口" 提供：
//  1. 一级 dispatch（/query_region_list）：返回所有区服列表
//  2. 二级 dispatch（/query_cur_region）：返回特定区服的 gate 地址 + region 加密配置
//  3. SDK 登录（/account/login + /account/verify）：账号-密码验证 → 发 Token
//  4. ComboToken 交换（v2Login）：用 Token 换取 ComboToken（游戏内会话凭证）
//  5. Gate 调用（/gate/token/verify）：让 Gate 验证客户端的 ComboToken
//
// 启动顺序：
//  1. 注册到 Node（不带 LoadCount 因为 dispatch 通常是单实例）
//  2. 每 15s keepalive
//  3. 启动 MQ + DB（账号表）
//  4. 启动 controller（gin HTTP 服务监听 8080 + 2345）
//  5. 等信号优雅退出

var APPID string      // 注册到 Node 的 AppId
var APPVERSION string // 编译时注入

func Run(ctx context.Context) error {
	// natsrpc client
	discoveryClient, err := rpc.NewDiscoveryClient()
	if err != nil {
		return err
	}

	// 注册到节点服务器
	rsp, err := discoveryClient.RegisterServer(context.TODO(), &api.RegisterServerReq{
		ServerType: api.DISPATCH,
		AppVersion: APPVERSION,
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
				ServerType: api.DISPATCH,
				AppId:      APPID,
			})
			if err != nil {
				logger.Error("keepalive error: %v", err)
			}
		}
	}()
	defer func() {
		_, _ = discoveryClient.CancelServer(context.TODO(), &api.CancelServerReq{
			ServerType: api.DISPATCH,
			AppId:      APPID,
		})
	}()

	if !config.GetConfig().Hk4e.StandaloneModeEnable {
		logger.InitLogger(&logger.Config{
			AppName:      "dispatch_" + APPID,
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
	logger.Warn("dispatch start, appid: %v", APPID)
	defer func() {
		logger.Warn("dispatch exit")
	}()

	messageQueue := mq.NewMessageQueue(api.DISPATCH, APPID, nil)
	defer messageQueue.Close()

	db, err := dao.NewDao()
	if err != nil {
		return err
	}
	defer db.CloseDao()

	http, err := controller.NewController(db, discoveryClient, messageQueue)
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
