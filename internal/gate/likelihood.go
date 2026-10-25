// Package gate 实现触发门控。
//
// 对应 MaiBot 的 src/maisaka/turn_trigger/：
//   - reply_likelihood.py  估算一批消息的回复概率
//   - dynamic_gate.py      动态门控（滑动窗口 + 保留比例）
//   - gates.py             频率阈值门控 + 空窗补偿
//   - idle_backoff.py      空闲退避
//   - scheduler.py         调度入口
//
// 所有常数与权重原样照抄 —— 那是用真实聊天数据拟合出来的，没有理由自己发明。
package gate

import "math"

// 回复概率估算。
//
// 权重由「无必要性门控期间的 Planner 首轮记录」拟合得到（MaiBot scripts/explore_reply_necessity.py），
// 样本只包含「有新的外部消息且没有 @ 机器人」的轮次；
// 被 @ 的消息由强制触发直接放行，不经过本函数。
const (
	// maxPendingCount 是待处理消息数进入函数前的上限，与拟合时的截断一致。
	maxPendingCount = 20
	// defaultSecondsSinceBot 是上下文中没有机器人发言时采用的间隔。
	defaultSecondsSinceBot = 3600.0
)

// 拟合权重：logit = 截距 + Σ 权重 × 特征
const (
	weightIntercept       = -1.2005
	weightMentionBot      = 1.5852
	weightAtOther         = -1.1157
	weightQuestionMark    = 0.4807
	weightPlaceholderOnly = -1.0281
	weightRecentSelfRatio = 2.6525
	weightRecentMsgCount  = -0.0367
	weightLogSecondsSince = 0.0571
	weightPendingCount    = 0.1012
)

// LikelihoodInput 是估计回复概率所需的对话快照。
type LikelihoodInput struct {
	// MentionBot 表示新消息中是否提到了机器人名字。
	MentionBot bool
	// AtOther 表示新消息中是否 @ 了机器人以外的人。
	AtOther bool
	// HasQuestionMark 表示新消息中是否带问号。
	HasQuestionMark bool
	// PlaceholderOnly 表示新消息是否全是表情、图片等媒体占位。
	PlaceholderOnly bool
	// RecentSelfRatio 是最近 5 分钟的消息里机器人发言所占比例。
	RecentSelfRatio float64
	// RecentMessageCount 是最近 5 分钟的消息条数。
	RecentMessageCount int
	// SecondsSinceBotMessage 是距离机器人上一次发言的秒数。
	SecondsSinceBotMessage float64
	// PendingCount 是本批新的外部消息条数。
	PendingCount int
}

// EstimateReplyProbability 估计这批消息进入 Planner 后会调用 reply 的概率。
func EstimateReplyProbability(in LikelihoodInput) float64 {
	logit := weightIntercept +
		weightMentionBot*boolToFloat(in.MentionBot) +
		weightAtOther*boolToFloat(in.AtOther) +
		weightQuestionMark*boolToFloat(in.HasQuestionMark) +
		weightPlaceholderOnly*boolToFloat(in.PlaceholderOnly) +
		weightRecentSelfRatio*clamp01(in.RecentSelfRatio) +
		weightRecentMsgCount*float64(maxInt(0, in.RecentMessageCount)) +
		weightLogSecondsSince*math.Log1p(math.Max(0, in.SecondsSinceBotMessage)) +
		weightPendingCount*float64(minInt(maxPendingCount, maxInt(0, in.PendingCount)))

	return 1.0 / (1.0 + math.Exp(-logit))
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
