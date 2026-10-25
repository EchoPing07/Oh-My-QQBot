package agent

// render.go 的测试。
//
// 这一层是纯函数，但它是模型「看到什么」的唯一入口，错一个字模型就会误判。
// 尤其是按 rune 截断这一条：按 byte 截会把汉字劈成非法 UTF-8，
// 整条 JSON 请求都发不出去（见 AGENTS.md 第 21 条），必须有断言守着。

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/chat"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
)

// renderTestTime 固定一个时刻，避免测试依赖运行时的当前时间。
var renderTestTime = time.Date(2026, 10, 21, 13, 5, 0, 0, time.UTC)

// newRenderTestMessage 构造一条消息，PlainText 走生产代码的渲染路径。
//
// 刻意不手写 PlainText：RenderMessage 读的就是这个字段，
// 手写会让测试与「真实解码出来的消息」脱节。
func newRenderTestMessage(id, user string, segs ...domain.Segment) *domain.Message {
	return &domain.Message{
		MessageID:  id,
		UserID:     user,
		UserName:   user,
		Segments:   segs,
		PlainText:  domain.BuildPlainText(segs),
		ReceivedAt: renderTestTime,
	}
}

// TestRenderMessageSegments 逐种消息段验证渲染结果。
//
// 用表驱动是因为「语音有 asr / 无 asr」「文件有名字 / 无名字」这类
// 分支很多，逐条写容易漏掉一半。
func TestRenderMessageSegments(t *testing.T) {
	cases := []struct {
		name   string
		segs   []domain.Segment
		want   string
		absent string
	}{
		{
			name: "纯文本",
			segs: []domain.Segment{domain.Text("今天好热")},
			want: "今天好热",
		},
		{
			name: "图片",
			segs: []domain.Segment{domain.Image("https://example.com/a.png")},
			want: "[图片]",
		},
		{
			name: "表情",
			segs: []domain.Segment{domain.Emoji("https://example.com/e.gif")},
			want: "[表情包]",
		},
		{
			name: "@ 某人",
			segs: []domain.Segment{domain.At("u-2", "小红"), domain.Text("快看")},
			want: "@小红 快看",
		},
		{
			// 引用段本身不进正文（否则模型会把被引正文当成「用户说的内容」）。
			// 引用关系走 quote 属性，被引正文走 Additional 快照，
			// 两者都不该以 [引用…] 的形式出现在正文里。
			name:   "引用不进正文",
			segs:   []domain.Segment{domain.Reply("m-0"), domain.Text("就是这个")},
			want:   "[回复了一条消息，但原消息已无法访问]\n就是这个",
			absent: "[引用",
		},
		{
			name: "语音带 asr",
			segs: []domain.Segment{{Type: domain.SegmentVoice, Text: "我晚上不来了"}},
			want: "我晚上不来了",
		},
		{
			name:   "语音无 asr",
			segs:   []domain.Segment{{Type: domain.SegmentVoice}},
			want:   "[语音]",
			absent: "我晚上不来了",
		},
		{
			name: "文件带名字",
			segs: []domain.Segment{{Type: domain.SegmentFile, Text: "周报.pdf"}},
			want: "周报.pdf",
		},
		{
			name: "文件无名字",
			segs: []domain.Segment{{Type: domain.SegmentFile}},
			want: "[文件]",
		},
		{
			name: "多段混合",
			segs: []domain.Segment{
				domain.At("u-3", "小李"),
				domain.Text("看这张图"),
				domain.Image("https://example.com/b.png"),
				domain.Emoji("https://example.com/c.gif"),
			},
			want: "@小李 看这张图 [图片] [表情包]",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := newRenderTestMessage("m-1", "小明", c.segs...)
			got := RenderMessage(msg)

			// 前缀格式固定，逐条都校验一次，防止有人「顺手」加闭合标签。
			prefix := `<message msg_id="m-1" time="13:05:00" user="小明">` + "\n"
			if strings.Contains(c.name, "引用") {
				// 带引用的消息多一个 quote 属性，位置在 msg_id 之后、time 之前。
				prefix = `<message msg_id="m-1" quote="m-0" time="13:05:00" user="小明">` + "\n"
			}
			if !strings.HasPrefix(got, prefix) {
				t.Fatalf("前缀不符，实际 = %q", got)
			}
			body := strings.TrimPrefix(got, prefix)
			if body != c.want {
				t.Errorf("正文 = %q，期望 %q", body, c.want)
			}
			if c.absent != "" && strings.Contains(body, c.absent) {
				t.Errorf("正文不该包含 %q，实际 %q", c.absent, body)
			}
		})
	}
}

