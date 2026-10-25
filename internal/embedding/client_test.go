package embedding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
)

// TestEmbedBatch 验证批量请求按 index 落位。
//
// 关键：服务端不保证返回顺序与请求一致，必须按 index 对账。
// 不按 index 落位会导致「向量与文本错配」，检索结果全乱，
// 而且不会报错 —— 属于最难查的一类 bug。
func TestEmbedBatch(t *testing.T) {
	srv := newEmbedServer(t, func(inputs []string) [][]float32 {
		// 故意倒序返回，验证我们按 index 而不是数组顺序落位。
		out := make([][]float32, len(inputs))
		for i := range inputs {
			out[i] = []float32{float32(i), float32(i + 1), float32(i + 2)}
		}
		return out
	}, true)

	c := newTestClient(t, srv.URL)

	vecs, err := c.Embed(context.Background(), []string{"第一", "第二", "第三"})
	if err != nil {
		t.Fatalf("向量化失败: %v", err)
	}
	if len(vecs) != 3 {
		t.Fatalf("返回 %d 条，期望 3", len(vecs))
	}
	// 即使服务端倒序，落位后第 0 条必须是输入第 0 条对应的向量。
	for i, v := range vecs {
		if len(v) != 3 {
			t.Fatalf("第 %d 条维度 = %d", i, len(v))
		}
		if v[0] != float32(i) {
			t.Errorf("第 %d 条向量首元素 = %v，期望 %d（index 落位错误）", i, v[0], i)
		}
	}
}

// TestEmbedChunksLargeInput 验证批量上限切分。
//
// 端点对单次 input 数组长度有限制，超了报 InvalidParameter
// 而不会明确说"太多了"，所以必须自己切。
func TestEmbedChunksLargeInput(t *testing.T) {
	var seenBatches []int
	srv := newEmbedServer(t, func(inputs []string) [][]float32 {
		seenBatches = append(seenBatches, len(inputs))
		out := make([][]float32, len(inputs))
		for i := range inputs {
			out[i] = []float32{1, 0, 0}
		}
		return out
	}, true)

	c := newTestClient(t, srv.URL)

	// 25 条，按 batchSize=10 应切成 10+10+5。
	texts := make([]string, 25)
	for i := range texts {
		texts[i] = "文本"
	}

	vecs, err := c.Embed(context.Background(), texts)
	if err != nil {
		t.Fatalf("向量化失败: %v", err)
	}
	if len(vecs) != 25 {
		t.Fatalf("返回 %d 条，期望 25", len(vecs))
	}
	if len(seenBatches) != 3 {
		t.Fatalf("请求了 %d 批，期望 3（10+10+5）", len(seenBatches))
	}
	if seenBatches[0] != 10 || seenBatches[1] != 10 || seenBatches[2] != 5 {
		t.Errorf("分批大小 = %v，期望 [10 10 5]", seenBatches)
	}
}

// TestEmbedNotConfigured 验证没配模型时返回明确错误。
func TestEmbedNotConfigured(t *testing.T) {
	cfg := config.Default()
	cfg.LLM.Tasks.Embedding.Models = nil

	c := New(config.Wrap(cfg))
	if c.Enabled() {
		t.Error("没配模型时 Enabled 应为 false")
	}
	if _, err := c.Embed(context.Background(), []string{"x"}); err == nil {
		t.Error("没配模型时应返回错误")
	}
}

// TestEmbedDimensionMismatch 验证维度不一致时报错。
//
// 库里混着不同维度的向量就没法算余弦，必须尽早失败。
func TestEmbedDimensionMismatch(t *testing.T) {
	call := 0
	srv := newEmbedServer(t, func(inputs []string) [][]float32 {
		call++
		dim := 3
		if call > 1 {
			dim = 4
		}
		out := make([][]float32, len(inputs))
		for i := range inputs {
			out[i] = make([]float32, dim)
		}
		return out
	}, true)

	c := newTestClient(t, srv.URL)

	if _, err := c.Embed(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("首次调用不该失败: %v", err)
	}
	if _, err := c.Embed(context.Background(), []string{"b"}); err == nil {
		t.Error("维度变化时应报错")
	}
}

