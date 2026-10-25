package llm

import (
	"context"
	"fmt"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
)

// probeFixture 起一个假服务端并构造一个指向它的 Client。
//
// 用 httptest 而不是 mock 掉 http.Client：探活函数的价值就在
// 「真的发了一个 HTTP 请求、真的解析了响应」，把这一层 mock 掉
// 就只剩「函数被调用了」这种没有信息量的断言。
func probeFixture(t *testing.T, handler http.HandlerFunc) (*Client, *config.Store) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	cfg := config.Default()
	cfg.LLM.Providers = []config.ProviderConfig{{
		Name:     "p1",
		BaseURL:  srv.URL + "/v1",
		APIKey:   "sk-test",
		AuthType: "bearer",
	}}
	cfg.LLM.Models = []config.ModelConfig{{
		Name:            "m1",
		ModelIdentifier: "real-model-id",
		Provider:        "p1",
	}}
	store := config.NewStoreFrom(cfg, "")
	return New(store), store
}

func TestProbeModelSuccess(t *testing.T) {
	c, _ := probeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("路径不对: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("鉴权头不对: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],
			"usage":{"total_tokens":12}
		}`))
	})

	res := c.ProbeModel(context.Background(), "m1", 5*time.Second)
	if !res.OK {
		t.Fatalf("应该成功，实际 error=%q hint=%q", res.Error, res.Hint)
	}
	if res.Reply != "ok" {
		t.Errorf("正文 = %q，期望 ok", res.Reply)
	}
	if res.Tokens != 12 {
		t.Errorf("tokens = %d，期望 12", res.Tokens)
	}
	// 实际发出去的必须是 model_identifier，不是模型别名。
	if res.Model != "real-model-id" {
		t.Errorf("请求的模型 = %q，期望 real-model-id", res.Model)
	}
	if res.Provider != "p1" {
		t.Errorf("provider = %q", res.Provider)
	}
	if res.HTTPStatus != 200 {
		t.Errorf("状态码 = %d", res.HTTPStatus)
	}
}

// TestProbeModelAuthError 守 401：必须给出「去改 api_key」这种可执行建议。
//
// 光显示 "HTTP 401" 用户还是不知道改哪个框，那这个功能就白做了。
func TestProbeModelAuthError(t *testing.T) {
	c, _ := probeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	})

	res := c.ProbeModel(context.Background(), "m1", 5*time.Second)
	if res.OK {
		t.Fatal("401 不能算成功")
	}
	if res.HTTPStatus != 401 {
		t.Errorf("状态码 = %d，期望 401", res.HTTPStatus)
	}
	if !strings.Contains(res.Hint, "api_key") {
		t.Errorf("401 的建议应指向 api_key，实际 = %q", res.Hint)
	}
}

// TestProbeModelNotFoundHintsModelIdentifier 守 404 的正确解释。
//
// AGENTS.md 第 41 条：404 是「这个模型名不存在」，不是「端点不支持这个能力」。
// 当时把这个搞错，导致整条记忆检索路线走偏。
func TestProbeModelNotFoundHintsModelIdentifier(t *testing.T) {
	c, _ := probeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"model not found"}}`))
	})

	res := c.ProbeModel(context.Background(), "m1", 5*time.Second)
	if res.OK {
		t.Fatal("404 不能算成功")
	}
	hint := res.Hint
	if !strings.Contains(hint, "标识符") && !strings.Contains(hint, "/v1") {
		t.Errorf("404 的建议应指向模型标识符或 /v1 路径，实际 = %q", hint)
	}
}

