package game

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"hk4e/gs/model"

	"github.com/flswld/halo/logger"
)

// GM 命令管理器模块
//
// 项目最重要的运维入口（详见 CLAUDE.md "三种 GM 入口"）：
//   - **主入口**: 玩家私聊"小可爱"AI 发命令（PrivateChatReq → PlayerInputCommand）
//     · 原神官方客户端没有 GM 输入框 但好友私聊有 → 用 AI 好友绕过此限制
//   - HTTP 后台：GM 服务（gm/）通过 RPC 调到 GS 的 GMService.Cmd → CallGMCmd
//   - 客户端 GM Talk：开发版客户端 GmTalkReq 走 SystemFuncGM/DevClientGM 两种格式
//
// 命令格式（与原神官方内部 GM 一致 不是 grasscutter 风格）：
//   - 不带 `/` 前缀 直接发命令文本
//   - 形如 `命令名 子模式 参数...` 或 `命令名 参数...`
//   - 例：`item add 1234 5`（加道具）、`goto 100 200 300`（传送）、`monster 21010101 5 90`（刷怪）
//   - 完整命令清单见 game_command_controller.go InitController 注册的 25 个 controller
//
// 命令解析流程：
//  1. 玩家输入 "item add 1234 5" → CommandManager.PlayerInputCommand 入队
//  2. 主循环 select 收到 CommandMessage → HandleCommand
//  3. 按命令名（item）查找 CommandController → 校验 CmdPerm（部分命令需要 GM 权限）
//  4. 解析参数 → 调用 CommandFunc 执行
//  5. 通过 SendPrivateChat 把执行结果回复给玩家（"小可爱"发的私聊）
//
// 命令分两类：
//   - 玩家命令（CommandPermNormal）：所有玩家都能用 如 /help
//   - GM 命令（CommandPermGM）：需要权限 玩家档 CmdPerm >= GM 才能用 大部分命令是这种
//
// 流式参数解析：
//   - Must("string", func(p)...): 必填参数（缺失则失败）
//   - Option("int", func(p)...): 可选参数
//   - Array("uint32", func(p)...): 数组参数（剩余所有参数都按此类型解析）
//   - Execute(thenFunc): 解析完成后调用主逻辑
//
// 颜色消息：通过 <color=#XXXXXX>text</color> 标签 客户端聊天面板会渲染颜色
//   绿色=成功 红色=失败 黄色=帮助标题 等

// CommandPerm 命令权限等级
// 0 为普通玩家 数越大权限越大
type CommandPerm uint8

const (
	CommandPermNormal = CommandPerm(iota) // 普通玩家
	CommandPermGM                         // 管理员
)

// CommandFunc 命令执行函数
type CommandFunc func(content *CommandContent) bool

const (
	PlayerChatGM = iota // 玩家聊天 GM（主入口 私聊"小可爱" "item add 1234 5" 不带 / 前缀）
	SystemFuncGM        // 系统函数 GM（开发版客户端用 "@@FuncName(p1,p2,...)" 格式）
	DevClientGM         // 开发客户端 GM（开发版 GmTalk 输入框 普通文本走聊天命令）
)

// CommandMessage 命令消息
// 给下层执行命令时提供数据
type CommandMessage struct {
	GMType int // GM类型
	// 玩家聊天GM以及开发客户端GM
	Executor *model.Player // 执行者
	Text     string        // 命令文本
	// 系统函数GM
	FuncName   string            // 函数名
	ParamList  []string          // 函数参数列表
	ResultChan chan *GMCmdResult // 执行结果返回管道
}

type GMCmdResult struct {
	Code int32
	Msg  string
}

// CommandContentStepFunc 命令步骤处理函数
type CommandContentStepFunc func(p any) bool

// CommandContentParamType 命令参数类型
type CommandContentParamType uint8

const (
	CommandContentParamTypeNone   = CommandContentParamType(iota)
	CommandContentParamTypeMust   // 必填
	CommandContentParamTypeOption // 可选
	CommandContentParamTypeArray  // 数组
)

