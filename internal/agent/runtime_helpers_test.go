package agent

// runtime 里那些「可以脱离会话主循环测」的部分：
//   - memory_runtime.go 的检索/画像/表情包（依赖 store 与 embedding，不起 goroutine）
//   - quote.go 里依赖 Runtime 字段的判定（quoteFrequency / findMessageByID / decideQuote）
//   - runtime.go 里的纯函数（pickImageBinary / buildAtSegments / scene / truncateRunes）
//
// 刻意不测 NewRuntime / run / think：那些要起 goroutine、要模拟 QQ 链路，
// 已有 internal/mock 的跨包场景测试兜底。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/chat"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/embedding"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/logx"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/store"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/tool"
)

// openAgentTestDB 打开一个临时库。
func openAgentTestDB(t *testing.T) (*store.DB, *config.Config) {
	t.Helper()

	cfg := config.Default()
	cfg.Database.Path = filepath.Join(t.TempDir(), "test.db")

	db, err := store.Open(context.Background(), cfg.Database.Path)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, cfg
}

// newTestRuntime 构造一个只填了必要字段的 Runtime。
//
// 不走 NewRuntime：那会起 goroutine、连门控、注册一堆组件，
// 而这些测试只关心「单次调用的行为」。
// setConfig 在测试里改运行时配置。
//
// 必须走 Store.Update：Runtime 读的是 Store.Get() 的快照，
// 直接改一份 *config.Config 不会影响它；而就地改共享快照会污染
// 后续用例（同一个 cfg 变量会被多个 newTestRuntime 复用）。
func setConfig(t *testing.T, st *config.Store, mutate func(*config.Config)) {
	t.Helper()
	if err := st.Update(func(c *config.Config) error {
		mutate(c)
		return nil
	}); err != nil {
		t.Fatalf("改配置失败: %v", err)
	}
}

func newTestRuntime(t *testing.T, cfg *config.Config, db *store.DB) *Runtime {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	return &Runtime{
		cfg:          config.Wrap(cfg),
		store:        db,
		ctx:          ctx,
		quit:         cancel,
		inbox:        make(chan *domain.Message, 4),
		sessionID:    "sess-1",
		accountID:    "bot-1",
		targetID:     "target-1",
		key:          domain.SessionKey{Platform: "qq", AccountID: "bot-1", Scope: domain.ScopeGroup, TargetID: "g1"},
		isGroup:      true,
		botName:      "木木",
		quoteCounter: chat.NewQuoteCounter(),
		log:          logx.Get("agent.test"),
	}
}

// testSender 是一个记录发送调用的假 Sender。
//
// passiveTos 与 quoteTos 分开记：这两者指向不同的消息、键空间也不同，
// 合成一个字段的话「引用到底有没有发出去」根本无法断言
// —— 那正是修复前引用从来没真正发出的原因之一。
type testSender struct {
	segments   [][]domain.Segment
	passiveTos []string
	quoteTos   []string
	texts      []string
	failWith   error
	nextIDs    []string
}

func (s *testSender) SendText(_ context.Context, _, _ string, text string, passiveTo string, quote domain.Reference) (domain.SendResult, error) {
	if s.failWith != nil {
		return domain.SendResult{}, s.failWith
	}
	s.texts = append(s.texts, text)
	s.passiveTos = append(s.passiveTos, passiveTo)
	s.quoteTos = append(s.quoteTos, quote.RefIdx)
	return domain.SendResult{MessageIDs: s.nextIDs}, nil
}

func (s *testSender) SendSegments(_ context.Context, _, _ string, segments []domain.Segment, passiveTo string, quote domain.Reference) (domain.SendResult, error) {
	if s.failWith != nil {
		return domain.SendResult{}, s.failWith
	}
	s.segments = append(s.segments, segments)
	s.passiveTos = append(s.passiveTos, passiveTo)
	s.quoteTos = append(s.quoteTos, quote.RefIdx)
	return domain.SendResult{MessageIDs: s.nextIDs}, nil
}

// TestTruncateRunesKeepsUTF8 验证画像截断不劈坏汉字。
func TestTruncateRunesKeepsUTF8(t *testing.T) {
	long := strings.Repeat("木", 50)

	// 未超限：原样返回，不追加省略号。
	if got := truncateRunes("短文本", 10); got != "短文本" {
		t.Errorf("未超限时应原样返回，实际 %q", got)
	}
	if got := truncateRunes(long, 50); got != long {
		t.Errorf("正好等于上限时不该截断")
	}

	got := truncateRunes(long, 10)
	if !utf8.ValidString(got) {
		t.Fatal("截断后不是合法 UTF-8")
	}
	if got != strings.Repeat("木", 10)+"..." {
		t.Errorf("截断结果 = %q", got)
	}
}

// TestScene 验证出站场景字符串（QQ 适配器按它选接口）。
func TestScene(t *testing.T) {
	r := &Runtime{isGroup: true}
	if got := r.scene(); got != "group" {
		t.Errorf("群聊场景 = %q，期望 group", got)
	}
	r.isGroup = false
	if got := r.scene(); got != "private" {
		t.Errorf("私聊场景 = %q，期望 private", got)
	}
}

// TestPickImageBinary 验证从消息里挑图片二进制。
func TestPickImageBinary(t *testing.T) {
	msg := &domain.Message{Segments: []domain.Segment{
		domain.Text("看这几张"),
		{Type: domain.SegmentImage, Binary: []byte("第一张")},
		// 只有 URL 没有二进制：不算一张图（数据库回填就是这种形态）。
		{Type: domain.SegmentImage, URL: "https://example.com/x.png"},
		{Type: domain.SegmentEmoji, Binary: []byte("表情")},
		{Type: domain.SegmentVoice, Binary: []byte("语音不该被选中")},
	}}

	if got, ok := pickImageBinary(msg, 0); !ok || string(got) != "第一张" {
		t.Errorf("index=0 应取第一张，实际 %q ok=%v", got, ok)
	}
	if got, ok := pickImageBinary(msg, 1); !ok || string(got) != "表情" {
		t.Errorf("index=1 应取表情（跳过无二进制的图），实际 %q ok=%v", got, ok)
	}
	// 越界返回 false，调用方据此记日志跳过。
	if _, ok := pickImageBinary(msg, 2); ok {
		t.Errorf("index=2 越界，应返回 false")
	}
	// 负索引按 0 处理（模型偶尔会填 -1）。
	if got, ok := pickImageBinary(msg, -1); !ok || string(got) != "第一张" {
		t.Errorf("负索引应按 0 处理，实际 %q", got)
	}
}

