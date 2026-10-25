// Package router 负责把消息路由到对应会话的 agent runtime。
//
// 对应 MaiBot 的 chat/message_receive/chat_manager.py + heart_flow/heartflow_manager.py。
//
// 放在独立包里的原因是：agent 依赖 chat（分割、打字延迟），
// 而 hub 同时依赖 agent 和 chat，放在 chat 里会形成循环引用。
package router

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/agent"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/chat"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/embedding"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/llm"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/logx"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/prompt"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/store"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/tool"
)

// Hub 管理所有会话 runtime。
//
// 对应 MaiBot 的 heartflow_manager：
//   - 按 session_id 惰性创建 runtime
//   - 闲置回收，避免长期运行后 goroutine 泄漏
//   - 上下文不常驻内存，回收后从数据库重建
type Hub struct {
	cfg      *config.Store
	store    *store.DB
	llm      *llm.Client
	prompts  *prompt.Manager
	registry *tool.Registry
	sender   agent.Sender
	filter   *chat.Filter
	// emoji 可以为 nil（表情包功能关闭时）。
	emoji EmojiCollector
	// embedding 可以为 nil。
	embedding *embedding.Client

	mu       sync.RWMutex
	runtimes map[string]*agent.Runtime
	// ctx 是父 context，用于派生 runtime 的 context。
	ctx context.Context

	// notifyMu 保护 notifier。SetNotifier 在 WebUI 就绪后调用
	// （app.go 里 hub 先建、web 后建），而 Dispatch 在网关协程跑，
	// 两边跨协程，必须加锁。
	notifyMu  sync.Mutex
	notifier  MessageNotifier
	// cancel 是 Hub 自己的取消函数，Close 时调用。
	// 不能直接依赖父 ctx：父 ctx 可能永远不会取消（例如整个进程的根 ctx），
	// 那样 Close 就会永远等下去。
	cancel context.CancelFunc

	log *logx.Logger

	// 回收参数。
	idleTimeout time.Duration
	sweepEvery  time.Duration

	wg sync.WaitGroup
}

// EmojiCollector 是入站表情包的收藏能力。
//
// 用接口而不是直接依赖 emoji 包：Hub 只在收消息时顺手存一张图，
// 不需要知道表情包管理器的全部细节，也避免路由层耦合太多子系统。
type EmojiCollector interface {
	Collect(ctx context.Context, binary []byte, mimeType, sessionID, userID string) (bool, error)
}

// MessageNotifier 在消息落库后收到通知，WebUI 用它驱动前端实时刷新。
//
// 对应 MaiBot dashboard 的统一 WebSocket（unified.py 的 chat 域事件：
// 消息入库后立刻推给前端，页面不用等轮询）。这里用接口而不是
// 直接依赖 web 包：web 也依赖 router（Hub 接口），直接引用会成环。
type MessageNotifier interface {
	// MessageSaved 报告一条消息已写入数据库。
	// isSelf 区分机器人自己的发言（出站）与别人的发言（入站）。
	MessageSaved(sessionID string, isSelf bool)
}

// HubOptions 是创建 Hub 的参数。
type HubOptions struct {
	Config   *config.Store
	Store    *store.DB
	LLM      *llm.Client
	Prompts  *prompt.Manager
	Registry *tool.Registry
	Sender   agent.Sender
	Filter   *chat.Filter
	// Emoji 可以为 nil。
	Emoji EmojiCollector
	// Embedding 可以为 nil（未配置向量模型时，记忆检索降级为关键词）。
	Embedding *embedding.Client
}

// NewHub 创建 Hub 并启动回收协程。
func NewHub(parent context.Context, opts HubOptions) *Hub {
	ctx, cancel := context.WithCancel(parent)

	h := &Hub{
		cfg:         opts.Config,
		store:       opts.Store,
		llm:         opts.LLM,
		prompts:     opts.Prompts,
		registry:    opts.Registry,
		sender:      opts.Sender,
		filter:      opts.Filter,
		emoji:       opts.Emoji,
		embedding:   opts.Embedding,
		runtimes:    make(map[string]*agent.Runtime),
		ctx:         ctx,
		cancel:      cancel,
		log:         logx.Get("chat.hub"),
		idleTimeout: 30 * time.Minute,
		sweepEvery:  5 * time.Minute,
	}

	h.wg.Add(1)
	go h.sweep()

	return h
}

