package store

// 本文件覆盖 MessageRepo 的写入与各类查询。
// 重点防线：segments 的 JSON 往返、会话过滤、排序方向、时间边界。

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
)

// newMessage 构造一条可落库的消息，只填 Save 会持久化的字段。
func newMessage(id, sessionID, messageID string, at int64) *domain.Message {
	return &domain.Message{
		ID:          id,
		Platform:    "qq",
		MessageID:   messageID,
		SessionID:   sessionID,
		UserID:      "u-1",
		UserName:    "测试用户",
		GroupID:     "g-1",
		GroupName:   "测试群",
		IsGroup:     true,
		IsAt:        true,
		IsMentioned: false,
		IsCommand:   true,
		PlainText:   "你好 世界",
		ReceivedAt:  unix(at),
	}
}

// TestMessageRepoSaveAndSegmentsRoundTrip 防的是：segments 序列化/反序列化丢字段，
// 尤其是 image 段的 url 以及 at 段的 user_id，重启后重放会失真。
func TestMessageRepoSaveAndSegmentsRoundTrip(t *testing.T) {
	repo := openTestDB(t).Messages()
	ctx := t.Context()

	m := newMessage("m-1", "s-1", "platform-1", 1000)
	m.Segments = []domain.Segment{
		domain.Text("看这个图 "),
		domain.Image("https://example.com/a.png"),
		domain.At("123456", "小明"),
		domain.Reply("platform-0"),
		{Type: domain.SegmentImage, URL: "https://example.com/b.png", MimeType: "image/png"},
	}
	if err := repo.Save(ctx, m); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	got, err := repo.ByMessageID(ctx, "platform-1")
	if err != nil {
		t.Fatalf("ByMessageID 失败: %v", err)
	}
	if got == nil {
		t.Fatal("ByMessageID 返回 nil，期望命中刚写入的消息")
	}

	// 基础字段往返。
	if got.ID != m.ID || got.SessionID != m.SessionID || got.MessageID != m.MessageID {
		t.Errorf("标识字段不一致: got=%+v want=%+v", got, m)
	}
	if got.UserID != m.UserID || got.UserName != m.UserName {
		t.Errorf("用户字段不一致: got=%q/%q want=%q/%q",
			got.UserID, got.UserName, m.UserID, m.UserName)
	}
	if got.GroupID != m.GroupID || got.GroupName != m.GroupName {
		t.Errorf("群字段不一致: got=%q/%q want=%q/%q",
			got.GroupID, got.GroupName, m.GroupID, m.GroupName)
	}
	if got.IsGroup != m.IsGroup || got.IsAt != m.IsAt ||
		got.IsMentioned != m.IsMentioned || got.IsCommand != m.IsCommand {
		t.Errorf("布尔标记不一致: got=%+v want=%+v", got, m)
	}
	if got.PlainText != m.PlainText {
		t.Errorf("PlainText = %q，期望 %q", got.PlainText, m.PlainText)
	}
	if !got.ReceivedAt.Equal(m.ReceivedAt) {
		t.Errorf("ReceivedAt = %v，期望 %v", got.ReceivedAt, m.ReceivedAt)
	}

	// segments 必须逐段一致。
	if len(got.Segments) != len(m.Segments) {
		t.Fatalf("segments 数量 = %d，期望 %d: %+v", len(got.Segments), len(m.Segments), got.Segments)
	}
	for i := range m.Segments {
		if !reflect.DeepEqual(got.Segments[i], m.Segments[i]) {
			t.Errorf("segments[%d] = %+v，期望 %+v", i, got.Segments[i], m.Segments[i])
		}
	}
	// 明确断言 image 段的 URL，防止 omitempty 之类改动把它吃掉。
	if got.Segments[1].Type != domain.SegmentImage || got.Segments[1].URL != "https://example.com/a.png" {
		t.Errorf("image 段 URL 丢失: %+v", got.Segments[1])
	}
}

