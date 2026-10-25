package prompt

import (
	"strings"
	"time"
)

// 本文件存放「代码生成」的提示词块。
// 对应 MaiBot 的 chat_loop_service.py 里那些 f-string 拼出来的块，
// 它们不是模板文件，因为内容依赖运行时状态或配置组合。

// PlannerFinalReminder 是每轮 Planner 请求末尾的 user 提醒。
// 原文见 MaiBot chat_loop_service.py 的 PLANNER_FINAL_USER_REMINDER。
const PlannerFinalReminder = "请你进行分析，是不调用工具，进行思考和等待\n还是使用工具搜集处理信息或使用reply回复"

// ReplyerOutputInstruction 是 replyer 的最终输出格式说明。
// 原文见 MaiBot maisaka_generator_base.py 的 _build_replyer_output_instruction（中文分支）。
const ReplyerOutputInstruction = "请注意不要输出多余内容(包括不必要的前后缀，冒号，括号，表情包，@等 )，只输出发言内容就好。"

// ReplyerReplyInstruction 是 Replyer 请求最后一条 user 消息里的「只输出正文」要求。
//
// 原文见 maisaka_generator_base.py:556 _build_reply_instruction。
//
// **绝不能拿 PlannerFinalReminder 顶替它**（早期版本就是这么干的）：
// 那句里的「不调用工具」「使用reply回复」是给 Planner 的决策指令，
// Replyer 侧没有工具概念，看到它最自然的解释就是「先回一句这个指令」，
// 于是产出「不调用工具，直接回。\n\n<正文>」这种带指令回声的回复。
// 实测同一份真实历史：用 PlannerFinalReminder 泄漏 5/10，用本句 0/10。
const ReplyerReplyInstruction = "请自然地回复。不要输出多余说明、括号、@ 或额外标记，只输出实际要发言的内容。"

// ReplyerStyleHint 把 reply 工具给的篇幅要求翻成本次请求的附加说明。
//
// 原文见 maisaka_generator_base.py:611 _build_requested_reply_style_message。
// 参考实现把它作为**最后一条独立 user 消息**发出去，而不是拼进 system：
// 拼进 system 的权重太弱，「简短表达」压不住模型铺开写的倾向。
// 返回空串表示不加这条消息（正常回复 / 未知风格都走这条路）。
func ReplyerStyleHint(style string) string {
	switch strings.TrimSpace(style) {
	case "简短表达":
		return "请简短的回复，允许句子残缺，奇怪表达，倒装，省略，符合口语习惯，符合省力随意回复习惯"
	case "长回复":
		return "可以针对问题做出较为详细的评论和说明"
	default:
		return ""
	}
}

// TargetMessageBlock 构造「本次回复目标」块。
//
// 原文见 maisaka_generator_base.py:162 _build_target_message_block 的非自身分支：
//
//	你想要回复的消息是 {sender} 发送的 msg_id为 {id} 的消息，你这次要回复的就是这条
//	目标消息，不要把其他历史消息当成当前回复对象。
//	- 发言内容：{content}
//
// 这一块同时解决两个问题：把「回哪条」钉死，以及给模型一个
// 「只输出正文」的强锚点 —— 它比任何格式说明都更能压住指令回声。
// content 按参考实现截断到 300 字。
func TargetMessageBlock(sender, msgID, content string) string {
	if s := strings.TrimSpace(msgID); s == "" {
		return ""
	}
	name := strings.TrimSpace(sender)
	if name == "" {
		name = "对方"
	}
	body := Truncate(content, 300)
	if body == "" {
		body = "[无可见文本内容]"
	}
	var b strings.Builder
	b.WriteString("你想要回复的消息是 ")
	b.WriteString(name)
	b.WriteString(" 发送的 msg_id为 ")
	b.WriteString(strings.TrimSpace(msgID))
	b.WriteString(" 的消息，你这次要回复的就是这条目标消息，不要把其他历史消息当成当前回复对象。\n")
	b.WriteString("- 发言内容：")
	b.WriteString(body)
	return b.String()
}

