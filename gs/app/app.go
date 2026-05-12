package app

// GS 启动入口 - 游戏服业务核心进程
//
// 启动顺序敏感（详见 CLAUDE.md "GS 启动"）：
//  1. InitLogger（standalone 模式跳过 复用 standalone 全局 logger）
//  2. InitGameDataConfig         ← gdconf 加载 5-10 秒 必须先于 NewGameCore
//                                  因为 World/Scene 初始化依赖场景 Lua 数据
//  3. discoveryClient.RegisterServer(GS) → 获得 8 字符 AppId + 1~MaxGsId 内的 GsId
//                                  defer CancelServer 优雅退出时通知 Node 立即移除
//  4. InitLogger 重新（AppName 带 GSID 区分多 GS 实例日志）
//  5. NewDao                     ← DB（GORM/MongoDB）+ Redis（单机/Cluster）三选一
//  6. NewMessageQueue            ← NATS + TCP 直连双通道（向所有 Gate 建立 TCP 长连接）
//  7. NewGameCore                ← 创建 8 个全局管理器 + 启动 gameMainLoop 主循环 goroutine
//                                  详见 gs/game/game.go NewGameCore
//  8. NewService                 ← natsrpc Server 暴露 GMService（HTTP 后台 → 这里）
//  9. 15s 定时 KeepaliveServer（LoadCount = ONLINE_PLAYER_NUM 上报给 Node 做负载均衡）
//  10. 阻塞等待 SIGTERM/SIGINT/SIGQUIT
//
// 关键全局变量 APPID/APPVERSION/GSID（包级）：
//   - APPVERSION 编译期注入（make build VERSION=x.x.x）
//   - APPID 由 Node 启动时分配
//   - GSID 用于 AI 玩家 uid 派生（AiBaseUid + GSID） + GM 后台路由

import (
	"context"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"hk4e/common/config"
	"hk4e/common/mq"
	"hk4e/common/rpc"
	"hk4e/gdconf"
	"hk4e/gs/dao"
	"hk4e/gs/game"
	"hk4e/gs/service"
	"hk4e/node/api"

	"github.com/flswld/halo/logger"
	"github.com/nats-io/nats.go"
)

var APPID string      // 服务实例 AppId（Node 注册时分配 8 字符随机串）
var APPVERSION string // 编译期注入（-ldflags "-X main.VERSION=x.x.x"）
var GSID uint32       // 游戏服编号 1~MaxGsId 注册时分配 影响 AI 玩家 uid + 全服广播路由

func Run(ctx context.Context) error {
	if !config.GetConfig().Hk4e.StandaloneModeEnable {
		logger.InitLogger(&logger.Config{
			AppName:      "gs_start",
			Level:        logger.ParseLevel(config.GetConfig().Logger.Level),
			TrackLine:    config.GetConfig().Logger.TrackLine,
			TrackThread:  config.GetConfig().Logger.TrackThread,
			EnableFile:   config.GetConfig().Logger.EnableFile,
			DisableColor: config.GetConfig().Logger.DisableColor,
			EnableJson:   config.GetConfig().Logger.EnableJson,
		})
	}
	gdconf.InitGameDataConfig()
	if !config.GetConfig().Hk4e.StandaloneModeEnable {
		logger.CloseLogger()
	}

	// natsrpc client
	discoveryClient, err := rpc.NewDiscoveryClient()
	if err != nil {
		return err
	}

	// 注册到节点服务器
	rsp, err := discoveryClient.RegisterServer(context.TODO(), &api.RegisterServerReq{
		ServerType: api.GS,
		AppVersion: APPVERSION,
	})
	if err != nil {
		return err
	}
	APPID = rsp.GetAppId()
	GSID = rsp.GetGsId()
	defer func() {
		_, _ = discoveryClient.CancelServer(context.TODO(), &api.CancelServerReq{
			ServerType: api.GS,
			AppId:      APPID,
		})
	}()

	if !config.GetConfig().Hk4e.StandaloneModeEnable {
		logger.InitLogger(&logger.Config{
			AppName:      "gs_" + strconv.Itoa(int(GSID)) + "_" + APPID,
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
	logger.Warn("gs start, appid: %v, gsid: %v", APPID, GSID)
	defer func() {
		logger.Warn("gs exit, appid: %v", APPID)
	}()

	db, err := dao.NewDao()
	if err != nil {
		return err
	}
	defer db.CloseDao()

	messageQueue := mq.NewMessageQueue(api.GS, APPID, discoveryClient)
	defer messageQueue.Close()

	gameCore := game.NewGameCore(discoveryClient, db, messageQueue, GSID, APPID, APPVERSION)
	defer gameCore.Close()

	// natsrpc server
	conn, err := nats.Connect(config.GetConfig().MQ.NatsUrl)
	if err != nil {
		logger.Error("connect nats error: %v", err)
		return err
	}
	defer conn.Close()
	s, err := service.NewService(conn, GSID)
	if err != nil {
		return err
	}
	defer s.Close()

	// 15 秒心跳 goroutine 上报当前在线玩家数作为负载指标
	// Node 据此选最小负载 GS（GetServerAppId）30 秒无心跳被 removeDeadServer 剔除
	go func() {
		ticker := time.NewTicker(time.Second * 15)
		for {
			select {
			case <-ctx.Done():
				ticker.Stop()
				return
			case <-ticker.C:
				_, err := discoveryClient.KeepaliveServer(context.TODO(), &api.KeepaliveServerReq{
					ServerType: api.GS,
					AppId:      APPID,
					LoadCount:  uint32(atomic.LoadInt32(&game.ONLINE_PLAYER_NUM)),
				})
				if err != nil {
					logger.Error("keepalive error: %v", err)
				}
			}
		}
	}()

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
