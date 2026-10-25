package store

// 本文件覆盖 Open 与迁移逻辑，并提供各仓库测试共用的辅助函数。
// 只新建测试文件，不修改任何生产代码。

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// unix 用秒级时间戳构造时间，方便与库中 INTEGER 秒精度对齐。
func unix(sec int64) time.Time { return time.Unix(sec, 0) }

// openTestDB 在临时目录里打开一个全新的数据库，测试结束自动关闭。
func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("关闭测试数据库失败: %v", err)
		}
	})
	return db
}

// TestOpenMigratesAndCreatesTables 防的是：迁移没跑、表没建，
// 后续所有仓库在真实启动时直接报 "no such table"。
func TestOpenMigratesAndCreatesTables(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()

	var version string
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT version FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatalf("查询 schema_migrations 失败: %v", err)
	}
	if version != "0001_init" {
		t.Fatalf("迁移版本 = %q，期望 %q", version, "0001_init")
	}

	// 初始迁移必须把 5 张业务表全部建出来。
	want := []string{"messages", "sessions", "persons", "llm_usage", "kv", "reasoning_records"}
	for _, table := range want {
		var n int
		err := db.SQL().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n)
		if err != nil {
			t.Fatalf("查询表 %s 失败: %v", table, err)
		}
		if n != 1 {
			t.Errorf("表 %s 不存在", table)
		}
	}
}

