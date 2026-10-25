package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
)

// MessageRepo 负责消息读写。
type MessageRepo struct {
	db *DB
}

// Messages 返回消息仓库。
func (d *DB) Messages() *MessageRepo { return &MessageRepo{db: d} }

// messageColumns 是所有查询共用的列清单。
//
// 抽成常量而不是每处手抄：漏一列的表现是「某个字段重启后永远是空的」，
// 编译期完全看不出来。这个坑已经踩过一次（UserCardName / AccountID 不落库）。
const messageColumns = `
	id, session_id, message_id, platform, user_id, user_name, user_card_name,
	group_id, group_name, is_group, is_at, is_mentioned, is_command, is_self,
	account_id, additional, ref_msg_idx, segments, plain_text, created_at_ms`

// Save 写入一条消息。
//
// 消息段里的图片/语音二进制不落库，只存元信息。
// 原因很实际：一张 200KB 的图 base64 之后是 267KB 文本，
// 而 Recent 每次恢复上下文都要把这些字节读回来反序列化一遍，
// 聊天记录一多就是几十 MB 的无谓 IO。
// 二进制本身只在本轮处理里需要（多模态、attach_pic），
// 过一阵再想看图本来也该重新下载。
func (r *MessageRepo) Save(ctx context.Context, m *domain.Message) error {
	segments, err := json.Marshal(stripSegmentBinary(m.Segments))
	if err != nil {
		return err
	}
	additional, err := json.Marshal(nonEmptyMap(m.Additional))
	if err != nil {
		return err
	}
	_, err = r.db.sql.ExecContext(ctx, `
		INSERT INTO messages (
			id, session_id, message_id, platform, user_id, user_name, user_card_name,
			group_id, group_name, is_group, is_at, is_mentioned, is_command, is_self,
			account_id, additional, ref_msg_idx, segments, plain_text, created_at, created_at_ms
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO NOTHING
	`,
		m.ID, m.SessionID, m.MessageID, m.Platform, m.UserID, m.UserName, m.UserCardName,
		m.GroupID, m.GroupName, boolInt(m.IsGroup), boolInt(m.IsAt),
		boolInt(m.IsMentioned), boolInt(m.IsCommand), boolInt(m.IsSelf),
		m.AccountID, string(additional), m.RefMsgIdx,
		string(segments), m.PlainText,
		m.ReceivedAt.Unix(), receivedAtMS(m.ReceivedAt),
	)
	return err
}

// receivedAtMS 把接收时刻转成毫秒，零值退化为「现在」。
//
// 出站消息是通过构造出来的，ReceivedAt 一定是设过的；
// 但测试与外部调用方可能忘了填，落成 0 会让它在恢复上下文时排到最前面。
func receivedAtMS(t time.Time) int64 {
	if t.IsZero() {
		return time.Now().UnixMilli()
	}
	return t.UnixMilli()
}

// nonEmptyMap 让 nil map 与空 map 都序列化成 {}。
//
// nil map 会被 json.Marshal 成 "null"，读回来 unmarshal 到 map 是 nil ——
// 语义上没区别，但库里存 "null" 看起来像脏数据。
func nonEmptyMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return map[string]string{}
	}
	return m
}

// Recent 返回某会话最近 limit 条消息，按时间正序。
func (r *MessageRepo) Recent(ctx context.Context, sessionID string, limit int) ([]*domain.Message, error) {
	if limit <= 0 {
		limit = 100
	}
	return r.page(ctx, sessionID, 0, limit, false)
}

// Before 返回某会话在 beforeMS 之前的 limit 条消息，按时间正序。
//
// beforeMS <= 0 时等价于「最新的 limit 条」。
// 这是给 WebUI 往上翻历史用的：先取最新一屏，再用最早一条的时间戳做游标往回翻。
func (r *MessageRepo) Before(ctx context.Context, sessionID string, beforeMS int64, limit int) ([]*domain.Message, error) {
	if limit <= 0 {
		limit = 100
	}
	return r.page(ctx, sessionID, beforeMS, limit, false)
}

// After 返回某会话在 afterMS 之后的 limit 条消息，按时间正序。
//
// 只用于增量拉取（WebUI 轮询新消息），不是列表分页。
func (r *MessageRepo) After(ctx context.Context, sessionID string, afterMS int64, limit int) ([]*domain.Message, error) {
	if limit <= 0 {
		limit = 100
	}
	return r.page(ctx, sessionID, afterMS, limit, true)
}

