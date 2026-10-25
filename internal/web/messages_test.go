package web

// 本文件覆盖 handleMessages 的分页协议与 handleOverview 的聚合接口。
//
// 分页与概览都是「前端一开工就必须有」的东西：
// 没有分页翻不了历史，没有概览首页要发 8 个请求。

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/store"
)

// TestMessagesPagination 守的是游标式分页的三条语义：
// 默认取最新、before_ms 往前翻、after_ms 增量拉，且游标处不重复。
func TestMessagesPagination(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	seedData(t, env.db)
	env.loginOK()

	// 用一个独立会话：seedData 已经在群会话里播了消息，
	// 混在一起就没法断言「最新一条应该是第 9 条」。
	const pageSession = "sess-pagination-0001"

	// 造 10 条时间递增的消息。
	ctx := context.Background()
	base := time.Now().Add(-10 * time.Minute).Truncate(time.Millisecond)
	for i := 0; i < 10; i++ {
		m := &domain.Message{
			ID:         "pg-" + itoa64(int64(i)),
			SessionID:  pageSession,
			MessageID:  "pm-pg-" + itoa64(int64(i)),
			Platform:   "qq",
			UserID:     "u1",
			UserName:   "小明",
			IsGroup:    true,
			Segments:   []domain.Segment{domain.Text("第 " + itoa64(int64(i)) + " 条")},
			PlainText:  "第 " + itoa64(int64(i)) + " 条",
			ReceivedAt: base.Add(time.Duration(i) * time.Second),
		}
		if err := env.db.Messages().Save(ctx, m); err != nil {
			t.Fatalf("播种失败: %v", err)
		}
	}

	type resp struct {
		Messages []messageRow `json:"messages"`
		OldestMS int64        `json:"oldest_ms"`
		NewestMS int64        `json:"newest_ms"`
		HasMore  bool         `json:"has_more"`
	}

	// 默认：最新 5 条，时间正序。
	_, body := env.do(http.MethodGet, "/api/messages?session_id="+pageSession+"&limit=5", "")
	var page1 resp
	if err := json.Unmarshal(body, &page1); err != nil {
		t.Fatalf("解析失败: %v，响应: %s", err, body)
	}
	if len(page1.Messages) != 5 {
		t.Fatalf("返回 %d 条，期望 5", len(page1.Messages))
	}
	if page1.Messages[4].Text != "第 9 条" {
		t.Errorf("最新一条 = %q，期望「第 9 条」", page1.Messages[4].Text)
	}
	// 时间必须正序：最老的在前面。
	for i := 1; i < len(page1.Messages); i++ {
		if page1.Messages[i].CreatedMS < page1.Messages[i-1].CreatedMS {
			t.Fatalf("时间不是正序: %d < %d", page1.Messages[i].CreatedMS, page1.Messages[i-1].CreatedMS)
		}
	}

	// 用最早一条的时间戳当游标往前翻。
	_, body = env.do(http.MethodGet,
		"/api/messages?session_id="+pageSession+"&limit=5&before_ms="+itoa64(page1.OldestMS), "")
	var page2 resp
	if err := json.Unmarshal(body, &page2); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(page2.Messages) != 5 {
		t.Fatalf("上一页 %d 条，期望 5", len(page2.Messages))
	}
	// 游标处那条不能重复出现。
	for _, m := range page2.Messages {
		if m.CreatedMS == page1.OldestMS {
			t.Error("游标消息被重复返回，翻页会看到重复行")
		}
	}
	if page2.NewestMS >= page1.OldestMS {
		t.Errorf("上一页的最新时间 %d 应早于本页最早时间 %d", page2.NewestMS, page1.OldestMS)
	}

	// after_ms 增量拉：只拿比游标新的。
	_, body = env.do(http.MethodGet,
		"/api/messages?session_id="+pageSession+"&after_ms="+itoa64(page1.OldestMS), "")
	var inc resp
	if err := json.Unmarshal(body, &inc); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(inc.Messages) != 4 {
		t.Errorf("增量返回 %d 条，期望 4（最新 5 条里比游标新的）", len(inc.Messages))
	}
	for _, m := range inc.Messages {
		if m.CreatedMS <= page1.OldestMS {
			t.Errorf("增量里出现了不比游标新的消息: %d", m.CreatedMS)
		}
	}
}

