// SDK 全局信息表 - 仅存放 1 行（ID=1）的全局状态
//
// 当前仅 1 个字段 nextSdkAccountId（账号自增计数器）
// standalone 模式：服务启动时读出来 关闭时写回 减少 DB 写入次数
// 集群模式：每次注册新账号都直接走 GetNextSdkAccountId 用 DB 自增锁

package dao

import (
	"errors"

	"hk4e/dispatch/model"

	"gorm.io/gorm"
)

func (d *Dao) InsertSdkGorm(sdk *model.Sdk) error {
	err := d.gormDb.Create(&model.SdkGorm{
		ID:               1,
		NextSdkAccountId: sdk.NextSdkAccountId,
	}).Error
	if err != nil {
		return err
	}
	return nil
}

func (d *Dao) UpdateSdkGorm(sdk *model.Sdk) error {
	err := d.gormDb.Updates(&model.SdkGorm{
		ID:               1,
		NextSdkAccountId: sdk.NextSdkAccountId,
	}).Error
	if err != nil {
		return err
	}
	return nil
}

func (d *Dao) QuerySdkGorm() (*model.Sdk, error) {
	sdkGorm := new(model.SdkGorm)
	err := d.gormDb.First(sdkGorm).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &model.Sdk{
		NextSdkAccountId: sdkGorm.NextSdkAccountId,
	}, nil
}
