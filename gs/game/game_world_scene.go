package game

import (
	"fmt"
	"math"
	"strconv"
	"time"

	"hk4e/common/constant"
	"hk4e/gdconf"
	"hk4e/gs/model"
	"hk4e/pkg/endec"
	"hk4e/protocol/proto"

	"github.com/dengsgo/math-engine/engine"
	"github.com/flswld/halo/logger"
)

// 场景与实体系统
//
// 层级模型：
//   World          世界（多人/单人）
//     └─ Scene     场景（蒙德/璃月/副本等独立地图）
//          ├─ Entity    实体（玩家角色/怪物/物件等 都是IEntity实现）
//          └─ Group     按group→suite分类的实体集合（与Lua trigger组织单元对应）
//                └─ Suite  group的某个状态变体（怪在/不在/物件状态）
//
// 实体类型（IEntity 9种实现）：
//   AvatarEntity       玩家角色
//   WeaponEntity       武器（角色携带 显示在角色右手）
//   MonsterEntity      怪物
//   NpcEntity          NPC
//   GadgetNormalEntity 普通物件
//   GadgetTrifleItemEntity 掉落物
//   GadgetGatherEntity 采集物（矿石/植物等）
//   GadgetWorktopEntity 操作台/机关
//   GadgetClientEntity 客户端创建的物件（由客户端发EvtCreateGadgetNotify触发）
//   GadgetVehicleEntity 载具（浪船等）
//
// Ability系统：每个entity带 abilityMap+modifierMap 加载ConfigAbility数据
// AbilityAction当前仅实现6种 大量技能效果未模拟（详见CLAUDE.md "Ability系统现状"）

// Scene 场景数据结构 由 World.GetSceneById 懒加载创建
type Scene struct {
	id          uint32
	world       *World
	playerMap   map[uint32]*model.Player
	entityMap   map[uint32]IEntity // 场景中全部的实体（含所有group的合集）
	groupMap    map[uint32]*Group  // 场景中按group->suite分类的实体（与Lua scene_lua_config对齐）
	createTime  int64              // 场景创建时间 GetSceneTime用于计算累计场景时间
	meeoIndex   uint32             // 客户端风元素染色同步协议的计数器（万叶/温迪等）
	monsterWudi bool               // 是否开启场景内怪物无敌（GM调试用）
}

func (s *Scene) GetId() uint32 {
	return s.id
}

func (s *Scene) GetWorld() *World {
	return s.world
}

func (s *Scene) GetAllPlayer() map[uint32]*model.Player {
	return s.playerMap
}

func (s *Scene) GetAllEntity() map[uint32]IEntity {
	return s.entityMap
}

func (s *Scene) GetGroupById(groupId uint32) *Group {
	return s.groupMap[groupId]
}

func (s *Scene) GetAllGroup() map[uint32]*Group {
	return s.groupMap
}

func (s *Scene) GetMeeoIndex() uint32 {
	return s.meeoIndex
}

func (s *Scene) SetMeeoIndex(meeoIndex uint32) {
	s.meeoIndex = meeoIndex
}

func (s *Scene) GetMonsterWudi() bool {
	return s.monsterWudi
}

func (s *Scene) SetMonsterWudi(monsterWudi bool) {
	s.monsterWudi = monsterWudi
}

func (s *Scene) GetSceneCreateTime() int64 {
	return s.createTime
}

func (s *Scene) GetSceneTime() int64 {
	now := time.Now().UnixMilli()
	return now - s.createTime
}

// AddPlayer 玩家加入场景 同时为玩家本地队伍的每个角色创建AvatarEntity+WeaponEntity
// 角色实体id持有在 worldAvatar 内 切换角色时只是改 isActive 不重建实体
func (s *Scene) AddPlayer(player *model.Player) {
	s.playerMap[player.PlayerId] = player
	for _, worldAvatar := range s.world.GetPlayerWorldAvatarList(player) {
		worldAvatar.SetAvatarEntityId(s.CreateEntityAvatar(player, worldAvatar.GetAvatarId()))
		worldAvatar.SetWeaponEntityId(s.CreateEntityWeapon(player.GetPos(), player.GetRot()))
	}
}

// RemovePlayer 玩家离开场景 销毁该玩家的所有Avatar+Weapon实体
func (s *Scene) RemovePlayer(player *model.Player) {
	delete(s.playerMap, player.PlayerId)
	worldAvatarList := s.world.GetPlayerWorldAvatarList(player)
	for _, worldAvatar := range worldAvatarList {
		s.DestroyEntity(worldAvatar.GetAvatarEntityId())
		s.DestroyEntity(worldAvatar.GetWeaponEntityId())
	}
}

func (s *Scene) CreateEntityAvatar(player *model.Player, avatarId uint32) uint32 {
	entityId := s.world.GetNextWorldEntityId(constant.ENTITY_TYPE_AVATAR)
	dbAvatar := player.GetDbAvatar()
	avatar := dbAvatar.GetAvatarById(avatarId)
	if avatar == nil {
		logger.Error("get avatar is nil, avatarId: %v", avatar)
		return 0
	}
	entity := &AvatarEntity{
		Entity: &Entity{
			id:          entityId,
			scene:       s,
			lifeState:   avatar.LifeState,
			pos:         player.GetPos(),
			rot:         player.GetRot(),
			moveState:   uint16(proto.MotionState_MOTION_NONE),
			fightProp:   avatar.FightPropMap, // 使用角色结构的数据
			entityType:  constant.ENTITY_TYPE_AVATAR,
			visionLevel: constant.VISION_LEVEL_NORMAL,
		},
		uid:      player.PlayerId,
		avatarId: avatarId,
	}
	return s.CreateEntity(entity)
}

func (s *Scene) CreateEntityWeapon(pos, rot *model.Vector) uint32 {
	entityId := s.world.GetNextWorldEntityId(constant.ENTITY_TYPE_WEAPON)
	entity := &WeaponEntity{
		&Entity{
			id:        entityId,
			scene:     s,
			lifeState: constant.LIFE_STATE_ALIVE,
			pos:       &model.Vector{X: pos.X, Y: pos.Y, Z: pos.Z},
			rot:       &model.Vector{X: rot.X, Y: rot.Y, Z: rot.Z},
			moveState: uint16(proto.MotionState_MOTION_NONE),
			fightProp: map[uint32]float32{
				constant.FIGHT_PROP_CUR_HP:  math.MaxFloat32,
				constant.FIGHT_PROP_MAX_HP:  math.MaxFloat32,
				constant.FIGHT_PROP_BASE_HP: float32(1),
			},
			entityType:  constant.ENTITY_TYPE_WEAPON,
			visionLevel: constant.VISION_LEVEL_NORMAL,
		},
	}
	return s.CreateEntity(entity)
}

