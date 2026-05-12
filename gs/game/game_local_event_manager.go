package game

import (
	"time"

	"hk4e/gdconf"

	"github.com/flswld/halo/logger"
)

// 本地事件队列管理器
//
// 异步IO同步化的核心机制：
//   1. 主循环单线程执行 任何阻塞IO都会卡死整个GS
//   2. 解决方案：阻塞IO在goroutine内执行 完成后通过 LocalEventChan 回主循环
//   3. 主循环在 select 中接收 LocalEvent 调用 LocalEventHandle 分发到具体业务函数
//
// 每个事件类型代表一个完整的"异步操作完成"事件 业务函数在主循环单线程内安全访问玩家数据

const (
	UserLoginLoadFromDbFinish  = iota // 玩家登录从数据库加载完成回调
	UserOfflineSaveToDbFinish         // 玩家离线保存完成
	RunUserCopyAndSave                // 执行一次在线玩家内存数据复制到数据库写入协程
	ExitRunUserCopyAndSave            // 停服时执行全部玩家保存操作
	ReloadGameDataConfig              // 执行热更表
	ReloadGameDataConfigFinish        // 热更表完成
	AsyncLoadSceneBlockFinish         // 异步加载场景区块存档完成
)

// LocalEvent 本地事件消息体 EventId定事件类型 Msg是事件携带的具体数据
type LocalEvent struct {
	EventId int
	Msg     any
}

// LocalEventManager 持有事件队列channel（缓冲1000） 主循环通过 GetLocalEventChan 接收
type LocalEventManager struct {
	localEventChan chan *LocalEvent
}

func NewLocalEventManager() (r *LocalEventManager) {
	r = new(LocalEventManager)
	r.localEventChan = make(chan *LocalEvent, 1000)
	return r
}

func (l *LocalEventManager) GetLocalEventChan() chan *LocalEvent {
	return l.localEventChan
}

// LocalEventHandle 主循环select分发事件到具体业务函数
// 每个case对应一种异步IO完成 内部调用主循环单线程内的业务方法
// 注意 ExitRunUserCopyAndSave 的最后会 select{} 永久阻塞主循环 等进程退出
func (l *LocalEventManager) LocalEventHandle(localEvent *LocalEvent) {
	switch localEvent.EventId {
	case UserLoginLoadFromDbFinish:
		playerLoginInfo := localEvent.Msg.(*PlayerLoginInfo)
		GAME.OnLogin(playerLoginInfo.UserId, playerLoginInfo.ClientSeq, playerLoginInfo.GateAppId, playerLoginInfo.Player, playerLoginInfo.Req, playerLoginInfo.Ok)
	case UserOfflineSaveToDbFinish:
		playerOfflineInfo := localEvent.Msg.(*PlayerOfflineInfo)
		USER_MANAGER.OfflineUser(playerOfflineInfo.Player, playerOfflineInfo.ChangeGsInfo)
	case RunUserCopyAndSave:
		USER_MANAGER.UserCopyAndSave(false)
	case ExitRunUserCopyAndSave:
		USER_MANAGER.UserCopyAndSave(true)
		// 在此阻塞掉主协程 不再进行任何消息和任务的处理
		logger.Warn("game main loop block")
		select {}
	case ReloadGameDataConfig:
		reloadSceneLua := localEvent.Msg.(bool)
		go func() {
			defer func() {
				if err := recover(); err != nil {
					logger.Error("reload game data config error: %v", err)
				}
			}()
			gdconf.ReloadGameDataConfig(reloadSceneLua)
			l.localEventChan <- &LocalEvent{
				EventId: ReloadGameDataConfigFinish,
			}
		}()
	case ReloadGameDataConfigFinish:
		gdconf.ReplaceGameDataConfig()
		startTime := time.Now().UnixNano()
		WORLD_MANAGER.LoadSceneAoi()
		endTime := time.Now().UnixNano()
		costTime := endTime - startTime
		logger.Info("run [LoadSceneAoi], cost time: %v ns", costTime)
	case AsyncLoadSceneBlockFinish:
		sceneBlockLoadInfo := localEvent.Msg.(*SceneBlockLoadInfo)
		GAME.OnSceneBlockLoad(sceneBlockLoadInfo)
	}
}
