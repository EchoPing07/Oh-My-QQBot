package mock_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/mock"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/store"
)

// fakeEmojiCollector 记录收到的表情包收藏请求。
type fakeEmojiCollector struct {
	calls []collectCall
}

type collectCall struct {
	SessionID string
	UserID    string
	Size      int
}

func (f *fakeEmojiCollector) Collect(_ context.Context, binary []byte, _ string, sessionID, userID string) (bool, error) {
	f.calls = append(f.calls, collectCall{SessionID: sessionID, UserID: userID, Size: len(binary)})
	return true, nil
}

// TestQueryMemoryToolIsRegisteredWhenEnabled 防的是「提示词描述了工具但没注册」。
//
// 这是踩过的坑：照抄了 query_memory 的说明却没实现，模型调用后失败、白烧一轮。
// 这里验证两件事：开关打开时工具可见，关闭时不可见。
func TestQueryMemoryToolIsRegisteredWhenEnabled(t *testing.T) {
	t.Run("打开时可调用", func(t *testing.T) {
		h := mock.NewHarness(t, mock.HarnessOptions{
			SetupConfig: func(cfg *config.Config) { cfg.Memory.Enabled = true },
			Planner: func(req *mock.RecordedRequest, _ int) *mock.Reply {
				if !hasTool(req, "query_memory") {
					t.Errorf("Planner 请求里应包含 query_memory，实际工具：%v", toolNames(req))
				}
				return mock.ToolReply("stop", `{}`)
			},
		})
		h.SendMessage("你好", true)
		h.WaitForLLMCalls(1, 10*time.Second)
	})

	t.Run("关闭时不可调用", func(t *testing.T) {
		h := mock.NewHarness(t, mock.HarnessOptions{
			SetupConfig: func(cfg *config.Config) { cfg.Memory.Enabled = false },
			Planner: func(req *mock.RecordedRequest, _ int) *mock.Reply {
				if hasTool(req, "query_memory") {
					t.Error("关闭后不应暴露 query_memory")
				}
				return mock.ToolReply("stop", `{}`)
			},
		})
		h.SendMessage("你好", true)
		h.WaitForLLMCalls(1, 10*time.Second)
	})
}

// TestQueryMemoryReturnsStoredSummary 端到端验证：
// 库里存了回想 → Planner 调 query_memory → 结果被写回上下文。
func TestQueryMemoryReturnsStoredSummary(t *testing.T) {
	var dbRef *store.DB
	var plannerCalls int

	h := mock.NewHarness(t, mock.HarnessOptions{
		SetupConfig: func(cfg *config.Config) { cfg.Memory.Enabled = true },
		Planner: func(req *mock.RecordedRequest, _ int) *mock.Reply {
			plannerCalls++

			if plannerCalls == 1 {
				return mock.ToolReply("query_memory", `{"query":"火锅","mode":"search"}`)
			}
			// 第二轮：检查上一轮的工具结果里有没有记忆内容。
			if !requestContains(req, "火锅很好吃") {
				t.Errorf("第二轮请求里没有记忆内容，请求文本：%s", dumpText(req))
			}
			return mock.ToolReply("stop", `{}`)
		},
	})

	// 往库里塞一条记忆，需要先拿到 harness 的 DB。
	dbRef = h.DB
	if err := dbRef.Memory().SaveSummary(context.Background(), &store.MemorySummary{
		SessionID:  "",
		Summary:    "上次聊到火锅很好吃，约好周末再去",
		RecallCues: []string{"讨论火锅"},
		TimeRange:  "2026/10/20 20:00",
		CreatedAt:  time.Now().Unix(),
	}); err != nil {
		t.Fatalf("写入记忆失败: %v", err)
	}

	h.SendMessage("还记得上次吃的火锅吗", true)
	h.WaitForLLMCalls(2, 15*time.Second)

	if plannerCalls < 2 {
		t.Fatalf("Planner 只被调用了 %d 次，期望至少 2", plannerCalls)
	}
}

// TestSendEmojiIsRegisteredWhenEnabled 验证表情包工具按开关注册。
func TestSendEmojiIsRegisteredWhenEnabled(t *testing.T) {
	h := mock.NewHarness(t, mock.HarnessOptions{
		SetupConfig: func(cfg *config.Config) { cfg.Emoji.Enabled = true },
		Planner: func(req *mock.RecordedRequest, _ int) *mock.Reply {
			for _, want := range []string{"send_emoji", "show_emoji_list"} {
				if !hasTool(req, want) {
					t.Errorf("缺少工具 %s，实际：%v", want, toolNames(req))
				}
			}
			return mock.ToolReply("stop", `{}`)
		},
	})
	h.SendMessage("你好", true)
	h.WaitForLLMCalls(1, 10*time.Second)
}

