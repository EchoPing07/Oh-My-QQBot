package agent

import (
	"testing"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/prompt"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/tool"
)

func TestEmojiHintProbability(t *testing.T) {
	cfg := config.Default()
	cfg.Bot.Nickname = "木木"

	pm, err := prompt.New("", "zh-CN")
	if err != nil {
		t.Fatal(err)
	}
	b := NewPromptBuilder(config.Wrap(cfg), pm)

	reg := tool.NewRegistry()
	reg.MustRegister(tool.NewSendEmojiTool())
	av := tool.Availability{IsGroup: true, Platform: "qq"}

	// 概率 0：永远不出现
	cfg.Emoji.SendProbability = 0
	for i := 0; i < 50; i++ {
		if got := b.emojiHintRule(reg, av); got != "" {
			t.Fatalf("概率 0 时应始终为空，第 %d 次得到 %q", i, got)
		}
	}

	// 概率 1：每次都出现
	cfg.Emoji.SendProbability = 1
	for i := 0; i < 20; i++ {
		if got := b.emojiHintRule(reg, av); got == "" {
			t.Fatalf("概率 1 时应始终有内容")
		}
	}

	// 概率 0.5：大致一半（宽松区间，避免偶发失败）
	cfg.Emoji.SendProbability = 0.5
	hits := 0
	const n = 400
	for i := 0; i < n; i++ {
		if b.emojiHintRule(reg, av) != "" {
			hits++
		}
	}
	if hits < n*3/10 || hits > n*7/10 {
		t.Errorf("概率 0.5 时命中 %d/%d，偏离过大", hits, n)
	}
}
