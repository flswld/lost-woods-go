package controller

import (
	"net/http"

	"hk4e/node/api"

	"github.com/flswld/halo/logger"
	"github.com/gin-gonic/gin"
)

// GM 后台管理接口集合 - 停服管理 / 白名单 / 调度控制 / 在线统计
//
// 全部通过 DiscoveryClient 调 Node 的 RPC（修改集群级状态）
//
// serverStopState/serverStopChange: 停服开关
//   - 停服时 dispatch 二级响应返回 RET_STOP_SERVER 客户端弹维护对话框
//   - 用 startTime/endTime 控制停服时间窗（客户端会显示倒计时）
//
// serverWhiteList/serverWhiteAdd/serverWhiteDel: 停服期间 IP 白名单
//   - 停服时仅白名单 IP 能通过 dispatch 进游戏
//   - 用于内部测试：停服期间运维/QA 仍能登录
//
// serverDispatchCancel: 调度取消
//   - 让指定 AppVersion 的所有服务实例不接受新连接（用于滚动升级）
//   - 已有连接继续服务直到自然下线
//
// serverOnlineStats: 全服在线人数（无鉴权 监控接口）

// serverStopState 查询当前停服信息（GET /server/stop/state）
func (c *Controller) serverStopState(ctx *gin.Context) {
	stopServerInfo, err := c.discoveryClient.GetStopServerInfo(ctx.Request.Context(), &api.NullMsg{})
	if err != nil {
		logger.Error("get stop server info error: %v", err)
		ctx.JSON(http.StatusOK, &CommonRsp{Code: -1, Msg: "服务器内部错误", Data: err})
		return
	}
	ctx.JSON(http.StatusOK, &CommonRsp{Code: 0, Msg: "", Data: stopServerInfo})
}

// ServerStopChangeReq 修改停服信息请求体
//   - StopServer: 是否启用停服模式（true=停服 false=运营中）
//   - StartTime/EndTime: 停服时间窗 Unix 秒级时间戳（客户端按此显示倒计时）
type ServerStopChangeReq struct {
	StopServer bool   `json:"stop_server"`
	StartTime  uint32 `json:"start_time"`
	EndTime    uint32 `json:"end_time"`
}

// serverStopChange 修改停服开关（POST /server/stop/change）
//
// 启用停服后：
//   - dispatch 二级响应返回 RET_STOP_SERVER 客户端弹维护对话框
//   - 已在线玩家不会立即被踢 但新连接进不来（除非在白名单）
//
// 通过 SetStopServerInfo RPC 写入 Node 各服务通过 sync 周期同步
func (c *Controller) serverStopChange(ctx *gin.Context) {
	req := new(ServerStopChangeReq)
	err := ctx.ShouldBindJSON(req)
	if err != nil {
		ctx.JSON(http.StatusOK, &CommonRsp{Code: -1, Msg: "参数解析错误", Data: err})
		return
	}
	_, err = c.discoveryClient.SetStopServerInfo(ctx.Request.Context(), &api.StopServerInfo{
		StopServer: req.StopServer,
		StartTime:  req.StartTime,
		EndTime:    req.EndTime,
	})
	if err != nil {
		logger.Error("set stop server info error: %v", err)
		ctx.JSON(http.StatusOK, &CommonRsp{Code: -1, Msg: "服务器内部错误", Data: err})
		return
	}
	ctx.JSON(http.StatusOK, &CommonRsp{Code: 0, Msg: "", Data: nil})
}

// serverWhiteList 获取停服期间 IP 白名单（GET /server/white/list）
// 白名单 IP 在停服模式下仍可正常登录 用于内部测试/QA 在维护期间访问
func (c *Controller) serverWhiteList(ctx *gin.Context) {
	whiteList, err := c.discoveryClient.GetWhiteList(ctx.Request.Context(), &api.NullMsg{})
	if err != nil {
		logger.Error("get white list error: %v", err)
		ctx.JSON(http.StatusOK, &CommonRsp{Code: -1, Msg: "服务器内部错误", Data: err})
		return
	}
	ctx.JSON(http.StatusOK, &CommonRsp{Code: 0, Msg: "", Data: whiteList.IpAddrList})
}

// ServerWhiteAdd 添加白名单 IP 请求体
type ServerWhiteAdd struct {
	IpAddr string `json:"ip_addr"`
}

