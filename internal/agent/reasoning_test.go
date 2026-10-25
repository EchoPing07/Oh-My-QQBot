package agent

// reasoning.go 的测试。
//
// 这一层出错的后果很隐蔽：推理记录照样写库、WebUI 照样能打开，
// 只是内容被截断在半个汉字上。非法 UTF-8 进 SQLite 不会报错，
// 但读出来回放到页面时就是乱码，而「乱码」很难让人联想到截断逻辑。

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/llm"
)

// TestClipTextKeepsShortText 验证未超限时原样返回（一个字节都不该动）。
func TestClipTextKeepsShortText(t *testing.T) {
	cases := []string{
		"",
		"短文本",
		strings.Repeat("木", 100),
	}
	for _, s := range cases {
		if got := clipText(s); got != s {
			t.Errorf("未超限时应原样返回，输入长度 %d，实际 %q", len(s), got)
		}
	}

	// 正好等于上限也不该截断。
	exact := strings.Repeat("a", reasoningFieldLimit)
	if got := clipText(exact); got != exact {
		t.Errorf("正好等于上限时不该截断，实际长度 %d", len(got))
	}
}

// TestClipTextTruncatesOnRuneBoundary 是这一组的关键断言。
//
// 用一个 3 字节的汉字构造「上限落在汉字中间」的场景：
// 直接 s[:limit] 会劈坏 UTF-8，整条记录读出来就是乱码。
func TestClipTextTruncatesOnRuneBoundary(t *testing.T) {
	// 每个汉字 3 字节。上限 64KB 不是 3 的倍数，
	// 所以 64KB 处的字节一定落在某个汉字中间。
	long := strings.Repeat("木", reasoningFieldLimit) // 远大于上限
	got := clipText(long)

	if !utf8.ValidString(got) {
		t.Fatal("截断后不是合法 UTF-8")
	}
	if !strings.HasSuffix(got, "…（已截断）") {
		t.Errorf("截断后应带提示文案")
	}

	body := strings.TrimSuffix(got, "…（已截断）")
	if len(body) > reasoningFieldLimit {
		t.Errorf("截断后正文长度 %d 超过上限 %d", len(body), reasoningFieldLimit)
	}
	// 回退不能退太多：最多退到 rune 边界，也就是最多 3 字节。
	if len(body) <= reasoningFieldLimit-4 {
		t.Errorf("回退过头：正文长度 %d，上限 %d", len(body), reasoningFieldLimit)
	}
	// 内容必须仍是完整的「木」。
	if strings.Trim(body, "木") != "" {
		t.Errorf("截断后出现了非预期字符")
	}
}

// TestClipTextWithMixedWidths 用 2/3/4 字节混排校验，覆盖 2 字节与 4 字节字符。
func TestClipTextWithMixedWidths(t *testing.T) {
	// 交替使用 1 字节、2 字节、3 字节、4 字节字符，
	// 这样上限几乎必然落在某个多字节字符内部。
	unit := "a" + "é" + "木" + "😀" // 1 + 2 + 3 + 4 = 10 字节
	var b strings.Builder
	for b.Len() < reasoningFieldLimit*2 {
		b.WriteString(unit)
	}

	got := clipText(b.String())
	if !utf8.ValidString(got) {
		t.Fatal("混排截断后不是合法 UTF-8")
	}
	if utf8.RuneCountInString(got) == 0 {
		t.Error("截断后不该是空串")
	}
}

