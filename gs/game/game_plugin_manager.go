package game

import (
	"errors"
	"fmt"
	"reflect"
	"sort"

	"hk4e/gs/model"
	"hk4e/protocol/proto"

	"github.com/flswld/halo/logger"
)

// 游戏服务器插件管理器（AOP 切面式扩展）
//
// 设计意图：在原神官方协议处理流程上叠加自创玩法（如 PUBG）但**不污染原版业务代码**
// 插件以"事件钩子"形式介入特定请求 监听到符合条件的事件再做扩展逻辑
//
// 核心能力：
//   1. 事件监听（ListenEvent）：插件订阅 9 种 PluginEventId（标点/死亡/物件交互/进场景/技能/受击/创建物件/子弹命中等）
//   2. 优先级（PluginEventPriority）：5 档 Lowest/Low/Normal/High/Highest 高优先级先执行
//   3. 事件取消（Cancel）：插件可拦截事件 让原版处理流程跳过该事件
//   4. 全局 tick（AddGlobalTick）：每秒/分钟变更时执行（如毒圈缩小判定）
//   5. 用户 timer（CreateUserTimer）：单个玩家的延迟任务（如吃鸡淘汰倒计时）
//   6. 命令注册（RegCommandController）：插件注册自己的 GM 命令（如 /pubg start）
//
// 当前注册的插件：仅 PluginPubg（PUBG 玩法）
// 扩展自创玩法的标准做法：实现 IPlugin 接口 → 在 InitPlugin 注册 → 监听需要的事件

// InitPlugin 初始化插件（在 GameCore 创建时调用 注册所有内置插件）
func (p *PluginManager) InitPlugin() {
	iPluginList := []IPlugin{
		NewPluginPubg(),
	}
	p.RegAllPlugin(iPluginList...)
}

// 事件定义
// 添加事件编号 事件结构
// 即可触发以及监听事件

// PluginEventId 事件编号
type PluginEventId uint16

const (
	PluginEventIdNone = PluginEventId(iota)
	PluginEventIdMarkMap
	PluginEventIdAvatarDieAnimationEnd
	PluginEventIdGadgetInteract
	PluginEventIdPostEnterScene
	PluginEventIdEvtDoSkillSucc
	PluginEventIdEvtBeingHit
	PluginEventIdEvtCreateGadget
	PluginEventIdEvtBulletHit
)

// PluginEventMarkMap 地图标点
type PluginEventMarkMap struct {
	*PluginEvent
	Player *model.Player     // 玩家
	Req    *proto.MarkMapReq // 请求
}

// PluginEventAvatarDieAnimationEnd 角色死亡动画结束
type PluginEventAvatarDieAnimationEnd struct {
	*PluginEvent
	Player *model.Player                   // 玩家
	Req    *proto.AvatarDieAnimationEndReq // 请求
}

// PluginEventGadgetInteract gadget交互
type PluginEventGadgetInteract struct {
	*PluginEvent
	Player *model.Player            // 玩家
	Req    *proto.GadgetInteractReq // 请求
}

// PluginEventPostEnterScene 进入场景后
type PluginEventPostEnterScene struct {
	*PluginEvent
	Player *model.Player            // 玩家
	Req    *proto.PostEnterSceneReq // 请求
}

// PluginEventEvtDoSkillSucc 使用技能
type PluginEventEvtDoSkillSucc struct {
	*PluginEvent
	Player *model.Player               // 玩家
	Ntf    *proto.EvtDoSkillSuccNotify // 请求
}

// PluginEventEvtBeingHit 实体受击
type PluginEventEvtBeingHit struct {
	*PluginEvent
	Player  *model.Player          // 玩家
	HitInfo *proto.EvtBeingHitInfo // 请求
}

// PluginEventEvtCreateGadget 创建物件实体
type PluginEventEvtCreateGadget struct {
	*PluginEvent
	Player *model.Player                // 玩家
	Ntf    *proto.EvtCreateGadgetNotify // 请求
}

// PluginEventEvtBulletHit 子弹命中
type PluginEventEvtBulletHit struct {
	*PluginEvent
	Player *model.Player             // 玩家
	Ntf    *proto.EvtBulletHitNotify // 请求
}

type PluginEventFunc func(event IPluginEvent)

// IPluginEvent 插件事件接口
type IPluginEvent interface {
	Cancel()
	IsCancel() bool
}

// PluginEvent 插件事件
type PluginEvent struct {
	isCancel bool // 事件是否已取消
}

func NewPluginEvent() *PluginEvent {
	return &PluginEvent{}
}

// Cancel 取消事件
// 仍会继续传递事件 但不会在触发事件的地方继续执行
func (p *PluginEvent) Cancel() {
	p.isCancel = true
}

// IsCancel 事件是否已取消
func (p *PluginEvent) IsCancel() bool {
	return p.isCancel
}

// IPlugin 插件接口
type IPlugin interface {
	GetPlugin() *Plugin
	OnEnable()
	OnDisable()
}

