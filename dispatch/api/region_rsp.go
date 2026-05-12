package api

// QueryCurRegionRspJson 二级 dispatch 响应（外层 JSON 包装）
//
// Content: base64 编码的 RSA 加密二进制（解密后是 QueryCurrRegionHttpRsp 的 PB 序列化）
// Sign: RSA 签名（v2.7.5+ 客户端要求 用 region_sign_key 私钥签名 客户端用公钥验证）
//
// 加密分块：256 字节一段（RSA-2048 单次最大密文长度）客户端按相同分块解密
type QueryCurRegionRspJson struct {
	Content string `json:"content"`
	Sign    string `json:"sign"`
}
