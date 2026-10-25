package router

// 本文件覆盖 Hub 的投递、落库、runtime 复用、闲置回收与关闭。
//
// 刻意避开真实模型链路：只用 talk_value=0 的「静默消费」配置，
// 消息会进历史但不进入 Planner，因此 runtime 不会调模型。
// 这样测试稳定、不依赖 httptest 假服务的响应细节。
// 只新建测试文件，不修改生产代码。

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/agent"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/chat"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/llm"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/logx"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/prompt"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/store"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/tool"
)

// fakeSender 是一个不会真的发消息的 agent.Sender。
type fakeSender struct {
	sends int
}

func (f *fakeSender) SendText(context.Context, string, string, string, string, domain.Reference) (domain.SendResult, error) {
	f.sends++
	return domain.SendResult{MessageIDs: []string{"fake-id"}, RefIdxs: []string{"REFIDX_fake"}}, nil
}

func (f *fakeSender) SendSegments(context.Context, string, string, []domain.Segment, string, domain.Reference) (domain.SendResult, error) {
	f.sends++
	return domain.SendResult{MessageIDs: []string{"fake-id"}, RefIdxs: []string{"REFIDX_fake"}}, nil
}

// testHub 在临时数据库上创建一个不调模型的 Hub。
type testHub struct {
	hub    *Hub
	db     *store.DB
	cfg    *config.Config
	sender *fakeSender
}

func newTestHub(t *testing.T) *testHub {
	t.Helper()
	return newTestHubWith(t, nil)
}

// newTestHubWith 允许按需修改 HubOptions（例如挂上表情包收藏器）。
func newTestHubWith(t *testing.T, mutate func(*HubOptions)) *testHub {
	t.Helper()

	// logx.Init 内部是 sync.Once，重复调用不会重新初始化；
	// 只要 Console=false 且 Dir 为空就退化为 io.Discard，不会污染测试输出。
	if err := logx.Init(logx.Options{Level: logx.LevelError, Console: false}); err != nil {
		t.Fatalf("初始化日志失败: %v", err)
	}

	cfg := config.Default()
	cfg.Bot.Nickname = "木木"
	cfg.QQ.AppID = "test"
	cfg.QQ.AppSecret = "test"
	cfg.Web.Enabled = false
	// 静默消费：消息进上下文但不进入 Planner，因此绝不会调模型。
	cfg.Chat.Timing.TalkValue = 0
	cfg.Chat.Timing.PrivateTalkValue = 0

	dir := t.TempDir()
	db, err := store.Open(t.Context(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("关闭数据库失败: %v", err)
		}
	})

	prompts, err := prompt.New("", "zh-CN")
	if err != nil {
		t.Fatalf("加载提示词失败: %v", err)
	}

	registry := tool.NewRegistry()
	registry.MustRegister(tool.NewReplyTool(cfg.Chat.EnableReplyAt))
	registry.MustRegister(tool.NewWaitTool())
	registry.MustRegister(tool.NewStopTool())

	filter, err := chat.NewFilter(nil, nil, nil)
	if err != nil {
		t.Fatalf("创建过滤器失败: %v", err)
	}

	sender := &fakeSender{}
	// 父 context 用 Background：Hub 必须自己派生 ctx 并在 Close 时取消，
	// 否则 Close 会永远等下去（AGENTS.md 第 9 条）。
	opts := HubOptions{
		Config:   config.Wrap(cfg),
		Store:    db,
		LLM:      llm.New(config.Wrap(cfg)),
		Prompts:  prompts,
		Registry: registry,
		Sender:   sender,
		Filter:   filter,
	}
	if mutate != nil {
		mutate(&opts)
	}
	hub := NewHub(context.Background(), opts)
	t.Cleanup(hub.Close)

	return &testHub{hub: hub, db: db, cfg: cfg, sender: sender}
}

