package dao

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"sync/atomic"
	"time"

	"hk4e/gs/model"

	"github.com/flswld/halo/logger"
	"github.com/pierrec/lz4/v4"
	"github.com/vmihailenco/msgpack/v5"
)

// Redis 玩家档存储 - 跨 GS 共享 + 高速命中（详见 CLAUDE.md "三层存储"）
//
// 序列化链路：
//   model.Player → msgpack 二进制 → LZ4 压缩 → Redis Set（30 天过期）
//
// 性能特点：
//   - msgpack 比 JSON 快几倍 + 比 BSON 紧凑
//   - LZ4 压缩比通常 0.3 ~ 0.5（大幅减少 Redis 内存）
//   - 实测压缩 + 序列化 < 1ms
//
// 分布式锁：
//   - Key: HK4E:USER_LOCK:{uid}
//   - 用 SetNX 实现 默认 10 秒 TTL
//   - LoadTempOfflineUser 修改场景必加锁 防止跨 GS 并发改同一玩家档
//
// 不保存到 Redis 的字段（详见 db_player.go）：
//   - ChatMsgMap：聊天记录仅 DB
//   - MailMap：邮件仅 DB
//   - SceneBlockMap：场景区块存档仅 DB

// RedisPlayerKeyPrefix key前缀
const RedisPlayerKeyPrefix = "HK4E"

// GetRedisPlayerKey 获取玩家数据key
func (d *Dao) GetRedisPlayerKey(userId uint32) string {
	return RedisPlayerKeyPrefix + ":USER:" + strconv.Itoa(int(userId))
}

// GetRedisPlayerLockKey 获取玩家分布式锁key
func (d *Dao) GetRedisPlayerLockKey(userId uint32) string {
	return RedisPlayerKeyPrefix + ":USER_LOCK:" + strconv.Itoa(int(userId))
}

// GetRedisPlayer 从 Redis 读取玩家档
//
// 流程：Redis Get → LZ4 解压 → msgpack 反序列化 → *model.Player
// 失败返回 nil（调用方应继续尝试 DB 加载）
// 性能日志：Debug 级别打印 Redis IO 耗时 + LZ4 压缩比
func (d *Dao) GetRedisPlayer(userId uint32) *model.Player {
	startTime := time.Now().UnixNano()
	var playerDataLz4 = ""
	var err error = nil
	if d.redisCluster != nil {
		playerDataLz4, err = d.redisCluster.Get(context.TODO(), d.GetRedisPlayerKey(userId)).Result()
	} else {
		playerDataLz4, err = d.redis.Get(context.TODO(), d.GetRedisPlayerKey(userId)).Result()
	}
	if err != nil {
		logger.Error("get player from redis error: %v", err)
		return nil
	}
	endTime := time.Now().UnixNano()
	costTime := endTime - startTime
	logger.Debug("get player from redis cost time: %v ns", costTime)
	// 解压
	startTime = time.Now().UnixNano()
	in := bytes.NewReader([]byte(playerDataLz4))
	out := new(bytes.Buffer)
	lz4Reader := lz4.NewReader(in)
	_, err = io.Copy(out, lz4Reader)
	if err != nil {
		logger.Error("lz4 decode player data error: %v", err)
		return nil
	}
	playerData := out.Bytes()
	endTime = time.Now().UnixNano()
	costTime = endTime - startTime
	logger.Debug("lz4 decode cost time: %v ns, before len: %v, after len: %v, ratio lz4/raw: %v",
		costTime, len(playerDataLz4), len(playerData), float64(len(playerDataLz4))/float64(len(playerData)))
	player := new(model.Player)
	err = msgpack.Unmarshal(playerData, player)
	if err != nil {
		logger.Error("unmarshal player error: %v", err)
		return nil
	}
	return player
}

