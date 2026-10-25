package gate

// 频率阈值门控 + 空窗补偿。
//
// 对应 MaiBot 的 FrequencyThresholdTurnGate：
//   - 待处理数达到阈值 → 触发
//   - 不够时，用「沉默时长 / 平均消息间隔」折算成等效消息数补齐
//   - 折算量封顶在「阈值 - 1」，且 pending == 0 时永不触发
//     （杜绝纯沉默累积反复唤醒回复）

// Decision 是门控的判定结果。
type Decision string

const (
	// DecisionTrigger 表示放行进入 Planner。
	DecisionTrigger Decision = "trigger"
	// DecisionWait 表示继续等待更多消息。
	DecisionWait Decision = "wait"
	// DecisionDelay 表示延迟一段时间后再检查。
	DecisionDelay Decision = "delay"
)

// Result 是单个门控的判定结果。
type Result struct {
	Decision Decision
	Detail   string
	// DelaySeconds 仅在 DecisionDelay 时有意义。
	DelaySeconds float64
}

// ShouldTrigger 判断是否应放行。
func (r Result) ShouldTrigger() bool { return r.Decision == DecisionTrigger }

// 空窗补偿的常数。
const (
	// externalMessageIntervalWindow 是外部消息间隔的采样窗口（30 分钟）。
	externalMessageIntervalWindow = 1800.0
	// idleCompensationMinInterval 是平均间隔的下限（30 秒），
	// 避免统计值过小导致空窗补偿把沉默时间过快折算成消息、反复触发回复。
	idleCompensationMinInterval = 30.0
)

// FrequencyGate 按回复频率折算消息阈值，并用空窗补偿辅助触发。
type FrequencyGate struct{}

// NewFrequencyGate 创建频率门控。
func NewFrequencyGate() *FrequencyGate { return &FrequencyGate{} }

// TriggerThreshold 根据频率折算出触发一轮循环所需的消息数。
//
//	threshold = max(1, ceil(1 / frequency))
func TriggerThreshold(frequency float64) int {
	f := clamp01(frequency)
	if f <= 0 {
		return 0
	}
	return maxInt(1, int(math_Ceil(1.0/f)))
}

// Evaluate 返回频率阈值门控的触发判定。
//
// 参数：
//   - pendingCount: 待处理消息数
//   - threshold: 触发阈值（由 TriggerThreshold 算出）
//   - averageInterval: 最近 30 分钟外部消息的平均间隔秒数；<=0 表示不可用
//   - lastExternalAt: 最后一条外部消息的时刻（单调秒），用于算空窗时长
//   - now: 当前单调秒
func (g *FrequencyGate) Evaluate(
	pendingCount, threshold int,
	averageInterval, lastExternalAt, now float64,
) Result {
	if pendingCount >= threshold {
		return Result{
			Decision: DecisionTrigger,
			Detail: sprintf("pending=%d 达到阈值=%d 判定=进入Planner",
				pendingCount, threshold),
		}
	}

	triggered, delay, detail := idleCompensation(
		pendingCount, threshold, averageInterval, lastExternalAt, now)

	if triggered {
		return Result{
			Decision: DecisionTrigger,
			Detail:   detail + " 判定=空窗补偿进入Planner",
		}
	}
	if delay > 0 {
		return Result{
			Decision:     DecisionDelay,
			Detail:       detail + " 判定=延迟检查",
			DelaySeconds: delay,
		}
	}
	return Result{
		Decision: DecisionWait,
		Detail:   detail + " 判定=等待更多消息",
	}
}

// idleCompensation 在新消息不足阈值时，按空窗时间折算补齐触发条件，并返回下次检查延迟。
//
// 空窗折算量被限制在 threshold-1 以内，确保至少要有一条真实新消息才可能触发，
// 杜绝纯靠沉默累积反复唤醒回复。
func idleCompensation(
	pendingCount, threshold int,
	averageInterval, lastExternalAt, now float64,
) (triggered bool, delaySeconds float64, detail string) {
	// 纯沉默一律不触发。
	if pendingCount < 1 {
		return false, 0, "pending=0，不允许纯沉默触发"
	}
	if averageInterval <= 0 {
		return false, 0, "平均消息间隔不可用，无法进行空窗补偿"
	}

	// 统计值过小时按下限处理。
	interval := averageInterval
	if interval < idleCompensationMinInterval {
		interval = idleCompensationMinInterval
	}

	idleSeconds := math_max(0, now-lastExternalAt)

	// 即便空窗无限长，也不能让纯沉默跨过阈值。
	maxEquivalent := float64(maxInt(0, threshold-1))
	idleEquivalent := math_Min(idleSeconds/interval, maxEquivalent)

	equivalent := float64(pendingCount) + idleEquivalent

	detail = sprintf("平均间隔=%.2fs 空窗=%.2fs 空窗折算=%.2f 等效消息数=%.2f/%d",
		interval, idleSeconds, idleEquivalent, equivalent, threshold)

	if equivalent >= float64(threshold) {
		return true, 0, detail
	}

	delay := math_max(0, float64(threshold-pendingCount)*interval-idleSeconds)
	return false, delay, detail
}
