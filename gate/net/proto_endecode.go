package net

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"hk4e/common/config"
	"hk4e/pkg/object"
	"hk4e/protocol/cmd"
	"hk4e/protocol/proto"

	"github.com/flswld/halo/logger"
	"github.com/jhump/protoreflect/desc"
	"github.com/jhump/protoreflect/desc/protoparse"
	"github.com/jhump/protoreflect/dynamic"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/protowire"
	pb "google.golang.org/protobuf/proto"
)

// 客户端协议动态代理 - 项目最重要的能力之一（详见 CLAUDE.md "客户端协议动态代理"）
//
// 核心问题：
//   - 服务端协议永远基于 3.2 版本（最完整泄漏的版本）
//   - 客户端可以是 3.2 ~ 6.5 任意版本
//   - 不能为每个新版本重新生成 .pb.go 否则 gs/game 业务代码要跟着改
//
// 解决方案：在 gate 层用反射做高低版本之间的实时转换
//   - 启动时 protoparse 解析 client_proto_dir/ 目录的全部 .proto 文件
//   - 用 dynamic.Message 反序列化客户端 proto 数据（按客户端版本的描述符）
//   - 用 object.CopyProtoMsgSameField 把同名字段复制到服务端 3.2 静态 proto 对象
//   - 通过个别 Conv* 函数处理新版本结构变化（cmd 名映射 + 字段结构变化）
//
// 编解码流向（每方向都要做反向转换）：
//
//   客户端 → 服务端（ProtoDecode）：
//     KcpMsg(client cmd id + client proto data)
//     → dynamic.Message 反序列化客户端 proto
//     → Decrypt 字段级 XOR 解密
//     → ConvHighVersionProtoCmdClientToServer 转 cmd 名（如新版本重命名）
//     → CopyProtoMsgSameField 同名字段复制到服务端 proto 对象
//     → ConvSubPbDataClientToServer 处理嵌套二级 PB（CombatInvocations 里的 InvokeEntry 等）
//     → ConvHighVersionProtoDataClientToServer 个别字段手工转换
//     → KcpMsg(server cmd id + server proto data) → 给 GS
//
//   服务端 → 客户端（ProtoEncode）：上面流程的镜像反向
//
// 字段级 XOR 加密：原神客户端为防作弊会对部分 int 字段做 XOR 编码
//   算法直接写在 .proto 字段的 option（tag 50001 enc / 50002 dec）
//   ParseMsgFieldXor 启动时解析 编解码时自动应用
//
// 新版本接入流程（详见 CLAUDE.md "新版本接入步骤"）：
//   1. 把客户端 .proto 文件放到 client_proto_dir/
//   2. 重启 gate
//   3. 找出新版本对不上的字段或新增 cmd
//   4. 在 ConvHighVersionProtoCmd*/Data* 加映射或手工字段转换
//   5. 极少数情况：服务端业务逻辑改 gs handler

// ProtoMsg 解码后的协议消息（gate 内部传输用）
type ProtoMsg struct {
	SessionId      uint32            // KCP session id
	CmdId          uint16            // 服务端 cmd id（已转换为 3.2 版本）
	HeadMessage    *proto.PacketHead // 包头（含 user_id / client_seq 等）
	PayloadMessage pb.Message        // 业务消息 protobuf 对象
}

// ProtoMessage UnionCmdNotify 拆包内部用 仅含 cmdId + message
type ProtoMessage struct {
	cmdId   uint16
	message pb.Message
}

// ProtoDecode 客户端→服务端协议解码（KcpMsg → []*ProtoMsg）
//
// ClientProtoProxyEnable=false 时（仅当客户端版本是 3.2 一致时） 直接走 PB 反序列化
// ClientProtoProxyEnable=true 时（多版本部署）走完整动态代理流程：
//  1. 按 clientCmdId 查 cmdName（来自 client_proto_dir 的 client_cmd.csv 或注释）
//  2. dynamic.Message 反序列化客户端 proto 数据
//  3. Decrypt 字段级 XOR 解密
//  4. ConvHighVersionProtoCmdClientToServer 转 cmd 名映射
//  5. CopyProtoMsgSameField 同名字段复制到服务端 proto 对象
//  6. ConvSubPbDataClientToServer 处理嵌套 PB
//  7. ConvHighVersionProtoDataClientToServer 个别字段手工转换
//  8. 重新序列化为服务端 proto 数据
//
// UnionCmdNotify 特殊处理：解包后会拆成多个 ProtoMsg 每个对应一条聚合消息
// TrackPacket=true 时打印每条消息的 JSON 内容（调试用 上线慎开）
func ProtoDecode(kcpMsg *KcpMsg, serverCmdProtoMap *cmd.CmdProtoMap, clientProtoProxy *ClientProtoProxy) (protoMsgList []*ProtoMsg) {
	protoMsgList = make([]*ProtoMsg, 0)
	if config.GetConfig().Hk4e.ClientProtoProxyEnable {
		clientCmdId := kcpMsg.CmdId
		clientProtoData := kcpMsg.ProtoData
		cmdName := clientProtoProxy.GetClientCmdNameByCmdId(clientCmdId)
		if cmdName == "" {
			logger.Error("get cmdName is nil, clientCmdId: %v", clientCmdId)
			return protoMsgList
		}
		clientProtoObj := clientProtoProxy.GetClientProtoObjByName(cmdName)
		if clientProtoObj == nil {
			logger.Error("get client proto obj is nil, cmdName: %v", cmdName)
			return protoMsgList
		}
		err := clientProtoObj.Unmarshal(clientProtoData)
		if err != nil {
			logger.Error("unmarshal client proto error: %v", err)
			return protoMsgList
		}
		clientProtoProxy.Decrypt(cmdName, clientProtoObj)
		cmdName = ConvHighVersionProtoCmdClientToServer(cmdName)
		serverCmdId := serverCmdProtoMap.GetCmdIdByCmdName(cmdName)
		if serverCmdId == 0 {
			logger.Error("get server cmdId is nil, cmdName: %v", cmdName)
			return protoMsgList
		}
		serverProtoObj := serverCmdProtoMap.GetProtoObjByCmdId(serverCmdId)
		if serverProtoObj == nil {
			logger.Error("get server proto obj is nil, serverCmdId: %v", serverCmdId)
			return protoMsgList
		}
		err = object.CopyProtoMsgSameField(serverProtoObj, clientProtoObj)
		if err != nil {
			logger.Error("copy proto obj error: %v", err)
			return protoMsgList
		}
		ConvSubPbDataClientToServer(serverProtoObj, clientProtoProxy)
		ConvHighVersionProtoDataClientToServer(serverProtoObj, clientProtoObj, clientProtoProxy)
		serverProtoData, err := pb.Marshal(serverProtoObj)
		if err != nil {
			logger.Error("marshal server proto error: %v", err)
			return protoMsgList
		}
		kcpMsg.CmdId = serverCmdId
		kcpMsg.ProtoData = serverProtoData
	}
	protoMsg := new(ProtoMsg)
	protoMsg.SessionId = kcpMsg.SessionId
	protoMsg.CmdId = kcpMsg.CmdId
	// head msg
	if kcpMsg.HeadData != nil && len(kcpMsg.HeadData) != 0 {
		headMsg := new(proto.PacketHead)
		err := pb.Unmarshal(kcpMsg.HeadData, headMsg)
		if err != nil {
			logger.Error("unmarshal head data err: %v", err)
			return protoMsgList
		}
		protoMsg.HeadMessage = headMsg
	} else {
		protoMsg.HeadMessage = nil
	}
	// payload msg
	protoMessageList := make([]*ProtoMessage, 0)
	ProtoDecodePayloadLoop(kcpMsg.CmdId, kcpMsg.ProtoData, &protoMessageList, serverCmdProtoMap, clientProtoProxy)
	if len(protoMessageList) == 0 {
		logger.Error("decode proto object is nil")
		return protoMsgList
	}
	if kcpMsg.CmdId == cmd.UnionCmdNotify {
		for _, protoMessage := range protoMessageList {
			msg := new(ProtoMsg)
			msg.SessionId = kcpMsg.SessionId
			msg.CmdId = protoMessage.cmdId
			msg.HeadMessage = protoMsg.HeadMessage
			msg.PayloadMessage = protoMessage.message
			protoMsgList = append(protoMsgList, msg)
		}
		for _, msg := range protoMsgList {
			if config.GetConfig().Hk4e.TrackPacket {
				cmdName := "???"
				if msg.PayloadMessage != nil {
					cmdName = string(msg.PayloadMessage.ProtoReflect().Descriptor().FullName())
				}
				data, _ := protojson.Marshal(msg.PayloadMessage)
				var buf bytes.Buffer
				_ = json.Indent(&buf, data, "", "\t")
				logger.Debug("[RECV UNION CMD] cmdId: %v, cmdName: %v, sessionId: %v, headMsg: %v, data: %v",
					msg.CmdId, cmdName, msg.SessionId, msg.HeadMessage, buf.String())
			}
		}
	} else {
		protoMsg.PayloadMessage = protoMessageList[0].message
		protoMsgList = append(protoMsgList, protoMsg)
		if config.GetConfig().Hk4e.TrackPacket {
			cmdName := "???"
			if protoMsg.PayloadMessage != nil {
				cmdName = string(protoMsg.PayloadMessage.ProtoReflect().Descriptor().FullName())
			}
			data, _ := protojson.Marshal(protoMsg.PayloadMessage)
			var buf bytes.Buffer
			_ = json.Indent(&buf, data, "", "\t")
			logger.Info("[RECV] cmdId: %v, cmdName: %v, sessionId: %v, headMsg: %v, data: %v",
				protoMsg.CmdId, cmdName, protoMsg.SessionId, protoMsg.HeadMessage, buf.String())
		}
	}
	return protoMsgList
}

