package api

// ComboTokenReq v2Login 请求体（Token → ComboToken 交换）
//
// Data 字段是 LoginTokenData 的 JSON 字符串（嵌套 JSON）
//   - 客户端先把 LoginTokenData 序列化为 JSON 串
//   - 再用整个 JSON 串作为 Data 字段值发出
//   - 服务端二次解析才能拿到 Token
//
// AppID/ChannelID 用 any 类型 因为客户端可能发数字也可能发字符串
type ComboTokenReq struct {
	AppID     any    `json:"app_id"`
	ChannelID any    `json:"channel_id"`
	Data      string `json:"data"`
	Device    string `json:"device"`
	Sign      string `json:"sign"`
}

type LoginTokenData struct {
	Uid   string `json:"uid"`
	Token string `json:"token"`
	Guest bool   `json:"guest"`
}
