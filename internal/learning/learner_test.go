package learning

// 本文件只覆盖三件最容易静默坏掉的事：
//  1. 模型返回的 JSON 不一定干净（围栏、废话、空数组、非法 JSON）；
//  2. 两个开关都关时绝不能发起模型调用（否则后台任务会偷偷烧钱）；
//  3. 学到的表达必须真的落库，而不只是日志里好看。
// 用标准库 testing + httptest 假服务，不引入 testify，也不连真实模型。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/llm"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/store"
)

// TestParseExpressions 覆盖模型回应最常见的五种形态。
func TestParseExpressions(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int
	}{
		{
			name:  "正常 JSON",
			input: `[{"situation":"表示惊叹","style":"使用 我嘞个xxxx","source_id":"3"}]`,
			want:  1,
		},
		{
			name: "带 json 围栏",
			input: "```json\n" +
				`[{"situation":"表示讽刺的赞同","style":"使用 对对对","source_id":"4"}]` +
				"\n```",
			want: 1,
		},
		{
			name: "前后有废话",
			input: "好的，结果如下：\n" +
				`[{"situation":"游戏相关夸赞","style":"这么强！","source_id":"5"}]` +
				"\n希望有帮助。",
			want: 1,
		},
		{
			name:  "空数组是合法的",
			input: `[]`,
			want:  0,
		},
		{
			name:  "非法 JSON 必须报错",
			input: `[{"situation": "缺引号, "style": }]`,
			want:  -1, // -1 表示期望返回错误
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseExpressions(tc.input)
			if tc.want < 0 {
				if err == nil {
					t.Fatalf("期望返回错误，实际得到 %d 条", len(got))
				}
				// 错误必须带上下文，不能是裸的 json 错误。
				if !strings.Contains(err.Error(), "解析表达方式失败") {
					t.Errorf("错误缺少上下文: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if len(got) != tc.want {
				t.Fatalf("条数 = %d，期望 %d", len(got), tc.want)
			}
			if tc.want > 0 {
				if got[0].Situation == "" || got[0].Style == "" {
					t.Errorf("字段为空: %+v", got[0])
				}
				// 提示词示例里的「使用」前缀要被清掉，否则注入给 Replyer 会读成「使用 使用 ...」。
				if strings.HasPrefix(got[0].Style, "使用") {
					t.Errorf("style 未去掉「使用」前缀: %q", got[0].Style)
				}
			}
		})
	}
}

// TestParseJargons 与表达解析同构，但字段是 content/source_id。
func TestParseJargons(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int
	}{
		{
			name:  "正常 JSON",
			input: `[{"content":"yyds","source_id":"12"}]`,
			want:  1,
		},
		{
			name: "带 json 围栏",
			input: "```json\n" +
				`[{"content":"社死","source_id":"7"},{"content":"内卷","source_id":"8"}]` +
				"\n```",
			want: 2,
		},
		{
			name: "前后有废话",
			input: "提取结果：\n" +
				`[{"content":"xswl","source_id":"5"}]` +
				"\n以上。",
			want: 1,
		},
		{
			name:  "空数组是合法的",
			input: `[]`,
			want:  0,
		},
		{
			name:  "非法 JSON 必须报错",
			input: `[{"content": "yyds" "source_id": "1"}]`,
			want:  -1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseJargons(tc.input)
			if tc.want < 0 {
				if err == nil {
					t.Fatalf("期望返回错误，实际得到 %d 条", len(got))
				}
				if !strings.Contains(err.Error(), "解析黑话失败") {
					t.Errorf("错误缺少上下文: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if len(got) != tc.want {
				t.Fatalf("条数 = %d，期望 %d", len(got), tc.want)
			}
			if tc.want > 0 && got[0].Content == "" {
				t.Errorf("content 为空: %+v", got[0])
			}
		})
	}
}