// ProtoDecodePayloadLoop UnionCmd 拆包递归处理
//
// 客户端 UnionCmdNotify 把多条业务消息打包发送（高频消息聚合 减少 KCP 包数）
// 这里展开每条 unionCmd.Body：
//  1. 单条业务消息走完整动态代理流程（与 ProtoDecode 主流程一致 但只处理这一条 不发外层包头）
//  2. 用 unionCmd.MessageId/Body 替换原值（让外层 ProtoDecode 拿到的是服务端版本数据）
//  3. 递归调用本函数处理嵌套（理论上 UnionCmd 不嵌套但保险起见）
//
// 拆出来的每条消息都会变成一个独立的 ProtoMsg 由 ProtoDecode 返回
func ProtoDecodePayloadLoop(cmdId uint16, protoData []byte, protoMessageList *[]*ProtoMessage,
	serverCmdProtoMap *cmd.CmdProtoMap, clientProtoProxy *ClientProtoProxy) {
	protoObj := DecodePayloadToProto(cmdId, protoData, serverCmdProtoMap)
	if protoObj == nil {
		logger.Error("decode proto object is nil")
		return
	}
	if cmdId == cmd.UnionCmdNotify {
		// 处理聚合消息
		unionCmdNotify, ok := protoObj.(*proto.UnionCmdNotify)
		if !ok {
			logger.Error("parse union cmd error")
			return
		}
		for _, unionCmd := range unionCmdNotify.GetCmdList() {
			if config.GetConfig().Hk4e.ClientProtoProxyEnable {
				clientCmdId := uint16(unionCmd.MessageId)
				clientProtoData := unionCmd.Body
				cmdName := clientProtoProxy.GetClientCmdNameByCmdId(clientCmdId)
				if cmdName == "" {
					logger.Error("get cmdName is nil, clientCmdId: %v", clientCmdId)
					continue
				}
				clientProtoObj := clientProtoProxy.GetClientProtoObjByName(cmdName)
				if clientProtoObj == nil {
					logger.Error("get client proto obj is nil, cmdName: %v", cmdName)
					continue
				}
				err := clientProtoObj.Unmarshal(clientProtoData)
				if err != nil {
					logger.Error("unmarshal client proto error: %v", err)
					continue
				}
				clientProtoProxy.Decrypt(cmdName, clientProtoObj)
				serverCmdId := serverCmdProtoMap.GetCmdIdByCmdName(cmdName)
				if serverCmdId == 0 {
					logger.Error("get server cmdId is nil, cmdName: %v", cmdName)
					continue
				}
				serverProtoObj := serverCmdProtoMap.GetProtoObjByCmdId(serverCmdId)
				if serverProtoObj == nil {
					logger.Error("get server proto obj is nil, serverCmdId: %v", serverCmdId)
					continue
				}
				err = object.CopyProtoMsgSameField(serverProtoObj, clientProtoObj)
				if err != nil {
					logger.Error("copy proto obj error: %v", err)
					continue
				}
				ConvSubPbDataClientToServer(serverProtoObj, clientProtoProxy)
				serverProtoData, err := pb.Marshal(serverProtoObj)
				if err != nil {
					logger.Error("marshal server proto error: %v", err)
					continue
				}
				unionCmd.MessageId = uint32(serverCmdId)
				unionCmd.Body = serverProtoData
			}
			ProtoDecodePayloadLoop(uint16(unionCmd.MessageId), unionCmd.Body, protoMessageList, serverCmdProtoMap, clientProtoProxy)
		}
	}
	*protoMessageList = append(*protoMessageList, &ProtoMessage{
		cmdId:   cmdId,
		message: protoObj,
	})
}

// ProtoEncode 服务端→客户端协议编码（ProtoMsg → KcpMsg）
//
// 与 ProtoDecode 对称的镜像操作：
//  1. 把服务端 PacketHead/PayloadMessage 序列化（基础 PB 编码）
//  2. ClientProtoProxyEnable=true 时再走客户端协议代理：
//     · 反序列化服务端 proto 数据到 dynamic.Message（先回到对象再转换）
//     · ConvHighVersionProtoCmdServerToClient 转 cmd 名（如 ChangeGameTimeReq → ClientSetGameTimeReq）
//     · ConvSubPbDataServerToClient 处理嵌套 PB
//     · CopyProtoMsgSameField 复制同名字段到客户端 proto 对象
//     · ConvHighVersionProtoDataServerToClient 个别字段手工转换
//     · Encrypt 字段级 XOR 加密（与 Decrypt 对称）
//     · 最终序列化为客户端版本的 proto 数据 + cmdId 改为客户端 cmdId
func ProtoEncode(protoMsg *ProtoMsg, serverCmdProtoMap *cmd.CmdProtoMap, clientProtoProxy *ClientProtoProxy) (kcpMsg *KcpMsg) {
	if config.GetConfig().Hk4e.TrackPacket {
		cmdName := "???"
		if protoMsg.PayloadMessage != nil {
			cmdName = string(protoMsg.PayloadMessage.ProtoReflect().Descriptor().FullName())
		}
		data, _ := protojson.Marshal(protoMsg.PayloadMessage)
		var buf bytes.Buffer
		_ = json.Indent(&buf, data, "", "\t")
		logger.Info("[SEND] cmdId: %v, cmdName: %v, sessionId: %v, headMsg: %v, data: %v",
			protoMsg.CmdId, cmdName, protoMsg.SessionId, protoMsg.HeadMessage, buf.String())
	}
	kcpMsg = new(KcpMsg)
	kcpMsg.SessionId = protoMsg.SessionId
	kcpMsg.CmdId = protoMsg.CmdId
	// head msg
	if protoMsg.HeadMessage != nil {
		headData, err := pb.Marshal(protoMsg.HeadMessage)
		if err != nil {
			logger.Error("marshal head data err: %v", err)
			return nil
		}
		kcpMsg.HeadData = headData
	} else {
		kcpMsg.HeadData = nil
	}
	// payload msg
	if protoMsg.PayloadMessage != nil {
		protoData := EncodeProtoToPayload(protoMsg.PayloadMessage, serverCmdProtoMap)
		if protoData == nil {
			logger.Error("encode proto data is nil")
			return nil
		}
		kcpMsg.ProtoData = protoData
	} else {
		kcpMsg.ProtoData = nil
	}
	if config.GetConfig().Hk4e.ClientProtoProxyEnable {
		serverCmdId := kcpMsg.CmdId
		serverProtoData := kcpMsg.ProtoData
		serverProtoObj := serverCmdProtoMap.GetProtoObjByCmdId(serverCmdId)
		if serverProtoObj == nil {
			logger.Error("get server proto obj is nil, serverCmdId: %v", serverCmdId)
			return nil
		}
		err := pb.Unmarshal(serverProtoData, serverProtoObj)
		if err != nil {
			logger.Error("unmarshal server proto error: %v", err)
			return nil
		}
		cmdName := serverCmdProtoMap.GetCmdNameByCmdId(serverCmdId)
		if cmdName == "" {
			logger.Error("get cmdName is nil, serverCmdId: %v", serverCmdId)
			return nil
		}
		cmdName = ConvHighVersionProtoCmdServerToClient(cmdName)
		clientProtoObj := clientProtoProxy.GetClientProtoObjByName(cmdName)
		if clientProtoObj == nil {
			logger.Error("get client proto obj is nil, cmdName: %v", cmdName)
			return nil
		}
		ConvSubPbDataServerToClient(serverProtoObj, clientProtoProxy)
		err = object.CopyProtoMsgSameField(clientProtoObj, serverProtoObj)
		if err != nil {
			logger.Error("copy proto obj error: %v", err)
			return nil
		}
		ConvHighVersionProtoDataServerToClient(clientProtoObj, serverProtoObj, clientProtoProxy)
		clientProtoProxy.Encrypt(cmdName, clientProtoObj)
		clientProtoData, err := clientProtoObj.Marshal()
		if err != nil {
			logger.Error("marshal client proto error: %v", err)
			return nil
		}
		clientCmdId := clientProtoProxy.GetClientCmdIdByCmdName(cmdName)
		if clientCmdId == 0 {
			logger.Error("get client cmdId is nil, cmdName: %v", cmdName)
			return nil
		}
		kcpMsg.CmdId = clientCmdId
		kcpMsg.ProtoData = clientProtoData
	}
	return kcpMsg
}