// CommandContentStep 命令步骤结构
type CommandContentStep struct {
	ParamType      CommandContentParamType // 参数类型
	ParamValueType string                  // 参数数值类型
	StepFunc       CommandContentStepFunc  // 处理函数
}

// CommandContent 命令内容
type CommandContent struct {
	Executor     *model.Player      // 执行者
	AssignPlayer *model.Player      // 指定玩家
	Name         string             // 玩家输入的命令名
	ParamList    []string           // 玩家输入的参数列表
	Controller   *CommandController // 命令控制器
	// 执行时数据
	paramIndex uint8                 // 当前执行到的参数索引
	elseFunc   func()                // 参数错误处理函数
	stepList   []*CommandContentStep // 步骤处理函数列表
}

// SendMessage 发送消息
func (c *CommandContent) SendMessage(player *model.Player, msg string, param ...any) {
	GAME.SendPrivateChat(COMMAND_MANAGER.system, player.PlayerId, fmt.Sprintf(msg, param...))
}

// SendColorMessage 发送颜色消息
func (c *CommandContent) SendColorMessage(player *model.Player, color, text string, param ...any) {
	c.SendMessage(player, "<color=%v>%v</color>", color, fmt.Sprintf(text, param...))
}

// SendSuccMessage 发送成功颜色消息
func (c *CommandContent) SendSuccMessage(player *model.Player, text string, param ...any) {
	c.SendColorMessage(player, "#CCFFCC", text, param...)
}

// SendFailMessage 发送失败颜色消息
func (c *CommandContent) SendFailMessage(player *model.Player, text string, param ...any) {
	c.SendColorMessage(player, "#FF9999", text, param...)
}

// getNextParam 获取下一个参数
func (c *CommandContent) getNextParam(typeStr string) (param any, ok bool) {
	// 索引变更
	c.paramIndex++
	// 确保参数长度足够 -1是因为第一次的时候也是获取下一个参数
	if len(c.ParamList) <= int(c.paramIndex)-1 {
		return
	}
	// 获取字符串参数
	paramStr := c.ParamList[c.paramIndex-1]
	// 转换参数类型
	switch typeStr {
	case "int":
		val, err := strconv.ParseInt(paramStr, 10, 64)
		if err != nil {
			return
		}
		return int(val), true
	case "uint":
		val, err := strconv.ParseUint(paramStr, 10, 64)
		if err != nil {
			return
		}
		return uint(val), true
	case "int8":
		val, err := strconv.ParseInt(paramStr, 10, 8)
		if err != nil {
			return
		}
		return int8(val), true
	case "uint8":
		val, err := strconv.ParseUint(paramStr, 10, 8)
		if err != nil {
			return
		}
		return uint8(val), true
	case "int16":
		val, err := strconv.ParseInt(paramStr, 10, 16)
		if err != nil {
			return
		}
		return int16(val), true
	case "uint16":
		val, err := strconv.ParseUint(paramStr, 10, 16)
		if err != nil {
			return
		}
		return uint16(val), true
	case "int32":
		val, err := strconv.ParseInt(paramStr, 10, 32)
		if err != nil {
			return
		}
		return int32(val), true
	case "uint32":
		val, err := strconv.ParseUint(paramStr, 10, 32)
		if err != nil {
			return
		}
		return uint32(val), true
	case "int64":
		val, err := strconv.ParseInt(paramStr, 10, 64)
		if err != nil {
			return
		}
		return val, true
	case "uint64":
		val, err := strconv.ParseUint(paramStr, 10, 64)
		if err != nil {
			return
		}
		return val, true
	case "float32":
		val, err := strconv.ParseFloat(paramStr, 32)
		if err != nil {
			return
		}
		return float32(val), true
	case "float64":
		val, err := strconv.ParseFloat(paramStr, 64)
		if err != nil {
			return
		}
		return val, true
	case "bool":
		val, err := strconv.ParseBool(paramStr)
		if err != nil {
			return
		}
		return val, true
	case "string":
		return paramStr, true
	default:
		return
	}
}