// SetNotifier 注入消息通知方（WebUI 服务器）。
//
// hub 在 app.go 里先于 web 创建，所以只能建好之后再挂；
// 等价于 logx.SetBroadcaster(a.web) 的注入方式。
// 传 nil 表示不通知（测试场景）。
func (h *Hub) SetNotifier(n MessageNotifier) {
	h.notifyMu.Lock()
	h.notifier = n
	h.notifyMu.Unlock()
}

// notifySaved 在锁外调用通知方：MessageSaved 的实现自己会拿锁
// （web.Server 广播时要锁 clients），持着 notifyMu 调它是给
// 未来的实现埋死锁。
func (h *Hub) notifySaved(sessionID string, isSelf bool) {
	h.notifyMu.Lock()
	n := h.notifier
	h.notifyMu.Unlock()
	if n != nil {
		n.MessageSaved(sessionID, isSelf)
	}
}

// Dispatch 把一条消息投递到对应会话。
//
// 步骤：
//  1. 计算 session key
//  2. 落库
//  3. 更新会话与用户信息
//  4. 投递给 runtime
func (h *Hub) Dispatch(msg *domain.Message, scene, targetID, accountID string) {
	key := domain.SessionKey{
		Platform:  msg.Platform,
		AccountID: accountID,
		Scope:     scene,
		TargetID:  targetID,
	}
	sessionID := key.ID()
	msg.SessionID = sessionID
	msg.AccountID = accountID

	ctx, cancel := context.WithTimeout(h.ctx, 5*time.Second)
	defer cancel()

	// 落库。
	if err := h.store.Messages().Save(ctx, msg); err != nil {
		h.log.Warn("消息落库失败", "err", err, "msg_id", msg.MessageID)
	} else {
		// 落库成功才通知：前端收到事件会来拉增量，
		// 没进库的消息拉不到，通知了反而制造一次空调用。
		h.notifySaved(sessionID, false)
	}

	// 补齐「引用了谁的消息」。
	//
	// 必须在落库之后：这里靠按 ref_msg_idx 反查库。
	// QQ 的 msg_elements 元素不含 author（实测 0/11），
	// 所以被引消息的发送者只能本地回查。
	h.fillQuotedSender(ctx, msg)

	// 会话与用户元信息。
	h.touchSession(ctx, msg, key, sessionID)
	h.touchPerson(ctx, msg)

	// 顺手收藏别人发的表情包。
	h.collectEmoji(ctx, msg, sessionID)

	// 投递。
	rt := h.getOrCreate(key, sessionID, targetID, accountID)
	if rt == nil {
		// Hub 已关闭，不再接受新会话。落库已经做过了，
		// 所以数据没丢，只是不回复。
		h.log.Debug("会话已关闭，丢弃投递", "session", sessionID)
		return
	}
	if !rt.Post(msg) {
		h.log.Warn("投递失败，会话不可用", "session", sessionID)
	}
}

