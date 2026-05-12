package config

import (
	"fmt"

	"github.com/BurntSushi/toml"
)

// 全局配置 - 整个项目的配置入口
//
// 单一全局变量 CONF 启动时调用 InitConfig 一次 后续都通过 GetConfig() 取
// 配置文件用 TOML 格式（cmd/{服务}/application.toml）
//
// 6 个分组：
//   - Hk4e: 游戏服核心配置（端口/版本/SDK/特性开关）
//   - Hk4eRobot: 模拟客户端配置（仅 robot 服用）
//   - Logger: 日志配置
//   - Database: DB url（mongodb:// / mysql:// / sqlite:// 三选一前缀）
//   - Redis: Redis 地址
//   - MQ: NATS URL
//
// **standalone vs cluster 部署**：
//   - StandaloneModeEnable=true: 单进程跑所有服务（开发/小规模）配置只用一份
//   - StandaloneModeEnable=false: 集群部署 每个服务独立 application.toml
//
// 加载失败 panic（启动期错误必须立即暴露）

var CONF *Config = nil

// Config 配置 6 大分组
type Config struct {
	Hk4e      Hk4e      `toml:"hk4e"`
	Hk4eRobot Hk4eRobot `toml:"hk4e_robot"`
	Logger    Logger    `toml:"logger"`
	Database  Database  `toml:"database"`
	Redis     Redis     `toml:"redis"`
	MQ        MQ        `toml:"mq"`
}

// Hk4e 游戏服配置（最重要的一组 影响所有服务行为）
//
// 关键特性开关：
//   - ClientProtoProxyEnable: 客户端协议动态代理（多版本部署必开 详见 gate/net/proto_endecode.go）
//   - HighVersionProtoConvEnable: 高版本协议字段转换（与 ClientProtoProxyEnable 配合）
//   - TcpModeEnable: TCP 客户端接入模式（实验性 默认 KCP）
//   - StandaloneModeEnable: 单进程模式（开发/小规模）
//   - LoadSceneLuaConfig: 是否加载场景 Lua（关掉可大幅减少内存 但任务/触发器失效）
//   - RegisterAllProtoMessage: 是否注册全部 proto（按需还是全量）
//   - TrackPacket: 调试用 打印每条 KCP 包的 JSON 内容
//
// 关键 URL 配置：
//   - DispatchUrl: 二级 dispatch 地址（一级 dispatch 通过此返回给客户端）
//   - LoginSdkUrl: gate 验证 ComboToken 时调用的 dispatch URL
//   - LoginSdkAccountKey: HMAC 签名密钥（gate ↔ dispatch 之间防伪造）
//   - GmAuthKey: GM 后台 HTTP 鉴权密钥（默认 "flswld" 生产必须改）
type Hk4e struct {
	DispatchHttpPort           int32  `toml:"dispatch_http_port"`             // dispatch的http端口
	GmHttpPort                 int32  `toml:"gm_http_port"`                   // gm的http端口
	KcpAddr                    string `toml:"kcp_addr"`                       // kcp地址 该地址只用来注册到节点服务器 填网关的外网地址 网关本地监听为0.0.0.0
	KcpPort                    int32  `toml:"kcp_port"`                       // kcp端口号
	TcpModeEnable              bool   `toml:"tcp_mode_enable"`                // 是否开启tcp模式 需要hook客户端网络库才能支持 共用kcp端口号
	GameDataConfigPath         string `toml:"game_data_config_path"`          // 配置表路径
	ClientProtoProxyEnable     bool   `toml:"client_proto_proxy_enable"`      // 是否开启客户端协议代理功能
	Version                    string `toml:"version"`                        // 支持的客户端协议版本号 三位数字 多个以逗号分隔 如300,310,315,320
	GateTcpMqAddr              string `toml:"gate_tcp_mq_addr"`               // 访问网关tcp直连消息队列的地址 填网关的内网地址
	GateTcpMqPort              int32  `toml:"gate_tcp_mq_port"`               // tcp消息队列端口号
	LoginSdkUrl                string `toml:"login_sdk_url"`                  // 网关登录验证token的sdk服务器地址 目前填dispatch的内网地址
	LoginSdkAccountKey         string `toml:"login_sdk_account_key"`          // sdk服务器账号验证的签名密钥
	LoadSceneLuaConfig         bool   `toml:"load_scene_lua_config"`          // 是否加载场景详情LUA配置数据
	DispatchUrl                string `toml:"dispatch_url"`                   // 二级dispatch地址 将域名改为dispatch的外网地址
	GmAuthKey                  string `toml:"gm_auth_key"`                    // gm认证密钥
	RegisterAllProtoMessage    bool   `toml:"register_all_proto_message"`     // 注册全部pb消息
	ByteCheckMode              int32  `toml:"byte_check_mode"`                // 网络包数据校验模式
	StandaloneModeEnable       bool   `toml:"standalone_mode_enable"`         // 是否开启单进程模式
	TrackPacket                bool   `toml:"track_packet"`                   // 追踪收发包
	SdkEnv                     int32  `toml:"sdk_env"`                        // sdk环境 0:国内 1:国内沙箱 2:海外
	ClientProtoDir             string `toml:"client_proto_dir"`               // 需要代理的客户端协议文件目录
	HighVersionProtoConvEnable bool   `toml:"high_version_proto_conv_enable"` // 是否开启高版本协议兼容
}

