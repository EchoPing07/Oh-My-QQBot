package qq

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
)

// 本文件负责事件解码与出站编码。
// 对应 refs/MaiBot-QQ-Adapter/codec.py。

var (
	// 匹配 QQ 表情协议标签，例如 <faceType=1,faceId="1",ext="...">
	faceTagPattern = regexp.MustCompile(`(?i)<faceType=.*?>`)
	// 匹配 QQ 附件协议标签。
	attachmentTagPattern = regexp.MustCompile(`(?i)<attachmentType=.*?>`)
	// 匹配 @ 协议标记，例如 <@!123456> 或 <@abc>
	mentionTokenPattern = regexp.MustCompile(`<@!?([0-9A-Za-z_-]+)>`)
)

// 引用消息的类型标记。
//
// 实测（bot/归档/QQ官方引用文档/QQ官方机器人引用报文实测.md）：
// 用户引用一条消息时，入站事件的 message_type 为 103，被引用正文与附件
// 随事件直接下发在 msg_elements 里，不需要回查历史。
// message_type 的其它实测取值：0 = 纯文本，102 = 聊天记录（合并转发）。
const messageTypeQuote = 103

// 消息事件 payload。
// 字段按 QQ 官方文档：群 @ 消息、全量群消息、单聊消息共用大部分结构。
type messagePayload struct {
	ID      string `json:"id"`
	Content string `json:"content"`

	// MessageType 见 messageTypeQuote 的说明。
	//
	// 注意 msg_elements[].message_type 恒为 103（引用元素的固定标记），
	// **不是**被引用消息的类型 —— 引用文本、图片、视频、语音、文件时都取 103。
	MessageType int `json:"message_type"`

	// MessageScene 装着引用索引。
	//
	// Ext 是 "key=value" 形式的字符串数组，实测稳定出现的三个键：
	//   msg_idx     本条消息自身的引用索引（21/21 出现）
	//   ref_msg_idx 被引用消息的引用索引（11/21，等于引用消息数）
	//   auth_token  群聊场景才有（群聊 18/18，单聊 0/3）
	// 两个索引都是 REFIDX_ 开头的 135 字符不透明串，只能整串比对。
	MessageScene struct {
		Ext    []string `json:"ext"`
		Source string   `json:"source"`
	} `json:"message_scene"`

	// ParallelMessage 只在引用事件里出现（11/11）。
	//
	// 它的 msg_nodes 是被引用内容的可读副本，比 msg_elements 更适合当正文兜底：
	// 实测 media 类引用在这里是 "[图片]" / "[语音] 4\"" 这种占位，
	// 而 msg_elements[].content 为空串。
	ParallelMessage struct {
		MsgNodes []json.RawMessage `json:"msg_nodes"`
	} `json:"parallel_message"`

	// 群消息才有。
	GroupOpenID string `json:"group_openid"`
	GroupName   string `json:"group_name"`

	Author struct {
		ID           string `json:"id"`
		UnionOpenID  string `json:"union_openid"`
		MemberOpenID string `json:"member_openid"`
		UserOpenID   string `json:"user_openid"`
		Username     string `json:"username"`
		Nickname     string `json:"nickname"`
	} `json:"author"`

	// 全量群消息才有。
	MsgElements []json.RawMessage `json:"msg_elements"`

	Mentions []struct {
		ID           string `json:"id"`
		OpenID       string `json:"openid"`
		UserOpenID   string `json:"user_openid"`
		MemberOpenID string `json:"member_openid"`
		Username     string `json:"username"`
		Nickname     string `json:"nickname"`
		// IsYou 表示这个 mention 就是当前机器人自己。
		//
		// 必须用它判断「是否 @ 了机器人」：实测 mentions[].id 是 openid 形态
		// （E4E7B42E…），而 READY 下发的 self_id 是 19 位纯数字
		// （7246569821734997014），两者属于不同标识空间，永远不相等。
		// 漏读这个字段的后果是全量群消息模式下 is_at 恒为 false ——
		// 旧日志 96 条群消息、其中 40 条正文带 @，at=true 零命中。
		IsYou bool `json:"is_you"`
	} `json:"mentions"`

	// 附件走 RawMessage 而不是 []attachmentRef。
	//
	// 原因：QQ 的附件结构在不同事件、不同版本里字段类型会变 ——
	// 实测 size 有时是字符串、有时是数字，直接反序列化到 string 会让
	// **整条消息连带正文一起丢掉**（日志里只剩一句「解析消息 payload 失败」）。
	// 表情包之所以「收不到」，就是在这里被整条丢弃的。
	Attachments []json.RawMessage `json:"attachments"`

	Timestamp string `json:"timestamp"`
}

