package game

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"hk4e/common/config"
	"hk4e/common/mq"
	"hk4e/gs/dao"
	"hk4e/gs/model"
	"hk4e/protocol/proto"

	"github.com/flswld/halo/logger"
	"github.com/vmihailenco/msgpack/v5"
)

// 玩家管理器
//
// 三层存储模型：
//   1. 内存（playerMap）              —— 在线玩家直接在此操作 单线程无锁
//   2. Redis（30天过期 msgpack+lz4压缩）—— 跨GS共享 临时离线档加载来源
//   3. DB（GORM/MongoDB/SQLite三选一）—— 永久持久化
//
// 玩家档案的IO都在goroutine异步执行 完成后通过 LOCAL_EVENT_MANAGER 回调主循环
// 玩家在线状态：
//   - 本地在线：playerMap中且Online=true
//   - 远程在线：remotePlayerMap中（玩家在其他GS）
//   - 全离线：两个map都没有
//
// 三个核心goroutine（NewUserManager中启动）：
//   - saveUserHandle      每分钟一次定时存档（投递RunUserCopyAndSave事件）+ 接收saveUserChan写库
//   - autoSyncRemotePlayerMap 每60秒同步全服在线表
//   - asyncWriteDbHandle 通用异步DB写入队列（聊天记录等场景）

type UserManager struct {
	db                  *dao.Dao                  // db对象
	playerMap           map[uint32]*model.Player  // 内存玩家数据 key:uid 主循环单线程访问无需加锁
	saveUserChan        chan *SaveUserData        // 主循环→定时保存协程的玩家数据通道（缓冲100）
	remotePlayerMap     map[uint32]string         // 远程玩家 key:userId value:玩家所在gs的appid
	remotePlayerMapLock sync.RWMutex              // remotePlayerMap读写锁（跨goroutine同步）
	asyncWriteDbChan    chan func(u *UserManager) // 通用异步DB写入队列（缓冲100）handler可投递任意闭包到这里执行
}

// NewUserManager 创建管理器并启动3个goroutine
// saveUserHandle: 定时保存+接收存档队列
// syncRemotePlayerMap: 启动时同步一次远程在线表
// autoSyncRemotePlayerMap: 60秒一次定时同步
// asyncWriteDbHandle: 异步DB写入队列消费
func NewUserManager(db *dao.Dao) (r *UserManager) {
	r = new(UserManager)
	r.db = db
	r.playerMap = make(map[uint32]*model.Player)
	r.saveUserChan = make(chan *SaveUserData, 100)
	r.remotePlayerMap = make(map[uint32]string)
	r.asyncWriteDbChan = make(chan func(u *UserManager), 100)
	r.saveUserHandle()
	r.syncRemotePlayerMap()
	r.autoSyncRemotePlayerMap()
	r.asyncWriteDbHandle()
	return r
}

// 在线玩家相关操作

// GetUserOnlineState 获取玩家在线状态
func (u *UserManager) GetUserOnlineState(userId uint32) bool {
	player, exist := u.playerMap[userId]
	if !exist {
		return false
	} else {
		return player.Online
	}
}

// GetOnlineUser 获取在线玩家对象
func (u *UserManager) GetOnlineUser(userId uint32) *model.Player {
	player, exist := u.playerMap[userId]
	if !exist {
		return nil
	} else {
		if player.Online {
			return player
		} else {
			return nil
		}
	}
}

// GetAllOnlineUserList 获取全部在线玩家
func (u *UserManager) GetAllOnlineUserList() map[uint32]*model.Player {
	onlinePlayerMap := make(map[uint32]*model.Player)
	for userId, player := range u.playerMap {
		if player.Online == false {
			continue
		}
		onlinePlayerMap[userId] = player
	}
	return onlinePlayerMap
}

// AddUser 向内存玩家数据里添加一个玩家
func (u *UserManager) AddUser(player *model.Player) {
	if player == nil {
		return
	}
	u.playerMap[player.PlayerId] = player
}

// DeleteUser 从内存玩家数据里删除一个玩家
func (u *UserManager) DeleteUser(userId uint32) {
	delete(u.playerMap, userId)
}

type PlayerLoginInfo struct {
	UserId    uint32
	Player    *model.Player
	ClientSeq uint32
	GateAppId string
	Req       *proto.PlayerLoginReq
	Ok        bool
}