// TestRunOnceSkipsWhenDisabled 防的是：开关都关了，后台任务仍然每秒发一次模型请求。
//
// 两层验证：既用 nil 依赖证明 RunOnce 在依赖检查之前就返回，
// 又用一个计数器假服务证明真的没有任何 HTTP 请求发出。
func TestRunOnceSkipsWhenDisabled(t *testing.T) {
	cfg := config.Default()
	cfg.Learning.ExpressionEnabled = false
	cfg.Learning.JargonEnabled = false

	// 依赖全为 nil：如果实现先做依赖检查而不是先看开关，这里会返回错误。
	l := New(Options{Config: config.Wrap(cfg)})
	expressions, jargons, err := l.RunOnce(t.Context())
	if err != nil {
		t.Fatalf("开关全关时不应返回错误: %v", err)
	}
	if expressions != 0 || jargons != 0 {
		t.Fatalf("开关全关时返回 (%d, %d)，期望 (0, 0)", expressions, jargons)
	}

	// 再确认一遍：即使依赖齐全，也不该碰到网络。
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"[]"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	db := openTestDB(t)
	seedSession(t, db, "sess-1")

	cfg2 := testConfig(srv.URL)
	cfg2.Learning.ExpressionEnabled = false
	cfg2.Learning.JargonEnabled = false

	l2 := New(Options{Config: config.Wrap(cfg2), LLM: llm.New(config.Wrap(cfg2)), Store: db})
	if _, _, err := l2.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce 失败: %v", err)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("开关全关时仍然发起了 %d 次模型调用", n)
	}
}

// TestRunOncePersistsExpressions 是最关键的一条：
// 假服务返回固定 JSON，验证表达真的写进了 expressions 表。
func TestRunOncePersistsExpressions(t *testing.T) {
	const expressionJSON = `[{"situation":"对某件事表示十分惊叹","style":"使用 我嘞个xxxx","source_id":"1"}]`

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)

		// 校验请求里真的带上了提示词，否则「学到的表达」可能只是空跑。
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("请求体不是合法 JSON: %v", err)
		}
		if len(req.Messages) == 0 || !strings.Contains(req.Messages[0].Content, "请从上面这段群聊中提取用户的语言风格和说话方式") {
			t.Errorf("请求里没有表达学习提示词: %s", string(body))
		}

		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"model": "test-model",
			"choices": []any{map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": expressionJSON},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	db := openTestDB(t)
	const sessionID = "sess-1"
	seedSession(t, db, sessionID)
	seedUserMessages(t, db, sessionID, 5)

	cfg := testConfig(srv.URL)
	cfg.Learning.ExpressionEnabled = true
	cfg.Learning.JargonEnabled = false
	cfg.Learning.MinMessages = 3

	l := New(Options{Config: config.Wrap(cfg), LLM: llm.New(config.Wrap(cfg)), Store: db})
	expressions, jargons, err := l.RunOnce(t.Context())
	if err != nil {
		t.Fatalf("RunOnce 失败: %v", err)
	}
	if expressions != 1 {
		t.Fatalf("学到表达 %d 条，期望 1 条", expressions)
	}
	if jargons != 0 {
		t.Errorf("黑话开关关闭时不该写入黑话，实际 %d 条", jargons)
	}
	if n := calls.Load(); n == 0 {
		t.Fatal("没有发起模型调用")
	}

	// 落库校验：直接用仓库读回来。
	saved, err := db.Memory().ListExpressions(t.Context(), sessionID, 10)
	if err != nil {
		t.Fatalf("读取表达失败: %v", err)
	}
	if len(saved) != 1 {
		t.Fatalf("库里有 %d 条表达，期望 1 条", len(saved))
	}
	if saved[0].Situation != "对某件事表示十分惊叹" {
		t.Errorf("situation = %q", saved[0].Situation)
	}
	// 「使用」前缀必须清掉，否则回填给 Replyer 会读成「使用 使用 我嘞个xxxx」。
	if saved[0].Style != "我嘞个xxxx" {
		t.Errorf("style = %q，期望 %q", saved[0].Style, "我嘞个xxxx")
	}
	if saved[0].SessionID != sessionID {
		t.Errorf("session_id = %q，期望 %q", saved[0].SessionID, sessionID)
	}
	if !strings.Contains(saved[0].SourceText, "用户") {
		t.Errorf("source_text 没回查到原文: %q", saved[0].SourceText)
	}

	// 游标必须推进，否则下一轮会把同一批消息再学一遍。
	raw, err := db.KV().Get(t.Context(), cursorKeyPrefix+sessionID)
	if err != nil {
		t.Fatalf("读取游标失败: %v", err)
	}
	if raw == "" {
		t.Fatal("学习游标没有写入")
	}
}

