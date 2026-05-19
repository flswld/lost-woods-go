package game

import (
	"math"
	"reflect"
	"strings"
	"time"

	"hk4e/common/constant"
	"hk4e/gs/model"
	"hk4e/protocol/cmd"
	"hk4e/protocol/proto"

	"github.com/flswld/halo/logger"
	pb "google.golang.org/protobuf/proto"
)

// PingReq 客户端心跳请求
// 维护 LastKeepaliveTime（onUserTickMinute 用它检查超时60s未心跳则踢人）
// 同时校验客户端时钟与服务端偏差 偏差>600s只打日志不踢（仅作排查线索）
func (g *Game) PingReq(player *model.Player, payloadMsg pb.Message) {
	req := payloadMsg.(*proto.PingReq)

	player.ClientTime = req.ClientTime
	now := uint32(time.Now().Unix())
	// 客户端与服务器时间相差太过严重
	if math.Abs(math.Max(float64(now), float64(player.ClientTime))-math.Min(float64(now), float64(player.ClientTime))) > 600.0 {
		logger.Error("abs of client time and server time above 600s, clientTime: %v, uid: %v", player.ClientTime, player.PlayerId)
	}
	player.LastKeepaliveTime = now

	g.SendMsg(cmd.PingRsp, player.PlayerId, player.ClientSeq, &proto.PingRsp{
		ClientTime: req.ClientTime,
		Seq:        req.Seq,
	})
}

// TowerAllDataReq 深境螺旋全部数据请求
// 当前未实现深渊玩法 返回固定假数据让客户端不报错（只能进塔界面但里面没内容）
func (g *Game) TowerAllDataReq(player *model.Player, payloadMsg pb.Message) {
	towerAllDataRsp := &proto.TowerAllDataRsp{
		TowerScheduleId:        29,
		TowerFloorRecordList:   []*proto.TowerFloorRecord{{FloorId: 1001}},
		CurLevelRecord:         &proto.TowerCurLevelRecord{IsEmpty: true},
		NextScheduleChangeTime: 4294967295,
		FloorOpenTimeMap: map[uint32]uint32{
			1024: 1630486800,
			1025: 1630486800,
			1026: 1630486800,
			1027: 1630486800,
		},
		ScheduleStartTime: 1630486800,
	}
	g.SendMsg(cmd.TowerAllDataRsp, player.PlayerId, player.ClientSeq, towerAllDataRsp)
}

// ClientRttNotify 由Gate上报的玩家网络往返时延
// 仅记录到 player.ClientRTT 字段 由 WorldPlayerRTTNotify 周期性广播给多人世界其他玩家显示
func (g *Game) ClientRttNotify(userId uint32, clientRtt uint32) {
	player := USER_MANAGER.GetOnlineUser(userId)
	if player == nil {
		logger.Error("player is nil, uid: %v", userId)
		return
	}
	// logger.Debug("client rtt notify, uid: %v, rtt: %v", userId, clientRtt)
	player.ClientRTT = clientRtt
}

// ServerAnnounceNotify 全服公告（屏幕中央滚动条文字）
// 1秒持续期 1次频率 GM命令 ServerAnnounce 调用
func (g *Game) ServerAnnounceNotify(announceId uint32, announceMsg string) {
	for _, onlinePlayer := range USER_MANAGER.GetAllOnlineUserList() {
		now := uint32(time.Now().Unix())
		serverAnnounceNotify := &proto.ServerAnnounceNotify{
			AnnounceDataList: []*proto.AnnounceData{{
				ConfigId:              announceId,
				BeginTime:             now + 1,
				EndTime:               now + 2,
				CenterSystemText:      announceMsg,
				CenterSystemFrequency: 1,
			}},
		}
		g.SendMsg(cmd.ServerAnnounceNotify, onlinePlayer.PlayerId, 0, serverAnnounceNotify)
	}
}