// SetRedisPlayer 写入玩家档到 Redis
// 流程：msgpack 序列化 → LZ4 压缩 → Redis Set（30 天过期）
// 30 天过期机制让长期不上线的玩家自然从 Redis 清掉 节约内存
func (d *Dao) SetRedisPlayer(player *model.Player) {
	playerData, err := msgpack.Marshal(player)
	if err != nil {
		logger.Error("marshal player error: %v", err)
		return
	}
	// 压缩
	startTime := time.Now().UnixNano()
	in := bytes.NewReader(playerData)
	out := new(bytes.Buffer)
	lz4Writer := lz4.NewWriter(out)
	_, err = io.Copy(lz4Writer, in)
	if err != nil {
		logger.Error("lz4 encode player data error: %v", err)
		return
	}
	err = lz4Writer.Close()
	if err != nil {
		logger.Error("lz4 encode player data error: %v", err)
		return
	}
	playerDataLz4 := out.Bytes()
	endTime := time.Now().UnixNano()
	costTime := endTime - startTime
	logger.Debug("lz4 encode cost time: %v ns, before len: %v, after len: %v, ratio lz4/raw: %v",
		costTime, len(playerData), len(playerDataLz4), float64(len(playerDataLz4))/float64(len(playerData)))
	startTime = time.Now().UnixNano()
	if d.redisCluster != nil {
		err = d.redisCluster.Set(context.TODO(), d.GetRedisPlayerKey(player.PlayerId), playerDataLz4, time.Hour*24*30).Err()
	} else {
		err = d.redis.Set(context.TODO(), d.GetRedisPlayerKey(player.PlayerId), playerDataLz4, time.Hour*24*30).Err()
	}
	if err != nil {
		logger.Error("set player to redis error: %v", err)
		return
	}
	endTime = time.Now().UnixNano()
	costTime = endTime - startTime
	logger.Debug("set player to redis cost time: %v ns", costTime)
}

// SetRedisPlayerList 批量写入玩家数据
func (d *Dao) SetRedisPlayerList(playerList []*model.Player) {
	// TODO 换成redis批量命令执行
	for _, player := range playerList {
		d.SetRedisPlayer(player)
	}
}

// DeleteRedisPlayer 删除玩家数据
func (d *Dao) DeleteRedisPlayer(userId uint32) {
	startTime := time.Now().UnixNano()
	var err error = nil
	if d.redisCluster != nil {
		err = d.redisCluster.Del(context.TODO(), d.GetRedisPlayerKey(userId)).Err()
	} else {
		err = d.redis.Del(context.TODO(), d.GetRedisPlayerKey(userId)).Err()
	}
	if err != nil {
		logger.Error("delete player from redis error: %v", err)
		return
	}
	endTime := time.Now().UnixNano()
	costTime := endTime - startTime
	logger.Debug("delete player from redis cost time: %v ns", costTime)
}

// 基于 redis 的玩家离线数据分布式锁实现
//
// **锁所有者校验机制**（防止误删别人的锁）：
//   - 加锁时生成随机 token 作为 Redis value 同时存到 Dao.lockTokenMap[uid]
//   - 解锁时从 lockTokenMap 取出 token 走 Lua 脚本原子 GET+DEL 校验
//   - 校验失败（说明锁已被自己超时释放并被别人重新获取）→ 不删除别人的锁
//
// **典型 race 场景**（已被本实现覆盖）：
//
//	A 持锁 → A 超时（10s TTL）→ Redis 自动 DEL → B 获取锁（新 token） →
//	A 苏醒调 DistUnlock → Lua 校验发现 Redis 里是 B 的 token ≠ A 的 token → 不删
//	→ B 正常持锁 不被误释放

const (
	MaxLockAliveTime  = 10000 // 单个锁的最大存活时间 毫秒
	LockRetryWaitTime = 50    // 同步加锁重试间隔时间 毫秒
	MaxLockRetryTimes = 2     // 同步加锁最大重试次数
)

// unlockLuaScript Lua 脚本：GET 当前 value 与传入 token 比对 一致才 DEL
// 保证"取值-比较-删除"三步原子执行（Redis 单线程模型下 Lua 脚本是原子的）
const unlockLuaScript = `if redis.call("GET", KEYS[1]) == ARGV[1] then return redis.call("DEL", KEYS[1]) else return 0 end`

