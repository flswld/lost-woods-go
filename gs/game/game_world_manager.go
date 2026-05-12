package game

import (
	"math"
	"time"

	"hk4e/common/constant"
	"hk4e/gdconf"
	"hk4e/gs/model"
	"hk4e/pkg/alg"
	"hk4e/pkg/random"
	"hk4e/protocol/proto"

	"github.com/flswld/halo/logger"
)

// 世界管理器
//
// 三层模型：WorldManager → World → Scene
//   WorldManager  全局单例 持有所有World和场景AOI
//   World         单个玩家世界（owner+其他Guest 单人/多人模式）含多个Scene
//   Scene         场景实例（蒙德/璃月/副本等）含 Player + Entity + Group
//
// AOI 双层划分：
//   sceneBlockAoiMap[sceneId]                    粗格子(1024x1024) 用于block级加载
//   sceneEntityAoiMap[sceneId][visionLevel]      细格子(按视野等级) 用于实体可见性
//
// 特殊世界：AI 世界（owner.PlayerId < PlayerBaseUid）
//   - 全GS共享一个 由WORLD_MANAGER.aiWorld单字段持有
//   - 不受4人上限约束（PUBG等大场玩法的容器）
//   - 启动物理引擎和 aiWorldAoi 网格

const (
	ENTITY_NUM_UNLIMIT        = false // true时 ENTITY_MAX_SEND_NUM 失效
	ENTITY_MAX_SEND_NUM       = 10000 // 场景内最大实体数量 防止AI世界实体爆炸
	MAX_MULTIPLAYER_WORLD_NUM = 10    // 本服务器最大多人世界数量
)

type WorldManager struct {
	worldMap            map[uint64]*World                  // 所有世界 key:worldId(雪花生成)
	snowflake           *alg.SnowflakeWorker               // 共用的雪花id生成器
	aiWorld             *World                             // 本服的Ai玩家世界（owner=本服AI玩家"小可爱"）
	sceneBlockAoiMap    map[uint32]*alg.AoiManager         // 场景区块aoi 启动时按sceneLuaConfig的Block范围一次性建立
	sceneEntityAoiMap   map[uint32]map[int]*alg.AoiManager // 场景实体aoi 按 sceneId × visionLevel 分多套
	multiplayerWorldNum uint32                             // 本服当前的多人世界数量
}

// NewWorldManager 启动时调用 加载所有场景的AOI（耗时较长 含读取scene_lua_config）
func NewWorldManager(snowflake *alg.SnowflakeWorker) (r *WorldManager) {
	r = new(WorldManager)
	r.worldMap = make(map[uint64]*World)
	r.snowflake = snowflake
	r.LoadSceneAoi()
	r.multiplayerWorldNum = 0
	return r
}

func (w *WorldManager) GetWorldById(worldId uint64) *World {
	return w.worldMap[worldId]
}

func (w *WorldManager) GetAllWorld() map[uint64]*World {
	return w.worldMap
}

// CreateWorld 创建一个新的世界 owner是世界拥有者（房主）
// enterSceneToken 初始化为5000~50000随机值 防止恶意客户端伪造
// 如果是AI世界（owner是本服AI玩家） 额外初始化AOI网格 + PUBG物理引擎
func (w *WorldManager) CreateWorld(owner *model.Player) *World {
	worldId := uint64(w.snowflake.GenId())
	world := &World{
		id:                   worldId,
		worldManager:         w,
		owner:                owner,
		playerMap:            make(map[uint32]*model.Player),
		sceneMap:             make(map[uint32]*Scene),
		enterSceneToken:      uint32(random.GetRandomInt32(5000, 50000)),
		enterSceneContextMap: make(map[uint32]*EnterSceneContext),
		entityIdCounter:      0,
		worldLevel:           0,
		multiplayer:          false,
		mpLevelEntityId:      0,
		chatMsgList:          make([]*proto.ChatInfo, 0),
		playerFirstEnterMap:  make(map[uint32]int64),
		waitEnterPlayerMap:   make(map[uint32]int64),
		multiplayerTeam:      CreateMultiplayerTeam(),
		peerList:             make([]*model.Player, 0),
		aiWorldAoi:           nil,
		bulletPhysicsEngine:  nil,
	}
	world.mpLevelEntityId = world.GetNextWorldEntityId(constant.ENTITY_TYPE_MP_LEVEL)
	w.worldMap[worldId] = world

	if w.IsAiWorld(world) {
		// AI世界专属：3D网格AOI + PUBG子弹物理引擎
		// AOI范围 X[-8000,4000] Y[-200,1000] Z[-5500,6500] 覆盖蒙德地图主要区域
		// 网格 120×12×120 = 17万格 用于大量玩家场景的视野过滤
		aoiManager := alg.NewAoiManager()
		aoiManager.SetAoiRange(-8000, 4000, -200, 1000, -5500, 6500)
		aoiManager.Init3DRectAoiManager(120, 12, 120, true)
		world.aiWorldAoi = aoiManager
		logger.Info("ai world aoi init finish")
		world.NewPhysicsEngine()
	}

	return world
}

