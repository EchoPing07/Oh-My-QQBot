// Package agent 是 Agent 核心。
//
// 对应 MaiBot 的 src/maisaka：
//   - runtime.py          会话级状态机
//   - reasoning_engine.py Planner 多轮循环
//   - chat_loop_service.py 请求组装与提示词块
//   - maisaka_generator_base.py Replyer 提示词组装
//
// 设计上严格照抄三层结构：
//
//	Runtime 持有状态 → Loop 驱动多轮工具调用 → Prompt 负责组装请求
package agent

import (
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/chat"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
)

// MessagePrefix 构造 Planner 使用的消息前缀。
//
// 严格照抄 MaiBot 的 build_planner_prefix（context/planner_messages.py:17-76）：
//
//	message_attrs = [msg_id, quote, time, user, group_card, is_self_message]
//	return f"<message {' '.join(message_attrs)}>\n"
//
// 注意四点，都是原版的既定行为，不要「优化」：
//   - 以 ">\n" 结尾，没有闭合标签
//   - 属性顺序固定：msg_id → quote → time → user → group_card
//   - group_card 是「用户的群名片」，不是群名
//   - quote 是**属性**，不是正文。引用关系放在元信息里，
//     被引用正文另外拼在正文开头（见 RenderMessage）
func MessagePrefix(msg *domain.Message) string {
	attrs := make([]string, 0, 5)

	if msg.MessageID != "" {
		attrs = append(attrs, fmt.Sprintf(`msg_id="%s"`, html.EscapeString(msg.MessageID)))
	}

	// quote 属性：本条消息引用了哪条（可能多条，逗号分隔）。
	// 与参考实现 insert 在 time 之前（planner_messages.py:58-60）。
	if quoteIDs := QuoteIDs(msg); len(quoteIDs) > 0 {
		attrs = append(attrs, fmt.Sprintf(`quote="%s"`, html.EscapeString(strings.Join(quoteIDs, ","))))
	}

	attrs = append(attrs, fmt.Sprintf(`time="%s"`, msg.ReceivedAt.Format("15:04:05")))

	userName := msg.UserName
	if userName == "" {
		userName = msg.UserID
	}
	attrs = append(attrs, fmt.Sprintf(`user="%s"`, html.EscapeString(userName)))

	// group_card 对应原版的 user_info.user_cardname —— 群名片。
	if card := strings.TrimSpace(msg.UserCardName); card != "" {
		attrs = append(attrs, fmt.Sprintf(`group_card="%s"`, html.EscapeString(card)))
	}

	return fmt.Sprintf("<message %s>\n", strings.Join(attrs, " "))
}

// RenderMessage 把一条消息渲染成 Planner 看到的 user 内容。
//
// 引用关系分两部分进上下文，两部分都不能少：
//
//	<message msg_id="…" quote="REFIDX_目标">
//	[回复了老板的消息: 今晚加班]
//	好耶
//
// 前者来自 `quote` 属性（告诉模型在引用哪条），
// 后者来自被引用正文（告诉模型被引的内容是什么）。
//
// 为什么两部分都要：参考实现的 Planner 只看得到 `quote` 属性，
// 被引用**正文**要等 Replyer 阶段才由 _build_target_message_block 提供
// （maisaka_generator_base.py:161-205）。我们的 Planner 没有那个目标消息块，
// 只给属性的话模型会看到一个够不着的 ID —— 看得见引用关系，看不见引用了什么。
func RenderMessage(msg *domain.Message) string {
	return MessagePrefix(msg) + quotedBodyPrefix(msg) + msg.PlainText
}

