package agent

import (
	"encoding/base64"
	"strconv"
	"strings"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/imgtx"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/llm"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/logx"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/prompt"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/tool"
)

// PromptBuilder 负责组装 Planner 与 Replyer 请求。
// 对应 MaiBot 的 chat_loop_service.py 与 maisaka_generator_base.py。
type PromptBuilder struct {
	cfg     *config.Store
	prompts *prompt.Manager
}

// NewPromptBuilder 创建提示词构建器。
func NewPromptBuilder(cfg *config.Store, prompts *prompt.Manager) *PromptBuilder {
	return &PromptBuilder{cfg: cfg, prompts: prompts}
}

// BuildSystemPrompt 组装 Planner 的 system prompt。
//
// 模板占位符与 MaiBot maisaka_chat.prompt 对齐。
//
// 关键点：只描述「真的注册过的工具」。
// 早期版本照抄了原版的 query_memory 说明，但我们没实现长期记忆，
// 结果模型调用一个不存在的工具、失败、然后 wait —— 白白浪费一轮。
// 现在按实际工具集生成说明。
func (b *PromptBuilder) BuildSystemPrompt(isGroup bool, av tool.Availability, registry *tool.Registry) (string, error) {
	values := map[string]string{
		"bot_name":            b.cfg.Get().Bot.Nickname,
		"bot_alias_notice":    prompt.BotAliasNotice(b.cfg.Get().Bot.AliasNames),
		"behavior_style":      prompt.BehaviorStylePrompt(b.cfg.Get().Personality.BehaviorStyle),
		"bot_nickname_notice": "",
		"group_chat_attention_block": prompt.GroupChatAttentionBlock(
			isGroup,
			b.cfg.Get().Chat.Style.GroupChatPrompt,
			b.cfg.Get().Chat.Style.PrivateChatPrompt,
		),
		// 只有注册了 query_memory 才写这条规则。
		"query_memory_rule": memoryRuleFor(registry, av, isGroup),
		"subagent_rule":     "", // 无子代理
		// 表情包提示：按 send_probability 概率出现。
		// 概率为 0 时永远不出现 —— 让「不想让它发表情包」成为一个可用配置。
		"emoji_hint_rule": b.emojiHintRule(registry, av),
	}

	return b.prompts.RenderNamed("planner", values)
}

// emojiHintRule 按概率决定是否提示「可以发表情包」。
//
// 为什么用概率而不是每次都提示：
// 每次都提示会让模型过度使用表情包，聊天里满屏表情反而假。
// 参考实现把这件事交给 VLM 选图子代理，我们没有子代理，
// 退化成一个概率开关 —— 简单，但足够让表情包自然地出现。
func (b *PromptBuilder) emojiHintRule(registry *tool.Registry, av tool.Availability) string {
	if registry == nil {
		return ""
	}
	t, ok := registry.Get("send_emoji")
	if !ok || !t.Visible(av) {
		return ""
	}

	p := b.cfg.Get().Emoji.SendProbability
	if p <= 0 {
		return ""
	}
	if p < 1 && randFloat() >= p {
		return ""
	}
	return "- send_emoji()：如果当前情绪适合用表情包表达（比如被逗笑、无语、想撒娇），可以发一个；不要每次都用。"
}

// memoryRuleFor 按实际工具集决定是否输出记忆检索规则。
func memoryRuleFor(registry *tool.Registry, av tool.Availability, isGroup bool) string {
	if registry == nil {
		return ""
	}
	t, ok := registry.Get("query_memory")
	if !ok || !t.Visible(av) {
		return ""
	}
	return prompt.QueryMemoryRule(isGroup)
}

// BuildReplyerSystemPrompt 组装 Replyer 的 system prompt。
//
// 模板占位符与 MaiBot maisaka_replyer.prompt 对齐：
//
//	identity / reply_style / group_chat_attention_block / replyer_output_instruction
//
// 注意参数是**基础表达风格**（配置里的 personality.reply_style），
// 不是本次的篇幅要求。篇幅要求走 Replyer 最后一条独立 user 消息
// （prompt.ReplyerStyleHint），与参考实现一致：拼进 system 的权重太弱。
//
// replyStyle 传空串时回退到配置值，方便调用方不关心配置。
func (b *PromptBuilder) BuildReplyerSystemPrompt(isGroup bool, replyStyle string) (string, error) {
	style := strings.TrimSpace(replyStyle)
	if style == "" {
		style = strings.TrimSpace(b.cfg.Get().Personality.ReplyStyle)
	}

	values := map[string]string{
		"identity": prompt.IdentityPrompt(
			b.cfg.Get().Bot.Nickname,
			b.cfg.Get().Bot.AliasNames,
			b.cfg.Get().Personality.Personality,
		),
		"reply_style":                b.selectReplyStyle(style),
		"group_chat_attention_block": prompt.GroupChatAttentionBlock(isGroup, b.cfg.Get().Chat.Style.GroupChatPrompt, b.cfg.Get().Chat.Style.PrivateChatPrompt),
		"replyer_output_instruction": prompt.ReplyerOutputInstruction,
	}

	return b.prompts.RenderNamed("replyer", values)
}

