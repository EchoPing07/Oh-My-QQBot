package prompt

// 本文件覆盖 blocks.go 里代码生成的提示词块。

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// TestQueryMemoryRuleGroupVsPrivate 防的是「群聊里用了私聊措辞」：
// 两套措辞的作用不一样，群聊版要强调不要把私聊隐私带进群。
func TestQueryMemoryRuleGroupVsPrivate(t *testing.T) {
	group := QueryMemoryRule(true)
	private := QueryMemoryRule(false)

	if group == "" || private == "" {
		t.Fatal("两套规则都不应为空")
	}
	if group == private {
		t.Error("群聊与私聊的记忆检索规则必须不同")
	}

	if !strings.HasPrefix(group, "- query_memory()：") {
		t.Errorf("群聊规则前缀 = %q", head(group, 30))
	}
	if !strings.HasPrefix(private, "- query_memory()：") {
		t.Errorf("私聊规则前缀 = %q", head(private, 30))
	}

	// 群聊版必须有隐私提醒。
	if !strings.Contains(group, "私聊") || !strings.Contains(group, "隐私") {
		t.Errorf("群聊规则缺少隐私提醒: %q", group)
	}
	// 私聊版必须有「之前/上次/还记得吗」这类信号词。
	for _, signal := range []string{"之前", "上次", "还记得吗"} {
		if !strings.Contains(private, signal) {
			t.Errorf("私聊规则缺少信号词 %q: %q", signal, private)
		}
	}
	// 群聊版不应该出现私聊版的信号词提醒。
	if strings.Contains(group, "还记得吗") {
		t.Error("群聊规则不应包含私聊的信号词提醒")
	}
}

// TestBotAliasNotice 覆盖别名说明的构造与空值。
func TestBotAliasNotice(t *testing.T) {
	cases := []struct {
		name    string
		aliases []string
		want    string
	}{
		{"空别名返回空串", nil, ""},
		{"全是空白返回空串", []string{"", "   ", "\t"}, ""},
		{"单个别名", []string{"小木"}, "，还有人叫你小木"},
		{"多个别名用顿号连接", []string{"小木", "木木子"}, "，还有人叫你小木、木木子"},
		{"空白别名被过滤", []string{" 小木 ", "", "木木子"}, "，还有人叫你小木、木木子"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := BotAliasNotice(c.aliases); got != c.want {
				t.Errorf("BotAliasNotice(%v) = %q，期望 %q", c.aliases, got, c.want)
			}
		})
	}
}

// TestGroupChatAttentionBlock 覆盖群聊/私聊措辞与空提示词。
func TestGroupChatAttentionBlock(t *testing.T) {
	const (
		groupPrompt   = "群聊通用提示"
		privatePrompt = "私聊通用提示"
	)

	t.Run("群聊", func(t *testing.T) {
		got := GroupChatAttentionBlock(true, groupPrompt, privatePrompt)
		want := "在该聊天中的注意事项：\n通用注意事项：\n" + groupPrompt + "\n"
		if got != want {
			t.Errorf("结果 = %q，期望 %q", got, want)
		}
	})

	t.Run("私聊", func(t *testing.T) {
		got := GroupChatAttentionBlock(false, groupPrompt, privatePrompt)
		want := "在该聊天中的注意事项：\n通用注意事项：\n" + privatePrompt + "\n"
		if got != want {
			t.Errorf("结果 = %q，期望 %q", got, want)
		}
	})

	t.Run("空提示词返回空串", func(t *testing.T) {
		if got := GroupChatAttentionBlock(true, "", privatePrompt); got != "" {
			t.Errorf("群聊提示词为空时应返回空串，实际 %q", got)
		}
		if got := GroupChatAttentionBlock(false, groupPrompt, "  \t "); got != "" {
			t.Errorf("私聊提示词只有空白时应返回空串，实际 %q", got)
		}
	})

	t.Run("提示词首尾空白被裁掉", func(t *testing.T) {
		got := GroupChatAttentionBlock(true, "  有提示  \n", privatePrompt)
		want := "在该聊天中的注意事项：\n通用注意事项：\n有提示\n"
		if got != want {
			t.Errorf("结果 = %q，期望 %q", got, want)
		}
	})
}

