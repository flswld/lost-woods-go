package game

import (
	"time"

	"hk4e/common/constant"
	"hk4e/gdconf"
	"hk4e/gs/model"
	"hk4e/protocol/cmd"
	"hk4e/protocol/proto"

	"github.com/flswld/halo/logger"
)

// 游戏服务器定时帧管理器
//
// 双层 tick 体系：
//   1. 全局 tick (50ms)         驱动 tick 多级派生（100ms / 200ms / 1s / 5s / 10s / 1min / 1h）
//   2. 玩家 tick (100ms / 玩家)  每个在线玩家独立计时 用于按玩家级别的检查（keepalive 等）
//
// 各级派生tick的业务挂载：
//   - 50ms     音乐播放器（MIDI弹奏给AI世界全场广播）
//   - 100ms    体力回复计数 + PUBG子弹物理引擎更新
//   - 200ms    体力消耗
//   - 1s       多人世界RTT广播 + 场景时间+1 + GAME_TIME_TICK任务条件 + GCG游戏tick
//   - 5s       多人世界玩家位置广播 + AI世界自动同意敲门
//   - 10s      场景时间通知 + 玩家时间通知
//   - 1min     清理LRU lua state + 天气随机
//   - 1hour    日志（暂无业务）
//
// 玩家定时器（CreateUserTimer）允许业务延迟N秒触发某个动作 用于Lua创建怪物、Plugin的延迟回调等

const (
	ServerTickTime = 50  // 服务器全局tick最小间隔毫秒
	UserTickTime   = 100 // 玩家自身tick最小间隔毫秒
)

// UserTimer 玩家级延迟任务
// timeout 触发时间戳(ms) action 动作类型 data 透传参数
type UserTimer struct {
	timeout int64
	action  int
	data    []any
}

// UserTick 玩家个人的tick上下文
// globalTick      100ms周期触发器
// globalTickCount 已触发次数 用于派生秒级/分钟级
// timerMap        延迟任务表 主循环每次玩家tick扫描该表执行到期任务
type UserTick struct {
	globalTick      *time.Ticker
	globalTickCount uint64
	timerIdCounter  uint64
	timerMap        map[uint64]*UserTimer
}

// TickManager 全局tick管理器
// userTickMap 每个玩家的UserTick实例（OnLogin时创建 OnOffline时销毁）
// tm 上一次tick的wall clock 用于检测分钟/小时/日/月切换
type TickManager struct {
	globalTick      *time.Ticker
	globalTickCount uint64
	userTickMap     map[uint32]*UserTick
	tm              time.Time
}

func NewTickManager() (r *TickManager) {
	r = new(TickManager)
	r.globalTick = time.NewTicker(time.Millisecond * ServerTickTime)
	r.globalTickCount = 0
	r.userTickMap = make(map[uint32]*UserTick)
	r.tm = time.Now()
	logger.Info("game server tick start at: %v", time.Now().UnixMilli())
	return r
}

func (t *TickManager) GetGlobalTick() *time.Ticker {
	return t.globalTick
}

// 每个玩家自己的tick

// CreateUserGlobalTick 创建玩家tick对象 OnLogin时调用
func (t *TickManager) CreateUserGlobalTick(userId uint32) {
	t.userTickMap[userId] = &UserTick{
		globalTick:      time.NewTicker(time.Millisecond * UserTickTime),
		globalTickCount: 0,
		timerIdCounter:  0,
		timerMap:        make(map[uint64]*UserTimer),
	}
}

// DestroyUserGlobalTick 销毁玩家tick对象 OnOffline时调用
// 必须Stop ticker防止goroutine泄漏
func (t *TickManager) DestroyUserGlobalTick(userId uint32) {
	userTick, exist := t.userTickMap[userId]
	if !exist {
		logger.Error("user not exist, uid: %v", userId)
		return
	}
	userTick.globalTick.Stop()
	delete(t.userTickMap, userId)
}

