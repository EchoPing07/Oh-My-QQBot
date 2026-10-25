package web

// API 层测试：状态 / 会话 / 消息 / 对话窗口 / 配置 / 用量 / 参数边界。
//
// 全部通过 httptest.NewServer(s.routes()) 走真实 HTTP，
// 这样路由注册、中间件、JSON 编码都在链路上被覆盖。

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
)

// TestStatusJSONShape 防：状态接口字段改名/漏字段，前端仪表盘静默显示 0。
func TestStatusJSONShape(t *testing.T) {
	hub := &fakeHub{n: 3}
	env := newTestEnv(t, envOptions{hub: hub})
	seedData(t, env.db)
	env.loginOK()

	resp, body := env.do(http.MethodGet, "/api/status", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200，body=%s", resp.StatusCode, body)
	}

	var out struct {
		Nickname      string `json:"nickname"`
		Platform      string `json:"platform"`
		AccountID     string `json:"account_id"`
		Messages      int64  `json:"messages"`
		Sessions      int64  `json:"sessions"`
		Persons       int64  `json:"persons"`
		ActiveChats   int    `json:"active_chats"`
		UptimeSeconds int64  `json:"uptime_seconds"`
	}
	decodeJSON(t, body, &out)

	if out.Nickname != env.cfg.Bot.Nickname {
		t.Errorf("nickname = %q，期望 %q", out.Nickname, env.cfg.Bot.Nickname)
	}
	if out.Platform != "qq" {
		t.Errorf("platform = %q，期望 qq", out.Platform)
	}
	if out.AccountID != env.cfg.Bot.AccountID {
		t.Errorf("account_id = %q，期望 %q", out.AccountID, env.cfg.Bot.AccountID)
	}
	// 播种：3 条消息、2 个会话、1 个用户。
	if out.Messages != 3 {
		t.Errorf("messages = %d，期望 3", out.Messages)
	}
	if out.Sessions != 2 {
		t.Errorf("sessions = %d，期望 2", out.Sessions)
	}
	if out.Persons != 1 {
		t.Errorf("persons = %d，期望 1", out.Persons)
	}
	if out.ActiveChats != 3 {
		t.Errorf("active_chats = %d，期望 3（来自 Hub）", out.ActiveChats)
	}
	if out.UptimeSeconds < 0 {
		t.Errorf("uptime_seconds = %d，不应为负", out.UptimeSeconds)
	}

	// 字段名必须一字不差地存在（防前端拿不到 key）。
	var raw map[string]json.RawMessage
	decodeJSON(t, body, &raw)
	for _, key := range []string{
		"nickname", "platform", "account_id", "messages",
		"sessions", "persons", "active_chats", "uptime_seconds",
	} {
		if _, ok := raw[key]; !ok {
			t.Errorf("状态响应缺少字段 %q，实际字段: %v", key, keysOf(raw))
		}
	}
}

// TestStatusWithoutHub 防：Hub 为 nil 时 /api/status 直接 panic（可用性回归）。
func TestStatusWithoutHub(t *testing.T) {
	env := newTestEnv(t, envOptions{}) // 不注入 Hub
	seedData(t, env.db)
	env.loginOK()

	resp, body := env.do(http.MethodGet, "/api/status", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("无 Hub 时状态码 = %d，期望 200，body=%s", resp.StatusCode, body)
	}
	var out map[string]any
	decodeJSON(t, body, &out)
	if got, _ := out["active_chats"].(float64); got != 0 {
		t.Errorf("无 Hub 时 active_chats = %v，期望 0", out["active_chats"])
	}
}