func DecodePayloadToProto(cmdId uint16, protoData []byte, serverCmdProtoMap *cmd.CmdProtoMap) (protoObj pb.Message) {
	protoObj = serverCmdProtoMap.GetProtoObjCacheByCmdId(cmdId)
	if protoObj == nil {
		logger.Error("get new proto object is nil")
		return nil
	}
	err := pb.Unmarshal(protoData, protoObj)
	if err != nil {
		logger.Error("unmarshal proto data err: %v", err)
		return nil
	}
	return protoObj
}

func EncodeProtoToPayload(protoObj pb.Message, serverCmdProtoMap *cmd.CmdProtoMap) (protoData []byte) {
	var err error = nil
	protoData, err = pb.Marshal(protoObj)
	if err != nil {
		logger.Error("marshal proto object err: %v", err)
		return nil
	}
	return protoData
}

// 客户端协议代理 - 高版本客户端 ↔ 3.2 服务端的反射桥梁

// ClientProtoProxy 客户端协议代理（启动时初始化 全局唯一）
//
// 字段：
//   - MsgDescMap: 消息名 → MessageDescriptor（jhump/protoreflect 描述符 用于 dynamic.Message）
//   - CmdIdCmdNameMap: 客户端 cmdId → cmdName 映射
//   - CmdNameCmdIdMap: cmdName → 客户端 cmdId 映射（反向）
//   - MsgFieldXorMap: 每条消息中需要做字段级 XOR 加密的字段配置列表
//     · 由 ParseMsgFieldXor 启动时从 .proto 字段 option 解析（tag 50001 enc / 50002 dec）
//     · Decrypt/Encrypt 编解码时按此配置应用 XOR 算法
type ClientProtoProxy struct {
	MsgDescMap      map[string]*desc.MessageDescriptor // 消息名 → 描述符
	CmdIdCmdNameMap map[uint16]string                  // 客户端 cmdId → cmdName
	CmdNameCmdIdMap map[string]uint16                  // cmdName → 客户端 cmdId
	MsgFieldXorMap  map[string][]*FieldXorConfig       // 消息名 → 字段 XOR 配置
}

// NewClientProtoProxy 启动时初始化客户端协议代理（gate/app 启动时调用一次）
//
// 处理：
//  1. 扫描 protoDir 下所有 .proto 文件
//  2. protoparse 解析全部文件 → MessageDescriptor
//  3. 注册到 MsgDescMap + 调 ParseMsgFieldXor 解析每个字段的 XOR 加密配置
//  4. CmdId ↔ CmdName 双向映射的两种来源（按优先级）：
//     · 优先：client_cmd.csv（cmdName,cmdId 每行一对）
//     · 回退：从每个 .proto 文件的 "// CmdId: xxx" 注释行解析
//
// **panic 行为**：任何启动失败都直接 panic（gate 没有这个能力就别启动）
//
//	protoDir 找不到 / parser 出错 / client_cmd.csv 格式错都会 panic
func NewClientProtoProxy(protoDir string) *ClientProtoProxy {
	c := new(ClientProtoProxy)
	dir, err := os.ReadDir(protoDir)
	if err != nil {
		panic(err)
	}
	protoFileList := make([]string, 0)
	for _, entry := range dir {
		if entry.IsDir() {
			continue
		}
		split := strings.Split(entry.Name(), ".")
		if len(split) < 2 || split[len(split)-1] != "proto" {
			continue
		}
		protoFileList = append(protoFileList, entry.Name())
	}
	parser := new(protoparse.Parser)
	parser.ImportPaths = []string{protoDir}
	fileDescList, err := parser.ParseFiles(protoFileList...)
	if err != nil {
		panic(err)
	}
	c.MsgDescMap = make(map[string]*desc.MessageDescriptor)
	c.MsgFieldXorMap = make(map[string][]*FieldXorConfig)
	for _, fileDesc := range fileDescList {
		for _, msgDesc := range fileDesc.GetMessageTypes() {
			c.MsgDescMap[msgDesc.GetName()] = msgDesc
			c.ParseMsgFieldXor(msgDesc)
		}
	}
	c.CmdIdCmdNameMap = make(map[uint16]string)
	c.CmdNameCmdIdMap = make(map[string]uint16)
	clientCmdFile, err := os.ReadFile(protoDir + "/client_cmd.csv")
	if err == nil {
		// 从预置client_cmd.csv文件中读取CmdId和CmdName
		clientCmdLineList := strings.Split(string(clientCmdFile), "\n")
		for _, clientCmdLine := range clientCmdLineList {
			// 清理空格以及换行符之类的
			clientCmdLine = strings.TrimSpace(clientCmdLine)
			if clientCmdLine == "" {
				continue
			}
			item := strings.Split(clientCmdLine, ",")
			if len(item) != 2 {
				panic("parse client cmd file error")
			}
			cmdName := item[0]
			cmdId, err := strconv.Atoi(item[1])
			if err != nil {
				panic(err)
			}
			c.CmdIdCmdNameMap[uint16(cmdId)] = cmdName
			c.CmdNameCmdIdMap[cmdName] = uint16(cmdId)
		}
	} else {
		// 从proto文件中读取CmdId和CmdName
		for _, protoFile := range protoFileList {
			protoFileData, err := os.ReadFile(protoDir + "/" + protoFile)
			if err != nil {
				panic(err)
			}
			protoFileStr := string(protoFileData)
			protoFileStr = strings.ReplaceAll(protoFileStr, "\r\n", "\n")
			protoFileStr = strings.ReplaceAll(protoFileStr, "\r", "\n")
			protoFileLineList := strings.Split(protoFileStr, "\n")
			for index, line := range protoFileLineList {
				if strings.Contains(line, "// CmdId: ") || strings.Contains(line, "// CmdID: ") {
					lineSplit := strings.Split(line, " ")
					if len(lineSplit) >= 3 {
						cmdId, err := strconv.Atoi(lineSplit[2])
						if err != nil {
							continue
						}
						for _, nextLine := range protoFileLineList[index+1:] {
							if strings.Contains(nextLine, "message ") && strings.Contains(nextLine, " {") {
								nextLineSplit := strings.Split(nextLine, " ")
								if len(nextLineSplit) == 3 {
									cmdName := nextLineSplit[1]
									c.CmdIdCmdNameMap[uint16(cmdId)] = cmdName
									c.CmdNameCmdIdMap[cmdName] = uint16(cmdId)
								}
								break
							}
						}
					}
				}
			}
		}
	}
	return c
}

