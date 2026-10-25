package web

import (
	"encoding/json"
	"net/http"
	"time"
)

// 消息事件流：消息落库后立刻通知前端，页面不用等轮询。
//
// 对应 MaiBot dashboard 的统一 WebSocket chat 域事件
// （webui/routers/websocket/unified.py：消息入库即推送）。
// 我们用 SSE 单向通知代替 WS：
//   - 前端只需要「某会话有新消息」的信号，数据本身仍走
//     /api/messages 拉增量 —— 这样首屏、上翻、增量三处共用
//     同一条渲染路径，不会长出第二份「简化版」渲染逻辑
//   - SSE 基础设施（心跳、通道所有权、Shutdown 行为）在
//     /api/logs 已经踩过一遍坑，直接复用同一套约定
//   - 鉴权复用 Cookie，不需要 WS 那套一次性 token

// messageEvent 是推给前端的事件负载。
type messageEvent struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id"`
	IsSelf    bool   `json:"is_self"`
	AtMs      int64  `json:"at_ms"`
}

// MessageSaved 实现 router.MessageNotifier：一条消息写库成功后调用。
//
// 事件只是信号，不带消息体 —— 前端收到后按自己的游标拉增量，
// 拉回来的数据与首屏格式完全一致。
func (s *Server) MessageSaved(sessionID string, isSelf bool) {
	payload, err := json.Marshal(messageEvent{
		Type:      "message.new",
		SessionID: sessionID,
		IsSelf:    isSelf,
		AtMs:      time.Now().UnixMilli(),
	})
	if err != nil {
		return
	}
	line := string(payload)

	s.mu.Lock()
	defer s.mu.Unlock()

	for ch := range s.eventClients {
		select {
		case ch <- line:
		default:
			// 慢客户端丢帧而不是阻塞：丢的只是「该刷新了」的信号，
			// 前端还有轮询兜底会补上数据。
		}
	}
}

// handleEventsStream 通过 SSE 推送消息事件。
func (s *Server) handleEventsStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "不支持流式响应"})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	ch := make(chan string, 16)

	s.mu.Lock()
	s.eventClients[ch] = struct{}{}
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.eventClients, ch)
		s.mu.Unlock()
		close(ch)
	}()

	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return

		case line, ok := <-ch:
			if !ok {
				return
			}
			if _, err := w.Write([]byte("data: ")); err != nil {
				return
			}
			if _, err := w.Write([]byte(line)); err != nil {
				return
			}
			if _, err := w.Write([]byte("\n\n")); err != nil {
				return
			}
			flusher.Flush()

		case <-heartbeat.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