// DecodeResult 是解码结果。
type DecodeResult struct {
	Message *domain.Message
	// Scene 是 group 或 private。
	Scene string
	// TargetID 是出站时要用的目标 OpenID（群为 group_openid，私聊为 user_openid）。
	TargetID string
}

// Decode 把网关事件解码成统一消息。
//
// 关键行为（与插件版一致）：
//   - 群 @ 消息天然 is_at=true
//   - 全量群消息需要检查 mentions / msg_elements / 文本协议标记来判断是否 @ 了机器人
//   - <@id> 协议标记要转写成 at 段，避免 OpenID 原文进入聊天流
//   - 只有表情协议标签的消息，正文置空由 emoji 段承载
//
// 时间口径统一用「本机接收时刻」，不读平台 timestamp —— 原因见下方时间戳处。
func Decode(eventType string, data json.RawMessage, accountID, botName string) (*DecodeResult, error) {
	if !IsMessageEvent(eventType) {
		return nil, fmt.Errorf("不支持的事件类型: %s", eventType)
	}

	var p messagePayload
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("解析消息 payload 失败: %w", err)
	}

	if p.ID == "" {
		return nil, fmt.Errorf("消息缺少 id")
	}

	isGroup := IsGroupEvent(eventType)

	userID := firstNonEmptyStr(
		p.Author.MemberOpenID, p.Author.UserOpenID,
		p.Author.ID, p.Author.UnionOpenID,
	)
	if userID == "" {
		return nil, fmt.Errorf("消息缺少用户 OpenID")
	}
	if isGroup && p.GroupOpenID == "" {
		return nil, fmt.Errorf("群消息缺少 group_openid")
	}

	// 时间口径：统一用「本机接收时刻」，**不读平台 timestamp**。
	//
	// 这是与参考实现刻意一致的选择，而它恰好是本轮 bug 的修法：
	// 参考实现用的是主程序接收入站消息的本机时间
	// （refs/MaiBot/src/platform_io/types.py:199 明写「不使用适配器提供的时间」）。
	//
	// 原来我们直接用了平台 timestamp，实测（库里数据 + 日志对比）：
	//
	//	日志收到 14:47:42  库里记录 14:49:19  偏差 +97s
	//	日志收到 16:50:50  库里记录 16:52:26  偏差 +96s
	//	日志收到 16:53:15  库里记录 16:54:51  偏差 +96s
	//
	// 即 QQ 服务端时间比本机**晚约 96 秒**。而出站消息（agent/outbound.go）
	// 用的是本机 time.Now() —— 两种时间混在一起排序的结果是：
	// 用户先发的表情包，反而排在机器人后发的回复**后面**，
	// 界面上看就是「回复比消息时间还早」。
	//
	// 不能只改成两者都用平台时间：出站消息拿不到平台时间
	// （发送回执只有 id，没有 timestamp），所以只能用本机时间。
	receivedAt := time.Now()

	// 提取正文：顶层 content 优先，全量群消息的文本在 msg_elements 里。
	//
	// rawContent 保留 QQ 的表情/附件协议标签，判断「这是不是动画表情」要靠它；
	// content 是归一化后的可读版本，喂给模型用。
	//
	// message_type == 103 时 msg_elements 装的是**被引用消息**，不是本轮正文，
	// 所以必须把引用类型的 payload 传进去，让它不要吃那层兜底。
	rawContent := extractContent(&p)
	content := normalizeContent(rawContent)

	// 判断是否 @ 了机器人。
	isAt := isBotMentioned(eventType, &p, content, accountID)

	// 把 <@id> 协议标记转写成 at 段。
	text, segments := rewriteMentionTokens(content, accountID, botName, &p)

	// 附件先解析出来：上面的正文清空判断要知道「到底有没有附件」。
	attachments := parseAttachments(p.Attachments)

	// 正文只剩表情协议标签转写出来的占位时清空：
	// 真实内容接下来由 emoji 段承载，留着 [表情] 会和附件重复展示。
	// 参考实现见 codec.py:decode_message 里的 `if is_emoji and content in {"[表情]", "[动画表情]"}`。
	if isEmojiOnlyText(text) && len(attachments) > 0 {
		text = ""
		segments = dropTextSegments(segments)
	}

	// 附件（图片、表情、语音、文件）转成消息段。
	//
	// 早期版本完全没处理 attachments，后果是「只发一张图不打字」
	// 这类消息会被下面的空消息检查直接丢掉，日志里只留一句
	// 「消息不包含文本或可处理的附件」，看起来像 QQ 没推消息。
	attSegments, attLabels := buildAttachmentSegments(attachments, rawContent)
	if len(attSegments) > 0 {
		segments = append(segments, attSegments...)
		// 正文只剩表情协议标签转写出来的占位时清空：
		// 真实内容已经由 emoji 段承载，再留一个 [表情] 会和附件重复展示。
		// 参考实现见 codec.py:decode_message 里的 `if is_emoji and content in {...}`。
		if isEmojiOnlyText(text) && hasEmojiSegment(attSegments) {
			text = ""
		}
		if text == "" {
			// 附件自带可读标签时，用它当正文，避免整条消息无文本。
			text = strings.Join(attLabels, " ")
		}
	}

	// 只有表情标签没有实际内容时，正文置空。
	//
	// 引用消息的例外：用户只引用、不打字时正文本来就是空的，
	// 此时真正有信息量的是被引用内容与引用关系本身，不能整条丢掉。
	quoted, hasQuote := extractQuoted(&p)
	if text == "" && len(segments) == 0 && !hasQuote {
		return nil, fmt.Errorf("消息不包含文本或可处理的附件")
	}

	// 引用段插在首位，对齐参考实现的 insert(0, ReplyComponent(...))：
	// 底层消息系统的回复组件总是排在消息段最前面
	// （refs/MaiBot/src/chat/message_receive/uni_message_sender.py:301）。
	//
	// 引用段的 MessageID 存被引用消息的引用索引（REFIDX_…），
	// 没有时退回 msg_elements[0].msg_idx，两者实测 10/10 相等。
	quoteSegmentID := ""
	if hasQuote {
		quoteSegmentID = quoted.refMsgIdx
		if quoteSegmentID == "" {
			quoteSegmentID = quoted.elementMsgIdx
		}
		if quoteSegmentID != "" {
			segments = append([]domain.Segment{domain.Reply(quoteSegmentID)}, segments...)
		}
	}

	nickname := firstNonEmptyStr(p.Author.Username, p.Author.Nickname, userID)

	// 引用元信息。键名与内嵌消息的 additional 字段保持一致：
	// Store 层不区分入站/出站，Additional 会原样落库成 JSON。
	additional := map[string]string{}
	selfMsgIdx := messageSceneExtValue(p.MessageScene.Ext, "msg_idx")
	if selfMsgIdx != "" {
		// 自己这条消息的引用索引。别人引用木木这一句时，
		// 他事件里的 ref_msg_idx 就等于这个值（实测 7/11 闭环）。
		// 同时存一份到 Additional：列里那份是给 SQL 反查用的，
		// 这份是给「不查库也能读到」的调用方与调试用的。
		additional[domain.AdditionalMsgIdx] = selfMsgIdx
	}
	if hasQuote {
		if quoted.refMsgIdx != "" {
			// 被引用目标的索引。它是 REFIDX_ 键空间里唯一能反查"被引的是哪句"的东西 ——
			// 不能用 msg_elements[0].msg_idx 代替：引用聊天记录时它是 null，
			// 而 ref_msg_idx 是 TMP_<uuid>，两者根本不在同一键空间。
			additional[domain.AdditionalRefMsgIdx] = quoted.refMsgIdx
		}
		if quoted.text != "" {
			// 被引用正文的纯文本快照。
			//
			// 为什么不只存索引、要用时再查库：被引消息可能是很久以前的，
			// 也可能因为保留期清理已经没了。QQ 把正文随事件下发，
			// 当场存在自己身上比事后回查可靠。
			additional[domain.AdditionalQuotedText] = quoted.text
		}
	}

	msg := &domain.Message{
		ID:         p.ID,
		Platform:   Platform,
		MessageID:  p.ID,
		UserID:     userID,
		UserName:   nickname,
		GroupID:    p.GroupOpenID,
		GroupName:  p.GroupName,
		Segments:   segments,
		IsAt:       isAt,
		IsGroup:    isGroup,
		ReceivedAt: receivedAt,
		AccountID:  accountID,
		RefMsgIdx:  selfMsgIdx,
	}
	if len(additional) > 0 {
		msg.Additional = additional
	}

	// is_mentioned 在插件版里直接等于 is_at；这里额外检查文本里是否出现了机器人名字。
	msg.IsMentioned = isAt || containsBotName(text, botName)

	msg.PlainText = domain.BuildPlainText(msg.Segments)

	targetID := p.GroupOpenID
	if !isGroup {
		targetID = userID
	}

	scene := domain.ScopeGroup
	if !isGroup {
		scene = domain.ScopePrivate
	}

	return &DecodeResult{
		Message:  msg,
		Scene:    scene,
		TargetID: targetID,
	}, nil
}

