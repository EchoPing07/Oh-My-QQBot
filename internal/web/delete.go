package web

// 本文件实现「删除」与「管理类写操作」。
//
// 与 MaiBot 的对照（逐个接口都在参考实现里有对应）：
//
//	参考实现                                     我们
//	-------------------------------------------  -----------------------------
//	DELETE /api/chat/sessions/{id}                DELETE /api/sessions?id=
//	DELETE /api/chat/history                      DELETE /api/messages?session_id=&all=1
//	DELETE /api/expression/{id}                   DELETE /api/expressions?id=
//	DELETE /api/jargon/{id}                       DELETE /api/jargons?id=
//	DELETE /api/emoji/{id}                        DELETE /api/emojis?id=
//	DELETE /api/memory/...（记忆摘要）             DELETE /api/memory?id=
//	DELETE /api/person/{id}                       DELETE /api/profiles?id=
//	GET/PUT/DELETE /api/config/prompts/...        GET/PUT/DELETE /api/prompts
//	POST /api/data-transfer/export|import         POST /api/backup/export|import
//
// 为什么统一用「查询参数 + DELETE 方法」而不是 RESTful 路径段：
// 我们的路由是 net/http 的 ServeMux，不支持路径参数，
// 用查询参数可以少一层自己解析路径的逻辑。
// 但**方法必须限定为 DELETE**：删除类接口只认 GET 时，
// 浏览器预取、爬虫扫到链接、甚至 <img> 的 src 都能触发删除。

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/store"
)

// requireMethod 挡住非预期方法，并把「应当用什么方法」写清楚。
func requireMethod(w http.ResponseWriter, r *http.Request, methods ...string) bool {
	for _, m := range methods {
		if r.Method == m {
			return true
		}
	}
	writeJSON(w, http.StatusMethodNotAllowed, map[string]any{
		"error": "只支持 " + joinMethods(methods) + " 方法",
	})
	return false
}

func joinMethods(methods []string) string {
	out := ""
	for i, m := range methods {
		if i > 0 {
			out += " / "
		}
		out += m
	}
	return out
}

// deleteResult 统一删除接口的响应。
//
// 带 Deleted 字段而不是只回 ok：界面要显示「删除了 N 条」，
// 而「删了 0 条」和「删了 1 条」在用户眼里是两件不同的事 ——
// 前者通常意味着 ID 传错了。
type deleteResult struct {
	OK      bool   `json:"ok"`
	Deleted int64  `json:"deleted"`
	Message string `json:"message,omitempty"`
}

// handleSessionDelete 删除一个会话及其全部消息与学习数据。
//
// 对应参考实现 chat/routes.py:1412 delete_chat_session。
func (s *Server) handleSessionDelete(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodDelete) {
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id 不能为空"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// 删会话前先把它从运行时里摘掉：不然那个会话的 run 协程还在跑，
	// 收到下一条消息时会用已经不存在的会话继续写入，
	// 表现为「删掉的会话又冒出来了」。
	if s.hub != nil {
		if closer, ok := any(s.hub).(interface{ CloseSession(string) }); ok {
			closer.CloseSession(id)
		}
	}

	if err := s.store.Sessions().Delete(ctx, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "会话不存在"})
			return
		}
		s.log.Error("删除会话失败", "session", id, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	s.log.Info("已删除会话", "session", id)
	writeJSON(w, http.StatusOK, deleteResult{OK: true, Deleted: 1})
}

