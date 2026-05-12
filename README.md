# lost-woods-go · 迷失森林服务端

> 一个 Go 语言实现的多人联机游戏服务端,对接配套的 [LostWoods](https://github.com/flswld/LostWoods) Unity 客户端,用于练手 Go 语言下的 MMO 服务端架构、二进制协议、加密通信、跨服分布式部署。

[![Go](https://img.shields.io/badge/Go-1.24-00ADD8?logo=go)](https://go.dev/)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue)](./LICENSE)
[![Client](https://img.shields.io/badge/Client-LostWoods-orange)](https://github.com/flswld/LostWoods)
[![Code](https://img.shields.io/badge/code-72k%20lines-green)](#)

---

## 项目简介

**lost-woods-go** 是一个练手性质的 Go 语言多人联机游戏服务端,跑在自实现的二进制协议 + KCP 传输层之上,对端是同作者的 [LostWoods](https://github.com/flswld/LostWoods) Unity 客户端。

项目历时 3 年开发,主要用来探索这些方向:

- Go 语言下的高并发游戏服务端架构 (主循环单线程串行 + 异步 IO 双通道)
- 自定义二进制协议 + 完整加密通信栈实现
- 多版本客户端协议动态适配 (无需重新生成代码)
- 跨服多人世界无感迁移
- AOP 切面式自创玩法扩展机制

→ 这不是一个面向玩家的成品后端,而是把网络栈、玩家管理、跨服架构、玩法系统串起来的 Go 服务端项目模板。

## 三大特色架构

### 1. GS 主循环单线程串行化

整个游戏服只有一个主循环 goroutine,所有玩家逻辑串行执行。**无需锁** + **绑定 OS 线程**减少调度抖动,配合 panic recover + 死循环熔断机制保证稳定性。所有阻塞 IO 都通过 `LocalEvent` 异步化回到主循环。

### 2. 客户端协议动态反射代理

服务端协议永远基于**固定基础版本**,通过 `ClientProtoProxy` 用 `protoreflect` 把更高版本客户端 proto **实时翻译**为基础版本。新版本接入只需放入 .proto 文件 + 配置个别字段映射,**不需要重新生成代码**,业务层无感知。

### 3. AI 世界 + AOP 切面插件机制

- **GM 机器人好友**:用 AI 玩家"小可爱"作为每个玩家的好友,玩家通过和它私聊发 GM 命令 (`item add 1234 5` / `goto 100 200 300`),绕过客户端没有 GM 命令输入框的限制
- **AI 世界**:豁免普通多人世界 4 人上限,承载自创大场玩法
- **完整大逃杀玩法**:已实现 4 阶段缩圈毒区 + 简化版子弹物理引擎 (重力 + 阻力 + AABB 碰撞)
- **AOP 插件系统**:用切面式事件钩子叠加自创玩法,不污染原版协议处理逻辑

## 主要特性

### 网络层
- 完整登录链:dispatch HTTP → 账号密码登录 → KCP 握手 → 玩家 Token 协商 → 场景进入
- 自实现 **KCP 编解码** (XOR 加密 + 魔数 0x4567/0x89AB 校验 + 多包递归拆分)
- 加密栈完整:**ec2b XorKey** 推导 / 会话 **KeyBlock** 切换 / **RSA PKCS#1** / **MT19937_64** PRNG (与客户端 Mono 实现逐位一致)
- **跨服无感迁移**:玩家从 GS1 切到 GS2 时 KCP 不断、不发重连通知,客户端零感知
- **顶号机制**:跨服重复登录自动踢旧会话,带分布式锁防 race

### 服务发现 / 分布式
- 自实现服务注册中心 (基于 natsrpc),GS/Gate/Dispatch/Multi 各服自动注册 + 心跳
- **TCP 直连优先 + NATS 广播兜底**双通道 (高频游戏消息走 TCP,广播走 NATS)
- 30s 心跳超时自动剔除 / 滚动升级 (DispatchCancel) / 停服管理 + IP 白名单
- 支持**同集群多版本 Gate 共存**,按客户端版本号路由

### 数据持久化
- **三选一 DB**:MongoDB / MySQL / SQLite,按 URL 前缀自动切换
- **三层存储**:内存 + Redis (msgpack + LZ4 压缩 30 天过期) + DB
- **异步存档**:主循环序列化 + 后台 goroutine 写库,单次序列化耗时上限 10ms
- **Redis 分布式锁** (token + Lua 脚本 GET+DEL) 防跨服并发改同一玩家档

### 玩法系统
- **4 人小队多人联机** + 跨服无感迁移 + 房间制
- **大世界场景**:Block/Group/Suite 三层结构 + Lua trigger (9 种事件类型)
- **战斗动作同步**:基于 InvokeEntry 的 Frame Sync,支持 UnionCmd 聚合消息
- **任务系统**:AcceptCond/FinishCond/ExecType + 6 种逻辑组合 + Lua 脚本驱动
- **抽卡系统**:含软保底 / 大保底 / 命之座转换机制
- **体力 / 载具 / 坠落扣血 / 溺水死亡**
- **聊天 (含跨服)** / 好友 / 邮件 / 商店
- **25 个 GM 命令** (item/avatar/goto/monster/quest/weather/...)
- **自定义大逃杀玩法** (跑在 AI 世界容器内)
- **MIDI 弹奏功能** (.mid 文件解析 → 场景音效广播)

### 可扩展性
- **AOP 插件系统**:8 种事件钩子 (标点/死亡/物件交互/进场/技能/受击/创建物件/子弹命中)
- **Lua 脚本场景**:38 个 ScriptLib API 暴露给 Lua 调用 (实体操作 / group 变量 / 任务推进等)
- **协议层热扩展**:接入新版本客户端不需要重新生成 .pb.go

## 技术栈

| 类别 | 技术 |
|---|---|
| 语言 | Go 1.24 (含泛型) |
| 客户端协议 | KCP + Protocol Buffers v1.36 |
| 协议反射 | jhump/protoreflect (动态多版本适配) |
| 服务间通信 | NATS Server v2.11 + natsrpc + TCP 直连双通道 |
| 服务间序列化 | msgpack v5 + LZ4 压缩 |
| 数据库 | MongoDB / MySQL / SQLite (GORM v1.25) 三选一 |
| 缓存 | Redis v8 (单机 / Cluster) |
| 配置 | TOML + CSV + HJSON + Lua (gopher-lua) |
| 寻路 | 自研 NavMesh (移植自 Unity) |
| 算法 | AOI (3D 格子) + 雪花 ID + MT19937 + 自实现 EC2B 加密 |
| HTTP | gin v1.9 |
| MIDI | gomidi/midi v2 |
| 客户端 | [LostWoods](https://github.com/flswld/LostWoods) |

## 快速开始

### 环境要求

- **Go 1.24+**
- **protoc + protoc-gen-go** (可选,仅生成协议代码需要)
- **数据库三选一**:SQLite (默认,无需配置) / MySQL / MongoDB
- **Redis** 单机或集群 (standalone 单进程模式不需要)

### 编译运行

```bash
# 1. 安装代码生成工具 (首次或更新协议时)
make dev_tool

# 2. 生成 protobuf 代码
make gen_proto
make gen_natsrpc

# 3. 编译所有服务到 bin/
make build

# 4. standalone 模式启动 (单进程内含 NATS + 6 个服务)
cd cmd/standalone
../../bin/standalone --config application.toml
```

启动后默认监听:
- `8080` HTTP dispatch
- `22222` UDP KCP / TCP 客户端接入
- `9001` HTTP GM 后台
- `4567` HTTP statsviz 性能监控

### Docker 部署

```bash
make docker_config              # 准备配置模板
make docker_build               # 构建 7 个镜像

# 单进程模式
docker-compose -f lost-woods-go-standalone.yaml up

# 集群模式 (含 1 node + 1 dispatch + 1 gate + 3 gs + 1 multi + 1 gm)
docker-compose -f lost-woods-go-cluster.yaml up
```

### 客户端连接

启动 [LostWoods](https://github.com/flswld/LostWoods) 客户端,在登录界面填入服务端 dispatch 地址 (默认 `http://192.168.111.222:8080`) 即可连接。

## 架构

```
                                 ┌──────────────┐
                                 │     Node     │  服务注册/发现
                                 │   (natsrpc)  │  UID 分配 / 全局在线表 / 停服管理
                                 └──────┬───────┘
                                        │
        ┌─────────────────┬─────────────┼─────────────┬─────────────────┐
        │                 │             │             │                 │
   ┌────▼─────┐    ┌──────▼──────┐  ┌──▼───┐   ┌─────▼──────┐    ┌─────▼────┐
   │ Dispatch │    │    Gate     │  │  GS  │   │   Multi    │    │    GM    │
   │ HTTP     │    │ KCP/TCP 接入 │  │ 业务 │   │  反作弊    │    │ HTTP 后台 │
   │ 8080     │    │ 22222/33333 │  │ 核心 │   │           │    │ 9001     │
   └────┬─────┘    └──────┬──────┘  └──┬───┘   └─────┬──────┘    └─────┬────┘
        │                 │            │              │                 │
        └───────────── 客户端 (LostWoods) ───────────┘                  │
                          ↓                                              │

服务间通信:TCP 直连 (高吞吐) + NATS (广播兜底) 双通道
```

| 服务 | 职责 |
|---|---|
| **Node** | 服务发现注册中心、UID 分配、密钥源、停服管理 |
| **Dispatch** | HTTP 一/二级 dispatch、SDK 登录、Token 验证 |
| **Gate** | KCP/TCP 客户端接入、KCP 编解码、客户端协议动态代理 |
| **GS** | 游戏业务核心 (玩家 / 战斗 / 场景 / Lua trigger) |
| **Multi** | 反作弊 (瞬移 / 超速 / 连发检测)、寻路服务 |
| **GM** | 后台 HTTP (停服管理、白名单、滚动升级) |
| **Robot** | 模拟客户端 + 压测客户端 |

**两种部署模式:**
- **standalone**:单进程跑所有服务,推荐开发用
- **cluster**:每个服务独立容器,可起多个 GS 横向扩展

## 项目结构

```
lost-woods-go/
├── cmd/             各服入口 (standalone + 7 个独立服务 + Cobra 元命令)
├── common/          跨服务共享代码 (config / mq / rpc / region / constant)
├── dispatch/        HTTP 调度服 (gin + RSA + EC2B + Token)
├── gate/            KCP/TCP 网关 (含客户端协议动态代理)
├── node/            服务发现注册中心 (natsrpc)
├── gs/              游戏服核心 (业务最复杂的部分)
│   ├── game/        40 个 .go ~3.4 万行 业务逻辑
│   ├── model/       玩家数据模型 (DbPlayer 含 9 个子模型)
│   ├── dao/         三选一 DB + Redis
│   └── service/     对外 RPC (GM 命令)
├── multi/           反作弊 + 寻路
├── gm/              GM 后台 HTTP
├── robot/           模拟客户端 + 压测
├── protocol/        客户端协议 (cmd_id 表 + 61 个 .proto)
├── gdconf/          游戏数据配置 (55 个加载器)
├── pkg/             工具包 (AOI / 雪花 / NavMesh / EC2B / MIDI 等)
└── docker/          Docker 镜像构建上下文
```

## 功能实现状态

| 模块 | 状态 | 说明 |
|---|:--:|---|
| 角色养成 (升级 / 突破 / 天赋 / 命之座) | 🟡 | 数据流程完成,效果依赖 Ability 系统不完整生效 |
| 武器 (升级 / 突破 / 精炼) | 🟡 | 数据流程完成,精炼效果未生效 |
| 装备词条系统 | 🟢 | 副词条经验暴击 1%×5/8%×2/91%×1 |
| 抽卡 | 🟢 | 4 池配置含完整保底机制 |
| 多人联机 (含跨服无感迁移) | 🟢 | 4 人上限 + AI 世界豁免 |
| 大世界 (怪物 / 物件 / 区块) | 🟢 | block/group/suite 体系跑通 |
| 战斗动作同步 | 🟢 | Frame Sync via UnionCmdNotify |
| 体力 / 载具 / 坠落扣血 | 🟢 | - |
| 聊天 (含跨服) | 🟢 | - |
| 社交 (好友 / 名片) | 🟡 | 黑名单未实现 |
| 邮件 | 🟡 | 极简版,附件功能未实现 |
| 商店 | 🟡 | 占位实现 |
| GM 命令系统 | 🟢 | 25 个命令 |
| 任务系统 | 🟡 | 基础框架接入,完整剧情需要逐个配 |
| 自定义大逃杀玩法 | 🟢 | AI 世界 + 物理引擎 + 4 阶段缩圈 |
| MIDI 弹奏 | 🟢 | 玩具功能 |
| 副本系统 | 🔴 | 协议层有,副本内容空 |
| 卡牌对战 | 🔴 | 仅到主阶段,TODO 中 |
| 玩家家园 / 副本挑战 / 活动 / 成就 | 🔴 | 未实现 |
| Ability 系统 | 🔴 | 仅 6 种 Action + 1 种 Mixin (伤害公式 / 反应 / buff 等未实现) |
| 反作弊 | 🟡 | 被动检测,默认仅记录不真踢 |

图例:🟢 完成 / 🟡 部分完成或占位 / 🔴 未实现

## 扩展开发指南

项目设计上鼓励通过以下方式扩展,**不破坏原版协议处理流程**:

| 想做的事 | 怎么做 |
|---|---|
| 加 GM 命令 | 在 `gs/game/game_command_controller.go` 注册 `CommandController` |
| 自创玩法 | 实现 `IPlugin` 接口 (参考 `game_plugin_pubg.go`),监听 8 种事件钩子 |
| 大场景玩法 | 利用 AI 世界容器 (豁免 4 人上限) |
| 接新客户端版本 | 把 .proto 放进 `ClientProtoDir/`,在 `proto_endecode.go` 加映射 |
| 加任务条件类型 | 在 `gs/game/player_quest.go` 的 `TriggerQuest/ExecQuest` 加 case |
| 自定义场景脚本 | 在 `gdconf/game_data_config/lua/` 加 group Lua + trigger |

**所有新功能必须在主循环 goroutine 内修改玩家数据**——所有阻塞操作走 `LocalEvent` 异步化。

## 相关项目

- **[LostWoods](https://github.com/flswld/LostWoods)** —— 配套的 Unity 3D 多人联机客户端

## License

本项目以 [Apache License 2.0](LICENSE) 开源。

---

> 本项目仅用于学习 Go 语言下的 MMO 服务端架构、二进制协议、加密通信与跨服分布式实现,不用于任何商业用途。