// SelfTargetMessageBlock 构造「回复自己发过的消息」块。
//
// 原文见 maisaka_generator_base.py:176 的 is_bot_self 分支：
// 要明确告诉模型「这是你自己说的」，否则它会把上一条自发言
// 当成群友说的话去回应（自我对话）。
func SelfTargetMessageBlock(botName, msgID, content string) string {
	name := strings.TrimSpace(botName)
	if name == "" {
		name = "你"
	}
	body := Truncate(content, 300)
	if body == "" {
		body = "[无可见文本内容]"
	}
	return "你想要补充说明你自己（" + name + "） 发送的 msg_id为 " + strings.TrimSpace(msgID) +
		" 的消息，你可以在这条目标消息的基础上补充发言，不要把你自己的发言当成别人的发言。\n" +
		"- 你之前的发言内容：" + body
}

// CurrentTimeLine 返回 Replyer 末尾块的「当前时间」行。
//
// 格式照抄参考实现（maisaka_generator_base.py:579 的 `%Y-%m-%d %H:%M:%S`）：
// 带星期是 Planner 侧的习惯，Replyer 这边与参考实现一致，不带。
func CurrentTimeLine(now time.Time) string {
	if now.IsZero() {
		now = time.Now()
	}
	return "当前时间：" + now.Format("2006-01-02 15:04:05")
}

// ReplyerFinalUserMessage 拼出 Replyer 最后一条 user 消息。
//
// 结构照抄 maisaka_generator_base.py:571 _build_final_user_message：
// 各段之间用空行分隔，顺序是
//
//	当前时间 → 目标消息 → 【额外发送内容参考】 → 只输出正文
//
// 附件段（attachmentBlock）可以为空，为空时那一格整段省略 ——
// 参考实现也是这么做的（`if attachment_prompt.strip()`）。
func ReplyerFinalUserMessage(now, targetBlock string, attachmentBlock ...string) string {
	sections := make([]string, 0, 4)
	if s := strings.TrimSpace(now); s != "" {
		sections = append(sections, s)
	}
	if s := strings.TrimSpace(targetBlock); s != "" {
		sections = append(sections, s)
	}
	if len(attachmentBlock) > 0 {
		if s := strings.TrimSpace(attachmentBlock[0]); s != "" {
			sections = append(sections, s)
		}
	}
	sections = append(sections, ReplyerReplyInstruction)
	return strings.Join(sections, "\n\n")
}

// JoinChineseItems 把多项按中文习惯连起来。
//
// 逐字照抄 maisaka_generator_base.py:266 _join_chinese_items：
// 一项原样；两项用「 和 」连接；三项以上用「、」连前几项、最后一项前用「 和 」。
// 不能简单用 strings.Join("、") —— 「图片A、图片B」 少了最后的「和」。
func JoinChineseItems(items []string) string {
	cleaned := make([]string, 0, len(items))
	for _, it := range items {
		if s := strings.TrimSpace(it); s != "" {
			cleaned = append(cleaned, s)
		}
	}
	switch len(cleaned) {
	case 0:
		return ""
	case 1:
		return cleaned[0]
	case 2:
		return strings.Join(cleaned, " 和 ")
	default:
		return strings.Join(cleaned[:len(cleaned)-1], "、") + " 和 " + cleaned[len(cleaned)-1]
	}
}

// Truncate 按 rune 截断字符串，超长时补省略号。
//
// 不能按 byte 切：中文会被劈成非法 UTF-8，整条 JSON 请求都发不出去。
func Truncate(s string, limit int) string {
	if limit <= 0 {
		return s
	}
	runes := []rune(strings.TrimSpace(s))
	if len(runes) <= limit {
		return string(runes)
	}
	return string(runes[:limit]) + "…"
}

// GroupChatAttentionBlock 构造「在该聊天中的注意事项」块。
// 原文见 MaiBot _build_group_chat_attention_block：
//
//	在该聊天中的注意事项：
//	通用注意事项：
//	<配置里的提示词>
func GroupChatAttentionBlock(isGroup bool, groupPrompt, privatePrompt string) string {
	var notice string
	if isGroup {
		notice = strings.TrimSpace(groupPrompt)
	} else {
		notice = strings.TrimSpace(privatePrompt)
	}
	if notice == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("在该聊天中的注意事项：\n")
	b.WriteString("通用注意事项：\n")
	b.WriteString(notice)
	b.WriteString("\n")
	return b.String()
}