// ServerAnnounceRevokeNotify 撤销之前发过的公告
func (g *Game) ServerAnnounceRevokeNotify(announceId uint32) {
	for _, onlinePlayer := range USER_MANAGER.GetAllOnlineUserList() {
		serverAnnounceRevokeNotify := &proto.ServerAnnounceRevokeNotify{
			ConfigIdList: []uint32{announceId},
		}
		g.SendMsg(cmd.ServerAnnounceRevokeNotify, onlinePlayer.PlayerId, 0, serverAnnounceRevokeNotify)
	}
}

// ToTheMoonEnterSceneReq 客户端进入场景时上报的 ttm（to-the-moon）数据 服务端不处理仅回空响应
func (g *Game) ToTheMoonEnterSceneReq(player *model.Player, payloadMsg pb.Message) {
	logger.Debug("player ttm enter scene, uid: %v", player.PlayerId)
	req := payloadMsg.(*proto.ToTheMoonEnterSceneReq)
	_ = req
	g.SendMsg(cmd.ToTheMoonEnterSceneRsp, player.PlayerId, player.ClientSeq, new(proto.ToTheMoonEnterSceneRsp))
}

// PathfindingEnterSceneReq 客户端进入场景时的寻路数据请求 服务端无寻路逻辑空回复
func (g *Game) PathfindingEnterSceneReq(player *model.Player, payloadMsg pb.Message) {
	logger.Debug("player pf enter scene, uid: %v", player.PlayerId)
	req := payloadMsg.(*proto.PathfindingEnterSceneReq)
	_ = req
	g.SendMsg(cmd.PathfindingEnterSceneRsp, player.PlayerId, player.ClientSeq, new(proto.PathfindingEnterSceneRsp))
}

// QueryPathReq 客户端寻路查询（怪物/NPC按server-side寻路移动时使用）
// 当前简化实现：直接返回起点→终点的直线路径 不实际调用 navmesh.CalculatePath
func (g *Game) QueryPathReq(player *model.Player, payloadMsg pb.Message) {
	req := payloadMsg.(*proto.QueryPathReq)
	queryPathRsp := &proto.QueryPathRsp{
		QueryId:     req.QueryId,
		QueryStatus: proto.QueryPathRsp_STATUS_SUCC,
		Corners:     []*proto.Vector{req.SourcePos, req.DestinationPos[0]},
	}
	g.SendMsg(cmd.QueryPathRsp, player.PlayerId, player.ClientSeq, queryPathRsp)
}

// ObstacleModifyNotify 客户端动态障碍物变更通知（如砍倒树木导致地形变化）
// 服务端不做处理（导航系统未深度集成动态避障）
func (g *Game) ObstacleModifyNotify(player *model.Player, payloadMsg pb.Message) {
	ntf := payloadMsg.(*proto.ObstacleModifyNotify)
	_ = ntf
	// logger.Debug("ObstacleModifyNotify: %v, uid: %v", ntf, player.PlayerId)
}

// WorldPlayerRTTNotify 多人世界的玩家网络延迟广播 由 onTickSecond 每秒触发
// 客户端用这些 RTT 在玩家头顶显示对方的网络延迟
func (g *Game) WorldPlayerRTTNotify(world *World) {
	ntf := &proto.WorldPlayerRTTNotify{
		PlayerRttList: make([]*proto.PlayerRTTInfo, 0),
	}
	for _, worldPlayer := range world.GetAllPlayer() {
		playerRTTInfo := &proto.PlayerRTTInfo{Uid: worldPlayer.PlayerId, Rtt: worldPlayer.ClientRTT}
		ntf.PlayerRttList = append(ntf.PlayerRttList, playerRTTInfo)
	}
	g.SendToWorldA(world, cmd.WorldPlayerRTTNotify, 0, ntf, 0)
}