func (w *WorldManager) DestroyWorld(worldId uint64) {
	world := w.GetWorldById(worldId)
	for _, player := range world.playerMap {
		world.RemovePlayer(player)
		player.WorldId = 0
	}
	delete(w.worldMap, worldId)
	if world.multiplayer {
		w.multiplayerWorldNum--
	}
}

// GetAiWorld 获取本服务器的Ai世界（全GS共享单例）
// 用于：玩家进入PUBG玩法时跳转的目标世界、JPEG像素屏摆放场景、MIDI弹奏广播范围 等
func (w *WorldManager) GetAiWorld() *World {
	return w.aiWorld
}

// InitAiWorld 在AI玩家创建并 CreateWorld 之后调用 把那个世界登记为本服AI世界
func (w *WorldManager) InitAiWorld(owner *model.Player) {
	w.aiWorld = w.GetWorldById(owner.WorldId)
}

// IsAiWorld 判断是否AI世界 准则：world.owner.PlayerId < PlayerBaseUid（10000+gsId对应AI玩家）
func (w *WorldManager) IsAiWorld(world *World) bool {
	return world.owner.PlayerId < PlayerBaseUid
}

func (w *WorldManager) GetSceneBlockAoiMap() map[uint32]*alg.AoiManager {
	return w.sceneBlockAoiMap
}

func (w *WorldManager) GetSceneEntityAoiMap() map[uint32]map[int]*alg.AoiManager {
	return w.sceneEntityAoiMap
}

// LoadSceneAoi 启动时一次性加载所有场景的AOI网格
// 1. 计算每个场景的BlockMap范围 → 建立粗格子(1024x1024) sceneBlockAoiMap
// 2. 按 visionLevel 建立细格子（多套） sceneEntityAoiMap[sceneId][visionLevel]
// 3. 把场景内所有 monster/npc/gadget/region 按位置预放进对应的细格子
// 这一过程会读 scene_lua_config 的所有group数据 启动时间较长（数十秒到分钟级）
// 热更配置时（ReloadGameDataConfigFinish）也会重新执行一次
func (w *WorldManager) LoadSceneAoi() {
	w.sceneBlockAoiMap = make(map[uint32]*alg.AoiManager)
	w.sceneEntityAoiMap = make(map[uint32]map[int]*alg.AoiManager)
	for _, sceneLuaConfig := range gdconf.GetSceneLuaConfigMap() {
		sceneId := uint32(sceneLuaConfig.Id)
		minX := int32(math.MaxInt32)
		maxX := int32(math.MinInt32)
		minZ := int32(math.MaxInt32)
		maxZ := int32(math.MinInt32)
		for _, blockConfig := range sceneLuaConfig.BlockMap {
			if int32(blockConfig.BlockRange.Min.X) < minX {
				minX = int32(blockConfig.BlockRange.Min.X)
			}
			if int32(blockConfig.BlockRange.Max.X) > maxX {
				maxX = int32(blockConfig.BlockRange.Max.X)
			}
			if int32(blockConfig.BlockRange.Min.Z) < minZ {
				minZ = int32(blockConfig.BlockRange.Min.Z)
			}
			if int32(blockConfig.BlockRange.Max.Z) > maxZ {
				maxZ = int32(blockConfig.BlockRange.Max.Z)
			}
		}
		numX := uint32(maxX-minX) / 1024
		if numX == 0 {
			numX = 1
		}
		numZ := uint32(maxZ-minZ) / 1024
		if numZ == 0 {
			numZ = 1
		}
		aoiManager := alg.NewAoiManager()
		aoiManager.SetAoiRange(minX, maxX, -1000, 1000, minZ, maxZ)
		aoiManager.Init3DRectAoiManager(numX, 1, numZ, true)
		w.sceneBlockAoiMap[sceneId] = aoiManager
		w.sceneEntityAoiMap[sceneId] = make(map[int]*alg.AoiManager)
		for visionLevel, vision := range constant.VISION_LEVEL {
			numX = uint32(maxX-minX) / vision.GridWidth
			if numX == 0 {
				numX = 1
			}
			numZ = uint32(maxZ-minZ) / vision.GridWidth
			if numZ == 0 {
				numZ = 1
			}
			aoiManager = alg.NewAoiManager()
			aoiManager.SetAoiRange(minX, maxX, -1000, 1000, minZ, maxZ)
			aoiManager.Init3DRectAoiManager(numX, 1, numZ, true)
			w.sceneEntityAoiMap[sceneId][visionLevel] = aoiManager
		}
		for _, block := range sceneLuaConfig.BlockMap {
			blockCenter := &gdconf.Vector{X: (block.BlockRange.Min.X + block.BlockRange.Max.X) / 2.0, Y: 0.0, Z: (block.BlockRange.Min.Z + block.BlockRange.Max.Z) / 2.0}
			w.sceneBlockAoiMap[sceneId].AddObjectToGridByPos(int64(block.Id), block, blockCenter.X, 0.0, blockCenter.Z)
			for _, group := range block.GroupMap {
				for _, monster := range group.MonsterMap {
					objectId := int64(group.Id)<<32 + int64(monster.ConfigId)
					w.sceneEntityAoiMap[sceneId][int(monster.VisionLevel)].AddObjectToGridByPos(objectId, monster, monster.Pos.X, 0.0, monster.Pos.Z)
				}
				for _, npc := range group.NpcMap {
					objectId := int64(group.Id)<<32 + int64(npc.ConfigId)
					w.sceneEntityAoiMap[sceneId][constant.VISION_LEVEL_NORMAL].AddObjectToGridByPos(objectId, npc, npc.Pos.X, 0.0, npc.Pos.Z)
				}
				for _, gadget := range group.GadgetMap {
					objectId := int64(group.Id)<<32 + int64(gadget.ConfigId)
					w.sceneEntityAoiMap[sceneId][int(gadget.VisionLevel)].AddObjectToGridByPos(objectId, gadget, gadget.Pos.X, 0.0, gadget.Pos.Z)
				}
				for _, region := range group.RegionMap {
					objectId := int64(group.Id)<<32 + int64(region.ConfigId)
					w.sceneEntityAoiMap[sceneId][constant.VISION_LEVEL_NORMAL].AddObjectToGridByPos(objectId, region, region.Pos.X, 0.0, region.Pos.Z)
				}
			}
		}
	}
}

