package game

// 玩家登录与离线流程
//
// 登录链：
//   1. PlayerLoginReq（来自Gate）→ 触发 USER_MANAGER.UserLoginLoad 异步从DB加载档案
//   2. 加载完成后 LOCAL_EVENT_MANAGER 推 UserLoginLoadFromDbFinish 回主循环
//   3. 主循环回调 OnLogin，完成内存初始化、注册tick、SELF切换、进入世界/场景
//   4. 新号需要先发 DoSetPlayerBornDataNotify 让客户端选主角，客户端回 SetPlayerBornDataReq 走出生流程
//
// 离线链：
//   1. Gate 检测连接断开 → MQ 推 UserOfflineNotify 给本服
//   2. ROUTE_MANAGER 转给 OnOffline → 移出世界、销毁tick、UserOfflineSave 异步保存档
//   3. 异步保存完成后 LOCAL_EVENT_MANAGER 推 UserOfflineSaveToDbFinish → OfflineUser 真正从内存删除

import (
	"strings"

	"hk4e/common/constant"
	"hk4e/common/region"
	"hk4e/gdconf"
	"hk4e/gs/model"
	"hk4e/pkg/object"
	"hk4e/protocol/cmd"
	"hk4e/protocol/proto"

	"github.com/flswld/halo/logger"
	pb "google.golang.org/protobuf/proto"
)

// PlayerLoginReq 玩家登录请求入口
// 此时玩家还未在内存（不能走doRoute的SELF切换路径），由ROUTE_MANAGER特殊路径直接调到这里
// 真正的加载工作由 USER_MANAGER.UserLoginLoad 起 goroutine 异步执行，加载完成后通过 LocalEvent 回调主循环 OnLogin
func (g *Game) PlayerLoginReq(userId uint32, clientSeq uint32, gateAppId string, payloadMsg pb.Message) {
	logger.Info("player login req, uid: %v, gateAppId: %v", userId, gateAppId)
	req := payloadMsg.(*proto.PlayerLoginReq)
	logger.Info("login data: %v", req)
	USER_MANAGER.UserLoginLoad(userId, clientSeq, gateAppId, req)
}

// SetPlayerBornDataReq 新玩家选主角后的出生数据设置（首次登录）
// 客户端在 DoSetPlayerBornDataNotify 之后回调，此时 IsBorn=false
// 完成后置 IsBorn=true 并发起进入场景流程（PlayerEnterSceneNotify → 客户端发起四步状态机）
func (g *Game) SetPlayerBornDataReq(player *model.Player, payloadMsg pb.Message) {
	req := payloadMsg.(*proto.SetPlayerBornDataReq)
	logger.Debug("avatar id: %v, nickname: %v", req.AvatarId, req.NickName)

	if player.IsBorn {
		logger.Error("player is already born, uid: %v", player.PlayerId)
		return
	}
	player.IsBorn = true

	// 主角只能是 10000005（空）或 10000007（荧）
	mainCharAvatarId := req.AvatarId
	if mainCharAvatarId != 10000005 && mainCharAvatarId != 10000007 {
		logger.Error("invalid main char avatar id: %v", mainCharAvatarId)
		return
	}
	player.NickName = req.NickName
	player.HeadImage = mainCharAvatarId // 默认头像就是主角图标

	dbAvatar := player.GetDbAvatar()
	dbAvatar.MainCharAvatarId = mainCharAvatarId
	// 添加选定的主角
	dbAvatar.AddAvatar(player, dbAvatar.MainCharAvatarId)
	// 添加主角初始武器（雪花id作为武器唯一guid）
	avatarDataConfig := gdconf.GetAvatarDataById(int32(dbAvatar.MainCharAvatarId))
	if avatarDataConfig == nil {
		logger.Error("get avatar data config is nil, avatarId: %v", dbAvatar.MainCharAvatarId)
		return
	}
	weaponId := uint64(g.snowflake.GenId())
	dbWeapon := player.GetDbWeapon()
	dbWeapon.AddWeapon(player, uint32(avatarDataConfig.InitialWeapon), weaponId)
	weapon := dbWeapon.GetWeaponById(weaponId)
	dbAvatar.WearWeapon(dbAvatar.MainCharAvatarId, weapon)
	// 主角添加进队伍（首次登录默认放第1队第1位）
	dbTeam := player.GetDbTeam()
	dbTeam.GetActiveTeam().SetAvatarIdList([]uint32{dbAvatar.MainCharAvatarId})
	// 计算主角初始面板（基础属性+武器加成+圣遗物加成）
	avatar := dbAvatar.GetAvatarById(dbAvatar.MainCharAvatarId)
	dbAvatar.UpdateAvatarFightProp(avatar)

	// 自动接受所有满足前置条件的任务（开场任务等）
	g.AcceptQuest(player, false)

	// 触发功能开放状态检查（按等级/任务条件解锁系统功能）
	g.TriggerOpenState(player.PlayerId)

	// 下发登录后必需的玩家数据通知（属性/背包/角色/任务/地图标记/小工具）
	g.LoginNotify(player.PlayerId, player.ClientSeq, player)

	// 创建玩家自己的单人世界并加入
	world := WORLD_MANAGER.CreateWorld(player)
	g.WorldAddPlayer(world, player)
	// 触发进入场景四步状态机
	player.SceneJump = true
	player.SceneLoadState = model.SceneNone
	player.SceneEnterReason = uint32(proto.EnterReason_ENTER_REASON_LOGIN)
	g.SendMsg(cmd.PlayerEnterSceneNotify, player.PlayerId, player.ClientSeq, g.PacketPlayerEnterSceneNotifyLogin(player))

	g.SendMsg(cmd.SetPlayerBornDataRsp, player.PlayerId, player.ClientSeq, new(proto.SetPlayerBornDataRsp))
}