func (s *Scene) CreateEntityMonster(pos, rot *model.Vector, level uint8, configId, groupId uint32, visionLevel int) *MonsterEntity {
	entityId := s.world.GetNextWorldEntityId(constant.ENTITY_TYPE_MONSTER)
	entity := &MonsterEntity{
		Entity: &Entity{
			id:          entityId,
			scene:       s,
			lifeState:   constant.LIFE_STATE_ALIVE,
			pos:         &model.Vector{X: pos.X, Y: pos.Y, Z: pos.Z},
			rot:         &model.Vector{X: rot.X, Y: rot.Y, Z: rot.Z},
			moveState:   uint16(proto.MotionState_MOTION_NONE),
			level:       level,
			entityType:  constant.ENTITY_TYPE_MONSTER,
			configId:    configId,
			groupId:     groupId,
			visionLevel: visionLevel,
		},
	}
	return entity
}

func (s *Scene) CreateEntityNpc(pos, rot *model.Vector, configId, groupId uint32) *NpcEntity {
	entityId := s.world.GetNextWorldEntityId(constant.ENTITY_TYPE_NPC)
	entity := &NpcEntity{
		Entity: &Entity{
			id:        entityId,
			scene:     s,
			lifeState: constant.LIFE_STATE_ALIVE,
			pos:       &model.Vector{X: pos.X, Y: pos.Y, Z: pos.Z},
			rot:       &model.Vector{X: rot.X, Y: rot.Y, Z: rot.Z},
			moveState: uint16(proto.MotionState_MOTION_NONE),
			fightProp: map[uint32]float32{
				constant.FIGHT_PROP_CUR_HP:  math.MaxFloat32,
				constant.FIGHT_PROP_MAX_HP:  math.MaxFloat32,
				constant.FIGHT_PROP_BASE_HP: float32(1),
			},
			entityType:  constant.ENTITY_TYPE_NPC,
			configId:    configId,
			groupId:     groupId,
			visionLevel: constant.VISION_LEVEL_NORMAL,
		},
	}
	return entity
}

func (s *Scene) CreateEntityGadgetNormal(pos, rot *model.Vector, configId, groupId uint32, visionLevel int, gadgetId, gadgetState uint32) *GadgetNormalEntity {
	entityId := s.world.GetNextWorldEntityId(constant.ENTITY_TYPE_GADGET)
	entity := &GadgetNormalEntity{
		GadgetEntity: &GadgetEntity{
			Entity: &Entity{
				id:        entityId,
				scene:     s,
				lifeState: constant.LIFE_STATE_ALIVE,
				pos:       &model.Vector{X: pos.X, Y: pos.Y, Z: pos.Z},
				rot:       &model.Vector{X: rot.X, Y: rot.Y, Z: rot.Z},
				moveState: uint16(proto.MotionState_MOTION_NONE),
				fightProp: map[uint32]float32{
					constant.FIGHT_PROP_CUR_HP:  math.MaxFloat32,
					constant.FIGHT_PROP_MAX_HP:  math.MaxFloat32,
					constant.FIGHT_PROP_BASE_HP: float32(1),
				},
				entityType:  constant.ENTITY_TYPE_GADGET,
				configId:    configId,
				groupId:     groupId,
				visionLevel: visionLevel,
			},
			gadgetId:    gadgetId,
			gadgetState: gadgetState,
		},
	}
	return entity
}

func (s *Scene) CreateEntityGadgetTrifleItem(pos, rot *model.Vector, visionLevel int, gadgetId, gadgetState uint32) *GadgetTrifleItemEntity {
	entityId := s.world.GetNextWorldEntityId(constant.ENTITY_TYPE_GADGET)
	entity := &GadgetTrifleItemEntity{
		GadgetEntity: &GadgetEntity{
			Entity: &Entity{
				id:        entityId,
				scene:     s,
				lifeState: constant.LIFE_STATE_ALIVE,
				pos:       &model.Vector{X: pos.X, Y: pos.Y, Z: pos.Z},
				rot:       &model.Vector{X: rot.X, Y: rot.Y, Z: rot.Z},
				moveState: uint16(proto.MotionState_MOTION_NONE),
				fightProp: map[uint32]float32{
					constant.FIGHT_PROP_CUR_HP:  math.MaxFloat32,
					constant.FIGHT_PROP_MAX_HP:  math.MaxFloat32,
					constant.FIGHT_PROP_BASE_HP: float32(1),
				},
				entityType:  constant.ENTITY_TYPE_GADGET,
				visionLevel: visionLevel,
			},
			gadgetId:    gadgetId,
			gadgetState: gadgetState,
		},
	}
	return entity
}

func (s *Scene) CreateEntityGadgetGather(pos, rot *model.Vector, configId, groupId uint32, visionLevel int, gadgetId, gadgetState uint32) *GadgetGatherEntity {
	entityId := s.world.GetNextWorldEntityId(constant.ENTITY_TYPE_GADGET)
	entity := &GadgetGatherEntity{
		GadgetEntity: &GadgetEntity{
			Entity: &Entity{
				id:        entityId,
				scene:     s,
				lifeState: constant.LIFE_STATE_ALIVE,
				pos:       &model.Vector{X: pos.X, Y: pos.Y, Z: pos.Z},
				rot:       &model.Vector{X: rot.X, Y: rot.Y, Z: rot.Z},
				moveState: uint16(proto.MotionState_MOTION_NONE),
				fightProp: map[uint32]float32{
					constant.FIGHT_PROP_CUR_HP:  math.MaxFloat32,
					constant.FIGHT_PROP_MAX_HP:  math.MaxFloat32,
					constant.FIGHT_PROP_BASE_HP: float32(1),
				},
				entityType:  constant.ENTITY_TYPE_GADGET,
				configId:    configId,
				groupId:     groupId,
				visionLevel: visionLevel,
			},
			gadgetId:    gadgetId,
			gadgetState: gadgetState,
		},
	}
	return entity
}

