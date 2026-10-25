package mock

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/agent"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/chat"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/embedding"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/emoji"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/learning"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/llm"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/logx"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/memory"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/prompt"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/router"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/store"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/tool"
)

// Harness 是一套完整的运行时，用模拟 LLM 驱动。
//
// 目的：不用真的连 QQ、不用真的调模型、不用人手动发 QQ 消息，
// 就能验证「消息进 → 防抖 → 门控 → Planner → 工具 → Replyer → 发出」整条链路。
//
// harness 与生产（app.go）刻意不一致、不覆盖的能力清单（audit 11 第 3 节）：
//   - QQ 适配器与入站解码（由 internal/qq 单测守，harness 直接 Dispatch）
//   - 管理命令处理器（/assign_id）、日志广播（SSE）、WebUI
//   - 保留期清理、用量统计、学习器后台协程、历史向量回填协程
//   - watchBotIdentity 的网关侧回填（harness 构造时一次性 SetBotIDs 代替）
//
// 需要验证这些能力时请写对应包的单测，不要往 harness 里塞。
type Harness struct {
	t   *testing.T
	srv *Server

	// Store 是运行时配置：改配置请走 Harness.Update（校验 + OnSave 钩子
	// + 快照替换，与生产 Store.Update 同一条路径），下一轮请求即可读到新值。
	// 断言当前配置用 Harness.Config()（只读副本）。
	Store *config.Store
	DB    *store.DB
	// Filter 是路由器用的过滤器。生产里由 app.go 的 OnSave 钩子在配置
	// 保存后推它；harness 接了同样的钩子，所以测试不再需要手动推。
	Filter    *chat.Filter
	Hub       *router.Hub
	Send      *RecordingSender
	Prompts   *prompt.Manager
	Embedding *embedding.Client

	cancel context.CancelFunc
}

// HarnessOptions 配置 harness。
//
// 注意 TalkValue / PrivateTalkValue 用指针：
// talk_value = 0 是合法配置（完全不说话），不能用 0 值判断「未设置」。
type HarnessOptions struct {
	// DebounceMode 是防抖档位，默认 fast（测试里用最快档）。
	DebounceMode string
	// TalkValue 是群聊发言频率；nil 表示用配置默认 1.0。
	TalkValue *float64
	// PrivateTalkValue 是私聊发言频率；nil 表示用配置默认 1.0。
	PrivateTalkValue *float64
	// TriggerMode 是触发模式，默认 frequency。
	TriggerMode string
	// Planner 定义 Planner 的模拟回复。
	Planner func(req *RecordedRequest, call int) *Reply
	// Replyer 定义 Replyer 的模拟回复。
	Replyer func(req *RecordedRequest, call int) *Reply
	// DBPath 为空时用临时目录。
	DBPath string
	// PromptsDir 为空时只用内嵌模板（旧行为不变）；
	// 传目录时优先读 <dir>/zh-CN/*.prompt，用于测「磁盘覆盖内嵌」与热更新。
	PromptsDir string
	// EmbeddingDim 是假 /embeddings 服务的向量维度，0 表示 1024。
	EmbeddingDim int
	// SetupConfig 在配置就绪、但组件尚未创建前调用。
	// 用于打开记忆/表情包/关键词等新功能，不影响旧测试。
	SetupConfig func(cfg *config.Config)
	// ExtraTools 是额外注册的工具，在三个内置工具之后注册。
	ExtraTools []tool.Tool
	// Emoji 可以注入一个假的收藏器；nil 表示不启用。
	Emoji router.EmojiCollector
}

// Float 是取浮点指针的便捷函数。
func Float(v float64) *float64 { return &v }