// extractContent 取消息正文。
// 顶层 content 优先；全量群消息的文本在 msg_elements 里。
//
// 引用消息（message_type == 103）必须排除在 msg_elements 兜底之外：
// 那一层装的是**被引用消息**的正文，不是本轮输入。
//
// 这个 bug 的两位都很严重，而且都不报错：
//   - 带字引用时顶层 content 非空，被引用正文静默丢掉（模型看不到引用）
//   - 只引用不打字时顶层 content 为空，于是**把被引用的旧消息当成
//     用户这次说的话**（错误归因，比看不到引用严重得多）
//
// 对照 qq-maid-bot 的做法：它只在 message_type == 103 时把 msg_elements
// 当引用内容解析，绝不当正文（qq-maid-gateway-rs/src/gateway/event/quoted_payload.rs:100）。
func extractContent(p *messagePayload) string {
	if s := strings.TrimSpace(p.Content); s != "" {
		return s
	}
	if isQuotePayload(p) {
		return ""
	}

	var parts []string
	for _, raw := range p.MsgElements {
		parts = append(parts, collectElementText(raw)...)
	}
	return strings.Join(parts, "")
}

// collectElementText 递归收集元素里的文本。
//
// msg_elements 的结构不稳定，不同事件类型字段名不同，
// 这里用宽松匹配：优先 content/text/text_element.content。
func collectElementText(raw json.RawMessage) []string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	var out []string

	var walk func(any)
	walk = func(node any) {
		switch n := node.(type) {
		case map[string]any:
			for _, key := range []string{"content", "text"} {
				if s, ok := n[key].(string); ok && strings.TrimSpace(s) != "" {
					out = append(out, s)
				}
			}
			if te, ok := n["text_element"].(map[string]any); ok {
				if s, ok := te["content"].(string); ok && strings.TrimSpace(s) != "" {
					out = append(out, s)
				}
			}
			for k, child := range n {
				switch k {
				case "content", "text", "text_element":
					continue
				}
				switch child.(type) {
				case map[string]any, []any:
					walk(child)
				}
			}
		case []any:
			for _, child := range n {
				walk(child)
			}
		}
	}
	walk(v)
	return out
}

