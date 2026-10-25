// Package llm 是模型调用层。
// 对应 MaiBot 的 src/llm_models，只保留 OpenAI 兼容客户端。
package llm

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
)

// 请求相关的错误类型，用于上层区分是否重试、是否切换模型。
var (
	// ErrAborted 表示请求被主动中断（例如思考中来了新消息）。
	ErrAborted = errors.New("llm: 请求被中断")
	// ErrNotOK 表示服务端返回非 2xx。
	ErrNotOK = errors.New("llm: 服务端返回错误")
	// ErrTimeout 表示请求超时。
	ErrTimeout = errors.New("llm: 请求超时")
	// ErrNoModel 表示没有可用模型。
	ErrNoModel = errors.New("llm: 没有可用模型")
)

// Request 是一次模型请求。
type Request struct {
	// Model 是服务商侧的模型标识符。
	Model string

	// Messages 是完整上下文。
	Messages []Message

	// Tools 是本次可用的工具定义。
	Tools []ToolDefinition

	// Temperature / MaxTokens 允许为零值，零值表示不发送。
	Temperature *float64
	MaxTokens   *int

	// Stream 强制流式。
	Stream bool

	// ResponseFormat 可选，目前支持 "json"。
	ResponseFormat string

	// ExtraBody 会合并到请求体。
	ExtraBody map[string]any
}

// Message 是发往模型的一条消息。
type Message struct {
	Role string `json:"role"`

	Content string `json:"content,omitempty"`

	ToolCalls []ToolCall `json:"tool_calls,omitempty"`

	ToolCallID string `json:"tool_call_id,omitempty"`

	Name string `json:"name,omitempty"`

	// Images 是 data URL 或 http URL 列表，多模态时转成 content 数组。
	Images []string `json:"-"`
}

// ToolCall 是模型请求的一次工具调用。
type ToolCall struct {
	ID   string `json:"id"`
	Type string `json:"type"`

	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction 是工具调用的函数部分。
type ToolCallFunction struct {
	Name string `json:"name"`
	// Arguments 是 JSON 字符串。不同模型可能返回不合法 JSON，需要 repair。
	Arguments string `json:"arguments"`
}

// ToolDefinition 是暴露给模型的工具。
type ToolDefinition struct {
	Type     string             `json:"type"`
	Function ToolFunctionDefine `json:"function"`
}

// ToolFunctionDefine 是工具的 schema。
type ToolFunctionDefine struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// Response 是一次模型响应。
type Response struct {
	// Content 是正文。工具调用时可能为空。
	Content string

	// Reasoning 是推理内容（如果服务商返回）。
	Reasoning string

	// ToolCalls 是模型请求的工具调用。
	ToolCalls []ToolCall

	// FinishReason 是结束原因。
	FinishReason string

	// Usage 是用量。
	Usage Usage

	// Model 是实际使用的模型。
	Model string

	// Duration 是耗时。
	Duration time.Duration
}

// Usage 是一次调用的 token 用量。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// CachedTokens 是命中缓存的 prompt token 数。
	CachedTokens int `json:"cached_tokens"`
}

// Delta 是流式增量。
type Delta struct {
	// Content 是本次增量正文。
	Content string
	// Reasoning 是本次增量推理内容。
	Reasoning string
	// ToolCallDelta 是工具调用的增量。
	ToolCallDelta *ToolCallDelta
}

// ToolCallDelta 是工具调用的流式增量。
type ToolCallDelta struct {
	// Index 是工具调用在数组中的位置。
	Index int
	// ID 只在首个增量里出现。
	ID string
	// Name 只在首个增量里出现。
	Name string
	// ArgumentsDelta 是参数 JSON 的片段。
	ArgumentsDelta string
}

// Chat 执行一次非流式请求。
func (c *Client) Chat(ctx context.Context, req *Request) (*Response, error) {
	return c.Generate(ctx, config.TaskPlanner, req)
}

// ChatStream 执行一次流式请求，每个增量回调一次。
// 返回的 Response 是拼接完成后的完整结果。
func (c *Client) ChatStream(ctx context.Context, req *Request, fn func(Delta)) (*Response, error) {
	if fn == nil {
		return c.Generate(ctx, config.TaskPlanner, req)
	}
	return c.GenerateStream(ctx, config.TaskPlanner, req, fn)
}

// 角色常量。
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// userText 构造一条纯文本 user 消息。
func userText(s string) Message { return Message{Role: RoleUser, Content: s} }

// systemText 构造一条 system 消息。
func systemText(s string) Message { return Message{Role: RoleSystem, Content: s} }

// assistantText 构造一条 assistant 消息。
func assistantText(s string) Message { return Message{Role: RoleAssistant, Content: s} }

// ErrUnsupported 表示请求了不支持的能力。
var ErrUnsupported = errors.New("llm: 不支持的能力")

// wrapf 用统一前缀包装错误。
func wrapf(err error, format string, args ...any) error {
	return fmt.Errorf("llm: "+format+": %w", append(args, err)...)
}