// WorldPlayerLocationNotify 多人世界中所有玩家的坐标朝向广播 onTick5Second 触发
// 客户端用这个数据在小地图上标记其他玩家位置 AI世界为了反作弊不暴露真实坐标 全置零
func (g *Game) WorldPlayerLocationNotify(world *World) {
	ntf := &proto.WorldPlayerLocationNotify{
		PlayerWorldLocList: make([]*proto.PlayerWorldLocationInfo, 0),
	}
	for _, worldPlayer := range world.GetAllPlayer() {
		pos := g.GetPlayerPos(worldPlayer)
		rot := g.GetPlayerRot(worldPlayer)
		playerWorldLocationInfo := &proto.PlayerWorldLocationInfo{
			SceneId: worldPlayer.GetSceneId(),
			PlayerLoc: &proto.PlayerLocationInfo{
				Uid: worldPlayer.PlayerId,
				Pos: &proto.Vector{X: float32(pos.X), Y: float32(pos.Y), Z: float32(pos.Z)},
				Rot: &proto.Vector{X: float32(rot.X), Y: float32(rot.Y), Z: float32(rot.Z)},
			},
		}

		if WORLD_MANAGER.IsAiWorld(world) {
			playerWorldLocationInfo.PlayerLoc.Pos = new(proto.Vector)
			playerWorldLocationInfo.PlayerLoc.Rot = new(proto.Vector)
		}

		ntf.PlayerWorldLocList = append(ntf.PlayerWorldLocList, playerWorldLocationInfo)
	}
	g.SendToWorldA(world, cmd.WorldPlayerLocationNotify, 0, ntf, 0)
}

// ScenePlayerLocationNotify 场景内玩家+载具坐标广播 比 WorldPlayerLocation 多了载具信息
// 客户端用这个数据更新场景内其他玩家和载具的实时位置
func (g *Game) ScenePlayerLocationNotify(world *World) {
	for _, scene := range world.GetAllScene() {
		ntf := &proto.ScenePlayerLocationNotify{
			SceneId:        scene.GetId(),
			PlayerLocList:  make([]*proto.PlayerLocationInfo, 0),
			VehicleLocList: make([]*proto.VehicleLocationInfo, 0),
		}
		for _, scenePlayer := range scene.GetAllPlayer() {
			pos := g.GetPlayerPos(scenePlayer)
			rot := g.GetPlayerRot(scenePlayer)
			// 玩家位置
			playerLocationInfo := &proto.PlayerLocationInfo{
				Uid: scenePlayer.PlayerId,
				Pos: &proto.Vector{X: float32(pos.X), Y: float32(pos.Y), Z: float32(pos.Z)},
				Rot: &proto.Vector{X: float32(rot.X), Y: float32(rot.Y), Z: float32(rot.Z)},
			}

			if WORLD_MANAGER.IsAiWorld(world) {
				playerLocationInfo.Pos = new(proto.Vector)
				playerLocationInfo.Rot = new(proto.Vector)
			}

			ntf.PlayerLocList = append(ntf.PlayerLocList, playerLocationInfo)
			// 载具位置
			for _, entityId := range scenePlayer.VehicleInfo.CreateEntityIdMap {
				entity := scene.GetEntity(entityId)
				// 确保实体类型是否为载具
				gadgetVehicleEntity, ok := entity.(*GadgetVehicleEntity)
				if ok {
					vehicleLocationInfo := &proto.VehicleLocationInfo{
						Rot: &proto.Vector{
							X: float32(entity.GetRot().X),
							Y: float32(entity.GetRot().Y),
							Z: float32(entity.GetRot().Z),
						},
						EntityId: entity.GetId(),
						CurHp:    entity.GetFightProp()[constant.FIGHT_PROP_CUR_HP],
						OwnerUid: gadgetVehicleEntity.GetOwnerUid(),
						Pos: &proto.Vector{
							X: float32(entity.GetPos().X),
							Y: float32(entity.GetPos().Y),
							Z: float32(entity.GetPos().Z),
						},
						UidList:  make([]uint32, 0, len(gadgetVehicleEntity.GetMemberMap())),
						GadgetId: gadgetVehicleEntity.GetGadgetId(),
						MaxHp:    entity.GetFightProp()[constant.FIGHT_PROP_MAX_HP],
					}
					for _, uid := range gadgetVehicleEntity.GetMemberMap() {
						vehicleLocationInfo.UidList = append(vehicleLocationInfo.UidList, uid)
					}
					ntf.VehicleLocList = append(ntf.VehicleLocList, vehicleLocationInfo)
				}
			}
		}
		g.SendToSceneA(scene, cmd.ScenePlayerLocationNotify, 0, ntf, 0)
	}
}