// OnLogin 异步加载档案完成后的主循环回调
// 由 LOCAL_EVENT_MANAGER 在收到 UserLoginLoadFromDbFinish 事件时调用，运行在主循环 goroutine 内
// 参数 player 来自DB加载结果：nil 表示新玩家（要 CreatePlayer 注册），非nil 表示老玩家直接上线
// ok=false 表示加载失败（DB故障/分布式锁失败等），直接给客户端回 LOGIN_INIT_FAIL
func (g *Game) OnLogin(userId uint32, clientSeq uint32, gateAppId string, player *model.Player, req *proto.PlayerLoginReq, ok bool) {
	logger.Info("player login, uid: %v, ok: %v", userId, ok)
	if !ok {
		g.SendMsgToGate(cmd.PlayerLoginRsp, userId, clientSeq, gateAppId, &proto.PlayerLoginRsp{Retcode: int32(proto.Retcode_RET_LOGIN_INIT_FAIL)})
		return
	}

	// 新玩家注册：创建初始Player对象并标记为DbInsert以便首次落库
	if player == nil {
		logger.Info("reg new player, uid: %v", userId)
		player = g.CreatePlayer(userId)
		USER_MANAGER.ChangeUserDbState(player, model.DbInsert)
	}
	USER_MANAGER.OnlineUser(player)

	// 创建玩家级tick（100ms）和一个测试用的延迟100ms定时任务
	TICK_MANAGER.CreateUserGlobalTick(userId)
	TICK_MANAGER.CreateUserTimer(userId, UserTimerActionTest, 100, player.NickName)

	player.GateAppId = gateAppId

	// 切换全局SELF指针 后续handler通过SELF获取当前玩家上下文
	SELF = player

	// 初始化在线态数据（不持久化的字段）：guid计数器、AOI、转发器、GCG等
	player.InitOnlineData()
	// 解析客户端版本号字符串如"OSRELWin3.2.0_R11611027" → 320，影响EntityId编码方式
	clientVersion, _ := region.GetClientVersionByName(req.ChecksumClientVersion)
	player.ClientVersion = clientVersion
	player.ClientVersionStr = req.ChecksumClientVersion

	// 防呆：sceneId>100 视为非法存档（场景id一般是 3/4/5/...，超过100可能是脏数据），重置到蒙德城出生点
	if player.GetSceneId() > 100 {
		player.SceneId = 3
		player.Pos = &model.Vector{X: 2747, Y: 194, Z: -1719}
		player.Rot = &model.Vector{X: 0, Y: 307, Z: 0}
	}
	player.MpPos = new(model.Vector)
	player.MpRot = new(model.Vector)

	// 老存档没有任务数据时（早期版本兼容）补加任务并重置出生点
	if player.DbQuest == nil {
		player.SceneId = 3
		player.Pos = &model.Vector{X: 2747, Y: 194, Z: -1719}
		player.Rot = &model.Vector{X: 0, Y: 307, Z: 0}
		g.AcceptQuest(player, false)
	}

	// LostWoods
	if strings.Contains(req.ChecksumClientVersion, "LostWoods") {
		player.Pos = &model.Vector{X: 500, Y: 10, Z: 500}
		player.Rot = &model.Vector{X: 0, Y: 0, Z: 0}
	}

	g.TriggerOpenState(userId)

	if player.IsBorn {
		// 已出生玩家：直接进世界
		g.LoginNotify(userId, clientSeq, player)
		if req.TargetUid != 0 {
			// 客户端登录请求里带了TargetUid 表示直接以多人Guest身份加入某个房主的世界
			hostPlayer := USER_MANAGER.GetOnlineUser(req.TargetUid)
			if hostPlayer != nil {
				g.JoinOtherWorld(player, hostPlayer)
			} else {
				logger.Error("player is nil, uid: %v", req.TargetUid)
			}
		} else {
			// 没指定TargetUid 创建自己的单人世界并加入
			world := WORLD_MANAGER.CreateWorld(player)
			g.WorldAddPlayer(world, player)
			// 触发进入场景四步状态机（EnterReason 标记为登录）
			player.SceneJump = true
			player.SceneLoadState = model.SceneNone
			player.SceneEnterReason = uint32(proto.EnterReason_ENTER_REASON_LOGIN)
			g.SendMsg(cmd.PlayerEnterSceneNotify, userId, clientSeq, g.PacketPlayerEnterSceneNotifyLogin(player))
		}
	} else {
		// 新号未出生：让客户端弹主角选择界面 → 客户端回 SetPlayerBornDataReq
		g.SendMsg(cmd.DoSetPlayerBornDataNotify, userId, clientSeq, new(proto.DoSetPlayerBornDataNotify))
	}

	// 回复登录响应（默认 hk4e_global 海外 mihoyo CPS 美区，私服固定值）
	rsp := &proto.PlayerLoginRsp{
		IsUseAbilityHash:        true,
		AbilityHashCode:         0,
		IsEnableClientHashDebug: true,
		IsScOpen:                false,
		ScInfo:                  []byte{},
		TotalTickTime:           0.0,
		GameBiz:                 "hk4e_global",
		RegisterCps:             "mihoyo",
		CountryCode:             "US",
		Birthday:                "2000-01-01",
	}
	g.SendMsg(cmd.PlayerLoginRsp, userId, clientSeq, rsp)

	// SELF 用完置空 防止异步goroutine误用
	SELF = nil
}

