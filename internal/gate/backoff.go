package gate

import "math"

// 空闲退避。
//
// 对应 MaiBot 的 idle_backoff.py。
//
// 逻辑：连续几次「想完了但没说话」之后，把下一次检查的时间指数级往后推，
// 避免在没人说话的群里反复空转烧模型。

// IdleCycleReasons 是算作「空闲」的整轮结束原因。
//
// 只有这些才算「想了一轮但决定不说话」，用于累计退避次数。
// 对应 MaiBot mode_policy.py 的 IDLE_CYCLE_REASONS。
var IdleCycleReasons = map[string]bool{
	"planner_no_tool_end":       true, // Planner 没调工具直接结束
	"planner_wait_rest":         true, // Planner 决定休息
	"tool_pause:wait":           true, // 调了 wait 工具
	"tool_stop_after_execution": true, // 调了 stop 工具
}

// IsIdleCycleReason 判断整轮结束原因是否属于空闲。
func IsIdleCycleReason(reason string) bool { return IdleCycleReasons[reason] }

// BackoffConfig 是空闲退避的配置。
type BackoffConfig struct {
	// BaseSeconds 是空闲退避基准秒数。
	BaseSeconds float64
	// CapSeconds 是退避等待上限秒数。
	CapSeconds float64
	// StartCount 是连续几次空闲后开始放慢。
	StartCount int
	// BypassPendingCount 是等待期间新消息达到多少条就立刻重新处理；0 表示不按条数打断。
	BypassPendingCount int
}

// DefaultBackoffConfig 返回与 MaiBot 默认配置一致的退避参数。
func DefaultBackoffConfig() BackoffConfig {
	return BackoffConfig{
		BaseSeconds:        15,
		CapSeconds:         300,
		StartCount:         2,
		BypassPendingCount: 6,
	}
}

// Backoff 维护连续空闲结束后的消息触发退避。
type Backoff struct {
	cfg   BackoffConfig
	count int
	until float64 // 单调时钟秒
}

// NewBackoff 创建退避控制器。
func NewBackoff(cfg BackoffConfig) *Backoff {
	if cfg.StartCount < 1 {
		cfg.StartCount = 1
	}
	return &Backoff{cfg: cfg}
}

// Reset 清理连续空闲退避状态。
func (b *Backoff) Reset() {
	b.count = 0
	b.until = 0
}

// Count 返回当前连续空闲次数。
func (b *Backoff) Count() int { return b.count }

// RecordCycleResult 按整轮结束原因维护空闲退避状态。
func (b *Backoff) RecordCycleResult(reason string, isGroup bool, now float64) {
	if !IsIdleCycleReason(reason) {
		b.Reset()
		return
	}
	// 私聊每条消息都是对机器人说的，不做退避。
	if !isGroup {
		b.Reset()
		return
	}

	b.count++

	backoff := b.backoffSeconds()
	if backoff <= 0 {
		b.until = 0
		return
	}
	b.until = now + backoff
}

// backoffSeconds 算出当前应退避的秒数。
//
//	backoff = min(cap, base × 2^(count - startCount))
func (b *Backoff) backoffSeconds() float64 {
	base := b.cfg.BaseSeconds
	cap := b.cfg.CapSeconds
	if base <= 0 || cap <= 0 {
		return 0
	}
	if b.count < b.cfg.StartCount {
		return 0
	}

	exponent := b.count - b.cfg.StartCount
	if exponent < 0 {
		exponent = 0
	}

	// 避免 2^n 溢出，指数封顶到 62。
	if exponent > 62 {
		exponent = 62
	}

	value := base * math.Pow(2, float64(exponent))
	if value > cap {
		return cap
	}
	return value
}

// ShouldDelay 判断当前消息触发是否应被空闲退避延迟。
//
// 返回 true 时，调用方应在 delaySeconds 之后重新检查。
func (b *Backoff) ShouldDelay(pendingCount int, isGroup bool, now float64) (bool, float64) {
	if !isGroup {
		return false, 0
	}
	if b.until <= 0 {
		return false, 0
	}

	remaining := b.until - now
	if remaining <= 0 {
		b.until = 0
		return false, 0
	}

	// 攒够足够多的新消息就立刻处理，不再等。
	if b.cfg.BypassPendingCount > 0 && pendingCount >= b.cfg.BypassPendingCount {
		return false, 0
	}

	return true, remaining
}

// Remaining 返回剩余退避秒数，无退避时为 0。
func (b *Backoff) Remaining(now float64) float64 {
	if b.until <= 0 {
		return 0
	}
	r := b.until - now
	if r <= 0 {
		return 0
	}
	return r
}
