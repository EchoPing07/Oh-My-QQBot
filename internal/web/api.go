package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/store"
)

// handleLogin 处理登录。
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST"})
		return
	}

	var req struct {
		Password string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}

	if !s.auth.Check(req.Password) {
		s.log.Warn("登录失败", "ip", clientIP(r))
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "密码错误"})
		return
	}

	token := s.auth.Issue()
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(cookieMaxAge.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		// 不设 Secure：允许 HTTP 访问。
	})

	s.log.Info("登录成功", "ip", clientIP(r))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleLogout 处理登出。
func (s *Server) handleLogout(w http.ResponseWriter, _ *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleStatus 返回系统状态。
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// 数据库故障要显式报错，不能静默显示 0 ——
	// 那样看起来像「新装的机器人还没收到消息」，实际是库挂了。
	msgCount, err := s.store.Messages().Count(ctx)
	if err != nil {
		s.log.Warn("统计消息数失败", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "查询消息数失败"})
		return
	}
	sessionCount, err := s.store.Sessions().Count(ctx)
	if err != nil {
		s.log.Warn("统计会话数失败", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "查询会话数失败"})
		return
	}
	personCount, err := s.store.Persons().Count(ctx)
	if err != nil {
		s.log.Warn("统计用户数失败", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "查询用户数失败"})
		return
	}

	var active int
	if s.hub != nil {
		active = s.hub.ActiveCount()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"nickname":       s.cfg.Get().Bot.Nickname,
		"platform":       s.cfg.Get().Bot.Platform,
		"account_id":     s.cfg.Get().Bot.AccountID,
		"messages":       msgCount,
		"sessions":       sessionCount,
		"persons":        personCount,
		"active_chats":   active,
		"uptime_seconds": int(time.Since(startedAt).Seconds()),
	})
}

// startedAt 是进程启动时刻。
var startedAt = time.Now()

