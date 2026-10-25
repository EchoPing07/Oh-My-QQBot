package chat

import (
	"testing"
)

// TestDebounceNaturalQuiet 验证「安静开批」的等待时间与 MaiBot 一致。
//
// natural 档安静开批应为 2 秒。
func TestDebounceNaturalQuiet(t *testing.T) {
	d := NewDebouncer()

	// 第一批消息，之前没有任何消息 → 安静开批。
	// receivedAt = wallTime 表示没有预处理延迟。
	now := 1000.0
	reason := d.Observe("alice", false, false, false, now, now, now, DebounceNatural)

	deadline, ok := d.Deadline()
	if !ok {
		t.Fatal("应该有截止时间")
	}

	// 安静开批 2 秒。
	if want := now + 2.0; deadline != want {
		t.Errorf("截止时间 = %v，期望 %v（安静开批 2 秒）", deadline, want)
	}
	if reason == "" {
		t.Error("应返回规则说明")
	}
}

// TestDebounceActiveBatch 验证「活跃开批」。
//
// 距上一条消息 30 秒内 = 活跃，natural 档应为 5 秒。
func TestDebounceActiveBatch(t *testing.T) {
	d := NewDebouncer()

	// 上一条消息在 now-10 秒（10 < 30，属于活跃）。
	d.Observe("alice", false, false, false, 990.0, 990.0, 990.0, DebounceNatural)
	d.Consume()

	// 新批次。
	now := 1000.0
	d.Observe("bob", false, false, false, now, now, now, DebounceNatural)

	deadline, _ := d.Deadline()
	// 活跃开批 5 秒，但整批上限是第一条第 8 秒。
	// 这里 first 是 1000，所以 1000+5 = 1005，未触及上限。
	if want := 1005.0; deadline != want {
		t.Errorf("截止时间 = %v，期望 %v（活跃开批 5 秒）", deadline, want)
	}
}

// TestDebounceSameAuthor 验证「同人续发」。
func TestDebounceSameAuthor(t *testing.T) {
	d := NewDebouncer()

	now := 1000.0
	d.Observe("alice", false, false, false, now, now, now, DebounceNatural)

	// 同人续发：natural 档 2 秒。
	now2 := 1001.0
	d.Observe("alice", false, false, false, now2, now2, now2, DebounceNatural)

	deadline, _ := d.Deadline()
	// candidate = arrival(1001) + 2 = 1003；上限 = first(1000) + 8 = 1008。
	if want := 1003.0; deadline != want {
		t.Errorf("截止时间 = %v，期望 %v（同人续发 2 秒）", deadline, want)
	}
}

// TestDebounceDifferentAuthor 验证「换人」。
func TestDebounceDifferentAuthor(t *testing.T) {
	d := NewDebouncer()

	now := 1000.0
	d.Observe("alice", false, false, false, now, now, now, DebounceNatural)

	// 换人：natural 档 0.5 秒。
	now2 := 1001.0
	d.Observe("bob", false, false, false, now2, now2, now2, DebounceNatural)

	deadline, _ := d.Deadline()
	if want := 1001.5; deadline != want {
		t.Errorf("截止时间 = %v，期望 %v（换人 0.5 秒）", deadline, want)
	}
}

// TestDebounceBatchLimit 验证 8 秒硬上限。
//
// 即便不断有同人续发，整批也不能超过第一条第 8 秒。
func TestDebounceBatchLimit(t *testing.T) {
	d := NewDebouncer()

	first := 1000.0
	d.Observe("alice", false, false, false, first, first, first, DebounceSteady)

	// steady 同人续发是 4 秒，连续续发会很快撞上限。
	for i := 1; i <= 5; i++ {
		at := first + float64(i)
		d.Observe("alice", false, false, false, at, at, at, DebounceSteady)
	}

	deadline, _ := d.Deadline()
	if deadline > first+batchLimit {
		t.Errorf("截止时间 = %v 超过整批上限 %v", deadline, first+batchLimit)
	}
}

