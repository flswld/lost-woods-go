package service

import (
	"context"
	"time"

	"hk4e/gs/api"
	"hk4e/gs/game"
)

// GMService GM 命令 RPC 服务实现（被 gm/ HTTP 后台 + 其他 GS 调用）
//
// 编译期断言（_ = nil）确保 GMService 实现了 api.GMNATSRPCServer 接口
// 接口由 protoc-gen-natsrpc 从 gs/api/*.proto 生成

var _ api.GMNATSRPCServer = (*GMService)(nil)

type GMService struct {
	g *game.Game
}

// Cmd 处理 GM 命令 RPC 调用（同步等待结果）
//
// 流程：
//  1. 把命令消息塞进 commandMessageInput 通道（异步交给主循环）
//  2. resultChan 等待主循环执行完成的回调
//  3. 10 秒超时（防止主循环卡住时调用方永久等待）
//
// 这样设计是因为：
//   - GMService 的 Cmd 是 NATS RPC 同步接口（调用方期望马上拿结果）
//   - GS 主循环单线程串行 不能阻塞 NATS goroutine 等结果
//   - 通过 channel 解耦：NATS goroutine 投递 → 主循环执行 → channel 回结果
//
// 调用链：HTTP /gm/cmd → gm/ 服务 → natsrpc → GS.GMService.Cmd → 主循环 CallGMCmd
func (s *GMService) Cmd(ctx context.Context, req *api.CmdRequest) (*api.CmdReply, error) {
	commandTextInput := game.COMMAND_MANAGER.GetCommandMessageInput()
	resultChan := make(chan *game.GMCmdResult)
	commandTextInput <- &game.CommandMessage{
		GMType:     game.SystemFuncGM,
		FuncName:   req.FuncName,
		ParamList:  req.ParamList,
		ResultChan: resultChan,
	}
	timer := time.NewTimer(time.Second * 10)
	var cmdReply *api.CmdReply = nil
	select {
	case <-timer.C:
		cmdReply = &api.CmdReply{Code: -1, Message: "执行结果等待超时"}
	case result := <-resultChan:
		cmdReply = &api.CmdReply{Code: result.Code, Message: result.Msg}
	}
	timer.Stop()
	return cmdReply, nil
}