// groupMessage 造一条群聊消息。
func groupMessage(id, groupID, userID, name, text string) *domain.Message {
	return &domain.Message{
		ID:         id,
		Platform:   "qq",
		MessageID:  "mid-" + id,
		UserID:     userID,
		UserName:   name,
		GroupID:    groupID,
		GroupName:  "测试群",
		Segments:   []domain.Segment{domain.Text(text)},
		PlainText:  text,
		IsGroup:    true,
		ReceivedAt: time.Now(),
	}
}

// TestHubDispatchPersistsMessageAndSession 是核心链路：
// 投递后消息必须落库、会话与用户必须被 Upsert、runtime 必须被创建。
func TestHubDispatchPersistsMessageAndSession(t *testing.T) {
	h := newTestHub(t)
	msg := groupMessage("m1", "g1", "u1", "小明", "你好")
	key := domain.SessionKey{Platform: "qq", AccountID: "bot-1", Scope: domain.ScopeGroup, TargetID: "g1"}
	wantSessionID := key.ID()

	if got := h.hub.ActiveCount(); got != 0 {
		t.Fatalf("投递前 ActiveCount = %d，期望 0", got)
	}

	h.hub.Dispatch(msg, domain.ScopeGroup, "g1", "bot-1")

	if got := h.hub.ActiveCount(); got != 1 {
		t.Errorf("投递后 ActiveCount = %d，期望 1", got)
	}
	if msg.SessionID != wantSessionID {
		t.Errorf("消息 SessionID = %q，期望 %q", msg.SessionID, wantSessionID)
	}
	if msg.AccountID != "bot-1" {
		t.Errorf("消息 AccountID = %q，期望 %q", msg.AccountID, "bot-1")
	}

	ctx := t.Context()

	// 消息落库。
	got, err := h.db.Messages().ByMessageID(ctx, msg.MessageID)
	if err != nil {
		t.Fatalf("按 message_id 查消息失败: %v", err)
	}
	if got == nil {
		t.Fatal("消息未落库")
	}
	if got.SessionID != wantSessionID {
		t.Errorf("落库的 session_id = %q，期望 %q", got.SessionID, wantSessionID)
	}
	if got.PlainText != "你好" {
		t.Errorf("落库的 plain_text = %q，期望 %q", got.PlainText, "你好")
	}

	// 会话被 Upsert。
	sess, err := h.db.Sessions().Get(ctx, wantSessionID)
	if err != nil {
		t.Fatalf("查会话失败: %v", err)
	}
	if sess == nil {
		t.Fatal("会话未创建")
	}
	if sess.Scope != domain.ScopeGroup || sess.TargetID != "g1" {
		t.Errorf("会话 = %+v，期望群聊 g1", sess)
	}
	if sess.DisplayName != "测试群" {
		t.Errorf("群聊会话显示名 = %q，期望群名 %q", sess.DisplayName, "测试群")
	}
	if sess.MessageCount != 1 {
		t.Errorf("会话消息数 = %d，期望 1", sess.MessageCount)
	}

	// 用户被 Upsert。
	person, err := h.db.Persons().Get(ctx, "qq", "u1")
	if err != nil {
		t.Fatalf("查用户失败: %v", err)
	}
	if person == nil {
		t.Fatal("用户未创建")
	}
	if person.Nickname != "小明" {
		t.Errorf("用户昵称 = %q，期望 %q", person.Nickname, "小明")
	}
}

// TestHubDispatchPrivateSessionDisplayName 校验私聊会话显示名带「的私聊」。
func TestHubDispatchPrivateSessionDisplayName(t *testing.T) {
	h := newTestHub(t)
	msg := &domain.Message{
		ID:         "p1",
		Platform:   "qq",
		MessageID:  "mid-p1",
		UserID:     "u2",
		UserName:   "小红",
		Segments:   []domain.Segment{domain.Text("私聊你")},
		PlainText:  "私聊你",
		ReceivedAt: time.Now(),
	}

	h.hub.Dispatch(msg, domain.ScopePrivate, "u2", "bot-1")

	key := domain.SessionKey{Platform: "qq", AccountID: "bot-1", Scope: domain.ScopePrivate, TargetID: "u2"}
	sess, err := h.db.Sessions().Get(t.Context(), key.ID())
	if err != nil {
		t.Fatalf("查会话失败: %v", err)
	}
	if sess == nil {
		t.Fatal("私聊会话未创建")
	}
	// 私聊直接用对端昵称。
	// 以前拼「小红的私聊」，在会话列表里宽度翻倍而信息量没多，
	// 而且机器人对端的昵称本身已经说明了这是私聊（scope 字段是 private）。
	if sess.DisplayName != "小红" {
		t.Errorf("私聊会话显示名 = %q，期望 %q", sess.DisplayName, "小红")
	}
}