// selectReplyStyle 按概率决定是否启用备用表达风格。
// 对应 MaiBot 的 _select_reply_style。
func (b *PromptBuilder) selectReplyStyle(base string) string {
	p := b.cfg.Get().Personality.MultipleProbability
	if p <= 0 || len(b.cfg.Get().Personality.MultipleReplyStyle) == 0 {
		return base
	}
	if randFloat() >= p {
		return base
	}
	styles := b.cfg.Get().Personality.MultipleReplyStyle
	return styles[int(randFloat()*float64(len(styles)))%len(styles)]
}

// BuildPlannerMessages 组装完整的 Planner 请求消息列表。
//
// 结构与 MaiBot 对齐：
//
//	system:            planner 模板渲染结果
//	user × N:          每条聊天消息一条独立 user 消息（含机器人自己说过的话）
//	user:              人物画像内部参考（可选，只在本轮有人物候选时出现）
//	user:              当前时间
//	user:              PLANNER_FINAL_USER_REMINDER
//
// 关键：聊天记录是「一条消息一个 user 消息」，不是拼成一大段。
// 参考实现见 reasoning_engine._ingest_messages —— 它遍历消息逐条建 UserMessageItem。
// 逐条的好处是每条自带 msg_id 边界，模型能准确引用要回复的那条。
func (b *PromptBuilder) BuildPlannerMessages(
	isGroup bool,
	history []*domain.Message,
	botReplies []BotReply,
	now time.Time,
	av tool.Availability,
	registry *tool.Registry,
) ([]llm.Message, error) {
	return b.BuildPlannerMessagesWithProfiles(isGroup, history, botReplies, now, av, registry, "")
}

// BuildPlannerMessagesWithProfiles 是 BuildPlannerMessages 的带画像版本。
//
// profiles 是已经拼好的【人物画像-内部参考】文本，空串表示不注入。
// 单独开一个方法而不是改原有签名，是因为工具、测试与 WebUI 都在调旧签名，
// 而画像只有 runtime 那条链路有数据。
func (b *PromptBuilder) BuildPlannerMessagesWithProfiles(
	isGroup bool,
	history []*domain.Message,
	botReplies []BotReply,
	now time.Time,
	av tool.Availability,
	registry *tool.Registry,
	profiles string,
) ([]llm.Message, error) {
	systemPrompt, err := b.BuildSystemPrompt(isGroup, av, registry)
	if err != nil {
		return nil, err
	}

	out := []llm.Message{
		{Role: llm.RoleSystem, Content: systemPrompt},
	}

	// 把历史消息与机器人自己的发言按时间排序后，逐条转成 user 消息。
	// Planner 侧按位置截断老消息，Replyer 侧保留完整正文。
	visual := b.cfg.Get().ModelIsVisual(config.TaskPlanner)
	items := mergeHistoryItemsTruncated(history, botReplies, b.cfg.Get().Bot.Nickname, true)
	for i, item := range items {
		msg := llm.Message{
			Role:    llm.RoleUser,
			Content: item.text,
		}
		// 多模态：只有 Planner 模型声明了 visual 才挂图片。
		// 往纯文本模型塞 image_url 会被服务端直接拒掉（400），
		// 表现是整轮思考失败，比看不到图严重得多。
		if visual {
			msg.Images = imagesOf(items[i].source)
		}
		out = append(out, msg)
	}

	// 人物画像：放在历史之后、时间之前。
	//
	// 位置很有讲究：放在最前面会让模型把画像当成「对所有人的描述」，
	// 放在最后又会被 PLANNER_FINAL_USER_REMINDER 盖过。
	// 夹在历史与当前时间之间最接近参考实现的效果 ——
	// 读完聊天后才知道本次涉及谁，然后读到这些人的背景。
	if strings.TrimSpace(profiles) != "" {
		out = append(out, llm.Message{
			Role:    llm.RoleUser,
			Content: profiles,
		})
	}

	// 当前时间：放在历史之后，让模型知道「现在」。
	out = append(out, llm.Message{
		Role:    llm.RoleUser,
		Content: TimeUserMessage(now),
	})

	// 每轮末尾的 user 提醒。
	out = append(out, llm.Message{
		Role:    llm.RoleUser,
		Content: prompt.PlannerFinalReminder,
	})

	return out, nil
}

