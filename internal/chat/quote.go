package chat

import (
	"errors"
	"math"
	"math/rand"
	"strings"
)

// 引用判定可能返回的错误。调用方通常只记日志、按不引用处理。
var (
	errNegativeDistance = errors.New("引用目标距离不能为负数")
	errUnknownFrequency = errors.New("未知的引用频率档位")
)

// 引用回复判定。
//
// 完全照抄 refs/MaiBot/src/maisaka/quote/：
//   - counter.py  记录计数与快照
//   - rules.py    概率公式
//
// 早期版本这里是个凭印象写的「never/occasionally/often 三档随机」(0.33/0.66)，
// 与参考实现的差距很大：原版是八类基础概率（按「上一句是不是自己说的 /
// 间隔是否够短 / 正文长度」分档）再乘以频率档位与目标距离系数，
// 私聊还额外用 logistic 回归并吃「连发条数」。系数来自原版离线试验，不要改。

// QuoteFrequency 是引用频率档位。
type QuoteFrequency string

const (
	QuoteNever        QuoteFrequency = "never"
	QuoteOccasionally QuoteFrequency = "occasionally"
	QuoteOften        QuoteFrequency = "often"
)

// 引用计数器窗口常量，对齐 counter.py。
const (
	// quoteWindowSize 是最近窗口保存的消息数，超出即视为「已离开窗口」。
	quoteWindowSize = 100
	// quoteMaxBurst 是连发条数上限，公式最多用到 16。
	quoteMaxBurst = 16
	// quoteDistanceCap 是私聊公式里目标距离的封顶，避免超出拟合窗口外推。
	quoteDistanceCap = 32
)

// QuoteContext 是一次引用判定的输入快照。
// 对应 counter.py 的 ReplyQuoteContext。
type QuoteContext struct {
	// TargetDistance 是目标消息距最新一条普通消息的距离（条数）。
	// 为 nil 表示已确认记录过、但已离开最近 100 条窗口。
	TargetDistance *int
	// PreviousSenderID 是上一条普通消息的发送者。
	PreviousSenderID string
	// PreviousSeconds 是距上一条普通消息的秒数。
	PreviousSeconds float64
	// PreviousBurst 是上一条发送者连续发言了第几条。
	PreviousBurst int
}

// QuoteCounter 按进入 runtime / 成功发送的顺序计数。
// 对应 counter.py 的 ReplyQuoteCounter。
//
// 只保存 ID 与序号，不持有正文；同一 message_id 只记一次，
// 因为历史回填会重复投递同一条消息。
type QuoteCounter struct {
	positions map[string]int
	// order 记录插入顺序，用于淘汰超出窗口的旧 ID。
	order    []string
	sequence int

	senderID string
	prevTime int64 // 纳秒时间戳，0 表示尚未记录
	burst    int
}

// NewQuoteCounter 创建计数器。
func NewQuoteCounter() *QuoteCounter {
	return &QuoteCounter{positions: make(map[string]int)}
}

// Record 记录一条普通消息。
//
// 参数 at 用纳秒时间戳传入（而不是 time.Time）是为了让调用方
// 统一使用消息自带的 received_at，避免时钟抖动影响「间隔」这个变量。
func (c *QuoteCounter) Record(messageID, senderID string, at int64) {
	if messageID == "" {
		return
	}
	// 同一条消息重复投递：保持原有序号，不重复计数。
	// 原版还会用消息对象上的实例标记区分，Go 里直接用 ID 去重即可。
	if _, ok := c.positions[messageID]; ok {
		return
	}

	c.sequence++
	c.positions[messageID] = c.sequence
	c.order = append(c.order, messageID)
	if len(c.positions) > quoteWindowSize {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.positions, oldest)
	}

	if senderID == c.senderID {
		c.burst++
		if c.burst > quoteMaxBurst {
			c.burst = quoteMaxBurst
		}
	} else {
		c.burst = 1
	}
	c.senderID = senderID
	c.prevTime = at
}

// Snapshot 生成引用判定所需的快照。
//
// 第二个返回值 false 表示目标消息不在当前窗口内（或从未记录过），
// 此时按参考实现直接引用，不虚构距离。
func (c *QuoteCounter) Snapshot(targetMessageID string, nowUnixNano int64) (QuoteContext, bool) {
	pos, ok := c.positions[targetMessageID]
	if !ok {
		return QuoteContext{}, false
	}
	if c.prevTime == 0 {
		return QuoteContext{}, false
	}

	distance := c.sequence - pos
	ctx := QuoteContext{
		PreviousSenderID: c.senderID,
		PreviousSeconds:  math.Max(0, float64(nowUnixNano-c.prevTime)/1e9),
		PreviousBurst:    c.burst,
	}
	if distance < quoteWindowSize {
		ctx.TargetDistance = &distance
	}
	return ctx, true
}

// quoteBaseProbability 是群聊的八类基础概率。
// 数值逐字照抄 rules.py:quote_probability，不要「整理」。
func quoteBaseProbability(textLength int, previousSenderSelf bool, previousSeconds float64) float64 {
	switch {
	case textLength == 0 && previousSenderSelf:
		if previousSeconds <= 3 {
			return 0.005913471588878362
		}
		return 0.013812173277286514
	case textLength == 0:
		// 前一分钟是否有人发言，等价于上一条发言距今是否不超过 60 秒。
		if previousSeconds <= 60 {
			return 0.025472825783009516
		}
		return 0.0108627046866078
	case previousSenderSelf:
		if previousSeconds <= 3 {
			return 0.011765162067321502
		}
		return 0.07760369512696548
	default:
		if textLength <= 50 {
			return 0.20934408508581265
		}
		return 0.08614950202149084
	}
}

