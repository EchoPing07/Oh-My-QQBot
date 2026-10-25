package chat

// 本文件覆盖 Filter 的自身消息识别 —— 之前 chat 包里没有 filter 的测试。
//
// 这条防线很关键：识别不出来就会自问自答。
// 现在有两条来源（实时事件与库恢复），任何一条漏判都会出问题。

import (
	"testing"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
)

// TestFilterSelfMessage 守的是自身消息识别的两条路径。
func TestFilterSelfMessage(t *testing.T) {
	f, err := NewFilter(nil, nil, nil)
	if err != nil {
		t.Fatalf("NewFilter 失败: %v", err)
	}
	f.SetBotIDs([]string{"bot-1"})

	cases := []struct {
		name string
		msg  *domain.Message
		want bool
	}{
		{
			name: "账号对得上（实时事件路径）",
			msg:  &domain.Message{UserID: "bot-1"},
			want: true,
		},
		{
			name: "IsSelf 标记（库恢复路径）",
			// 账号可能已经换了，但这条记录确实是机器人发的。
			msg:  &domain.Message{UserID: "old-bot-id", IsSelf: true},
			want: true,
		},
		{
			name: "普通用户",
			msg:  &domain.Message{UserID: "u1"},
			want: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := f.BanReason(c.msg) == "自身消息"
			if got != c.want {
				t.Errorf("识别为自己 = %v，期望 %v（原因: %q）", got, c.want, f.BanReason(c.msg))
			}
		})
	}
}

// TestFilterBanWordsAndWhitelist 守的是禁用词与群白名单。
func TestFilterBanWordsAndWhitelist(t *testing.T) {
	f, err := NewFilter([]string{"敏感词"}, []string{`(?i)spam\d+`}, []string{"g-allowed"})
	if err != nil {
		t.Fatalf("NewFilter 失败: %v", err)
	}

	if reason := f.BanReason(&domain.Message{IsGroup: true, GroupID: "g-allowed", PlainText: "正常聊天"}); reason != "" {
		t.Errorf("白名单群里的正常消息被拦: %q", reason)
	}
	if reason := f.BanReason(&domain.Message{IsGroup: true, GroupID: "g-other", PlainText: "正常聊天"}); reason != "群不在白名单" {
		t.Errorf("非白名单群应被拦，实际原因: %q", reason)
	}
	if reason := f.BanReason(&domain.Message{IsGroup: true, GroupID: "g-allowed", PlainText: "这里有敏感词"}); reason == "" {
		t.Error("禁用词消息应被拦")
	}
	if reason := f.BanReason(&domain.Message{IsGroup: true, GroupID: "g-allowed", PlainText: "SPAM123"}); reason == "" {
		t.Error("正则禁用模式应被拦")
	}
}