// UserLoginLoad 玩家登录入口 异步加载玩家档
// 由 PlayerLoginReq handler 调用 起独立goroutine执行所有阻塞IO 完成后通过 LocalEvent 回调主循环
// 流程：分布式锁(redis SetNX) → DB加载 → Redis写入 → 加载聊天记录 → 同步加载附近场景block
//  1. 集群模式才加分布式锁 防止跨GS并发登录同一uid
//  2. 加载场景block用同步方式（LoadSceneBlockSync）因为登录时玩家还未在内存 不会卡主循环
//  3. 任何步骤失败都会发 Ok=false 的 UserLoginLoadFromDbFinish 事件回主循环
func (u *UserManager) UserLoginLoad(userId uint32, clientSeq uint32, gateAppId string, req *proto.PlayerLoginReq) {
	_, exist := u.playerMap[userId]
	// 正常登录
	if exist {
		// 每次玩家上线必须从数据库加载最新的档 如果之前存在于内存则删掉
		u.DeleteUser(userId)
	}
	go func() {
		if !config.GetConfig().Hk4e.StandaloneModeEnable {
			// 加离线玩家数据分布式锁（10秒TTL 50ms重试 共2次）
			ok := u.db.DistLockSync(userId)
			if !ok {
				logger.Error("lock redis offline player data error, uid: %v", userId)
				LOCAL_EVENT_MANAGER.GetLocalEventChan() <- &LocalEvent{
					EventId: UserLoginLoadFromDbFinish,
					Msg: &PlayerLoginInfo{
						UserId:    userId,
						ClientSeq: clientSeq,
						GateAppId: gateAppId,
						Ok:        false,
					},
				}
				return
			}
		}
		player, err := u.LoadUserFromDbSync(userId)
		if err != nil {
			logger.Error("can not load user from db, uid: %v", userId)
			LOCAL_EVENT_MANAGER.GetLocalEventChan() <- &LocalEvent{
				EventId: UserLoginLoadFromDbFinish,
				Msg: &PlayerLoginInfo{
					UserId:    userId,
					ClientSeq: clientSeq,
					GateAppId: gateAppId,
					Ok:        false,
				},
			}
			if !config.GetConfig().Hk4e.StandaloneModeEnable {
				// 解离线玩家数据分布式锁
				u.db.DistUnlock(userId)
			}
			return
		}
		if player != nil {
			u.SaveUserToRedisSync(player)
			u.ChangeUserDbState(player, model.DbNormal)
			player.ChatMsgMap = u.LoadUserChatMsgFromDbSync(userId)
			sceneBlockMap := GAME.LoadSceneBlockSync(player.PlayerId, player.GetSceneId(), player.GetPos())
			if sceneBlockMap != nil {
				player.SceneBlockMap = sceneBlockMap
			}
		}
		LOCAL_EVENT_MANAGER.GetLocalEventChan() <- &LocalEvent{
			EventId: UserLoginLoadFromDbFinish,
			Msg: &PlayerLoginInfo{
				UserId:    userId,
				Player:    player,
				ClientSeq: clientSeq,
				GateAppId: gateAppId,
				Req:       req,
				Ok:        true,
			},
		}
		if !config.GetConfig().Hk4e.StandaloneModeEnable {
			// 解离线玩家数据分布式锁
			u.db.DistUnlock(userId)
		}
	}()
}

// OnlineUser 玩家上线 加入playerMap并广播全服在线状态变更
// 由 OnLogin 在DB加载完成后调用 同时原子递增 ONLINE_PLAYER_NUM（用于Keepalive上报负载）
// ServerUserOnlineStateChangeNotify 让其他GS的remotePlayerMap更新
func (u *UserManager) OnlineUser(player *model.Player) {
	player.Online = true
	player.OnlineTime = uint32(time.Now().Unix())
	u.AddUser(player)
	GAME.messageQueue.SendToAll(&mq.NetMsg{
		MsgType: mq.MsgTypeServer,
		EventId: mq.ServerUserOnlineStateChangeNotify,
		ServerMsg: &mq.ServerMsg{
			UserId:   player.PlayerId,
			IsOnline: true,
		},
	})
	atomic.AddInt32(&ONLINE_PLAYER_NUM, 1)
}

// ChangeGsInfo 跨服切换信息 从旧GS下线时附带
// IsChangeGs=true表示玩家要迁移到另一个GS（如跨服多人）非真正下线 走"类重登"流程
// JoinHostUserId 指定要加入的房主uid 用于查 remotePlayerMap 找到目标GS的appid
type ChangeGsInfo struct {
	IsChangeGs     bool
	JoinHostUserId uint32
}

type PlayerOfflineInfo struct {
	Player       *model.Player
	ChangeGsInfo *ChangeGsInfo
}

