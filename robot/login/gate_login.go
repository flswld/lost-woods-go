package login

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"

	"hk4e/common/region"
	"hk4e/pkg/endec"
	"hk4e/pkg/random"
	"hk4e/protocol/cmd"
	"hk4e/protocol/proto"
	"hk4e/robot/net"

	"github.com/flswld/halo/logger"
)

// GateLogin Robot 模拟客户端的 KCP 握手 + Gate 层登录
//
// 完整流程（**全程对称加解密走 XOR 没有 AES**）：
//  1. 创建 KCP 会话连到 Gate（dispatchKey 用于初次握手前的 XOR 加密）
//  2. 生成 ClientRandKey（8 字节客户端 seed）用 region_sign_key 公钥 RSA 加密
//  3. 构造 GetPlayerTokenReq 发给 Gate（含 ComboToken + KeyId + ClientRandKey）
//  4. 收到 GetPlayerTokenRsp 完成密钥协商：
//     · ServerRandKey 用 region_enc_key_N.pem 私钥 RSA 解密拿到 ServerSeed
//     · ServerSeed 内部已经是 serverSeed XOR clientSeed 的合并 seed
//     · 把这个合并 seed 喂给 MT19937 派生 4096 字节伪随机字节流（详见 pkg/random/hk4e_ec2b.go）
//     · 后续所有 KCP 报文都用这 4096 字节循环 XOR 加密
//  5. session.Uid 写入 准备 PlayerLoginReq
//
// region_sign_key_pub.pem 存在时优先用（公钥模式 用于 verify-only 部署）
// 否则从 region_sign_key.pem 私钥推导出公钥
func GateLogin(dispatchInfo *DispatchInfo, accountInfo *AccountInfo, keyId string) (*net.Session, error) {
	gateAddr := dispatchInfo.GateIp + ":" + strconv.Itoa(int(dispatchInfo.GatePort))
	logger.Debug("connect gate addr: %v", gateAddr)
	session, err := net.NewSession(gateAddr, dispatchInfo.DispatchKey)
	if err != nil {
		return nil, err
	}
	logger.Debug("connect gate ok")
	timeRand := random.GetTimeRand()
	clientSeedUint64 := timeRand.Uint64()
	clientSeedBuf := new(bytes.Buffer)
	err = binary.Write(clientSeedBuf, binary.BigEndian, &clientSeedUint64)
	if err != nil {
		return nil, err
	}
	clientSeed := clientSeedBuf.Bytes()
	logger.Debug("clientSeed: %v, clientSeedUint64: %v", clientSeed, clientSeedUint64)
	signRsaKey, encRsaKeyMap, _ := region.LoadRegionRsaKey()
	if signRsaKey == nil || encRsaKeyMap == nil {
		return nil, errors.New("load key error")
	}
	signPubkey, err := endec.RsaParsePubKeyByPrivKey(signRsaKey)
	if err != nil {
		logger.Error("parse rsa pub key error: %v", err)
		return nil, err
	}
	signRsaKey, err = os.ReadFile("key/region_sign_key_pub.pem")
	if err == nil {
		logger.Debug("use pub key")
		signPubkey, err = endec.RsaParsePubKey(signRsaKey)
		if err != nil {
			logger.Error("parse rsa pub key error: %v", err)
			return nil, err
		}
	}
	clientSeedEnc, err := endec.RsaEncrypt(clientSeed, signPubkey)
	if err != nil {
		logger.Error("rsa enc error: %v", err)
		return nil, err
	}
	clientSeedBase64 := base64.StdEncoding.EncodeToString(clientSeedEnc)
	keyIdInt, err := strconv.Atoi(keyId)
	if err != nil {
		logger.Error("parse key id error: %v", err)
		return nil, err
	}
	getPlayerTokenReq := &proto.GetPlayerTokenReq{
		AccountToken:  accountInfo.ComboToken,
		AccountUid:    strconv.Itoa(int(accountInfo.AccountId)),
		KeyId:         uint32(keyIdInt),
		ClientRandKey: clientSeedBase64,
		AccountType:   1,
		ChannelId:     1,
		SubChannelId:  1,
		PlatformType:  3,
	}
	session.SendMsg(cmd.GetPlayerTokenReq, getPlayerTokenReq)
	getPlayerTokenReqJson, _ := json.Marshal(getPlayerTokenReq)
	logger.Debug("GetPlayerTokenReq: %v", string(getPlayerTokenReqJson))
	protoMsg := <-session.RecvChan
	if protoMsg.CmdId != cmd.GetPlayerTokenRsp {
		return nil, errors.New("recv pkt is not GetPlayerTokenRsp")
	}
	getPlayerTokenRsp := protoMsg.PayloadMessage.(*proto.GetPlayerTokenRsp)
	getPlayerTokenRspJson, _ := json.Marshal(getPlayerTokenRsp)
	logger.Debug("GetPlayerTokenRsp: %v", string(getPlayerTokenRspJson))
	if getPlayerTokenRsp.Retcode != 0 {
		return nil, errors.New(fmt.Sprintf("gate login error, retCode: %v", getPlayerTokenRsp.Retcode))
	}
	logger.Info("gate login ok, uid: %v", getPlayerTokenRsp.Uid)
	session.Uid = getPlayerTokenRsp.Uid
	// XOR密钥切换
	seedEnc, err := base64.StdEncoding.DecodeString(getPlayerTokenRsp.ServerRandKey)
	if err != nil {
		logger.Error("base64 decode error: %v", err)
		return nil, err
	}
	encPubPrivKey, exist := encRsaKeyMap[keyId]
	if !exist {
		logger.Error("can not found key id: %v", keyId)
		return nil, err
	}
	privKey, err := endec.RsaParsePrivKey(encPubPrivKey)
	if err != nil {
		logger.Error("parse rsa pub key error: %v", err)
		return nil, err
	}
	seed, err := endec.RsaDecrypt(seedEnc, privKey)
	if err != nil {
		logger.Error("rsa dec error: %v", err)
		return nil, err
	}
	seedUint64 := uint64(0)
	err = binary.Read(bytes.NewReader(seed), binary.BigEndian, &seedUint64)
	if err != nil {
		logger.Error("parse seed error: %v", err)
		return nil, err
	}
	serverSeedUint64 := seedUint64 ^ clientSeedUint64
	logger.Debug("seed: %v, seedUint64: %v", seed, seedUint64)
	logger.Debug("serverSeedUint64: %v", serverSeedUint64)
	logger.Info("change session xor key")
	keyBlock := random.NewKeyBlock(serverSeedUint64, true)
	xorKey := keyBlock.XorKey()
	key := make([]byte, 4096)
	copy(key, xorKey[:])
	session.XorKey = key
	session.ClientVersionRandomKey = getPlayerTokenRsp.ClientVersionRandomKey
	session.SecurityCmdBuffer = getPlayerTokenRsp.SecurityCmdBuffer
	return session, nil
}
