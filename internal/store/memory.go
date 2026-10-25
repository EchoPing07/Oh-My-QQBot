package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/tokenize"
)

// MemoryRepo 负责长期记忆的读写。
//
// 对应参考实现的 memory_service + mid_term.py 的持久化部分。
// 文本检索走 FTS5（见 memory_fts 表），没有向量。
type MemoryRepo struct {
	db *DB
}

// Memory 返回长期记忆仓库。
func (d *DB) Memory() *MemoryRepo { return &MemoryRepo{db: d} }

// MemorySummary 是一条聊天回想。
// 字段与参考实现的 MidTermMemorySummaryModel 对应。
type MemorySummary struct {
	ID           int64    `json:"id"`
	SessionID    string   `json:"session_id"`
	GroupID      string   `json:"group_id"`
	Summary      string   `json:"summary"`
	RecallCues   []string `json:"recall_cues"`
	Participants []string `json:"participants"`
	TimeRange    string   `json:"time_range"`
	SourceCount  int      `json:"source_count"`
	CoveredUntil int64    `json:"covered_until"`
	CreatedAt    int64    `json:"created_at"`

	// Embedding 是这条记忆的向量（已序列化成 float32 字节）。
	// 不导出到 JSON：1024 维的数组塞进 API 响应既没用又占带宽。
	Embedding []byte `json:"-"`
	// EmbedModel / EmbedDim 记录向量化时用的模型与维度。
	//
	// 换 embedding 模型后维度可能变化，必须能识别出旧向量并跳过 ——
	// 拿不同维度的向量算余弦相似度会得到无意义的结果（通常是 0 或随机值），
	// 表现为「检索总是空的」或「总命中不相关的东西」，极难排查。
	EmbedModel string `json:"embed_model"`
	EmbedDim   int    `json:"embed_dim"`
}