// TestOpenMigrationIsIdempotent 防的是：重复启动时迁移被重复执行，
// 导致 INSERT 主键冲突或数据被二次覆盖。
func TestOpenMigrationIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idempotent.db")
	ctx := t.Context()

	db1, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("第一次 Open 失败: %v", err)
	}
	var firstApplied int64
	if err := db1.SQL().QueryRowContext(ctx,
		`SELECT applied_at FROM schema_migrations WHERE version='0001_init'`).Scan(&firstApplied); err != nil {
		t.Fatalf("查询迁移记录失败: %v", err)
	}
	if err := db1.Close(); err != nil {
		t.Fatalf("第一次 Close 失败: %v", err)
	}

	// 第二次打开同一路径：迁移记录不应重复插入，applied_at 也不应变化。
	db2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("第二次 Open 失败: %v", err)
	}
	defer func() {
		if err := db2.Close(); err != nil {
			t.Errorf("第二次 Close 失败: %v", err)
		}
	}()

	var n int
	if err := db2.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatalf("统计迁移记录失败: %v", err)
	}
	// 这里不能把条数写死成 1：迁移文件会不断新增（现在是 0001 + 0002），
	// 写死会让每次加迁移都得改测试。「条数与版本数相等」才是幂等的真正含义。
	var distinct int
	if err := db2.SQL().QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT version) FROM schema_migrations`).Scan(&distinct); err != nil {
		t.Fatalf("统计迁移版本失败: %v", err)
	}
	if n != distinct {
		t.Fatalf("迁移记录条数 %d 不等于版本数 %d（迁移被重复应用）", n, distinct)
	}
	if n < 2 {
		t.Fatalf("迁移记录条数 = %d，期望至少 2（0001_init 与 0002_memory_emoji_expression）", n)
	}

	var secondApplied int64
	if err := db2.SQL().QueryRowContext(ctx,
		`SELECT applied_at FROM schema_migrations WHERE version='0001_init'`).Scan(&secondApplied); err != nil {
		t.Fatalf("第二次查询迁移记录失败: %v", err)
	}
	if secondApplied != firstApplied {
		t.Errorf("applied_at 被改写: %d -> %d", firstApplied, secondApplied)
	}
}

// TestOpenCreatesParentDirectories 防的是：首次启动时数据目录不存在，
// Open 应在建库前把多级父目录补出来。
func TestOpenCreatesParentDirectories(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "b", "c", "db.sqlite")
	db, err := Open(t.Context(), path)
	if err != nil {
		t.Fatalf("在深层目录打开数据库失败: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("关闭数据库失败: %v", err)
	}

	// 再开一次，证明文件确实落地且可复用。
	db2, err := Open(t.Context(), path)
	if err != nil {
		t.Fatalf("复用深层目录数据库失败: %v", err)
	}
	defer db2.Close()
	if _, err := db2.Messages().Count(context.Background()); err != nil {
		t.Fatalf("复用后查询失败: %v", err)
	}
}

// TestFTSNormalizeMigration 验证 0009_fts_normalize 会按新分词规则重算已落库的 tokens。
//
// 模拟旧数据：直接把 memory_fts / emoji_fts 的 tokens 列改成旧规则
// （未做全角→半角归一）的产物，新查询侧归一后跨形态查不到；
// 删掉迁移版本号重开库，迁移应按新规则重切 tokens 并 rebuild，查询恢复命中。
func TestFTSNormalizeMigration(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	path := db.path

	// 正常写入一条记忆与一条表情包描述（原文都是半角 GPT4）。
	s := &MemorySummary{
		SessionID: "sess-1",
		Summary:   "他说GPT4很强",
		CreatedAt: time.Now().Unix(),
	}
	if err := db.Memory().SaveSummary(ctx, s); err != nil {
		t.Fatalf("写入回想失败: %v", err)
	}
	created, err := db.Emojis().Save(ctx, &Emoji{
		FileHash: "hash-1", FilePath: "data/emoji/a.png",
		Description: "GPT4表情", CreatedAt: time.Now().Unix(),
	})
	if err != nil || !created {
		t.Fatalf("写入表情包失败: created=%v err=%v", created, err)
	}
	// Save 成功后 ID 已回填，emoji_fts 的 rowid 对应的就是它。
	emojiID := int64(0)
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT id FROM emojis WHERE file_hash = 'hash-1'`).Scan(&emojiID); err != nil {
		t.Fatalf("查询表情包 ID 失败: %v", err)
	}

	// 把 tokens 列改回旧规则的产物：全角形态的 ｇｐｔ４ token。
	// （旧分词器对全角输入不做归一，「ＧＰＴ４」会切成 "ｇｐｔ４" 这一个 token。）
	if _, err := db.SQL().ExecContext(ctx,
		`UPDATE memory_fts SET tokens = 'ｇｐｔ４ 很 很强 强' WHERE rowid = ?`, s.ID,
	); err != nil {
		t.Fatalf("污染 memory_fts 失败: %v", err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`UPDATE emoji_fts SET tokens = 'ｇｐｔ４ 表 表情 情' WHERE rowid = ?`, emojiID,
	); err != nil {
		t.Fatalf("污染 emoji_fts 失败: %v", err)
	}

	// 迁移前：查询「gpt4」跨形态查不到（这正是 P2-19 要修的现象）。
	if hits, _ := db.Memory().SearchSummaries(ctx, "", "gpt4", 5); len(hits) != 0 {
		t.Fatalf("污染后不应命中，实际 %v", hits)
	}
	if hits, _ := db.Emojis().Search(ctx, "gpt4", 5); len(hits) != 0 {
		t.Fatalf("污染后表情包不应命中，实际 %d 条", len(hits))
	}

	// 删掉迁移记录并重开库 → 0009 重跑。
	if _, err := db.SQL().ExecContext(ctx,
		`DELETE FROM schema_migrations WHERE version = '0009_fts_normalize'`,
	); err != nil {
		t.Fatalf("删除迁移记录失败: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("关闭数据库失败: %v", err)
	}
	db2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("重开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })

	// 迁移后：按新规则重切的 tokens 含半角 gpt4，两条检索都恢复。
	hits, err := db2.Memory().SearchSummaries(ctx, "", "gpt4", 5)
	if err != nil || len(hits) == 0 {
		t.Errorf("迁移后记忆检索应命中: hits=%d err=%v", len(hits), err)
	}
	emojis, err := db2.Emojis().Search(ctx, "gpt4", 5)
	if err != nil || len(emojis) == 0 {
		t.Errorf("迁移后表情包检索应命中: hits=%d err=%v", len(emojis), err)
	}

	// 版本号要被重新记录，否则每次启动都会重跑一遍。
	var recorded int
	if err := db2.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations WHERE version = '0009_fts_normalize'`,
	).Scan(&recorded); err != nil || recorded != 1 {
		t.Errorf("迁移版本未记录: count=%d err=%v", recorded, err)
	}
}