func (c *ClientProtoProxy) GetClientCmdNameByCmdId(cmdId uint16) string {
	cmdName, exist := c.CmdIdCmdNameMap[cmdId]
	if !exist {
		logger.Error("unknown cmd id: %v", cmdId)
		return ""
	}
	return cmdName
}

func (c *ClientProtoProxy) GetClientCmdIdByCmdName(cmdName string) uint16 {
	cmdId, exist := c.CmdNameCmdIdMap[cmdName]
	if !exist {
		logger.Error("unknown cmd name: %v", cmdName)
		return 0
	}
	return cmdId
}

func (c *ClientProtoProxy) GetClientProtoObjByName(protoObjName string) *dynamic.Message {
	msgDesc, exist := c.MsgDescMap[protoObjName]
	if !exist {
		logger.Error("unknown proto obj name: %v", protoObjName)
		return nil
	}
	dMsg := dynamic.NewMessage(msgDesc)
	return dMsg
}

// int 字段 XOR 加解密 - 原神客户端反作弊机制

// XorAlg XOR 算法配置
//
// 算法形式：(val OP1 KEY1) OP2 KEY2
// OP 是 + - ^ 之一 KEY 是 16 位无符号数
// 例：(val + 123) ^ 456 → 把字段值先加 123 再异或 456
//
// 加密用 EncAlg 解密用 DecAlg（理论上互逆但客户端 .proto 里分别配置）
type XorAlg struct {
	Op1  byte   // 第一步操作符（+/-/^）
	Key1 uint16 // 第一步密钥
	Op2  byte   // 第二步操作符
	Key2 uint16 // 第二步密钥
}

func (x XorAlg) String() string {
	return fmt.Sprintf("(val %s %d) %s %d", string(x.Op1), x.Key1, string(x.Op2), x.Key2)
}

type FieldXorConfig struct {
	FieldName   string
	FieldNumber int32
	EncAlg      XorAlg
	DecAlg      XorAlg
}

func (c *ClientProtoProxy) ParseXorAlg(s string) *XorAlg {
	p := `\(\s*val\s*([\+\-\^])\s*(\d+)\s*\)\s*([\+\-\^])\s*(\d+)`
	r := regexp.MustCompile(p)
	m := r.FindStringSubmatch(s)
	if m == nil {
		return nil
	}
	op1 := m[1]
	key1 := m[2]
	op2 := m[3]
	key2 := m[4]
	k1, _ := strconv.Atoi(key1)
	k2, _ := strconv.Atoi(key2)
	return &XorAlg{
		Op1:  op1[0],
		Key1: uint16(k1),
		Op2:  op2[0],
		Key2: uint16(k2),
	}
}

// ParseMsgFieldXor 启动时解析消息中需要 XOR 加密的字段
//
// 来源：proto 字段的自定义 option（tag 50001=encAlg / 50002=decAlg）
// 通过 protoreflect 取 fieldOption 的 unknown bytes 然后用 protowire 解析
// 形式如：
//
//	int32 myField = 1 [(enc) = "(val + 123) ^ 456", (dec) = "(val ^ 456) - 123"];
//
// 解析后存入 MsgFieldXorMap 编解码时按消息名查到字段列表 应用 XOR 算法
func (c *ClientProtoProxy) ParseMsgFieldXor(msg *desc.MessageDescriptor) {
	fieldXorConfigList := make([]*FieldXorConfig, 0)
	for _, field := range msg.GetFields() {
		fieldOptionRef := field.GetFieldOptions().ProtoReflect()
		data := fieldOptionRef.GetUnknown()
		if len(data) == 0 {
			continue
		}
		var encAlg *XorAlg = nil
		var decAlg *XorAlg = nil
		for {
			tag, n := protowire.ConsumeVarint(data)
			if n < 0 {
				break
			}
			data = data[n:]
			fieldNum, wireType := protowire.DecodeTag(tag)
			if wireType != protowire.BytesType {
				break
			}
			s, n := protowire.ConsumeString(data)
			if n < 0 {
				break
			}
			xorAlg := c.ParseXorAlg(s)
			if xorAlg == nil {
				break
			}
			switch fieldNum {
			case 50001:
				encAlg = xorAlg
			case 50002:
				decAlg = xorAlg
			}
			if n == len(data) {
				break
			}
			data = data[n:]
		}
		if encAlg == nil || decAlg == nil {
			continue
		}
		fieldXorConfigList = append(fieldXorConfigList, &FieldXorConfig{
			FieldName:   field.GetName(),
			FieldNumber: field.GetNumber(),
			EncAlg:      *encAlg,
			DecAlg:      *decAlg,
		})
	}
	if len(fieldXorConfigList) > 0 {
		c.MsgFieldXorMap[msg.GetName()] = fieldXorConfigList
	}
}

// Decrypt 字段级 XOR 解密（客户端 → 服务端方向）
//
// 遍历消息中所有需要解密的字段 应用 DecAlg 还原原值
// 支持的整型：int32 / int64 / uint32 / uint64
// 算法：先取出当前值 → 第一步 OP1+KEY1 → 第二步 OP2+KEY2 → 写回字段
func (c *ClientProtoProxy) Decrypt(name string, dMsg *dynamic.Message) {
	fieldXorConfigList := c.MsgFieldXorMap[name]
	if fieldXorConfigList == nil {
		return
	}
	for _, fieldXorConfig := range fieldXorConfigList {
		val := dMsg.GetFieldByNumber(int(fieldXorConfig.FieldNumber))
		v := int64(0)
		switch val.(type) {
		case int32:
			v = int64(val.(int32))
		case int64:
			v = val.(int64)
		case uint32:
			v = int64(val.(uint32))
		case uint64:
			v = int64(val.(uint64))
		}
		switch fieldXorConfig.DecAlg.Op1 {
		case '+':
			v += int64(fieldXorConfig.DecAlg.Key1)
		case '-':
			v -= int64(fieldXorConfig.DecAlg.Key1)
		case '^':
			v ^= int64(fieldXorConfig.DecAlg.Key1)
		}
		switch fieldXorConfig.DecAlg.Op2 {
		case '+':
			v += int64(fieldXorConfig.DecAlg.Key2)
		case '-':
			v -= int64(fieldXorConfig.DecAlg.Key2)
		case '^':
			v ^= int64(fieldXorConfig.DecAlg.Key2)
		}
		switch val.(type) {
		case int32:
			dMsg.SetFieldByNumber(int(fieldXorConfig.FieldNumber), int32(v))
			logger.Debug("[Decrypt] msg:%v field:%v enc:%v dec:%v", name, fieldXorConfig.FieldName, val, int32(v))
		case int64:
			dMsg.SetFieldByNumber(int(fieldXorConfig.FieldNumber), v)
			logger.Debug("[Decrypt] msg:%v field:%v enc:%v dec:%v", name, fieldXorConfig.FieldName, val, v)
		case uint32:
			dMsg.SetFieldByNumber(int(fieldXorConfig.FieldNumber), uint32(v))
			logger.Debug("[Decrypt] msg:%v field:%v enc:%v dec:%v", name, fieldXorConfig.FieldName, val, uint32(v))
		case uint64:
			dMsg.SetFieldByNumber(int(fieldXorConfig.FieldNumber), uint64(v))
			logger.Debug("[Decrypt] msg:%v field:%v enc:%v dec:%v", name, fieldXorConfig.FieldName, val, uint64(v))
		}
	}
}