// TestRunOnceSkipsBotAndNonTextMessages 防的是把机器人自己的话学进去
// （learn_style.prompt 第 2 条明确禁止），以及把图片占位符当成语言风格。
func TestRunOnceSkipsBotAndNonTextMessages(t *testing.T) {
	const expressionJSON = `[{"situation":"表示赞同","style":"对对对","source_id":"1"}]`

	var lastPrompt atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		if len(req.Messages) > 0 {
			lastPrompt.Store(req.Messages[0].Content)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": expressionJSON},
				"finish_reason": "stop",
			}},
		})
	}))
	defer srv.Close()

	db := openTestDB(t)
	const sessionID = "sess-mixed"
	seedSession(t, db, sessionID)

	const botID = "bot-openid"
	now := time.Now()
	msgs := []*domain.Message{
		{
			ID: "m1", SessionID: sessionID, UserID: "u1", UserName: "小明",
			Segments:   []domain.Segment{domain.Text("这游戏太强了")},
			ReceivedAt: now,
		},
		{
			// 机器人自己的发言，绝不能学到。
			ID: "m2", SessionID: sessionID, UserID: botID, UserName: "木木",
			Segments:   []domain.Segment{domain.Text("这是机器人说的话")},
			ReceivedAt: now.Add(time.Second),
		},
		{
			// 纯图片消息，没有文字。
			ID: "m3", SessionID: sessionID, UserID: "u2", UserName: "小红",
			Segments:   []domain.Segment{domain.Image("https://example.com/a.png")},
			ReceivedAt: now.Add(2 * time.Second),
		},
		{
			ID: "m4", SessionID: sessionID, UserID: "u3", UserName: "小刚",
			Segments:   []domain.Segment{domain.Text("确实")},
			ReceivedAt: now.Add(3 * time.Second),
		},
	}
	for _, m := range msgs {
		if err := db.Messages().Save(t.Context(), m); err != nil {
			t.Fatalf("写入消息 %s 失败: %v", m.ID, err)
		}
	}

	cfg := testConfig(srv.URL)
	cfg.Bot.AccountID = botID
	cfg.Learning.ExpressionEnabled = true
	cfg.Learning.JargonEnabled = false
	cfg.Learning.MinMessages = 2

	l := New(Options{Config: config.Wrap(cfg), LLM: llm.New(config.Wrap(cfg)), Store: db})
	expressions, _, err := l.RunOnce(t.Context())
	if err != nil {
		t.Fatalf("RunOnce 失败: %v", err)
	}
	if expressions != 1 {
		t.Fatalf("学到表达 %d 条，期望 1 条", expressions)
	}

	prompt, _ := lastPrompt.Load().(string)
	if prompt == "" {
		t.Fatal("没有捕获到提示词")
	}
	if strings.Contains(prompt, "这是机器人说的话") {
		t.Errorf("提示词里混进了机器人自己的发言:\n%s", prompt)
	}
	if strings.Contains(prompt, "[图片]") || strings.Contains(prompt, "a.png") {
		t.Errorf("提示词里混进了图片内容:\n%s", prompt)
	}
	// 一行一条，且 source_id 连续编号。
	if !strings.Contains(prompt, "[source_id:1] 小明: 这游戏太强了") {
		t.Errorf("聊天记录格式不符合预期:\n%s", prompt)
	}
	if !strings.Contains(prompt, "[source_id:2] 小刚: 确实") {
		t.Errorf("过滤后的消息没有重新连续编号:\n%s", prompt)
	}
}