// SceneTimeNotify 场景启动至今的累计时间 onTick10Second 触发
// 用于客户端的某些动画/特效同步需要场景时间作为时间基准
func (g *Game) SceneTimeNotify(world *World) {
	for _, scene := range world.GetAllScene() {
		for _, player := range scene.GetAllPlayer() {
			sceneTimeNotify := &proto.SceneTimeNotify{
				SceneId:   player.GetSceneId(),
				SceneTime: uint64(scene.GetSceneTime()),
			}
			g.SendMsg(cmd.SceneTimeNotify, player.PlayerId, 0, sceneTimeNotify)
		}
	}
}

// PlayerTimeNotify 玩家累计在线时间 + 服务器时间 onTick10Second 触发
// 客户端按这个数据校准本地时钟和在线时长统计
func (g *Game) PlayerTimeNotify(world *World) {
	for _, player := range world.GetAllPlayer() {
		playerTimeNotify := &proto.PlayerTimeNotify{
			IsPaused:   player.Pause,
			PlayerTime: uint64(player.TotalOnlineTime),
			ServerTime: uint64(time.Now().UnixMilli()),
		}
		g.SendMsg(cmd.PlayerTimeNotify, player.PlayerId, 0, playerTimeNotify)
	}
}

// PlayerGameTimeNotify 游戏时间通知（GameTime为玩家总在线时长秒数 客户端 mod 86400 换算成游戏内时间）
// 影响白天/黑夜、天气、NPC对话分支等
func (g *Game) PlayerGameTimeNotify(world *World) {
	for _, player := range world.GetAllPlayer() {
		playerGameTimeNotify := &proto.PlayerGameTimeNotify{
			GameTime: world.GetGameTime(),
			Uid:      player.PlayerId,
		}
		g.SendMsg(cmd.PlayerGameTimeNotify, player.PlayerId, 0, playerGameTimeNotify)
	}
}

// GmTalkReq 客户端开发版GM Talk输入框
// 支持两种格式：
//  1. @@FuncName(p1,p2,...) → 反射调用 GMCmd 类的 FuncName 方法（SystemFuncGM）
//  2. 普通文本                → 等同于私聊小可爱的命令（DevClientGM）
//
// 必须 CmdPerm >= GM 才能用 否则直接拒绝
func (g *Game) GmTalkReq(player *model.Player, payloadMsg pb.Message) {
	req := payloadMsg.(*proto.GmTalkReq)
	logger.Info("GmTalkReq: %v", req.Msg)

	commandMessageInput := COMMAND_MANAGER.GetCommandMessageInput()
	if strings.Contains(req.Msg, "@@") {
		commandText := req.Msg
		commandText = strings.ReplaceAll(commandText, "@@", "")
		commandText = strings.ReplaceAll(commandText, " ", "")
		beginIndex := strings.Index(commandText, "(")
		endIndex := strings.Index(commandText, ")")
		if beginIndex == 0 || beginIndex == -1 || endIndex == -1 || beginIndex >= endIndex {
			g.SendMsg(cmd.GmTalkRsp, player.PlayerId, player.ClientSeq, &proto.GmTalkRsp{Retmsg: "GM函数解析失败", Msg: req.Msg})
			return
		}
		// 判断玩家权限是否足够
		if CommandPerm(player.CmdPerm) < CommandPermGM {
			g.SendMsg(cmd.GmTalkRsp, player.PlayerId, player.ClientSeq, &proto.GmTalkRsp{Retmsg: "权限不足", Msg: req.Msg})
			return
		}
		funcName := commandText[:beginIndex]
		funcParam := commandText[beginIndex+1 : endIndex]
		paramList := make([]string, 0)
		if funcParam != "" {
			paramList = strings.Split(funcParam, ",")
		}
		commandMessageInput <- &CommandMessage{
			GMType:    SystemFuncGM,
			FuncName:  funcName,
			ParamList: paramList,
		}
	} else {
		commandMessageInput <- &CommandMessage{
			GMType:   DevClientGM,
			Executor: player,
			Text:     strings.ToLower(req.Msg),
		}
	}
	g.SendMsg(cmd.GmTalkRsp, player.PlayerId, player.ClientSeq, &proto.GmTalkRsp{Retmsg: "执行成功", Msg: req.Msg})
}

