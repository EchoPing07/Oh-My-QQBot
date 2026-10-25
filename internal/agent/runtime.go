package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/chat"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/embedding"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/gate"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/llm"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/logx"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/memory"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/prompt"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/store"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/tool"
)

// Sender 是 runtime 需要的发送能力。
// 用接口而不是直接依赖 qq 包，避免循环引用。
type Sender interface {
	// SendText 发送纯文本，返回消息 ID 列表。
	//
	// passiveTo 是要占用的被动回复目标（触发本轮的那条入站消息），
	// quote 是要挂的引用卡片。两者指向不同的消息、键空间也不同，
	// 所以必须是两个参数而不是一个。
	SendText(ctx context.Context, scene, target string, text string, passiveTo string, quote domain.Reference) (domain.SendResult, error)
	// SendSegments 发送一段由消息段组成的内容。
	//
	// 需要 @ 与图片时必须走这个接口：QQ 侧 @ 只能靠 markdown 段的
	// <qqbot-at-user /> 渲染，图片要靠富媒体段，纯文本接口表达不了。
	SendSegments(ctx context.Context, scene, target string, segments []domain.Segment, passiveTo string, quote domain.Reference) (domain.SendResult, error)
}

// Runtime 是一个聊天流的状态机。
//
// 对应 MaiBot 的 MaisakaHeartFlowChatting。
//
// 并发模型：每个会话一个 goroutine 独占所有状态，外界通过 inbox 投递消息。
// 这样无锁、无竞态，所有状态机逻辑都在单 goroutine 里顺序执行。
// Runtime 是单个会话的运行时。
//
// 并发模型（改之前先看清这一节，越界的字段必须加锁或改原子）：
//   - run() 协程拥有 history / pending / botReplies / cancel / 调度器等全部状态；
//   - Post() 由网关协程调用，只碰 inbox 与 closed；
//   - Close() 由 Hub 的 sweep 协程（或 Hub.Close）调用，只碰 closed 与 quit；
//   - LastActive() 同理被 sweep 协程读。
//
// 所以 closed 与 lastActiveAt 是唯二真正跨协程的字段，必须原子访问。
// 曾经 closed 是裸 bool，sweep 协程 Close() 与网关协程 Post() 并发读写，
// 是实打实的数据竞争（且只有 -race 看得到，功能测试全绿）。
type Runtime struct {
	cfg     *config.Store
	store   *store.DB
	llm     *llm.Client
	prompts *prompt.Manager
	loop    *Loop
	replyer *Replyer
	sender  Sender

	// inbox 是外部投递消息的入口。
	inbox chan *domain.Message

	// waitRequest 把 wait 秒数从 think() 传回主循环去设置定时器。
	waitRequest chan float64

	// 长期记忆摘要器。数据库不可用时为 nil，所有相关功能自动跳过。
	summarizer *memory.Summarizer

	// embedding 客户端。未配置 embedding 模型时为 nil，
	// 记忆检索会自动降级为关键词检索。
	embedding *embedding.Client

	// 人物画像生成器。数据库不可用时为 nil。
	profileBuilder *memory.ProfileBuilder

	// 会话标识。
	key       domain.SessionKey
	sessionID string
	isGroup   bool
	targetID  string // 出站目标（群 openid 或用户 openid）
	accountID string

	// 状态机。
	debouncer *chat.Debouncer
	scheduler *gate.Scheduler
	filter    *chat.Filter

	// 上下文。
	history    []*domain.Message
	pending    []*domain.Message
	botReplies []BotReply

	// lastBatch 是当前这轮思考的触发消息批次。
	//
	// 工具层需要它做两件事：
	//   - 关键词反应只匹配「正在回复的那条」，而不是整批历史
	//   - 人物画像注入需要知道本轮谁在说话
	// 它是每轮 think 开始时的一份快照，不参与持久化。
	lastBatch []*domain.Message

	// 最近外部消息间隔，用于空窗补偿。
	lastExternalAt        float64
	recentIntervals       []intervalSample
	lastExternalMonotonic float64

	// 打断控制。
	cancel context.CancelFunc

	// 工具运行时。
	toolState *State
	registry  *tool.Registry
	botName   string

	// quoteCounter 记录最近 100 条普通消息，供引用判定取快照。
	// 对应 MaiBot 的 ReplyQuoteCounter（maisaka/quote/counter.py）。
	quoteCounter *chat.QuoteCounter

	// onSaved 在出站消息落库成功后回调（hub 注入，驱动 WebUI 实时刷新）。
	// 可以为 nil。只在 run 协程里调用，不需要并发保护。
	onSaved func(sessionID string, isSelf bool)

	// 命中过强制触发（@ 或提及）。
	forcedTrigger bool

	// interruptCtl 管理「思考中来新消息要不要打断」。
	// 对应参考实现 runtime.py:103 PlannerInterruptController。
	interruptCtl *PlannerInterruptController

	// resumeAfterWait 表示这次 think 是 wait 到期后的「继续想想」，
	// 允许在 pending 为空的情况下仍进入一轮判断。
	resumeAfterWait bool

	// 生命周期。
	ctx  context.Context
	quit context.CancelFunc
	wg   sync.WaitGroup
	log  *logx.Logger

	// closed 会被网关协程（Post）与回收协程（Close）同时访问。
	closed atomic.Bool

	// 后台任务的并发闸门（摘要与画像各自限流）。
	summaryMu       sync.Mutex
	summarizing     bool
	updatingProfile bool

	// 统计。lastActiveAt 由 run 协程写、sweep 协程读，用原子存纳秒。
	lastActiveNano atomic.Int64
}

// intervalSample 是一条外部消息的到达时刻。
type intervalSample struct {
	at float64
}

// RuntimeOptions 是创建 Runtime 的参数。
type RuntimeOptions struct {
	Config    *config.Store
	Store     *store.DB
	LLM       *llm.Client
	Prompts   *prompt.Manager
	Registry  *tool.Registry
	Sender    Sender
	Filter    *chat.Filter
	Key       domain.SessionKey
	SessionID string
	TargetID  string
	AccountID string
	BotName   string
	// Embedding 可以为 nil（未配置向量模型时）。
	Embedding *embedding.Client
	// OnSaved 在出站消息落库成功后回调。可以为 nil。
	// sessionID 由 Runtime 自己填（回调签名带参数是为了与
	// router.MessageNotifier 对齐，hub 直接把方法值传进来）。
	OnSaved func(sessionID string, isSelf bool)
}