// TestHubReusesRuntimeForSameSession 防的是「每条消息都新建 runtime」：
// 那会丢掉上下文、泄漏 goroutine，还会重复恢复历史。
func TestHubReusesRuntimeForSameSession(t *testing.T) {
	h := newTestHub(t)

	h.hub.Dispatch(groupMessage("m1", "g1", "u1", "小明", "第一条"), domain.ScopeGroup, "g1", "bot-1")

	h.hub.mu.RLock()
	first := h.hub.runtimes[sessionIDOf("g1")]
	h.hub.mu.RUnlock()
	if first == nil {
		t.Fatal("首次投递未创建 runtime")
	}

	h.hub.Dispatch(groupMessage("m2", "g1", "u1", "小明", "第二条"), domain.ScopeGroup, "g1", "bot-1")

	h.hub.mu.RLock()
	second := h.hub.runtimes[sessionIDOf("g1")]
	h.hub.mu.RUnlock()
	if second != first {
		t.Error("同一会话的第二次投递应复用同一个 runtime")
	}
	if got := h.hub.ActiveCount(); got != 1 {
		t.Errorf("ActiveCount = %d，期望仍是 1", got)
	}

	// 不同会话（不同群）应各自一个。
	h.hub.Dispatch(groupMessage("m3", "g2", "u2", "小红", "另一个群"), domain.ScopeGroup, "g2", "bot-1")
	if got := h.hub.ActiveCount(); got != 2 {
		t.Errorf("不同会话后 ActiveCount = %d，期望 2", got)
	}

	// 私聊与群聊即使 target 同名也必须是两条会话。
	h.hub.Dispatch(&domain.Message{
		ID: "p1", Platform: "qq", MessageID: "mid-p1",
		UserID: "g1", UserName: "同名用户",
		Segments: []domain.Segment{domain.Text("私聊")}, PlainText: "私聊",
		ReceivedAt: time.Now(),
	}, domain.ScopePrivate, "g1", "bot-1")
	if got := h.hub.ActiveCount(); got != 3 {
		t.Errorf("群聊与私聊同名目标应各自一条会话，ActiveCount = %d，期望 3", got)
	}
}

// sessionIDOf 用默认平台/账号算群聊 session_id，与 Dispatch 内部一致。
func sessionIDOf(target string) string {
	return domain.SessionKey{
		Platform:  "qq",
		AccountID: "bot-1",
		Scope:     domain.ScopeGroup,
		TargetID:  target,
	}.ID()
}

// TestHubCloseStopsRuntimesAndSweep 防的是 Hub.Close 死锁与 sweep 协程泄漏。
func TestHubCloseStopsRuntimesAndSweep(t *testing.T) {
	h := newTestHub(t)

	h.hub.Dispatch(groupMessage("m1", "g1", "u1", "小明", "你好"), domain.ScopeGroup, "g1", "bot-1")
	if got := h.hub.ActiveCount(); got != 1 {
		t.Fatalf("ActiveCount = %d，期望 1", got)
	}

	done := make(chan struct{})
	go func() {
		h.hub.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Hub.Close 超时（可能是死锁）")
	}

	if got := h.hub.ActiveCount(); got != 0 {
		t.Errorf("Close 后 ActiveCount = %d，期望 0", got)
	}
	select {
	case <-h.hub.ctx.Done():
	default:
		t.Error("Close 后 Hub 的 ctx 应已取消")
	}
}