// NewHarness 创建并启动 harness。
func NewHarness(t *testing.T, opts HarnessOptions) *Harness {
	t.Helper()

	if opts.DebounceMode == "" {
		opts.DebounceMode = "fast" // 测试里用最快档，减少等待
	}
	if opts.TriggerMode == "" {
		opts.TriggerMode = "frequency"
	}

	srv := NewServer(Options{
		Planner:      opts.Planner,
		Replyer:      opts.Replyer,
		EmbeddingDim: opts.EmbeddingDim,
	})

	cfg := config.Default()
	cfg.Bot.Nickname = "木木"
	cfg.QQ.AppID = "mock"
	cfg.QQ.AppSecret = "mock"
	cfg.Chat.Timing.DebounceMode = opts.DebounceMode
	cfg.Chat.Timing.ReplyTriggerMode = opts.TriggerMode
	cfg.Chat.Timing.InevitableAtReply = true
	if opts.TalkValue != nil {
		cfg.Chat.Timing.TalkValue = *opts.TalkValue
	}
	if opts.PrivateTalkValue != nil {
		cfg.Chat.Timing.PrivateTalkValue = *opts.PrivateTalkValue
	}
	cfg.LLM.Providers = []config.ProviderConfig{{
		Name:    "mock",
		BaseURL: srv.URL,
		APIKey:  "mock",
		Timeout: 10,
	}}
	cfg.LLM.Models = []config.ModelConfig{{
		Name:            "mock-model",
		ModelIdentifier: "mock-model",
		Provider:        "mock",
	}}
	cfg.LLM.Tasks.Planner.Models = []string{"mock-model"}
	cfg.LLM.Tasks.Replyer.Models = []string{"mock-model"}
	cfg.LLM.Tasks.Planner.HardTimeout = 10
	cfg.LLM.Tasks.Replyer.HardTimeout = 10
	cfg.Web.Enabled = false

	// 允许测试打开记忆 / 表情包 / 关键词等新功能。
	if opts.SetupConfig != nil {
		opts.SetupConfig(cfg)
	}

	dbPath := opts.DBPath
	if dbPath == "" {
		dbPath = filepath.Join(t.TempDir(), "test.db")
	}
	cfg.Database.Path = dbPath

	// 与生产同构：Load 之后必过一遍 Validate（audit 11 P2-27）。
	// Validate 的兜底（VectorMinScore 非法值归一、Emoji.MaxSizeKB 下限等）
	// 在 harness 里同样生效，否则测试可以用一个「线上会被悄悄修正」的
	// 配置跑出「正常」结论。
	if err := cfg.Validate(); err != nil {
		t.Fatalf("harness 配置未通过校验: %v", err)
	}

	// Init 由 sync.Once 守着，同一进程第二个 harness 的 Init 会静默无效
	// （audit 11 P2-30）。Reset 是 logx 提供的测试专用入口。
	logx.Reset()
	if err := logx.Init(logx.Options{Level: logx.LevelWarn, Console: false}); err != nil {
		t.Fatalf("初始化日志失败: %v", err)
	}

	db, err := store.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}

	prompts, err := prompt.New(opts.PromptsDir, "zh-CN")
	if err != nil {
		t.Fatalf("加载提示词失败: %v", err)
	}

	// 与 app.go 一致：把同一个 Manager 交给各业务包。
	//
	// 这里漏接的话会有个很难发现的后果：harness 里测的是「内嵌默认文案」，
	// 而线上跑的是用户改过的版本 —— 测试全绿、真机不一样。
	// （同理 app.go 里的注入漏一个，对应功能会静默地用回默认文案。）
	memory.SetPromptManager(prompts)
	learning.SetPromptManager(prompts)
	emoji.SetPromptManager(prompts)

	cfgStore := config.Wrap(cfg)
	client := llm.New(cfgStore)

	// 与 app.go 一致：向量客户端总是构造（未配 embedding 任务时
	// Enabled() 为 false，记忆检索自动降级关键词）。
	// 以前 harness 根本不传它 —— 向量链路在所有 harness 测试里
	// 都不存在，「配置了向量模型」的场景永远测不到（audit 11 P1-7）。
	embedClient := embedding.New(cfgStore)

	registry := tool.NewRegistry()
	registry.MustRegister(tool.NewReplyToolFunc(func() bool { return cfgStore.Get().Chat.EnableReplyAt }))
	registry.MustRegister(tool.NewWaitTool())
	registry.MustRegister(tool.NewStopTool())

	// 按配置注册可选工具，与 app.go 的逻辑保持一致。
	//
	// 这里是刻意重复生产代码的判断，而不是让测试自己传 ExtraTools：
	// harness 一旦与真实启动路径不一致，测出来的结论就不可信
	// ——「开了记忆开关但工具没注册」这类缺陷会在 harness 里被掩盖。
	if cfg.Memory.Enabled {
		registry.MustRegister(tool.NewQueryMemoryTool())
	}
	if cfg.Emoji.Enabled {
		registry.MustRegister(tool.NewSendEmojiTool())
		registry.MustRegister(tool.NewShowEmojiListTool())
	}
	for _, extra := range opts.ExtraTools {
		registry.MustRegister(extra)
	}

	// 与 app.go 一致：过滤器吃配置里的初始屏蔽词/正则/白名单，
	// 而不是空集合 —— 否则「初始配置就带屏蔽词」的场景测不到。
	filter, err := chat.NewFilter(cfg.Message.BanWords, cfg.Message.BanRegexes, cfg.Message.GroupWhitelist)
	if err != nil {
		t.Fatalf("创建过滤器失败: %v", err)
	}

	// 与 app.go 一致：机器人自身 ID 进过滤名单。
	// bot-openid 是 SendMessageIn 用的账号 ID，必须含它，否则
	// 「机器人自己的消息」这条过滤路径在 harness 里永远走不到（audit 11 P2-22）。
	filter.SetBotIDs([]string{"bot-openid", cfg.Bot.AccountID, cfg.QQ.UnifiedAccountID})

	// 与 app.go 的 OnSave 接线一致：配置保存后把过滤器推一把。
	// 不接的话这段生产接线零测试覆盖，而且测试都得手动推 filter
	// ——「WebUI 改屏蔽词立刻生效」这条线只能在 app 包里测（audit 11 P2-21）。
	cfgStore.OnSave(func(_, next *config.Config) {
		filter.Update(next.Message.BanWords, next.Message.BanRegexes, next.Message.GroupWhitelist)
		filter.SetBotIDs([]string{"bot-openid", next.Bot.AccountID, next.QQ.UnifiedAccountID})
	})

	sender := &RecordingSender{}

	ctx, cancel := context.WithCancel(t.Context())
	hub := router.NewHub(ctx, router.HubOptions{
		Config:    cfgStore,
		Store:     db,
		LLM:       client,
		Prompts:   prompts,
		Registry:  registry,
		Sender:    sender,
		Filter:    filter,
		Emoji:     opts.Emoji,
		Embedding: embedClient,
	})

	h := &Harness{
		t:         t,
		srv:       srv,
		Store:     cfgStore,
		DB:        db,
		Filter:    filter,
		Hub:       hub,
		Send:      sender,
		Prompts:   prompts,
		Embedding: embedClient,
		cancel:    cancel,
	}

	t.Cleanup(func() {
		hub.Close()
		srv.Close()
		db.Close()
	})

	return h
}

