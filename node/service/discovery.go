package service

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"hk4e/common/config"
	"hk4e/common/mq"
	"hk4e/node/api"
	"hk4e/node/dao"
	"hk4e/pkg/random"

	"github.com/flswld/halo/logger"
)

// Node 服务发现 - 整个集群的注册中心
//
// 主要职责：
//   1. 服务实例注册（RegisterServer/CancelServer）：所有服务启动时来这里登记
//   2. 心跳保活（KeepaliveServer）：每 15s 一次 超时（30s）自动剔除
//   3. UID 分配（GetNextUid）：玩家首次登录时分配 起始 100000000 全局自增
//   4. EC2B 密钥管理：region_ec2b 全集群共享 dispatch/gate 都从这里拿
//   5. 玩家在线状态（globalGsOnlineMap）：跨服查询玩家所在 GS
//   6. 停服管理（StopServerInfo + IpAddrWhiteList）
//   7. 服务路由：GetServerAppId 给 gate 选最小负载 GS
//   8. AppId 分配：每个服务实例分配 8 字符随机 AppId
//   9. GsId 分配：GS 服务额外分配 1~MaxGsId 内的 GsId（用于 AI 玩家 uid 派生）
//
// **核心约束**：Node 必须**第一个启动**（其他服务依赖它）
//   单点故障：Node 挂了所有服务无法注册新连接（已注册的还能跑）
//   高可用部署需要主备但项目未实现

const (
	MaxGsId  = 1000      // GS 编号上限（理论上限 实际部署远小于此）
	UidBegin = 100000000 // 玩家 uid 起始值 1 亿
)

var _ api.DiscoveryNATSRPCServer = (*DiscoveryService)(nil)

// ServerInstance 单个服务实例的元数据
//
// 字段说明：
//   - serverType: 服务类型（GATE/GS/MULTI/ROBOT/DISPATCH）
//   - appId: 8 字符随机字符串 服务实例的全局唯一标识
//   - appVersion: 编译版本号 用于版本兼容判断
//   - gateServerKcpAddr/Port: 仅 GATE 有 客户端连过来的 KCP 地址
//   - gateServerMqAddr/Port: 仅 GATE 有 GS/Multi/Robot 连过来的 TCP 直连 MQ 地址
//   - gameVersionList: 仅 GATE 有 支持的客户端版本列表
//   - lastAliveTime: 最后心跳时间 超过 30s 视为死服
//   - gsId: 仅 GS 有 1~MaxGsId 范围 用于 AI 玩家 uid 派生（AiBaseUid + gsId）
//   - loadCount: 当前负载（GATE 是连接数 GS 是玩家数）选最小负载用
//   - dispatchCancel: 是否取消调度（停止接受新连接 但已有连接继续服务 用于优雅停服）
type ServerInstance struct {
	serverType        string   // 服务器类型
	appId             string   // appid
	appVersion        string   // app版本
	gateServerKcpAddr string   // 网关kcp地址
	gateServerKcpPort uint32   // 网关kcp端口
	gateServerMqAddr  string   // 网关tcp直连消息队列地址
	gateServerMqPort  uint32   // 网关tcp直连消息队列端口
	gameVersionList   []string // 网关支持的客户端协议版本
	lastAliveTime     int64    // 最后保活时间
	gsId              uint32   // 游戏服务器编号
	loadCount         uint32   // 负载数
	dispatchCancel    bool     // 是否取消调度
}

// StopServerInfo 停服信息
type StopServerInfo struct {
	stopServer      bool                // 是否停服
	startTime       uint32              // 停服开始时间
	endTime         uint32              // 停服结束时间
	ipAddrWhiteList map[string]struct{} // ip地址白名单
}

