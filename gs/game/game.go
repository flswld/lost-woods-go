package game

import (
	"os"
	"runtime"
	"strconv"
	"time"

	"hk4e/common/constant"
	"hk4e/common/mq"
	"hk4e/common/rpc"
	"hk4e/gs/dao"
	"hk4e/gs/model"
	"hk4e/pkg/alg"
	"hk4e/pkg/reflection"
	"hk4e/protocol/cmd"
	"hk4e/protocol/proto"

	"github.com/flswld/halo/logger"
	"github.com/flswld/halo/protocol/kcp"
	pb "google.golang.org/protobuf/proto"
)

// 游戏服务器主结构与单线程主循环
//
// 设计核心约束：
//   1. 整个GS只有一个主循环goroutine（gameMainLoop），所有玩家逻辑串行执行
//   2. 玩家数据访问无需加锁（直接字段读写），但任何阻塞操作都会卡死整个GS
//   3. DB IO 必须放到后台 goroutine（asyncWriteDbChan/saveUserChan），完成后通过
//      LOCAL_EVENT_MANAGER 回调主循环
//   4. 主循环 panic 会被 defer recover 捕获 踢掉SELF玩家后重启循环
//      （10秒内连续panic>10次才会退出进程）

const (
	PlayerBaseUid    = 100000000 // 玩家uid下限 < 此值视为非玩家（AI/系统）
	MaxPlayerBaseUid = 200000000 // 玩家uid上限
	AiBaseUid        = 10000     // AI玩家uid基准 实际AI uid = AiBaseUid + gsId（每GS一个）
	AiName           = "小可爱"     // AI玩家昵称（GM机器人好友）
	AiSign           = "快捷指令"    // AI玩家签名前缀
)

// 全局单例管理器 在 NewGameCore 中按顺序初始化
// 顺序敏感：LOCAL_EVENT → ROUTE → USER → WORLD → TICK → COMMAND → GCG → PLUGIN
// 因为后初始化的模块可能依赖前面的模块（PLUGIN最后才InitPlugin）
var GAME *Game = nil
var LOCAL_EVENT_MANAGER *LocalEventManager = nil
var ROUTE_MANAGER *RouteManager = nil
var USER_MANAGER *UserManager = nil
var WORLD_MANAGER *WorldManager = nil
var TICK_MANAGER *TickManager = nil
var COMMAND_MANAGER *CommandManager = nil
var GCG_MANAGER *GCGManager = nil
var PLUGIN_MANAGER *PluginManager = nil

var ONLINE_PLAYER_NUM int32 = 0 // 当前在线玩家数

// SELF 当前主循环正在处理的玩家 用于handler内部传递上下文避免每个调用层层传player参数
// 由 ROUTE_MANAGER.doRoute 在调用handler前后置换 OnLogin 内部也临时设置
// 警告：单线程模式下才安全 绝不能持有到异步goroutine中（会被下一个请求覆盖）
var SELF *model.Player

type Game struct {
	discoveryClient    *rpc.DiscoveryClient // node节点服务器的natsrpc客户端
	db                 *dao.Dao             // 数据访问对象
	messageQueue       *mq.MessageQueue     // 消息队列（NATS+TCP双通道）
	gsId               uint32               // 游戏服务器编号 由Node注册时分配
	gsAppid            string               // 游戏服务器appid
	gsAppVersion       string               // 游戏服务器版本
	snowflake          *alg.SnowflakeWorker // 雪花唯一id生成器（worldId/weaponId/reliquaryId 等用）
	isStop             bool                 // 停服标志 Close后置位
	dispatchCancel     bool                 // 取消调度标志 同版本服务器更新切换时置位
	endlessLoopCounter map[int]uint64       // 死循环保护计数器（按checkType计数 阈值见EndlessLoopCheck）
	transactionSeq     uint32               // 事务序列号 用于NewTransaction生成唯一事务ID
	ai                 *model.Player        // 本服的AI玩家对象（"小可爱" GM机器人好友 + AI世界owner）
}

