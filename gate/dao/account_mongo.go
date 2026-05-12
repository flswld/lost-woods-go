package dao

import (
	"context"
	"errors"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

// gate Account 表的 Mongo/Gorm 双实现（与 dispatch dao 一样的双实现模式）
//
// d.mongo != nil 走 Mongo 操作；为 nil 时 fallback 到 Gorm 实现
// 上层调用 InsertAccount / QueryAccountByOpenId 不感知底层 DB 类型
//
// 表/集合名: "account"（gate_hk4e 数据库）

// Account 玩家账号（OpenId ↔ Uid 映射）
//   - OpenId: SDK 账号 ID（dispatch 给出的字符串形式 AccountId）
//   - Uid: 游戏内 uid（首次登录时通过 Node.GetNextUid 分配）
//   - IsForbid: 是否被封号（停用账号）
//   - ForbidEndTime: 封号截止时间戳（0=永封）
type Account struct {
	ID            primitive.ObjectID `bson:"_id,omitempty"`
	OpenId        string             `bson:"open_id"`
	Uid           uint32             `bson:"uid"`
	IsForbid      bool               `bson:"is_forbid"`
	ForbidEndTime uint32             `bson:"forbid_end_time"`
}

// InsertAccount 创建新账号（首次登录该 OpenId 的玩家时调用）
func (d *Dao) InsertAccount(account *Account) error {
	if d.mongo == nil {
		return d.InsertAccountGorm(account)
	}
	db := d.mongoDb.Collection("account")
	_, err := db.InsertOne(context.TODO(), account)
	if err != nil {
		return err
	}
	return nil
}

// QueryAccountByOpenId 按 OpenId 查询账号（doGateLogin 主流程用）
// 返回 nil 表示该 OpenId 是首次登录 调用方应继续 InsertAccount
func (d *Dao) QueryAccountByOpenId(openId string) (*Account, error) {
	if d.mongo == nil {
		return d.QueryAccountByOpenIdGorm(openId)
	}
	db := d.mongoDb.Collection("account")
	result := db.FindOne(
		context.TODO(),
		bson.D{{"open_id", openId}},
	)
	account := new(Account)
	err := result.Decode(account)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		} else {
			return nil, err
		}
	}
	return account, nil
}
