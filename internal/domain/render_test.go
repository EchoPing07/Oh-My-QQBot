package domain

// 本文件覆盖 domain 的渲染函数。
// 重点守住 AGENTS.md 第 20 条：BuildPlainText 必须用 voice/file 段里的文本，
// 不能硬编码 [语音] / [文件]，否则平台给的 ASR 转写与文件名全丢。

import (
	"strings"
	"testing"
)

// TestBuildPlainText 表驱动覆盖每种消息段的渲染结果。
func TestBuildPlainText(t *testing.T) {
	cases := []struct {
		name     string
		segments []Segment
		want     string
	}{
		{
			name:     "纯文本",
			segments: []Segment{Text("你好")},
			want:     "你好",
		},
		{
			name:     "多段文本按空格拼接",
			segments: []Segment{Text("你好"), Text("在吗")},
			want:     "你好 在吗",
		},
		{
			name:     "空白文本被丢弃",
			segments: []Segment{Text("   "), Text("喵")},
			want:     "喵",
		},
		{
			name:     "at 有名字",
			segments: []Segment{At("u1", "小明"), Text("出来")},
			want:     "@小明 出来",
		},
		{
			name:     "at 无名字退化用 ID",
			segments: []Segment{At("u1", "")},
			want:     "@u1",
		},
		{
			name:     "图片占位",
			segments: []Segment{Image("http://x/1.png")},
			want:     "[图片]",
		},
		{
			name:     "表情包占位",
			segments: []Segment{Emoji("http://x/1.gif")},
			want:     "[表情包]",
		},
		{
			name:     "语音无 ASR 用占位",
			segments: []Segment{{Type: SegmentVoice, URL: "http://x/1.silk"}},
			want:     "[语音]",
		},
		{
			// 这是修过的真 bug：语音段的 ASR 转写放在 seg.Text 里，必须用它。
			name:     "语音有 ASR 用转写文本",
			segments: []Segment{{Type: SegmentVoice, URL: "http://x/1.silk", Text: "今天吃什么"}},
			want:     "今天吃什么",
		},
		{
			name:     "文件有名字用文件名",
			segments: []Segment{{Type: SegmentFile, URL: "http://x/a.pdf", Text: "报告.pdf"}},
			want:     "报告.pdf",
		},
		{
			name:     "文件无名字用占位",
			segments: []Segment{{Type: SegmentFile, URL: "http://x/a.pdf"}},
			want:     "[文件]",
		},
		{
			name:     "合并转发占位",
			segments: []Segment{{Type: SegmentForward}},
			want:     "[合并转发]",
		},
		{
			// 引用不进入正文，否则模型会看到一串消息 ID。
			name:     "引用段被忽略",
			segments: []Segment{Reply("m0"), Text("回你")},
			want:     "回你",
		},
		{
			name:     "未知段型被忽略",
			segments: []Segment{{Type: SegmentType("nope")}, Text("喵")},
			want:     "喵",
		},
		{
			name:     "空段列表渲染空串",
			segments: nil,
			want:     "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := BuildPlainText(c.segments); got != c.want {
				t.Errorf("BuildPlainText() = %q，期望 %q", got, c.want)
			}
		})
	}
}

// TestBuildPlainTextVoiceDoesNotLeakPlaceholder 单独强调那条回归：
// 有 ASR 时输出里不能出现硬编码的 [语音]。
func TestBuildPlainTextVoiceDoesNotLeakPlaceholder(t *testing.T) {
	got := BuildPlainText([]Segment{
		{Type: SegmentVoice, Text: "  明天九点开会  "},
	})
	if got != "明天九点开会" {
		t.Fatalf("语音段渲染 = %q，期望 %q", got, "明天九点开会")
	}
	if strings.Contains(got, "[语音]") {
		t.Error("语音段有 ASR 文本时不应出现 [语音] 占位")
	}
}

// TestIsMediaPlaceholderOnly 覆盖「只有媒体没有文字」的判定。
func TestIsMediaPlaceholderOnly(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"空串不算", "", false},
		{"纯空白不算", "   ", false},
		{"单图片", "[图片]", true},
		{"图片加表情", "[图片] [表情包]", true},
		{"带前缀的图片占位", "[图片: 1 张]", true},
		{"语音占位", "[语音]", true},
		{"文件占位", "[文件]", true},
		{"带正文不算", "[图片] 你看这个", false},
		{"纯文字不算", "你好", false},
		{"未闭合的占位不算", "[图片", false},
		// 三个以上的占位拼接：以前靠外层循环碰巧也能过，
		// 现在改成了显式循环，必须确认没把它弄坏。
		{"三个占位", "[图片][语音][文件]", true},
		{"占位中间夹正文", "[图片] 中间 [表情包]", false},
		// 开头不是占位、但中间含占位：不能算纯占位。
		{"文字在前占位在后", "看 [图片]", false},
		// 占位后跟半个方括号：无法完整解析成占位链。
		{"占位后跟残括号", "[图片] [表情包", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsMediaPlaceholderOnly(c.text); got != c.want {
				t.Errorf("IsMediaPlaceholderOnly(%q) = %v，期望 %v", c.text, got, c.want)
			}
		})
	}
}

// TestContainsQuestion 覆盖中英文问号判定。
func TestContainsQuestion(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"在吗？", true},
		{"are you ok?", true},
		{"好的", false},
		{"", false},
	}
	for _, c := range cases {
		if got := ContainsQuestion(c.text); got != c.want {
			t.Errorf("ContainsQuestion(%q) = %v，期望 %v", c.text, got, c.want)
		}
	}
}

// TestContainsAtOther 覆盖「@ 了别人」的判定。
func TestContainsAtOther(t *testing.T) {
	cases := []struct {
		name     string
		text     string
		hasAtBot bool
		want     bool
	}{
		{"@ 了机器人不算别人", "@木木 你好", true, false},
		{"没 @ 机器人但文本有 @", "@小明 你好", false, true},
		{"完全没 @", "你好", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ContainsAtOther(c.text, c.hasAtBot); got != c.want {
				t.Errorf("ContainsAtOther(%q, %v) = %v，期望 %v", c.text, c.hasAtBot, got, c.want)
			}
		})
	}
}

// TestRenderAt 覆盖 at 段渲染的回退。
func TestRenderAt(t *testing.T) {
	if got := RenderAt(At("u1", "小明")); got != "@小明" {
		t.Errorf("RenderAt() = %q，期望 %q", got, "@小明")
	}
	if got := RenderAt(At("u1", "")); got != "@u1" {
		t.Errorf("无名时 RenderAt() = %q，期望 %q", got, "@u1")
	}
}

// TestNormalizeSpace 覆盖空白归一化。
func TestNormalizeSpace(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"  你好   世界  ", "你好 世界"},
		{"a\tb\nc", "a b c"},
		{"   ", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := NormalizeSpace(c.in); got != c.want {
			t.Errorf("NormalizeSpace(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}
