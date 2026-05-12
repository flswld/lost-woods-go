package login

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"strconv"

	"hk4e/common/config"
	"hk4e/common/region"
	"hk4e/dispatch/api"
	"hk4e/pkg/endec"
	"hk4e/pkg/httpclient"
	"hk4e/pkg/random"
	"hk4e/protocol/proto"

	"github.com/flswld/halo/logger"
	pb "google.golang.org/protobuf/proto"
)

// Robot 模拟客户端的 dispatch / SDK 登录 HTTP 流程
//
// 完整还原原神客户端的网络层登录链路 用于测试 dispatch 实现的正确性
//
// 关键解密步骤：
//   - 一级 dispatch: 返回 base64 编码的 PB（QueryRegionListHttpRsp）直接解码
//   - 二级 dispatch: 返回 JSON 包含加密的 Content 字段
//     · Content 是 RSA 加密的（用 region_enc_key_N.pem 私钥）
//     · 256 字节一段（RSA 单次最大密文长度）需要分段解密
//     · 解密后是 PB（QueryCurrRegionHttpRsp）含 GateIp/Port/SecretKey
//
// DispatchKey: 从 region 响应中拿到 用于客户端 KCP 通信的 XOR 密钥派生

// DispatchInfo 二级 dispatch 解密后的关键信息
type DispatchInfo struct {
	GateIp      string // Gate KCP 监听地址
	GatePort    uint32 // Gate KCP 监听端口
	DispatchKey []byte // ec2b 派生的 XOR 密钥
}

// GetDispatchInfo 模拟一/二级 dispatch 全流程获取 Gate 地址
//
// 处理流程：
//  1. GET regionListUrl → base64 → PB QueryRegionListHttpRsp（区服列表）
//  2. 按 SelectRegionIndex 选一个区服 + 对应 dispatchUrl
//  3. GET curRegionUrl → JSON → Content 字段是 RSA 加密二进制
//  4. 用 keyId 选对应私钥 RSA 分段解密（256B 一段）
//  5. 解密后是 PB QueryCurrRegionHttpRsp 含 GateIp/Port + SecretKey（ec2b）
//
// keyId 来自 robot 配置 必须与 region_enc_key_N.pem 文件名匹配（N=1~5）
func GetDispatchInfo(regionListUrl string, regionListParam string, curRegionUrl string, curRegionParam string, keyId string) (*DispatchInfo, error) {
	logger.Info("http get url: %v", regionListUrl+regionListParam)
	regionListBase64, err := httpclient.GetRaw(regionListUrl + regionListParam)
	if err != nil {
		return nil, err
	}
	regionListData, err := base64.StdEncoding.DecodeString(regionListBase64)
	if err != nil {
		return nil, err
	}
	queryRegionListHttpRsp := new(proto.QueryRegionListHttpRsp)
	err = pb.Unmarshal(regionListData, queryRegionListHttpRsp)
	if err != nil {
		return nil, err
	}
	logger.Info("region list: %v", queryRegionListHttpRsp.RegionList)
	if len(queryRegionListHttpRsp.RegionList) == 0 {
		return nil, errors.New("no region found")
	}
	if curRegionUrl == "" {
		selectRegion := queryRegionListHttpRsp.RegionList[int(config.GetConfig().Hk4eRobot.SelectRegionIndex)]
		logger.Info("select region: %v", selectRegion)
		curRegionUrl = selectRegion.DispatchUrl
	}
	logger.Info("http get url: %v", curRegionUrl+curRegionParam)
	regionCurrJson, err := httpclient.GetRaw(curRegionUrl + curRegionParam)
	if err != nil {
		return nil, err
	}
	queryCurRegionRspJson := new(api.QueryCurRegionRspJson)
	err = json.Unmarshal([]byte(regionCurrJson), queryCurRegionRspJson)
	if err != nil {
		return nil, err
	}
	encryptedRegionInfo, err := base64.StdEncoding.DecodeString(queryCurRegionRspJson.Content)
	if err != nil {
		return nil, err
	}
	chunkSize := 256
	regionInfoLength := len(encryptedRegionInfo)
	numChunks := int(math.Ceil(float64(regionInfoLength) / float64(chunkSize)))
	regionCurrData := make([]byte, 0)
	_, encRsaKeyMap, _ := region.LoadRegionRsaKey()
	encPubPrivKey, exist := encRsaKeyMap[keyId]
	if !exist {
		logger.Error("can not found key id: %v", keyId)
		return nil, err
	}
	for i := 0; i < numChunks; i++ {
		from := i * chunkSize
		to := int(math.Min(float64((i+1)*chunkSize), float64(regionInfoLength)))
		chunk := encryptedRegionInfo[from:to]
		privKey, err := endec.RsaParsePrivKey(encPubPrivKey)
		if err != nil {
			logger.Error("parse rsa priv key error: %v", err)
			return nil, err
		}
		decrypt, err := endec.RsaDecrypt(chunk, privKey)
		if err != nil {
			logger.Error("rsa dec error: %v", err)
			return nil, err
		}
		regionCurrData = append(regionCurrData, decrypt...)
	}
	queryCurrRegionHttpRsp := new(proto.QueryCurrRegionHttpRsp)
	err = pb.Unmarshal(regionCurrData, queryCurrRegionHttpRsp)
	if err != nil {
		return nil, err
	}
	regionInfo := queryCurrRegionHttpRsp.RegionInfo
	if regionInfo == nil {
		return nil, errors.New("region info is nil")
	}
	ec2b, err := random.LoadEc2bKey(queryCurrRegionHttpRsp.ClientSecretKey)
	if err != nil {
		return nil, err
	}
	dispatchInfo := &DispatchInfo{
		GateIp:      regionInfo.GateserverIp,
		GatePort:    regionInfo.GateserverPort,
		DispatchKey: ec2b.XorKey(),
	}
	return dispatchInfo, nil
}

