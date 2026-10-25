package agent

// loop.go 的测试。
//
// Loop 本身依赖 llm 客户端与 store，端到端由 internal/mock 的跨包场景测试兜；
// 这里只覆盖它内部那些「错了不报错、只会悄悄改变行为」的纯函数：
//   - extractReplyText：返回 ok=false 会让 reply 工具成功却不记为已回复
//   - buildLikelihoodInput：判定用的输入错了，门控就会在错误的时机开口
//   - State 的 wait 计数：错了会导致「wait 退化成 stop」

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/llm"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/tool"
)

// TestExtractReplyText 验证从 reply 工具结果里取文本。
//
// 注意：现在的 reply 工具不写 Metadata["reply_text"]（正文由 replyer 生成后
// 直接发出），所以正常路径走的是「没有 metadata 但工具成功」这一支。
// 这里同时覆盖两条分支，防止将来改工具时漏改 loop。
func TestExtractReplyText(t *testing.T) {
	cases := []struct {
		name   string
		result tool.Result
		want   string
		wantOK bool
	}{
		{
			name:   "带 metadata 取正文",
			result: tool.Result{Content: "已发送回复。", Metadata: map[string]string{"reply_text": "今天好热"}},
			want:   "今天好热",
			wantOK: true,
		},
		{
			name:   "无 metadata 但成功",
			result: tool.Result{Content: "已发送回复。"},
			want:   "",
			wantOK: true,
		},
		{
			// 工具失败时不能当成「已回复」，否则本轮会被误判为成功发送。
			name:   "工具失败",
			result: tool.ErrorResult("发送失败: 网络错误"),
			want:   "",
			wantOK: false,
		},
		{
			name:   "失败时即使有 metadata 也不算",
			result: tool.Result{IsError: true, Metadata: map[string]string{"reply_text": "x"}},
			want:   "",
			wantOK: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := extractReplyText(c.result)
			if ok != c.wantOK {
				t.Errorf("ok = %v，期望 %v", ok, c.wantOK)
			}
			if got != c.want {
				t.Errorf("text = %q，期望 %q", got, c.want)
			}
		})
	}
}

// TestFirstNonEmpty 验证取值顺序：返回第一个非空白值，且去除首尾空白。
func TestFirstNonEmpty(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want string
	}{
		{"第一个就非空", []string{"a", "b"}, "a"},
		{"跳过空串", []string{"", "b"}, "b"},
		{"跳过纯空白", []string{"  \n\t ", "b"}, "b"},
		{"全部为空", []string{"", "  "}, ""},
		{"没有参数", nil, ""},
		{"去除首尾空白", []string{"  a  "}, "a"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := firstNonEmpty(c.in...); got != c.want {
				t.Errorf("= %q，期望 %q", got, c.want)
			}
		})
	}
}

// TestToolCallNames 验证工具名提取（日志用）。
func TestToolCallNames(t *testing.T) {
	if got := toolCallNames(nil); len(got) != 0 {
		t.Errorf("空输入应返回空切片，实际 %v", got)
	}

	calls := []llm.ToolCall{
		{Function: llm.ToolCallFunction{Name: "reply"}},
		{Function: llm.ToolCallFunction{Name: "wait"}},
	}
	got := toolCallNames(calls)
	if len(got) != 2 || got[0] != "reply" || got[1] != "wait" {
		t.Errorf("= %v，期望 [reply wait]", got)
	}
}

// TestTruncateForLog 验证日志截断。
//
// 两个约束：换行要压平（否则一条日志变多行）、超长要截断（否则刷屏）。
func TestTruncateForLog(t *testing.T) {
	// 短文本原样返回（换行仍要压平）。
	if got := truncateForLog("短", 10); got != "短" {
		t.Errorf("短文本不该变，实际 %q", got)
	}
	if got := truncateForLog("第一行\n第二行", 100); strings.Contains(got, "\n") {
		t.Errorf("换行应被压平，实际 %q", got)
	}

	// 正好等于上限：不截断。
	exact := strings.Repeat("a", 10)
	if got := truncateForLog(exact, 10); got != exact {
		t.Errorf("正好等于上限时不该截断，实际 %q", got)
	}

	// 超长：保留前 n 字节并加省略号。
	got := truncateForLog(strings.Repeat("a", 20), 10)
	if got != strings.Repeat("a", 10)+"..." {
		t.Errorf("截断结果 = %q", got)
	}

	// 已知行为：这里按字节切，中文会被劈开。
	// 日志是给人看的，不是喂给模型的 JSON，所以不构成发送失败风险；
	// 这条断言只是把这个事实写下来，避免有人误以为是 rune 截断。
	cut := truncateForLog(strings.Repeat("木", 10), 4)
	if utf8.ValidString(cut) {
		t.Logf("注意：按字节截断居然切在了合法边界上（%q）", cut)
	}
	if len(cut) != 4+3 {
		t.Errorf("应保留 4 字节再追加省略号，实际 %d 字节", len(cut))
	}
}

