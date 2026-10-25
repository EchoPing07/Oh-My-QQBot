package web

// 认证层测试：登录、Cookie 签发与校验、登出、伪造/过期 Cookie。
//
// 防的是：认证被绕过（任何伪造串都能进后台）、密码比对被误放行、
// 登出后本地 Cookie 没被清掉（浏览器仍带着旧凭据）。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// protectedEndpoints 列出所有必须登录的 /api/* 端点及其探测方式。
// 防：新增端点时忘记套 requireAuth。
var protectedEndpoints = []struct {
	Name   string
	Method string
	Path   string
	Body   string
}{
	{Name: "status", Method: http.MethodGet, Path: "/api/status"},
	{Name: "sessions", Method: http.MethodGet, Path: "/api/sessions"},
	{Name: "messages", Method: http.MethodGet, Path: "/api/messages?session_id=" + groupSessionID},
	{Name: "chat-session", Method: http.MethodGet, Path: "/api/chat/session"},
	{Name: "chat-send", Method: http.MethodPost, Path: "/api/chat/send", Body: `{"text":"hi"}`},
	{Name: "config", Method: http.MethodGet, Path: "/api/config"},
	{Name: "usage", Method: http.MethodGet, Path: "/api/usage"},
	{Name: "logs", Method: http.MethodGet, Path: "/api/logs"},
}