// normalizeContent 把 QQ 的表情/附件协议标签转成可读文本。
func normalizeContent(content string) string {
	content = attachmentTagPattern.ReplaceAllString(content, "")
	content = faceTagPattern.ReplaceAllString(content, "[表情]")
	return strings.TrimSpace(content)
}

// quoted 是一条引用消息解析出来的引用信息。
type quoted struct {
	// refMsgIdx 来自 message_scene.ext 的 ref_msg_idx。
	// 引用聊天记录时它是 TMP_<uuid>，与 REFIDX_ 不互通。
	refMsgIdx string
	// elementMsgIdx 是 msg_elements[0].msg_idx。
	// 实测在 REFIDX_ 类引用上与 refMsgIdx 10/10 相等；
	// 引用聊天记录时为 null（此时靠 refMsgIdx 识别出这是引用）。
	elementMsgIdx string
	// text 是被引用正文的可读文本。
	text string
	// textAttachments 是被引用元素里的附件（与顶层 attachments 同构）。
	textAttachments []json.RawMessage
}

// isQuotePayload 判断这条消息是否为引用消息。
//
// 两个依据取或：
//   - message_type == 103（实测 11/11）
//   - message_scene.ext 里带 ref_msg_idx
//
// 只看 message_type 会漏掉那些把标记重置为 0 的实现；
// 只看 ref_msg_idx 则对字段缺失的旧版本不友好。
func isQuotePayload(p *messagePayload) bool {
	if p.MessageType == messageTypeQuote {
		return true
	}
	return messageSceneExtValue(p.MessageScene.Ext, "ref_msg_idx") != ""
}

// messageSceneExtValue 从 message_scene.ext 里按 key 取值。
//
// ext 是 "key=value" 形式的字符串数组（不是 map）：
//
//	["ref_msg_idx=REFIDX_…", "msg_idx=REFIDX_…", "auth_token=…"]
//
// value 里可能带 '='（base64 的填充符），所以只能按第一个 '=' 切。
func messageSceneExtValue(ext []string, key string) string {
	prefix := key + "="
	for _, item := range ext {
		item = strings.TrimSpace(item)
		if !strings.HasPrefix(item, prefix) {
			continue
		}
		return strings.TrimSpace(strings.TrimPrefix(item, prefix))
	}
	return ""
}