// NewRuntime 创建并启动一个会话 runtime。
func NewRuntime(parent context.Context, opts RuntimeOptions) *Runtime {
	ctx, cancel := context.WithCancel(parent)

	// 构造期读一次快照就够了：下面这些只用于一次性初始化
	// （门控调度器、要不要建摘要器）。运行期一律走 r.cfg.Get()，
	// 这样 WebUI 改配置后下一轮就生效。
	cfg := opts.Config.Get()

	r := &Runtime{
		cfg:          opts.Config,
		store:        opts.Store,
		llm:          opts.LLM,
		prompts:      opts.Prompts,
		sender:       opts.Sender,
		inbox:        make(chan *domain.Message, 64),
		waitRequest:  make(chan float64, 4),
		key:          opts.Key,
		sessionID:    opts.SessionID,
		isGroup:      opts.Key.Scope == domain.ScopeGroup,
		targetID:     opts.TargetID,
		accountID:    opts.AccountID,
		debouncer:    chat.NewDebouncer(),
		quoteCounter: chat.NewQuoteCounter(),
		interruptCtl: NewPlannerInterruptController(),
		filter:       opts.Filter,
		registry:     opts.Registry,
		botName:      opts.BotName,
		onSaved:      opts.OnSaved,
		ctx:          ctx,
		quit:         cancel,
		log:          logx.Get("agent.runtime"),
	}
	r.touchActive()

	// 门控调度。
	r.scheduler = gate.NewScheduler(
		cfg.Chat.Timing.ReplyTriggerMode == "dynamic",
		gate.BackoffConfig{
			BaseSeconds:        cfg.Chat.Timing.NoActionBackoffBaseSeconds,
			CapSeconds:         cfg.Chat.Timing.NoActionBackoffCapSeconds,
			StartCount:         cfg.Chat.Timing.NoActionBackoffStartCount,
			BypassPendingCount: cfg.Chat.Timing.NoActionBackoffBypassPending,
		},
	)

	// 工具运行时状态。
	r.toolState = &State{
		maxRounds: 8,
		registry:  opts.Registry,
		botName:   opts.BotName,
		avail: tool.Availability{
			SessionID: opts.SessionID,
			IsGroup:   r.isGroup,
			GroupID:   opts.Key.TargetID,
			Platform:  opts.Key.Platform,
		},
	}

	// 组件。
	pb := NewPromptBuilder(opts.Config, opts.Prompts)
	r.loop = NewLoop(opts.Config, opts.LLM, pb, opts.Store)
	r.replyer = NewReplyer(opts.Config, opts.LLM, pb, opts.Store)
	if opts.Store != nil && cfg.Memory.Enabled {
		r.summarizer = memory.NewSummarizer(opts.Config, opts.LLM, opts.Store, opts.Embedding)
		r.profileBuilder = memory.NewProfileBuilder(opts.Config, opts.LLM, opts.Store)
	}
	r.embedding = opts.Embedding

	// 从数据库恢复最近上下文。
	//
	// 对应 MaiBot runtime._restore_recent_context_from_db。
	// 不恢复的后果很直接：进程重启后模型只能看到新到的这一条消息，
	// 完全不知道刚才聊了什么，于是合理地选择 stop —— 表现就是「失忆」。
	r.restoreRecentContext()

	r.wg.Add(1)
	go r.run()

	return r
}

// restoreRecentContext 从消息库载入最近上下文。
func (r *Runtime) restoreRecentContext() {
	if r.store == nil {
		return
	}

	ctx, cancel := context.WithTimeout(r.ctx, 10*time.Second)
	defer cancel()

	limit := r.cfg.Get().SessionContextSize(r.isGroup)
	if limit <= 0 {
		return
	}

	msgs, err := r.store.Messages().Recent(ctx, r.sessionID, limit)
	if err != nil {
		r.log.Warn("恢复最近上下文失败", "session", r.sessionID, "err", err)
		return
	}
	if len(msgs) == 0 {
		return
	}

	// 恢复上下文时必须同步喂给引用计数器。
	//
	// 参考实现（runtime.py:_restore_recent_context_from_db）在循环里
	// 对每条恢复的消息都调 `self._quote_counter.record(message)`。
	// 漏掉这一步的后果：重启后计数器是空的，于是
	//   - 目标消息全部「不在窗口内」→ 每次都直接引用（永远引用）
	//   - 「上一条是不是我说的」「连发几条」全部归零
	// 表现就是重启后引用行为突然变得异常频繁。
	restoringQuoteCounter(r.quoteCounter, msgs)

	// 按来源拆分：自己发的进 botReplies，别人发的进 history。
	//
	// 对应参考实现 runtime.py:526 的 _resolve_restored_message_source_kind：
	// 自己发的标记成 guided_reply，渲染时带 is_self_message 属性。
	// 不拆的后果很隐蔽 —— 机器人自己的话会被当成「某个用户说的」渲染，
	// 于是模型以为自己没说过，重复自己；而 Replyer 也会看到
	// 一个冒充用户的自己在说话。
	restoredHistory, restoredReplies := splitRestoredMessages(msgs, r.accountID)
	r.history = restoredHistory
	r.botReplies = restoredReplies

	r.log.Info("已恢复最近上下文",
		"session", r.sessionID,
		"count", len(restoredHistory),
		"bot_replies", len(restoredReplies),
	)
}

// splitRestoredMessages 把恢复出来的消息按来源拆成两路。
//
// 判定优先用 IsSelf 字段（入库时的来源标记），
// 退化到 accountID 比对 —— 老数据里 IsSelf 全是 0，
// 但机器人账号还能对上，这样升级后也能正确拆分历史。
//
// 超过 20 条的自发消息只留最近 20 条：与 RememberReply 的窗口一致，
// 上下文里不需要无限多的「我说过什么」。
func splitRestoredMessages(msgs []*domain.Message, accountID string) ([]*domain.Message, []BotReply) {
	history := make([]*domain.Message, 0, len(msgs))
	replies := make([]BotReply, 0, len(msgs))

	for _, m := range msgs {
		if m == nil {
			continue
		}
		self := m.IsSelf || (accountID != "" && m.UserID == accountID)
		if !self {
			history = append(history, m)
			continue
		}
		replies = append(replies, BotReply{Text: m.PlainText, At: m.ReceivedAt})
	}

	if len(replies) > 20 {
		replies = replies[len(replies)-20:]
	}
	return history, replies
}

