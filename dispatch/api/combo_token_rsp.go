package api

// ComboTokenRsp v2Login 响应（ComboToken 下发）
//
// 关键字段：
//   - ComboToken: 客户端连 KCP 时通过 GetPlayerTokenReq.AccountToken 提交
//   - OpenID: 玩家账号 ID（即 AccountId 字符串）
//   - Heartbeat: 是否启用心跳（项目不实现 false）
//   - FatigueRemind: 防沉迷提醒（项目不实现 nil）
//
// NewComboTokenRsp 构造默认成功响应
type ComboTokenRsp struct {
	Message string    `json:"message"`
	Retcode int       `json:"retcode"`
	Data    LoginData `json:"data"`
}

type LoginData struct {
	AccountType   int    `json:"account_type"`
	Heartbeat     bool   `json:"heartbeat"`
	ComboID       string `json:"combo_id"`
	ComboToken    string `json:"combo_token"`
	OpenID        string `json:"open_id"`
	Data          string `json:"data"`
	FatigueRemind any    `json:"fatigue_remind"`
}

func NewComboTokenRsp() (r *ComboTokenRsp) {
	r = &ComboTokenRsp{
		Message: "",
		Retcode: 0,
		Data: LoginData{
			AccountType:   1,
			Heartbeat:     false,
			ComboID:       "",
			ComboToken:    "",
			OpenID:        "",
			Data:          "{\"guest\":false}",
			FatigueRemind: nil,
		},
	}
	return r
}