// CreatePlayer 创建新玩家初始档（仅在内存）
// 默认昵称"旅行者"、头像默认为荧（10000007）、初始场景为蒙德城（sceneId=3）
// PropMap 设置所有初始属性值（等级1/原石0/树脂160/最大体力10000等）
// OpenStateMap 按 OpenStateData 配置中的 DefaultOpen 字段批量初始化
// SceneBlockMap 按出生点附近的 block AOI 预创建空白存档块
func (g *Game) CreatePlayer(userId uint32) *model.Player {
	player := new(model.Player)
	player.IsBorn = false // 等待客户端发SetPlayerBornDataReq选主角后才置true
	player.PlayerId = userId
	player.NickName = "旅行者"
	player.Signature = ""
	player.HeadImage = 10000007 // 默认头像 = 荧
	player.PropMap = make(map[uint32]uint32)
	player.OpenStateMap = make(map[uint32]uint32)
	player.ChatMsgMap = make(map[uint32][]*model.ChatMsg)
	player.SceneId = 3 // 提瓦特

	player.PropMap[constant.PLAYER_PROP_PLAYER_WORLD_LEVEL] = 0
	player.PropMap[constant.PLAYER_PROP_CUR_PERSIST_STAMINA] = 10000 // 当前体力(实际显示值=值/100=100体力)
	player.PropMap[constant.PLAYER_PROP_CUR_TEMPORARY_STAMINA] = 0
	player.PropMap[constant.PLAYER_PROP_PLAYER_LEVEL] = 1
	player.PropMap[constant.PLAYER_PROP_PLAYER_EXP] = 0
	player.PropMap[constant.PLAYER_PROP_PLAYER_HCOIN] = 0           // 原石
	player.PropMap[constant.PLAYER_PROP_PLAYER_SCOIN] = 0           // 摩拉
	player.PropMap[constant.PLAYER_PROP_PLAYER_MP_SETTING_TYPE] = 2 // 多人模式 0禁止 1直接加入 2需要申请
	player.PropMap[constant.PLAYER_PROP_PLAYER_RESIN] = 160         // 树脂初始满（160）
	player.PropMap[constant.PLAYER_PROP_PLAYER_WAIT_SUB_HCOIN] = 0
	player.PropMap[constant.PLAYER_PROP_PLAYER_WAIT_SUB_SCOIN] = 0
	player.PropMap[constant.PLAYER_PROP_PLAYER_MCOIN] = 0
	player.PropMap[constant.PLAYER_PROP_PLAYER_WAIT_SUB_MCOIN] = 0
	player.PropMap[constant.PLAYER_PROP_PLAYER_LEGENDARY_KEY] = 0
	player.PropMap[constant.PLAYER_PROP_CUR_CLIMATE_METER] = 0
	player.PropMap[constant.PLAYER_PROP_CUR_CLIMATE_TYPE] = 0
	player.PropMap[constant.PLAYER_PROP_CUR_CLIMATE_AREA_ID] = 0
	player.PropMap[constant.PLAYER_PROP_CUR_CLIMATE_AREA_CLIMATE_TYPE] = 0
	player.PropMap[constant.PLAYER_PROP_PLAYER_WORLD_LEVEL_LIMIT] = 0
	player.PropMap[constant.PLAYER_PROP_PLAYER_WORLD_LEVEL_ADJUST_CD] = 0
	player.PropMap[constant.PLAYER_PROP_PLAYER_LEGENDARY_DAILY_TASK_NUM] = 0
	player.PropMap[constant.PLAYER_PROP_PLAYER_HOME_COIN] = 0
	player.PropMap[constant.PLAYER_PROP_PLAYER_WAIT_SUB_HOME_COIN] = 0
	player.PropMap[constant.PLAYER_PROP_IS_AUTO_UNLOCK_SPECIFIC_EQUIP] = 0
	player.PropMap[constant.PLAYER_PROP_IS_SPRING_AUTO_USE] = 1 // 七天神像靠近自动回血
	player.PropMap[constant.PLAYER_PROP_SPRING_AUTO_USE_PERCENT] = 50
	player.PropMap[constant.PLAYER_PROP_IS_FLYABLE] = 0 // 初始禁飞 解锁风之翼任务后开启
	player.PropMap[constant.PLAYER_PROP_IS_WEATHER_LOCKED] = 0
	player.PropMap[constant.PLAYER_PROP_IS_GAME_TIME_LOCKED] = 0
	player.PropMap[constant.PLAYER_PROP_IS_TRANSFERABLE] = 1
	player.PropMap[constant.PLAYER_PROP_MAX_STAMINA] = 10000

	// 按OpenStateData配置自动开启默认开放的功能
	for _, openStateDataConfig := range gdconf.GetOpenStateDataMap() {
		if object.ConvInt64ToBool(int64(openStateDataConfig.DefaultOpen)) {
			player.OpenStateMap[uint32(openStateDataConfig.OpenStateId)] = 1
		}
	}

	// 出生点取场景Lua配置中的SceneConfig.BornPos/BornRot 取不到则用蒙德城硬编码兜底
	sceneLuaConfig := gdconf.GetSceneLuaConfigById(int32(player.GetSceneId()))
	if sceneLuaConfig != nil {
		bornPos := sceneLuaConfig.SceneConfig.BornPos
		bornRot := sceneLuaConfig.SceneConfig.BornRot
		player.Pos = &model.Vector{X: float64(bornPos.X), Y: float64(bornPos.Y), Z: float64(bornPos.Z)}
		player.Rot = &model.Vector{X: float64(bornRot.X), Y: float64(bornRot.Y), Z: float64(bornRot.Z)}
	} else {
		logger.Error("get scene lua config is nil, sceneId: %v, uid: %v", player.GetSceneId(), player.PlayerId)
		player.Pos = &model.Vector{X: 2747, Y: 194, Z: -1719}
		player.Rot = &model.Vector{X: 0, Y: 307, Z: 0}
	}

	// 按出生点附近的block AOI预创建空白存档块（玩家走到哪里加载到哪里）
	player.SceneBlockMap = make(map[uint32]*model.SceneBlock)
	sceneBlockAoi, exist := WORLD_MANAGER.GetSceneBlockAoiMap()[player.SceneId]
	if !exist {
		logger.Error("scene not exist in aoi, sceneId: %v", player.SceneId)
		return player
	}
	for _, blockAny := range sceneBlockAoi.GetObjectListByPos(float32(player.Pos.X), 0.0, float32(player.Pos.Z), 1) {
		block := blockAny.(*gdconf.Block)
		sceneBlock := &model.SceneBlock{
			Uid:           player.PlayerId,
			BlockId:       uint32(block.Id),
			SceneGroupMap: make(map[uint32]*model.SceneGroup),
			IsNew:         true, // 标记为新创建 SaveSceneBlockSync时走Insert而不是Update
		}
		player.SceneBlockMap[sceneBlock.BlockId] = sceneBlock
	}

	return player
}