// TestBuildAtSegments 验证 attach_at 的 msg_id 解析。
//
// @ 失败只能跳过不能报错：模型填错一个 ID 不该让整条回复发不出去。
func TestBuildAtSegments(t *testing.T) {
	r := &Runtime{
		log: logx.Get("agent.test"),
		history: []*domain.Message{
			{MessageID: "m1", UserID: "u1", UserName: "小明"},
			{MessageID: "m2", UserID: "", UserName: "匿名"},
		},
	}

	// 空输入直接返回 nil。
	if got := r.buildAtSegments(nil); got != nil {
		t.Errorf("空输入应返回 nil，实际 %v", got)
	}

	got := r.buildAtSegments([]string{"m1", "  m1  ", "", "不存在", "m2"})
	if len(got) != 1 {
		t.Fatalf("应只有 1 个有效 at 段（去重 + 跳过无效），实际 %d 个：%v", len(got), got)
	}
	if got[0].Type != domain.SegmentAt || got[0].UserID != "u1" || got[0].UserName != "小明" {
		t.Errorf("at 段构造不对：%+v", got[0])
	}
}

// TestSendAttachedImages 验证附图发送：解析图片、逐条单独发送、失败不阻断其它。
func TestSendAttachedImages(t *testing.T) {
	sender := &testSender{nextIDs: []string{"sent-1"}}
	r := &Runtime{
		log:      logx.Get("agent.test"),
		sender:   sender,
		targetID: "t",
		isGroup:  true,
		history: []*domain.Message{
			{MessageID: "m1", Segments: []domain.Segment{
				{Type: domain.SegmentImage, Binary: []byte("图1")},
				{Type: domain.SegmentImage, Binary: []byte("图2")},
			}},
		},
	}

	// 没有要发的图：直接返回，不该调用 sender。
	ids, err := r.sendAttachedImages(context.Background(), nil)
	if err != nil || ids != nil {
		t.Errorf("空输入应返回 nil,nil，实际 %v,%v", ids, err)
	}
	if len(sender.segments) != 0 {
		t.Errorf("空输入不该发送")
	}

	ids, err = r.sendAttachedImages(context.Background(), []tool.AttachPic{
		{MsgID: "m1", Index: 1},
		{MsgID: "不存在", Index: 0}, // 目标不在上下文：跳过
		{MsgID: "m1", Index: 9},  // 越界：跳过
	})
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if len(sender.segments) != 1 {
		t.Fatalf("应只发送 1 张（其余跳过），实际 %d 次", len(sender.segments))
	}
	if string(sender.segments[0][0].Binary) != "图2" {
		t.Errorf("应发送 index=1 那张，实际 %q", sender.segments[0][0].Binary)
	}
	if len(ids) != 1 || ids[0] != "sent-1" {
		t.Errorf("发送结果 = %v", ids)
	}
}

// TestSearchMemoryKeywordFallback 验证未配向量时降级到关键词检索。
//
// 这是 AGENTS.md 第 43 条的核心：关键词路径必须仍然可用，
// 否则端点一挂记忆功能就整体消失。
func TestSearchMemoryKeywordFallback(t *testing.T) {
	db, cfg := openAgentTestDB(t)
	r := newTestRuntime(t, cfg, db)

	// 先写一条能被分词命中的记忆。
	if err := db.Memory().SaveSummary(context.Background(), &store.MemorySummary{
		SessionID:    "sess-1",
		Summary:      "木木昨天说要去吃火锅",
		TimeRange:    "昨天",
		Participants: []string{"小明"},
		CreatedAt:    time.Now().Unix(),
	}); err != nil {
		t.Fatalf("写入记忆失败: %v", err)
	}

	res, err := r.SearchMemory(tool.MemoryQuery{Query: "火锅", Mode: "search"})
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if !res.Fallback {
		t.Errorf("没配向量时应标记为降级")
	}
	if res.Mode != "keyword" {
		t.Errorf("降级后 mode = %q，期望 keyword", res.Mode)
	}
	if len(res.Hits) == 0 {
		t.Fatalf("应命中「火锅」这条记忆")
	}
	if !strings.Contains(res.Hits[0].Summary, "火锅") {
		t.Errorf("命中内容不对：%q", res.Hits[0].Summary)
	}

	// limit 默认值来自配置。
	res, err = r.SearchMemory(tool.MemoryQuery{Query: "火锅", Mode: "search", Limit: 0})
	if err != nil || len(res.Hits) == 0 {
		t.Errorf("limit=0 时应回退到配置默认值，err=%v", err)
	}

	// 空查询不查库，直接返回（避免把整库拉出来）。
	res, err = r.SearchMemory(tool.MemoryQuery{Query: "   ", Mode: "search"})
	if err != nil || len(res.Hits) != 0 {
		t.Errorf("空查询不该有结果，err=%v hits=%v", err, res.Hits)
	}
}

// TestSearchMemoryTimeMode 验证按时间检索。
func TestSearchMemoryTimeMode(t *testing.T) {
	db, cfg := openAgentTestDB(t)
	r := newTestRuntime(t, cfg, db)

	now := time.Now().Unix()
	for i, text := range []string{"很久以前的事", "刚刚发生的事"} {
		if err := db.Memory().SaveSummary(context.Background(), &store.MemorySummary{
			SessionID: "sess-1",
			Summary:   text,
			CreatedAt: now - int64(3600*(1-i)),
		}); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
	}

	res, err := r.SearchMemory(tool.MemoryQuery{Mode: "time", TimeStart: now - 60, Limit: 10})
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if res.Mode != "time" {
		t.Errorf("mode = %q，期望 time", res.Mode)
	}
	if len(res.Hits) != 1 || !strings.Contains(res.Hits[0].Summary, "刚刚发生的事") {
		t.Errorf("时间过滤不对：%v", res.Hits)
	}

	// hybrid：先按时间圈定，再在圈内排序（没配向量时退化为纯时间序）。
	res, err = r.SearchMemory(tool.MemoryQuery{Mode: "hybrid", Query: "事", TimeStart: now - 86400, Limit: 10})
	if err != nil {
		t.Fatalf("hybrid 检索失败: %v", err)
	}
	if res.Mode != "hybrid" {
		t.Errorf("hybrid 的 mode = %q", res.Mode)
	}
	if len(res.Hits) != 2 {
		t.Errorf("hybrid 应拿到 2 条，实际 %d", len(res.Hits))
	}
}

