package model

import (
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Sdk SDK 全局元数据（仅 1 行 ID=1）
//
// 唯一字段 NextSdkAccountId 用于 standalone 模式分配新账号 ID
// 集群模式不使用此表 直接走 Redis INCR
//
// 单行表设计：通过 ID=1 作为唯一主键 任何写操作都更新这一行
type Sdk struct {
	ID               primitive.ObjectID `bson:"_id,omitempty"`
	NextSdkAccountId uint32             `bson:"next_sdk_account_id"` // 下一个自增账号id
}

type SdkGorm struct {
	ID               uint32 `gorm:"column:id;type:bigint(20);primaryKey"`
	NextSdkAccountId uint32 `gorm:"column:next_sdk_account_id;type:bigint(20)"` // 下一个自增账号id
}

func (s SdkGorm) TableName() string {
	return "sdk"
}
