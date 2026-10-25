package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
)

// openAIRequest 是发往 /chat/completions 的请求体。
type openAIRequest struct {
	Model          string           `json:"model"`
	Messages       []openAIMessage  `json:"messages"`
	Tools          []ToolDefinition `json:"tools,omitempty"`
	Temperature    *float64         `json:"temperature,omitempty"`
	MaxTokens      *int             `json:"max_tokens,omitempty"`
	Stream         bool             `json:"stream,omitempty"`
	StreamOptions  *streamOptions   `json:"stream_options,omitempty"`
	ResponseFormat *responseFormat  `json:"response_format,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type responseFormat struct {
	Type string `json:"type"`
}

// openAIMessage 支持纯文本与多模态两种形态。
type openAIMessage struct {
	Role       string     `json:"role"`
	Content    any        `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// openAIContentPart 是多模态 content 数组的一项。
type openAIContentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *imageURLPart `json:"image_url,omitempty"`
}

type imageURLPart struct {
	URL string `json:"url"`
}

// openAIResponse 是非流式响应。
type openAIResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index   int `json:"index"`
		Message struct {
			Role             string     `json:"role"`
			Content          string     `json:"content"`
			ReasoningContent string     `json:"reasoning_content"`
			Reasoning        string     `json:"reasoning"`
			ToolCalls        []ToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage openAIUsage  `json:"usage"`
	Error *openAIError `json:"error"`
}

type openAIUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

type openAIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
}

func (e *openAIError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s (type=%s code=%v)", e.Message, e.Type, e.Code)
}

// openAIStreamChunk 是流式响应的一块。
type openAIStreamChunk struct {
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *openAIUsage `json:"usage"`
	Error *openAIError `json:"error"`
}

// doOpenAI 执行一次 OpenAI 兼容请求。
func (c *Client) doOpenAI(ctx context.Context, provider *config.ProviderConfig, req *Request, fn func(Delta)) (*Response, error) {
	body, err := c.buildBody(req, fn != nil)
	if err != nil {
		return nil, err
	}

	endpoint := strings.TrimRight(provider.BaseURL, "/") + "/chat/completions"
	if q := buildQuery(provider.DefaultQuery); q != "" {
		endpoint += "?" + q
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, wrapf(err, "构造请求")
	}
	httpReq.Header.Set("Content-Type", "application/json")
	applyAuth(httpReq, provider)
	for k, v := range provider.DefaultHeaders {
		httpReq.Header.Set(k, v)
	}

	start := time.Now()
	resp, err := c.http.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ErrAborted
		}
		return nil, wrapf(err, "发送请求")
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%w: HTTP %d: %s", ErrNotOK, resp.StatusCode, summarizeBody(raw))
	}

	// 解析方式必须与请求体的 stream 标志同口径（fn != nil || req.Stream）。
	// force_stream 的模型让服务端按 SSE 返回，这里若只看 fn 是否为 nil，
	// 会把 SSE 文本当普通 JSON 解析、整轮失败 —— 以前 mock 服务永远回
	// JSON，这个缺陷在测试里完全显形不了（audit 11 P2-24）。
	if fn != nil || req.Stream {
		return c.readStream(resp.Body, fn, start)
	}
	return c.readOnce(resp.Body, start)
}

// buildBody 构造请求体。
func (c *Client) buildBody(req *Request, stream bool) ([]byte, error) {
	msgs, err := convertMessages(req.Messages)
	if err != nil {
		return nil, err
	}

	out := openAIRequest{
		Model:    req.Model,
		Messages: msgs,
		Tools:    req.Tools,
		Stream:   stream || req.Stream,
	}

	if out.Stream {
		out.StreamOptions = &streamOptions{IncludeUsage: true}
	}

	if req.Temperature != nil {
		out.Temperature = req.Temperature
	}
	if req.MaxTokens != nil {
		out.MaxTokens = req.MaxTokens
	}
	if req.ResponseFormat == "json" {
		out.ResponseFormat = &responseFormat{Type: "json_object"}
	}

	// 合并额外请求体字段。
	base, err := json.Marshal(out)
	if err != nil {
		return nil, wrapf(err, "序列化请求")
	}
	if len(req.ExtraBody) == 0 {
		return base, nil
	}

	var merged map[string]any
	if err := json.Unmarshal(base, &merged); err != nil {
		return nil, wrapf(err, "反序列化请求")
	}
	for k, v := range req.ExtraBody {
		merged[k] = v
	}
	return json.Marshal(merged)
}