func (s *Scene) CreateEntityGadgetWorktop(pos, rot *model.Vector, configId, groupId uint32, visionLevel int, gadgetId, gadgetState uint32) *GadgetWorktopEntity {
	entityId := s.world.GetNextWorldEntityId(constant.ENTITY_TYPE_GADGET)
	entity := &GadgetWorktopEntity{
		GadgetEntity: &GadgetEntity{
			Entity: &Entity{
				id:        entityId,
				scene:     s,
				lifeState: constant.LIFE_STATE_ALIVE,
				pos:       &model.Vector{X: pos.X, Y: pos.Y, Z: pos.Z},
				rot:       &model.Vector{X: rot.X, Y: rot.Y, Z: rot.Z},
				moveState: uint16(proto.MotionState_MOTION_NONE),
				fightProp: map[uint32]float32{
					constant.FIGHT_PROP_CUR_HP:  math.MaxFloat32,
					constant.FIGHT_PROP_MAX_HP:  math.MaxFloat32,
					constant.FIGHT_PROP_BASE_HP: float32(1),
				},
				entityType:  constant.ENTITY_TYPE_GADGET,
				configId:    configId,
				groupId:     groupId,
				visionLevel: visionLevel,
			},
			gadgetId:    gadgetId,
			gadgetState: gadgetState,
		},
		optionMap: make(map[uint32]struct{}),
	}
	return entity
}

func (s *Scene) CreateEntityGadgetClient(entityId uint32, pos, rot *model.Vector, gadgetId uint32) *GadgetClientEntity {
	entity := &GadgetClientEntity{
		GadgetEntity: &GadgetEntity{
			Entity: &Entity{
				id:        entityId,
				scene:     s,
				lifeState: constant.LIFE_STATE_ALIVE,
				pos:       &model.Vector{X: pos.X, Y: pos.Y, Z: pos.Z},
				rot:       &model.Vector{X: rot.X, Y: rot.Y, Z: rot.Z},
				moveState: uint16(proto.MotionState_MOTION_NONE),
				fightProp: map[uint32]float32{
					constant.FIGHT_PROP_CUR_HP:  math.MaxFloat32,
					constant.FIGHT_PROP_MAX_HP:  math.MaxFloat32,
					constant.FIGHT_PROP_BASE_HP: float32(1),
				},
				entityType:  constant.ENTITY_TYPE_GADGET,
				visionLevel: constant.VISION_LEVEL_NORMAL,
			},
			gadgetId: gadgetId,
		},
	}
	return entity
}

func (s *Scene) CreateEntityGadgetVehicle(pos, rot *model.Vector, gadgetId uint32) *GadgetVehicleEntity {
	entityId := s.world.GetNextWorldEntityId(constant.ENTITY_TYPE_GADGET)
	entity := &GadgetVehicleEntity{
		GadgetEntity: &GadgetEntity{
			Entity: &Entity{
				id:          entityId,
				scene:       s,
				lifeState:   constant.LIFE_STATE_ALIVE,
				pos:         &model.Vector{X: pos.X, Y: pos.Y, Z: pos.Z},
				rot:         &model.Vector{X: rot.X, Y: rot.Y, Z: rot.Z},
				moveState:   uint16(proto.MotionState_MOTION_NONE),
				entityType:  constant.ENTITY_TYPE_GADGET,
				visionLevel: constant.VISION_LEVEL_NORMAL,
			},
			gadgetId: gadgetId,
		},
	}
	return entity
}

// CreateEntity 把已构造好的Entity加入场景
// 防爆保护：场景实体数量超过ENTITY_MAX_SEND_NUM(10000)直接拒绝（除非ENTITY_NUM_UNLIMIT=true）
// 自动调用InitAbility 加载该实体配置中的Ability数据
func (s *Scene) CreateEntity(entity IEntity) uint32 {
	if len(s.entityMap) >= ENTITY_MAX_SEND_NUM && !ENTITY_NUM_UNLIMIT {
		logger.Error("above max scene entity num limit: %v, id: %v, pos: %v", ENTITY_MAX_SEND_NUM, entity.GetId(), entity.GetPos())
		return 0
	}
	s.entityMap[entity.GetId()] = entity
	entity.InitAbility()
	return entity.GetId()
}

func (s *Scene) DestroyEntity(entityId uint32) {
	entity := s.GetEntity(entityId)
	if entity == nil {
		return
	}
	delete(s.entityMap, entity.GetId())
}

func (s *Scene) GetEntity(entityId uint32) IEntity {
	return s.entityMap[entityId]
}

// AddGroupSuite 把一组实体登记到 group→suite 分类下
// 同一group可加载多个suite共存（如基础地形+玩家额外触发出来的）
// 保留group/suite的归属关系是为了 Suite 切换时定位要销毁的实体
func (s *Scene) AddGroupSuite(groupId uint32, suiteId uint8, entityMap map[uint32]IEntity) {
	group, exist := s.groupMap[groupId]
	if !exist {
		group = &Group{
			id:       groupId,
			suiteMap: make(map[uint8]*Suite),
		}
		s.groupMap[groupId] = group
	}
	suite, exist := group.suiteMap[suiteId]
	if !exist {
		suite = &Suite{
			id:        suiteId,
			entityMap: make(map[uint32]IEntity),
		}
		group.suiteMap[suiteId] = suite
	}
	for k, v := range entityMap {
		suite.entityMap[k] = v
	}
}

func (s *Scene) RemoveGroupSuite(groupId uint32, suiteId uint8) {
	group := s.groupMap[groupId]
	if group == nil {
		logger.Error("group not exist, groupId: %v", groupId)
		return
	}
	suite := group.suiteMap[suiteId]
	if suite == nil {
		logger.Error("suite not exist, suiteId: %v", suiteId)
		return
	}
	for _, entity := range suite.entityMap {
		s.DestroyEntity(entity.GetId())
	}
	delete(group.suiteMap, suiteId)
	if len(group.suiteMap) == 0 {
		delete(s.groupMap, groupId)
	}
}

// Group 场景小组 一处场景小角落（如"3只丘丘人+1个宝箱"）的实体集合
// 与 gdconf 的 scene_lua_config 中Group对齐 是Lua trigger的组织单元
type Group struct {
	id       uint32
	suiteMap map[uint8]*Suite
}

// Suite group的状态变体
// 例：怪都还在=suite1（初始）/怪打完宝箱出现=suite2/玩家拿走宝箱=suite3（完成态）
// Lua脚本通过 ChangeGroupVariableValue + RefreshGroup 切换 suite
type Suite struct {
	id        uint8
	entityMap map[uint32]IEntity
}

func (g *Group) GetId() uint32 {
	return g.id
}

