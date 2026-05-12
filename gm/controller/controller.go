package controller

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"hk4e/common/config"
	"hk4e/common/mq"
	"hk4e/common/rpc"

	"github.com/flswld/halo/logger"
	"github.com/gin-gonic/gin"
)

// GM 后台服务（独立 HTTP 服务 端口 9001）
//
// 提供运维接口：
//   - 命令执行：POST /gm/cmd 调 GM 命令（指定 GS / 全服）
//   - 在线统计：GET /server/online/stats（不需要鉴权）
//   - 停服管理：GET/POST /server/stop/* 查/改停服信息
//   - 白名单管理：GET/POST /server/white/* 查/增/删 IP 白名单
//   - 调度取消：POST /server/dispatch/cancel 优雅停服
//
// 鉴权：除 /server/online/stats 外所有接口需要 Header GmAuthKey
//
//	默认 "flswld" 来自 config.GetConfig().Hk4e.GmAuthKey
type Controller struct {
	gmClientMap           map[uint32]*rpc.GMClient // GS RPC 客户端缓存（按 GsId 索引）
	gmClientMapLock       sync.RWMutex
	discoveryClient       *rpc.DiscoveryClient
	messageQueue          *mq.MessageQueue
	globalGsOnlineMap     map[uint32]string // 全服玩家在线表（每 60s 同步）
	globalGsOnlineMapLock sync.RWMutex
}

func NewController(discoveryClient *rpc.DiscoveryClient, messageQueue *mq.MessageQueue) (*Controller, error) {
	r := new(Controller)
	r.gmClientMap = make(map[uint32]*rpc.GMClient)
	r.discoveryClient = discoveryClient
	r.messageQueue = messageQueue
	go func() {
		for {
			_, ok := <-r.messageQueue.GetNetMsg()
			if !ok {
				return
			}
		}
	}()
	r.globalGsOnlineMap = make(map[uint32]string)
	r.syncGlobalGsOnlineMap()
	go r.autoSyncGlobalGsOnlineMap()
	go r.registerRouter()
	return r, nil
}

func (c *Controller) Close() {
}

func (c *Controller) autoSyncGlobalGsOnlineMap() {
	ticker := time.NewTicker(time.Second * 60)
	for {
		<-ticker.C
		c.syncGlobalGsOnlineMap()
	}
}

func (c *Controller) syncGlobalGsOnlineMap() {
	rsp, err := c.discoveryClient.GetGlobalGsOnlineMap(context.TODO(), nil)
	if err != nil {
		logger.Error("get global gs online map error: %v", err)
		return
	}
	copyMap := make(map[uint32]string)
	for k, v := range rsp.OnlineMap {
		copyMap[k] = v
	}
	copyMapLen := len(copyMap)
	c.globalGsOnlineMapLock.Lock()
	c.globalGsOnlineMap = copyMap
	c.globalGsOnlineMapLock.Unlock()
	logger.Info("sync global gs online map finish, len: %v", copyMapLen)
}

// authorize 鉴权中间件 检查 Header GmAuthKey 与配置匹配
// 不通过返回 code=10001 防止未授权访问 GM 后台
// **生产部署必须改 GmAuthKey** 默认值 "flswld" 太弱
func (c *Controller) authorize() gin.HandlerFunc {
	return func(context *gin.Context) {
		if context.GetHeader("GmAuthKey") == config.GetConfig().Hk4e.GmAuthKey {
			// 验证通过
			context.Next()
			return
		}
		// 验证不通过
		context.Abort()
		context.JSON(http.StatusOK, gin.H{
			"code": "10001",
			"msg":  "没有访问权限",
		})
	}
}

type CommonRsp struct {
	Code int32  `json:"code"`
	Msg  string `json:"msg"`
	Data any    `json:"data"`
}

// registerRouter 注册 GM HTTP 路由
//
// 路由分组：
//   - GET /server/online/stats: 在线统计（无鉴权 监控接口可公开）
//   - 其他全部需要 GmAuthKey 鉴权（authorize 中间件）
//   - POST /gm/cmd: 执行 GM 命令
//   - GET /server/stop/state + POST /server/stop/change: 停服开关
//   - GET /server/white/list + POST /server/white/add/del: 停服白名单
//   - POST /server/dispatch/cancel: 调度取消（优雅停服）
func (c *Controller) registerRouter() {
	if logger.GetConfig().Level == logger.DEBUG {
		gin.SetMode(gin.DebugMode)
	} else {
		gin.SetMode(gin.ReleaseMode)
	}
	engine := gin.Default()
	engine.GET("/server/online/stats", c.serverOnlineStats)
	engine.Use(c.authorize())
	engine.POST("/gm/cmd", c.gmCmd)
	engine.GET("/server/stop/state", c.serverStopState)
	engine.POST("/server/stop/change", c.serverStopChange)
	engine.GET("/server/white/list", c.serverWhiteList)
	engine.POST("/server/white/add", c.serverWhiteAdd)
	engine.POST("/server/white/del", c.serverWhiteDel)
	engine.POST("/server/dispatch/cancel", c.serverDispatchCancel)
	port := config.GetConfig().Hk4e.GmHttpPort
	addr := ":" + strconv.Itoa(int(port))
	err := engine.Run(addr)
	if err != nil {
		logger.Error("gin run error: %v", err)
	}
}