// getOrCreate 取或创建 runtime。
//
// 先检查 ctx 是否已取消：Hub 关闭后网关可能还会推来几条在途消息，
// 此时不该再建新 runtime —— 新建出来的 runtime 的 ctx 也是已取消的，
// 它只会白白做一次从数据库恢复上下文的读取，然后 Post 失败。
func (h *Hub) getOrCreate(key domain.SessionKey, sessionID, targetID, accountID string) *agent.Runtime {
	if h.ctx.Err() != nil {
		return nil
	}

	h.mu.RLock()
	rt, ok := h.runtimes[sessionID]
	h.mu.RUnlock()
	if ok {
		return rt
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	// 双检，避免并发创建。
	if rt, ok := h.runtimes[sessionID]; ok {
		return rt
	}

	// 拿锁期间可能刚好关闭，再检查一次。
	if h.ctx.Err() != nil {
		return nil
	}

	// 网页对话窗口（platform=webui）的回复只回网页，不走 QQ 适配器。
	// 对应 MaiBot 里 webui 平台自己的 sender；没有这一步，
	// 网页会话的回复会被当成私聊消息发给一个不存在的 openid，全部失败。
	sender := h.sender
	if key.Platform == domain.PlatformWebUI {
		sender = agent.WebUISender{}
	}

	rt = agent.NewRuntime(h.ctx, agent.RuntimeOptions{
		Config:    h.cfg,
		Store:     h.store,
		LLM:       h.llm,
		Prompts:   h.prompts,
		Registry:  h.registry,
		Sender:    sender,
		Filter:    h.filter,
		Key:       key,
		SessionID: sessionID,
		TargetID:  targetID,
		AccountID: accountID,
		BotName:   h.cfg.Get().Bot.Nickname,
		Embedding: h.embedding,
		// 出站落库的通知走 hub 转发（闭包读 hub 的最新 notifier），
		// 而不是构造时快照：runtime 可能活得很久，期间 WebUI
		// 随时可能刚建好、刚注入 notifier。
		OnSaved: h.notifySaved,
	})

	h.runtimes[sessionID] = rt
	h.log.Info("创建会话 runtime", "session", sessionID, "scope", key.Scope)
	return rt
}

// touchSession 更新会话记录。
//
// 群名只在这一步能拿到：QQ 官方事件有时带 group_name 有时不带，
// 而 Upsert 的冲突分支是「新值为空则保留旧值」，
// 所以只要有过一次带名字的事件，群名就不会再丢。
//
// 私聊的展示名直接用对端昵称，不再拼「xxx 的私聊」——
// 后者在会话列表里宽度翻倍而信息量没多。
func (h *Hub) touchSession(ctx context.Context, msg *domain.Message, key domain.SessionKey, sessionID string) {
	displayName := ""
	if msg.IsGroup {
		displayName = msg.GroupName
	} else if msg.UserName != "" {
		displayName = msg.UserName
	}

	s := &domain.Session{
		ID:           sessionID,
		Platform:     key.Platform,
		AccountID:    key.AccountID,
		Scope:        key.Scope,
		TargetID:     key.TargetID,
		DisplayName:  displayName,
		MessageCount: 1,
		LastActiveAt: msg.ReceivedAt,
		CreatedAt:    time.Now(),
	}
	if err := h.store.Sessions().Upsert(ctx, s); err != nil {
		h.log.Warn("更新会话失败", "err", err, "session", sessionID)
	}
}

// fillQuotedSender 按被引消息的引用索引回查发送者，补进 additional。
//
// 为什么要单独做：QQ 的 msg_elements 元素**不含 author**（实测 0/11），
// 事件里根本没说被引消息是谁发的。只能靠自己在入站时记下每条消息的
// msg_idx（存进 messages.ref_msg_idx），再拿 ref_msg_idx 反查。
//
// 查不到时只记调试日志、不报错：对方那条没收到、或已被保留期清掉，
// 就退化成「不知道是谁」，引用正文依旧能用 —— 部分信息比整条丢掉好。
func (h *Hub) fillQuotedSender(ctx context.Context, msg *domain.Message) {
	if msg == nil || len(msg.Additional) == 0 {
		return
	}
	refIdx := strings.TrimSpace(msg.Additional[domain.AdditionalRefMsgIdx])
	if refIdx == "" {
		return
	}
	// 已经知道是谁了就不用查（重投递场景）。
	if strings.TrimSpace(msg.Additional[domain.AdditionalQuotedSender]) != "" {
		return
	}

	byIdx, err := h.store.Messages().ByRefMsgIdx(ctx, []string{refIdx})
	if err != nil {
		h.log.Warn("回查被引消息发送者失败", "err", err, "ref_msg_idx", refIdx)
		return
	}
	target, ok := byIdx[refIdx]
	if !ok || target == nil {
		h.log.Debug("被引消息不在本地，无法确定发送者", "ref_msg_idx", refIdx)
		return
	}
	name := strings.TrimSpace(target.UserName)
	if name == "" {
		name = strings.TrimSpace(target.UserID)
	}
	if name == "" {
		return
	}
	msg.Additional[domain.AdditionalQuotedSender] = name

	// 补完要回写库，否则重启后恢复的上下文里引用又变成「不知道是谁」。
	if err := h.store.Messages().Save(ctx, msg); err != nil {
		h.log.Warn("回写被引发送者失败", "err", err, "msg_id", msg.MessageID)
	}
}

// collectEmoji 把入站消息里的表情包收进库。
//
// 只收 emoji 段，不收普通 image 段：
// 群里的随手拍、截图、表情包混在一起，全收会让库变成垃圾场。
// 参考实现也是靠「是不是表情包」这个判断来决定收不收的，
// 它用的是 QQ 侧的表情包标记；我们这边 codec 已经把该标记映射成 SegmentEmoji。
//
// 一条消息里的**每一个**表情段都要收：参考实现走的是
// message.py:process_emoji_component → emoji_manager.get_emoji_description
// → ensure_emoji_saved，每个 EmojiComponent 都会落库，不存在
// 「一条消息只收第一张」这种限制。
// 防止塞爆库靠的是内容哈希去重与 emoji.max_count，不是靠这里截断。
func (h *Hub) collectEmoji(ctx context.Context, msg *domain.Message, sessionID string) {
	if h.emoji == nil {
		return
	}
	// 官方单条消息最多 10 个附件（codec 里已经限过），这里再兜一层，
	// 防止上游构造的消息把一次入站变成 50 次 VLM 调用。
	const maxCollectPerMessage = 10
	collected := 0
	for _, seg := range msg.Segments {
		if collected >= maxCollectPerMessage {
			return
		}
		if seg.Type != domain.SegmentEmoji {
			continue
		}
		if len(seg.Binary) == 0 {
			// 下载失败或来自数据库回填的占位段，没有二进制可用。
			continue
		}
		collected++
		if _, err := h.emoji.Collect(ctx, seg.Binary, seg.MimeType, sessionID, msg.UserID); err != nil {
			h.log.Warn("收藏表情包失败", "err", err, "user", msg.UserID)
		}
	}
}

// touchPerson 更新用户记录。
func (h *Hub) touchPerson(ctx context.Context, msg *domain.Message) {
	now := time.Now()
	p := &domain.Person{
		Platform:   msg.Platform,
		UserID:     msg.UserID,
		Nickname:   msg.UserName,
		FirstSeen:  now,
		LastSeenAt: now,
	}
	if err := h.store.Persons().Upsert(ctx, p); err != nil {
		h.log.Warn("更新用户失败", "err", err, "user", msg.UserID)
	}
}

// sweep 定期回收闲置会话。
func (h *Hub) sweep() {
	defer h.wg.Done()

	ticker := time.NewTicker(h.sweepEvery)
	defer ticker.Stop()

	for {
		select {
		case <-h.ctx.Done():
			return
		case <-ticker.C:
			h.collectIdle()
		}
	}
}

// collectIdle 回收超过闲置时长的 runtime。
func (h *Hub) collectIdle() {
	cutoff := time.Now().Add(-h.idleTimeout)

	h.mu.Lock()
	var toClose []*agent.Runtime
	for id, rt := range h.runtimes {
		if rt.LastActive().Before(cutoff) {
			toClose = append(toClose, rt)
			delete(h.runtimes, id)
		}
	}
	h.mu.Unlock()

	for _, rt := range toClose {
		rt.Close()
		h.log.Info("回收闲置会话", "session", rt.SessionID())
	}
}

// Close 关闭所有 runtime。
func (h *Hub) Close() {
	// 先取消 Hub 的 context，让 sweep 协程退出、让所有 runtime 收到停止信号。
	h.cancel()

	h.mu.Lock()
	runtimes := make([]*agent.Runtime, 0, len(h.runtimes))
	for _, rt := range h.runtimes {
		runtimes = append(runtimes, rt)
	}
	h.runtimes = make(map[string]*agent.Runtime)
	h.mu.Unlock()

	for _, rt := range runtimes {
		rt.Close()
	}
	h.wg.Wait()
}

// ActiveCount 返回活跃会话数。
func (h *Hub) ActiveCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.runtimes)
}