// extractQuoted 解析引用消息的被引用内容。
//
// 被引用正文有三个可能的载体，按可靠性从高到低排：
//
//  1. msg_elements[].content —— 官方主载体，纯文本引用时就是原文
//  2. parallel_message.msg_nodes[].content —— 可读副本。
//     关键是**媒体类引用只在它有值**：实测引用图片/语音/文件时
//     msg_elements[].content 是空串，而这里是 "[图片]" / "[语音] 4\""。
//  3. 两者都空时由调用方按附件类型补占位
//
// 附件走 msg_elements[].attachments —— 实测与顶层 attachments 字段集合
// 完全一致（无独立 schema），所以直接复用 parseAttachments。
func extractQuoted(p *messagePayload) (quoted, bool) {
	if !isQuotePayload(p) {
		return quoted{}, false
	}

	out := quoted{refMsgIdx: messageSceneExtValue(p.MessageScene.Ext, "ref_msg_idx")}

	// msg_elements 只取第一层：元素内部的 msg_elements 属于更早的历史引用，
	// 不递归恢复（对照 qq-maid-bot: 「子元素只表示更早的结构化历史引用」）。
	var elementTexts []string
	for _, raw := range p.MsgElements {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil || len(m) == 0 {
			continue
		}
		if idx := strings.TrimSpace(looseString(m["msg_idx"])); idx != "" && out.elementMsgIdx == "" {
			out.elementMsgIdx = idx
		}
		if s := strings.TrimSpace(looseString(m["content"])); s != "" {
			elementTexts = append(elementTexts, s)
		}
		// attachments 与顶层同构，复用同一个解析器。
		if rawAtts, ok := m["attachments"].([]any); ok {
			for _, att := range rawAtts {
				if buf, err := json.Marshal(att); err == nil {
					out.textAttachments = append(out.textAttachments, buf)
				}
			}
		}
	}
	if len(elementTexts) > 0 {
		out.text = strings.Join(elementTexts, "\n")
	}

	// parallel_message 当媒体类引用的正文来源。
	// 有 msg_elements 正文时不覆盖 —— 官方主载体的优先级更高。
	if out.text == "" {
		var nodes []string
		for _, raw := range p.ParallelMessage.MsgNodes {
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil || len(m) == 0 {
				continue
			}
			if s := strings.TrimSpace(looseString(m["content"])); s != "" {
				nodes = append(nodes, s)
			}
		}
		if len(nodes) > 0 {
			out.text = strings.Join(nodes, "\n")
		}
	}

	// 正文全空但有附件时，用类型占位 —— 否则模型只看到「引用了一条消息」
	// 却不知道引的是什么。
	if strings.TrimSpace(out.text) == "" && len(out.textAttachments) > 0 {
		labels := quotedAttachmentLabels(parseAttachments(out.textAttachments))
		if len(labels) > 0 {
			out.text = strings.Join(labels, " ")
		}
	}

	return out, true
}

// quotedAttachmentLabels 把引用附件转成可读占位（[图片] / [语音] / 文件名）。
//
// 不下载二进制：被引用的图/语音只需要让模型知道「引的是一张图」，
// 真正的视觉理解应该在原消息那一轮就做完了。
func quotedAttachmentLabels(atts []attachmentRef) []string {
	labels := make([]string, 0, len(atts))
	for _, att := range atts {
		switch {
		case att.isVoice():
			if t := strings.TrimSpace(att.ASRText); t != "" {
				labels = append(labels, t)
				continue
			}
			labels = append(labels, "[语音]")
		case att.isImage():
			labels = append(labels, "[图片]")
		default:
			name := strings.TrimSpace(att.FileName)
			if name == "" {
				name = "未命名"
			}
			labels = append(labels, "[文件："+name+"]")
		}
	}
	return labels
}

// isBotMentioned 判断消息是否 @ 了机器人。
//
// 群 @ 事件天然指向机器人；
// 全量群消息需要检查 mentions、msg_elements 与文本协议标记。
func isBotMentioned(eventType string, p *messagePayload, content, accountID string) bool {
	if eventType == GroupAtMessageEvent {
		return true
	}
	if eventType != GroupMessageEvent || accountID == "" {
		return false
	}

	for _, m := range p.Mentions {
		// is_you 是唯一可靠的依据。
		//
		// 实测 mentions[].id 是 openid 形态（E4E7B42E…），
		// 而 accountID 来自 READY 的 user.id，是 19 位纯数字 —— 两者永远不相等。
		// 旧实现只比 id，导致全量群消息模式的 is_at 恒为 false
		// （旧日志：96 条群消息、其中 40 条正文带 @，at=true 零命中）。
		// 对照 qq-maid-bot 的做法也是优先认 is_you
		// （qq-maid-gateway-rs/src/gateway/event/mod.rs:442-446）。
		if m.IsYou {
			return true
		}
		id := firstNonEmptyStr(m.ID, m.OpenID, m.UserOpenID, m.MemberOpenID)
		if id == accountID {
			return true
		}
	}

	if treeMentionsBot(p.MsgElements, accountID) {
		return true
	}

	// 频道式 <@id>/<@!id> 协议标记，以及 @机器人AppID/@裸 ID 的文本写法。
	if strings.Contains(content, "<@"+accountID+">") || strings.Contains(content, "<@!"+accountID+">") {
		return true
	}
	pattern := regexp.MustCompile(`@(?:机器人)?` + regexp.QuoteMeta(accountID) + `(?:\s|$)`)
	return pattern.MatchString(content)
}

