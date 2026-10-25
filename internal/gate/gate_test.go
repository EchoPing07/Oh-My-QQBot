package gate

import (
	"math"
	"testing"
)

// TestEstimateReplyProbabilityNeutral 验证中性输入下的概率。
//
// 全特征为 0 时，logit 就等于截距 -1.2005，
// 概率 = sigmoid(-1.2005) ≈ 0.2315。
func TestEstimateReplyProbabilityNeutral(t *testing.T) {
	got := EstimateReplyProbability(LikelihoodInput{
		SecondsSinceBotMessage: 0,
	})

	want := 1.0 / (1.0 + math.Exp(1.2005))
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("概率 = %v，期望 %v", got, want)
	}
	if got < 0.22 || got > 0.24 {
		t.Errorf("概率 = %v，期望在 0.22~0.24 附近", got)
	}
}

// TestEstimateReplyProbabilityMention 验证「提到机器人名字」显著提高概率。
func TestEstimateReplyProbabilityMention(t *testing.T) {
	base := EstimateReplyProbability(LikelihoodInput{SecondsSinceBotMessage: 0})
	mentioned := EstimateReplyProbability(LikelihoodInput{
		MentionBot:             true,
		SecondsSinceBotMessage: 0,
	})

	if mentioned <= base {
		t.Errorf("提到机器人后概率应上升：%v -> %v", base, mentioned)
	}
	// 权重 1.5852，概率应该明显提高。
	if mentioned < 0.5 {
		t.Errorf("提到机器人后概率 = %v，期望 > 0.5", mentioned)
	}
}

// TestEstimateReplyProbabilityAtOther 验证「@ 了别人」显著降低概率。
//
// 这是有道理的：@ 别人说明在跟别人说话，机器人不该插嘴。
func TestEstimateReplyProbabilityAtOther(t *testing.T) {
	base := EstimateReplyProbability(LikelihoodInput{SecondsSinceBotMessage: 0})
	atOther := EstimateReplyProbability(LikelihoodInput{
		AtOther:                true,
		SecondsSinceBotMessage: 0,
	})

	if atOther >= base {
		t.Errorf("@ 别人后概率应下降：%v -> %v", base, atOther)
	}
	if atOther > 0.1 {
		t.Errorf("@ 别人后概率 = %v，期望 < 0.1", atOther)
	}
}

// TestEstimateReplyProbabilityQuestion 验证「带问号」提高概率。
func TestEstimateReplyProbabilityQuestion(t *testing.T) {
	base := EstimateReplyProbability(LikelihoodInput{SecondsSinceBotMessage: 0})
	question := EstimateReplyProbability(LikelihoodInput{
		HasQuestionMark:        true,
		SecondsSinceBotMessage: 0,
	})

	if question <= base {
		t.Errorf("带问号后概率应上升：%v -> %v", base, question)
	}
}

// TestEstimateReplyProbabilityPlaceholderOnly 验证「纯媒体占位」降低概率。
func TestEstimateReplyProbabilityPlaceholderOnly(t *testing.T) {
	base := EstimateReplyProbability(LikelihoodInput{SecondsSinceBotMessage: 0})
	placeholder := EstimateReplyProbability(LikelihoodInput{
		PlaceholderOnly:        true,
		SecondsSinceBotMessage: 0,
	})

	if placeholder >= base {
		t.Errorf("纯媒体占位后概率应下降：%v -> %v", base, placeholder)
	}
}

// TestEstimateReplyProbabilityPendingCount 验证待处理条数提高概率。
//
// 消息越多说明聊得越热，插嘴的可能性越大。
func TestEstimateReplyProbabilityPendingCount(t *testing.T) {
	few := EstimateReplyProbability(LikelihoodInput{
		PendingCount:           1,
		SecondsSinceBotMessage: 0,
	})
	many := EstimateReplyProbability(LikelihoodInput{
		PendingCount:           10,
		SecondsSinceBotMessage: 0,
	})

	if many <= few {
		t.Errorf("待处理条数增加后概率应上升：%v -> %v", few, many)
	}
}

// TestEstimateReplyProbabilityPendingCap 验证待处理条数封顶在 20。
func TestEstimateReplyProbabilityPendingCap(t *testing.T) {
	at20 := EstimateReplyProbability(LikelihoodInput{
		PendingCount:           20,
		SecondsSinceBotMessage: 0,
	})
	at100 := EstimateReplyProbability(LikelihoodInput{
		PendingCount:           100,
		SecondsSinceBotMessage: 0,
	})

	if math.Abs(at20-at100) > 1e-9 {
		t.Errorf("超过 20 后概率不应再变：%v vs %v", at20, at100)
	}
}

// TestEstimateReplyProbabilitySelfRatio 验证近期自身发言占比提高概率。
//
// 逻辑：自己刚说过话，说明正处在对话里，更容易继续说。
func TestEstimateReplyProbabilitySelfRatio(t *testing.T) {
	low := EstimateReplyProbability(LikelihoodInput{
		RecentSelfRatio:        0.0,
		SecondsSinceBotMessage: 0,
	})
	high := EstimateReplyProbability(LikelihoodInput{
		RecentSelfRatio:        1.0,
		SecondsSinceBotMessage: 0,
	})

	if high <= low {
		t.Errorf("自身发言占比高时概率应上升：%v -> %v", low, high)
	}
}