// Must 必填参数执行
func (c *CommandContent) Must(typeStr string, stepFunc CommandContentStepFunc) *CommandContent {
	step := &CommandContentStep{
		ParamType:      CommandContentParamTypeMust,
		ParamValueType: typeStr,
		StepFunc:       stepFunc,
	}
	c.stepList = append(c.stepList, step)
	return c
}

// Option 可选参数执行
func (c *CommandContent) Option(typeStr string, stepFunc CommandContentStepFunc) *CommandContent {
	step := &CommandContentStep{
		ParamType:      CommandContentParamTypeOption,
		ParamValueType: typeStr,
		StepFunc:       stepFunc,
	}
	c.stepList = append(c.stepList, step)
	return c
}

func (c *CommandContent) Array(typeStr string, stepFunc CommandContentStepFunc) *CommandContent {
	step := &CommandContentStep{
		ParamType:      CommandContentParamTypeArray,
		ParamValueType: typeStr,
		StepFunc:       stepFunc,
	}
	c.stepList = append(c.stepList, step)
	return c
}

// Execute 解析参数并执行命令业务（流式 API 的终点）
//
// 参数解析规则：
//   - Must: 必须按顺序提供 数量不足时返回 false
//   - Option: 在 Must 之后 数量不够则跳过
//   - Array: 必须放最后（吃掉剩余所有参数）
//
// 任意 stepFunc 返回 false → 整体执行失败
// 全部 stepFunc 通过后调用 thenFunc 执行业务
func (c *CommandContent) Execute(thenFunc func() bool) bool {
	// 获取必填参数的数量
	mustParamExecCount := 0
	for _, step := range c.stepList {
		if step.ParamType == CommandContentParamTypeMust {
			mustParamExecCount++
		}
	}
	// 计算可选参数可执行的数量
	optionParamExecCount := len(c.ParamList) - mustParamExecCount
	// 可选参数可执行的数量为负数代表肯定有个必填参数缺少
	if optionParamExecCount < 0 {
		return false
	}
	// 执行每个步骤
	for index, step := range c.stepList {
		if step.ParamType == CommandContentParamTypeArray {
			if index != len(c.stepList)-1 {
				continue
			}
			paramList := make([]any, 0)
			for {
				param, ok := c.getNextParam(step.ParamValueType)
				if !ok {
					break
				}
				paramList = append(paramList, param)
			}
			if !step.StepFunc(paramList) {
				return false
			}
			break
		}
		// 确保为可选参数 参数不足则不执行
		if step.ParamType == CommandContentParamTypeOption {
			// 参数数量不足时跳过可选参数
			if optionParamExecCount == 0 {
				continue
			}
			// 没有跳过代表后面会执行本次可选参数
			optionParamExecCount--
		}
		// 获取当前参数
		param, ok := c.getNextParam(step.ParamValueType)
		if !ok {
			return false
		}
		// 执行处理函数
		if !step.StepFunc(param) {
			return false
		}
	}
	// 执行命令业务逻辑
	if thenFunc() {
		return true
	}
	return false
}

// SetElse 设置参数执行错误处理
func (c *CommandContent) SetElse(elseFunc func()) {
	c.elseFunc = elseFunc
}

// CommandManager 命令管理器
type CommandManager struct {
	system                *model.Player                 // GM指令聊天消息机器人
	commandControllerList []*CommandController          // 命令控制器注册列表
	commandControllerMap  map[string]*CommandController // 记录命令控制器
	commandMessageInput   chan *CommandMessage          // 传输要处理的命令消息
	gmCmd                 *GMCmd
	gmCmdRefValue         reflect.Value
}

// NewCommandManager 新建命令管理器
func NewCommandManager() *CommandManager {
	r := new(CommandManager)
	// 初始化
	r.commandControllerList = make([]*CommandController, 0)
	r.commandControllerMap = make(map[string]*CommandController)
	r.commandMessageInput = make(chan *CommandMessage, 1000)
	// 初始化命令控制器
	r.InitController()
	r.gmCmd = new(GMCmd)
	r.gmCmdRefValue = reflect.ValueOf(r.gmCmd)
	return r
}

