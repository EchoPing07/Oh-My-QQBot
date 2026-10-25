package llm

// 这个文件专测 tryModel 里「每次尝试的超时与取消」这段。
//
// 为什么值得单独测：这段出错的两种表现都是静默的 ——
//   - 超时没生效 → 请求永远挂着，表现为「机器人不理人」
//   - timedOut 读得太晚 → 永远读到 Canceled，超时被当成普通错误，
//     于是不该重试的失败被重试，也不该切模型的被切掉
// 以前这里覆盖率是 0，靠 mock 端到端兜，抓不到这类判定错误。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
)

// newTestClient 构造一个指向假服务端的最小客户端。
func newTestClient(t *testing.T, baseURL string) (*Client, *config.Config) {
	t.Helper()

	cfg := config.Default()
	cfg.LLM.Providers = []config.ProviderConfig{{
		Name:    "fake",
		BaseURL: baseURL,
		APIKey:  "test-key",
		// 重试间隔设为 0 会走默认 4 秒，测试里必须显式压到毫秒级，
		// 否则「重试」这个分支每次都要等 4 秒。
		MaxRetry: 1,
	}}
	cfg.LLM.Models = []config.ModelConfig{{
		Name:            "m1",
		ModelIdentifier: "fake-model",
		Provider:        "fake",
	}}
	cfg.LLM.Tasks.Planner = config.TaskModelConfig{
		Models:      []string{"m1"},
		MaxTokens:   64,
		Temperature: 0.7,
	}

	return New(config.Wrap(cfg)), cfg
}

// TestTryModelAppliesHardTimeout 防：hard_timeout 配置了却不生效。
//
// 服务端故意睡 2 秒，硬超时给 100 毫秒，期望拿到 ErrTimeout 而不是傻等。
func TestTryModelAppliesHardTimeout(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"迟到"}}]}`))
	}))
	defer srv.Close()

	c, cfg := newTestClient(t, srv.URL)
	// 硬超时压到 100ms，让测试不用真等。
	cfg.LLM.Tasks.Planner.HardTimeout = 0.1

	start := time.Now()
	_, err := c.Generate(context.Background(), config.TaskPlanner, &Request{
		Messages: []Message{{Role: "user", Content: "你好"}},
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("期望超时错误，实际成功")
	}
	if !strings.Contains(err.Error(), ErrTimeout.Error()) {
		t.Errorf("错误 = %v，期望包含 %v", err, ErrTimeout)
	}
	// 硬超时 100ms，宽限到 1.5s 足够宽松，但能抓住「完全没设超时」。
	if elapsed > 1500*time.Millisecond {
		t.Errorf("耗时 %v，硬超时没生效", elapsed)
	}
	if hits.Load() == 0 {
		t.Error("服务端没收到请求，说明请求根本没发出去")
	}
}

// TestTryModelCancelledReturnsAborted 防：上层取消（打断）被当成可重试的失败。
//
// 打断的语义是「别再试了」，如果这里退化成普通错误，
// 上层会切下一个模型重头再来一遍，打断等于失效。
func TestTryModelCancelledReturnsAborted(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	c, _ := newTestClient(t, srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// 等请求真的发出去了再取消，避免取消早于请求建立。
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	_, err := c.Generate(ctx, config.TaskPlanner, &Request{
		Messages: []Message{{Role: "user", Content: "你好"}},
	})
	if err == nil {
		t.Fatal("期望被取消，实际成功")
	}
	if !strings.Contains(err.Error(), ErrAborted.Error()) && ctx.Err() == nil {
		t.Errorf("错误 = %v，期望 ErrAborted 或 ctx 已取消", err)
	}
}

// TestTryModelNoTimeoutStillCancellable 防：两边都不配超时时，
// 请求变得完全无法打断。
//
// 硬超时 0 + provider.timeout 0 是合法的「不限时」配置，
// 但「不限时」不等于「不可取消」—— 否则一个卡死的请求会永远占着会话。
func TestTryModelNoTimeoutStillCancellable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	defer srv.Close()

	c, cfg := newTestClient(t, srv.URL)
	// 两个超时都清 0：这是「不限时」的合法配置。
	cfg.LLM.Tasks.Planner.HardTimeout = 0
	cfg.LLM.Providers[0].Timeout = 0

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.Generate(ctx, config.TaskPlanner, &Request{
			Messages: []Message{{Role: "user", Content: "你好"}},
		})
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("不限时配置下取消没有生效，请求无法被打断")
	}
}

// TestMinPositive 覆盖「取两个超时里较小的那个」的边界。
func TestMinPositive(t *testing.T) {
	cases := []struct {
		name string
		a, b time.Duration
		want time.Duration
	}{
		{"都为正取小", 3 * time.Second, 5 * time.Second, 3 * time.Second},
		{"a 更大取 b", 5 * time.Second, 3 * time.Second, 3 * time.Second},
		{"a 为 0 取 b", 0, 5 * time.Second, 5 * time.Second},
		{"b 为 0 取 a", 3 * time.Second, 0, 3 * time.Second},
		{"都为 0 返回 0", 0, 0, 0},
		{"负数视为未设置", -1, 2 * time.Second, 2 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := minPositive(c.a, c.b); got != c.want {
				t.Errorf("minPositive(%v, %v) = %v，期望 %v", c.a, c.b, got, c.want)
			}
		})
	}
}
