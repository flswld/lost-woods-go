package controller

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"hk4e/common/config"
	"hk4e/common/mq"
	"hk4e/common/region"
	"hk4e/common/rpc"
	"hk4e/dispatch/dao"
	"hk4e/dispatch/model"
	"hk4e/node/api"
	"hk4e/pkg/random"

	"github.com/flswld/halo/logger"
	"github.com/gin-gonic/gin"
)

// Controller dispatch HTTP 服务控制器
//
// 字段：
//   - db: SDK 账号 DB（账号-密码-token 表）+ Sdk 主键自增（standalone 模式用）
//   - signRsaKey: 区服签名密钥（v2.7.5+ 客户端要求 region 响应必须签名）
//   - encRsaKeyMap: 5 套区服加密密钥（KeyId 1~5）让客户端 region 响应加密传输
//   - pwdRsaKey: 账号密码 RSA 解密私钥（客户端用对应公钥加密密码 dispatch 解密）
//   - ec2b: dispatch ↔ gate 之间的 ec2b 密钥（与 client 协商时下发）
//   - gateServerMap: 已登录玩家 token 缓存（apiVerify → v2Login → gateTokenVerify 三步用同一 token）
//   - stopServerInfo: 停服信息
//   - whiteList: 停服期间白名单
//   - nextSdkAccountId: SDK 账号自增 id（standalone 模式持久化到 sdk 表）
type Controller struct {
	db               *dao.Dao
	discoveryClient  *rpc.DiscoveryClient
	signRsaKey       []byte
	encRsaKeyMap     map[string][]byte
	pwdRsaKey        []byte
	ec2b             *random.Ec2b
	messageQueue     *mq.MessageQueue
	gateServerMap    *sync.Map
	stopServerInfo   *api.StopServerInfo
	whiteList        *api.GetWhiteListRsp
	nextSdkAccountId uint32
}

func NewController(db *dao.Dao, discovery *rpc.DiscoveryClient, messageQueue *mq.MessageQueue) (*Controller, error) {
	r := new(Controller)
	r.db = db
	r.discoveryClient = discovery
	r.signRsaKey, r.encRsaKeyMap, r.pwdRsaKey = region.LoadRegionRsaKey()
	rsp, err := r.discoveryClient.GetRegionEc2B(context.TODO(), &api.NullMsg{})
	if err != nil {
		logger.Error("get region ec2b error: %v", err)
		return nil, err
	}
	ec2b, err := random.LoadEc2bKey(rsp.Data)
	if err != nil {
		logger.Error("parse region ec2b error: %v", err)
		return nil, err
	}
	r.ec2b = ec2b
	r.messageQueue = messageQueue
	go func() {
		for {
			_, ok := <-r.messageQueue.GetNetMsg()
			if !ok {
				return
			}
		}
	}()
	r.gateServerMap = new(sync.Map)
	r.stopServerInfo = nil
	r.whiteList = nil
	if config.GetConfig().Hk4e.StandaloneModeEnable {
		sdk, err := r.db.QuerySdk()
		if err != nil {
			logger.Error("load sdk from db error: %v", err)
			return nil, err
		}
		if sdk == nil {
			sdk = &model.Sdk{
				NextSdkAccountId: dao.SdkAccountIdBegin,
			}
			err := r.db.InsertSdk(sdk)
			if err != nil {
				logger.Error("save sdk to db error: %v", err)
				return nil, err
			}
		}
		r.nextSdkAccountId = sdk.NextSdkAccountId
	}
	go r.registerRouter()
	r.syncWhiteList()
	go r.autoSyncWhiteList()
	r.syncStopServerInfo()
	go r.autoSyncStopServerInfo()
	return r, nil
}

func (c *Controller) Close() {
	if config.GetConfig().Hk4e.StandaloneModeEnable {
		sdk := &model.Sdk{
			NextSdkAccountId: c.nextSdkAccountId,
		}
		err := c.db.UpdateSdk(sdk)
		if err != nil {
			logger.Error("save sdk to db error: %v", err)
		}
	}
}