// TestRenderMessageEscapesAndFallsBackToUserID 验证属性转义与用户名为空的兜底。
func TestRenderMessageEscapesAndFallsBackToUserID(t *testing.T) {
	msg := &domain.Message{
		MessageID:  `m"<1>`,
		UserID:     "u-9",
		UserName:   "",
		PlainText:  "hi",
		ReceivedAt: renderTestTime,
	}
	got := RenderMessage(msg)

	if strings.Contains(got, `m"<1>`) {
		t.Errorf("msg_id 没被转义：%q", got)
	}
	if !strings.Contains(got, `user="u-9"`) {
		t.Errorf("用户名为空时应回退到 UserID，实际 %q", got)
	}
	// 没有群名片时不该出现 group_card 属性。
	if strings.Contains(got, "group_card") {
		t.Errorf("无群名片时不该有 group_card：%q", got)
	}
}

// TestRenderMessages 验证拼接：空切片返回空串、单条不带分隔、多条按顺序用换行分隔。
func TestRenderMessages(t *testing.T) {
	if got := RenderMessages(nil); got != "" {
		t.Errorf("空切片应返回空串，实际 %q", got)
	}
	if got := RenderMessages([]*domain.Message{}); got != "" {
		t.Errorf("空切片应返回空串，实际 %q", got)
	}

	m1 := newRenderTestMessage("m-1", "甲", domain.Text("第一句"))
	m2 := newRenderTestMessage("m-2", "乙", domain.Text("第二句"))

	single := RenderMessages([]*domain.Message{m1})
	if single != RenderMessage(m1) {
		t.Errorf("单条渲染不该多出分隔符：%q", single)
	}
	if strings.HasSuffix(single, "\n") {
		t.Errorf("单条渲染不该以换行结尾：%q", single)
	}

	both := RenderMessages([]*domain.Message{m1, m2})
	parts := strings.Split(both, "\n")
	if len(parts) != 4 {
		// 每条消息本身以 ">\n" 结尾，所以两条拼起来是 4 行。
		t.Fatalf("行数 = %d，期望 4，实际 %q", len(parts), both)
	}
	if !strings.Contains(parts[0], `msg_id="m-1"`) || !strings.Contains(parts[2], `msg_id="m-2"`) {
		t.Errorf("顺序不对，实际 %q", both)
	}
	// 分隔符是单个换行：出现连续两个换行说明多了空行。
	if strings.Contains(both, "\n\n") {
		t.Errorf("多条之间只该有一个换行：%q", both)
	}
}

// TestTruncateMessageTiers 逐档校验截断长度与替换文案。
//
// 档位数值照抄参考实现 _default_truncate_func，抄错了会让「老消息更短」
// 这个设计悄悄失衡。
func TestTruncateMessageTiers(t *testing.T) {
	cases := []struct {
		percentile float64
		wantLimit  int
		wantSuffix string
	}{
		{0.0, 400, "......（内容太长了）"},
		{0.29, 400, "......（内容太长了）"},
		{0.3, 200, "......（内容太长了）"},
		{0.49, 200, "......（内容太长了）"},
		{0.5, 100, "......（有点记不清了）"},
		{0.79, 100, "......（有点记不清了）"},
		{0.8, 50, "......（记不清了）"},
		{0.99, 50, "......（记不清了）"},
	}

	for _, c := range cases {
		limit, replacement := truncateLimitFor(c.percentile)
		if limit != c.wantLimit {
			t.Errorf("percentile=%.2f 的档位 = %d，期望 %d", c.percentile, limit, c.wantLimit)
		}
		if replacement != c.wantSuffix {
			t.Errorf("percentile=%.2f 的替换文案 = %q，期望 %q", c.percentile, replacement, c.wantSuffix)
		}
	}
}

// TestRenderMessageAtTruncatesWithoutBreakingRunes 是这一组最重要的断言。
//
// 中文消息按 rune 截断后必须是合法 UTF-8；按 byte 截会把汉字劈成两半，
// 序列化成 JSON 时直接报错，表现为「模型调用失败」，极难定位。
func TestRenderMessageAtTruncatesWithoutBreakingRunes(t *testing.T) {
	long := strings.Repeat("木", 500)
	msg := newRenderTestMessage("m-long", "小明", domain.Text(long))

	cases := []struct {
		percentile float64
		wantBody   int
	}{
		{0.1, 400},
		{0.4, 200},
		{0.6, 100},
		{0.9, 50},
	}

	for _, c := range cases {
		got := RenderMessageAt(msg, c.percentile)
		if !utf8.ValidString(got) {
			t.Fatalf("percentile=%.1f 截断后不是合法 UTF-8", c.percentile)
		}
		body := strings.TrimPrefix(got, MessagePrefix(msg))
		if !strings.HasPrefix(body, strings.Repeat("木", c.wantBody)) {
			t.Errorf("percentile=%.1f 正文开头不对：%q", c.percentile, body)
		}
		// 替换文案跟在截断点后面，长度应正好是 上限 + 文案。
		_, replacement := truncateLimitFor(c.percentile)
		if want := c.wantBody + utf8.RuneCountInString(replacement); utf8.RuneCountInString(body) != want {
			t.Errorf("percentile=%.1f 正文长度 = %d，期望 %d", c.percentile, utf8.RuneCountInString(body), want)
		}
	}
}