// SaveSummary 写入一条回想，并同步维护 FTS 索引。
func (r *MemoryRepo) SaveSummary(ctx context.Context, s *MemorySummary) error {
	cues, err := json.Marshal(s.RecallCues)
	if err != nil {
		return fmt.Errorf("序列化 recall_cues 失败: %w", err)
	}
	participants, err := json.Marshal(s.Participants)
	if err != nil {
		return fmt.Errorf("序列化 participants 失败: %w", err)
	}

	var id int64
	err = r.db.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO memory_summaries (
				session_id, group_id, summary, recall_cues, participants,
				time_range, source_count, covered_until, created_at,
				embedding, embed_model, embed_dim
			) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		`,
			s.SessionID, s.GroupID, s.Summary, string(cues), string(participants),
			s.TimeRange, s.SourceCount, s.CoveredUntil, s.CreatedAt,
			nullableBytes(s.Embedding), s.EmbedModel, s.EmbedDim,
		)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		if err != nil {
			return err
		}

		// FTS 索引内容 = 摘要 + 所有召回线索，两者都要能被检索到。
		content := s.Summary + "\n" + joinLines(s.RecallCues)
		_, err = tx.ExecContext(ctx, `
			INSERT INTO memory_fts (rowid, content, tokens) VALUES (?,?,?)
		`, id, content, tokenize.TokenizeJoined(content))
		return err
	})
	if err != nil {
		return fmt.Errorf("写入记忆摘要失败: %w", err)
	}
	s.ID = id
	return nil
}

// SearchSummaries 按关键词检索聊天回想。
//
// sessionID 非空时只在该会话内检索；为空时跨会话检索（用于私聊里问「之前说过什么」）。
// 返回结果按 bm25 相关度升序（FTS5 的 bm25 是越小越相关）。
func (r *MemoryRepo) SearchSummaries(ctx context.Context, sessionID, query string, limit int) ([]*MemorySummary, error) {
	expr := tokenize.BuildMatchExpr(query)
	if expr == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 5
	}

	// 注意：MATCH 左侧与 bm25() 的参数都必须是完整表名 memory_fts，
	// 不能写别名。FTS5 的 MATCH 与 bm25 都对别名支持有问题 ——
	// 写 `bm25(f)` 会报 "no such column: f"，而 `f.tokens MATCH` 报的
	// 是 "unable to use function MATCH in the requested context"。
	// 这两个错都只在真跑 SQL 时出现，编译期完全看不出来。
	sqlText := `
		SELECT m.id, m.session_id, m.group_id, m.summary, m.recall_cues,
		       m.participants, m.time_range, m.source_count, m.covered_until, m.created_at,
		       m.embed_model, m.embed_dim
		FROM memory_fts
		JOIN memory_summaries m ON m.id = memory_fts.rowid
		WHERE memory_fts.tokens MATCH ?
	`
	args := []any{expr}
	if sessionID != "" {
		// 会话内检索要带上全局记忆（session_id 为空串）。
		sqlText += ` AND (m.session_id = ? OR m.session_id = '')`
		args = append(args, sessionID)
	}
	sqlText += ` ORDER BY bm25(memory_fts) LIMIT ?`
	args = append(args, limit)

	rows, err := r.db.sql.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("检索记忆失败: %w", err)
	}
	defer rows.Close()

	var out []*MemorySummary
	for rows.Next() {
		s, err := scanSummary(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// LoadSummariesForVectorSearch 载入候选记忆，供上层做向量相似度排序。
//
// 为什么把「载入」和「排序」分开、把排序放在上层：
// SQLite 没有向量索引（不引入 sqlite-vec 这类扩展），只能全表扫。
// 与其在 store 里硬编码一个「取最近 N 条」的策略，不如把候选集交给
// 上层决定 —— 它知道当前会话、时间窗口、以及向量模型是否可用。
//
// dim 非零时只返回维度匹配且模型匹配的记录；其余视为不可用（跳过）。
// 这样换模型后不会拿旧向量算出垃圾结果。
func (r *MemoryRepo) LoadSummariesForVectorSearch(ctx context.Context, sessionID, model string, dim int, limit int) ([]*MemorySummary, error) {
	if limit <= 0 {
		limit = 200
	}

	sqlText := `
		SELECT id, session_id, group_id, summary, recall_cues, participants,
		       time_range, source_count, covered_until, created_at,
		       embedding, embed_model, embed_dim
		FROM memory_summaries
		WHERE embedding IS NOT NULL AND embed_model = ? AND embed_dim = ?
	`
	args := []any{model, dim}
	if sessionID != "" {
		// 会话内检索要带上全局记忆（session_id 为空串）。
		sqlText += ` AND (session_id = ? OR session_id = '')`
		args = append(args, sessionID)
	}
	sqlText += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := r.db.sql.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("载入记忆向量失败: %w", err)
	}
	defer rows.Close()

	var out []*MemorySummary
	for rows.Next() {
		s, err := scanSummaryWithEmbedding(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// PendingEmbedding 返回还没有向量、或向量属于旧模型的记忆。
//
// 用于两种场景：
//   - 首次开启向量检索时回填历史记忆
//   - 换 embedding 模型后重新向量化
func (r *MemoryRepo) PendingEmbedding(ctx context.Context, model string, dim int, limit int) ([]*MemorySummary, error) {
	if limit <= 0 {
		limit = 50
	}

	rows, err := r.db.sql.QueryContext(ctx, `
		SELECT id, session_id, group_id, summary, recall_cues, participants,
		       time_range, source_count, covered_until, created_at,
		       embedding, embed_model, embed_dim
		FROM memory_summaries
		WHERE embedding IS NULL OR embed_model <> ? OR embed_dim <> ?
		ORDER BY id ASC LIMIT ?
	`, model, dim, limit)
	if err != nil {
		return nil, fmt.Errorf("查询待向量化记忆失败: %w", err)
	}
	defer rows.Close()

	var out []*MemorySummary
	for rows.Next() {
		s, err := scanSummaryWithEmbedding(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// UpdateEmbedding 写入一条记忆的向量。
func (r *MemoryRepo) UpdateEmbedding(ctx context.Context, id int64, vec []byte, model string, dim int) error {
	_, err := r.db.sql.ExecContext(ctx, `
		UPDATE memory_summaries SET embedding = ?, embed_model = ?, embed_dim = ?
		WHERE id = ?
	`, vec, model, dim, id)
	if err != nil {
		return fmt.Errorf("写入记忆向量失败: %w", err)
	}
	return nil
}

// CountPendingEmbedding 返回待向量化的条数，供启动日志与 WebUI 展示。
func (r *MemoryRepo) CountPendingEmbedding(ctx context.Context, model string, dim int) (int64, error) {
	var n int64
	err := r.db.sql.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM memory_summaries
		WHERE embedding IS NULL OR embed_model <> ? OR embed_dim <> ?
	`, model, dim).Scan(&n)
	return n, err
}