// Server 返回模拟 LLM 服务。
func (h *Harness) Server() *Server { return h.srv }

// Config 返回当前配置快照的副本，仅供测试断言。
//
// 返回副本而不是 Store.Get() 的指针：快照指针按契约是只读的，
// 直接暴露出去等于邀请测试就地改字段 —— 那样既不触发 OnSave 钩子、
// 也不走 Validate，和「绕过 Store.Update」是同一个坑（audit 11 P2-28）。
func (h *Harness) Config() *config.Config {
	h.t.Helper()
	clone, err := h.Store.Get().Clone()
	if err != nil {
		h.t.Fatalf("克隆配置快照失败: %v", err)
	}
	return clone
}

// Update 走生产同一条路径改配置：Clone → 改 → Validate → 落盘（临时目录）
// → 换快照 → OnSave 钩子（过滤器等派生状态跟着刷新）。
func (h *Harness) Update(fn func(*config.Config)) {
	h.t.Helper()
	if err := h.Store.Update(func(c *config.Config) error {
		fn(c)
		return nil
	}); err != nil {
		h.t.Fatalf("更新配置失败: %v", err)
	}
}

// Requests 返回所有 LLM 请求。
func (h *Harness) Requests() []*RecordedRequest { return h.srv.Requests() }

// PlannerRequests 只返回 Planner 请求（带工具的）。
func (h *Harness) PlannerRequests() []*RecordedRequest {
	var out []*RecordedRequest
	for _, r := range h.Requests() {
		if r.HasTools {
			out = append(out, r)
		}
	}
	return out
}

// ReplyerRequests 只返回 Replyer 请求（不带工具的）。
func (h *Harness) ReplyerRequests() []*RecordedRequest {
	var out []*RecordedRequest
	for _, r := range h.Requests() {
		if !r.HasTools {
			out = append(out, r)
		}
	}
	return out
}

// SendMessage 投递一条消息，模拟用户说话。
func (h *Harness) SendMessage(text string, isAt bool) {
	h.SendMessageIn("group-1", "user-1", "小明", text, isAt)
}