func (g *Group) GetSuiteById(suiteId uint8) *Suite {
	return g.suiteMap[suiteId]
}

func (g *Group) GetAllSuite() map[uint8]*Suite {
	return g.suiteMap
}

func (g *Group) GetAllEntity() map[uint32]IEntity {
	entityMap := make(map[uint32]IEntity)
	for _, suite := range g.suiteMap {
		for _, entity := range suite.entityMap {
			entityMap[entity.GetId()] = entity
		}
	}
	return entityMap
}

func (g *Group) GetEntityByConfigId(configId uint32) IEntity {
	for _, suite := range g.suiteMap {
		for _, entity := range suite.entityMap {
			if entity.GetConfigId() == configId {
				return entity
			}
		}
	}
	return nil
}

func (g *Group) DestroyEntity(entityId uint32) {
	for _, suite := range g.suiteMap {
		for _, entity := range suite.entityMap {
			if entity.GetId() == entityId {
				delete(suite.entityMap, entity.GetId())
				return
			}
		}
	}
}

func (s *Suite) GetId() uint8 {
	return s.id
}

func (s *Suite) GetEntityById(entityId uint32) IEntity {
	return s.entityMap[entityId]
}

func (s *Suite) GetAllEntity() map[uint32]IEntity {
	return s.entityMap
}

// Ability 实体能力实例 对应原神Ability系统中的Ability层
// abilityName 能力的字符串名（如 Avatar_Hutao_Element） abilityNameHash 是其Hk4eAbilityHashCode哈希值（proto传输用）
// abilitySpecialOverrideMap 动态值覆盖表 用于DynamicFloat求值时优先使用
type Ability struct {
	abilityName               string
	abilityNameHash           uint32
	instancedAbilityId        uint32
	abilitySpecialOverrideMap map[uint32]float32
}

// Modifier 修饰器 Ability下的Modifier层 表示实体身上挂载的状态/Buff/属性加成
// 一个Ability可以触发多个Modifier 通过 modifierLocalId 区分
type Modifier struct {
	modifierLocalId       uint32
	parentAbilityName     string
	parentAbilityNameHash uint32
	instancedAbilityId    uint32
	instancedModifierId   uint32
}

// IEntity 场景实体抽象接口
// 9种实体类型都实现这个接口 通过interface调用业务逻辑
// 注意：当前实现的Ability相关方法（AbilityAction/AbilityMixin）覆盖能力非常有限
// 大部分技能/伤害/反应效果未在服务端模拟（详见CLAUDE.md "Ability系统现状"）
type IEntity interface {
	IsEntity()
	GetId() uint32
	GetScene() *Scene
	GetLifeState() uint16
	GetLastDieType() int32
	GetPos() *model.Vector
	GetRot() *model.Vector
	GetMoveState() uint16
	GetLastMoveSceneTimeMs() uint32
	GetLastMoveReliableSeq() uint32
	GetFightProp() map[uint32]float32
	GetLevel() uint8
	GetEntityType() uint8
	GetConfigId() uint32
	GetGroupId() uint32
	GetVisionLevel() int
	SetLifeState(lifeState uint16)
	SetLastDieType(lastDieType int32)
	SetPos(pos *model.Vector)
	SetRot(rot *model.Vector)
	SetMoveState(moveState uint16)
	SetLastMoveSceneTimeMs(lastMoveSceneTimeMs uint32)
	SetLastMoveReliableSeq(lastMoveReliableSeq uint32)
	SetFightProp(fightProp map[uint32]float32)
	InitAbility()
	AddAbility(abilityName string, instancedAbilityId uint32)
	GetAbility(instancedAbilityId uint32) *Ability
	GetAllAbility() []*Ability
	AddModifier(ability *Ability, instancedModifierId uint32, modifierLocalId uint32)
	GetAllModifier() []*Modifier
	RemoveModifier(instancedModifierId uint32)
	AbilityAction(ability *Ability, action *gdconf.ActionData, entity IEntity)
	AbilityMixin(ability *Ability, mixin *gdconf.MixinData, entity IEntity)
	GetDynamicValueMap() map[uint32]float32
}

// Entity 实体基础结构 9种Entity子类型都内嵌它复用基础字段
// 注意 InitAbility 在基类是错误兜底 必须由子类override（每种实体的ability加载来源不同）
type Entity struct {
	id                  uint32               // 实体id（GetNextWorldEntityId生成 高位编码entityType）
	scene               *Scene               // 实体归属上级场景的访问指针
	lifeState           uint16               // 存活状态（LIFE_STATE_ALIVE/DEAD/REVIVE）
	lastDieType         int32                // 最近死亡原因
	pos                 *model.Vector        // 位置
	rot                 *model.Vector        // 朝向
	moveState           uint16               // 运动状态（MotionState枚举 行走/奔跑/游泳等）
	lastMoveSceneTimeMs uint32               // 最近一次MoveInfo的场景时间戳 防止旧消息覆盖新位置
	lastMoveReliableSeq uint32               // 最近一次可靠MoveInfo的seq
	fightProp           map[uint32]float32   // 战斗属性 FightProp枚举→数值（HP/Attack/Defense/暴击等）
	level               uint8                // 等级
	entityType          uint8                // 实体类型（ENTITY_TYPE_AVATAR/MONSTER等）
	configId            uint32               // LUA配置相关 怪物/物件/NPC在group内的唯一标识
	groupId             uint32               // 所属group
	visionLevel         int                  // 视野等级 决定客户端从多远开始能看见此实体
	abilityMap          map[uint32]*Ability  // 已加载的能力 key:instancedAbilityId
	modifierMap         map[uint32]*Modifier // 已应用的修饰器 key:instancedModifierId
	dynamicValueMap     map[uint32]float32   // 能力动态值表（hash→当前值）
}

func (e *Entity) IsEntity() {
}

func (e *Entity) GetId() uint32 {
	return e.id
}

func (e *Entity) GetScene() *Scene {
	return e.scene
}

func (e *Entity) GetLifeState() uint16 {
	return e.lifeState
}

func (e *Entity) GetLastDieType() int32 {
	return e.lastDieType
}

func (e *Entity) GetPos() *model.Vector {
	return &model.Vector{X: e.pos.X, Y: e.pos.Y, Z: e.pos.Z}
}

func (e *Entity) GetRot() *model.Vector {
	return &model.Vector{X: e.rot.X, Y: e.rot.Y, Z: e.rot.Z}
}

func (e *Entity) GetMoveState() uint16 {
	return e.moveState
}

