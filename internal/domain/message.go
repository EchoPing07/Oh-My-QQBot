// Package domain 定义与平台无关的核心数据模型。
// 对应 MaiBot 的 src/common/data_models 与 src/core/types.py。
package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// SegmentType 是消息段的类型。
type SegmentType string

const (
	SegmentText    SegmentType = "text"
	SegmentImage   SegmentType = "image"
	SegmentEmoji   SegmentType = "emoji"
	SegmentAt      SegmentType = "at"
	SegmentReply   SegmentType = "reply"
	SegmentVoice   SegmentType = "voice"
	SegmentFile    SegmentType = "file"
	SegmentForward SegmentType = "forward"
)

// Segment 是一个消息段。
type Segment struct {
	Type SegmentType `json:"type"`

	// Text 用于 text 段。
	Text string `json:"text,omitempty"`

	// At 段的目标。
	UserID   string `json:"user_id,omitempty"`
	UserName string `json:"user_name,omitempty"`

	// 媒体段。
	URL      string `json:"url,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	Binary   []byte `json:"binary,omitempty"`

	// Hash 是媒体内容的 sha256，落库时保留。
	//
	// 为什么需要它：二进制不落库（见 store.MessageRepo.Save 的说明），
	// 但 WebUI 要把图片/表情直接显示出来。表情包文件按 sha256 存在
	// data/emoji/<前两位>/<hash>.<ext>，拿到 hash 就能回查文件。
	// 没有 hash 的话前端只能显示「[表情]」占位文字 —— 那正是之前的观感问题。
	Hash string `json:"hash,omitempty"`

	// Reply 段引用的消息。
	MessageID string `json:"message_id,omitempty"`
}

// 消息段构造辅助函数。

func Text(s string) Segment          { return Segment{Type: SegmentText, Text: s} }
func Image(url string) Segment       { return Segment{Type: SegmentImage, URL: url} }
func Emoji(url string) Segment       { return Segment{Type: SegmentEmoji, URL: url} }
func At(userID, name string) Segment { return Segment{Type: SegmentAt, UserID: userID, UserName: name} }
func Reply(msgID string) Segment     { return Segment{Type: SegmentReply, MessageID: msgID} }

// Message 是一条统一格式的消息。
type Message struct {
	// ID 是本系统内部的唯一标识。
	ID string `json:"id"`

	// Platform 固定为 qq。
	Platform string `json:"platform"`

	// MessageID 是平台侧的消息 ID。
	MessageID string `json:"message_id"`

	// SessionID 是所属聊天流。
	SessionID string `json:"session_id"`

	// 发送者。
	UserID   string `json:"user_id"`
	UserName string `json:"user_name"`
	// UserCardName 是群名片。
	// 对应 MaiBot 的 user_info.user_cardname，渲染进 Planner 前缀的 group_card 属性。
	UserCardName string `json:"user_card_name,omitempty"`

	// 群信息，私聊时为空。
	GroupID   string `json:"group_id,omitempty"`
	GroupName string `json:"group_name,omitempty"`

	Segments []Segment `json:"segments"`

	// PlainText 是用于喂给模型的纯文本表示。
	PlainText string `json:"plain_text"`

	// IsAt 表示这条消息 @ 了机器人。
	IsAt bool `json:"is_at"`

	// IsMentioned 表示这条消息在文本里提到了机器人名字。
	IsMentioned bool `json:"is_mentioned"`

	// IsGroup 表示是否为群聊消息。
	IsGroup bool `json:"is_group"`

	// IsCommand 表示已被命令链路处理。
	IsCommand bool `json:"is_command"`

	// IsSelf 表示这条消息是机器人自己发的。
	//
	// 出站消息也要入库（参考实现 send_service.py:_store_sent_message 每条成功
	// 发送都写库），否则重启后上下文只有用户说的那半边，
	// WebUI 的会话页也看不到机器人回过什么。
	IsSelf bool `json:"is_self"`

	// ReceivedAt 是收到时刻。
	ReceivedAt time.Time `json:"received_at"`

	// AccountID 是收到这条消息的机器人账号。
	AccountID string `json:"account_id"`

	// RefMsgIdx 是本条消息自身的引用索引（QQ 的 msg_idx，形如 REFIDX_…）。
	//
	// 单独成列而不是只放 Additional 里：别人引用木木这句话时，
	// 他事件里的 ref_msg_idx 就等于这个值，所以要按它反查。
	// SQLite 对 JSON 值无法建可用索引，塞在 additional 里就是全表扫描。
	// 平台无关：非 QQ 渠道留空即可。
	RefMsgIdx string `json:"ref_msg_idx,omitempty"`

	// Additional 存放平台特定的额外字段，例如 OpenID。
	//
	// 引用相关的键名见下方常量：AdditionalRefMsgIdx / AdditionalMsgIdx /
	// AdditionalQuotedText / AdditionalQuotedSender / AdditionalReplyTo。
	// 放在 domain 里而不是 qq 包，是因为读写两头跨包：
	// qq 包写入（codec），agent 包与 web 包都要读。
	Additional map[string]string `json:"additional,omitempty"`
}

// additional 里与引用相关的键名。
//
// 这几个键横跨 qq（写入）、agent（拼提示词）、web（引用预览）三层，
// 必须共用同一份定义 —— 单侧笔误的表现是「那个字段永远是空的」，
// 不报错、不影响其他逻辑，只有把键名写进断言才能抓到。
const (
	// AdditionalRefMsgIdx 是被引用消息的引用索引（REFIDX_… 或 TMP_<uuid>）。
	//
	// REFIDX_ 可用作出站 message_reference 的目标；TMP_<uuid> 是聊天记录
	// （合并转发）的引用，属于另一套键空间，不能作为出站引用目标。
	AdditionalRefMsgIdx = "ref_msg_idx"
	// AdditionalMsgIdx 是本条消息自身的引用索引。
	//
	// 别人的消息引用木木这一句时，他事件里的 ref_msg_idx 就等于这个值。
	AdditionalMsgIdx = "msg_idx"
	// AdditionalQuotedText 是被引用正文的纯文本快照。
	//
	// 存快照而不是事后再查库：被引消息可能已被保留期清掉，
	// 而 QQ 本来就把正文随事件下发了，当场存下来最可靠。
	AdditionalQuotedText = "quoted_text"
	// AdditionalQuotedSender 是被引用消息的发送者昵称，取不到时为空。
	//
	// QQ 的 msg_elements 元素**不含 author**（实测 0/11），
	// 所以要填这个只能靠本地按索引反查。
	AdditionalQuotedSender = "quoted_sender"
	// AdditionalReplyTo 是出站消息引用的目标消息编号。
	AdditionalReplyTo = "reply_to"
)

// Reference 是一次发送要挂的引用。
//
// 为什么单独成一个类型而不直接传 string：出站引用需要的是 REFIDX_ 形态
// 的引用索引，而这个索引与 Message.MessageID（ROBOT1.0_ 形态）是两个
// 不同的键空间。用同一个 string 传的话，调用方很容易把 d.id 当成引用目标
// 传进去 —— 两者都是长串不透明字符串，而平台对它们都接受，
// 所以传错了不会当场报错，只会让「引用过谁」这条链永远对不上。
//
// 零值表示「不引用」。
type Reference struct {
	// RefIdx 是引用目标的引用索引（REFIDX_…）。
	//
	// 来源只有两处：入站消息的 msg_idx，与发送响应的 ext_info.ref_idx。
	RefIdx string `json:"ref_idx,omitempty"`
}

// IsZero 报告这个引用是否为空。
func (r Reference) IsZero() bool { return strings.TrimSpace(r.RefIdx) == "" }

// Ref 构造一个引用。
func Ref(refIdx string) Reference { return Reference{RefIdx: strings.TrimSpace(refIdx)} }

// ReferenceOf 从一条消息取它可作为引用目标的索引。
//
// 只认 RefMsgIdx（它就是 codec 写入的 msg_idx）。
// 不能退回到 MessageID（d.id / ROBOT1.0_）：虽然平台实测也接受，
// 但它属于另一个键空间，而我们所有反查索引都按 REFIDX_ 建，
// 两种混用会让引用关系无法反查。
func ReferenceOf(m *Message) Reference {
	if m == nil {
		return Reference{}
	}
	return Ref(m.RefMsgIdx)
}

// SendResult 是一次发送的结果。
//
// 放在 domain 而不是 agent：qq 适配层要产出它，agent 层要消费它，
// 而 qq 不能反向依赖 agent（会形成循环）。
//
// 为什么不是单纯的 []string：除了消息 ID（ROBOT1.0_ 形态），
// 还需要每条消息自己的引用索引（REFIDX_ 形态，来自响应 ext_info.ref_idx）。
// 机器人引用自己上一句话时必须用它 —— 入站事件里根本没有这个值，
// 只有发送响应才有。少了它，「引用自己的发言」永远落空。
type SendResult struct {
	// MessageIDs 是按顺序发出的各条消息的 ID。
	MessageIDs []string
	// RefIdxs 与 MessageIDs 一一对应，是各条消息自己的引用索引。
	// 平台没返回时会是一个空串占位，长度与 MessageIDs 对齐。
	RefIdxs []string
}

// IsFromBot 判断消息是否由机器人自己发出。
//
// 与 IsSelf 字段区分：字段是「入库来源标记」，这里是「按账号判断」。
// 两条路都要有 —— codec 解码的入站消息不一定带 IsSelf，
// 而重启恢复的消息库里 IsSelf 才是唯一可靠的依据。
func (m *Message) IsFromBot(botIDs map[string]struct{}) bool {
	if m.IsSelf {
		return true
	}
	_, ok := botIDs[m.UserID]
	return ok
}

// Scope 返回聊天类型。
func (m *Message) Scope() string {
	if m.IsGroup {
		return ScopeGroup
	}
	return ScopePrivate
}

// TargetID 返回群号或用户 ID。
func (m *Message) TargetID() string {
	if m.IsGroup {
		return m.GroupID
	}
	return m.UserID
}

// 聊天类型。
const (
	ScopeGroup   = "group"
	ScopePrivate = "private"
)

// WebUI 网页对话窗口是一个虚拟平台，与 QQ 适配器平行。
//
// 对应 MaiBot 的 WEBUI_CHAT_PLATFORM = "webui"：网页用户以人类身份
// 与机器人私聊，消息走同一条 Dispatch 链路，机器人的回复只回到网页，
// 不发往任何真实平台。单密码登录没有多用户，所以路由位是固定的。
const (
	PlatformWebUI = "webui"

	// WebUIAccountID 是对话窗口里「机器人账号」这一位的固定值。
	WebUIAccountID = "webui-bot"
	// WebUITargetID 是对话里「网页用户」这一位的固定值。
	// 昵称可变不影响会话 key —— 昵称只是消息上的 UserName。
	WebUITargetID = "webui-user"
)

// WebUISessionKey 返回网页对话窗口的会话 key。
func WebUISessionKey() SessionKey {
	return SessionKey{
		Platform:  PlatformWebUI,
		AccountID: WebUIAccountID,
		Scope:     ScopePrivate,
		TargetID:  WebUITargetID,
	}
}

// SessionKey 唯一标识一个聊天流。
// 对应 MaiBot 的 platform + account + target 三段路由。
type SessionKey struct {
	Platform  string
	AccountID string
	Scope     string
	TargetID  string
}

// ID 算出稳定的 session_id。
// 与 MaiBot 的 SessionUtils.calculate_session_id 语义一致：同样输入必得同样 ID。
func (k SessionKey) ID() string {
	raw := fmt.Sprintf("%s:%s:%s:%s", k.Platform, k.AccountID, k.Scope, k.TargetID)
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:16])
}

func (k SessionKey) String() string {
	return fmt.Sprintf("%s/%s/%s/%s", k.Platform, k.AccountID, k.Scope, k.TargetID)
}

// Session 是一条聊天流的元信息。
type Session struct {
	ID           string
	Platform     string
	AccountID    string
	Scope        string
	TargetID     string
	DisplayName  string
	MessageCount int
	LastActiveAt time.Time
	CreatedAt    time.Time
}

// Person 是一个用户。
type Person struct {
	Platform   string
	UserID     string
	Nickname   string
	CardName   string
	FirstSeen  time.Time
	LastSeenAt time.Time
}

// DisplayName 返回优先展示的名字。
func (p *Person) DisplayName() string {
	if p.CardName != "" {
		return p.CardName
	}
	if p.Nickname != "" {
		return p.Nickname
	}
	return p.UserID
}

// 角色常量，用于构造 LLM 上下文。
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// ToolCall 是一次工具调用请求。
type ToolCall struct {
	ID   string
	Name string
	// Args 是原始 JSON 字符串，交给工具自己解析。
	Args string
}

// ToolResult 是一次工具执行结果。
type ToolResult struct {
	CallID  string
	Name    string
	Content string
	// IsError 标记工具执行失败。
	IsError bool
}

// ChatMessage 是喂给 LLM 的一条上下文消息。
type ChatMessage struct {
	Role string `json:"role"`

	// Content 在 assistant 带工具调用时可能为空。
	Content string `json:"content,omitempty"`

	// ToolCalls 仅 assistant 使用。
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`

	// ToolCallID 仅 tool 角色使用。
	ToolCallID string `json:"tool_call_id,omitempty"`

	// Name 仅 tool 角色使用，标记来源工具。
	Name string `json:"name,omitempty"`

	// Images 是该条消息携带的图片 URL，用于多模态。
	Images []string `json:"images,omitempty"`

	// Timestamp 用于在提示词中标注时间。
	Timestamp time.Time `json:"timestamp,omitempty"`
}

// HasToolCalls 判断 assistant 消息是否请求了工具。
func (m ChatMessage) HasToolCalls() bool { return len(m.ToolCalls) > 0 }

// RenderText 返回把工具调用也编码进去的文本形式，用于日志与调试。
func (m ChatMessage) RenderText() string {
	var b strings.Builder
	b.WriteString(m.Content)
	for _, call := range m.ToolCalls {
		fmt.Fprintf(&b, "\n[tool_call %s %s]", call.Name, call.Args)
	}
	return b.String()
}
