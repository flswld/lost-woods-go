package game

import (
	"math"

	"hk4e/gs/model"
	"hk4e/pkg/alg"

	"github.com/flswld/halo/logger"
)

// 子弹物理引擎 - PUBG 弓箭对狙的核心
//
// 这是一个简化版的 3D 物理引擎 仅用于 AI 世界的 PUBG 玩法：
//   - 子弹按"重力 + 阻力"运动模型模拟（不考虑反弹/穿透/材质）
//   - 角色简化为长方体碰撞箱（半径0.5 高度2.0 中心点上偏 1.0）
//   - 每 50ms tick 一次 推进所有刚体位置 + AABB 包围盒碰撞检测
//   - 命中后销毁子弹刚体 调 PluginPubg.PubgHit 走 PUBG 伤害计算
//
// 启用条件：仅 AI 世界的 World 才有 bulletPhysicsEngine（普通世界 = nil）
//
// 简化代价：
//   - 不考虑场景障碍物（子弹会穿墙）
//   - 不考虑角色姿态/朝向（角色被简化为静态包围盒）
//   - 不考虑空气阻力/温度等真实物理因素
//   - PITCH_ANGLE_OFFSET = 3 度补偿（客户端实际朝向比抛物线略低）

const (
	AVATAR_RADIUS      = 0.5  // 角色碰撞半径（XZ 平面）
	AVATAR_HEIGHT      = 2.0  // 角色高度（Y 方向）
	AVATAR_Y_OFFSET    = 1.0  // 角色中心点 Y 偏移（角色脚下到几何中心的距离）
	ACC                = -5.0 // 重力加速度（负值表示向下）
	DRAG               = 0.01 // 空气阻力系数
	PITCH_ANGLE_OFFSET = 3.0  // 子弹俯仰角偏移补偿（客户端朝向校正）
	INIT_SPEED         = 50.0 // 子弹初始速度
)

// RigidBody 刚体
type RigidBody struct {
	entityId          uint32       // 子弹实体id
	avatarEntityId    uint32       // 子弹发射者角色实体id
	hitAvatarEntityId uint32       // 子弹命中的角色实体id
	sceneId           uint32       // 子弹所在场景id
	position          *alg.Vector3 // 坐标
	velocity          *alg.Vector3 // 速度
}

// PhysicsEngine 物理引擎
type PhysicsEngine struct {
	rigidBodyMap     map[uint32]*RigidBody // 刚体集合
	pathTracing      bool                  // 子弹路径追踪调试
	acc              float32               // 重力加速度
	drag             float32               // 阻力参数
	pitchAngleOffset float32               // 子弹俯仰角偏移
	initSpeed        float32               // 子弹初始速度
	avatarYOffset    float32               // 角色中心点位置高度偏移
	lastUpdateTime   int64                 // 上一次更新时间
	world            *World                // 世界对象
}

func (w *World) NewPhysicsEngine() {
	w.bulletPhysicsEngine = &PhysicsEngine{
		rigidBodyMap:     make(map[uint32]*RigidBody),
		pathTracing:      false,
		acc:              ACC,
		drag:             DRAG,
		pitchAngleOffset: PITCH_ANGLE_OFFSET,
		initSpeed:        INIT_SPEED,
		avatarYOffset:    AVATAR_Y_OFFSET,
		lastUpdateTime:   0,
		world:            w,
	}
}

func (p *PhysicsEngine) SetPhysicsEngineParam(pathTracing bool) {
	p.pathTracing = pathTracing
}

func (p *PhysicsEngine) ShowAvatarCollider() {
	for _, scene := range p.world.GetAllScene() {
		for _, player := range scene.GetAllPlayer() {
			entity := p.world.GetPlayerActiveAvatarEntity(player)
			avatarPos := entity.GetPos()
			avatarPos.Y += float64(p.avatarYOffset)
			GAME.CreateGadget(p.world.GetOwner(), &model.Vector{X: avatarPos.X, Y: avatarPos.Y, Z: avatarPos.Z}, GADGET_GREEN)
			GAME.CreateGadget(p.world.GetOwner(), &model.Vector{X: avatarPos.X, Y: avatarPos.Y + AVATAR_HEIGHT/2.0, Z: avatarPos.Z}, GADGET_GREEN)
			GAME.CreateGadget(p.world.GetOwner(), &model.Vector{X: avatarPos.X, Y: avatarPos.Y - AVATAR_HEIGHT/2.0, Z: avatarPos.Z}, GADGET_GREEN)
			GAME.CreateGadget(p.world.GetOwner(), &model.Vector{X: avatarPos.X + AVATAR_RADIUS, Y: avatarPos.Y, Z: avatarPos.Z}, GADGET_GREEN)
			GAME.CreateGadget(p.world.GetOwner(), &model.Vector{X: avatarPos.X - AVATAR_RADIUS, Y: avatarPos.Y, Z: avatarPos.Z}, GADGET_GREEN)
			GAME.CreateGadget(p.world.GetOwner(), &model.Vector{X: avatarPos.X, Y: avatarPos.Y, Z: avatarPos.Z + AVATAR_RADIUS}, GADGET_GREEN)
			GAME.CreateGadget(p.world.GetOwner(), &model.Vector{X: avatarPos.X, Y: avatarPos.Y, Z: avatarPos.Z - AVATAR_RADIUS}, GADGET_GREEN)
		}
	}
}

