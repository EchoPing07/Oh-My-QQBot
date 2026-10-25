package tool

import (
	"context"
	"strings"
)

// SendEmojiTool 让 Planner 主动发表情包。
//
// 对应 MaiBot builtin_tool/send_emoji.py。
//
// 与参考实现的关键差异（必须记下来，否则后来人会以为是漏做）：
// 参考实现会先用 VLM 把候选表情包拼成一张图、让子代理看图选序号。
// 本项目没有子代理（AGENTS.md 明确不做），也没有稳定可用的视觉 Planner，
// 所以改成「Planner 填情绪描述 → runtime 按描述检索 → 选不到就跳过」。
// 代价是选图不如看图准确；收益是不引入拼图、VLM、子代理三条链路。
type SendEmojiTool struct{}

// NewSendEmojiTool 创建发表情包工具。
func NewSendEmojiTool() *SendEmojiTool { return &SendEmojiTool{} }

func (t *SendEmojiTool) Name() string { return "send_emoji" }

func (t *SendEmojiTool) Description() string {
	return "发送一个表情包来表达情绪。需要用情绪词描述想要的感觉，例如「无语」「开心」「嘲讽」。"
}

func (t *SendEmojiTool) Schema() map[string]any {
	return objectSchema(map[string]any{
		"emotion": map[string]any{
			"type":        "string",
			"description": "想要表达的情绪或场景，用一两个词描述，例如「无语」「乐」「嘲讽」。",
		},
	}, []string{"emotion"})
}

func (t *SendEmojiTool) Visible(av Availability) bool { return true }

func (t *SendEmojiTool) Execute(_ context.Context, inv Invocation, rt Runtime) (Result, error) {
	var args struct {
		Emotion string `json:"emotion"`
	}
	if err := inv.Decode(&args); err != nil {
		return ErrorResult("参数解析失败: %v", err), nil
	}

	emotion := strings.TrimSpace(args.Emotion)
	if emotion == "" {
		return ErrorResult("emotion 不能为空"), nil
	}

	ref, ok := rt.PickEmoji(emotion)
	if !ok {
		// 选不到不是错误：表情包库是空的，或者没有贴合的。
		// 报错会让模型以为自己操作错了，然后重试，白白浪费轮次。
		return TextResult("表情包库中没有贴合这个情绪的表情，本次就不发了。"), nil
	}

	ids, err := rt.SendEmoji(ref)
	if err != nil {
		return ErrorResult("发送表情包失败: %v", err), nil
	}

	desc := ref.Description
	if desc == "" {
		desc = emotion
	}
	if len(ids) == 0 {
		return TextResult(printf("已发送表情包：%s", desc)), nil
	}
	return TextResult(printf("已发送表情包：%s。消息编号：%s", desc, strings.Join(ids, ", "))), nil
}

// ShowEmojiListTool 让 Planner 查看表情包库的可用情绪标签。
//
// 对应 MaiBot builtin_tool/show_emoji_list.py。
//
// 同样因为不做子代理与拼图，这里退化成「列出文字标签」。
// 参考实现返回的是图片拼图，模型能直接看图选；这里模型只能看到文字，
// 所以这个工具的价值是「知道库里有什么情绪可选」，而不是「挑具体某一张」。
type ShowEmojiListTool struct{}

// NewShowEmojiListTool 创建查看表情包列表工具。
func NewShowEmojiListTool() *ShowEmojiListTool { return &ShowEmojiListTool{} }

func (t *ShowEmojiListTool) Name() string { return "show_emoji_list" }

func (t *ShowEmojiListTool) Description() string {
	return "查看表情包库里有哪些情绪标签可用。只会返回文字标签，不会发送任何消息。"
}

func (t *ShowEmojiListTool) Schema() map[string]any { return objectSchema(nil, nil) }

func (t *ShowEmojiListTool) Visible(av Availability) bool { return true }

func (t *ShowEmojiListTool) Execute(_ context.Context, inv Invocation, rt Runtime) (Result, error) {
	// 用一个宽泛的查询把库里近期常用的标签捞出来。
	// 不做「列出全部」是因为库可能有几百个，全塞进上下文纯浪费 token。
	ref, ok := rt.PickEmoji("")
	if !ok {
		return TextResult("表情包库目前是空的，还没有可用的表情。"), nil
	}
	return TextResult(printf("表情包库可用，示例情绪标签：%s。想发表情用 send_emoji 填情绪描述即可。", ref.Description)), nil
}