// serverWhiteAdd 添加 IP 到停服白名单（POST /server/white/add）
// 走 SetWhiteList RPC 设 IsAdd=true
func (c *Controller) serverWhiteAdd(ctx *gin.Context) {
	req := new(ServerWhiteAdd)
	err := ctx.ShouldBindJSON(req)
	if err != nil {
		ctx.JSON(http.StatusOK, &CommonRsp{Code: -1, Msg: "参数解析错误", Data: err})
		return
	}
	_, err = c.discoveryClient.SetWhiteList(ctx.Request.Context(), &api.SetWhiteListReq{
		IsAdd:  true,
		IpAddr: req.IpAddr,
	})
	if err != nil {
		logger.Error("set white list error: %v", err)
		ctx.JSON(http.StatusOK, &CommonRsp{Code: -1, Msg: "服务器内部错误", Data: err})
		return
	}
	ctx.JSON(http.StatusOK, &CommonRsp{Code: 0, Msg: "", Data: nil})
}

// ServerWhiteDel 删除白名单 IP 请求体
type ServerWhiteDel struct {
	IpAddr string `json:"ip_addr"`
}

// serverWhiteDel 从停服白名单移除 IP（POST /server/white/del）
// 走 SetWhiteList RPC 设 IsAdd=false
func (c *Controller) serverWhiteDel(ctx *gin.Context) {
	req := new(ServerWhiteDel)
	err := ctx.ShouldBindJSON(req)
	if err != nil {
		ctx.JSON(http.StatusOK, &CommonRsp{Code: -1, Msg: "参数解析错误", Data: err})
		return
	}
	_, err = c.discoveryClient.SetWhiteList(ctx.Request.Context(), &api.SetWhiteListReq{
		IsAdd:  false,
		IpAddr: req.IpAddr,
	})
	if err != nil {
		logger.Error("set white list error: %v", err)
		ctx.JSON(http.StatusOK, &CommonRsp{Code: -1, Msg: "服务器内部错误", Data: err})
		return
	}
	ctx.JSON(http.StatusOK, &CommonRsp{Code: 0, Msg: "", Data: nil})
}

// ServerDispatchCancel 调度取消请求体
//   - AppVersion: 要取消调度的服务实例版本号（编译时注入的 APPVERSION）
type ServerDispatchCancel struct {
	AppVersion string `json:"app_version"`
}

// serverDispatchCancel 取消指定版本服务实例的调度（POST /server/dispatch/cancel）
//
// 用于滚动升级：
//  1. 启动新版本 GS（v2.0）→ 玩家可登录到新 GS
//  2. 调此接口设 v1.0 dispatchCancel=true → v1.0 GS 不再接受新连接
//  3. 等 v1.0 GS 上的玩家自然下线 → 安全停掉旧版本实例
//
// 已有连接继续服务 不强制踢人
func (c *Controller) serverDispatchCancel(ctx *gin.Context) {
	req := new(ServerDispatchCancel)
	err := ctx.ShouldBindJSON(req)
	if err != nil {
		ctx.JSON(http.StatusOK, &CommonRsp{Code: -1, Msg: "参数解析错误", Data: err})
		return
	}
	_, err = c.discoveryClient.ServerDispatchCancel(ctx.Request.Context(), &api.ServerDispatchCancelReq{
		AppVersion: req.AppVersion,
	})
	if err != nil {
		logger.Error("server dispatch cancel error: %v", err)
		ctx.JSON(http.StatusOK, &CommonRsp{Code: -1, Msg: "服务器内部错误", Data: err})
		return
	}
	ctx.JSON(http.StatusOK, &CommonRsp{Code: 0, Msg: "", Data: nil})
}

// ServerOnlineStats 在线统计响应体
type ServerOnlineStats struct {
	TotalOnlinePlayerNum uint32 `json:"total_online_player_num"`
}

// serverOnlineStats 全服在线人数（GET /server/online/stats）
//
// **不需要鉴权**：用于外部监控系统采集（Prometheus/Grafana 等）
// 直接读本地缓存的 globalGsOnlineMap.size 不调 RPC（避免高频调用打 Node）
// 数据滞后最多 60s（globalGsOnlineMap 同步周期）
func (c *Controller) serverOnlineStats(ctx *gin.Context) {
	num := 0
	c.globalGsOnlineMapLock.RLock()
	num = len(c.globalGsOnlineMap)
	c.globalGsOnlineMapLock.RUnlock()
	ctx.JSON(http.StatusOK, &CommonRsp{Code: 0, Msg: "", Data: &ServerOnlineStats{TotalOnlinePlayerNum: uint32(num)}})
}
