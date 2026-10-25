// Package mock 提供一个假的 OpenAI 兼容服务，
// 用于在没有真实模型、也不想反复手动发 QQ 消息时做端到端验证。
//
// 用法：
//
//	srv := mock.NewServer(mock.Options{ ... })
//	defer srv.Close()
//	// 把 srv.URL 填进 config.LLM.Providers[0].BaseURL
//
// 它会：
//   - 记录收到的每个请求（提示词、工具、消息）
//   - 按脚本决定返回「工具调用」还是「文本」
//   - 模拟真实模型的行为，包括 reasoning_content
package mock

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// RecordedRequest 是一次被记录的请求。
type RecordedRequest struct {
	At       time.Time
	Model    string
	Messages []Message
	Tools    []Tool
	HasTools bool
	Raw      []byte
}

// Message 是请求里的一条消息。
type Message struct {
	Role       string     `json:"role"`
	Content    any        `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls"`
	ToolCallID string     `json:"tool_call_id"`
	Name       string     `json:"name"`
}

// Images 从 content 数组里提取 image_url 的数量。
//
// 真实的 OpenAI 请求把多模态塞在 content 数组里（type=image_url），
// 不像内部 llm.Message 那样有独立的 Images 字段，所以这里要解出来。
func (m Message) Images() []string {
	parts, ok := m.Content.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, part := range parts {
		mp, ok := part.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := mp["type"].(string); t != "image_url" {
			continue
		}
		if iu, ok := mp["image_url"].(map[string]any); ok {
			if u, ok := iu["url"].(string); ok && u != "" {
				out = append(out, u)
			}
		}
	}
	return out
}

// ContentText 返回文本内容（content 可能是字符串或数组）。
func (m Message) ContentText() string {
	switch v := m.Content.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, part := range v {
			if mp, ok := part.(map[string]any); ok {
				if s, ok := mp["text"].(string); ok {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	}
	return ""
}

// ToolCall 是 assistant 消息里的工具调用。
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Tool 是请求里的工具定义。
type Tool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

// Options 配置模拟服务的行为。
type Options struct {
	// Planner 决定 Planner 请求返回什么。参数是请求上下文。
	// 返回 nil 表示返回纯文本。
	Planner func(req *RecordedRequest, call int) *Reply
	// Replyer 决定 Replyer 请求（无工具）返回什么。
	Replyer func(req *RecordedRequest, call int) *Reply
	// Latency 是模拟延迟。
	Latency time.Duration
	// EmbeddingDim 是 /embeddings 返回的向量维度；0 表示 1024。
	EmbeddingDim int
	// EmbeddingStatus 非 0 时 /embeddings 一律返回该状态码，
	// 用于模拟 429 限流等故障（embedding 客户端对 429 有专门的退避路径）。
	EmbeddingStatus int
	// EmbeddingLatency 是 /embeddings 的模拟延迟，用于测慢响应。
	EmbeddingLatency time.Duration
}

// Reply 是一次模拟回复。
type Reply struct {
	// Text 是正文。
	Text string
	// Reasoning 是模拟的推理内容。
	Reasoning string
	// ToolCalls 是工具调用。为空则返回纯文本。
	ToolCalls []ToolCall
	// PromptTokens / CompletionTokens 用于统计。
	PromptTokens     int
	CompletionTokens int
	// HTTPStatus 非 0 时返回该状态码，用于模拟错误。
	HTTPStatus int
	// ErrorBody 是错误响应体。
	ErrorBody string
}

// TextReply 构造纯文本回复。
func TextReply(text string) *Reply {
	return &Reply{Text: text, CompletionTokens: len([]rune(text)) + 8}
}

// ToolReply 构造工具调用回复。
func ToolReply(name, args string) *Reply {
	tc := ToolCall{ID: "call_" + name, Type: "function"}
	tc.Function.Name = name
	tc.Function.Arguments = args
	return &Reply{ToolCalls: []ToolCall{tc}, CompletionTokens: 20}
}

// Server 是模拟服务。
type Server struct {
	*httptest.Server

	opts Options

	mu       sync.Mutex
	requests []*RecordedRequest
	planner  int
	replyer  int
	// embedRequests 单独记录 /embeddings 的请求文本。
	//
	// 不混进 requests：WaitForLLMCalls / PlannerRequests 等辅助方法
	// 的语义都是「对话请求」，混入向量请求会让现有测试的计数全部漂移。
	embedRequests [][]string
}

// NewServer 创建并启动模拟服务。
func NewServer(opts Options) *Server {
	if opts.Latency == 0 {
		opts.Latency = 0
	}

	s := &Server{opts: opts}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// Requests 返回所有已记录请求。
func (s *Server) Requests() []*RecordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*RecordedRequest, len(s.requests))
	copy(out, s.requests)
	return out
}

// Count 返回请求总数。
func (s *Server) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

// EmbedRequests 返回 /embeddings 每次调用收到的输入文本批次。
func (s *Server) EmbedRequests() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]string, len(s.embedRequests))
	copy(out, s.embedRequests)
	return out
}

