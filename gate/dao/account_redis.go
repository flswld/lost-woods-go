package dao

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/flswld/halo/logger"
)

// gate 账号 Redis 操作 - 主要是分布式锁
//
// 用途：在 doGateLogin 时防止同一 OpenId 并发登录
//   - 同一玩家短时间内多次登录请求（如客户端重试）
//   - 跨 gate 实例的并发登录（多个 gate 各自处理同一账号）
//
// 锁实现：Redis SetNX + 10 秒 TTL（防止持锁服务挂掉时锁泄漏）
// 不实现锁的可重入 因为 gate 不会嵌套加锁
//
// **锁所有者校验机制**（防止误删别人的锁）：
//   - 加锁时生成随机 token 作为 Redis value 同时存到 Dao.lockTokenMap[openId]
//   - 解锁时从 lockTokenMap 取出 token 走 Lua 脚本原子 GET+DEL 校验
//   - 校验失败（锁已被自己超时释放并被别人获取）→ 不删除别人的锁

// RedisAccountKeyPrefix Redis Key 前缀（与 gs 共用 HK4E 命名空间）
const RedisAccountKeyPrefix = "HK4E"

// GetRedisAccountLockKey 获取账号分布式锁key
func (d *Dao) GetRedisAccountLockKey(openId string) string {
	return RedisAccountKeyPrefix + ":ACCOUNT_LOCK:" + openId
}

// 基于redis的玩家离线数据分布式锁实现

const (
	MaxLockAliveTime = 10000 // 单个锁的最大存活时间 毫秒
)

// unlockLuaScript Lua 脚本：GET 当前 value 与传入 token 比对 一致才 DEL（与 gs/dao 共享同款实现）
const unlockLuaScript = `if redis.call("GET", KEYS[1]) == ARGV[1] then return redis.call("DEL", KEYS[1]) else return 0 end`

// lockTokenFallback crypto/rand 失败时的兜底计数器
var lockTokenFallback uint64

// genLockToken 生成 16 字节随机 token（hex 编码 32 字符）
func genLockToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d-%d", time.Now().UnixNano(), atomic.AddUint64(&lockTokenFallback, 1))
	}
	return hex.EncodeToString(b)
}

// DistLock 加分布式锁 用 SetNX 原子操作 + token 校验
//
// TTL=10 秒 持锁方崩溃也能自动释放
// 返回 true 表示加锁成功 false 表示已被其他实例加锁
// 成功时把 token 存到 Dao.lockTokenMap[openId] 供 DistUnlock 校验
func (d *Dao) DistLock(openId string) bool {
	token := genLockToken()
	var result = false
	var err error = nil
	if d.redisCluster != nil {
		result, err = d.redisCluster.SetNX(context.TODO(),
			d.GetRedisAccountLockKey(openId),
			token,
			time.Millisecond*time.Duration(MaxLockAliveTime)).Result()
	} else {
		result, err = d.redis.SetNX(context.TODO(),
			d.GetRedisAccountLockKey(openId),
			token,
			time.Millisecond*time.Duration(MaxLockAliveTime)).Result()
	}
	if err != nil {
		logger.Error("redis lock setnx error: %v", err)
		return false
	}
	if result {
		d.lockTokenMap.Store(openId, token)
	}
	return result
}

// DistUnlock 解锁 用 Lua 脚本原子 GET+DEL 校验锁所有者
//
// 三种结果：
//  1. 内存 token 表无记录 → 本服根本没持过锁 直接 return（防误删别人的锁）
//  2. Lua 脚本删除成功 → 正常释放
//  3. Lua 校验失败（返回 0）→ Redis 里是别人的 token（自己锁已超时被别人抢）不删 仅清内存
func (d *Dao) DistUnlock(openId string) {
	v, ok := d.lockTokenMap.LoadAndDelete(openId)
	if !ok {
		return
	}
	token := v.(string)
	key := d.GetRedisAccountLockKey(openId)
	var result any
	var err error
	if d.redisCluster != nil {
		result, err = d.redisCluster.Eval(context.TODO(), unlockLuaScript, []string{key}, token).Result()
	} else {
		result, err = d.redis.Eval(context.TODO(), unlockLuaScript, []string{key}, token).Result()
	}
	if err != nil {
		logger.Error("redis lock unlock eval error: %v, openId: %v", err, openId)
		return
	}
	if delCount, ok := result.(int64); ok && delCount == 0 {
		logger.Warn("redis lock unlock token mismatch, lock already expired or owned by others, openId: %v", openId)
	}
}