// TestProbeModelTimeout 守超时：必须在上限内返回，不能挂住。
//
// 配置写错最常见的结果就是指向一个黑洞地址。不设上限的话
// 界面上一直转圈，用户以为「程序卡了」。
func TestProbeModelTimeout(t *testing.T) {
	c, _ := probeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second)
	})

	start := time.Now()
	res := c.ProbeModel(context.Background(), "m1", 300*time.Millisecond)
	elapsed := time.Since(start)

	if res.OK {
		t.Fatal("超时不能算成功")
	}
	if elapsed > 2*time.Second {
		t.Errorf("应该在超时后很快返回，实际耗时 %v", elapsed)
	}
	if !strings.Contains(res.Hint, "超时") && !strings.Contains(res.Hint, "连不上") {
		t.Errorf("超时的建议不对: %q", res.Hint)
	}
}

// TestProbeModelEmptyContent 守「通了但没正文」。
//
// 这不算失败（链路是好的），但必须给出提示 —— 否则用户会拿推理模型
// 配 Replyer，然后在真机上看到机器人「不说话」（AGENTS.md 第 1 条）。
func TestProbeModelEmptyContent(t *testing.T) {
	c, _ := probeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"content":"","reasoning_content":"思考了一大堆"},"finish_reason":"length"}]
		}`))
	})

	res := c.ProbeModel(context.Background(), "m1", 5*time.Second)
	if !res.OK {
		t.Fatalf("空正文仍然是链路通，不该算失败: %q", res.Error)
	}
	if res.Reply != "" {
		t.Errorf("正文应为空，实际 %q", res.Reply)
	}
	if !strings.Contains(res.Hint, "推理模型") {
		t.Errorf("空正文时应提示推理模型问题，实际 = %q", res.Hint)
	}
}

// TestProbeModelMissingProvider 守配置自身的错误。
func TestProbeModelMissingProvider(t *testing.T) {
	c, _ := probeFixture(t, func(w http.ResponseWriter, r *http.Request) {})

	res := c.ProbeModel(context.Background(), "不存在的模型", 5*time.Second)
	if res.OK {
		t.Fatal("模型不存在不能算成功")
	}
	if !strings.Contains(res.Hint, "模型") {
		t.Errorf("建议应指向模型列表，实际 = %q", res.Hint)
	}
}

// TestProbeModelHangingConnection 守「连不上」（不是超时，是立刻失败）。
func TestProbeModelHangingConnection(t *testing.T) {
	cfg := config.Default()
	// 一个必然连不上的地址。
	cfg.LLM.Providers = []config.ProviderConfig{{
		Name: "p1", BaseURL: "http://127.0.0.1:1/v1", AuthType: "bearer",
	}}
	cfg.LLM.Models = []config.ModelConfig{{Name: "m1", ModelIdentifier: "x", Provider: "p1"}}
	c := New(config.NewStoreFrom(cfg, ""))

	res := c.ProbeModel(context.Background(), "m1", 3*time.Second)
	if res.OK {
		t.Fatal("连不上不能算成功")
	}
	if res.Hint == "" {
		t.Error("连不上时必须给出排查建议")
	}
	if res.HTTPStatus != 0 {
		t.Errorf("没拿到响应时状态码应为 0，实际 %d", res.HTTPStatus)
	}
}

// TestClipDoesNotSplitRunes 守 clip 按 rune 截断（AGENTS.md 第 21 条）。
func TestClipDoesNotSplitRunes(t *testing.T) {
	// 3 个汉字 = 9 字节，截 2 个字符应当得到合法的 2 字。
	got := clip([]byte("中文测试"), 2)
	if got != "中文…" {
		t.Errorf("clip = %q，期望 中文…", got)
	}
	if !strings.HasPrefix(got, "中文") {
		t.Error("按 rune 截断后前缀必须完整")
	}
}

// TestProbeProviderListsModels 守：填完服务商但还没加模型时也能自查。
//
// 这是这个接口存在的全部理由 —— 那个阶段没有模型名可测。
func TestProbeProviderListsModels(t *testing.T) {
	c, _ := probeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/models") {
			t.Errorf("应该调 /models，实际 %s", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("应该是 GET，实际 %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("鉴权头不对: %q", got)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"glm-5.3"},{"id":"deepseek-v4"}]}`))
	})

	res := c.ProbeProvider(context.Background(), "p1", 5*time.Second)
	if !res.OK {
		t.Fatalf("应该成功：%s / %s", res.Error, res.Hint)
	}
	if len(res.Models) != 2 {
		t.Errorf("应返回 2 个模型，实际 %v", res.Models)
	}
	if res.HTTPStatus != 200 {
		t.Errorf("状态码 = %d", res.HTTPStatus)
	}
}