// SendPrivateMessage 投递一条私聊消息。
func (h *Harness) SendPrivateMessage(text string) {
	h.SendMessageIn("", "user-openid-1", "小明", text, false)
}

// SendWebUIMessage 投递一条来自网页对话窗口的消息。
//
// 字段构造与生产入口（web.handleChatSend）完全一致：
// platform=webui 的私聊、IsAt 恒为 true（对齐 MaiBot 的 is_at_bot=True，
// 网页消息总是视为 @ 了机器人）。刻意重复这段构造而不是去调 web 包，
// 是为了让端到端测试不依赖 HTTP 层 —— harness 直接验证的是 Hub 链路。
func (h *Harness) SendWebUIMessage(userName, text string) string {
	seq := time.Now().UnixNano()
	msgID := fmt.Sprintf("webui-%d", seq)
	msg := &domain.Message{
		ID:        msgID,
		Platform:  domain.PlatformWebUI,
		MessageID: msgID,
		UserID:    domain.WebUITargetID,
		UserName:  userName,
		IsGroup:   false,
		IsAt:      true,
		Segments:  []domain.Segment{domain.Text(text)},
		PlainText: text,
		ReceivedAt: time.Now(),
		AccountID: domain.WebUIAccountID,
	}
	h.Hub.Dispatch(msg, domain.ScopePrivate, domain.WebUITargetID, domain.WebUIAccountID)
	return domain.WebUISessionKey().ID()
}

// SendMessageIn 投递一条指定来源的消息。
func (h *Harness) SendMessageIn(groupID, userID, userName, text string, isAt bool) {
	isGroup := groupID != ""

	segments := []domain.Segment{domain.Text(text)}
	if isAt {
		segments = append([]domain.Segment{domain.At("bot-openid", "木木")}, segments...)
	}

	seq := time.Now().UnixNano()
	msg := &domain.Message{
		ID:        fmt.Sprintf("local-%d", seq),
		Platform:  "qq",
		MessageID: fmt.Sprintf("msg-%d", seq),
		// 生产 codec 一定会填 RefMsgIdx（来自 message_scene.ext 的 msg_idx），
		// 出站引用要靠它做目标。harness 不填的话「引用判定」这条链路
		// 在测试里永远是断的 —— 正是 AGENTS 第 34 条那个坑。
		RefMsgIdx:  fmt.Sprintf("REFIDX_msg_%d", seq),
		UserID:     userID,
		UserName:   userName,
		GroupID:    groupID,
		GroupName:  "测试群",
		IsGroup:    isGroup,
		IsAt:       isAt,
		Segments:   segments,
		PlainText:  domain.BuildPlainText(segments),
		ReceivedAt: time.Now(),
	}

	scene := domain.ScopePrivate
	target := userID
	if isGroup {
		scene = domain.ScopeGroup
		target = groupID
	}

	h.Hub.Dispatch(msg, scene, target, "bot-openid")
}

// SendQuotedMessage 投递一条引用消息。
//
// 构造方式完全对齐生产解码结果（qq/codec.go 的 extractQuoted）：
//   - 段列表首位是 reply 段，MessageID 是被引目标的引用索引
//   - Additional 带 ref_msg_idx 与 quoted_text
//   - RefMsgIdx 是自己这条消息的索引
//
// 手写这几项而不是走 qq.Decode，是因为 harness 要能在不依赖
// QQ 报文结构的情况下复现「引用链路」这个场景。
func (h *Harness) SendQuotedMessage(text, quotedTarget, quotedText string) {
	segments := []domain.Segment{domain.Reply(quotedTarget)}
	if text != "" {
		segments = append(segments, domain.Text(text))
	}

	msg := &domain.Message{
		ID:         fmt.Sprintf("local-%d", time.Now().UnixNano()),
		Platform:   "qq",
		MessageID:  fmt.Sprintf("msg-%d", time.Now().UnixNano()),
		UserID:     "user-1",
		UserName:   "小明",
		GroupID:    "group-1",
		GroupName:  "测试群",
		IsGroup:    true,
		Segments:   segments,
		PlainText:  domain.BuildPlainText(segments),
		ReceivedAt: time.Now(),
		RefMsgIdx:  fmt.Sprintf("REFIDX_self_%d", time.Now().UnixNano()),
		Additional: map[string]string{
			domain.AdditionalRefMsgIdx:    quotedTarget,
			domain.AdditionalQuotedText:   quotedText,
			domain.AdditionalQuotedSender: "小红",
		},
	}

	h.Hub.Dispatch(msg, domain.ScopeGroup, "group-1", "bot-openid")
}