// PluginEventPriority 插件事件优先级
type PluginEventPriority uint8

const (
	PluginEventPriorityLowest = PluginEventPriority(iota)
	PluginEventPriorityLow
	PluginEventPriorityNormal
	PluginEventPriorityHigh
	PluginEventPriorityHighest
)

// PluginEventInfo 插件事件信息
type PluginEventInfo struct {
	EventId   PluginEventId       // 事件id
	Priority  PluginEventPriority // 优先级
	EventFunc PluginEventFunc     // 事件执行函数
}

// PluginGlobalTick 全局tick
type PluginGlobalTick uint8

const (
	PluginGlobalTickSecond = PluginGlobalTick(iota)
	PluginGlobalTickMinuteChange
)

// PluginUserTimerFunc 用户timer处理函数
type PluginUserTimerFunc func(player *model.Player, data []any)

// Plugin 插件结构
type Plugin struct {
	isEnable              bool                                 // 是否启用
	eventMap              map[PluginEventId][]*PluginEventInfo // 事件集合
	globalTickMap         map[PluginGlobalTick][]func()        // 全局tick集合
	userTimerMap          map[uint64]PluginUserTimerFunc       // 用户timer集合
	commandControllerList []*CommandController                 // 命令控制器列表
}

func NewPlugin() *Plugin {
	return &Plugin{
		isEnable:              true,
		eventMap:              make(map[PluginEventId][]*PluginEventInfo),
		globalTickMap:         make(map[PluginGlobalTick][]func()),
		userTimerMap:          make(map[uint64]PluginUserTimerFunc),
		commandControllerList: make([]*CommandController, 0),
	}
}

// GetPlugin 获取插件
func (p *Plugin) GetPlugin() *Plugin {
	return p
}

// OnEnable 插件启用时的生命周期
func (p *Plugin) OnEnable() {
	// 具体逻辑由插件来重写
}

// OnDisable 插件禁用时的生命周期
func (p *Plugin) OnDisable() {
	// 具体逻辑由插件来重写
}

// ListenEvent 监听事件
func (p *Plugin) ListenEvent(eventId PluginEventId, priority PluginEventPriority, eventFuncList ...PluginEventFunc) {
	for _, eventFunc := range eventFuncList {
		_, exist := p.eventMap[eventId]
		if !exist {
			p.eventMap[eventId] = make([]*PluginEventInfo, 0)
		}
		pluginEventInfo := &PluginEventInfo{
			EventId:   eventId,
			Priority:  priority,
			EventFunc: eventFunc,
		}
		p.eventMap[eventId] = append(p.eventMap[eventId], pluginEventInfo)
	}
}

// AddGlobalTick 添加全局tick
func (p *Plugin) AddGlobalTick(tick PluginGlobalTick, tickFuncList ...func()) {
	for _, tickFunc := range tickFuncList {
		_, exist := p.globalTickMap[tick]
		if !exist {
			p.globalTickMap[tick] = make([]func(), 0)
		}
		p.globalTickMap[tick] = append(p.globalTickMap[tick], tickFunc)
	}
}

// CreateUserTimer 创建插件级用户 timer（玩家专用延迟任务）
//
// 与 TICK_MANAGER.CreateUserTimer 不同：插件 timer 通过 userTimerCounter 自增编号
// 转发到 TICK_MANAGER 时把编号塞进 data 第一项 timer 触发时通过编号反查处理函数
// 这样多个插件可独立管理自己的 timer 不会互相干扰
//
// 用例：PUBG 插件 30 秒后判定淘汰、5 分钟后毒圈缩小等
func (p *Plugin) CreateUserTimer(userId uint32, delay uint32, timerFunc PluginUserTimerFunc, data ...any) {
	PLUGIN_MANAGER.userTimerCounter++
	userTimerId := PLUGIN_MANAGER.userTimerCounter
	p.userTimerMap[userTimerId] = timerFunc
	// 用户timer编号插入到数据前
	data = append([]any{userTimerId}, data...)
	TICK_MANAGER.CreateUserTimer(userId, UserTimerActionPlugin, delay, data...)
}

// RegCommandController 注册命令控制器
func (p *Plugin) RegCommandController(controller *CommandController) {
	COMMAND_MANAGER.RegController(controller)
}

type PluginManager struct {
	pluginMap        map[reflect.Type]IPlugin // 插件集合
	userTimerCounter uint64                   // 用户timer计数器
}

func NewPluginManager() *PluginManager {
	r := new(PluginManager)
	r.pluginMap = make(map[reflect.Type]IPlugin)
	return r
}

// RegAllPlugin 注册全部插件
func (p *PluginManager) RegAllPlugin(iPluginList ...IPlugin) {
	for _, plugin := range iPluginList {
		p.RegPlugin(plugin)
	}
}

