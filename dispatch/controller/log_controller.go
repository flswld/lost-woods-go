package controller

import (
	"hk4e/dispatch/model"

	"github.com/flswld/halo/logger"
	"github.com/gin-gonic/gin"
)

// 客户端日志上报接口集合 - 主要是接收并丢弃客户端的各种日志
//
// 接口分类：
//   - sdkDataUpload: SDK 行为日志（账号登录/付费等）
//   - perfConfigVerify/perfDataUpload: 性能数据（FPS/掉帧/网络延迟）
//   - log8888: 客户端运行时日志（错误/警告 项目唯一会真打印的）
//   - crashDataUpload: 客户端崩溃日志
//
// 所有接口都返回 {"code":0} 表示"上报成功"
// 项目只有 log8888 会真把日志记录到服务端 logger.Error 其他全部丢弃
//   原因：客户端崩溃日志通常带敏感信息 项目作者不想保留这些
//   保留 log8888 是因为客户端 ScriptError/Lua 错误对调试有帮助

// POST https://log-upload-os.mihoyo.com/sdk/dataUpload HTTP/1.1
// sdkDataUpload SDK 行为日志（账号/支付） 直接丢弃
func (c *Controller) sdkDataUpload(ctx *gin.Context) {
	ctx.Header("Content-type", "application/json")
	_, _ = ctx.Writer.WriteString("{\"code\":0}")
}

// GET http://log-upload-os.hoyoverse.com/perf/config/verify?device_id=dd664c97f924af747b4576a297c132038be239291651474673768&platform=2&name=DESKTOP-EDUS2DL HTTP/1.1
func (c *Controller) perfConfigVerify(ctx *gin.Context) {
	ctx.Header("Content-type", "application/json")
	_, _ = ctx.Writer.WriteString("{\"code\":0}")
}

// POST http://log-upload-os.hoyoverse.com/perf/dataUpload HTTP/1.1
func (c *Controller) perfDataUpload(ctx *gin.Context) {
	ctx.Header("Content-type", "application/json")
	_, _ = ctx.Writer.WriteString("{\"code\":0}")
}

// log8888 客户端运行时日志（**唯一保留输出的**）
// 客户端发现 Lua 错误 / ScriptError / 异常调用时会上报到这个接口
// 项目用 logger.Error 记录到服务端日志 方便调试客户端配置错误
// POST http://overseauspider.yuanshen.com:8888/log HTTP/1.1
func (c *Controller) log8888(ctx *gin.Context) {
	clientLog := new(model.ClientLog)
	err := ctx.ShouldBindJSON(clientLog)
	if err != nil {
		logger.Error("parse client log error: %v", err)
		return
	}
	logger.Error("[ClientLog] %+v", clientLog)
	ctx.Header("Content-type", "application/json")
	_, _ = ctx.Writer.WriteString("{\"code\":0}")
}

// POST http://log-upload-os.hoyoverse.com/crash/dataUpload HTTP/1.1
func (c *Controller) crashDataUpload(ctx *gin.Context) {
	ctx.Header("Content-type", "application/json")
	_, _ = ctx.Writer.WriteString("{\"code\":0}")
}
