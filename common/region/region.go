package region

import (
	"os"
	"regexp"
	"strconv"

	"github.com/flswld/halo/logger"
)

// LoadRegionRsaKey 加载 7 个 RSA 密钥文件（dispatch / gate 启动时调用）
//
// 三类密钥（位于 key/ 目录）：
//   - region_sign_key.pem: 区服签名私钥（v2.7.5+ 客户端要求 region 响应必须签名）
//   - region_enc_key_1~5.pem: 5 套加密私钥（按 KeyId 选择）
//     · 客户端用对应公钥加密 region 响应内容
//     · gate 也用同一密钥做 KCP 握手时的密钥协商
//   - account_password_key.pem: 账号密码 RSA 私钥（apiLogin 解密客户端密文密码）
//
// **重要**：5 套加密密钥让客户端可指定使用哪套
//
//	不同版本/不同部署的客户端 PublicKey 可能不同 5 套兼容多版本
//	实际生产部署 grasscutter / 各种 hook 工具用的密钥都在这 5 套里
//
// 任意密钥加载失败返回 nil 上层应直接 panic（启动期错误必须暴露）
func LoadRegionRsaKey() (signRsaKey []byte, encRsaKeyMap map[string][]byte, pwdRsaKey []byte) {
	var err error = nil
	encRsaKeyMap = make(map[string][]byte)
	signRsaKey, err = os.ReadFile("key/region_sign_key.pem")
	if err != nil {
		logger.Error("open region_sign_key.pem error: %v", err)
		return nil, nil, nil
	}
	encKeyIdList := []string{"1", "2", "3", "4", "5"}
	for _, v := range encKeyIdList {
		encRsaKeyMap[v], err = os.ReadFile("key/region_enc_key_" + v + ".pem")
		if err != nil {
			logger.Error("open region_enc_key_"+v+".pem error: %v", err)
			return nil, nil, nil
		}
	}
	pwdRsaKey, err = os.ReadFile("key/account_password_key.pem")
	if err != nil {
		logger.Error("open account_password_key.pem error: %v", err)
		return nil, nil, nil
	}
	return signRsaKey, encRsaKeyMap, pwdRsaKey
}

// GetClientVersionByName 从客户端版本字符串提取数字版本号
//
// 输入格式（米哈游官方约定）：
//   - OSRELWin3.2.0_R11611027  → 320（海外 Windows 3.2.0 版本）
//   - OSRELWin3.2.50_xxx       → 325（3.2.50 末位 50 视为测试版本 /10）
//   - CNRELWin3.2.0_xxx        → 320（国内版同样规则）
//
// 算法：用正则提取数字部分（如 ["3", "2", "0"]）
//   - 主版本 ×100 + 中版本 ×10 + 子版本 = 320
//   - 子版本 ≥10 视为测试版本（除 10）
//
// 返回值：版本数字 + 字符串形式（用于配置匹配）
func GetClientVersionByName(versionName string) (int, string) {
	reg, err := regexp.Compile("[0-9]+")
	if err != nil {
		logger.Error("compile regexp error: %v", err)
		return 0, ""
	}
	versionSlice := reg.FindAllString(versionName, -1)
	version := 0
	for index, value := range versionSlice {
		v, err := strconv.Atoi(value)
		if err != nil {
			logger.Error("parse client version error: %v", err)
			return 0, ""
		}
		if v >= 10 {
			// 测试版本
			if index != 2 {
				logger.Error("invalid client version")
				return 0, ""
			}
			v /= 10
		}
		for i := 0; i < 2-index; i++ {
			v *= 10
		}
		version += v
	}
	return version, strconv.Itoa(version)
}