// NewGameCore 创建Game主对象并启动主循环
// 调用顺序：app.go 完成基础设施（DAO/MQ/RPC）后调用此函数 之后阻塞直到收到信号
// 内部按依赖顺序初始化全局单例 然后创建AI玩家+AI世界 最后初始化插件并启动主循环goroutine
func NewGameCore(discoveryClient *rpc.DiscoveryClient, db *dao.Dao, messageQueue *mq.MessageQueue, gsId uint32, gsAppid string, gsAppVersion string) (r *Game) {
	r = new(Game)
	r.discoveryClient = discoveryClient
	r.db = db
	r.messageQueue = messageQueue
	r.gsId = gsId
	r.gsAppid = gsAppid
	r.gsAppVersion = gsAppVersion
	r.snowflake = alg.NewSnowflakeWorker(int64(gsId))
	r.isStop = false
	r.dispatchCancel = false
	r.endlessLoopCounter = make(map[int]uint64)
	r.transactionSeq = 0
	GAME = r
	LOCAL_EVENT_MANAGER = NewLocalEventManager()
	ROUTE_MANAGER = NewRouteManager()
	USER_MANAGER = NewUserManager(db)
	WORLD_MANAGER = NewWorldManager(r.snowflake)
	TICK_MANAGER = NewTickManager()
	COMMAND_MANAGER = NewCommandManager()
	GCG_MANAGER = NewGCGManager()
	PLUGIN_MANAGER = NewPluginManager()
	RegLuaScriptLibFunc()
	// 创建本服的Ai世界
	uid := AiBaseUid + gsId
	name := AiName
	sign := AiSign + " GS:" + strconv.Itoa(int(gsId))
	r.ai = r.CreateRobot(uid, name, sign)
	WORLD_MANAGER.InitAiWorld(r.ai)
	COMMAND_MANAGER.SetSystem(r.ai)
	// 初始化插件 最后再调用以免插件需要访问其他模块导致出错
	PLUGIN_MANAGER.InitPlugin()
	go r.gameMainLoopD()
	return r
}

// gameMainLoopD 主循环监控守护
// 包裹 gameMainLoop 在panic后自动重启 但10秒内连续panic>10次会判定为不可恢复 进程退出
func (g *Game) gameMainLoopD() {
	times := 1
	panicCounter := 0
	lastPanicTime := time.Now().UnixNano()
	for {
		logger.Warn("game main loop start, times: %v", times)
		g.gameMainLoop()
		logger.Warn("game main loop stop, times: %v", times)
		times++
		panicCounter++
		if panicCounter > 10 {
			now := time.Now().UnixNano()
			if now-lastPanicTime > int64(time.Second) {
				panicCounter = 0
				lastPanicTime = now
			} else {
				logger.Error("!!! GAME MAIN LOOP STOP !!!")
				time.Sleep(time.Second * 10)
				os.Exit(-1)
			}
		}
	}
}