// Update 物理引擎主循环（每 tick 调用一次 由 TICK_MANAGER 全局 tick 触发）
//
// 处理所有刚体：
//  1. 越界检查：超出 AI 世界范围 → 销毁
//  2. 阻力作用：v -= drag*v*dt（每个轴向独立计算 注意速度衰减不会反向）
//  3. 重力作用：vy += -5*dt（仅Y轴 直接减重力加速度）
//  4. 速度作用于位移：pos += v*dt（线性插值）
//  5. 碰撞检测：Collision 函数 AABB 包围盒判定
//  6. 命中 → 加入 hitList 销毁刚体
//
// pathTracing=true 时（GM 调试用）每帧创建一个红色物件做轨迹可视化
func (p *PhysicsEngine) Update(now int64) []*RigidBody {
	hitList := make([]*RigidBody, 0)
	dt := float32(now-p.lastUpdateTime) / 1000.0
	for _, rigidBody := range p.rigidBodyMap {
		if !p.world.IsValidAiWorldPos(rigidBody.sceneId, rigidBody.position.X, rigidBody.position.Y, rigidBody.position.Z) {
			p.DestroyRigidBody(rigidBody.entityId)
			continue
		}
		// 阻力作用于速度
		dvx := p.drag * rigidBody.velocity.X * dt
		if math.Abs(float64(dvx)) >= math.Abs(float64(rigidBody.velocity.X)) {
			rigidBody.velocity.X = 0.0
		} else {
			rigidBody.velocity.X -= dvx
		}
		dvy := p.drag * rigidBody.velocity.Y * dt
		if math.Abs(float64(dvy)) >= math.Abs(float64(rigidBody.velocity.Y)) {
			rigidBody.velocity.Y = 0.0
		} else {
			rigidBody.velocity.Y -= dvy
		}
		dvz := p.drag * rigidBody.velocity.Z * dt
		if math.Abs(float64(dvz)) >= math.Abs(float64(rigidBody.velocity.Z)) {
			rigidBody.velocity.Z = 0.0
		} else {
			rigidBody.velocity.Z -= dvz
		}
		// 重力作用于速度
		rigidBody.velocity.Y += p.acc * dt
		// 速度作用于位移
		oldPos := &alg.Vector3{X: rigidBody.position.X, Y: rigidBody.position.Y, Z: rigidBody.position.Z}
		rigidBody.position.X += rigidBody.velocity.X * dt
		rigidBody.position.Y += rigidBody.velocity.Y * dt
		rigidBody.position.Z += rigidBody.velocity.Z * dt
		newPos := &alg.Vector3{X: rigidBody.position.X, Y: rigidBody.position.Y, Z: rigidBody.position.Z}
		// 碰撞检测
		hitAvatarEntityId := p.Collision(rigidBody.sceneId, rigidBody.avatarEntityId, oldPos, newPos)
		if hitAvatarEntityId != 0 {
			rigidBody.hitAvatarEntityId = hitAvatarEntityId
			hitList = append(hitList, rigidBody)
			p.DestroyRigidBody(rigidBody.entityId)
		}
		if p.pathTracing {
			logger.Debug("[PhysicsEngineUpdate] e: %v, s: %v, p: %v, v: %v", rigidBody.entityId, rigidBody.sceneId, rigidBody.position, rigidBody.velocity)
			GAME.CreateGadget(
				p.world.GetOwner(),
				&model.Vector{X: float64(rigidBody.position.X), Y: float64(rigidBody.position.Y), Z: float64(rigidBody.position.Z)},
				GADGET_RED,
			)
		}
	}
	p.lastUpdateTime = now
	return hitList
}