// SearchSummariesByTime 按时间范围检索回想。
//
// 对应 query_memory 的 mode=time。参考实现用 ISO 时间串，
// 这里直接收 Unix 秒，由工具层负责解析用户给的日期。
func (r *MemoryRepo) SearchSummariesByTime(ctx context.Context, sessionID string, start, end int64, limit int) ([]*MemorySummary, error) {
	if limit <= 0 {
		limit = 5
	}
	sqlText := `
		SELECT id, session_id, group_id, summary, recall_cues, participants,
		       time_range, source_count, covered_until, created_at, embed_model, embed_dim
		FROM memory_summaries
		WHERE 1=1
	`
	var args []any
	if sessionID != "" {
		sqlText += ` AND (session_id = ? OR session_id = '')`
		args = append(args, sessionID)
	}
	if start > 0 {
		sqlText += ` AND created_at >= ?`
		args = append(args, start)
	}
	if end > 0 {
		sqlText += ` AND created_at <= ?`
		args = append(args, end)
	}
	sqlText += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := r.db.sql.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("按时间检索记忆失败: %w", err)
	}
	defer rows.Close()

	var out []*MemorySummary
	for rows.Next() {
		s, err := scanSummary(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// LastCoveredUntil 返回某会话已被摘要覆盖到的时间戳。
// 增量摘要靠它判断从哪之后的消息还没总结。
func (r *MemoryRepo) LastCoveredUntil(ctx context.Context, sessionID string) (int64, error) {
	var v int64
	err := r.db.sql.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(covered_until), 0) FROM memory_summaries WHERE session_id = ?`,
		sessionID,
	).Scan(&v)
	if err != nil && !isNoRows(err) {
		return 0, err
	}
	return v, nil
}

// CountSummaries 返回回想总数。
func (r *MemoryRepo) CountSummaries(ctx context.Context) (int64, error) {
	var n int64
	err := r.db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_summaries`).Scan(&n)
	return n, err
}

// DeleteSummary 删除一条回想，同时清理 FTS 索引。
//
// 必须手动删 FTS：外部内容模式不会自动同步。
func (r *MemoryRepo) DeleteSummary(ctx context.Context, id int64) error {
	return r.db.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM memory_fts WHERE rowid = ?`, id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM memory_summaries WHERE id = ?`, id)
		return err
	})
}

// scanSummary 读一行回想。
//
// 必须把 embed_model / embed_dim 一起读出来：WebUI 靠它们判断
// 「这条记忆是否已向量化」，只读向量列（或干脆不读）会让界面
// 把所有记忆都当成「仅关键词」，而实际上向量是好的。
// 向量本体不在这里读 —— 列表展示用不上它，1024 维很占内存。
func scanSummary(rows *sql.Rows) (*MemorySummary, error) {
	var (
		s            MemorySummary
		recallRaw    string
		participants string
	)
	err := rows.Scan(
		&s.ID, &s.SessionID, &s.GroupID, &s.Summary, &recallRaw,
		&participants, &s.TimeRange, &s.SourceCount, &s.CoveredUntil, &s.CreatedAt,
		&s.EmbedModel, &s.EmbedDim,
	)
	if err != nil {
		return nil, err
	}
	if recallRaw != "" {
		if err := json.Unmarshal([]byte(recallRaw), &s.RecallCues); err != nil {
			// 历史数据损坏不该让整个检索失败，降级为空线索。
			s.RecallCues = nil
		}
	}
	if participants != "" {
		if err := json.Unmarshal([]byte(participants), &s.Participants); err != nil {
			s.Participants = nil
		}
	}
	return &s, nil
}

