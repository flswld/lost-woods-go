package dao

import (
	"errors"

	"gorm.io/gorm"
)

// AccountGorm gate 账号表（GORM 模式 mysql / sqlite 用）
//
// 字段：
//   - OpenId: SDK 账号 ID（来自 dispatch 的 SdkAccount.AccountId 字符串形式）主键
//   - Uid: 玩家 uid（首次登录时通过 Node.GetNextUid 分配 1 亿+）
//   - IsForbid: 是否被封号
//   - ForbidEndTime: 封号截止时间（0 表示永久封）
//
// 一个 OpenId 对应一个 Uid（一对一关系 不支持小号机制）
// 表名 "account" Mongo 模式集合名相同
type AccountGorm struct {
	OpenId        string `gorm:"column:open_id;type:varchar(255);primaryKey"`
	Uid           uint32 `gorm:"column:uid;type:bigint(20)"`
	IsForbid      bool   `gorm:"column:is_forbid;type:tinyint(1)"`
	ForbidEndTime uint32 `gorm:"column:forbid_end_time;type:bigint(20)"`
}

func (a AccountGorm) TableName() string {
	return "account"
}

func (d *Dao) InsertAccountGorm(account *Account) error {
	err := d.gormDb.Create(&AccountGorm{
		OpenId:        account.OpenId,
		Uid:           account.Uid,
		IsForbid:      account.IsForbid,
		ForbidEndTime: account.ForbidEndTime,
	}).Error
	if err != nil {
		return err
	}
	return nil
}

func (d *Dao) QueryAccountByOpenIdGorm(openId string) (*Account, error) {
	accountGorm := new(AccountGorm)
	err := d.gormDb.Where("open_id = ?", openId).First(accountGorm).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &Account{
		OpenId:        accountGorm.OpenId,
		Uid:           accountGorm.Uid,
		IsForbid:      accountGorm.IsForbid,
		ForbidEndTime: accountGorm.ForbidEndTime,
	}, nil
}
