package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
)

// WebUISender 是网页对话窗口（platform=webui）的发送出口。
//
// 对应 MaiBot 里 webui 平台自己的 sender：网页会话的回复只回网页，
// 不发往任何真实平台。它不需要真正「发送」什么 —— 只要生成唯一消息 ID
// 返回成功，recordOutbound 就会把回复落库（IsSelf=true），
// WebUI 轮询 /api/messages 即可看到机器人说了什么。
//
// passiveTo（被动回复额度）与 quote（引用）在这里没有意义：
// 网页窗口没有被动窗口限制，也没有 REFIDX 形态的引用索引。
type WebUISender struct{}

// SendText 实现 Sender 接口。文本可能被上层拆成一条，这里固定返回一个 ID。
func (WebUISender) SendText(_ context.Context, _, _, _ string, _ string, _ domain.Reference) (domain.SendResult, error) {
	return webUISendResult(1), nil
}

// SendSegments 实现 Sender 接口。按段数返回对应数量的 ID：
// 出站落库以「有几条 ID 就有几条记录」为准（见 recordOutbound）。
func (WebUISender) SendSegments(_ context.Context, _, _ string, segments []domain.Segment, _ string, _ domain.Reference) (domain.SendResult, error) {
	if len(segments) == 0 {
		return domain.SendResult{}, nil
	}
	return webUISendResult(len(segments)), nil
}

// webUISendResult 生成 n 条唯一消息 ID。
//
// 出站消息的 MessageID 会落库并被 WebUI 用来定位，只要全局唯一即可；
// 用随机字节而不是计数器，避免重启后撞上旧 ID。
func webUISendResult(n int) domain.SendResult {
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			// crypto/rand 失败几乎只意味着系统熵池出了问题，
			// 这里不能静默：ID 撞了会覆盖库里的出站记录。
			panic("webui sender: 生成消息 ID 失败: " + err.Error())
		}
		ids = append(ids, "webui-"+hex.EncodeToString(b[:]))
	}
	return domain.SendResult{MessageIDs: ids}
}