// DiscoveryService Node 主服务（natsrpc Server）
//
// serverInstanceMap 双层结构：
//
//	外层 key: 服务类型（GATE/GS/...）value: *sync.Map
//	内层 key: appId               value: *ServerInstance
//
// 使用 sync.Map 是因为多 goroutine 频繁读写：
//   - RegisterServer: 注册新实例（写）
//   - KeepaliveServer: 更新 lastAliveTime + loadCount（每 15s 写一次）
//   - GetServerAppId: 查询最小负载（频繁读）
//   - removeDeadServer: 后台 goroutine 周期清理（写）
type DiscoveryService struct {
	db                *dao.Dao             // 数据库访问对象（Region 表 + UID/Ec2b 持久化）
	regionEc2b        *random.Ec2b         // 区服密钥信息
	nextUid           uint32               // 自增uid（写操作要 atomic）
	serverInstanceMap map[string]*sync.Map // 全部服务器实例集合 双层 map
	serverAppIdMap    *sync.Map            // 全局 appId 唯一性检查表
	globalGsOnlineMap *sync.Map            // 全服玩家在线表 uid → gsAppid
	stopServerInfo    *StopServerInfo      // 停服信息
	messageQueue      *mq.MessageQueue     // 消息队列实例（接收 GS 的 OnlineStateChange 广播）
}

// NewDiscoveryService Node 启动入口
//
// 处理：
//  1. 从 DB 加载 Region 表（首次启动时自动创建）
//     · Region 包含全集群共享的 Ec2b/NextUid/StopServerInfo
//     · 持久化让 Node 重启后保留这些状态
//  2. 加载 ec2b 密钥（dispatch/gate 都从这里拿）
//  3. 初始化 5 种服务类型的 instMap
//  4. 启动 3 个后台 goroutine：
//     · removeDeadServer: 每秒检查 30s 无心跳的实例 自动剔除
//     · broadcastReceiver: 接收 GS 广播的玩家在线状态变更
//     · serverState: 每 60s 打印各服务实例统计日志
func NewDiscoveryService(db *dao.Dao, messageQueue *mq.MessageQueue) (*DiscoveryService, error) {
	r := new(DiscoveryService)
	r.db = db
	region, err := r.db.QueryRegion()
	if err != nil {
		logger.Error("load region from db error: %v", err)
		return nil, err
	}
	if region == nil {
		logger.Info("init region")
		stopServer := false
		if !config.GetConfig().Hk4e.StandaloneModeEnable {
			stopServer = true
		} else {
			stopServer = false
		}
		region = &dao.Region{
			Ec2bData:            random.NewEc2b().Bytes(),
			NextUid:             UidBegin,
			StopServer:          stopServer,
			StopServerStartTime: uint32(time.Now().Unix()),
			StopServerEndTime:   uint32(time.Now().AddDate(10, 0, 0).Unix()),
			IpAddrWhiteList:     make([]string, 0),
		}
		err := r.db.InsertRegion(region)
		if err != nil {
			logger.Error("save region to db error: %v", err)
			return nil, err
		}
	}
	r.regionEc2b, err = random.LoadEc2bKey(region.Ec2bData)
	if err != nil {
		logger.Error("parse ec2b data error: %v", err)
		return nil, err
	}
	logger.Info("region ec2b load ok, seed: %v", r.regionEc2b.Seed())
	r.nextUid = region.NextUid
	r.stopServerInfo = &StopServerInfo{
		stopServer:      region.StopServer,
		startTime:       region.StopServerStartTime,
		endTime:         region.StopServerEndTime,
		ipAddrWhiteList: make(map[string]struct{}),
	}
	for _, ipAddr := range region.IpAddrWhiteList {
		r.stopServerInfo.ipAddrWhiteList[ipAddr] = struct{}{}
	}
	r.serverInstanceMap = make(map[string]*sync.Map)
	r.serverInstanceMap[api.GATE] = new(sync.Map)
	r.serverInstanceMap[api.GS] = new(sync.Map)
	r.serverInstanceMap[api.MULTI] = new(sync.Map)
	r.serverInstanceMap[api.ROBOT] = new(sync.Map)
	r.serverInstanceMap[api.DISPATCH] = new(sync.Map)
	r.serverAppIdMap = new(sync.Map)
	r.globalGsOnlineMap = new(sync.Map)
	r.messageQueue = messageQueue
	go r.removeDeadServer()
	go r.broadcastReceiver()
	go r.serverState()
	return r, nil
}

func (s *DiscoveryService) close() {
	ipAddrWhiteList := make([]string, 0)
	for ipAddr := range s.stopServerInfo.ipAddrWhiteList {
		ipAddrWhiteList = append(ipAddrWhiteList, ipAddr)
	}
	region := &dao.Region{
		Ec2bData:            s.regionEc2b.Bytes(),
		NextUid:             s.nextUid,
		StopServer:          s.stopServerInfo.stopServer,
		StopServerStartTime: s.stopServerInfo.startTime,
		StopServerEndTime:   s.stopServerInfo.endTime,
		IpAddrWhiteList:     ipAddrWhiteList,
	}
	err := s.db.UpdateRegion(region)
	if err != nil {
		logger.Error("save region to db error: %v", err)
	}
}

