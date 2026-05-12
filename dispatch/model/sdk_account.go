package model

import (
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// SdkAccount SDK 账号表（dispatch 用）
//
// Token 与 ComboToken 区别：
//   - Token: 账号长期凭证 7 天有效（通过 apiLogin/apiVerify 拿到）
//   - ComboToken: 游戏会话凭证 24 小时有效（通过 v2Login 拿到）
//
// Token 用于"客户端记住登录"（避免每次都输入账号密码）
// ComboToken 用于客户端连 Gate 时携带 Gate 通过 /gate/token/verify 验证
//
// 密码用 MD5 存（无 salt）项目作者重视便利性多于安全性
//
//	生产部署应该改成 bcrypt 或 argon2 + salt
type SdkAccount struct {
	ID                   primitive.ObjectID `bson:"_id,omitempty"`
	AccountId            uint32             `bson:"account_id"`              // 账号id（即玩家 OpenId 自增 dao.SdkAccountIdBegin 起始）
	Username             string             `bson:"username"`                // 用户名（6-20 字符 仅大小写字母数字）
	Password             string             `bson:"password"`                // 密码 MD5（无 salt）
	Token                string             `bson:"token"`                   // 账号 Token（7 天有效）
	TokenCreateTime      uint64             `bson:"token_create_time"`       // Token 创建毫秒时间戳
	ComboToken           string             `bson:"combo_token"`             // 游戏会话 ComboToken（24 小时有效）
	ComboTokenCreateTime uint64             `bson:"combo_token_create_time"` // ComboToken 创建毫秒时间戳
}

type SdkAccountGorm struct {
	AccountId            uint32 `gorm:"column:account_id;type:bigint(20);primaryKey"`   // 账号id
	Username             string `gorm:"column:username;type:varchar(255)"`              // 用户名
	Password             string `gorm:"column:password;type:varchar(255)"`              // 密码
	Token                string `gorm:"column:token;type:varchar(255)"`                 // 账号token
	TokenCreateTime      uint64 `gorm:"column:token_create_time;type:bigint(20)"`       // 毫秒时间戳
	ComboToken           string `gorm:"column:combo_token;type:varchar(255)"`           // 游戏服务器token
	ComboTokenCreateTime uint64 `gorm:"column:combo_token_create_time;type:bigint(20)"` // 毫秒时间戳
}

func (s SdkAccountGorm) TableName() string {
	return "sdk_account"
}