// PacketPropValue 把(key, value any)封装成proto.PropValue
// 数值大小经过类型适配 浮点走Fval分支 整数走Ival分支
// 用于 PlayerDataNotify/PlayerPropNotify 等的 PropMap 字段构建
func (g *Game) PacketPropValue(key uint32, value any) *proto.PropValue {
	propValue := new(proto.PropValue)
	propValue.Type = key
	switch value.(type) {
	case int:
		v := value.(int)
		propValue.Val = int64(v)
		propValue.Value = &proto.PropValue_Ival{Ival: int64(v)}
	case int8:
		v := value.(int8)
		propValue.Val = int64(v)
		propValue.Value = &proto.PropValue_Ival{Ival: int64(v)}
	case int16:
		v := value.(int16)
		propValue.Val = int64(v)
		propValue.Value = &proto.PropValue_Ival{Ival: int64(v)}
	case int32:
		v := value.(int32)
		propValue.Val = int64(v)
		propValue.Value = &proto.PropValue_Ival{Ival: int64(v)}
	case int64:
		v := value.(int64)
		propValue.Val = v
		propValue.Value = &proto.PropValue_Ival{Ival: v}
	case float32:
		v := value.(float32)
		propValue.Value = &proto.PropValue_Fval{Fval: v}
	case float64:
		v := value.(float64)
		propValue.Value = &proto.PropValue_Fval{Fval: float32(v)}
	case uint8:
		v := value.(uint8)
		propValue.Val = int64(v)
		propValue.Value = &proto.PropValue_Ival{Ival: int64(v)}
	case uint16:
		v := value.(uint16)
		propValue.Val = int64(v)
		propValue.Value = &proto.PropValue_Ival{Ival: int64(v)}
	case uint32:
		v := value.(uint32)
		propValue.Val = int64(v)
		propValue.Value = &proto.PropValue_Ival{Ival: int64(v)}
	case uint64:
		v := value.(uint64)
		propValue.Val = int64(v)
		propValue.Value = &proto.PropValue_Ival{Ival: int64(v)}
	default:
		logger.Error("unknown value type: %v, value: %v", reflect.TypeOf(value), value)
		return nil
	}
	return propValue
}

// GetPlayerPos 获取玩家实时位置 优先取活跃角色实体的pos（运行时坐标）
// 取不到时回退到 player.Pos（存档坐标 仅在保存时同步）
func (g *Game) GetPlayerPos(player *model.Player) *model.Vector {
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		return player.GetPos()
	}
	entity := world.GetPlayerActiveAvatarEntity(player)
	if entity == nil {
		return player.GetPos()
	}
	return entity.GetPos()
}

// GetPlayerRot 获取玩家实时朝向 取值规则与 GetPlayerPos 一致
func (g *Game) GetPlayerRot(player *model.Player) *model.Vector {
	world := WORLD_MANAGER.GetWorldById(player.WorldId)
	if world == nil {
		return player.GetRot()
	}
	entity := world.GetPlayerActiveAvatarEntity(player)
	if entity == nil {
		return player.GetRot()
	}
	return entity.GetRot()
}