// broadcastReceiver 接收 GS 广播的玩家在线状态变更
//
// GS 在玩家登录/离线时会广播 ServerUserOnlineStateChangeNotify
// Node 收到后维护 globalGsOnlineMap 让其他服务（特别是 gate）知道玩家在哪个 GS
// 这是"全服玩家在线表"的真实 source of truth gate 通过 syncGlobalGsOnlineMap 同步
func (s *DiscoveryService) broadcastReceiver() {
	for {
		netMsg := <-s.messageQueue.GetNetMsg()
		if netMsg.MsgType != mq.MsgTypeServer {
			continue
		}
		if netMsg.EventId != mq.ServerUserOnlineStateChangeNotify {
			continue
		}
		if netMsg.OriginServerType != api.GS {
			continue
		}
		serverMsg := netMsg.ServerMsg
		if serverMsg.IsOnline {
			s.globalGsOnlineMap.Store(serverMsg.UserId, netMsg.OriginServerAppId)
		} else {
			s.globalGsOnlineMap.Delete(serverMsg.UserId)
		}
	}
}

// RegisterServer 服务实例注册（natsrpc 接口）
//
// 调用方：所有服务启动时第一件事就是调这个接口拿 AppId
//
// 处理：
//  1. 校验服务类型有效（GATE/GS/MULTI/ROBOT/DISPATCH 之一）
//  2. 生成 8 字符随机 AppId 用 serverAppIdMap 确保全局唯一
//  3. 创建 ServerInstance 写入 instMap[serverType][appId]
//  4. GS 额外分配 GsId（详见 GS 注册分支）
//  5. 返回 AppId（+ GsId 给 GS）
func (s *DiscoveryService) RegisterServer(ctx context.Context, req *api.RegisterServerReq) (*api.RegisterServerRsp, error) {
	logger.Info("register new server, server type: %v", req.ServerType)
	instMap, exist := s.serverInstanceMap[req.ServerType]
	if !exist {
		return nil, errors.New("server type not exist")
	}
	var appId string
	for {
		appId = strings.ToLower(random.GetRandomStr(8))
		_, exist := s.serverAppIdMap.Load(appId)
		if !exist {
			s.serverAppIdMap.Store(appId, true)
			break
		}
	}
	inst := &ServerInstance{
		serverType:     req.ServerType,
		appId:          appId,
		appVersion:     req.AppVersion,
		lastAliveTime:  time.Now().Unix(),
		loadCount:      0,
		dispatchCancel: false,
	}
	if req.ServerType == api.GATE {
		logger.Info("register new gate server, ip: %v, port: %v", req.GateServerAddr.KcpAddr, req.GateServerAddr.KcpPort)
		inst.gateServerKcpAddr = req.GateServerAddr.KcpAddr
		inst.gateServerKcpPort = req.GateServerAddr.KcpPort
		inst.gateServerMqAddr = req.GateServerAddr.MqAddr
		inst.gateServerMqPort = req.GateServerAddr.MqPort
		inst.gameVersionList = req.GameVersionList
	}
	instMap.Store(appId, inst)
	logger.Info("new server appid is: %v", appId)
	rsp := &api.RegisterServerRsp{
		AppId: appId,
	}
	if req.ServerType == api.GS {
		gsIdUseList := make([]bool, MaxGsId+1)
		gsIdUseList[0] = true
		instMap.Range(func(key, value any) bool {
			serverInstance := value.(*ServerInstance)
			if serverInstance.gsId > MaxGsId {
				logger.Error("invalid gs id inst: %v", serverInstance)
				return true
			}
			gsIdUseList[serverInstance.gsId] = true
			return true
		})
		newGsId := uint32(0)
		for gsId, use := range gsIdUseList {
			if !use {
				newGsId = uint32(gsId)
				break
			}
		}
		if newGsId == 0 {
			return nil, errors.New("no gs id can use")
		}
		inst.gsId = newGsId
		rsp.GsId = newGsId
	}
	return rsp, nil
}