// CreateUserTimer 创建玩家级延迟任务 delay秒后触发指定action
// 任务在玩家tick扫描时执行（精度100ms）已过期则在下次tick触发
// data按action类型决定语义 由 userTimerHandle 分支解析
func (t *TickManager) CreateUserTimer(userId uint32, action int, delay uint32, data ...any) {
	userTick, exist := t.userTickMap[userId]
	if !exist {
		logger.Error("user not exist, uid: %v", userId)
		return
	}
	userTick.timerIdCounter++
	timeout := time.Now().UnixMilli() + int64(delay)*1000
	userTick.timerMap[userTick.timerIdCounter] = &UserTimer{
		timeout: timeout,
		action:  action,
		data:    data,
	}
	logger.Debug("create user timer, uid: %v, action: %v, time: %v",
		userId, action, time.Now().Add(time.Second*time.Duration(delay)).Format("2006-01-02 15:04:05"))
}

func (t *TickManager) onUserTickSecond(userId uint32, now int64) {
}

// onUserTickMinute 玩家级分钟tick 主要任务：检测60秒未心跳则踢人
// AI玩家（uid<PlayerBaseUid）跳过 因为不会有心跳
func (t *TickManager) onUserTickMinute(userId uint32, now int64) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	if userId < PlayerBaseUid {
		return
	}
	if uint32(now/1000)-player.LastKeepaliveTime > 60 {
		logger.Error("remove keepalive timeout user, uid: %v", userId)
		GAME.OnOffline(userId, "", &ChangeGsInfo{
			IsChangeGs: false,
		})
	}
}

// 玩家定时任务常量
// 由 CreateUserTimer 的 action 参数标识 被 userTimerHandle 分发

const (
	UserTimerActionTest               = iota // 测试用 仅打印日志
	UserTimerActionLuaCreateMonster          // Lua脚本调用 ScriptLib.CreateMonster 延迟创建怪物
	UserTimerActionLuaGroupTimerEvent        // Lua脚本调用 ScriptLib.CreateGroupTimerEvent 延迟触发场景组timer事件
	UserTimerActionPlugin                    // 插件系统CreateUserTimer延迟回调（PUBG的UserTimerPubgEnd等）
)

// userTimerHandle 玩家定时任务到期分发
// 主循环每次玩家tick扫描其timerMap 执行已到期的timer
func (t *TickManager) userTimerHandle(userId uint32, action int, data []any) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		return
	}
	switch action {
	case UserTimerActionTest:
		logger.Debug("UserTimerActionTest, data: %v, uid: %v", data[0], userId)
	case UserTimerActionLuaCreateMonster:
		logger.Debug("UserTimerActionLuaCreateMonster, groupId: %v, configId: %v, uid: %v", data[0], data[1], userId)
		groupId := data[0].(uint32)
		configId := data[1].(uint32)
		GAME.SceneGroupCreateEntity(player, groupId, configId, constant.ENTITY_TYPE_MONSTER)
	case UserTimerActionLuaGroupTimerEvent:
		logger.Debug("UserTimerActionLuaGroupTimerEvent, groupId: %v, source: %v, uid: %v", data[0], data[1], userId)
		groupId := data[0].(uint32)
		source := data[1].(string)
		world := WORLD_MANAGER.GetWorldById(player.WorldId)
		if world == nil {
			logger.Error("get world is nil, worldId: %v, uid: %v", player.WorldId, userId)
			return
		}
		scene := world.GetSceneById(player.GetSceneId())
		group := scene.GetGroupById(groupId)
		if group == nil {
			logger.Error("get group is nil, groupId: %v, uid: %v", groupId, userId)
			return
		}
		GAME.TimerEventTriggerCheck(player, group, source)
	case UserTimerActionPlugin:
		logger.Debug("UserTimerActionPlugin, data: %v", data)
		PLUGIN_MANAGER.HandleUserTimer(player, data)
	}
}

// 服务器全局tick