// Encrypt 字段级 XOR 加密（服务端 → 客户端方向）
// 与 Decrypt 对称操作 应用 EncAlg 把原值编码成客户端期望的形式
func (c *ClientProtoProxy) Encrypt(name string, dMsg *dynamic.Message) {
	fieldXorConfigList := c.MsgFieldXorMap[name]
	if fieldXorConfigList == nil {
		return
	}
	for _, fieldXorConfig := range fieldXorConfigList {
		val := dMsg.GetFieldByNumber(int(fieldXorConfig.FieldNumber))
		v := int64(0)
		switch val.(type) {
		case int32:
			v = int64(val.(int32))
		case int64:
			v = val.(int64)
		case uint32:
			v = int64(val.(uint32))
		case uint64:
			v = int64(val.(uint64))
		}
		switch fieldXorConfig.EncAlg.Op1 {
		case '+':
			v += int64(fieldXorConfig.EncAlg.Key1)
		case '-':
			v -= int64(fieldXorConfig.EncAlg.Key1)
		case '^':
			v ^= int64(fieldXorConfig.EncAlg.Key1)
		}
		switch fieldXorConfig.EncAlg.Op2 {
		case '+':
			v += int64(fieldXorConfig.EncAlg.Key2)
		case '-':
			v -= int64(fieldXorConfig.EncAlg.Key2)
		case '^':
			v ^= int64(fieldXorConfig.EncAlg.Key2)
		}
		switch val.(type) {
		case int32:
			dMsg.SetFieldByNumber(int(fieldXorConfig.FieldNumber), int32(v))
			logger.Debug("[Encrypt] msg:%v field:%v dec:%v enc:%v", name, fieldXorConfig.FieldName, val, int32(v))
		case int64:
			dMsg.SetFieldByNumber(int(fieldXorConfig.FieldNumber), v)
			logger.Debug("[Encrypt] msg:%v field:%v dec:%v enc:%v", name, fieldXorConfig.FieldName, val, v)
		case uint32:
			dMsg.SetFieldByNumber(int(fieldXorConfig.FieldNumber), uint32(v))
			logger.Debug("[Encrypt] msg:%v field:%v dec:%v enc:%v", name, fieldXorConfig.FieldName, val, uint32(v))
		case uint64:
			dMsg.SetFieldByNumber(int(fieldXorConfig.FieldNumber), uint64(v))
			logger.Debug("[Encrypt] msg:%v field:%v dec:%v enc:%v", name, fieldXorConfig.FieldName, val, uint64(v))
		}
	}
}

// 二级 PB 数据转换 - 处理"消息里嵌套的 PB 二进制"
//
// 战斗事件协议有两层 PB：
//   - 外层：CombatInvocationsNotify / AbilityInvocationsNotify 等
//   - 内层：每个 InvokeEntry.CombatData/AbilityData 是 bytes 类型 内容是另一个 PB 对象
//
// 内层 PB 也需要做版本转换 但 CopyProtoMsgSameField 不会递归到 bytes 字段
// 所以这里手动按 ArgumentType 解析内层 PB 转换后再序列化回 bytes
//
// 已处理的 ArgumentType（HandleCombatInvokeEntry/HandleAbilityInvokeEntry 内的 case）：
//   - Combat 18 种：BEING_HIT/ENTITY_MOVE/动画/位移/治疗 等
//   - Ability 几十种：META 系列 / ACTION 系列 / MIXIN 系列
// 新版本如果加新的 ArgumentType 需要在对应 Handle 函数加 case

const (
	ClientPbDataToServer = iota // 客户端 PB → 服务端 PB
	ServerPbDataToClient        // 服务端 PB → 客户端 PB
)

// ConvSubPbDataClientToServer 客户端→服务端方向的二级 PB 转换（只处理特定消息）
// 仅 4 个消息有二级 PB：CombatInvocationsNotify / AbilityInvocationsNotify /
//
//	ClientAbilityInitFinishNotify / ClientAbilityChangeNotify
//
// 调用方：ProtoDecode 在 CopyProtoMsgSameField 之后调用
func ConvSubPbDataClientToServer(protoObj pb.Message, clientProtoProxy *ClientProtoProxy) pb.Message {
	cmdName := string(protoObj.ProtoReflect().Descriptor().FullName())
	if strings.Contains(cmdName, "proto.") {
		cmdName = strings.Split(cmdName, ".")[1]
	}
	switch cmdName {
	case "CombatInvocationsNotify":
		ntf := protoObj.(*proto.CombatInvocationsNotify)
		for _, entry := range ntf.InvokeList {
			HandleCombatInvokeEntry(ClientPbDataToServer, entry, clientProtoProxy)
		}
	case "AbilityInvocationsNotify":
		ntf := protoObj.(*proto.AbilityInvocationsNotify)
		for _, entry := range ntf.Invokes {
			HandleAbilityInvokeEntry(ClientPbDataToServer, entry, clientProtoProxy)
		}
	case "ClientAbilityInitFinishNotify":
		ntf := protoObj.(*proto.ClientAbilityInitFinishNotify)
		for _, entry := range ntf.Invokes {
			HandleAbilityInvokeEntry(ClientPbDataToServer, entry, clientProtoProxy)
		}
	case "ClientAbilityChangeNotify":
		ntf := protoObj.(*proto.ClientAbilityChangeNotify)
		for _, entry := range ntf.Invokes {
			HandleAbilityInvokeEntry(ClientPbDataToServer, entry, clientProtoProxy)
		}
	}
	return protoObj
}

// ConvSubPbDataServerToClient 服务端→客户端方向的二级 PB 转换（与 ClientToServer 镜像）
// 调用方：ProtoEncode 在 CopyProtoMsgSameField 之前调用
func ConvSubPbDataServerToClient(protoObj pb.Message, clientProtoProxy *ClientProtoProxy) pb.Message {
	cmdName := string(protoObj.ProtoReflect().Descriptor().FullName())
	if strings.Contains(cmdName, "proto.") {
		cmdName = strings.Split(cmdName, ".")[1]
	}
	switch cmdName {
	case "CombatInvocationsNotify":
		ntf := protoObj.(*proto.CombatInvocationsNotify)
		for _, entry := range ntf.InvokeList {
			HandleCombatInvokeEntry(ServerPbDataToClient, entry, clientProtoProxy)
		}
	case "AbilityInvocationsNotify":
		ntf := protoObj.(*proto.AbilityInvocationsNotify)
		for _, entry := range ntf.Invokes {
			HandleAbilityInvokeEntry(ServerPbDataToClient, entry, clientProtoProxy)
		}
	case "ClientAbilityInitFinishNotify":
		ntf := protoObj.(*proto.ClientAbilityInitFinishNotify)
		for _, entry := range ntf.Invokes {
			HandleAbilityInvokeEntry(ServerPbDataToClient, entry, clientProtoProxy)
		}
	case "ClientAbilityChangeNotify":
		ntf := protoObj.(*proto.ClientAbilityChangeNotify)
		for _, entry := range ntf.Invokes {
			HandleAbilityInvokeEntry(ServerPbDataToClient, entry, clientProtoProxy)
		}
	}
	return protoObj
}

// ConvSubPbData 单个二级 PB 字段的版本转换（HandleXxxInvokeEntry 各个 case 都调它）
//
// 处理：
//  1. 把 bytes 数据反序列化为对应版本的 dynamic/static PB 对象
//  2. CopyProtoMsgSameField 同名字段复制到目标版本对象
//  3. 重新序列化为 bytes 写回 protoDataRef
//
// 通过 protoDataRef 引用就地修改 调用方的 entry.CombatData/AbilityData 直接被替换
func ConvSubPbData(convType int, protoObjName string, serverProtoObj pb.Message, protoDataRef *[]byte,
	clientProtoProxy *ClientProtoProxy) {
	switch convType {
	case ClientPbDataToServer:
		clientProtoObj := clientProtoProxy.GetClientProtoObjByName(protoObjName)
		if clientProtoObj == nil {
			return
		}
		err := clientProtoObj.Unmarshal(*protoDataRef)
		if err != nil {
			return
		}
		err = object.CopyProtoMsgSameField(serverProtoObj, clientProtoObj)
		if err != nil {
			return
		}
		serverProtoData, err := pb.Marshal(serverProtoObj)
		if err != nil {
			return
		}
		*protoDataRef = serverProtoData
	case ServerPbDataToClient:
		err := pb.Unmarshal(*protoDataRef, serverProtoObj)
		if err != nil {
			return
		}
		clientProtoObj := clientProtoProxy.GetClientProtoObjByName(protoObjName)
		if clientProtoObj == nil {
			return
		}
		err = object.CopyProtoMsgSameField(clientProtoObj, serverProtoObj)
		if err != nil {
			return
		}
		clientProtoData, err := clientProtoObj.Marshal()
		if err != nil {
			return
		}
		*protoDataRef = clientProtoData
	}
}

