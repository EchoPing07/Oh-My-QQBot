package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// ReasoningRepo 负责推理记录，用于 WebUI 的推理重放。
type ReasoningRepo struct {
	db *DB
}

// Reasoning 返回推理记录仓库。
func (d *DB) Reasoning() *ReasoningRepo { return &ReasoningRepo{db: d} }

// ReasoningRecord 是一条推理记录。
type ReasoningRecord struct {
	ID        int64  `json:"id"`
	SessionID string `json:"session_id"`
	TurnID    string `json:"turn_id"`
	Round     int    `json:"round"`
	Kind      string `json:"kind"`
	Request   string `json:"request"`
	Response  string `json:"response"`
	CreatedAt int64  `json:"created_at"`
	// Name 是事件名：工具名、任务名（planner/replyer）、门控动作等。
	//
	// 参考实现的每个事件都有自己的名字，前端据此选择渲染器
	// （maisaka-monitor.tsx:2273 的 switch (entry.type)）。
	Name string `json:"name,omitempty"`
	// Failed 表示这次事件是否失败。
	//
	// 刻意用「失败」而不是「成功」：Go 的 bool 零值是 false，
	// 如果字段叫 OK，那「忘了设置」就等于「失败」，一堆记录会被误标。
	// 叫 Failed 时「忘了设置」等于「成功」，这才是安全的默认。
	Failed bool `json:"failed,omitempty"`
	// DurationMS 是这个事件自己的耗时。
	//
	// 和 CreatedAtMS 分开：CreatedAtMS 是「什么时候发生」，
	// DurationMS 是「花了多久」。前者用于排序，后者用于显示。
	DurationMS int64 `json:"duration_ms,omitempty"`
	// Payload 是事件的补充信息（JSON 字符串）。
	//
	// 工具事件放 tool_args / result_summary；门控事件放输入与结论。
	// 用 TEXT 而不是结构化列：不同 kind 的字段差异太大，
	// 拆成列会有大量 NULL。
	Payload string `json:"payload,omitempty"`
	// Parent 指向关联的上级记录 ID（工具执行 → 它所属的 Planner round）。
	//
	// 0 表示没有上级。
	Parent int64 `json:"parent,omitempty"`
	// CreatedAtMS 是毫秒时间戳。
	//
	// 时间线要把思考与消息混排，而消息侧是毫秒。只靠秒级 created_at
	// 无法判断「同一秒内先发消息还是先思考」，表现是思考块偶尔
	// 跑到它对应的那条消息上面去。
	CreatedAtMS int64 `json:"created_at_ms"`
}

// reasoningColumns 是读取推理记录的列清单。
//
// 抽成常量是因为有多处查询要扫同一个结构体。漏一列的表现是
// 「某个字段永远是零值」，编译期完全看不出来 ——
// messages 表已经因为这个踩过一次（AGENTS.md 存储层欠账第 2 条）。
const reasoningColumns = "id, session_id, turn_id, round, kind, name, ok, duration_ms, payload, parent, request, response, created_at, created_at_ms"

// scanOK 把数据库里「ok=1」的列扫成「Failed」。
//
// 存在的理由：列名是 ok（成功），而 Go 字段叫 Failed（失败）。
// 直接 &rec.Failed 会得到相反的值 —— 库里 1 表示成功，
// 扫进来就成了 Failed=true，所有成功的记录都会被显示成失败。
// 这种错误编译期看不出来，只能靠这个包装挡一次。
type scanOK struct{ failed *bool }

func (s scanOK) Scan(v any) error {
	n, ok := v.(int64)
	if !ok {
		return fmt.Errorf("reasoning_records.ok 期望整数，实际 %T", v)
	}
	*s.failed = n == 0
	return nil
}