// OnGameServerTick 全局tick入口 主循环select接到globalTick触发时调用
// 按 globalTickCount 累计派生多级周期：50ms / 100ms / 200ms / 1s / 5s / 10s / 1min / 1h
// 所有玩家tick扫描在此一并完成（每玩家100ms精度 取决于select是否轮到它）
// 末尾用 wall clock 比对检测分钟/小时/日/月切换（onMinuteChange等）
func (t *TickManager) OnGameServerTick() {
	t.globalTickCount++
	tm := time.Now()
	now := tm.UnixMilli()
	if t.globalTickCount%(50/ServerTickTime) == 0 {
		t.onTick50MilliSecond(now)
	}
	if t.globalTickCount%(100/ServerTickTime) == 0 {
		t.onTick100MilliSecond(now)
	}
	if t.globalTickCount%(200/ServerTickTime) == 0 {
		t.onTick200MilliSecond(now)
	}
	if t.globalTickCount%(1000/ServerTickTime) == 0 {
		t.onTickSecond(now)
		PLUGIN_MANAGER.HandleGlobalTick(PluginGlobalTickSecond)
	}
	if t.globalTickCount%(5000/ServerTickTime) == 0 {
		t.onTick5Second(now)
	}
	if t.globalTickCount%(10000/ServerTickTime) == 0 {
		t.onTick10Second(now)
	}
	if t.globalTickCount%(60000/ServerTickTime) == 0 {
		t.onTickMinute(now)
	}
	if t.globalTickCount%(60000*60/ServerTickTime) == 0 {
		t.onTickHour(now)
	}
	for userId, userTick := range t.userTickMap {
		select {
		case <-userTick.globalTick.C:
			break
		default:
			// 跳过还没到时间的定时器
			continue
		}
		userTick.globalTickCount++
		if userTick.globalTickCount%(1000/UserTickTime) == 0 {
			t.onUserTickSecond(userId, now)
		}
		if userTick.globalTickCount%(60000/UserTickTime) == 0 {
			t.onUserTickMinute(userId, now)
		}
		for timerId, timer := range userTick.timerMap {
			if now < timer.timeout {
				// 跳过还没到时间的定时器
				continue
			}
			delete(userTick.timerMap, timerId)
			t.userTimerHandle(userId, timer.action, timer.data)
		}
	}
	if tm.Minute() != t.tm.Minute() {
		t.onMinuteChange(now)
		PLUGIN_MANAGER.HandleGlobalTick(PluginGlobalTickMinuteChange)
	}
	if tm.Hour() != t.tm.Hour() {
		t.onHourChange(now)
	}
	if tm.Day() != t.tm.Day() {
		t.onDayChange(now)
	}
	if tm.Month() != t.tm.Month() {
		t.onMonthChange(now)
	}
	t.tm = tm
}

func (t *TickManager) onMonthChange(now int64) {
	logger.Info("on month change, time: %v", now)
}

func (t *TickManager) onDayChange(now int64) {
	logger.Info("on day change, time: %v", now)
}

func (t *TickManager) onHourChange(now int64) {
	logger.Info("on hour change, time: %v", now)
}

func (t *TickManager) onMinuteChange(now int64) {
}

func (t *TickManager) onTickHour(now int64) {
	logger.Info("on tick hour, time: %v", now)
}

func (t *TickManager) onTickMinute(now int64) {
	gdconf.LuaStateLruRemove()
	for _, world := range WORLD_MANAGER.GetAllWorld() {
		if world.GetOwner().SceneLoadState == model.SceneEnterDone {
			// 天气气象随机
			for _, scene := range world.GetAllScene() {
				for _, scenePlayer := range scene.GetAllPlayer() {
					GAME.WeatherClimateRandom(scenePlayer, scenePlayer.WeatherInfo.WeatherAreaId)
				}
			}
		}
	}
}

func (t *TickManager) onTick10Second(now int64) {
	for _, world := range WORLD_MANAGER.GetAllWorld() {
		if world.GetOwner().SceneLoadState == model.SceneEnterDone {
			GAME.SceneTimeNotify(world)
			GAME.PlayerTimeNotify(world)
		}
	}
}

