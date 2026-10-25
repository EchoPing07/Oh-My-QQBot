package memory

import (
	"strings"
	"testing"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
)

// msgWith 构造一条测试消息。
func msgWith(ts int64, name string, segs []domain.Segment) *domain.Message {
	return &domain.Message{
		UserID:     "u-" + name,
		UserName:   name,
		Segments:   segs,
		ReceivedAt: time.Unix(ts, 0),
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// TestParseSummaryResponsePlain 覆盖最理想的情况：模型老实输出纯 JSON。
func TestParseSummaryResponsePlain(t *testing.T) {
	raw := `{"summary":"他们在聊火锅和延迟","recall_cues":["讨论晚餐吃什么","抱怨模型延迟"]}`

	got, err := ParseSummaryResponse(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got.Summary != "他们在聊火锅和延迟" {
		t.Errorf("summary = %q", got.Summary)
	}
	if len(got.RecallCues) != 2 {
		t.Errorf("recall_cues 条数 = %d，期望 2", len(got.RecallCues))
	}
}

// TestParseSummaryResponseCodeFence 覆盖模型把 JSON 包进代码块的情况。
//
// 这是实际最常出现的形态 —— 提示词里明确说了「不要用 Markdown 代码块」，
// 但模型照样会包，所以必须能剥掉。
func TestParseSummaryResponseCodeFence(t *testing.T) {
	raw := "```json\n" +
		`{"summary":"聊了天气","recall_cues":["提到天气"]}` + "\n```"

	got, err := ParseSummaryResponse(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got.Summary != "聊了天气" {
		t.Errorf("summary = %q", got.Summary)
	}
}

// TestParseSummaryResponseChatter 覆盖前后带废话的情况。
func TestParseSummaryResponseChatter(t *testing.T) {
	raw := `好的，这是摘要：
{"summary":"聊了模型选型","recall_cues":["讨论用哪个模型"]}
希望有帮助。`

	got, err := ParseSummaryResponse(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got.Summary != "聊了模型选型" {
		t.Errorf("summary = %q", got.Summary)
	}
}

// TestParseSummaryResponseInvalid 覆盖各种垃圾输入，必须报错而不是返回空结构。
func TestParseSummaryResponseInvalid(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"空串", ""},
		{"没有 JSON", "我不太确定该怎么总结"},
		{"半个对象", `{"summary": "缺右括号"`},
		{"非法 JSON", `{"summary": }`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseSummaryResponse(tc.raw); err == nil {
				t.Error("期望报错，实际成功了")
			}
		})
	}
}

// TestNormalizeCues 覆盖去空、去重、上限 5 条。
func TestNormalizeCues(t *testing.T) {
	in := []string{"a", " ", "a", "b", "", "c", "d", "e", "f"}
	got := normalizeCues(in)

	if len(got) != 5 {
		t.Fatalf("条数 = %d，期望 5（上限）", len(got))
	}
	if got[0] != "a" {
		t.Errorf("首条 = %q，期望 a（去重后保留首次出现）", got[0])
	}
	for _, v := range got {
		if v == "" {
			t.Error("结果里不应有空串")
		}
	}
}

// TestRenderForSummaryKeepsVoiceASR 防的是「语音转写被 [语音] 覆盖」这个老 bug。
func TestRenderForSummaryKeepsVoiceASR(t *testing.T) {
	m := msgWith(1000, "小明", []domain.Segment{
		{Type: domain.SegmentVoice, Text: "我说的是明天下午三点"},
	})

	got := renderForSummary(m)
	if !contains(got, "明天下午三点") {
		t.Errorf("转写文本丢失，实际 = %q", got)
	}
	if contains(got, "[语音]") {
		t.Errorf("有转写时不该再出现占位符，实际 = %q", got)
	}
}

// TestRenderForSummaryPlaceholders 覆盖无转写的语音、图片、表情。
func TestRenderForSummaryPlaceholders(t *testing.T) {
	m := msgWith(1000, "小明", []domain.Segment{
		{Type: domain.SegmentImage},
		{Type: domain.SegmentEmoji},
		{Type: domain.SegmentVoice},
	})

	got := renderForSummary(m)
	for _, want := range []string{"[图片]", "[表情]", "[语音]"} {
		if !contains(got, want) {
			t.Errorf("缺少 %q，实际 = %q", want, got)
		}
	}
}

// TestSelectSummaryMessagesSkipsEmpty 覆盖「纯图片消息不进摘要」。
func TestSelectSummaryMessagesSkipsEmpty(t *testing.T) {
	msgs := []*domain.Message{
		msgWith(1000, "a", []domain.Segment{{Type: domain.SegmentText, Text: "有内容"}}),
		msgWith(1001, "b", nil), // 完全空
	}

	got := selectSummaryMessages(msgs, 1)
	if len(got) != 1 {
		t.Fatalf("条数 = %d，期望 1（空消息被跳过）", len(got))
	}
}

// TestSelectSummaryMessagesRespectsMin 覆盖「未达门槛不做摘要」。
func TestSelectSummaryMessagesRespectsMin(t *testing.T) {
	msgs := []*domain.Message{
		msgWith(1000, "a", []domain.Segment{{Type: domain.SegmentText, Text: "1"}}),
		msgWith(1001, "b", []domain.Segment{{Type: domain.SegmentText, Text: "2"}}),
	}

	if got := selectSummaryMessages(msgs, 3); got != nil {
		t.Fatalf("未达门槛时应返回 nil，实际 %d 条", len(got))
	}
}