// UserOfflineSave 玩家离线流程的"保存阶段"
// 异步msgpack序列化玩家档+场景block 后台goroutine写DB+Redis 完成后通过LocalEvent推UserOfflineSaveToDbFinish
// 特殊标志位：
//   - NotSave        跳过保存（GM测试用）直接发完成事件
//   - OfflineClear   清档（重新CreatePlayer 保留DbState）+ 删除聊天记录
//
// 注意：序列化在主循环中同步执行（msgpack.Marshal），不能太久 否则卡死整个GS
//
//	玩家档大小一般几十KB 序列化耗时<10ms 可接受
func (u *UserManager) UserOfflineSave(player *model.Player, changeGsInfo *ChangeGsInfo) {
	player.Online = false
	player.OfflineTime = uint32(time.Now().Unix())
	player.TotalOnlineTime += uint32(time.Now().Unix()) - player.OnlineTime
	if player.NotSave {
		LOCAL_EVENT_MANAGER.GetLocalEventChan() <- &LocalEvent{
			EventId: UserOfflineSaveToDbFinish,
			Msg: &PlayerOfflineInfo{
				Player:       player,
				ChangeGsInfo: changeGsInfo,
			},
		}
		return
	}
	if player.OfflineClear {
		u.AsyncWriteDb(func(u *UserManager) {
			u.DeleteUserAllChatMsgToDbSync(player.PlayerId)
		})
		newPlayer := GAME.CreatePlayer(player.PlayerId)
		newPlayer.DbState = player.DbState
		player = newPlayer
	}
	startTime := time.Now().UnixNano()
	playerData, err := msgpack.Marshal(player)
	if err != nil {
		logger.Error("marshal player data error: %v", err)
		playerData = nil
	}
	endTime := time.Now().UnixNano()
	costTime := endTime - startTime
	logger.Info("offline copy player data cost time: %v ns", costTime)
	startTime = time.Now().UnixNano()
	sceneBlockData, err := msgpack.Marshal(player.SceneBlockMap)
	if err != nil {
		logger.Error("marshal scene block data error: %v", err)
		sceneBlockData = nil
	}
	endTime = time.Now().UnixNano()
	costTime = endTime - startTime
	logger.Info("offline copy scene block data cost time: %v ns", costTime)
	go func() {
		if playerData != nil {
			playerCopy := new(model.Player)
			err := msgpack.Unmarshal(playerData, playerCopy)
			if err != nil {
				logger.Error("unmarshal player data error: %v", err)
				playerCopy = nil
			}
			if playerCopy != nil {
				playerCopy.DbState = player.DbState
				u.SaveUserToDbSync(playerCopy)
				u.SaveUserToRedisSync(playerCopy)
			}
		}
		if sceneBlockData != nil {
			sceneBlockMapCopy := make(map[uint32]*model.SceneBlock)
			err = msgpack.Unmarshal(sceneBlockData, &sceneBlockMapCopy)
			if err != nil {
				logger.Error("unmarshal scene block data error: %v", err)
				sceneBlockMapCopy = nil
			}
			if sceneBlockMapCopy != nil {
				GAME.SaveSceneBlockSync(player.PlayerId, sceneBlockMapCopy)
			}
		}
		LOCAL_EVENT_MANAGER.GetLocalEventChan() <- &LocalEvent{
			EventId: UserOfflineSaveToDbFinish,
			Msg: &PlayerOfflineInfo{
				Player:       player,
				ChangeGsInfo: changeGsInfo,
			},
		}
	}()
}

// OfflineUser 玩家离线流程的"清理阶段"
// 由 LocalEvent UserOfflineSaveToDbFinish 触发（异步保存完成后）
// 真正从playerMap移除玩家 广播离线状态 跨服切换时通知Gate把KCP连接路由到新GS
func (u *UserManager) OfflineUser(player *model.Player, changeGsInfo *ChangeGsInfo) {
	u.DeleteUser(player.PlayerId)
	GAME.messageQueue.SendToAll(&mq.NetMsg{
		MsgType: mq.MsgTypeServer,
		EventId: mq.ServerUserOnlineStateChangeNotify,
		ServerMsg: &mq.ServerMsg{
			UserId:   player.PlayerId,
			IsOnline: false,
		},
	})
	atomic.AddInt32(&ONLINE_PLAYER_NUM, -1)
	if changeGsInfo.IsChangeGs {
		// 跨服无感切换：通知Gate把玩家KCP连接路由到新GS appid
		// Gate 收到 ServerUserGsChangeNotify 后 自己代发 PlayerLoginReq 到新GS（gate/net/session.go:260）
		// 客户端**完全无感** KCP不断 也不收 ClientReconnectNotify（与 ReLoginPlayer 流程不同）
		gsAppId := u.GetRemoteUserGsAppId(changeGsInfo.JoinHostUserId)
		GAME.messageQueue.SendToGate(player.GateAppId, &mq.NetMsg{
			MsgType: mq.MsgTypeServer,
			EventId: mq.ServerUserGsChangeNotify,
			ServerMsg: &mq.ServerMsg{
				UserId:          player.PlayerId,
				GameServerAppId: gsAppId,
				JoinHostUserId:  changeGsInfo.JoinHostUserId,
			},
		})
		logger.Info("user change gs notify to gate, uid: %v, gate appid: %v, gs appid: %v, host uid: %v",
			player.PlayerId, player.GateAppId, gsAppId, changeGsInfo.JoinHostUserId)
	}
}