// genLockToken 生成 16 字节随机 token（hex 编码 32 字符）
// crypto/rand 保证跨服跨进程不碰撞
func genLockToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 在正常系统上不会失败 兜底用纳秒+counter 仍可保证唯一
		return fmt.Sprintf("%d-%d", time.Now().UnixNano(), atomic.AddUint64(&lockTokenFallback, 1))
	}
	return hex.EncodeToString(b)
}

// lockTokenFallback crypto/rand 失败时的兜底计数器（极少触发）
var lockTokenFallback uint64

// DistLock 加锁并返回是否成功（非阻塞）
//
// 成功时：生成 token 存 Redis value（带 10s TTL）+ 存 Dao.lockTokenMap[uid]
// 失败时（被他人持锁）：返回 false 不污染 lockTokenMap
func (d *Dao) DistLock(userId uint32) bool {
	token := genLockToken()
	var result = false
	var err error = nil
	if d.redisCluster != nil {
		result, err = d.redisCluster.SetNX(context.TODO(),
			d.GetRedisPlayerLockKey(userId),
			token,
			time.Millisecond*time.Duration(MaxLockAliveTime)).Result()
	} else {
		result, err = d.redis.SetNX(context.TODO(),
			d.GetRedisPlayerLockKey(userId),
			token,
			time.Millisecond*time.Duration(MaxLockAliveTime)).Result()
	}
	if err != nil {
		logger.Error("redis lock setnx error: %v", err)
		return false
	}
	if result {
		d.lockTokenMap.Store(userId, token)
	}
	return result
}

// DistLockSync 加锁同步阻塞 50ms 重试间隔 × 最多 2 次（约 150ms 内拿不到就放弃）
//
// **修复**：原实现重试用尽仍 return true 现在正确返回 false
// 调用方（UserLoginLoad / LoadTempOfflineUser）已经正确处理 false 路径
func (d *Dao) DistLockSync(userId uint32) bool {
	for i := 0; i < MaxLockRetryTimes; i++ {
		token := genLockToken()
		var result = false
		var err error = nil
		if d.redisCluster != nil {
			result, err = d.redisCluster.SetNX(context.TODO(),
				d.GetRedisPlayerLockKey(userId),
				token,
				time.Millisecond*time.Duration(MaxLockAliveTime)).Result()
		} else {
			result, err = d.redis.SetNX(context.TODO(),
				d.GetRedisPlayerLockKey(userId),
				token,
				time.Millisecond*time.Duration(MaxLockAliveTime)).Result()
		}
		if err != nil {
			logger.Error("redis lock setnx error: %v", err)
			return false
		}
		if result {
			d.lockTokenMap.Store(userId, token)
			return true
		}
		time.Sleep(time.Millisecond * time.Duration(LockRetryWaitTime))
	}
	return false
}

// DistUnlock 解锁 用 Lua 脚本原子 GET+DEL 校验锁所有者
//
// 三种结果：
//  1. 内存 token 表无记录 → 本服根本没持过锁 直接 return（防止误删别人的锁）
//  2. Lua 脚本删除成功 → 正常释放
//  3. Lua 校验失败（返回 0）→ Redis 里是别人的 token（自己锁已超时被别人抢）不删 仅清内存
//
// 任何情况都从 lockTokenMap 清理本 uid 条目 防内存泄漏
func (d *Dao) DistUnlock(userId uint32) {
	v, ok := d.lockTokenMap.LoadAndDelete(userId)
	if !ok {
		// 本服没持锁 不该解锁（防止误删别人的锁）
		return
	}
	token := v.(string)
	key := d.GetRedisPlayerLockKey(userId)
	var result any
	var err error
	if d.redisCluster != nil {
		result, err = d.redisCluster.Eval(context.TODO(), unlockLuaScript, []string{key}, token).Result()
	} else {
		result, err = d.redis.Eval(context.TODO(), unlockLuaScript, []string{key}, token).Result()
	}
	if err != nil {
		logger.Error("redis lock unlock eval error: %v, uid: %v", err, userId)
		return
	}
	// Lua 返回 0 = 校验失败（锁已被别人持有 不属于本服）
	if delCount, ok := result.(int64); ok && delCount == 0 {
		logger.Warn("redis lock unlock token mismatch, lock already expired or owned by others, uid: %v", userId)
	}
}