// TestBehaviorStylePrompt 覆盖行为风格的空值兜底。
func TestBehaviorStylePrompt(t *testing.T) {
	if got := BehaviorStylePrompt("  爱吐槽  "); got != "爱吐槽" {
		t.Errorf("BehaviorStylePrompt() = %q，期望去空白后返回 %q", got, "爱吐槽")
	}

	for _, empty := range []string{"", "   ", "\n\t"} {
		got := BehaviorStylePrompt(empty)
		if got == "" {
			t.Errorf("输入 %q 时应返回兜底文案而不是空串", empty)
		}
		if !strings.Contains(got, "普通人") {
			t.Errorf("兜底文案 = %q，期望包含 %q", got, "普通人")
		}
	}
}

// TestIdentityPrompt 覆盖人格块的结构与兜底。
//
// 结构必须与参考实现一致（maisaka_generator_base.py:101）：
//
//	你的名字是{bot_name}{bot_aliases}。\n{personality}
//
// 少了名字与别名，群友喊「肥鱼」时模型不认这是自己。
func TestIdentityPrompt(t *testing.T) {
	got := IdentityPrompt("木木", []string{"肥鱼", "大肥鱼"}, "  爱玩梗  ")
	if !strings.HasPrefix(got, "你的名字是木木") {
		t.Errorf("应以「你的名字是木木」开头，实际 %q", got)
	}
	// replyer 侧的别名分隔是逗号（不是 Planner 侧的顿号），别统一。
	if !strings.Contains(got, "也有人叫你肥鱼,大肥鱼") {
		t.Errorf("别名措辞不对：%q", got)
	}
	if !strings.HasSuffix(got, "\n爱玩梗") {
		t.Errorf("人格应换行跟在名字后面：%q", got)
	}

	// 没别名时不该出现「也有人叫你」。
	if got := IdentityPrompt("木木", nil, "爱玩梗"); strings.Contains(got, "也有人叫你") {
		t.Errorf("没别名时不该出现别名说明：%q", got)
	}

	// 空白别名要过滤掉，不能拼出「，也有人叫你、」。
	got = IdentityPrompt("木木", []string{" ", "肥鱼", ""}, "爱玩梗")
	if !strings.Contains(got, "也有人叫你肥鱼") || strings.Contains(got, "肥鱼,") {
		t.Errorf("空白别名没被过滤：%q", got)
	}

	// 名字与人格都空时要有兜底，不能拼出「你的名字是。」
	for _, c := range []struct{ name, personality string }{
		{"", ""}, {" ", "  "}, {"木木", ""},
	} {
		got := IdentityPrompt(c.name, nil, c.personality)
		if got == "" {
			t.Errorf("输入 %q/%q 时应返回兜底文案", c.name, c.personality)
		}
		if strings.Contains(got, "你的名字是。") {
			t.Errorf("名字为空时不能拼出「你的名字是。」：%q", got)
		}
	}

	// 人格空时用参考实现的兜底「是人类。」
	if got := IdentityPrompt("木木", nil, "  "); !strings.HasSuffix(got, "是人类。") {
		t.Errorf("人格为空时应兜底成「是人类。」，实际 %q", got)
	}
}

// TestConstBlocksNonEmpty 校验常量块没被误删成空串：
// 删了它们提示词会突然少一段，模型行为会漂移。
func TestConstBlocksNonEmpty(t *testing.T) {
	checks := []struct {
		name string
		got  string
		want string
	}{
		{"PlannerFinalReminder", PlannerFinalReminder, "请你进行分析"},
		{"ReplyerOutputInstruction", ReplyerOutputInstruction, "只输出发言内容"},
		{"ReplyerReplyInstruction", ReplyerReplyInstruction, "只输出实际要发言的内容"},
	}
	for _, c := range checks {
		if !strings.Contains(c.got, c.want) {
			t.Errorf("%s = %q，期望包含 %q", c.name, c.got, c.want)
		}
	}
}

// TestReplyerReplyInstructionHasNoToolWords 是一条回归锁。
//
// Replyer 侧没有工具概念，尾注里一旦出现「不调用工具」「reply」这类词，
// 模型就会把它当成待回应的指令照抄，产出「不调用工具，直接回。\n\n<正文>」。
// 曾经因为误用了 PlannerFinalReminder 实际泄漏 5/10（见 AGENTS.md）。
func TestReplyerReplyInstructionHasNoToolWords(t *testing.T) {
	for _, banned := range []string{"不调用工具", "reply", "工具", "查询"} {
		if strings.Contains(ReplyerReplyInstruction, banned) {
			t.Errorf("ReplyerReplyInstruction 不得包含 %q：%q", banned, ReplyerReplyInstruction)
		}
	}
}