// quoteDistanceMultiplier 是目标距离系数。
func quoteDistanceMultiplier(targetDistance int) float64 {
	switch {
	case targetDistance == 0:
		return 0.5
	case targetDistance <= 2:
		return 1.0
	case targetDistance <= 5:
		return 1.1
	default:
		return 1.5
	}
}

// QuoteProbability 计算群聊引用概率。对应 rules.py:quote_probability。
func QuoteProbability(textLength int, previousSenderSelf bool, previousSeconds float64, freq QuoteFrequency, targetDistance int) (float64, error) {
	if targetDistance < 0 {
		return 0, errNegativeDistance
	}
	if freq == QuoteNever {
		return 0, nil
	}
	if freq != QuoteOccasionally && freq != QuoteOften {
		return 0, errUnknownFrequency
	}

	p := quoteBaseProbability(textLength, previousSenderSelf, previousSeconds)
	freqMultiplier := 1.0
	if freq == QuoteOften {
		freqMultiplier = 2.0
	}
	return math.Min(1.0, p*freqMultiplier*quoteDistanceMultiplier(targetDistance)), nil
}

// PrivateQuoteProbability 计算私聊引用概率。对应 rules.py:private_quote_probability。
//
// 距离参数是中间隔的消息数；公式里的「往前第几条」封顶 32。
func PrivateQuoteProbability(textLength int, previousSenderSelf bool, previousSeconds float64, previousBurst, targetDistance int, freq QuoteFrequency) (float64, error) {
	if targetDistance < 0 {
		return 0, errNegativeDistance
	}
	if freq == QuoteNever {
		return 0, nil
	}
	if freq != QuoteOccasionally && freq != QuoteOften {
		return 0, errUnknownFrequency
	}

	z := -4.176576611204103
	if previousSenderSelf {
		if previousSeconds <= 3 {
			z += -2.0390866101745555
		} else {
			z += 0.407259409803155
		}
	} else {
		burst := previousBurst
		if burst < 1 {
			burst = 1
		}
		if burst > 16 {
			burst = 16
		}
		z += 0.6811570734750046 * math.Log2(float64(burst))
	}
	if textLength == 0 {
		z -= 3.4527659205203576
	}
	dist := targetDistance + 1
	if dist > quoteDistanceCap {
		dist = quoteDistanceCap
	}
	z += 0.521800209819322 * math.Log2(float64(dist))

	p := 1 / (1 + math.Exp(-z))
	freqMultiplier := 1.0
	if freq == QuoteOften {
		freqMultiplier = 2.0
	}
	return math.Min(1.0, p*freqMultiplier), nil
}

// DecideQuote 决定这次回复要不要引用目标消息。
// 对应 rules.py:decide_quote。
//
// 第三个返回值是本次使用的概率，便于日志与调试。
// ctx 为 nil（目标不在窗口内）时按参考实现直接引用，概率记 1.0。
func DecideQuote(ctx *QuoteContext, botUserID string, text string, freq QuoteFrequency, isGroup bool) (bool, float64, error) {
	if freq == QuoteNever {
		return false, 0, nil
	}
	if freq != QuoteOccasionally && freq != QuoteOften {
		return false, 0, errUnknownFrequency
	}
	return decideQuoteWithRoll(ctx, botUserID, text, freq, isGroup, rand.Float64())
}

// decideQuoteWithRoll 是注入随机数的版本，供测试用。
func decideQuoteWithRoll(ctx *QuoteContext, botUserID, text string, freq QuoteFrequency, isGroup bool, roll float64) (bool, float64, error) {
	if freq == QuoteNever {
		return false, 0, nil
	}
	if freq != QuoteOccasionally && freq != QuoteOften {
		return false, 0, errUnknownFrequency
	}
	// 目标不在窗口内：直接引用。
	if ctx == nil || ctx.TargetDistance == nil {
		return true, 1.0, nil
	}

	textLength := len([]rune(strings.TrimSpace(text)))
	previousSelf := ctx.PreviousSenderID == botUserID && botUserID != ""

	var (
		p   float64
		err error
	)
	if isGroup {
		p, err = QuoteProbability(textLength, previousSelf, ctx.PreviousSeconds, freq, *ctx.TargetDistance)
	} else {
		p, err = PrivateQuoteProbability(textLength, previousSelf, ctx.PreviousSeconds, ctx.PreviousBurst, *ctx.TargetDistance, freq)
	}
	if err != nil {
		return false, 0, err
	}
	return roll < p, p, nil
}

// QuoteFrequencyOf 把配置里的字符串转成档位，非法值按 never 处理并返回 false。
func QuoteFrequencyOf(raw string) (QuoteFrequency, bool) {
	switch QuoteFrequency(raw) {
	case QuoteNever:
		return QuoteNever, true
	case QuoteOccasionally:
		return QuoteOccasionally, true
	case QuoteOften:
		return QuoteOften, true
	}
	return QuoteNever, false
}