// gameMainLoop 单线程主循环 整个GS的命脉
// select多路复用4个通道（按优先级近似公平 Go runtime 随机选择）：
//  1. netMsg     —— 客户端消息（路由分发到handler）
//  2. globalTick —— 50ms定时帧（驱动玩家移动同步、tick回调、PUBG物理引擎等）
//  3. localEvent —— 异步IO完成回调（DB加载/保存完成、热更配置完成 等）
//  4. command    —— GM命令（聊天命令/RPC调用）
//
// 每分钟打印一次CPU时间分布日志 用于排查热点
// runtime.LockOSThread 把goroutine绑定到OS线程 减少调度抖动
func (g *Game) gameMainLoop() {
	// panic捕获
	defer func() {
		if err := recover(); err != nil {
			logger.Error("!!! GAME MAIN LOOP PANIC !!!")
			logger.Error("error: %v", err)
			logger.Error("stack: %v", logger.Stack())
			if SELF != nil {
				logger.Error("the motherfucker player uid: %v", SELF.PlayerId)
				g.KickPlayer(SELF.PlayerId, kcp.EnetServerKick)
				SELF = nil
			}
		}
	}()
	intervalTime := time.Second.Nanoseconds() * 60
	lastTime := time.Now().UnixNano()
	routeCost := int64(0)
	tickCost := int64(0)
	localEventCost := int64(0)
	commandCost := int64(0)
	routeCount := int64(0)
	maxRouteCost := int64(0)
	maxRouteCmdId := uint16(0)
	runtime.LockOSThread()
	for {
		// 消耗CPU时间性能统计
		now := time.Now().UnixNano()
		if now-lastTime > intervalTime {
			routeCost /= 1e6
			tickCost /= 1e6
			localEventCost /= 1e6
			commandCost /= 1e6
			maxRouteCost /= 1e6
			logger.Info("[GAME MAIN LOOP] cpu time cost detail, routeCost: %v ms, tickCost: %v ms, localEventCost: %v ms, commandCost: %v ms",
				routeCost, tickCost, localEventCost, commandCost)
			totalCost := routeCost + tickCost + localEventCost + commandCost
			logger.Info("[GAME MAIN LOOP] cpu time cost percent, routeCost: %v%%, tickCost: %v%%, localEventCost: %v%%, commandCost: %v%%",
				float32(routeCost)/float32(totalCost)*100.0,
				float32(tickCost)/float32(totalCost)*100.0,
				float32(localEventCost)/float32(totalCost)*100.0,
				float32(commandCost)/float32(totalCost)*100.0)
			logger.Info("[GAME MAIN LOOP] total cpu time cost detail, totalCost: %v ms",
				totalCost)
			logger.Info("[GAME MAIN LOOP] total cpu time cost percent, totalCost: %v%%",
				float32(totalCost)/float32(intervalTime/1e6)*100.0)
			avgRouteCost := float32(0)
			if routeCount != 0 {
				avgRouteCost = float32(routeCost) / float32(routeCount)
			}
			logger.Info("[GAME MAIN LOOP] avg route cost: %v ms", avgRouteCost)
			logger.Info("[GAME MAIN LOOP] max route cost: %v ms, cmdId: %v", maxRouteCost, maxRouteCmdId)
			lastTime = now
			routeCost = 0
			tickCost = 0
			localEventCost = 0
			commandCost = 0
			routeCount = 0
			maxRouteCost = 0
			maxRouteCmdId = 0
		}
		g.endlessLoopCounter = make(map[int]uint64)
		select {
		case netMsg := <-g.messageQueue.GetNetMsg():
			// 接收客户端消息
			start := time.Now().UnixNano()
			ROUTE_MANAGER.RouteHandle(netMsg)
			end := time.Now().UnixNano()
			if netMsg.MsgType == mq.MsgTypeGame && (end-start) > maxRouteCost {
				maxRouteCost = end - start
				maxRouteCmdId = netMsg.GameMsg.CmdId
			}
			routeCost += end - start
			routeCount++
		case <-TICK_MANAGER.GetGlobalTick().C:
			// 游戏服务器定时帧
			start := time.Now().UnixNano()
			TICK_MANAGER.OnGameServerTick()
			end := time.Now().UnixNano()
			tickCost += end - start
		case localEvent := <-LOCAL_EVENT_MANAGER.GetLocalEventChan():
			// 处理本地事件
			start := time.Now().UnixNano()
			LOCAL_EVENT_MANAGER.LocalEventHandle(localEvent)
			end := time.Now().UnixNano()
			localEventCost += end - start
		case command := <-COMMAND_MANAGER.GetCommandMessageInput():
			// 处理GM命令
			start := time.Now().UnixNano()
			COMMAND_MANAGER.HandleCommand(command)
			end := time.Now().UnixNano()
			commandCost += end - start
			logger.Info("run gm cmd cost: %v ns", end-start)
		}
	}
}

func (g *Game) GetGsId() uint32 {
	return g.gsId
}

func (g *Game) GetGsAppid() string {
	return g.gsAppid
}

// GetAi 获取本服的Ai玩家对象
func (g *Game) GetAi() *model.Player {
	return g.ai
}

// CreateRobot 创建机器人玩家（uid < PlayerBaseUid）
// 用于：
//   - GS启动时创建本服AI玩家"小可爱"（GM机器人好友 + AI世界owner）
//   - GM命令 CreateRobotInAiWorld 在AI世界中创建陪玩机器人（当前无AI行为 仅占位）
//
// 模拟完整的登录-出生-进场景流程 末尾设置WuDi避免被误杀
func (g *Game) CreateRobot(uid uint32, name string, sign string) *model.Player {
	g.OnLogin(uid, 0, "", nil, new(proto.PlayerLoginReq), true)
	robot := USER_MANAGER.GetOnlineUser(uid)
	robot.DbState = model.DbNormal // 机器人不写库 直接置为Normal跳过Insert
	g.SetPlayerBornDataReq(robot, &proto.SetPlayerBornDataReq{AvatarId: 10000007, NickName: name})
	robot.Signature = sign
	world := WORLD_MANAGER.GetWorldById(robot.WorldId)
	g.HostEnterMpWorld(robot) // 转为多人世界（让其他玩家可以加入）
	// 直接调用四步状态机的handler 模拟客户端依次发送
	g.EnterSceneReadyReq(robot, &proto.EnterSceneReadyReq{
		EnterSceneToken: world.GetEnterSceneToken(),
	})
	g.SceneInitFinishReq(robot, &proto.SceneInitFinishReq{
		EnterSceneToken: world.GetEnterSceneToken(),
	})
	g.EnterSceneDoneReq(robot, &proto.EnterSceneDoneReq{
		EnterSceneToken: world.GetEnterSceneToken(),
	})
	g.PostEnterSceneReq(robot, &proto.PostEnterSceneReq{
		EnterSceneToken: world.GetEnterSceneToken(),
	})
	robot.WuDi = true
	return robot
}

