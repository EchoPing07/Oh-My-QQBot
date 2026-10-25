package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/tokenize"
)

// EmojiRepo 负责表情包元数据。
//
// 对应 emoji_system/emoji_manager.py 的 MaiEmoji 持久化部分。
// 文件本身存在磁盘上（emoji.dir），库里只存相对路径与描述。
type EmojiRepo struct {
	db *DB
}

// Emojis 返回表情包仓库。
func (d *DB) Emojis() *EmojiRepo { return &EmojiRepo{db: d} }

// Emoji 是一个表情包。
type Emoji struct {
	ID          int64  `json:"id"`
	FileHash    string `json:"file_hash"`
	FilePath    string `json:"file_path"`
	Description string `json:"description"`
	// SourceSession / SourceUser 记录这张图是从哪来的。
	SourceSession string `json:"source_session"`
	SourceUser    string `json:"source_user"`
	UsageCount    int    `json:"usage_count"`
	LastUsedAt    int64  `json:"last_used_at"`
	CreatedAt     int64  `json:"created_at"`
}

// Save 写入表情包元数据并同步 FTS 索引。
//
// 已存在（file_hash 相同）时返回 false，调用方据此避免重复下载落盘。
func (r *EmojiRepo) Save(ctx context.Context, e *Emoji) (bool, error) {
	created := false
	err := r.db.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO emojis (
				file_hash, file_path, description, source_session,
				source_user, created_at
			) VALUES (?,?,?,?,?,?)
			ON CONFLICT(file_hash) DO NOTHING
		`, e.FileHash, e.FilePath, e.Description, e.SourceSession, e.SourceUser, e.CreatedAt)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		created = true

		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		e.ID = id

		// 描述为空时不建索引：空内容的 FTS 行只会浪费检索时的扫描。
		if e.Description == "" {
			return nil
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO emoji_fts (rowid, description, tokens) VALUES (?,?,?)
		`, id, e.Description, tokenize.TokenizeJoined(e.Description))
		return err
	})
	return created, err
}

