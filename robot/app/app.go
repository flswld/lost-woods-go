package app

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"hk4e/common/config"
	"hk4e/pkg/endec"
	"hk4e/protocol/cmd"
	"hk4e/protocol/proto"
	"hk4e/robot/client"
	"hk4e/robot/login"

	"github.com/flswld/halo/logger"
)

// Robot 服务启动入口
//
// 与其他服务不同：robot 不向 Node 注册（自己是客户端不是服务端）
// 直接连 dispatch 走完整客户端登录链路
//
// 两种模式：
//   - 单账号（DosEnable=false）：仅用 Account 配置一个账号登录 用于开发测试
//   - 压测（DosEnable=true）：起 DosTotalNum 个虚拟账号
//     · 账号名 = Account+i（如 robot0, robot1, ..., robot999）
//     · 每批 DosBatchNum 个账号并发启动 间隔 10ms 模拟真实登录波动
//     · DosLoopLogin=true 时每个虚拟客户端登入后立即重连 模拟"用户频繁重连"压力

var APPVERSION string // 编译时注入

// Run robot 主入口（cmd/robot/main.go 调用）
// 启动 runRobot goroutine 然后阻塞等信号
func Run(ctx context.Context) error {
	logger.InitLogger(&logger.Config{
		AppName:      "robot",
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
	logger.Warn("robot start")
	defer func() {
		logger.Warn("robot exit")
	}()

	go runRobot()

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

// runRobot 决定是单机模式还是压测模式
//
// 压测模式实现：
//   - 每批 dosBatchNum 个虚拟账号同时登录
//   - WaitGroup 等本批全部登录完才启动下一批（避免短时太多 RSA 解密 CPU 飙满）
//   - 批间隔 10ms（如 batch=10 总数=1000 → 1000/10=100 批 → 总耗时约 1 秒铺满）
func runRobot() {
	if config.GetConfig().Hk4eRobot.DosEnable {
		dosBatchNum := int(config.GetConfig().Hk4eRobot.DosBatchNum)
		for i := 0; i < int(config.GetConfig().Hk4eRobot.DosTotalNum); i += dosBatchNum {
			wg := new(sync.WaitGroup)
			wg.Add(dosBatchNum)
			for j := 0; j < dosBatchNum; j++ {
				go httpLogin(config.GetConfig().Hk4eRobot.Account+strconv.Itoa(i+j), wg)
			}
			wg.Wait()
			time.Sleep(time.Millisecond * 10)
		}
	} else {
		httpLogin(config.GetConfig().Hk4eRobot.Account, nil)
	}
}

// httpLogin 单个账号的完整登录流程
//
// 步骤：
//  1. GetDispatchInfo: 一/二级 dispatch 拿 Gate 地址
//  2. AccountLogin: SDK 登录拿 ComboToken（apiLogin → apiVerify → v2Login）
//  3. 起 goroutine 调 gateLogin（支持 DosLoopLogin 循环登录）
//
// DosEnable 时把 wg.Done() 放在 defer 等所有账号 dispatch 都成功才返回
func httpLogin(account string, wg *sync.WaitGroup) {
	defer func() {
		if config.GetConfig().Hk4eRobot.DosEnable {
			wg.Done()
		}
	}()
	dispatchInfo, err := login.GetDispatchInfo(config.GetConfig().Hk4eRobot.RegionListUrl,
		config.GetConfig().Hk4eRobot.RegionListParam,
		config.GetConfig().Hk4eRobot.CurRegionUrl,
		config.GetConfig().Hk4eRobot.CurRegionParam,
		config.GetConfig().Hk4eRobot.KeyId)
	if err != nil {
		logger.Error("get dispatch info error: %v", err)
		return
	}
	accountInfo, err := login.AccountLogin(config.GetConfig().Hk4eRobot.LoginSdkUrl, account, config.GetConfig().Hk4eRobot.Password)
	if err != nil {
		logger.Error("account login error: %v", err)
		return
	}
	logger.Info("robot http login ok, account: %v", account)
	go func() {
		for {
			gateLogin(account, dispatchInfo, accountInfo)
			if !config.GetConfig().Hk4eRobot.DosLoopLogin {
				break
			}
			time.Sleep(time.Second)
			continue
		}
	}()
}

// gateLogin Gate KCP 握手 + 发 PlayerLoginReq 进游戏服
//
// 处理：
//  1. login.GateLogin: KCP 握手 + GetPlayerToken 流程（密钥协商）
//  2. 计算 ClientVersionHash: sha1(ClientVersion + ServerRandomKey + "mhy2020")
//     · "mhy2020" 是米哈游内部固定盐值（业内已知）
//     · 服务端用同样算法验证客户端版本一致性
//  3. 发 PlayerLoginReq 含一系列 hardcode 字段：
//     · Checksum: 客户端 IL2CPP 校验和（伪造的 不会真校验）
//     · ClientDataVersion: 11793813（3.2 版本号）
//     · SecurityLibraryMd5: 防作弊库 MD5
//  4. 进 client.Logic 主循环
func gateLogin(account string, dispatchInfo *login.DispatchInfo, accountInfo *login.AccountInfo) {
	session, err := login.GateLogin(dispatchInfo, accountInfo, config.GetConfig().Hk4eRobot.KeyId)
	if err != nil {
		logger.Error("gate login error: %v", err)
		return
	}
	logger.Info("robot gate login ok, account: %v", account)
	clientVersionHashData, err := hex.DecodeString(
		endec.Sha1Str(config.GetConfig().Hk4eRobot.ClientVersion + session.ClientVersionRandomKey + "mhy2020"),
	)
	if err != nil {
		logger.Error("gen clientVersionHashData error: %v", err)
		return
	}
	checksumClientVersion := strings.Split(config.GetConfig().Hk4eRobot.ClientVersion, "_")[0]
	session.SendMsg(cmd.PlayerLoginReq, &proto.PlayerLoginReq{
		AccountType:           1,
		SubChannelId:          1,
		LanguageType:          2,
		PlatformType:          3,
		Checksum:              "$008094416f86a051270e64eb0b405a38825",
		ChecksumClientVersion: checksumClientVersion,
		ClientDataVersion:     11793813,
		ClientVerisonHash:     base64.StdEncoding.EncodeToString(clientVersionHashData),
		ClientVersion:         config.GetConfig().Hk4eRobot.ClientVersion,
		SecurityCmdReply:      session.SecurityCmdBuffer,
		SecurityLibraryMd5:    "574a507ffee2eb6f997d11f71c8ae1fa",
		Token:                 accountInfo.ComboToken,
	})
	client.Logic(account, session)
}
