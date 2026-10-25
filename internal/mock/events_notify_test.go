package mock_test

// 事件流通知的端到端测试：入站与出站消息落库后，
// WebUI 的事件通道都必须收到通知（前端实时刷新靠这条线）。

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/mock"
)

// notifyRecorder 记录全部 MessageSaved 调用。
type notifyRecorder struct {
	mu      sync.Mutex
	records []string
}

func (n *notifyRecorder) MessageSaved(sessionID string, isSelf bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.records = append(n.records, sessionID+"|"+map[bool]string{true: "self", false: "user"}[isSelf])
}

func (n *notifyRecorder) snapshot() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]string, len(n.records))
	copy(out, n.records)
	return out
}

// TestMessageEventsNotifiedEndToEnd 走完整链路（真 hub、真 runtime、
// mock 模型）：用户发言与机器人回复各自落库后，通知都要发出来。
//
// 缺了入站通知：前端看不到新消息实时出现。
// 缺了出站通知：前端看不到机器人的回复，「正在输入」提示撤不掉。
func TestMessageEventsNotifiedEndToEnd(t *testing.T) {
	rec := &notifyRecorder{}

	h := mock.NewHarness(t, mock.HarnessOptions{
		Planner: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.ToolReply("reply", `{"msg_id":"`+extractMsgID(t, req)+`","reply_reference":"回应"}`)
		},
		Replyer: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.TextReply("收到")
		},
	})
	h.Hub.SetNotifier(rec)

	h.SendMessage("你好", true)
	h.WaitForSend(10 * time.Second)
	// 出站落库在发送成功之后异步进行，给它一点时间。
	time.Sleep(500 * time.Millisecond)

	got := rec.snapshot()
	if len(got) < 2 {
		t.Fatalf("通知记录 = %v，期望至少 2 条（用户一条 + 机器人一条）", got)
	}

	var sawUser, sawSelf bool
	for _, r := range got {
		switch {
		case strings.HasSuffix(r, "|user"):
			sawUser = true
		case strings.HasSuffix(r, "|self"):
			sawSelf = true
		}
	}
	if !sawUser {
		t.Errorf("没有收到入站消息（user）通知，记录 = %v", got)
	}
	if !sawSelf {
		t.Errorf("没有收到出站回复（self）通知，记录 = %v", got)
	}

	// 所有通知必须指向同一个会话：事件里的 session_id 是前端
	// 判断「要不要拉当前会话增量」的依据，拼错了就永远匹配不上。
	sessions, err := h.DB.Sessions().List(context.Background(), 5)
	if err != nil || len(sessions) == 0 {
		t.Fatalf("查会话失败: err=%v len=%d", err, len(sessions))
	}
	want := sessions[0].ID
	for _, r := range got {
		if !strings.HasPrefix(r, want+"|") {
			t.Errorf("通知的会话 = %q，期望 %q", r, want)
			break
		}
	}
}
