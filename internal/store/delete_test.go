package store

// 删除类方法的测试。
//
// 这一层的两个共同风险：
//  1. **删了不报错也不生效**：不看 RowsAffected 时，删一个不存在的 id
//     会返回 nil，调用方以为成功。
//  2. **漏删关联数据**：会话删了、消息还在，界面上是孤儿数据。

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
)

// TestSessionDeleteRemovesScopedData 防：删会话留下孤儿数据。
func TestSessionDeleteRemovesScopedData(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now()

	if err := db.Sessions().Upsert(ctx, &domain.Session{
		ID: "s1", Platform: "qq", Scope: domain.ScopeGroup,
		TargetID: "g1", DisplayName: "群一", MessageCount: 1,
		LastActiveAt: now, CreatedAt: now,
	}); err != nil {
		t.Fatalf("写入会话失败: %v", err)
	}
	if err := db.Messages().Save(ctx, &domain.Message{
		ID: "m1", MessageID: "m1", SessionID: "s1", Platform: "qq",
		UserID: "u1", PlainText: "hi", ReceivedAt: now,
	}); err != nil {
		t.Fatalf("写入消息失败: %v", err)
	}
	if err := db.Memory().SaveSummary(ctx, &MemorySummary{
		SessionID: "s1", Summary: "聊过", CreatedAt: now.Unix(),
	}); err != nil {
		t.Fatalf("写入记忆失败: %v", err)
	}

	if err := db.Sessions().Delete(ctx, "s1"); err != nil {
		t.Fatalf("删除失败: %v", err)
	}

	msgs, err := db.Messages().Recent(ctx, "s1", 10)
	if err != nil {
		t.Fatalf("查消息失败: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("消息没被删: %d 条", len(msgs))
	}

	var n int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions WHERE id = 's1'`).Scan(&n); err != nil {
		t.Fatalf("查会话失败: %v", err)
	}
	if n != 0 {
		t.Error("会话没被删")
	}
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM memory_summaries WHERE session_id = 's1'`).Scan(&n); err != nil {
		t.Fatalf("查记忆失败: %v", err)
	}
	if n != 0 {
		t.Error("记忆没被删")
	}
}

// TestSessionDeleteMissing 防：删不存在的会话返回 nil。
func TestSessionDeleteMissing(t *testing.T) {
	db := openTestDB(t)
	err := db.Sessions().Delete(context.Background(), "不存在")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("期望 ErrNotFound，实际 %v", err)
	}
}

// TestMessageDeleteBySession 覆盖按会话删消息。
func TestMessageDeleteBySession(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now()

	for _, c := range []struct{ id, sid string }{
		{"m1", "s1"}, {"m2", "s1"}, {"m3", "s2"},
	} {
		if err := db.Messages().Save(ctx, &domain.Message{
			ID: c.id, MessageID: c.id, SessionID: c.sid, Platform: "qq",
			PlainText: "x", ReceivedAt: now,
		}); err != nil {
			t.Fatalf("写入消息失败: %v", err)
		}
	}

	n, err := db.Messages().DeleteBySession(ctx, "s1")
	if err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if n != 2 {
		t.Errorf("删除条数 = %d，期望 2", n)
	}

	left, err := db.Messages().Recent(ctx, "s2", 10)
	if err != nil {
		t.Fatalf("查消息失败: %v", err)
	}
	if len(left) != 1 {
		t.Errorf("另一个会话被误删: %d 条", len(left))
	}
}

// TestMessageDeleteAll 覆盖清空全库。
func TestMessageDeleteAll(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now()

	for _, id := range []string{"m1", "m2"} {
		if err := db.Messages().Save(ctx, &domain.Message{
			ID: id, MessageID: id, SessionID: "s1", Platform: "qq",
			PlainText: "x", ReceivedAt: now,
		}); err != nil {
			t.Fatalf("写入消息失败: %v", err)
		}
	}

	n, err := db.Messages().DeleteAll(ctx)
	if err != nil {
		t.Fatalf("清空失败: %v", err)
	}
	if n != 2 {
		t.Errorf("清空条数 = %d，期望 2", n)
	}
	total, err := db.Messages().Count(ctx)
	if err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if total != 0 {
		t.Errorf("清空后仍有 %d 条", total)
	}
}