// TestHubCloseIsIdempotent 校验重复 Close 不会 panic。
func TestHubCloseIsIdempotent(t *testing.T) {
	h := newTestHub(t)
	h.hub.Dispatch(groupMessage("m1", "g1", "u1", "小明", "你好"), domain.ScopeGroup, "g1", "bot-1")

	h.hub.Close()
	h.hub.Close()
}

// TestHubDispatchAfterCloseDoesNotPanic 校验关闭后投递不会崩，
// 且不会新建 runtime（此时 context 已取消）。
func TestHubDispatchAfterCloseDoesNotPanic(t *testing.T) {
	h := newTestHub(t)
	h.hub.Close()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("关闭后投递不应 panic: %v", r)
		}
	}()

	h.hub.Dispatch(groupMessage("m1", "g1", "u1", "小明", "你好"), domain.ScopeGroup, "g1", "bot-1")

	// 已取消的 Hub 仍可能创建 runtime 结构体，但其 ctx 已结束，
	// 这里只断言不会崩、不会无限增长。行为细节留给实现，不锁死。
	if got := h.hub.ActiveCount(); got > 1 {
		t.Errorf("关闭后 ActiveCount = %d，期望不超过 1", got)
	}
}

// TestHubCollectIdleClosesRuntime 覆盖闲置回收。
func TestHubCollectIdleClosesRuntime(t *testing.T) {
	h := newTestHub(t)
	h.hub.Dispatch(groupMessage("m1", "g1", "u1", "小明", "你好"), domain.ScopeGroup, "g1", "bot-1")
	if got := h.hub.ActiveCount(); got != 1 {
		t.Fatalf("ActiveCount = %d，期望 1", got)
	}

	// 正常情况下闲置阈值是 30 分钟，刚创建的会话不该被回收。
	h.hub.collectIdle()
	if got := h.hub.ActiveCount(); got != 1 {
		t.Fatalf("刚创建的会话被误回收，ActiveCount = %d", got)
	}

	// 把闲置阈值调成负数，模拟所有会话都已超时。
	h.hub.idleTimeout = -time.Second
	h.hub.collectIdle()
	if got := h.hub.ActiveCount(); got != 0 {
		t.Errorf("超时会话未回收，ActiveCount = %d，期望 0", got)
	}
}

// TestHubDefinitionsAndRuntimeState 校验 runtime 拿到的会话标识正确。
func TestHubDefinitionsAndRuntimeState(t *testing.T) {
	h := newTestHub(t)
	h.hub.Dispatch(groupMessage("m1", "g1", "u1", "小明", "你好"), domain.ScopeGroup, "g1", "bot-1")

	h.hub.mu.RLock()
	rt := h.hub.runtimes[sessionIDOf("g1")]
	h.hub.mu.RUnlock()
	if rt == nil {
		t.Fatal("runtime 未创建")
	}
	if got, want := rt.SessionID(), sessionIDOf("g1"); got != want {
		t.Errorf("runtime SessionID = %q，期望 %q", got, want)
	}
	if rt.LastActive().IsZero() {
		t.Error("LastActive 不应为零值")
	}
}

// fakeEmojiCollector 记录被收藏的表情包。
type fakeEmojiCollector struct {
	calls int
	last  struct {
		binary    []byte
		mimeType  string
		sessionID string
		userID    string
	}
}

func (f *fakeEmojiCollector) Collect(_ context.Context, binary []byte, mimeType, sessionID, userID string) (bool, error) {
	f.calls++
	f.last.binary = binary
	f.last.mimeType = mimeType
	f.last.sessionID = sessionID
	f.last.userID = userID
	return true, nil
}