// TestTruncateMessageWithUsesRunes 守住「不能按 byte 切」这条线。
//
// 与生产代码里的同名注释对应：这是踩过的坑，不是理论风险。
func TestTruncateMessageWithUsesRunes(t *testing.T) {
	long := strings.Repeat("测试中文", 100) // 400 个 rune、1200 字节
	got := chat.TruncateMessageWith(long, 100, "......（内容太长了）")

	if !utf8.ValidString(got) {
		t.Fatalf("截断结果不是合法 UTF-8：%q", got)
	}
	if want := strings.Repeat("测试中文", 25) + "......（内容太长了）"; got != want {
		t.Errorf("截断结果 = %q，期望 %q", got, want)
	}

	// 短消息一个字节都不该动，也不能被追加替换文案。
	short := "今天好热"
	if got := chat.TruncateMessage(short, 100); got != short {
		t.Errorf("短消息不该被处理，实际 %q", got)
	}

	// maxLen <= 0 表示不截断（调用方用 0 表示「关掉」）。
	if got := chat.TruncateMessageWith(long, 0, "......"); got != long {
		t.Errorf("maxLen=0 时不该截断")
	}

	// 边界：正好等于上限也不截断。
	exact := strings.Repeat("木", 100)
	if got := chat.TruncateMessage(exact, 100); got != exact {
		t.Errorf("正好等于上限时不该截断，实际 %q", got)
	}
	// 超出 1 个 rune 就要截断。
	over := strings.Repeat("木", 101)
	if got := chat.TruncateMessageWith(over, 100, "!"); got != strings.Repeat("木", 100)+"!" {
		t.Errorf("超出 1 个 rune 应截断，实际 %q", got)
	}
}

// TestTimeUserMessageFormat 验证时间提示的格式。
//
// 模型靠这一行知道「现在」，格式变了（比如换成 RFC3339 或中文星期）
// 会让它算错「刚才」「今天」这类相对时间。
func TestTimeUserMessageFormat(t *testing.T) {
	// 2024-01-01 是星期一，用固定日期避免依赖本机时区。
	at := time.Date(2024, 1, 1, 8, 30, 0, 0, time.UTC)
	got := TimeUserMessage(at)

	want := "当前时间：2024-01-01 08:30:00 Monday"
	if got != want {
		t.Errorf("时间提示 = %q，期望 %q", got, want)
	}
}

// TestRenderBotMessage 验证机器人自述的渲染，is_self_message 必须出现。
func TestRenderBotMessage(t *testing.T) {
	got := RenderBotMessage("我在呢", renderTestTime, "木木")

	if !strings.Contains(got, `is_self_message="true"`) {
		t.Errorf("缺少 is_self_message 标记：%q", got)
	}
	if !strings.Contains(got, `user="木木"`) || !strings.Contains(got, `time="13:05:00"`) {
		t.Errorf("属性不全：%q", got)
	}
	if !strings.HasSuffix(got, "\n我在呢") {
		t.Errorf("正文应在最后：%q", got)
	}
	if strings.Contains(got, "msg_id") {
		t.Errorf("机器人自述没有 msg_id，不该凭空造一个：%q", got)
	}
}

// TestResolveMsgID 验证 msg_id 参数的空值兜底。
func TestResolveMsgID(t *testing.T) {
	cases := []struct {
		raw      string
		fallback string
		want     string
	}{
		{"m-1", "m-2", "m-1"},
		{"  m-1  ", "m-2", "m-1"},
		{"", "m-2", "m-2"},
		{"   ", "m-2", "m-2"},
	}
	for _, c := range cases {
		if got := resolveMsgID(c.raw, c.fallback); got != c.want {
			t.Errorf("resolveMsgID(%q, %q) = %q，期望 %q", c.raw, c.fallback, got, c.want)
		}
	}
}

// TestMessagePrefixGroupCard 验证群名片会渲染进 group_card 属性。
//
// group_card 是「用户的群名片」，不是群名 —— 参考实现里它决定模型怎么称呼人。
func TestMessagePrefixGroupCard(t *testing.T) {
	msg := &domain.Message{
		MessageID:    "m-1",
		UserID:       "u-1",
		UserName:     "account-name",
		UserCardName: "群里的昵称",
		PlainText:    "hi",
		ReceivedAt:   renderTestTime,
	}
	got := MessagePrefix(msg)

	if !strings.Contains(got, `group_card="群里的昵称"`) {
		t.Errorf("缺少 group_card 属性：%q", got)
	}
	// 名字里可能带引号，必须转义否则整条属性会被截断。
	msg.UserCardName = `带"引号`
	if got := MessagePrefix(msg); strings.Contains(got, `group_card="带"引号"`) {
		t.Errorf("group_card 没被转义：%q", got)
	}
	// 只有空白的群名片等于没有。
	msg.UserCardName = "   "
	if got := MessagePrefix(msg); strings.Contains(got, "group_card") {
		t.Errorf("空白群名片不该渲染：%q", got)
	}
}