// CancelServer 服务实例注销（natsrpc 接口）
//
// 调用方：服务优雅退出时（defer CancelServer）调一次
// 处理 handleServerOffline 后从 instMap 删除
// 不调 CancelServer 也不会有问题（30s 后 removeDeadServer 自动剔除）
// 但调了能让其他服务立即感知 减少切换抖动
func (s *DiscoveryService) CancelServer(ctx context.Context, req *api.CancelServerReq) (*api.NullMsg, error) {
	logger.Info("server cancel, server type: %v, appid: %v", req.ServerType, req.AppId)
	instMap, exist := s.serverInstanceMap[req.ServerType]
	if !exist {
		return nil, errors.New("server type not exist")
	}
	serverInstance, exist := instMap.Load(req.AppId)
	if !exist {
		logger.Error("recv not exist server cancel, server type: %v, appid: %v", req.ServerType, req.AppId)
		return nil, errors.New("server not exist")
	}
	s.handleServerOffline(serverInstance.(*ServerInstance))
	instMap.Delete(req.AppId)
	return &api.NullMsg{}, nil
}

// KeepaliveServer 心跳保活（每 15s 一次）
// 更新 lastAliveTime + loadCount（让 GetServerAppId 知道当前实时负载）
// 30s 内没收到心跳的实例会被 removeDeadServer 自动剔除
func (s *DiscoveryService) KeepaliveServer(ctx context.Context, req *api.KeepaliveServerReq) (*api.NullMsg, error) {
	logger.Debug("server keepalive, server type: %v, appid: %v, load: %v", req.ServerType, req.AppId, req.LoadCount)
	instMap, exist := s.serverInstanceMap[req.ServerType]
	if !exist {
		return nil, errors.New("server type not exist")
	}
	inst, exist := instMap.Load(req.AppId)
	if !exist {
		logger.Error("recv not exist server keepalive, server type: %v, appid: %v", req.ServerType, req.AppId)
		return nil, errors.New("server not exist")
	}
	serverInstance := inst.(*ServerInstance)
	serverInstance.lastAliveTime = time.Now().Unix()
	serverInstance.loadCount = req.LoadCount
	logger.Debug("server instance: %+v", serverInstance)
	return &api.NullMsg{}, nil
}

// GetServerAppId 服务路由（按负载选 AppId）
//
// 路由策略：
//   - GATE/GS: 选当前 loadCount 最小的实例（最小负载）
//   - 其他类型（MULTI/ROBOT/DISPATCH）: 随机选一个（实例数少时不需要负载均衡）
//
// 调用方：
//   - gate 每 15s 调用 同步 minLoadGsServerAppId / minLoadMultiServerAppId
//   - dispatch 二级 dispatch 时调用选 gate 实例
func (s *DiscoveryService) GetServerAppId(ctx context.Context, req *api.GetServerAppIdReq) (*api.GetServerAppIdRsp, error) {
	logger.Debug("get server instance, server type: %v", req.ServerType)
	instMap, exist := s.serverInstanceMap[req.ServerType]
	if !exist {
		return nil, errors.New("server type not exist")
	}
	if s.getServerInstanceMapLen(instMap) == 0 {
		return nil, errors.New("no server found")
	}
	var inst *ServerInstance = nil
	if req.ServerType == api.GATE || req.ServerType == api.GS {
		inst = s.getMinLoadServerInstance(instMap)
	} else {
		inst = s.getRandomServerInstance(instMap)
	}
	if inst == nil {
		return nil, errors.New("no server found")
	}
	logger.Debug("get server appid is: %v", inst.appId)
	return &api.GetServerAppIdRsp{
		AppId: inst.appId,
	}, nil
}

// GetRegionEc2B 获取区服密钥信息
func (s *DiscoveryService) GetRegionEc2B(ctx context.Context, req *api.NullMsg) (*api.RegionEc2B, error) {
	logger.Info("get region ec2b ok")
	return &api.RegionEc2B{
		Data: s.regionEc2b.Bytes(),
	}, nil
}