func (e *Entity) GetLastMoveSceneTimeMs() uint32 {
	return e.lastMoveSceneTimeMs
}

func (e *Entity) GetLastMoveReliableSeq() uint32 {
	return e.lastMoveReliableSeq
}

func (e *Entity) GetFightProp() map[uint32]float32 {
	return e.fightProp
}

func (e *Entity) GetLevel() uint8 {
	return e.level
}

func (e *Entity) GetEntityType() uint8 {
	return e.entityType
}

func (e *Entity) GetConfigId() uint32 {
	return e.configId
}

func (e *Entity) GetGroupId() uint32 {
	return e.groupId
}

func (e *Entity) GetVisionLevel() int {
	return e.visionLevel
}

func (e *Entity) SetLifeState(lifeState uint16) {
	e.lifeState = lifeState
}

func (e *Entity) SetLastDieType(lastDieType int32) {
	e.lastDieType = lastDieType
}

func (e *Entity) SetPos(pos *model.Vector) {
	e.pos.X, e.pos.Y, e.pos.Z = pos.X, pos.Y, pos.Z
}

func (e *Entity) SetRot(rot *model.Vector) {
	e.rot.X, e.rot.Y, e.rot.Z = rot.X, rot.Y, rot.Z
}

func (e *Entity) SetMoveState(moveState uint16) {
	e.moveState = moveState
}

func (e *Entity) SetLastMoveSceneTimeMs(lastMoveSceneTimeMs uint32) {
	e.lastMoveSceneTimeMs = lastMoveSceneTimeMs
}

func (e *Entity) SetLastMoveReliableSeq(lastMoveReliableSeq uint32) {
	e.lastMoveReliableSeq = lastMoveReliableSeq
}

func (e *Entity) SetFightProp(fightProp map[uint32]float32) {
	e.fightProp = fightProp
}

func (e *Entity) InitAbility() {
	logger.Error("parent entity init ability func can not be invoke, entityId: %v", e.GetId())
}

func (e *Entity) AddAbility(abilityName string, instancedAbilityId uint32) {
	// logger.Debug("[AddAbility] abilityName: %v, instancedAbilityId: %v, entityId: %v", abilityName, instancedAbilityId, e.GetId())
	_, exist := e.abilityMap[instancedAbilityId]
	if exist {
		logger.Error("ability already exist, abilityName: %v, entityId: %v", abilityName, e.GetId())
		return
	}
	e.abilityMap[instancedAbilityId] = &Ability{
		abilityName:               abilityName,
		abilityNameHash:           uint32(endec.Hk4eAbilityHashCode(abilityName)),
		instancedAbilityId:        instancedAbilityId,
		abilitySpecialOverrideMap: make(map[uint32]float32),
	}
}

func (e *Entity) GetAbility(instancedAbilityId uint32) *Ability {
	return e.abilityMap[instancedAbilityId]
}

func (e *Entity) GetAllAbility() []*Ability {
	ret := make([]*Ability, 0)
	for _, ability := range e.abilityMap {
		ret = append(ret, ability)
	}
	return ret
}

func (e *Entity) AddModifier(ability *Ability, instancedModifierId uint32, modifierLocalId uint32) {
	// logger.Debug("[AddModifier] abilityName: %v, instancedModifierId: %v, modifierLocalId: %v, entityId: %v",
	// 	ability.abilityName, instancedModifierId, modifierLocalId, e.GetId())
	_, exist := e.modifierMap[instancedModifierId]
	if exist {
		logger.Error("modifier already exist, abilityName: %v, modifierLocalId: %v, entityId: %v", ability.abilityName, modifierLocalId, e.GetId())
		return
	}
	e.modifierMap[instancedModifierId] = &Modifier{
		modifierLocalId:       modifierLocalId,
		parentAbilityName:     ability.abilityName,
		parentAbilityNameHash: ability.abilityNameHash,
		instancedAbilityId:    ability.instancedAbilityId,
		instancedModifierId:   instancedModifierId,
	}
}

func (e *Entity) GetAllModifier() []*Modifier {
	ret := make([]*Modifier, 0)
	for _, modifier := range e.modifierMap {
		ret = append(ret, modifier)
	}
	return ret
}

func (e *Entity) RemoveModifier(instancedModifierId uint32) {
	modifier, exist := e.modifierMap[instancedModifierId]
	if !exist {
		logger.Error("modifier not exist, instancedModifierId: %v, entityId: %v", instancedModifierId, e.GetId())
		return
	}
	// logger.Debug("[RemoveModifier] abilityName: %v, modifierLocalId: %v, entityId: %v", modifier.parentAbilityName, modifier.modifierLocalId, e.GetId())
	delete(e.modifierMap, modifier.instancedModifierId)
}

// GetDynamicFloat 求值动态浮点表达式
// 支持两种格式：
//  1. float64 直接数值
//  2. string  形如 "%KEY1+%KEY2*123" 的字符串表达式
//     %前缀的key先从 abilityData.AbilitySpecials 取基础值
//     再用 abilitySpecialOverrideMap[hash(key)] 覆盖（如有）
//     最后用 dengsgo/math-engine 库计算表达式
//
// 这是 Ability 系统中"配置数据→运行时数值"的核心机制
// 用于AbilityAction的Amount/CostStaminaRatio/BaseEnergy/Ratio等参数解析
func (a *Ability) GetDynamicFloat(abilityData *gdconf.AbilityData, dynamicFloat gdconf.DynamicFloat) float32 {
	switch dynamicFloat.(type) {
	case float64:
		return float32(dynamicFloat.(float64))
	case string:
		rawExp := dynamicFloat.(string)
		exp := ""
		for i := 0; i < len(rawExp); i++ {
			c := string(rawExp[i])
			if c == "%" {
				for j := i + 1; j < len(rawExp); j++ {
					cc := string(rawExp[j])
					end := j == len(rawExp)-1
					if cc == "+" || cc == "-" || cc == "*" || cc == "/" || end {
						key := ""
						if end {
							key = rawExp[i+1 : j+1]
						} else {
							key = rawExp[i+1 : j]
						}
						value := float32(0.0)
						v1, exist := abilityData.AbilitySpecials[key]
						if !exist {
							logger.Error("ability special key not exist, key: %v", key)
							return 0.0
						}
						value = v1
						v2, exist := a.abilitySpecialOverrideMap[uint32(endec.Hk4eAbilityHashCode(key))]
						if exist {
							value = v2
						}
						exp += fmt.Sprintf("%f", value)
						if end {
							i = j
						} else {
							i = j - 1
						}
						break
					}
				}
			} else {
				exp += c
			}
		}
		r, err := engine.ParseAndExec(exp)
		if err != nil {
			logger.Error("calc dynamic float error: %v", err)
			return 0.0
		}
		return float32(r)
	default:
		return 0.0
	}
}

