package api

// LoginTokenRequest apiVerify 请求体（Token 续期验证）
//
// 客户端缓存的 Token 通过此请求续期：
//   - Uid: 客户端记住的玩家 OpenId（其实是 SdkAccount.AccountId 字符串）
//   - Token: 客户端缓存的 Token 字符串
//
// 服务端校验 Token 与 DB 一致 + 创建时间不超过 7 天
type LoginTokenRequest struct {
	Uid   string `json:"uid"`
	Token string `json:"token"`
}
