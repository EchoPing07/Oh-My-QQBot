package web

// 测试公共设施：临时 SQLite、httptest 服务器、假 Hub / Sender、数据播种。
//
// 注意：这里只做「测试夹具」，不碰任何生产代码。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/prompt"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/store"
)

// 测试用的固定凭据，保证测试不依赖 config.Default() 里的默认密码。
const (
	testPassword      = "test-password-8f3a"
	testSessionSecret = "test-session-secret-c1d2"
)

// 播种数据使用的固定 ID，便于断言。
const (
	groupSessionID   = "sess-group-0001"
	privateSessionID = "sess-private-0001"
	groupTargetID    = "g-10001"
	privateTargetID  = "u-20002"
)

// testConfig 构造测试配置。
//
// 显式设置 Web.Password / Web.SessionSecret，避免测试跑在「空密码」上；
// 同时塞入一批假密钥，供 handleConfig 的脱敏回归测试使用。
func testConfig() *config.Config {
	cfg := config.Default()

	cfg.Bot.Nickname = "木木测试"
	cfg.Bot.Platform = "qq"
	cfg.Bot.AccountID = "bot-openid-001"
	cfg.Bot.AliasNames = []string{"木木", "小木"}

	cfg.Web.Enabled = true
	cfg.Web.Host = "127.0.0.1"
	cfg.Web.Port = 0
	cfg.Web.Password = testPassword
	cfg.Web.SessionSecret = testSessionSecret

	// 这些值绝不能出现在 /api/config 的响应里。
	cfg.QQ.AppSecret = "qq-app-secret-DO-NOT-LEAK"
	cfg.LLM.Providers = []config.ProviderConfig{
		{
			Name:    "test-provider",
			BaseURL: "https://example.invalid/v1",
			APIKey:  "sk-llm-api-key-DO-NOT-LEAK",
		},
	}
	cfg.LLM.Models = []config.ModelConfig{
		{Name: "test-model", ModelIdentifier: "model-identifier-1", Provider: "test-provider"},
	}
	return cfg
}

// fakeHub 是 Hub 的测试替身。
//
// dispatched 记录 handleChatSend 投进来的消息，供 /api/chat/send 的
// 测试断言「消息真的进了对话窗口、字段构造正确」。
type fakeHub struct {
	n          int
	mu         sync.Mutex
	dispatched []*domain.Message
	scenes     []string
}

func (h *fakeHub) ActiveCount() int { return h.n }

func (h *fakeHub) Dispatch(msg *domain.Message, scene, targetID, accountID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dispatched = append(h.dispatched, msg)
	h.scenes = append(h.scenes, scene+"|"+targetID+"|"+accountID)
}

func (h *fakeHub) snapshotDispatched() ([]*domain.Message, []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	msgs := make([]*domain.Message, len(h.dispatched))
	copy(msgs, h.dispatched)
	scenes := make([]string, len(h.scenes))
	copy(scenes, h.scenes)
	return msgs, scenes
}

// envOptions 控制测试环境的可选依赖。
type envOptions struct {
	cfg *config.Config
	hub Hub
	// promptsDir 是提示词覆盖目录。为空时用内存里的内嵌模板
	// （此时保存会失败，因为无处可写 —— 这与生产里「未配置目录」一致）。
	promptsDir string
}

// testEnv 是一套完整的 HTTP 测试环境。
//
// 不走 Server.Start，只用 httptest + s.routes()，因此不会占用真实端口。
type testEnv struct {
	t      *testing.T
	s      *Server
	cfg    *config.Config
	db     *store.DB
	ts     *httptest.Server
	client *http.Client
	token  string
}

// newTestEnv 创建测试环境：临时目录里的 SQLite + httptest 服务器 + Cookie Jar。
func newTestEnv(t *testing.T, opt envOptions) *testEnv {
	t.Helper()

	cfg := opt.cfg
	if cfg == nil {
		cfg = testConfig()
	}

	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "omq-test.db"))
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var pm *prompt.Manager
	var err2 error
	if opt.promptsDir != "" {
		pm, err2 = prompt.New(opt.promptsDir, "zh-CN")
		if err2 != nil {
			t.Fatalf("加载提示词失败: %v", err2)
		}
	} else {
		pm, err2 = prompt.New("", "zh-CN")
		if err2 != nil {
			t.Fatalf("加载提示词失败: %v", err2)
		}
	}

	s := NewServer(Options{
		Config: config.Wrap(cfg), Store: db, Hub: opt.hub,
		Prompts: pm,
	})
	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("创建 Cookie Jar 失败: %v", err)
	}
	client := ts.Client()
	client.Jar = jar

	return &testEnv{t: t, s: s, cfg: cfg, db: db, ts: ts, client: client}
}

// do 通过真实 HTTP 发请求（带 Cookie Jar）。
func (e *testEnv) do(method, path, body string) (*http.Response, []byte) {
	e.t.Helper()

	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, e.ts.URL+path, rdr)
	if err != nil {
		e.t.Fatalf("构造请求 %s %s 失败: %v", method, path, err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatalf("请求 %s %s 失败: %v", method, path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatalf("读取 %s %s 响应失败: %v", method, path, err)
	}
	return resp, raw
}

// doWithCookie 用「手工塞 Cookie」的方式发请求，不经过 Cookie Jar。
// 用于伪造/过期 Cookie 的负向测试。
func (e *testEnv) doWithCookie(method, path, body, cookieValue string) (*http.Response, []byte) {
	e.t.Helper()

	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, e.ts.URL+path, rdr)
	if err != nil {
		e.t.Fatalf("构造请求 %s %s 失败: %v", method, path, err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookieValue != "" {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: cookieValue})
	}

	// 独立 client：不带 Jar，避免登录态互相污染。
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		e.t.Fatalf("请求 %s %s 失败: %v", method, path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatalf("读取 %s %s 响应失败: %v", method, path, err)
	}
	return resp, raw
}