// ServerAppidBindNotify Multi服告知本玩家绑定到的Multi appid
// 用于跨服反作弊数据收集时定位玩家归属的Multi服实例
func (g *Game) ServerAppidBindNotify(userId uint32, multiServerAppId string) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	logger.Debug("server appid bind notify, uid: %v, multiServerAppId: %v", userId, multiServerAppId)
	player.MultiServerAppId = multiServerAppId
}

// OnOffline 玩家离线主流程
// 由 ROUTE_MANAGER 在收到 UserOfflineNotify 时调用 也用于跨服迁移（changeGsInfo.IsChangeGs=true）和服务器停服踢人
// 流程：保存战斗属性 → 移出世界 → 销毁tick → 异步保存档 → 销毁GCG对局
// 玩家从内存的最终删除发生在异步保存完成后的 OfflineUser 回调
func (g *Game) OnOffline(userId uint32, gateAppId string, changeGsInfo *ChangeGsInfo) {
	logger.Info("player offline, uid: %v", userId)
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	if gateAppId != "" && gateAppId != player.GateAppId {
		logger.Error("recv user offline notify from other gate server, uid: %v, gateAppId: %v", userId, gateAppId)
		return
	}
	player.NetFreeze = true

	// 把每个角色的当前血量/能量等运行时战斗属性回写到持久化字段（FightPropMap → CurrHP/CurrEnergy）
	dbAvatar := player.GetDbAvatar()
	for _, avatar := range dbAvatar.GetAvatarMap() {
		dbAvatar.SaveOfflineFightProp(avatar)
	}

	// 从世界中移除玩家（多人世界如果是Guest会触发其他Guest离开通知）
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world != nil {
		g.WorldRemovePlayer(world, player)
	}

	// 销毁玩家级tick 防止后续误触发
	TICK_MANAGER.DestroyUserGlobalTick(userId)

	// 异步保存档案到DB+Redis 完成后通过LocalEvent回调OfflineUser真正从playerMap移除
	USER_MANAGER.UserOfflineSave(player, changeGsInfo)

	// 销毁正在进行的GCG对局
	game, exist := GCG_MANAGER.gameMap[player.GCGCurGameGuid]
	if exist {
		GCG_MANAGER.DestroyGame(game.guid)
	}
}

