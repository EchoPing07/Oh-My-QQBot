package web

// 网页对话窗口的接口测试：/api/chat/session 与 /api/chat/send。
//
// 这是「以机器人身份发话」（旧 /api/send，已移除）的替代方向：
// 网页用户以人类身份把消息投进 platform=webui 的私聊会话。

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
)

// TestChatSessionStable 防：session_id 算法变化导致前端缓存的 ID 失效，
// 或者两次调用返回不同 ID（前端会把历史拉到不存在的会话上）。
func TestChatSessionStable(t *testing.T) {
	hub := &fakeHub{}
	env := newTestEnv(t, envOptions{hub: hub})
	env.loginOK()

	resp, body := env.do(http.MethodGet, "/api/chat/session", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200，body=%s", resp.StatusCode, body)
	}
	var out struct {
		SessionID string `json:"session_id"`
		BotName   string `json:"bot_name"`
	}
	decodeJSON(t, body, &out)
	if out.SessionID != domain.WebUISessionKey().ID() {
		t.Errorf("session_id = %q，期望 domain.WebUISessionKey().ID()", out.SessionID)
	}
	if out.BotName != "木木测试" {
		t.Errorf("bot_name = %q，期望测试配置里的昵称", out.BotName)
	}

	// 第二次调用必须一致。
	_, body2 := env.do(http.MethodGet, "/api/chat/session", "")
	var out2 struct {
		SessionID string `json:"session_id"`
	}
	decodeJSON(t, body2, &out2)
	if out2.SessionID != out.SessionID {
		t.Errorf("两次 session_id 不一致: %q vs %q", out.SessionID, out2.SessionID)
	}
}

// TestChatSendDispatchesToWebUISession 防：投递的消息字段构造错误 ——
// 平台不是 webui、没带 IsAt、路由位不对，机器人这条链路就断了。
// 对齐 MaiBot 的 is_at_bot=True 语义（网页消息总是视为 @ 了机器人）。
func TestChatSendDispatchesToWebUISession(t *testing.T) {
	hub := &fakeHub{}
	env := newTestEnv(t, envOptions{hub: hub})
	env.loginOK()

	resp, raw := env.do(http.MethodPost, "/api/chat/send", `{"text":"你好呀","user_name":"阿平"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("发送状态码 = %d，期望 200，body=%s", resp.StatusCode, raw)
	}

	msgs, scenes := hub.snapshotDispatched()
	if len(msgs) != 1 {
		t.Fatalf("Dispatch 调用次数 = %d，期望 1", len(msgs))
	}
	m := msgs[0]
	if m.Platform != domain.PlatformWebUI {
		t.Errorf("Platform = %q，期望 webui", m.Platform)
	}
	if m.IsGroup {
		t.Error("IsGroup = true，期望私聊")
	}
	if !m.IsAt {
		t.Error("IsAt = false，期望 true（网页消息视为 @ 了机器人，必触发回复）")
	}
	if m.UserName != "阿平" {
		t.Errorf("UserName = %q，期望传入昵称", m.UserName)
	}
	if m.PlainText != "你好呀" || len(m.Segments) != 1 || m.Segments[0].Text != "你好呀" {
		t.Errorf("消息内容 = %q / %d 段，期望纯文本一段「你好呀」", m.PlainText, len(m.Segments))
	}
	if m.UserID != domain.WebUITargetID || m.AccountID != domain.WebUIAccountID {
		t.Errorf("UserID/AccountID = %q/%q，期望 webui 路由位", m.UserID, m.AccountID)
	}
	if len(scenes) != 1 || scenes[0] != domain.ScopePrivate+"|"+domain.WebUITargetID+"|"+domain.WebUIAccountID {
		t.Errorf("Dispatch 路由参数 = %v，期望 private|webui-user|webui-bot", scenes)
	}

	var out struct {
		OK        bool   `json:"ok"`
		SessionID string `json:"session_id"`
	}
	decodeJSON(t, raw, &out)
	if !out.OK {
		t.Errorf("ok = false，body=%s", raw)
	}
	if out.SessionID != domain.WebUISessionKey().ID() {
		t.Errorf("session_id = %q，期望与 /api/chat/session 一致", out.SessionID)
	}
}

// TestChatSendNormalizesIdentity 防：空昵称/超长昵称/纯空格文本被放行。
// 昵称会渲染进每条消息的前缀，脏值会污染机器人看到的上下文。
func TestChatSendNormalizesIdentity(t *testing.T) {
	hub := &fakeHub{}
	env := newTestEnv(t, envOptions{hub: hub})
	env.loginOK()

	// 空昵称 → 默认「网友」；文本两端空格被去掉。
	resp, _ := env.do(http.MethodPost, "/api/chat/send", `{"text":"  你好  ","user_name":""}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatal("空昵称应被接受并给默认值")
	}
	// 超长昵称 → 截断到 24 字。
	long := strings.Repeat("很长的名字", 20)
	env.do(http.MethodPost, "/api/chat/send", `{"text":"hi","user_name":"`+long+`"}`)

	msgs, _ := hub.snapshotDispatched()
	if len(msgs) != 2 {
		t.Fatalf("Dispatch 次数 = %d，期望 2", len(msgs))
	}
	if msgs[0].UserName != "网友" {
		t.Errorf("空昵称 UserName = %q，期望 网友", msgs[0].UserName)
	}
	if msgs[0].PlainText != "你好" {
		t.Errorf("PlainText = %q，期望去空格后的「你好」", msgs[0].PlainText)
	}
	if got := len([]rune(msgs[1].UserName)); got > 24 {
		t.Errorf("超长昵称未截断，长度 = %d", got)
	}
}