// TestProbeProviderNoModelsEndpoint 守 404 的正确解释。
//
// 有些 OpenAI 兼容端点不实现 /models。这时报「配置错误」是错的 ——
// 用户会去改一个本来正确的 BaseURL，越改越乱。
func TestProbeProviderNoModelsEndpoint(t *testing.T) {
	c, _ := probeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	res := c.ProbeProvider(context.Background(), "p1", 5*time.Second)
	if res.OK {
		t.Fatal("404 不能算成功")
	}
	if !strings.Contains(res.Hint, "测试模型") {
		t.Errorf("404 时应引导改用模型级测试，实际 = %q", res.Hint)
	}
}

// TestProbeProviderAuthError 守 401 指向 api_key。
func TestProbeProviderAuthError(t *testing.T) {
	c, _ := probeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})

	res := c.ProbeProvider(context.Background(), "p1", 5*time.Second)
	if res.OK {
		t.Fatal("401 不能算成功")
	}
	if !strings.Contains(res.Hint, "API Key") {
		t.Errorf("401 建议应指向 API Key，实际 = %q", res.Hint)
	}
}

// TestProbeProviderEmptyList 守「通了但没返回列表」。
func TestProbeProviderEmptyList(t *testing.T) {
	c, _ := probeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	})

	res := c.ProbeProvider(context.Background(), "p1", 5*time.Second)
	if !res.OK {
		t.Fatal("空列表仍然是连通成功")
	}
	if res.Hint == "" {
		t.Error("没有列表时应提示改用模型级测试")
	}
}

// TestProbeProviderEmptyBaseURL 守配置自身的错误。
func TestProbeProviderEmptyBaseURL(t *testing.T) {
	cfg := config.Default()
	cfg.LLM.Providers = []config.ProviderConfig{{Name: "p1", BaseURL: ""}}
	c := New(config.NewStoreFrom(cfg, ""))

	res := c.ProbeProvider(context.Background(), "p1", 5*time.Second)
	if res.OK {
		t.Fatal("空 BaseURL 不能算成功")
	}
	// 断言「指向那个输入框」而不是「包含字面量 BaseURL」：
	// 面向用户的提示应该说「填完整地址」，术语留在错误原文里。
	if !strings.Contains(res.Hint, "地址") {
		t.Errorf("建议应指向地址填写，实际 = %q", res.Hint)
	}
}

// TestExplainProbeErrorAllStatuses 逐条覆盖状态码 → 建议的映射。
//
// 这张表是这个功能的全部价值：单回 "HTTP 429" 用户还是不知道该做什么。
// 每条分支都对着一个真实会遇到的场景，所以每条都要有用例。
func TestExplainProbeErrorAllStatuses(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{401, "api_key"},
		{403, "api_key"},
		{404, "模型标识符"},
		{400, "max_tokens"},
		{429, "限流"},
		{500, "故障"},
		{502, "故障"},
	}
	for _, tc := range cases {
		_, hint := explainProbeError(errors.New("boom"), tc.status)
		if !strings.Contains(hint, tc.want) {
			t.Errorf("status %d 的建议应包含 %q，实际 = %q", tc.status, tc.want, hint)
		}
	}
}

// TestExplainProbeErrorDeadline 守超时的建议。
func TestExplainProbeErrorDeadline(t *testing.T) {
	_, hint := explainProbeError(context.DeadlineExceeded, 0)
	if !strings.Contains(hint, "超时") {
		t.Errorf("超时应提示超时，实际 = %q", hint)
	}
}

