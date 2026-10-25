package chat

import (
	"strings"
	"testing"
)

// TestSplitDisabled 验证关闭分割时原样返回。
func TestSplitDisabled(t *testing.T) {
	long := strings.Repeat("这是一句很长的话。", 50)
	got := SplitResponse(long, SplitConfig{Enable: false})
	if len(got) != 1 || got[0] != long {
		t.Errorf("关闭分割应原样返回，实际 %d 段", len(got))
	}
}

// TestSplitShortText 验证短文本不切。
func TestSplitShortText(t *testing.T) {
	got := SplitResponse("好的", DefaultSplitConfig())
	if len(got) != 1 || got[0] != "好的" {
		t.Errorf("短文本不应分割，实际 %v", got)
	}
}

// TestSplitBySentences 验证按句子切分。
func TestSplitBySentences(t *testing.T) {
	cfg := SplitConfig{
		Enable:         true,
		MaxLength:      10,
		MaxSentenceNum: 1,
		MaxSplitNum:    5,
	}
	got := SplitResponse("第一句。第二句。第三句。", cfg)
	if len(got) < 2 {
		t.Errorf("应按句切分，实际 %d 段: %v", len(got), got)
	}
	for _, s := range got {
		if utf8Len(s) > cfg.MaxLength {
			t.Errorf("段 %q 长度 %d 超过上限 %d", s, utf8Len(s), cfg.MaxLength)
		}
	}
}

// TestSplitMaxSplitNum 验证条数上限，且不丢内容。
func TestSplitMaxSplitNum(t *testing.T) {
	cfg := SplitConfig{
		Enable:         true,
		MaxLength:      5,
		MaxSentenceNum: 1,
		MaxSplitNum:    2,
	}
	got := SplitResponse("一。二。三。四。五。六。", cfg)
	if len(got) > cfg.MaxSplitNum {
		t.Errorf("段数 = %d 超过上限 %d", len(got), cfg.MaxSplitNum)
	}
	// 内容不能丢。
	joined := strings.Join(got, "")
	for _, ch := range []string{"一", "二", "三", "四", "五", "六"} {
		if !strings.Contains(joined, ch) {
			t.Errorf("内容丢失: %q 不在 %q", ch, joined)
		}
	}
}

// TestSplitOverflowReturnAll 验证超限保留全文。
func TestSplitOverflowReturnAll(t *testing.T) {
	cfg := SplitConfig{
		Enable:            true,
		MaxLength:         4,
		MaxSentenceNum:    1,
		MaxSplitNum:       2,
		OverflowReturnAll: true,
	}
	text := "一。二。三。四。五。"
	got := SplitResponse(text, cfg)
	if len(got) != 1 || got[0] != text {
		t.Errorf("配置要求保留全文时应返回整段，实际 %v", got)
	}
}

// TestSplitEmpty 验证空输入。
func TestSplitEmpty(t *testing.T) {
	if got := SplitResponse("", DefaultSplitConfig()); len(got) != 0 {
		t.Errorf("空输入应返回空，实际 %v", got)
	}
	if got := SplitResponse("   ", DefaultSplitConfig()); len(got) != 0 {
		t.Errorf("纯空白应返回空，实际 %v", got)
	}
}

// TestSplitProtectKaomoji 验证颜文字保护。
func TestSplitProtectKaomoji(t *testing.T) {
	cfg := SplitConfig{
		Enable:         true,
		MaxLength:      6,
		MaxSentenceNum: 1,
		MaxSplitNum:    5,
		ProtectKaomoji: true,
	}
	text := "你好呀（｡･ω･｡）今天天气不错"
	got := SplitResponse(text, cfg)
	// 颜文字不应该被拆开。
	for _, s := range got {
		if strings.Contains(s, "（") && !strings.Contains(s, "）") {
			t.Errorf("颜文字被拆开: %q", s)
		}
	}
}