// TestStateWaitLifecycle 验证 State 的 wait 状态机。
func TestStateWaitLifecycle(t *testing.T) {
	st := &State{}

	// 初始：没有 wait 请求。
	if requested, sec, reason := st.WaitState(); requested || sec != 0 || reason != "" {
		t.Errorf("初始状态应为空，实际 (%v,%v,%q)", requested, sec, reason)
	}

	// 模拟 runtime.Wait 写入的三个字段（runtime 直接用字段，没有 setter）。
	st.waitRequested = true
	st.waitSeconds = 30
	st.waitReason = "等对方补充"

	requested, sec, reason := st.WaitState()
	if !requested || sec != 30 || reason != "等对方补充" {
		t.Errorf("WaitState = (%v,%v,%q)", requested, sec, reason)
	}

	// ClearWait 只清 wait，不动其它状态。
	st.stopped = true
	st.stopReason = "测试"
	st.ClearWait()
	if requested, sec, reason := st.WaitState(); requested || sec != 0 || reason != "" {
		t.Errorf("ClearWait 后应为空，实际 (%v,%v,%q)", requested, sec, reason)
	}
	if !st.stopped || st.stopReason != "测试" {
		t.Errorf("ClearWait 不该动 stop 状态")
	}

	// ResetForNewTurn 把 stop 与 wait 都清掉 —— 每轮都必须从干净状态开始，
	// 否则上一轮的 stop 会让这一轮直接结束。
	st.waitRequested = true
	st.waitSeconds = 5
	st.waitReason = "再等等"
	st.ResetForNewTurn()
	if requested, _, _ := st.WaitState(); requested {
		t.Errorf("ResetForNewTurn 后 wait 应被清掉")
	}
	if st.stopped || st.stopReason != "" {
		t.Errorf("ResetForNewTurn 后 stop 应被清掉")
	}
}

// TestStateConsecutiveWaits 验证连续 wait 计数。
//
// 计数涨不上去的后果：wait 永远不会被判定为「连续太多次」，
// 于是 Planner 可以无限 wait，整轮思考永远不产出回复。
func TestStateConsecutiveWaits(t *testing.T) {
	st := &State{}
	if got := st.ConsecutiveWaits(); got != 0 {
		t.Errorf("初始应为 0，实际 %d", got)
	}

	st.IncrWaits()
	st.IncrWaits()
	if got := st.ConsecutiveWaits(); got != 2 {
		t.Errorf("= %d，期望 2", got)
	}

	st.ResetWaits()
	if got := st.ConsecutiveWaits(); got != 0 {
		t.Errorf("ResetWaits 后应为 0，实际 %d", got)
	}

	// ResetForNewTurn 不该清零连续 wait 计数：
	// 计数是「跨轮」概念，每轮清零等于让上限永远达不到。
	st.IncrWaits()
	st.ResetForNewTurn()
	if got := st.ConsecutiveWaits(); got != 1 {
		t.Errorf("ResetForNewTurn 不该清连续 wait 计数，实际 %d", got)
	}
}

// TestMessageIDs 验证消息 ID 提取，顺序必须与输入一致（门控靠它去重计数）。
func TestMessageIDs(t *testing.T) {
	if got := messageIDs(nil); len(got) != 0 {
		t.Errorf("空输入应返回空切片，实际 %v", got)
	}

	msgs := []*domain.Message{
		{MessageID: "m1"},
		{MessageID: "m2"},
		{MessageID: ""},
	}
	got := messageIDs(msgs)
	if len(got) != 3 {
		t.Fatalf("长度 = %d，期望 3", len(got))
	}
	if got[0] != "m1" || got[1] != "m2" || got[2] != "" {
		t.Errorf("顺序或内容不对：%v", got)
	}
}