// TestExplainProbeErrorNoResponse 守连不上。
func TestExplainProbeErrorNoResponse(t *testing.T) {
	_, hint := explainProbeError(errors.New("dial tcp: refused"), 0)
	if !strings.Contains(hint, "连不上") {
		t.Errorf("无响应时应提示连不上，实际 = %q", hint)
	}
}

// TestProbeModelBadJSON 守「返回了非 JSON」。
//
// 常见于 BaseURL 指向了一个网页（比如把 https://api.xxx.com
// 漏了 /v1 而它根路径是个 HTML 首页）。这时报「解析失败 + 原文片段」
// 比报「连不上」有用得多。
func TestProbeModelBadJSON(t *testing.T) {
	c, _ := probeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<!DOCTYPE html><html><body>Welcome</body></html>`))
	})

	res := c.ProbeModel(context.Background(), "m1", 5*time.Second)
	if res.OK {
		t.Fatal("非 JSON 不能算成功")
	}
	if !strings.Contains(res.Error, "解析") {
		t.Errorf("应报解析失败，实际 = %q", res.Error)
	}
}

// TestProbeModelNoChoices 守「JSON 合法但没有 choices」。
func TestProbeModelNoChoices(t *testing.T) {
	c, _ := probeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"x","choices":[]}`))
	})

	res := c.ProbeModel(context.Background(), "m1", 5*time.Second)
	if res.OK {
		t.Fatal("没有 choices 不能算成功")
	}
	if !strings.Contains(res.Error, "choices") {
		t.Errorf("应指出没有 choices，实际 = %q", res.Error)
	}
}