// BotReply 是机器人自己发过的一条消息，用于重建历史。
type BotReply struct {
	Text string
	At   time.Time

	// MessageID 是这条出站消息的平台 ID（QQ 的 ROBOT1.0_ 形态）。
	MessageID string
	// RefMsgIdx 是这条出站消息自己的引用索引（QQ 的 REFIDX_ 形态）。
	//
	// 为什么必须存：群里有人说「你刚才那句」时，模型要引用的是
	// **木木自己发过的那条**；message_reference 只接受 REFIDX_ 形态，
	// 而这个值只有发送响应 ext_info.ref_idx 才有 —— 入站事件里没有。
	// 不存的话「引用自己的发言」永远落空。
	RefMsgIdx string
}

// mergedItem 是合并后的历史项。
type mergedItem struct {
	at   time.Time
	text string
	// body 是机器人自己那条发言的纯正文（不带 <message> 包裹）。
	// Replyer 侧要把机器人的话渲染成 assistant 角色，那条消息的内容
	// 只能是一句人话，不能带 XML 头（参考实现见下面 BuildReplyerMessages）。
	body string
	// isSelf 表示这条是机器人自己发过的。
	isSelf bool
	// source 指向原始消息，机器人自己发的发言为 nil。
	// 多模态挂图需要它。
	source *domain.Message
}

// mergeHistoryItems 把外部消息与机器人发言按时间排序，返回逐条渲染结果。
func mergeHistoryItems(history []*domain.Message, botReplies []BotReply, botName string) []mergedItem {
	return mergeHistoryItemsTruncated(history, botReplies, botName, false)
}

// mergeHistoryItemsTruncated 与 mergeHistoryItems 相同，但可以按位置截断。
//
// 为什么需要截断：Planner 的上下文越长，越早的消息越容易被"读到一半就不看了"，
// 参考实现的做法就是越老的消息保留越少（见 render.go:truncateLimitFor），
// 既省 token 又贴合人的记忆衰减。
// Replyer 不截断 —— 它要靠完整历史来措辞，而且它的请求本来就短。
func mergeHistoryItemsTruncated(history []*domain.Message, botReplies []BotReply, botName string, truncate bool) []mergedItem {
	items := make([]mergedItem, 0, len(history)+len(botReplies))

	// 分母只算 history，不含 botReplies。
	//
	// 这与参考实现一致：utils_message.py:480 的 `msg_count = len(msg_list)`
	// 里 msg_list 只有外部消息，机器人自己的发言不在其中
	// （它是单独拼进上下文的）。用 history+botReplies 当分母会让
	// 「老消息」的分档整体往「新」的方向偏一格 —— 机器人话多的时候尤其明显。
	total := len(history)
	for i, m := range history {
		text := RenderMessage(m)
		if truncate && total > 0 {
			text = RenderMessageAt(m, float64(i)/float64(total))
		}
		items = append(items, mergedItem{at: m.ReceivedAt, text: text, source: m})
	}
	for _, r := range botReplies {
		items = append(items, mergedItem{
			at:     r.At,
			text:   RenderBotMessage(r.Text, r.At, botName),
			body:   r.Text,
			isSelf: true,
			// 让 mergedItem 也能带回自己的引用索引：
			// 模型要引用木木自己说过的话时需要它。
			source: botReplyAsMessage(r),
		})
	}

	// 按时间升序，稳定排序。
	stableSortItems(items)
	return items
}

// stableSortItems 是插入排序，历史条数很少。
func stableSortItems(items []mergedItem) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].at.Before(items[j-1].at); j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}