// TestSearchMemoryPersonFilter 验证按人物过滤。
func TestSearchMemoryPersonFilter(t *testing.T) {
	db, cfg := openAgentTestDB(t)
	r := newTestRuntime(t, cfg, db)

	now := time.Now().Unix()
	_ = db.Memory().SaveSummary(context.Background(), &store.MemorySummary{
		SessionID: "sess-1", Summary: "聊了摄影", Participants: []string{"小明"}, CreatedAt: now,
	})
	_ = db.Memory().SaveSummary(context.Background(), &store.MemorySummary{
		SessionID: "sess-1", Summary: "聊了做饭", Participants: []string{"小红"}, CreatedAt: now - 1,
	})

	res, err := r.SearchMemory(tool.MemoryQuery{Mode: "time", Limit: 10, PersonName: "小明"})
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if len(res.Hits) != 1 || !strings.Contains(res.Hits[0].Summary, "摄影") {
		t.Errorf("人物过滤不对：%v", res.Hits)
	}
}

// TestSearchMemoryWithoutStore 验证没有数据库时明确报错，而不是静默返回空。
func TestSearchMemoryWithoutStore(t *testing.T) {
	r := newTestRuntime(t, config.Default(), nil)
	if _, err := r.SearchMemory(tool.MemoryQuery{Query: "x"}); err == nil {
		t.Errorf("没有数据库时应报错")
	}
}

// TestPersonProfile 验证画像读取与截断。
func TestPersonProfile(t *testing.T) {
	db, cfg := openAgentTestDB(t)
	r := newTestRuntime(t, cfg, db)

	// 没有画像时返回空串（调用方据此不注入）。
	if got := r.PersonProfile("u1"); got != "" {
		t.Errorf("无画像时应返回空串，实际 %q", got)
	}

	profile := `# 人物画像
## 身份设定
- 大二学生
## 关系设定
- 暂无
## 稳定了解
- 喜欢摄影
## 相处偏好
- 说话喜欢简短
## 近期互动
- 昨天聊了火锅
## 不确定信息
- 也许住在南方
## 维护备注
- 自动画像仅供内部参考`
	if err := db.Memory().UpsertProfile(context.Background(), &store.PersonProfile{
		Platform: "qq", UserID: "u1", SessionID: "sess-1", DisplayName: "小明", ProfileText: profile,
	}); err != nil {
		t.Fatalf("写入画像失败: %v", err)
	}

	got := r.PersonProfile("u1")
	if got == "" {
		t.Fatal("应读到画像")
	}
	// 只注入前 5 节：后两节是模型猜测与维护信息，塞进提示词只会增加噪声。
	for _, want := range []string{"大二学生", "喜欢摄影", "说话喜欢简短", "昨天聊了火锅"} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少 %q：%s", want, got)
		}
	}
	for _, bad := range []string{"也许住在南方", "自动画像仅供内部参考", "暂无"} {
		if strings.Contains(got, bad) {
			t.Errorf("不该包含 %q：%s", bad, got)
		}
	}

	// 开关关闭时不读库。
	cfg.Memory.ProfileEnabled = false
	if got := r.PersonProfile("u1"); got != "" {
		t.Errorf("画像开关关闭时应返回空串，实际 %q", got)
	}
	cfg.Memory.ProfileEnabled = true
	if got := r.PersonProfile(""); got != "" {
		t.Errorf("空 UserID 应返回空串")
	}

	// 超长画像按 rune 截断，不能劈坏 UTF-8。
	cfg.Memory.ProfileMaxChars = 10
	long := "## 身份设定\n" + strings.Repeat("木", 200)
	_ = db.Memory().UpsertProfile(context.Background(), &store.PersonProfile{
		Platform: "qq", UserID: "u2", SessionID: "sess-1", ProfileText: long,
	})
	cut := r.PersonProfile("u2")
	if !utf8.ValidString(cut) {
		t.Fatal("截断后不是合法 UTF-8")
	}
	if utf8.RuneCountInString(cut) > 13 { // 10 个 rune + "..."
		t.Errorf("截断后字数 %d 超限", utf8.RuneCountInString(cut))
	}
}

// TestPickEmoji 验证表情包选取。
func TestPickEmoji(t *testing.T) {
	db, cfg := openAgentTestDB(t)
	r := newTestRuntime(t, cfg, db)

	// 库里没有表情包：返回 false，调用方静默跳过。
	if _, ok := r.PickEmoji("开心"); ok {
		t.Errorf("空库时不该返回表情包")
	}

	// 写两张进去。
	for _, e := range []*store.Emoji{
		{FileHash: "h1", FilePath: "data/emoji/1.png", Description: "开心 大笑", CreatedAt: time.Now().Unix()},
		{FileHash: "h2", FilePath: "data/emoji/2.png", Description: "无语 翻白眼", CreatedAt: time.Now().Unix()},
	} {
		if _, err := db.Emojis().Save(context.Background(), e); err != nil {
			t.Fatalf("写入表情包失败: %v", err)
		}
	}

	ref, ok := r.PickEmoji("开心")
	if !ok {
		t.Fatal("应选出一张表情包")
	}
	if ref.Hash != "h1" || !strings.Contains(ref.Description, "开心") || ref.Path == "" {
		t.Errorf("选中结果不对：%+v", ref)
	}

	// 情绪描述没命中时随机兜底：VLM 打的标签未必与模型填的词字面重合。
	ref, ok = r.PickEmoji("完全不存在的情绪词")
	if !ok {
		t.Errorf("没命中时应随机兜底")
	}
	if ref.Hash == "" {
		t.Errorf("兜底结果应带 hash")
	}

	// 空情绪词走随机路径。
	if _, ok := r.PickEmoji("  "); !ok {
		t.Errorf("空情绪词应随机取一张")
	}

	// 表情包总开关关闭时一律返回 false。
	cfg.Emoji.Enabled = false
	if _, ok := r.PickEmoji("开心"); ok {
		t.Errorf("开关关闭时不该返回表情包")
	}
}