func (w *World) IsValidScenePos(sceneId uint32, x, y, z float32) bool {
	aoiManager, exist := w.worldManager.sceneBlockAoiMap[sceneId]
	if !exist {
		return false
	}
	return aoiManager.IsValidAoiPos(x, y, z)
}

func (w *World) IsValidAiWorldPos(sceneId uint32, x, y, z float32) bool {
	return w.aiWorldAoi.IsValidAoiPos(x, y, z)
}

func (w *WorldManager) GetMultiplayerWorldNum() uint32 {
	return w.multiplayerWorldNum
}

// EnterSceneContext 场景切换上下文 由 SceneTransToPointReq 等动作创建 在 EnterScene 4步状态机中跨步骤共享
// OldSceneId=0 表示首次进入（登录）
// DungeonId/DungeonPointId 副本相关 普通传送都为0
type EnterSceneContext struct {
	OldSceneId     uint32
	OldPos         *model.Vector
	NewSceneId     uint32
	NewPos         *model.Vector
	NewRot         *model.Vector
	DungeonId      uint32
	DungeonPointId uint32
	Uid            uint32
}

// World 世界数据结构 房主owner+playerMap里的Guest 各自有自己的activeAvatar 共享同一组Scene
// 普通玩家世界最多4人 AI世界不限人数（PUBG等大场玩法用）
type World struct {
	id                   uint64
	worldManager         *WorldManager
	owner                *model.Player                 // 世界拥有者（房主）
	playerMap            map[uint32]*model.Player      // 世界内所有玩家（含owner）
	sceneMap             map[uint32]*Scene             // 世界内所有场景实例 玩家穿越场景（蒙德/璃月）会创建新Scene
	enterSceneToken      uint32                        // 进入场景令牌 防伪用 每次切场景+100
	enterSceneContextMap map[uint32]*EnterSceneContext // 场景切换上下文 key:EnterSceneToken value:EnterSceneContext
	entityIdCounter      uint32                        // 世界的实体id生成计数器
	worldLevel           uint8                         // 世界等级
	multiplayer          bool                          // 是否多人世界
	mpLevelEntityId      uint32                        // 多人世界等级实体id 客户端同步多人世界等级用
	chatMsgList          []*proto.ChatInfo             // 世界聊天消息列表（最多保留100条）
	playerFirstEnterMap  map[uint32]int64              // 玩家第一次进入世界的时间 key:uid value:进入时间
	waitEnterPlayerMap   map[uint32]int64              // 进入世界的玩家等待列表（房主未完成进场前积压的Guest申请）
	multiplayerTeam      *MultiplayerTeam              // 多人队伍 把每个玩家的本地队伍合成worldTeam发给客户端
	peerList             []*model.Player               // 玩家编号列表 索引→peerId 客户端用peerId标识房间内玩家
	aiWorldAoi           *alg.AoiManager               // ai世界专属的aoi管理器（普通世界为nil）
	bulletPhysicsEngine  *PhysicsEngine                // 蓄力箭子弹物理引擎（仅AI世界有 PUBG用）
}

func (w *World) GetBulletPhysicsEngine() *PhysicsEngine {
	return w.bulletPhysicsEngine
}

func (w *World) GetId() uint64 {
	return w.id
}

func (w *World) GetWorldManager() *WorldManager {
	return w.worldManager
}

func (w *World) GetOwner() *model.Player {
	return w.owner
}

func (w *World) GetAllPlayer() map[uint32]*model.Player {
	return w.playerMap
}

func (w *World) GetAllScene() map[uint32]*Scene {
	return w.sceneMap
}

func (w *World) GetEnterSceneToken() uint32 {
	return w.enterSceneToken
}

func (w *World) GetEnterSceneContextByToken(token uint32) *EnterSceneContext {
	return w.enterSceneContextMap[token]
}