// GetGateServerAddr 给客户端选 Gate 地址（dispatch 二级 dispatch 调用）
//
// 关键过滤：按 GameVersion 筛选
//   - 单 gate 实例只能加载一个版本的客户端 proto 代理
//   - 同一集群可起多个不同版本的 gate（如 3.2 + 6.5 各一个）
//   - 客户端来 dispatch 时带版本号 dispatch 用 GetGateServerAddr 找匹配版本的 gate
//   - 找到匹配版本后再选最小负载实例
func (s *DiscoveryService) GetGateServerAddr(ctx context.Context, req *api.GetGateServerAddrReq) (*api.GateServerAddr, error) {
	logger.Debug("get gate server addr")
	instMap, exist := s.serverInstanceMap[api.GATE]
	if !exist {
		return nil, errors.New("gate server not exist")
	}
	if s.getServerInstanceMapLen(instMap) == 0 {
		return nil, errors.New("no gate server found")
	}
	versionInstMap := sync.Map{}
	instMap.Range(func(key, value any) bool {
		serverInstance := value.(*ServerInstance)
		for _, gameVersion := range serverInstance.gameVersionList {
			if gameVersion == req.GameVersion {
				versionInstMap.Store(key, serverInstance)
				return true
			}
		}
		return true
	})
	if s.getServerInstanceMapLen(&versionInstMap) == 0 {
		return nil, errors.New("no gate server found")
	}
	inst := s.getMinLoadServerInstance(&versionInstMap)
	if inst == nil {
		return nil, errors.New("no gate server found")
	}
	logger.Debug("get gate server addr is, ip: %v, port: %v", inst.gateServerKcpAddr, inst.gateServerKcpPort)
	return &api.GateServerAddr{
		KcpAddr: inst.gateServerKcpAddr,
		KcpPort: inst.gateServerKcpPort,
	}, nil
}

// GetAllGateServerInfoList 给 GS/Multi/Robot 拉所有 Gate 列表
//
// 这些服务启动时调用一次 之后每分钟同步一次（messageQueue 自动维护）
// 用于建立 GS → Gate 的 TCP 直连（高吞吐路径 详见 CLAUDE.md "TCP 直连优先 NATS 兜底"）
// 返回 MqAddr/MqPort 是 Gate 的 TCP 直连 MQ 端口（与客户端 KCP 端口不同）
func (s *DiscoveryService) GetAllGateServerInfoList(ctx context.Context, req *api.NullMsg) (*api.GateServerInfoList, error) {
	logger.Debug("get all gate server info list")
	instMap, exist := s.serverInstanceMap[api.GATE]
	if !exist {
		return nil, errors.New("gate server not exist")
	}
	if s.getServerInstanceMapLen(instMap) == 0 {
		return nil, errors.New("no gate server found")
	}
	gateServerInfoList := make([]*api.GateServerInfo, 0)
	instMap.Range(func(key, value any) bool {
		serverInstance := value.(*ServerInstance)
		gateServerInfoList = append(gateServerInfoList, &api.GateServerInfo{
			AppId:  serverInstance.appId,
			MqAddr: serverInstance.gateServerMqAddr,
			MqPort: serverInstance.gateServerMqPort,
		})
		return true
	})
	return &api.GateServerInfoList{
		GateServerInfoList: gateServerInfoList,
	}, nil
}

// GetMainGameServerAppId 获取主 GS（gsId=1 的实例）的 AppId
// 用于全服广播（如 GM 全服公告）需要从一个固定 GS 发起
// 通常 gsId=1 是集群中第一个启动的 GS
func (s *DiscoveryService) GetMainGameServerAppId(ctx context.Context, req *api.NullMsg) (*api.GetMainGameServerAppIdRsp, error) {
	logger.Debug("get main game server appid")
	instMap, exist := s.serverInstanceMap[api.GS]
	if !exist {
		return nil, errors.New("game server not exist")
	}
	if s.getServerInstanceMapLen(instMap) == 0 {
		return nil, errors.New("no game server found")
	}
	appid := ""
	mainGsId := uint32(1)
	instMap.Range(func(key, value any) bool {
		serverInstance := value.(*ServerInstance)
		if serverInstance.gsId == mainGsId {
			appid = serverInstance.appId
			return false
		}
		return true
	})
	if appid == "" {
		return nil, errors.New("main game server not found")
	}
	return &api.GetMainGameServerAppIdRsp{
		AppId: appid,
	}, nil
}

