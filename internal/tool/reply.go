package tool

import (
	"context"
	"strings"
)

// ReplyTool 是 Planner 唯一的可见输出通道。
//
// 对应 MaiBot builtin_tool/reply.py —— 整个系统最核心的工具。
//
// 参数设计里最关键的是 reply_reference：
// Planner 不直接写回复正文，而是描述「这次想表达什么方向、基于什么上下文」，
// 交给 Replyer 用另一套提示词（带人格与表达风格）生成真正的措辞。
// 这样做的好处是思考逻辑和说话风格彻底解耦，可以各自配模型、各自调优。
type ReplyTool struct {
	// enableReplyAt 对应 chat.enable_reply_at。
	//
	// 存取值函数而不是 bool：schema 是每次请求时现算的，
	// 存 bool 就等于「启动时读一次，之后改配置没反应」——
	// 而这个开关在 WebUI 上标的是「即时生效」。
	enableReplyAt func() bool
}

// NewReplyTool 用固定值创建（测试与不需要热更新的场景）。
func NewReplyTool(enableReplyAt bool) *ReplyTool {
	return &ReplyTool{enableReplyAt: func() bool { return enableReplyAt }}
}

// NewReplyToolFunc 每次取 schema 时重新求值，用于跟随配置热更新。
func NewReplyToolFunc(fn func() bool) *ReplyTool {
	if fn == nil {
		fn = func() bool { return false }
	}
	return &ReplyTool{enableReplyAt: fn}
}

// allowsReplyAt 把 nil 也当成关闭，避免直接调空函数指针崩掉。
func (t *ReplyTool) allowsReplyAt() bool {
	if t.enableReplyAt == nil {
		return false
	}
	return t.enableReplyAt()
}

func (t *ReplyTool) Name() string { return "reply" }

func (t *ReplyTool) Description() string {
	return "根据当前思考生成并发送可见回复。发言必须通过本工具，否则用户无法看见。" +
		"reply_reference 用于提供本次回复的上下文关键信息与方向指引，它不是回复正文。"
}

func (t *ReplyTool) Schema() map[string]any {
	props := map[string]any{
		"msg_id": map[string]any{
			"type":        "string",
			"description": "要回复的消息 msg_id。",
		},
		"reply_reference": map[string]any{
			"type":      "string",
			"minLength": 1,
			"description": "提供本次回复的上下文关键信息与回复方向，" +
				"包括当前聊天状态、人物关系、事实信息、回忆信息；不是回复正文。",
		},
		"reply_style": map[string]any{
			"type":        "string",
			"description": "可选。控制本次回复的篇幅和表达方式。",
			"enum":        []string{"简短表达", "正常回复", "长回复"},
		},
		"attach_pic": map[string]any{
			"type":        "array",
			"description": "可选。随本次回复附加一张或多张上下文图片。",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"msg_id": map[string]any{
						"type":        "string",
						"description": "图片所在的消息编号。",
					},
					"index": map[string]any{
						"type":        "integer",
						"description": "同一消息中的图片序号，从 0 开始。",
					},
				},
			},
		},
	}

	required := []string{"msg_id", "reply_reference"}

	// 只有在允许时 @ 时才暴露这个参数。
	if t.allowsReplyAt() {
		props["attach_at"] = map[string]any{
			"type":        "array",
			"description": "可选。随本次回复 at 一个或多个目标消息的发送者，填写目标 msg_id。",
			"items":       map[string]any{"type": "string"},
		}
	}

	return objectSchema(props, required)
}

func (t *ReplyTool) Visible(Availability) bool { return true }

// replyArgs 是 reply 工具的参数。
type replyArgs struct {
	MsgID          string `json:"msg_id"`
	ReplyReference string `json:"reply_reference"`
	ReplyStyle     string `json:"reply_style"`
	AttachPics     []struct {
		MsgID string `json:"msg_id"`
		Index int    `json:"index"`
	} `json:"attach_pic"`
	AttachAt []string `json:"attach_at"`
}

func (t *ReplyTool) Execute(ctx context.Context, inv Invocation, rt Runtime) (Result, error) {
	var args replyArgs
	if err := inv.Decode(&args); err != nil {
		return ErrorResult("参数解析失败: %v", err), nil
	}

	reference := strings.TrimSpace(args.Reference())
	if reference == "" {
		// 兜底：用 Planner 的思考内容。总比拒绝执行好。
		reference = strings.TrimSpace(inv.Reasoning)
	}
	if reference == "" {
		return ErrorResult("reply_reference 不能为空"), nil
	}

	req := ReplyRequest{
		MsgID:     strings.TrimSpace(args.MsgID),
		Reference: reference,
		Style:     strings.TrimSpace(args.ReplyStyle),
		AttachAt:  args.AttachAt,
		TurnID:    inv.TurnID,
	}
	for _, p := range args.AttachPics {
		req.AttachPics = append(req.AttachPics, AttachPic{MsgID: p.MsgID, Index: p.Index})
	}

	outcome, err := rt.Reply(req)
	if err != nil {
		return ErrorResult("发送失败: %v", err), nil
	}

	rt.RememberReply(outcome.Text, outcome.MessageIDs, outcome.RefIdxs)

	if len(outcome.MessageIDs) == 0 {
		return TextResult("已发送回复。"), nil
	}
	return TextResult(printf("已发送回复。消息编号：%s", strings.Join(outcome.MessageIDs, ", "))), nil
}

// Reference 返回非空的 reply_reference。
func (a replyArgs) Reference() string { return a.ReplyReference }
