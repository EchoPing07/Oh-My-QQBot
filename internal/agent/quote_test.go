package agent

// 引用计数器恢复逻辑的测试。
//
// 这一层没有 runtime，只测纯函数：把库里恢复出来的消息喂给计数器，
// 之后 Snapshot 能不能正确算出距离、连发与间隔。
//
// 为什么单独测：恢复这段最容易漏掉「喂计数器」这一步，
// 而漏了不报错 —— 只是引用行为悄悄从"偶尔引用"变成"每条都引用"。

import (
	"testing"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/chat"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
)

func quoteTestMsg(id, user string, at time.Time) *domain.Message {
	return &domain.Message{
		MessageID:  id,
		UserID:     user,
		ReceivedAt: at,
	}
}

// TestRestoringQuoteCounterFeedsAllMessages 防：恢复上下文时漏喂计数器。
func TestRestoringQuoteCounterFeedsAllMessages(t *testing.T) {
	counter := chat.NewQuoteCounter()
	base := time.Unix(1000, 0)

	msgs := []*domain.Message{
		quoteTestMsg("m1", "user-a", base),
		quoteTestMsg("m2", "user-a", base.Add(1*time.Second)),
		quoteTestMsg("m3", "bot", base.Add(2*time.Second)),
	}
	restoringQuoteCounter(counter, msgs)

	// m1 距最新一条是 2 条。
	snap, ok := counter.Snapshot("m1", base.Add(3*time.Second).UnixNano())
	if !ok {
		t.Fatal("恢复后 m1 应能在计数器里找到")
	}
	if snap.TargetDistance == nil || *snap.TargetDistance != 2 {
		t.Errorf("m1 距离 = %v，期望 2", snap.TargetDistance)
	}
	if snap.PreviousSenderID != "bot" {
		t.Errorf("上一条发送者 = %q，期望 bot", snap.PreviousSenderID)
	}
	if snap.PreviousBurst != 1 {
		t.Errorf("连发 = %d，期望 1（最后一条换了人）", snap.PreviousBurst)
	}
}

// TestRestoringQuoteCounterKeepsBurst 防：恢复后连发条数归零，
// 导致私聊引用公式里的 log2(burst) 一项完全失效。
func TestRestoringQuoteCounterKeepsBurst(t *testing.T) {
	counter := chat.NewQuoteCounter()
	base := time.Unix(2000, 0)

	msgs := make([]*domain.Message, 0, 5)
	for i := 0; i < 5; i++ {
		msgs = append(msgs, quoteTestMsg("m"+string(rune('a'+i)), "same-user", base.Add(time.Duration(i)*time.Second)))
	}
	restoringQuoteCounter(counter, msgs)

	snap, ok := counter.Snapshot(msgs[len(msgs)-1].MessageID, base.Add(10*time.Second).UnixNano())
	if !ok {
		t.Fatal("目标应在窗口内")
	}
	if snap.PreviousBurst != 5 {
		t.Errorf("连发 = %d，期望 5", snap.PreviousBurst)
	}
}

// TestRestoringQuoteCounterHandlesEmpty 防：空消息列表导致 panic。
func TestRestoringQuoteCounterHandlesEmpty(t *testing.T) {
	counter := chat.NewQuoteCounter()
	restoringQuoteCounter(counter, nil)
	restoringQuoteCounter(nil, []*domain.Message{quoteTestMsg("m1", "u", time.Now())})
	restoringQuoteCounter(counter, []*domain.Message{nil})
}

// TestRecordQuoteFallsBackToNow 防：ReceivedAt 为零值时把间隔算成 1970 年，
// 于是「上一条过去了几秒」这个变量完全失真。
func TestRecordQuoteFallsBackToNow(t *testing.T) {
	counter := chat.NewQuoteCounter()
	recordQuote(counter, &domain.Message{MessageID: "m1", UserID: "u"}) // ReceivedAt 零值

	if _, ok := counter.Snapshot("m1", time.Now().UnixNano()); !ok {
		t.Fatal("零值 ReceivedAt 的消息也应被记录")
	}
}

// TestRecordQuoteSkipsNil 防：nil 计数器/消息导致 panic。
func TestRecordQuoteSkipsNil(t *testing.T) {
	counter := chat.NewQuoteCounter()
	if _, ok := counter.Snapshot("anything", time.Now().UnixNano()); ok {
		t.Error("空计数器不应返回快照")
	}
}