// HandleCombatInvokeEntry 战斗事件二级 PB 转换（按 ArgumentType 分发到 18 种事件类型）
//
// 当前覆盖：BEING_HIT / 动画状态 / 朝向 / 攻击目标 / 移动 / 治疗等
// 新版本如果加新的 CombatTypeArgument 需要在这里加 case 否则该字段不会做版本转换
func HandleCombatInvokeEntry(convType int, entry *proto.CombatInvokeEntry, clientProtoProxy *ClientProtoProxy) {
	switch entry.ArgumentType {
	case proto.CombatTypeArgument_COMBAT_EVT_BEING_HIT:
		ConvSubPbData(convType, "EvtBeingHitInfo", new(proto.EvtBeingHitInfo), &entry.CombatData, clientProtoProxy)
	case proto.CombatTypeArgument_COMBAT_ANIMATOR_STATE_CHANGED:
		ConvSubPbData(convType, "EvtAnimatorStateChangedInfo", new(proto.EvtAnimatorStateChangedInfo), &entry.CombatData, clientProtoProxy)
	case proto.CombatTypeArgument_COMBAT_FACE_TO_DIR:
		ConvSubPbData(convType, "EvtFaceToDirInfo", new(proto.EvtFaceToDirInfo), &entry.CombatData, clientProtoProxy)
	case proto.CombatTypeArgument_COMBAT_SET_ATTACK_TARGET:
		ConvSubPbData(convType, "EvtSetAttackTargetInfo", new(proto.EvtSetAttackTargetInfo), &entry.CombatData, clientProtoProxy)
	case proto.CombatTypeArgument_COMBAT_RUSH_MOVE:
		ConvSubPbData(convType, "EvtRushMoveInfo", new(proto.EvtRushMoveInfo), &entry.CombatData, clientProtoProxy)
	case proto.CombatTypeArgument_COMBAT_ANIMATOR_PARAMETER_CHANGED:
		ConvSubPbData(convType, "EvtAnimatorParameterInfo", new(proto.EvtAnimatorParameterInfo), &entry.CombatData, clientProtoProxy)
	case proto.CombatTypeArgument_ENTITY_MOVE:
		ConvSubPbData(convType, "EntityMoveInfo", new(proto.EntityMoveInfo), &entry.CombatData, clientProtoProxy)
	case proto.CombatTypeArgument_SYNC_ENTITY_POSITION:
		ConvSubPbData(convType, "EvtSyncEntityPositionInfo", new(proto.EvtSyncEntityPositionInfo), &entry.CombatData, clientProtoProxy)
	case proto.CombatTypeArgument_COMBAT_STEER_MOTION_INFO:
		ConvSubPbData(convType, "EvtCombatSteerMotionInfo", new(proto.EvtCombatSteerMotionInfo), &entry.CombatData, clientProtoProxy)
	case proto.CombatTypeArgument_COMBAT_FORCE_SET_POS_INFO:
		ConvSubPbData(convType, "EvtCombatForceSetPosInfo", new(proto.EvtCombatForceSetPosInfo), &entry.CombatData, clientProtoProxy)
	case proto.CombatTypeArgument_COMBAT_COMPENSATE_POS_DIFF:
		ConvSubPbData(convType, "EvtCompensatePosDiffInfo", new(proto.EvtCompensatePosDiffInfo), &entry.CombatData, clientProtoProxy)
	case proto.CombatTypeArgument_COMBAT_MONSTER_DO_BLINK:
		ConvSubPbData(convType, "EvtMonsterDoBlink", new(proto.EvtMonsterDoBlink), &entry.CombatData, clientProtoProxy)
	case proto.CombatTypeArgument_COMBAT_FIXED_RUSH_MOVE:
		ConvSubPbData(convType, "EvtFixedRushMove", new(proto.EvtFixedRushMove), &entry.CombatData, clientProtoProxy)
	case proto.CombatTypeArgument_COMBAT_SYNC_TRANSFORM:
		ConvSubPbData(convType, "EvtSyncTransform", new(proto.EvtSyncTransform), &entry.CombatData, clientProtoProxy)
	case proto.CombatTypeArgument_COMBAT_LIGHT_CORE_MOVE:
		ConvSubPbData(convType, "EvtLightCoreMove", new(proto.EvtLightCoreMove), &entry.CombatData, clientProtoProxy)
	case proto.CombatTypeArgument_COMBAT_BEING_HEALED_NTF:
		ConvSubPbData(convType, "EvtBeingHealedNotify", new(proto.EvtBeingHealedNotify), &entry.CombatData, clientProtoProxy)
	case proto.CombatTypeArgument_COMBAT_SKILL_ANCHOR_POSITION_NTF:
		ConvSubPbData(convType, "EvtSyncSkillAnchorPosition", new(proto.EvtSyncSkillAnchorPosition), &entry.CombatData, clientProtoProxy)
	case proto.CombatTypeArgument_COMBAT_GRAPPLING_HOOK_MOVE:
		ConvSubPbData(convType, "EvtGrapplingHookMove", new(proto.EvtGrapplingHookMove), &entry.CombatData, clientProtoProxy)
	}
}