// reasoningScanDest 给 Scan 用的目标切片，顺序必须与 reasoningColumns 一致。
func reasoningScanDest(rec *ReasoningRecord) []any {
	return []any{
		&rec.ID, &rec.SessionID, &rec.TurnID, &rec.Round, &rec.Kind,
		&rec.Name, scanOK{failed: &rec.Failed}, &rec.DurationMS, &rec.Payload, &rec.Parent,
		&rec.Request, &rec.Response, &rec.CreatedAt, &rec.CreatedAtMS,
	}
}

// Save 写入一条推理记录。
func (r *ReasoningRepo) Save(ctx context.Context, rec *ReasoningRecord) error {
	if rec.CreatedAtMS == 0 {
		rec.CreatedAtMS = rec.CreatedAt * 1000
	}
	res, err := r.db.sql.ExecContext(ctx, `
		INSERT INTO reasoning_records
			(session_id, turn_id, round, kind, name, ok, duration_ms, payload, parent,
			 request, response, created_at, created_at_ms)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
	`, rec.SessionID, rec.TurnID, rec.Round, rec.Kind, rec.Name, !rec.Failed, rec.DurationMS,
		rec.Payload, rec.Parent, rec.Request, rec.Response, rec.CreatedAt, rec.CreatedAtMS)
	if err != nil {
		return err
	}
	// 回填自增 ID：工具执行记录要挂到它所属的 Planner round 上（Parent）。
	if id, idErr := res.LastInsertId(); idErr == nil {
		rec.ID = id
	}
	rec.ID, _ = res.LastInsertId()
	return nil
}

// ListBySession 返回某会话最近 limit 条推理记录，按时间正序。
//
// 先按 created_at DESC 取最近 limit 条再反转，保证「拿到的是最近的，
// 但读起来是从早到晚」。
//
// 排序必须带 id 作为次级键：created_at 是秒级精度，同一轮里的多次
// 工具循环会落在同一秒，只按 created_at 排出的顺序是不稳定的。
//
// 用 created_at 而不是 id 作为主排序键，是为了和 Cleanup 的口径一致 ——
// 否则补录历史（created_at 早于已有记录）时会出现「列出来的顺序」
// 和「被清理的范围」对不上的情况。
func (r *ReasoningRepo) ListBySession(ctx context.Context, sessionID string, limit int) ([]ReasoningRecord, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := r.db.sql.QueryContext(ctx, `
		SELECT `+reasoningColumns+`
		FROM reasoning_records WHERE session_id = ?
		ORDER BY created_at_ms DESC, id DESC LIMIT ?
	`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ReasoningRecord
	for rows.Next() {
		var rec ReasoningRecord
		if err := rows.Scan(reasoningScanDest(&rec)...); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// InRange 返回某会话在 [fromMS, toMS] 区间内的推理记录，按时间正序。
//
// 供 WebUI 时间线使用：消息与思考要按时间混排，所以必须能按
// 「当前这一屏消息的时间范围」取思考，而不是「最近 N 条」。
// 后者在往上翻历史时完全对不上（翻到三小时前，取回来的却是最近的思考）。
//
// toMS <= 0 表示不限上界。
func (r *ReasoningRepo) InRange(ctx context.Context, sessionID string, fromMS, toMS int64) ([]ReasoningRecord, error) {
	if sessionID == "" {
		return nil, nil
	}
	rows, err := r.db.sql.QueryContext(ctx, `
		SELECT `+reasoningColumns+`
		FROM reasoning_records
		WHERE session_id = ? AND created_at_ms >= ? AND (? <= 0 OR created_at_ms <= ?)
		ORDER BY created_at_ms ASC, id ASC
	`, sessionID, fromMS, toMS, toMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ReasoningRecord
	for rows.Next() {
		var rec ReasoningRecord
		if err := rows.Scan(reasoningScanDest(&rec)...); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// Cleanup 删除某时刻之前的推理记录。
func (r *ReasoningRepo) Cleanup(ctx context.Context, before int64) (int64, error) {
	res, err := r.db.sql.ExecContext(ctx, `DELETE FROM reasoning_records WHERE created_at < ?`, before)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