// TestCalculateTypingTime 验证打字时间在合理范围。
func TestCalculateTypingTime(t *testing.T) {
	cfg := TypingConfig{Speed: 1, Enabled: true}

	// 极短文本。
	short := CalculateTypingTime("好", cfg)
	if short < 200*1000*1000 { // 200ms in ns
		t.Errorf("最短应为 200ms，实际 %v", short)
	}

	// 很长文本不应超过上限。
	long := CalculateTypingTime(strings.Repeat("字", 5000), cfg)
	if long.Seconds() > 6.1 {
		t.Errorf("最长应为 6 秒，实际 %v", long)
	}

	// 关闭时应该是 0。
	if d := CalculateTypingTime("你好", TypingConfig{Enabled: false}); d != 0 {
		t.Errorf("关闭时应为 0，实际 %v", d)
	}
}

// TestCleanReplyText 验证清理模型输出的噪声。
func TestCleanReplyText(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`"你好"`, "你好"},
		{"“你好”", "你好"},
		{"「你好」", "你好"},
		{"回复：你好", "你好"},
		{"回复: 你好", "你好"},
		{"  你好  ", "你好"},
		{"你好", "你好"},
	}

	for _, c := range cases {
		if got := CleanReplyText(c.in); got != c.want {
			t.Errorf("CleanReplyText(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// TestCleanReplyTextStripsInstructionEcho 是「不调用工具，直接回。」的兜底回归。
//
// 历史事故：Replyer 请求末尾误用了 Planner 的提醒，模型把它当指令照抄，
// 产出「不调用工具，直接回就行。\n\n<正文>」并直接发进了群里（12.7% 的回复）。
// 代码侧已改用正确尾注，这里是最后一道防线：已有脏数据、或别的模型偶然模仿，
// 也不能让这行指令发出去。
func TestCleanReplyTextStripsInstructionEcho(t *testing.T) {
	cases := []struct{ in, want string }{
		// 典型形态：第一行是回声，空行后是正文。
		{"不调用工具，直接回就行。\n\n行，那这锅巴我就踏实收着了", "行，那这锅巴我就踏实收着了"},
		{"不调用工具，直接回。\n\n一粒顶三行", "一粒顶三行"},
		{"不用调用工具\n\n直接聊", "直接聊"},
		{"用reply回复：\n\n好的", "好的"},
		{"不需要工具，直接说\n正文在后", "正文在后"},
		{"不用搜，先回\n行", "行"},
		// 回声后面还有多层空行，也要能清。
		{"不调用工具\n\n\n正文", "正文"},
	}

	for _, c := range cases {
		in := c.in
		if c.want == "" {
			// 未填 want 的行只是用来验证「不会 panic、结果非空且不含回声」。
			got := CleanReplyText(in)
			if got == "" {
				t.Errorf("CleanReplyText(%q) 返回空串", in)
			}
			continue
		}
		if got := CleanReplyText(in); got != c.want {
			t.Errorf("CleanReplyText(%q) = %q，期望 %q", in, got, c.want)
		}
	}
}

// TestCleanReplyTextKeepsEchoWithoutBody 锁住「没有正文就不动手」。
//
// 模型可能只说了这一句（或者群友真的在聊「不调用工具」这件事）。
// 删掉之后什么都不剩，那就不是回声，而是一条完整发言 —— 原样保留。
func TestCleanReplyTextKeepsEchoWithoutBody(t *testing.T) {
	for _, in := range []string{
		"不调用工具，直接回就行。",
		"不调用工具",
		"不调用工具，直接回就行。\n\n",
		"不调用工具，直接回就行。\n   ",
	} {
		if got := CleanReplyText(in); got == "" {
			t.Errorf("CleanReplyText(%q) 不该清成空串", in)
		}
	}

	// 正文里提到这个词（不是开头）绝不能动。
	for _, in := range []string{
		"你说不调用工具？那是后台的事",
		"行\n不调用工具也挺好",
	} {
		if got := CleanReplyText(in); got != in {
			t.Errorf("CleanReplyText(%q) = %q，正文不该被改", in, got)
		}
	}
}

func utf8Len(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}
