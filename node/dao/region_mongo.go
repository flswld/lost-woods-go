package dao

import (
	"context"
	"errors"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

// node Region 表 Mongo/Gorm 双实现（仅 1 行 ID=1）
//
// d.mongo != nil 走 Mongo；为 nil 时 fallback 到 Gorm 实现
// Mongo 模式直接存字段；Gorm 模式存 msgpack 序列化的 BLOB（详见 region_gorm.go）

// Region 集群级共享状态（详见 dao.go 注释）
//   - Ec2bData: 区服 ec2b 密钥序列化数据 启动时加载 关闭时不写
//   - NextUid: 玩家 uid 自增计数 关闭时回写防止重启倒退
//   - StopServer/Time: 停服开关
//   - IpAddrWhiteList: 停服期间白名单 IP 列表
type Region struct {
	ID                  primitive.ObjectID `bson:"_id,omitempty"`
	Ec2bData            []byte             `bson:"ec2b_data"`
	NextUid             uint32             `bson:"next_uid"`
	StopServer          bool               `bson:"stop_server"`
	StopServerStartTime uint32             `bson:"stop_server_start_time"`
	StopServerEndTime   uint32             `bson:"stop_server_end_time"`
	IpAddrWhiteList     []string           `bson:"ip_addr_white_list"`
}

// InsertRegion Node 首次启动时调用一次（仅 1 行）
func (d *Dao) InsertRegion(region *Region) error {
	if d.mongo == nil {
		return d.InsertRegionGorm(region)
	}
	db := d.mongoDb.Collection("region")
	_, err := db.InsertOne(context.TODO(), region)
	if err != nil {
		return err
	}
	return nil
}

// UpdateRegion Node 关闭时调用 把 NextUid/StopServerInfo 写回（防止重启丢失）
func (d *Dao) UpdateRegion(region *Region) error {
	if d.mongo == nil {
		return d.UpdateRegionGorm(region)
	}
	db := d.mongoDb.Collection("region")
	_, err := db.UpdateMany(
		context.TODO(),
		bson.D{},
		bson.D{{"$set", region}},
	)
	if err != nil {
		return err
	}
	return nil
}

// QueryRegion Node 启动时加载（不存在返回 nil 触发首次 InsertRegion）
func (d *Dao) QueryRegion() (*Region, error) {
	if d.mongo == nil {
		return d.QueryRegionGorm()
	}
	db := d.mongoDb.Collection("region")
	result := db.FindOne(
		context.TODO(),
		bson.D{},
	)
	region := new(Region)
	err := result.Decode(region)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		} else {
			return nil, err
		}
	}
	return region, nil
}
