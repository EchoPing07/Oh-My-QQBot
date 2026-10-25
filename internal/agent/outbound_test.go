package agent

// 本文件覆盖出站消息落库（outbound.go）—— 之前整条链路都没有的功能。
//
// 背景：出站消息过去只进内存 botReplies（上限 20 条、重启即丢），
// 导致 WebUI 会话页只能看到用户消息、重启后模型看不到自己说过什么。
// 这些测试钉住「发送成功就必须入库」与「恢复时必须按来源拆分」。

import (
	"context"
	"testing"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/logx"
)

// TestRecordOutboundPersistsMessage 守的是发送成功的消息真的进了库。
func TestRecordOutboundPersistsMessage(t *testing.T) {
	db, cfg := openAgentTestDB(t)
	r := newTestRuntime(t, cfg, db)

	segments := []domain.Segment{domain.Text("嗨，我是木木")}
	r.recordOutbound(context.Background(), segments, domain.SendResult{MessageIDs: []string{"out-1"}, RefIdxs: []string{"REFIDX_out_1"}}, "in-9")

	got, err := db.Messages().ByMessageID(context.Background(), "out-1")
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if got == nil {
		t.Fatal("出站消息没有落库")
	}
	if !got.IsSelf {
		t.Error("IsSelf 应为 true，否则重启后会被当成用户消息")
	}
	if got.PlainText != "嗨，我是木木" {
		t.Errorf("正文 = %q", got.PlainText)
	}
	if got.UserID != "bot-1" {
		t.Errorf("UserID = %q，期望机器人账号 bot-1", got.UserID)
	}
	if got.Additional["reply_to"] != "in-9" {
		t.Errorf("引用目标丢失: %+v", got.Additional)
	}
	if got.GroupID != "g1" {
		t.Errorf("群号 = %q，期望 g1", got.GroupID)
	}
}

// TestRecordOutboundSkipsEmptyIDs 守的是没有消息 ID 时不写脏数据。
//
// 发送失败、或平台没回 ID 时 ids 是空的，此时不该凭空造一条记录。
func TestRecordOutboundSkipsEmptyIDs(t *testing.T) {
	db, cfg := openAgentTestDB(t)
	r := newTestRuntime(t, cfg, db)

	r.recordOutbound(context.Background(), []domain.Segment{domain.Text("x")}, domain.SendResult{}, "")
	r.recordOutbound(context.Background(), []domain.Segment{domain.Text("x")}, domain.SendResult{MessageIDs: []string{"", ""}}, "")

	n, err := db.Messages().Count(context.Background())
	if err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if n != 0 {
		t.Errorf("空 ID 时应写入 0 条，实际 %d 条", n)
	}
}

// TestRecordOutboundNilStoreDoesNotPanic 守的是没有数据库时不崩。
//
// 单测与 livecheck 都会构造没有 store 的 Runtime。
func TestRecordOutboundNilStoreDoesNotPanic(t *testing.T) {
	r := &Runtime{log: logx.Get("agent.test")}
	r.recordOutbound(context.Background(), []domain.Segment{domain.Text("x")}, domain.SendResult{MessageIDs: []string{"id"}}, "")
}

// TestSplitRestoredMessages 守的是重启恢复时按来源拆分。
//
// 不拆的后果很隐蔽：机器人自己的话会被当成「某个用户说的」渲染，
// 于是模型以为自己没说过、重复自己；Replyer 也会看到一个冒充用户的自己。
func TestSplitRestoredMessages(t *testing.T) {
	now := time.Now()
	msgs := []*domain.Message{
		{MessageID: "m1", UserID: "u1", PlainText: "用户说", ReceivedAt: now},
		{MessageID: "m2", UserID: "bot-1", PlainText: "机器人说", IsSelf: true, ReceivedAt: now.Add(time.Second)},
		{MessageID: "m3", UserID: "u2", PlainText: "另一个人说", ReceivedAt: now.Add(2 * time.Second)},
		// 老数据没有 IsSelf 标记，靠账号比对识别。
		{MessageID: "m4", UserID: "bot-1", PlainText: "老的自发言", ReceivedAt: now.Add(3 * time.Second)},
	}

	history, replies := splitRestoredMessages(msgs, "bot-1")

	if len(history) != 2 {
		t.Fatalf("history 应有 2 条，实际 %d: %+v", len(history), history)
	}
	if history[0].MessageID != "m1" || history[1].MessageID != "m3" {
		t.Errorf("history 内容错误: %s,%s", history[0].MessageID, history[1].MessageID)
	}

	if len(replies) != 2 {
		t.Fatalf("botReplies 应有 2 条，实际 %d", len(replies))
	}
	if replies[0].Text != "机器人说" || replies[1].Text != "老的自发言" {
		t.Errorf("botReplies 内容错误: %q,%q", replies[0].Text, replies[1].Text)
	}
}