// convertMessages 把内部消息转成 OpenAI 结构，处理多模态。
func convertMessages(msgs []Message) ([]openAIMessage, error) {
	out := make([]openAIMessage, 0, len(msgs))

	for _, m := range msgs {
		om := openAIMessage{
			Role:       m.Role,
			ToolCalls:  m.ToolCalls,
			ToolCallID: m.ToolCallID,
			Name:       m.Name,
		}

		// 有图片时 content 必须是数组。
		if len(m.Images) > 0 {
			parts := make([]openAIContentPart, 0, len(m.Images)+1)
			if m.Content != "" {
				parts = append(parts, openAIContentPart{Type: "text", Text: m.Content})
			}
			for _, img := range m.Images {
				parts = append(parts, openAIContentPart{
					Type:     "image_url",
					ImageURL: &imageURLPart{URL: img},
				})
			}
			om.Content = parts
		} else if m.Content != "" {
			om.Content = m.Content
		} else if len(m.ToolCalls) == 0 && m.ToolCallID == "" {
			// 空内容且非工具消息，补一个空串避免部分服务端报错。
			om.Content = ""
		}

		out = append(out, om)
	}
	return out, nil
}

// applyAuth 按配置设置鉴权。
func applyAuth(req *http.Request, p *config.ProviderConfig) {
	authType := p.AuthType
	if authType == "" {
		authType = "bearer"
	}

	switch authType {
	case "none":
		return
	case "header":
		name := p.AuthHeaderName
		if name == "" {
			name = "Authorization"
		}
		// 拼接统一走 config.BuildAuthHeaderValue（与 embedding 层共用同一语义，
		// 对齐参考实现 openai_compat.py 的 _build_auth_header_value）。
		req.Header.Set(name, config.BuildAuthHeaderValue(p.AuthHeaderPrefix, p.APIKey))
	case "query":
		name := p.AuthQueryName
		if name == "" {
			name = "api_key"
		}
		q := req.URL.Query()
		q.Set(name, p.APIKey)
		req.URL.RawQuery = q.Encode()
	default: // bearer
		req.Header.Set("Authorization", config.BuildAuthHeaderValue("Bearer", p.APIKey))
	}
}

func buildQuery(params map[string]string) string {
	if len(params) == 0 {
		return ""
	}
	v := url.Values{}
	for k, val := range params {
		v.Set(k, val)
	}
	return v.Encode()
}

// readOnce 解析非流式响应。
func (c *Client) readOnce(body io.Reader, start time.Time) (*Response, error) {
	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, wrapf(err, "读取响应")
	}

	var parsed openAIResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, wrapf(err, "解析响应: %s", summarizeBody(raw))
	}
	if parsed.Error != nil {
		return nil, fmt.Errorf("%w: %s", ErrNotOK, parsed.Error.Error())
	}
	if len(parsed.Choices) == 0 {
		return nil, fmt.Errorf("%w: 响应没有 choices", ErrNotOK)
	}

	choice := parsed.Choices[0]
	out := &Response{
		Content:      choice.Message.Content,
		Reasoning:    firstNonEmpty(choice.Message.ReasoningContent, choice.Message.Reasoning),
		ToolCalls:    choice.Message.ToolCalls,
		FinishReason: choice.FinishReason,
		Model:        parsed.Model,
		Duration:     time.Since(start),
		Usage: Usage{
			PromptTokens:     parsed.Usage.PromptTokens,
			CompletionTokens: parsed.Usage.CompletionTokens,
			TotalTokens:      parsed.Usage.TotalTokens,
			CachedTokens:     parsed.Usage.PromptTokensDetails.CachedTokens,
		},
	}

	// 部分服务商不返回 total_tokens，需要自己算。
	if out.Usage.TotalTokens == 0 {
		out.Usage.TotalTokens = out.Usage.PromptTokens + out.Usage.CompletionTokens
	}

	for i := range out.ToolCalls {
		out.ToolCalls[i].Function.Arguments = RepairArguments(out.ToolCalls[i].Function.Arguments)
	}

	return out, nil
}