// ReplyerPromptInput 是组装 Replyer 请求的全部输入。
//
// 用结构体而不是一串位置参数：字段已经到七个，再加一个
// （就是本轮新增的 TargetMsgID）就会变成「第5个参数到底是什么」的猜谜游戏。
type ReplyerPromptInput struct {
	IsGroup    bool
	History    []*domain.Message
	BotReplies []BotReply
	// Reference 是 Planner 给的回复方向指引（reply_reference）。
	Reference string
	// Style 是本次的篇幅要求：简短表达 / 正常回复 / 长回复。
	// 注意与 BaseStyle 分开：BaseStyle 进 system 的 {reply_style}，
	// Style 进末尾的独立 user 消息。
	Style string
	// BaseStyle 是配置里的基础表达风格（personality.reply_style）。
	// 空串时由 BuildReplyerSystemPrompt 回退到配置。
	BaseStyle string
	Now       time.Time
	// TargetMsgID 是本次要回复的那条消息的 msg_id。
	// 用来在历史里反查出「回复目标块」（参考实现 _build_target_message_block）。
	TargetMsgID string
	// ExpressionHabits 是学到的表达习惯，作为一条独立 user 消息插在历史之后。
	// 对应参考实现的 expression_habits 参数。
	ExpressionHabits string
	// KeywordReaction 是命中的关键词反应提示，插在回复参考之后、末尾块之前。
	// 对应参考实现 _build_keyword_reaction_prompt。
	KeywordReaction string
	// AttachPics / AttachEmoji 是本次回复要额外发出的图片与表情包。
	//
	// 它们不进正文，但必须告诉 Replyer「系统会另外发这些东西」，
	// 否则它会在正文里写「（图）」「[表情]」这类占位，
	// 或者干脆把内容复述一遍 —— 因为模型不知道那些东西会真的被发出去。
	// 对应参考实现 maisaka_generator_base.py:351 _build_reply_attachment_prompt。
	AttachPics  []tool.AttachPic
	AttachEmoji string
}

// BuildReplyerMessages 组装 Replyer 请求。
//
// Replyer 拿到的东西比 Planner 少得多。结构严格对齐参考实现
// maisaka_generator_base.py:668 _build_request_messages：
//
//	system:  身份 + 表达风格 + 群聊注意事项 + 输出格式说明
//	user×N:  聊天记录（机器人自己的发言是 assistant 角色）
//	user:    表达习惯（可选）
//	user:    【回复信息参考】+ reply_reference（可选）
//	user:    关键词反应（可选）
//	user:    当前时间 + 目标消息块 + 「只输出正文」（固定在末尾）
//	user:    篇幅要求（可选，只在这种情况下才多一条）
//
// 补充材料的插入位置写在这段流程里，而不是由调用方 insertBeforeLast 再插：
// 早期版本的尾巴只有末尾块，插到它前面看着没问题；现在末尾块后面
// 还跟着篇幅要求，再「插到倒数第一条之前」就会把补充材料塞到
// 末尾块之后 —— 而那格是模型最看重的位置，不能被补充材料占据。
//
// 两处与早期版本不同，都是修 bug 的结果：
//
//  1. 末尾不再用 prompt.PlannerFinalReminder。那句是给 Planner 的决策指令，
//     里面带「不调用工具」「使用reply回复」，Replyer 会把它当成要回应的指令，
//     于是产出「不调用工具，直接回。\n\n<正文>」。实测泄漏 5/10 → 0/10。
//
//  2. 机器人自己的发言改用 assistant 角色。参考实现见
//     maisaka_generator_base.py:638-644（guided_reply → RoleType.Assistant）。
//     以前全部当成 user，模型会把它们当成「别人/自己刚说过的话」照着模仿，
//     这是指令回声自我强化的根因之一。
//
// 刻意保留的差异：参考实现把 reply_reference 原样作为一条 user 消息，
// 我们多加了「【回复信息参考】」标题。保留是因为 replyer 模板本身就写着
// 「你可以参考【回复信息参考】中的信息」—— 去掉标题会让这句指向空气；
// 而且有标题时模型不会把这段指引当成一条待回复的聊天消息。
func (b *PromptBuilder) BuildReplyerMessages(in ReplyerPromptInput) ([]llm.Message, int, error) {
	systemPrompt, err := b.BuildReplyerSystemPrompt(in.IsGroup, in.BaseStyle)
	if err != nil {
		return nil, 0, err
	}

	out := []llm.Message{
		{Role: llm.RoleSystem, Content: systemPrompt},
	}

	// 与 Planner 一致：聊天记录按条拆开，不拼成一大段。
	// 区别只在角色：机器人的发言走 assistant。
	items := mergeHistoryItems(in.History, in.BotReplies, b.cfg.Get().Bot.Nickname)
	for _, item := range items {
		if item.isSelf {
			if s := strings.TrimSpace(item.body); s != "" {
				out = append(out, llm.Message{Role: llm.RoleAssistant, Content: s})
			}
			continue
		}
		out = append(out, llm.Message{
			Role:    llm.RoleUser,
			Content: item.text,
		})
	}

	// 表达习惯：怎么说话。放在历史之后、回复指引之前。
	if habits := strings.TrimSpace(in.ExpressionHabits); habits != "" {
		out = append(out, llm.Message{Role: llm.RoleUser, Content: habits})
	}

	// 回复方向指引（说什么）。
	if reference := strings.TrimSpace(in.Reference); reference != "" {
		out = append(out, llm.Message{
			Role:    llm.RoleUser,
			Content: "【回复信息参考】\n" + reference,
		})
	}

	// 关键词反应：这条消息必须照做的硬要求。
	// 放在参考之后、末尾块之前 —— 先看完「想说什么」，再读「必须怎么回」，
	// 最后才是格式约束。
	if reaction := strings.TrimSpace(in.KeywordReaction); reaction != "" {
		out = append(out, llm.Message{Role: llm.RoleUser, Content: reaction})
	}

	// 末尾块：当前时间 + 目标消息 + 附件说明 + 「只输出正文」。
	//
	// 附件说明放在末尾块里（而不是单独一条）是因为参考实现就是
	// `sections.append("【额外发送内容参考】\n" + attachment_prompt)`
	// 再 append 输出指令，属于同一条 user 消息（maisaka_generator_base.py:598）。
	out = append(out, llm.Message{
		Role: llm.RoleUser,
		Content: prompt.ReplyerFinalUserMessage(
			prompt.CurrentTimeLine(in.Now),
			b.targetMessageBlock(in.History, in.TargetMsgID),
			b.attachmentBlock(in.AttachPics, in.AttachEmoji),
		),
	})

	// 篇幅要求单独一条，放在最后。
	// 参考实现也是这个位置（maisaka_generator_base.py:734）：
	// 放在 system 里的权重太弱，「简短表达」压不住模型铺开写。
	if hint := prompt.ReplyerStyleHint(in.Style); hint != "" {
		out = append(out, llm.Message{Role: llm.RoleUser, Content: hint})
	}

	return out, len(items), nil
}

