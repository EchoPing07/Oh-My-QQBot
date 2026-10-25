package chat

import (
	"sync"
	"time"
)

// 消息防抖。
//
// 严格照抄 MaiBot 的 chat/debounce/controller.py：
// 消息不立刻处理，先等一小段时间看有没有后续。
// 这批等待规则是它调出来的经验值，原样搬。

// DebounceMode 是防抖档位。
type DebounceMode string

const (
	DebounceOff     DebounceMode = "off"
	DebounceFast    DebounceMode = "fast"
	DebounceNatural DebounceMode = "natural"
	DebounceSteady  DebounceMode = "steady"
)

// WaitTimes 是一档防抖的四组等待时间。
type WaitTimes struct {
	Quiet           float64 // 安静开批
	Active          float64 // 活跃开批
	SameAuthor      float64 // 同人续发
	DifferentAuthor float64 // 换人
}

// waitProfiles 是四档的等待时间表。
var waitProfiles = map[DebounceMode]WaitTimes{
	DebounceFast:    {Quiet: 1.0, Active: 3.0, SameAuthor: 1.0, DifferentAuthor: 0.5},
	DebounceNatural: {Quiet: 2.0, Active: 5.0, SameAuthor: 2.0, DifferentAuthor: 0.5},
	DebounceSteady:  {Quiet: 4.0, Active: 7.0, SameAuthor: 4.0, DifferentAuthor: 2.0},
}

const (
	// quietThreshold 是「安静/活跃」的判定线：距上一条超 30 秒 = 安静开批。
	quietThreshold = 30.0
	// bareImageMinWait 是裸图开批的最短等待。
	bareImageMinWait = 4.0
	// batchLimit 是整批从第一条起的最长等待。
	batchLimit = 8.0
	// mentionLimit 是 @ 或提及后的最长等待。
	mentionLimit = 1.0
)

// Debouncer 是一个聊天流的防抖状态。
//
// 一个 Runtime 一个实例；只管理防抖，不启动后台任务。
type Debouncer struct {
	mu sync.Mutex

	first       *float64 // 批次首条到达时刻（单调时钟秒）
	lastArrival *float64
	lastAuthor  string
	deadline    *float64
	urgent      *float64
	changed     chan struct{}
}

// NewDebouncer 创建防抖器。
func NewDebouncer() *Debouncer {
	return &Debouncer{changed: make(chan struct{}, 1)}
}

// Deadline 返回当前批次的截止时刻（单调时钟秒），无批次时返回 0。
func (d *Debouncer) Deadline() (float64, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.deadline == nil {
		return 0, false
	}
	return *d.deadline, true
}

// Changed 返回一个在截止时间更新时被触发的 channel。
func (d *Debouncer) Changed() <-chan struct{} { return d.changed }

// Observe 更新截止时间并返回本次规则说明。
//
// 参数：
//   - now: 当前单调时钟秒
//   - receivedAt: 消息的接收时刻（墙钟 unix 秒）
//   - wallTime: 当前墙钟 unix 秒
//
// 预处理耗时也占用 8 秒/1 秒上限 —— 这是原版的细节：
// 用 now - (wallTime - receivedAt) 把墙钟时刻折算到单调时钟上。
func (d *Debouncer) Observe(
	author string,
	isBareImage, isPureEmoji, atBot bool,
	receivedAt, wallTime, now float64,
	mode DebounceMode,
) string {
	d.mu.Lock()
	defer d.mu.Unlock()

	waits, ok := waitProfiles[mode]
	if !ok {
		waits = waitProfiles[DebounceNatural]
	}

	arrival := now - maxf(0, wallTime-receivedAt)
	if d.lastArrival != nil {
		arrival = maxf(arrival, *d.lastArrival)
	}

	previousArrival := d.lastArrival
	previousAuthor := d.lastAuthor

	d.lastArrival = &arrival
	d.lastAuthor = author

	newBatch := d.first == nil
	if newBatch {
		first := arrival
		d.first = &first
	}

	if atBot {
		urgent := now - maxf(0, wallTime-receivedAt) + mentionLimit
		if d.urgent == nil {
			d.urgent = &urgent
		} else if urgent < *d.urgent {
			d.urgent = &urgent
		}
	}

	var reason string

	// 已到期但因模型/工具忙碌尚未消费的批次，不允许被新消息重新延长。
	// 这些消息仍进入原有上下文缓存，一起消费，不为补偿模型忙碌再排一遍队。
	if d.deadline == nil || *d.deadline > now {
		var reasons []string

		limit := *d.first + batchLimit
		if d.urgent != nil && *d.urgent < limit {
			limit = *d.urgent
		}

		var wait float64
		if newBatch {
			active := previousArrival != nil && arrival-*previousArrival <= quietThreshold
			if active {
				wait = waits.Active
				reasons = append(reasons, "活跃开批")
			} else {
				wait = waits.Quiet
				reasons = append(reasons, "安静开批")
			}
			if isBareImage {
				reasons = append(reasons, "裸图开批至少 4 秒")
				wait = maxf(wait, bareImageMinWait)
			}
		} else {
			if author == previousAuthor {
				wait = waits.SameAuthor
				reasons = append(reasons, "同人续发")
			} else {
				wait = waits.DifferentAuthor
				reasons = append(reasons, "换人")
			}
		}

		candidate := arrival + wait

		// 纯表情包仍可把截止时间提前，但不能在已有批次上继续续长。
		if isPureEmoji && !newBatch && d.deadline != nil {
			reasons = append(reasons, "纯表情包不延长等待")
			candidate = minf(candidate, *d.deadline)
		}

		if candidate > *d.first+batchLimit {
			reasons = append(reasons, "受整批 8 秒上限限制")
		}
		if d.urgent != nil && candidate > *d.urgent {
			reasons = append(reasons, "受@或提及 1 秒上限限制")
		}

		deadline := minf(candidate, limit)
		d.deadline = &deadline

		reason = joinReasons(reasons, wait)
	} else {
		reason = "批次已到期，不再延长等待"
	}

	d.notify()
	return reason
}

// Consume 表示本批消息已收走。
// 只保留上一条的时间和发言人，用于下一批判断。
func (d *Debouncer) Consume() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.first = nil
	d.deadline = nil
	d.urgent = nil
	d.notify()
}

// Reset 完全重置，用于清空上下文等场景。
func (d *Debouncer) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.first = nil
	d.lastArrival = nil
	d.lastAuthor = ""
	d.deadline = nil
	d.urgent = nil
	d.notify()
}

// notify 非阻塞地通知等待者。
func (d *Debouncer) notify() {
	select {
	case d.changed <- struct{}{}:
	default:
	}
}

func joinReasons(reasons []string, wait float64) string {
	out := ""
	for i, r := range reasons {
		if i > 0 {
			out += "；"
		}
		out += r
	}
	return out + " " + formatFloat(wait) + " 秒"
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func minf(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func formatFloat(f float64) string {
	// 去掉多余的小数位，1.0 -> "1"，0.5 -> "0.5"
	s := strconvFormatFloat(f)
	return s
}

// Now 返回单调时钟秒，用于防抖计算。
// 用 time.Since 的基准，避免系统时钟被调导致批次错乱。
var monoBase = time.Now()

// Monotonic 返回自进程启动以来的秒数。
func Monotonic() float64 { return time.Since(monoBase).Seconds() }

// WallNow 返回当前墙钟 unix 秒。
func WallNow() float64 { return float64(time.Now().UnixNano()) / 1e9 }