// TestSendEmoji 验证表情包发送与使用计数。
func TestSendEmoji(t *testing.T) {
	db, cfg := openAgentTestDB(t)
	sender := &testSender{nextIDs: []string{"emoji-msg"}}
	r := newTestRuntime(t, cfg, db)
	r.sender = sender

	// 路径为空：直接报错，别让 sender 收到空图。
	if _, err := r.SendEmoji(tool.EmojiRef{}); err == nil {
		t.Errorf("路径为空时应报错")
	}

	dir := t.TempDir()
	file := filepath.Join(dir, "e.png")
	if err := os.WriteFile(file, testPNG(t), 0o644); err != nil {
		t.Fatalf("写测试文件失败: %v", err)
	}
	if _, err := db.Emojis().Save(context.Background(), &store.Emoji{
		FileHash: "h1", FilePath: file, Description: "开心", CreatedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("写入表情包失败: %v", err)
	}

	ids, err := r.SendEmoji(tool.EmojiRef{Hash: "h1", Path: file})
	if err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	if len(ids) != 1 || ids[0] != "emoji-msg" {
		t.Errorf("返回 ID = %v", ids)
	}
	if len(sender.segments) != 1 || sender.segments[0][0].Type != domain.SegmentImage {
		t.Errorf("应作为图片段发送：%v", sender.segments)
	}
	// 发送后应记一次使用，供之后「常用优先」排序。
	got, err := db.Emojis().GetByHash(context.Background(), "h1")
	if err != nil || got == nil {
		t.Fatalf("查询表情包失败: %v", err)
	}
	if got.UsageCount == 0 {
		t.Errorf("使用计数没有增加")
	}

	// 文件不存在：报错并带上路径上下文。
	if _, err := r.SendEmoji(tool.EmojiRef{Path: filepath.Join(dir, "missing.png")}); err == nil {
		t.Errorf("文件不存在时应报错")
	}
}

// TestEmojiIDByHash 验证按哈希查 ID 的边界。
func TestEmojiIDByHash(t *testing.T) {
	db, cfg := openAgentTestDB(t)
	r := newTestRuntime(t, cfg, db)

	if got := r.emojiIDByHash(""); got != 0 {
		t.Errorf("空哈希应返回 0，实际 %d", got)
	}
	if got := r.emojiIDByHash("不存在"); got != 0 {
		t.Errorf("查不到应返回 0，实际 %d", got)
	}

	// 没有数据库时也不能 panic。
	noStore := newTestRuntime(t, cfg, nil)
	if got := noStore.emojiIDByHash("h1"); got != 0 {
		t.Errorf("无数据库应返回 0，实际 %d", got)
	}
}

// TestRandN 验证随机取模的边界。
func TestRandN(t *testing.T) {
	r := &Runtime{}
	for _, n := range []int{-1, 0, 1} {
		if got := r.randN(n); got != 0 {
			t.Errorf("randN(%d) = %d，期望 0", n, got)
		}
	}
	for i := 0; i < 200; i++ {
		if got := r.randN(5); got < 0 || got >= 5 {
			t.Fatalf("randN(5) 越界：%d", got)
		}
	}
}

// TestDedupeSummaries 验证按 ID 去重并保持顺序。
func TestDedupeSummaries(t *testing.T) {
	// 空输入返回空切片（不是 nil）。
	if got := dedupeSummaries(nil); len(got) != 0 {
		t.Errorf("空输入应返回空切片，实际 %v", got)
	}

	in := []*store.MemorySummary{
		{ID: 1, Summary: "a"},
		{ID: 2, Summary: "b"},
		nil,
		{ID: 1, Summary: "a 重复"},
		{ID: 3, Summary: "c"},
	}
	got := dedupeSummaries(in)
	if len(got) != 3 {
		t.Fatalf("去重后应剩 3 条，实际 %d", len(got))
	}
	order := []int64{1, 2, 3}
	for i, want := range order {
		if got[i].ID != want {
			t.Errorf("第 %d 条 ID = %d，期望 %d（顺序必须保持）", i, got[i].ID, want)
		}
	}
}

// TestFilterByPerson 验证人物过滤的两种命中方式。
func TestFilterByPerson(t *testing.T) {
	in := []*store.MemorySummary{
		{ID: 1, Summary: "聊了别的", Participants: []string{"小明", "小红"}},
		{ID: 2, Summary: "小明说他要考研", Participants: []string{"其他人"}},
		{ID: 3, Summary: "完全无关", Participants: nil},
	}

	got := filterByPerson(in, "小明")
	if len(got) != 2 {
		t.Fatalf("应命中 2 条（participants 与正文各一条），实际 %d", len(got))
	}
	// 顺序保持原样。
	if got[0].ID != 1 || got[1].ID != 2 {
		t.Errorf("过滤后顺序不对：%v", got)
	}

	if got := filterByPerson(in, "查无此人"); len(got) != 0 {
		t.Errorf("无匹配时应返回空，实际 %v", got)
	}
}

// TestEmbeddingModelInfo 验证 embedding 模型标识与维度解析。
func TestEmbeddingModelInfo(t *testing.T) {
	cfg := config.Default()
	r := &Runtime{cfg: config.Wrap(cfg)}

	// 没配 embedding 模型：报错（调用方据此降级到关键词）。
	if _, _, err := r.embeddingModelInfo(); err == nil {
		t.Errorf("未配置 embedding 模型时应报错")
	}

	// 运行期改配置必须走 Store.Update：直接改字段改的是共享快照，
	// 而且 Runtime 读的是 Store.Get()，就地改根本不会生效。
	dim := 1024
	if err := r.cfg.Update(func(c *config.Config) error {
		c.LLM.Providers = []config.ProviderConfig{{Name: "p", BaseURL: "http://127.0.0.1:1"}}
		c.LLM.Models = []config.ModelConfig{{
			Name: "e", ModelIdentifier: "emb-model", Provider: "p", Dimension: &dim,
		}}
		c.LLM.Tasks.Embedding.Models = []string{"e"}
		return nil
	}); err != nil {
		t.Fatalf("改配置失败: %v", err)
	}

	model, gotDim, err := r.embeddingModelInfo()
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if model != "emb-model" || gotDim != 1024 {
		t.Errorf("= (%q,%d)，期望 (emb-model,1024)", model, gotDim)
	}

	// 配置里没写维度、客户端也没测到维度：报错。
	if err := r.cfg.Update(func(c *config.Config) error {
		c.LLM.Models[0].Dimension = nil
		return nil
	}); err != nil {
		t.Fatalf("改配置失败: %v", err)
	}
	r.embedding = embedding.New(r.cfg)
	if _, _, err := r.embeddingModelInfo(); err == nil {
		t.Errorf("维度未知时应报错")
	}
}

// TestSearchByVectorEndToEnd 用假 embedding 服务走一遍向量检索。
//
// 这条路径的收益是「与原文无字面重合也能命中」，
// 只测关键词路径证明不了这一点（AGENTS.md 第 43、44 条）。
func TestSearchByVectorEndToEnd(t *testing.T) {
	db, cfg := openAgentTestDB(t)

	// 假向量服务：固定返回一个三维向量。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "emb",
			"data": []map[string]any{
				{"index": 0, "embedding": []float32{1, 0, 0}},
			},
		})
	}))
	t.Cleanup(srv.Close)

	dim := 3
	cfg.LLM.Providers = []config.ProviderConfig{{Name: "p", BaseURL: srv.URL, APIKey: "k", Timeout: 5}}
	cfg.LLM.Models = []config.ModelConfig{{
		Name: "e", ModelIdentifier: "emb", Provider: "p", Dimension: &dim,
	}}
	cfg.LLM.Tasks.Embedding.Models = []string{"e"}

	r := newTestRuntime(t, cfg, db)
	r.embedding = embedding.New(config.Wrap(cfg))

	// 一条向量与查询方向一致，一条正交。
	vecA := embedding.Pack([]float32{1, 0, 0})
	vecB := embedding.Pack([]float32{0, 1, 0})
	for _, s := range []*store.MemorySummary{
		{SessionID: "sess-1", Summary: "语义相关的记忆", CreatedAt: time.Now().Unix(),
			Embedding: vecA, EmbedModel: "emb", EmbedDim: 3},
		{SessionID: "sess-1", Summary: "完全无关的记忆", CreatedAt: time.Now().Unix() - 1,
			Embedding: vecB, EmbedModel: "emb", EmbedDim: 3},
	} {
		if err := db.Memory().SaveSummary(context.Background(), s); err != nil {
			t.Fatalf("写入记忆失败: %v", err)
		}
	}

	res, err := r.SearchMemory(tool.MemoryQuery{Query: "没有字面重合的问题", Mode: "search"})
	if err != nil {
		t.Fatalf("向量检索失败: %v", err)
	}
	if res.Fallback {
		t.Errorf("向量检索成功时不该标记降级")
	}
	if res.Mode != "vector" {
		t.Errorf("mode = %q，期望 vector", res.Mode)
	}
	if len(res.Hits) != 1 || !strings.Contains(res.Hits[0].Summary, "语义相关") {
		t.Errorf("应只命中语义相关那条：%v", res.Hits)
	}

	// hybrid 模式：时间圈定 + 向量重排。
	res, err = r.SearchMemory(tool.MemoryQuery{
		Query: "没有字面重合的问题", Mode: "hybrid",
		TimeStart: time.Now().Add(-time.Hour).Unix(), Limit: 10,
	})
	if err != nil {
		t.Fatalf("hybrid 失败: %v", err)
	}
	if res.Mode != "hybrid" || len(res.Hits) == 0 {
		t.Errorf("hybrid 应有结果：mode=%q hits=%d", res.Mode, len(res.Hits))
	}
}