func (c *CommandManager) GetCommandMessageInput() chan *CommandMessage {
	return c.commandMessageInput
}

// SetSystem 设置GM指令聊天消息机器人
func (c *CommandManager) SetSystem(system *model.Player) {
	c.system = system
}

// RegAllController 注册所有命令控制器
func (c *CommandManager) RegAllController(controllerList ...*CommandController) {
	for _, controller := range controllerList {
		c.RegController(controller)
	}
}

// RegController 注册命令控制器
func (c *CommandManager) RegController(controller *CommandController) {
	// 支持一个命令拥有多个别名
	for _, name := range controller.AliasList {
		// 命令名统一转为小写
		name = strings.ToLower(name)
		// 如果命令已注册则报错 后者覆盖前者
		_, ok := c.commandControllerMap[name]
		if ok {
			// 别名重复注册提示功能
			controller.Func = func(content *CommandContent) bool {
				content.SetElse(func() {
					content.SendFailMessage(content.Executor, "命令别名重复注册，重复的别名：%v。", name)
				})
				return false
			}
			logger.Error("register command repeat, name: %v", name)
		}
		// 记录命令
		c.commandControllerMap[name] = controller
	}
	c.commandControllerList = append(c.commandControllerList, controller)
}

// DelAllController 卸载所有命令控制器
func (c *CommandManager) DelAllController(controllerList ...*CommandController) {
	for _, controller := range controllerList {
		c.DelController(controller)
	}
}

// DelController 卸载命令控制器
func (c *CommandManager) DelController(controller *CommandController) {
	// 支持一个命令拥有多个别名
	for _, name := range controller.AliasList {
		delete(c.commandControllerMap, name)
	}
	// 卸载列表上的控制器
	for i, commandController := range c.commandControllerList {
		if commandController == controller {
			c.commandControllerList = append(c.commandControllerList[:i], c.commandControllerList[i+1:]...)
		}
	}
}

// PlayerInputCommand 玩家私聊输入命令的入口（PrivateChatReq 调用）
//
// 关键约束：
//   - 仅识别私聊"小可爱"AI 的消息（targetUid == system.PlayerId）
//   - AI 世界中禁用（PUBG 玩法时不能用 GM 命令 防止破坏游戏平衡）
//
// 通过 commandMessageInput 通道把命令丢给主循环 select 处理（异步化）
// 主循环收到后调 HandleCommand → ExecCommand 实际执行
func (c *CommandManager) PlayerInputCommand(player *model.Player, targetUid uint32, text string) {
	// 机器人不会读命令所以写到了 PrivateChatReq

	// 确保私聊的目标是处理命令的机器人
	if targetUid != c.system.PlayerId {
		return
	}

	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world != nil && WORLD_MANAGER.IsAiWorld(world) {
		return
	}

	// 输入的命令将在主协程中处理
	c.commandMessageInput <- &CommandMessage{
		GMType:   PlayerChatGM,
		Executor: player,
		Text:     text,
	}
}

