package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
)

// PersonRepo 负责用户信息读写。
type PersonRepo struct {
	db *DB
}

// Persons 返回用户仓库。
func (d *DB) Persons() *PersonRepo { return &PersonRepo{db: d} }

// Upsert 插入或更新一个用户。
func (r *PersonRepo) Upsert(ctx context.Context, p *domain.Person) error {
	_, err := r.db.sql.ExecContext(ctx, `
		INSERT INTO persons (platform, user_id, nickname, card_name, first_seen, last_seen)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(platform, user_id) DO UPDATE SET
			nickname  = CASE WHEN excluded.nickname  <> '' THEN excluded.nickname  ELSE persons.nickname  END,
			card_name = CASE WHEN excluded.card_name <> '' THEN excluded.card_name ELSE persons.card_name END,
			last_seen = excluded.last_seen
	`,
		p.Platform, p.UserID, p.Nickname, p.CardName,
		p.FirstSeen.Unix(), p.LastSeenAt.Unix(),
	)
	return err
}

// Get 按平台与用户 ID 查用户。
func (r *PersonRepo) Get(ctx context.Context, platform, userID string) (*domain.Person, error) {
	row := r.db.sql.QueryRowContext(ctx, `
		SELECT platform, user_id, nickname, card_name, first_seen, last_seen
		FROM persons WHERE platform = ? AND user_id = ?
	`, platform, userID)

	var p domain.Person
	var first, last int64
	err := row.Scan(&p.Platform, &p.UserID, &p.Nickname, &p.CardName, &first, &last)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	p.FirstSeen = time.Unix(first, 0)
	p.LastSeenAt = time.Unix(last, 0)
	return &p, nil
}

// Count 返回用户总数。
func (r *PersonRepo) Count(ctx context.Context) (int64, error) {
	var n int64
	err := r.db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM persons`).Scan(&n)
	return n, err
}

// UsageRepo 负责模型调用记录。
type UsageRepo struct {
	db *DB
}

// Usage 返回用量仓库。
func (d *DB) Usage() *UsageRepo { return &UsageRepo{db: d} }

// Record 记录一次模型调用。
func (r *UsageRepo) Record(ctx context.Context, u *UsageRecord) error {
	_, err := r.db.sql.ExecContext(ctx, `
		INSERT INTO llm_usage (
			task, model, session_id, prompt_tokens, completion_tokens,
			total_tokens, cost, duration_ms, created_at
		) VALUES (?,?,?,?,?,?,?,?,?)
	`,
		u.Task, u.Model, u.SessionID, u.PromptTokens, u.CompletionTokens,
		u.TotalTokens, u.Cost, u.DurationMS, u.CreatedAt.Unix(),
	)
	return err
}

// UsageRecord 是一条调用记录。
type UsageRecord struct {
	Task             string
	Model            string
	SessionID        string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	Cost             float64
	DurationMS       int64
	CreatedAt        time.Time
}

// UsageSummary 是用量汇总。
type UsageSummary struct {
	Calls            int64   `json:"calls"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	Cost             float64 `json:"cost"`
}

// Summary 返回某时刻之后的用量汇总。
func (r *UsageRepo) Summary(ctx context.Context, since time.Time) (UsageSummary, error) {
	var s UsageSummary
	err := r.db.sql.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(prompt_tokens),0),
		       COALESCE(SUM(completion_tokens),0),
		       COALESCE(SUM(total_tokens),0),
		       COALESCE(SUM(cost),0)
		FROM llm_usage WHERE created_at >= ?
	`, since.Unix()).Scan(&s.Calls, &s.PromptTokens, &s.CompletionTokens, &s.TotalTokens, &s.Cost)
	return s, err
}

// ByTask 返回按任务分组的用量。
func (r *UsageRepo) ByTask(ctx context.Context, since time.Time) ([]TaskUsage, error) {
	rows, err := r.db.sql.QueryContext(ctx, `
		SELECT task, COUNT(*), COALESCE(SUM(total_tokens),0), COALESCE(SUM(cost),0)
		FROM llm_usage WHERE created_at >= ?
		GROUP BY task ORDER BY SUM(total_tokens) DESC
	`, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TaskUsage
	for rows.Next() {
		var t TaskUsage
		if err := rows.Scan(&t.Task, &t.Calls, &t.TotalTokens, &t.Cost); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TaskUsage 是单个任务的用量。
type TaskUsage struct {
	Task        string  `json:"task"`
	Calls       int64   `json:"calls"`
	TotalTokens int64   `json:"total_tokens"`
	Cost        float64 `json:"cost"`
}

// KVRepo 是键值存储。
type KVRepo struct {
	db *DB
}

// KV 返回键值仓库。
func (d *DB) KV() *KVRepo { return &KVRepo{db: d} }

// ErrNotFound 表示要操作的记录不存在。
//
// 单独一个错误而不是返回 nil：删除接口把「删掉了」和「本来就没有」
// 混为一谈时，前端无法区分「操作成功」和「ID 传错了」。
var ErrNotFound = errors.New("记录不存在")

// ErrKVNotFound 表示键不存在。
//
// 以前 Get 未命中返回 ("", nil)，与「键存在但值为空串」完全分不清。
// 调用方拿空串当「没配过」是对的，但想区分「删除」和「清空」就没有办法。
// 现在把不存在做成显式错误，需要旧语义的调用方用 Lookup。
var ErrKVNotFound = errors.New("kv: 键不存在")

// Get 读取一个键，键不存在时返回 ErrKVNotFound。
func (r *KVRepo) Get(ctx context.Context, key string) (string, error) {
	var v string
	err := r.db.sql.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, key).Scan(&v)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrKVNotFound
		}
		return "", err
	}
	return v, nil
}

// Lookup 读取一个键，键不存在时返回 ("", false, nil)。
//
// 这是「没有就从头开始」这类调用方要的语义：游标、计数器、
// 首次运行时的空状态都属于这种，它们不该被当成错误。
func (r *KVRepo) Lookup(ctx context.Context, key string) (string, bool, error) {
	v, err := r.Get(ctx, key)
	if errors.Is(err, ErrKVNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// Set 写入一个键。
func (r *KVRepo) Set(ctx context.Context, key, value string) error {
	_, err := r.db.sql.ExecContext(ctx, `
		INSERT INTO kv (key, value, updated_at) VALUES (?,?,?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
	`, key, value, time.Now().Unix())
	return err
}

// Delete 删除一个键。
func (r *KVRepo) Delete(ctx context.Context, key string) error {
	_, err := r.db.sql.ExecContext(ctx, `DELETE FROM kv WHERE key = ?`, key)
	return err
}