func joinLines(items []string) string {
	if len(items) == 0 {
		return ""
	}
	out := ""
	for i, it := range items {
		if i > 0 {
			out += "\n"
		}
		out += it
	}
	return out
}

// ============ 人物画像 ============

// PersonProfile 是一个人的画像。
//
// 全部带 snake_case json tag：这些结构体直接进 API 响应，
// 没有 tag 会输出 Go 字段名（Platform / ProfileText），
// 前端按惯例读 profile_text 就永远是 undefined。
type PersonProfile struct {
	Platform    string `json:"platform"`
	UserID      string `json:"user_id"`
	SessionID   string `json:"session_id"`
	DisplayName string `json:"display_name"`
	ProfileText string `json:"profile_text"`
	// MessageCount 是生成这条画像时累计的本人消息数。
	MessageCount int `json:"message_count"`
	// LastMessageAt 是最后一次参与聊天的时刻（秒级）。
	LastMessageAt int64 `json:"last_message_at"`
	// UpdatedAt 是画像最后更新的时刻（秒级）。
	UpdatedAt int64 `json:"updated_at"`
}

// UpsertProfile 写入或更新画像。
func (r *MemoryRepo) UpsertProfile(ctx context.Context, p *PersonProfile) error {
	_, err := r.db.sql.ExecContext(ctx, `
		INSERT INTO person_profiles (
			platform, user_id, session_id, display_name, profile_text,
			message_count, last_message_at, updated_at
		) VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(platform, user_id, session_id) DO UPDATE SET
			display_name   = excluded.display_name,
			profile_text   = excluded.profile_text,
			message_count  = excluded.message_count,
			last_message_at = excluded.last_message_at,
			updated_at     = excluded.updated_at
	`,
		p.Platform, p.UserID, p.SessionID, p.DisplayName, p.ProfileText,
		p.MessageCount, p.LastMessageAt, p.UpdatedAt,
	)
	return err
}

// GetProfile 按平台、用户、会话取画像。
//
// 先找会话专属画像，没有则回退到跨会话画像（session_id 为空串）。
// 这个回退很关键：群里认识的人，私聊时也应该认得。
func (r *MemoryRepo) GetProfile(ctx context.Context, platform, userID, sessionID string) (*PersonProfile, error) {
	row := r.db.sql.QueryRowContext(ctx, `
		SELECT platform, user_id, session_id, display_name, profile_text,
		       message_count, last_message_at, updated_at
		FROM person_profiles
		WHERE platform = ? AND user_id = ?
		ORDER BY CASE WHEN session_id = ? THEN 0 ELSE 1 END, updated_at DESC
		LIMIT 1
	`, platform, userID, sessionID)

	var p PersonProfile
	err := row.Scan(
		&p.Platform, &p.UserID, &p.SessionID, &p.DisplayName, &p.ProfileText,
		&p.MessageCount, &p.LastMessageAt, &p.UpdatedAt,
	)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	return &p, nil
}

