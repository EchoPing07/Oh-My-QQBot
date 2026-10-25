package gate

// 触发调度。
//
// 对应 MaiBot 的 turn_trigger/scheduler.py。
//
// 这是把三层门控串起来的地方，判定顺序很关键：
//   1. 静默频率（talk_value=0）→ 直接静默消费，不进入 Planner
//   2. @ 强制触发 → 跳过所有门控
//   3. 空闲退避 → 还在退避期内就延迟检查
//   4. 动态门控 / 频率阈值门控 → 决定是否放行

// Scheduler 决定外部消息何时进入内部循环。
type Scheduler struct {
	dynamic   *DynamicGate
	frequency *FrequencyGate
	backoff   *Backoff

	// useDynamic 对应配置里的 reply_trigger_mode == "dynamic"。
	useDynamic bool
}

// NewScheduler 创建调度器。
func NewScheduler(useDynamic bool, backoffCfg BackoffConfig) *Scheduler {
	return &Scheduler{
		dynamic:    NewDynamicGate(),
		frequency:  NewFrequencyGate(),
		backoff:    NewBackoff(backoffCfg),
		useDynamic: useDynamic,
	}
}

// Backoff 返回退避控制器，供 cycle 结束后记录结果。
func (s *Scheduler) Backoff() *Backoff { return s.backoff }

// Dynamic 返回动态门控，供记录强制触发与回复。
func (s *Scheduler) Dynamic() *DynamicGate { return s.dynamic }

// RecordReply 记录 Planner 实际调用了一次 reply。
func (s *Scheduler) RecordReply(now float64) {
	s.dynamic.RecordReply(now)
}

// RecordForcedTurn 记录一次 @ 强制触发。
func (s *Scheduler) RecordForcedTurn(messageIDs []string, now float64) {
	if len(s.dynamic.TakeUncountedMessageIDs(messageIDs)) > 0 {
		s.dynamic.RecordForcedTurn(now)
	}
}

// Input 是调度所需的运行时快照。
type Input struct {
	// PendingCount 是待处理消息数。
	PendingCount int
	// PendingMessageIDs 是待处理消息的 ID，用于动态门控去重计数。
	PendingMessageIDs []string

	// IsGroup 表示是否群聊。
	IsGroup bool
	// Frequency 是当前会话生效的回复频率。
	Frequency float64

	// HasForcedTrigger 表示这批消息里有 @ 或提及机器人。
	HasForcedTrigger bool

	// Likelihood 是这批消息的概率估算输入。
	Likelihood LikelihoodInput

	// AverageInterval 是最近 30 分钟外部消息的平均间隔秒数。
	AverageInterval float64
	// LastExternalAt 是最后一条外部消息的时刻。
	LastExternalAt float64

	// Now 是当前单调秒。
	Now float64
}

// Outcome 是调度结果。
type Outcome struct {
	// Enqueue 表示应投递一次内部循环。
	Enqueue bool
	// DeferSeconds 大于 0 表示应延迟这么久后重新检查。
	DeferSeconds float64
	// Silent 表示静默消费：消息进入上下文但不触发思考。
	Silent bool
	// Detail 是可读的判定说明，用于日志与调试面板。
	Detail string
}

// Schedule 判断这批消息是否应触发内部循环。
func (s *Scheduler) Schedule(in Input) Outcome {
	// 1. 静默频率：talk_value = 0 表示完全闭嘴。
	if in.Frequency <= 0 {
		return Outcome{
			Enqueue: true,
			Silent:  true,
			Detail:  sprintf("调度=静默消费 待处理=%d 频率=0", in.PendingCount),
		}
	}

	if in.PendingCount <= 0 {
		return Outcome{Detail: "没有待处理消息"}
	}

	// 2. @ 强制触发：跳过所有门控。
	if in.HasForcedTrigger {
		s.RecordForcedTurn(in.PendingMessageIDs, in.Now)
		return Outcome{
			Enqueue: true,
			Detail:  sprintf("调度=强制触发 待处理=%d 频率=%.3f", in.PendingCount, in.Frequency),
		}
	}

	// 3. 空闲退避。
	if shouldDelay, delay := s.backoff.ShouldDelay(in.PendingCount, in.IsGroup, in.Now); shouldDelay {
		return Outcome{
			DeferSeconds: delay,
			Detail:       sprintf("调度=空闲退避 剩余=%.1fs 待处理=%d", delay, in.PendingCount),
		}
	}

	// 4. 动态门控。
	if s.useDynamic {
		return s.scheduleDynamic(in)
	}

	// 4'. 频率阈值门控。
	return s.scheduleFrequency(in)
}

func (s *Scheduler) scheduleDynamic(in Input) Outcome {
	// 私聊不设动态门控：每条消息都是对机器人说的。
	if !in.IsGroup {
		return Outcome{
			Enqueue: true,
			Detail:  "调度=私聊不设动态门控 判定=进入Planner",
		}
	}

	newIDs := s.dynamic.TakeUncountedMessageIDs(in.PendingMessageIDs)
	if len(newIDs) > 0 {
		// 预计回复数按虚拟轮次计：只用本轮次内到达的消息估计，
		// 被门控拦下而积压的旧消息不重复计入。
		if !s.dynamic.IsRoundOpen(in.Now) {
			// 新轮次开始，旧轮次的评分已过期，但保留在窗口里参与阈值计算。
		}
		s.dynamic.RecordProactiveDemand(EstimateReplyProbability(in.Likelihood), in.Now)
	}

	probability := EstimateReplyProbability(in.Likelihood)
	decision := s.dynamic.Evaluate(probability, in.Frequency, in.Now)

	label := "等待更多消息"
	if decision.ShouldTrigger {
		label = "进入Planner"
		s.dynamic.CloseRound()
	}

	return Outcome{
		Enqueue: decision.ShouldTrigger,
		Detail: sprintf("回复调度 待处理=%d 频率=%.3f 动态门控: %s 判定=%s",
			in.PendingCount, in.Frequency, decision.Detail(), label),
	}
}

func (s *Scheduler) scheduleFrequency(in Input) Outcome {
	threshold := TriggerThreshold(in.Frequency)
	result := s.frequency.Evaluate(
		in.PendingCount, threshold,
		in.AverageInterval, in.LastExternalAt, in.Now,
	)

	out := Outcome{
		Enqueue: result.ShouldTrigger(),
		Detail: sprintf("回复调度 待处理=%d 阈值=%d 频率=%.3f %s",
			in.PendingCount, threshold, in.Frequency, result.Detail),
	}
	if result.Decision == DecisionDelay {
		out.DeferSeconds = result.DelaySeconds
	}
	return out
}