// AbilityAction 执行 Ability 配置中的Action动作
// 当前仅实现6种 大部分原神官服AbilityAction（伤害/护盾/反应/位移等几十种）未实现
// 想完整复刻技能效果需要在这里继续扩 case
//   - ExecuteGadgetLua  执行物件的服务端Lua脚本（OnClientExecuteReq入口）
//   - KillSelf          自杀（怪物自爆等）
//   - AvatarSkillStart  角色技能开始 扣体力 + 触发任务条件
//   - CreateGadget      创建gadget物件（仅ByServer标志为true时执行）
//   - GenerateElemBall  生成元素球（角色技能后产生的元素粒子）
//   - HealHP            全队治疗
func (e *Entity) AbilityAction(ability *Ability, action *gdconf.ActionData, entity IEntity) {
	logger.Debug("[AbilityAction] type: %v, entityId: %v", action.Type, entity.GetId())
	scene := entity.GetScene()
	world := scene.GetWorld()
	owner := world.GetOwner()
	switch action.Type {
	case "ExecuteGadgetLua":
		iGadgetEntity, ok := entity.(IGadgetEntity)
		if !ok {
			logger.Error("entity is not gadget, entityId: %v", entity.GetId())
			return
		}
		gadgetDataConfig := gdconf.GetGadgetDataById(int32(iGadgetEntity.GetGadgetId()))
		if gadgetDataConfig == nil {
			logger.Error("get gadget data config is nil, gadgetId: %v", iGadgetEntity.GetGadgetId())
			return
		}
		if gadgetDataConfig.ServerLuaScript != "" {
			gadgetLuaConfig := gdconf.GetGadgetLuaConfigByName(gadgetDataConfig.ServerLuaScript)
			if gadgetLuaConfig == nil {
				logger.Error("get gadget lua config is nil, name: %v", gadgetDataConfig.ServerLuaScript)
				return
			}
			CallGadgetLuaFunc(gadgetLuaConfig.LuaState, "OnClientExecuteReq",
				&LuaCtx{uid: owner.PlayerId, targetEntityId: entity.GetId(), groupId: entity.GetGroupId()},
				action.Param1, action.Param2, action.Param3)
		}
	case "KillSelf":
		GAME.SubEntityHp(owner, scene, entity.GetId(), 0.0, 1.0, proto.ChangHpReason_CHANGE_HP_SUB_ABILITY)
	case "AvatarSkillStart":
		abilityDataConfig := gdconf.GetAbilityDataByName(ability.abilityName)
		if abilityDataConfig == nil {
			logger.Error("get ability data config is nil, abilityName: %v", ability.abilityName)
			return
		}
		staminaCost := ability.GetDynamicFloat(abilityDataConfig, action.CostStaminaRatio)
		GAME.UpdatePlayerStamina(owner, int32(staminaCost)*-100)
		GAME.TriggerQuest(owner, constant.QUEST_FINISH_COND_TYPE_SKILL, "", action.SkillID)
	case "CreateGadget":
		if !action.ByServer {
			return
		}
		GAME.CreateGadget(owner, entity.GetPos(), uint32(action.GadgetID))
	case "GenerateElemBall":
		itemDataConfig := gdconf.GetItemDataById(action.ConfigID)
		if itemDataConfig == nil {
			logger.Error("get item data config is nil, itemId: %v", action.ConfigID)
			return
		}
		if itemDataConfig.GadgetId == 0 {
			return
		}
		abilityDataConfig := gdconf.GetAbilityDataByName(ability.abilityName)
		if abilityDataConfig == nil {
			logger.Error("get ability data config is nil, abilityName: %v", ability.abilityName)
			return
		}
		baseEnergy := ability.GetDynamicFloat(abilityDataConfig, action.BaseEnergy)
		ratio := ability.GetDynamicFloat(abilityDataConfig, action.Ratio)
		totalEnergy := baseEnergy * ratio
		for _, itemUse := range itemDataConfig.ItemUseList {
			if itemUse.UseOption != constant.ITEM_USE_ADD_ELEM_ENERGY {
				continue
			}
			if len(itemUse.UseParam) != 3 {
				continue
			}
			sameEnergy, err := strconv.Atoi(itemUse.UseParam[1])
			if err != nil {
				continue
			}
			count := math.Ceil(float64(totalEnergy) / float64(sameEnergy))
			for i := 0; i < int(count); i++ {
				GAME.CreateDropGadget(owner, entity.GetPos(), uint32(itemDataConfig.GadgetId), uint32(action.ConfigID), 1)
			}
		}
	case "HealHP":
		abilityDataConfig := gdconf.GetAbilityDataByName(ability.abilityName)
		if abilityDataConfig == nil {
			logger.Error("get ability data config is nil, abilityName: %v", ability.abilityName)
			return
		}
		amount := ability.GetDynamicFloat(abilityDataConfig, action.Amount)
		for _, worldAvatar := range world.GetPlayerWorldAvatarList(owner) {
			GAME.AddPlayerAvatarHp(owner.PlayerId, worldAvatar.GetAvatarId(), amount, 0.0, proto.ChangHpReason_CHANGE_HP_ADD_ABILITY)
		}
	default:
		logger.Error("not support ability action type: %v, abilityName: %v, entityId: %v", action.Type, ability.abilityName, entity.GetId())
	}
}

// AbilityMixin 执行 Ability 配置中的 Mixin 混入
// 仅实现1种：CostStaminaMixin 持续扣体力
// 原神官服还有几十种 Mixin 类型（元素附着/物理碰撞/范围伤害等）均未实现
func (e *Entity) AbilityMixin(ability *Ability, mixin *gdconf.MixinData, entity IEntity) {
	logger.Debug("[AbilityMixin] type: %v, entityId: %v", mixin.Type, entity.GetId())
	owner := entity.GetScene().GetWorld().GetOwner()
	switch mixin.Type {
	case "CostStaminaMixin":
		abilityDataConfig := gdconf.GetAbilityDataByName(ability.abilityName)
		if abilityDataConfig == nil {
			logger.Error("get ability data config is nil, abilityName: %v", ability.abilityName)
			return
		}
		staminaCost := ability.GetDynamicFloat(abilityDataConfig, mixin.CostStaminaDelta)
		GAME.UpdatePlayerStamina(owner, int32(staminaCost)*-100)
	}
}