// TestHubCollectEmoji 覆盖入站表情包收藏：
// 只收带二进制的 emoji 段，一条消息最多收一张。
func TestHubCollectEmoji(t *testing.T) {
	t.Run("收藏带二进制的表情包", func(t *testing.T) {
		collector := &fakeEmojiCollector{}
		h := newTestHubWith(t, func(o *HubOptions) { o.Emoji = collector })

		msg := groupMessage("m1", "g1", "u1", "小明", "[表情包]")
		msg.Segments = []domain.Segment{
			{Type: domain.SegmentEmoji, MimeType: "image/gif", Binary: []byte("GIF89a")},
		}
		h.hub.Dispatch(msg, domain.ScopeGroup, "g1", "bot-1")

		if collector.calls != 1 {
			t.Fatalf("收藏次数 = %d，期望 1", collector.calls)
		}
		if string(collector.last.binary) != "GIF89a" {
			t.Errorf("收藏的二进制 = %q，期望 %q", collector.last.binary, "GIF89a")
		}
		if collector.last.sessionID != sessionIDOf("g1") {
			t.Errorf("sessionID = %q，期望 %q", collector.last.sessionID, sessionIDOf("g1"))
		}
		if collector.last.userID != "u1" {
			t.Errorf("userID = %q，期望 %q", collector.last.userID, "u1")
		}
	})

	t.Run("普通图片不收藏", func(t *testing.T) {
		collector := &fakeEmojiCollector{}
		h := newTestHubWith(t, func(o *HubOptions) { o.Emoji = collector })

		msg := groupMessage("m1", "g1", "u1", "小明", "[图片]")
		msg.Segments = []domain.Segment{
			{Type: domain.SegmentImage, MimeType: "image/png", Binary: []byte("PNG")},
		}
		h.hub.Dispatch(msg, domain.ScopeGroup, "g1", "bot-1")

		if collector.calls != 0 {
			t.Errorf("普通图片不应被收藏，实际收藏 %d 次", collector.calls)
		}
	})

	t.Run("没有二进制不收藏", func(t *testing.T) {
		collector := &fakeEmojiCollector{}
		h := newTestHubWith(t, func(o *HubOptions) { o.Emoji = collector })

		msg := groupMessage("m1", "g1", "u1", "小明", "[表情包]")
		msg.Segments = []domain.Segment{{Type: domain.SegmentEmoji, URL: "https://x/1.gif"}}
		h.hub.Dispatch(msg, domain.ScopeGroup, "g1", "bot-1")

		if collector.calls != 0 {
			t.Errorf("没有二进制的表情包不应被收藏，实际 %d 次", collector.calls)
		}
	})

	// 参考实现（message.py:process_emoji_component → ensure_emoji_saved）每个
	// EmojiComponent 都会落库，不存在「一条消息只收第一张」的限制；
	// 防塞爆靠的是内容哈希去重与 emoji.max_count。
	t.Run("同一条消息里的多张表情都收藏", func(t *testing.T) {
		collector := &fakeEmojiCollector{}
		h := newTestHubWith(t, func(o *HubOptions) { o.Emoji = collector })

		msg := groupMessage("m1", "g1", "u1", "小明", "[表情包]")
		msg.Segments = []domain.Segment{
			{Type: domain.SegmentEmoji, Binary: []byte("a")},
			{Type: domain.SegmentEmoji, Binary: []byte("b")},
		}
		h.hub.Dispatch(msg, domain.ScopeGroup, "g1", "bot-1")

		if collector.calls != 2 {
			t.Errorf("收藏次数 = %d，期望 2（参考实现逐附件落库）", collector.calls)
		}
	})

	t.Run("一条消息最多收 10 张", func(t *testing.T) {
		collector := &fakeEmojiCollector{}
		h := newTestHubWith(t, func(o *HubOptions) { o.Emoji = collector })

		msg := groupMessage("m1", "g1", "u1", "小明", "[表情包]")
		for i := 0; i < 20; i++ {
			msg.Segments = append(msg.Segments, domain.Segment{Type: domain.SegmentEmoji, Binary: []byte{byte(i)}})
		}
		h.hub.Dispatch(msg, domain.ScopeGroup, "g1", "bot-1")

		if collector.calls != 10 {
			t.Errorf("收藏次数 = %d，期望 10（上游构造消息的兜底上限）", collector.calls)
		}
	})

	t.Run("表情包功能关闭时不崩", func(t *testing.T) {
		h := newTestHub(t) // 默认 Emoji 为 nil
		msg := groupMessage("m1", "g1", "u1", "小明", "[表情包]")
		msg.Segments = []domain.Segment{{Type: domain.SegmentEmoji, Binary: []byte("a")}}
		h.hub.Dispatch(msg, domain.ScopeGroup, "g1", "bot-1")
	})
}