// quoteBodyPrefix 把被引用正文渲染成正文前缀。
//
// 文案照抄 MaiBot 的 process_reply_component
// （chat/message_receive/message.py:428-458）：
//
//	[回复了{发送者}的消息: {正文}]
//	[回复了一条消息，但原消息已无法访问]
func quotedBodyPrefix(msg *domain.Message) string {
	if msg == nil || len(QuoteIDs(msg)) == 0 {
		return ""
	}
	text := strings.TrimSpace(msg.Additional[domain.AdditionalQuotedText])
	if text == "" {
		return "[回复了一条消息，但原消息已无法访问]\n"
	}
	// 发送者名查不到时省略「X的」，保留「回复了消息」，
	// 因为不知道是谁说的也不影响理解内容。
	if sender := strings.TrimSpace(msg.Additional[domain.AdditionalQuotedSender]); sender != "" {
		return "[回复了" + sender + "的消息: " + text + "]\n"
	}
	return "[回复了消息: " + text + "]\n"
}

// QuoteIDs 返回一条消息引用的目标 ID 列表（去重、保序）。
//
// 对应参考实现的 extract_quote_ids_from_message_sequence（planner_messages.py:94）。
// 引用段本身不进正文（domain.BuildPlainText 已忽略它），只能从段里反查。
func QuoteIDs(msg *domain.Message) []string {
	if msg == nil {
		return nil
	}
	var ids []string
	seen := make(map[string]struct{}, 1)
	for _, seg := range msg.Segments {
		if seg.Type != domain.SegmentReply {
			continue
		}
		id := strings.TrimSpace(seg.MessageID)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}

// RenderMessageAt 渲染一条消息，并按它在上下文窗口里的位置截断正文。
//
// 对应 MaiBot 的 build_readable_message(truncate_message=..., truncate_func=...)
// 与 _default_truncate_func：越早的消息保留得越少，因为读者对它们本来就更模糊。
// 分档数值照抄参考实现（common/utils/utils_message.py:566）。
//
// percentile 是"这条消息在窗口里的相对位置"，取值 [0,1)。
func RenderMessageAt(msg *domain.Message, percentile float64) string {
	limit, replacement := truncateLimitFor(percentile)
	return MessagePrefix(msg) + chat.TruncateMessageWith(msg.PlainText, limit, replacement)
}

// truncateLimitFor 返回截断长度与替换文案。
// 逐档照抄 _default_truncate_func，不要"优化"成连续的线性插值。
func truncateLimitFor(percentile float64) (int, string) {
	switch {
	case percentile < 0.3:
		return 400, "......（内容太长了）"
	case percentile < 0.5:
		return 200, "......（内容太长了）"
	case percentile < 0.8:
		return 100, "......（有点记不清了）"
	default:
		return 50, "......（记不清了）"
	}
}

// RenderBotMessage 把机器人自己发过的话渲染成同格式，标注 is_self_message。
//
// 让 Planner 看到自己说过什么很重要 —— 否则它会重复自己。
// 格式与 MessagePrefix 一致，同样不带闭合标签。
func RenderBotMessage(text string, at time.Time, botName string) string {
	attrs := []string{
		fmt.Sprintf(`time="%s"`, at.Format("15:04:05")),
		fmt.Sprintf(`user="%s"`, html.EscapeString(botName)),
		`is_self_message="true"`,
	}
	return fmt.Sprintf("<message %s>\n%s", strings.Join(attrs, " "), text)
}

// RenderMessages 把一批消息渲染成一条拼接文本。
//
// 说明：MaiBot 的做法是每条消息一个独立的 UserMessageItem（见 reasoning_engine._ingest_messages），
// 而不是拼成一条。所以 Planner 请求的构造走 RenderMessage 逐条生成，
// 这个拼接版本只用于日志与调试展示。
func RenderMessages(msgs []*domain.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(RenderMessage(m))
	}
	return b.String()
}

// TimeUserMessage 构造时间提示。
// 对应 MaiBot 的 _build_current_time_user_message。
func TimeUserMessage(t time.Time) string {
	return fmt.Sprintf("当前时间：%s", t.Format("2006-01-02 15:04:05 Monday"))
}

// ExtractMsgID 从 Planner 传入的 msg_id 参数里取真实 ID。
//
// Planner 可能传：
//   - 完整的平台消息 ID
//   - 空串（表示回复最新消息）
func resolveMsgID(raw string, fallback string) string {
	if s := strings.TrimSpace(raw); s != "" {
		return s
	}
	return fallback
}
