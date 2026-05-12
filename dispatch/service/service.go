package service

import (
	"hk4e/dispatch/dao"
)

// dispatch 业务服务层（仅占位 几乎没用）
//
// 当前只有 1 个方法 UserPasswordChange 用于修改密码后让现有 token 失效
// 实际密码修改接口未实现（**项目没有"用户改密码"功能**）

type Service struct {
	dao *dao.Dao
}

// UserPasswordChange 用户密码改变（让现有 Token 失效）
//
// 把 token_create_time 设为 0 → apiVerify 会因为 "Token 已失效" 失败
// 强制玩家重新输入新密码登录
//
// TODO 游戏内登录态失效（理论上还要踢已在线的玩家 但项目未实现）
func (s *Service) UserPasswordChange(accountId uint32) bool {
	// http登录态失效
	err := s.dao.UpdateSdkAccountFieldByFieldName("account_id", accountId, "token_create_time", 0)
	if err != nil {
		return false
	}
	// TODO 游戏内登录态失效
	return true
}