// ChangeUserDbState 玩家存档状态机 严格控制状态转换
// 状态转换图：
//
//	DbNone   → Insert/Delete/Normal（新玩家初始化时）
//	DbInsert → 任何状态（不允许 必须先insert到DB才能转换）
//	DbDelete → DbNormal（撤销删除时）
//	DbNormal → DbDelete（删除玩家时）
//
// UserCopyAndSave 按当前DbState决定走 Insert/Update/Delete DB操作
func (u *UserManager) ChangeUserDbState(player *model.Player, state int) {
	if player == nil {
		return
	}
	switch player.DbState {
	case model.DbNone:
		if state == model.DbInsert {
			player.DbState = model.DbInsert
		} else if state == model.DbDelete {
			player.DbState = model.DbDelete
		} else if state == model.DbNormal {
			player.DbState = model.DbNormal
		} else {
			logger.Error("player db state change not allow, before: %v, after: %v", player.DbState, state)
		}
	case model.DbInsert:
		logger.Error("player db state change not allow, before: %v, after: %v", player.DbState, state)
		break
	case model.DbDelete:
		if state == model.DbNormal {
			player.DbState = model.DbNormal
		} else {
			logger.Error("player db state change not allow, before: %v, after: %v", player.DbState, state)
		}
	case model.DbNormal:
		if state == model.DbDelete {
			player.DbState = model.DbDelete
		} else {
			logger.Error("player db state change not allow, before: %v, after: %v", player.DbState, state)
		}
	}
}

// 远程玩家相关操作
//
// remotePlayerMap 维护"在其他GS上在线"的玩家uid → 所在GS appid 的映射
// 用于跨服多人/聊天/添加好友/GM 等需要联络其他GS玩家的场景
// 数据来源：
//   1. 启动时和每60秒一次：从Node拉全局在线表（GetGlobalGsOnlineMap）
//   2. 实时事件：其他GS广播 ServerUserOnlineStateChangeNotify 时增量更新

// autoSyncRemotePlayerMap 启动定时同步远程在线表的goroutine 60秒一次
func (u *UserManager) autoSyncRemotePlayerMap() {
	go func() {
		ticker := time.NewTicker(time.Second * 60)
		for {
			<-ticker.C
			u.syncRemotePlayerMap()
		}
	}()
}

// syncRemotePlayerMap 从Node拉取全局在线表 整体替换remotePlayerMap
// 本地在线的玩家不放进remotePlayerMap（避免同一uid同时本地+远程在线导致路由错误）
// 操作在主循环外的goroutine中执行 用remotePlayerMapLock保护写入
func (u *UserManager) syncRemotePlayerMap() {
	rsp, err := GAME.discoveryClient.GetGlobalGsOnlineMap(context.TODO(), nil)
	if err != nil {
		logger.Error("get global gs online map error: %v", err)
		return
	}
	copyMap := make(map[uint32]string)
	for k, v := range rsp.OnlineMap {
		player, exist := u.playerMap[k]
		if exist && player.Online {
			continue
		}
		copyMap[k] = v
	}
	copyMapLen := len(copyMap)
	u.remotePlayerMapLock.Lock()
	u.remotePlayerMap = copyMap
	u.remotePlayerMapLock.Unlock()
	logger.Info("sync remote player map finish, len: %v", copyMapLen)
}

// GetRemoteUserOnlineState 查询玩家是否在远程GS在线
func (u *UserManager) GetRemoteUserOnlineState(userId uint32) bool {
	u.remotePlayerMapLock.RLock()
	_, exist := u.remotePlayerMap[userId]
	u.remotePlayerMapLock.RUnlock()
	if !exist {
		return false
	} else {
		return true
	}
}

// GetRemoteUserGsAppId 查询玩家所在的GS appid（远程在线时） 不在线返回空串
func (u *UserManager) GetRemoteUserGsAppId(userId uint32) string {
	u.remotePlayerMapLock.RLock()
	appId, exist := u.remotePlayerMap[userId]
	u.remotePlayerMapLock.RUnlock()
	if !exist {
		return ""
	} else {
		return appId
	}
}

// SetRemoteUserOnlineState 实时事件驱动的远程在线状态更新
// 由 ROUTE_MANAGER 在收到 ServerUserOnlineStateChangeNotify 时调用
// 玩家从远程下线时同时清理本地 playerMap（防止跨服迁移残留状态）
func (u *UserManager) SetRemoteUserOnlineState(userId uint32, isOnline bool, appId string) {
	u.remotePlayerMapLock.Lock()
	if isOnline {
		u.remotePlayerMap[userId] = appId
	} else {
		delete(u.remotePlayerMap, userId)
		u.DeleteUser(userId)
	}
	u.remotePlayerMapLock.Unlock()
}