// CallGMCmd 反射调用 GMCmd 类型上的方法（系统函数 GM 入口 + HTTP 后台 GM 入口）
//
// 调用方：
//   - GMService.Cmd RPC（gm/ HTTP 后台）传入 FuncName + 字符串参数列表
//   - GmTalkReq "@@FuncName(p1,p2,...)" 格式
//
// 处理：
//  1. 反射查找 c.gmCmd 上的方法
//  2. 校验参数数量
//  3. 按方法签名的参数类型字符串转换（int/uint8/float64/bool/string 等）
//  4. 调用方法 把返回值序列化为 JSON 字符串返回
//
// GMCmd 是另一组 GM 命令实现（详见 game_command_gm.go）数十个 GMXxx 方法
// 这套机制让运维可以通过 HTTP 后台远程操控游戏服 不需要进客户端
func (c *CommandManager) CallGMCmd(funcName string, paramList []string) (bool, string) {
	fn := c.gmCmdRefValue.MethodByName(funcName)
	if !fn.IsValid() {
		logger.Error("gm func not valid, func: %v", funcName)
		return false, ""
	}
	if fn.Type().NumIn() != len(paramList) {
		logger.Error("gm func param num not match, func: %v, need: %v, give: %v", funcName, fn.Type().NumIn(), len(paramList))
		return false, ""
	}
	in := make([]reflect.Value, fn.Type().NumIn())
	for i := 0; i < fn.Type().NumIn(); i++ {
		kind := fn.Type().In(i).Kind()
		param := paramList[i]
		var value reflect.Value
		switch kind {
		case reflect.Int:
			val, err := strconv.ParseInt(param, 10, 64)
			if err != nil {
				return false, ""
			}
			value = reflect.ValueOf(int(val))
		case reflect.Uint:
			val, err := strconv.ParseUint(param, 10, 64)
			if err != nil {
				return false, ""
			}
			value = reflect.ValueOf(uint(val))
		case reflect.Int8:
			val, err := strconv.ParseInt(param, 10, 8)
			if err != nil {
				return false, ""
			}
			value = reflect.ValueOf(int8(val))
		case reflect.Uint8:
			val, err := strconv.ParseUint(param, 10, 8)
			if err != nil {
				return false, ""
			}
			value = reflect.ValueOf(uint8(val))
		case reflect.Int16:
			val, err := strconv.ParseInt(param, 10, 16)
			if err != nil {
				return false, ""
			}
			value = reflect.ValueOf(int16(val))
		case reflect.Uint16:
			val, err := strconv.ParseUint(param, 10, 16)
			if err != nil {
				return false, ""
			}
			value = reflect.ValueOf(uint16(val))
		case reflect.Int32:
			val, err := strconv.ParseInt(param, 10, 32)
			if err != nil {
				return false, ""
			}
			value = reflect.ValueOf(int32(val))
		case reflect.Uint32:
			val, err := strconv.ParseUint(param, 10, 32)
			if err != nil {
				return false, ""
			}
			value = reflect.ValueOf(uint32(val))
		case reflect.Int64:
			val, err := strconv.ParseInt(param, 10, 64)
			if err != nil {
				return false, ""
			}
			value = reflect.ValueOf(val)
		case reflect.Uint64:
			val, err := strconv.ParseUint(param, 10, 64)
			if err != nil {
				return false, ""
			}
			value = reflect.ValueOf(val)
		case reflect.Float32:
			val, err := strconv.ParseFloat(param, 32)
			if err != nil {
				return false, ""
			}
			value = reflect.ValueOf(float32(val))
		case reflect.Float64:
			val, err := strconv.ParseFloat(param, 64)
			if err != nil {
				return false, ""
			}
			value = reflect.ValueOf(val)
		case reflect.Bool:
			val, err := strconv.ParseBool(param)
			if err != nil {
				return false, ""
			}
			value = reflect.ValueOf(val)
		case reflect.String:
			value = reflect.ValueOf(param)
		default:
			return false, ""
		}
		in[i] = value
	}
	out := fn.Call(in)
	ret := make([]any, 0)
	for _, v := range out {
		ret = append(ret, v.Interface())
	}
	data, _ := json.Marshal(ret)
	return true, string(data)
}