// LoginNotify 登录后下发玩家所需的全套基础数据通知
// 顺序敏感：玩家属性 → 背包重量 → 背包内容 → 角色 → 功能开放 → 任务 → 父任务 → 地图标记 → 小工具
// SetPlayerBornDataReq（首登）和 OnLogin（已出生）都会调用 客户端必须收到这些后才能正常进场景
func (g *Game) LoginNotify(userId uint32, clientSeq uint32, player *model.Player) {
	g.SendMsg(cmd.PlayerDataNotify, userId, clientSeq, g.PacketPlayerDataNotify(player))
	g.SendMsg(cmd.StoreWeightLimitNotify, userId, clientSeq, g.PacketStoreWeightLimitNotify())
	g.SendMsg(cmd.PlayerStoreNotify, userId, clientSeq, g.PacketPlayerStoreNotify(player))
	g.SendMsg(cmd.AvatarDataNotify, userId, clientSeq, g.PacketAvatarDataNotify(player))
	g.SendMsg(cmd.OpenStateUpdateNotify, userId, clientSeq, g.PacketOpenStateUpdateNotify(player))
	g.SendMsg(cmd.QuestListNotify, userId, clientSeq, g.PacketQuestListNotify(player))
	g.SendMsg(cmd.FinishedParentQuestNotify, userId, clientSeq, g.PacketFinishedParentQuestNotify(player))
	g.SendMsg(cmd.AllMarkPointNotify, userId, clientSeq, &proto.AllMarkPointNotify{MarkList: g.PacketMapMarkPointList(player)})
	g.SendMsg(cmd.AllWidgetDataNotify, userId, clientSeq, &proto.AllWidgetDataNotify{SlotList: g.PacketWidgetSlotDataList(player)})
	g.GCGLogin(player) // 发送GCG登录相关的通知包
}