// AddEnterSceneContext 为新一次场景切换分配token+保存上下文
// token步长100是仿照官服设计 每次切换token产生100的间隙 便于日志/抓包定位
func (w *World) AddEnterSceneContext(ctx *EnterSceneContext) uint32 {
	w.enterSceneToken += 100
	w.enterSceneContextMap[w.enterSceneToken] = ctx
	return w.enterSceneToken
}

func (w *World) GetLastEnterSceneContextByUid(uid uint32) *EnterSceneContext {
	for token := w.enterSceneToken; token >= 5000; token -= 100 {
		ctx, exist := w.enterSceneContextMap[token]
		if !exist {
			continue
		}
		if ctx.Uid != uid {
			continue
		}
		return ctx
	}
	return nil
}

func (w *World) RemoveAllEnterSceneContextByUid(uid uint32) {
	for token := w.enterSceneToken; token >= 5000; token -= 100 {
		ctx, exist := w.enterSceneContextMap[token]
		if !exist {
			continue
		}
		if ctx.Uid != uid {
			continue
		}
		delete(w.enterSceneContextMap, token)
	}
}

func (w *World) GetWorldLevel() uint8 {
	return w.worldLevel
}

func (w *World) IsMultiplayerWorld() bool {
	return w.multiplayer
}

func (w *World) GetMpLevelEntityId() uint32 {
	return w.mpLevelEntityId
}

// GetNextWorldEntityId 生成场景实体id 高位编码entityType低位是计数器
// 因客户端版本不同 entityType所占bit数也不同（高版本为支持更多实体类型扩了bit）：
//
//	v6.5+    type<<21  type占11bit 计数器21bit
//	v6.0+    type<<22  type占10bit 计数器22bit
//	v5.x及更早 type<<24  type占8bit  计数器24bit
//
// 解析entityId时（如lua_func.go的GetEntityType）也要用相同分支 否则解码错乱
func (w *World) GetNextWorldEntityId(entityType uint8) uint32 {
	w.entityIdCounter++
	entityId := uint32(0)
	if w.GetOwner().ClientVersion >= 650 {
		entityId = (uint32(entityType) << 21) + w.entityIdCounter
	} else if w.GetOwner().ClientVersion >= 600 {
		entityId = (uint32(entityType) << 22) + w.entityIdCounter
	} else {
		entityId = (uint32(entityType) << 24) + w.entityIdCounter
	}
	return entityId
}

// GetPlayerPeerId 获取当前玩家世界内编号
func (w *World) GetPlayerPeerId(player *model.Player) uint32 {
	peerId := uint32(0)
	for peerIdIndex, worldPlayer := range w.peerList {
		if worldPlayer.PlayerId == player.PlayerId {
			peerId = uint32(peerIdIndex) + 1
		}
	}
	return peerId
}

// GetPlayerByPeerId 通过世界内编号获取玩家
func (w *World) GetPlayerByPeerId(peerId uint32) *model.Player {
	peerIdIndex := int(peerId) - 1
	if peerIdIndex >= len(w.peerList) {
		return nil
	}
	return w.peerList[peerIdIndex]
}

// GetWorldPlayerNum 获取世界中玩家的数量
func (w *World) GetWorldPlayerNum() int {
	return len(w.playerMap)
}

func (w *World) GetAiWorldAoi() *alg.AoiManager {
	return w.aiWorldAoi
}

func (w *World) AddPlayer(player *model.Player) {
	w.peerList = append(w.peerList, player)
	w.playerMap[player.PlayerId] = player

	// 将玩家自身当前的队伍角色信息复制到世界的玩家本地队伍
	dbTeam := player.GetDbTeam()
	team := dbTeam.GetActiveTeam()
	if w.worldManager.IsAiWorld(w) {
		w.SetPlayerLocalTeam(player, []uint32{dbTeam.GetActiveAvatarId()})
	} else {
		w.SetPlayerLocalTeam(player, team.GetAvatarIdList())
	}
	w.SetPlayerActiveAvatarId(player, dbTeam.GetActiveAvatarId())
	if w.worldManager.IsAiWorld(w) {
		w.AddMultiplayerTeam(player)
	} else {
		w.UpdateMultiplayerTeam()
	}

	scene := w.GetSceneById(player.GetSceneId())
	scene.AddPlayer(player)
	w.InitPlayerTeamEntityId(player)
}

func (w *World) RemovePlayer(player *model.Player) {
	peerId := w.GetPlayerPeerId(player)
	w.peerList = append(w.peerList[:peerId-1], w.peerList[peerId:]...)
	scene := w.sceneMap[player.GetSceneId()]
	scene.RemovePlayer(player)
	w.RemoveAllEnterSceneContextByUid(player.PlayerId)
	delete(w.playerMap, player.PlayerId)
	delete(w.playerFirstEnterMap, player.PlayerId)
	delete(w.multiplayerTeam.localTeamMap, player.PlayerId)
	delete(w.multiplayerTeam.localTeamEntityMap, player.PlayerId)
	delete(w.multiplayerTeam.localActiveAvatarMap, player.PlayerId)
	if w.worldManager.IsAiWorld(w) {
		w.RemoveMultiplayerTeam(player)
	} else {
		if player.PlayerId != w.owner.PlayerId {
			w.UpdateMultiplayerTeam()
		}
	}
}