// GetAllRemoteAiUidList 获取所有远程在线的AI玩家uid列表
// AI玩家uid范围 [AiBaseUid, AiBaseUid+1000) 实际就是其他GS的"小可爱"
// 用于广播聊天等需要给所有AI同步消息的场景
func (u *UserManager) GetAllRemoteAiUidList() []uint32 {
	userIdList := make([]uint32, 0)
	u.remotePlayerMapLock.RLock()
	for userId := uint32(AiBaseUid); userId < AiBaseUid+1000; userId++ {
		_, exist := u.remotePlayerMap[userId]
		if !exist {
			continue
		}
		userIdList = append(userIdList, userId)
	}
	u.remotePlayerMapLock.RUnlock()
	return userIdList
}

// GetRemoteOnlineUserList 获取指定数量的远程在线玩家 玩家数据只读禁止修改
func (u *UserManager) GetRemoteOnlineUserList(total int) map[uint32]*model.Player {
	if total > 50 {
		return nil
	}
	onlinePlayerMap := make(map[uint32]*model.Player)
	count := 0
	userIdList := make([]uint32, 0)
	u.remotePlayerMapLock.RLock()
	for userId := range u.remotePlayerMap {
		if userId < PlayerBaseUid || userId > MaxPlayerBaseUid {
			continue
		}
		userIdList = append(userIdList, userId)
		count++
		if count >= total {
			break
		}
	}
	u.remotePlayerMapLock.RUnlock()
	for _, userId := range userIdList {
		player := u.LoadTempOfflineUser(userId, false)
		if player == nil {
			continue
		}
		onlinePlayerMap[player.PlayerId] = player
	}
	return onlinePlayerMap
}

// LoadGlobalPlayer 一站式获取全服玩家信息（不论本地/远程/离线）
// 三态返回：本地在线（取自playerMap）/ 远程在线（加载临时离线档）/ 全离线（加载临时离线档）
// 远程在线情况为简化实现统一走临时离线档加载 数据有滞后但够用
// 调用方只读使用 不要修改返回的player对象（远程/离线情况下修改不会持久化）
func (u *UserManager) LoadGlobalPlayer(userId uint32) (player *model.Player, online bool, remote bool) {
	online = u.GetUserOnlineState(userId)
	remote = false
	if !online {
		// 本地不在线就看看远程在不在线
		online = u.GetRemoteUserOnlineState(userId)
		if online {
			remote = true
		}
	}
	if online {
		if remote {
			// 远程在线玩家 为了简化实现流程 直接加载数据库临时档
			player = u.LoadTempOfflineUser(userId, false)
		} else {
			// 本地在线玩家
			player = u.GetOnlineUser(userId)
		}
	} else {
		// 全服离线玩家
		player = u.LoadTempOfflineUser(userId, false)
	}
	return player, online, remote
}

// 离线玩家相关操作
//
// 用于跨服添加好友、跨服多人申请、查询离线玩家档等需要临时操作离线档的场景
// 加载流程：先查Redis（绝大多数活跃玩家在缓存）→ 不存在则查DB → 写回Redis
// 标记 DbState=DbDelete 表示是"临时占位" 主循环UserCopyAndSave轮询时会跳过保存
//
// 关键约束：调用 LoadTempOfflineUser 取档后 修改并保存必须配套调 SaveTempOfflineUser
// 否则分布式锁不会释放 该玩家在其他GS会被锁住

// LoadTempOfflineUser 加载离线玩家档（含远程在线玩家场景）
// lock=true 加分布式锁（修改场景必须true）lock=false 仅读不锁（如查询场景）
// 正常情况Redis命中 速度较快可在主循环同步阻塞调用 极少数情况走DB回源
// TODO 防止恶意攻击造成redis缓存穿透
func (u *UserManager) LoadTempOfflineUser(userId uint32, lock bool) *model.Player {
	if userId < PlayerBaseUid || userId > MaxPlayerBaseUid {
		logger.Error("try to load a not exist uid, uid: %v", userId)
		return nil
	}
	player := u.GetOnlineUser(userId)
	if player != nil {
		logger.Error("not allow get a online player as offline player, uid: %v", userId)
		return nil
	}
	if lock {
		if !config.GetConfig().Hk4e.StandaloneModeEnable {
			// 加离线玩家数据分布式锁
			ok := u.db.DistLockSync(userId)
			if !ok {
				logger.Error("lock redis offline player data error, uid: %v", userId)
				return nil
			}
		}
	}
	player = u.LoadUserFromRedisSync(userId)
	if player == nil {
		// 玩家可能不存在于redis 尝试从db查询出来然后写入redis
		// 大多数情况下活跃玩家都在redis 所以不会走到下面
		// TODO 防止恶意攻击造成redis缓存穿透
		startTime := time.Now().UnixNano()
		player, _ = u.LoadUserFromDbSync(userId)
		endTime := time.Now().UnixNano()
		costTime := endTime - startTime
		logger.Info("try to load player from db sync in game main loop, cost time: %v ns", costTime)
		if player == nil {
			// 玩家根本就不存在
			logger.Error("try to load a not exist player from db, uid: %v", userId)
			return nil
		}
		u.SaveUserToRedisSync(player)
	}
	u.ChangeUserDbState(player, model.DbDelete)
	u.playerMap[player.PlayerId] = player
	return player
}