func (e *Entity) GetDynamicValueMap() map[uint32]float32 {
	return e.dynamicValueMap
}

// AvatarEntity 玩家角色实体 携带 uid 和 avatarId 信息
// 一个玩家本地队伍的所有角色都会创建AvatarEntity 切换角色不重建实体只切isActive
type AvatarEntity struct {
	*Entity
	uid      uint32
	avatarId uint32
}

func (a *AvatarEntity) GetUid() uint32 {
	return a.uid
}

func (a *AvatarEntity) GetAvatarId() uint32 {
	return a.avatarId
}

func (a *AvatarEntity) InitAbility() {
	a.abilityMap = make(map[uint32]*Ability)
	a.modifierMap = make(map[uint32]*Modifier)
	a.dynamicValueMap = make(map[uint32]float32)
}

// WeaponEntity 武器实体 跟随AvatarEntity创建/销毁 客户端用于挂载武器模型在角色右手
type WeaponEntity struct {
	*Entity
}

func (w *WeaponEntity) InitAbility() {
	w.abilityMap = make(map[uint32]*Ability)
	w.modifierMap = make(map[uint32]*Modifier)
	w.dynamicValueMap = make(map[uint32]float32)
}

// MonsterEntity 怪物实体 InitAbility 会从 monsterDataConfig.ConfigAbility 加载怪物的所有Ability
type MonsterEntity struct {
	*Entity
	monsterId uint32
}

func (m *MonsterEntity) GetMonsterId() uint32 {
	return m.monsterId
}

func (m *MonsterEntity) InitAbility() {
	m.abilityMap = make(map[uint32]*Ability)
	m.modifierMap = make(map[uint32]*Modifier)
	m.dynamicValueMap = make(map[uint32]float32)
	monsterDataConfig := gdconf.GetMonsterDataById(int32(m.GetMonsterId()))
	if monsterDataConfig == nil {
		logger.Error("get monster data config is nil, monsterId: %v", m.GetMonsterId())
		return
	}
	if monsterDataConfig.ConfigAbility == nil {
		return
	}
	for configAbilityIndex, configAbility := range monsterDataConfig.ConfigAbility.Abilities {
		abilityDataConfig := gdconf.GetAbilityDataByName(configAbility.AbilityName)
		if abilityDataConfig == nil {
			logger.Error("get ability data config is nil, abilityName: %v", configAbility.AbilityName)
			continue
		}
		instancedAbilityId := uint32(configAbilityIndex + 1)
		m.AddAbility(abilityDataConfig.AbilityName, instancedAbilityId)
	}
}

func (m *MonsterEntity) CreateMonsterEntity(monsterId uint32) {
	fightPropMap := gdconf.GetMonsterFightPropMap(monsterId, m.GetLevel())
	fightPropMap[constant.FIGHT_PROP_CUR_ATTACK] = fightPropMap[constant.FIGHT_PROP_BASE_ATTACK]
	fightPropMap[constant.FIGHT_PROP_CUR_DEFENSE] = fightPropMap[constant.FIGHT_PROP_BASE_DEFENSE]
	fightPropMap[constant.FIGHT_PROP_MAX_HP] = fightPropMap[constant.FIGHT_PROP_BASE_HP]
	fightPropMap[constant.FIGHT_PROP_CUR_HP] = fightPropMap[constant.FIGHT_PROP_MAX_HP]
	m.fightProp = fightPropMap
	m.monsterId = monsterId
}

// NpcEntity NPC实体 携带房间id/父任务id/区块id等任务相关属性
// 与玩家可对话的村民/任务NPC等都用此类型
type NpcEntity struct {
	*Entity
	npcId         uint32
	roomId        uint32
	parentQuestId uint32
	blockId       uint32
}

func (n *NpcEntity) GetNpcId() uint32 {
	return n.npcId
}

func (n *NpcEntity) GetRoomId() uint32 {
	return n.roomId
}

func (n *NpcEntity) GetParentQuestId() uint32 {
	return n.parentQuestId
}

func (n *NpcEntity) GetBlockId() uint32 {
	return n.blockId
}

func (n *NpcEntity) InitAbility() {
	n.abilityMap = make(map[uint32]*Ability)
	n.modifierMap = make(map[uint32]*Modifier)
	n.dynamicValueMap = make(map[uint32]float32)
}

func (n *NpcEntity) CreateNpcEntity(npcId, roomId, parentQuestId, blockId uint32) {
	n.npcId = npcId
	n.roomId = roomId
	n.parentQuestId = parentQuestId
	n.blockId = blockId
}

// IGadgetEntity 物件类实体共同接口 6种Gadget子类型都实现它
type IGadgetEntity interface {
	GetGadgetId() uint32
	GetGadgetState() uint32
	SetGadgetState(state uint32)
	InitAbility()
}

// GadgetEntity 物件实体基类 6种Gadget子类型嵌入它
// gadgetId 物件配置id（70xxx等系列） gadgetState 物件状态（开/关/破坏等）
type GadgetEntity struct {
	*Entity
	gadgetId    uint32
	gadgetState uint32
}

func (g *GadgetEntity) GetGadgetId() uint32 {
	return g.gadgetId
}

func (g *GadgetEntity) GetGadgetState() uint32 {
	return g.gadgetState
}

func (g *GadgetEntity) SetGadgetState(state uint32) {
	g.gadgetState = state
}

func (g *GadgetEntity) InitAbility() {
	g.abilityMap = make(map[uint32]*Ability)
	g.modifierMap = make(map[uint32]*Modifier)
	g.dynamicValueMap = make(map[uint32]float32)
	gadgetDataConfig := gdconf.GetGadgetDataById(int32(g.GetGadgetId()))
	if gadgetDataConfig == nil {
		logger.Error("get gadget data config is nil, gadgetId: %v", g.GetGadgetId())
		return
	}
	if gadgetDataConfig.JsonName == "" {
		return
	}
	gadgetJsonConfig := gdconf.GetGadgetJsonConfigByName(gadgetDataConfig.JsonName)
	if gadgetJsonConfig == nil {
		logger.Error("get gadget json config is nil, name: %v", gadgetDataConfig.JsonName)
		return
	}
	for configAbilityIndex, configAbility := range gadgetJsonConfig.Abilities {
		abilityDataConfig := gdconf.GetAbilityDataByName(configAbility.AbilityName)
		if abilityDataConfig == nil {
			logger.Error("get ability data config is nil, abilityName: %v", configAbility.AbilityName)
			continue
		}
		instancedAbilityId := uint32(configAbilityIndex + 1)
		g.AddAbility(abilityDataConfig.AbilityName, instancedAbilityId)
	}
}

