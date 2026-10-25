package web

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/logx"
)

// handleLogsStream 通过 SSE 推送实时日志。
//
// 为什么用 SSE 而不是 WebSocket：
// 日志是单向推送，SSE 更简单 —— 浏览器原生 EventSource，
// 不用引入 ws 库，也不用处理重连协议（浏览器自己会重连）。
func (s *Server) handleLogsStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "不支持流式响应"})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// 关闭 Nginx 之类的缓冲。
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	ch := make(chan logx.LogLine, 256)

	s.mu.Lock()
	s.clients[ch] = struct{}{}
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.clients, ch)
		s.mu.Unlock()
		close(ch)
	}()

	// 心跳，避免中间层掐掉空闲连接。
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
			payload, err := json.Marshal(line)
			if err != nil {
				continue
			}
			if _, err := w.Write([]byte("data: ")); err != nil {
				return
			}
			if _, err := w.Write(payload); err != nil {
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