// SaveTempOfflineUser 保存临时离线玩家
// 如果调用LoadTempOfflineUser获取了离线玩家数据 则必须在逻辑完成后立即调用此函数回写并解锁
func (u *UserManager) SaveTempOfflineUser(player *model.Player) {
	if player.PlayerId < PlayerBaseUid || player.PlayerId > MaxPlayerBaseUid {
		logger.Error("try to save a not exist uid, uid: %v", player.PlayerId)
		return
	}
	// 主协程同步写入redis
	u.SaveUserToRedisSync(player)
	// 另一个协程异步的写回db
	playerData, err := msgpack.Marshal(player)
	if err != nil {
		logger.Error("marshal player data error: %v", err)
		if !config.GetConfig().Hk4e.StandaloneModeEnable {
			// 解离线玩家数据分布式锁
			u.db.DistUnlock(player.PlayerId)
		}
		return
	}
	go func() {
		if !config.GetConfig().Hk4e.StandaloneModeEnable {
			defer func() {
				// 解离线玩家数据分布式锁
				u.db.DistUnlock(player.PlayerId)
			}()
		}
		playerCopy := new(model.Player)
		err := msgpack.Unmarshal(playerData, playerCopy)
		if err != nil {
			logger.Error("unmarshal player data error: %v", err)
			return
		}
		playerCopy.DbState = player.DbState
		u.SaveUserToDbSync(playerCopy)
	}()
}

// db和redis相关操作

func (u *UserManager) GetSaveUserChan() chan *SaveUserData {
	return u.saveUserChan
}

// SaveUserData 主循环→保存协程的数据包
// insertPlayerList/updatePlayerList 是已经序列化好的msgpack字节数据 保存协程只负责写库
// exitSave=true 表示是停服时的最后一次保存 写完会通知 EXIT_SAVE_FIN_CHAN
type SaveUserData struct {
	insertPlayerList [][]byte
	updatePlayerList [][]byte
	exitSave         bool
}

// saveUserHandle 启动两个常驻goroutine
//  1. 定时触发器：每分钟向主循环投递 RunUserCopyAndSave 事件 主循环执行 UserCopyAndSave 序列化数据
//  2. 实际写库器：从 saveUserChan 接收已序列化的数据 同步写DB+Redis
//
// 解耦"序列化"（主循环单线程内）和"写库"（异步goroutine）防止DB卡死整个GS
func (u *UserManager) saveUserHandle() {
	go func() {
		ticker := time.NewTicker(time.Minute)
		for {
			<-ticker.C
			// 保存玩家数据
			LOCAL_EVENT_MANAGER.GetLocalEventChan() <- &LocalEvent{
				EventId: RunUserCopyAndSave,
			}
		}
	}()
	go func() {
		for {
			saveUserData := <-u.saveUserChan
			insertPlayerList := make([]*model.Player, 0)
			updatePlayerList := make([]*model.Player, 0)
			setPlayerList := make([]*model.Player, 0)
			for _, playerData := range saveUserData.insertPlayerList {
				player := new(model.Player)
				err := msgpack.Unmarshal(playerData, player)
				if err != nil {
					logger.Error("unmarshal player data error: %v", err)
					continue
				}
				insertPlayerList = append(insertPlayerList, player)
				setPlayerList = append(setPlayerList, player)
			}
			for _, playerData := range saveUserData.updatePlayerList {
				player := new(model.Player)
				err := msgpack.Unmarshal(playerData, player)
				if err != nil {
					logger.Error("unmarshal player data error: %v", err)
					continue
				}
				updatePlayerList = append(updatePlayerList, player)
				setPlayerList = append(setPlayerList, player)
			}
			u.SaveUserListToDbSync(insertPlayerList, updatePlayerList)
			u.SaveUserListToRedisSync(setPlayerList)
			if saveUserData.exitSave {
				// 停服落地玩家数据完毕 通知APP主协程关闭程序
				EXIT_SAVE_FIN_CHAN <- true
			}
		}
	}()
}

const (
	UserCopyGoroutineLimit = 4 // 序列化并发度 每批最多4个玩家档同时msgpack
)

// PlayerLastSaveTimeSortList 按 LastSaveTime 升序排序 优先保存最久未存档的玩家
type PlayerLastSaveTimeSortList []*model.Player

func (p PlayerLastSaveTimeSortList) Len() int {
	return len(p)
}