// TestEstimateReplyProbabilityAlwaysInRange 验证概率永远在 [0,1]。
func TestEstimateReplyProbabilityAlwaysInRange(t *testing.T) {
	cases := []LikelihoodInput{
		{},
		{MentionBot: true, AtOther: true, HasQuestionMark: true, PlaceholderOnly: true,
			RecentSelfRatio: 1, RecentMessageCount: 1000, SecondsSinceBotMessage: 1e9, PendingCount: 1000},
		{RecentSelfRatio: -5, RecentMessageCount: -100, SecondsSinceBotMessage: -100, PendingCount: -100},
	}

	for i, c := range cases {
		p := EstimateReplyProbability(c)
		if p < 0 || p > 1 || math.IsNaN(p) {
			t.Errorf("case %d: 概率 = %v 越界", i, p)
		}
	}
}

// TestTriggerThreshold 验证频率到阈值的折算。
//
// threshold = max(1, ceil(1/frequency))
func TestTriggerThreshold(t *testing.T) {
	cases := []struct {
		freq float64
		want int
	}{
		{1.0, 1},  // 1/1 = 1
		{0.5, 2},  // 1/0.5 = 2
		{0.25, 4}, // 1/0.25 = 4
		{0.1, 10}, // 1/0.1 = 10
		{0.3, 4},  // 1/0.3 = 3.33 → ceil 4
		{0.0, 0},  // 频率 0 → 不触发
		{0.001, 1000},
	}

	for _, c := range cases {
		if got := TriggerThreshold(c.freq); got != c.want {
			t.Errorf("TriggerThreshold(%v) = %d，期望 %d", c.freq, got, c.want)
		}
	}
}

// TestFrequencyGateReachesThreshold 验证待处理数达标即触发。
func TestFrequencyGateReachesThreshold(t *testing.T) {
	g := NewFrequencyGate()

	result := g.Evaluate(5, 5, 10, 0, 100)
	if !result.ShouldTrigger() {
		t.Errorf("待处理 5 达到阈值 5，应触发；实际 %v（%s）", result.Decision, result.Detail)
	}
}

// TestFrequencyGateWaitsBelowThreshold 验证未达标且无空窗时等待。
func TestFrequencyGateWaitsBelowThreshold(t *testing.T) {
	g := NewFrequencyGate()

	// 待处理 1，阈值 5，平均间隔 10 秒，刚收到消息（空窗 0）。
	result := g.Evaluate(1, 5, 10, 100, 100)
	if result.ShouldTrigger() {
		t.Errorf("待处理 1 未达阈值 5，不应触发；实际 %v", result.Decision)
	}
	if result.Decision != DecisionDelay {
		t.Errorf("应返回延迟检查，实际 %v（%s）", result.Decision, result.Detail)
	}
	if result.DelaySeconds <= 0 {
		t.Errorf("延迟秒数应 > 0，实际 %v", result.DelaySeconds)
	}
}

// TestFrequencyGatePureSilenceNeverTriggers 验证纯沉默永不触发。
//
// 这是原版一个关键的安全设计：不能因为长时间没人说话就自己冒出来。
func TestFrequencyGatePureSilenceNeverTriggers(t *testing.T) {
	g := NewFrequencyGate()

	// 待处理 0，即便空窗极长也不触发。
	result := g.Evaluate(0, 5, 10, 0, 1e9)
	if result.ShouldTrigger() {
		t.Errorf("pending=0 时绝不应触发；实际触发了（%s）", result.Detail)
	}
}

// TestFrequencyGateIdleCompensation 验证空窗补偿能触发。
//
// 待处理 1，阈值 3，平均间隔 30 秒。
// 空窗 65 秒 → 折算 2.17 条 → 1 + 2.17 = 3.17 >= 3 → 触发。
//
// 注意平均间隔有 30 秒下限，所以这里必须用 >= 30 的值，
// 否则会被夹到 30（见 TestFrequencyGateMinIntervalFloor）。
func TestFrequencyGateIdleCompensation(t *testing.T) {
	g := NewFrequencyGate()

	result := g.Evaluate(1, 3, 30, 0, 65)
	if !result.ShouldTrigger() {
		t.Errorf("空窗补偿应触发；实际 %v（%s）", result.Decision, result.Detail)
	}
}

// TestFrequencyGateIdleCompensationCap 验证空窗折算封顶在 threshold-1。
//
// 折算量封顶意味着：必须有至少一条真实新消息才可能触发。
func TestFrequencyGateIdleCompensationCap(t *testing.T) {
	g := NewFrequencyGate()

	// 待处理 1，阈值 10，空窗极长。
	// 折算量最多 9，1+9=10 >= 10，仍然只会触发（因为 pending >= 1）。
	result := g.Evaluate(1, 10, 1, 0, 1e9)
	if !result.ShouldTrigger() {
		t.Errorf("有 1 条消息且空窗极长时应触发；实际 %v（%s）", result.Decision, result.Detail)
	}

	// 但待处理 0 时，无论空窗多长都不能触发。
	result = g.Evaluate(0, 10, 1, 0, 1e9)
	if result.ShouldTrigger() {
		t.Error("pending=0 且空窗极长时不应触发")
	}
}

// TestFrequencyGateMinIntervalFloor 验证平均间隔下限 30 秒。
//
// 避免统计值过小时，空窗补偿把沉默过快折算成消息。
func TestFrequencyGateMinIntervalFloor(t *testing.T) {
	g := NewFrequencyGate()

	// 平均间隔 1 秒（低于下限 30），空窗 60 秒。
	// 按下限 30 秒算 → 折算 2 条。
	// 待处理 1，阈值 10 → 1+2=3 < 10，不触发。
	result := g.Evaluate(1, 10, 1, 0, 60)
	if result.ShouldTrigger() {
		t.Errorf("应按下限 30 秒折算，实际触发了（%s）", result.Detail)
	}
}
