package httpclient

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/flswld/halo/logger"
)

// HTTP 客户端工具集（泛型 GET/POST）
//
// 项目用例：
//   - gate doGateLogin → POST /gate/token/verify 验证 ComboToken
//   - robot 模拟客户端的 dispatch / SDK 登录请求
//
// 配置：
//   - 超时 10 秒
//   - InsecureSkipVerify=true（跳过 TLS 证书校验 项目内部通信用 自签证书也能用）
//   - DisableKeepAlives=true（每次请求独立连接 避免长连接异常）
//
// 泛型支持：T 是响应 JSON 解析的目标结构体类型
//   - GetJson[Rsp]: GET 请求返回 *Rsp
//   - PostJson[Rsp]: POST 请求返回 *Rsp
//
// authToken 可选参数：传入则添加 Authorization: Bearer xxx 头

var httpClient http.Client

func init() {
	httpClient = http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
			DisableKeepAlives: true,
		},
		Timeout: time.Second * 10,
	}
}

func GetJson[T any](url string, authToken ...string) (*T, error) {
	logger.Debug("http get req url: %v", url)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	if len(authToken) != 0 {
		req.Header.Set("Authorization", "Bearer"+" "+authToken[0])
	}
	rsp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(rsp.Body)
	_ = rsp.Body.Close()
	if err != nil {
		return nil, err
	}
	logger.Debug("http get rsp data: %v", string(data))
	responseData := new(T)
	err = json.Unmarshal(data, responseData)
	if err != nil {
		return nil, err
	}
	return responseData, nil
}

func GetRaw(url string, authToken ...string) (string, error) {
	logger.Debug("http get req url: %v", url)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}
	if len(authToken) != 0 {
		req.Header.Set("Authorization", "Bearer"+" "+authToken[0])
	}
	rsp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	data, err := io.ReadAll(rsp.Body)
	_ = rsp.Body.Close()
	if err != nil {
		return "", err
	}
	logger.Debug("http get rsp data: %v", string(data))
	return string(data), nil
}

func PostJson[T any](url string, body any, authToken ...string) (*T, error) {
	reqData, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	logger.Debug("http post req url: %v", url)
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(reqData))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if len(authToken) != 0 {
		req.Header.Set("Authorization", "Bearer"+" "+authToken[0])
	}
	rsp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	rspData, err := io.ReadAll(rsp.Body)
	_ = rsp.Body.Close()
	if err != nil {
		return nil, err
	}
	logger.Debug("http post rsp data: %v", string(rspData))
	responseData := new(T)
	err = json.Unmarshal(rspData, responseData)
	if err != nil {
		return nil, err
	}
	return responseData, nil
}

func PostRaw(url string, body string, authToken ...string) (string, error) {
	logger.Debug("http post req url: %v", url)
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if len(authToken) != 0 {
		req.Header.Set("Authorization", "Bearer"+" "+authToken[0])
	}
	rsp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	rspData, err := io.ReadAll(rsp.Body)
	_ = rsp.Body.Close()
	if err != nil {
		return "", err
	}
	logger.Debug("http post rsp data: %v", string(rspData))
	return string(rspData), nil
}
