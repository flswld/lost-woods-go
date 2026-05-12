package dao

import (
	"context"
	"errors"

	"hk4e/dispatch/model"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// SDK 全局信息双实现（mongo / gorm 自动切换）

// InsertSdk 仅服务首次启动调用一次（只插 1 行）
func (d *Dao) InsertSdk(sdk *model.Sdk) error {
	if d.mongo == nil {
		return d.InsertSdkGorm(sdk)
	}
	db := d.mongoDb.Collection("sdk")
	_, err := db.InsertOne(context.TODO(), sdk)
	if err != nil {
		return err
	}
	return nil
}

// UpdateSdk 服务关闭时把 nextSdkAccountId 写回（standalone 模式）
func (d *Dao) UpdateSdk(sdk *model.Sdk) error {
	if d.mongo == nil {
		return d.UpdateSdkGorm(sdk)
	}
	db := d.mongoDb.Collection("sdk")
	_, err := db.UpdateMany(
		context.TODO(),
		bson.D{},
		bson.D{{"$set", sdk}},
	)
	if err != nil {
		return err
	}
	return nil
}

// QuerySdk 服务启动时加载（不存在返回 nil 触发首次 InsertSdk）
func (d *Dao) QuerySdk() (*model.Sdk, error) {
	if d.mongo == nil {
		return d.QuerySdkGorm()
	}
	db := d.mongoDb.Collection("sdk")
	result := db.FindOne(
		context.TODO(),
		bson.D{},
	)
	sdk := new(model.Sdk)
	err := result.Decode(sdk)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		} else {
			return nil, err
		}
	}
	return sdk, nil
}