// TestBuildExpressionHabitsFromStore 验证学到的表达被拼成注入文本并记一次使用。
func TestBuildExpressionHabitsFromStore(t *testing.T) {
	db, cfg := openAgentTestDB(t)
	r := newTestRuntime(t, cfg, db)

	// 开关关闭：不读库。
	cfg.Learning.ExpressionEnabled = false
	if got := r.buildExpressionHabits(); got != "" {
		t.Errorf("开关关闭时应返回空串，实际 %q", got)
	}

	cfg.Learning.ExpressionEnabled = true
	// 库里没有表达：返回空串，不产生「【表达习惯参考】」空块。
	if got := r.buildExpressionHabits(); got != "" {
		t.Errorf("没有表达时应返回空串，实际 %q", got)
	}

	for _, e := range []*store.Expression{
		{SessionID: "sess-1", Situation: "被夸奖", Style: "谦虚地岔开话题", CreatedAt: time.Now().Unix()},
		{SessionID: "sess-1", Situation: "被问技术问题", Style: "先给结论再说原因", CreatedAt: time.Now().Unix()},
	} {
		if _, err := db.Memory().SaveExpression(context.Background(), e); err != nil {
			t.Fatalf("写入表达失败: %v", err)
		}
	}

	cfg.Learning.ExpressionMaxUsed = 2
	got := r.buildExpressionHabits()
	if !strings.Contains(got, "【表达习惯参考】") {
		t.Fatalf("缺少标题：%q", got)
	}
	// 措辞照抄参考实现：明确要求「不要直接照搬」，
	// 否则模型会把学到的例句原样贴出来，一眼假。
	if !strings.Contains(got, "不要直接照搬") {
		t.Errorf("缺少「不要照搬」的约束：%q", got)
	}
	if !strings.Contains(got, "当「被夸奖」时，可以「谦虚地岔开话题」") {
		t.Errorf("表达格式不对：%q", got)
	}

	// 用了要记一次，靠 used_count 做轮换。
	items, err := db.Memory().ListExpressions(context.Background(), "sess-1", 5)
	if err != nil {
		t.Fatalf("查询表达失败: %v", err)
	}
	used := 0
	for _, it := range items {
		if it.UsedCount > 0 {
			used++
		}
	}
	if used == 0 {
		t.Errorf("使用计数没有增加，轮换会失效")
	}

	// 没有数据库时不 panic。
	noStore := newTestRuntime(t, cfg, nil)
	if got := noStore.buildExpressionHabits(); got != "" {
		t.Errorf("无数据库时应返回空串，实际 %q", got)
	}
}

// TestBuildKeywordReactionUsesLastUserMessage 验证只匹配最近一条用户消息。
//
// 匹配整批历史会让一条旧消息的关键词在之后每条回复里反复触发。
func TestBuildKeywordReactionUsesLastUserMessage(t *testing.T) {
	r := newTestRuntime(t, config.Default(), nil)
	setConfig(t, r.cfg, func(c *config.Config) {
		c.Keyword.Rules = []config.KeywordRule{
			{Keywords: []string{"报错"}, Reaction: "先安抚再问细节"},
		}
	})
	r.lastBatch = []*domain.Message{
		{UserID: "u1", PlainText: "程序报错了"},
		{UserID: "u1", PlainText: "今天天气不错"}, // 最近一条，没命中
	}

	if got := r.buildKeywordReaction(); got != "" {
		t.Errorf("应只看最近一条（没命中），实际 %q", got)
	}

	// 最近一条命中时才有反应。
	r.lastBatch = []*domain.Message{
		{UserID: "u1", PlainText: "今天天气不错"},
		{UserID: "u1", PlainText: "又报错了"},
	}
	got := r.buildKeywordReaction()
	if !strings.Contains(got, "先安抚再问细节") {
		t.Errorf("应命中关键词，实际 %q", got)
	}

	// 没有规则时返回空串。
	setConfig(t, r.cfg, func(c *config.Config) { c.Keyword.Rules = nil })
	if got := r.buildKeywordReaction(); got != "" {
		t.Errorf("没有规则时应返回空串，实际 %q", got)
	}
}