// page 是三个查询共用的分页实现。
//
// after == true 时取「比游标新」的，否则取「比游标旧」的。
// 两条路径都按毫秒倒序取 limit 条再反转成时间正序 ——
// 正序 limit 会取到最早的那批，方向就错了。
func (r *MessageRepo) page(ctx context.Context, sessionID string, cursorMS int64, limit int, after bool) ([]*domain.Message, error) {
	sqlText := `SELECT ` + messageColumns + ` FROM messages WHERE session_id = ?`
	args := []any{sessionID}

	switch {
	case after && cursorMS > 0:
		sqlText += ` AND created_at_ms > ?`
		args = append(args, cursorMS)
	case cursorMS > 0:
		sqlText += ` AND created_at_ms < ?`
		args = append(args, cursorMS)
	}

	sqlText += ` ORDER BY created_at_ms DESC, rowid DESC LIMIT ?`
	args = append(args, limit)

	rows, err := r.db.sql.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*domain.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 反向成时间正序。
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// Since 返回某会话某时刻（含该时刻）之后的消息，按时间正序。
//
// 语义与 SQL 的 `created_at >= ?` 一致，边界包含在内 ——
// 学习器拿它做增量扫描，漏掉边界那条就会永远漏读。
//
// 不能直接复用 After：After 是 `> 游标` 语义，
// 用于 WebUI 轮询「有没有新消息」，两者差一个边界。
func (r *MessageRepo) Since(ctx context.Context, sessionID string, since time.Time, limit int) ([]*domain.Message, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.db.sql.QueryContext(ctx, `
		SELECT `+messageColumns+`
		FROM messages
		WHERE session_id = ? AND created_at_ms >= ?
		ORDER BY created_at_ms ASC, rowid ASC
		LIMIT ?
	`, sessionID, since.UnixMilli(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*domain.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ByMessageID 按平台消息 ID 查消息。
func (r *MessageRepo) ByMessageID(ctx context.Context, messageID string) (*domain.Message, error) {
	row := r.db.sql.QueryRowContext(ctx, `
		SELECT `+messageColumns+`
		FROM messages WHERE message_id = ? ORDER BY created_at_ms DESC LIMIT 1
	`, messageID)
	m, err := scanMessage(row)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	return m, nil
}

// ByMessageIDs 批量按平台消息 ID 查消息，返回 message_id -> 消息。
//
// 为什么不循环调 ByMessageID：一屏消息里可能十几条引用，
// 逐条查就是十几次往返。引用预览是「顺手带出来的信息」，
// 为它付 N 次查询不值得。
//
// 查不到的直接不出现在结果里（被清理、不在本会话、或压根没落库），
// 调用方据此退回成显示 ID。
func (r *MessageRepo) ByMessageIDs(ctx context.Context, messageIDs []string) (map[string]*domain.Message, error) {
	out := make(map[string]*domain.Message, len(messageIDs))
	// 去重：同一条消息可能被多条消息引用。
	uniq := dedupeNonEmpty(messageIDs)
	if len(uniq) == 0 {
		return out, nil
	}

	// SQLite 默认的变量上限是 999，这里按 500 分批就够安全了。
	const batch = 500
	for start := 0; start < len(uniq); start += batch {
		end := start + batch
		if end > len(uniq) {
			end = len(uniq)
		}
		chunk := uniq[start:end]

		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		placeholders := strings.Repeat("?, ", len(chunk))
		placeholders = placeholders[:len(placeholders)-2]

		rows, err := r.db.sql.QueryContext(ctx, `
			SELECT `+messageColumns+`
			FROM messages WHERE message_id IN (`+placeholders+`)
			ORDER BY created_at_ms ASC
		`, args...)
		if err != nil {
			return out, err
		}
		for rows.Next() {
			m, err := scanMessage(rows)
			if err != nil {
				rows.Close()
				return out, err
			}
			out[m.MessageID] = m
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return out, err
		}
		rows.Close()
	}
	return out, nil
}

// ByRefMsgIdx 按引用索引（QQ 的 REFIDX_…）反查被引消息。
//
// 用途：QQ 的 msg_elements 元素**不含 author**（实测 0/11），
// 所以「回复了谁的消息」只能靠本地反查。我们自己在入站时
// 把每条消息的 msg_idx 存进 ref_msg_idx 列，这里按它回查。
//
// 必须在同一个 session 内查：实测 REFIDX_ 在会话内共享很长一段后缀，
// 跨会话虽然不冲突，但同值在不同会话里没有语义关联，
// 不过 session_id 一并限定会让「引用其他群的消息」这种跨群诡异场景落空。
// 所以这里**不限定 session**：REFIDX_ 是全局不透明的，
// 限定范围只会在跨群时意外查不到。
//
// 查不到的直接不出现在结果里（被清理、或那条消息当时没收到），
// 调用方据此退回成「不知道是谁」。
func (r *MessageRepo) ByRefMsgIdx(ctx context.Context, refIdx []string) (map[string]*domain.Message, error) {
	out := make(map[string]*domain.Message, len(refIdx))
	uniq := dedupeNonEmpty(refIdx)
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
		for i, id := range chunk {
			args[i] = id
		}
		placeholders := strings.Repeat("?,", len(chunk))
		placeholders = placeholders[:len(placeholders)-1]

		rows, err := r.db.sql.QueryContext(ctx, `
			SELECT `+messageColumns+`
			FROM messages WHERE ref_msg_idx IN (`+placeholders+`)
			ORDER BY created_at_ms ASC
		`, args...)
		if err != nil {
			return out, err
		}
		for rows.Next() {
			m, err := scanMessage(rows)
			if err != nil {
				rows.Close()
				return out, err
			}
			// 同一索引理论上只对应一条消息（实测每条被引消息只有一个取值）。
			// 若真撞了，保留先落库的那条 —— 结果集已按时间正序。
			if _, ok := out[m.RefMsgIdx]; !ok {
				out[m.RefMsgIdx] = m
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return out, err
		}
		rows.Close()
	}
	return out, nil
}

// dedupeNonEmpty 去重并丢弃空串，保持原顺序。
func dedupeNonEmpty(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// Count 返回总消息数。
func (r *MessageRepo) Count(ctx context.Context) (int64, error) {
	var n int64
	err := r.db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages`).Scan(&n)
	return n, err
}

// CountSince 返回某时刻之后的消息数。
func (r *MessageRepo) CountSince(ctx context.Context, since time.Time) (int64, error) {
	var n int64
	err := r.db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE created_at >= ?`, since.Unix()).Scan(&n)
	return n, err
}

// DailyCount 是某一天的消息条数。
type DailyCount struct {
	// Date 是 YYYY-MM-DD（本地时区）。
	Date string `json:"date"`
	// Total 是当天全部消息数。
	Total int64 `json:"total"`
	// Inbound 是用户发来的条数。
	Inbound int64 `json:"inbound"`
	// Outbound 是机器人发出的条数。
	Outbound int64 `json:"outbound"`
}

// Daily 返回最近 days 天每天的消息条数，按日期正序。
//
// 用 SQLite 的 date(..., 'unixepoch', 'localtime') 分桶，
// 这样统计口径与用户在 WebUI 上看到的日期一致。
// 缺数据的日期不补齐：补零是展示层的事，存储层不要替它编数据。
func (r *MessageRepo) Daily(ctx context.Context, days int) ([]DailyCount, error) {
	if days <= 0 {
		days = 7
	}
	since := time.Now().AddDate(0, 0, -(days - 1))
	sinceStart := time.Date(since.Year(), since.Month(), since.Day(), 0, 0, 0, 0, time.Local)

	rows, err := r.db.sql.QueryContext(ctx, `
		SELECT date(created_at_ms / 1000, 'unixepoch', 'localtime') AS d,
		       COUNT(*),
		       SUM(CASE WHEN is_self = 0 THEN 1 ELSE 0 END),
		       SUM(CASE WHEN is_self = 1 THEN 1 ELSE 0 END)
		FROM messages
		WHERE created_at_ms >= ?
		GROUP BY d
		ORDER BY d ASC
	`, sinceStart.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DailyCount
	for rows.Next() {
		var d DailyCount
		var inbound, outbound int64
		if err := rows.Scan(&d.Date, &d.Total, &inbound, &outbound); err != nil {
			return nil, err
		}
		d.Inbound = inbound
		d.Outbound = outbound
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeleteBefore 删除某时刻之前的消息，返回删除条数。
//
// 只供保留期清理使用。不删 sessions / persons：
// 会话与人的元信息很轻，而且删了之后「这个群来过」的历史就没了。
func (r *MessageRepo) DeleteBefore(ctx context.Context, before time.Time) (int64, error) {
	res, err := r.db.sql.ExecContext(ctx,
		`DELETE FROM messages WHERE created_at_ms < ?`, before.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// scanner 抽出行扫描接口，兼容 *sql.Row 与 *sql.Rows。
type scanner interface {
	Scan(dest ...any) error
}

func scanMessage(s scanner) (*domain.Message, error) {
	var m domain.Message
	var segmentsJSON, additionalJSON string
	var createdMS int64
	var isGroup, isAt, isMentioned, isCommand, isSelf int
	// id 用 sql.NullString 扫：这一列存的是业务消息 ID（如 "m-1"），
	// 历史数据或人工导入可能为 NULL。直接扫进 string 会让
	// 「一条脏数据」变成「整个会话打不开」，报的还是一句
	// 「converting NULL to string is unsupported」，完全指不到根因。
	var rowID sql.NullString

	err := s.Scan(
		&rowID, &m.SessionID, &m.MessageID, &m.Platform, &m.UserID, &m.UserName, &m.UserCardName,
		&m.GroupID, &m.GroupName, &isGroup, &isAt, &isMentioned, &isCommand, &isSelf,
		&m.AccountID, &additionalJSON, &m.RefMsgIdx, &segmentsJSON, &m.PlainText, &createdMS,
	)
	if err != nil {
		return nil, err
	}

	if rowID.Valid {
		m.ID = rowID.String
	}

	m.IsGroup = isGroup != 0
	m.IsAt = isAt != 0
	m.IsMentioned = isMentioned != 0
	m.IsCommand = isCommand != 0
	m.IsSelf = isSelf != 0

	// 毫秒精度必须保留：引用公式里「上一句距今几秒」直接吃这个值，
	// 秒级精度会让间隔恒为整数，<= 3 这一档在重启后判定偏移。
	m.ReceivedAt = time.UnixMilli(createdMS)

	if segmentsJSON != "" && segmentsJSON != "[]" {
		if err := json.Unmarshal([]byte(segmentsJSON), &m.Segments); err != nil {
			return nil, err
		}
	}
	// additional 是平台附加字段，坏了只影响它自己。
	//
	// 不能因为一条消息的 additional 是脏数据就让整条（乃至整个会话）
	// 读不出来 —— 这与 created_at 为 NULL 那个坑同类（AGENTS.md 第 101 条）：
	// 一条脏数据不该瘫痪整个功能。直接跳过，其余字段照常可用。
	if additionalJSON != "" && additionalJSON != "null" && additionalJSON != "{}" {
		if err := json.Unmarshal([]byte(additionalJSON), &m.Additional); err != nil {
			m.Additional = nil
		}
	}
	return &m, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// stripSegmentBinary 复制一份消息段，去掉其中的二进制数据。
//
// 不直接改原 slice：调用方可能还在用这些段（例如同一轮里 attach_pic 要复用），
// 就地清空会让「刚才那张图」凭空消失。
func stripSegmentBinary(segments []domain.Segment) []domain.Segment {
	if len(segments) == 0 {
		return segments
	}
	out := make([]domain.Segment, len(segments))
	copy(out, segments)
	for i := range out {
		if len(out[i].Binary) > 0 {
			out[i].Binary = nil
		}
	}
	return out
}

// DeleteBySession 删除一个会话的全部消息。
func (r *MessageRepo) DeleteBySession(ctx context.Context, sessionID string) (int64, error) {
	res, err := r.db.sql.ExecContext(ctx, `DELETE FROM messages WHERE session_id = ?`, sessionID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteAll 清空全部消息。
//
// 单独一个方法而不是给 DeleteBySession 传空串：
// 「传空串等于删全部」这种约定一旦有人漏判就会清库。
func (r *MessageRepo) DeleteAll(ctx context.Context) (int64, error) {
	res, err := r.db.sql.ExecContext(ctx, `DELETE FROM messages`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
