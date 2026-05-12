package controller

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"hk4e/common/config"

	"github.com/flswld/halo/logger"
	"github.com/gin-gonic/gin"
)

// gateTokenVerify 接口实现 - Gate 内部调用验证 ComboToken
//
// 流程：玩家通过 KCP 连到 Gate 后 Gate 用 doGateLogin → POST /gate/token/verify 验证 ComboToken
// 同时 Gate 会用 LoginSdkAccountKey HMAC-SHA256 算签名 防止伪造请求
//
// 校验：
//   1. HMAC 签名匹配（防伪造）
//   2. ComboToken 与 DB 一致
//   3. ComboToken 创建时间不超过 24 小时（比 Token 的 7 天短 防止长期会话被劫持）

// gateReqErrorRsp gate 请求错误响应
func (c *Controller) gateReqErrorRsp(ctx *gin.Context) {
	ctx.JSON(http.StatusOK, gin.H{"retcode": -101, "message": "系统错误", "data": nil})
}

// TokenVerifyReq Gate → Dispatch 的 ComboToken 验证请求
//   - AppID/ChannelID: 平台标识（hk4e=1）
//   - OpenID: 玩家账号 ID（其实是 SdkAccount.AccountId 字符串形式）
//   - ComboToken: 客户端通过 KCP 提交的 token 由 Dispatch 验证
//   - Sign: HMAC-SHA256(LoginSdkAccountKey, "app_id=1&channel_id=1&combo_token=...&open_id=...") 防伪造
type TokenVerifyReq struct {
	AppID      uint32 `json:"app_id"`
	ChannelID  uint32 `json:"channel_id"`
	OpenID     string `json:"open_id"`
	ComboToken string `json:"combo_token"`
	Sign       string `json:"sign"`
	Region     string `json:"region"`
}

type TokenVerifyRsp struct {
	RetCode int32  `json:"retcode"`
	Message string `json:"message"`
	Data    struct {
		Guest       bool   `json:"guest"`
		AccountType uint32 `json:"account_type"`
		AccountUid  uint32 `json:"account_uid"`
		IpInfo      struct {
			CountryCode string `json:"country_code"`
		} `json:"ip_info"`
	} `json:"data"`
}

// gateTokenVerify ComboToken 验证接口（POST /gate/token/verify）
// 仅供 Gate 内部调用 通过 LoginSdkAccountKey 共享密钥做 HMAC 防止外部伪造
func (c *Controller) gateTokenVerify(ctx *gin.Context) {
	tokenVerifyReq := new(TokenVerifyReq)
	err := ctx.ShouldBindJSON(tokenVerifyReq)
	if err != nil {
		c.gateReqErrorRsp(ctx)
		return
	}
	logger.Info("gate token verify, req: %v", tokenVerifyReq)
	signStr := fmt.Sprintf("app_id=%d&channel_id=%d&combo_token=%s&open_id=%s", 1, 1, tokenVerifyReq.ComboToken, tokenVerifyReq.OpenID)
	signHash := hmac.New(sha256.New, []byte(config.GetConfig().Hk4e.LoginSdkAccountKey))
	signHash.Write([]byte(signStr))
	signData := signHash.Sum(nil)
	sign := hex.EncodeToString(signData)
	if tokenVerifyReq.Sign != sign {
		c.gateReqErrorRsp(ctx)
		return
	}
	accountId, err := strconv.Atoi(tokenVerifyReq.OpenID)
	if err != nil {
		c.gateReqErrorRsp(ctx)
		return
	}
	account, err := c.db.QuerySdkAccountByField("account_id", uint64(accountId))
	if err != nil || account == nil {
		c.gateReqErrorRsp(ctx)
		return
	}
	if tokenVerifyReq.ComboToken != account.ComboToken {
		c.gateReqErrorRsp(ctx)
		return
	}
	if time.Now().UnixMilli()-int64(account.ComboTokenCreateTime) > time.Hour.Milliseconds()*24 {
		c.gateReqErrorRsp(ctx)
		return
	}
	ctx.JSON(http.StatusOK, &TokenVerifyRsp{
		RetCode: 0,
		Message: "OK",
		Data: struct {
			Guest       bool   `json:"guest"`
			AccountType uint32 `json:"account_type"`
			AccountUid  uint32 `json:"account_uid"`
			IpInfo      struct {
				CountryCode string `json:"country_code"`
			} `json:"ip_info"`
		}{
			Guest:       false,
			AccountType: 1,
			AccountUid:  uint32(accountId),
			IpInfo: struct {
				CountryCode string `json:"country_code"`
			}{
				CountryCode: "US",
			},
		},
	})
}