// AccountInfo SDK 登录拿到的账号信息（gateLogin 需要）
//   - AccountId: 玩家 OpenId 数字形式
//   - Token: apiLogin 拿到的 Token（7 天有效）
//   - ComboToken: v2Login 拿到的 ComboToken（24 小时有效 KCP 握手用）
type AccountInfo struct {
	AccountId  uint32
	Token      string
	ComboToken string
}

// AccountLogin SDK 登录拿 ComboToken 的两步流程
//
// 步骤：
//  1. POST /hk4e_global/mdk/shield/api/login → 用账号密码登录拿 Token
//  2. POST /hk4e_global/combo/granter/login/v2/login → Token 换 ComboToken
//
// 注意：第二步的 Data 字段是 LoginTokenData JSON 字符串（嵌套 JSON）
//
//	服务端二次解析才能拿到 Token
//
// IsCrypto=true 表示密码已 RSA 加密（但 robot 没真加密 dispatch 解密失败会 fallback 到 @@ mode）
func AccountLogin(loginSdkUrl string, account string, password string) (*AccountInfo, error) {
	loginAccountRequestJson := &api.LoginAccountRequestJson{
		Account:  account,
		Password: password,
		IsCrypto: true,
	}
	logger.Info("http post url: %v", loginSdkUrl+"/hk4e_global/mdk/shield/api/login")
	loginResult, err := httpclient.PostJson[api.LoginResult](loginSdkUrl+"/hk4e_global/mdk/shield/api/login", loginAccountRequestJson)
	if err != nil {
		return nil, err
	}
	if loginResult.Retcode != 0 {
		logger.Error("login error msg: %v", loginResult.Message)
		return nil, errors.New("login error")
	}
	accountId, err := strconv.Atoi(loginResult.Data.Account.Uid)
	if err != nil {
		return nil, err
	}
	loginTokenData := &api.LoginTokenData{
		Uid:   loginResult.Data.Account.Uid,
		Token: loginResult.Data.Account.Token,
	}
	loginTokenDataJson, err := json.Marshal(loginTokenData)
	if err != nil {
		return nil, err
	}
	comboTokenReq := &api.ComboTokenReq{
		AppID:     4,
		ChannelID: 1,
		Data:      string(loginTokenDataJson),
	}
	logger.Info("http post url: %v", loginSdkUrl+"/hk4e_global/combo/granter/login/v2/login")
	comboTokenRsp, err := httpclient.PostJson[api.ComboTokenRsp](loginSdkUrl+"/hk4e_global/combo/granter/login/v2/login", comboTokenReq)
	if err != nil {
		return nil, err
	}
	if comboTokenRsp.Retcode != 0 {
		logger.Error("v2 login error msg: %v", comboTokenRsp.Message)
		return nil, errors.New("v2 login error")
	}
	accountInfo := &AccountInfo{
		AccountId:  uint32(accountId),
		Token:      loginResult.Data.Account.Token,
		ComboToken: comboTokenRsp.Data.ComboToken,
	}
	return accountInfo, nil
}