func (p PlayerLastSaveTimeSortList) Less(i, j int) bool {
	return p[i].LastSaveTime < p[j].LastSaveTime
}

func (p PlayerLastSaveTimeSortList) Swap(i, j int) {
	p[i], p[j] = p[j], p[i]
}

// UserCopyAndSave 主循环执行的"序列化阶段" 由 LocalEvent RunUserCopyAndSave/ExitRunUserCopyAndSave 触发
// 流程：按LastSaveTime排序所有在线玩家 → 分批4并发msgpack序列化 → 装包后投递给saveUserChan
// **关键性能保护：单次主循环执行总耗时上限10ms 超时直接中止**
//   - 4并发是 UserCopyGoroutineLimit 实测每个玩家档msgpack约2-5ms
//   - 玩家多时这一轮存不完没关系 下一分钟继续
//   - 排序确保不会"饿死"某些玩家（每次都从最久未存的开始）
//
// exitSave=true 是停服情况 不限时全部保存 写完通知 EXIT_SAVE_FIN_CHAN 让进程退出
func (u *UserManager) UserCopyAndSave(exitSave bool) {
	startTime := time.Now().UnixNano()
	playerList := make(PlayerLastSaveTimeSortList, 0)
	for _, player := range u.GetAllOnlineUserList() {
		if player.PlayerId < PlayerBaseUid {
			continue
		}
		if player.NotSave {
			continue
		}
		playerList = append(playerList, player)
	}
	sort.Stable(playerList)
	// 拷贝一份数据避免并发访问
	insertPlayerList := make([][]byte, 0)
	updatePlayerList := make([][]byte, 0)
	saveCount := 0
	times := len(playerList) / UserCopyGoroutineLimit
	if times == 0 && len(playerList) > 0 {
		times = 1
	}
	for index := 0; index < times; index++ {
		totalCostTime := time.Now().UnixNano() - startTime
		if totalCostTime > time.Millisecond.Nanoseconds()*10 {
			// 总耗时超过10ms就中止本轮保存
			logger.Info("user copy loop overtime exit, total cost time: %v ns", totalCostTime)
			break
		}
		// 分批次并发序列化玩家数据
		oncePlayerListEndIndex := 0
		if index < times-1 {
			oncePlayerListEndIndex = (index + 1) * UserCopyGoroutineLimit
		} else {
			oncePlayerListEndIndex = len(playerList)
		}
		oncePlayerList := playerList[index*UserCopyGoroutineLimit : oncePlayerListEndIndex]
		var playerDataMapLock sync.Mutex
		playerDataMap := make(map[uint32][]byte)
		var wg sync.WaitGroup
		for _, player := range oncePlayerList {
			wg.Add(1)
			go func(player *model.Player) {
				defer func() {
					wg.Done()
				}()
				playerData, err := msgpack.Marshal(player)
				if err != nil {
					logger.Error("marshal player data error: %v", err)
					return
				}
				playerDataMapLock.Lock()
				playerDataMap[player.PlayerId] = playerData
				playerDataMapLock.Unlock()
			}(player)
		}
		wg.Wait()
		for _, player := range oncePlayerList {
			playerData, exist := playerDataMap[player.PlayerId]
			if !exist {
				continue
			}
			switch player.DbState {
			case model.DbNone:
				break
			case model.DbInsert:
				insertPlayerList = append(insertPlayerList, playerData)
				player.DbState = model.DbNormal
				player.LastSaveTime = uint32(time.Now().UnixMilli())
				saveCount++
			case model.DbDelete:
				u.DeleteUser(player.PlayerId)
			case model.DbNormal:
				updatePlayerList = append(updatePlayerList, playerData)
				player.LastSaveTime = uint32(time.Now().UnixMilli())
				saveCount++
			}
		}
	}
	saveUserData := &SaveUserData{
		insertPlayerList: insertPlayerList,
		updatePlayerList: updatePlayerList,
		exitSave:         exitSave,
	}
	u.GetSaveUserChan() <- saveUserData
	endTime := time.Now().UnixNano()
	costTime := endTime - startTime
	logger.Info("run save user copy cost time: %v ns, save user count: %v", costTime, saveCount)
}

// LoadUserFromDbSync 从DB按uid查询玩家档（GORM/MongoDB自动选）
// 阻塞IO 仅在goroutine内调用
func (u *UserManager) LoadUserFromDbSync(userId uint32) (*model.Player, error) {
	player, err := u.db.QueryPlayerById(userId)
	if err != nil {
		logger.Error("query player error: %v", err)
		return nil, err
	}
	return player, nil
}