// WorldAvatar 世界角色
type WorldAvatar struct {
	uid            uint32
	avatarId       uint32
	avatarEntityId uint32
	weaponEntityId uint32
	isActive       bool
	abilityMap     map[uint32]*proto.AbilityAppliedAbility
	modifierMap    map[uint32]*proto.AbilityAppliedModifier
}

func (w *WorldAvatar) GetUid() uint32 {
	return w.uid
}

func (w *WorldAvatar) GetAvatarId() uint32 {
	return w.avatarId
}

func (w *WorldAvatar) GetAvatarEntityId() uint32 {
	return w.avatarEntityId
}

func (w *WorldAvatar) GetWeaponEntityId() uint32 {
	return w.weaponEntityId
}

func (w *WorldAvatar) GetIsActive() bool {
	return w.isActive
}

func (w *WorldAvatar) SetAvatarEntityId(avatarEntityId uint32) {
	w.avatarEntityId = avatarEntityId
}

func (w *WorldAvatar) SetWeaponEntityId(weaponEntityId uint32) {
	w.weaponEntityId = weaponEntityId
}

func (w *WorldAvatar) SetIsActive(isActive bool) {
	w.isActive = isActive
}

func (w *WorldAvatar) AddAbility(ability *proto.AbilityAppliedAbility) {
	w.abilityMap[ability.InstancedAbilityId] = ability
}

func (w *WorldAvatar) GetAbilityByInstanceId(instanceId uint32) *proto.AbilityAppliedAbility {
	return w.abilityMap[instanceId]
}

func (w *WorldAvatar) PacketAbilityList() []*proto.AbilityAppliedAbility {
	abilityList := make([]*proto.AbilityAppliedAbility, 0)
	for _, ability := range w.abilityMap {
		abilityList = append(abilityList, ability)
	}
	return abilityList
}

func (w *WorldAvatar) AddModifier(modifier *proto.AbilityAppliedModifier) {
	w.modifierMap[modifier.InstancedModifierId] = modifier
}

func (w *WorldAvatar) GetModifierByInstanceId(instanceId uint32) *proto.AbilityAppliedModifier {
	return w.modifierMap[instanceId]
}

func (w *WorldAvatar) PacketModifierList() []*proto.AbilityAppliedModifier {
	modifierList := make([]*proto.AbilityAppliedModifier, 0)
	for _, modifier := range w.modifierMap {
		modifierList = append(modifierList, modifier)
	}
	return modifierList
}

// GetWorldAvatarList 获取世界队伍的全部角色列表
func (w *World) GetWorldAvatarList() []*WorldAvatar {
	worldAvatarList := make([]*WorldAvatar, 0)
	for _, worldAvatar := range w.multiplayerTeam.worldTeam {
		if worldAvatar.GetUid() == 0 {
			continue
		}
		worldAvatarList = append(worldAvatarList, worldAvatar)
	}
	return worldAvatarList
}

// GetPlayerWorldAvatar 获取某玩家在世界队伍中的某角色
func (w *World) GetPlayerWorldAvatar(player *model.Player, avatarId uint32) *WorldAvatar {
	for _, worldAvatar := range w.GetWorldAvatarList() {
		if worldAvatar.GetUid() == player.PlayerId && worldAvatar.GetAvatarId() == avatarId {
			return worldAvatar
		}
	}
	return nil
}

// GetPlayerWorldAvatarList 获取某玩家在世界队伍中的所有角色列表
func (w *World) GetPlayerWorldAvatarList(player *model.Player) []*WorldAvatar {
	worldAvatarList := make([]*WorldAvatar, 0)
	for _, worldAvatar := range w.GetWorldAvatarList() {
		if worldAvatar.GetUid() == player.PlayerId {
			worldAvatarList = append(worldAvatarList, worldAvatar)
		}
	}
	return worldAvatarList
}

// GetWorldAvatarByEntityId 通过场景实体id获取世界队伍中的角色
func (w *World) GetWorldAvatarByEntityId(avatarEntityId uint32) *WorldAvatar {
	for _, worldAvatar := range w.GetWorldAvatarList() {
		if worldAvatar.GetAvatarEntityId() == avatarEntityId {
			return worldAvatar
		}
	}
	return nil
}

// UpdatePlayerWorldAvatar 更新某玩家在世界队伍中的所有角色
func (w *World) UpdatePlayerWorldAvatar(player *model.Player) {
	scene := w.GetSceneById(player.GetSceneId())
	for _, worldAvatar := range w.GetPlayerWorldAvatarList(player) {
		if worldAvatar.GetAvatarEntityId() != 0 {
			continue
		}
		worldAvatar.SetAvatarEntityId(scene.CreateEntityAvatar(player, worldAvatar.GetAvatarId()))
		worldAvatar.SetWeaponEntityId(scene.CreateEntityWeapon(player.GetPos(), player.GetRot()))
	}
}

