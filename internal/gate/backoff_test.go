package gate

import (
	"math"
	"testing"
)

// TestBackoffStartsAfterStartCount 验证连续空闲达到起点后才开始退避。
//
// 默认 start_count = 2：第 1 次空闲不退避，第 2 次开始退避。
func TestBackoffStartsAfterStartCount(t *testing.T) {
	b := NewBackoff(DefaultBackoffConfig())
	const now = 1000.0

	// 第 1 次空闲：count=1 < startCount=2，不应退避。
	b.RecordCycleResult("tool_pause:wait", true, now)
	if delay, _ := b.ShouldDelay(1, true, now); delay {
		t.Error("第 1 次空闲不应退避")
	}

	// 第 2 次空闲：count=2，应该退避。
	b.RecordCycleResult("tool_pause:wait", true, now)
	shouldDelay, seconds := b.ShouldDelay(1, true, now)
	if !shouldDelay {
		t.Error("第 2 次空闲应该退避")
	}
	// base=15, exponent = 2-2 = 0 → 15 秒。
	if seconds != 15 {
		t.Errorf("退避秒数 = %v，期望 15", seconds)
	}
}

// TestBackoffExponentialGrowth 验证退避时间指数增长。
//
// base=15, cap=300
// n=2 → 15, n=3 → 30, n=4 → 60, n=5 → 120, n=6 → 240, n=7 → 300(封顶)
func TestBackoffExponentialGrowth(t *testing.T) {
	cases := []struct {
		count int
		want  float64
	}{
		{2, 15},
		{3, 30},
		{4, 60},
		{5, 120},
		{6, 240},
		{7, 300}, // 15*32=480 超上限，封顶 300
		{8, 300},
	}

	for _, c := range cases {
		b := NewBackoff(DefaultBackoffConfig())
		const now = 1000.0
		// 连续触发 c 次空闲。
		for i := 0; i < c.count; i++ {
			b.RecordCycleResult("planner_no_tool_end", true, now)
		}
		_, seconds := b.ShouldDelay(1, true, now)
		if seconds != c.want {
			t.Errorf("count=%d 退避 = %v，期望 %v", c.count, seconds, c.want)
		}
	}
}

// TestBackoffResetsOnNonIdle 验证非空闲结束会重置计数。
func TestBackoffResetsOnNonIdle(t *testing.T) {
	b := NewBackoff(DefaultBackoffConfig())
	const now = 1000.0

	b.RecordCycleResult("tool_pause:wait", true, now)
	b.RecordCycleResult("tool_pause:wait", true, now)
	if b.Count() != 2 {
		t.Fatalf("计数 = %d，期望 2", b.Count())
	}

	// 回复了，说明不是空闲。
	b.RecordCycleResult("reply", true, now)
	if b.Count() != 0 {
		t.Errorf("回复后计数应清零，实际 %d", b.Count())
	}
	if shouldDelay, _ := b.ShouldDelay(1, true, now); shouldDelay {
		t.Error("重置后不应退避")
	}
}

// TestBackoffPrivateNeverBacksOff 验证私聊不做退避。
//
// 私聊里每条消息都是对机器人说的，退避没有意义。
func TestBackoffPrivateNeverBacksOff(t *testing.T) {
	b := NewBackoff(DefaultBackoffConfig())
	const now = 1000.0

	for i := 0; i < 5; i++ {
		b.RecordCycleResult("tool_pause:wait", false, now)
	}
	if shouldDelay, _ := b.ShouldDelay(1, false, now); shouldDelay {
		t.Error("私聊不应退避")
	}
}

// TestBackoffBypassByPendingCount 验证攒够消息就绕过退避。
//
// 默认 bypass_pending_count = 6。
func TestBackoffBypassByPendingCount(t *testing.T) {
	b := NewBackoff(DefaultBackoffConfig())
	const now = 1000.0

	for i := 0; i < 3; i++ {
		b.RecordCycleResult("tool_pause:wait", true, now)
	}

	// 5 条还不够。
	if shouldDelay, _ := b.ShouldDelay(5, true, now); !shouldDelay {
		t.Error("5 条未达绕过阈值，应仍在退避")
	}

	// 6 条达到阈值，绕过。
	if shouldDelay, _ := b.ShouldDelay(6, true, now); shouldDelay {
		t.Error("6 条达到绕过阈值，不应再退避")
	}
}