// targetMessageBlock 在历史里按 msg_id 反查出「本次回复目标」块。
//
// 查不到就返回空串（参考实现同理：拿不到消息就不追加这一段），
// 绝不能编一个空的目标块 —— 那会让模型以为要去回复一条空消息。
func (b *PromptBuilder) targetMessageBlock(history []*domain.Message, msgID string) string {
	id := strings.TrimSpace(msgID)
	if id == "" {
		return ""
	}
	// 倒序：同一个 msg_id 极少重复，倒序能更快命中，而且真的重复时
	// 应该命中「最新那次出现」的同一条。
	for i := len(history) - 1; i >= 0; i-- {
		m := history[i]
		if m == nil || strings.TrimSpace(m.MessageID) != id {
			continue
		}
		// 回复自己的消息：参考实现单开一个分支
		// （maisaka_generator_base.py:176 is_bot_self），
		// 明确告诉模型「这是你自己说的，别当成别人的话」。
		if m.IsSelf {
			return prompt.SelfTargetMessageBlock(
				b.cfg.Get().Bot.Nickname, id, m.PlainText,
			)
		}
		name := strings.TrimSpace(m.UserName)
		if name == "" {
			name = m.UserID
		}
		return prompt.TargetMessageBlock(name, id, m.PlainText)
	}
	return ""
}