// GetPlayerTeamEntityId 获取某玩家的本地队伍实体id
func (w *World) GetPlayerTeamEntityId(player *model.Player) uint32 {
	return w.multiplayerTeam.localTeamEntityMap[player.PlayerId]
}

// InitPlayerTeamEntityId 初始化某玩家的本地队伍实体id
func (w *World) InitPlayerTeamEntityId(player *model.Player) {
	w.multiplayerTeam.localTeamEntityMap[player.PlayerId] = w.GetNextWorldEntityId(constant.ENTITY_TYPE_TEAM)
}

// GetPlayerWorldAvatarEntityId 获取某玩家在世界队伍中的某角色的实体id
func (w *World) GetPlayerWorldAvatarEntityId(player *model.Player, avatarId uint32) uint32 {
	worldAvatar := w.GetPlayerWorldAvatar(player, avatarId)
	if worldAvatar == nil {
		return 0
	}
	return worldAvatar.GetAvatarEntityId()
}

// GetPlayerWorldAvatarWeaponEntityId 获取某玩家在世界队伍中的某角色的武器的实体id
func (w *World) GetPlayerWorldAvatarWeaponEntityId(player *model.Player, avatarId uint32) uint32 {
	worldAvatar := w.GetPlayerWorldAvatar(player, avatarId)
	if worldAvatar == nil {
		return 0
	}
	return worldAvatar.GetWeaponEntityId()
}

// GetPlayerActiveAvatarId 获取玩家当前活跃角色id
func (w *World) GetPlayerActiveAvatarId(player *model.Player) uint32 {
	return w.multiplayerTeam.localActiveAvatarMap[player.PlayerId]
}

// SetPlayerActiveAvatarId 设置玩家当前活跃角色id
func (w *World) SetPlayerActiveAvatarId(player *model.Player, avatarId uint32) {
	localTeam := w.GetPlayerLocalTeam(player)
	for _, worldAvatar := range localTeam {
		if worldAvatar.GetAvatarId() == avatarId {
			w.multiplayerTeam.localActiveAvatarMap[player.PlayerId] = avatarId
			worldAvatar.SetIsActive(true)
		} else {
			worldAvatar.SetIsActive(false)
		}
	}
}

// GetPlayerAvatarIndexByAvatarId 获取玩家某角色的索引
func (w *World) GetPlayerAvatarIndexByAvatarId(player *model.Player, avatarId uint32) int {
	localTeam := w.GetPlayerLocalTeam(player)
	for index, worldAvatar := range localTeam {
		if worldAvatar.GetAvatarId() == avatarId {
			return index
		}
	}
	return -1
}

// GetPlayerActiveAvatarEntity 获取玩家当前活跃角色场景实体
func (w *World) GetPlayerActiveAvatarEntity(player *model.Player) IEntity {
	activeAvatarId := w.GetPlayerActiveAvatarId(player)
	avatarEntityId := w.GetPlayerWorldAvatarEntityId(player, activeAvatarId)
	scene := w.GetSceneById(player.GetSceneId())
	entity := scene.GetEntity(avatarEntityId)
	return entity
}

// IsPlayerActiveAvatarEntity 是否为玩家当前活跃角色场景实体
func (w *World) IsPlayerActiveAvatarEntity(player *model.Player, entityId uint32) bool {
	entity := w.GetPlayerActiveAvatarEntity(player)
	if entity == nil {
		return false
	}
	return entity.GetId() == entityId
}

type MultiplayerTeam struct {
	// key:uid value:玩家的本地队伍
	localTeamMap map[uint32][]*WorldAvatar
	// key:uid value:玩家的本地队伍实体id
	localTeamEntityMap map[uint32]uint32
	// key:uid value:玩家当前活跃角色id
	localActiveAvatarMap map[uint32]uint32
	// 最终的世界队伍
	worldTeam []*WorldAvatar
}

func CreateMultiplayerTeam() (r *MultiplayerTeam) {
	r = new(MultiplayerTeam)
	r.localTeamMap = make(map[uint32][]*WorldAvatar)
	r.localTeamEntityMap = make(map[uint32]uint32)
	r.localActiveAvatarMap = make(map[uint32]uint32)
	r.worldTeam = make([]*WorldAvatar, 0)
	return r
}

func (w *World) GetPlayerLocalTeam(player *model.Player) []*WorldAvatar {
	return w.multiplayerTeam.localTeamMap[player.PlayerId]
}

