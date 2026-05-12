package dao

import (
	"errors"

	"github.com/vmihailenco/msgpack/v5"
	"gorm.io/gorm"
)

// RegionGorm 区服信息表（仅 1 行 ID=1）
//
// 设计：用 BLOB 存 msgpack 序列化的 Region 对象
//
//	优点：Region 字段变化时不需要 ALTER TABLE 直接序列化新字段即可
//	代价：不能用 SQL where 过滤字段（但 Region 仅 1 行无所谓）
//
// 模型 Region 字段（dao/region.go）：
//   - Ec2bData: ec2b 密钥序列化数据
//   - NextUid: 玩家 uid 自增计数
//   - StopServer/StartTime/EndTime: 停服信息
//   - IpAddrWhiteList: 停服白名单 IP 列表
type RegionGorm struct {
	ID   uint32 `gorm:"column:id;type:bigint(20);primaryKey"`
	Data []byte `gorm:"column:data;type:longblob"` // msgpack 序列化的 Region 对象
}

func (r RegionGorm) TableName() string {
	return "region"
}

func (d *Dao) InsertRegionGorm(region *Region) error {
	data, err := msgpack.Marshal(region)
	if err != nil {
		return err
	}
	err = d.gormDb.Create(&RegionGorm{
		ID:   1,
		Data: data,
	}).Error
	if err != nil {
		return err
	}
	return nil
}

func (d *Dao) UpdateRegionGorm(region *Region) error {
	data, err := msgpack.Marshal(region)
	if err != nil {
		return err
	}
	err = d.gormDb.Updates(&RegionGorm{
		ID:   1,
		Data: data,
	}).Error
	if err != nil {
		return err
	}
	return nil
}

func (d *Dao) QueryRegionGorm() (*Region, error) {
	regionGorm := new(RegionGorm)
	err := d.gormDb.First(regionGorm).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	region := new(Region)
	err = msgpack.Unmarshal(regionGorm.Data, region)
	if err != nil {
		return nil, err
	}
	return region, nil
}