// EmbedCount 返回 /embeddings 调用次数。
func (s *Server) EmbedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.embedRequests)
}

// Reset 清空记录。
func (s *Server) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = nil
	s.planner = 0
	s.replyer = 0
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	// /embeddings 单独分流：它不是对话请求，
	// 混进 requests 会让 WaitForLLMCalls / PlannerRequests 的计数全部漂移。
	if strings.HasSuffix(r.URL.Path, "/embeddings") {
		s.handleEmbeddings(w, r)
		return
	}
	if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
		http.NotFound(w, r)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, 400, "读取请求失败")
		return
	}

	var req struct {
		Model    string    `json:"model"`
		Messages []Message `json:"messages"`
		Tools    []Tool    `json:"tools"`
		Stream   bool      `json:"stream"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, 400, "解析请求失败: "+err.Error())
		return
	}

	rec := &RecordedRequest{
		At:       time.Now(),
		Model:    req.Model,
		Messages: req.Messages,
		Tools:    req.Tools,
		HasTools: len(req.Tools) > 0,
		Raw:      body,
	}

	s.mu.Lock()
	s.requests = append(s.requests, rec)
	var call int
	var reply *Reply
	if rec.HasTools {
		s.planner++
		call = s.planner
	} else {
		s.replyer++
		call = s.replyer
	}
	opts := s.opts
	s.mu.Unlock()

	if opts.Latency > 0 {
		time.Sleep(opts.Latency)
	}

	// 决定回复内容。
	switch {
	case rec.HasTools && opts.Planner != nil:
		reply = opts.Planner(rec, call)
	case !rec.HasTools && opts.Replyer != nil:
		reply = opts.Replyer(rec, call)
	case rec.HasTools:
		reply = ToolReply("stop", "{}")
	default:
		reply = TextReply("嗯嗯")
	}

	if reply == nil {
		reply = TextReply("嗯嗯")
	}

	if reply.HTTPStatus != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(reply.HTTPStatus)
		fmt.Fprint(w, reply.ErrorBody)
		return
	}

	if req.Stream {
		writeReplyStream(w, req.Model, reply)
		return
	}
	writeReply(w, req.Model, reply)
}

// handleEmbeddings 处理 /v1/embeddings。
//
// 向量按文本确定性生成（同文本永远同向量），
// 这样测试可以断言「同文本向量一致、检索能命中自己写入的摘要」，
// 而不需要假模型具有真实的语义能力。
func (s *Server) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, 400, "读取请求失败")
		return
	}
	var req struct {
		Model string   `json:"model"`
		Input []string `json:"input"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, 400, "解析请求失败: "+err.Error())
		return
	}

	s.mu.Lock()
	s.embedRequests = append(s.embedRequests, req.Input)
	opts := s.opts
	s.mu.Unlock()

	dim := opts.EmbeddingDim
	if dim == 0 {
		dim = 1024
	}
	if opts.EmbeddingLatency > 0 {
		time.Sleep(opts.EmbeddingLatency)
	}
	if opts.EmbeddingStatus != 0 {
		writeError(w, opts.EmbeddingStatus, "模拟 embedding 故障")
		return
	}

	data := make([]any, 0, len(req.Input))
	for i, text := range req.Input {
		data = append(data, map[string]any{
			"object":    "embedding",
			"index":     i,
			"embedding": deterministicVector(text, dim),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   data,
		"model":  req.Model,
		"usage": map[string]any{
			"prompt_tokens": len(req.Input),
			"total_tokens":  len(req.Input),
		},
	})
}

// deterministicVector 用文本哈希做种子生成单位向量。
//
// 手写 LCG 而不用 math/rand：避免依赖已弃用的全局源，
// 也保证跨版本、跨平台的输出稳定（测试断言依赖可复现性）。
func deterministicVector(text string, dim int) []float64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(text))
	state := h.Sum64()
	next := func() float64 {
		state = state*6364136223846793005 + 1442695040888963407
		return float64(state>>11) / float64(1<<53)
	}
	vec := make([]float64, dim)
	var norm float64
	for i := range vec {
		v := next() - 0.5
		vec[i] = v
		norm += v * v
	}
	if norm > 0 {
		inv := 1 / math.Sqrt(norm)
		for i := range vec {
			vec[i] *= inv
		}
	}
	return vec
}

