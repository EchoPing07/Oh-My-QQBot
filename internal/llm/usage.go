package llm

import (
	"context"
	"sync"
	"time"
)

// UsageRecorder 接收每次调用的用量，用于落库统计。
type UsageRecorder interface {
	RecordUsage(ctx context.Context, task, model string, usage Usage, cost float64, duration time.Duration)
}

// CostCalculator 按模型价格算出花费。
// 价格单位是元 / M token。
func CostCalculator(promptTokens, completionTokens, cachedTokens int, priceIn, priceOut, cachePriceIn float64) float64 {
	// 缓存命中的部分按缓存价，剩余输入按正常价。
	uncachedPrompt := promptTokens - cachedTokens
	if uncachedPrompt < 0 {
		uncachedPrompt = 0
	}
	cachePrice := cachePriceIn
	if cachePrice <= 0 {
		cachePrice = priceIn
	}

	const perMillion = 1_000_000.0
	inCost := float64(uncachedPrompt) / perMillion * priceIn
	cacheCost := float64(cachedTokens) / perMillion * cachePrice
	outCost := float64(completionTokens) / perMillion * priceOut

	return inCost + cacheCost + outCost
}

// tokenCounter 是朴素的 token 估算，用于上下文预算。
//
// 为什么不用真正的 tokenizer：
// 引入一个分词库只为了裁剪上下文，收益不成正比。
// 中文按 1 字 ≈ 1 token、英文按 4 字符 ≈ 1 token 估算，
// 再留出安全余量，够用。
type tokenCounter struct{}

var defaultCounter = tokenCounter{}

// Estimate 估算一段文本的 token 数。
func (tokenCounter) Estimate(s string) int {
	var cjk, other int
	for _, r := range s {
		if isCJK(r) {
			cjk++
		} else {
			other++
		}
	}
	// 中文 1 字 1 token；非中文 4 字符 1 token。
	n := cjk + (other+3)/4
	// 保守上浮 10%，避免边界超限。
	return n + n/10
}

// EstimateMessages 估算一组消息的 token 数。
func (tokenCounter) EstimateMessages(msgs []Message) int {
	total := 0
	for _, m := range msgs {
		total += 4 // 每条消息的角色与分隔开销
		total += defaultCounter.Estimate(m.Content)
		for _, tc := range m.ToolCalls {
			total += defaultCounter.Estimate(tc.Function.Name)
			total += defaultCounter.Estimate(tc.Function.Arguments)
		}
		for _, img := range m.Images {
			// 图片按固定值估算，具体取决于分辨率与模型。
			_ = img
			total += 850
		}
	}
	return total
}

func isCJK(r rune) bool {
	switch {
	case r >= 0x4E00 && r <= 0x9FFF: // CJK 统一汉字
		return true
	case r >= 0x3400 && r <= 0x4DBF: // 扩展 A
		return true
	case r >= 0x3000 && r <= 0x303F: // CJK 标点
		return true
	case r >= 0xFF00 && r <= 0xFFEF: // 全角
		return true
	case r >= 0x3040 && r <= 0x30FF: // 日文假名
		return true
	case r >= 0xAC00 && r <= 0xD7AF: // 韩文
		return true
	}
	return false
}

// EstimateTokens 是包级快捷函数。
func EstimateTokens(s string) int { return defaultCounter.Estimate(s) }

// EstimateMessageTokens 是包级快捷函数。
func EstimateMessageTokens(msgs []Message) int { return defaultCounter.EstimateMessages(msgs) }

// usageCollector 收集一次生成过程中的所有调用用量。
type usageCollector struct {
	mu      sync.Mutex
	records []usageEntry
}

type usageEntry struct {
	task     string
	model    string
	usage    Usage
	cost     float64
	duration time.Duration
}

func (u *usageCollector) add(task, model string, usage Usage, cost float64, d time.Duration) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.records = append(u.records, usageEntry{task, model, usage, cost, d})
}

func (u *usageCollector) all() []usageEntry {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]usageEntry(nil), u.records...)
}
