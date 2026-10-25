package learning

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/llm"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/store"
)

// TestShouldInferMeaning 逐条对照参考实现 _should_infer_meaning 的阈值逻辑。
func TestShouldInferMeaning(t *testing.T) {
	cases := []struct {
		name           string
		count          int
		lastInferredAt int
		meaning        string
		want           bool
	}{
		// 不到第一个阈值，一律不推断。
		{"1 次不推断", 1, 0, "", false},
		{"3 次不推断", 3, 0, "", false},
		// 刚好到 4 次，该推断了。
		{"4 次推断", 4, 0, "", true},
		// 4 到 8 之间，没到下一个阈值。
		{"5 次已推断过不再推", 5, 4, "旧含义", false},
		// 到 8 次，第二次推断。
		{"8 次再推断", 8, 4, "旧含义", true},
		{"9 次未达 25 不推", 9, 8, "旧含义", false},
		{"25 次再推断", 25, 8, "旧含义", true},
		{"100 次再推断", 100, 25, "旧含义", true},
		// 超过 100 之后没有下一个阈值了。
		{"200 次不再推断", 200, 100, "旧含义", false},
		// 同一 count 重复判断时不重复推断（防重启后重算）。
		{"同 count 不重复推断", 4, 4, "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldInferMeaning(tc.count, tc.lastInferredAt, tc.meaning)
			if got != tc.want {
				t.Errorf("shouldInferMeaning(%d, %d, %q) = %v，期望 %v",
					tc.count, tc.lastInferredAt, tc.meaning, got, tc.want)
			}
		})
	}
}

// TestNextThreshold 验证阈值推进。
func TestNextThreshold(t *testing.T) {
	cases := []struct{ last, want int }{
		{0, 4},
		{4, 8},
		{8, 25},
		{25, 100},
		{100, 0},
		{200, 0},
	}
	for _, tc := range cases {
		if got := nextThreshold(tc.last); got != tc.want {
			t.Errorf("nextThreshold(%d) = %d，期望 %d", tc.last, got, tc.want)
		}
	}
}

// TestInferMeaningUsesContext 验证第一步（带上下文）能拿到含义时不走第二步。
func TestInferMeaningUsesContext(t *testing.T) {
	srv, calls := newInferenceServer(t, []string{
		`{"meaning":"永远的神，表示极度推崇","no_info":false}`,
	})

	l := newTestLearner(t, srv.URL)
	got, err := l.inferMeaning(context.Background(), "yyds", "小明：这个配置 yyds", "")
	if err != nil {
		t.Fatalf("推断失败: %v", err)
	}
	if !strings.Contains(got, "永远的神") {
		t.Errorf("含义 = %q", got)
	}
	if n := calls(); n != 1 {
		t.Errorf("模型被调用 %d 次，期望 1（第一步就够了）", n)
	}
}

// TestInferMeaningFallsBackToContentOnly 验证第一步说 no_info 时会走第二步。
//
// 这是参考实现的关键设计：很多黑话脱离上下文也能猜，
// 而上下文里常常只有「笑死」这种零信息量的用法。
func TestInferMeaningFallsBackToContentOnly(t *testing.T) {
	srv, calls := newInferenceServer(t, []string{
		`{"meaning":"","no_info":true}`,
		`{"meaning":"意思是笑死我了"}`,
	})

	l := newTestLearner(t, srv.URL)
	got, err := l.inferMeaning(context.Background(), "xswl", "小明：xswl", "")
	if err != nil {
		t.Fatalf("推断失败: %v", err)
	}
	if !strings.Contains(got, "笑死") {
		t.Errorf("含义 = %q，期望来自第二次调用", got)
	}
	if n := calls(); n != 2 {
		t.Errorf("模型被调用 %d 次，期望 2（一次 no_info 后重试）", n)
	}
}

// TestInferMeaningBothFail 验证两次都推不出时返回空串而不是报错。
func TestInferMeaningBothFail(t *testing.T) {
	srv, _ := newInferenceServer(t, []string{
		`{"meaning":"","no_info":true}`,
		`{"meaning":""}`,
	})

	l := newTestLearner(t, srv.URL)
	got, err := l.inferMeaning(context.Background(), "???", "没有线索", "")
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if got != "" {
		t.Errorf("期望空串，实际 = %q", got)
	}
}

// TestInferMeaningHandlesNonJSON 验证模型直接回一句话时不崩。
func TestInferMeaningHandlesNonJSON(t *testing.T) {
	srv, _ := newInferenceServer(t, []string{
		"我不太确定这个词是什么意思",
		`{"meaning":"可能是语气词"}`,
	})

	l := newTestLearner(t, srv.URL)
	got, err := l.inferMeaning(context.Background(), "emm", "上下文", "")
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if !strings.Contains(got, "语气词") {
		t.Errorf("含义 = %q", got)
	}
}

// TestLearnJargonsIncrementsCount 验证重复学到同一词条时命中次数累加。
//
// 这是推断含义的前提：count 不涨，就永远触发不了推断。
func TestLearnJargonsIncrementsCount(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().Unix()

	for i := 0; i < 3; i++ {
		if err := db.Memory().SaveJargon(ctx, &store.Jargon{
			Content: "yyds", Context: "s1", CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("第 %d 次写入失败: %v", i+1, err)
		}
	}

	got, err := db.Memory().GetJargon(ctx, "yyds")
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if got == nil {
		t.Fatal("词条不存在")
	}
	if got.Count != 3 {
		t.Errorf("命中次数 = %d，期望 3", got.Count)
	}
}

// ============ 测试辅助 ============

// newInferenceServer 起一个按脚本依次返回的假模型服务。
func newInferenceServer(t *testing.T, replies []string) (*httptest.Server, func() int) {
	t.Helper()

	var (
		mu sync.Mutex
		n  int
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		idx := n
		n++
		mu.Unlock()

		reply := replies[len(replies)-1]
		if idx < len(replies) {
			reply = replies[idx]
		}

		body := map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"role": "assistant", "content": reply}, "finish_reason": "stop"},
			},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 10, "total_tokens": 20},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)

	return srv, func() int {
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}

// newTestLearner 构造一个指向假服务的 Learner。
func newTestLearner(t *testing.T, baseURL string) *Learner {
	t.Helper()

	cfg := config.Default()
	cfg.LLM.Providers = []config.ProviderConfig{{
		Name:    "mock",
		BaseURL: baseURL,
		APIKey:  "mock",
		Timeout: 5,
	}}
	cfg.LLM.Models = []config.ModelConfig{{
		Name:            "mock",
		ModelIdentifier: "mock",
		Provider:        "mock",
	}}
	cfg.LLM.Tasks.Learner.Models = []string{"mock"}
	cfg.LLM.Tasks.Learner.HardTimeout = 5

	return New(Options{Config: config.Wrap(cfg), LLM: llm.New(config.Wrap(cfg)), Store: nil})
}
