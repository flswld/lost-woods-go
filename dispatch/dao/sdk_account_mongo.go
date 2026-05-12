package dao

import (
	"context"

	"hk4e/dispatch/model"

	"go.mongodb.org/mongo-driver/bson"
)

// SDK 账号表 Mongo/Gorm 双实现（按 d.mongo 是否存在自动切换）
//
// 双实现模式：
//   - d.mongo != nil 走 Mongo（mongodb 模式）
//   - d.mongo == nil 走 Gorm（mysql/sqlite 模式）
//
// 这种模式让上层代码无需关心底层 DB 类型 直接调 InsertSdkAccount/QuerySdkAccountByField 等
// dispatch 项目所有 DAO 操作都遵循此模式

// InsertSdkAccount 新增 SDK 账号
func (d *Dao) InsertSdkAccount(account *model.SdkAccount) error {
	if d.mongo == nil {
		return d.InsertSdkAccountGorm(account)
	}
	db := d.mongoDb.Collection("sdk_account")
	_, err := db.InsertOne(context.TODO(), account)
	if err != nil {
		return err
	}
	return nil
}

// UpdateSdkAccountFieldByFieldName 通用单字段更新
// 比如：UpdateSdkAccountFieldByFieldName("account_id", 1, "combo_token", "xxx")
//
//	会更新 account_id=1 的记录的 combo_token 字段
func (d *Dao) UpdateSdkAccountFieldByFieldName(fieldName string, fieldValue any, fieldUpdateName string, fieldUpdateValue any) error {
	if d.mongo == nil {
		return d.UpdateSdkAccountFieldByFieldNameGorm(fieldName, fieldValue, fieldUpdateName, fieldUpdateValue)
	}
	db := d.mongoDb.Collection("sdk_account")
	_, err := db.UpdateMany(
		context.TODO(),
		bson.D{
			{fieldName, fieldValue},
		},
		bson.D{
			{"$set", bson.D{
				{fieldUpdateName, fieldUpdateValue},
			}},
		},
	)
	if err != nil {
		return err
	}
	return nil
}

// QuerySdkAccountByField 通用单字段查询（取第一个匹配记录）
// 用例：QuerySdkAccountByField("username", "xxx") / QuerySdkAccountByField("account_id", 1)
func (d *Dao) QuerySdkAccountByField(fieldName string, fieldValue any) (*model.SdkAccount, error) {
	if d.mongo == nil {
		return d.QuerySdkAccountByFieldGorm(fieldName, fieldValue)
	}
	db := d.mongoDb.Collection("sdk_account")
	find, err := db.Find(
		context.TODO(),
		bson.D{
			{fieldName, fieldValue},
		},
	)
	if err != nil {
		return nil, err
	}
	result := make([]*model.SdkAccount, 0)
	for find.Next(context.TODO()) {
		item := new(model.SdkAccount)
		err := find.Decode(item)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	if len(result) == 0 {
		return nil, nil
	} else {
		return result[0], nil
	}
}