// GadgetNormalEntity 普通物件 包含场景里的大部分静态/可交互gadget
type GadgetNormalEntity struct {
	*GadgetEntity
}

// GadgetTrifleItemEntity 掉落物 怪物死亡或采集后产生的可拾取道具
// itemId+count 表示拾取后获得什么物品
type GadgetTrifleItemEntity struct {
	*GadgetEntity
	itemId uint32
	count  uint32
}

func (g *GadgetTrifleItemEntity) GetItemId() uint32 {
	return g.itemId
}

func (g *GadgetTrifleItemEntity) GetCount() uint32 {
	return g.count
}

func (g *GadgetTrifleItemEntity) CreateGadgetTrifleItemEntity(itemId, count uint32) {
	g.itemId = itemId
	g.count = count
}

// GadgetGatherEntity 采集物 矿石/植物等需要交互采集的物件
// itemId+count 表示采集后获得什么物品
type GadgetGatherEntity struct {
	*GadgetEntity
	itemId uint32
	count  uint32
}

func (g *GadgetGatherEntity) GetItemId() uint32 {
	return g.itemId
}

func (g *GadgetGatherEntity) GetCount() uint32 {
	return g.count
}

func (g *GadgetGatherEntity) CreateGadgetGatherEntity(itemId, count uint32) {
	g.itemId = itemId
	g.count = count
}

// GadgetWorktopEntity 操作台/机关 玩家点击后弹出选项菜单（如风之翼解锁锚点的"扇叶机关"）
// optionMap 当前可用的选项id集合 由 Lua trigger.SetWorktopOptions 设置
type GadgetWorktopEntity struct {
	*GadgetEntity
	optionMap map[uint32]struct{}
}

func (g *GadgetWorktopEntity) GetOptionMap() map[uint32]struct{} {
	return g.optionMap
}

// GadgetClientEntity 客户端创建的物件 由客户端发EvtCreateGadgetNotify触发
// 用于：角色技能特效物件、阵营战斗中的临时召唤物等
// campId/campType 阵营信息（敌我识别）ownerEntityId 谁创建的 targetEntityId 谁拥有
type GadgetClientEntity struct {
	*GadgetEntity
	campId            uint32
	campType          uint32
	ownerEntityId     uint32
	targetEntityId    uint32
	propOwnerEntityId uint32
}

func (g *GadgetClientEntity) GetCampId() uint32 {
	return g.campId
}

func (g *GadgetClientEntity) GetCampType() uint32 {
	return g.campType
}

func (g *GadgetClientEntity) GetOwnerEntityId() uint32 {
	return g.ownerEntityId
}

func (g *GadgetClientEntity) GetTargetEntityId() uint32 {
	return g.targetEntityId
}

func (g *GadgetClientEntity) GetPropOwnerEntityId() uint32 {
	return g.propOwnerEntityId
}

func (g *GadgetClientEntity) CreateGadgetClientEntity(campId, campType, ownerEntityId, targetEntityId, propOwnerEntityId uint32) {
	g.campId = campId
	g.campType = campType
	g.ownerEntityId = ownerEntityId
	g.targetEntityId = targetEntityId
	g.propOwnerEntityId = propOwnerEntityId
}

// GadgetVehicleEntity 载具实体 当前主要是浪船
// 多人载具时 memberMap 记录每个座位上的玩家uid
// curStamina/maxStamina 是载具自己的耐力（与玩家耐力分开）加速时消耗 静止时恢复
type GadgetVehicleEntity struct {
	*GadgetEntity
	ownerUid     uint32
	maxStamina   float32
	curStamina   float32
	restoreDelay uint8             // 载具耐力回复延时（每次消耗后等待 StaminaRecoverWaitTime 才开始恢复）
	memberMap    map[uint32]uint32 // key:pos(座位号) value:uid
}

func (g *GadgetVehicleEntity) GetOwnerUid() uint32 {
	return g.ownerUid
}

func (g *GadgetVehicleEntity) GetMaxStamina() float32 {
	return g.maxStamina
}

func (g *GadgetVehicleEntity) GetCurStamina() float32 {
	return g.curStamina
}

func (g *GadgetVehicleEntity) GetRestoreDelay() uint8 {
	return g.restoreDelay
}

func (g *GadgetVehicleEntity) GetMemberMap() map[uint32]uint32 {
	return g.memberMap
}

func (g *GadgetVehicleEntity) SetCurStamina(curStamina float32) {
	g.curStamina = curStamina
}

func (g *GadgetVehicleEntity) SetRestoreDelay(restoreDelay uint8) {
	g.restoreDelay = restoreDelay
}

func (g *GadgetVehicleEntity) CreateGadgetVehicleEntity(ownerUid uint32) {
	// 获取载具配置表
	gadgetDataConfig := gdconf.GetGadgetDataById(int32(g.GetGadgetId()))
	if gadgetDataConfig == nil {
		logger.Error("get gadget data config is nil, gadgetId: %v", g.GetGadgetId())
		return
	}
	gadgetJsonConfig := gdconf.GetGadgetJsonConfigByName(gadgetDataConfig.JsonName)
	if gadgetJsonConfig == nil {
		logger.Error("get gadget json config is nil, name: %v", gadgetDataConfig.JsonName)
		return
	}
	fightPropMap := map[uint32]float32{
		constant.FIGHT_PROP_BASE_DEFENSE: gadgetJsonConfig.Combat.Property.DefenseBase,
		constant.FIGHT_PROP_CUR_HP:       gadgetJsonConfig.Combat.Property.HP,
		constant.FIGHT_PROP_MAX_HP:       gadgetJsonConfig.Combat.Property.HP,
		constant.FIGHT_PROP_CUR_ATTACK:   gadgetJsonConfig.Combat.Property.Attack,
	}
	g.fightProp = fightPropMap
	g.ownerUid = ownerUid
	g.maxStamina = gadgetJsonConfig.Vehicle.Stamina.StaminaUpperLimit
	g.curStamina = gadgetJsonConfig.Vehicle.Stamina.StaminaUpperLimit
	g.memberMap = make(map[uint32]uint32)
}
