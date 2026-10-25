package gate

import "sort"

// 动态门控。
//
// 对应 MaiBot 的 dynamic_gate.py。
//
// 核心思路：让窗口内实际的 reply 次数贴近「不设门控时预计的 reply 次数 × 回复频率」。
// 预计次数来自回复概率，实际次数按 Planner 成功调用 reply 的次数统计。

const (
	// replyWindowSeconds 是统计预计回复数与实际回复数的滑动窗口长度。
	replyWindowSeconds = 3600.0
	// forcedTurnExpectedReplies 是一次 @ 强制触发预计产生的 reply 次数（拟合数据中的实测均值）。
	forcedTurnExpectedReplies = 0.91
	// minWindowScoreCount 是窗口内已评分的批次少于该值时改用静态阈值表。
	minWindowScoreCount = 20
	// virtualRoundSeconds 是估算「不设门控时 Planner 会判定几次」所用的轮次时长。
	//
	// 不设门控时 Planner 并不是每条消息判一次：思考耗时、空闲退避与 wait 会把消息攒成一批
	// （拟合数据中平均每批 2.9 条）。把同一时长内到达的消息归为一个虚拟轮次、只计一次预计回复数；
	// 按 40 秒切分时，切出的轮次数与拟合数据中的实际 Planner 轮次数最接近。
	virtualRoundSeconds = 40.0
	// maxCountedMessageIDs 是已计入预计回复数的消息 ID 最多保留多少个。
	maxCountedMessageIDs = 2000
)

// staticKeepRatioThresholds 是离线统计：保留多少比例的主动回复时对应的概率阈值。
// 窗口数据不足时使用。
var staticKeepRatioThresholds = []struct {
	ratio     float64
	threshold float64
}{
	{0.0, 1.0},
	{0.1, 0.605},
	{0.2, 0.482},
	{0.3, 0.430},
	{0.4, 0.376},
	{0.5, 0.336},
	{0.6, 0.313},
	{0.7, 0.290},
	{0.8, 0.250},
	{0.9, 0.206},
	{1.0, 0.0},
}

// staticThreshold 按离线统计表线性插值出保留比例对应的概率阈值。
func staticThreshold(keepRatio float64) float64 {
	if keepRatio <= 0 {
		return 1.0
	}
	if keepRatio >= 1 {
		return 0.0
	}
	for i := 0; i < len(staticKeepRatioThresholds)-1; i++ {
		low := staticKeepRatioThresholds[i]
		high := staticKeepRatioThresholds[i+1]
		if keepRatio <= high.ratio {
			progress := (keepRatio - low.ratio) / (high.ratio - low.ratio)
			return low.threshold + (high.threshold-low.threshold)*progress
		}
	}
	return 0.0
}

// dynamicScore 是一次被评分的虚拟轮次。
type dynamicScore struct {
	at          float64
	probability float64
}

// DynamicDecision 是动态门控的一次判定。
type DynamicDecision struct {
	ShouldTrigger bool
	Probability   float64
	Threshold     float64

	// ExpectedReplies 是窗口内不设门控时预计的 reply 次数。
	ExpectedReplies float64
	// TargetReplies 是窗口内按回复频率折算后的目标 reply 次数。
	TargetReplies float64
	// ActualReplies 是窗口内实际的 reply 次数。
	ActualReplies int
	// KeepRatio 是当前允许保留的主动回复比例。
	KeepRatio float64
}

// Detail 返回可读的判定说明。
func (d DynamicDecision) Detail() string {
	return sprintf("概率=%.2f 阈值=%.2f 预计回复=%.1f 目标=%.1f 已回复=%d 保留比例=%.2f",
		d.Probability, d.Threshold, d.ExpectedReplies, d.TargetReplies,
		d.ActualReplies, d.KeepRatio)
}

// DynamicGate 是单个聊天流的动态回复门控状态。
type DynamicGate struct {
	windowSeconds float64

	proactiveScores    []dynamicScore
	forcedExpectations []struct {
		at    float64
		value float64
	}
	replyTimes []float64

	countedMessageIDs map[string]struct{}
	countedOrder      []string

	openRoundStartedAt *float64
}

// NewDynamicGate 创建动态门控。
func NewDynamicGate() *DynamicGate {
	return &DynamicGate{
		windowSeconds:     replyWindowSeconds,
		countedMessageIDs: make(map[string]struct{}),
	}
}

// TakeUncountedMessageIDs 返回尚未计入预计回复数的消息 ID，并把它们标记为已计入。
func (g *DynamicGate) TakeUncountedMessageIDs(ids []string) []string {
	var out []string
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, seen := g.countedMessageIDs[id]; seen {
			continue
		}
		out = append(out, id)
		g.countedMessageIDs[id] = struct{}{}
		g.countedOrder = append(g.countedOrder, id)
	}

	// 限制记忆长度，避免长期运行时无限增长。
	for len(g.countedOrder) > maxCountedMessageIDs {
		oldest := g.countedOrder[0]
		g.countedOrder = g.countedOrder[1:]
		delete(g.countedMessageIDs, oldest)
	}
	return out
}