// TestSessionsJSONShape 防：会话列表字段名/排序/回退显示名被改坏。
func TestSessionsJSONShape(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	seedData(t, env.db)
	env.loginOK()

	resp, body := env.do(http.MethodGet, "/api/sessions", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200，body=%s", resp.StatusCode, body)
	}

	type sessionRow struct {
		ID           string `json:"id"`
		Scope        string `json:"scope"`
		TargetID     string `json:"target_id"`
		Name         string `json:"name"`
		MessageCount int    `json:"message_count"`
		LastActive   int64  `json:"last_active"`
	}
	var out struct {
		Sessions []sessionRow `json:"sessions"`
	}
	decodeJSON(t, body, &out)

	if len(out.Sessions) != 2 {
		t.Fatalf("会话数 = %d，期望 2，body=%s", len(out.Sessions), body)
	}

	byID := make(map[string]sessionRow, len(out.Sessions))
	for _, s := range out.Sessions {
		byID[s.ID] = s
	}

	group, ok := byID[groupSessionID]
	if !ok {
		t.Fatalf("缺少群会话 %s，body=%s", groupSessionID, body)
	}
	if group.Scope != domain.ScopeGroup {
		t.Errorf("群会话 scope = %q，期望 group", group.Scope)
	}
	if group.TargetID != groupTargetID {
		t.Errorf("群会话 target_id = %q，期望 %q", group.TargetID, groupTargetID)
	}
	// display_name 为空时的回退规则必须稳定。
	if group.Name != "群 "+groupTargetID {
		t.Errorf("无名群会话 name = %q，期望 %q", group.Name, "群 "+groupTargetID)
	}
	if group.MessageCount != 2 {
		t.Errorf("群会话 message_count = %d，期望 2", group.MessageCount)
	}
	if group.LastActive <= 0 {
		t.Errorf("群会话 last_active = %d，期望正数时间戳", group.LastActive)
	}

	private, ok := byID[privateSessionID]
	if !ok {
		t.Fatalf("缺少私聊会话 %s，body=%s", privateSessionID, body)
	}
	if private.Name != "小明" {
		t.Errorf("私聊会话 name = %q，期望 小明（来自 display_name）", private.Name)
	}

	// 必须按最近活跃倒序（群会话更近）。
	if out.Sessions[0].ID != groupSessionID {
		t.Errorf("首个会话 = %q，期望最近活跃的 %q", out.Sessions[0].ID, groupSessionID)
	}
}

// TestSessionsEmptyIsArrayNotNull 防：无数据时返回 null，前端 forEach 崩溃。
func TestSessionsEmptyIsArrayNotNull(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	env.loginOK()

	_, body := env.do(http.MethodGet, "/api/sessions", "")
	var raw map[string]json.RawMessage
	decodeJSON(t, body, &raw)
	if string(raw["sessions"]) != "[]" {
		t.Errorf("空会话列表 = %s，期望 []", raw["sessions"])
	}
}

// TestSessionsLimit 防：limit 参数失效（一次拉全库）或非法值把查询打成 500。
func TestSessionsLimit(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	seedData(t, env.db)
	env.loginOK()

	var out struct {
		Sessions []json.RawMessage `json:"sessions"`
	}
	_, body := env.do(http.MethodGet, "/api/sessions?limit=1", "")
	decodeJSON(t, body, &out)
	if len(out.Sessions) != 1 {
		t.Fatalf("limit=1 返回 %d 条，期望 1，body=%s", len(out.Sessions), body)
	}

	// 非法 limit 应回退默认值而不是报错。
	for _, q := range []string{"limit=abc", "limit=", "limit=-5", "limit=999999999999999999999"} {
		resp, body := env.do(http.MethodGet, "/api/sessions?"+q, "")
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s 状态码 = %d，期望 200（回退默认 limit），body=%s", q, resp.StatusCode, body)
		}
	}

	// limit=0 / 负数在 repo 层被归一为 100，因此仍应返回全部 2 条。
	_, body = env.do(http.MethodGet, "/api/sessions?limit=0", "")
	decodeJSON(t, body, &out)
	if len(out.Sessions) != 2 {
		t.Errorf("limit=0 返回 %d 条，期望回退后 2 条，body=%s", len(out.Sessions), body)
	}
}

// TestMessagesJSONShape 防：消息字段名/顺序/时间戳被改坏，聊天回放错乱。
func TestMessagesJSONShape(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	seedData(t, env.db)
	env.loginOK()

	resp, body := env.do(http.MethodGet, "/api/messages?session_id="+groupSessionID, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200，body=%s", resp.StatusCode, body)
	}

	type messageRow struct {
		ID        string `json:"id"`
		UserID    string `json:"user_id"`
		UserName  string `json:"user_name"`
		Text      string `json:"text"`
		IsAt      bool   `json:"is_at"`
		CreatedAt int64  `json:"created_at"`
	}
	var out struct {
		Messages []messageRow `json:"messages"`
	}
	decodeJSON(t, body, &out)

	if len(out.Messages) != 2 {
		t.Fatalf("消息数 = %d，期望 2，body=%s", len(out.Messages), body)
	}

	// 必须按时间正序（repo 里做了反向）。
	if out.Messages[0].ID != "platform-msg-0001" {
		t.Errorf("首条消息 id = %q，期望 platform-msg-0001（正序）", out.Messages[0].ID)
	}
	first := out.Messages[0]
	if first.UserID != "user-001" {
		t.Errorf("user_id = %q，期望 user-001", first.UserID)
	}
	if first.UserName != "阿一" {
		t.Errorf("user_name = %q，期望 阿一", first.UserName)
	}
	if first.Text != "第一句" {
		t.Errorf("text = %q，期望 第一句", first.Text)
	}
	if !first.IsAt {
		t.Error("is_at = false，期望 true")
	}
	if first.CreatedAt <= 0 {
		t.Errorf("created_at = %d，期望正数时间戳", first.CreatedAt)
	}

	second := out.Messages[1]
	if second.IsAt {
		t.Error("第二条消息 is_at = true，期望 false")
	}

	var raw map[string]json.RawMessage
	decodeJSON(t, body, &raw)
	if _, ok := raw["messages"]; !ok {
		t.Fatalf("响应缺少 messages 字段: %s", body)
	}
}