// Collision AABB 包围盒碰撞检测（线段 vs 长方体）
//
// 算法：把子弹一帧内的运动看作一条线段（oldPos→newPos） 与每个角色的包围盒做相交测试
// 三轴独立判定：线段在 X/Y/Z 三个轴上的投影必须都与包围盒投影相交
//   - lineMin/lineMax: 线段端点在某轴上的最小/最大值
//   - shapeMin/shapeMax: 包围盒在某轴上的最小/最大值
//   - lineMax < shapeMin || lineMin > shapeMax → 不相交
//
// 跳过自己（avatarEntityId 相同的玩家）防止自伤
// 返回命中的角色实体 id（0 表示无命中）
//
// 限制：这是简化的 AABB 测试 不是真正的"线段与立方体相交"算法
//
//	实际上只检查"线段包围盒与立方体重叠" 在子弹长距离飞行时可能误判
//	但对 50 米内的弓箭对射够用了
func (p *PhysicsEngine) Collision(sceneId uint32, avatarEntityId uint32, oldPos *alg.Vector3, newPos *alg.Vector3) uint32 {
	scene := p.world.GetSceneById(sceneId)
	world := scene.GetWorld()
	for _, player := range scene.GetAllPlayer() {
		entity := world.GetPlayerActiveAvatarEntity(player)
		if entity.GetId() == avatarEntityId {
			continue
		}
		avatarPos := entity.GetPos()
		avatarPos.Y += float64(p.avatarYOffset)
		// x轴
		lineMinX := float32(0)
		lineMaxX := float32(0)
		if oldPos.X < newPos.X {
			lineMinX = oldPos.X
			lineMaxX = newPos.X
		} else {
			lineMinX = newPos.X
			lineMaxX = oldPos.X
		}
		shapeMinX := float32(avatarPos.X) - AVATAR_RADIUS
		shapeMaxX := float32(avatarPos.X) + AVATAR_RADIUS
		if lineMaxX < shapeMinX || lineMinX > shapeMaxX {
			continue
		}
		// z轴
		lineMinZ := float32(0)
		lineMaxZ := float32(0)
		if oldPos.Z < newPos.Z {
			lineMinZ = oldPos.Z
			lineMaxZ = newPos.Z
		} else {
			lineMinZ = newPos.Z
			lineMaxZ = oldPos.Z
		}
		shapeMinZ := float32(avatarPos.Z) - AVATAR_RADIUS
		shapeMaxZ := float32(avatarPos.Z) + AVATAR_RADIUS
		if lineMaxZ < shapeMinZ || lineMinZ > shapeMaxZ {
			continue
		}
		// y轴
		lineMinY := float32(0)
		lineMaxY := float32(0)
		if oldPos.Y < newPos.Y {
			lineMinY = oldPos.Y
			lineMaxY = newPos.Y
		} else {
			lineMinY = newPos.Y
			lineMaxY = oldPos.Y
		}
		shapeMinY := float32(avatarPos.Y) - AVATAR_HEIGHT/2.0
		shapeMaxY := float32(avatarPos.Y) + AVATAR_HEIGHT/2.0
		if lineMaxY < shapeMinY || lineMinY > shapeMaxY {
			continue
		}
		return entity.GetId()
	}
	return 0
}

func (p *PhysicsEngine) IsRigidBody(entityId uint32) bool {
	_, exist := p.rigidBodyMap[entityId]
	return exist
}

// CreateRigidBody 创建子弹刚体（弓箭从弓上射出时调用）
//
// 参数 pitchAngle/yawAngle 来自客户端 EvtCreateGadgetNotify 的 InitEulerAngles
//   - pitchAngle: 俯仰角（仰头为正 低头为负）
//   - yawAngle: 偏航角（玩家朝向）
//
// 由角度算出三轴速度分量：
//   - vy = sin(pitch) × initSpeed（Y 分量按俯仰角投影）
//   - vxz = cos(pitch) × initSpeed（XZ 平面分量）
//   - vx = sin(yaw) × vxz, vz = cos(yaw) × vxz（XZ 分量按偏航角分解）
//
// 加 pitchAngleOffset=3 度补偿客户端瞄准与服务端轨迹的差异
func (p *PhysicsEngine) CreateRigidBody(entityId, avatarEntityId, sceneId uint32, x, y, z float32, pitchAngle, yawAngle float32) {
	pitchAngle += p.pitchAngleOffset
	vy := math.Sin(float64(pitchAngle)/360.0*2*math.Pi) * float64(p.initSpeed)
	vxz := math.Cos(float64(pitchAngle)/360.0*2*math.Pi) * float64(p.initSpeed)
	vx := math.Sin(float64(yawAngle)/360.0*2*math.Pi) * vxz
	vz := math.Cos(float64(yawAngle)/360.0*2*math.Pi) * vxz
	rigidBody := &RigidBody{
		entityId:       entityId,
		avatarEntityId: avatarEntityId,
		sceneId:        sceneId,
		position:       &alg.Vector3{X: x, Y: y, Z: z},
		velocity:       &alg.Vector3{X: float32(vx), Y: float32(vy), Z: float32(vz)},
	}
	logger.Debug("[CreateRigidBody] e: %v, s: %v, p: %v, v: %v", rigidBody.entityId, rigidBody.sceneId, rigidBody.position, rigidBody.velocity)
	p.rigidBodyMap[entityId] = rigidBody
}

func (p *PhysicsEngine) DestroyRigidBody(entityId uint32) {
	if !p.IsRigidBody(entityId) {
		return
	}
	rigidBody := p.rigidBodyMap[entityId]
	logger.Debug("[DestroyRigidBody] e: %v, s: %v, p: %v, v: %v", rigidBody.entityId, rigidBody.sceneId, rigidBody.position, rigidBody.velocity)
	delete(p.rigidBodyMap, entityId)
}
