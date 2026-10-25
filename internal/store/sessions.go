package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
)

// SessionRepo 负责聊天流读写。
type SessionRepo struct {
	db *DB
}

// Sessions 返回聊天流仓库。
func (d *DB) Sessions() *SessionRepo { return &SessionRepo{db: d} }

// Upsert 插入或更新一条聊天流。
//
// 冲突分支按传入的 MessageCount 累加，不再硬编码 +1。
// 原来写死 `message_count + 1` 的后果：正常收发一条 +1 是对的，
// 但批量补录（例如出站落库补历史）会把 10 条算成 1 条，
// 而且这种偏差不报错，只表现为 WebUI 上的条数长期偏小。
//
// MessageCount <= 0 时按 1 处理：调用方只是「碰一下」会话，
// 没有具体的增量语义。
func (r *SessionRepo) Upsert(ctx context.Context, s *domain.Session) error {
	delta := s.MessageCount
	if delta <= 0 {
		delta = 1
	}
	_, err := r.db.sql.ExecContext(ctx, `
		INSERT INTO sessions (
			id, platform, account_id, scope, target_id, display_name,
			message_count, last_active_at, created_at
		) VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			display_name   = CASE WHEN excluded.display_name <> '' THEN excluded.display_name ELSE sessions.display_name END,
			message_count  = sessions.message_count + excluded.message_count,
			last_active_at = MAX(sessions.last_active_at, excluded.last_active_at)
	`,
		s.ID, s.Platform, s.AccountID, s.Scope, s.TargetID, s.DisplayName,
		delta, s.LastActiveAt.Unix(), s.CreatedAt.Unix(),
	)
	return err
}

// Get 按 ID 查聊天流。
func (r *SessionRepo) Get(ctx context.Context, id string) (*domain.Session, error) {
	row := r.db.sql.QueryRowContext(ctx, `
		SELECT id, platform, account_id, scope, target_id, display_name,
		       message_count, last_active_at, created_at
		FROM sessions WHERE id = ?
	`, id)

	var s domain.Session
	var lastActive, created int64
	err := row.Scan(&s.ID, &s.Platform, &s.AccountID, &s.Scope, &s.TargetID,
		&s.DisplayName, &s.MessageCount, &lastActive, &created)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	s.LastActiveAt = time.Unix(lastActive, 0)
	s.CreatedAt = time.Unix(created, 0)
	return &s, nil
}

// SetDisplayName 更新展示名。
//
// QQ 官方事件通常不带群名，需要外部补齐。
// 名字为空、或与现有值相同时直接返回，不发无谓的 UPDATE。
//
// 对不存在的会话现在会报错而不是静默成功 —— 静默成功让调用方
// 完全无法发现「会话 ID 拼错了」这类问题（0 行受影响但不报错）。
func (r *SessionRepo) SetDisplayName(ctx context.Context, id, name string) error {
	name = strings.TrimSpace(name)
	if id == "" || name == "" {
		return nil
	}

	cur, err := r.Get(ctx, id)
	if err != nil {
		return err
	}
	if cur == nil {
		// 返回 ErrNotFound 而不是临时拼的字符串：
		// HTTP 层要靠 errors.Is 区分「会话不存在」(404)
		// 和「库挂了」(500)，字符串匹配会把两者混在一起。
		return ErrNotFound
	}
	if cur.DisplayName == name {
		return nil
	}

	_, err = r.db.sql.ExecContext(ctx,
		`UPDATE sessions SET display_name = ? WHERE id = ?`, name, id)
	return err
}

// List 返回聊天流列表，按最近活跃倒序。
func (r *SessionRepo) List(ctx context.Context, limit int) ([]*domain.Session, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.db.sql.QueryContext(ctx, `
		SELECT id, platform, account_id, scope, target_id, display_name,
		       message_count, last_active_at, created_at
		FROM sessions ORDER BY last_active_at DESC LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*domain.Session
	for rows.Next() {
		var s domain.Session
		var lastActive, created int64
		if err := rows.Scan(&s.ID, &s.Platform, &s.AccountID, &s.Scope, &s.TargetID,
			&s.DisplayName, &s.MessageCount, &lastActive, &created); err != nil {
			return nil, err
		}
		s.LastActiveAt = time.Unix(lastActive, 0)
		s.CreatedAt = time.Unix(created, 0)
		out = append(out, &s)
	}
	return out, rows.Err()
}

// Count 返回聊天流总数。
func (r *SessionRepo) Count(ctx context.Context) (int64, error) {
	var n int64
	err := r.db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&n)
	return n, err
}

// Delete 删除一个会话及其全部消息。
//
// 参考实现的 delete_chat_session 会连带清理这个会话的学习数据
// （chat/routes.py:1165 `_delete_chat_session_scope`）：表达、黑话、
// 记忆摘要、画像。不清理的话，会话删了、记忆页里还挂着一堆
// 属于不存在会话的条目，看起来像删不掉。
//
// 整个删除放在一个事务里：中间失败会留下「消息删了、会话还在」
// 的半截状态，而前端只看到一次报错，完全无法察觉数据已经不完整。
func (r *SessionRepo) Delete(ctx context.Context, sessionID string) error {
	return r.db.tx(ctx, func(tx *sql.Tx) error {
		// 先确认存在，避免「删了个拼错的 ID 也返回成功」。
		var n int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sessions WHERE id = ?`, sessionID).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}

		// 会话删除要连带的东西，按依赖顺序清理。
		for _, stmt := range []string{
			`DELETE FROM messages WHERE session_id = ?`,
			`DELETE FROM expressions WHERE session_id = ?`,
			`DELETE FROM person_profiles WHERE session_id = ?`,
			`DELETE FROM memory_summaries WHERE session_id = ?`,
			`DELETE FROM sessions WHERE id = ?`,
		} {
			if _, err := tx.ExecContext(ctx, stmt, sessionID); err != nil {
				return err
			}
		}
		return nil
	})
}
