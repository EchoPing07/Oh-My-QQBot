package web

// /api/events 事件流的测试：SSE 推送、鉴权、事件负载。
//
// 事件流是前端实时刷新的主通道（对应 MaiBot dashboard 的
// chat 域 WS 推送），这些测试守的是「消息落库 → 前端能立刻知道」。

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestEventsRequiresAuth 守「事件流必须登录才能连」。
// 它和日志流一样会持续输出内容，匿名连上等于实时旁听所有会话动态。
func TestEventsRequiresAuth(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	resp, body := env.do(http.MethodGet, "/api/events", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("未登录状态码 = %d，期望 401，body=%s", resp.StatusCode, body)
	}
}

// TestEventsStreamPushesMessageEvents 是主链路：
// 打开 SSE → MessageSaved → 客户端收到 message.new 事件。
func TestEventsStreamPushesMessageEvents(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	env.loginOK()

	req, err := http.NewRequest(http.MethodGet, env.ts.URL+"/api/events", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("连接事件流失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("事件流状态码 = %d，期望 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q，期望 text/event-stream", ct)
	}

	events := make(chan string, 8)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "data: ") {
				select {
				case events <- strings.TrimPrefix(line, "data: "):
				default:
				}
			}
		}
	}()

	// 请求返回 ≠ handler 已把通道注册进广播表，
	// 期间发的通知会丢。所以重发直到读到，总时长 2 秒足够。
	deadline := time.After(2 * time.Second)
	for {
		env.s.MessageSaved(groupSessionID, true)
		select {
		case raw := <-events:
			var ev messageEvent
			if err := json.Unmarshal([]byte(raw), &ev); err != nil {
				t.Fatalf("事件不是合法 JSON: %v, raw=%s", err, raw)
			}
			if ev.Type != "message.new" {
				t.Errorf("事件 type = %q，期望 message.new", ev.Type)
			}
			if ev.SessionID != groupSessionID {
				t.Errorf("事件 session_id = %q，期望 %q", ev.SessionID, groupSessionID)
			}
			if !ev.IsSelf {
				t.Errorf("事件 is_self = false，期望 true")
			}
			if ev.AtMs <= 0 {
				t.Errorf("事件 at_ms = %d，期望大于 0", ev.AtMs)
			}
			return
		case <-time.After(80 * time.Millisecond):
			// 还没注册上或还没读到，重发一次。
		case <-deadline:
			t.Fatal("2 秒内没收到 message.new 事件")
		}
	}
}

// TestMessageSavedWithoutSubscribersNoop 守「没有订阅者时广播不 panic」。
// 程序启动后、第一个前端连上事件流之前，每条消息都会走这条路径。
func TestMessageSavedWithoutSubscribersNoop(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	env.s.MessageSaved("any-session", false) // 不应 panic
	env.s.MessageSaved("any-session", true)
}