// handleMessagesDelete 删除一个会话的全部消息，或删整个库的消息。
//
// 对应参考实现 chat/routes.py:1691 delete_history。
// 清空整个库要显式传 all=1：仅漏掉 session_id 就清库太危险。
func (s *Server) handleMessagesDelete(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodDelete) {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	q := r.URL.Query()

	if q.Get("all") == "1" {
		n, err := s.store.Messages().DeleteAll(ctx)
		if err != nil {
			s.log.Error("清空消息失败", "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		s.log.Warn("已清空全部消息", "deleted", n)
		writeJSON(w, http.StatusOK, deleteResult{OK: true, Deleted: n})
		return
	}

	sessionID := q.Get("session_id")
	if sessionID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "需要 session_id，或传 all=1 清空全部",
		})
		return
	}

	n, err := s.store.Messages().DeleteBySession(ctx, sessionID)
	if err != nil {
		s.log.Error("删除消息失败", "session", sessionID, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.log.Info("已删除会话消息", "session", sessionID, "count", n)
	writeJSON(w, http.StatusOK, deleteResult{OK: true, Deleted: n})
}

// handleSessionRename 改会话的展示名。
//
// 数据源是 SessionRepo.SetDisplayName（早就有，且已经改成「会话不存在就报错」），
// 之前缺的只是一个写入入口 —— 于是界面上会话名永远是从 target_id 拼出来的，
// 用户想把「g-10001」改成「家人群」只能直接改数据库。
func (s *Server) handleSessionRename(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPatch, http.MethodPost) {
		return
	}

	var req struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		// 空名字会让会话在列表里显示成空行，比不让改更糟。
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "名字不能为空"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if err := s.store.Sessions().SetDisplayName(ctx, req.ID, req.Name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "会话不存在"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	s.log.Info("已重命名会话", "session", req.ID, "name", req.Name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": strings.TrimSpace(req.Name)})
}

// parseIDParam 取出整数 id 参数。
func parseIDParam(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.URL.Query().Get("id")
	if raw == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id 不能为空"})
		return 0, false
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id 必须是正整数"})
		return 0, false
	}
	return id, true
}

// handleExpressionDelete 删除一条表达。
func (s *Server) handleExpressionDelete(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodDelete) {
		return
	}
	id, ok := parseIDParam(w, r)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if err := s.store.Memory().DeleteExpression(ctx, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "表达不存在"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, deleteResult{OK: true, Deleted: 1})
}

// handleJargonDelete 删除一条黑话。
func (s *Server) handleJargonDelete(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodDelete) {
		return
	}
	id, ok := parseIDParam(w, r)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if err := s.store.Memory().DeleteJargon(ctx, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "黑话不存在"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, deleteResult{OK: true, Deleted: 1})
}

// handleEmojiDelete 删除一张表情包，并清掉磁盘文件。
//
// 只删库不删文件的话，data/emoji 会一直涨而界面上看不见 ——
// 用户删了几百张，磁盘占用一点没变，这种「删了好像没删」很难解释。
func (s *Server) handleEmojiDelete(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodDelete) {
		return
	}
	id, ok := parseIDParam(w, r)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	filePath, err := s.store.Emojis().Delete(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "表情包不存在"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	// 库里的记录删掉之后再删文件。
	//
	// 顺序不能反：先删文件、事务失败会留下「记录在、文件没了」，
	// 界面上是一堆碎图标。反过来最坏情况只是磁盘上多一个孤儿文件，
	// 下一轮采集的哈希去重会把它复用掉。
	if filePath != "" {
		if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
			s.log.Warn("删除表情包文件失败（记录已删）", "path", filePath, "err", err)
		}
	}

	writeJSON(w, http.StatusOK, deleteResult{OK: true, Deleted: 1})
}

// handleProfileDelete 删除一份人物画像。
//
// 参考实现 person.py:310 有对应的删除接口。画像里是真实的人的信息，
// 让人能删掉是对「这机器人记了别人什么」的最低限度控制。
func (s *Server) handleProfileDelete(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodDelete) {
		return
	}
	q := r.URL.Query()
	userID := q.Get("user_id")
	if userID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "user_id 不能为空"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	ok, err := s.store.Memory().DeleteProfile(ctx, q.Get("platform"), userID, q.Get("session_id"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "画像不存在"})
		return
	}
	writeJSON(w, http.StatusOK, deleteResult{OK: true, Deleted: 1})
}