// SessionID 返回聊天流 ID。
func (r *Runtime) SessionID() string { return r.sessionID }

// Post 投递一条消息。
// 非阻塞：inbox 满了就丢弃并记录，避免拖住网关读取循环。
func (r *Runtime) Post(msg *domain.Message) bool {
	if r.closed.Load() {
		return false
	}
	select {
	case r.inbox <- msg:
		return true
	default:
		r.log.Warn("会话 inbox 已满，丢弃消息", "session", r.sessionID, "msg_id", msg.MessageID)
		return false
	}
}

// Close 停止 runtime。
//
// 幂等：Hub 的 sweep 回收与 Hub.Close 可能都调到同一个 runtime。
func (r *Runtime) Close() {
	if r.closed.Swap(true) {
		return
	}
	r.quit()
	r.wg.Wait()
}

// LastActive 返回最近活跃时间，供 hub 做回收判断。
//
// 从未记录过时返回 time.Now()（视为刚活跃），而不是 time.Time{} 的零值。
// 零值早于任何 cutoff，会让「手工构造的、还没来得及记一次活跃的 runtime」
// 在第一次回收时就被当成过期会话关掉。宁可不回收也不能误关。
func (r *Runtime) LastActive() time.Time {
	n := r.lastActiveNano.Load()
	if n == 0 {
		return time.Now()
	}
	return time.Unix(0, n)
}

// touchActive 刷新最近活跃时间。只由 run 协程与构造函数调用。
func (r *Runtime) touchActive() {
	r.lastActiveNano.Store(time.Now().UnixNano())
}

// run 是主循环。
//
// select 同时监听：
//   - 新消息
//   - 防抖到期
//   - 延迟检查到期
//   - wait 到期
//   - 退出
func (r *Runtime) run() {
	defer r.wg.Done()

	// 定时器。
	var (
		debounceTimer *time.Timer
		debounceCh    <-chan time.Time
		delayTimer    *time.Timer
		delayCh       <-chan time.Time
		waitTimer     *time.Timer
		waitCh        <-chan time.Time
	)

	stopTimer := func(t *time.Timer) {
		if t != nil {
			t.Stop()
		}
	}
	defer func() {
		stopTimer(debounceTimer)
		stopTimer(delayTimer)
		stopTimer(waitTimer)
	}()

	armDebounce := func(deadline float64) {
		stopTimer(debounceTimer)
		remaining := deadline - chat.Monotonic()
		if remaining < 0 {
			remaining = 0
		}
		debounceTimer = time.NewTimer(time.Duration(remaining * float64(time.Second)))
		debounceCh = debounceTimer.C
	}

	armDelay := func(seconds float64) {
		stopTimer(delayTimer)
		if seconds <= 0 {
			seconds = 0.1
		}
		delayTimer = time.NewTimer(time.Duration(seconds * float64(time.Second)))
		delayCh = delayTimer.C
	}

	armWait := func(seconds float64) {
		stopTimer(waitTimer)
		if seconds <= 0 {
			seconds = 1
		}
		waitTimer = time.NewTimer(time.Duration(seconds * float64(time.Second)))
		waitCh = waitTimer.C
	}

	for {
		select {
		case <-r.ctx.Done():
			return

		case msg := <-r.inbox:
			r.onMessage(msg, armDebounce)

		case <-debounceCh:
			debounceCh = nil
			r.onDebounceFired(armDelay)

		case <-delayCh:
			delayCh = nil
			r.onDebounceFired(armDelay)

		case <-waitCh:
			waitCh = nil
			r.onWaitFired(armWait)

		case seconds := <-r.waitRequest:
			armWait(seconds)
		}
	}
}

// onMessage 处理一条新到达的消息。
func (r *Runtime) onMessage(msg *domain.Message, armDebounce func(float64)) {
	r.touchActive()

	// 过滤。
	if r.filter != nil {
		if reason := r.filter.BanReason(msg); reason != "" {
			r.log.Debug("消息被过滤", "reason", reason, "msg_id", msg.MessageID)
			return
		}
	}

	// 记入历史。
	r.recordInboundQuote(msg)
	r.history = append(r.history, msg)
	r.pending = append(r.pending, msg)
	r.trimHistory()

	// 记录到达间隔，供空窗补偿。
	now := chat.Monotonic()
	if r.lastExternalMonotonic > 0 {
		r.recentIntervals = append(r.recentIntervals, intervalSample{at: now})
		r.pruneIntervals(now)
	}
	r.lastExternalMonotonic = now
	r.lastExternalAt = now

	// @ 或提及 → 强制触发。
	if msg.IsAt || (msg.IsMentioned && r.cfg.Get().Chat.Timing.MentionedBotReply) ||
		(msg.IsAt && r.cfg.Get().Chat.Timing.InevitableAtReply) {
		r.forcedTrigger = true
		r.scheduler.Backoff().Reset()
		r.log.Info("检测到 @ 或提及，强制进入 Planner", "msg_id", msg.MessageID)
	}

	// 打断正在进行的思考。
	//
	// 这里走控制器而不是直接 cancel：无节制的打断会让模型永远跑不完一轮，
	// 群里连发时表现为「机器人完全不理人」。
	// 上限来自 chat.timing.planner_interrupt_max_consecutive_count，
	// 值为 0（默认）时不打断，等当前请求自然完成。
	if r.interruptCtl != nil {
		result := r.interruptCtl.Request(r.cfg.Get().Chat.Timing.PlannerInterruptMaxConsecutiveCount)
		switch result {
		case InterruptRequest:
			r.log.Info("发起 Planner 打断",
				"session", r.sessionID,
				"msg_id", msg.MessageID,
				"consecutive", r.interruptCtl.Consecutive(),
				"limit", r.cfg.Get().Chat.Timing.PlannerInterruptMaxConsecutiveCount,
			)
		case InterruptLimit:
			r.log.Debug("已达 Planner 连续打断上限，等当前请求完成",
				"session", r.sessionID,
				"consecutive", r.interruptCtl.Consecutive(),
				"limit", r.cfg.Get().Chat.Timing.PlannerInterruptMaxConsecutiveCount,
			)
		}
	} else if r.cancel != nil {
		// 兜底：单测里手工构造的 Runtime 可能没有控制器。
		r.cancel()
		r.cancel = nil
	}

	// 更新防抖截止时间。
	bareImage := r.isBareImage(msg)
	pureEmoji := r.isPureEmoji(msg)
	reason := r.debouncer.Observe(
		msg.UserID,
		bareImage, pureEmoji, msg.IsAt,
		float64(msg.ReceivedAt.UnixNano())/1e9,
		chat.WallNow(),
		chat.Monotonic(),
		chat.DebounceMode(r.cfg.Get().Chat.Timing.DebounceMode),
	)

	if deadline, ok := r.debouncer.Deadline(); ok {
		armDebounce(deadline)
	}

	r.log.Debug("防抖更新",
		"session", r.sessionID,
		"pending", len(r.pending),
		"reason", reason,
	)
}