// attachmentBlock 拼出「系统会另外发这些东西」的说明。
//
// 对应参考实现 maisaka_generator_base.py:351 _build_reply_attachment_prompt，
// 但图片的描述比它简单：我们没有 media_index / tool 返回媒体的概念，
// 图片只有一个来源（上下文里的某条消息第几张），所以直接写名字与序号。
//
// 文案本身放在 reply_attachments.prompt 里（可在 WebUI 改），
// 只有 {attachments} 一个占位符。
func (b *PromptBuilder) attachmentBlock(pics []tool.AttachPic, emoji string) string {
	lines := make([]string, 0, 2)
	if len(pics) > 0 {
		targets := make([]string, 0, len(pics))
		for _, p := range pics {
			idx := p.Index
			if idx < 0 {
				idx = 0
			}
			targets = append(targets, "msg_id="+strings.TrimSpace(p.MsgID)+
				" 的第 "+strconv.Itoa(idx+1)+" 张图片")
		}
		lines = append(lines, "当前文字回复后还会另外发送图片"+prompt.JoinChineseItems(targets)+"。")
	}
	if e := strings.TrimSpace(emoji); e != "" {
		lines = append(lines, "当前文字回复后还会单独发送一张表情包（"+e+"），无需在正文中描述它。")
	}
	if len(lines) == 0 {
		return ""
	}

	body := b.renderAttachmentPrompt(strings.Join(lines, "\n"))
	if body == "" {
		return ""
	}
	// 与参考实现一致的前缀（maisaka_generator_base.py:598）。
	return "【额外发送内容参考】\n" + body
}

// renderAttachmentPrompt 渲染附件提示词模板。
//
// 模板缺失时直接用 attachments 原文：这里宁可少一层措辞，
// 也不能返回空串 —— 空串意味着模型完全不知道有此附件，
// 就会在正文里开始描述一张它自己杜撰的图。
func (b *PromptBuilder) renderAttachmentPrompt(attachments string) string {
	values := map[string]string{"attachments": attachments}
	if b.prompts == nil {
		return attachments
	}
	tmpl, ok := b.prompts.Get("reply_attachments")
	if !ok || strings.TrimSpace(tmpl) == "" {
		return attachments
	}
	return prompt.Render(tmpl, values)
}

// imagesOf 把消息里的图片/表情二进制转成 data URL 列表。
//
// 为什么用 data URL 而不是把二进制塞给 llm 层：
// llm.Message.Images 已经是 []string，语义就是"可直接放进 image_url 的地址"，
// data URL 正好匹配，省掉一层约定。
//
// 单条消息最多挂 4 张：再多会显著拉高 token 与延迟，
// 而一条群里连发 10 张图的情况，看前 4 张基本就够判断了。
func imagesOf(msg *domain.Message) []string {
	if msg == nil {
		return nil
	}

	const maxImagesPerMessage = 4
	out := make([]string, 0, maxImagesPerMessage)
	for i, seg := range msg.Segments {
		if seg.Type != domain.SegmentImage && seg.Type != domain.SegmentEmoji {
			continue
		}
		if len(seg.Binary) == 0 {
			continue
		}

		// 转码：把 BMP 转 JPEG、GIF 转首帧 PNG，拒绝 WEBP 等模型吃不下的格式。
		//
		// 这一步不能省。把模型不认的格式原样发出去，服务端返回 400，
		// 表现是整轮思考失败 —— 比看不到图严重得多（AGENTS.md 第 19 条）。
		// 转码失败就跳过这张图，绝退回原始字节。
		normalized, err := imgtx.Normalize(seg.Binary)
		if err != nil {
			// 丢弃必须留痕（audit 11 P2-13）：QQ 表情包里 WEBP 占比高，
			// 静默跳过时用户只看到「机器人对这张图没反应」，日志里一个字都没有。
			logx.Get("agent.prompt").Warn("图片不可用，已跳过: "+err.Error(),
				"format", normalized.Format, "segment_index", i)
			continue
		}

		out = append(out, "data:"+normalized.MimeType+";base64,"+base64.StdEncoding.EncodeToString(normalized.Binary))
		if len(out) >= maxImagesPerMessage {
			break
		}
	}
	return out
}

// botReplyAsMessage 把一条机器人发言包装成 domain.Message。
//
// 只为让「引用木木自己说过的话」这条路能拿到 RefMsgIdx ——
// mergedItem.source 原本对自发言是 nil（那时机器人不可能引用自己），
// 现在出站引用要落地，它就必须能带回索引。
//
// 只填引用需要的三个字段，刻意不填 PlainText/Segments：
// 这里的用途是「作为引用目标」，不是「重新渲染一遍」。
func botReplyAsMessage(r BotReply) *domain.Message {
	if r.RefMsgIdx == "" && r.MessageID == "" {
		return nil
	}
	return &domain.Message{
		MessageID:  r.MessageID,
		RefMsgIdx:  r.RefMsgIdx,
		ReceivedAt: r.At,
	}
}