// SendMessageWithImage 投递一条带图片的消息。
//
// 用途：验证 attach_pic 能不能从上下文消息里取出图片二进制再发出去，
// 以及多模态链路能不能把图挂进 Planner 请求。
func (h *Harness) SendMessageWithImage(groupID, userID, userName, text string) *domain.Message {
	return h.SendMessageWithSegments(groupID, userID, userName, text, domain.Segment{
		Type:   domain.SegmentImage,
		Binary: fakePNG(),
		URL:    "ctx-image",
	})
}

// SendMessageWithSegments 投递一条带自定义段的消息（audit 11 P2-26）。
//
// 测试自己拼段（WEBP 假头、GIF 动图、任意 MIME），
// 用于覆盖 imgtx 的转码与拒绝路径 —— SendMessageWithImage 只覆盖
// 「正常 PNG 直通」这一种情况。
func (h *Harness) SendMessageWithSegments(groupID, userID, userName, text string, extra ...domain.Segment) *domain.Message {
	isGroup := groupID != ""

	segments := []domain.Segment{domain.Text(text)}
	if strings.TrimSpace(text) == "" {
		segments = nil
	}
	segments = append(segments, extra...)

	msg := &domain.Message{
		ID:         fmt.Sprintf("local-img-%d", time.Now().UnixNano()),
		Platform:   "qq",
		MessageID:  fmt.Sprintf("msg-img-%d", time.Now().UnixNano()),
		UserID:     userID,
		UserName:   userName,
		GroupID:    groupID,
		GroupName:  "测试群",
		IsGroup:    isGroup,
		Segments:   segments,
		PlainText:  domain.BuildPlainText(segments),
		ReceivedAt: time.Now(),
	}

	scene := domain.ScopePrivate
	target := userID
	if isGroup {
		scene = domain.ScopeGroup
		target = groupID
	}
	h.Hub.Dispatch(msg, scene, target, "bot-openid")
	return msg
}

// fakePNG 返回一张真实可解码的 1x1 PNG。
//
// 必须是真图（audit 11 P2-26）：入站图片会先过 imgtx.Normalize，
// 它现在会校验图片头 —— 只带魔数的残缺假头会被拒绝。
// 以前「假图被当合法图发出去」正好踩中 imgtx 的 P2-11 缺陷，
// 多模态链路测试全靠假头直通才通过。
func fakePNG() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 255, G: 80, B: 80, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		// 对内存里的 1x1 图编码不可能失败；万一失败，返回空数据
		// 会让依赖它的测试立刻红，不会被静默掩盖。
		return nil
	}
	return buf.Bytes()
}

// WaitForSend 等待至少一条消息发出，返回**最新**一条的文本，超时则失败。
//
// 返回最新而不是最早（audit 11 P2-25）：多轮测试里最早一条通常是
// 上一轮的旧内容，对着它断言会得到「假绿」。
// 需要全部出站文本时用 WaitForSendCount。
func (h *Harness) WaitForSend(timeout time.Duration) string {
	h.t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if texts := h.Send.Texts(); len(texts) > 0 {
			return texts[len(texts)-1]
		}
		time.Sleep(20 * time.Millisecond)
	}

	h.t.Fatalf("等待发送超时（%v）。已发出 %d 条，LLM 请求 %d 次",
		timeout, len(h.Send.Texts()), h.srv.Count())
	return ""
}

// WaitForSendCount 等待累计发出 n 条，返回全部出站文本（按发送顺序）。
func (h *Harness) WaitForSendCount(n int, timeout time.Duration) []string {
	h.t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if texts := h.Send.Texts(); len(texts) >= n {
			return texts
		}
		time.Sleep(20 * time.Millisecond)
	}

	h.t.Fatalf("等待 %d 条发送超时（%v）。已发出 %d 条，LLM 请求 %d 次",
		n, timeout, len(h.Send.Texts()), h.srv.Count())
	return nil
}