// RegPlugin 注册插件
func (p *PluginManager) RegPlugin(iPlugin IPlugin) {
	// 反射类型
	refType := reflect.TypeOf(iPlugin)
	// 校验插件名是否已被注册
	_, exist := p.pluginMap[refType]
	if exist {
		logger.Error("plugin has been register,refType: %v", refType)
		return
	}
	logger.Info("plugin enable, refType: %v", refType)
	// 调用插件启用的生命周期
	iPlugin.OnEnable()
	p.pluginMap[refType] = iPlugin
}

// DelAllPlugin 卸载全部插件
func (p *PluginManager) DelAllPlugin() {
	for _, plugin := range p.pluginMap {
		p.DelPlugin(plugin)
	}
}

// DelPlugin 卸载插件
func (p *PluginManager) DelPlugin(iPlugin IPlugin) {
	// 反射类型
	refType := reflect.TypeOf(iPlugin)
	// 校验插件是否注册
	_, exist := p.pluginMap[refType]
	if !exist {
		logger.Error("plugin not exist, refType: %v", refType)
		return
	}
	logger.Info("plugin disable, refType: %v", refType)
	// 调用插件禁用的生命周期
	iPlugin.OnDisable()
	// 卸载插件注册的命令
	plugin := iPlugin.GetPlugin()
	for _, controller := range plugin.commandControllerList {
		COMMAND_MANAGER.DelAllController(controller)
	}
	delete(p.pluginMap, refType)
}

// GetPlugin 获取插件实例
func (p *PluginManager) GetPlugin(value IPlugin) (plugin IPlugin, err error) {
	refType := reflect.TypeOf(value)
	// 校验插件是否注册
	plugin, exist := p.pluginMap[refType]
	if !exist {
		err = errors.New(fmt.Sprintf("plugin not exist, refType: %v", refType))
		return nil, err
	}
	return plugin, nil
}

// TriggerEvent 触发事件（核心入口 业务代码通过此函数把事件发给插件）
//
// 处理：
//  1. 收集所有插件中监听该 eventId 的处理函数
//  2. 按优先级排序（高优先级先执行 同优先级按注册顺序）
//  3. 依次调用所有处理函数（即使被 Cancel 仍会传递给所有插件）
//  4. 返回事件是否被取消（业务方根据返回值决定是否跳过原版处理）
//
// 调用示例（业务代码 → 插件 → 决定是否拦截）：
//
//	if PLUGIN_MANAGER.TriggerEvent(PluginEventIdMarkMap, &PluginEventMarkMap{...}) {
//	    return // 插件拦截了 跳过原版处理
//	}
//	// 原版处理逻辑
func (p *PluginManager) TriggerEvent(eventId PluginEventId, event IPluginEvent) bool {
	// 获取每个插件监听的事件并根据优先级排序
	eventInfoList := make([]*PluginEventInfo, 0)
	for _, iPlugin := range p.pluginMap {
		plugin := iPlugin.GetPlugin()
		// 插件未启用则跳过
		if !plugin.isEnable {
			continue
		}
		// 获取插件事件列表
		infoList, exist := plugin.eventMap[eventId]
		if !exist {
			continue
		}
		for _, info := range infoList {
			eventInfoList = append(eventInfoList, info)
		}
	}
	// 根据优先级排序
	sort.Slice(eventInfoList, func(i, j int) bool {
		return eventInfoList[i].Priority > eventInfoList[j].Priority
	})
	// 执行每个处理函数
	for _, info := range eventInfoList {
		info.EventFunc(event)
	}
	// 判断事件是否被取消
	return event.IsCancel()
}

// HandleGlobalTick 处理全局tick
func (p *PluginManager) HandleGlobalTick(tick PluginGlobalTick) {
	for _, iPlugin := range p.pluginMap {
		plugin := iPlugin.GetPlugin()
		// 插件未启用则跳过
		if !plugin.isEnable {
			continue
		}
		// 获取插件tick处理函数列表
		tickFuncList, exist := plugin.globalTickMap[tick]
		if !exist {
			continue
		}
		for _, tickFunc := range tickFuncList {
			tickFunc()
		}
	}
}

// HandleUserTimer 处理用户timer
func (p *PluginManager) HandleUserTimer(player *model.Player, data []any) {
	// 如果创建的用户timer没有id数据则报错
	if len(data) < 1 {
		logger.Error("data len less 1, len: %v", len(data))
		return
	}
	userTimerId := data[0].(uint64)
	data = data[1:]
	// 通知插件
	for _, iPlugin := range p.pluginMap {
		plugin := iPlugin.GetPlugin()
		// 插件未启用则跳过
		if !plugin.isEnable {
			continue
		}
		// 获取插件用户timer处理函数列表
		timerFunc, exist := plugin.userTimerMap[userTimerId]
		if !exist {
			logger.Error("plugin timer not exist, id: %v", userTimerId)
			continue
		}
		timerFunc(player, data)
		delete(plugin.userTimerMap, userTimerId)
		// 只需要执行一次
		break
	}
}