// UpdateDescription 更新表情包描述并重建索引。
//
// VLM 分析是异步的：先落库占位，分析完成后回填描述。
func (r *EmojiRepo) UpdateDescription(ctx context.Context, id int64, description string) error {
	return r.db.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			UPDATE emojis SET description = ? WHERE id = ?
		`, description, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM emoji_fts WHERE rowid = ?`, id); err != nil {
			return err
		}
		if description == "" {
			return nil
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO emoji_fts (rowid, description, tokens) VALUES (?,?,?)
		`, id, description, tokenize.TokenizeJoined(description))
		return err
	})
}

// Search 按情绪标签检索表情包。
//
// 返回按「相关度 + 使用频率」综合排序的结果：
// bm25 越小越相关，usage_count 越大越常用，两者相加让常用且贴切的排在前面。
func (r *EmojiRepo) Search(ctx context.Context, query string, limit int) ([]*Emoji, error) {
	expr := tokenize.BuildMatchExpr(query)
	if expr == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}

	// 同 memory.go：FTS5 的 MATCH 与 bm25() 都要求完整表名，不能用别名。
	rows, err := r.db.sql.QueryContext(ctx, `
		SELECT e.id, e.file_hash, e.file_path, e.description, e.source_session,
		       e.source_user, e.usage_count, e.last_used_at, e.created_at
		FROM emoji_fts
		JOIN emojis e ON e.id = emoji_fts.rowid
		WHERE emoji_fts.tokens MATCH ?
		ORDER BY bm25(emoji_fts) - e.usage_count * 0.1
		LIMIT ?
	`, expr, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Emoji
	for rows.Next() {
		e, err := scanEmoji(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Random 随机取 limit 个表情包。
//
// 用于「随机抽取候选给模型选」的场景，不依赖描述。
func (r *EmojiRepo) Random(ctx context.Context, limit int) ([]*Emoji, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := r.db.sql.QueryContext(ctx, `
		SELECT id, file_hash, file_path, description, source_session,
		       source_user, usage_count, last_used_at, created_at
		FROM emojis ORDER BY RANDOM() LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Emoji
	for rows.Next() {
		e, err := scanEmoji(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetByHash 按内容哈希取表情包。
func (r *EmojiRepo) GetByHash(ctx context.Context, hash string) (*Emoji, error) {
	row := r.db.sql.QueryRowContext(ctx, `
		SELECT id, file_hash, file_path, description, source_session,
		       source_user, usage_count, last_used_at, created_at
		FROM emojis WHERE file_hash = ?
	`, hash)
	e, err := scanEmoji(row)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	return e, nil
}

// ExistingHashes 返回给定哈希里哪些在库中存在。
//
// 批量查而不是 GetByHash 循环：WebUI 一屏可能几十个图片段，
// 逐段查库就是几十次往返，而这里只需要一个「在不在」的布尔值。
// 查不到的直接不出现在结果里。
func (r *EmojiRepo) ExistingHashes(ctx context.Context, hashes []string) (map[string]bool, error) {
	out := make(map[string]bool, len(hashes))
	if len(hashes) == 0 {
		return out, nil
	}

	uniq := make([]string, 0, len(hashes))
	seen := make(map[string]struct{}, len(hashes))
	for _, h := range hashes {
		if h == "" {
			continue
		}
		if _, ok := seen[h]; ok {
			continue
		}
		seen[h] = struct{}{}
		uniq = append(uniq, h)
	}
	if len(uniq) == 0 {
		return out, nil
	}

	// SQLite 变量上限 999，按 500 分批。
	const batch = 500
	for start := 0; start < len(uniq); start += batch {
		end := start + batch
		if end > len(uniq) {
			end = len(uniq)
		}
		chunk := uniq[start:end]

		args := make([]any, len(chunk))
		for i, h := range chunk {
			args[i] = h
		}
		placeholders := strings.Repeat("?, ", len(chunk))
		placeholders = placeholders[:len(placeholders)-2]

		rows, err := r.db.sql.QueryContext(ctx,
			`SELECT file_hash FROM emojis WHERE file_hash IN (`+placeholders+`)`, args...)
		if err != nil {
			return out, err
		}
		for rows.Next() {
			var h string
			if err := rows.Scan(&h); err != nil {
				rows.Close()
				return out, err
			}
			out[h] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return out, err
		}
		rows.Close()
	}
	return out, nil
}

// MarkUsed 把使用次数 +1。
func (r *EmojiRepo) MarkUsed(ctx context.Context, id int64, now time.Time) error {
	_, err := r.db.sql.ExecContext(ctx, `
		UPDATE emojis SET usage_count = usage_count + 1, last_used_at = ? WHERE id = ?
	`, now.Unix(), id)
	return err
}

// Count 返回表情包总数。
func (r *EmojiRepo) Count(ctx context.Context) (int64, error) {
	var n int64
	err := r.db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM emojis`).Scan(&n)
	return n, err
}

// Delete 删除表情包元数据与索引，并返回被删文件的路径。
//
// 返回路径而不是让调用方自己查：文件必须在事务成功后删，
// 顺序反了会出现「文件没了、库里还有记录」，界面上是一堆碎图。
//
// 主表没删到行时返回 ErrNotFound。之前不看 RowsAffected，
// 于是删一个不存在的 id 也返回成功 —— 界面上会以为删掉了。
func (r *EmojiRepo) Delete(ctx context.Context, id int64) (string, error) {
	var filePath string
	err := r.db.tx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx,
			`SELECT file_path FROM emojis WHERE id = ?`, id).Scan(&filePath); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM emoji_fts WHERE rowid = ?`, id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM emojis WHERE id = ?`, id)
		return err
	})
	if err != nil {
		return "", err
	}
	return filePath, nil
}

// scanner 是 *sql.Row 与 *sql.Rows 的公共扫描接口。
type rowScanner interface {
	Scan(dest ...any) error
}

func scanEmoji(s rowScanner) (*Emoji, error) {
	var e Emoji
	err := s.Scan(
		&e.ID, &e.FileHash, &e.FilePath, &e.Description, &e.SourceSession,
		&e.SourceUser, &e.UsageCount, &e.LastUsedAt, &e.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &e, nil
}