// CountProfiles 返回画像总数。
//
// 注意这是「画像记录数」而不是「人数」：同一个人在不同群里
// 各有一条画像（session_id 不同），所以这个数会大于实际人数。
func (r *MemoryRepo) CountProfiles(ctx context.Context) (int64, error) {
	var n int64
	err := r.db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM person_profiles`).Scan(&n)
	return n, err
}

// ListProfiles 返回画像列表，按最近更新倒序。
//
// sessionID 非空时只返回该会话的画像；为空时返回全部。
//
// 之前没有这个方法，WebUI 只能按 user_id 查单条，管理页因此
// 无法列出「认识了哪些人」。画像本身是模型根据聊天内容生成的摘要，
// 不是原始聊天记录，列出摘要用于检查质量是合理的。
func (r *MemoryRepo) ListProfiles(ctx context.Context, sessionID string, limit int) ([]*PersonProfile, error) {
	if limit <= 0 {
		limit = 100
	}

	sqlText := `
		SELECT platform, user_id, session_id, display_name, profile_text,
		       message_count, last_message_at, updated_at
		FROM person_profiles
	`
	var args []any
	if sessionID != "" {
		sqlText += ` WHERE session_id = ?`
		args = append(args, sessionID)
	}
	sqlText += ` ORDER BY updated_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := r.db.sql.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("查询画像列表失败: %w", err)
	}
	defer rows.Close()

	var out []*PersonProfile
	for rows.Next() {
		var p PersonProfile
		if err := rows.Scan(
			&p.Platform, &p.UserID, &p.SessionID, &p.DisplayName, &p.ProfileText,
			&p.MessageCount, &p.LastMessageAt, &p.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, &p)
	}
	return out, rows.Err()
}

// ============ 表达学习 ============

// Expression 是一条学到的表达方式。
type Expression struct {
	ID        int64  `json:"id"`
	SessionID string `json:"session_id"`
	// Situation 是「什么情境下说」，Style 是「怎么说的」。
	Situation  string `json:"situation"`
	Style      string `json:"style"`
	SourceText string `json:"source_text"`
	UsedCount  int    `json:"used_count"`
	LastUsedAt int64  `json:"last_used_at"`
	CreatedAt  int64  `json:"created_at"`
}

