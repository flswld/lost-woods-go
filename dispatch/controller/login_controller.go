package controller

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"hk4e/common/config"
	"hk4e/dispatch/api"
	"hk4e/dispatch/model"
	"hk4e/pkg/endec"
	"hk4e/pkg/random"

	"github.com/flswld/halo/logger"
	"github.com/gin-gonic/gin"
)

// SDK 登录接口实现 - 三段式登录
//
// 1) apiLogin: 账号密码登录拿 Token（首次登录或新设备）
// 2) apiVerify: Token 续期验证（已登录玩家恢复会话）
// 3) v2Login: 用 Token 换 ComboToken（实际游戏内会话凭证）
//
// 加密设计：
//   - 客户端用 region 下发的 PwdPubKey RSA 加密密码 dispatch 用 PwdPrivKey 解密
//   - **特殊兼容**：RSA 解密失败时 fallback 到 "@@ mode"
//     · 客户端使用其他工具修改 PublicKey 后 dispatch 解密会失败
//     · @@ mode 让玩家把 "用户名@@密码" 都填到用户名输入框 密码框任意填
//     · 这是项目作者照顾各种 hook 工具的兼容方案
//
// 账号自动注册：第一次登录的用户名直接注册（用 MD5 存密码 不加盐）
//   StandaloneMode 用本地自增 id（c.nextSdkAccountId）
//   集群模式用 dao.GetNextSdkAccountId（DB 自增）

// apiLogin 账号密码登录（POST /hk4e_:name/mdk/shield/api/login）
func (c *Controller) apiLogin(ctx *gin.Context) {
	requestData := new(api.LoginAccountRequestJson)
	err := ctx.ShouldBindJSON(requestData)
	if err != nil {
		logger.Error("parse LoginAccountRequestJson error: %v", err)
		return
	}

	encPwdData, err := base64.StdEncoding.DecodeString(requestData.Password)
	if err != nil {
		logger.Error("decode password enc data error: %v", err)
		return
	}
	pwdPrivKey, err := endec.RsaParsePrivKey(c.pwdRsaKey)
	if err != nil {
		logger.Error("parse rsa key error: %v", err)
		return
	}
	pwdDecData, err := endec.RsaDecrypt(encPwdData, pwdPrivKey)
	useAtAtMode := false
	if err != nil {
		logger.Debug("rsa dec error: %v", err)
		logger.Debug("password rsa dec fail, fallback to @@ mode")
		useAtAtMode = true
	} else {
		logger.Debug("password dec: %v", string(pwdDecData))
		useAtAtMode = false
	}

	responseData := api.NewLoginResult()

	var username string
	var password string
	if useAtAtMode {
		// 账号格式检查 用户名6-20字符 密码8-20字符 用户名和密码公用account字段 第一次出现的@@视为分割标识 username@@password
		if len(requestData.Account) > 20+20+2 {
			responseData.Retcode = -201
			responseData.Message = "用户名或密码长度超限"
			ctx.JSON(http.StatusOK, responseData)
			return
		}
		if !strings.Contains(requestData.Account, "@@") {
			responseData.Retcode = -201
			responseData.Message = "用户名同密码均填写到用户名输入框，填写格式为：用户名@@密码，密码输入框填写任意字符均可"
			ctx.JSON(http.StatusOK, responseData)
			return
		}
		atIndex := strings.Index(requestData.Account, "@@")
		username = requestData.Account[:atIndex]
		password = requestData.Account[atIndex+2:]
	} else {
		username = requestData.Account
		password = string(pwdDecData)
	}

	if len(username) < 6 || len(username) > 20 {
		responseData.Retcode = -201
		responseData.Message = "用户名为6-20位字符"
		ctx.JSON(http.StatusOK, responseData)
		return
	}
	if len(password) < 8 || len(password) > 20 {
		responseData.Retcode = -201
		responseData.Message = "密码为8-20位字符"
		ctx.JSON(http.StatusOK, responseData)
		return
	}
	ok, err := regexp.MatchString("^[a-zA-Z0-9]{6,20}$", username)
	if err != nil || !ok {
		responseData.Retcode = -201
		responseData.Message = "用户名只能包含大小写字母和数字"
		ctx.JSON(http.StatusOK, responseData)
		return
	}
	account, err := c.db.QuerySdkAccountByField("username", username)
	if err != nil {
		logger.Error("query account from db error: %v", err)
		return
	}
	if account == nil {
		// 自动注册
		accountId := uint32(0)
		if !config.GetConfig().Hk4e.StandaloneModeEnable {
			accountId, err = c.db.GetNextSdkAccountId()
			if err != nil {
				logger.Error("get next account id error: %v", err)
				responseData.Retcode = -201
				responseData.Message = "服务器内部错误:-1"
				ctx.JSON(http.StatusOK, responseData)
				return
			}
		} else {
			c.nextSdkAccountId++
			accountId = c.nextSdkAccountId
		}
		regAccount := &model.SdkAccount{
			AccountId:  accountId,
			Username:   username,
			Password:   endec.Md5Str(password),
			Token:      "",
			ComboToken: "",
		}
		err = c.db.InsertSdkAccount(regAccount)
		if err != nil {
			logger.Error("insert account error: %v", err)
			responseData.Retcode = -201
			responseData.Message = "服务器内部错误:-2"
			ctx.JSON(http.StatusOK, responseData)
			return
		}
		account = regAccount
	}
	if endec.Md5Str(password) != account.Password {
		responseData.Retcode = -201
		responseData.Message = "用户名或密码错误"
		ctx.JSON(http.StatusOK, responseData)
		return
	}
	// 生成新的token
	account.Token = base64.StdEncoding.EncodeToString(random.GetRandomByte(24))
	err = c.db.UpdateSdkAccountFieldByFieldName("account_id", account.AccountId, "token", account.Token)
	if err != nil {
		logger.Error("update account token error: %v", err)
		responseData.Retcode = -201
		responseData.Message = "服务器内部错误:-3"
		ctx.JSON(http.StatusOK, responseData)
		return
	}
	err = c.db.UpdateSdkAccountFieldByFieldName("account_id", account.AccountId, "token_create_time", time.Now().UnixMilli())
	if err != nil {
		logger.Error("update account token time error: %v", err)
		responseData.Retcode = -201
		responseData.Message = "服务器内部错误:-4"
		ctx.JSON(http.StatusOK, responseData)
		return
	}
	responseData.Message = "OK"
	responseData.Data.Account.Uid = strconv.FormatInt(int64(account.AccountId), 10)
	responseData.Data.Account.Token = account.Token
	responseData.Data.Account.Email = account.Username
	ctx.JSON(http.StatusOK, responseData)
}

