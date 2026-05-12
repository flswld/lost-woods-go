// 加解密工具包 - RSA / XOR / 哈希（含 AES 工具函数但业务未实际使用）
//
// **整个 hk4e 协议没有 AES 加密**：对称加解密链路全是 XOR
//   - 密钥协商: 双方 64-bit seed RSA 互发 → XOR 合并 → MT19937 派生 4096 字节流
//   - KCP 报文: 用上面的 4096 字节流循环 XOR 加密
//   - region 配置: 用 ec2b 派生的 XOR 密钥加密
//   - 字段级反作弊: ClientProtoProxy 按 .proto option 对个别 int 字段做 XOR 编码
//
// 项目使用场景：
//   - RSA: dispatch 加密 region 响应 / KCP 握手 seed 交换 / 账号密码加密
//     · PKCS1v15 padding（与原神客户端兼容 不能改 PSS）
//     · 256 字节分块加密（RSA-2048 单次最大密文长度）
//   - XOR: KCP 报文 + region 配置 + 字段级 XOR（详见 gate/net/proto_endecode.go）
//   - MD5: 账号密码哈希（无 salt 项目作者重视便利性）
//   - SHA1: 客户端版本签名（与 "mhy2020" 盐值拼接）
//   - SHA256: HMAC 签名（gate ↔ dispatch 防伪造）
//
// **AesCFBEncrypt/Decrypt + AesCBCEncrypt/Decrypt 仅是 helper 函数 业务代码未使用**
//   仅 endec_test.go 自测调用 保留备用
//
// 关键 hash 算法：
//   - Hk4eAbilityHashCode: 原神 Ability 名字的特殊 hash（用于 PB 字段索引）
//     · 与官服内部一致 不能改算法
//   - Md5Str / Sha1Str / Sha256Str: 标准哈希 + hex 输出

package endec

import (
	"bytes"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"hash"
)

func RsaParsePubKey(pubKeyPem []byte) (*rsa.PublicKey, error) {
	block, _ := pem.Decode(pubKeyPem)
	if block == nil {
		return nil, errors.New("invalid rsa public key")
	}
	pubInfo, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	pubKey := pubInfo.(*rsa.PublicKey)
	return pubKey, nil
}

func RsaParsePubKeyByPrivKey(privKeyPem []byte) (*rsa.PublicKey, error) {
	block, _ := pem.Decode(privKeyPem)
	if block == nil {
		return nil, errors.New("invalid rsa private key")
	}
	privKey, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	return &privKey.PublicKey, nil
}

func RsaParsePrivKey(privKeyPem []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(privKeyPem)
	if block == nil {
		return nil, errors.New("invalid rsa private key")
	}
	privKey, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	return privKey, nil
}

func RsaEncrypt(rawData []byte, pubKey *rsa.PublicKey) (encData []byte, err error) {
	return rsa.EncryptPKCS1v15(rand.Reader, pubKey, rawData)
}

func RsaDecrypt(encData []byte, privKey *rsa.PrivateKey) (decData []byte, err error) {
	return rsa.DecryptPKCS1v15(rand.Reader, privKey, encData)
}

func RsaSign(rawData []byte, privKey *rsa.PrivateKey) (signData []byte, err error) {
	msgHash := sha256.New()
	_, err = msgHash.Write(rawData)
	if err != nil {
		return nil, err
	}
	msgHashSum := msgHash.Sum(nil)
	signData, err = rsa.SignPKCS1v15(rand.Reader, privKey, crypto.SHA256, msgHashSum)
	if err != nil {
		return nil, err
	}
	return signData, nil
}

func RsaVerify(rawData []byte, signData []byte, pubKey *rsa.PublicKey) (ok bool, err error) {
	msgHash := sha256.New()
	_, err = msgHash.Write(rawData)
	if err != nil {
		return false, err
	}
	msgHashSum := msgHash.Sum(nil)
	err = rsa.VerifyPKCS1v15(pubKey, crypto.SHA256, msgHashSum, signData)
	if err != nil {
		return false, err
	}
	return true, nil
}

func AesCFBEncrypt(rawData []byte, key []byte, iv []byte) (encData []byte, err error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	encData = make([]byte, len(rawData))
	if iv == nil {
		iv = make([]byte, aes.BlockSize)
	}
	stream := cipher.NewCFBEncrypter(block, iv)
	stream.XORKeyStream(encData, rawData)
	return encData, nil
}

func AesCFBDecrypt(encData []byte, key []byte, iv []byte) (decData []byte, err error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if iv == nil {
		iv = make([]byte, aes.BlockSize)
	}
	stream := cipher.NewCFBDecrypter(block, iv)
	decData = make([]byte, len(encData))
	stream.XORKeyStream(decData, encData)
	return decData, nil
}

func AesCBCEncrypt(rawData []byte, key []byte, iv []byte) (encData []byte, err error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	paddingChar := block.BlockSize() - len(rawData)%block.BlockSize()
	paddingData := bytes.Repeat([]byte{byte(paddingChar)}, paddingChar)
	rawData = append(rawData, paddingData...)
	encData = make([]byte, len(rawData))
	if iv == nil {
		iv = make([]byte, aes.BlockSize)
	}
	blockMode := cipher.NewCBCEncrypter(block, iv)
	blockMode.CryptBlocks(encData, rawData)
	return encData, nil
}

func AesCBCDecrypt(encData []byte, key []byte, iv []byte) (decData []byte, err error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if iv == nil {
		iv = make([]byte, aes.BlockSize)
	}
	blockMode := cipher.NewCBCDecrypter(block, iv)
	decData = make([]byte, len(encData))
	blockMode.CryptBlocks(decData, encData)
	paddingChar := int(decData[len(decData)-1])
	decData = decData[:len(decData)-paddingChar]
	return decData, nil
}

func Sha1Str(inputStr string) string {
	h := sha1.New()
	return hashStr(h, inputStr)
}

func Sha256Str(inputStr string) string {
	h := sha256.New()
	return hashStr(h, inputStr)
}

func Md5Str(inputStr string) string {
	h := md5.New()
	return hashStr(h, inputStr)
}

func hashStr(h hash.Hash, inputStr string) string {
	h.Write([]byte(inputStr))
	return hex.EncodeToString(h.Sum(nil))
}

func Xor(data []byte, key []byte) {
	for i := 0; i < len(data); i++ {
		data[i] ^= key[i%len(key)]
	}
}

func Hk4eAbilityHashCode(ability string) int32 {
	hashCode := int32(0)
	for i := 0; i < len(ability); i++ {
		hashCode = int32(ability[i]) + 131*hashCode
	}
	return hashCode
}