// TestBackoffExpiresOverTime 验证退避到期后自动解除。
func TestBackoffExpiresOverTime(t *testing.T) {
	b := NewBackoff(DefaultBackoffConfig())
	const now = 1000.0

	b.RecordCycleResult("tool_pause:wait", true, now)
	b.RecordCycleResult("tool_pause:wait", true, now)

	shouldDelay, seconds := b.ShouldDelay(1, true, now)
	if !shouldDelay {
		t.Fatal("应该在退避中")
	}

	// 时间推进到退避结束之后。
	later := now + seconds + 1
	if shouldDelay, _ := b.ShouldDelay(1, true, later); shouldDelay {
		t.Error("退避到期后不应再延迟")
	}
}

// TestIsIdleCycleReason 验证空闲结束原因的判定。
func TestIsIdleCycleReason(t *testing.T) {
	idle := []string{
		"planner_no_tool_end",
		"planner_wait_rest",
		"tool_pause:wait",
		"tool_stop_after_execution",
	}
	for _, r := range idle {
		if !IsIdleCycleReason(r) {
			t.Errorf("%q 应算作空闲", r)
		}
	}

	notIdle := []string{
		"reply",
		"planner_interrupted",
		"max_rounds",
		"error",
		"",
	}
	for _, r := range notIdle {
		if IsIdleCycleReason(r) {
			t.Errorf("%q 不应算作空闲", r)
		}
	}
}

// TestDynamicGateEmptyWindowBlocked 验证空窗口时概率不够就不放行。
func TestDynamicGateEmptyWindowBlocked(t *testing.T) {
	g := NewDynamicGate()
	const now = 1000.0

	// 空窗口，keepRatio 会走 normalized 分支。
	// frequency=0.1 时 keepRatio=0.1，静态表阈值约 0.605。
	decision := g.Evaluate(0.1, 0.1, now)
	if decision.ShouldTrigger {
		t.Errorf("概率 0.1 低于阈值 %.3f，不应放行", decision.Threshold)
	}
}

// TestDynamicGateHighFrequencyAllows 验证频率 1.0 时全部放行。
func TestDynamicGateHighFrequencyAllows(t *testing.T) {
	g := NewDynamicGate()
	const now = 1000.0

	// frequency >= 1.0 → keepRatio = 1.0 → 阈值 0。
	decision := g.Evaluate(0.001, 1.0, now)
	if !decision.ShouldTrigger {
		t.Errorf("频率 1.0 时任何概率都应放行，阈值=%.3f", decision.Threshold)
	}
	if decision.Threshold != 0 {
		t.Errorf("阈值 = %v，期望 0", decision.Threshold)
	}
}

// TestDynamicGateClosedRoundCountsOnce 验证同一虚拟轮次只计一次。
func TestDynamicGateClosedRoundCountsOnce(t *testing.T) {
	g := NewDynamicGate()
	const now = 1000.0

	// 同一轮次内多次记录，只应保留一次（覆盖概率）。
	g.RecordProactiveDemand(0.3, now)
	g.RecordProactiveDemand(0.5, now+1)
	g.RecordProactiveDemand(0.7, now+2)

	if len(g.proactiveScores) != 1 {
		t.Fatalf("虚拟轮次内应只有 1 条评分，实际 %d", len(g.proactiveScores))
	}
	if g.proactiveScores[0].probability != 0.7 {
		t.Errorf("应保留最后一次概率 0.7，实际 %v", g.proactiveScores[0].probability)
	}
}

// TestDynamicGateNewRoundAfterClose 验证关闭轮次后开始新轮次。
func TestDynamicGateNewRoundAfterClose(t *testing.T) {
	g := NewDynamicGate()
	const now = 1000.0

	g.RecordProactiveDemand(0.3, now)
	g.CloseRound()

	// 超过 virtualRoundSeconds 后算新轮次。
	g.RecordProactiveDemand(0.6, now+virtualRoundSeconds+1)

	if len(g.proactiveScores) != 2 {
		t.Errorf("应有 2 条评分，实际 %d", len(g.proactiveScores))
	}
}