// apiVerify Token 续期验证（POST /hk4e_:name/mdk/shield/api/verify）
//
// 客户端在已登录状态再次启动时不重新输入密码 直接用本地缓存的 Token 验证
// 校验：Token 与 DB 一致 + 创建时间不超过 7 天
// 通过则返回 200 让客户端走下一步（v2Login 拿 ComboToken）
func (c *Controller) apiVerify(ctx *gin.Context) {
	requestData := new(api.LoginTokenRequest)
	err := ctx.ShouldBindJSON(requestData)
	if err != nil {
		logger.Error("parse LoginTokenRequest error: %v", err)
		return
	}
	uid, err := strconv.ParseInt(requestData.Uid, 10, 64)
	if err != nil {
		logger.Error("parse uid error: %v", err)
		return
	}
	account, err := c.db.QuerySdkAccountByField("account_id", uid)
	if err != nil {
		logger.Error("query account from db error: %v", err)
		return
	}
	responseData := api.NewLoginResult()
	if account == nil || account.Token != requestData.Token {
		responseData.Retcode = -111
		responseData.Message = "账号本地缓存信息错误"
		ctx.JSON(http.StatusOK, responseData)
		return
	}
	if uint64(time.Now().UnixMilli())-account.TokenCreateTime > uint64(time.Hour.Milliseconds()*24*7) {
		responseData.Retcode = -111
		responseData.Message = "登录已失效"
		ctx.JSON(http.StatusOK, responseData)
		return
	}
	responseData.Message = "OK"
	responseData.Data.Account.Uid = requestData.Uid
	responseData.Data.Account.Token = requestData.Token
	responseData.Data.Account.Email = account.Username
	ctx.JSON(http.StatusOK, responseData)
}

// v2Login Token → ComboToken 交换（POST /hk4e_:name/combo/granter/login/v2/login）
//
// SDK 登录流程的最后一步：
//  1. 校验 Token 有效（与 DB 中保存的一致）
//  2. 生成新的 ComboToken（20 字节随机十六进制 = 40 字符）写入 DB
//  3. 返回给客户端
//
// ComboToken 是游戏会话凭证 客户端连 KCP 时通过 GetPlayerTokenReq.AccountToken 提交
// Gate 通过 /gate/token/verify 调 dispatch 验证 ComboToken（即 gateTokenVerify 接口）
func (c *Controller) v2Login(ctx *gin.Context) {
	requestData := new(api.ComboTokenReq)
	err := ctx.ShouldBindJSON(requestData)
	if err != nil {
		logger.Error("parse ComboTokenReq error: %v", err)
		return
	}
	data := requestData.Data
	if len(data) == 0 {
		logger.Error("requestData.Data len == 0")
		return
	}
	loginData := new(api.LoginTokenData)
	err = json.Unmarshal([]byte(data), loginData)
	if err != nil {
		logger.Error("Unmarshal LoginTokenData error: %v", err)
		return
	}
	uid, err := strconv.ParseInt(loginData.Uid, 10, 64)
	if err != nil {
		logger.Error("ParseInt uid error: %v", err)
		return
	}
	responseData := api.NewComboTokenRsp()
	account, err := c.db.QuerySdkAccountByField("account_id", uid)
	if account == nil {
		logger.Error("account not exist, account id: %v", uid)
		responseData.Retcode = -201
		responseData.Message = "账号不存在"
		ctx.JSON(http.StatusOK, responseData)
		return
	}
	if account.Token != "" && account.Token != loginData.Token {
		logger.Error("token not match, account token: %v, client token: %v", account.Token, loginData.Token)
		responseData.Retcode = -201
		responseData.Message = "token错误"
		ctx.JSON(http.StatusOK, responseData)
		return
	}
	// 生成新的comboToken
	account.ComboToken = random.GetRandomByteHexStr(20)
	err = c.db.UpdateSdkAccountFieldByFieldName("account_id", account.AccountId, "combo_token", account.ComboToken)
	if err != nil {
		logger.Error("update combo token error: %v", err)
		responseData.Retcode = -201
		responseData.Message = "服务器内部错误:-1"
		ctx.JSON(http.StatusOK, responseData)
		return
	}
	err = c.db.UpdateSdkAccountFieldByFieldName("account_id", account.AccountId, "combo_token_create_time", time.Now().UnixMilli())
	if err != nil {
		logger.Error("update combo token time error: %v", err)
		responseData.Retcode = -201
		responseData.Message = "服务器内部错误:-2"
		ctx.JSON(http.StatusOK, responseData)
		return
	}
	responseData.Message = "OK"
	responseData.Data.OpenID = loginData.Uid
	responseData.Data.ComboID = "0"
	responseData.Data.ComboToken = account.ComboToken
	ctx.JSON(http.StatusOK, responseData)
}