// HandleCommand 处理命令
// 主协程接收到命令消息后执行
func (c *CommandManager) HandleCommand(command *CommandMessage) {
	switch command.GMType {
	case PlayerChatGM, DevClientGM:
		logger.Info("run gm cmd, text: %v, uid: %v", command.Text, command.Executor.PlayerId)
		// 执行命令
		c.ExecCommand(command)
	case SystemFuncGM:
		logger.Info("run gm func, funcName: %v, paramList: %v", command.FuncName, command.ParamList)
		// 反射调用game_command_gm.go中的函数并反射解析传入参数类型
		ok, ret := c.CallGMCmd(command.FuncName, command.ParamList)
		if command.ResultChan != nil {
			var gmCmdResult *GMCmdResult = nil
			if ok {
				gmCmdResult = &GMCmdResult{Code: 0, Msg: ret}
			} else {
				gmCmdResult = &GMCmdResult{Code: -1, Msg: ""}
			}
			command.ResultChan <- gmCmdResult
			close(command.ResultChan)
		}
	}
}

// ExecCommand 实际执行玩家聊天 GM 命令（PlayerChatGM/DevClientGM 共用）
//
// 处理：
//  1. 按空格分割命令文本（如"give 1234 5"→ ["give", "1234", "5"]）
//  2. 命令名转小写 查找 controllerMap
//  3. 校验玩家权限（CmdPerm < controller.Perm 时拒绝）
//  4. 处理 CommandAssignUid（"@uid xxx" 让 GM 给指定玩家执行命令）
//  5. 调用 controller.Func(content)
//  6. 失败时打印 controller.UsageList 帮助提示（{alias} 替换为实际命令名）
func (c *CommandManager) ExecCommand(cmd *CommandMessage) {
	// 命令内容
	content := new(CommandContent)
	content.Executor = cmd.Executor
	// 默认指定玩家为执行者
	content.AssignPlayer = cmd.Executor

	// 分割出命令的每个参数
	cmdSplit := strings.Split(cmd.Text, " ")
	// 分割出来啥也没有可能是个空的字符串
	// 此时将会返回的命令名和命令参数都为空
	if len(cmdSplit) == 0 {
		content.SendFailMessage(content.Executor, "命令错误：命令名为空。")
		return
	}
	// 有些命令没有参数 也要适配
	var paramList []string
	if len(cmdSplit) >= 2 {
		paramList = cmdSplit[1:]
	}
	// 不区分命令名的大小写 统一转为小写
	content.Name = strings.ToLower(cmdSplit[0]) // 首个参数必是命令名
	content.ParamList = paramList               // 命令名后当然是命令的参数喽

	// 判断命令是否注册
	controller, ok := c.commandControllerMap[content.Name]
	if !ok {
		// 玩家可能会执行一些没有的命令仅做调试输出
		content.SendFailMessage(content.Executor, "命令 %v 不存在，你输入的命令 %v，输入 help 查看帮助。", content.Name, cmd.Text)
		return
	}
	// 设置控制器
	content.Controller = controller
	// 判断玩家的权限是否符合要求
	player := content.Executor
	if ok && player.CmdPerm < uint8(controller.Perm) {
		content.SendFailMessage(content.Executor, "权限不足，该命令需要%v级权限。\n你目前的权限等级：%v", controller.Perm, player.CmdPerm)
		return
	}
	// 命令指定uid
	if player.CommandAssignUid != 0 {
		// 判断指定玩家是否在线
		target := USER_MANAGER.GetOnlineUser(player.CommandAssignUid)
		// 目标玩家属于非本地玩家
		if target == nil {
			content.SendFailMessage(content.Executor, "命令执行失败，指定玩家离线或不在当前服务器。")
			return
		}
		content.AssignPlayer = target
	}
	// 执行命令
	if controller.Func(content) {
		// 命令执行过程中没有问题就跳出
		return
	}
	// 命令参数错误处理
	if content.elseFunc == nil {
		// 默认的错误处理
		usage := "命令用法：\n"
		for i, s := range controller.UsageList {
			s = strings.ReplaceAll(s, "{alias}", content.Name)
			usage += fmt.Sprintf("%v. %v", i+1, s)
			// 换行
			if i != len(controller.UsageList)-1 {
				usage += "\n"
			}
		}
		content.SendFailMessage(content.Executor, "参数或格式错误，正确用法：\n\n<color=white>%v</color>", usage)
	} else {
		// 自定义的错误处理
		content.elseFunc()
	}
}