// TestEmbedServerError 验证非 2xx 会带出服务端信息。
func TestEmbedServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv.URL)
	_, err := c.Embed(context.Background(), []string{"x"})
	if err == nil {
		t.Fatal("401 应报错")
	}
	// 错误信息要能看出服务端说了什么，否则排查时只能靠猜。
	if !contains(err.Error(), "401") {
		t.Errorf("错误信息缺少状态码: %v", err)
	}
}

// TestPackUnpackRoundTrip 验证向量的序列化往返。
func TestPackUnpackRoundTrip(t *testing.T) {
	orig := []float32{0.1, -0.25, 1e-6, 123.456, -789.0}

	packed := Pack(orig)
	if len(packed) != len(orig)*4 {
		t.Fatalf("打包后 %d 字节，期望 %d", len(packed), len(orig)*4)
	}

	got := Unpack(packed)
	if len(got) != len(orig) {
		t.Fatalf("还原后 %d 维，期望 %d", len(got), len(orig))
	}
	for i := range orig {
		// float32 有精度损失，用相对误差判断。
		diff := got[i] - orig[i]
		if diff < 0 {
			diff = -diff
		}
		if diff > 1e-5*max(1, abs(orig[i])) {
			t.Errorf("第 %d 维 = %v，原始 %v", i, got[i], orig[i])
		}
	}
}

// TestPackUnpackEdgeCases 验证空值与非法长度的处理。
func TestPackUnpackEdgeCases(t *testing.T) {
	if got := Pack(nil); got != nil {
		t.Errorf("Pack(nil) 应为 nil，实际 %v", got)
	}
	if got := Unpack(nil); got != nil {
		t.Errorf("Unpack(nil) 应为 nil")
	}
	// 长度不是 4 的倍数说明数据损坏，返回 nil 而不是越界读。
	if got := Unpack([]byte{1, 2, 3}); got != nil {
		t.Errorf("非法长度应返回 nil，实际 %v", got)
	}
}

// TestCosine 验证余弦相似度的基本性质。
func TestCosine(t *testing.T) {
	a := []float32{1, 0, 0}
	b := []float32{1, 0, 0}
	if got := Cosine(a, b); abs64(got-1) > 1e-6 {
		t.Errorf("同向量应得 1，实际 %v", got)
	}

	c := []float32{0, 1, 0}
	if got := Cosine(a, c); abs64(got) > 1e-6 {
		t.Errorf("正交向量应得 0，实际 %v", got)
	}

	d := []float32{-1, 0, 0}
	if got := Cosine(a, d); abs64(got+1) > 1e-6 {
		t.Errorf("反向量应得 -1，实际 %v", got)
	}

	// 维度不一致时返回 0 而不是 panic：库里的旧向量可能是别的维度。
	e := []float32{1, 0}
	if got := Cosine(a, e); got != 0 {
		t.Errorf("维度不一致应得 0，实际 %v", got)
	}

	// 零向量不能除零。
	if got := Cosine([]float32{0, 0, 0}, a); got != 0 {
		t.Errorf("零向量应得 0，实际 %v", got)
	}
}

// ============ 测试辅助 ============

