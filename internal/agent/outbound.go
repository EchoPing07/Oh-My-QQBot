package agent

import (
	"context"
	"strings"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
)

// 出站消息入库。
//
// 对应参考实现 MaiBot 的 send_service.py:_store_sent_message ——
// 每条发送成功的消息都写库，source_kind 记为 guided_reply。
//
// 为什么必须做（之前一直缺）：
//   - 重启后恢复上下文只捞到用户说的那半边，模型看不到自己说过什么，
//     于是会重复自己、也判断不出「上一句是不是我说的」
//   - WebUI 的会话页只能看到用户消息，看起来像机器人从没回过话
//   - 引用计数器重启后的初始状态少一半依据
//
// 写入的是「已经发出去的内容」而不是「打算发的内容」，
// 所以必须在 sender 返回成功之后调用。

// recordOutbound 把一条发送成功的出站消息落库。
//
// 失败只记日志不打断发送：消息已经在平台侧送达了，
// 落库失败是本地问题，不该让调用方以为发送失败。
func (r *Runtime) recordOutbound(ctx context.Context, segments []domain.Segment, sent domain.SendResult, replyTo string) {
	if r.store == nil || len(sent.MessageIDs) == 0 {
		return
	}

	// 每条消息一个 ID。平台返回的 ID 数量与内容是一一对应的，
	// 但为稳妥起见按「有几条 ID 就有几条记录」写，
	// 内容统一用本次发送的段 —— 一段文本即使被平台拆成两条，
	// 语义上也还是同一句话。
	now := time.Now()
	plain := domain.BuildPlainText(segments)
	if plain == "" {
		plain = "[非文本消息]"
	}
	savedAny := false

	// additional 记下引用了谁，WebUI 与恢复上下文都能用上。
	additional := map[string]string{}
	if replyTo != "" {
		additional[domain.AdditionalReplyTo] = replyTo
	}

	for i, id := range sent.MessageIDs {
		if id == "" {
			continue
		}
		msg := &domain.Message{
			ID:         id,
			Platform:   r.key.Platform,
			MessageID:  id,
			SessionID:  r.sessionID,
			UserID:     r.accountID,
			UserName:   r.botName,
			GroupID:    groupIDOf(r),
			IsGroup:    r.isGroup,
			IsSelf:     true,
			Segments:   segments,
			PlainText:  plain,
			ReceivedAt: now,
			AccountID:  r.accountID,
			Additional: additional,
		}
		// 记下这条出站消息自己的引用索引。
		//
		// 这是「引用木木自己的发言」唯一的索引来源：入站事件里只有
		// 别人的 msg_idx，机器人自己发的消息只有发送响应的 ext_info.ref_idx。
		// 不存的话，群里有人说「你刚才那句」时按 ref_msg_idx 反查永远查不到，
		// 引用块只能显示一串 ID。
		if i < len(sent.RefIdxs) {
			if refIdx := strings.TrimSpace(sent.RefIdxs[i]); refIdx != "" {
				msg.RefMsgIdx = refIdx
				msg.Additional[domain.AdditionalMsgIdx] = refIdx
			}
		}
		if err := r.store.Messages().Save(ctx, msg); err != nil {
			r.log.Warn("出站消息落库失败", "session", r.sessionID, "msg_id", id, "err", err)
			continue
		}
		savedAny = true
	}

	// 通知一次就够：事件只是「这个会话有新消息」的信号，
	// 前端收到后按游标拉增量，一条与多条都会拉全。
	if savedAny && r.onSaved != nil {
		r.onSaved(r.sessionID, true)
	}
}

// groupIDOf 返回群号，私聊时为空。
func groupIDOf(r *Runtime) string {
	if !r.isGroup {
		return ""
	}
	return r.key.TargetID
}

// RememberReply 记录本次回复，供后续引用判断。
//
// 这里只管内存里的 botReplies（引用与概率估算要用），
// 落库由 recordOutbound 负责 —— 两者生命周期不同：
// botReplies 只保留最近 20 条，库里的记录是永久的。
//
// 出站消息的 MessageID 与 RefMsgIdx 由 recordOutbound 回填：
// 发送响应拿到之后才知道，而这里只是先把文本记下来。
func (r *Runtime) RememberReply(text string, messageIDs, refIdxs []string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}

	// 一段回复可能被切成多条发出，每条各有自己的 ID 与引用索引。
	// 全部记下来：模型引用木木「上一句话」时，指的可能是其中任意一条。
	for i, id := range messageIDs {
		refIdx := ""
		if i < len(refIdxs) {
			refIdx = strings.TrimSpace(refIdxs[i])
		}
		r.botReplies = append(r.botReplies, BotReply{
			Text:      text,
			At:        time.Now(),
			MessageID: id,
			RefMsgIdx: refIdx,
		})
	}

	// 一条 ID 都没有时仍然记一条纯文本，
	// 否则概率估算里「上一句是不是我说的」会缺一段。
	if len(messageIDs) == 0 {
		r.botReplies = append(r.botReplies, BotReply{Text: text, At: time.Now()})
	}

	// 只保留最近 20 条，避免无限增长。
	if len(r.botReplies) > 20 {
		r.botReplies = r.botReplies[len(r.botReplies)-20:]
	}
}

// outboundContext 为落库单独派生一个 ctx，带短超时。
//
// 不用调用方 ctx：回复成功之后它可能已被取消（打断），
// 用它会静默丢掉这次落库。
//
// parent 为 nil 时要兜住：单测会手工构造 &Runtime{}（没有 ctx），
// 而 context.WithoutCancel(nil) 直接 panic。
func outboundContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
}