// IsRoundOpen 判断当前是否仍处在一个尚未结束的虚拟轮次内。
func (g *DynamicGate) IsRoundOpen(now float64) bool {
	return g.openRoundStartedAt != nil && now-*g.openRoundStartedAt < virtualRoundSeconds
}

// RecordProactiveDemand 记录当前虚拟轮次预计带来的 reply 次数。
//
// 轮次仍未结束时，覆盖该轮次已记录的概率（同一轮次只计一次）；否则开启新的轮次。
func (g *DynamicGate) RecordProactiveDemand(probability, now float64) {
	if g.IsRoundOpen(now) && len(g.proactiveScores) > 0 {
		g.proactiveScores[len(g.proactiveScores)-1].probability = probability
		return
	}
	g.openRoundStartedAt = &now
	g.proactiveScores = append(g.proactiveScores, dynamicScore{at: now, probability: probability})
}

// CloseRound 表示 Planner 已实际运行，结束当前虚拟轮次。
func (g *DynamicGate) CloseRound() {
	g.openRoundStartedAt = nil
}

// RecordForcedTurn 记录一次 @ 强制触发预计带来的 reply 次数。
func (g *DynamicGate) RecordForcedTurn(now float64) {
	g.CloseRound()
	g.forcedExpectations = append(g.forcedExpectations, struct {
		at    float64
		value float64
	}{at: now, value: forcedTurnExpectedReplies})
}

// RecordReply 记录 Planner 实际调用了一次 reply。
func (g *DynamicGate) RecordReply(now float64) {
	g.replyTimes = append(g.replyTimes, now)
}

// prune 清理窗口外的记录。
func (g *DynamicGate) prune(now float64) {
	expireBefore := now - g.windowSeconds

	idx := 0
	for idx < len(g.proactiveScores) && g.proactiveScores[idx].at < expireBefore {
		idx++
	}
	g.proactiveScores = g.proactiveScores[idx:]

	idx = 0
	for idx < len(g.forcedExpectations) && g.forcedExpectations[idx].at < expireBefore {
		idx++
	}
	g.forcedExpectations = g.forcedExpectations[idx:]

	idx = 0
	for idx < len(g.replyTimes) && g.replyTimes[idx] < expireBefore {
		idx++
	}
	g.replyTimes = g.replyTimes[idx:]
}

// Evaluate 判断当前这批待处理消息是否放行进入 Planner。
//
// 阈值由两部分决定：
//   - 前馈：目标回复数先扣除 @ 强制触发预计占用的部分，剩余额度占主动回复预计数的比例即保留比例
//   - 反馈：窗口内实际 reply 次数高于目标就收紧保留比例
//
// 只收紧不放宽 —— 一波消息刚到时预计数先涨、实际回复还没跟上，
// 若据此放宽，等这波消息过去就没有机会再收回来，回复会系统性偏多。
func (g *DynamicGate) Evaluate(probability, frequency, now float64) DynamicDecision {
	g.prune(now)

	scores := make([]float64, 0, len(g.proactiveScores))
	var expectedProactive float64
	for _, s := range g.proactiveScores {
		scores = append(scores, s.probability)
		expectedProactive += s.probability
	}

	var expectedForced float64
	for _, f := range g.forcedExpectations {
		expectedForced += f.value
	}

	expectedTotal := expectedProactive + expectedForced
	actualReplies := len(g.replyTimes)

	normalized := clamp01(frequency)
	targetReplies := normalized * expectedTotal

	var keepRatio float64
	switch {
	case normalized >= 1.0:
		keepRatio = 1.0
	case expectedProactive <= 0:
		keepRatio = normalized
	default:
		feedforward := (targetReplies - expectedForced) / expectedProactive
		feedback := clamp01(1.0 + (targetReplies-float64(actualReplies))/math_max(1.0, targetReplies))
		keepRatio = clamp01(feedforward * feedback)
	}

	threshold := resolveThreshold(scores, expectedProactive, keepRatio)

	return DynamicDecision{
		ShouldTrigger:   probability >= threshold,
		Probability:     probability,
		Threshold:       threshold,
		ExpectedReplies: expectedTotal,
		TargetReplies:   targetReplies,
		ActualReplies:   actualReplies,
		KeepRatio:       keepRatio,
	}
}

// resolveThreshold 按概率从高到低累计，累计到保留额度时的概率即阈值。
func resolveThreshold(scores []float64, expectedProactive, keepRatio float64) float64 {
	if keepRatio >= 1.0 {
		return 0.0
	}
	if keepRatio <= 0.0 {
		return 1.0
	}
	if len(scores) < minWindowScoreCount {
		return staticThreshold(keepRatio)
	}

	budget := keepRatio * expectedProactive

	sorted := append([]float64(nil), scores...)
	sort.Sort(sort.Reverse(sort.Float64Slice(sorted)))

	var accumulated float64
	for _, s := range sorted {
		accumulated += s
		if accumulated >= budget {
			return s
		}
	}
	return 0.0
}