// TestRunOnceDoesNotRelearnSameBatch 防的是最贵的 bug：
// 每轮都把同一批消息再学一遍，模型调用量变成「间隔次数」而不是「新消息数」。
func TestRunOnceDoesNotRelearnSameBatch(t *testing.T) {
	const expressionJSON = `[{"situation":"表示赞同","style":"对对对","source_id":"1"}]`

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": expressionJSON},
				"finish_reason": "stop",
			}},
		})
	}))
	defer srv.Close()

	db := openTestDB(t)
	const sessionID = "sess-1"
	seedSession(t, db, sessionID)
	seedUserMessages(t, db, sessionID, 4)

	cfg := testConfig(srv.URL)
	cfg.Learning.ExpressionEnabled = true
	cfg.Learning.MinMessages = 3

	l := New(Options{Config: config.Wrap(cfg), LLM: llm.New(config.Wrap(cfg)), Store: db})

	if _, _, err := l.RunOnce(t.Context()); err != nil {
		t.Fatalf("第一轮学习失败: %v", err)
	}
	firstRound := calls.Load()
	if firstRound == 0 {
		t.Fatal("第一轮没有发起模型调用")
	}

	// 没有新消息，第二轮应该完全跳过。
	if _, _, err := l.RunOnce(t.Context()); err != nil {
		t.Fatalf("第二轮学习失败: %v", err)
	}
	if got := calls.Load(); got != firstRound {
		t.Fatalf("第二轮又调了模型（累计 %d 次，第一轮 %d 次），游标没起作用", got, firstRound)
	}
}

// TestRunOncePersistsJargons 验证黑话链路：走了黑话提示词、写进了 jargons 表。
// 与表达是两条独立链路，只测其中一条等于没测黑话。
func TestRunOncePersistsJargons(t *testing.T) {
	const jargonJSON = `[{"content":"yyds","source_id":"1"},{"content":"社死","source_id":"2"}]`

	var sawJargonPrompt atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		if len(req.Messages) > 0 && strings.Contains(req.Messages[0].Content, "可能是黑话") {
			sawJargonPrompt.Store(true)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": jargonJSON},
				"finish_reason": "stop",
			}},
		})
	}))
	defer srv.Close()

	db := openTestDB(t)
	const sessionID = "sess-jargon"
	seedSession(t, db, sessionID)
	seedUserMessages(t, db, sessionID, 4)

	cfg := testConfig(srv.URL)
	cfg.Learning.ExpressionEnabled = false
	cfg.Learning.JargonEnabled = true
	cfg.Learning.MinMessages = 3

	l := New(Options{Config: config.Wrap(cfg), LLM: llm.New(config.Wrap(cfg)), Store: db})
	_, jargons, err := l.RunOnce(t.Context())
	if err != nil {
		t.Fatalf("RunOnce 失败: %v", err)
	}
	if jargons != 2 {
		t.Fatalf("学到黑话 %d 条，期望 2 条", jargons)
	}
	if !sawJargonPrompt.Load() {
		t.Error("请求里没有黑话提示词")
	}

	saved, err := db.Memory().ListJargons(t.Context(), 10)
	if err != nil {
		t.Fatalf("读取黑话失败: %v", err)
	}
	if len(saved) != 2 {
		t.Fatalf("库里有 %d 条黑话，期望 2 条", len(saved))
	}
}

// TestStartReturnsImmediately 防的是：Start 阻塞在 ticker 上，把整个 app 启动卡死。
// goroutine 在 ctx 取消后自动退出（t.Context() 在测试结束时取消）。
func TestStartReturnsImmediately(t *testing.T) {
	cfg := testConfig("http://127.0.0.1:0")
	cfg.Learning.ExpressionEnabled = true
	cfg.Learning.IntervalMinutes = 60 // 测试存活期内不会真的跑一轮

	l := New(Options{Config: config.Wrap(cfg)})

	done := make(chan struct{})
	go func() {
		l.Start(t.Context())
		close(done)
	}()

	select {
	case <-done:
		// 期望：Start 立刻返回。
	case <-time.After(2 * time.Second):
		t.Fatal("Start 没有立即返回")
	}
}