// EndlessLoopCheck 死循环保护 主循环单线程模式下卡死会影响所有玩家所以必须熔断
// 各类型阈值不同：TriggerQuest最大10000（任务触发递归较深） 其他默认1000
// 触发后踢掉SELF玩家并 panic 由 gameMainLoopD 重启循环
const (
	EndlessLoopCheckTypeAcceptQuest       = iota // 接受任务
	EndlessLoopCheckTypeStartQuest               // 开始任务
	EndlessLoopCheckTypeExecQuest                // 执行任务动作
	EndlessLoopCheckTypeTriggerQuest             // 推进任务进度（递归层数最深 阈值最大）
	EndlessLoopCheckTypeUseItem                  // 使用道具
	EndlessLoopCheckTypeCallLuaFunc              // 调用Lua函数
	EndlessLoopCheckTypeCheckFinishedCond        // 检查任务完成条件
)

// EndlessLoopCheck 在容易递归的入口处调用 累计计数 超过阈值则panic
// 计数器在主循环每次select都会清零（见gameMainLoop开头）所以仅检测单次主循环步内的递归深度
func (g *Game) EndlessLoopCheck(checkType int) {
	g.endlessLoopCounter[checkType]++
	checkCount := g.endlessLoopCounter[checkType]
	EndlessLoopHandleFunc := func() {
		logger.Error("!!! GAME MAIN LOOP ENDLESS LOOP !!!")
		logger.Error("checkType: %v, checkCount: %v", checkType, checkCount)
		logger.Error("stack: %v", logger.Stack())
		if SELF != nil {
			logger.Error("the motherfucker player uid: %v", SELF.PlayerId)
			g.KickPlayer(SELF.PlayerId, kcp.EnetServerKick)
			SELF = nil
		}
		panic("EndlessLoopCheck")
	}
	switch checkType {
	case EndlessLoopCheckTypeAcceptQuest:
		if checkCount > 1000 {
			EndlessLoopHandleFunc()
		}
	case EndlessLoopCheckTypeStartQuest:
		if checkCount > 1000 {
			EndlessLoopHandleFunc()
		}
	case EndlessLoopCheckTypeExecQuest:
		if checkCount > 1000 {
			EndlessLoopHandleFunc()
		}
	case EndlessLoopCheckTypeTriggerQuest:
		if checkCount > 10000 {
			EndlessLoopHandleFunc()
		}
	case EndlessLoopCheckTypeUseItem:
		if checkCount > 1000 {
			EndlessLoopHandleFunc()
		}
	case EndlessLoopCheckTypeCallLuaFunc:
		if checkCount > 1000 {
			EndlessLoopHandleFunc()
		}
	case EndlessLoopCheckTypeCheckFinishedCond:
		if checkCount > 1000 {
			EndlessLoopHandleFunc()
		}
	default:
	}
}

// NewTransaction 生成事务唯一ID 格式 "uid-时间戳-序列号"
// 用于客户端响应中的Transaction字段（GetAllMailResultNotify等）
func (g *Game) NewTransaction(uid uint32) string {
	g.transactionSeq++
	return strconv.Itoa(int(uid)) + "-" + strconv.Itoa(int(time.Now().Unix())) + "-" + strconv.Itoa(int(g.transactionSeq))
}

// EXIT_SAVE_FIN_CHAN 停服时主循环阻塞等待此channel 用于等异步保存全部完成
var EXIT_SAVE_FIN_CHAN chan bool

