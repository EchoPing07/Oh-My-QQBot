// Package store 负责 SQLite 持久化。
// 对应 MaiBot 的 src/common/database，但只保留精简版需要的表。
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/tokenize"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// DB 是数据库句柄。
type DB struct {
	sql *sql.DB
	// path 是数据库文件路径，导入导出与备份要用到。
	path string
}

// Open 打开数据库并执行迁移。
func Open(ctx context.Context, path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("创建数据目录失败: %w", err)
	}

	// 关键参数：
	//   _pragma=journal_mode(WAL)  并发读写更稳
	//   _pragma=busy_timeout(5000) 避免瞬时锁冲突直接报错
	//   _pragma=foreign_keys(1)    开启外键约束
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}

	// SQLite 写入是串行的，连接数不宜过大。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}

	d := &DB{sql: db, path: path}
	if err := d.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return d, nil
}

// Close 关闭数据库。
func (d *DB) Close() error { return d.sql.Close() }

// SQL 暴露底层句柄，供 repository 使用。
func (d *DB) SQL() *sql.DB { return d.sql }

// migrate 按文件名顺序执行未应用的迁移。
func (d *DB) migrate(ctx context.Context) error {
	if _, err := d.sql.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at INTEGER NOT NULL
		)
	`); err != nil {
		return fmt.Errorf("创建迁移表失败: %w", err)
	}

	applied, err := d.appliedVersions(ctx)
	if err != nil {
		return err
	}

	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("读取迁移目录失败: %w", err)
	}

	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		version := strings.TrimSuffix(name, ".sql")
		if applied[version] {
			continue
		}

		raw, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("读取迁移 %s 失败: %w", name, err)
		}

		tx, err := d.sql.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("开启迁移事务失败: %w", err)
		}
		if _, err := tx.ExecContext(ctx, string(raw)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("执行迁移 %s 失败: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations(version, applied_at) VALUES(?, ?)`,
			version, time.Now().Unix(),
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("记录迁移 %s 失败: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("提交迁移 %s 失败: %w", name, err)
		}
	}

	// Go 代码迁移：像「按新分词规则重算 FTS tokens 列」这种要用 Go 函数的
	// 逻辑，SQL 文件表达不了。版本同样记录进 schema_migrations。
	// 约定：Go 迁移总在全部 SQL 迁移之后执行，版本号不与 .sql 文件重名。
	for _, m := range goMigrations {
		if applied[m.version] {
			continue
		}
		if err := d.runGoMigration(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

// goMigration 是一条需要 Go 代码执行的迁移。
type goMigration struct {
	version string
	fn      func(context.Context, *sql.Tx) error
}

// goMigrations 按 version 升序排列。
var goMigrations = []goMigration{
	{"0009_fts_normalize", migrateFTSNormalize},
}

// runGoMigration 在一个事务里执行 Go 迁移并记录版本，失败即回滚并报错。
func (d *DB) runGoMigration(ctx context.Context, m goMigration) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启迁移事务失败: %w", err)
	}
	if err := m.fn(ctx, tx); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("执行迁移 %s 失败: %w", m.version, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations(version, applied_at) VALUES(?, ?)`,
		m.version, time.Now().Unix(),
	); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("记录迁移 %s 失败: %w", m.version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交迁移 %s 失败: %w", m.version, err)
	}
	return nil
}

// migrateFTSNormalize 按新分词规则重算两张 FTS 表的 tokens 列。
//
// 背景：分词器加了全角→半角归一（internal/tokenize 的 normalizeWidth），
// 查询侧跟着归一，但已落库的 tokens 列还是旧规则切出来的形态 ——
// 跨形态（半角库文 vs 全角输入，或反过来）永远查不到。
// 两张 FTS 表都是普通 FTS5 表、自己存了原文（content / description 列），
// 所以可以按新规则重切。
//
// 为什么不能只写一条 `INSERT INTO memory_fts(memory_fts) VALUES('rebuild')`：
// rebuild 只按「已存的列值」重建倒排索引，不会重算 tokens 列的值 ——
// 归一必须在这里用 Go 代码做。重切完再各 rebuild 一次，保证倒排与
// 列值完全一致（对刚 UPDATE 过的普通表来说 rebuild 是冗余的，但便宜，
// 相当于顺手做了一次一致性校验）。
func migrateFTSNormalize(ctx context.Context, tx *sql.Tx) error {
	for _, t := range []struct{ table, textCol string }{
		{"memory_fts", "content"},
		{"emoji_fts", "description"},
	} {
		rows, err := tx.QueryContext(ctx, `SELECT rowid, `+t.textCol+` FROM `+t.table)
		if err != nil {
			return fmt.Errorf("读取 %s 失败: %w", t.table, err)
		}
		type retok struct {
			id     int64
			tokens string
		}
		// 先收集再更新：同一个事务里边遍历 rows 边 UPDATE，
		// 部分驱动会在同一连接上死锁。
		var pending []retok
		for rows.Next() {
			var (
				id   int64
				text sql.NullString
			)
			if err := rows.Scan(&id, &text); err != nil {
				rows.Close()
				return fmt.Errorf("读取 %s 行失败: %w", t.table, err)
			}
			pending = append(pending, retok{id: id, tokens: tokenize.TokenizeJoined(text.String)})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("遍历 %s 失败: %w", t.table, err)
		}
		rows.Close()

		for _, u := range pending {
			if _, err := tx.ExecContext(ctx,
				`UPDATE `+t.table+` SET tokens = ? WHERE rowid = ?`,
				u.tokens, u.id,
			); err != nil {
				return fmt.Errorf("重算 %s 的 tokens 失败（rowid=%d）: %w", t.table, u.id, err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO `+t.table+`(`+t.table+`) VALUES('rebuild')`,
		); err != nil {
			return fmt.Errorf("重建 %s 索引失败: %w", t.table, err)
		}
	}
	return nil
}