func (w *World) SetPlayerLocalTeam(player *model.Player, avatarIdList []uint32) {
	oldLocalTeam := w.multiplayerTeam.localTeamMap[player.PlayerId]
	sameAvatarIdList := make([]uint32, 0)
	addAvatarIdList := make([]uint32, 0)
	for _, avatarId := range avatarIdList {
		exist := false
		for _, worldAvatar := range oldLocalTeam {
			if worldAvatar.GetAvatarId() == avatarId {
				exist = true
			}
		}
		if exist {
			sameAvatarIdList = append(sameAvatarIdList, avatarId)
		} else {
			addAvatarIdList = append(addAvatarIdList, avatarId)
		}
	}
	newLocalTeam := make([]*WorldAvatar, len(avatarIdList))
	for _, avatarId := range sameAvatarIdList {
		for _, worldAvatar := range oldLocalTeam {
			if worldAvatar.GetAvatarId() == avatarId {
				index := 0
				for i, v := range avatarIdList {
					if avatarId == v {
						index = i
					}
				}
				newLocalTeam[index] = worldAvatar
			}
		}
	}
	for _, avatarId := range addAvatarIdList {
		index := 0
		for i, v := range avatarIdList {
			if avatarId == v {
				index = i
			}
		}
		newLocalTeam[index] = &WorldAvatar{
			uid:            player.PlayerId,
			avatarId:       avatarId,
			avatarEntityId: 0,
			weaponEntityId: 0,
			abilityMap:     make(map[uint32]*proto.AbilityAppliedAbility),
			modifierMap:    make(map[uint32]*proto.AbilityAppliedModifier),
			isActive:       false,
		}
	}
	scene := w.GetSceneById(player.GetSceneId())
	for _, worldAvatar := range oldLocalTeam {
		exist := false
		for _, avatarId := range avatarIdList {
			if worldAvatar.GetAvatarId() == avatarId {
				exist = true
			}
		}
		if !exist {
			scene.DestroyEntity(worldAvatar.GetAvatarEntityId())
			scene.DestroyEntity(worldAvatar.GetWeaponEntityId())
		}
	}
	w.multiplayerTeam.localTeamMap[player.PlayerId] = newLocalTeam
}

// 为了实现大世界无限人数写的
// 现在看来把世界里所有人放进队伍里发给客户端超过8个客户端会崩溃
// 看来还是不能简单的走通用逻辑 需要对大世界场景队伍做特殊处理 欺骗客户端其他玩家仅仅以场景角色实体的形式出现

// AddMultiplayerTeam AI世界专用：把玩家本地队伍直接追加到worldTeam（不限4人）
func (w *World) AddMultiplayerTeam(player *model.Player) {
	localTeam := w.GetPlayerLocalTeam(player)
	w.multiplayerTeam.worldTeam = append(w.multiplayerTeam.worldTeam, localTeam...)
}

func (w *World) RemoveMultiplayerTeam(player *model.Player) {
	worldTeam := make([]*WorldAvatar, 0)
	for _, worldAvatar := range w.multiplayerTeam.worldTeam {
		if worldAvatar.GetUid() == player.PlayerId {
			continue
		}
		worldTeam = append(worldTeam, worldAvatar)
	}
	w.multiplayerTeam.worldTeam = worldTeam
}

// UpdateMultiplayerTeam 普通世界专用：整合所有玩家本地队伍 强制构造出4个槽位的worldTeam
// 原神协议层面世界队伍固定4个槽位 服务端按玩家数动态分配：
//
//	1人 → 1P×4（同一玩家队伍填满4槽 单人体验保持4个角色可切换）
//	2人 → 1P×2 + 2P×2
//	3人 → 1P×2 + 2P×1 + 3P×1（房主多1槽）
//	4人 → 各占1槽
//
// 超过4人会直接return（普通世界硬上限）AI世界走 AddMultiplayerTeam 不受此限
func (w *World) UpdateMultiplayerTeam() {
	playerNum := w.GetWorldPlayerNum()
	if playerNum > 4 {
		return
	}
	w.multiplayerTeam.worldTeam = make([]*WorldAvatar, 4)
	switch playerNum {
	case 1:
		// 1P*4
		p1 := w.GetPlayerByPeerId(1)
		p1LocalTeam := w.GetPlayerLocalTeam(p1)
		for index := 0; index <= 3; index++ {
			worldAvatar := &WorldAvatar{
				uid:            0,
				avatarId:       0,
				avatarEntityId: 0,
				weaponEntityId: 0,
				abilityMap:     nil,
				modifierMap:    nil,
				isActive:       false,
			}
			if index < len(p1LocalTeam) {
				worldAvatar = p1LocalTeam[index]
			}
			w.multiplayerTeam.worldTeam[index] = worldAvatar
		}
	case 2:
		// 1P*2 + 2P*2
		for index := 0; index <= 3; index++ {
			switch index {
			case 0:
				w.multiplayerTeam.worldTeam[index] = w.SelectPlayerWorldAvatar(1, true)
			case 1:
				w.multiplayerTeam.worldTeam[index] = w.SelectPlayerWorldAvatar(1, false)
			case 2:
				w.multiplayerTeam.worldTeam[index] = w.SelectPlayerWorldAvatar(2, true)
			case 3:
				w.multiplayerTeam.worldTeam[index] = w.SelectPlayerWorldAvatar(2, false)
			}
		}
	case 3:
		// 1P*2 + 2P*1 + 3P*1
		for index := 0; index <= 3; index++ {
			switch index {
			case 0:
				w.multiplayerTeam.worldTeam[index] = w.SelectPlayerWorldAvatar(1, true)
			case 1:
				w.multiplayerTeam.worldTeam[index] = w.SelectPlayerWorldAvatar(1, false)
			case 2:
				w.multiplayerTeam.worldTeam[index] = w.SelectPlayerWorldAvatar(2, true)
			case 3:
				w.multiplayerTeam.worldTeam[index] = w.SelectPlayerWorldAvatar(3, true)
			}
		}
	case 4:
		// 1P*1 + 2P*1 + 3P*1 + 4P*1
		for index := 0; index <= 3; index++ {
			switch index {
			case 0:
				w.multiplayerTeam.worldTeam[index] = w.SelectPlayerWorldAvatar(1, true)
			case 1:
				w.multiplayerTeam.worldTeam[index] = w.SelectPlayerWorldAvatar(2, true)
			case 2:
				w.multiplayerTeam.worldTeam[index] = w.SelectPlayerWorldAvatar(3, true)
			case 3:
				w.multiplayerTeam.worldTeam[index] = w.SelectPlayerWorldAvatar(4, true)
			}
		}
	}
}