func (t *TickManager) onTick5Second(now int64) {
	for _, world := range WORLD_MANAGER.GetAllWorld() {
		if world.GetOwner().SceneLoadState == model.SceneEnterDone {
			// 多人世界其他玩家的坐标位置广播
			if world.IsMultiplayerWorld() {
				GAME.WorldPlayerLocationNotify(world)
				GAME.ScenePlayerLocationNotify(world)
			}
		}
	}
	iPlugin, err := PLUGIN_MANAGER.GetPlugin(&PluginPubg{})
	if err != nil {
		logger.Error("get plugin pubg error: %v", err)
		return
	}
	pluginPubg := iPlugin.(*PluginPubg)
	agree := true
	if pluginPubg.IsStartPubg() {
		agree = false
	}
	aiWorld := WORLD_MANAGER.GetAiWorld()
	if aiWorld.GetWorldPlayerNum() >= 100 {
		agree = false
	}
	for applyUid := range aiWorld.GetOwner().CoopApplyMap {
		GAME.PlayerDealEnterWorld(aiWorld.GetOwner(), applyUid, agree)
	}
}

func (t *TickManager) onTickSecond(now int64) {
	for _, world := range WORLD_MANAGER.GetAllWorld() {
		if world.GetOwner().SceneLoadState == model.SceneEnterDone {
			// 世界里所有玩家的网络延迟广播
			if world.IsMultiplayerWorld() {
				GAME.WorldPlayerRTTNotify(world)
			}
			// 场景时间增加
			if !world.GetOwner().Pause && world.GetOwner().PropMap[constant.PLAYER_PROP_IS_GAME_TIME_LOCKED] != 1 {
				world.ChangeGameTime(world.GetGameTime() + 1)
				GAME.TriggerQuest(world.GetOwner(), constant.QUEST_FINISH_COND_TYPE_GAME_TIME_TICK, "")
			}
		}
	}
	// GCG游戏Tick
	for _, game := range GCG_MANAGER.gameMap {
		game.onTick()
	}
}

func (t *TickManager) onTick200MilliSecond(now int64) {
	for _, world := range WORLD_MANAGER.GetAllWorld() {
		for _, player := range world.GetAllPlayer() {
			if player.SceneLoadState == model.SceneEnterDone {
				// 耐力消耗
				GAME.SustainStaminaHandler(player)
				GAME.VehicleRestoreStaminaHandler(player)
			}
		}
	}
}

func (t *TickManager) onTick100MilliSecond(now int64) {
	for _, world := range WORLD_MANAGER.GetAllWorld() {
		for _, player := range world.GetAllPlayer() {
			if player.SceneLoadState == model.SceneEnterDone {
				// 耐力回复计数器
				GAME.RestoreCountStaminaHandler(player)
			}
		}
	}

	iPlugin, err := PLUGIN_MANAGER.GetPlugin(&PluginPubg{})
	if err != nil {
		logger.Error("get plugin pubg error: %v", err)
		return
	}
	pluginPubg := iPlugin.(*PluginPubg)
	if !pluginPubg.IsStartPubg() {
		return
	}
	world := WORLD_MANAGER.GetAiWorld()
	bulletPhysicsEngine := world.GetBulletPhysicsEngine()
	hitList := bulletPhysicsEngine.Update(now)
	for _, rigidBody := range hitList {
		scene := world.GetSceneById(rigidBody.sceneId)
		pluginPubg.PubgHit(scene, rigidBody.hitAvatarEntityId, rigidBody.avatarEntityId, true)
	}
}

func (t *TickManager) onTick50MilliSecond(now int64) {
	// 音乐播放器
	for i := 0; i < len(AudioChan); i++ {
		world := WORLD_MANAGER.GetAiWorld()
		GAME.SendToWorldA(world, cmd.SceneAudioNotify, 0, &proto.SceneAudioNotify{
			Type:      5,
			SourceUid: world.GetOwner().PlayerId,
			Param1:    []uint32{1, <-AudioChan},
			Param2:    nil,
			Param3:    nil,
		}, 0)
	}
}
