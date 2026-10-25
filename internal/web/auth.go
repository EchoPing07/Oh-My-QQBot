package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// 单密码认证。
//
// 明确不做的东西（配置里已确认）：
//   - 多用户
//   - 限流
//   - 反爬
//   - CSRF token
//   - 强制 HTTPS
//
// 只做一件事：密码对了给一个签名 Cookie。

const (
	cookieName   = "omq_session"
	cookieMaxAge = 7 * 24 * time.Hour
)

// authManager 管理登录状态。
type authManager struct {
	password string
	secret   []byte
}

func newAuthManager(password, secret string) *authManager {
	if secret == "" {
		// 没配密钥就随机生成一个，进程重启后需要重新登录。
		secret = randomHex(32)
	}
	return &authManager{
		password: password,
		secret:   []byte(secret),
	}
}

// Check 校验密码。
//
// 用 subtle.ConstantTimeCompare 做常数时间比较 —— 不是为了防什么强攻击，
// 只是这个习惯比裸 == 好，成本为零。
func (a *authManager) Check(password string) bool {
	expected := []byte(a.password)
	given := []byte(password)
	if len(expected) != len(given) {
		return false
	}
	return subtle.ConstantTimeCompare(expected, given) == 1
}

// Issue 签发 Cookie 值。
//
// 格式：过期时间戳.signature
// 签名 = HMAC-SHA256(secret, 过期时间戳)
func (a *authManager) Issue() string {
	expires := fmt.Sprint(time.Now().Add(cookieMaxAge).Unix())
	sig := a.sign(expires)
	return expires + "." + sig
}

// Verify 校验 Cookie 值。
func (a *authManager) Verify(token string) bool {
	dot := -1
	for i := len(token) - 1; i >= 0; i-- {
		if token[i] == '.' {
			dot = i
			break
		}
	}
	if dot <= 0 || dot >= len(token)-1 {
		return false
	}

	expires := token[:dot]
	sig := token[dot+1:]

	expected := a.sign(expires)
	if subtle.ConstantTimeCompare([]byte(expected), []byte(sig)) != 1 {
		return false
	}

	// 检查过期。
	//
	// 必须用 ParseInt 而不是 fmt.Sscan：Sscan 会接受数字后的垃圾，
	// 于是 "1792595302x.<正确签名>" 这类值也能通过。
	// 虽然伪造仍然需要密钥，但这种"宽松解析"是没有理由留着的口子。
	exp, err := strconv.ParseInt(expires, 10, 64)
	if err != nil {
		return false
	}
	return time.Now().Unix() < exp
}

func (a *authManager) sign(payload string) string {
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// verify 从请求里取 Cookie 并校验。
func (a *authManager) verify(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	return a.Verify(c.Value)
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// 极端情况下退化为时间戳，仍然能用。
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}
