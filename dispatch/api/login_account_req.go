package api

// LoginAccountRequestJson apiLogin 请求体（账号密码登录）
//
// 字段：
//   - Account: 用户名（普通模式）或 "用户名@@密码"（@@ 模式）
//   - Password: RSA 加密的密码 base64 编码（IsCrypto=true 时）或明文（false 时）
//   - IsCrypto: 是否启用 RSA 加密
//
// **特殊兼容**：dispatch.apiLogin 解密失败时 fallback 到 @@ 模式（详见 login_controller.go）
//
//	方便玩家在使用各种 hook 工具时仍能登录
type LoginAccountRequestJson struct {
	Account  string `json:"account"`
	Password string `json:"password"`
	IsCrypto bool   `json:"is_crypto"`
}