// TestQuoteFrequencyFromConfig 验证群聊/私聊各读各的配置，非法值按 never。
func TestQuoteFrequencyFromConfig(t *testing.T) {
	r := newTestRuntime(t, config.Default(), nil)
	setConfig(t, r.cfg, func(c *config.Config) {
		c.Chat.Style.GroupQuoteFrequency = "often"
		c.Chat.Style.PrivateQuoteFrequency = "never"
	})

	r.isGroup = true
	if got := r.quoteFrequency(); got != chat.QuoteOften {
		t.Errorf("群聊档位 = %v，期望 often", got)
	}
	r.isGroup = false
	if got := r.quoteFrequency(); got != chat.QuoteNever {
		t.Errorf("私聊档位 = %v，期望 never", got)
	}

	// 配置写错时按 never 处理：宁可少引用，也不要随机乱引用。
	setConfig(t, r.cfg, func(c *config.Config) { c.Chat.Style.GroupQuoteFrequency = "有时候" })
	r.isGroup = true
	if got := r.quoteFrequency(); got != chat.QuoteNever {
		t.Errorf("非法配置应回退到 never，实际 %v", got)
	}
}

// TestFindMessageByID 验证在 history 与 pending 里反查消息。
func TestFindMessageByID(t *testing.T) {
	r := &Runtime{
		history: []*domain.Message{
			{MessageID: "h1", PlainText: "历史一"},
			{MessageID: "h2", PlainText: "历史二"},
		},
		pending: []*domain.Message{
			{MessageID: "p1", PlainText: "待处理"},
		},
	}

	if got := r.findMessageByID("h1"); got == nil || got.PlainText != "历史一" {
		t.Errorf("应查到 h1，实际 %v", got)
	}
	if got := r.findMessageByID("  p1  "); got == nil || got.PlainText != "待处理" {
		t.Errorf("应去除空白后查到 p1，实际 %v", got)
	}
	if got := r.findMessageByID("不存在"); got != nil {
		t.Errorf("查不到应返回 nil，实际 %v", got)
	}
	if got := r.findMessageByID("   "); got != nil {
		t.Errorf("空 ID 应返回 nil，实际 %v", got)
	}
}

// TestDecideQuotePaths 验证引用判定的各条分支。
//
// 注意断言的**不是** MessageID 而是 RefMsgIdx：
// 出站 message_reference 要的是 REFIDX_ 形态的引用索引，
// 拿 ROBOT1.0_ 形态的 MessageID 填进去平台也接受，
// 但客户端引用块无法被后续消息正确回引（见 domain.Reference 的说明）。
func TestDecideQuotePaths(t *testing.T) {
	// 档位 never：一律不引用，不看其它条件。
	r := newTestRuntime(t, config.Default(), nil)
	setConfig(t, r.cfg, func(c *config.Config) { c.Chat.Style.GroupQuoteFrequency = "never" })
	if got := r.decideQuote("m1", "你好"); !got.IsZero() {
		t.Errorf("never 档位不该引用，实际 %q", got.RefIdx)
	}

	// 没有计数器：不引用（避免在没有计数依据时随机引用）。
	r.quoteCounter = nil
	setConfig(t, r.cfg, func(c *config.Config) { c.Chat.Style.GroupQuoteFrequency = "often" })
	if got := r.decideQuote("m1", "你好"); !got.IsZero() {
		t.Errorf("没有计数器时不该引用，实际 %q", got.RefIdx)
	}

	// 目标消息不在上下文里：不能引用。
	//
	// 这里与参考实现有一处刻意差异：参考实现会「直接引用目标」，
	// 但它手里有完整的消息对象；我们找不到消息就拿不到它的引用索引，
	// 此时拿 MessageID 充数会让客户端显示一个指向错误消息的引用块。
	r.quoteCounter = chat.NewQuoteCounter()
	if got := r.decideQuote("m-unknown", "你好"); !got.IsZero() {
		t.Errorf("找不到目标时不该引用，实际 %q", got.RefIdx)
	}

	// 目标在上下文但**没有引用索引**（改造前入库的旧消息）：不能引用。
	r.history = []*domain.Message{{MessageID: "m1", UserID: "u1", ReceivedAt: time.Now()}}
	if got := r.decideQuote("m1", "你好"); !got.IsZero() {
		t.Errorf("目标没有引用索引时不该引用，实际 %q", got.RefIdx)
	}

	// 目标有引用索引但不在计数窗口内：直接引用。
	r.history = []*domain.Message{{
		MessageID: "m1", UserID: "u1", RefMsgIdx: "REFIDX_m1", ReceivedAt: time.Now(),
	}}
	if got := r.decideQuote("m1", "你好"); got.RefIdx != "REFIDX_m1" {
		t.Errorf("目标不在窗口内时应直接引用 REFIDX_m1，实际 %q", got.RefIdx)
	}

	// 目标在窗口内：结果取决于概率抽签，但只能是要么不引用、要么引用那个索引。
	r.quoteCounter.Record("m1", "u1", time.Now().UnixNano())
	got := r.decideQuote("m1", "你好")
	if !got.IsZero() && got.RefIdx != "REFIDX_m1" {
		t.Errorf("返回值只能是空或 REFIDX_m1，实际 %q", got.RefIdx)
	}
}

