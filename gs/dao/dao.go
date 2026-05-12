package dao

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"hk4e/common/config"

	"github.com/flswld/halo/logger"
	"github.com/glebarez/sqlite"
	"github.com/go-redis/redis/v8"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// Dao 数据访问层
//
// 三选一数据库（按 database.url 前缀自动切换）：
//   - mongodb://...  → MongoDB（Bson 嵌入式存储 字段名直接对应）
//   - mysql://...    → MySQL/GORM（玩家档作为 BLOB 存储）
//   - sqlite://...   → SQLite/GORM（开发/小规模 单文件）
//
// 双路缓存（standalone 模式不用 Redis）：
//   - 单 Redis：地址不含 "," 时
//   - Redis Cluster：地址含 "," 时（集群模式）
//
// 数据写入策略（详见 CLAUDE.md "三层存储"）：
//   - 内存（in-process）：USER_MANAGER.playerMap
//   - Redis：跨 GS 共享 + msgpack+LZ4 压缩 + 30 天过期
//   - DB：MongoDB/SQL 主存 写频率最低
//
// 写入流向：内存修改 → 异步存档协程 → Redis + DB
// 读取流向：内存 → Redis（命中率高）→ DB（最后回源）

type Dao struct {
	mongo        *mongo.Client        // MongoDB 客户端（mongodb 模式）
	mongoDb      *mongo.Database      // MongoDB 数据库实例（gs_hk4e）
	gormDb       *gorm.DB             // GORM 客户端（mysql/sqlite 模式）
	redis        *redis.Client        // Redis 单实例
	redisCluster *redis.ClusterClient // Redis 集群（用 Addrs 列表配置）
	lockTokenMap sync.Map             // 分布式锁 token 表（uid → token 字符串）见 player_redis.go DistLock
}

// NewDao 创建数据访问层（GS 启动时调用一次）
//
// 处理：
//  1. 按 url 前缀选择 DB 实现：
//     · mongodb://: 连 Mongo + 连接池 10/100
//     · mysql://: GORM 连 MySQL + 连接池 10/100/1h
//     · sqlite://: GORM 连 SQLite 单文件
//  2. SQL 模式自动建表（PlayerGorm/ChatMsgGorm/SceneBlockGorm）
//  3. 集群模式（StandaloneModeEnable=false）才连 Redis
//     · 地址含逗号 → Redis Cluster
//     · 否则 → 单实例
func NewDao() (*Dao, error) {
	r := new(Dao)

	if strings.Contains(config.GetConfig().Database.Url, "mongodb://") {
		clientOptions := options.Client().ApplyURI(config.GetConfig().Database.Url)
		clientOptions = clientOptions.SetMinPoolSize(10)
		clientOptions = clientOptions.SetMaxPoolSize(100)
		client, err := mongo.Connect(context.TODO(), clientOptions)
		if err != nil {
			logger.Error("mongo connect error: %v", err)
			return nil, err
		}
		err = client.Ping(context.TODO(), readpref.Primary())
		if err != nil {
			logger.Error("mongo ping error: %v", err)
			return nil, err
		}
		r.mongo = client
		r.mongoDb = client.Database("gs_hk4e")
	} else {
		if strings.Contains(config.GetConfig().Database.Url, "mysql://") {
			dsn := strings.ReplaceAll(config.GetConfig().Database.Url, "mysql://", "")
			db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{
				Logger: gormlogger.Default.LogMode(gormlogger.Info),
			})
			if err != nil {
				logger.Error("gorm open error: %v", err)
				return nil, err
			}
			r.gormDb = db
			sqlDb, err := db.DB()
			if err != nil {
				logger.Error("sql db open error: %v", err)
				return nil, err
			}
			sqlDb.SetMaxIdleConns(10)
			sqlDb.SetMaxOpenConns(100)
			sqlDb.SetConnMaxLifetime(time.Hour)
		} else if strings.Contains(config.GetConfig().Database.Url, "sqlite://") {
			dsn := strings.ReplaceAll(config.GetConfig().Database.Url, "sqlite://", "")
			db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
				Logger: gormlogger.Default.LogMode(gormlogger.Info),
			})
			if err != nil {
				logger.Error("gorm open error: %v", err)
				return nil, err
			}
			r.gormDb = db
		} else {
			err := errors.New(fmt.Sprintf("not support db type, url: %v", config.GetConfig().Database.Url))
			logger.Error("%v", err)
			return nil, err
		}
		tableList := []any{new(PlayerGorm), new(ChatMsgGorm), new(SceneBlockGorm)}
		for _, table := range tableList {
			err := r.gormDb.AutoMigrate(table)
			if err != nil {
				logger.Error("auto migrate error: %v", err)
				return nil, err
			}
		}
	}

	if !config.GetConfig().Hk4e.StandaloneModeEnable {
		r.redis = nil
		r.redisCluster = nil
		redisAddr := strings.ReplaceAll(config.GetConfig().Redis.Addr, "redis://", "")
		if strings.Contains(redisAddr, ",") {
			redisAddrList := strings.Split(redisAddr, ",")
			r.redisCluster = redis.NewClusterClient(&redis.ClusterOptions{
				Addrs:        redisAddrList,
				Password:     config.GetConfig().Redis.Password,
				PoolSize:     10,
				MinIdleConns: 1,
			})
		} else {
			r.redis = redis.NewClient(&redis.Options{
				Addr:         redisAddr,
				Password:     config.GetConfig().Redis.Password,
				DB:           0,
				PoolSize:     10,
				MinIdleConns: 1,
			})
		}
		var err error = nil
		if r.redisCluster != nil {
			err = r.redisCluster.Ping(context.TODO()).Err()
		} else {
			err = r.redis.Ping(context.TODO()).Err()
		}
		if err != nil {
			logger.Error("redis ping error: %v", err)
			return nil, err
		}
	}

	return r, nil
}

func (d *Dao) CloseDao() {
	if d.mongo != nil {
		err := d.mongo.Disconnect(context.TODO())
		if err != nil {
			logger.Error("mongo close error: %v", err)
		}
	}

	if !config.GetConfig().Hk4e.StandaloneModeEnable {
		var err error = nil
		if d.redisCluster != nil {
			err = d.redisCluster.Close()
		} else {
			err = d.redis.Close()
		}
		if err != nil {
			logger.Error("redis close error: %v", err)
		}
	}
}