// TestMessagesReturnsSegments 守的是消息段真的返回给前端。
//
// 少了这个，前端只能显示一行纯文本：图片全变 [图片] 占位，
// @ 与引用完全不可见。
func TestMessagesReturnsSegments(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	seedData(t, env.db)
	env.loginOK()

	ctx := context.Background()
	m := &domain.Message{
		ID:        "seg-1",
		SessionID: groupSessionID,
		MessageID: "pm-seg-1",
		Platform:  "qq",
		UserID:    "u1",
		UserName:  "小明",
		IsGroup:   true,
		Segments: []domain.Segment{
			domain.Text("看图"),
			{Type: domain.SegmentImage, URL: "https://example.invalid/a.jpg"},
			domain.At("u2", "小红"),
			domain.Reply("pm-old"),
		},
		PlainText:    "看图",
		ReceivedAt:   time.Now(),
		UserCardName: "老王",
		Additional:   map[string]string{"reply_to": "pm-old"},
	}
	if err := env.db.Messages().Save(ctx, m); err != nil {
		t.Fatalf("播种失败: %v", err)
	}

	_, body := env.do(http.MethodGet, "/api/messages?session_id="+groupSessionID+"&limit=50", "")
	var out struct {
		Messages []messageRow `json:"messages"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	var found *messageRow
	for i := range out.Messages {
		if out.Messages[i].ID == "pm-seg-1" {
			found = &out.Messages[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("没找到刚写入的消息，响应: %s", body)
	}

	if len(found.Segments) != 4 {
		t.Fatalf("消息段 %d 个，期望 4: %+v", len(found.Segments), found.Segments)
	}
	if !found.Segments[1].HasMedia {
		t.Error("图片段的 has_media 应为 true")
	}
	if found.Segments[2].UserName != "小红" {
		t.Errorf("@ 段目标 = %q", found.Segments[2].UserName)
	}
	if found.Segments[3].MessageID != "pm-old" {
		t.Errorf("引用段 ID = %q", found.Segments[3].MessageID)
	}
	if found.UserCard != "老王" {
		t.Errorf("群名片 = %q，期望「老王」", found.UserCard)
	}
	if found.ReplyTo != "pm-old" {
		t.Errorf("reply_to = %q", found.ReplyTo)
	}
}

// TestMessagesMissingSessionID 守的是缺参数时报 400 而不是 500 或空数组。
func TestMessagesMissingSessionID(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	env.loginOK()

	resp, _ := env.do(http.MethodGet, "/api/messages", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("状态码 = %d，期望 400", resp.StatusCode)
	}
}

// TestOverviewAPI 守的是首页聚合接口。
func TestOverviewAPI(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	seedData(t, env.db)
	env.loginOK()

	resp, body := env.do(http.MethodGet, "/api/overview", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，响应: %s", resp.StatusCode, body)
	}

	var out struct {
		Bot struct {
			Nickname string `json:"nickname"`
		} `json:"bot"`
		Runtime struct {
			ActiveChats  int `json:"active_chats"`
			UptimeSecods int `json:"uptime_seconds"`
		} `json:"runtime"`
		Counts struct {
			Messages int64 `json:"messages"`
			Sessions int64 `json:"sessions"`
			Persons  int64 `json:"persons"`
			MsgToday int64 `json:"messages_today"`
		} `json:"counts"`
		Features map[string]bool  `json:"features"`
		Daily    []DailyCountJSON `json:"daily"`
		Errors   []overviewError  `json:"errors"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("解析失败: %v，响应: %s", err, body)
	}

	if out.Bot.Nickname == "" {
		t.Error("缺少 bot.nickname")
	}
	if out.Counts.Messages == 0 || out.Counts.Sessions != 2 {
		t.Errorf("计数不对: %+v", out.Counts)
	}
	if out.Counts.MsgToday == 0 {
		t.Error("今日消息数应为正：播种的消息都在今天")
	}
	if out.Features == nil {
		t.Error("缺少 features 字段")
	}
	if out.Daily == nil {
		t.Error("daily 应为数组而不是 null，否则前端 forEach 抛错")
	}
	for _, e := range out.Errors {
		t.Errorf("概览报错 %s: %s", e.Section, e.Message)
	}
}

// DailyCountJSON 只是为了让上面的解析能拿到字段。
type DailyCountJSON struct {
	Date     string `json:"date"`
	Total    int64  `json:"total"`
	Inbound  int64  `json:"inbound"`
	Outbound int64  `json:"outbound"`
}

// TestOverviewRequiresAuth 守的是新接口没忘记鉴权。
func TestOverviewRequiresAuth(t *testing.T) {
	env := newTestEnv(t, envOptions{})

	resp, _ := env.doWithCookie(http.MethodGet, "/api/overview", "", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("状态码 = %d，期望 401", resp.StatusCode)
	}
}

