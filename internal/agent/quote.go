package agent

import (
	"strings"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/chat"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
)

// 引用计数与「回复谁」的解析。
//
// 对应 refs/MaiBot/src/maisaka/runtime.py：
//   - _quote_counter.record(...)   入站与出站消息都记
//   - get_reply_quote_context(...) 发送前取快照
//   - find_source_message_by_id    按 msg_id 反查原始消息

// recordInboundQuote 记录一条入站消息到引用计数器。
func (r *Runtime) recordInboundQuote(msg *domain.Message) {
	if r.quoteCounter == nil {
		return
	}
	recordQuote(counterOf(r), msg)
}

// counterOf 只是让上面的空值检查能复用下面的纯函数。
func counterOf(r *Runtime) *chat.QuoteCounter { return r.quoteCounter }

// recordQuote 把一条消息记进计数器的纯函数版本。
//
// 抽出来是为了让「重启恢复」这段逻辑可以脱离 Runtime 实例单独测试：
// 恢复时最容易漏的就是「喂计数器」这一步，而漏了的表现
// 不是报错，是引用行为悄悄变了。
func recordQuote(counter *chat.QuoteCounter, msg *domain.Message) {
	if counter == nil || msg == nil {
		return
	}
	at := msg.ReceivedAt.UnixNano()
	if msg.ReceivedAt.IsZero() {
		at = time.Now().UnixNano()
	}
	counter.Record(msg.MessageID, msg.UserID, at)
}

// restoringQuoteCounter 是 restoreRecentContext 用的包装，便于测试。
func restoringQuoteCounter(counter *chat.QuoteCounter, msgs []*domain.Message) {
	for _, m := range msgs {
		recordQuote(counter, m)
	}
}

// recordOutboundQuote 记录一条刚发出的消息。
//
// 出站消息没有 received_at，用发送时刻；发送者记成机器人自己，
// 这样「上一句是不是我说的」这个变量才算得对（它直接决定基础概率档位）。
func (r *Runtime) recordOutboundQuote(messageIDs []string) {
	if r.quoteCounter == nil || len(messageIDs) == 0 {
		return
	}
	now := time.Now().UnixNano()
	for _, id := range messageIDs {
		if id == "" {
			continue
		}
		r.quoteCounter.Record(id, r.accountID, now)
	}
}

// findMessageByID 在当前上下文里按 msg_id 反查消息。
//
// 只查 history：pending 里的消息尚未进入历史，但它们在注入 history 之后
// 也会被 onMessage 追加，所以这里一并查一遍更稳妥。
func (r *Runtime) findMessageByID(messageID string) *domain.Message {
	id := strings.TrimSpace(messageID)
	if id == "" {
		return nil
	}
	for i := len(r.history) - 1; i >= 0; i-- {
		if r.history[i].MessageID == id {
			return r.history[i]
		}
	}
	for i := len(r.pending) - 1; i >= 0; i-- {
		if r.pending[i].MessageID == id {
			return r.pending[i]
		}
	}
	// 也要能查到机器人自己发过的消息。
	//
	// 漏了这一段的表现很具体：模型说「引用我刚才那句」时
	// findMessageByID 返回 nil，引用被静默跳过 ——
	// 日志里只有一句 debug，看起来像「模型没说要引用」。
	for i := len(r.botReplies) - 1; i >= 0; i-- {
		reply := r.botReplies[i]
		if reply.MessageID == id {
			return botReplyAsMessage(reply)
		}
		if reply.RefMsgIdx != "" && reply.RefMsgIdx == id {
			return botReplyAsMessage(reply)
		}
	}
	return nil
}

// quoteFrequency 返回当前会话生效的引用频率档位。
//
// 群聊与私聊各有一套配置，与参考实现一致（reply.py:_resolve_reply_quote）。
func (r *Runtime) quoteFrequency() chat.QuoteFrequency {
	raw := r.cfg.Get().Chat.Style.PrivateQuoteFrequency
	if r.isGroup {
		raw = r.cfg.Get().Chat.Style.GroupQuoteFrequency
	}
	freq, ok := chat.QuoteFrequencyOf(strings.TrimSpace(raw))
	if !ok {
		// 配置写错时按 never 处理：宁可少引用，也不要随机乱引用。
		r.log.Warn("引用频率配置无效，按 never 处理", "value", raw, "is_group", r.isGroup)
		return chat.QuoteNever
	}
	return freq
}

// decideQuote 决定这次回复是否引用目标消息。
//
// 返回的 Reference 是出站 message_reference 的目标（REFIDX_ 形态），
// 零值表示不引用。
//
// 注意力所在：返回值**不能**是 Message.MessageID。
// 那个值是 ROBOT1.0_ 形态的消息 ID，而 message_reference 需要的是
// REFIDX_ 形态的引用索引 —— 两者属于不同键空间。
// 平台实测两种都接受，所以传错了不报错，只是客户端引用块
// 永远无法被后续消息正确回引。
func (r *Runtime) decideQuote(targetMessageID, text string) domain.Reference {
	freq := r.quoteFrequency()
	if freq == chat.QuoteNever {
		return domain.Reference{}
	}
	if r.quoteCounter == nil {
		return domain.Reference{}
	}

	target := r.findMessageByID(targetMessageID)
	if target == nil {
		// 找不到目标消息时不能虚构距离，按参考实现直接引用最新消息。
		// 但这时拿不到它的引用索引，只能放弃 —— 拿 MessageID 充数
		// 会让客户端显示一个指向错消息的引用块。
		return domain.Reference{}
	}

	quote := domain.ReferenceOf(target)
	if quote.IsZero() {
		// 目标没有引用索引：可能是改造前入库的旧消息，
		// 也可能是平台没下发 msg_idx。两种情况都不能挂引用。
		r.log.Debug("目标消息没有引用索引，跳过引用", "session", r.sessionID, "msg_id", target.MessageID)
		return domain.Reference{}
	}

	snapshot, ok := r.quoteCounter.Snapshot(target.MessageID, time.Now().UnixNano())
	if !ok {
		// 目标已离开窗口：直接引用。
		return quote
	}

	should, prob, err := chat.DecideQuote(&snapshot, r.accountID, text, freq, r.isGroup)
	if err != nil {
		r.log.Warn("引用判定失败，跳过引用", "err", err)
		return domain.Reference{}
	}
	r.log.Debug("引用判定",
		"session", r.sessionID,
		"frequency", freq,
		"probability", prob,
		"should_quote", should,
		"distance", *snapshot.TargetDistance,
	)
	if !should {
		return domain.Reference{}
	}
	return quote
}