// TestBuildReasoningRequestSerializesMessages 验证请求被序列化成可读 JSON。
func TestBuildReasoningRequestSerializesMessages(t *testing.T) {
	req := &llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "系统提示"},
			{Role: llm.RoleUser, Content: "用户消息", Images: []string{"data:image/png;base64,AAA"}},
		},
		Tools: []llm.ToolDefinition{{
			Type: "function",
			Function: llm.ToolFunctionDefine{
				Name:        "reply",
				Description: "发送回复",
				Parameters:  map[string]any{"type": "object"},
			},
		}},
	}

	got := buildReasoningRequest(req)

	var payload struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Name string `json:"name"`
			Desc string `json:"description"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(got), &payload); err != nil {
		t.Fatalf("输出不是合法 JSON: %v\n%s", err, got)
	}
	if len(payload.Messages) != 2 {
		t.Fatalf("消息条数 = %d，期望 2", len(payload.Messages))
	}
	if payload.Messages[0].Content != "系统提示" || payload.Messages[1].Content != "用户消息" {
		t.Errorf("消息内容不对：%v", payload.Messages)
	}
	if len(payload.Tools) != 1 || payload.Tools[0].Name != "reply" {
		t.Errorf("工具没被记录：%v", payload.Tools)
	}
	if payload.Tools[0].Desc != "发送回复" {
		t.Errorf("工具说明没被记录")
	}

	// 没有工具时 tools 字段应省略（omitempty），别留一个空数组占地方。
	noTools := buildReasoningRequest(&llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "x"}},
	})
	if strings.Contains(noTools, `"tools"`) {
		t.Errorf("无工具时不该出现 tools 字段：%s", noTools)
	}
}

// TestBuildReasoningResponseSerializesFields 验证响应的序列化。
func TestBuildReasoningResponseSerializesFields(t *testing.T) {
	resp := &llm.Response{
		Content:      "我打算回复他",
		Reasoning:    "原生的思考内容",
		FinishReason: "tool_calls",
		Model:        "glm-5.3",
		Duration:     1500 * time.Millisecond,
		ToolCalls: []llm.ToolCall{{
			ID:   "call_1",
			Type: "function",
			Function: llm.ToolCallFunction{
				Name:      "reply",
				Arguments: `{"msg_id":"m1"}`,
			},
		}},
	}

	var payload struct {
		Content    string         `json:"content"`
		Reasoning  string         `json:"reasoning"`
		ToolCalls  []llm.ToolCall `json:"tool_calls"`
		Finish     string         `json:"finish_reason"`
		Model      string         `json:"model"`
		DurationMS int64          `json:"duration_ms"`
	}
	got := buildReasoningResponse(resp)
	if err := json.Unmarshal([]byte(got), &payload); err != nil {
		t.Fatalf("输出不是合法 JSON: %v\n%s", err, got)
	}

	if payload.Content != "我打算回复他" {
		t.Errorf("正文 = %q", payload.Content)
	}
	if payload.Reasoning != "原生的思考内容" {
		t.Errorf("推理内容 = %q", payload.Reasoning)
	}
	if payload.Finish != "tool_calls" || payload.Model != "glm-5.3" {
		t.Errorf("finish/model 不对：%q %q", payload.Finish, payload.Model)
	}
	if payload.DurationMS != 1500 {
		t.Errorf("耗时 = %d ms，期望 1500", payload.DurationMS)
	}
	if len(payload.ToolCalls) != 1 || payload.ToolCalls[0].Function.Name != "reply" {
		t.Errorf("工具调用没被记录：%v", payload.ToolCalls)
	}

	// nil 响应（请求失败）返回空串，不能 panic。
	if got := buildReasoningResponse(nil); got != "" {
		t.Errorf("nil 响应应返回空串，实际 %q", got)
	}
}

// TestErrSuffix 验证错误信息被拼进记录。
//
// 失败的那次调用恰恰最需要回看，所以失败也要留记录并带上原因。
func TestErrSuffix(t *testing.T) {
	if got := errSuffix(nil); got != "" {
		t.Errorf("无错误时不该有后缀，实际 %q", got)
	}
	got := errSuffix(errFakeTest)
	if !strings.Contains(got, "调用失败") {
		t.Errorf("应标记调用失败：%q", got)
	}
	if !strings.Contains(got, errFakeTest.Error()) {
		t.Errorf("应包含原始错误信息：%q", got)
	}
}

// errFakeTest 是测试用的固定错误。
var errFakeTest = &fakeErr{msg: "服务端 400"}

type fakeErr struct{ msg string }

func (e *fakeErr) Error() string { return e.msg }

// TestNewTurnID 验证 turn_id 的形态。
//
// 注意：这里不断言「连续两次调用必定不同」。
// time.Now().UnixNano() 的精度取决于操作系统
// （Windows 上实测只有毫秒级，1 万次调用只能得到 2~3 个不同值），
// 所以同一毫秒内连续调用会押到同一个 ID。生产里一轮思考是秒级间隔，不受影响。
func TestNewTurnID(t *testing.T) {
	a := newTurnID("sess-1")

	if !strings.HasPrefix(a, "sess-1-") {
		t.Errorf("turn_id 应带会话前缀：%q", a)
	}
	suffix := strings.TrimPrefix(a, "sess-1-")
	if suffix == "" {
		t.Fatalf("turn_id 缺少时间部分：%q", a)
	}
	for _, r := range suffix {
		if r < '0' || r > '9' {
			t.Errorf("turn_id 的时间部分应全为数字：%q", a)
			break
		}
	}

	// 隔开一段时间后必须不同，否则 WebUI 会把两轮思考串成一轮。
	time.Sleep(5 * time.Millisecond)
	if b := newTurnID("sess-1"); a == b {
		t.Errorf("相隔 5ms 的 turn_id 相同：%q", a)
	}
}