// TestMessagesRequiresSessionID 防：缺 session_id 时去查空字符串，返回全库 0 条而不是报错。
func TestMessagesRequiresSessionID(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	seedData(t, env.db)
	env.loginOK()

	resp, body := env.do(http.MethodGet, "/api/messages", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("缺少 session_id 状态码 = %d，期望 400，body=%s", resp.StatusCode, body)
	}
	var out map[string]any
	decodeJSON(t, body, &out)
	if out["error"] == nil {
		t.Errorf("400 响应缺少 error 字段: %s", body)
	}
}

// TestMessagesUnknownSessionIsEmptyArray 防：不存在的会话导致 500 或 null。
func TestMessagesUnknownSessionIsEmptyArray(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	seedData(t, env.db)
	env.loginOK()

	resp, body := env.do(http.MethodGet, "/api/messages?session_id=does-not-exist", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("未知会话状态码 = %d，期望 200，body=%s", resp.StatusCode, body)
	}
	var raw map[string]json.RawMessage
	decodeJSON(t, body, &raw)
	if string(raw["messages"]) != "[]" {
		t.Errorf("未知会话 messages = %s，期望 []", raw["messages"])
	}
}

// TestConfigDoesNotLeakSecrets 安全回归：
// 防：有人把 app_secret / api_key / password / session_secret 塞进配置接口。
func TestConfigDoesNotLeakSecrets(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	env.loginOK()

	resp, body := env.do(http.MethodGet, "/api/config", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200，body=%s", resp.StatusCode, body)
	}

	lower := strings.ToLower(string(body))
	for _, needle := range []string{
		"app_secret",
		"api_key",
		"password",
		"session_secret",
	} {
		if strings.Contains(lower, needle) {
			t.Errorf("配置响应里出现敏感字段名 %q，响应体: %s", needle, body)
		}
	}
	// 真正的密钥值也不能出现（配置里塞了明显的哨兵值）。
	for _, secret := range []string{
		testPassword,
		testSessionSecret,
		env.cfg.QQ.AppSecret,
		"sk-llm-api-key-DO-NOT-LEAK",
	} {
		if strings.Contains(string(body), secret) {
			t.Errorf("配置响应里出现密钥值 %q，响应体: %s", secret, body)
		}
	}

	// 正向：白名单字段必须还在，避免「脱敏脱过头」。
	for _, key := range []string{"bot", "personality", "chat", "models", "nickname", "identifier"} {
		if !strings.Contains(string(body), key) {
			t.Errorf("配置响应缺少字段 %q，响应体: %s", key, body)
		}
	}
}

// TestConfigModelSummaryShape 防：模型列表把 base_url / 价格等配置带出去，或字段漂移。
func TestConfigModelSummaryShape(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	env.loginOK()

	_, body := env.do(http.MethodGet, "/api/config", "")

	var out struct {
		Bot struct {
			Nickname   string   `json:"nickname"`
			AliasNames []string `json:"alias_names"`
		} `json:"bot"`
		Chat struct {
			MaxContextSize int     `json:"max_context_size"`
			TalkValue      float64 `json:"talk_value"`
		} `json:"chat"`
		Models []map[string]any `json:"models"`
	}
	decodeJSON(t, body, &out)

	if out.Bot.Nickname != env.cfg.Bot.Nickname {
		t.Errorf("bot.nickname = %q，期望 %q", out.Bot.Nickname, env.cfg.Bot.Nickname)
	}
	if len(out.Bot.AliasNames) != 2 {
		t.Errorf("bot.alias_names = %v，期望 2 个", out.Bot.AliasNames)
	}
	if out.Chat.MaxContextSize != env.cfg.Chat.MaxContextSize {
		t.Errorf("chat.max_context_size = %d，期望 %d", out.Chat.MaxContextSize, env.cfg.Chat.MaxContextSize)
	}
	if len(out.Models) != 1 {
		t.Fatalf("models 长度 = %d，期望 1，body=%s", len(out.Models), body)
	}
	model := out.Models[0]
	if model["name"] != "test-model" {
		t.Errorf("models[0].name = %v，期望 test-model", model["name"])
	}
	if model["identifier"] != "model-identifier-1" {
		t.Errorf("models[0].identifier = %v，期望 model-identifier-1", model["identifier"])
	}
	if model["provider"] != "test-provider" {
		t.Errorf("models[0].provider = %v，期望 test-provider", model["provider"])
	}
	for _, forbidden := range []string{"api_key", "base_url", "price_in", "price_out"} {
		if _, ok := model[forbidden]; ok {
			t.Errorf("models[0] 泄漏字段 %q: %v", forbidden, model)
		}
	}
}