// TestReplyerStyleHint 验证篇幅要求的映射。
func TestReplyerStyleHint(t *testing.T) {
	if got := ReplyerStyleHint("简短表达"); !strings.Contains(got, "简短的回复") {
		t.Errorf("简短表达映射不对：%q", got)
	}
	if got := ReplyerStyleHint("长回复"); !strings.Contains(got, "较为详细的评论") {
		t.Errorf("长回复映射不对：%q", got)
	}
	// 「正常回复」与未知风格都返回空串，表示不追加这条消息。
	// 这里不能返回「请正常回复」之类的话：多一条多余指令会稀释末尾块。
	for _, s := range []string{"正常回复", "", "  ", "不知所云"} {
		if got := ReplyerStyleHint(s); got != "" {
			t.Errorf("%q 应返回空串，实际 %q", s, got)
		}
	}
}

// TestTargetMessageBlock 验证目标消息块的措辞与截断。
func TestTargetMessageBlock(t *testing.T) {
	got := TargetMessageBlock("小明", "m-1", "今天好热")
	for _, want := range []string{
		"你想要回复的消息是 小明 发送的 msg_id为 m-1 的消息",
		"不要把其他历史消息当成当前回复对象",
		"- 发言内容：今天好热",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少 %q，实际 %q", want, got)
		}
	}

	// 没有 msg_id 时不产出块：编一个空目标比不写更糟。
	if got := TargetMessageBlock("小明", "  ", "内容"); got != "" {
		t.Errorf("空 msg_id 应返回空串，实际 %q", got)
	}

	// 空正文要有占位，不能留下「- 发言内容：」这种半句。
	if got := TargetMessageBlock("小明", "m-1", "  "); !strings.Contains(got, "[无可见文本内容]") {
		t.Errorf("空正文应给占位：%q", got)
	}

	// 超长正文按 rune 截到 300，且不能劈出非法 UTF-8。
	long := strings.Repeat("很长的正文", 200)
	got = TargetMessageBlock("小明", "m-1", long)
	if !utf8.ValidString(got) {
		t.Error("截断后出现非法 UTF-8")
	}
	if utf8.RuneCountInString(got) > 400 {
		t.Errorf("截断没生效，长度 %d", utf8.RuneCountInString(got))
	}
}

// TestSelfTargetMessageBlock 验证「回复自己发过的消息」分支。
//
// 这一段存在的意义是防止自我对话：模型把自己的上一条发言当成群友说的。
func TestSelfTargetMessageBlock(t *testing.T) {
	got := SelfTargetMessageBlock("木木", "m-2", "我刚说的那句")
	for _, want := range []string{
		"你想要补充说明你自己（木木）",
		"不要把你自己的发言当成别人的发言",
		"- 你之前的发言内容：我刚说的那句",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少 %q，实际 %q", want, got)
		}
	}
}

// TestReplyerFinalUserMessage 验证末尾块的拼装顺序。
func TestReplyerFinalUserMessage(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 44, 39, 0, time.UTC)
	got := ReplyerFinalUserMessage(CurrentTimeLine(now), TargetMessageBlock("小明", "m-1", "火别太旺"))

	// 顺序：当前时间 → 目标消息 → 只输出正文。
	tIdx := strings.Index(got, "当前时间：2026-10-24 12:44:39")
	gIdx := strings.Index(got, "你想要回复的消息是")
	iIdx := strings.Index(got, ReplyerReplyInstruction)
	if tIdx < 0 || gIdx < 0 || iIdx < 0 {
		t.Fatalf("三段不齐：%q", got)
	}
	if !(tIdx < gIdx && gIdx < iIdx) {
		t.Errorf("顺序应为 时间(第%d) → 目标(第%d) → 指令(第%d)", tIdx, gIdx, iIdx)
	}

	// 没有目标块时也要能拼（私聊直接回消息、或 msg_id 查不到）。
	got = ReplyerFinalUserMessage(CurrentTimeLine(now), "")
	if strings.Contains(got, "你想要回复的消息是") {
		t.Errorf("无目标时不该出现目标块：%q", got)
	}
	if !strings.HasSuffix(got, ReplyerReplyInstruction) {
		t.Errorf("指令必须留在最末：%q", got)
	}
}