// TestBuildLikelihoodInput 验证回复概率估算的输入构造。
//
// 这些布尔量直接决定门控觉得「该不该开口」。算错了不会报错，
// 只会让机器人在被 @ 时不爱回、或者在纯表情刷屏时抢话。
func TestBuildLikelihoodInput(t *testing.T) {
	now := time.Date(2026, 10, 21, 13, 0, 0, 0, time.UTC)

	cases := []struct {
		name             string
		pending, history []*domain.Message
		botReplies       []BotReply
	}{
		// 只占位，具体断言在下面的 t.Run 里分组写，避免一条表太长。
		{name: "汇总冒烟", pending: []*domain.Message{{PlainText: "在吗？"}}},
	}
	for _, c := range cases {
		in := buildLikelihoodInput(c.pending, c.history, c.botReplies, now)
		if !in.HasQuestionMark {
			t.Errorf("%s: 问句应被识别", c.name)
		}
	}

	t.Run("被 @ 与提及时标记", func(t *testing.T) {
		in := buildLikelihoodInput([]*domain.Message{
			{MessageID: "m1", IsAt: true, PlainText: "木木你好"},
		}, nil, nil, now)
		if !in.MentionBot {
			t.Errorf("IsAt 应置 MentionBot")
		}
		if in.PlaceholderOnly {
			t.Errorf("有正文时不该是纯占位")
		}

		in = buildLikelihoodInput([]*domain.Message{
			{MessageID: "m1", IsMentioned: true, PlainText: "木木在吗"},
		}, nil, nil, now)
		if !in.MentionBot {
			t.Errorf("IsMentioned 应置 MentionBot")
		}
	})

	t.Run("问号检测中英文都算", func(t *testing.T) {
		for _, text := range []string{"在吗?", "在吗？"} {
			in := buildLikelihoodInput([]*domain.Message{{PlainText: text}}, nil, nil, now)
			if !in.HasQuestionMark {
				t.Errorf("%q 应被识别为问句", text)
			}
		}
		in := buildLikelihoodInput([]*domain.Message{{PlainText: "我在"}}, nil, nil, now)
		if in.HasQuestionMark {
			t.Errorf("无问号时不该置位")
		}
	})

	t.Run("纯媒体占位", func(t *testing.T) {
		// 只有 [图片] 的消息：模型看到的信息量极低，回复价值小。
		in := buildLikelihoodInput([]*domain.Message{{PlainText: "[图片]"}}, nil, nil, now)
		if !in.PlaceholderOnly {
			t.Errorf("[图片] 应算纯占位")
		}
		// 图片 + 一句文字就不算纯占位。
		in = buildLikelihoodInput([]*domain.Message{{PlainText: "[图片] 你看这个"}}, nil, nil, now)
		if in.PlaceholderOnly {
			t.Errorf("带正文时不该算纯占位")
		}
		// 空批次不该算纯占位（否则门控会以为「全是表情」）。
		in = buildLikelihoodInput(nil, nil, nil, now)
		if in.PlaceholderOnly {
			t.Errorf("空批次不该算纯占位")
		}
	})

	t.Run("@ 了别人", func(t *testing.T) {
		// 没有 at 段但正文里有 @ 符号：算「@ 了别人」。
		in := buildLikelihoodInput([]*domain.Message{{PlainText: "@张三 你看"}}, nil, nil, now)
		if !in.AtOther {
			t.Errorf("正文里的 @ 应置 AtOther")
		}
		// 自己就是被 @ 的那个：不算「在跟别人说话」。
		in = buildLikelihoodInput([]*domain.Message{{IsAt: true, PlainText: "@木木 你看"}}, nil, nil, now)
		if in.AtOther {
			t.Errorf("@ 机器人自己时不该置 AtOther")
		}
	})

	t.Run("最近活跃度与机器人占比", func(t *testing.T) {
		history := []*domain.Message{
			{ReceivedAt: now.Add(-1 * time.Minute)},  // 窗口内
			{ReceivedAt: now.Add(-4 * time.Minute)},  // 窗口内
			{ReceivedAt: now.Add(-10 * time.Minute)}, // 窗口外
		}
		botReplies := []BotReply{
			{At: now.Add(-30 * time.Minute)}, // 窗口外
			{At: now.Add(-2 * time.Minute)},  // 窗口内
		}
		// 注意：botReplies 必须按时间升序传（最后一个视为最新）。
		// runtime 里是 append 进去的，天然有序；乱序传会让「距上次发言」算错。

		in := buildLikelihoodInput(nil, history, botReplies, now)
		// 窗口内共 3 条（2 条外部 + 1 条自己）。
		if in.RecentMessageCount != 3 {
			t.Errorf("最近条数 = %d，期望 3", in.RecentMessageCount)
		}
		if diff := in.RecentSelfRatio - 1.0/3.0; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("机器人占比 = %v，期望 %.4f", in.RecentSelfRatio, 1.0/3.0)
		}
		// 距最近一次自己发言 2 分钟 = 120 秒。
		if diff := in.SecondsSinceBotMessage - 120; diff > 1e-6 || diff < -1e-6 {
			t.Errorf("距上次发言 = %v 秒，期望 120", in.SecondsSinceBotMessage)
		}

		// 从未发过言：给 3600 秒兜底，避免 0 被当成「刚说过」。
		in = buildLikelihoodInput(nil, nil, nil, now)
		if in.SecondsSinceBotMessage != 3600 {
			t.Errorf("无历史发言时应兜底 3600 秒，实际 %v", in.SecondsSinceBotMessage)
		}
		if in.RecentSelfRatio != 0 || in.RecentMessageCount != 0 {
			t.Errorf("无历史时活跃度应为 0")
		}
	})

	t.Run("待处理条数", func(t *testing.T) {
		pending := []*domain.Message{{}, {}, {}}
		in := buildLikelihoodInput(pending, nil, nil, now)
		if in.PendingCount != len(pending) {
			t.Errorf("待处理条数 = %d，期望 %d", in.PendingCount, len(pending))
		}
	})
}