// ServerStopNotify 停服流程入口（GM命令ServerStop或node通知）
// 异步执行：先全服公告"停服维护" → 等待1分钟让玩家退出 → 按 GsId 错峰再等几秒 → Close
// 错峰是为了多GS同时停服时不会同时压垮DB
func (g *Game) ServerStopNotify() {
	go func() {
		info := "停服维护"
		GAME.ServerAnnounceNotify(1, info)
		logger.Warn("stop game server last 1 minute")
		time.Sleep(time.Minute)
		delay := GAME.GetGsId()
		logger.Warn("stop game server last %v second", delay)
		time.Sleep(time.Second * time.Duration(delay))
		GAME.Close()
	}()
}

// Close 实际停服流程 同步阻塞直到所有玩家档保存完成
// 步骤：1. 投递ExitRunUserCopyAndSave事件并等保存完成 2. 踢掉所有玩家+广播下线 3. 卸载插件
// 主循环会在 ExitRunUserCopyAndSave 处理完后永久阻塞（select{}）等进程退出
func (g *Game) Close() {
	if g.isStop {
		return
	}
	g.isStop = true
	logger.Warn("stop game server begin")
	// 保存玩家数据
	EXIT_SAVE_FIN_CHAN = make(chan bool)
	LOCAL_EVENT_MANAGER.GetLocalEventChan() <- &LocalEvent{
		EventId: ExitRunUserCopyAndSave,
	}
	<-EXIT_SAVE_FIN_CHAN
	logger.Warn("stop game server save player finish")
	// 告诉网关下线玩家并全服广播玩家离线
	userList := USER_MANAGER.GetAllOnlineUserList()
	for _, player := range userList {
		g.KickPlayer(player.PlayerId, kcp.EnetServerShutdown)
		g.messageQueue.SendToAll(&mq.NetMsg{
			MsgType: mq.MsgTypeServer,
			EventId: mq.ServerUserOnlineStateChangeNotify,
			ServerMsg: &mq.ServerMsg{
				UserId:   player.PlayerId,
				IsOnline: false,
			},
		})
		time.Sleep(time.Millisecond * 100)
	}
	// 卸载插件
	PLUGIN_MANAGER.DelAllPlugin()
	logger.Warn("stop game server finish")
}

// ServerDispatchCancelNotify 收到调度取消通知（同版本服务器更新前用 让Dispatch不再分配新玩家到本服）
// 仅当通知中的版本号匹配本服才生效（避免误伤）
func (g *Game) ServerDispatchCancelNotify(appVersion string) {
	if appVersion != g.gsAppVersion {
		return
	}
	logger.Warn("game server dispatch cancel")
	g.dispatchCancel = true
}

// SendMsgToGate 发送消息给客户端 指定网关
// 用于玩家不在本服内存（OnLogin失败时只能用userId+gateAppId定位）的场景
func (g *Game) SendMsgToGate(cmdId uint16, userId uint32, clientSeq uint32, gateAppId string, payloadMsg pb.Message) {
	if userId < PlayerBaseUid {
		return
	}
	if payloadMsg == nil {
		logger.Error("payload msg is nil, stack: %v", logger.Stack())
		return
	}
	// 在这里直接序列化成二进制数据 防止发送的消息内包含各种游戏数据指针 而造成并发读写的问题
	payloadMessageData, err := pb.Marshal(payloadMsg)
	if err != nil {
		logger.Error("parse payload msg to bin error: %v, stack: %v", err, logger.Stack())
		return
	}
	gameMsg := &mq.GameMsg{
		UserId:             userId,
		CmdId:              cmdId,
		ClientSeq:          clientSeq,
		PayloadMessageData: payloadMessageData,
	}
	g.messageQueue.SendToGate(gateAppId, &mq.NetMsg{
		MsgType: mq.MsgTypeGame,
		EventId: mq.NormalMsg,
		GameMsg: gameMsg,
	})
}

// SendMsg 发送消息给指定玩家 是handler最常用的下行接口
// 自动从在线表查到玩家所在Gate appid 序列化proto后通过mq转发
func (g *Game) SendMsg(cmdId uint16, userId uint32, clientSeq uint32, payloadMsg pb.Message) {
	if userId < PlayerBaseUid {
		return
	}
	if payloadMsg == nil {
		logger.Error("payload msg is nil, stack: %v", logger.Stack())
		return
	}
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player not exist, uid: %v, stack: %v", userId, logger.Stack())
		return
	}
	if !player.Online {
		return
	}
	if player.NetFreeze {
		return
	}
	gameMsg := new(mq.GameMsg)
	gameMsg.UserId = userId
	gameMsg.CmdId = cmdId
	gameMsg.ClientSeq = clientSeq
	// 在这里直接序列化成二进制数据 防止发送的消息内包含各种游戏数据指针 而造成并发读写的问题
	payloadMessageData, err := pb.Marshal(payloadMsg)
	if err != nil {
		logger.Error("parse payload msg to bin error: %v, stack: %v", err, logger.Stack())
		return
	}
	gameMsg.PayloadMessageData = payloadMessageData
	g.messageQueue.SendToGate(player.GateAppId, &mq.NetMsg{
		MsgType: mq.MsgTypeGame,
		EventId: mq.NormalMsg,
		GameMsg: gameMsg,
	})
}