// TestCurrentTimeLine 验证时间格式（不带星期，与参考实现一致）。
func TestCurrentTimeLine(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 44, 39, 0, time.UTC)
	want := "当前时间：2026-10-24 12:44:39"
	if got := CurrentTimeLine(now); got != want {
		t.Errorf("CurrentTimeLine() = %q，期望 %q", got, want)
	}
	// 零值时间不能渲染成 0001-01-01，否则模型会以为现在是古代。
	if got := CurrentTimeLine(time.Time{}); strings.Contains(got, "0001-") {
		t.Errorf("零值时间应回退到当前时刻，实际 %q", got)
	}
}

// TestIdentityPromptFallbackMatchesConfigDefault 守住 IdentityPrompt 的兜底名
// 与 config.Default().Bot.Nickname 是同一个值。
//
// 两处各写一遍默认昵称，改了一处忘了另一处不会编译报错，
// 只会在「昵称被配成空」这个罕见路径上给出不一致的身份提示。
func TestIdentityPromptFallbackMatchesConfigDefault(t *testing.T) {
	// prompt 包刻意不依赖 config（它只做模板渲染），
	// 所以这里直接断言常量值，值本身由 config 包的测试守着。
	if DefaultBotName != "木木" {
		t.Errorf("DefaultBotName = %q", DefaultBotName)
	}
	got := IdentityPrompt("", nil, "爱玩梗")
	if !strings.HasPrefix(got, "你的名字是木木") {
		t.Errorf("兜底名没生效: %q", got)
	}
	// 正常路径必须用调用方传入的名字，不能被兜底覆盖。
	got = IdentityPrompt("肥鱼", nil, "爱玩梗")
	if !strings.HasPrefix(got, "你的名字是肥鱼") {
		t.Errorf("传入名字没生效: %q", got)
	}
}

// TestJoinChineseItems 逐条对照 maisaka_generator_base.py:266 的
// _join_chinese_items：一项原样、两项用「 和 」、三项以上最后一项前用「 和 」。
//
// 不能简单 strings.Join("、")：图片说明里会变成「图片A、图片B」，
// 少了中文习惯的连词，读起来像列举清单而不是一句话。
func TestJoinChineseItems(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{nil, ""},
		{[]string{"", "  "}, ""},
		{[]string{"甲"}, "甲"},
		{[]string{"甲", "乙"}, "甲 和 乙"},
		{[]string{"甲", "乙", "丙"}, "甲、乙 和 丙"},
		{[]string{" 甲 ", "", "乙"}, "甲 和 乙"},
	}
	for _, c := range cases {
		if got := JoinChineseItems(c.in); got != c.want {
			t.Errorf("JoinChineseItems(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// TestReplyerFinalUserMessageWithAttachments 守附件段的位置与省略规则。
//
// 附件说明属于末尾块（参考实现把它 append 进同一份 sections 再拼），
// 必须落在「只输出正文」之前；没有附件时整段省略，不能留空行。
func TestReplyerFinalUserMessageWithAttachments(t *testing.T) {
	got := ReplyerFinalUserMessage(
		CurrentTimeLine(time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)),
		TargetMessageBlock("小明", "m-1", "火别太旺"),
		"【额外发送内容参考】\n当前文字回复后还会另外发送图片。",
	)
	attach := strings.Index(got, "【额外发送内容参考】")
	inst := strings.Index(got, ReplyerReplyInstruction)
	if attach < 0 {
		t.Fatalf("附件段没进末尾块：%q", got)
	}
	if inst < 0 || attach > inst {
		t.Errorf("附件段必须落在「只输出正文」之前：%q", got)
	}

	// 空附件段整段省略。
	got = ReplyerFinalUserMessage("当前时间：12:00:00", "目标", "   ")
	if strings.Contains(got, "【额外发送内容参考】") {
		t.Errorf("空附件不该留下标题：%q", got)
	}
	if !strings.HasSuffix(got, ReplyerReplyInstruction) {
		t.Errorf("指令必须留在最末：%q", got)
	}
}