// TestSplitRestoredMessagesCapsReplies 守的是恢复出来的自发言也有上限。
//
// 不设上限的话，一个话多的会话恢复时会把几百条自发言全塞进上下文。
func TestSplitRestoredMessagesCapsReplies(t *testing.T) {
	now := time.Now()
	var msgs []*domain.Message
	for i := 0; i < 40; i++ {
		msgs = append(msgs, &domain.Message{
			MessageID:  "b",
			UserID:     "bot-1",
			PlainText:  "自发言",
			IsSelf:     true,
			ReceivedAt: now.Add(time.Duration(i) * time.Second),
		})
	}

	_, replies := splitRestoredMessages(msgs, "bot-1")
	if len(replies) != 20 {
		t.Errorf("botReplies = %d 条，期望封顶 20 条", len(replies))
	}
}

// TestPlannerInterruptController 守的是连续打断上限真的生效。
//
// 上限的语义照抄参考实现：配置为 0 时不打断正在进行的思考，
// 让它自然跑完 —— 否则群里连发会让模型永远跑不完一轮，
// 表现是「机器人完全不理人」而日志里全是重复请求。
func TestPlannerInterruptController(t *testing.T) {
	t.Run("没有进行中的请求时返回 idle", func(t *testing.T) {
		c := NewPlannerInterruptController()
		if got := c.Request(3); got != InterruptIdle {
			t.Errorf("返回值 = %q，期望 idle", got)
		}
	})

	t.Run("同一轮只打断一次", func(t *testing.T) {
		c := NewPlannerInterruptController()
		c.Bind(nil)

		if got := c.Request(3); got != InterruptRequest {
			t.Fatalf("第一次 = %q，期望 request", got)
		}
		if got := c.Request(3); got != InterruptDuplicate {
			t.Errorf("第二次 = %q，期望 duplicate（不重复打断同一轮）", got)
		}
		if c.Consecutive() != 1 {
			t.Errorf("连续打断次数 = %d，期望 1", c.Consecutive())
		}
	})

	t.Run("上限为 0 时不打断", func(t *testing.T) {
		c := NewPlannerInterruptController()
		c.Bind(nil)
		if got := c.Request(0); got != InterruptLimit {
			t.Errorf("返回值 = %q，期望 limit（0 表示不打断）", got)
		}
		if c.Consecutive() != 0 {
			t.Errorf("被拒的打断不该计数，实际 %d", c.Consecutive())
		}
	})

	t.Run("达到上限后拒绝", func(t *testing.T) {
		c := NewPlannerInterruptController()

		for i := 0; i < 2; i++ {
			c.Bind(nil)
			if got := c.Request(2); got != InterruptRequest {
				t.Fatalf("第 %d 次 = %q，期望 request", i+1, got)
			}
			// 被打断结束：计数保留。
			c.Unbind(true)
		}

		c.Bind(nil)
		if got := c.Request(2); got != InterruptLimit {
			t.Errorf("达到上限后 = %q，期望 limit", got)
		}
	})

	t.Run("自然完成会清零计数", func(t *testing.T) {
		c := NewPlannerInterruptController()
		c.Bind(nil)
		c.Request(5)
		c.Unbind(true)
		if c.Consecutive() != 1 {
			t.Fatalf("计数 = %d，期望 1", c.Consecutive())
		}

		c.Bind(nil)
		c.Unbind(false) // 自然跑完
		if c.Consecutive() != 0 {
			t.Errorf("自然完成后计数 = %d，期望 0", c.Consecutive())
		}
	})

	t.Run("打断会调用 cancel", func(t *testing.T) {
		c := NewPlannerInterruptController()
		canceled := false
		c.Bind(func() { canceled = true })

		c.Request(1)
		if !canceled {
			t.Error("request 结果应触发 cancel")
		}
	})
}