// onDebounceFired 在防抖到期后判断是否进入 Planner。
func (r *Runtime) onDebounceFired(armDelay func(float64)) {
	if len(r.pending) == 0 {
		r.debouncer.Consume()
		return
	}

	now := chat.Monotonic()

	input := gate.Input{
		PendingCount:      len(r.pending),
		PendingMessageIDs: messageIDs(r.pending),
		IsGroup:           r.isGroup,
		Frequency:         r.effectiveFrequency(),
		HasForcedTrigger:  r.forcedTrigger,
		Likelihood:        buildLikelihoodInput(r.pending, r.history, r.botReplies, time.Now()),
		AverageInterval:   r.averageInterval(),
		LastExternalAt:    r.lastExternalMonotonic,
		Now:               now,
	}

	outcome := r.scheduler.Schedule(input)

	r.log.Info("回复调度",
		"session", r.sessionID,
		"enqueue", outcome.Enqueue,
		"silent", outcome.Silent,
		"detail", outcome.Detail,
	)

	// 记录门控事件。
	//
	// 参考实现把每次门控判断作为 timing_gate.result 事件放进时间线
	// （TimingGateCard，maisaka-monitor.tsx:1031），显示「反应」标签、
	// 三种动作（继续执行/等待/不回复）与耗时。
	// 这是我们原来完全没有的一环：界面上看不到「为什么这次没回复」。
	r.recordGateEvent(input, outcome)

	if outcome.DeferSeconds > 0 {
		armDelay(outcome.DeferSeconds)
		return
	}

	if !outcome.Enqueue {
		// 不放行：批次已消费，消息留在 pending 里等下一次。
		// 但防抖状态要清掉，否则永远不再重新调度。
		r.debouncer.Consume()
		return
	}

	if outcome.Silent {
		// 静默消费：消息进历史但不触发思考。
		r.pending = nil
		r.debouncer.Consume()
		r.forcedTrigger = false
		return
	}

	r.debouncer.Consume()
	r.think()
}

// recordGateEvent 记录一次门控判断。
//
// 为什么值得记：「机器人刚才为什么没理我」是最常被问的问题，
// 而门控是唯一的答案来源。只写日志的话只能翻控制台，
// 而且日志会滚动丢掉。
func (r *Runtime) recordGateEvent(in gate.Input, out gate.Outcome) {
	if r.store == nil {
		return
	}

	payload := struct {
		PendingCount int     `json:"pending_count"`
		IsGroup      bool    `json:"is_group"`
		Frequency    float64 `json:"frequency"`
		Forced       bool    `json:"forced_trigger"`
		AverageGap   float64 `json:"average_interval"`
		Action       string  `json:"action"`
		DeferSeconds float64 `json:"defer_seconds,omitempty"`
		Detail       string  `json:"detail"`
	}{
		PendingCount: in.PendingCount,
		IsGroup:      in.IsGroup,
		Frequency:    in.Frequency,
		Forced:       in.HasForcedTrigger,
		AverageGap:   in.AverageInterval,
		Action:       gateAction(out),
		DeferSeconds: out.DeferSeconds,
		Detail:       out.Detail,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), 3*time.Second)
	defer cancel()

	now := time.Now()
	if err := r.store.Reasoning().Save(ctx, &store.ReasoningRecord{
		SessionID:   r.sessionID,
		Kind:        "gate",
		Name:        gateAction(out),
		Payload:     clipText(string(raw)),
		CreatedAt:   now.Unix(),
		CreatedAtMS: now.UnixMilli(),
	}); err != nil {
		r.log.Warn("写入门控事件失败", "err", err)
	}
}

// gateAction 把门控结论映射成参考实现的三个动作名。
//
// 对应 maisaka-monitor.tsx:1039 的 actionConfig：
// continue（继续执行）/ wait（等待）/ no_action（不回复）。
func gateAction(out gate.Outcome) string {
	switch {
	case out.DeferSeconds > 0:
		return "wait"
	case out.Silent:
		return "no_action"
	case out.Enqueue:
		return "continue"
	default:
		return "no_action"
	}
}

// onWaitFired 在 wait 到期后继续。
//
// wait 的语义是「等一会儿再想想」，所以即使没有新消息也要再进一轮判断 ——
// 这正是模型主动拖延然后重新评估的机制。如果这里直接返回，
// wait 就变成了「停止」，和 stop 没区别，机制就废了。
func (r *Runtime) onWaitFired(_ func(float64)) {
	r.resumeAfterWait = true
	r.think()
}