// TestProbeModelForceStream 守 force_stream 的模型能被正确探活。
//
// 真实根因：探活曾经固定发非流式请求，而 mergeRequest 会把
// model.ForceStream 合进 req.Stream —— 但 probeOnce 调 buildBody(req, false)
// 时第一个参数当成权威，标记被丢掉。表现是「测试失败但调用正常」。
func TestProbeModelForceStream(t *testing.T) {
	var sawStream bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 8192)
		n, _ := r.Body.Read(buf)
		body := string(buf[:n])
		if strings.Contains(body, `"stream":true`) {
			sawStream = true
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		// 只接流式的端点：非流式请求直接拒绝。
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"streaming required"}}`)
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.LLM.Providers = []config.ProviderConfig{{
		Name: "p1", BaseURL: srv.URL + "/v1", APIKey: "k", AuthType: "bearer",
	}}
	cfg.LLM.Models = []config.ModelConfig{{
		Name: "m1", ModelIdentifier: "mid", Provider: "p1", ForceStream: true,
	}}
	c := New(config.NewStoreFrom(cfg, ""))

	res := c.ProbeModel(context.Background(), "m1", 5*time.Second)
	if !res.OK {
		t.Fatalf("force_stream 模型应能探活成功，实际 error=%q hint=%q", res.Error, res.Hint)
	}
	if !sawStream {
		t.Error("探活必须发出 stream:true 的请求")
	}
	if res.Kind != "chat" {
		t.Errorf("kind = %q，期望 chat", res.Kind)
	}
}

// TestProbeModelUsesModelConfig 守：探活必须用模型级参数，不能自带一套。
//
// 真实根因：以前写死 max_tokens=64 / temperature=0，绕过 mergeRequest。
// 有些模型不接受太小的 max_tokens，于是「实际调用能通、测试报错」。
func TestProbeModelUsesModelConfig(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 8192)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	maxTokens := 2048
	temp := 0.7
	cfg := config.Default()
	cfg.LLM.Providers = []config.ProviderConfig{{
		Name: "p1", BaseURL: srv.URL + "/v1", APIKey: "k", AuthType: "bearer",
	}}
	cfg.LLM.Models = []config.ModelConfig{{
		Name: "m1", ModelIdentifier: "mid", Provider: "p1",
		MaxTokens: &maxTokens, Temperature: &temp,
	}}
	c := New(config.NewStoreFrom(cfg, ""))

	res := c.ProbeModel(context.Background(), "m1", 5*time.Second)
	if !res.OK {
		t.Fatalf("应成功: %s", res.Error)
	}
	if !strings.Contains(gotBody, `"max_tokens":2048`) {
		t.Errorf("探活应使用模型配置里的 max_tokens=2048，实际请求体: %s", gotBody)
	}
}

// TestProbeEmbeddingModel 守向量模型走 /embeddings 而不是 /chat/completions。
//
// 真实根因：不区分模型类型时，向量模型调 /chat/completions 会被
// 服务端以 404 "Unsupported model" 拒绝，看起来像模型名写错 ——
// 而它在实际调用（embedding.Client）里一直是好的。
func TestProbeEmbeddingModel(t *testing.T) {
	var hitChat bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/chat/completions") {
			hitChat = true
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":{"message":"Unsupported model for OpenAI compatibility mode"}}`)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/embeddings") {
			t.Errorf("意外的路径: %s", r.URL.Path)
		}
		vec := make([]string, 1024)
		for i := range vec {
			vec[i] = "0.01"
		}
		fmt.Fprintf(w, `{"data":[{"embedding":[%s],"index":0}]}`, strings.Join(vec, ","))
	}))
	defer srv.Close()

	dim := 1024
	cfg := config.Default()
	cfg.LLM.Providers = []config.ProviderConfig{{
		Name: "p1", BaseURL: srv.URL + "/v1", APIKey: "k", AuthType: "bearer",
	}}
	cfg.LLM.Models = []config.ModelConfig{{
		Name: "emb", ModelIdentifier: "text-embedding-3", Provider: "p1", Dimension: &dim,
	}}
	c := New(config.NewStoreFrom(cfg, ""))

	res := c.ProbeModel(context.Background(), "emb", 5*time.Second)
	if hitChat {
		t.Error("向量模型不能调 /chat/completions")
	}
	if !res.OK {
		t.Fatalf("向量模型应探活成功: %s / %s", res.Error, res.Hint)
	}
	if res.Kind != "embedding" {
		t.Errorf("kind = %q，期望 embedding", res.Kind)
	}
	if res.Dimension != 1024 {
		t.Errorf("维度 = %d，期望 1024", res.Dimension)
	}
}

// TestProbeEmbeddingDimensionMismatch 守维度不一致要报出来。
//
// 不报的后果很具体：memory_summaries 按「模型 + 维度」匹配向量
// （AGENTS.md 第 45 条），维度错了检索永远落空，而不报任何错 ——
// 表现只是「记忆功能没效果」。
func TestProbeEmbeddingDimensionMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 服务端返回 8 维，配置写 1024。
		fmt.Fprint(w, `{"data":[{"embedding":[0,0,0,0,0,0,0,0],"index":0}]}`)
	}))
	defer srv.Close()

	dim := 1024
	cfg := config.Default()
	cfg.LLM.Providers = []config.ProviderConfig{{
		Name: "p1", BaseURL: srv.URL + "/v1", AuthType: "bearer",
	}}
	cfg.LLM.Models = []config.ModelConfig{{
		Name: "emb", ModelIdentifier: "e", Provider: "p1", Dimension: &dim,
	}}
	c := New(config.NewStoreFrom(cfg, ""))

	res := c.ProbeModel(context.Background(), "emb", 5*time.Second)
	if !res.OK {
		t.Error("链路是通的，ok 应为 true")
	}
	if !strings.Contains(res.Error, "维度") {
		t.Errorf("应报维度不一致，实际 = %q", res.Error)
	}
	if res.Dimension != 8 {
		t.Errorf("应报告实际维度 8，实际 %d", res.Dimension)
	}
}