// handleSessions 返回会话列表。
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	limit := queryInt(r, "limit", 100)
	sessions, err := s.store.Sessions().List(ctx, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	type row struct {
		ID           string `json:"id"`
		Scope        string `json:"scope"`
		TargetID     string `json:"target_id"`
		Name         string `json:"name"`
		MessageCount int    `json:"message_count"`
		LastActive   int64  `json:"last_active"`
	}

	out := make([]row, 0, len(sessions))
	for _, sess := range sessions {
		name := sess.DisplayName
		if name == "" {
			if sess.Scope == domain.ScopeGroup {
				name = "群 " + sess.TargetID
			} else {
				name = "用户 " + sess.TargetID
			}
		}
		out = append(out, row{
			ID:           sess.ID,
			Scope:        sess.Scope,
			TargetID:     sess.TargetID,
			Name:         name,
			MessageCount: sess.MessageCount,
			LastActive:   sess.LastActiveAt.Unix(),
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

// messageRow 是 /api/messages 的一行。
//
// Segments 必须返回：前端要显示图片、@、引用，少了这个就只能
// 显示一行纯文本，图片全部变成 [图片] 占位。
//
// Text 是「去掉占位后的纯文本正文」，由 segmentRowText 算出。
// 为什么要单独给：PlainText 里含 [图片] / [表情包] / @名字 这些占位，
// 前端如果拿 PlainText 当正文渲染，@ 会重复一次（at 段渲染一次 +
// 正文里又有一份），纯图片消息还会凭空多出「[图片]」文本。
type messageRow struct {
	ID       string `json:"id"`
	UserID   string `json:"user_id"`
	UserName string `json:"user_name"`
	UserCard string `json:"user_card,omitempty"`
	// Text 是不含占位符的正文（只看 text 段）。
	Text string `json:"text"`
	// PlainText 是带占位的完整渲染，保留给需要全文检索类展示的地方。
	PlainText string `json:"plain_text"`
	IsAt      bool   `json:"is_at"`
	IsSelf    bool   `json:"is_self"`
	// ReplyTo 是引用目标的 message_id。
	ReplyTo string `json:"reply_to,omitempty"`
	// IsQuoteTarget 标记这条消息是「引用了别的消息」
	// （而不是被动回复）。两者前端表现不同。
	IsQuoteTarget bool `json:"is_quote_target,omitempty"`
	// QuotedText 是被引消息的正文快照。
	//
	// 入站引用随事件下发（QQ 的 msg_elements），当场存下来；
	// 所以就算被引消息已经不在库里，前端也能显示引用内容。
	QuotedText string `json:"quoted_text,omitempty"`
	// QuotedSender 是被引消息的发送者昵称，取不到时为空。
	QuotedSender string `json:"quoted_sender,omitempty"`
	// ReplyPreview 是被引消息的可读预览，例如「小明: 在吗」。
	//
	// 为什么不让前端自己查：一句话可能被多条消息引用，
	// 逐条请求就是十几次往返；而且前端也拿不到「被引的那条」
	// 在不在当前页里。
	ReplyPreview string       `json:"reply_preview,omitempty"`
	Segments     []segmentRow `json:"segments"`
	CreatedAt    int64        `json:"created_at"`
	CreatedMS    int64        `json:"created_ms"`
}

// segmentRow 是消息段的展示形态。
//
// 不直接返回 domain.Segment：那里面带 Binary []byte，
// 序列化成 base64 会让一条 200KB 的图片变成 267KB 的 JSON，
// 而数据库里本来就没存二进制（Save 会剥掉），读回来永远是空的。
type segmentRow struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	UserID    string `json:"user_id,omitempty"`
	UserName  string `json:"user_name,omitempty"`
	MessageID string `json:"message_id,omitempty"`
	// HasMedia 表示这一段原本有媒体内容（图片/语音/文件）。
	// 二进制不落库，所以前端只能知道「这里有一张图」，拿不到图本身。
	HasMedia bool `json:"has_media,omitempty"`
	// Src 是可直接给 <img src> 的地址。
	//
	// 有 hash 就走 /api/media?hash=，由后端去 data/emoji 里找文件；
	// 找不到文件时为空，前端退回显示占位字（而不是一个碎图标）。
	//
	// 为什么不转 data URL：一张 200KB 的图 base64 后是 267KB，
	// 一屏 50 条消息就是十几 MB 的 JSON，浏览器会直接卡死。
	// 参考实现能这么做是因为它一次只取一页少量消息。
	Src string `json:"src,omitempty"`
	// MimeType 用于前端判断是图片还是贴纸（gif 需要不同的渲染尺寸）。
	MimeType string `json:"mime_type,omitempty"`
}

// handleMessages 返回某个会话的消息。
//
// 分页协议（游标式，不是 offset）：
//
//	limit       每次条数，默认 100
//	before_ms   返回严格早于该毫秒时间的消息 —— 往上翻历史
//	after_ms    返回严格晚于该毫秒时间的消息 —— 轮询新消息
//
// 用时间游标而不是 offset：聊天流持续在写入，offset 分页会漏读或重复。
// 响应里带最早/最晚一条的时间戳，前端直接拿去当下一轮的游标。
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少 session_id"})
		return
	}

	limit := queryInt(r, "limit", 100)
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}

	beforeMS := queryInt64(r, "before_ms", 0)
	afterMS := queryInt64(r, "after_ms", 0)

	var (
		msgs []*domain.Message
		err  error
	)
	switch {
	case afterMS > 0:
		msgs, err = s.store.Messages().After(ctx, sessionID, afterMS, limit)
	case beforeMS > 0:
		msgs, err = s.store.Messages().Before(ctx, sessionID, beforeMS, limit)
	default:
		msgs, err = s.store.Messages().Recent(ctx, sessionID, limit)
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	// 先一次性查出「这批消息里哪些图片哈希在表情包库里有文件」，
	// 再逐条组装。闭包里查表而不是每段查库：一屏 50 条消息
	// 可能带几十个图片段，逐段查就是几十次往返。
	existing, err := s.collectExistingHashes(ctx, msgs)
	if err != nil {
		// 查失败不影响消息本身，只是图片显示不出来。
		s.log.Warn("查询图片哈希失败", "err", err)
		existing = map[string]bool{}
	}
	hasHash := func(h string) bool { return existing[h] }

	out := make([]messageRow, 0, len(msgs))
	for _, m := range msgs {
		row := messageRow{
			ID:        m.MessageID,
			UserID:    m.UserID,
			UserName:  m.UserName,
			UserCard:  m.UserCardName,
			Text:      segmentRowText(m.Segments),
			PlainText: m.PlainText,
			IsAt:      m.IsAt,
			IsSelf:    m.IsSelf,
			Segments:  toSegmentRows(m.Segments, hasHash),
			CreatedAt: m.ReceivedAt.Unix(),
			CreatedMS: m.ReceivedAt.UnixMilli(),
		}
		if m.Additional != nil {
			// 出站消息用 reply_to（木木引用了哪条）；
			// 入站引用消息用 additional 里的 ref_msg_idx（对方引用了哪条）。
			// 两者语义相同，都是「这条消息在引谁」，所以填同一个字段，
			// 前端的引用预览与渲染逻辑不需要分两份。
			//
			// 注意**不能**用 m.RefMsgIdx 当 ReplyTo：
			// 那一列存的是「本条消息自身的索引」（别人引用它时会用到），
			// 方向恰好相反，拿错会变成「引用自己」。
			row.ReplyTo = m.Additional[domain.AdditionalReplyTo]
			if row.ReplyTo == "" {
				row.ReplyTo = m.Additional[domain.AdditionalRefMsgIdx]
				// 入站引用已经是引用关系了，前端要能画引用块
				// （而不是当成普通的「有人在引用」标记）。
				row.IsQuoteTarget = row.ReplyTo != ""
			}
			// 被引用正文快照。入站引用随事件下发，出站消息没有这个值。
			row.QuotedText = m.Additional[domain.AdditionalQuotedText]
			if s := m.Additional[domain.AdditionalQuotedSender]; s != "" {
				row.QuotedSender = s
			}
		}
		out = append(out, row)
	}

	// 给引用补上「被引的那条是谁说了什么」。
	//
	// 不补的话前端只能显示一个截断的 ROBOT1…4kM! 长 ID，
	// 对看聊天记录的人毫无信息量 —— 而那正是引用的意义。
	// 查不到的（被清理、不在本会话）什么都不填，前端退回显示 ID。
	s.fillReplyPreviews(ctx, out)

	// 游标与「还有没有更多」直接给前端算好，
	// 免得每个调用点各写一遍同样的取边界的逻辑。
	resp := map[string]any{"messages": out}
	if len(out) > 0 {
		resp["oldest_ms"] = out[0].CreatedMS
		resp["newest_ms"] = out[len(out)-1].CreatedMS
	}
	if beforeMS > 0 || afterMS > 0 {
		// 上翻/增拉：一次取满说明这个方向上可能还有。
		resp["has_more"] = len(out) >= limit
	} else {
		// 首屏（无游标）也必须给 has_more，否则前端无从判断
		// 「还要不要渲染上一页按钮」。曾经只在带游标时返回，
		// 结果首屏永远看不到「加载更早的消息」—— 超过一屏的会话
		// 历史完全翻不上去，而那恰恰是最需要翻的地方。
		resp["has_more"] = len(out) >= limit
	}

	// 把同一时间范围内的「思考」也带上，让前端能把两者混排。
	//
	// 为什么放在这个接口里而不是单独一个 /api/reasoning：
	// 参考实现用的是一本主事件账本（timeline），消息事件与推理事件
	// 都按时间戳插进去，前端只管按顺序渲染。分成两个接口的话，
	// 前端要自己做一次归并 —— 而它手上没有完整的上下文来判断
	// 「这段思考属于哪几条消息之间」，很容易插错位置。
	//
	// 查询范围就是这一屏消息覆盖的时间区间（before/after 各自的边界）。
	if len(out) > 0 {
		fromMS, toMS := out[0].CreatedMS, out[len(out)-1].CreatedMS
		// 往外扩一点：思考通常发生在消息之后（先收到消息再思考），
		// 最后一轮思考的时间会晚于最后一条消息。
		// 上翻时也要带上下界，否则会把「比这一屏更新的思考」也拉进来。
		if beforeMS > 0 {
			toMS = beforeMS
		} else if afterMS > 0 {
			// 增量拉取：只要比 after 更新的思考。
			fromMS = afterMS
		} else {
			toMS = 0 // 最新一屏：不限上界
		}

		recs, err := s.store.Reasoning().InRange(ctx, sessionID, fromMS, toMS)
		if err != nil {
			// 思考显示不出来不该拖垮消息列表。
			s.log.Warn("查询推理记录失败", "err", err, "session", sessionID)
		} else if len(recs) > 0 {
			resp["timeline"] = buildTimeline(out, recs)
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// timelineEntry 是时间线上的一项。
//
// 三种形态，对齐参考实现主事件账本里的事件分类
// （maisaka-monitor.tsx:2273 的 switch (entry.type)）：
//
//	kind=message   一条聊天消息
//	kind=reasoning 一轮思考（内部再分 Planner / 工具 / Replyer 几步）
//	kind=gate      一次门控判断
//	kind=trim      一次上下文裁切
type timelineEntry struct {
	// Kind 是 "message" / "reasoning" / "gate" / "trim"。
	Kind string `json:"kind"`
	// At 是排序用的毫秒时间戳。
	At int64 `json:"at"`
	// Message 仅在 kind=message 时有值（复用 messageRow，不重复定义）。
	Message *messageRow `json:"message,omitempty"`
	// Reasoning 仅在 kind=reasoning 时有值。
	Reasoning *timelineTurn `json:"reasoning,omitempty"`
	// Gate / Trim 是独立的小事件，字段少，直接内联。
	Gate *timelineGate `json:"gate,omitempty"`
	Trim *timelineTrim `json:"trim,omitempty"`
}

// timelineGate 是一次门控判断。
//
// 对应参考实现的 TimingGateCard（maisaka-monitor.tsx:1031）：
// 三种动作 continue / wait / no_action，外加判定说明与耗时。
type timelineGate struct {
	Action       string  `json:"action"`
	Detail       string  `json:"detail,omitempty"`
	PendingCount int     `json:"pending_count,omitempty"`
	Frequency    float64 `json:"frequency,omitempty"`
	DeferSeconds float64 `json:"defer_seconds,omitempty"`
}

// timelineTrim 是一次上下文裁切。
//
// 对应参考实现的 ContextTrimCard（maisaka-monitor.tsx:2166）。
// 它意味着「模型从这一刻起看不到更早的内容了」——
// 排查「机器人怎么忘了刚才说的」时这是唯一线索。
type timelineTrim struct {
	Removed int `json:"removed"`
	Keep    int `json:"keep"`
}

// timelineTurn 是时间线上的「一轮思考」。
//
// 为什么不按单条记录拆成多项：一轮思考里的 Planner 循环、工具执行、
// Replyer 生成是连贯的一件事（想→调工具→再想→说出话），
// 拆开放到时间线上会被中间的消息打断，读起来反而更乱。
// 参考实现也是一轮一个外框（conversation.tsx:224 TraceRound），
// 里面用竖线串起 TraceStep。
type timelineTurn struct {
	TurnID string `json:"turn_id"`
	// At 是本轮第一次思考的时间，用于定位。
	At int64 `json:"at"`
	// Rounds 是本轮内的各个步骤，按时间正序。
	//
	// 元素类型统一（而不是分成 Rounds/Tools 几个数组）：
	// 时间线要按发生顺序展示「Planner → 工具 → Planner → 工具 → Replyer」，
	// 分成几个数组就得在前端再归并一次，而这正是最容易出错的地方。
	Steps []timelineStep `json:"steps"`
}

// timelineStep 是轮次里的一步。
//
// Type 取值对齐参考实现的事件名，前端据此选渲染器：
//
//	planner  PlannerFinalizedCard  —— 想了什么
//	tool     PlannerToolResultCard —— 实际做了什么
//	replyer  ReplierResponseCard   —— 最后怎么说的
type timelineStep struct {
	Type string `json:"type"`
	// At / Duration 是这一步自己的时间与耗时。
	At       int64 `json:"at"`
	Duration int64 `json:"duration_ms,omitempty"`
	Failed   bool  `json:"failed,omitempty"`
	// Name 是工具名或任务名。
	Name string `json:"name,omitempty"`

	// —— planner 专用 ——
	Model     string         `json:"model,omitempty"`
	Finish    string         `json:"finish_reason,omitempty"`
	Content   string         `json:"content,omitempty"`
	Thinking  string         `json:"thinking,omitempty"`
	ToolCalls []timelineTool `json:"tool_calls,omitempty"`
	ToolCount int            `json:"tool_count,omitempty"`
	History   int            `json:"history_count,omitempty"`
	Round     int            `json:"round,omitempty"`
	Request   string         `json:"request,omitempty"`
	Response  string         `json:"response,omitempty"`

	// —— tool / replyer 专用（来自 payload）——
	Args   string `json:"args,omitempty"`
	Result string `json:"result,omitempty"`

	// —— replyer 专用 ——
	PromptTokens     int `json:"prompt_tokens,omitempty"`
	CompletionTokens int `json:"completion_tokens,omitempty"`
	TotalTokens      int `json:"total_tokens,omitempty"`
}

// timelineTool 是模型请求调用的一个工具。
//
// 参考实现在折叠标题里就用「彩色圆点 + 工具名」概括每一步
// （maisaka-monitor.tsx:2222 CollapsedPlannerToolSummary），
// 展开后才是完整参数与结果。
type timelineTool struct {
	Name string `json:"name"`
	Args string `json:"args,omitempty"`
	// Done 表示这一步已经有执行结果（对应那个绿/红圆点）。
	Done   bool `json:"done,omitempty"`
	Failed bool `json:"failed,omitempty"`
}

// buildTimeline 把消息行与推理记录按时间合并成一条有序时间线。
//
// 推理记录里混着 5 类事件（planner / tool / replyer / gate / trim），
// 这里按 kind 分流：
//
//   - planner/tool/replyer 属于同一轮思考，聚合成一个 timelineTurn，
//     内部按时间排成 Steps
//   - gate / trim 是独立的小事件，各自成项
//
// 同一毫秒的排序规则：消息在前。
// 理由是「先收到消息才可能开始思考」，反过来不符合因果。
func buildTimeline(msgs []messageRow, recs []store.ReasoningRecord) []timelineEntry {
	out := make([]timelineEntry, 0, len(msgs)+8)

	for i := range msgs {
		out = append(out, timelineEntry{
			Kind:    "message",
			At:      msgs[i].CreatedMS,
			Message: &msgs[i],
		})
	}

	// 工具执行结果按 tool_call_id 索引，用于给 Planner 步骤里的圆点上色。
	// 先把所有 tool 事件过一遍，再处理 planner，避免顺序依赖。
	toolByName := map[string]bool{} // "turnID|工具名" -> 是否有执行结果
	toolFailed := map[string]bool{}

	for _, rec := range recs {
		if rec.Kind != "tool" {
			continue
		}
		key := rec.TurnID + "|" + rec.Name
		toolByName[key] = true
		if rec.Failed {
			toolFailed[key] = true
		}
	}

	turns := make(map[string]*timelineTurn, len(recs))
	order := make([]string, 0, len(recs))

	for _, rec := range recs {
		switch rec.Kind {
		case "gate":
			out = append(out, timelineEntry{
				Kind: "gate",
				At:   rec.CreatedAtMS,
				Gate: decodeGate(rec),
			})
			continue
		case "trim":
			out = append(out, timelineEntry{
				Kind: "trim",
				At:   rec.CreatedAtMS,
				Trim: decodeTrim(rec),
			})
			continue
		}

		// planner / tool / replyer → 归到同一轮。
		turn, ok := turns[rec.TurnID]
		if !ok {
			turn = &timelineTurn{TurnID: rec.TurnID, At: rec.CreatedAtMS}
			turns[rec.TurnID] = turn
			order = append(order, rec.TurnID)
		}
		if rec.CreatedAtMS > 0 && rec.CreatedAtMS < turn.At {
			turn.At = rec.CreatedAtMS
		}
		turn.Steps = append(turn.Steps, decodeStep(rec, toolByName, toolFailed))
	}

	for _, id := range order {
		turn := turns[id]
		// 按时间正序；CreatedAtMS 相同时用类型定序（planner 在前，
		// 因为「想了才会做」）。
		sort.SliceStable(turn.Steps, func(a, b int) bool {
			if turn.Steps[a].At != turn.Steps[b].At {
				return turn.Steps[a].At < turn.Steps[b].At
			}
			return stepPriority(turn.Steps[a].Type) < stepPriority(turn.Steps[b].Type)
		})
		out = append(out, timelineEntry{Kind: "reasoning", At: turn.At, Reasoning: turn})
	}

	// 稳定排序：At 相同时保持插入顺序（消息在前）。
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].At != out[b].At {
			return out[a].At < out[b].At
		}
		return out[a].Kind == "message" && out[b].Kind != "message"
	})

	return out
}

// stepPriority 给同一毫秒的步骤定序。
func stepPriority(typ string) int {
	switch typ {
	case "planner":
		return 0
	case "tool":
		return 1
	case "replyer":
		return 2
	default:
		return 3
	}
}

// decodeStep 把一条记录转成轮次里的一步。
func decodeStep(rec store.ReasoningRecord, done, failed map[string]bool) timelineStep {
	switch rec.Kind {
	case "tool":
		return decodeToolStep(rec)
	case "replyer":
		return decodeReplyerStep(rec)
	default:
		return decodePlannerStep(rec, done, failed)
	}
}

// decodePlannerStep 解析一条 Planner 记录。
//
// 为什么要拆：前端拿到一坨 JSON 字符串只能「原样展示」或「自己解析」，
// 两种都不好 —— 前者是一堆折叠的原始 JSON，后者等于把解析规则复制到前端。
func decodePlannerStep(rec store.ReasoningRecord, done, failed map[string]bool) timelineStep {
	step := timelineStep{
		Type:     "planner",
		At:       rec.CreatedAtMS,
		Duration: rec.DurationMS,
		Failed:   rec.Failed,
		Round:    rec.Round,
		Request:  rec.Request,
		Response: rec.Response,
	}

	var parsed struct {
		Content   string `json:"content"`
		Reasoning string `json:"reasoning"`
		Finish    string `json:"finish_reason"`
		Model     string `json:"model"`
		ToolCalls []struct {
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"tool_calls"`
	}
	if err := json.Unmarshal([]byte(rec.Response), &parsed); err == nil {
		step.Content = parsed.Content
		step.Thinking = parsed.Reasoning
		step.Finish = parsed.Finish
		step.Model = parsed.Model
		if step.Duration == 0 {
			step.Duration = rec.DurationMS
		}
		for _, call := range parsed.ToolCalls {
			name := call.Function.Name
			args := call.Function.Arguments
			if name == "" {
				name = call.Name
			}
			if args == "" {
				args = call.Arguments
			}
			if name == "" {
				continue
			}
			key := rec.TurnID + "|" + name
			step.ToolCalls = append(step.ToolCalls, timelineTool{
				Name:   name,
				Args:   args,
				Done:   done[key],
				Failed: failed[key],
			})
		}
	}

	// 「可用工具数 / 上下文档数」从请求里数，参考实现也这样显示
	// （maisaka-monitor.tsx:1209「上下文 N 条 / 可用工具 M」）。
	var req struct {
		Tools    []json.RawMessage `json:"tools"`
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal([]byte(rec.Request), &req); err == nil {
		step.ToolCount = len(req.Tools)
		step.History = len(req.Messages)
	}

	return step
}

// decodeToolStep 解析一条工具执行记录。
//
// 对应参考实现的 PlannerToolResultCard（maisaka-monitor.tsx:1580）：
// 工具名、参数、执行结果摘要、成功/失败、耗时。
func decodeToolStep(rec store.ReasoningRecord) timelineStep {
	step := timelineStep{
		Type:     "tool",
		At:       rec.CreatedAtMS,
		Duration: rec.DurationMS,
		Failed:   rec.Failed,
		Name:     rec.Name,
		Round:    rec.Round,
	}
	var payload struct {
		Args   json.RawMessage `json:"args"`
		Result string          `json:"result"`
	}
	if err := json.Unmarshal([]byte(rec.Payload), &payload); err == nil {
		// 参数原样给出（它本身就是 JSON 文本），前端压成一行展示。
		step.Args = string(payload.Args)
		step.Result = payload.Result
	}
	return step
}

// decodeReplyerStep 解析一条 Replyer 记录。
//
// 对应参考实现的 ReplierResponseCard（maisaka-monitor.tsx:2056）：
// 正文、思考、模型、输入/输出/总 token、耗时、成败。
func decodeReplyerStep(rec store.ReasoningRecord) timelineStep {
	step := timelineStep{
		Type:     "replyer",
		At:       rec.CreatedAtMS,
		Duration: rec.DurationMS,
		Failed:   rec.Failed,
		Name:     rec.Name,
	}
	var payload struct {
		Content  string `json:"content"`
		Thinking string `json:"thinking"`
		Model    string `json:"model"`
		Prompt   int    `json:"prompt_tokens"`
		Complete int    `json:"completion_tokens"`
		Total    int    `json:"total_tokens"`
		Error    string `json:"error"`
	}
	if err := json.Unmarshal([]byte(rec.Payload), &payload); err == nil {
		step.Content = payload.Content
		step.Thinking = payload.Thinking
		step.Model = payload.Model
		step.PromptTokens = payload.Prompt
		step.CompletionTokens = payload.Complete
		step.TotalTokens = payload.Total
		step.Result = payload.Error
	}
	return step
}

// decodeGate 解析一次门控判断。
func decodeGate(rec store.ReasoningRecord) *timelineGate {
	gate := &timelineGate{Action: rec.Name}
	var payload struct {
		Action       string  `json:"action"`
		DeferSeconds float64 `json:"defer_seconds"`
		Detail       string  `json:"detail"`
		PendingCount int     `json:"pending_count"`
		Frequency    float64 `json:"frequency"`
	}
	if err := json.Unmarshal([]byte(rec.Payload), &payload); err == nil {
		if payload.Action != "" {
			gate.Action = payload.Action
		}
		gate.DeferSeconds = payload.DeferSeconds
		gate.Detail = payload.Detail
		gate.PendingCount = payload.PendingCount
		gate.Frequency = payload.Frequency
	}
	return gate
}

// decodeTrim 解析一次上下文裁切。
func decodeTrim(rec store.ReasoningRecord) *timelineTrim {
	trim := &timelineTrim{}
	var payload struct {
		Removed int `json:"removed"`
		Keep    int `json:"keep"`
	}
	if err := json.Unmarshal([]byte(rec.Payload), &payload); err == nil {
		trim.Removed = payload.Removed
		trim.Keep = payload.Keep
	}
	return trim
}

// toSegmentRows 把消息段转成可安全序列化的展示形态。
func toSegmentRows(segments []domain.Segment, hasHash func(string) bool) []segmentRow {
	out := make([]segmentRow, 0, len(segments))
	for _, seg := range segments {
		row := segmentRow{
			Type:      string(seg.Type),
			Text:      seg.Text,
			UserID:    seg.UserID,
			UserName:  seg.UserName,
			MessageID: seg.MessageID,
			MimeType:  seg.MimeType,
		}
		// 二进制在落库时被剥掉了（AGENTS.md 第 24 条），
		// 但哈希留着 —— 前端拿它去 /api/media?hash= 取文件。
		if seg.Hash != "" && hasHash != nil && hasHash(seg.Hash) {
			row.Src = "/api/media?hash=" + url.QueryEscape(seg.Hash)
		}
		switch seg.Type {
		case domain.SegmentImage, domain.SegmentEmoji, domain.SegmentVoice, domain.SegmentFile:
			row.HasMedia = true
		}
		out = append(out, row)
	}
	return out
}

// segmentRowText 只拼 text 段，丢掉所有占位符。
//
// 为什么不能直接用 PlainText：它含 [图片] / [表情包] / @名字，
// 前端拿它当正文渲染时 @ 会重复一次（at 段渲染一次 + 正文又有一份），
// 纯图片消息还会凭空多出「[图片]」这几个字。
func segmentRowText(segments []domain.Segment) string {
	parts := make([]string, 0, len(segments))
	for _, seg := range segments {
		if seg.Type != domain.SegmentText {
			continue
		}
		// 每段单独 trim 再收集：QQ 的分段边界常带空格，
		// 直接连起来会出现「甲乙」这种粘连（原本是两段话）。
		if t := strings.TrimSpace(seg.Text); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, " ")
}

// collectExistingHashes 收集一批消息里所有媒体段的哈希，查出哪些在库。
//
// 批量查而不是逐段查：一屏 50 条消息可能带几十个图片段，
// 逐段查就是几十次往返。
func (s *Server) collectExistingHashes(ctx context.Context, msgs []*domain.Message) (map[string]bool, error) {
	var hashes []string
	for _, m := range msgs {
		for _, seg := range m.Segments {
			if seg.Hash != "" {
				hashes = append(hashes, seg.Hash)
			}
		}
	}
	return s.store.Emojis().ExistingHashes(ctx, hashes)
}

// fillReplyPreviews 给一批消息行补上引用预览。
//
// 一次批量查全部被引 ID，而不是逐行查：一屏消息里可能十几条引用，
// 逐行查就是十几次往返。
//
// 两条查法都要走：
//   - ByMessageIDs：出站的 reply_to 与 d.id 形态的目标
//   - ByRefMsgIdx：入站的 ref_msg_idx（QQ 的 REFIDX_…）
//
// 入站引用靠后者，因为被引消息落库时 MessageID 存的是 d.id，
// 而引用事件里只有 REFIDX_，两者属于不同键空间。
//
// 都查不到时不填 —— 前端退回用 QuotedText 快照，
// 再退一步才显示截断后的 ID。
func (s *Server) fillReplyPreviews(ctx context.Context, rows []messageRow) {
	// 收集所有被引用的 ID。
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.ReplyTo != "" {
			ids = append(ids, row.ReplyTo)
		}
	}
	if len(ids) == 0 {
		return
	}

	byID, err := s.store.Messages().ByMessageIDs(ctx, ids)
	if err != nil {
		// 补不上预览不影响消息本身，前端退回显示 ID。
		s.log.Warn("查询引用预览失败", "err", err)
		byID = nil
	}
	byRefIdx, err := s.store.Messages().ByRefMsgIdx(ctx, ids)
	if err != nil {
		s.log.Warn("按引用索引查询预览失败", "err", err)
		byRefIdx = nil
	}
	for i := range rows {
		if rows[i].ReplyTo == "" {
			continue
		}
		target := byID[rows[i].ReplyTo]
		if target == nil {
			target = byRefIdx[rows[i].ReplyTo]
		}
		if target == nil {
			continue
		}
		rows[i].ReplyPreview = replyPreviewOf(target)
		// 发送者名也要补：入站引用事件不带 author，
		// 而 Hub 回查时可能因为顺序问题没拿到。
		if rows[i].QuotedSender == "" {
			rows[i].QuotedSender = target.UserName
		}
	}
}

// replyPreviewOf 把被引消息压成一行「谁: 说了什么」。
//
// 发送者名字一律用 m.UserName：出站消息在 agent/outbound.go 落库时
// 就把 UserName 填成了当次的 botName（来自配置）。
// 曾经这里写的是 `if m.IsSelf { name = "木木" }`，后果是用户把
// bot.nickname 改成别的之后，引用预览里永远显示「木木」——
// 界面上看得见、但不报错的错（见 AGENTS.md 第 146 条同类）。
func replyPreviewOf(m *domain.Message) string {
	if m == nil {
		return ""
	}
	text := segmentRowText(m.Segments)
	if text == "" {
		// 纯图片/表情消息没有正文，给一个可读的占位。
		text = m.PlainText
	}
	text = strings.TrimSpace(text)
	// 预览太长会把气泡撑爆，截断到 40 个字符（按 rune，别劈汉字）。
	if r := []rune(text); len(r) > 40 {
		text = string(r[:40]) + "…"
	}
	name := m.UserName
	if name == "" {
		return text
	}
	if text == "" {
		return name
	}
	return name + ": " + text
}

// handleConfig 返回当前配置（脱敏）。
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	// 只返回可安全展示的字段，密钥一律不出去。
	writeJSON(w, http.StatusOK, map[string]any{
		"bot": map[string]any{
			"nickname":    s.cfg.Get().Bot.Nickname,
			"alias_names": s.cfg.Get().Bot.AliasNames,
		},
		"personality": map[string]any{
			"personality":    s.cfg.Get().Personality.Personality,
			"behavior_style": s.cfg.Get().Personality.BehaviorStyle,
			"reply_style":    s.cfg.Get().Personality.ReplyStyle,
		},
		"chat": map[string]any{
			"max_context_size":         s.cfg.Get().Chat.MaxContextSize,
			"max_private_context_size": s.cfg.Get().Chat.MaxPrivateContextSize,
			"enable_reply_at":          s.cfg.Get().Chat.EnableReplyAt,
			"debounce_mode":            s.cfg.Get().Chat.Timing.DebounceMode,
			"talk_value":               s.cfg.Get().Chat.Timing.TalkValue,
			"reply_trigger_mode":       s.cfg.Get().Chat.Timing.ReplyTriggerMode,
		},
		"memory": map[string]any{
			"enabled":         s.cfg.Get().Memory.Enabled,
			"summary_enabled": s.cfg.Get().Memory.SummaryEnabled,
			"profile_enabled": s.cfg.Get().Memory.ProfileEnabled,
		},
		"emoji": map[string]any{
			"enabled":         s.cfg.Get().Emoji.Enabled,
			"collect_enabled": s.cfg.Get().Emoji.CollectEnabled,
			"max_count":       s.cfg.Get().Emoji.MaxCount,
		},
		"learning": map[string]any{
			"expression_enabled": s.cfg.Get().Learning.ExpressionEnabled,
			"jargon_enabled":     s.cfg.Get().Learning.JargonEnabled,
		},
		"keyword": map[string]any{
			"rule_count": len(s.cfg.Get().Keyword.Rules),
		},
		"models": s.modelSummary(),
	})
}

// modelSummary 汇总模型配置，不含密钥。
func (s *Server) modelSummary() []map[string]any {
	out := make([]map[string]any, 0, len(s.cfg.Get().LLM.Models))
	for _, m := range s.cfg.Get().LLM.Models {
		out = append(out, map[string]any{
			"name":       m.Name,
			"identifier": m.ModelIdentifier,
			"provider":   m.Provider,
			"visual":     m.Visual,
		})
	}
	return out
}

// handleUsage 返回用量统计。
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	days := queryInt(r, "days", 1)
	if days <= 0 {
		days = 1
	}
	since := time.Now().AddDate(0, 0, -days)

	summary, err := s.store.Usage().Summary(ctx, since)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	byTask, err := s.store.Usage().ByTask(ctx, since)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// 显式初始化成空切片：nil 会序列化成 null，前端 forEach 直接抛错。
	// sessions / messages 两处也都是这么处理的，保持一致。
	if byTask == nil {
		byTask = []store.TaskUsage{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"summary": summary,
		"by_task": byTask,
	})
}

func queryInt(r *http.Request, key string, def int) int {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return n
}

// queryInt64 读一个 64 位整数参数。
//
// 时间戳是毫秒级的，32 位不够用 —— 用 Atoi 会在 2038 年之前就先溢出。
func queryInt64(r *http.Request, key string, def int64) int64 {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return def
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return def
	}
	return n
}
