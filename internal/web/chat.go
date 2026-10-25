package web

import (
	"net/http"
	"strings"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
)

// 网页对话窗口：以人类身份与机器人私聊。
//
// 对应 MaiBot WebUI 的聊天页（dashboard /chat + webui/routers/chat/service.py）。
// 机制照抄它的做法：
//   - 网页是独立于 QQ 的虚拟平台（platform=webui），私聊形态
//   - 消息带 is_at_bot=True（我们对应 IsAt=true，@ 必触发 Planner）
//   - 投递进主链路（我们对应 hub.Dispatch），机器人正常思考
//   - 回复只回网页（我们对应 agent.WebUISender + 出站落库），
//     前端轮询 /api/messages 拿到机器人说的话
//
// 与被移除的 /api/send 的本质区别：那是「以机器人身份往真实 QQ 会话
// 发主动消息」，这是「以人类身份把消息投进对话窗口」。

// handleChatSession 返回网页对话窗口的会话信息。
//
// 前端进页面先调它拿 session_id，之后拉历史、轮询新消息、
// 发送用的都是这一个 ID —— 它由 domain.WebUISessionKey() 稳定算出。
func (s *Server) handleChatSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 GET"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": domain.WebUISessionKey().ID(),
		"bot_name":   s.cfg.Get().Bot.Nickname,
	})
}

// handleChatSend 把网页用户的一句话投进对话窗口。
//
// 同步做的事只有「构造入站消息并 Dispatch」；机器人何时回复、
// 回复什么，由会话 runtime 按正常链路决定，前端轮询拿结果。
func (s *Server) handleChatSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST"})
		return
	}

	var req struct {
		Text     string `json:"text"`
		UserName string `json:"user_name"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	req.Text = strings.TrimSpace(req.Text)
	if req.Text == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "内容不能为空"})
		return
	}
	// 昵称为空时给个中性默认值：模型看到的发言者不能没有名字。
	if strings.TrimSpace(req.UserName) == "" {
		req.UserName = "网友"
	}
	// 昵称截断：它会渲染进每条消息的前缀，几十字的昵称会污染上下文。
	if n := []rune(req.UserName); len(n) > 24 {
		req.UserName = string(n[:24])
	}

	if s.hub == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "会话中心未就绪"})
		return
	}

	msgID := "webui-" + time.Now().Format("20060102150405") + "-" + randomHex(4)
	msg := &domain.Message{
		ID:         msgID,
		Platform:   domain.PlatformWebUI,
		MessageID:  msgID,
		UserID:     domain.WebUITargetID,
		UserName:   req.UserName,
		IsGroup:    false,
		// 对齐 MaiBot 的 is_at_bot=True：网页消息总是视为 @ 了机器人，
		// 保证走「强制触发」而不是被 talk_value 概率拦下。
		IsAt:       true,
		Segments:   []domain.Segment{domain.Text(req.Text)},
		PlainText:  req.Text,
		ReceivedAt: time.Now(),
		AccountID:  domain.WebUIAccountID,
	}

	s.hub.Dispatch(msg, domain.ScopePrivate, domain.WebUITargetID, domain.WebUIAccountID)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"session_id": domain.WebUISessionKey().ID(),
		"message_id": msgID,
	})
}
