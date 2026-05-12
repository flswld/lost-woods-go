package net

import (
	"encoding/binary"

	"hk4e/pkg/endec"

	"github.com/flswld/halo/logger"
)

// hk4e 游戏协议编解码 - KCP 数据包到协议消息的转换层
//
// 这一层负责把 KCP 传输层的二进制数据流解码为单个协议消息
// 上层 ProtoDecode 再把 KcpMsg 转换为最终的 *ProtoMsg
//
// 协议结构（带 * 为 xor 加密的部分）：
//   - KCP 头（前 28 字节）：sessionId / conv / cmd / wnd / ts / sn / una / len
//     · 由 KCP 协议库（halo/protocol/kcp）处理 这里只看里面的 payload 部分
//   - hk4e 业务包（紧跟在 KCP payload 里）：
//     · 头部幻数 0x4567（2 字节 大端 BE）
//     · cmdId（2 字节 BE）：业务命令号
//     · headLen（2 字节 BE）：PacketHead 长度
//     · payloadLen（4 字节 BE）：proto 数据长度
//     · headData（headLen 字节）：PacketHead protobuf
//     · protoData（payloadLen 字节）：业务消息 protobuf
//     · 尾部幻数 0x89AB（2 字节 BE）：作为完整性校验
//
// 整个 hk4e 业务包都被 XOR 加密 密钥来自 GetPlayerTokenRsp 的 SecretKey

/*
										原神KCP协议(带*为xor加密数据)
0			1			2					4											8(字节)
+---------------------------------------------------------------------------------------+
|					sessionId(le)			|					conv(le)				|
+---------------------------------------------------------------------------------------+
|	cmd		|	frg		|		wnd			|					ts						|
+---------------------------------------------------------------------------------------+
|						sn					|					una						|
+---------------------------------------------------------------------------------------+
|						len					|		0X4567*		|		cmdId*			|
+---------------------------------------------------------------------------------------+
|		headLen*		|				payloadLen*				|		head*			|
+---------------------------------------------------------------------------------------+
|								payload*						|		0X89AB*			|
+---------------------------------------------------------------------------------------+
*/

// KcpMsg 单个 hk4e 业务包（KCP payload 解析后的中间结构）
type KcpMsg struct {
	SessionId uint32 // KCP session id
	CmdId     uint16 // 业务命令号（已转换为服务端 3.2 版本的 cmdId）
	HeadData  []byte // PacketHead protobuf 二进制
	ProtoData []byte // 业务消息 protobuf 二进制
}

// DecodeBinToPayload 把 KCP payload 解码为 KcpMsg 列表（外层入口）
// 一个 KCP payload 可能包含多个连续的业务包 走 DecodeLoop 递归解析
// xor 解密用 session 的 SecretKey 服务端通过 GetPlayerTokenRsp 下发给客户端
func DecodeBinToPayload(data []byte, sessionId uint32, kcpMsgList *[]*KcpMsg, xorKey []byte) {
	// xor解密
	endec.Xor(data, xorKey)
	DecodeLoop(data, sessionId, kcpMsgList)
	return
}

// DecodeLoop 递归解析 KCP payload 中的所有 hk4e 业务包
//
// 校验：
//  1. 长度至少 12 字节（头 10 字节 + 尾 2 字节）
//  2. 头部幻数 0x4567
//  3. PacketMaxLen 上限保护（343KB 约定上限 防止恶意大包）
//  4. 实际长度 >= 声明长度
//  5. 尾部幻数 0x89AB
//
// 任意校验失败直接 return 不抛错（避免被恶意客户端造包刷日志）
func DecodeLoop(data []byte, sessionId uint32, kcpMsgList *[]*KcpMsg) {
	// 长度太短
	if len(data) < 12 {
		logger.Error("packet len less than 12 byte")
		return
	}
	// 头部幻数错误
	if data[0] != 0x45 || data[1] != 0x67 {
		logger.Error("packet head magic 0x4567 error")
		return
	}
	// 协议号
	cmdId := binary.BigEndian.Uint16(data[2:4])
	// 头部长度
	headLen := binary.BigEndian.Uint16(data[4:6])
	// proto长度
	protoLen := binary.BigEndian.Uint32(data[6:10])
	// 检查长度
	packetLen := int(headLen) + int(protoLen) + 12
	if packetLen > PacketMaxLen {
		logger.Error("packet len too long")
		return
	}
	if len(data) < packetLen {
		logger.Error("packet len not enough")
		return
	}
	// 尾部幻数错误
	if data[int(headLen)+int(protoLen)+10] != 0x89 || data[int(headLen)+int(protoLen)+11] != 0xAB {
		logger.Error("packet tail magic 0x89AB error")
		return
	}
	// 头部数据
	headData := data[10 : 10+int(headLen)]
	// proto数据
	protoData := data[10+int(headLen) : 10+int(headLen)+int(protoLen)]
	// 返回数据
	kcpMsg := new(KcpMsg)
	kcpMsg.SessionId = sessionId
	kcpMsg.CmdId = cmdId
	kcpMsg.HeadData = headData
	kcpMsg.ProtoData = protoData
	*kcpMsgList = append(*kcpMsgList, kcpMsg)
	// 有不止一个包 递归解析
	if len(data) > packetLen {
		DecodeLoop(data[packetLen:], sessionId, kcpMsgList)
	}
}

// EncodePayloadToBin 把 KcpMsg 编码为 KCP payload 二进制
// 与 DecodeBinToPayload 对称：组装幻数+长度字段+headData+protoData → xor 加密
// 单包大小受 PacketMaxLen 限制（一般 < 343KB）超限直接返回空切片
func EncodePayloadToBin(kcpMsg *KcpMsg, xorKey []byte) (bin []byte) {
	if kcpMsg.HeadData == nil {
		kcpMsg.HeadData = make([]byte, 0)
	}
	if kcpMsg.ProtoData == nil {
		kcpMsg.ProtoData = make([]byte, 0)
	}
	// 检查长度
	packetLen := len(kcpMsg.HeadData) + len(kcpMsg.ProtoData) + 12
	if packetLen > PacketMaxLen {
		logger.Error("packet len too long")
		return make([]byte, 0)
	}
	bin = make([]byte, packetLen)
	// 头部幻数
	bin[0] = 0x45
	bin[1] = 0x67
	// 协议号
	binary.BigEndian.PutUint16(bin[2:4], kcpMsg.CmdId)
	// 头部长度
	binary.BigEndian.PutUint16(bin[4:6], uint16(len(kcpMsg.HeadData)))
	// proto长度
	binary.BigEndian.PutUint32(bin[6:10], uint32(len(kcpMsg.ProtoData)))
	// 头部数据
	copy(bin[10:], kcpMsg.HeadData)
	// proto数据
	copy(bin[10+len(kcpMsg.HeadData):], kcpMsg.ProtoData)
	// 尾部幻数
	bin[len(bin)-2] = 0x89
	bin[len(bin)-1] = 0xAB
	// xor加密
	endec.Xor(bin, xorKey)
	return bin
}