func (w *World) SelectPlayerWorldAvatar(peerId uint32, active bool) *WorldAvatar {
	worldAvatar := &WorldAvatar{
		uid:            0,
		avatarId:       0,
		avatarEntityId: 0,
		weaponEntityId: 0,
		abilityMap:     nil,
		modifierMap:    nil,
		isActive:       false,
	}
	player := w.GetPlayerByPeerId(peerId)
	localTeam := w.GetPlayerLocalTeam(player)
	activeAvatarId := w.GetPlayerActiveAvatarId(player)
	for _, wa := range localTeam {
		if active {
			if wa.GetAvatarId() == activeAvatarId {
				worldAvatar = wa
				break
			}
		} else {
			if wa.GetAvatarId() != activeAvatarId {
				worldAvatar = wa
				break
			}
		}
	}
	return worldAvatar
}

// 世界聊天

func (w *World) AddChat(chatInfo *proto.ChatInfo) {
	if len(w.chatMsgList) > 100 {
		w.chatMsgList = w.chatMsgList[1:]
	}
	w.chatMsgList = append(w.chatMsgList, chatInfo)
}

func (w *World) GetChatList() []*proto.ChatInfo {
	return w.chatMsgList
}

// ChangeToMultiplayer 把单人世界转为多人世界
// 仅当其他玩家敲门成功时调用 全服多人世界数受 MAX_MULTIPLAYER_WORLD_NUM 限制
func (w *World) ChangeToMultiplayer() {
	w.worldManager.multiplayerWorldNum++
	w.multiplayer = true
	w.owner.IsInMp = true
}

// IsPlayerFirstEnter 获取玩家是否首次加入本世界
func (w *World) IsPlayerFirstEnter(player *model.Player) bool {
	_, exist := w.playerFirstEnterMap[player.PlayerId]
	if !exist {
		return true
	} else {
		return false
	}
}

func (w *World) PlayerEnter(uid uint32) {
	w.playerFirstEnterMap[uid] = time.Now().UnixMilli()
}

func (w *World) AddWaitPlayer(uid uint32) {
	w.waitEnterPlayerMap[uid] = time.Now().UnixMilli()
}

func (w *World) GetAllWaitPlayer() []uint32 {
	uidList := make([]uint32, 0)
	for uid := range w.waitEnterPlayerMap {
		uidList = append(uidList, uid)
	}
	return uidList
}

func (w *World) RemoveWaitPlayer(uid uint32) {
	delete(w.waitEnterPlayerMap, uid)
}

// GetGameTime 获取玩家累计游戏时间（秒 一直累加 离线不计时 不循环）
// 数据存在房主的存档里 多人世界使用房主的时间
// 客户端拿这个秒数 mod 86400 后映射到当天某个时刻 用来驱动昼夜变化
func (w *World) GetGameTime() uint32 {
	return w.GetOwner().GetDbWorld().GameTime
}

// ChangeGameTime 改变游戏时间（累计秒数 不做 mod）
// 由 onTickSecond 每秒+1 也可由任务执行/Lua脚本设置到某个目标时刻
func (w *World) ChangeGameTime(time uint32) {
	w.GetOwner().GetDbWorld().GameTime = time
}

// CreateScene 创建一个场景实例 仅由 GetSceneById 在懒加载时调用
func (w *World) CreateScene(sceneId uint32) *Scene {
	scene := &Scene{
		id:         sceneId,
		world:      w,
		playerMap:  make(map[uint32]*model.Player),
		entityMap:  make(map[uint32]IEntity),
		groupMap:   make(map[uint32]*Group),
		createTime: time.Now().UnixMilli(),
		meeoIndex:  0,
	}
	w.sceneMap[sceneId] = scene
	return scene
}

func (w *World) GetSceneById(sceneId uint32) *Scene {
	// 场景是取时创建 可以简化代码不判空
	scene, exist := w.sceneMap[sceneId]
	if !exist {
		scene = w.CreateScene(sceneId)
	}
	return scene
}