// think 执行一次完整的思考。
//
// 用循环而不是递归处理「被新消息打断后立刻重新思考」，
// 避免极端情况下递归过深。
func (r *Runtime) think() {
	for {
		// 没有待处理消息时也允许进入一轮思考：wait 到期后的「继续想想」
		// 就是这种情况 —— 此时 pending 为空，但需要基于已有上下文重新判断。
		if len(r.pending) == 0 && !r.resumeAfterWait {
			return
		}

		batch := r.pending
		r.pending = nil
		r.resumeAfterWait = false
		r.forcedTrigger = false
		r.toolState.ResetForNewTurn()
		r.toolState.ClearWait()
		r.lastBatch = batch

		// 为本轮建立可取消的 context，用于打断。
		ctx, cancel := context.WithCancel(r.ctx)
		r.cancel = cancel
		if r.interruptCtl != nil {
			r.interruptCtl.Bind(cancel)
		}

		// batch 为空时 history 就是全部上下文。
		history := r.historyBefore(batch)

		in := TurnInput{
			SessionID:  r.sessionID,
			IsGroup:    r.isGroup,
			GroupID:    r.key.TargetID,
			Messages:   batch,
			History:    history,
			BotReplies: r.botReplies,
			Now:        time.Now(),
		}

		result, err := r.loop.RunTurn(ctx, in, r, r.toolState)
		cancel()
		r.cancel = nil
		if r.interruptCtl != nil {
			// 被新消息打断才算「连续打断」，自然结束会清零计数。
			r.interruptCtl.Unbind(err == nil && result.Interrupted)
		}

		if err != nil {
			r.log.Error("思考失败", "session", r.sessionID, "err", err)
			r.scheduler.Backoff().RecordCycleResult(CycleEndError, r.isGroup, chat.Monotonic())
			return
		}

		r.log.Info("思考结束",
			"session", r.sessionID,
			"reason", result.End.Reason,
			"rounds", result.Rounds,
		)

		// 交给空闲退避累计。这是很容易漏的一环。
		r.scheduler.Backoff().RecordCycleResult(result.End.Reason, r.isGroup, chat.Monotonic())

		switch result.End.Reason {
		case CycleEndWaitTool:
			requested, seconds, _ := r.toolState.WaitState()
			if !requested {
				return
			}
			if r.toolState.ConsecutiveWaits() >= r.cfg.Get().Chat.Timing.MaxConsecutiveWaitCount {
				r.log.Info("连续 wait 达到上限，进入休息", "session", r.sessionID)
				r.toolState.ResetWaits()
				r.scheduler.Backoff().RecordCycleResult(CycleEndWaitRest, r.isGroup, chat.Monotonic())
				return
			}
			// 把秒数交给主循环去设定时器，然后退出本层。
			r.armWaitFromThink(seconds)
			return

		case CycleEndReplied:
			r.toolState.ResetWaits()
			r.scheduler.RecordReply(chat.Monotonic())
			// 回复后如果恰好又攒了新消息，继续处理。
			if len(r.pending) == 0 {
				return
			}
			continue

		case CycleEndInterrupted:
			// 被新消息打断：如果确实有新消息，立刻重新判断。
			if len(r.pending) == 0 {
				return
			}
			continue

		default:
			return
		}
	}
}

// armWaitFromThink 在思考返回后安排 wait 定时器。
//
// 因为定时器变量在 run() 的闭包里，这里通过向 inbox 之外的方式传递不合适，
// 所以用一个专门的 channel 通知主循环。
func (r *Runtime) armWaitFromThink(seconds float64) {
	select {
	case r.waitRequest <- seconds:
	default:
		// 已经有挂起的等待请求，忽略。
	}
}

// waitRequest 用于把 wait 秒数从 think 传回 run。
// 有缓冲，避免阻塞。

// History 返回当前上下文，供工具读取。
func (r *Runtime) History() []*domain.Message {
	out := make([]*domain.Message, len(r.history))
	copy(out, r.history)
	return out
}

// historyBefore 返回本批消息之前的上下文。
func (r *Runtime) historyBefore(batch []*domain.Message) []*domain.Message {
	if len(r.history) <= len(batch) {
		return nil
	}
	return r.history[:len(r.history)-len(batch)]
}

// trimHistory 裁剪上下文到配置上限。
//
// 裁掉的消息不能白白丢掉 —— 它们正是「聊天回想」的原料。
// 参考实现在同一个位置触发 mid_term memory 构建（裁切时把被丢掉的那段压缩成回想），
// 这里保持一致：裁得够多就丢一个后台任务去摘要，不阻塞消息处理。
func (r *Runtime) trimHistory() {
	limit := r.cfg.Get().SessionContextSize(r.isGroup)
	if limit <= 0 || len(r.history) <= limit {
		return
	}
	excess := len(r.history) - limit
	removed := r.history[:excess]
	r.history = r.history[excess:]

	// 记录裁切事件。
	//
	// 参考实现的 flow.step: context.trimmed 是时间线上的一类独立事件
	// （ContextTrimCard，maisaka-monitor.tsx:2166），
	// 因为它意味着「模型从这一刻起看不到更早的内容了」——
	// 排查「机器人怎么忘了刚才说的」时，这是唯一线索。
	r.recordTrimEvent(len(removed), limit)

	r.queueSummary(removed)
}

// recordTrimEvent 记录一次上下文裁切。
func (r *Runtime) recordTrimEvent(removed, keep int) {
	if r.store == nil || removed <= 0 {
		return
	}
	payload := fmt.Sprintf(`{"removed":%d,"keep":%d}`, removed, keep)

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), 3*time.Second)
	defer cancel()

	now := time.Now()
	if err := r.store.Reasoning().Save(ctx, &store.ReasoningRecord{
		SessionID:   r.sessionID,
		Kind:        "trim",
		Name:        "context.trimmed",
		Payload:     payload,
		CreatedAt:   now.Unix(),
		CreatedAtMS: now.UnixMilli(),
	}); err != nil {
		r.log.Warn("写入裁切事件失败", "err", err)
	}
}

// queueSummary 把被裁掉的消息交给摘要器。
//
// 异步且失败不报错：摘要只是「让模型以后还能想起来」的优化，
// 它既不能阻塞回复，也不能因为模型抽风而影响主流程。
//
// 节流：同一会话最多同时跑一个摘要任务。
// 历史一下子裁掉一大批时，逐条触发会瞬间打出去几十个请求。
func (r *Runtime) queueSummary(removed []*domain.Message) {
	if r.summarizer == nil || len(removed) == 0 {
		return
	}
	if !r.cfg.Get().Memory.Enabled || !r.cfg.Get().Memory.SummaryEnabled {
		return
	}
	if len(removed) < r.cfg.Get().Memory.SummaryMinMessages {
		return
	}

	// 抄一份：removed 是底层数组的切片，后续 history 追加会覆盖它。
	batch := make([]*domain.Message, len(removed))
	copy(batch, removed)

	r.summaryMu.Lock()
	if r.summarizing {
		r.summaryMu.Unlock()
		return
	}
	r.summarizing = true
	r.summaryMu.Unlock()

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer func() {
			r.summaryMu.Lock()
			r.summarizing = false
			r.summaryMu.Unlock()
		}()

		ctx, cancel := context.WithTimeout(r.ctx, 120*time.Second)
		defer cancel()

		if _, err := r.summarizer.Summarize(ctx, r.sessionID, r.key.TargetID, batch); err != nil {
			r.log.Warn("生成聊天回想失败", "session", r.sessionID, "err", err)
		}
	}()
}