// treeMentionsBot 递归检查 msg_elements 树里是否有指向机器人的 @ 元素。
func treeMentionsBot(elements []json.RawMessage, accountID string) bool {
	for _, raw := range elements {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			continue
		}
		if nodeMentionsBot(v, accountID) {
			return true
		}
	}
	return false
}

func nodeMentionsBot(node any, accountID string) bool {
	switch n := node.(type) {
	case []any:
		for _, child := range n {
			if nodeMentionsBot(child, accountID) {
				return true
			}
		}
	case map[string]any:
		elementType := strings.ToLower(fmt.Sprint(firstNonNil(n["type"], n["element_type"])))
		looksLikeMention := strings.Contains(elementType, "at") || strings.Contains(elementType, "mention")

		for key, raw := range n {
			lowerKey := strings.ToLower(key)

			// {"at": {"id": "..."}} 这类结构。
			if strings.Contains(lowerKey, "at") || strings.Contains(lowerKey, "mention") {
				if child, ok := raw.(map[string]any); ok {
					for _, targetKey := range []string{"id", "openid", "user_id", "target_id", "member_openid"} {
						if s, ok := child[targetKey].(string); ok && s == accountID {
							return true
						}
					}
				}
			}

			// 元素本身是 mention 类型，直接看它的 id 字段。
			if looksLikeMention {
				switch lowerKey {
				case "id", "openid", "user_id", "target_id", "member_openid":
					if s, ok := raw.(string); ok && s == accountID {
						return true
					}
				}
			}

			// 递归。
			switch raw.(type) {
			case map[string]any, []any:
				if nodeMentionsBot(raw, accountID) {
					return true
				}
			}
		}
	}
	return false
}

// mentionPiece 是转写过程中的一段：要么是文本，要么是一个 at 段。
type mentionPiece struct {
	text string
	at   *domain.Segment
}

// rewriteMentionTokens 把 <@id> 协议标记转写成标准 at 段。
//
// 规则（照抄插件版）：
//   - @ 机器人：删除标记并生成机器人的 at 段
//   - @ 其他人：用 mentions 数组的名字生成 at 段，取不到名字则仅删除标记
//   - 保持 "文本A @x 文本B @y 文本C" 的穿插顺序
func rewriteMentionTokens(content, accountID, botName string, p *messagePayload) (string, []domain.Segment) {
	// 先把 mentions 整理成 id -> 名字。
	displayNames := make(map[string]string)
	for _, m := range p.Mentions {
		id := firstNonEmptyStr(m.ID, m.OpenID, m.UserOpenID, m.MemberOpenID)
		name := firstNonEmptyStr(m.Username, m.Nickname)
		if id != "" && name != "" {
			displayNames[id] = name
		}
	}

	var pieces []mentionPiece
	cursor := 0
	for _, match := range mentionTokenPattern.FindAllStringSubmatchIndex(content, -1) {
		pieces = append(pieces, mentionPiece{text: content[cursor:match[0]]})
		cursor = match[1]

		mentionID := content[match[2]:match[3]]

		if mentionID == accountID {
			seg := domain.At(accountID, botName)
			pieces = append(pieces, mentionPiece{at: &seg})
			continue
		}

		if name, ok := displayNames[mentionID]; ok {
			seg := domain.At(mentionID, name)
			pieces = append(pieces, mentionPiece{at: &seg})
		}
	}
	pieces = append(pieces, mentionPiece{text: content[cursor:]})

	// 合并相邻文本。
	var merged []mentionPiece
	for _, pc := range pieces {
		if pc.at == nil && len(merged) > 0 && merged[len(merged)-1].at == nil {
			merged[len(merged)-1].text += pc.text
			continue
		}
		merged = append(merged, pc)
	}

	// 去掉首尾空白。
	if len(merged) > 0 && merged[0].at == nil {
		merged[0].text = strings.TrimLeft(merged[0].text, " \t\n")
	}
	if len(merged) > 0 && merged[len(merged)-1].at == nil {
		merged[len(merged)-1].text = strings.TrimRight(merged[len(merged)-1].text, " \t\n")
	}

	var (
		segments []domain.Segment
		plain    strings.Builder
	)
	for _, pc := range merged {
		if pc.at != nil {
			segments = append(segments, *pc.at)
			plain.WriteString(domain.RenderAt(*pc.at))
			plain.WriteString(" ")
			continue
		}
		if pc.text == "" {
			continue
		}
		segments = append(segments, domain.Text(pc.text))
		plain.WriteString(pc.text)
	}

	return strings.TrimSpace(plain.String()), segments
}