// TestMessagesTextExcludesPlaceholders 防：正文里混进占位导致重复显示。
//
// 前端拿 messageRow.Text 当正文渲染，同时又把 at 段与媒体段单独渲染。
// 如果 Text 用的是 BuildPlainText（它会把 at 渲染成「@名字」、
// 媒体渲染成「[图片]」），界面上就会变成：
//
//	[@麦麦] @麦麦 你好          ← @ 重复
//	[图片]                      ← 纯图片消息凭空多出一行字
//
// 这是真实看到的现象，所以 Text 现在只拼 text 段。
func TestMessagesTextExcludesPlaceholders(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	seedData(t, env.db)
	env.loginOK()

	ctx := context.Background()
	msgs := []*domain.Message{
		{
			ID:        "ph-1",
			SessionID: groupSessionID,
			MessageID: "pm-ph-1",
			Platform:  "qq",
			UserID:    "u1",
			UserName:  "小明",
			IsGroup:   true,
			Segments: []domain.Segment{
				domain.At("u2", "小红"),
				domain.Text(" 你好"),
			},
			PlainText:  "@小红 你好",
			ReceivedAt: time.Now(),
		},
		{
			ID:        "ph-2",
			SessionID: groupSessionID,
			MessageID: "pm-ph-2",
			Platform:  "qq",
			UserID:    "u1",
			UserName:  "小明",
			IsGroup:   true,
			Segments: []domain.Segment{
				{Type: domain.SegmentImage, URL: "https://example.invalid/a.jpg"},
			},
			PlainText:  "[图片]",
			ReceivedAt: time.Now().Add(time.Second),
		},
	}
	for _, m := range msgs {
		if err := env.db.Messages().Save(ctx, m); err != nil {
			t.Fatalf("播种失败: %v", err)
		}
	}

	_, body := env.do(http.MethodGet, "/api/messages?session_id="+groupSessionID+"&limit=50", "")
	var out struct {
		Messages []messageRow `json:"messages"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	byID := map[string]messageRow{}
	for _, m := range out.Messages {
		byID[m.ID] = m
	}

	// @ + 文字：Text 只能是「你好」，不能带 @名字。
	if got := byID["pm-ph-1"].Text; got != "你好" {
		t.Errorf("at+文本的 Text = %q，期望 %q（不含 @ 占位）", got, "你好")
	}
	// PlainText 仍然要保留完整渲染，这是另一条独立的契约。
	if got := byID["pm-ph-1"].PlainText; got != "@小红 你好" {
		t.Errorf("PlainText = %q，期望保留完整渲染", got)
	}

	// 纯图片：Text 必须为空，否则前端会在气泡里多画一行「[图片]」。
	if got := byID["pm-ph-2"].Text; got != "" {
		t.Errorf("纯图片消息的 Text = %q，期望空串", got)
	}
	if got := byID["pm-ph-2"].PlainText; got != "[图片]" {
		t.Errorf("PlainText = %q，期望 [图片]", got)
	}
}

// TestSegmentRowTextFallsBackToEmpty 守住没有 text 段时的边界。
func TestSegmentRowTextFallsBackToEmpty(t *testing.T) {
	cases := []struct {
		name     string
		segments []domain.Segment
		want     string
	}{
		{"nil", nil, ""},
		{"只有 at", []domain.Segment{domain.At("u1", "小明")}, ""},
		{"只有媒体", []domain.Segment{{Type: domain.SegmentImage}}, ""},
		{"只有空白文本", []domain.Segment{domain.Text("   ")}, ""},
		{"多段文本拼接", []domain.Segment{domain.Text("甲"), domain.Text("乙")}, "甲 乙"},
		{"at 与文本混合", []domain.Segment{domain.At("u1", "小明"), domain.Text(" 你好")}, "你好"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := segmentRowText(c.segments); got != c.want {
				t.Errorf("segmentRowText = %q，期望 %q", got, c.want)
			}
		})
	}
}

// TestMessagesReplyPreview 防：引用只显示一个截断的 ROBOT1…4kM! 长 ID。
//
// 那对看聊天记录的人毫无信息量，而「引用了什么」正是引用的意义。
// 后端批量查出被引消息并渲染成「谁: 内容」。
func TestMessagesReplyPreview(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	seedData(t, env.db)
	env.loginOK()

	ctx := context.Background()
	base := time.Now().Add(-time.Minute)

	// 被引用的原消息。
	target := &domain.Message{
		ID:         "rp-target",
		SessionID:  groupSessionID,
		MessageID:  "pm-rp-target",
		Platform:   "qq",
		UserID:     "u2",
		UserName:   "小红",
		IsGroup:    true,
		Segments:   []domain.Segment{domain.Text("原话在这里")},
		PlainText:  "原话在这里",
		ReceivedAt: base,
	}
	if err := env.db.Messages().Save(ctx, target); err != nil {
		t.Fatalf("播种原消息失败: %v", err)
	}

	// 引用它的那条。
	quoter := &domain.Message{
		ID:         "rp-quoter",
		SessionID:  groupSessionID,
		MessageID:  "pm-rp-quoter",
		Platform:   "qq",
		UserID:     "u1",
		UserName:   "小明",
		IsGroup:    true,
		Segments:   []domain.Segment{domain.Text("接一句")},
		PlainText:  "接一句",
		ReceivedAt: base.Add(10 * time.Second),
		Additional: map[string]string{"reply_to": "pm-rp-target"},
	}
	if err := env.db.Messages().Save(ctx, quoter); err != nil {
		t.Fatalf("播种引用消息失败: %v", err)
	}

	_, body := env.do(http.MethodGet, "/api/messages?session_id="+groupSessionID+"&limit=50", "")
	var out struct {
		Messages []messageRow `json:"messages"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	var found *messageRow
	for i := range out.Messages {
		if out.Messages[i].ID == "pm-rp-quoter" {
			found = &out.Messages[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("没找到引用消息，响应: %s", body)
	}
	if !strings.Contains(found.ReplyPreview, "小红") {
		t.Errorf("引用预览 = %q，应包含被引消息的发送者", found.ReplyPreview)
	}
	if !strings.Contains(found.ReplyPreview, "原话在这里") {
		t.Errorf("引用预览 = %q，应包含被引消息的正文", found.ReplyPreview)
	}
}

// TestReplyPreviewOfTruncates 守住超长引用的截断（按 rune，不劈汉字）。
func TestReplyPreviewOfTruncates(t *testing.T) {
	long := strings.Repeat("汉", 200)
	m := &domain.Message{
		UserName: "某人",
		Segments: []domain.Segment{domain.Text(long)},
	}
	got := replyPreviewOf(m)
	if runes := []rune(got); len(runes) > 80 {
		t.Errorf("预览长度 %d runes，未截断", len(runes))
	}
	if !strings.Contains(got, "…") {
		t.Errorf("预览 = %q，应带省略号", got)
	}

	// 纯媒体消息没有 text 段，应退回占位而不是空串。
	media := &domain.Message{
		UserName: "某人",
		Segments: []domain.Segment{{Type: domain.SegmentImage}},
	}
	if got := replyPreviewOf(media); got == "" || got == "某人: " {
		t.Errorf("纯媒体被引消息的预览 = %q，不该是空内容", got)
	}
}

// TestMessagesSegmentSrcWhenHashExists 防：表情包显示成「[表情]」文字。
//
// 二进制不落库（见 store.MessageRepo.Save），所以图片要靠段上的 hash
// 回查 data/emoji 里采集时落盘的文件。只有 hash 在库里存在时才给 src，
// 否则前端会拿到一个 404 的图片地址、显示成碎图标。
func TestMessagesSegmentSrcWhenHashExists(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	seedData(t, env.db)
	env.loginOK()

	ctx := context.Background()
	const knownHash = "aaaa1111bbbb2222cccc3333dddd4444eeee5555ffff6666aaaa7777bbbb8888"
	const unknownHash = "1111222233334444555566667777888899990000aaaabbbbccccddddeeeeffff"

	if _, err := env.db.Emojis().Save(ctx, &store.Emoji{
		FileHash:  knownHash,
		FilePath:  "data/emoji/aa/" + knownHash + ".png",
		CreatedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("播种表情包失败: %v", err)
	}

	m := &domain.Message{
		ID:        "media-1",
		SessionID: groupSessionID,
		MessageID: "pm-media-1",
		Platform:  "qq",
		UserID:    "u1",
		UserName:  "小明",
		IsGroup:   true,
		Segments: []domain.Segment{
			{Type: domain.SegmentEmoji, MimeType: "image/gif", Hash: knownHash},
			{Type: domain.SegmentEmoji, MimeType: "image/gif", Hash: unknownHash},
			{Type: domain.SegmentEmoji, MimeType: "image/gif"}, // 老数据：没有 hash
		},
		PlainText:  "[表情包] [表情包] [表情包]",
		ReceivedAt: time.Now(),
	}
	if err := env.db.Messages().Save(ctx, m); err != nil {
		t.Fatalf("播种消息失败: %v", err)
	}

	_, body := env.do(http.MethodGet, "/api/messages?session_id="+groupSessionID+"&limit=50", "")
	var out struct {
		Messages []messageRow `json:"messages"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	var found *messageRow
	for i := range out.Messages {
		if out.Messages[i].ID == "pm-media-1" {
			found = &out.Messages[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("没找到刚写入的消息，响应: %s", body)
	}

	if got := found.Segments[0].Src; !strings.Contains(got, knownHash) {
		t.Errorf("库里有文件的段 Src = %q，应指向 /api/media?hash=...", got)
	}
	if got := found.Segments[1].Src; got != "" {
		t.Errorf("库里没有的 hash 不该给 Src，实际 %q（前端会显示碎图标）", got)
	}
	if got := found.Segments[2].Src; got != "" {
		t.Errorf("没 hash 的老数据不该给 Src，实际 %q", got)
	}
	// 三段都要保留 has_media，前端据此知道「这里本来有个媒体」。
	for i, seg := range found.Segments {
		if !seg.HasMedia {
			t.Errorf("第 %d 段 has_media 应为 true", i)
		}
	}
}

// TestMessagesTimelineMergesMessagesAndReasoning 防：思考与消息分成两页。
//
// 参考实现用的是**一本主事件账本**（dashboard 的 TimelineEntry）：
// 消息事件与推理事件都按时间戳插进同一条流，前端只管顺序渲染。
// 分开两个接口的话前端要自己归并，而它不知道「这段思考属于哪几条消息之间」。
func TestMessagesTimelineMergesMessagesAndReasoning(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	seedData(t, env.db)
	env.loginOK()

	ctx := context.Background()
	base := time.Now().Add(-time.Minute).Truncate(time.Millisecond)

	// 消息 1 → 思考 → 消息 2，验证思考插在两者之间。
	mk := func(id string, offset time.Duration) *domain.Message {
		return &domain.Message{
			ID: id, SessionID: groupSessionID, MessageID: id, Platform: "qq",
			UserID: "u1", UserName: "小明", IsGroup: true,
			Segments:   []domain.Segment{domain.Text(id)},
			PlainText:  id,
			ReceivedAt: base.Add(offset),
		}
	}
	if err := env.db.Messages().Save(ctx, mk("tl-msg-1", 0)); err != nil {
		t.Fatalf("播种失败: %v", err)
	}
	if err := env.db.Messages().Save(ctx, mk("tl-msg-2", 10*time.Second)); err != nil {
		t.Fatalf("播种失败: %v", err)
	}

	// 思考落在两条消息之间。
	if err := env.db.Reasoning().Save(ctx, &store.ReasoningRecord{
		SessionID:   groupSessionID,
		TurnID:      "turn-tl",
		Round:       1,
		Kind:        "planner",
		Request:     `{"messages":[]}`,
		Response:    `{"reasoning":"想想怎么回","tool_calls":[{"function":{"name":"reply"}}]}`,
		CreatedAt:   base.Add(5 * time.Second).Unix(),
		CreatedAtMS: base.Add(5 * time.Second).UnixMilli(),
	}); err != nil {
		t.Fatalf("播种思考失败: %v", err)
	}

	_, body := env.do(http.MethodGet, "/api/messages?session_id="+groupSessionID+"&limit=50", "")
	var out struct {
		Messages []messageRow    `json:"messages"`
		Timeline []timelineEntry `json:"timeline"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("解析失败: %v: %s", err, body)
	}

	if len(out.Timeline) == 0 {
		t.Fatalf("响应里没有 timeline，前端只能分页显示两堆数据: %s", body)
	}

	// 找到我们播的两条消息与那一轮思考的相对位置。
	var idxMsg1, idxReasoning, idxMsg2 = -1, -1, -1
	for i, e := range out.Timeline {
		switch {
		case e.Kind == "message" && e.Message != nil && e.Message.ID == "tl-msg-1":
			idxMsg1 = i
		case e.Kind == "message" && e.Message != nil && e.Message.ID == "tl-msg-2":
			idxMsg2 = i
		case e.Kind == "reasoning" && e.Reasoning != nil && e.Reasoning.TurnID == "turn-tl":
			idxReasoning = i
		}
	}
	if idxMsg1 < 0 || idxMsg2 < 0 || idxReasoning < 0 {
		t.Fatalf("时间线里缺项: msg1=%d reasoning=%d msg2=%d", idxMsg1, idxReasoning, idxMsg2)
	}
	if !(idxMsg1 < idxReasoning && idxReasoning < idxMsg2) {
		t.Errorf("思考应插在两条消息之间，实际顺序 msg1=%d reasoning=%d msg2=%d",
			idxMsg1, idxReasoning, idxMsg2)
	}

	// 一轮里的多个步骤应该合成一项，而不是散开。
	if got := len(out.Timeline[idxReasoning].Reasoning.Steps); got != 1 {
		t.Errorf("本轮步骤数 = %d，期望 1", got)
	}

	// 时间线必须整体按 At 升序。
	for i := 1; i < len(out.Timeline); i++ {
		if out.Timeline[i].At < out.Timeline[i-1].At {
			t.Errorf("时间线未按时间升序: [%d].At=%d < [%d].At=%d",
				i, out.Timeline[i].At, i-1, out.Timeline[i-1].At)
		}
	}
}

// TestBuildTimelineMessagesBeforeReasoningAtSameMS 守同一毫秒的排序规则。
//
// 先收到消息才可能开始思考，反过来不符合因果。
func TestBuildTimelineMessagesBeforeReasoningAtSameMS(t *testing.T) {
	const at = int64(1000)
	msgs := []messageRow{{ID: "m1", CreatedMS: at}}
	recs := []store.ReasoningRecord{{
		TurnID: "t1", Round: 1, CreatedAtMS: at, Response: `{}`,
	}}

	got := buildTimeline(msgs, recs)
	if len(got) != 2 {
		t.Fatalf("条目数 = %d，期望 2", len(got))
	}
	if got[0].Kind != "message" {
		t.Errorf("同一毫秒时消息应在前，实际第一项是 %q", got[0].Kind)
	}
}

// TestMessagesTimelineHasAllEventKinds 守「时间线上要有 5 类事件」。
//
// 参考实现的主事件账本里同时有消息、Planner 请求、工具执行、
// Replyer 响应、门控结果、上下文裁切（maisaka-monitor.tsx:2273 的 switch）。
// 我们原来只记了 Planner 一种 —— 界面上只有「模型想了什么」，
// 看不到「工具实际做了什么」「为什么这次没回复」。
func TestMessagesTimelineHasAllEventKinds(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	seedData(t, env.db)
	env.loginOK()

	ctx := context.Background()
	base := time.Now().Add(-2 * time.Minute).Truncate(time.Millisecond)

	if err := env.db.Messages().Save(ctx, &domain.Message{
		ID: "ak-msg", SessionID: groupSessionID, MessageID: "ak-msg", Platform: "qq",
		UserID: "u1", UserName: "小明", IsGroup: true,
		Segments:   []domain.Segment{domain.Text("在吗")},
		PlainText:  "在吗",
		ReceivedAt: base,
	}); err != nil {
		t.Fatalf("播种消息失败: %v", err)
	}

	save := func(rec *store.ReasoningRecord) {
		t.Helper()
		rec.SessionID = groupSessionID
		rec.CreatedAt = rec.CreatedAtMS / 1000
		if err := env.db.Reasoning().Save(ctx, rec); err != nil {
			t.Fatalf("播种 %s 事件失败: %v", rec.Kind, err)
		}
	}

	save(&store.ReasoningRecord{
		TurnID: "turn-ak", Round: 1, Kind: "planner", Name: "planner",
		Request:     `{"tools":[{"x":1}],"messages":[{},{}]}`,
		Response:    `{"model":"glm","content":"想回复","tool_calls":[{"function":{"name":"reply"}}]}`,
		CreatedAtMS: base.Add(1 * time.Second).UnixMilli(),
	})
	save(&store.ReasoningRecord{
		TurnID: "turn-ak", Round: 1, Kind: "tool", Name: "reply",
		Payload:     `{"args":{"msg_id":"m1"},"result":"已发送回复"}`,
		CreatedAtMS: base.Add(2 * time.Second).UnixMilli(),
	})
	save(&store.ReasoningRecord{
		TurnID: "turn-ak", Round: 1, Kind: "replyer", Name: "replyer",
		Payload:     `{"content":"嗨呀","model":"flash","total_tokens":42}`,
		CreatedAtMS: base.Add(3 * time.Second).UnixMilli(),
	})
	save(&store.ReasoningRecord{
		Kind: "gate", Name: "continue",
		Payload:     `{"action":"continue","detail":"频率够","pending_count":1}`,
		CreatedAtMS: base.Add(4 * time.Second).UnixMilli(),
	})
	save(&store.ReasoningRecord{
		Kind: "trim", Name: "context.trimmed",
		Payload:     `{"removed":10,"keep":40}`,
		CreatedAtMS: base.Add(5 * time.Second).UnixMilli(),
	})

	_, body := env.do(http.MethodGet, "/api/messages?session_id="+groupSessionID+"&limit=50", "")
	var out struct {
		Timeline []timelineEntry `json:"timeline"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("解析失败: %v: %s", err, body)
	}

	var sawGate, sawTrim bool
	var turn *timelineTurn
	for i := range out.Timeline {
		e := out.Timeline[i]
		switch e.Kind {
		case "gate":
			sawGate = true
			if e.Gate == nil || e.Gate.Action != "continue" {
				t.Errorf("门控事件没解析出来: %+v", e.Gate)
			} else if e.Gate.PendingCount != 1 {
				t.Errorf("门控 pending_count = %d，期望 1", e.Gate.PendingCount)
			}
		case "trim":
			sawTrim = true
			if e.Trim == nil || e.Trim.Removed != 10 || e.Trim.Keep != 40 {
				t.Errorf("裁切事件没解析出来: %+v", e.Trim)
			}
		case "reasoning":
			if e.Reasoning != nil && e.Reasoning.TurnID == "turn-ak" {
				turn = e.Reasoning
			}
		}
	}
	if !sawGate {
		t.Error("时间线里没有门控事件 —— 用户问「为什么没回复」时无从查起")
	}
	if !sawTrim {
		t.Error("时间线里没有上下文裁切事件")
	}
	if turn == nil {
		t.Fatal("时间线里没有那一轮思考")
	}

	// 一轮里的三类步骤都要在，且按时间正序。
	types := make([]string, 0, len(turn.Steps))
	for _, st := range turn.Steps {
		types = append(types, st.Type)
	}
	want := []string{"planner", "tool", "replyer"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Errorf("步骤顺序 = %v，期望 %v", types, want)
	}

	// 每类步骤的专有字段都要解析出来，不能原样甩 JSON 给前端。
	for _, st := range turn.Steps {
		switch st.Type {
		case "planner":
			if st.Model != "glm" || st.Content != "想回复" {
				t.Errorf("planner 步骤解析不对: %+v", st)
			}
			if st.ToolCount != 1 || st.History != 2 {
				t.Errorf("planner 的工具数/文档数 = %d/%d，期望 1/2", st.ToolCount, st.History)
			}
		case "tool":
			if st.Name != "reply" || st.Result != "已发送回复" {
				t.Errorf("tool 步骤解析不对: %+v", st)
			}
			if !strings.Contains(st.Args, "msg_id") {
				t.Errorf("tool 参数没解析出来: %q", st.Args)
			}
		case "replyer":
			if st.Content != "嗨呀" || st.TotalTokens != 42 {
				t.Errorf("replyer 步骤解析不对: %+v", st)
			}
		}
	}
}

// TestMessagesFirstPageHasMore 守的是首屏也返回 has_more。
//
// 之前只在带游标（before_ms / after_ms）时返回 has_more，
// 首屏读到 undefined → 前端永远不渲染「加载更早的消息」按钮 →
// 超过一屏的会话历史完全翻不上去。而首屏恰恰最需要那个按钮。
func TestMessagesFirstPageHasMore(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	env.loginOK()

	const sess = "sess-hasmore-0001"
	ctx := context.Background()
	base := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	for i := 0; i < 12; i++ {
		m := &domain.Message{
			ID:         "hm-" + itoa64(int64(i)),
			SessionID:  sess,
			MessageID:  "pm-hm-" + itoa64(int64(i)),
			Platform:   "qq",
			UserID:     "u1",
			UserName:   "小明",
			Segments:   []domain.Segment{domain.Text("第 " + itoa64(int64(i)) + " 条")},
			PlainText:  "第 " + itoa64(int64(i)) + " 条",
			ReceivedAt: base.Add(time.Duration(i) * time.Second),
		}
		if err := env.db.Messages().Save(ctx, m); err != nil {
			t.Fatalf("播种失败: %v", err)
		}
	}

	// 1. 首屏（无游标）取 5 条，库里还有更多 → has_more 必须是 true。
	_, body := env.do(http.MethodGet, "/api/messages?session_id="+sess+"&limit=5", "")
	var first struct {
		Messages []messageRow `json:"messages"`
		HasMore  *bool        `json:"has_more"`
	}
	if err := json.Unmarshal(body, &first); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if first.HasMore == nil {
		t.Fatal("首屏没有返回 has_more，前端无法判断要不要显示「加载更早的消息」")
	}
	if !*first.HasMore {
		t.Error("库里还有 7 条，首屏 has_more 应为 true")
	}

	// 2. 一次取完（limit 足够大）→ has_more 必须是 false。
	_, body = env.do(http.MethodGet, "/api/messages?session_id="+sess+"&limit=50", "")
	var all struct {
		Messages []messageRow `json:"messages"`
		HasMore  *bool        `json:"has_more"`
	}
	if err := json.Unmarshal(body, &all); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if all.HasMore == nil || *all.HasMore {
		t.Error("一次取完全部消息时 has_more 应为 false")
	}
}

// TestReplyPreviewOfUsesStoredSenderName 防：机器人改名后引用预览仍显示「木木」。
//
// 出站消息落库时 UserName 已经填成了当次的 botName（agent/outbound.go），
// 所以预览只需要用 m.UserName。曾经这里写的是 `if m.IsSelf { name = "木木" }`，
// 后果是用户把 bot.nickname 改成别的之后，界面上看得见但永远不改——
// 不报错、不丢数据，只是那一行字永远是错的。
func TestReplyPreviewOfUsesStoredSenderName(t *testing.T) {
	m := &domain.Message{
		IsSelf:   true,
		UserName: "肥鱼",
		Segments: []domain.Segment{domain.Text("我说的这句")},
	}
	got := replyPreviewOf(m)
	if !strings.HasPrefix(got, "肥鱼: ") {
		t.Errorf("引用预览 = %q，应使用落库的发送者名字", got)
	}
	if strings.Contains(got, "木木") {
		t.Errorf("引用预览 = %q，不该出现硬编码的默认昵称", got)
	}
}

// TestMessagesInboundQuote 验证入站引用（走 ref_msg_idx）在 WebUI 上也能显示。
//
// 入站引用的键空间与出站不同：被引消息落库时 MessageID 是 d.id（ROBOT1.0_…），
// 而引用事件里只有 REFIDX_，所以必须走 ByRefMsgIdx 才查得到。
func TestMessagesInboundQuote(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	seedData(t, env.db)
	env.loginOK()

	ctx := context.Background()
	base := time.Now().Add(-time.Minute)

	// 被引用的原消息，带自己的引用索引。
	target := &domain.Message{
		ID:         "iq-target",
		SessionID:  groupSessionID,
		MessageID:  "ROBOT1.0_target",
		Platform:   "qq",
		UserID:     "u2",
		UserName:   "小红",
		IsGroup:    true,
		Segments:   []domain.Segment{domain.Text("今晚加班")},
		PlainText:  "今晚加班",
		RefMsgIdx:  "REFIDX_target",
		ReceivedAt: base,
	}
	if err := env.db.Messages().Save(ctx, target); err != nil {
		t.Fatalf("播种原消息失败: %v", err)
	}

	// 引用它的那条：只有 ref_msg_idx，没有 reply_to。
	quoter := &domain.Message{
		ID:         "iq-quoter",
		SessionID:  groupSessionID,
		MessageID:  "ROBOT1.0_quoter",
		Platform:   "qq",
		UserID:     "u1",
		UserName:   "小明",
		IsGroup:    true,
		Segments:   []domain.Segment{domain.Reply("REFIDX_target"), domain.Text("真的假的")},
		PlainText:  "真的假的",
		RefMsgIdx:  "REFIDX_self",
		ReceivedAt: base.Add(10 * time.Second),
		Additional: map[string]string{
			domain.AdditionalRefMsgIdx:  "REFIDX_target",
			domain.AdditionalQuotedText: "今晚加班",
		},
	}
	if err := env.db.Messages().Save(ctx, quoter); err != nil {
		t.Fatalf("播种引用消息失败: %v", err)
	}

	_, body := env.do(http.MethodGet, "/api/messages?session_id="+groupSessionID+"&limit=50", "")
	var out struct {
		Messages []messageRow `json:"messages"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	var found *messageRow
	for i := range out.Messages {
		if out.Messages[i].ID == "ROBOT1.0_quoter" {
			found = &out.Messages[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("没找到引用消息，响应: %s", body)
	}
	if found.ReplyTo != "REFIDX_target" {
		t.Errorf("ReplyTo = %q，入站引用应取 ref_msg_idx", found.ReplyTo)
	}
	if !found.IsQuoteTarget {
		t.Error("IsQuoteTarget 应为 true，前端要靠它画引用块")
	}
	if found.QuotedText != "今晚加班" {
		t.Errorf("QuotedText = %q", found.QuotedText)
	}
	// 反查应该补出发送者。
	if found.QuotedSender != "小红" {
		t.Errorf("QuotedSender = %q，期望反查到小红", found.QuotedSender)
	}
	if !strings.Contains(found.ReplyPreview, "今晚加班") {
		t.Errorf("ReplyPreview = %q，应含被引正文", found.ReplyPreview)
	}
}

// TestMessagesOutboundQuotePreview 守「出站引用的预览能查出来」。
//
// 出站消息的 reply_to 存的是 REFIDX_ 形态（引用索引），
// 不是 MessageID。所以预览必须走 ByRefMsgIdx ——
// 只走 ByMessageIDs 的话每条出站引用都显示成一串截断 ID。
//
// 这也依赖 recordOutbound 把发送响应的 ext_info.ref_idx 写进 ref_msg_idx：
// 不写的话按 ref_msg_idx 反查永远查不到机器人自己说过的话。
func TestMessagesOutboundQuotePreview(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	seedData(t, env.db)

	base := time.Now().Add(-time.Minute)

	// 木木先说过一句，带自己的引用索引。
	self := &domain.Message{
		ID:         "self-1",
		Platform:   "qq",
		MessageID:  "ROBOT1.0_self",
		SessionID:  groupSessionID,
		UserID:     "bot-openid",
		UserName:   "木木",
		IsGroup:    true,
		IsSelf:     true,
		RefMsgIdx:  "REFIDX_self",
		PlainText:  "我先说一句",
		Segments:   []domain.Segment{domain.Text("我先说一句")},
		ReceivedAt: base,
	}
	if err := env.db.Messages().Save(context.Background(), self); err != nil {
		t.Fatalf("播种自助消息失败: %v", err)
	}

	// 木木又一条，引用了自己刚才那句（reply_to = 引用索引）。
	quoted := &domain.Message{
		ID:         "self-2",
		Platform:   "qq",
		MessageID:  "ROBOT1.0_self2",
		SessionID:  groupSessionID,
		UserID:     "bot-openid",
		UserName:   "木木",
		IsGroup:    true,
		IsSelf:     true,
		PlainText:  "接着说",
		Segments:   []domain.Segment{domain.Text("接着说")},
		ReceivedAt: base.Add(time.Second),
		Additional: map[string]string{
			domain.AdditionalReplyTo: "REFIDX_self",
		},
	}
	if err := env.db.Messages().Save(context.Background(), quoted); err != nil {
		t.Fatalf("播种引用消息失败: %v", err)
	}

	env.loginOK()
	_, body := env.do(http.MethodGet, "/api/messages?session_id="+groupSessionID+"&limit=50", "")

	var out struct {
		Messages []messageRow `json:"messages"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}

	var found *messageRow
	for i := range out.Messages {
		if out.Messages[i].ID == "ROBOT1.0_self2" {
			found = &out.Messages[i]
		}
	}
	if found == nil {
		t.Fatal("没找到引用消息")
	}
	if found.ReplyTo != "REFIDX_self" {
		t.Errorf("reply_to = %q，期望 REFIDX_self", found.ReplyTo)
	}
	if !strings.Contains(found.ReplyPreview, "我先说一句") {
		t.Errorf("引用预览 = %q，应包含被引正文（靠 ref_msg_idx 反查）", found.ReplyPreview)
	}
}
