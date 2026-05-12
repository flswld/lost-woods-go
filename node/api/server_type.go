package api

// 服务类型常量 - Node 注册中心识别的 7 种服务类型
//
// 用途：
//   - RegisterServer.ServerType: 服务自报类型让 Node 分类管理
//   - GetServerAppId.ServerType: 按类型查询负载最小的实例
//   - mq.NetMsg.OriginServerType: 消息来源服务类型（用于路由判断）
//
// 各类型的 instMap 在 DiscoveryService.serverInstanceMap 中独立存储
//
//	key=ServerType value=*sync.Map (appId → ServerInstance)
const (
	NODE     = "NODE"     // 服务发现中心（自身 不会注册到 instMap）
	DISPATCH = "DISPATCH" // HTTP 调度服务（处理客户端登录前的 region 请求）
	GATE     = "GATE"     // 客户端 KCP 网关
	GS       = "GS"       // 游戏逻辑服（每个实例独立 GsId 1~MaxGsId）
	MULTI    = "MULTI"    // 反作弊+寻路服
	GM       = "GM"       // GM 后台 HTTP 服务（不注册到 Node 仅 MQ AppId="gm"）
	ROBOT    = "ROBOT"    // 模拟客户端/压测客户端（不真注册 因为是 client 角色）
)