// WaitForLLMCalls 等待达到指定请求数。
func (h *Harness) WaitForLLMCalls(n int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if h.srv.Count() >= n {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// WaitForNoSend 确认一段时间内没有发送（用于验证「决定不说话」）。
func (h *Harness) WaitForNoSend(d time.Duration) {
	time.Sleep(d)
	if texts := h.Send.Texts(); len(texts) > 0 {
		h.t.Fatalf("不应发送，但发出了: %v", texts)
	}
}

// RecordingSender 记录所有出站消息。
type RecordingSender struct {
	mu    sync.Mutex
	sends []SendRecord
}

// SendRecord 是一条出站记录。
type SendRecord struct {
	At     time.Time
	Scene  string
	Target string
	Text   string
	// PassiveTo 是这条消息占的被动回复目标（触发本轮的那条入站消息）。
	PassiveTo string
	// QuoteTo 是这条消息挂的引用目标（REFIDX_ 形态）。
	//
	// 与 PassiveTo 分开记：两者指向不同的消息，
	// 之前合成一个字段时「引用到底有没有发出去」根本无法断言。
	QuoteTo string
	// AtUserIDs 是这条消息 @ 的 OpenID，按出现顺序。
	AtUserIDs []string
	// ImageCount 是这条消息携带的图片张数。
	ImageCount int
}

// SendText 实现 agent.Sender。
func (r *RecordingSender) SendText(ctx context.Context, scene, target, text, passiveTo string, quote domain.Reference) (domain.SendResult, error) {
	return r.SendSegments(ctx, scene, target, []domain.Segment{domain.Text(text)}, passiveTo, quote)
}

// SendSegments 实现 agent.Sender。
//
// 把段拆成可断言的字段存下来：@ 与图片是否真的发出去了，
// 只有把这个记录下来才能测。
func (r *RecordingSender) SendSegments(_ context.Context, scene, target string, segments []domain.Segment, passiveTo string, quote domain.Reference) (domain.SendResult, error) {
	var (
		buf    strings.Builder
		atIDs  []string
		images int
	)
	for _, seg := range segments {
		switch seg.Type {
		case domain.SegmentText:
			buf.WriteString(seg.Text)
		case domain.SegmentAt:
			atIDs = append(atIDs, seg.UserID)
			buf.WriteString("@" + seg.UserName + " ")
		case domain.SegmentImage, domain.SegmentEmoji:
			images++
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.sends = append(r.sends, SendRecord{
		At:         time.Now(),
		Scene:      scene,
		Target:     target,
		Text:       buf.String(),
		PassiveTo:  passiveTo,
		QuoteTo:    quote.RefIdx,
		AtUserIDs:  atIDs,
		ImageCount: images,
	})
	idx := len(r.sends)
	// 与真实发送一致：每条发出的消息自己也有一个引用索引，
	// 供下一轮引用自己的发言。少了它，「引用自己说过的话」
	// 这条链路在测试里就永远是断的。
	msgID := fmt.Sprintf("sent-%d", idx)
	refIdx := fmt.Sprintf("REFIDX_sent_%d", idx)
	return domain.SendResult{
		MessageIDs: []string{msgID},
		RefIdxs:    []string{refIdx},
	}, nil
}

// Sends 返回所有出站记录。
func (r *RecordingSender) Sends() []SendRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]SendRecord(nil), r.sends...)
}

// Texts 返回所有出站文本。
func (r *RecordingSender) Texts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.sends))
	for _, s := range r.sends {
		out = append(out, s.Text)
	}
	return out
}

// Reset 清空记录。
func (r *RecordingSender) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sends = nil
}

var _ agent.Sender = (*RecordingSender)(nil)

// DispatchMessage 直接把一条构造好的消息投给 Hub。
//
// 与 SendMessageIn 的区别：后者只管文本，这里允许测试自己拼 Segments
// （表情包、多模态附件等），用于验证入站链路上的旁路逻辑
// —— 例如表情包收藏，它发生在投递之前、和 Planner 无关。
func (h *Harness) DispatchMessage(msg *domain.Message, scene, target, accountID string) {
	if msg.ID == "" {
		msg.ID = fmt.Sprintf("local-%d", time.Now().UnixNano())
	}
	if msg.Platform == "" {
		msg.Platform = "qq"
	}
	if msg.ReceivedAt.IsZero() {
		msg.ReceivedAt = time.Now()
	}
	h.Hub.Dispatch(msg, scene, target, accountID)
}