// GetGlobalGsOnlineMap 获取全服玩家GS在线列表
func (s *DiscoveryService) GetGlobalGsOnlineMap(ctx context.Context, req *api.NullMsg) (*api.GlobalGsOnlineMap, error) {
	copyMap := make(map[uint32]string)
	s.globalGsOnlineMap.Range(func(key, value any) bool {
		copyMap[key.(uint32)] = value.(string)
		return true
	})
	return &api.GlobalGsOnlineMap{
		OnlineMap: copyMap,
	}, nil
}

// GetStopServerInfo 获取停服维护信息
func (s *DiscoveryService) GetStopServerInfo(ctx context.Context, req *api.NullMsg) (*api.StopServerInfo, error) {
	return &api.StopServerInfo{
		StopServer: s.stopServerInfo.stopServer,
		StartTime:  s.stopServerInfo.startTime,
		EndTime:    s.stopServerInfo.endTime,
	}, nil
}

// SetStopServerInfo 修改停服维护信息
func (s *DiscoveryService) SetStopServerInfo(ctx context.Context, req *api.StopServerInfo) (*api.NullMsg, error) {
	shutdown := false
	if s.stopServerInfo.stopServer == false && req.StopServer == true {
		shutdown = true
	}
	s.stopServerInfo.stopServer = req.StopServer
	s.stopServerInfo.startTime = req.StartTime
	s.stopServerInfo.endTime = req.EndTime
	if shutdown {
		s.messageQueue.SendToAll(&mq.NetMsg{
			MsgType: mq.MsgTypeServer,
			EventId: mq.ServerStopNotify,
		})
	}
	return &api.NullMsg{}, nil
}

// GetWhiteList 获取停服维护白名单
func (s *DiscoveryService) GetWhiteList(ctx context.Context, req *api.NullMsg) (*api.GetWhiteListRsp, error) {
	ipAddrList := make([]string, 0)
	for ipAddr := range s.stopServerInfo.ipAddrWhiteList {
		ipAddrList = append(ipAddrList, ipAddr)
	}
	return &api.GetWhiteListRsp{IpAddrList: ipAddrList}, nil
}

// SetWhiteList 修改停服维护白名单
func (s *DiscoveryService) SetWhiteList(ctx context.Context, req *api.SetWhiteListReq) (*api.NullMsg, error) {
	if req.IsAdd {
		s.stopServerInfo.ipAddrWhiteList[req.IpAddr] = struct{}{}
	} else {
		delete(s.stopServerInfo.ipAddrWhiteList, req.IpAddr)
	}
	return &api.NullMsg{}, nil
}

// GetNextUid 分配玩家 uid（单调自增 atomic 安全）
//
// 调用方：gate doGateLogin 中首次登录的玩家会分配 uid
// 起始值 100000000 = 1 亿（与官服 uid 风格一致）
//
// **持久化**：nextUid 仅在 Node 关闭时通过 close() 写回 DB
// 正常退出（CancelServer）能保留 异常崩溃可能丢失最近分配的 uid 范围
// 但因为账号注册时记录了 accountId↔uid 关系 不会出错
func (s *DiscoveryService) GetNextUid(ctx context.Context, req *api.NullMsg) (*api.GetNextUidRsp, error) {
	return &api.GetNextUidRsp{
		Uid: atomic.AddUint32(&s.nextUid, 1),
	}, nil
}

// ServerDispatchCancel 取消指定 AppVersion 服务实例的调度（优雅停服 / 滚动升级用）
//
// 处理：
//  1. 把所有 AppVersion 匹配的实例 dispatchCancel 设为 true
//     · GetServerAppId / GetGateServerAddr 后续会跳过这些实例（不分配新连接）
//     · 但已有连接继续服务（不强制踢人）
//  2. 广播 ServerDispatchCancelNotify 让 GS 进入"准备停服"状态（可能拒绝新登录）
//
// 用例：要更新 GS 时先调这个 → 等已有玩家自然下线 → 安全停掉这批实例 → 启动新版本
func (s *DiscoveryService) ServerDispatchCancel(ctx context.Context, req *api.ServerDispatchCancelReq) (*api.NullMsg, error) {
	for _, instMap := range s.serverInstanceMap {
		instMap.Range(func(appid, inst any) bool {
			serverInstance := inst.(*ServerInstance)
			if serverInstance.appVersion == req.AppVersion {
				serverInstance.dispatchCancel = true
			}
			return true
		})
	}
	s.messageQueue.SendToAll(&mq.NetMsg{
		MsgType: mq.MsgTypeServer,
		EventId: mq.ServerDispatchCancelNotify,
		ServerMsg: &mq.ServerMsg{
			AppVersion: req.AppVersion,
		},
	})
	return &api.NullMsg{}, nil
}

