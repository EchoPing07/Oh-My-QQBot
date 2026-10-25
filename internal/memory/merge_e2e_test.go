package memory

import (
	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"strings"
	"testing"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
)

// TestUpdateIfNeededMergesWithExisting 验证第二次更新不会覆盖掉第一次的信息。
//
// 这是画像合并接入的真实路径测试：走 UpdateIfNeeded（而不是直接调 MergeProfile），
// 确保「合并」这一步真的被调用到了。
func TestUpdateIfNeededMergesWithExisting(t *testing.T) {
	// 第一次返回「喜欢摄影」，第二次只返回「住校」。
	replies := []string{
		"## 身份设定\n- 大二学生\n## 稳定了解\n- 喜欢摄影",
		"## 身份设定\n- 住校",
	}
	call := 0
	srv, _ := newProfileServer(t, func() string {
		r := replies[len(replies)-1]
		if call < len(replies) {
			r = replies[call]
		}
		call++
		return r
	})

	db, cfg := openProfileTestDB(t)
	cfg.Memory.ProfileUpdateIntervalMessages = 1
	builder := NewProfileBuilder(config.Wrap(cfg), buildLLMClient(cfg, srv.URL), db)

	msgs := func(texts ...string) []*domain.Message {
		var out []*domain.Message
		for _, t2 := range texts {
			out = append(out, &domain.Message{
				UserID: "u1", UserName: "小明",
				Segments:   []domain.Segment{domain.Text(t2)},
				ReceivedAt: time.Now(),
			})
		}
		return out
	}

	// 第一次更新。
	if _, err := builder.UpdateIfNeeded(t.Context(), "qq", "u1", "s1", "小明", msgs("我在读大二")); err != nil {
		t.Fatalf("第一次更新失败: %v", err)
	}

	// 第二次更新（只提到住校）。
	if _, err := builder.UpdateIfNeeded(t.Context(), "qq", "u1", "s1", "小明", msgs("我住校")); err != nil {
		t.Fatalf("第二次更新失败: %v", err)
	}

	got, err := db.Memory().GetProfile(t.Context(), "qq", "u1", "s1")
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if got == nil {
		t.Fatal("画像应为空")
	}

	// 关键断言：第一次的「喜欢摄影」不能被第二次覆盖掉。
	if !strings.Contains(got.ProfileText, "喜欢摄影") {
		t.Errorf("第一次的信息被覆盖了：\n%s", got.ProfileText)
	}
	if !strings.Contains(got.ProfileText, "住校") {
		t.Errorf("第二次的信息没合并进来：\n%s", got.ProfileText)
	}
}
