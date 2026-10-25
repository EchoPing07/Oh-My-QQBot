package web

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/store"
)

// 本文件是「长期记忆 / 表情包 / 表达」三块新功能的只读查询接口。
//
// 设计上只读：这三类数据的写入都要经过模型（摘要、打标签、学习），
// WebUI 手工改没有意义 —— 改完下次模型调用又会被覆盖。
// 需要删数据时保留 delete 接口，那是运维动作，不是日常操作。

// handleMemory 返回长期记忆列表。
func (s *Server) handleMemory(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	sessionID := r.URL.Query().Get("session_id")
	query := r.URL.Query().Get("q")
	limit := queryInt(r, "limit", 20)

	var (
		list []*store.MemorySummary
		err  error
	)
	switch {
	case query != "":
		list, err = s.store.Memory().SearchSummaries(ctx, sessionID, query, limit)
	case sessionID != "":
		// 没有关键词时走时间检索，把该会话的回想全捞出来。
		list, err = s.store.Memory().SearchSummariesByTime(ctx, sessionID, 0, 0, limit)
	default:
		list, err = s.store.Memory().SearchSummariesByTime(ctx, "", 0, 0, limit)
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	// 显式初始化空切片：nil 序列化成 null，前端 forEach 直接抛错。
	if list == nil {
		list = []*store.MemorySummary{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list})
}

// handleMemoryDelete 删除一条记忆。
func (s *Server) handleMemoryDelete(w http.ResponseWriter, r *http.Request) {
	// 删除是有副作用的操作，必须限定方法。
	// 只靠 query 参数的话，浏览器预取、爬虫或一次 GET 链接
	// 都会真的删掉一条记忆，而且删完无从恢复。
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST / DELETE"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	id := int64(queryInt(r, "id", 0))
	if id <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id 无效"})
		return
	}
	if err := s.store.Memory().DeleteSummary(ctx, id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleProfiles 返回人物画像。
//
// 两种用法：
//
//	?user_id=xxx[&session_id=yyy]  查单个用户（含跨会话回退）
//	[?session_id=yyy]              列出画像，按最近更新倒序
//
// 以前只支持第一种，管理页无法列出「都认识了谁」，只能靠人肉
// 记住 OpenID 来查 —— 实际上等于这个页面没做。
// 这里不返回原始聊天记录，只返回模型生成的画像摘要，
// 用它检查画像质量是合理的用途。
func (s *Server) handleProfiles(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	userID := r.URL.Query().Get("user_id")
	sessionID := r.URL.Query().Get("session_id")
	limit := queryInt(r, "limit", 100)

	if userID != "" {
		p, err := s.store.Memory().GetProfile(ctx, s.cfg.Get().Bot.Platform, userID, sessionID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		if p == nil {
			writeJSON(w, http.StatusOK, map[string]any{"items": []any{}, "total": 0})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": []any{p}, "total": 1})
		return
	}

	list, err := s.store.Memory().ListProfiles(ctx, sessionID, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if list == nil {
		list = []*store.PersonProfile{}
	}

	writeJSON(w, http.StatusOK, map[string]any{"items": list, "total": len(list)})
}

// handleEmojis 返回表情包列表。
func (s *Server) handleEmojis(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	limit := queryInt(r, "limit", 50)
	query := r.URL.Query().Get("q")

	var (
		list []*store.Emoji
		err  error
	)
	if query != "" {
		list, err = s.store.Emojis().Search(ctx, query, limit)
	} else {
		list, err = s.store.Emojis().Random(ctx, limit)
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if list == nil {
		list = []*store.Emoji{}
	}

	total, err := s.store.Emojis().Count(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"items": list,
		"total": total,
	})
}

// handleExpressions 返回学到的表达。
func (s *Server) handleExpressions(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	limit := queryInt(r, "limit", 50)
	sessionID := r.URL.Query().Get("session_id")

	// 指定了会话就按会话查（含全局），否则查全库。
	// 管理界面默认要看全库 —— 用 ListExpressions 传空 sessionID 只会
	// 命中「session_id 为空串」的那几条，看起来像「什么都没学到」。
	var (
		list []*store.Expression
		err  error
	)
	if sessionID != "" {
		list, err = s.store.Memory().ListExpressions(ctx, sessionID, limit)
	} else {
		list, err = s.store.Memory().ListAllExpressions(ctx, limit)
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if list == nil {
		list = []*store.Expression{}
	}

	total, err := s.store.Memory().CountExpressions(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"items": list,
		"total": total,
	})
}

// handleJargons 返回学到的黑话。
func (s *Server) handleJargons(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	limit := queryInt(r, "limit", 50)
	list, err := s.store.Memory().ListJargons(ctx, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if list == nil {
		list = []*store.Jargon{}
	}

	total, err := s.store.Memory().CountJargons(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"items": list,
		"total": total,
	})
}

// handleEmojiImage 返回表情包图片本体。
//
// 为什么不用 FileServer 直接暴露目录：那会把整个 data/emoji 变成公开静态资源，
// 绕过登录。这里按 hash 查库拿到路径、校验后再读文件，
// 既保证只有登录用户能看，也天然避免了路径穿越（hash 是查出来的，不是拼出来的）。
func (s *Server) handleEmojiImage(w http.ResponseWriter, r *http.Request) {
	s.serveEmojiByHash(w, r)
}

// handleMedia 给会话页的图片/表情段回查文件。
//
// 与 handleEmojiImage 共用实现：两者都是「按 sha256 从表情包库取一张图」。
// 分开两个路由是为了前端语义清晰（一个是「表情包管理」，
// 一个是「这条消息里的那张图」），以后可以各自加缓存策略。
//
// 为什么聊天图片要过表情包库：二进制不落库（见 store.MessageRepo.Save），
// 而表情包采集时会把文件写到 data/emoji/<前两位>/<sha256>.<ext>。
// 所以同一张图如果被采集过，就能直接显示；没采集过的（普通图片）
// 就退回显示占位字。这是与参考实现的差异：
// 参考实现把图转成 data URL 内联返回（serializers.py:148），
// 但那需要图片二进制留在库里，而我们的库里没有。
func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	s.serveEmojiByHash(w, r)
}

// serveEmojiByHash 是按 hash 读表情包文件的共用实现。
func (s *Server) serveEmojiByHash(w http.ResponseWriter, r *http.Request) {
	hash := r.URL.Query().Get("hash")
	if hash == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "hash 不能为空"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	rec, err := s.store.Emojis().GetByHash(ctx, hash)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if rec == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "表情包不存在"})
		return
	}

	// 只允许读表情包目录下的文件。
	//
	// 即便 file_path 来自我们自己的库，也要校验一次：
	// 库里的值可能是旧版本写的、也可能被人工改过。
	dir := s.cfg.Get().Emoji.Dir
	if dir == "" {
		dir = "data/emoji"
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	absFile, err := filepath.Abs(rec.FilePath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if !strings.HasPrefix(absFile, absDir+string(filepath.Separator)) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "路径越界"})
		return
	}

	data, err := os.ReadFile(absFile)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "文件读取失败"})
		return
	}

	w.Header().Set("Content-Type", mimeByExt(absFile))
	// private：图片可能要过登录才能看，不能被共享缓存代理缓存。
	// 内容按 hash 定位，同一个 hash 内容永不变化，所以可以长缓存。
	w.Header().Set("Cache-Control", "private, max-age=86400")
	_, _ = w.Write(data)
}

// mimeByExt 按扩展名猜 Content-Type。
func mimeByExt(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".bmp":
		return "image/bmp"
	default:
		return "image/png"
	}
}