// QueryMemoryRule 构造记忆检索规则块。
//
// 严格照抄 MaiBot _build_query_memory_rule 的群聊 / 私聊两套措辞：
// 群聊强调「不要把私聊或个人隐私记忆带到群聊里」，
// 私聊强调「对方提到之前/上次/还记得吗等信号时更积极检索」。
func QueryMemoryRule(isGroup bool) string {
	if isGroup {
		return "- query_memory()：只有回复明显依赖群内过去对话、共同经历、公开约定、任务进展或近期线索时使用；" +
			"不要为了寒暄、即时情绪回应、轻松接话、只看最近消息就能回答的内容而检索。" +
			"不要把私聊或个人隐私记忆带到群聊里。"
	}
	return "- query_memory()：当对方提到“之前”“上次”“最近”“还记得吗”“我喜欢”“我说过”等信号，" +
		"或回复依赖长期偏好、先前承诺、共同经历、人物长期信息时，可以更积极检索。"
}

// BotAliasNotice 构造紧跟在名字后面的别名说明。
// 原文见 MaiBot _build_bot_alias_notice：没有别名时返回空串。
func BotAliasNotice(aliases []string) string {
	cleaned := make([]string, 0, len(aliases))
	for _, a := range aliases {
		if s := strings.TrimSpace(a); s != "" {
			cleaned = append(cleaned, s)
		}
	}
	if len(cleaned) == 0 {
		return ""
	}
	return "，还有人叫你" + strings.Join(cleaned, "、")
}

// BehaviorStylePrompt 返回行为风格。
//
// MaiBot 在配置为空时不会补默认值，而是依赖配置里的默认值。
// 这里额外做一次兜底，避免空串导致提示词出现「你的行为风格是：」这样断掉。
func BehaviorStylePrompt(style string) string {
	if s := strings.TrimSpace(style); s != "" {
		return s
	}
	return "是一个在群里聊天的普通人，说话自然随意。"
}

// DefaultBotName 是参考实现里的默认机器人昵称，仅用作**防御性兜底**：
// 正常路径下 botName 由调用方从 config.Bot.Nickname 传入，永远不会走到这里。
//
// 它与 config.Default().Bot.Nickname 必须同值（由 config 包的测试守着）。
// 不直接把兜底写成调用方的名字，是因为这个函数是导出的，
// 默认值散落在两处比散落在一处更容易漂。
const DefaultBotName = "木木"

// IdentityPrompt 返回 replyer 的人格块。
//
// 严格照抄 MaiBot maisaka_generator_base.py:101 _build_personality_prompt：
//
//	f"你的名字是{bot_name}{bot_aliases}。\n{prompt_personality}"
//
// 早期版本只返回 personality，Replyer 因此不知道自己叫什么、
// 有哪些别名 —— 群友喊「肥鱼」时它不认这是自己。
func IdentityPrompt(botName string, aliases []string, personality string) string {
	name := strings.TrimSpace(botName)
	if name == "" {
		name = DefaultBotName
	}
	p := strings.TrimSpace(personality)
	if p == "" {
		p = "是人类。"
	}
	// 注意：这里的别名措辞与 Planner 侧的 BotAliasNotice 不同，不是笔误。
	// 参考实现里本就是两套：planner_messages._build_bot_alias_notice 是
	// 「，还有人叫你A、B」（顿号），而 replyer 的 _build_personality_prompt 是
	// 「，也有人叫你A,B」（逗号）。逐个对齐，别统一。
	cleaned := make([]string, 0, len(aliases))
	for _, a := range aliases {
		if s := strings.TrimSpace(a); s != "" {
			cleaned = append(cleaned, s)
		}
	}
	var aliasNotice string
	if len(cleaned) > 0 {
		aliasNotice = "，也有人叫你" + strings.Join(cleaned, ",")
	}
	return "你的名字是" + name + aliasNotice + "。\n" + p
}