// TestMemoryDeleteExpression 防：删不存在的表达返回 nil。
func TestMemoryDeleteExpression(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if _, err := db.Memory().SaveExpression(ctx, &Expression{
		SessionID: "s1", Situation: "被夸", Style: "嘿嘿",
	}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	list, err := db.Memory().ListAllExpressions(ctx, 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("读取失败: %v n=%d", err, len(list))
	}

	if err := db.Memory().DeleteExpression(ctx, list[0].ID); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if err := db.Memory().DeleteExpression(ctx, list[0].ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("删第二次应返回 ErrNotFound，实际 %v", err)
	}
}

// TestMemoryDeleteJargon 同上。
func TestMemoryDeleteJargon(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().Unix()

	if err := db.Memory().SaveJargon(ctx, &Jargon{
		Content: "破防", Count: 1, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	list, err := db.Memory().ListJargons(ctx, 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("读取失败: %v n=%d", err, len(list))
	}

	if err := db.Memory().DeleteJargon(ctx, list[0].ID); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if err := db.Memory().DeleteJargon(ctx, list[0].ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("删第二次应返回 ErrNotFound，实际 %v", err)
	}
}

// TestEmojiDeleteReturnsPathAndReportsMissing 守表情包删除的两件事：
//  1. 返回文件路径，让调用方能删物理文件
//  2. 删不存在的 id 要报错，不能静默成功
func TestEmojiDeleteReturnsPathAndReportsMissing(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	e := &Emoji{FileHash: "h1", FilePath: "/tmp/x.png", Description: "猫", CreatedAt: time.Now().Unix()}
	if _, err := db.Emojis().Save(ctx, e); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	path, err := db.Emojis().Delete(ctx, e.ID)
	if err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if path != "/tmp/x.png" {
		t.Errorf("返回路径 = %q", path)
	}

	if _, err := db.Emojis().Delete(ctx, e.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("删第二次应返回 ErrNotFound，实际 %v", err)
	}
}

// TestDeleteProfile 覆盖画像删除。
func TestDeleteProfile(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().Unix()

	if err := db.Memory().UpsertProfile(ctx, &PersonProfile{
		Platform: "qq", UserID: "u1", SessionID: "s1",
		DisplayName: "小明", ProfileText: "喜欢摄影",
		MessageCount: 5, LastMessageAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	ok, err := db.Memory().DeleteProfile(ctx, "qq", "u1", "s1")
	if err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if !ok {
		t.Error("应当删掉了 1 条")
	}

	// 再删一次应返回 false（没有删除任何行）。
	ok, err = db.Memory().DeleteProfile(ctx, "qq", "u1", "s1")
	if err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if ok {
		t.Error("重复删除不该报告成功")
	}
}

// TestBackupToProducesUsableFile 防：备份出来的文件打不开。
func TestBackupToProducesUsableFile(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.Messages().Save(ctx, &domain.Message{
		ID: "m1", MessageID: "m1", SessionID: "s1", Platform: "qq",
		PlainText: "备份这条", ReceivedAt: time.Now(),
	}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	path := t.TempDir() + "/backup.db"
	if err := db.BackupTo(ctx, path); err != nil {
		t.Fatalf("备份失败: %v", err)
	}

	// 备份文件必须能被当成数据库打开，并且内容完整。
	restored, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("打开备份失败: %v", err)
	}
	defer restored.Close()

	msgs, err := restored.Messages().Recent(ctx, "s1", 10)
	if err != nil {
		t.Fatalf("读备份失败: %v", err)
	}
	if len(msgs) != 1 || msgs[0].PlainText != "备份这条" {
		t.Errorf("备份内容不对: %+v", msgs)
	}
}

// TestBackupToBadPath 覆盖路径不可写时的错误。
func TestBackupToBadPath(t *testing.T) {
	db := openTestDB(t)
	err := db.BackupTo(context.Background(), t.TempDir()+"/不存在的目录/x.db")
	if err == nil {
		t.Error("路径不可写时应当报错")
	}
}