// Hk4eRobot 模拟客户端配置（仅 robot 服读取）
//
// 关键功能：
//   - 单账号模式（DosEnable=false）：仅用 Account 配置一个账号 用于开发测试
//   - 压测模式（DosEnable=true）：起 DosTotalNum 个虚拟账号 每批 DosBatchNum 并发
//
// ClientMove 系列：让 robot 在场景内随机移动 模拟真实玩家行为
// SelectRegionIndex: 一级 dispatch 返回多个区服时选哪个 默认 0
type Hk4eRobot struct {
	RobotHttpPort      int32  `toml:"robot_http_port"`       // robot的http端口
	RegionListUrl      string `toml:"region_list_url"`       // 一级dispatch地址
	RegionListParam    string `toml:"region_list_param"`     // 一级dispatch的url参数
	SelectRegionIndex  int32  `toml:"select_region_index"`   // 选择的二级dispatch索引
	CurRegionUrl       string `toml:"cur_region_url"`        // 二级dispatch地址 可强制指定 为空则使用一级dispatch获取的地址
	CurRegionParam     string `toml:"cur_region_param"`      // 二级dispatch的url参数
	KeyId              string `toml:"key_id"`                // 客户端密钥编号
	LoginSdkUrl        string `toml:"login_sdk_url"`         // sdk登录服务器地址
	Account            string `toml:"account"`               // 帐号
	Password           string `toml:"password"`              // base64编码的rsa公钥加密后的密码
	ClientVersion      string `toml:"client_version"`        // 客户端版本号
	DosEnable          bool   `toml:"dos_enable"`            // 是否开启压力测试
	DosTotalNum        int32  `toml:"dos_total_num"`         // 压力测试总并发数量 帐号自动添加后缀编号
	DosBatchNum        int32  `toml:"dos_batch_num"`         // 压力测试每批登录并发数量
	DosLoopLogin       bool   `toml:"dos_loop_login"`        // 压力测试是否循环登录退出
	ClientMoveEnable   bool   `toml:"client_move_enable"`    // 是否开启客户端模拟移动
	ClientMoveSpeed    int32  `toml:"client_move_speed"`     // 客户端模拟移动速度
	ClientMoveRangeExt int32  `toml:"client_move_range_ext"` // 客户端模拟移动区域半径
}

// Logger 日志
type Logger struct {
	Level        string `toml:"level"`
	TrackLine    bool   `toml:"track_line"`
	TrackThread  bool   `toml:"track_thread"`
	EnableFile   bool   `toml:"enable_file"`
	DisableColor bool   `toml:"disable_color"`
	EnableJson   bool   `toml:"enable_json"`
}

// Database 数据库
type Database struct {
	Url string `toml:"url"`
}

// Redis 缓存
type Redis struct {
	Addr     string `toml:"addr"`
	Password string `toml:"password"`
}

// MQ 消息队列
type MQ struct {
	NatsUrl string `toml:"nats_url"`
}

func InitConfig(filePath string) {
	CONF = new(Config)
	CONF.loadConfigFile(filePath)
}

func GetConfig() *Config {
	return CONF
}

// 加载配置文件
func (c *Config) loadConfigFile(filePath string) {
	_, err := toml.DecodeFile(filePath, &c)
	if err != nil {
		info := fmt.Sprintf("config file load error: %v\n", err)
		panic(info)
	}
}