// loginOK 用正确密码登录，返回 Cookie 值（同时写入 Jar 与 e.token）。
func (e *testEnv) loginOK() string {
	e.t.Helper()

	payload, err := json.Marshal(map[string]string{"password": testPassword})
	if err != nil {
		e.t.Fatalf("编码登录请求失败: %v", err)
	}
	resp, body := e.do(http.MethodPost, "/api/login", string(payload))
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("登录状态码 = %d，期望 200，body=%s", resp.StatusCode, body)
	}
	for _, c := range resp.Cookies() {
		if c.Name == cookieName {
			if c.Value == "" {
				e.t.Fatal("登录下发的 Cookie 值为空")
			}
			e.token = c.Value
			return c.Value
		}
	}
	e.t.Fatalf("登录响应缺少 %s Cookie，Set-Cookie=%v", cookieName, resp.Header.Values("Set-Cookie"))
	return ""
}

// decodeJSON 把响应体解到 v，失败直接终止测试。
func decodeJSON(t *testing.T, raw []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("解析 JSON 失败: %v，body=%s", err, raw)
	}
}

// seedData 向临时数据库预置固定数据：
//   - 2 个会话（一个群聊无 display_name，一个私聊有 display_name）
//   - 3 条消息（群聊 2 条 + 私聊 1 条）
//   - 1 个用户
//   - 3 条用量记录（2 条昨天以内 + 1 条 10 天前，用于验证 days 过滤）
func seedData(t *testing.T, db *store.DB) {
	t.Helper()

	ctx := context.Background()
	now := time.Now().Truncate(time.Second)

	sessions := []*domain.Session{
		{
			ID:           groupSessionID,
			Platform:     "qq",
			AccountID:    "bot-openid-001",
			Scope:        domain.ScopeGroup,
			TargetID:     groupTargetID,
			DisplayName:  "",
			MessageCount: 2,
			LastActiveAt: now.Add(-1 * time.Minute),
			CreatedAt:    now.Add(-1 * time.Hour),
		},
		{
			ID:           privateSessionID,
			Platform:     "qq",
			AccountID:    "bot-openid-001",
			Scope:        domain.ScopePrivate,
			TargetID:     privateTargetID,
			DisplayName:  "小明",
			MessageCount: 1,
			LastActiveAt: now.Add(-2 * time.Minute),
			CreatedAt:    now.Add(-2 * time.Hour),
		},
	}
	for _, sess := range sessions {
		if err := db.Sessions().Upsert(ctx, sess); err != nil {
			t.Fatalf("写入会话 %s 失败: %v", sess.ID, err)
		}
	}

	msgs := []*domain.Message{
		{
			ID:         "msg-0001",
			SessionID:  groupSessionID,
			MessageID:  "platform-msg-0001",
			Platform:   "qq",
			UserID:     "user-001",
			UserName:   "阿一",
			GroupID:    groupTargetID,
			IsGroup:    true,
			IsAt:       true,
			PlainText:  "第一句",
			Segments:   []domain.Segment{domain.Text("第一句")},
			ReceivedAt: now.Add(-3 * time.Minute),
		},
		{
			ID:         "msg-0002",
			SessionID:  groupSessionID,
			MessageID:  "platform-msg-0002",
			Platform:   "qq",
			UserID:     "user-002",
			UserName:   "阿二",
			GroupID:    groupTargetID,
			IsGroup:    true,
			PlainText:  "第二句",
			Segments:   []domain.Segment{domain.Text("第二句")},
			ReceivedAt: now.Add(-2 * time.Minute),
		},
		{
			ID:         "msg-0003",
			SessionID:  privateSessionID,
			MessageID:  "platform-msg-0003",
			Platform:   "qq",
			UserID:     privateTargetID,
			UserName:   "小明",
			PlainText:  "私聊一句",
			Segments:   []domain.Segment{domain.Text("私聊一句")},
			ReceivedAt: now.Add(-1 * time.Minute),
		},
	}
	for _, m := range msgs {
		if err := db.Messages().Save(ctx, m); err != nil {
			t.Fatalf("写入消息 %s 失败: %v", m.ID, err)
		}
	}

	if err := db.Persons().Upsert(ctx, &domain.Person{
		Platform:   "qq",
		UserID:     "user-001",
		Nickname:   "阿一",
		FirstSeen:  now.Add(-24 * time.Hour),
		LastSeenAt: now.Add(-3 * time.Minute),
	}); err != nil {
		t.Fatalf("写入用户失败: %v", err)
	}

	usage := []*store.UsageRecord{
		{
			Task: "planner", Model: "test-model", SessionID: groupSessionID,
			PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15,
			Cost: 0.5, DurationMS: 120, CreatedAt: now.Add(-1 * time.Hour),
		},
		{
			Task: "replyer", Model: "test-model", SessionID: groupSessionID,
			PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10,
			Cost: 0.25, DurationMS: 80, CreatedAt: now.Add(-2 * time.Hour),
		},
		{
			// 超出 days=1 窗口，必须被过滤掉。
			Task: "planner", Model: "test-model", SessionID: groupSessionID,
			PromptTokens: 900, CompletionTokens: 99, TotalTokens: 999,
			Cost: 9.99, DurationMS: 1000, CreatedAt: now.AddDate(0, 0, -10),
		},
	}
	for _, u := range usage {
		if err := db.Usage().Record(ctx, u); err != nil {
			t.Fatalf("写入用量记录失败: %v", err)
		}
	}
}