// registerRouter 注册所有 HTTP 路由（gin 引擎）
//
// 路由分 6 组：
//  1. 调度（一/二级 dispatch）：客户端启动时第一批请求
//     · query_security_file: 安全文件下发
//     · query_region_list: 一级 dispatch 返回区服列表
//     · query_cur_region: 二级 dispatch 返回 gate 地址 + region 加密配置
//  2. 登录：账号-密码 → token → ComboToken 三段式
//     · /hk4e_:name/mdk/shield/api/login: 账号-密码登录拿 Token
//     · /hk4e_:name/mdk/shield/api/verify: Token 续期验证
//     · /hk4e_:name/combo/granter/login/v2/login: Token → ComboToken 交换
//  3. 日志：客户端崩溃日志/性能日志/SDK 日志（仅记录不返回真实数据）
//  4. 收集数据：device-fp 设备指纹（反作弊用 项目不强制）
//  5. 返回固定数据：协议版本/SDK config/字体下发等（占位让客户端不报错）
//  6. 静态资源：mi18n 多语言文件 + geetest 验证码资源
//
// /gate/token/verify 是 Gate 内部调用接口（验证客户端 ComboToken）
//
// 404 fallback：返回 "FUCK MHY"（作者中二风格的错误页 防止客户端发到不存在的路由报错）
func (c *Controller) registerRouter() {
	if logger.GetConfig().Level == logger.DEBUG {
		gin.SetMode(gin.DebugMode)
	} else {
		gin.SetMode(gin.ReleaseMode)
	}
	engine := gin.Default()
	{
		// 404
		engine.NoRoute(func(context *gin.Context) {
			logger.Info("no route find, fallback to fuck mhy, url: %v", context.Request.RequestURI)
			context.Header("Content-type", "text/html; charset=UTF-8")
			context.Status(http.StatusNotFound)
			_, _ = context.Writer.WriteString("FUCK MHY")
		})
	}
	{
		// 调度
		// dispatchosglobal.yuanshen.com
		engine.GET("/query_security_file", c.querySecurityFile)
		engine.GET("/query_region_list", c.queryRegionList)
		// osusadispatch.yuanshen.com
		engine.GET("/query_cur_region", c.queryCurRegion)
	}
	{
		// 登录
		// hk4e-sdk-os.hoyoverse.com
		// 账号登录
		engine.POST("/hk4e_:name/mdk/shield/api/login", c.apiLogin)
		// token登录
		engine.POST("/hk4e_:name/mdk/shield/api/verify", c.apiVerify)
		// 获取combo token
		engine.POST("/hk4e_:name/combo/granter/login/v2/login", c.v2Login)
	}
	{
		// 日志
		engine.POST("/sdk/dataUpload", c.sdkDataUpload)
		engine.GET("/perf/config/verify", c.perfConfigVerify)
		engine.POST("/perf/dataUpload", c.perfDataUpload)
		engine.POST("/log", c.log8888)
		engine.POST("/crash/dataUpload", c.crashDataUpload)
	}
	{
		// 收集数据
		engine.GET("/device-fp/api/getExtList", c.deviceExtList)
		engine.POST("/device-fp/api/getFp", c.deviceFp)
	}
	{
		// 返回固定数据
		// Windows
		engine.GET("/hk4e_:name/mdk/agreement/api/getAgreementInfos", c.getAgreementInfos)
		engine.POST("/hk4e_:name/combo/granter/api/compareProtocolVersion", c.postCompareProtocolVersion)
		engine.POST("/account/risky/api/check", c.check)
		engine.GET("/combo/box/api/config/sdk/combo", c.combo)
		engine.GET("/hk4e_:name/combo/granter/api/getConfig", c.getConfig)
		engine.GET("/hk4e_:name/mdk/shield/api/loadConfig", c.loadConfig)
		engine.POST("/data_abtest_api/config/experiment/list", c.list)
		// Android
		engine.POST("/common/h5log/log/batch", c.batch)
		engine.GET("/hk4e_:name/combo/granter/api/getFont", c.getFont)
	}
	{
		// 静态资源
		// GET https://webstatic-sea.hoyoverse.com/admin/mi18n/plat_oversea/m2020030410/m2020030410-version.json HTTP/1.1
		// GET https://webstatic-sea.hoyoverse.com/admin/mi18n/plat_oversea/m2020030410/m2020030410-zh-cn.json HTTP/1.1
		engine.StaticFS("/admin/mi18n/plat_oversea/m2020030410", http.Dir("./static/m2020030410"))
		// GET https://webstatic-sea.hoyoverse.com/admin/mi18n/plat_os/m09291531181441/m09291531181441-version.json HTTP/1.1
		// GET https://webstatic-sea.hoyoverse.com/admin/mi18n/plat_os/m09291531181441/m09291531181441-zh-cn.json HTTP/1.1
		engine.StaticFS("/admin/mi18n/plat_os/m09291531181441", http.Dir("./static/m09291531181441"))
		// GET https://webstatic-sea.hoyoverse.com/admin/mi18n/plat_oversea/m202003049/m202003049-version.json HTTP/1.1
		// GET https://webstatic-sea.hoyoverse.com/admin/mi18n/plat_oversea/m202003049/m202003049-zh-cn.json HTTP/1.1
		engine.StaticFS("/admin/mi18n/plat_oversea/m202003049", http.Dir("./static/m202003049"))
	}
	{
		// geetest
		engine.GET("/geetestV2.html", c.gtGeetestV2)
		// Android geetest
		engine.GET("/favicon.ico", c.gtFaviconIco)
		engine.GET("/gettype.php", c.gtGetType)
		engine.GET("/get.php", c.gtGet)
		engine.POST("/ajax.php", c.gtAjax)
		engine.GET("/ajax.php", c.gtAjax)
		// GET https://static.geetest.com/static/appweb/app3-index.html?gt=16bddce04c7385dbb7282778c29bba3e&challenge=616018607b6940f52fbd349004038686&lang=zh-CN&title=&type=slide&api_server=api-na.geetest.com&static_servers=static.geetest.com,%20dn-staticdown.qbox.me&width=100%&timeout=10000&debug=false&aspect_radio_voice=128&aspect_radio_slide=103&aspect_radio_beeline=50&aspect_radio_pencil=128&aspect_radio_click=128&voice=/static/js/voice.1.2.0.js&slide=/static/js/slide.7.8.6.js&beeline=/static/js/beeline.1.0.1.js&pencil=/static/js/pencil.1.0.3.js&click=/static/js/click.3.0.4.js HTTP/1.1
		// GET https://static.geetest.com/static/js/slide.7.8.6.js HTTP/1.1
		// GET https://static.geetest.com/static/js/gct.e7810b5b525994e2fb1f89135f8df14a.js HTTP/1.1
		// GET https://static.geetest.com/static/ant/style_https.1.2.6.css HTTP/1.1
		// GET https://static.geetest.com/pictures/gt/a330cf996/a330cf996.webp HTTP/1.1
		// GET https://static.geetest.com/pictures/gt/a330cf996/bg/86f9db021.webp HTTP/1.1
		// GET https://static.geetest.com/pictures/gt/a330cf996/slice/86f9db021.png HTTP/1.1
		// GET https://static.geetest.com/static/ant/sprite2x.1.2.6.png HTTP/1.1
		engine.StaticFS("/static", http.Dir("./static/geetest/static"))
		engine.StaticFS("/pictures", http.Dir("./static/geetest/pictures"))
	}
	engine.POST("/gate/token/verify", c.gateTokenVerify)
	port := config.GetConfig().Hk4e.DispatchHttpPort
	addr := ":" + strconv.Itoa(int(port))
	err := engine.Run(addr)
	if err != nil {
		logger.Error("gin run error: %v", err)
	}
}

func (c *Controller) autoSyncStopServerInfo() {
	ticker := time.NewTicker(time.Minute * 1)
	for {
		<-ticker.C
		c.syncStopServerInfo()
	}
}

func (c *Controller) syncStopServerInfo() {
	stopServerInfo, err := c.discoveryClient.GetStopServerInfo(context.TODO(), &api.NullMsg{})
	if err != nil {
		logger.Error("get stop server info error: %v", err)
		return
	}
	c.stopServerInfo = stopServerInfo
}

func (c *Controller) autoSyncWhiteList() {
	ticker := time.NewTicker(time.Minute * 1)
	for {
		<-ticker.C
		c.syncWhiteList()
	}
}

func (c *Controller) syncWhiteList() {
	whiteList, err := c.discoveryClient.GetWhiteList(context.TODO(), &api.NullMsg{})
	if err != nil {
		logger.Error("get white list error: %v", err)
		return
	}
	c.whiteList = whiteList
}