// TestPersonProfileErrorPath 覆盖读库出错时的兜底。
//
// 库挂了只能返回空串并记日志：画像缺失不影响这轮回复，
// 但让整轮思考失败就是大问题。
func TestPersonProfileErrorPath(t *testing.T) {
	db, cfg := openAgentTestDB(t)
	r := newTestRuntime(t, cfg, db)
	// 关掉库：后续查询必然报错。
	if err := db.Close(); err != nil {
		t.Fatalf("关闭数据库失败: %v", err)
	}

	if got := r.PersonProfile("u1"); got != "" {
		t.Errorf("读库失败时应返回空串，实际 %q", got)
	}
	// 表情包与表达习惯同样要静默降级。
	if _, ok := r.PickEmoji("开心"); ok {
		t.Errorf("读库失败时不该返回表情包")
	}
	if got := r.buildExpressionHabits(); got != "" {
		t.Errorf("读库失败时应返回空串，实际 %q", got)
	}
}

// TestSearchMemoryFallbackToTime 覆盖「向量与关键词都没结果时退一步按时间查」。
//
// 宁可给点相关背景，也不要空手而归。
func TestSearchMemoryFallbackToTime(t *testing.T) {
	db, cfg := openAgentTestDB(t)
	r := newTestRuntime(t, cfg, db)

	createdAt := time.Now().Unix()
	if err := db.Memory().SaveSummary(context.Background(), &store.MemorySummary{
		SessionID: "sess-1",
		Summary:   "一条与查询无字面重合的记忆",
		CreatedAt: createdAt,
	}); err != nil {
		t.Fatalf("写入记忆失败: %v", err)
	}

	// 查询词与正文完全不重合 → 关键词 0 命中；但给了时间范围 → 退化为时间检索。
	res, err := r.SearchMemory(tool.MemoryQuery{
		Query:     "zzz-完全不相干-zzz",
		Mode:      "search",
		TimeStart: createdAt - 60,
		TimeEnd:   createdAt + 60,
		Limit:     10,
	})
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if len(res.Hits) != 1 {
		t.Fatalf("应退化为时间检索拿到 1 条，实际 %d 条", len(res.Hits))
	}
	if res.Mode != "time" {
		t.Errorf("退化后 mode = %q，期望 time", res.Mode)
	}
	if !res.Fallback {
		t.Errorf("应标记为降级")
	}
}

// TestRuntimeCloseIsIdempotentAndBlocksPost 防两件事：
//
//  1. Close 被 Hub 的回收协程与 Hub.Close 各调一次时不能 panic。
//     以前 Close 直接把 closed 置 true 再 quit，第二次调 quit 虽然安全，
//     但「幂等」只靠 cancel 恰好可重入来兜，语义上是碰巧。
//  2. Close 之后 Post 必须立刻返回 false，不能把消息塞进没人读的 inbox。
//
// 构造用 newTestRuntime 而不是 NewRuntime：后者会起 run 协程，
// 而这里要的正是「没有消费者时 Post 的行为」。
func TestRuntimeCloseIsIdempotentAndBlocksPost(t *testing.T) {
	cfg := config.Default()
	r := newTestRuntime(t, cfg, nil)

	if !r.Post(&domain.Message{ID: "m1", PlainText: "你好"}) {
		t.Fatal("关闭前 Post 应成功")
	}

	r.Close()
	r.Close() // 第二次不能 panic

	if r.Post(&domain.Message{ID: "m2", PlainText: "还在吗"}) {
		t.Error("关闭后 Post 应返回 false")
	}
	if r.ctx.Err() == nil {
		t.Error("关闭后 ctx 应已取消")
	}
}

// TestRuntimeConcurrentCloseAndPost 防：Close 与 Post 并发时的数据竞争。
//
// Hub 的回收协程会调 Close，网关协程同时在 Post —— 这两条路径真的会撞上。
// 曾经 closed 是裸 bool、lastActiveAt 是裸 time.Time，功能上「多数时候没事」，
// 但这正是最难查的一类：只有 -race 能看到，线上表现为偶发的
// 「刚回收的会话又收到一条消息」或彻底不相关的字段被写坏。
//
// 没有 -race 时这个测试只能验证「不出错」，但它至少锁住了并发语义。
func TestRuntimeConcurrentCloseAndPost(t *testing.T) {
	cfg := config.Default()
	r := newTestRuntime(t, cfg, nil)

	var wg sync.WaitGroup

	// 一个协程反复投递。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			r.Post(&domain.Message{ID: "m", PlainText: "消息"})
		}
	}()

	// 另一个协程反复读最近活跃时间（Hub 的回收路径就是这么干的）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = r.LastActive()
		}
	}()

	// 主协程在中间关闭。
	time.Sleep(5 * time.Millisecond)
	r.Close()

	wg.Wait()

	if r.Post(&domain.Message{ID: "after"}) {
		t.Error("关闭后 Post 必须返回 false")
	}
}

// TestLastActiveNeverZeroTime 防：还没记过一次活跃的 runtime 被当成过期会话回收。
//
// time.Time{} 的零值早于任何 cutoff，Hub 的 collectIdle 会立刻把它关掉。
// 而 Close 之后 wg.Wait 又会等 run 协程退出 —— 表现为「刚建的会话立刻消失」。
func TestLastActiveNeverZeroTime(t *testing.T) {
	r := newTestRuntime(t, config.Default(), nil)

	// newTestRuntime 手工构造，不经过赋值路径，lastActiveNano 此时是 0。
	got := r.LastActive()
	if got.IsZero() {
		t.Fatal("LastActive 返回了零值时间，会让 Hub 误回收")
	}
	if time.Since(got) > time.Minute {
		t.Errorf("LastActive = %v，期望接近当前时间", got)
	}
}