// TestMessageRepoRecentFiltersAndOrders 防的是：Recent 漏掉 session 过滤、
// 忽略 limit，或返回的是倒序而不是注释承诺的时间正序。
func TestMessageRepoRecentFiltersAndOrders(t *testing.T) {
	repo := openTestDB(t).Messages()
	ctx := t.Context()

	// 两个会话交错写入，时间戳递增。
	seq := []struct {
		id, session string
		at          int64
	}{
		{"a1", "s-a", 100},
		{"b1", "s-b", 110},
		{"a2", "s-a", 120},
		{"b2", "s-b", 130},
		{"a3", "s-a", 140},
	}
	for _, it := range seq {
		if err := repo.Save(ctx, newMessage(it.id, it.session, "pm-"+it.id, it.at)); err != nil {
			t.Fatalf("Save %s 失败: %v", it.id, err)
		}
	}

	got, err := repo.Recent(ctx, "s-a", 10)
	if err != nil {
		t.Fatalf("Recent 失败: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("s-a 消息数 = %d，期望 3（有跨会话泄漏或漏查）", len(got))
	}
	wantIDs := []string{"a1", "a2", "a3"}
	for i, want := range wantIDs {
		if got[i].ID != want {
			t.Errorf("Recent[%d].ID = %q，期望 %q（应为时间正序）", i, got[i].ID, want)
		}
	}
	for _, m := range got {
		if m.SessionID != "s-a" {
			t.Errorf("Recent 返回了其他会话的消息: %q", m.SessionID)
		}
	}

	// limit 应取最近的 N 条，但仍按正序返回。
	limited, err := repo.Recent(ctx, "s-a", 2)
	if err != nil {
		t.Fatalf("带 limit 的 Recent 失败: %v", err)
	}
	if len(limited) != 2 {
		t.Fatalf("limit=2 返回 %d 条", len(limited))
	}
	if limited[0].ID != "a2" || limited[1].ID != "a3" {
		t.Errorf("limit 截取错误: %q,%q，期望 a2,a3", limited[0].ID, limited[1].ID)
	}
}

// TestMessageRepoSameTimestampOrderIsStable 防的是：同一秒内多条消息因排序退化
// 导致 Recent 的翻转顺序错乱（依赖 rowid 兜底，而 rowid 方向要和 created_at 一致）。
func TestMessageRepoSameTimestampOrderIsStable(t *testing.T) {
	repo := openTestDB(t).Messages()
	ctx := t.Context()

	for _, id := range []string{"x1", "x2", "x3"} {
		if err := repo.Save(ctx, newMessage(id, "s-x", "pm-"+id, 500)); err != nil {
			t.Fatalf("Save %s 失败: %v", id, err)
		}
	}

	got, err := repo.Recent(ctx, "s-x", 10)
	if err != nil {
		t.Fatalf("Recent 失败: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("消息数 = %d，期望 3", len(got))
	}
	for i, want := range []string{"x1", "x2", "x3"} {
		if got[i].ID != want {
			t.Errorf("同秒消息顺序[%d] = %q，期望 %q", i, got[i].ID, want)
		}
	}
}

// TestMessageRepoSaveIsIdempotentOnID 防的是：同一条消息被重复投递时
// 触发主键冲突报错，或把已有行覆盖成新值。
func TestMessageRepoSaveIsIdempotentOnID(t *testing.T) {
	repo := openTestDB(t).Messages()
	ctx := t.Context()

	first := newMessage("dup", "s-1", "pm-dup", 1000)
	first.PlainText = "原始内容"
	if err := repo.Save(ctx, first); err != nil {
		t.Fatalf("首次 Save 失败: %v", err)
	}

	second := newMessage("dup", "s-1", "pm-dup", 2000)
	second.PlainText = "覆盖内容"
	if err := repo.Save(ctx, second); err != nil {
		t.Fatalf("重复 Save 不应报错: %v", err)
	}

	n, err := repo.Count(ctx)
	if err != nil {
		t.Fatalf("Count 失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("Count = %d，期望 1（重复 ID 被插入多次）", n)
	}
	got, err := repo.ByMessageID(ctx, "pm-dup")
	if err != nil {
		t.Fatalf("ByMessageID 失败: %v", err)
	}
	if got.PlainText != "原始内容" {
		t.Errorf("重复 Save 覆盖了已有行: PlainText = %q", got.PlainText)
	}
}

// TestMessageRepoSinceBoundary 防的是：Since 的边界条件（>= vs >）与排序方向，
// 以及 session 过滤。差一个边界就会把上一轮已处理的消息再喂一遍。
func TestMessageRepoSinceBoundary(t *testing.T) {
	repo := openTestDB(t).Messages()
	ctx := t.Context()

	for _, it := range []struct {
		id, session string
		at          int64
	}{
		{"s1", "s-1", 100},
		{"s2", "s-1", 200},
		{"s3", "s-1", 300},
		{"other", "s-2", 250},
	} {
		if err := repo.Save(ctx, newMessage(it.id, it.session, "pm-"+it.id, it.at)); err != nil {
			t.Fatalf("Save %s 失败: %v", it.id, err)
		}
	}

	got, err := repo.Since(ctx, "s-1", unix(200), 10)
	if err != nil {
		t.Fatalf("Since 失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Since 返回 %d 条，期望 2（包含边界 200，且不跨会话）", len(got))
	}
	if got[0].ID != "s2" || got[1].ID != "s3" {
		t.Errorf("Since 顺序/内容错误: %q,%q，期望 s2,s3", got[0].ID, got[1].ID)
	}

	// limit 应从头截断（最早的消息优先）。
	limited, err := repo.Since(ctx, "s-1", unix(100), 1)
	if err != nil {
		t.Fatalf("带 limit 的 Since 失败: %v", err)
	}
	if len(limited) != 1 || limited[0].ID != "s1" {
		t.Errorf("Since limit 截取错误: %+v，期望仅 s1", limited)
	}

	// 时间点之后没有任何消息时返回空切片。
	empty, err := repo.Since(ctx, "s-1", unix(9999), 10)
	if err != nil {
		t.Fatalf("Since(未来) 失败: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("Since(未来) 返回 %d 条，期望 0", len(empty))
	}
}

// TestMessageRepoLookupsAndCounts 防的是：ByMessageID 的未命中语义（nil,nil）、
// Count/CountSince 的口径与 Since 不一致。
func TestMessageRepoLookupsAndCounts(t *testing.T) {
	repo := openTestDB(t).Messages()
	ctx := t.Context()

	// 空库上先验证未命中与计数。
	missing, err := repo.ByMessageID(ctx, "not-exist")
	if err != nil {
		t.Fatalf("ByMessageID(不存在) 返回错误: %v", err)
	}
	if missing != nil {
		t.Errorf("ByMessageID(不存在) = %+v，期望 nil", missing)
	}
	if n, err := repo.Count(ctx); err != nil || n != 0 {
		t.Fatalf("空库 Count = %d, err = %v", n, err)
	}
	if n, err := repo.CountSince(ctx, unix(0)); err != nil || n != 0 {
		t.Fatalf("空库 CountSince = %d, err = %v", n, err)
	}

	for i, at := range []int64{100, 200, 300} {
		id := "c" + string(rune('1'+i))
		if err := repo.Save(ctx, newMessage(id, "s-c", "pm-"+id, at)); err != nil {
			t.Fatalf("Save 失败: %v", err)
		}
	}

	if n, err := repo.Count(ctx); err != nil || n != 3 {
		t.Fatalf("Count = %d, err = %v，期望 3", n, err)
	}
	// CountSince 与 Since 必须用同一口径（>=）。
	if n, err := repo.CountSince(ctx, unix(200)); err != nil || n != 2 {
		t.Fatalf("CountSince(200) = %d, err = %v，期望 2", n, err)
	}
	if n, err := repo.CountSince(ctx, unix(0)); err != nil || n != 3 {
		t.Fatalf("CountSince(0) = %d, err = %v，期望 3", n, err)
	}
	if n, err := repo.CountSince(ctx, unix(10_000)); err != nil || n != 0 {
		t.Fatalf("CountSince(未来) = %d, err = %v，期望 0", n, err)
	}

	got, err := repo.ByMessageID(ctx, "pm-c2")
	if err != nil {
		t.Fatalf("ByMessageID 失败: %v", err)
	}
	if got == nil || got.ID != "c2" || got.ReceivedAt.Unix() != 200 {
		t.Errorf("ByMessageID 结果错误: %+v", got)
	}
}

// TestMessageRepoTimePrecision 守的是毫秒精度不再丢失。
//
// 这个测试以前断言「纳秒丢失是已知限制」，现在限制已经修掉了
// （migration 0005 加了 created_at_ms），所以断言反过来：
// 毫秒必须原样往返，只有亚毫秒部分允许丢。
//
// 为什么这事不小事：引用公式里「上一句距今 <= 3 秒」这一档直接吃这个精度，
// 秒级精度在重启后会让同一段对话的间隔全部变成整数，判定整体偏移。
func TestMessageRepoTimePrecision(t *testing.T) {
	repo := openTestDB(t).Messages()
	ctx := t.Context()

	m := newMessage("precise", "s-p", "pm-precise", 1000)
	m.ReceivedAt = time.Unix(1000, 987_654_321)
	if err := repo.Save(ctx, m); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	got, err := repo.ByMessageID(ctx, "pm-precise")
	if err != nil || got == nil {
		t.Fatalf("ByMessageID 失败: %v, %+v", err, got)
	}
	if got.ReceivedAt.UnixMilli() != m.ReceivedAt.UnixMilli() {
		t.Errorf("毫秒时间戳不一致: %d vs %d", got.ReceivedAt.UnixMilli(), m.ReceivedAt.UnixMilli())
	}
	if got.ReceivedAt.Nanosecond()%int(time.Millisecond) != 0 {
		t.Errorf("毫秒以下的精度不该保留，实际纳秒=%d", got.ReceivedAt.Nanosecond())
	}
}

// TestMessageRepoPersistsFidelityFields 守的是「重启后字段不丢」。
//
// 以前 UserCardName / AccountID / Additional 三列在表里根本不存在，
// 于是重启恢复的上下文里 group_card 属性凭空消失、
// OpenID 映射也读不回来。字段缺失不报错，只表现为「模型突然不认识这个人」。
func TestMessageRepoPersistsFidelityFields(t *testing.T) {
	repo := openTestDB(t).Messages()
	ctx := t.Context()

	m := newMessage("fid", "s-f", "pm-fid", 1000)
	m.UserCardName = "群里的老王"
	m.AccountID = "bot-openid-9"
	m.Additional = map[string]string{"openid": "abc", "reply_to": "pm-x"}
	m.IsSelf = true
	if err := repo.Save(ctx, m); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	got, err := repo.ByMessageID(ctx, "pm-fid")
	if err != nil || got == nil {
		t.Fatalf("ByMessageID 失败: %v, %+v", err, got)
	}
	if got.UserCardName != m.UserCardName {
		t.Errorf("UserCardName = %q，期望 %q", got.UserCardName, m.UserCardName)
	}
	if got.AccountID != m.AccountID {
		t.Errorf("AccountID = %q，期望 %q", got.AccountID, m.AccountID)
	}
	if got.Additional["openid"] != "abc" || got.Additional["reply_to"] != "pm-x" {
		t.Errorf("Additional 丢失: %+v", got.Additional)
	}
	if !got.IsSelf {
		t.Error("IsSelf 丢失，出站消息会被当成用户消息")
	}
}

// TestMessageRepoBeforePage 守的是往上翻历史的游标语义。
func TestMessageRepoBeforePage(t *testing.T) {
	repo := openTestDB(t).Messages()
	ctx := t.Context()

	for i := 1; i <= 5; i++ {
		m := newMessage(fmt.Sprintf("b%d", i), "s-b", fmt.Sprintf("pm-b%d", i), int64(100*i))
		if err := repo.Save(ctx, m); err != nil {
			t.Fatalf("Save 失败: %v", err)
		}
	}

	// 不带游标 = 最新 limit 条，正序返回。
	latest, err := repo.Before(ctx, "s-b", 0, 2)
	if err != nil {
		t.Fatalf("Before 失败: %v", err)
	}
	if len(latest) != 2 || latest[0].ID != "b4" || latest[1].ID != "b5" {
		t.Fatalf("最新两条错误: %+v", ids(latest))
	}

	// 用最早一条的时间戳当游标往回翻。
	older, err := repo.Before(ctx, "s-b", latest[0].ReceivedAt.UnixMilli(), 2)
	if err != nil {
		t.Fatalf("Before 翻页失败: %v", err)
	}
	if len(older) != 2 || older[0].ID != "b2" || older[1].ID != "b3" {
		t.Fatalf("上一页错误: %+v，期望 b2,b3", ids(older))
	}

	// 游标处那条不能重复出现。
	for _, m := range older {
		if m.ID == "b4" {
			t.Error("游标消息被重复返回，翻页会看到重复行")
		}
	}
}

// TestMessageRepoDaily 守的是按天统计的分桶。
//
// 用 is_self 区分收发：出站消息现在也入库了，
// 不区分的话「机器人说了多少」这个数据就永远等于 0。
func TestMessageRepoDaily(t *testing.T) {
	repo := openTestDB(t).Messages()
	ctx := t.Context()

	now := time.Now()
	if err := repo.Save(ctx, newMessage("d1", "s-d", "pm-d1", now.Unix())); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}
	self := newMessage("d2", "s-d", "pm-d2", now.Unix())
	self.IsSelf = true
	if err := repo.Save(ctx, self); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	// 30 天前的消息不该出现在「最近 7 天」里。
	old := newMessage("d3", "s-d", "pm-d3", now.AddDate(0, 0, -30).Unix())
	if err := repo.Save(ctx, old); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	got, err := repo.Daily(ctx, 7)
	if err != nil {
		t.Fatalf("Daily 失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("返回 %d 天，期望 1 天: %+v", len(got), got)
	}
	if got[0].Total != 2 {
		t.Errorf("今日总数 = %d，期望 2", got[0].Total)
	}
	if got[0].Inbound != 1 || got[0].Outbound != 1 {
		t.Errorf("收发拆分错误: inbound=%d outbound=%d，期望各 1", got[0].Inbound, got[0].Outbound)
	}
}

// ids 取出消息 ID 列表，失败信息里看得清。
func ids(msgs []*domain.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.ID)
	}
	return out
}

// TestMessageRepoPersistsRefMsgIdx 守 ref_msg_idx 这一列不丢。
//
// 它单独成列而不是只塞在 additional 里，是因为要按它建索引反查
// （别人引用木木这句话时，事件里只给 REFIDX_，得靠它找回发送者）。
// 漏写这一列的表现是「引用预览永远查不到人」，不报错。
func TestMessageRepoPersistsRefMsgIdx(t *testing.T) {
	repo := openTestDB(t).Messages()
	ctx := t.Context()

	m := newMessage("ref-1", "s-ref", "pm-ref-1", 1000)
	m.RefMsgIdx = "REFIDX_abc+/=="
	m.Additional = map[string]string{domain.AdditionalMsgIdx: m.RefMsgIdx}
	if err := repo.Save(ctx, m); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	got, err := repo.ByMessageID(ctx, "pm-ref-1")
	if err != nil || got == nil {
		t.Fatalf("ByMessageID 失败: %v", err)
	}
	if got.RefMsgIdx != "REFIDX_abc+/==" {
		t.Errorf("RefMsgIdx = %q，期望 REFIDX_abc+/==", got.RefMsgIdx)
	}
}

// TestMessageRepoByRefMsgIdx 验证按引用索引批量反查。
func TestMessageRepoByRefMsgIdx(t *testing.T) {
	repo := openTestDB(t).Messages()
	ctx := t.Context()

	// 三条消息各带索引，外加一条没有索引的。
	for i, idx := range []string{"REFIDX_a", "REFIDX_b", "REFIDX_c"} {
		m := newMessage(fmt.Sprintf("r%d", i), "s", fmt.Sprintf("pm-%d", i), int64(1000+i))
		m.RefMsgIdx = idx
		m.UserName = "用户" + idx[len(idx)-1:]
		if err := repo.Save(ctx, m); err != nil {
			t.Fatalf("Save 失败: %v", err)
		}
	}
	plain := newMessage("r-plain", "s", "pm-plain", 2000)
	if err := repo.Save(ctx, plain); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	got, err := repo.ByRefMsgIdx(ctx, []string{"REFIDX_a", "REFIDX_c", "REFIDX_a", "", "不存在"})
	if err != nil {
		t.Fatalf("ByRefMsgIdx 失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("命中 %d 条，期望 2 条（去重 + 忽略空串与查不到的）: %+v", len(got), got)
	}
	if got["REFIDX_a"] == nil || got["REFIDX_a"].UserName != "用户a" {
		t.Errorf("REFIDX_a 命中错误: %+v", got["REFIDX_a"])
	}
	if got["REFIDX_c"] == nil {
		t.Error("REFIDX_c 未命中")
	}

	// 空输入必须返回空 map 而不是报错 —— 绝大多数消息没有引用。
	empty, err := repo.ByRefMsgIdx(ctx, nil)
	if err != nil || len(empty) != 0 {
		t.Errorf("空输入应返回空 map，实际 %v, %v", empty, err)
	}
}

// TestMigrationBackfillsRefMsgIdx 验证 0008 迁移从 additional 回填 ref_msg_idx。
//
// 回填的意义：改造前入库的消息，msg_idx 只存在于 additional 的 JSON 里，
// 不回填的话历史引用永远反查不到发送者。
func TestMigrationBackfillsRefMsgIdx(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()

	// 模拟旧数据：ref_msg_idx 列为空，索引只写在 additional 里。
	_, err := db.sql.ExecContext(ctx, `
		INSERT INTO messages (
			id, session_id, message_id, platform, user_id, user_name, user_card_name,
			group_id, group_name, is_group, is_at, is_mentioned, is_command, is_self,
			account_id, additional, ref_msg_idx, segments, plain_text, created_at, created_at_ms
		) VALUES ('old-1','s','pm-old','qq','u','n','','g','',1,0,0,0,0,'',
			'{"msg_idx":"REFIDX_old"}', '', '[]', 'hi', 1000, 1000000)
	`)
	if err != nil {
		t.Fatalf("插入旧数据失败: %v", err)
	}

	// 迁移语句在测试库里重新跑一遍（openTestDB 已经跑过全量迁移，
	// 这里复制的是同一段 SQL 的语义，验证表达式本身可用）。
	_, err = db.sql.ExecContext(ctx, `
		UPDATE messages
		SET ref_msg_idx = COALESCE(json_extract(additional, '$.msg_idx'), '')
		WHERE ref_msg_idx = ''
		  AND json_valid(additional) = 1
		  AND json_extract(additional, '$.msg_idx') IS NOT NULL
	`)
	if err != nil {
		t.Fatalf("回填失败: %v", err)
	}

	got, err := db.Messages().ByMessageID(ctx, "pm-old")
	if err != nil || got == nil {
		t.Fatalf("ByMessageID 失败: %v", err)
	}
	if got.RefMsgIdx != "REFIDX_old" {
		t.Errorf("回填后 RefMsgIdx = %q，期望 REFIDX_old", got.RefMsgIdx)
	}
}

// TestMigrationBackfillSkipsInvalidJSON 验证回填不会因为脏 JSON 整体失败。
//
// 一条 additional 是坏数据就让迁移中止，等于整库升不了级。
func TestMigrationBackfillSkipsInvalidJSON(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()

	_, err := db.sql.ExecContext(ctx, `
		INSERT INTO messages (
			id, session_id, message_id, platform, user_id, user_name, user_card_name,
			group_id, group_name, is_group, is_at, is_mentioned, is_command, is_self,
			account_id, additional, ref_msg_idx, segments, plain_text, created_at, created_at_ms
		) VALUES ('bad-1','s','pm-bad','qq','u','n','','g','',1,0,0,0,0,'',
			'这不是 JSON', '', '[]', 'hi', 1000, 1000000)
	`)
	if err != nil {
		t.Fatalf("插入脏数据失败: %v", err)
	}

	if _, err := db.sql.ExecContext(ctx, `
		UPDATE messages
		SET ref_msg_idx = COALESCE(json_extract(additional, '$.msg_idx'), '')
		WHERE ref_msg_idx = ''
		  AND json_valid(additional) = 1
		  AND json_extract(additional, '$.msg_idx') IS NOT NULL
	`); err != nil {
		t.Fatalf("脏 JSON 让迁移整体失败了: %v", err)
	}

	got, err := db.Messages().ByMessageID(ctx, "pm-bad")
	if err != nil || got == nil {
		t.Fatalf("脏数据应该还能读出来: %v", err)
	}
	if got.RefMsgIdx != "" {
		t.Errorf("坏 JSON 不该回填出值，实际 %q", got.RefMsgIdx)
	}
}