// TestBuildGateInput 验证门控输入的透传。
func TestBuildGateInput(t *testing.T) {
	now := time.Date(2026, 10, 21, 13, 0, 0, 0, time.UTC)
	pending := []*domain.Message{
		{MessageID: "m1", PlainText: "在吗？"},
		{MessageID: "m2", PlainText: "有事问你"},
	}
	history := []*domain.Message{
		{MessageID: "h1", ReceivedAt: now.Add(-time.Minute)},
	}
	botReplies := []BotReply{{At: now.Add(-30 * time.Second)}}

	in := buildGateInput(true, 0.8, pending, history, botReplies, true, now, 42.5, 1000.5)

	if in.PendingCount != 2 {
		t.Errorf("待处理条数 = %d，期望 2", in.PendingCount)
	}
	if len(in.PendingMessageIDs) != 2 || in.PendingMessageIDs[0] != "m1" {
		t.Errorf("待处理 ID = %v", in.PendingMessageIDs)
	}
	if !in.IsGroup {
		t.Errorf("IsGroup 没透传")
	}
	if in.Frequency != 0.8 {
		t.Errorf("Frequency = %v，期望 0.8", in.Frequency)
	}
	if !in.HasForcedTrigger {
		t.Errorf("HasForcedTrigger 没透传")
	}
	if in.AverageInterval != 42.5 || in.LastExternalAt != 1000.5 {
		t.Errorf("间隔/最后时刻没透传：%v %v", in.AverageInterval, in.LastExternalAt)
	}
	// Now 是「距 epoch 的秒数」，不是 Unix 时间戳以外的东西。
	if diff := in.Now - float64(now.Unix()); diff > 1 || diff < -1 {
		t.Errorf("Now = %v，期望约 %d", in.Now, now.Unix())
	}
	// Likelihood 必须由 pending 派生，而不是留零值。
	if !in.Likelihood.HasQuestionMark {
		t.Errorf("问号检测没有透传到 Likelihood")
	}
	if in.Likelihood.PendingCount != 2 {
		t.Errorf("Likelihood.PendingCount = %d，期望 2", in.Likelihood.PendingCount)
	}
}

// TestLoopRunTurnRequiresRegistry 验证没有工具注册表时直接报错而不是 panic。
//
// 这条路径在配置错误时会被走到，报错比空指针崩栈更有用。
func TestLoopRunTurnRequiresRegistry(t *testing.T) {
	l := &Loop{}
	_, err := l.RunTurn(context.Background(), TurnInput{SessionID: "s"}, nil, &State{})
	if err == nil {
		t.Fatal("没有注册表时应报错")
	}
	if !strings.Contains(err.Error(), "注册表") {
		t.Errorf("错误信息应说明原因，实际 %v", err)
	}
}