// TestUsageSummaryFiltersByDays 防：days 参数失效导致把全部历史用量算进今日。
func TestUsageSummaryFiltersByDays(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	seedData(t, env.db)
	env.loginOK()

	resp, body := env.do(http.MethodGet, "/api/usage?days=1", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200，body=%s", resp.StatusCode, body)
	}

	var out struct {
		Summary struct {
			Calls            int64   `json:"calls"`
			PromptTokens     int64   `json:"prompt_tokens"`
			CompletionTokens int64   `json:"completion_tokens"`
			TotalTokens      int64   `json:"total_tokens"`
			Cost             float64 `json:"cost"`
		} `json:"summary"`
		ByTask []struct {
			Task        string  `json:"task"`
			Calls       int64   `json:"calls"`
			TotalTokens int64   `json:"total_tokens"`
			Cost        float64 `json:"cost"`
		} `json:"by_task"`
	}
	decodeJSON(t, body, &out)

	// 10 天前那条 999 token 的记录必须被 days=1 过滤掉。
	if out.Summary.Calls != 2 {
		t.Errorf("summary.calls = %d，期望 2", out.Summary.Calls)
	}
	if out.Summary.TotalTokens != 25 {
		t.Errorf("summary.total_tokens = %d，期望 25", out.Summary.TotalTokens)
	}
	if out.Summary.PromptTokens != 17 {
		t.Errorf("summary.prompt_tokens = %d，期望 17", out.Summary.PromptTokens)
	}
	if out.Summary.CompletionTokens != 8 {
		t.Errorf("summary.completion_tokens = %d，期望 8", out.Summary.CompletionTokens)
	}
	if out.Summary.Cost < 0.74 || out.Summary.Cost > 0.76 {
		t.Errorf("summary.cost = %v，期望约 0.75", out.Summary.Cost)
	}

	if len(out.ByTask) != 2 {
		t.Fatalf("by_task 长度 = %d，期望 2，body=%s", len(out.ByTask), body)
	}
	// 按 total_tokens 倒序：planner 15 > replyer 10。
	if out.ByTask[0].Task != "planner" || out.ByTask[0].TotalTokens != 15 {
		t.Errorf("by_task[0] = %+v，期望 planner/15", out.ByTask[0])
	}
	if out.ByTask[1].Task != "replyer" {
		t.Errorf("by_task[1].task = %q，期望 replyer", out.ByTask[1].Task)
	}
}

// TestUsageParameterBoundaries 防：days 非法/负数导致 500 或空统计。
func TestUsageParameterBoundaries(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	seedData(t, env.db)
	env.loginOK()

	for _, q := range []string{"", "?days=abc", "?days=", "?days=0", "?days=-3"} {
		resp, body := env.do(http.MethodGet, "/api/usage"+q, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("days 参数 %q 状态码 = %d，期望 200，body=%s", q, resp.StatusCode, body)
		}
		var out map[string]json.RawMessage
		decodeJSON(t, body, &out)
		if _, ok := out["summary"]; !ok {
			t.Errorf("days 参数 %q 响应缺少 summary: %s", q, body)
		}
		if _, ok := out["by_task"]; !ok {
			t.Errorf("days 参数 %q 响应缺少 by_task: %s", q, body)
		}
	}
}

