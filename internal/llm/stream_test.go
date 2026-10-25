package llm

// 这个文件专测「请求带 stream:true 时响应必须按 SSE 解析」（audit 11 P2-24）。
//
// 缺陷形态：force_stream 的模型让请求体带上 stream:true，服务端按 SSE 返回，
// 但客户端只看「调用方是否传了增量回调」决定解析方式 —— 没传回调就把
// SSE 文本当普通 JSON 解析，整轮失败。以前 mock 服务永远回 JSON，
// 这条路径在端到端测试里显形不了。

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
)

// TestStreamRequestWithoutCallbackParsesSSE 防：req.Stream=true 且没有增量回调时，
// SSE 响应被 readOnce 当普通 JSON 解析而报错。
func TestStreamRequestWithoutCallbackParsesSSE(t *testing.T) {
	var sawStreamFlag atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		if _, err := r.Body.Read(body); err != nil && err.Error() != "EOF" {
			// 读满即可，不用处理
		}
		if strings.Contains(string(body), `"stream":true`) {
			sawStreamFlag.Store(true)
		}

		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := func(payload string) {
			fmt.Fprintf(w, "data: %s\n\n", payload)
			if flusher != nil {
				flusher.Flush()
			}
		}
		// 正文故意分两块，验证没有回调时也能完整拼接。
		chunk(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`)
		chunk(`{"choices":[{"index":0,"delta":{"content":"第一块"}}]}`)
		chunk(`{"choices":[{"index":0,"delta":{"content":"第二块"}}]}`)
		chunk(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv.URL)
	resp, err := c.Generate(t.Context(), config.TaskPlanner, &Request{
		Model:  "fake-model",
		Stream: true,
		Messages: []Message{
			{Role: "user", Content: "你好"},
		},
	})
	if err != nil {
		t.Fatalf("stream 请求不应失败: %v", err)
	}
	if resp.Content != "第一块第二块" {
		t.Errorf("SSE 增量拼接错误，实际: %q", resp.Content)
	}
	if !sawStreamFlag.Load() {
		t.Error("请求体没有带 stream:true")
	}
}

// TestForceStreamModelUsesSSE 防：ForceStream 合并进请求体后，
// 端到端走到的也是 SSE 解析路径（这是线上 force_stream 的真实形态）。
func TestForceStreamModelUsesSSE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"index":0,"delta":{"role":"assistant"}}]}`)
		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer srv.Close()

	c, cfg := newTestClient(t, srv.URL)
	cfg.LLM.Models[0].ForceStream = true
	// 重新包装：改的是构造前的那份配置，直接再造一个客户端。
	c = New(config.Wrap(cfg))

	resp, err := c.Generate(t.Context(), config.TaskPlanner, &Request{
		Model: "fake-model",
		Messages: []Message{
			{Role: "user", Content: "你好"},
		},
	})
	if err != nil {
		t.Fatalf("force_stream 模型不应失败: %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("流式正文不对，实际: %q", resp.Content)
	}
}