// failingEmojiCollector 模拟收藏失败，用来校验错误只记录日志、不影响投递。
type failingEmojiCollector struct{ calls int }

func (f *failingEmojiCollector) Collect(context.Context, []byte, string, string, string) (bool, error) {
	f.calls++
	return false, errFakeEmoji
}

var errFakeEmoji = errors.New("收藏失败")

// TestHubCollectEmojiErrorDoesNotBreakDispatch 防的是「收藏失败拖垮整条投递链路」。
func TestHubCollectEmojiErrorDoesNotBreakDispatch(t *testing.T) {
	collector := &failingEmojiCollector{}
	h := newTestHubWith(t, func(o *HubOptions) { o.Emoji = collector })

	msg := groupMessage("m1", "g1", "u1", "小明", "[表情包]")
	msg.Segments = []domain.Segment{{Type: domain.SegmentEmoji, Binary: []byte("a")}}
	h.hub.Dispatch(msg, domain.ScopeGroup, "g1", "bot-1")

	if collector.calls != 1 {
		t.Errorf("收藏次数 = %d，期望 1", collector.calls)
	}
	// 消息仍应落库，会话仍应存在。
	if got, err := h.db.Messages().ByMessageID(t.Context(), msg.MessageID); err != nil || got == nil {
		t.Errorf("收藏失败不应影响消息落库: got=%v err=%v", got, err)
	}
	if h.hub.ActiveCount() != 1 {
		t.Errorf("收藏失败不应影响 runtime 创建，ActiveCount = %d", h.hub.ActiveCount())
	}
}

// 确认 fakeSender 实现了 agent.Sender（接口变动时这里会编译失败）。
var _ agent.Sender = (*fakeSender)(nil)

// recordingNotifier 记录 MessageSaved 调用，供事件流接线测试断言。
type recordingNotifier struct {
	mu      sync.Mutex
	records []string
}

func (r *recordingNotifier) MessageSaved(sessionID string, isSelf bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, fmt.Sprintf("%s|%t", sessionID, isSelf))
}

func (r *recordingNotifier) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.records))
	copy(out, r.records)
	return out
}

// TestHubDispatchNotifiesMessageSaved 守「入站落库后通知事件流」。
//
// WebUI 的实时刷新全靠这条线（对应 MaiBot dashboard 的 chat 域推送）：
// 消息进库但通知没发，前端只能等轮询，实时性退化。
func TestHubDispatchNotifiesMessageSaved(t *testing.T) {
	notif := &recordingNotifier{}
	h := newTestHub(t)
	h.hub.SetNotifier(notif)

	msg := groupMessage("m1", "g1", "u1", "小明", "你好")
	key := domain.SessionKey{Platform: "qq", AccountID: "bot-1", Scope: domain.ScopeGroup, TargetID: "g1"}
	h.hub.Dispatch(msg, domain.ScopeGroup, "g1", "bot-1")

	got := notif.snapshot()
	if len(got) != 1 {
		t.Fatalf("通知次数 = %d（%v），期望 1：一条入站消息应且只应通知一次", len(got), got)
	}
	want := key.ID() + "|false"
	if got[0] != want {
		t.Errorf("通知内容 = %q，期望 %q", got[0], want)
	}
}

// TestHubWithoutNotifierDoesNotPanic 守「未注入 notifier 时投递照常」。
// 测试与工具场景不配 WebUI，这条路径必须和有 notifier 时一样稳。
func TestHubWithoutNotifierDoesNotPanic(t *testing.T) {
	h := newTestHub(t)
	msg := groupMessage("m1", "g1", "u1", "小明", "你好")
	h.hub.Dispatch(msg, domain.ScopeGroup, "g1", "bot-1")
	if h.hub.ActiveCount() != 1 {
		t.Errorf("ActiveCount = %d，期望 1：无 notifier 不应影响投递", h.hub.ActiveCount())
	}
}