// SaveUserToDbSync 单玩家落库 按 DbState 决定 Insert/Update（DbDelete也走Update防止数据丢失）
// 阻塞IO 仅在goroutine内调用
func (u *UserManager) SaveUserToDbSync(player *model.Player) {
	if player.DbState == model.DbInsert {
		err := u.db.InsertPlayer(player)
		if err != nil {
			logger.Error("insert player error: %v", err)
			return
		}
	} else if player.DbState == model.DbNormal || player.DbState == model.DbDelete {
		err := u.db.UpdatePlayer(player)
		if err != nil {
			logger.Error("update player error: %v", err)
			return
		}
	} else {
		logger.Error("invalid player db state: %v", player.DbState)
	}
}

// SaveUserListToDbSync 批量落库 提高吞吐 由 saveUserHandle 的写库goroutine调用
func (u *UserManager) SaveUserListToDbSync(insertPlayerList []*model.Player, updatePlayerList []*model.Player) {
	err := u.db.InsertPlayerList(insertPlayerList)
	if err != nil {
		logger.Error("insert player list error: %v", err)
		return
	}
	err = u.db.UpdatePlayerList(updatePlayerList)
	if err != nil {
		logger.Error("update player list error: %v", err)
		return
	}
	logger.Info("save user finish, insert user count: %v, update user count: %v", len(insertPlayerList), len(updatePlayerList))
}

// LoadUserChatMsgFromDbSync 加载玩家全部历史聊天记录 按对话方uid分组
// 每对私聊保留最新 MaxMsgListLen 条 sequence从101开始递增（客户端用sequence增量拉取）
// 登录时调用 仅查DB不查Redis（聊天记录不缓存）
func (u *UserManager) LoadUserChatMsgFromDbSync(userId uint32) map[uint32][]*model.ChatMsg {
	chatMsgMap := make(map[uint32][]*model.ChatMsg)
	chatMsgList, err := u.db.QueryChatMsgListByUid(userId)
	if err != nil {
		logger.Error("query chat msg list error: %v", err)
		return chatMsgMap
	}
	for _, chatMsg := range chatMsgList {
		otherUid := uint32(0)
		if chatMsg.Uid == userId {
			otherUid = chatMsg.ToUid
		} else if chatMsg.ToUid == userId {
			otherUid = chatMsg.Uid
		} else {
			continue
		}
		msgList, exist := chatMsgMap[otherUid]
		if !exist {
			msgList = make([]*model.ChatMsg, 0)
		}
		msgList = append(msgList, chatMsg)
		chatMsgMap[otherUid] = msgList
	}
	for otherUid, msgList := range chatMsgMap {
		if len(msgList) > MaxMsgListLen {
			msgList = msgList[len(msgList)-MaxMsgListLen:]
		}
		for index, chatMsg := range msgList {
			chatMsg.Sequence = uint32(index) + 101
		}
		chatMsgMap[otherUid] = msgList
	}
	return chatMsgMap
}

func (u *UserManager) SaveUserChatMsgToDbSync(chatMsg *model.ChatMsg) {
	err := u.db.InsertChatMsg(chatMsg)
	if err != nil {
		logger.Error("insert chat msg error: %v", err)
		return
	}
}

func (u *UserManager) ReadUserChatMsgToDbSync(uid uint32, targetUid uint32) {
	err := u.db.UpdateChatMsgByUidAndToUidActionRead(uid, targetUid)
	if err != nil {
		logger.Error("read chat msg error: %v", err)
		return
	}
}

func (u *UserManager) DeleteUserAllChatMsgToDbSync(uid uint32) {
	err := u.db.DeleteUpdateChatMsgByUid(uid)
	if err != nil {
		logger.Error("delete chat msg error: %v", err)
		return
	}
}

func (u *UserManager) LoadUserFromRedisSync(userId uint32) *model.Player {
	if config.GetConfig().Hk4e.StandaloneModeEnable {
		return nil
	}
	player := u.db.GetRedisPlayer(userId)
	return player
}

func (u *UserManager) SaveUserToRedisSync(player *model.Player) {
	if config.GetConfig().Hk4e.StandaloneModeEnable {
		return
	}
	u.db.SetRedisPlayer(player)
}

func (u *UserManager) SaveUserListToRedisSync(setPlayerList []*model.Player) {
	if config.GetConfig().Hk4e.StandaloneModeEnable {
		return
	}
	u.db.SetRedisPlayerList(setPlayerList)
}

// AsyncWriteDb 通用异步DB写入入口 把任意闭包投递到异步队列
// 用于聊天记录、邮件等不与玩家档绑定的小事务（玩家档定时存有专门通道）
// 调用方传入闭包通过 u 参数访问DB 闭包内执行的IO不会卡主循环
func (u *UserManager) AsyncWriteDb(fn func(u *UserManager)) {
	u.asyncWriteDbChan <- fn
}

// asyncWriteDbHandle 异步DB写入队列消费者 单goroutine顺序执行投递的闭包
func (u *UserManager) asyncWriteDbHandle() {
	go func() {
		for {
			fn := <-u.asyncWriteDbChan
			fn(u)
		}
	}()
}