// SendError 通用错误响应 通过反射设置 rsp.Retcode 字段后发送
// 不传retCode默认 RET_SVR_ERROR 业务handler通常显式传具体错误码
func (g *Game) SendError(cmdId uint16, player *model.Player, rsp pb.Message, retCode ...proto.Retcode) {
	if rsp == nil {
		return
	}
	if len(retCode) == 0 {
		retCode = []proto.Retcode{proto.Retcode_RET_SVR_ERROR}
	}
	ok := reflection.SetStructFieldValue(rsp, "Retcode", int32(retCode[0]))
	if !ok {
		return
	}
	logger.Error("send common error, rsp: %v, err: %v, uid: %v", rsp.ProtoReflect().Descriptor().FullName(), retCode[0].String(), player.PlayerId)
	g.SendMsg(cmdId, player.PlayerId, player.ClientSeq, rsp)
}

// SendSucc 通用成功响应 通过反射设置 rsp.Retcode = RET_SUCC 后发送
func (g *Game) SendSucc(cmdId uint16, player *model.Player, rsp pb.Message) {
	if rsp == nil {
		return
	}
	ok := reflection.SetStructFieldValue(rsp, "Retcode", int32(proto.Retcode_RET_SUCC))
	if !ok {
		return
	}
	g.SendMsg(cmdId, player.PlayerId, player.ClientSeq, rsp)
}

// SendToWorldA 广播给世界内所有玩家（aecUid参数指定要排除的uid 0表示不排除）
// "A"表示All "aec"表示AllExceptCur 命名沿用 ForwardType 的语义
func (g *Game) SendToWorldA(world *World, cmdId uint16, seq uint32, msg pb.Message, aecUid uint32) {
	for _, v := range world.GetAllPlayer() {
		if aecUid == v.PlayerId {
			continue
		}
		g.SendMsg(cmdId, v.PlayerId, seq, msg)
	}
}

// SendToWorldH 仅发送给世界房主 "H"表示Host
func (g *Game) SendToWorldH(world *World, cmdId uint16, seq uint32, msg pb.Message) {
	g.SendMsg(cmdId, world.GetOwner().PlayerId, seq, msg)
}

// SendToSceneA 给场景内所有玩家广播 带视野/AOI过滤
// 普通世界：按视野距离判定（IsInVision）超出视野不发
// AI世界：用 aiWorldAoi 网格过滤 仅发给AOI范围内的玩家（PUBG等大规模玩法的优化）
// 注意：依赖SELF作为"参考点"判断视野 SELF为nil时不做过滤直接全发
func (g *Game) SendToSceneA(scene *Scene, cmdId uint16, seq uint32, msg pb.Message, aecUid uint32) {
	world := scene.GetWorld()
	if WORLD_MANAGER.IsAiWorld(world) && SELF != nil {
		aiWorldAoi := world.GetAiWorldAoi()
		pos := g.GetPlayerPos(SELF)
		otherWorldAvatarMap := aiWorldAoi.GetObjectListByPos(float32(pos.X), float32(pos.Y), float32(pos.Z), 1)
		for uid := range otherWorldAvatarMap {
			if aecUid == uint32(uid) {
				continue
			}
			g.SendMsg(cmdId, uint32(uid), seq, msg)
		}
	} else {
		for _, v := range scene.GetAllPlayer() {
			if aecUid == v.PlayerId {
				continue
			}
			if SELF != nil {
				p1 := g.GetPlayerPos(SELF)
				p2 := g.GetPlayerPos(v)
				if !g.IsInVision(p1, p2, constant.VISION_LEVEL_NORMAL) {
					continue
				}
			}
			g.SendMsg(cmdId, v.PlayerId, seq, msg)
		}
	}
}