// TestMessagePrefixQuoteAttribute 验证引用渲染成 quote 属性，且位置对齐参考实现。
//
// 参考实现见 maisaka/context/planner_messages.py:44-63：
// 属性顺序是 msg_id → quote → time → user → group_card。
// 引用关系只放属性里，不拼进正文 —— 这正是「引用不进正文」那条约定。
func TestMessagePrefixQuoteAttribute(t *testing.T) {
	msg := &domain.Message{
		MessageID:  "m-1",
		UserID:     "u-1",
		UserName:   "小明",
		Segments:   []domain.Segment{domain.Reply("REFIDX_target"), domain.Text("就是这个")},
		PlainText:  "就是这个",
		ReceivedAt: renderTestTime,
	}
	got := MessagePrefix(msg)

	want := `<message msg_id="m-1" quote="REFIDX_target" time="13:05:00" user="小明">` + "\n"
	if got != want {
		t.Errorf("前缀 = %q，期望 %q", got, want)
	}

	// 多个引用用逗号连接（参考实现 _format_quote_ids 的行为）。
	msg.Segments = []domain.Segment{
		domain.Reply("q-1"), domain.Reply("q-2"), domain.Reply("q-1"), domain.Text("嗯"),
	}
	if got := MessagePrefix(msg); !strings.Contains(got, `quote="q-1,q-2"`) {
		t.Errorf("多引用应去重并用逗号连接：%q", got)
	}

	// 没有引用段时不该出现 quote 属性。
	msg.Segments = []domain.Segment{domain.Text("嗯")}
	if got := MessagePrefix(msg); strings.Contains(got, "quote=") {
		t.Errorf("无引用不该渲染 quote：%q", got)
	}
}

// TestRenderMessageQuotedBody 验证被引用正文拼在正文开头。
//
// 文案照抄参考实现 chat/message_receive/message.py:428-458 的三条分支。
func TestRenderMessageQuotedBody(t *testing.T) {
	mk := func(additional map[string]string) *domain.Message {
		return &domain.Message{
			MessageID:  "m-1",
			UserID:     "u-1",
			UserName:   "小明",
			Segments:   []domain.Segment{domain.Reply("REFIDX_target"), domain.Text("好耶")},
			PlainText:  "好耶",
			Additional: additional,
			ReceivedAt: renderTestTime,
		}
	}

	// 有正文、有发送者：完整形态。
	got := RenderMessage(mk(map[string]string{
		domain.AdditionalQuotedText:   "今晚加班",
		domain.AdditionalQuotedSender: "老板",
	}))
	if !strings.Contains(got, "[回复了老板的消息: 今晚加班]\n好耶") {
		t.Errorf("被引正文渲染不符：%q", got)
	}

	// 只有正文、不知道是谁：省略「X的」，内容仍然给模型。
	got = RenderMessage(mk(map[string]string{domain.AdditionalQuotedText: "今晚加班"}))
	if !strings.Contains(got, "[回复了消息: 今晚加班]") {
		t.Errorf("缺发送者时应省略「X的」：%q", got)
	}

	// 有引用段但正文快照丢了：必须明确告诉模型「看不到」，而不是静默省略。
	got = RenderMessage(mk(nil))
	if !strings.Contains(got, "[回复了一条消息，但原消息已无法访问]") {
		t.Errorf("缺正文时应给出兜底文案：%q", got)
	}

	// 没有引用段时不该多出任何前缀。
	plain := mk(nil)
	plain.Segments = []domain.Segment{domain.Text("好耶")}
	if got := RenderMessage(plain); got != MessagePrefix(plain)+"好耶" {
		t.Errorf("无引用不该加前缀：%q", got)
	}
}

// TestQuoteIDs 验证从段里提取引用目标。
func TestQuoteIDs(t *testing.T) {
	if got := QuoteIDs(nil); got != nil {
		t.Errorf("nil 消息应返回 nil，实际 %v", got)
	}
	msg := &domain.Message{Segments: []domain.Segment{
		domain.Text("hi"), domain.Reply("a"), domain.Reply(""), domain.Reply("a"), domain.Reply("b"),
	}}
	got := QuoteIDs(msg)
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("QuoteIDs = %v，期望 [a b]", got)
	}
}