// pruneIntervals 只保留最近 30 分钟的间隔样本。
func (r *Runtime) pruneIntervals(now float64) {
	const window = 1800.0
	cut := now - window
	i := 0
	for i < len(r.recentIntervals) && r.recentIntervals[i].at < cut {
		i++
	}
	r.recentIntervals = r.recentIntervals[i:]
}

// averageInterval 返回最近外部消息的平均间隔秒数。
func (r *Runtime) averageInterval() float64 {
	if len(r.recentIntervals) < 2 {
		return 0
	}
	first := r.recentIntervals[0].at
	last := r.recentIntervals[len(r.recentIntervals)-1].at
	span := last - first
	if span <= 0 {
		return 0
	}
	return span / float64(len(r.recentIntervals)-1)
}

// effectiveFrequency 返回当前会话生效的回复频率。
func (r *Runtime) effectiveFrequency() float64 {
	return r.cfg.Get().TalkValue(r.isGroup)
}

// messageShape 返回消息的「形状」，供防抖逻辑使用。
//
// 逐字照抄参考实现 refs/MaiBot/src/chat/debounce/message_adapter.py:get_message_shape：
//
//	kind = 1 if "image" in names else 2 if "emoji" in names else 3 if ... else 0
//	return kind, len(text)   # text 只来自 TextComponent
//
// kind: 0 = 纯文本/未知，1 = 含图片，2 = 含表情包（无图片），3 = 语音/文件/转发等。
//
// 为什么不能用 PlainText 判空：PlainText 是把段渲染成可读文本的结果，
// 图片会变成 "[图片]"、表情包会变成 "[表情包]" —— 它永远不会为空。
// 早期用 PlainText 判空导致 isBareImage / isPureEmoji 恒为 false，
// 于是「裸图开批至少 4 秒」与「纯表情包不延长等待」两条规则静默失效。
func messageShape(msg *domain.Message) (kind, textLen int) {
	if msg == nil {
		return 0, 0
	}

	var (
		hasImage bool
		hasEmoji bool
		hasOther bool // 语音 / 文件 / 转发
		text     strings.Builder
	)
	for _, seg := range msg.Segments {
		switch seg.Type {
		case domain.SegmentText:
			text.WriteString(seg.Text)
		case domain.SegmentImage:
			hasImage = true
		case domain.SegmentEmoji:
			hasEmoji = true
		case domain.SegmentVoice, domain.SegmentFile, domain.SegmentForward:
			hasOther = true
		}
	}

	switch {
	case hasImage:
		kind = 1
	case hasEmoji:
		kind = 2
	case hasOther:
		kind = 3
	default:
		kind = 0
	}

	return kind, len([]rune(strings.TrimSpace(text.String())))
}

// isBareImage 判断是否只有图片没有文字。
//
// 对应参考实现 runtime.py: is_bare_image=kind == 1 and length == 0。
func (r *Runtime) isBareImage(msg *domain.Message) bool {
	kind, length := messageShape(msg)
	return kind == 1 && length == 0
}

// isPureEmoji 判断是否只有表情包。
//
// 对应参考实现 runtime.py: is_pure_emoji=kind == 2 and length == 0。
func (r *Runtime) isPureEmoji(msg *domain.Message) bool {
	kind, length := messageShape(msg)
	return kind == 2 && length == 0
}

// ============ tool.Runtime 接口实现 ============

// IsGroup 返回是否群聊。
func (r *Runtime) IsGroup() bool { return r.isGroup }

// BotName 返回机器人名字。
func (r *Runtime) BotName() string { return r.botName }

// Conversation 返回最近的对话历史。
//
// 与 BuildReplyerMessages 保持同一套角色约定：机器人的发言是 assistant。
// 两边不一致的话，WebUI 时间线与“模型实际看到的上下文”会打架。
func (r *Runtime) Conversation() []llm.Message {
	items := mergeHistoryItems(r.history, r.botReplies, r.botName)
	out := make([]llm.Message, 0, len(items))
	for _, item := range items {
		if item.isSelf {
			if s := strings.TrimSpace(item.body); s != "" {
				out = append(out, llm.Message{Role: llm.RoleAssistant, Content: s})
			}
			continue
		}
		out = append(out, llm.Message{Role: llm.RoleUser, Content: item.text})
	}
	return out
}

// Wait 请求等待。
func (r *Runtime) Wait(seconds float64, reason string) error {
	if r.toolState.ConsecutiveWaits() >= r.cfg.Get().Chat.Timing.MaxConsecutiveWaitCount {
		return fmt.Errorf("连续 wait 已达上限")
	}
	r.toolState.waitRequested = true
	r.toolState.waitSeconds = seconds
	r.toolState.waitReason = reason
	return nil
}

// Stop 结束本轮思考。
func (r *Runtime) Stop(reason string) {
	r.toolState.stopped = true
	r.toolState.stopReason = reason
}