// HandleAbilityInvokeEntry 能力事件二级 PB 转换（按 ArgumentType 分发到几十种能力类型）
//
// 覆盖三大类：
//   - META_*: 元能力操作（修改器/参数/能力增删/元素强度等）
//   - ACTION_*: 能力动作（创建gadget/伤害/位移/治疗等）
//   - MIXIN_*: 持续混入（持续效果/动画事件等）
//
// 这是 gate 协议代理需要扩展最频繁的地方 新版本经常加新的 ArgumentType
func HandleAbilityInvokeEntry(convType int, entry *proto.AbilityInvokeEntry, clientProtoProxy *ClientProtoProxy) {
	switch entry.ArgumentType {
	case proto.AbilityInvokeArgument_ABILITY_META_MODIFIER_CHANGE:
		ConvSubPbData(convType, "AbilityMetaModifierChange", new(proto.AbilityMetaModifierChange), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_META_SPECIAL_FLOAT_ARGUMENT:
		ConvSubPbData(convType, "AbilityMetaSpecialFloatArgument", new(proto.AbilityMetaSpecialFloatArgument), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_META_OVERRIDE_PARAM:
		ConvSubPbData(convType, "AbilityScalarValueEntry", new(proto.AbilityScalarValueEntry), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_META_CLEAR_OVERRIDE_PARAM:
		ConvSubPbData(convType, "AbilityString", new(proto.AbilityString), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_META_REINIT_OVERRIDEMAP:
		ConvSubPbData(convType, "AbilityMetaReInitOverrideMap", new(proto.AbilityMetaReInitOverrideMap), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_META_GLOBAL_FLOAT_VALUE:
		ConvSubPbData(convType, "AbilityScalarValueEntry", new(proto.AbilityScalarValueEntry), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_META_CLEAR_GLOBAL_FLOAT_VALUE:
		ConvSubPbData(convType, "AbilityString", new(proto.AbilityString), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_META_ABILITY_ELEMENT_STRENGTH:
		ConvSubPbData(convType, "AbilityFloatValue", new(proto.AbilityFloatValue), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_META_ADD_OR_GET_ABILITY_AND_TRIGGER:
		ConvSubPbData(convType, "AbilityMetaAddOrGetAbilityAndTrigger", new(proto.AbilityMetaAddOrGetAbilityAndTrigger), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_META_SET_KILLED_SETATE:
		ConvSubPbData(convType, "AbilityMetaSetKilledState", new(proto.AbilityMetaSetKilledState), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_META_SET_ABILITY_TRIGGER:
		ConvSubPbData(convType, "AbilityMetaSetAbilityTrigger", new(proto.AbilityMetaSetAbilityTrigger), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_META_ADD_NEW_ABILITY:
		ConvSubPbData(convType, "AbilityMetaAddAbility", new(proto.AbilityMetaAddAbility), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_META_SET_MODIFIER_APPLY_ENTITY:
		ConvSubPbData(convType, "AbilityMetaSetModifierApplyEntityId", new(proto.AbilityMetaSetModifierApplyEntityId), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_META_MODIFIER_DURABILITY_CHANGE:
		ConvSubPbData(convType, "AbilityMetaModifierDurabilityChange", new(proto.AbilityMetaModifierDurabilityChange), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_META_ELEMENT_REACTION_VISUAL:
		ConvSubPbData(convType, "AbilityMetaElementReactionVisual", new(proto.AbilityMetaElementReactionVisual), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_META_SET_POSE_PARAMETER:
		ConvSubPbData(convType, "AbilityMetaSetPoseParameter", new(proto.AbilityMetaSetPoseParameter), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_META_UPDATE_BASE_REACTION_DAMAGE:
		ConvSubPbData(convType, "AbilityMetaUpdateBaseReactionDamage", new(proto.AbilityMetaUpdateBaseReactionDamage), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_META_TRIGGER_ELEMENT_REACTION:
		ConvSubPbData(convType, "AbilityMetaTriggerElementReaction", new(proto.AbilityMetaTriggerElementReaction), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_META_LOSE_HP:
		ConvSubPbData(convType, "AbilityMetaLoseHp", new(proto.AbilityMetaLoseHp), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_META_DURABILITY_IS_ZERO:
		ConvSubPbData(convType, "AbilityMetaDurabilityIsZero", new(proto.AbilityMetaDurabilityIsZero), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_ACTION_TRIGGER_ABILITY:
		ConvSubPbData(convType, "AbilityActionTriggerAbility", new(proto.AbilityActionTriggerAbility), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_ACTION_SET_CRASH_DAMAGE:
		ConvSubPbData(convType, "AbilityActionSetCrashDamage", new(proto.AbilityActionSetCrashDamage), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_ACTION_SUMMON:
		ConvSubPbData(convType, "AbilityActionSummon", new(proto.AbilityActionSummon), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_ACTION_BLINK:
		ConvSubPbData(convType, "AbilityActionBlink", new(proto.AbilityActionBlink), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_ACTION_CREATE_GADGET:
		ConvSubPbData(convType, "AbilityActionCreateGadget", new(proto.AbilityActionCreateGadget), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_ACTION_APPLY_LEVEL_MODIFIER:
		ConvSubPbData(convType, "AbilityApplyLevelModifier", new(proto.AbilityApplyLevelModifier), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_ACTION_GENERATE_ELEM_BALL:
		ConvSubPbData(convType, "AbilityActionGenerateElemBall", new(proto.AbilityActionGenerateElemBall), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_ACTION_SET_RANDOM_OVERRIDE_MAP_VALUE:
		ConvSubPbData(convType, "AbilityActionSetRandomOverrideMapValue", new(proto.AbilityActionSetRandomOverrideMapValue), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_ACTION_SERVER_MONSTER_LOG:
		ConvSubPbData(convType, "AbilityActionServerMonsterLog", new(proto.AbilityActionServerMonsterLog), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_ACTION_CREATE_TILE:
		ConvSubPbData(convType, "AbilityActionCreateTile", new(proto.AbilityActionCreateTile), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_ACTION_DESTROY_TILE:
		ConvSubPbData(convType, "AbilityActionDestroyTile", new(proto.AbilityActionDestroyTile), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_ACTION_FIRE_AFTER_IMAGE:
		ConvSubPbData(convType, "AbilityActionFireAfterImgae", new(proto.AbilityActionFireAfterImgae), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_ACTION_DEDUCT_STAMINA:
		ConvSubPbData(convType, "AbilityActionDeductStamina", new(proto.AbilityActionDeductStamina), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_ACTION_HIT_EFFECT:
		ConvSubPbData(convType, "AbilityActionHitEffect", new(proto.AbilityActionHitEffect), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_ACTION_SET_BULLET_TRACK_TARGET:
		ConvSubPbData(convType, "AbilityActionSetBulletTrackTarget", new(proto.AbilityActionSetBulletTrackTarget), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_MIXIN_AVATAR_STEER_BY_CAMERA:
		ConvSubPbData(convType, "AbilityMixinAvatarSteerByCamera", new(proto.AbilityMixinAvatarSteerByCamera), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_MIXIN_WIND_ZONE:
		ConvSubPbData(convType, "AbilityMixinWindZone", new(proto.AbilityMixinWindZone), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_MIXIN_COST_STAMINA:
		ConvSubPbData(convType, "AbilityMixinCostStamina", new(proto.AbilityMixinCostStamina), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_MIXIN_ELEMENT_SHIELD:
		ConvSubPbData(convType, "AbilityMixinElementShield", new(proto.AbilityMixinElementShield), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_MIXIN_GLOBAL_SHIELD:
		ConvSubPbData(convType, "AbilityMixinGlobalShield", new(proto.AbilityMixinGlobalShield), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_MIXIN_SHIELD_BAR:
		ConvSubPbData(convType, "AbilityMixinShieldBar", new(proto.AbilityMixinShieldBar), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_MIXIN_WIND_SEED_SPAWNER:
		ConvSubPbData(convType, "AbilityMixinWindSeedSpawner", new(proto.AbilityMixinWindSeedSpawner), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_MIXIN_DO_ACTION_BY_ELEMENT_REACTION:
		ConvSubPbData(convType, "AbilityMixinDoActionByElementReaction", new(proto.AbilityMixinDoActionByElementReaction), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_MIXIN_FIELD_ENTITY_COUNT_CHANGE:
		ConvSubPbData(convType, "AbilityMixinFieldEntityCountChange", new(proto.AbilityMixinFieldEntityCountChange), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_MIXIN_SCENE_PROP_SYNC:
		ConvSubPbData(convType, "AbilityMixinScenePropSync", new(proto.AbilityMixinScenePropSync), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_MIXIN_WIDGET_MP_SUPPORT:
		ConvSubPbData(convType, "AbilityMixinWidgetMpSupport", new(proto.AbilityMixinWidgetMpSupport), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_MIXIN_DO_ACTION_BY_SELF_MODIFIER_ELEMENT_DURABILITY_RATIO:
		ConvSubPbData(convType, "AbilityMixinDoActionBySelfModifierElementDurabilityRatio", new(proto.AbilityMixinDoActionBySelfModifierElementDurabilityRatio), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_MIXIN_FIREWORKS_LAUNCHER:
		ConvSubPbData(convType, "AbilityMixinFireworksLauncher", new(proto.AbilityMixinFireworksLauncher), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_MIXIN_ATTACK_RESULT_CREATE_COUNT:
		ConvSubPbData(convType, "AttackResultCreateCount", new(proto.AttackResultCreateCount), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_MIXIN_UGC_TIME_CONTROL:
		ConvSubPbData(convType, "AbilityMixinUGCTimeControl", new(proto.AbilityMixinUGCTimeControl), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_MIXIN_AVATAR_COMBAT:
		ConvSubPbData(convType, "AbilityMixinAvatarCombat", new(proto.AbilityMixinAvatarCombat), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_MIXIN_UI_INTERACT:
		ConvSubPbData(convType, "AbilityMixinUIInteract", new(proto.AbilityMixinUIInteract), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_MIXIN_SHOOT_FROM_CAMERA:
		ConvSubPbData(convType, "AbilityMixinShootFromCamera", new(proto.AbilityMixinShootFromCamera), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_MIXIN_ERASE_BRICK_ACTIVITY:
		ConvSubPbData(convType, "AbilityMixinEraseBrickActivity", new(proto.AbilityMixinEraseBrickActivity), &entry.AbilityData, clientProtoProxy)
	case proto.AbilityInvokeArgument_ABILITY_MIXIN_BREAKOUT:
		ConvSubPbData(convType, "AbilityMixinBreakout", new(proto.AbilityMixinBreakout), &entry.AbilityData, clientProtoProxy)
	}
}

// 高版本协议兼容 - 处理新版本对 cmd 的重命名 + 字段结构变化
//
// 触发开关：HighVersionProtoConvEnable=true
//
// 已知特殊适配点（每出新版本可能需要扩展 详见 CLAUDE.md "已知特殊适配点"）：
//   - cmd 名映射 ConvHighVersionProtoCmd*：目前只有 ChangeGameTimeReq ↔ ClientSetGameTimeReq 一对
//   - 上行特殊数据 ConvHighVersionProtoDataClientToServer：
//     · ChangeGameTimeReq / AvatarUpgradeReq（item_param_list 改结构）/ WeaponAwakenReq
//   - 下行特殊数据 ConvHighVersionProtoDataServerToClient：
//     · SceneEntityAppearNotify（TrifleItem → TrifleGadgetInfo 包装变化）
//     · ChangeGameTimeRsp

// ConvHighVersionProtoCmdServerToClient cmd 名转换 服务端 → 客户端
// 例：服务端 ChangeGameTimeRsp → 客户端 ClientSetGameTimeRsp
func ConvHighVersionProtoCmdServerToClient(cmdName string) string {
	if !config.GetConfig().Hk4e.HighVersionProtoConvEnable {
		return cmdName
	}
	switch cmdName {
	case "ChangeGameTimeRsp":
		return "ClientSetGameTimeRsp"
	default:
		return cmdName
	}
}

// ConvHighVersionProtoCmdClientToServer cmd 名转换 客户端 → 服务端（Server 方向反向）
func ConvHighVersionProtoCmdClientToServer(cmdName string) string {
	if !config.GetConfig().Hk4e.HighVersionProtoConvEnable {
		return cmdName
	}
	switch cmdName {
	case "ClientSetGameTimeReq":
		return "ChangeGameTimeReq"
	default:
		return cmdName
	}
}

// ConvHighVersionProtoDataServerToClient 服务端 → 客户端方向的字段结构手工转换
//
// 用于 CopyProtoMsgSameField 处理不了的情况：字段名变了 / 字段类型变了 / 嵌套结构变了
// 例如 SceneEntityAppearNotify 中 trifle_item 在新版本变成 TrifleGadgetInfo 包装结构
// 需要手工把服务端的 trifle_item.item_id 写到客户端的 trifle_item.gadget_info.item_id
func ConvHighVersionProtoDataServerToClient(clientProtoObj *dynamic.Message, serverProtoObj pb.Message, clientProtoProxy *ClientProtoProxy) {
	if !config.GetConfig().Hk4e.HighVersionProtoConvEnable {
		return
	}
	cmdName := string(serverProtoObj.ProtoReflect().Descriptor().FullName())
	if strings.Contains(cmdName, "proto.") {
		cmdName = strings.Split(cmdName, ".")[1]
	}
	switch cmdName {
	case "SceneEntityAppearNotify":
		ntf := serverProtoObj.(*proto.SceneEntityAppearNotify)
		for index, sceneEntityInfo := range ntf.EntityList {
			gadget, ok := sceneEntityInfo.Entity.(*proto.SceneEntityInfo_Gadget)
			if !ok {
				continue
			}
			trifleItem, ok := gadget.Gadget.Content.(*proto.SceneGadgetInfo_TrifleItem)
			if !ok {
				continue
			}
			item := trifleItem.TrifleItem
			clientItem := clientProtoProxy.GetClientProtoObjByName("Item")
			if clientItem == nil {
				continue
			}
			err := object.CopyProtoMsgSameField(clientItem, item)
			if err != nil {
				continue
			}
			clientSceneEntityInfoAny, err := clientProtoObj.TryGetRepeatedFieldByName("entity_list", index)
			if err != nil {
				continue
			}
			clientSceneEntityInfo := clientSceneEntityInfoAny.(*dynamic.Message)
			msgDesc := clientSceneEntityInfo.GetMessageDescriptor()
			var ood *desc.OneOfDescriptor
			for _, o := range msgDesc.GetOneOfs() {
				if o.GetName() == "entity" {
					ood = o
					break
				}
			}
			if ood == nil {
				continue
			}
			_, clientSceneGadgetInfoAny := clientSceneEntityInfo.GetOneOfField(ood)
			clientSceneGadgetInfo := clientSceneGadgetInfoAny.(*dynamic.Message)
			clientTrifleGadgetInfo := clientProtoProxy.GetClientProtoObjByName("TrifleGadgetInfo")
			if clientTrifleGadgetInfo == nil {
				continue
			}
			_ = clientTrifleGadgetInfo.TrySetFieldByName("item", clientItem)
			_ = clientSceneGadgetInfo.TrySetFieldByName("trifle_gadget", clientTrifleGadgetInfo)
		}
	case "ChangeGameTimeRsp":
		rsp := serverProtoObj.(*proto.ChangeGameTimeRsp)
		_ = clientProtoObj.TrySetFieldByName("game_time", rsp.CurGameTime)
		_ = clientProtoObj.TrySetFieldByName("client_game_time", rsp.ExtraDays)
	}
}

// ConvHighVersionProtoDataClientToServer 客户端 → 服务端方向的字段结构手工转换
//
// 已知 case：
//   - ChangeGameTimeReq: cur_game_time 字段名变化
//   - AvatarUpgradeReq: item_param_list 数组变成嵌套结构
//   - WeaponAwakenReq: item_guid_list 改结构
//
// 这是 gate 协议代理需要扩展最频繁的地方之一 新版本如果改了某 cmd 的字段结构
// 必须在这里加 case 把客户端字段映射到服务端 3.2 版本字段
func ConvHighVersionProtoDataClientToServer(serverProtoObj pb.Message, clientProtoObj *dynamic.Message, clientProtoProxy *ClientProtoProxy) {
	if !config.GetConfig().Hk4e.HighVersionProtoConvEnable {
		return
	}
	cmdName := string(serverProtoObj.ProtoReflect().Descriptor().FullName())
	if strings.Contains(cmdName, "proto.") {
		cmdName = strings.Split(cmdName, ".")[1]
	}
	switch cmdName {
	case "ChangeGameTimeReq":
		req := serverProtoObj.(*proto.ChangeGameTimeReq)
		gameTimeAny, err := clientProtoObj.TryGetFieldByName("game_time")
		if err == nil {
			req.GameTime = gameTimeAny.(uint32)
		}
		extraDaysAny, err := clientProtoObj.TryGetFieldByName("client_game_time")
		if err == nil {
			req.ExtraDays = extraDaysAny.(uint32)
		}
		isForceSetAny, err := clientProtoObj.TryGetFieldByName("is_force_set")
		if err == nil {
			req.IsForceSet = isForceSetAny.(bool)
		}
	case "AvatarUpgradeReq":
		req := serverProtoObj.(*proto.AvatarUpgradeReq)
		itemParamListAny, err := clientProtoObj.TryGetFieldByName("item_param_list")
		if err != nil {
			return
		}
		itemParamList := itemParamListAny.([]any)
		for _, itemParamAny := range itemParamList {
			itemParam := itemParamAny.(*dynamic.Message)
			itemId, err := itemParam.TryGetFieldByName("item_id")
			if err != nil {
				continue
			}
			count, err := itemParam.TryGetFieldByName("count")
			if err != nil {
				continue
			}
			req.ItemId = itemId.(uint32)
			req.Count = count.(uint32)
			break
		}
	case "WeaponAwakenReq":
		req := serverProtoObj.(*proto.WeaponAwakenReq)
		itemGuidListAny, err := clientProtoObj.TryGetFieldByName("item_guid_list")
		if err != nil {
			return
		}
		itemGuidList := itemGuidListAny.([]any)
		for _, itemGuidAny := range itemGuidList {
			req.ItemGuid = itemGuidAny.(uint64)
			break
		}
	}
}
