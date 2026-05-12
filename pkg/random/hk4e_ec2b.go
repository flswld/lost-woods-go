package random

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
)

// Ec2b 原神客户端的密钥派生算法 - 用于 KCP 通信加密
//
// **没有 AES 加密**：算法虽然借用了 AES 的 S-box / shift rows / mix cols 等组件
//
//	但仅作为"密钥置乱"操作 不是标准 AES 块加密
//	整个派生链路最终走 XOR + MT19937
//
// Ec2b 文件格式（米哈游约定 不能改）：
//   - 前 4 字节: 魔数 "Ec2b"
//   - 后 4 字节: key 长度字段（固定 16）
//   - 16 字节: 主密钥 key
//   - 4 字节: data 长度字段（固定 2048）
//   - 2048 字节: 辅助数据块 data
//
// 派生流程（init 函数）：
//  1. keyScramble(key): 用 11 轮"反向 AES 操作 + XOR 魔数表"置乱 key
//     · 借用 subBytesInv / shiftRowsInv / mixColsInv + aesXorTable + keyXorTable
//     · 输出仍是 16 字节但与原 key 完全不同
//  2. getSeed(scrambledKey, data): 把置乱 key 和 data 全部按 8 字节对齐 XOR 折叠
//     · 起始值 ^0xCEAC3B5A867837AC（魔数）
//     · 累加异或 16 字节 key + 2048 字节 data 共 ~258 个 uint64
//     · 输出 64-bit seed
//  3. SetSeed(seed): 用 seed 种子 MT19937-64 生成 4096 字节"伪随机字节流"
//     · 这就是最终的 XOR 密钥（temp 字段）
//
// 用途：
//   - dispatch 通过 region 响应下发 ec2b 给客户端（base64 编码）
//   - 客户端和服务端各自独立用同样算法派生出相同的 XOR 密钥流
//   - 后续 KCP 通信用此 XOR 密钥流异或加密报文
//
// 安全性：seed 是 64 bit 暴力枚举不现实 但 ec2b 文件本身需要保密
//
//	（拿到 ec2b 就能解密所有 KCP 通信）
type Ec2b struct {
	key  []byte // 主密钥（16 字节 keyScramble 的输入）
	data []byte // 辅助数据块（2048 字节 与 scrambledKey 一起 XOR 折叠出 seed）
	seed uint64 // XOR 折叠出的 64-bit seed（mt19937 种子）
	temp []byte // 4096 字节伪随机字节流（KCP payload 实际异或加密用）
}

func LoadEc2bKey(b []byte) (*Ec2b, error) {
	if len(b) < 4+4+16+4+2048 {
		return nil, fmt.Errorf("invalid ec2b key")
	}
	if string(b[0:4]) != "Ec2b" {
		return nil, fmt.Errorf("invalid ec2b key")
	}
	keyLen := binary.LittleEndian.Uint32(b[4:])
	if keyLen != 16 {
		return nil, fmt.Errorf("invalid ec2b key")
	}
	dataLen := binary.LittleEndian.Uint32(b[24:])
	if dataLen != 2048 {
		return nil, fmt.Errorf("invalid ec2b key")
	}
	e := &Ec2b{
		key:  b[8:24],
		data: b[28 : 28+2048],
	}
	e.init()
	return e, nil
}

func NewEc2b() *Ec2b {
	e := &Ec2b{
		key:  make([]byte, 16),
		data: make([]byte, 2048),
	}
	rand.Read(e.key)
	rand.Read(e.data)
	e.init()
	return e
}

func (e *Ec2b) init() {
	k := make([]byte, 16)
	copy(k[:], e.key)
	keyScramble(k)
	e.SetSeed(getSeed(k, e.data))
}

func (e *Ec2b) Bytes() []byte {
	b := make([]byte, 4+4+16+4+2048)
	copy(b[0:4], []byte("Ec2b"))
	binary.LittleEndian.PutUint32(b[4:], 16)
	copy(b[8:], e.key)
	binary.LittleEndian.PutUint32(b[24:], 2048)
	copy(b[28:], e.data)
	return b
}