// newEmbedServer 起一个假的 embedding 服务。
// reverse=true 时故意倒序返回 data 数组。
func newEmbedServer(t *testing.T, gen func([]string) [][]float32, reverse bool) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		vecs := gen(req.Input)

		type item struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}
		items := make([]item, len(vecs))
		for i, v := range vecs {
			items[i] = item{Embedding: v, Index: i}
		}
		if reverse {
			for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
				items[i], items[j] = items[j], items[i]
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data":  items,
			"model": "mock-embed",
			"usage": map[string]int{"prompt_tokens": len(req.Input), "total_tokens": len(req.Input)},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestClient(t *testing.T, baseURL string) *Client {
	t.Helper()

	cfg := config.Default()
	cfg.LLM.Providers = []config.ProviderConfig{{
		Name: "mock", BaseURL: baseURL, APIKey: "mock", Timeout: 5,
	}}
	cfg.LLM.Models = []config.ModelConfig{{
		Name: "mock-embed", ModelIdentifier: "mock-embed", Provider: "mock",
	}}
	cfg.LLM.Tasks.Embedding.Models = []string{"mock-embed"}

	return New(config.Wrap(cfg))
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func abs(v float32) float32 {
	if v < 0 {
		return -v
	}
	return v
}

func abs64(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func max(a, b float32) float32 {
	if a > b {
		return a
	}
	return b
}

// TestEmbedAuthHeaderValue 验证 embedding 层发出的鉴权头与 config.BuildAuthHeaderValue 一致。
//
// 这是 P1-5 的另一半：llm 层与 embedding 层必须共用同一拼接语义，
// 否则 prefix 写成 "Bearer "（带尾空格）时两条链路必有一条 401。
func TestEmbedAuthHeaderValue(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Api-Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"embedding":[1,0,0],"index":0}],"model":"m"}`))
	}))
	t.Cleanup(srv.Close)

	cfg := config.Default()
	cfg.LLM.Providers = []config.ProviderConfig{{
		Name: "mock", BaseURL: srv.URL, APIKey: "mock", Timeout: 5,
		AuthType: "header", AuthHeaderName: "X-Api-Key", AuthHeaderPrefix: "Bearer ",
	}}
	cfg.LLM.Models = []config.ModelConfig{{
		Name: "mock-embed", ModelIdentifier: "mock-embed", Provider: "mock",
	}}
	cfg.LLM.Tasks.Embedding.Models = []string{"mock-embed"}
	c := New(config.Wrap(cfg))

	if _, err := c.Embed(context.Background(), []string{"x"}); err != nil {
		t.Fatalf("向量化失败: %v", err)
	}
	if want := config.BuildAuthHeaderValue("Bearer ", "mock"); got != want {
		t.Errorf("header = %q，期望与 helper 一致 %q", got, want)
	}
	if got != "Bearer mock" {
		t.Errorf("header = %q，期望 \"Bearer mock\"（不得出现双空格）", got)
	}
}

// TestEmbedReportsConfigDimensionMismatch 验证端点忽略 dimensions 时报明确错误（P1-4 写入侧）。
//
// 服务端固定返回 4 维（很多中转站会无视 dimensions 参数），配置写 3 维。
// 旧口径静默把 4 维向量写进库，检索侧按配置的 3 维过滤 → 候选集恒空
// 且无任何日志；现在必须报错并带上两个数字，让上层的 Warn 能触发。
func TestEmbedReportsConfigDimensionMismatch(t *testing.T) {
	srv := newEmbedServer(t, func(inputs []string) [][]float32 {
		out := make([][]float32, len(inputs))
		for i := range out {
			out[i] = make([]float32, 4) // 端点无视 dimensions，返回原生 4 维
		}
		return out
	}, true)

	cfg := config.Default()
	cfg.LLM.Providers = []config.ProviderConfig{{Name: "mock", BaseURL: srv.URL, APIKey: "mock", Timeout: 5}}
	dim := 3
	cfg.LLM.Models = []config.ModelConfig{{
		Name: "mock-embed", ModelIdentifier: "mock-embed", Provider: "mock", Dimension: &dim,
	}}
	cfg.LLM.Tasks.Embedding.Models = []string{"mock-embed"}
	c := New(config.Wrap(cfg))

	_, err := c.Embed(context.Background(), []string{"x"})
	if err == nil {
		t.Fatal("配置维度与实测不一致时应报错")
	}
	msg := err.Error()
	if !contains(msg, "端点忽略了 dimensions 配置") {
		t.Errorf("错误应说明端点忽略了 dimensions 配置: %v", err)
	}
	if !contains(msg, "3") || !contains(msg, "4") {
		t.Errorf("错误应同时包含配置维度 3 与实测维度 4: %v", err)
	}
}

// TestEmbedSplitsByCharLimit 验证按总字符数动态分批（P2-16）。
//
// 每条 3000 字符：两条一批（6000 ≤ 8000），第三条会超限（9000）先切走；
// 单条超过上限的文本独占一批，不能切开。
func TestEmbedSplitsByCharLimit(t *testing.T) {
	var seenChars []int
	srv := newEmbedServer(t, func(inputs []string) [][]float32 {
		total := 0
		for _, in := range inputs {
			total += len([]rune(in))
		}
		seenChars = append(seenChars, total)
		out := make([][]float32, len(inputs))
		for i := range out {
			out[i] = []float32{1}
		}
		return out
	}, false)

	c := newTestClient(t, srv.URL)

	long := strings.Repeat("长", 3000)
	over := strings.Repeat("超", 9000)

	if _, err := c.Embed(context.Background(), []string{long, long, long, over}); err != nil {
		t.Fatalf("向量化失败: %v", err)
	}
	// 期望：[long,long]（6000 字）、[long]（3000 字，加上 over 会到 12000）、[over]（独占一批）
	want := []int{6000, 3000, 9000}
	if len(seenChars) != len(want) {
		t.Fatalf("分了 %d 批，期望 %d 批：%v", len(seenChars), len(want), seenChars)
	}
	for i := range want {
		if seenChars[i] != want[i] {
			t.Errorf("第 %d 批 %d 字符，期望 %d", i, seenChars[i], want[i])
		}
	}
}

// TestEmbedRetriesOnRateLimit 验证 429 会重试且能恢复（P2-15）。
//
// 限流型端点（DashScope 等）批量回填时 429 很常见，
// 以前一次失败就中断整轮回填。
func TestEmbedRetriesOnRateLimit(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"embedding":[1,0],"index":0}],"model":"m"}`))
	}))
	t.Cleanup(srv.Close)

	cfg := config.Default()
	cfg.LLM.Providers = []config.ProviderConfig{{
		Name: "mock", BaseURL: srv.URL, APIKey: "mock", Timeout: 5,
		MaxRetry: 2, RetryInterval: 1,
	}}
	cfg.LLM.Models = []config.ModelConfig{{
		Name: "mock-embed", ModelIdentifier: "mock-embed", Provider: "mock",
	}}
	cfg.LLM.Tasks.Embedding.Models = []string{"mock-embed"}
	c := New(config.Wrap(cfg))

	vecs, err := c.Embed(context.Background(), []string{"x"})
	if err != nil {
		t.Fatalf("429 后重试应成功: %v", err)
	}
	if len(vecs) != 1 || len(vecs[0]) != 2 {
		t.Fatalf("返回向量不对: %v", vecs)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("请求数 = %d，期望 2（首败一成）", got)
	}
}

// TestEmbedHonorsProviderTimeout 验证 provider.timeout 真正生效（P2-15）。
//
// 以前固定 60 秒、provider.timeout 完全没被读取；服务端挂住时
// 回填协程会陪它挂满 60 秒。
func TestEmbedHonorsProviderTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(5 * time.Second):
		case <-r.Context().Done():
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	cfg := config.Default()
	cfg.LLM.Providers = []config.ProviderConfig{{
		Name: "mock", BaseURL: srv.URL, APIKey: "mock",
		Timeout: 1, // 1 秒；任务硬超时默认 240 秒，取较小者
	}}
	cfg.LLM.Models = []config.ModelConfig{{
		Name: "mock-embed", ModelIdentifier: "mock-embed", Provider: "mock",
	}}
	cfg.LLM.Tasks.Embedding.Models = []string{"mock-embed"}
	c := New(config.Wrap(cfg))

	start := time.Now()
	_, err := c.Embed(context.Background(), []string{"x"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("服务端挂死时应超时报错")
	}
	if elapsed > 2500*time.Millisecond {
		t.Errorf("耗时 %v，provider 超时 1 秒没有生效", elapsed)
	}
}