// Reply 执行一次真实回复。
//
// 这是两段式的交界处：Planner 请求回复 → Replyer 生成正文 → 发送。
//
// 发送阶段的顺序严格对齐参考实现（refs/MaiBot/src/maisaka/builtin_tool/reply.py
// 与 builtin_tool/context.py:post_process_reply_message_items_async）：
//
//	第一条：@前缀 + 正文（引用挂在第一条上）
//	后续各条：正文分段，不重复 @ 也不重复引用
//	最后：图片、表情包各自单独一条（平台限制：一条消息只能一种内容）
func (r *Runtime) Reply(req tool.ReplyRequest) (tool.ReplyOutcome, error) {
	ctx, cancel := context.WithTimeout(r.ctx, 90*time.Second)
	defer cancel()

	// 第二段：生成正文。
	//
	// 三份补充材料在这里一起准备好：
	//   - 关键词反应：命中配置规则时给 Replyer 的硬要求
	//   - 表达习惯：从群里学到的句式与语气
	//
	// 两者都只影响「怎么说」，不改变「说什么」，所以放在 Replyer 而不是 Planner。
	// 参考实现也是这么分的：Planner 看不到 expression_habits 与 keyword_reaction。
	text, err := r.replyer.Generate(ctx, ReplyInput{
		SessionID:        r.sessionID,
		TurnID:           req.TurnID,
		IsGroup:          r.isGroup,
		History:          r.history,
		BotReplies:       r.botReplies,
		Reference:        req.Reference,
		Style:            req.Style,
		TargetMsgID:      req.MsgID,
		Now:              time.Now(),
		KeywordReaction:  r.buildKeywordReaction(),
		ExpressionHabits: r.buildExpressionHabits(),
		// 附件告诉 Replyer 而不是只告诉发送层：
		// 不说的话模型会在正文里写「（图）」或描述一张它杜撰的图。
		AttachPics:  req.AttachPics,
		AttachEmoji: req.AttachEmoji,
	})
	if err != nil {
		return tool.ReplyOutcome{}, err
	}

	// 分割成多条。
	chunks := chat.SplitResponse(text, chat.SplitConfig{
		Enable:         r.cfg.Get().Response.EnableSplit,
		MaxLength:      r.cfg.Get().Response.MaxLength,
		MaxSentenceNum: r.cfg.Get().Response.MaxSentenceNum,
		MaxSplitNum:    r.cfg.Get().Response.MaxSplitNum,
	})
	if len(chunks) == 0 {
		return tool.ReplyOutcome{}, fmt.Errorf("回复内容为空")
	}

	// 引用只在第一条上抽一次，后续分段沿用同一个判定结果。
	// 参考实现就是这么做的：`_resolve_reply_quote` 只对首段正文抽样。
	//
	// 注意 quote 与 passiveTo 是两件不同的事：
	//   passiveTo = req.MsgID  占哪条入站消息的被动回复额度
	//   quote                 引用卡片指向哪条消息（REFIDX_ 形态）
	// 把两者混成一个参数正是之前引用发不出的原因之一。
	quote := r.decideQuote(req.MsgID, chunks[0])

	// @ 前缀只挂在第一条上，与参考实现一致。
	atSegments := r.buildAtSegments(req.AttachAt)

	var allIDs []string
	var allRefIdxs []string
	for i, chunk := range chunks {
		if i > 0 {
			// 多条之间留一点间隔，像人在连续打字。
			delay := chat.CalculateTypingTime(chunk, chat.TypingConfig{
				Speed:   r.cfg.Get().Response.TypingSpeed,
				Enabled: r.cfg.Get().Response.TypingSpeed > 0,
			})
			select {
			case <-ctx.Done():
				return tool.ReplyOutcome{
					Text:       strings.Join(chunks[:i], ""),
					MessageIDs: allIDs,
					RefIdxs:    allRefIdxs,
				}, ctx.Err()
			case <-time.After(delay):
			}
		}

		// 首条带 @ 前缀，后续各条只有正文。
		// 引用卡片也只在首条上挂（由 Send 内部处理）。
		segments := make([]domain.Segment, 0, len(atSegments)+1)
		var chunkQuote domain.Reference
		if i == 0 {
			segments = append(segments, atSegments...)
			chunkQuote = quote
		}
		segments = append(segments, domain.Text(chunk))

		sent, err := r.sender.SendSegments(ctx, r.scene(), r.targetID, segments, req.MsgID, chunkQuote)
		if err != nil {
			return tool.ReplyOutcome{Text: text, MessageIDs: allIDs, RefIdxs: allRefIdxs}, err
		}
		allIDs = append(allIDs, sent.MessageIDs...)
		allRefIdxs = append(allRefIdxs, sent.RefIdxs...)

		// 发出去的内容必须落库：重启后恢复上下文要靠它，
		// WebUI 的会话页也要能看到机器人说过什么。
		// 用独立 ctx，避免发送成功后调用方 ctx 已经取消而静默丢写。
		storeCtx, cancelStore := outboundContext(r.ctx)
		r.recordOutbound(storeCtx, segments, sent, chunkQuote.RefIdx)
		cancelStore()
	}

	// 图片：每条单独发，各自占一个被动回复序号。
	imgIDs, err := r.sendAttachedImages(ctx, req.AttachPics)
	allIDs = append(allIDs, imgIDs...)
	if err != nil {
		// 正文已经发出去了，附件失败不该让上层以为整条回复失败。
		r.log.Warn("附加图片发送失败", "session", r.sessionID, "err", err)
	}

	// 刚发出的消息也要进引用计数器，否则下一轮的「上一句是不是我说的」是错的。
	r.recordOutboundQuote(allIDs)

	// 顺手更新本轮说话人的画像。
	//
	// 放在「回复成功之后」而不是「收到消息时」：回复成功说明这轮对话
	// 有实质内容且模型判断值得回应，是画像更新的好时机；
	// 而且不满足阈值时它立即返回，不产生任何模型调用。
	r.queueProfileUpdate()

	// 表情包：与图片一样单独一条。
	//
	// 异步发、不等结果：表情包只是锦上添花，不值得让整条回复多等一次上传。
	// 失败只记日志 —— 正文已经发出去了。
	if emo := strings.TrimSpace(req.AttachEmoji); emo != "" {
		if ref, ok := r.PickEmoji(emo); ok {
			if ids, eerr := r.SendEmoji(ref); eerr == nil {
				allIDs = append(allIDs, ids...)
			} else {
				r.log.Warn("附带表情包发送失败", "session", r.sessionID, "err", eerr)
			}
		}
	}

	return tool.ReplyOutcome{Text: text, MessageIDs: allIDs, RefIdxs: allRefIdxs}, nil
}