// SendToSceneACV SendToSceneA 的版本过滤变体（"CV"=ClientVersion）
// 仅发送给客户端版本号匹配的玩家 用于多版本同服共存时下发版本专属消息
func (g *Game) SendToSceneACV(scene *Scene, cmdId uint16, seq uint32, msg pb.Message, aecUid uint32, clientVersion int) {
	world := scene.GetWorld()
	if WORLD_MANAGER.IsAiWorld(world) && SELF != nil {
		aiWorldAoi := world.GetAiWorldAoi()
		pos := g.GetPlayerPos(SELF)
		otherWorldAvatarMap := aiWorldAoi.GetObjectListByPos(float32(pos.X), float32(pos.Y), float32(pos.Z), 1)
		for uid := range otherWorldAvatarMap {
			player := USER_MANAGER.GetOnlineUser(uint32(uid))
			if player == nil {
				logger.Error("player not exist, uid: %v, stack: %v", uid, logger.Stack())
				continue
			}
			if aecUid == player.PlayerId {
				continue
			}
			if player.ClientVersion != clientVersion {
				continue
			}
			g.SendMsg(cmdId, uint32(uid), seq, msg)
		}
	} else {
		for _, v := range scene.GetAllPlayer() {
			if aecUid == v.PlayerId {
				continue
			}
			if v.ClientVersion != clientVersion {
				continue
			}
			if SELF != nil {
				p1 := g.GetPlayerPos(SELF)
				p2 := g.GetPlayerPos(v)
				if !g.IsInVision(p1, p2, constant.VISION_LEVEL_NORMAL) {
					continue
				}
			}
			g.SendMsg(cmdId, v.PlayerId, seq, msg)
		}
	}
}

// ReLoginPlayer 触发客户端"类重登"
//
// 发 ClientReconnectNotify 后**客户端会主动断开 KCP 重连**（见 robot/client/client.go:134）
// 不是无感切换 玩家会看到加载界面 然后从 dispatch → gate → gs 完整走一遍登录流程
// 调用场景：
//   - 退出多人世界（BackMyWorld/ChangeWorldToSingleMode/SceneKickPlayer/PlayerLeaveWorld）
//   - PUBG 死亡退出（game_plugin_pubg.go）
//   - AI 世界角色复活（player_team.go WorldPlayerReviveReq）
//
// **注意**：跨服无感迁移走的是 OnOffline(IsChangeGs=true) → ServerUserGsChangeNotify 路径
// 那条路径不发 ClientReconnectNotify 客户端 KCP 也不断（见 game_user_manager.go OfflineUser）
//
// isQuitMp=true 表示从多人世界退出（reason=QUIT_MP）否则 reason=NONE
// 设置 NetFreeze 阻止断连前的后续误下发
func (g *Game) ReLoginPlayer(userId uint32, isQuitMp bool) {
	reason := proto.ClientReconnectReason_CLIENT_RECONNNECT_NONE
	if isQuitMp {
		reason = proto.ClientReconnectReason_CLIENT_RECONNNECT_QUIT_MP
	}
	g.SendMsg(cmd.ClientReconnectNotify, userId, 0, &proto.ClientReconnectNotify{
		Reason: reason,
	})
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		return
	}
	player.NetFreeze = true
}

// LogoutPlayer 通知客户端登出（不踢KCP连接 客户端会自己关闭）
// 用于服务端主动让玩家下线但保持登录态可重连的场景
func (g *Game) LogoutPlayer(userId uint32) {
	g.SendMsg(cmd.PlayerLogoutNotify, userId, 0, &proto.PlayerLogoutNotify{})
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		return
	}
	// 冻结掉服务器对该玩家的下行 避免大量发包对整个系统造成压力
	player.NetFreeze = true
}

// KickPlayer 强制踢人 给Gate发ConnCtrlMsg让Gate关掉对应KCP会话
// reason 见 kcp.Enet*（EnetServerKick/EnetServerShutdown 等）会显示给客户端
func (g *Game) KickPlayer(userId uint32, reason uint32) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		return
	}
	g.messageQueue.SendToGate(player.GateAppId, &mq.NetMsg{
		MsgType: mq.MsgTypeConnCtrl,
		EventId: mq.KickPlayerNotify,
		ConnCtrlMsg: &mq.ConnCtrlMsg{
			KickUserId: userId,
			KickReason: reason,
		},
	})
	player.NetFreeze = true
}