func (e *Ec2b) SetSeed(seed uint64) {
	e.seed = seed
	r := NewRand64()
	r.Seed(int64(e.seed))
	e.temp = make([]byte, 4096)
	for i := 0; i < 4096>>3; i++ {
		binary.LittleEndian.PutUint64(e.temp[i<<3:], r.Uint64())
	}
}

func (e *Ec2b) Seed() uint64 {
	return e.seed
}

func (e *Ec2b) Key() []byte {
	b := make([]byte, 4+4+16+4+2048)
	copy(b[0:4], []byte("Ec2b"))
	binary.LittleEndian.PutUint32(b[4:], 16)
	copy(b[8:], e.key)
	binary.LittleEndian.PutUint32(b[24:], 2048)
	copy(b[28:], e.data)
	return b
}

func (e *Ec2b) XorKey() []byte {
	return e.temp
}

func keyScramble(key []byte) {
	var roundKeys [11][16]byte
	for r := 0; r < 11; r++ {
		for i := 0; i < 16; i++ {
			for j := 0; j < 16; j++ {
				idx := (r << 8) + (i << 4) + j
				roundKeys[r][i] ^= aesXorTable[1][idx] ^ aesXorTable[0][idx]
			}
		}
	}
	xorRoundKey(key, roundKeys[0][:])
	for r := 1; r < 10; r++ {
		subBytesInv(key)
		shiftRowsInv(key)
		mixColsInv(key)
		xorRoundKey(key, roundKeys[r][:])
	}
	subBytesInv(key)
	shiftRowsInv(key)
	xorRoundKey(key, roundKeys[10][:])
	for i := 0; i < 16; i++ {
		key[i] ^= keyXorTable[i]
	}
}

func xorRoundKey(key, roundKey []byte) {
	for i := 0; i < 16; i++ {
		key[i] ^= roundKey[i]
	}
}

func subBytes(key []byte) {
	for i := 0; i < 16; i++ {
		key[i] = lookupSbox[key[i]]
	}
}

func subBytesInv(key []byte) {
	for i := 0; i < 16; i++ {
		key[i] = lookupSboxInv[key[i]]
	}
}

func shiftRows(key []byte) {
	var temp [16]byte
	copy(temp[:], key[:])
	for i := 0; i < 16; i++ {
		key[i] = temp[shiftRowsTable[i]]
	}
}

func shiftRowsInv(key []byte) {
	var temp [16]byte
	copy(temp[:], key[:])
	for i := 0; i < 16; i++ {
		key[i] = temp[shiftRowsTableInv[i]]
	}
}

func mixColInv(key []byte) {
	a0, a1, a2, a3 := key[0], key[1], key[2], key[3]
	key[0] = lookupG14[a0] ^ lookupG9[a3] ^ lookupG13[a2] ^ lookupG11[a1]
	key[1] = lookupG14[a1] ^ lookupG9[a0] ^ lookupG13[a3] ^ lookupG11[a2]
	key[2] = lookupG14[a2] ^ lookupG9[a1] ^ lookupG13[a0] ^ lookupG11[a3]
	key[3] = lookupG14[a3] ^ lookupG9[a2] ^ lookupG13[a1] ^ lookupG11[a0]
}

func mixColsInv(key []byte) {
	mixColInv(key[0:])
	mixColInv(key[4:])
	mixColInv(key[8:])
	mixColInv(key[12:])
}

func getSeed(key, data []byte) uint64 {
	v := ^uint64(0xCEAC3B5A867837AC)
	v ^= binary.LittleEndian.Uint64(key[0:])
	v ^= binary.LittleEndian.Uint64(key[8:])
	for i := 0; i < len(data)>>3; i++ {
		v ^= binary.LittleEndian.Uint64(data[i<<3:])
	}
	return v
}