// queueProfileUpdate 在后台更新本轮说话人的画像。
//
// 异步：画像要花一次模型调用，绝不能拖住回复。
// 同一时刻只跑一个：几个人同时说话时，串行更新比并发更省，
// 也不会因为并发写同一条画像记录互相覆盖。
func (r *Runtime) queueProfileUpdate() {
	if r.profileBuilder == nil || len(r.lastBatch) == 0 {
		return
	}
	if !r.cfg.Get().Memory.Enabled || !r.cfg.Get().Memory.ProfileEnabled {
		return
	}

	r.summaryMu.Lock()
	if r.updatingProfile {
		r.summaryMu.Unlock()
		return
	}
	r.updatingProfile = true
	r.summaryMu.Unlock()

	batch := make([]*domain.Message, len(r.lastBatch))
	copy(batch, r.lastBatch)

	// 本轮可能有多人说话，逐个尝试；不满足阈值的会立即返回。
	targets := make([]struct{ id, name string }, 0, 4)
	seen := make(map[string]struct{}, 4)
	for _, m := range batch {
		if m == nil || m.UserID == "" {
			continue
		}
		if _, dup := seen[m.UserID]; dup {
			continue
		}
		seen[m.UserID] = struct{}{}
		targets = append(targets, struct{ id, name string }{m.UserID, m.UserName})
		if len(targets) >= 4 {
			break
		}
	}

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer func() {
			r.summaryMu.Lock()
			r.updatingProfile = false
			r.summaryMu.Unlock()
		}()

		ctx, cancel := context.WithTimeout(r.ctx, 120*time.Second)
		defer cancel()

		for _, t := range targets {
			if _, err := r.profileBuilder.UpdateIfNeeded(
				ctx, r.key.Platform, t.id, r.sessionID, t.name, batch,
			); err != nil {
				r.log.Warn("更新人物画像失败", "user", t.id, "err", err)
			}
		}
	}()
}

// buildKeywordReaction 返回命中关键词反应的提示文本。
//
// 传进去的是「最近一条用户消息」，与参考实现一致：
// 只对正在回复的那条消息做匹配，而不是整个批次 ——
// 否则一条旧消息的关键词会在之后每条回复里反复触发。
func (r *Runtime) buildKeywordReaction() string {
	m := newKeywordMatcher(r.cfg.Get().Keyword.Rules)
	if m == nil {
		return ""
	}

	botIDs := make(map[string]struct{}, 2)
	if id := r.cfg.Get().Bot.AccountID; id != "" {
		botIDs[id] = struct{}{}
	}
	if id := r.cfg.Get().QQ.UnifiedAccountID; id != "" {
		botIDs[id] = struct{}{}
	}

	return m.match(lastUserText(r.lastBatch, botIDs))
}

// buildExpressionHabits 取几条学到的表达拼成注入文本。
func (r *Runtime) buildExpressionHabits() string {
	if r.store == nil || !r.cfg.Get().Learning.ExpressionEnabled {
		return ""
	}

	ctx, cancel := context.WithTimeout(r.ctx, 3*time.Second)
	defer cancel()

	items, err := r.store.Memory().ListExpressions(ctx, r.sessionID, r.cfg.Get().Learning.ExpressionMaxUsed)
	if err != nil {
		r.log.Warn("读取表达习惯失败", "session", r.sessionID, "err", err)
		return ""
	}
	if len(items) == 0 {
		return ""
	}

	lines := make([]string, 0, len(items))
	ids := make([]int64, 0, len(items))
	for _, it := range items {
		line := formatExpression(it.Situation, it.Style)
		if line == "" {
			continue
		}
		lines = append(lines, line)
		ids = append(ids, it.ID)
	}
	if len(lines) == 0 {
		return ""
	}

	// 用了就记一次，靠 used_count 做轮换：老是同几条会让说话方式变僵。
	if err := r.store.Memory().MarkExpressionsUsed(ctx, ids, time.Now()); err != nil {
		r.log.Warn("记录表达使用失败", "err", err)
	}

	return buildExpressionHabits(lines)
}

// buildAtSegments 把 attach_at 的 msg_id 列表解析成 at 段。
//
// 无法解析的目标直接跳过而不是报错：@ 只是锦上添花，
// 不应该因为模型填错一个 msg_id 就让整条回复发不出去。
func (r *Runtime) buildAtSegments(msgIDs []string) []domain.Segment {
	if len(msgIDs) == 0 {
		return nil
	}

	out := make([]domain.Segment, 0, len(msgIDs))
	seen := make(map[string]struct{}, len(msgIDs))
	for _, raw := range msgIDs {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		target := r.findMessageByID(id)
		if target == nil {
			r.log.Warn("attach_at 目标消息不在上下文内，跳过", "msg_id", id)
			continue
		}
		if target.UserID == "" {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, domain.At(target.UserID, target.UserName))
	}
	return out
}

// sendAttachedImages 发送随回复附带的图片。
//
// 图片来源是上下文消息里的 image/emoji 段：解析出二进制后重新上传发送。
// 参考实现对应的入口是 resolve_context_image。
func (r *Runtime) sendAttachedImages(ctx context.Context, pics []tool.AttachPic) ([]string, error) {
	if len(pics) == 0 {
		return nil, nil
	}

	var (
		ids     []string
		lastErr error
	)
	for _, pic := range pics {
		target := r.findMessageByID(pic.MsgID)
		if target == nil {
			r.log.Warn("attach_pic 目标消息不在上下文内，跳过", "msg_id", pic.MsgID)
			continue
		}
		binary, ok := pickImageBinary(target, pic.Index)
		if !ok {
			r.log.Warn("attach_pic 找不到对应图片", "msg_id", pic.MsgID, "index", pic.Index)
			continue
		}

		segments := []domain.Segment{{
			Type:   domain.SegmentImage,
			Binary: binary,
			URL:    pic.MsgID,
		}}
		sent, err := r.sender.SendSegments(ctx, r.scene(), r.targetID, segments, "", domain.Reference{})
		if err != nil {
			lastErr = err
			continue
		}
		ids = append(ids, sent.MessageIDs...)

		storeCtx, cancelStore := outboundContext(r.ctx)
		r.recordOutbound(storeCtx, segments, sent, "")
		cancelStore()
	}
	return ids, lastErr
}

// pickImageBinary 从消息里挑出第 index 张图片的二进制。
//
// 只认 image/emoji 段：这两类才有可能带二进制数据。
func pickImageBinary(msg *domain.Message, index int) ([]byte, bool) {
	if index < 0 {
		index = 0
	}
	seen := 0
	for _, seg := range msg.Segments {
		if seg.Type != domain.SegmentImage && seg.Type != domain.SegmentEmoji {
			continue
		}
		if len(seg.Binary) == 0 {
			// 没有二进制（例如来自数据库回填的占位段）时不算一张图。
			continue
		}
		if seen == index {
			return seg.Binary, true
		}
		seen++
	}
	return nil, false
}

// scene 返回出站场景。
func (r *Runtime) scene() string {
	if r.isGroup {
		return "group"
	}
	return "private"
}
