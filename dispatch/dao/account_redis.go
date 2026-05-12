package dao

import (
	"context"
	"strconv"
)

// dispatch SDK 账号 ID 自增计数器（Redis 模式）
//
// 集群模式下用 Redis INCR 原子自增 避免多 dispatch 实例并发分配同一 ID
// standalone 模式不走这里 直接用 SdkGorm 表的 NextSdkAccountId 字段
//
// SdkAccountIdBegin=10000 起始账号 ID（避免与 0~9999 的预留 ID 冲突）

const RedisPlayerKeyPrefix = "HK4E"

const (
	SdkAccountIdRedisKey        = "SdkAccountId" // Redis Key 后缀
	SdkAccountIdBegin    uint32 = 10000          // 起始账号 ID
)

// GetNextSdkAccountId 分配下一个 SDK 账号 ID（apiLogin 注册新账号时调用）
// 利用 Redis INCR 原子自增 集群环境下安全
// 首次调用时若 Key 不存在 自动初始化为 SdkAccountIdBegin=10000
func (d *Dao) GetNextSdkAccountId() (uint32, error) {
	return d.redisInc(RedisPlayerKeyPrefix + ":" + SdkAccountIdRedisKey)
}

func (d *Dao) GetSdkAccountId() (uint32, error) {
	return d.redisGet(RedisPlayerKeyPrefix + ":" + SdkAccountIdRedisKey)
}

func (d *Dao) SetSdkAccountId(accountId uint32) error {
	return d.redisSet(RedisPlayerKeyPrefix+":"+SdkAccountIdRedisKey, accountId)
}

func (d *Dao) redisInc(keyName string) (uint32, error) {
	var exist int64 = 0
	var err error = nil
	if d.redisCluster != nil {
		exist, err = d.redisCluster.Exists(context.TODO(), keyName).Result()
	} else {
		exist, err = d.redis.Exists(context.TODO(), keyName).Result()
	}
	if err != nil {
		return 0, err
	}
	if exist == 0 {
		var err error = nil
		if d.redisCluster != nil {
			err = d.redisCluster.Set(context.TODO(), keyName, SdkAccountIdBegin, 0).Err()
		} else {
			err = d.redis.Set(context.TODO(), keyName, SdkAccountIdBegin, 0).Err()
		}
		if err != nil {
			return 0, err
		}
	}
	var id int64 = 0
	if d.redisCluster != nil {
		id, err = d.redisCluster.Incr(context.TODO(), keyName).Result()
	} else {
		id, err = d.redis.Incr(context.TODO(), keyName).Result()
	}
	if err != nil {
		return 0, err
	}
	return uint32(id), nil
}

func (d *Dao) redisGet(keyName string) (uint32, error) {
	var result = ""
	var err error = nil
	if d.redisCluster != nil {
		result, err = d.redisCluster.Get(context.TODO(), keyName).Result()
	} else {
		result, err = d.redis.Get(context.TODO(), keyName).Result()
	}
	if err != nil {
		return 0, err
	}
	value, err := strconv.Atoi(result)
	if err != nil {
		return 0, err
	}
	return uint32(value), nil
}

func (d *Dao) redisSet(keyName string, value uint32) error {
	var err error = nil
	if d.redisCluster != nil {
		err = d.redisCluster.Set(context.TODO(), keyName, value, 0).Err()
	} else {
		err = d.redis.Set(context.TODO(), keyName, value, 0).Err()
	}
	return err
}