// TestSendEmojiWithEmptyLibraryDoesNotFail 验证「库里没表情」不是错误。
//
// 如果返回 IsError，模型会以为操作失败然后反复重试，白白烧掉整轮。
func TestSendEmojiWithEmptyLibraryDoesNotFail(t *testing.T) {
	var plannerCalls int

	h := mock.NewHarness(t, mock.HarnessOptions{
		SetupConfig: func(cfg *config.Config) { cfg.Emoji.Enabled = true },
		Planner: func(req *mock.RecordedRequest, _ int) *mock.Reply {
			plannerCalls++
			if plannerCalls == 1 {
				return mock.ToolReply("send_emoji", `{"emotion":"开心"}`)
			}
			// 第二轮应该看到「没有贴合表情」的提示，而不是错误。
			if requestContains(req, "执行出错") {
				t.Errorf("空库不该产生错误结果：%s", dumpText(req))
			}
			return mock.ToolReply("stop", `{}`)
		},
	})

	h.SendMessage("你好", true)
	h.WaitForLLMCalls(2, 15*time.Second)
}

// TestEmojiCollectedFromInboundMessage 验证入站表情包会被收藏。
func TestEmojiCollectedFromInboundMessage(t *testing.T) {
	collector := &fakeEmojiCollector{}

	h := mock.NewHarness(t, mock.HarnessOptions{
		Emoji: collector,
		Planner: func(_ *mock.RecordedRequest, _ int) *mock.Reply {
			return mock.ToolReply("stop", `{}`)
		},
	})

	msg := &domain.Message{
		ID:        "m-emoji",
		Platform:  "qq",
		MessageID: "wx-emoji",
		UserID:    "u1",
		UserName:  "小明",
		GroupID:   "g1",
		GroupName: "测试群",
		IsGroup:   true,
		Segments: []domain.Segment{
			{Type: domain.SegmentEmoji, Binary: []byte("fake-image-bytes"), MimeType: "image/png"},
		},
		PlainText:  "[表情]",
		ReceivedAt: time.Now(),
	}
	h.DispatchMessage(msg, "group", "g1", "bot-1")

	// 收藏是同步在 Dispatch 里做的，但投递是异步的，稍等一下。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(collector.calls) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if len(collector.calls) != 1 {
		t.Fatalf("收藏被调用 %d 次，期望 1", len(collector.calls))
	}
	if collector.calls[0].UserID != "u1" {
		t.Errorf("收藏者 = %q，期望 u1", collector.calls[0].UserID)
	}
	if collector.calls[0].Size != len("fake-image-bytes") {
		t.Errorf("二进制大小 = %d", collector.calls[0].Size)
	}
}

// TestPlainImageIsNotCollected 验证普通图片不会被当成表情包收藏。
//
// 群里的随手拍、截图都走 image 段，全收会让库变成垃圾场。
func TestPlainImageIsNotCollected(t *testing.T) {
	collector := &fakeEmojiCollector{}

	h := mock.NewHarness(t, mock.HarnessOptions{
		Emoji: collector,
		Planner: func(_ *mock.RecordedRequest, _ int) *mock.Reply {
			return mock.ToolReply("stop", `{}`)
		},
	})

	msg := &domain.Message{
		ID:        "m-image",
		Platform:  "qq",
		MessageID: "wx-image",
		UserID:    "u1",
		UserName:  "小明",
		GroupID:   "g1",
		IsGroup:   true,
		Segments: []domain.Segment{
			{Type: domain.SegmentImage, Binary: []byte("a-photo"), MimeType: "image/jpeg"},
		},
		PlainText:  "[图片]",
		ReceivedAt: time.Now(),
	}
	h.DispatchMessage(msg, "group", "g1", "bot-1")

	time.Sleep(300 * time.Millisecond)
	if len(collector.calls) != 0 {
		t.Errorf("普通图片不该被收藏，实际调用 %d 次", len(collector.calls))
	}
}

// ============ 辅助函数 ============

func hasTool(req *mock.RecordedRequest, name string) bool {
	for _, t := range req.Tools {
		if t.Function.Name == name {
			return true
		}
	}
	return false
}

func toolNames(req *mock.RecordedRequest) []string {
	out := make([]string, 0, len(req.Tools))
	for _, t := range req.Tools {
		out = append(out, t.Function.Name)
	}
	return out
}

// dumpText 把请求内容拼起来，失败时方便看。
func dumpText(req *mock.RecordedRequest) string {
	var b strings.Builder
	for _, m := range req.Messages {
		b.WriteString(m.ContentText())
		b.WriteString("\n---\n")
	}
	return b.String()
}
