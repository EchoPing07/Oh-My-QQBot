package memory

import (
	"strings"
	"testing"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
)

// TestProfileCounterAccumulatesAcrossTurns 是「画像再也不会更新」这个 bug 的回归测试。
//
// 缺陷形态：拿「本批次条数」直接与「累计条数 + 阈值」比，
// 1 < 21 永远成立，于是第一次生成后就再也不更新，且不报错。
//
// 这个测试用阈值 3、每轮只来 1 条消息，验证第 3 轮才触发。
func TestProfileCounterAccumulatesAcrossTurns(t *testing.T) {
	var calls int
	srv, _ := newProfileServer(t, func() string {
		calls++
		return "## 身份设定\n- 第" + string(rune('0'+calls)) + "次"
	})

	db, cfg := openProfileTestDB(t)
	cfg.Memory.ProfileUpdateIntervalMessages = 3
	builder := NewProfileBuilder(config.Wrap(cfg), buildLLMClient(cfg, srv.URL), db)

	one := func(text string) []*domain.Message {
		return []*domain.Message{{
			UserID: "u1", UserName: "小明",
			Segments:   []domain.Segment{domain.Text(text)},
			ReceivedAt: time.Now(),
		}}
	}

	// 第 1、2 轮：累计 1、2，未达阈值 3，不该调模型。
	for i := 1; i <= 2; i++ {
		updated, err := builder.UpdateIfNeeded(t.Context(), "qq", "u1", "s1", "小明", one("消息"))
		if err != nil {
			t.Fatalf("第 %d 轮失败: %v", i, err)
		}
		if updated {
			t.Fatalf("第 %d 轮不该更新（累计 %d < 阈值 3）", i, i)
		}
	}
	if calls != 0 {
		t.Fatalf("前两轮不该调用模型，实际 %d 次", calls)
	}

	// 第 3 轮：累计到 3，触发。
	updated, err := builder.UpdateIfNeeded(t.Context(), "qq", "u1", "s1", "小明", one("消息"))
	if err != nil {
		t.Fatalf("第 3 轮失败: %v", err)
	}
	if !updated {
		t.Fatal("第 3 轮应该触发更新")
	}
	if calls != 1 {
		t.Errorf("应调用模型 1 次，实际 %d", calls)
	}

	// 计数器应已清零：第 4 轮不该立刻再触发。
	updated, err = builder.UpdateIfNeeded(t.Context(), "qq", "u1", "s1", "小明", one("消息"))
	if err != nil {
		t.Fatalf("第 4 轮失败: %v", err)
	}
	if updated {
		t.Error("更新后计数器应清零，第 4 轮不该立刻再触发")
	}
}

// TestProfileCounterSeparatePerSession 验证不同会话的计数器互不影响。
//
// 同一个人在不同群里画像不同，计数器也要分开：
// 否则一个活跃群会把另一个安静群的更新提前触发。
func TestProfileCounterSeparatePerSession(t *testing.T) {
	var calls int
	srv, _ := newProfileServer(t, func() string {
		calls++
		return "## 身份设定\n- x"
	})

	db, cfg := openProfileTestDB(t)
	cfg.Memory.ProfileUpdateIntervalMessages = 2
	builder := NewProfileBuilder(config.Wrap(cfg), buildLLMClient(cfg, srv.URL), db)

	one := []*domain.Message{{
		UserID: "u1", UserName: "小明",
		Segments:   []domain.Segment{domain.Text("消息")},
		ReceivedAt: time.Now(),
	}}

	// 会话 A 累计 1 条。
	if _, err := builder.UpdateIfNeeded(t.Context(), "qq", "u1", "session-A", "小明", one); err != nil {
		t.Fatalf("A 失败: %v", err)
	}
	// 会话 B 累计 1 条 —— 不该因为 A 的累计而触发。
	if _, err := builder.UpdateIfNeeded(t.Context(), "qq", "u1", "session-B", "小明", one); err != nil {
		t.Fatalf("B 失败: %v", err)
	}
	if calls != 0 {
		t.Fatalf("两个会话各 1 条，都不该触发，实际调用 %d 次", calls)
	}

	// A 再来一条，A 触发；B 仍不该触发。
	updated, err := builder.UpdateIfNeeded(t.Context(), "qq", "u1", "session-A", "小明", one)
	if err != nil {
		t.Fatalf("A 第二轮失败: %v", err)
	}
	if !updated {
		t.Error("session-A 累计到 2 条应触发")
	}
	if calls != 1 {
		t.Errorf("应调用 1 次，实际 %d", calls)
	}
}

// TestProfileCounterKeyFormat 验证计数器键包含三段信息。
func TestProfileCounterKeyFormat(t *testing.T) {
	key := profileCounterKey("qq", "u1", "s1")

	for _, want := range []string{"profile.counter", "qq", "u1", "s1"} {
		if !strings.Contains(key, want) {
			t.Errorf("键 %q 缺少 %q", key, want)
		}
	}
}