func (d *DB) appliedVersions(ctx context.Context) (map[string]bool, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("查询迁移记录失败: %w", err)
	}
	defer rows.Close()

	out := make(map[string]bool)
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = true
	}
	return out, rows.Err()
}

// tx 在事务中执行 fn。
func (d *DB) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// BackupTo 把当前数据库导出成一份一致的快照文件。
//
// 用 VACUUM INTO 而不是复制文件：直接 cp 一个正在写入的 SQLite
// 会拿到「前半页是新、后半页是旧」的损坏副本，而且不报错 ——
// 只在恢复时才发现数据烂了。VACUUM INTO 由 SQLite 保证一致性。
func (d *DB) BackupTo(ctx context.Context, path string) error {
	// VACUUM INTO 不接受参数占位符，路径必须拼进 SQL。
	// 这里的 path 由我们自己在临时目录里生成，不含用户输入；
	// 仍然把单引号转义掉，避免将来有人改成「用户指定路径」时被绕过。
	escaped := strings.ReplaceAll(path, "'", "''")
	_, err := d.sql.ExecContext(ctx, `VACUUM INTO '`+escaped+`'`)
	if err != nil {
		return fmt.Errorf("导出数据库快照失败: %w", err)
	}
	return nil
}

// ReplaceFrom 用给定文件覆盖当前库，返回覆盖前的备份路径。
//
// 只在文件层面替换，不重新打开连接：正在使用的连接池指向旧文件，
// 所以调用方必须让用户重启。这一点在响应里写清楚，
// 不能假装「导入后立即生效」——那会让人以为数据回来了，
// 实际看到的还是旧库。
func (d *DB) ReplaceFrom(ctx context.Context, src string) (string, error) {
	if d.path == "" {
		return "", errors.New("数据库路径未知，无法导入")
	}

	// 先备份当前库。用 BackupTo 而不是 os.Rename：
	// 后者会把旧库从原地移走，导入失败时连接池指向的文件已经不见了。
	backupPath := d.path + ".before-import-" + time.Now().Format("20060102-150405")
	if err := d.BackupTo(ctx, backupPath); err != nil {
		return "", fmt.Errorf("导入前备份失败（已中止）: %w", err)
	}

	// 复制上传的文件覆盖原库。
	if err := copyFile(src, d.path); err != nil {
		return backupPath, fmt.Errorf("覆盖数据库失败: %w", err)
	}

	// WAL / SHM 是旧库的残留，不清掉会让新库读到旧的未提交数据。
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Remove(d.path + suffix)
	}
	return backupPath, nil
}

// copyFile 复制文件内容。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	// Sync 之后再关：否则断电可能留下截断的库文件。
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