// TestChatSendWithoutHubReturns503 防：Hub 未就绪时返回 200，
// 前端以为消息已送达而机器人永远不回。
func TestChatSendWithoutHubReturns503(t *testing.T) {
	env := newTestEnv(t, envOptions{}) // hub 为 nil
	env.loginOK()

	resp, body := env.do(http.MethodPost, "/api/chat/send", `{"text":"hi"}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("无 Hub 状态码 = %d，期望 503，body=%s", resp.StatusCode, body)
	}
}

// TestChatSendParameterBoundaries 防：空文本/坏 JSON/错误方法被放行。
func TestChatSendParameterBoundaries(t *testing.T) {
	hub := &fakeHub{}
	env := newTestEnv(t, envOptions{hub: hub})
	env.loginOK()

	cases := []struct {
		Name   string
		Method string
		Path   string
		Body   string
		Want   int
	}{
		{Name: "空文本", Method: http.MethodPost, Body: `{"text":""}`, Want: http.StatusBadRequest},
		{Name: "纯空格文本", Method: http.MethodPost, Body: `{"text":"   "}`, Want: http.StatusBadRequest},
		{Name: "缺 text 字段", Method: http.MethodPost, Body: `{}`, Want: http.StatusBadRequest},
		{Name: "坏 JSON", Method: http.MethodPost, Body: `{"text":`, Want: http.StatusBadRequest},
		{Name: "空 body", Method: http.MethodPost, Body: ``, Want: http.StatusBadRequest},
		{Name: "GET 方法", Method: http.MethodGet, Path: "/api/chat/send", Want: http.StatusMethodNotAllowed},
		{Name: "session 接口 POST", Method: http.MethodPost, Path: "/api/chat/session", Want: http.StatusMethodNotAllowed},
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			path := c.Path
			if path == "" {
				path = "/api/chat/send"
			}
			resp, body := env.do(c.Method, path, c.Body)
			if resp.StatusCode != c.Want {
				t.Fatalf("状态码 = %d，期望 %d，body=%s", resp.StatusCode, c.Want, body)
			}
		})
	}

	if msgs, _ := hub.snapshotDispatched(); len(msgs) != 0 {
		t.Errorf("非法参数下不应 Dispatch，实际: %d 条", len(msgs))
	}
}

// TestOldSendEndpointRemoved 守住「以机器人身份发话」真的删干净了：
// 旧 /api/send 不再注册，返回 404 而不是悄悄继续能用。
func TestOldSendEndpointRemoved(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	env.loginOK()

	resp, _ := env.do(http.MethodPost, "/api/send", `{"session_id":"x","text":"hi"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("旧 /api/send 状态码 = %d，期望 404（接口已移除）", resp.StatusCode)
	}
}