// containsBotName 判断文本里是否出现了机器人名字或别名。
func containsBotName(text, botName string) bool {
	if botName == "" {
		return false
	}
	return strings.Contains(strings.ToLower(text), strings.ToLower(botName))
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func firstNonNil(vals ...any) any {
	for _, v := range vals {
		if v != nil {
			return v
		}
	}
	return nil
}

// atoiSafe 解析整数，失败返回 0。
func atoiSafe(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}

// attachmentRef 是一条入站附件的描述。
//
// 只保留判断与下载需要的字段：
//   - ContentType / Type 用来区分图片、表情、语音、文件
//   - URL 是 QQ 的临时下载地址（会过期，所以必须即时下载）
//   - FileName 只用于日志与落库命名
type attachmentRef struct {
	ContentType string
	Type        string
	URL         string
	VoiceWAVURL string
	FileName    string
	Size        string
	IsEmoji     bool
	ASRText     string
}

// parseAttachments 把原始附件列表解析成 attachmentRef。
//
// 为什么不能直接 json.Unmarshal 到结构体数组：
// QQ 的附件字段类型不稳定 —— 实测 attachments[].size 有时下发数字、
// 有时下发字符串；content_type 也可能缺失。
// 一旦某个字段类型对不上，**整条消息连同正文一起反序列化失败**，
// 日志里只剩一句「解析消息 payload 失败」，表现为「机器人对表情包完全没反应」。
//
// 所以逐个附件、逐个字段宽松扫码：拿不到就是零值，解不开就跳过这一条，
// 绝不因为一个附件字段的格式变化把整条消息丢掉。
func parseAttachments(raws []json.RawMessage) []attachmentRef {
	if len(raws) == 0 {
		return nil
	}
	out := make([]attachmentRef, 0, len(raws))
	for _, raw := range raws {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil || len(m) == 0 {
			continue
		}
		out = append(out, attachmentRef{
			ContentType: looseString(m["content_type"]),
			Type:        looseString(m["type"]),
			URL:         looseString(m["url"]),
			VoiceWAVURL: looseString(m["voice_wav_url"]),
			FileName:    looseString(m["filename"]),
			Size:        looseString(m["size"]),
			IsEmoji:     looseBool(m["is_emoji"]),
			ASRText:     looseString(m["asr_refer_text"]),
		})
	}
	return out
}

// looseString 把任意 JSON 值转成字符串，兼容数字/布尔被当成字符串下发的情况。
func looseString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	default:
		return strings.TrimSpace(fmt.Sprint(x))
	}
}

// looseBool 兼容 true / "true" / 1 / "1" 几种写法。
//
// 参考实现直接用 Python 的真值判断（plugin.py:_build_attachments 里的 `bool(attachment.get("is_emoji"))`），
// 字符串 "false" 在 Python 里也是真值 —— 这里保持一致：非空字符串算真。
func looseBool(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0
	case string:
		return strings.TrimSpace(x) != "" && strings.TrimSpace(x) != "0"
	default:
		return false
	}
}

// isImage 判断附件是不是图片类（含表情、贴纸）。
func (a attachmentRef) isImage() bool {
	ct := strings.ToLower(strings.TrimSpace(a.ContentType))
	t := strings.ToLower(strings.TrimSpace(a.Type))
	if strings.HasPrefix(ct, "image/") {
		return true
	}
	switch t {
	case "image", "emoji", "face", "sticker":
		return true
	}
	return false
}

// isVoice 判断附件是不是语音。
func (a attachmentRef) isVoice() bool {
	ct := strings.ToLower(strings.TrimSpace(a.ContentType))
	t := strings.ToLower(strings.TrimSpace(a.Type))
	return ct == "voice" || strings.HasPrefix(ct, "audio/") || t == "voice"
}

// isEmojiAttachment 判断附件是否应被当作表情包而不是普通图片。
//
// 参考实现（plugin.py:_build_attachments 里的 is_emoji_attachment）四个条件取或：
//
//	attachment_type in {"emoji", "face", "sticker"}
//	or bool(attachment.get("is_emoji"))
//	or has_single_inline_emoji
//	or content_hint in {"[表情]", "[动画表情]"}
//
// 最后一条很容易漏：QQ 对「动画表情」有时不发附件类型，只把正文写成
// 字面量「[表情]」/「[动画表情]」。漏了它的后果是这类表情包
// 被当成普通图片存进 image 段，于是表情包库永远收不到东西。
func (a attachmentRef) isEmojiAttachment(singleInlineEmoji bool, contentHint string) bool {
	t := strings.ToLower(strings.TrimSpace(a.Type))
	switch t {
	case "emoji", "face", "sticker":
		return true
	}
	if a.IsEmoji || singleInlineEmoji {
		return true
	}
	switch strings.TrimSpace(contentHint) {
	case "[表情]", "[动画表情]":
		return true
	}
	return false
}