// readStream 解析 SSE 流，并拼接工具调用增量。
func (c *Client) readStream(body io.Reader, fn func(Delta), start time.Time) (*Response, error) {
	// fn 允许为 nil：force_stream 的模型没有增量回调也要走这条解析路径。
	if fn == nil {
		fn = func(Delta) {}
	}

	var (
		content   strings.Builder
		reasoning strings.Builder
		model     string
		finish    string
		usage     Usage
	)

	// 工具调用按 index 累积，因为参数是分片到达的。
	toolAcc := newToolCallAccumulator()

	scanner := bufio.NewScanner(body)
	// 单个 SSE 行可能很长（图片或长参数），放大缓冲。
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}

		var chunk openAIStreamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			// 单块解析失败不该中断整个流，跳过即可。
			c.log.Debug("跳过无法解析的流式块", "err", err)
			continue
		}
		if chunk.Error != nil {
			return nil, fmt.Errorf("%w: %s", ErrNotOK, chunk.Error.Error())
		}
		if chunk.Model != "" {
			model = chunk.Model
		}
		if chunk.Usage != nil {
			usage = Usage{
				PromptTokens:     chunk.Usage.PromptTokens,
				CompletionTokens: chunk.Usage.CompletionTokens,
				TotalTokens:      chunk.Usage.TotalTokens,
				CachedTokens:     chunk.Usage.PromptTokensDetails.CachedTokens,
			}
		}

		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				content.WriteString(choice.Delta.Content)
				fn(Delta{Content: choice.Delta.Content})
			}
			if r := firstNonEmpty(choice.Delta.ReasoningContent, choice.Delta.Reasoning); r != "" {
				reasoning.WriteString(r)
				fn(Delta{Reasoning: r})
			}

			for _, tc := range choice.Delta.ToolCalls {
				acc := toolAcc.get(tc.Index)
				if tc.ID != "" {
					acc.id = tc.ID
				}
				if tc.Function.Name != "" {
					acc.name = tc.Function.Name
				}
				acc.args.WriteString(tc.Function.Arguments)

				fn(Delta{ToolCallDelta: &ToolCallDelta{
					Index:          tc.Index,
					ID:             tc.ID,
					Name:           tc.Function.Name,
					ArgumentsDelta: tc.Function.Arguments,
				}})
			}

			if choice.FinishReason != "" {
				finish = choice.FinishReason
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, wrapf(err, "读取流")
	}

	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}

	out := &Response{
		Content:      content.String(),
		Reasoning:    reasoning.String(),
		ToolCalls:    toolAcc.build(),
		FinishReason: finish,
		Model:        model,
		Duration:     time.Since(start),
		Usage:        usage,
	}
	return out, nil
}

// toolCallAccumulator 按 index 累积流式工具调用。
type toolCallAccumulator struct {
	order []int
	calls map[int]*accumulatedCall
}

type accumulatedCall struct {
	id   string
	name string
	args strings.Builder
}

func newToolCallAccumulator() *toolCallAccumulator {
	return &toolCallAccumulator{calls: make(map[int]*accumulatedCall)}
}

func (a *toolCallAccumulator) get(index int) *accumulatedCall {
	if call, ok := a.calls[index]; ok {
		return call
	}
	call := &accumulatedCall{}
	a.calls[index] = call
	a.order = append(a.order, index)
	return call
}

func (a *toolCallAccumulator) build() []ToolCall {
	if len(a.order) == 0 {
		return nil
	}
	out := make([]ToolCall, 0, len(a.order))
	for _, idx := range a.order {
		call := a.calls[idx]
		if call.name == "" {
			continue
		}
		out = append(out, ToolCall{
			ID:   call.id,
			Type: "function",
			Function: ToolCallFunction{
				Name:      call.name,
				Arguments: RepairArguments(call.args.String()),
			},
		})
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func summarizeBody(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}