// SaveExpression 写入一条表达，重复则忽略。
//
// 返回 true 表示是新增的（不是已存在的重复项），用于统计学习进度。
func (r *MemoryRepo) SaveExpression(ctx context.Context, e *Expression) (bool, error) {
	res, err := r.db.sql.ExecContext(ctx, `
		INSERT INTO expressions (session_id, situation, style, source_text, created_at)
		VALUES (?,?,?,?,?)
		ON CONFLICT(session_id, situation, style) DO NOTHING
	`, e.SessionID, e.Situation, e.Style, e.SourceText, e.CreatedAt)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ListAllExpressions 返回所有会话的表达，供 WebUI 展示。
//
// 与 ListExpressions 的区别很重要：
// 后者是「给某个会话挑几条注入」，必须按 session_id 过滤（含全局）；
// 前者是「管理界面要看全库」，不过滤 —— 否则在管理页里
// 只会看到 session_id 为空的那几条，其余会话学到的全都看不见。
func (r *MemoryRepo) ListAllExpressions(ctx context.Context, limit int) ([]*Expression, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.sql.QueryContext(ctx, `
		SELECT id, session_id, situation, style, source_text, used_count, last_used_at, created_at
		FROM expressions
		ORDER BY created_at DESC, id DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanExpressions(rows)
}

// ListExpressions 返回某会话可用的表达，按使用次数升序（少用的优先展示以增加多样性）。
//
// 包含跨会话通用的表达（session_id 为空串）。
func (r *MemoryRepo) ListExpressions(ctx context.Context, sessionID string, limit int) ([]*Expression, error) {
	if limit <= 0 {
		limit = 5
	}
	rows, err := r.db.sql.QueryContext(ctx, `
		SELECT id, session_id, situation, style, source_text, used_count, last_used_at, created_at
		FROM expressions
		WHERE session_id = ? OR session_id = ''
		ORDER BY used_count ASC, RANDOM()
		LIMIT ?
	`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanExpressions(rows)
}

// scanExpressions 把结果集扫成表达列表。
func scanExpressions(rows *sql.Rows) ([]*Expression, error) {
	var out []*Expression
	for rows.Next() {
		var e Expression
		if err := rows.Scan(&e.ID, &e.SessionID, &e.Situation, &e.Style,
			&e.SourceText, &e.UsedCount, &e.LastUsedAt, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

// MarkExpressionsUsed 把用过的表达次数 +1。
func (r *MemoryRepo) MarkExpressionsUsed(ctx context.Context, ids []int64, now time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	return r.db.tx(ctx, func(tx *sql.Tx) error {
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `
				UPDATE expressions SET used_count = used_count + 1, last_used_at = ?
				WHERE id = ?
			`, now.Unix(), id); err != nil {
				return err
			}
		}
		return nil
	})
}

// CountExpressions 返回表达总数。
func (r *MemoryRepo) CountExpressions(ctx context.Context) (int64, error) {
	var n int64
	err := r.db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM expressions`).Scan(&n)
	return n, err
}

// ============ 黑话 ============

// Jargon 是一条黑话。
//
// Count / LastInferenceCount 用来决定何时推断含义：
// 黑话的含义要靠多次不同语境才能确定，见 internal/learning/jargon_meaning.go。
type Jargon struct {
	ID      int64  `json:"id"`
	Content string `json:"content"`
	Meaning string `json:"meaning"`
	Context string `json:"context"`
	// Count 是这个词出现过的次数，到达 4/8/25/100 阈值才触发含义推断。
	Count              int   `json:"count"`
	LastInferenceCount int   `json:"last_inference_count"`
	CreatedAt          int64 `json:"created_at"`
	UpdatedAt          int64 `json:"updated_at"`
}

// SaveJargon 写入黑话；已存在则命中次数 +1。
//
// 命中次数是「推断含义的触发器」，所以重复遇到必须累加 ——
// 否则一条黑话永远停在 count=1，永远不会被推断。
func (r *MemoryRepo) SaveJargon(ctx context.Context, j *Jargon) error {
	count := j.Count
	if count <= 0 {
		count = 1
	}
	_, err := r.db.sql.ExecContext(ctx, `
		INSERT INTO jargons (content, meaning, context, count, created_at, updated_at)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(content) DO UPDATE SET
			meaning    = CASE WHEN excluded.meaning <> '' THEN excluded.meaning ELSE jargons.meaning END,
			context    = CASE WHEN excluded.context <> '' THEN excluded.context ELSE jargons.context END,
			count      = jargons.count + 1,
			updated_at = excluded.updated_at
	`, j.Content, j.Meaning, j.Context, count, j.CreatedAt, j.UpdatedAt)
	return err
}

// IncrementJargonCount 把命中次数 +1，并返回新值。
//
// 与 SaveJargon 的区别：这个只改计数，不碰含义与上下文，
// 用于「已经学会的词又被用了一次」这种场景。
func (r *MemoryRepo) IncrementJargonCount(ctx context.Context, content string, now int64) (int, error) {
	if _, err := r.db.sql.ExecContext(ctx, `
		UPDATE jargons SET count = count + 1, updated_at = ? WHERE content = ?
	`, now, content); err != nil {
		return 0, err
	}

	var count int
	err := r.db.sql.QueryRowContext(ctx, `SELECT count FROM jargons WHERE content = ?`, content).Scan(&count)
	if err != nil {
		if isNoRows(err) {
			return 0, nil
		}
		return 0, err
	}
	return count, nil
}

// UpdateJargonMeaning 写入推断出的含义，并记录本次推断时的 count。
//
// last_inference_count 必须一起写：它是「下次还要不要推断」的判据，
// 漏写会导致同一条黑话在每个阈值上被反复推断。
func (r *MemoryRepo) UpdateJargonMeaning(ctx context.Context, id int64, meaning string, atCount int, now int64) error {
	_, err := r.db.sql.ExecContext(ctx, `
		UPDATE jargons
		SET meaning = ?, last_inference_count = ?, updated_at = ?
		WHERE id = ?
	`, meaning, atCount, now, id)
	return err
}

// GetJargon 按词条内容取黑话。
func (r *MemoryRepo) GetJargon(ctx context.Context, content string) (*Jargon, error) {
	row := r.db.sql.QueryRowContext(ctx, `
		SELECT id, content, meaning, context, count, last_inference_count, created_at, updated_at
		FROM jargons WHERE content = ?
	`, content)

	var j Jargon
	err := row.Scan(&j.ID, &j.Content, &j.Meaning, &j.Context, &j.Count,
		&j.LastInferenceCount, &j.CreatedAt, &j.UpdatedAt)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	return &j, nil
}

// ListJargons 返回最近学到的黑话。
func (r *MemoryRepo) ListJargons(ctx context.Context, limit int) ([]*Jargon, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := r.db.sql.QueryContext(ctx, `
		SELECT id, content, meaning, context, count, last_inference_count, created_at, updated_at
		FROM jargons ORDER BY updated_at DESC LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Jargon
	for rows.Next() {
		var j Jargon
		if err := rows.Scan(&j.ID, &j.Content, &j.Meaning, &j.Context, &j.Count,
			&j.LastInferenceCount, &j.CreatedAt, &j.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, &j)
	}
	return out, rows.Err()
}

// CountJargons 返回黑话总数。
func (r *MemoryRepo) CountJargons(ctx context.Context) (int64, error) {
	var n int64
	err := r.db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM jargons`).Scan(&n)
	return n, err
}

// scanSummaryWithEmbedding 扫描带向量的结果行。
func scanSummaryWithEmbedding(rows *sql.Rows) (*MemorySummary, error) {
	var (
		s            MemorySummary
		recallRaw    string
		participants string
		embedding    []byte
	)
	err := rows.Scan(
		&s.ID, &s.SessionID, &s.GroupID, &s.Summary, &recallRaw,
		&participants, &s.TimeRange, &s.SourceCount, &s.CoveredUntil, &s.CreatedAt,
		&embedding, &s.EmbedModel, &s.EmbedDim,
	)
	if err != nil {
		return nil, err
	}
	s.Embedding = embedding
	if recallRaw != "" {
		if err := json.Unmarshal([]byte(recallRaw), &s.RecallCues); err != nil {
			s.RecallCues = nil
		}
	}
	if participants != "" {
		if err := json.Unmarshal([]byte(participants), &s.Participants); err != nil {
			s.Participants = nil
		}
	}
	return &s, nil
}

// nullableBytes 把空切片转成 SQL NULL。
//
// 直接传空 []byte 会写入零长度 BLOB 而不是 NULL，
// 于是 `WHERE embedding IS NULL` 的待回填查询永远查不到它。
func nullableBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// ============ 删除 ============

// DeleteExpression 删除一条表达。
//
// 参考实现有单条与批量两条删除接口（expression.py:1816 / 1856），
// 我们只做单条：批量删除在前端循环调用即可，
// 后端多一个批量接口只多一条要维护的路径。
func (r *MemoryRepo) DeleteExpression(ctx context.Context, id int64) error {
	res, err := r.db.sql.ExecContext(ctx, `DELETE FROM expressions WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteJargon 删除一条黑话。
func (r *MemoryRepo) DeleteJargon(ctx context.Context, id int64) error {
	res, err := r.db.sql.ExecContext(ctx, `DELETE FROM jargons WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteProfile 删除一份人物画像。
//
// platform 为空时按「任意平台」删：管理页只传 user_id + session_id，
// 而用户在界面上看到的是一个人，不是「某平台上的这个人」。
func (r *MemoryRepo) DeleteProfile(ctx context.Context, platform, userID, sessionID string) (bool, error) {
	q := `DELETE FROM person_profiles WHERE user_id = ? AND session_id = ?`
	args := []any{userID, sessionID}
	if platform != "" {
		q += ` AND platform = ?`
		args = append(args, platform)
	}
	res, err := r.db.sql.ExecContext(ctx, q, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