// TestLoginWrongPasswordReturns401 防：错误密码也能拿到登录态。
func TestLoginWrongPasswordReturns401(t *testing.T) {
	env := newTestEnv(t, envOptions{})

	resp, body := env.do(http.MethodPost, "/api/login", `{"password":"wrong-password"}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("错误密码状态码 = %d，期望 401，body=%s", resp.StatusCode, body)
	}
	if cookies := resp.Cookies(); len(cookies) > 0 {
		t.Fatalf("错误密码不应下发任何 Cookie，实际: %v", cookies)
	}

	// 响应结构断言。
	var out map[string]any
	decodeJSON(t, body, &out)
	if _, ok := out["error"]; !ok {
		t.Fatalf("401 响应缺少 error 字段: %s", body)
	}
}

// TestLoginCorrectPasswordIssuesCookie 防：登录成功后没有可用的签名 Cookie，
// 或者 Cookie 属性缺失（HttpOnly / Path / MaxAge）导致前端拿不到登录态。
func TestLoginCorrectPasswordIssuesCookie(t *testing.T) {
	env := newTestEnv(t, envOptions{})

	payload, err := json.Marshal(map[string]string{"password": testPassword})
	if err != nil {
		t.Fatalf("编码请求失败: %v", err)
	}
	resp, body := env.do(http.MethodPost, "/api/login", string(payload))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("正确密码状态码 = %d，期望 200，body=%s", resp.StatusCode, body)
	}

	var out map[string]any
	decodeJSON(t, body, &out)
	if ok, _ := out["ok"].(bool); !ok {
		t.Fatalf("登录成功响应 ok 字段 = %v，期望 true，body=%s", out["ok"], body)
	}

	var session *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == cookieName {
			session = c
			break
		}
	}
	if session == nil {
		t.Fatalf("登录响应缺少 %s Cookie，Set-Cookie=%v", cookieName, resp.Header.Values("Set-Cookie"))
	}
	if session.Value == "" {
		t.Fatal("登录 Cookie 值为空")
	}
	if !session.HttpOnly {
		t.Error("登录 Cookie 必须 HttpOnly，防 XSS 直接读走")
	}
	if session.Path != "/" {
		t.Errorf("登录 Cookie Path = %q，期望 /", session.Path)
	}
	if session.MaxAge <= 0 {
		t.Errorf("登录 Cookie MaxAge = %d，期望 > 0", session.MaxAge)
	}
	if !strings.Contains(session.Value, ".") {
		t.Errorf("Cookie 值 %q 不符合 过期时间戳.签名 格式", session.Value)
	}
	// Cookie 必须真的是本服务签发的。
	if !env.s.auth.Verify(session.Value) {
		t.Errorf("服务端无法校验自己签发的 Cookie: %q", session.Value)
	}
	// Cookie 不应携带配置里的密码明文。
	if strings.Contains(session.Value, testPassword) {
		t.Error("Cookie 值里出现了明文密码")
	}
}

// TestProtectedEndpointsRejectAnonymous 防：任何一个受保护端点在未登录时泄漏数据。
func TestProtectedEndpointsRejectAnonymous(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	seedData(t, env.db)

	for _, ep := range protectedEndpoints {
		t.Run(ep.Name, func(t *testing.T) {
			resp, body := env.do(ep.Method, ep.Path, ep.Body)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("匿名访问 %s 状态码 = %d，期望 401，body=%s", ep.Path, resp.StatusCode, body)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
				t.Errorf("匿名访问 %s 的 Content-Type = %q，期望 JSON", ep.Path, ct)
			}
		})
	}
}

// TestProtectedEndpointsAcceptValidCookie 防：合法 Cookie 被误拒（登录后反而是空后台）。
func TestProtectedEndpointsAcceptValidCookie(t *testing.T) {
	// 带上 fakeHub：chat/send 在 Hub 缺失时按设计返回 503，
	// 这个测试只关心「认证放行」，不该被 503 打断。
	env := newTestEnv(t, envOptions{hub: &fakeHub{}})
	seedData(t, env.db)
	token := env.loginOK()

	for _, ep := range protectedEndpoints {
		if ep.Name == "logs" {
			// SSE 会挂住连接，单独在 logs 测试里验证。
			continue
		}
		t.Run(ep.Name, func(t *testing.T) {
			resp, body := env.doWithCookie(ep.Method, ep.Path, ep.Body, token)
			if resp.StatusCode == http.StatusUnauthorized {
				t.Fatalf("合法 Cookie 访问 %s 被拒，body=%s", ep.Path, body)
			}
			if resp.StatusCode >= 500 {
				t.Fatalf("合法 Cookie 访问 %s 返回 %d，body=%s", ep.Path, resp.StatusCode, body)
			}
		})
	}
}

// TestForgedCookieRejected 防：伪造 / 篡改 / 过期 Cookie 被当成登录态。
func TestForgedCookieRejected(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	valid := env.s.auth.Issue()

	// 构造一个「过期时间在将来、签名正确」的 token，用来验证过期分支本身有效。
	future := fmt.Sprint(time.Now().Add(time.Hour).Unix())
	expired := fmt.Sprint(time.Now().Add(-time.Hour).Unix())

	cases := []struct {
		Name  string
		Value string
	}{
		{Name: "完全乱码", Value: "not-a-token"},
		{Name: "空值", Value: ""},
		{Name: "无签名", Value: future + "."},
		{Name: "无过期时间", Value: "." + env.s.auth.sign(future)},
		{Name: "签名被篡改", Value: future + "." + strings.Repeat("a", 64)},
		{Name: "过期时间被篡改", Value: future + valid[strings.Index(valid, "."):]},
		{Name: "已过期但签名正确", Value: expired + "." + env.s.auth.sign(expired)},
		{Name: "过期时间非数字", Value: "abc." + env.s.auth.sign("abc")},
		{Name: "用别的密钥签的", Value: future + "." + newAuthManager("", "another-secret").sign(future)},
	}

	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			resp, body := env.doWithCookie(http.MethodGet, "/api/status", "", c.Value)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("Cookie %q 状态码 = %d，期望 401，body=%s", c.Value, resp.StatusCode, body)
			}
		})
	}

	// 反向确认：合法 token 在同样调用方式下必须通过，避免上面全是假阳性。
	resp, body := env.doWithCookie(http.MethodGet, "/api/status", "", valid)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("合法 token 状态码 = %d，期望 200，body=%s", resp.StatusCode, body)
	}
}

// TestLogoutClearsCookie 防：登出后浏览器仍保留旧 Cookie，刷新页面又进后台。
func TestLogoutClearsCookie(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	env.loginOK()

	// 登录后确认能访问。
	if resp, body := env.do(http.MethodGet, "/api/status", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("登录后访问 /api/status = %d，期望 200，body=%s", resp.StatusCode, body)
	}

	resp, body := env.do(http.MethodPost, "/api/logout", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("登出状态码 = %d，期望 200，body=%s", resp.StatusCode, body)
	}

	// 登出响应必须把 Cookie 置空并让 MaxAge 为负（删除指令）。
	var cleared bool
	for _, c := range resp.Cookies() {
		if c.Name != cookieName {
			continue
		}
		cleared = true
		if c.Value != "" {
			t.Errorf("登出 Cookie 值 = %q，期望空", c.Value)
		}
		if c.MaxAge >= 0 {
			t.Errorf("登出 Cookie MaxAge = %d，期望负数（删除）", c.MaxAge)
		}
		if c.Path != "/" {
			t.Errorf("登出 Cookie Path = %q，期望 /", c.Path)
		}
	}
	if !cleared {
		t.Fatalf("登出响应缺少 %s Cookie，Set-Cookie=%v", cookieName, resp.Header.Values("Set-Cookie"))
	}

	// Cookie Jar 应已删除该 Cookie，后续请求回到未登录。
	resp, body = env.do(http.MethodGet, "/api/status", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("登出后访问 /api/status = %d，期望 401，body=%s", resp.StatusCode, body)
	}
}

// TestLogoutLeavesTokenReplayable 记录一个已知设计限制（不是回归，而是事实）：
// 认证是无状态签名 Cookie，登出只清浏览器端的 Cookie，
// 旧 token 在过期前仍可被重放。注释在此，防止有人误以为登出做了服务端吊销。
func TestLogoutLeavesTokenReplayable(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	token := env.loginOK()

	if _, body := env.do(http.MethodPost, "/api/logout", ""); len(body) == 0 {
		t.Fatal("登出响应为空")
	}

	// 手工重放旧 token：当前实现仍然放行。
	resp, body := env.doWithCookie(http.MethodGet, "/api/status", "", token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("重放旧 token 状态码 = %d，期望 200（当前无状态设计），body=%s", resp.StatusCode, body)
	}
}

// TestLoginRejectsNonPostAndBadBody 防：登录接口被 GET 触发或坏 JSON 导致 500。
func TestLoginRejectsNonPostAndBadBody(t *testing.T) {
	env := newTestEnv(t, envOptions{})

	if resp, body := env.do(http.MethodGet, "/api/login", ""); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/login = %d，期望 405，body=%s", resp.StatusCode, body)
	}
	if resp, body := env.do(http.MethodPost, "/api/login", "{not json"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("坏 JSON 登录 = %d，期望 400，body=%s", resp.StatusCode, body)
	}
}

// TestVerifyRejectsTrailingGarbageInExpiry 防：过期时间宽松解析。
//
// 原来用 fmt.Sscan，它接受数字后缀，于是 "1792595302x.<合法签名>" 也能通过。
// 现在改用 strconv.ParseInt，带后缀的值必须被拒。
func TestVerifyRejectsTrailingGarbageInExpiry(t *testing.T) {
	a := newAuthManager(testPassword, testSessionSecret)

	for _, suffix := range []string{"x", " ", "0x", ".5", "abc"} {
		payload := fmt.Sprint(time.Now().Add(time.Hour).Unix()) + suffix
		token := payload + "." + a.sign(payload)
		if a.Verify(token) {
			t.Errorf("过期时间带后缀 %q 的 token 应被拒绝，实际通过", suffix)
		}
	}

	// 反向验证：正常 token 仍然可用，避免「改成一律拒绝」这种过度修复。
	ok := fmt.Sprint(time.Now().Add(time.Hour).Unix())
	if !a.Verify(ok + "." + a.sign(ok)) {
		t.Error("正常 token 应通过校验")
	}
}

// TestAuthManagerCheckIsExactMatch 防：ConstantTimeCompare 的长度分支被改坏，
// 导致空密码 / 前缀密码被放行。
func TestAuthManagerCheckIsExactMatch(t *testing.T) {
	a := newAuthManager(testPassword, testSessionSecret)

	if !a.Check(testPassword) {
		t.Error("正确密码校验失败")
	}
	for _, bad := range []string{"", "test-password", testPassword + "x", strings.ToUpper(testPassword)} {
		if a.Check(bad) {
			t.Errorf("密码 %q 被误判为正确", bad)
		}
	}
}

// TestAuthManagerIssueVerifyRoundTrip 防：签发的 token 自己都验不过，
// 或者过期时间解析被 fmt.Sscan 的宽松匹配绕过。
func TestAuthManagerIssueVerifyRoundTrip(t *testing.T) {
	a := newAuthManager(testPassword, testSessionSecret)

	token := a.Issue()
	if !a.Verify(token) {
		t.Fatalf("签发后立即校验失败: %q", token)
	}

	// 使用另一个密钥的 manager 必须验不过。
	if newAuthManager(testPassword, "other-secret").Verify(token) {
		t.Error("不同密钥签发的 token 被误放行")
	}

	// 结构被破坏的 token。
	for _, bad := range []string{"", ".", "abc", "123.", ".abc", "123.abc"} {
		if a.Verify(bad) {
			t.Errorf("非法 token %q 被误放行", bad)
		}
	}
}