func (s *DiscoveryService) getRandomServerInstance(instMap *sync.Map) *ServerInstance {
	instList := make([]*ServerInstance, 0)
	instMap.Range(func(key, value any) bool {
		serverInstance := value.(*ServerInstance)
		if serverInstance.dispatchCancel {
			return true
		}
		instList = append(instList, serverInstance)
		return true
	})
	if len(instList) == 0 {
		return nil
	}
	index := random.GetRandomInt32(0, int32(len(instList)-1))
	inst := instList[index]
	return inst
}

func (s *DiscoveryService) getMinLoadServerInstance(instMap *sync.Map) *ServerInstance {
	instList := make([]*ServerInstance, 0)
	instMap.Range(func(key, value any) bool {
		serverInstance := value.(*ServerInstance)
		if serverInstance.dispatchCancel {
			return true
		}
		instList = append(instList, serverInstance)
		return true
	})
	if len(instList) == 0 {
		return nil
	}
	minLoadInstIndex := 0
	minLoadInstCount := math.MaxUint32
	for index, inst := range instList {
		if inst.loadCount < uint32(minLoadInstCount) {
			minLoadInstCount = int(inst.loadCount)
			minLoadInstIndex = index
		}
	}
	inst := instList[minLoadInstIndex]
	return inst
}

func (s *DiscoveryService) getServerInstanceMapLen(instMap *sync.Map) int {
	count := 0
	instMap.Range(func(key, value any) bool {
		count++
		return true
	})
	return count
}

// removeDeadServer 定时清理死服（每 10 秒检查）
//
// 30 秒内没收到心跳的服务实例视为死服 自动从 instMap 删除
// 同时调 handleServerOffline 处理副作用：
//   - GS 死了：清理它在 globalGsOnlineMap 中的所有玩家在线记录
//
// 这是分布式系统的"心跳超时剔除"标准实现 防止僵尸实例阻塞调度
func (s *DiscoveryService) removeDeadServer() {
	ticker := time.NewTicker(time.Second * 10)
	for {
		<-ticker.C
		nowTime := time.Now().Unix()
		for _, instMap := range s.serverInstanceMap {
			instMap.Range(func(appid, inst any) bool {
				serverInstance := inst.(*ServerInstance)
				if nowTime-serverInstance.lastAliveTime > 30 {
					logger.Warn("remove dead server, server type: %v, appid: %v, last alive time: %v",
						serverInstance.serverType, serverInstance.appId, serverInstance.lastAliveTime)
					instMap.Delete(appid)
					s.handleServerOffline(serverInstance)
				}
				return true
			})
		}
	}
}

func (s *DiscoveryService) handleServerOffline(serverInstance *ServerInstance) {
	if serverInstance.serverType == api.GS {
		s.globalGsOnlineMap.Range(func(uid, gsAppid any) bool {
			if serverInstance.appId == gsAppid.(string) {
				s.globalGsOnlineMap.Delete(uid)
			}
			return true
		})
	}
}

func (s *DiscoveryService) serverState() {
	ticker := time.NewTicker(time.Minute * 1)
	for {
		<-ticker.C
		totalGateLoad := uint32(0)
		totalGsLoad := uint32(0)
		for _, instMap := range s.serverInstanceMap {
			instMap.Range(func(appid, inst any) bool {
				serverInstance := inst.(*ServerInstance)
				logger.Info("server state, type: %v, appid: %v, load: %v", serverInstance.serverType, serverInstance.appId, serverInstance.loadCount)
				switch serverInstance.serverType {
				case api.GATE:
					totalGateLoad += serverInstance.loadCount
				case api.GS:
					totalGsLoad += serverInstance.loadCount
				}
				return true
			})
		}
		logger.Info("total gate load: %v, total gs load: %v", totalGateLoad, totalGsLoad)
	}
}