// TestLearnExpressionsFiltersBadCandidates 验证 filterExpressions 的三类过滤
// 真的生效：溯源不上、跟机器人名字重、含非文字标记。
// 这些不像「报错」那么明显，被写进库后只会表现为「木木突然自称自己」。
func TestLearnExpressionsFiltersBadCandidates(t *testing.T) {
	const expressionJSON = `[
		{"situation":"正常场景","style":"可以可以","source_id":"1"},
		{"situation":"溯源不上","style":"离谱","source_id":"99"},
		{"situation":"叫自己名字","style":"木木","source_id":"1"},
		{"situation":"提到 SELF","style":"SELF 说过的话","source_id":"1"},
		{"situation":"含表情标记","style":"[表情包] 哈哈","source_id":"1"}
	]`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": expressionJSON},
				"finish_reason": "stop",
			}},
		})
	}))
	defer srv.Close()

	db := openTestDB(t)
	const sessionID = "sess-filter"
	seedSession(t, db, sessionID)
	seedUserMessages(t, db, sessionID, 4)

	cfg := testConfig(srv.URL)
	cfg.Bot.Nickname = "木木"
	cfg.Learning.ExpressionEnabled = true
	cfg.Learning.MinMessages = 3

	l := New(Options{Config: config.Wrap(cfg), LLM: llm.New(config.Wrap(cfg)), Store: db})
	expressions, _, err := l.RunOnce(t.Context())
	if err != nil {
		t.Fatalf("RunOnce 失败: %v", err)
	}
	if expressions != 1 {
		t.Fatalf("写入 %d 条表达，期望只有 1 条通过过滤", expressions)
	}

	saved, err := db.Memory().ListExpressions(t.Context(), sessionID, 10)
	if err != nil {
		t.Fatalf("读取表达失败: %v", err)
	}
	if len(saved) != 1 || saved[0].Style != "可以可以" {
		t.Fatalf("库里内容不对: %+v", saved)
	}
}

// ============ 测试辅助 ============

// testConfig 造一份指向假服务的最小可用配置。
func testConfig(baseURL string) *config.Config {
	cfg := config.Default()
	cfg.Bot.Nickname = "木木"
	cfg.LLM.Providers = []config.ProviderConfig{{
		Name:          "test",
		BaseURL:       baseURL,
		APIKey:        "sk-test",
		Timeout:       10,
		MaxRetry:      1,
		RetryInterval: 1,
	}}
	cfg.LLM.Models = []config.ModelConfig{{
		Name:            "test-model",
		ModelIdentifier: "test-model",
		Provider:        "test",
	}}
	cfg.LLM.Tasks.Learner.Models = []string{"test-model"}
	cfg.LLM.Tasks.Learner.HardTimeout = 10
	return cfg
}

// openTestDB 在临时目录里开一个全新数据库，测试结束自动关闭。
func openTestDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "learning.db"))
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("关闭测试数据库失败: %v", err)
		}
	})
	return db
}

// seedSession 写入一个会话，RunOnce 靠 Sessions().List 发现要学哪些会话。
func seedSession(t *testing.T, db *store.DB, sessionID string) {
	t.Helper()
	now := time.Now()
	err := db.Sessions().Upsert(t.Context(), &domain.Session{
		ID:           sessionID,
		Platform:     "qq",
		Scope:        domain.ScopeGroup,
		TargetID:     "group-1",
		DisplayName:  "测试群",
		LastActiveAt: now,
		CreatedAt:    now,
	})
	if err != nil {
		t.Fatalf("写入会话失败: %v", err)
	}
}

// seedUserMessages 造 n 条普通用户的文字消息。
func seedUserMessages(t *testing.T, db *store.DB, sessionID string, n int) {
	t.Helper()
	base := time.Now().Add(-time.Duration(n) * time.Minute).Truncate(time.Second)
	for i := 0; i < n; i++ {
		msg := &domain.Message{
			ID:         fmt.Sprintf("m-%d", i),
			MessageID:  fmt.Sprintf("plat-%d", i),
			Platform:   "qq",
			SessionID:  sessionID,
			UserID:     fmt.Sprintf("u-%d", i%3),
			UserName:   fmt.Sprintf("用户%d", i%3),
			GroupID:    "group-1",
			IsGroup:    true,
			Segments:   []domain.Segment{domain.Text(fmt.Sprintf("第 %d 条聊天内容，随便说点什么", i))},
			PlainText:  fmt.Sprintf("第 %d 条聊天内容，随便说点什么", i),
			ReceivedAt: base.Add(time.Duration(i) * time.Minute),
		}
		if err := db.Messages().Save(t.Context(), msg); err != nil {
			t.Fatalf("写入消息失败: %v", err)
		}
	}
}