// mediaURL 返回实际可下载的地址。
// 语音优先用 wav 转码地址（原始地址是 silk，拿不到也没法播）。
func (a attachmentRef) mediaURL() string {
	if s := strings.TrimSpace(a.VoiceWAVURL); s != "" {
		return s
	}
	return strings.TrimSpace(a.URL)
}

// buildAttachmentSegments 把入站附件转换成消息段。
//
// 这里只产出"带 URL、待下载"的段，二进制由 adapter 层补齐 ——
// Decode 保持纯函数，测试里能直接断言段结构而不用起 HTTP 服务。
//
// 参考实现见 refs/MaiBot-QQ-Adapter/plugin.py:_build_attachments。
func buildAttachmentSegments(atts []attachmentRef, contentHint string) (segments []domain.Segment, labels []string) {
	if len(atts) == 0 {
		return nil, nil
	}

	// 官方限制最多 10 个附件，超出部分只处理前面这些。
	const maxAttachments = 10
	if len(atts) > maxAttachments {
		atts = atts[:maxAttachments]
	}

	// 整条消息只有一个附件、且正文里出现 QQ 表情协议标签时，
	// 这个附件按表情包处理（QQ 的"动画表情"就是这么下发的）。
	singleInlineEmoji := len(atts) == 1 && faceTagPattern.MatchString(contentHint)

	for _, att := range atts {
		switch {
		case att.isImage():
			emoji := att.isEmojiAttachment(singleInlineEmoji, contentHint)
			segType := domain.SegmentImage
			label := "[图片]"
			if emoji {
				segType = domain.SegmentEmoji
				label = "[表情]"
			}
			segments = append(segments, domain.Segment{
				Type: segType,
				URL:  att.mediaURL(),
				// MimeType 先按 content_type 填，下载后由 adapter 按文件头修正。
				MimeType: strings.TrimSpace(att.ContentType),
			})
			labels = append(labels, label)

		case att.isVoice():
			text := strings.TrimSpace(att.ASRText)
			if text == "" {
				text = "[语音]"
			}
			segments = append(segments, domain.Segment{
				Type:     domain.SegmentVoice,
				URL:      att.mediaURL(),
				MimeType: strings.TrimSpace(att.ContentType),
				Text:     text,
			})
			labels = append(labels, text)

		default:
			// 文件、视频等：只留占位，不做下载。
			// 它们的价值主要在"有人发了东西"，正文里有个标记就够了。
			name := strings.TrimSpace(att.FileName)
			if name == "" {
				name = "未命名"
			}
			ct := strings.ToLower(att.ContentType)
			if strings.HasPrefix(ct, "video/") {
				segments = append(segments, domain.Segment{Type: domain.SegmentFile, URL: att.mediaURL(), Text: "[视频]"})
				labels = append(labels, "[视频]")
				continue
			}
			segments = append(segments, domain.Segment{
				Type: domain.SegmentFile,
				URL:  att.mediaURL(),
				Text: name,
			})
			labels = append(labels, "[文件："+name+"]")
		}
	}
	return segments, labels
}

// isEmojiOnlyText 判断正文是否只是表情占位。
func isEmojiOnlyText(text string) bool {
	switch strings.TrimSpace(text) {
	case "[表情]", "[动画表情]":
		return true
	}
	return false
}

// hasEmojiSegment 判断段列表里是否有表情段。
func hasEmojiSegment(segments []domain.Segment) bool {
	for _, seg := range segments {
		if seg.Type == domain.SegmentEmoji {
			return true
		}
	}
	return false
}

// dropTextSegments 去掉段列表里的文本段，只保留 at 与媒体段。
func dropTextSegments(segments []domain.Segment) []domain.Segment {
	out := segments[:0]
	for _, seg := range segments {
		if seg.Type == domain.SegmentText {
			continue
		}
		out = append(out, seg)
	}
	return out
}

// looseInt 把任意 JSON 值转成 int，兼容数字与字符串两种下发形式。
//
// 与 looseString 是同一类兼容：QQ 的接口在不同端点、不同版本里
// 会把同一个字段下发成数字或字符串（实测 upload_prepare 的 block_size、
// parts[].index 都是字符串）。用写死类型的结构体接就会直接报
// `cannot unmarshal string into Go struct field ... of type int`，
// 而这类错误会整条链路失败，且错误信息完全不提「字段类型不稳定」。
func looseInt(v any) int {
	switch x := v.(type) {
	case nil:
		return 0
	case float64:
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(x))
		if err != nil {
			return 0
		}
		return n
	case bool:
		if x {
			return 1
		}
		return 0
	default:
		return 0
	}
}