// writeReplyStream 按 SSE 输出回复。
//
// 格式对齐 internal/llm/openai.go readStream 的解析约定：
// `data: {chunk}\n\n` 增量 + 结尾 `data: [DONE]`。
// 正文刻意切成多块发送 —— 客户端的增量拼接（content 与 tool_calls
// 参数都靠分片累积）只有在这种切法下才会真的被走到。
func writeReplyStream(w http.ResponseWriter, model string, reply *Reply) {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	send := func(payload any) {
		b, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	choice := func(delta any, extra map[string]any) map[string]any {
		c := map[string]any{"index": 0, "delta": delta}
		for k, v := range extra {
			c[k] = v
		}
		return c
	}

	send(map[string]any{
		"id": "chatcmpl-mock", "model": model,
		"choices": []any{choice(map[string]any{"role": "assistant", "content": ""}, nil)},
	})

	if reply.Reasoning != "" {
		send(map[string]any{
			"id": "chatcmpl-mock", "model": model,
			"choices": []any{choice(map[string]any{"reasoning_content": reply.Reasoning}, nil)},
		})
	}

	finish := "stop"
	if len(reply.ToolCalls) > 0 {
		finish = "tool_calls"
	} else {
		// 正文切 3 块，验证客户端能把分片拼回完整文本。
		runes := []rune(reply.Text)
		size := (len(runes) + 2) / 3
		for start := 0; start < len(runes); start += size {
			end := start + size
			if end > len(runes) {
				end = len(runes)
			}
			send(map[string]any{
				"id": "chatcmpl-mock", "model": model,
				"choices": []any{choice(map[string]any{"content": string(runes[start:end])}, nil)},
			})
		}
	}

	if len(reply.ToolCalls) > 0 {
		tcs := make([]any, 0, len(reply.ToolCalls))
		for i, tc := range reply.ToolCalls {
			tcs = append(tcs, map[string]any{
				"index":    i,
				"id":       tc.ID,
				"type":     tc.Type,
				"function": map[string]any{"name": tc.Function.Name, "arguments": tc.Function.Arguments},
			})
		}
		send(map[string]any{
			"id": "chatcmpl-mock", "model": model,
			"choices": []any{choice(map[string]any{"tool_calls": tcs}, nil)},
		})
	}

	promptTokens := reply.PromptTokens
	if promptTokens == 0 {
		promptTokens = 100
	}
	send(map[string]any{
		"id": "chatcmpl-mock", "model": model,
		"choices": []any{choice(map[string]any{}, map[string]any{"finish_reason": finish})},
		"usage": map[string]any{
			"prompt_tokens":     promptTokens,
			"completion_tokens": reply.CompletionTokens,
			"total_tokens":      promptTokens + reply.CompletionTokens,
		},
	})

	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func writeReply(w http.ResponseWriter, model string, reply *Reply) {
	var msg map[string]any
	if len(reply.ToolCalls) > 0 {
		msg = map[string]any{
			"role":       "assistant",
			"content":    nil,
			"tool_calls": reply.ToolCalls,
		}
	} else {
		msg = map[string]any{
			"role":    "assistant",
			"content": reply.Text,
		}
	}
	if reply.Reasoning != "" {
		msg["reasoning_content"] = reply.Reasoning
	}

	finish := "stop"
	if len(reply.ToolCalls) > 0 {
		finish = "tool_calls"
	}

	promptTokens := reply.PromptTokens
	if promptTokens == 0 {
		promptTokens = 100
	}

	resp := map[string]any{
		"id":    "chatcmpl-mock",
		"model": model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       msg,
				"finish_reason": finish,
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     promptTokens,
			"completion_tokens": reply.CompletionTokens,
			"total_tokens":      promptTokens + reply.CompletionTokens,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": "mock_error"},
	})
}

// SystemText 返回请求里第一条 system 消息的正文。
//
// 单独提供而不是让测试自己遍历：几乎所有「提示词有没有带上 X」
// 的断言都要它，而且角色筛选与「content 可能是数组」这两件事
// 每个测试各写一遍必然会有人写错。
func (r *RecordedRequest) SystemText() string {
	for _, m := range r.Messages {
		if m.Role == "system" {
			return m.ContentText()
		}
	}
	return ""
}

// FullText 把所有消息的文本拼在一起，用于「某个词是否出现在请求里」的粗查。
func (r *RecordedRequest) FullText() string {
	var b strings.Builder
	for _, m := range r.Messages {
		b.WriteString(m.ContentText())
		b.WriteString("\n")
	}
	return b.String()
}
