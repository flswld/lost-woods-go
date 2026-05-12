package controller

import (
	"net/http"

	"hk4e/common/mq"
	"hk4e/common/rpc"
	"hk4e/gs/api"

	"github.com/flswld/halo/logger"
	"github.com/gin-gonic/gin"
)

// GM 后台 HTTP 接口（运维入口）
//
// 调用方：运维通过 HTTP 接口调 GM 命令（与玩家私聊"小可爱"不同）
// 鉴权：通过 Header GmAuthKey（默认 "flswld"）防止未授权访问
//
// 三种执行方式：
//   1. 指定 GsId：通过 natsrpc 直连目标 GS 的 GMService（同步等结果）
//      · 走 rpc.NewGMClient 拿连接 缓存到 gmClientMap 复用
//   2. 指定 GsAppId：通过 MQ 发 ServerGmCmdNotify 到目标 GS（异步不等结果）
//   3. 全服执行：广播到所有 GS（异步不等结果 通常用于停服公告等）

// GmCmdReq HTTP 请求体
//   - FuncName: 要调用的 GM 函数名（gs/game/game_command_gm.go 中的 GMxxx 方法）
//   - ParamList: 字符串参数列表（接收端按方法签名反射转换类型）
//   - GsId / GsAppid: 二选一指定执行目标 都不传则全服执行
type GmCmdReq struct {
	FuncName  string   `json:"func_name"`
	ParamList []string `json:"param_list"`
	GsId      uint32   `json:"gs_id"`
	GsAppid   string   `json:"gs_appid"`
}

type GmCmdRsp struct {
	ResultCode int32  `json:"result_code"`
	ResultMsg  string `json:"result_msg"`
	Desc       string `json:"desc"`
}

// gmCmd GM 命令统一入口（POST /cmd）
//
// 按 GsId / GsAppid 是否设置走三种路径：
//  1. GsId != 0: natsrpc 同步调用 等待结果返回（最常用 能拿到执行结果）
//  2. GsAppid != "": MQ 发到指定 GS（异步 仅 ServerGmCmdNotify 通知）
//  3. 都为空: 全服广播（用于全服公告/全服重载配置等）
//
// gmClientMap 缓存 natsrpc 客户端连接 避免每次新建（连接复用）
func (c *Controller) gmCmd(ctx *gin.Context) {
	gmCmdReq := new(GmCmdReq)
	err := ctx.ShouldBindJSON(gmCmdReq)
	if err != nil {
		logger.Error("parse json error: %v", err)
		ctx.JSON(http.StatusOK, &CommonRsp{Code: -1, Msg: "参数解析错误", Data: err})
		return
	}
	logger.Info("GmCmdReq: %v", gmCmdReq)
	if gmCmdReq.GsId != 0 {
		// 指定GSID执行
		c.gmClientMapLock.RLock()
		gmClient, exist := c.gmClientMap[gmCmdReq.GsId]
		c.gmClientMapLock.RUnlock()
		if !exist {
			var err error = nil
			gmClient, err = rpc.NewGMClient(gmCmdReq.GsId)
			if err != nil {
				logger.Error("new gm client error: %v", err)
				ctx.JSON(http.StatusOK, &CommonRsp{Code: -1, Msg: "服务器内部错误", Data: err})
				return
			}
			c.gmClientMapLock.Lock()
			c.gmClientMap[gmCmdReq.GsId] = gmClient
			c.gmClientMapLock.Unlock()
		}
		rsp, err := gmClient.Cmd(ctx.Request.Context(), &api.CmdRequest{
			FuncName:  gmCmdReq.FuncName,
			ParamList: gmCmdReq.ParamList,
		})
		if err != nil {
			ctx.JSON(http.StatusOK, &CommonRsp{Code: -1, Msg: "服务器内部错误", Data: err})
			return
		}
		ctx.JSON(http.StatusOK, &CommonRsp{Code: 0, Msg: "", Data: &GmCmdRsp{ResultCode: rsp.Code, ResultMsg: rsp.Message, Desc: "指定GSID执行"}})
	} else if gmCmdReq.GsAppid != "" {
		// 指定GSAPPID执行
		c.messageQueue.SendToGs(gmCmdReq.GsAppid, &mq.NetMsg{
			MsgType: mq.MsgTypeServer,
			EventId: mq.ServerGmCmdNotify,
			ServerMsg: &mq.ServerMsg{
				GmCmdFuncName:  gmCmdReq.FuncName,
				GmCmdParamList: gmCmdReq.ParamList,
			},
		})
		ctx.JSON(http.StatusOK, &CommonRsp{Code: 0, Msg: "", Data: &GmCmdRsp{ResultCode: 0, ResultMsg: "", Desc: "指定GSAPPID执行"}})
	} else {
		// 全服GS执行
		c.messageQueue.SendToAll(&mq.NetMsg{
			MsgType: mq.MsgTypeServer,
			EventId: mq.ServerGmCmdNotify,
			ServerMsg: &mq.ServerMsg{
				GmCmdFuncName:  gmCmdReq.FuncName,
				GmCmdParamList: gmCmdReq.ParamList,
			},
		})
		ctx.JSON(http.StatusOK, &CommonRsp{Code: 0, Msg: "", Data: &GmCmdRsp{ResultCode: 0, ResultMsg: "", Desc: "全服GS执行"}})
	}
}
