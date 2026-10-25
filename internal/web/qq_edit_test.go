package web

// QQ 凭据接口的测试。
//
// 最重要的一条：`GET /api/config/qq` 的原始响应体里不能出现密钥。
// 不是「检查 JSON 里的某个字段」——直接把整个 body 拿出来查子串，
// 这样连「以后有人加了个 debug 字段」也能拦住。

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
)

const qqTestSecret = "QQ-SECRET-DO-NOT-LEAK-1234567890"

// TestQQGetHidesSecret 守：响应体里没有密钥原文。
func TestQQGetHidesSecret(t *testing.T) {
	env, _ := newConfigTestEnv(t)
	env.setQQ("102000000", qqTestSecret)
	env.loginOK()

	resp, body := env.do(http.MethodGet, "/api/config/qq", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), qqTestSecret) {
		t.Errorf("响应里出现了密钥明文: %s", body)
	}

	var view config.QQView
	if err := json.Unmarshal([]byte(body), &view); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !view.SecretSet {
		t.Error("secret_set 应为 true")
	}
	if view.AppID != "102000000" {
		t.Errorf("AppID = %q", view.AppID)
	}
}

// TestQQSaveKeepsSecretWhenEmpty 守：留空 = 不修改。
func TestQQSaveKeepsSecretWhenEmpty(t *testing.T) {
	env, _ := newConfigTestEnv(t)
	env.setQQ("111", qqTestSecret)
	env.loginOK()

	resp, body := env.do(http.MethodPost, "/api/config/qq/save",
		`{"app_id":"222"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), qqTestSecret) {
		t.Errorf("响应里出现了密钥明文: %s", body)
	}

	cfg := env.s.cfg.Get()
	if cfg.QQ.AppID != "222" {
		t.Errorf("AppID = %q", cfg.QQ.AppID)
	}
	if cfg.QQ.AppSecret != qqTestSecret {
		t.Errorf("留空时密钥不该变，实际 %q", cfg.QQ.AppSecret)
	}
}

// TestQQSaveClearSecret 守：显式清空。
func TestQQSaveClearSecret(t *testing.T) {
	env, _ := newConfigTestEnv(t)
	env.setQQ("111", qqTestSecret)
	env.loginOK()

	resp, body := env.do(http.MethodPost, "/api/config/qq/save",
		`{"app_id":"111","clear_app_secret":true}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, body)
	}
	if env.s.cfg.Get().QQ.AppSecret != "" {
		t.Error("密钥应当被清空")
	}
}

// TestQQSaveRejectsBadAppID 守：非数字 AppID 返回 400 而不是写进去。
func TestQQSaveRejectsBadAppID(t *testing.T) {
	env, _ := newConfigTestEnv(t)
	env.loginOK()

	resp, body := env.do(http.MethodPost, "/api/config/qq/save",
		`{"app_id":"abc"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码 = %d（期望 400），body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "数字") {
		t.Errorf("错误信息应说明原因: %s", body)
	}
}

// TestQQSaveSetsRestartNotice 守：保存后告诉用户需重启。
//
// 网关客户端在 app.Start 里用当时的凭据构造，改配置不会重建它。
// 不说的话，「填完凭据机器人还是没反应」会被当成 bug 排查很久。
func TestQQSaveSetsRestartNotice(t *testing.T) {
	env, _ := newConfigTestEnv(t)
	env.loginOK()

	resp, body := env.do(http.MethodPost, "/api/config/qq/save",
		`{"app_id":"111","app_secret":"abc"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, body)
	}

	var out struct {
		Restart []string `json:"restart"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(out.Restart) == 0 {
		t.Error("保存凭据后应当提示需重启")
	}

	// 后续读配置页时这条提示也要还在（用户可能关掉再打开）。
	_, fieldsBody := env.do(http.MethodGet, "/api/config/edit", "")
	if !strings.Contains(string(fieldsBody), "QQ 凭据") {
		t.Errorf("配置页应显示重启提示: %s", fieldsBody)
	}
}

// TestQQEndpointsRequireAuth 守：未鉴权被拒。
func TestQQEndpointsRequireAuth(t *testing.T) {
	env, _ := newConfigTestEnv(t)

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/config/qq"},
		{http.MethodPost, "/api/config/qq/save"},
	} {
		resp, _ := env.do(tc.method, tc.path, "{}")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s 未鉴权时状态码 = %d，期望 401", tc.method, tc.path, resp.StatusCode)
		}
	}
}

// TestQQEndpointsMethodLimited 守：方法限定。
func TestQQEndpointsMethodLimited(t *testing.T) {
	env, _ := newConfigTestEnv(t)
	env.loginOK()

	resp, _ := env.do(http.MethodPost, "/api/config/qq", "{}")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/config/qq 状态码 = %d，期望 405", resp.StatusCode)
	}

	resp, _ = env.do(http.MethodGet, "/api/config/qq/save", "")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /api/config/qq/save 状态码 = %d，期望 405", resp.StatusCode)
	}
}

// setQQ 把凭据写进当前生效的配置。
//
// 不能直接改 env.cfg：newConfigTestEnv 把 Server 的 Store 换成了
// 一份独立的 NewStoreFrom(cfg, path)，之后 env.cfg 与它就不是同一个对象了
// —— 改 env.cfg 的话接口读到的还是旧值，测试会以「值没变」的形式失败，
// 看起来像保存逻辑坏了，实际是测试自己搞错了对象。
func (e *testEnv) setQQ(appID, secret string) {
	e.t.Helper()
	current := e.s.cfg.Get()
	current.QQ.AppID = appID
	current.QQ.AppSecret = secret
}