// TestUsageEmptyByTaskIsArrayNotNull 防：无用量时 by_task 返回 null，前端 forEach 崩。
//
// ByTask 在无数据时返回 nil 切片，JSON 编码结果是 null。
// handler 里显式换成空切片，与 sessions / messages 的口径保持一致。
func TestUsageEmptyByTaskIsArrayNotNull(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	env.loginOK()

	_, body := env.do(http.MethodGet, "/api/usage?days=1", "")
	var raw map[string]json.RawMessage
	decodeJSON(t, body, &raw)

	got := string(raw["by_task"])
	if got != "[]" {
		t.Errorf("by_task = %s，期望 []（null 会让前端 forEach 抛错）", got)
	}
}

// TestQueryInt 防：queryInt 的默认值/非法值回退被改坏（这是多个接口的共同依赖）。
func TestQueryInt(t *testing.T) {
	cases := []struct {
		Query string
		Def   int
		Want  int
	}{
		{Query: "", Def: 100, Want: 100},
		{Query: "?limit=5", Def: 100, Want: 5},
		{Query: "?limit=abc", Def: 100, Want: 100},
		{Query: "?limit=", Def: 100, Want: 100},
		{Query: "?limit=-1", Def: 100, Want: -1},
		{Query: "?limit=0", Def: 100, Want: 0},
		{Query: "?limit=1.5", Def: 100, Want: 100},
		{Query: "?limit=99999999999999999999", Def: 100, Want: 100},
	}
	for _, c := range cases {
		req, err := http.NewRequest(http.MethodGet, "/api/sessions"+c.Query, nil)
		if err != nil {
			t.Fatalf("构造请求失败: %v", err)
		}
		if got := queryInt(req, "limit", c.Def); got != c.Want {
			t.Errorf("queryInt(%q, %d) = %d，期望 %d", c.Query, c.Def, got, c.Want)
		}
	}
}

// TestRoutesProtectEverythingButLogin 防：注册路由时漏套 requireAuth。
// 直接对 s.routes() 做一次「无 Cookie 全端点探测」，不依赖列表是否最新。
func TestRoutesProtectEverythingButLogin(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	seedData(t, env.db)

	paths := []string{
		"/api/status", "/api/sessions", "/api/messages?session_id=" + groupSessionID,
		"/api/chat/session", "/api/chat/send", "/api/config", "/api/usage", "/api/logs",
		"/api/events",
	}
	for _, p := range paths {
		method := http.MethodGet
		if p == "/api/chat/send" {
			method = http.MethodPost
		}
		req, err := http.NewRequest(method, env.ts.URL+p, strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("构造请求失败: %v", err)
		}
		// 独立 client：绝不带 Cookie。
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("请求 %s 失败: %v", p, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s 匿名状态码 = %d，期望 401", p, resp.StatusCode)
		}
	}

	// /api/logout 不要求登录（重复登出应当幂等）。
	resp, body := env.do(http.MethodPost, "/api/logout", "")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("匿名 /api/logout = %d，期望 200，body=%s", resp.StatusCode, body)
	}
}

// TestStoreFailureReturns500 防：数据库不可用时报 200 + 空数据，掩盖后端故障。
//
// 通过关闭 DB 连接模拟故障；测试结束后 cleanup 里再 Close 一次是安全的（返回错误忽略）。
func TestStoreFailureReturns500(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	env.loginOK()

	if err := env.db.Close(); err != nil {
		t.Fatalf("关闭测试数据库失败: %v", err)
	}

	// 全部端点都必须报 500。
	// 注意 /api/chat/send 不在此列：它不查库（只做 Dispatch，
	// 落库失败由 Hub 记日志），断库时它应该照常工作。
	for _, ep := range []struct {
		Path string
		Body string
	}{
		{Path: "/api/status"},
		{Path: "/api/sessions"},
		{Path: "/api/messages?session_id=" + groupSessionID},
		{Path: "/api/usage?days=1"},
		{Path: "/api/messages?session_id=" + groupSessionID},
	} {
		method := http.MethodGet
		if ep.Body != "" {
			method = http.MethodPost
		}
		resp, body := env.do(method, ep.Path, ep.Body)
		if resp.StatusCode != http.StatusInternalServerError {
			t.Errorf("%s 断库状态码 = %d，期望 500，body=%s", ep.Path, resp.StatusCode, body)
		}
	}
}

// TestWriteJSONHeaders 防：JSON 响应缺少 Content-Type，浏览器按文本解析。
func TestWriteJSONHeaders(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	env.loginOK()

	resp, _ := env.do(http.MethodGet, "/api/config", "")
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q，期望 application/json", ct)
	}
}

// 编译期确认 fakeHub 满足 Hub。
var _ Hub = (*fakeHub)(nil)

// keysOf 取出 map 的键，用于失败信息。
func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