// TestDebounceMentionLimit 验证 @ 的 1 秒上限。
//
// 被 @ 后最多等 1 秒，优先于其他等待规则。
func TestDebounceMentionLimit(t *testing.T) {
	d := NewDebouncer()

	now := 1000.0
	d.Observe("alice", false, false, true, now, now, now, DebounceSteady)

	deadline, _ := d.Deadline()
	// steady 安静开批 4 秒，但 @ 上限 1 秒。
	if want := now + mentionLimit; deadline != want {
		t.Errorf("截止时间 = %v，期望 %v（@ 上限 1 秒）", deadline, want)
	}
}

// TestDebounceBareImage 验证裸图开批至少 4 秒。
func TestDebounceBareImage(t *testing.T) {
	d := NewDebouncer()

	now := 1000.0
	// fast 档安静开批 1 秒，但裸图至少 4 秒。
	d.Observe("alice", true, false, false, now, now, now, DebounceFast)

	deadline, _ := d.Deadline()
	if want := now + bareImageMinWait; deadline != want {
		t.Errorf("截止时间 = %v，期望 %v（裸图至少 4 秒）", deadline, want)
	}
}

// TestDebouncePureEmojiNoExtend 验证纯表情包不延长等待。
//
// 纯表情包可以把截止时间提前，但不能在已有批次上继续续长。
func TestDebouncePureEmojiNoExtend(t *testing.T) {
	d := NewDebouncer()

	now := 1000.0
	d.Observe("alice", false, false, false, now, now, now, DebounceNatural)

	before, _ := d.Deadline()

	// 纯表情包，间隔较大本来会把 deadline 推后，但规则要求不能延长。
	at := now + 1.0
	d.Observe("alice", false, true, false, at, at, at, DebounceNatural)

	after, _ := d.Deadline()
	if after > before {
		t.Errorf("纯表情包把截止时间从 %v 延长到 %v，不应延长", before, after)
	}
}

// TestDebounceExpiredNotExtended 验证已到期批次不被新消息延长。
//
// 这是原版一个很重要的细节：模型忙的时候批次会过期，
// 此时新消息不应该重新排队，而是直接进入下一次消费。
func TestDebounceExpiredNotExtended(t *testing.T) {
	d := NewDebouncer()

	now := 1000.0
	d.Observe("alice", false, false, false, now, now, now, DebounceNatural)
	expired, _ := d.Deadline()

	// 时间已经越过截止点。
	later := expired + 5.0
	reason := d.Observe("bob", false, false, false, later, later, later, DebounceNatural)

	after, _ := d.Deadline()
	if after != expired {
		t.Errorf("已到期批次被延长：%v -> %v", expired, after)
	}
	if reason != "批次已到期，不再延长等待" {
		t.Errorf("说明 = %q，期望提示已到期", reason)
	}
}

// TestDebounceConsumeResets 验证 Consume 后批次状态清空。
func TestDebounceConsumeResets(t *testing.T) {
	d := NewDebouncer()

	now := 1000.0
	d.Observe("alice", false, false, false, now, now, now, DebounceNatural)
	d.Consume()

	if _, ok := d.Deadline(); ok {
		t.Error("Consume 后不应有截止时间")
	}
}

// TestDebounceOffMode 验证 off 档不生效时的行为。
//
// off 不在 waitProfiles 里，应该回退到 natural。
func TestDebounceOffMode(t *testing.T) {
	d := NewDebouncer()

	now := 1000.0
	d.Observe("alice", false, false, false, now, now, now, DebounceOff)

	deadline, ok := d.Deadline()
	if !ok {
		t.Fatal("应该有截止时间")
	}
	// 回退到 natural 安静开批 2 秒。
	if want := now + 2.0; deadline != want {
		t.Errorf("截止时间 = %v，期望 %v（回退 natural）", deadline, want)
	}
}
