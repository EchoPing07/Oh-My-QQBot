// Package qq 实现 QQ 官方机器人适配。
//
// 对应 refs/MaiBot-QQ-Adapter 插件：
//   - 直连 QQ 开放平台 WebSocket 网关接收事件
//   - 通过 OpenAPI 发送消息、上传图片、禁言、撤回
//
// 与插件版的区别只在于：这里是主程序内建适配器，不需要跨进程通信。
package qq

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// 平台常量。
const (
	Platform = "qq"

	// GroupAtMessageEvent 是「@机器人」的群消息事件。
	GroupAtMessageEvent = "GROUP_AT_MESSAGE_CREATE"
	// GroupMessageEvent 是全量群消息事件。
	GroupMessageEvent = "GROUP_MESSAGE_CREATE"
	// C2CMessageEvent 是单聊消息事件。
	C2CMessageEvent = "C2C_MESSAGE_CREATE"
)

// IsGroupEvent 判断事件是否为群聊消息。
func IsGroupEvent(event string) bool {
	return event == GroupAtMessageEvent || event == GroupMessageEvent
}

// IsMessageEvent 判断事件是否为消息事件。
func IsMessageEvent(event string) bool {
	return IsGroupEvent(event) || event == C2CMessageEvent
}

// APIError 是 OpenAPI 返回的错误。
type APIError struct {
	StatusCode int
	Path       string
	Code       any
	Message    string
	Raw        string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("QQ 接口 %s 失败: HTTP %d code=%v message=%s",
		e.Path, e.StatusCode, e.Code, e.Message)
}

// Client 是 OpenAPI 客户端。
// 对应插件里的 _authorize / _api_post / _upload_image。
type Client struct {
	baseURL   string
	appID     string
	appSecret string
	http      *http.Client

	mu           sync.Mutex
	accessToken  string
	tokenExpires time.Time
}

// NewClient 创建 OpenAPI 客户端。
func NewClient(baseURL, appID, appSecret string) *Client {
	if baseURL == "" {
		baseURL = "https://api.bot.qq.com"
	}
	return &Client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		appID:     appID,
		appSecret: appSecret,
		http:      &http.Client{Timeout: 30 * time.Second},
	}
}

// Token 获取访问凭证，提前 50 秒刷新。
// 对应插件 _authorize。
func (c *Client) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.accessToken != "" && time.Now().Before(c.tokenExpires.Add(-50*time.Second)) {
		return c.accessToken, nil
	}

	body, err := json.Marshal(map[string]string{
		"appId":        strings.TrimSpace(c.appID),
		"clientSecret": strings.TrimSpace(c.appSecret),
	})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/app/getAppAccessToken", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("获取 QQ 访问凭证失败: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	var parsed struct {
		AccessToken string `json:"access_token"`
		// QQ 平台实际返回的是字符串 "7126" 而不是数字。
		// 用 json.Number 兼容两种形态，避免死在一个类型上。
		ExpiresIn json.Number `json:"expires_in"`
		Code      any         `json:"code"`
		Message   string      `json:"message"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("解析访问凭证失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK || parsed.AccessToken == "" {
		return "", fmt.Errorf("获取 QQ 访问凭证失败: HTTP %d code=%v message=%s",
			resp.StatusCode, parsed.Code, parsed.Message)
	}

	// 拿不到有效期就按官方默认 7200 秒处理，总比不缓存好。
	expiresIn := 7200
	if n, err := parsed.ExpiresIn.Int64(); err == nil && n > 0 {
		expiresIn = int(n)
	}

	c.accessToken = parsed.AccessToken
	c.tokenExpires = time.Now().Add(time.Duration(expiresIn) * time.Second)
	return c.accessToken, nil
}

// GatewayURL 获取 WebSocket 网关地址。
// 对应插件 _connect 里的 GET /gateway。
func (c *Client) GatewayURL(ctx context.Context) (string, error) {
	var out struct {
		URL string `json:"url"`
	}
	if err := c.do(ctx, http.MethodGet, "/gateway", nil, &out); err != nil {
		return "", err
	}
	if !strings.HasPrefix(out.URL, "wss://") {
		return "", fmt.Errorf("QQ 网关没有返回有效的 wss 地址: %q", out.URL)
	}
	return out.URL, nil
}

// do 是统一的 OpenAPI 调用。
// 对应插件 _api_post：HTTP 错误时带上平台错误码与说明抛出。
func (c *Client) do(ctx context.Context, method, path string, payload, out any) error {
	token, err := c.Token(ctx)
	if err != nil {
		return err
	}

	var bodyReader io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "QQBot "+token)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("QQ 接口请求失败 %s: %w", path, err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		var errBody struct {
			Code    any    `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &errBody)
		return &APIError{
			StatusCode: resp.StatusCode,
			Path:       path,
			Code:       errBody.Code,
			Message:    errBody.Message,
			Raw:        truncate(string(raw), 200),
		}
	}

	if out == nil {
		return nil
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("解析 QQ 接口响应失败 %s: %w", path, err)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