// TestDynamicGateForcedTurnExcluded 验证 @ 强制触发占用额度。
//
// 强制触发会消耗掉一部分目标回复额度，让主动回复更保守。
func TestDynamicGateForcedTurnExcluded(t *testing.T) {
	g := NewDynamicGate()
	const now = 1000.0

	// 记录一次强制触发。
	g.RecordForcedTurn(now)

	if len(g.forcedExpectations) != 1 {
		t.Fatalf("应有 1 条强制触发记录，实际 %d", len(g.forcedExpectations))
	}
	if g.forcedExpectations[0].value != forcedTurnExpectedReplies {
		t.Errorf("强制触发预计值 = %v，期望 %v",
			g.forcedExpectations[0].value, forcedTurnExpectedReplies)
	}
}

// TestDynamicGateWindowPrune 验证窗口外记录被清理。
func TestDynamicGateWindowPrune(t *testing.T) {
	g := NewDynamicGate()

	g.RecordProactiveDemand(0.5, 1000)
	g.RecordReply(1000)
	g.RecordForcedTurn(1000)

	// 推进到窗口之外。
	future := 1000 + replyWindowSeconds + 10
	g.prune(future)

	if len(g.proactiveScores) != 0 {
		t.Errorf("评分应被清理，实际 %d 条", len(g.proactiveScores))
	}
	if len(g.replyTimes) != 0 {
		t.Errorf("回复记录应被清理，实际 %d 条", len(g.replyTimes))
	}
	if len(g.forcedExpectations) != 0 {
		t.Errorf("强制触发记录应被清理，实际 %d 条", len(g.forcedExpectations))
	}
}

// TestDynamicGateImmutability 验证前馈反馈只收紧不放宽的核心性质。
//
// 实际回复数少于目标时，反馈因子 > 1 但被 clamp 到 1，
// 所以不会因为「还没回够」而放宽阈值。
func TestDynamicGateImmutability(t *testing.T) {
	g := NewDynamicGate()
	const now = 1000.0

	// 建立窗口：10 条评分 0.5，预计 5 次回复。
	for i := 0; i < 10; i++ {
		g.RecordProactiveDemand(0.5, now+float64(i)*virtualRoundSeconds)
		g.CloseRound()
	}

	// 实际一次都没回复（actual=0，远低于目标）。
	decision := g.Evaluate(0.5, 0.5, now+100)

	// keepRatio 不应超过 1。
	if decision.KeepRatio > 1.0 {
		t.Errorf("保留比例 = %v 超过 1", decision.KeepRatio)
	}
	if decision.KeepRatio < 0 {
		t.Errorf("保留比例 = %v 小于 0", decision.KeepRatio)
	}
	// 阈值必须在 [0,1]。
	if decision.Threshold < 0 || decision.Threshold > 1 {
		t.Errorf("阈值 = %v 越界", decision.Threshold)
	}
}

// TestStaticThresholdTable 验证静态阈值表的关键取值。
func TestStaticThresholdTable(t *testing.T) {
	cases := []struct {
		ratio float64
		want  float64
	}{
		{0.0, 1.0},
		{1.0, 0.0},
		{0.5, 0.336},
		{0.9, 0.206},
	}

	for _, c := range cases {
		got := staticThreshold(c.ratio)
		if math.Abs(got-c.want) > 1e-9 {
			t.Errorf("staticThreshold(%v) = %v，期望 %v", c.ratio, got, c.want)
		}
	}
}

// TestStaticThresholdInterpolation 验证插值。
//
// ratio=0.05 应落在 1.0 与 0.605 之间。
func TestStaticThresholdInterpolation(t *testing.T) {
	got := staticThreshold(0.05)
	if got <= 0.605 || got >= 1.0 {
		t.Errorf("staticThreshold(0.05) = %v，期望在 0.605~1.0 之间", got)
	}
}
