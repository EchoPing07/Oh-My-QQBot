package store

import (
	"context"
	"testing"
	"time"
)

// TestEmojiSaveAndSearch 验证「写入表情包 → 按情绪标签检索」。
func TestEmojiSaveAndSearch(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().Unix()

	e := &Emoji{
		FileHash:    "hash-aaa",
		FilePath:    "data/emoji/aa/hash-aaa.png",
		Description: "无语，翻白眼，无奈",
		CreatedAt:   now,
	}
	created, err := db.Emojis().Save(ctx, e)
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if !created {
		t.Fatal("首次写入应返回 true")
	}

	hits, err := db.Emojis().Search(ctx, "无语", 5)
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("命中 %d 条，期望 1", len(hits))
	}
	if hits[0].FileHash != "hash-aaa" {
		t.Errorf("哈希 = %q", hits[0].FileHash)
	}
}

// TestEmojiSaveDedupesByHash 验证按内容哈希去重。
//
// 群里同一张表情包会被反复发，不按哈希去重会瞬间把库撑爆。
func TestEmojiSaveDedupesByHash(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().Unix()

	e := &Emoji{FileHash: "dup", FilePath: "a.png", CreatedAt: now}
	if created, err := db.Emojis().Save(ctx, e); err != nil || !created {
		t.Fatalf("首次写入失败: created=%v err=%v", created, err)
	}

	again := &Emoji{FileHash: "dup", FilePath: "b.png", CreatedAt: now}
	created, err := db.Emojis().Save(ctx, again)
	if err != nil {
		t.Fatalf("二次写入失败: %v", err)
	}
	if created {
		t.Error("重复哈希应返回 false")
	}

	n, err := db.Emojis().Count(ctx)
	if err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if n != 1 {
		t.Errorf("总数 = %d，期望 1", n)
	}
}

// TestEmojiUpdateDescriptionRebuildsIndex 验证补描述后能立刻被检索到。
//
// VLM 打标签是异步的：落库时描述为空、索引里也没有它。
// 如果 UpdateDescription 忘了重建索引，表情包会永远检索不到，
// 表现是「库里有图但模型说没有合适的表情」。
func TestEmojiUpdateDescriptionRebuildsIndex(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().Unix()

	e := &Emoji{FileHash: "later", FilePath: "x.png", CreatedAt: now}
	if _, err := db.Emojis().Save(ctx, e); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	// 描述为空时不该被检索到。
	if hits, err := db.Emojis().Search(ctx, "开心", 5); err != nil {
		t.Fatalf("检索失败: %v", err)
	} else if len(hits) != 0 {
		t.Errorf("还没描述就命中了 %d 条", len(hits))
	}

	if err := db.Emojis().UpdateDescription(ctx, e.ID, "开心，大笑"); err != nil {
		t.Fatalf("补描述失败: %v", err)
	}

	hits, err := db.Emojis().Search(ctx, "开心", 5)
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("补描述后命中 %d 条，期望 1", len(hits))
	}
}

// TestEmojiMarkUsedAndOrdering 验证使用次数会参与排序。
func TestEmojiMarkUsedAndOrdering(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().Unix()

	low := &Emoji{FileHash: "low", FilePath: "l.png", Description: "开心", CreatedAt: now}
	high := &Emoji{FileHash: "high", FilePath: "h.png", Description: "开心", CreatedAt: now}
	for _, e := range []*Emoji{low, high} {
		if _, err := db.Emojis().Save(ctx, e); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
	}

	// 给 high 刷几次使用次数。
	for i := 0; i < 5; i++ {
		if err := db.Emojis().MarkUsed(ctx, high.ID, time.Unix(now, 0)); err != nil {
			t.Fatalf("标记失败: %v", err)
		}
	}

	hits, err := db.Emojis().Search(ctx, "开心", 5)
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("命中 %d 条，期望 2", len(hits))
	}
	if hits[0].FileHash != "high" {
		t.Errorf("首条应是使用次数多的那个，实际 %q", hits[0].FileHash)
	}
}

// TestEmojiDeleteRemovesFTS 同 summary：删除必须同步清索引。
func TestEmojiDeleteRemovesFTS(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().Unix()

	e := &Emoji{FileHash: "del", FilePath: "d.png", Description: "独一无二异形虫", CreatedAt: now}
	if _, err := db.Emojis().Save(ctx, e); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	path, err := db.Emojis().Delete(ctx, e.ID)
	if err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	// 返回路径是为了让调用方删文件；这里顺带确认拿到了。
	if path != "d.png" {
		t.Errorf("返回的文件路径 = %q，期望 d.png", path)
	}

	var orphan int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM emoji_fts WHERE rowid = ?`, e.ID).Scan(&orphan); err != nil {
		t.Fatalf("查询 FTS 失败: %v", err)
	}
	if orphan != 0 {
		t.Errorf("emoji_fts 残留 %d 行", orphan)
	}
}

// TestEmojiRandom 验证随机取样能返回记录。
func TestEmojiRandom(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().Unix()

	for i, h := range []string{"r1", "r2", "r3"} {
		if _, err := db.Emojis().Save(ctx, &Emoji{
			FileHash: h, FilePath: h + ".png", CreatedAt: now + int64(i),
		}); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
	}

	hits, err := db.Emojis().Random(ctx, 2)
	if err != nil {
		t.Fatalf("随机取样失败: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("取到 %d 条，期望 2", len(hits))
	}
}