// TestMessageShape 逐条对照参考实现 chat/debounce/message_adapter.py:get_message_shape。
//
// 防的是本轮发现的静默失效：早期用 PlainText 判空，
// 而 PlainText 永远带 [图片]/[表情包] 占位、恒不为空，
// 于是 isBareImage / isPureEmoji 恒为 false，
// 「裸图开批至少 4 秒」「纯表情包不延长等待」两条防抖规则从未生效过。
func TestMessageShape(t *testing.T) {
	cases := []struct {
		name     string
		segments []domain.Segment
		wantKind int
		wantLen  int
	}{
		{
			name:     "纯文本",
			segments: []domain.Segment{domain.Text("你好")},
			wantKind: 0, wantLen: 2,
		},
		{
			name:     "只有图",
			segments: []domain.Segment{{Type: domain.SegmentImage, Binary: []byte("x")}},
			wantKind: 1, wantLen: 0,
		},
		{
			name:     "图 + 文字（不算裸图）",
			segments: []domain.Segment{{Type: domain.SegmentImage}, domain.Text("你看")},
			wantKind: 1, wantLen: 2,
		},
		{
			name:     "只有表情包",
			segments: []domain.Segment{{Type: domain.SegmentEmoji, Binary: []byte("x")}},
			wantKind: 2, wantLen: 0,
		},
		{
			name:     "表情包 + 文字（不算纯表情）",
			segments: []domain.Segment{{Type: domain.SegmentEmoji}, domain.Text("笑死")},
			wantKind: 2, wantLen: 2,
		},
		{
			name:     "图和表情包同时存在时图优先",
			segments: []domain.Segment{{Type: domain.SegmentEmoji}, {Type: domain.SegmentImage}},
			wantKind: 1, wantLen: 0,
		},
		{
			name:     "语音",
			segments: []domain.Segment{{Type: domain.SegmentVoice, Text: "[语音]"}},
			wantKind: 3, wantLen: 0,
		},
		{
			name:     "文件",
			segments: []domain.Segment{{Type: domain.SegmentFile, Text: "合同.pdf"}},
			wantKind: 3, wantLen: 0,
		},
		{
			name:     "空段列表",
			segments: nil,
			wantKind: 0, wantLen: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, length := messageShape(&domain.Message{Segments: tc.segments})
			if kind != tc.wantKind {
				t.Errorf("kind = %d，期望 %d", kind, tc.wantKind)
			}
			if length != tc.wantLen {
				t.Errorf("length = %d，期望 %d（只统计文本段）", length, tc.wantLen)
			}
		})
	}

	// nil 消息不能 panic：恢复上下文时可能出现。
	if kind, length := messageShape(nil); kind != 0 || length != 0 {
		t.Errorf("nil 消息应返回 (0,0)，实际 (%d,%d)", kind, length)
	}
}

// TestIsBareImageAndPureEmoji 验证两个判定真的会为 true。
//
// 关键点：PlainText 有值（"[图片]"/"[表情包]"）时仍然要判定成立 ——
// 参考实现统计的是 TextComponent 的长度，不是渲染后的占位文本。
func TestIsBareImageAndPureEmoji(t *testing.T) {
	r := &Runtime{}

	bareImage := &domain.Message{
		PlainText: "[图片]",
		Segments:  []domain.Segment{{Type: domain.SegmentImage, Binary: []byte("x")}},
	}
	if !r.isBareImage(bareImage) {
		t.Error("只有一张图时应判定为裸图")
	}
	if r.isPureEmoji(bareImage) {
		t.Error("图片不该被判定为纯表情包")
	}

	pureEmoji := &domain.Message{
		PlainText: "[表情包]",
		Segments:  []domain.Segment{{Type: domain.SegmentEmoji, Binary: []byte("x")}},
	}
	if !r.isPureEmoji(pureEmoji) {
		t.Error("只有表情包时应判定为纯表情包")
	}
	if r.isBareImage(pureEmoji) {
		t.Error("表情包不该被判定为裸图")
	}

	withText := &domain.Message{
		PlainText: "你看",
		Segments:  []domain.Segment{domain.Text("你看"), {Type: domain.SegmentImage}},
	}
	if r.isBareImage(withText) || r.isPureEmoji(withText) {
		t.Error("带文字时两个判定都应为 false")
	}
}

// TestEmbeddingModelInfoPrefersMeasuredDimension 验证维度口径：实测优先（P1-4）。
//
// 旧口径优先用配置里的 dimension —— 端点忽略 dimensions 参数时
// （很多中转站如此），库里写的是实测维度、检索却按配置维度过滤，
// 候选集恒空且无任何日志。现在客户端成功调用过一次就用实测值，
// 配置维度只是「发给端点的请求参数」与「还没调用过时的初值」。
func TestEmbeddingModelInfoPrefersMeasuredDimension(t *testing.T) {
	// 假端点固定返回 4 维向量（无视任何 dimensions 请求参数）。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "emb",
			"data": []map[string]any{
				{"index": 0, "embedding": []float32{1, 0, 0, 0}},
			},
		})
	}))
	t.Cleanup(srv.Close)

	cfg := config.Default()
	cfg.LLM.Providers = []config.ProviderConfig{{Name: "p", BaseURL: srv.URL, APIKey: "k", Timeout: 5}}
	cfg.LLM.Models = []config.ModelConfig{{
		Name: "e", ModelIdentifier: "emb", Provider: "p", // 故意不写 dimension
	}}
	cfg.LLM.Tasks.Embedding.Models = []string{"e"}

	r := newTestRuntime(t, cfg, nil)
	r.embedding = embedding.New(r.cfg)

	// 还没成功调用过：维度未知 → 报错（调用方据此降级关键词）。
	if _, _, err := r.embeddingModelInfo(); err == nil {
		t.Error("维度未知时应报错")
	}

	// 一次成功调用后，实测维度 = 4。
	if _, err := r.embedding.Embed(context.Background(), []string{"x"}); err != nil {
		t.Fatalf("向量化失败: %v", err)
	}
	_, dim, err := r.embeddingModelInfo()
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if dim != 4 {
		t.Fatalf("实测维度应优先，得 %d，期望 4", dim)
	}

	// 用户事后把配置维度写成 1024（写错/端点行为变化）：
	// 检索仍用实测 4，不能被配置值带偏。
	setConfig(t, r.cfg, func(c *config.Config) {
		wrong := 1024
		c.LLM.Models[0].Dimension = &wrong
	})
	_, dim, err = r.embeddingModelInfo()
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if dim != 4 {
		t.Errorf("配置维度 %d 不应覆盖实测维度 4", dim)
	}
}

// TestUnpackCandidatesCountsFailures 验证损坏 BLOB 的计数（P2-17）。
//
// 检索路径靠 failed>0 触发 Warn；以前静默跳过，
// 表现为「某些记忆永远检不到」且没有任何线索。
func TestUnpackCandidatesCountsFailures(t *testing.T) {
	candidates := []*store.MemorySummary{
		{ID: 1, Embedding: embedding.Pack([]float32{1, 0})},
		{ID: 2, Embedding: []byte{1, 2, 3}}, // 长度不是 4 的倍数 → 损坏
		{ID: 3, Embedding: nil},             // 空
	}
	flat, failed := unpackCandidates(candidates)
	if failed != 2 {
		t.Errorf("解包失败数 = %d，期望 2", failed)
	}
	if len(flat) != 3 || flat[0] == nil || flat[1] != nil || flat[2] != nil {
		t.Errorf("向量集落位不对: %v", flat)
	}
}
