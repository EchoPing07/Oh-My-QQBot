package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/chat"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/llm"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/logx"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/store"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/tool"
)

// Replyer 是两段式的第二段：专门负责「怎么说」。
//
// 对应 MaiBot 的 maisaka_generator_base.py。
//
// Planner 给出的是 reply_reference（想表达什么方向、基于什么上下文），
// Replyer 带着人格与表达风格把它变成真正的话。
// 这样思考逻辑和说话风格彻底解耦，可以各自配模型、各自调优。
type Replyer struct {
	cfg     *config.Store
	llm     *llm.Client
	prompts *PromptBuilder
	log     *logx.Logger
	// db 可以为 nil（测试场景），此时不写时间线事件。
	//
	// 放在 Replyer 而不是 Runtime：Replyer 才知道自己这次用了多久、
	// 返回了多少 token、模型是谁 —— 这些正是参考实现 ReplierResponseCard
	// 显示的内容（maisaka-monitor.tsx:2056）。
	db *store.DB
}

// NewReplyer 创建回复生成器。
func NewReplyer(cfg *config.Store, client *llm.Client, prompts *PromptBuilder, db *store.DB) *Replyer {
	return &Replyer{
		cfg:     cfg,
		llm:     client,
		prompts: prompts,
		log:     logx.Get("agent.replyer"),
		db:      db,
	}
}

// ReplyInput 是一次回复生成的输入。
type ReplyInput struct {
	IsGroup    bool
	History    []*domain.Message
	BotReplies []BotReply
	// SessionID / TurnID 用于把这次生成挂到时间线的正确会话与轮次上。
	SessionID string
	TurnID    string
	// Reference 是 Planner 给出的回复方向指引。
	Reference string
	// Style 是 Planner 请求的篇幅：简短表达 / 正常回复 / 长回复。
	Style string
	// TargetMsgID 是本次要回复的那条消息的 msg_id。
	// 用于在历史里反查出「回复目标块」（参考实现 _build_target_message_block）。
	// 为空时不追加该块。
	TargetMsgID string
	Now         time.Time
	// KeywordReaction 是命中的关键词反应提示，为空表示没命中。
	// 对应参考实现 maisaka_generator_base.py 的 _build_keyword_reaction_prompt。
	KeywordReaction string
	// ExpressionHabits 是学到的表达习惯，注入成一条额外的 user 消息。
	// 对应参考实现的 expression_habits 参数。
	ExpressionHabits string
	// AttachPics / AttachEmoji 是本次回复额外要发的图片与表情包，
	// 只用于告诉 Replyer「系统会另外发这些」，不进正文。
	AttachPics  []tool.AttachPic
	AttachEmoji string
}

// Generate 生成回复文本。
//
// 注意这里用的是 TaskReplyer，不是 TaskPlanner ——
// 两个任务可以绑不同的模型，这是两段式的实际收益。
func (r *Replyer) Generate(ctx context.Context, in ReplyInput) (string, error) {
	messages, _, err := r.prompts.BuildReplyerMessages(ReplyerPromptInput{
		IsGroup:          in.IsGroup,
		History:          in.History,
		BotReplies:       in.BotReplies,
		Reference:        in.Reference,
		Style:            in.Style,
		BaseStyle:        r.cfg.Get().Personality.ReplyStyle,
		Now:              in.Now,
		TargetMsgID:      in.TargetMsgID,
		ExpressionHabits: in.ExpressionHabits,
		KeywordReaction:  in.KeywordReaction,
		AttachPics:       in.AttachPics,
		AttachEmoji:      in.AttachEmoji,
	})
	if err != nil {
		return "", fmt.Errorf("组装 Replyer 请求失败: %w", err)
	}

	resp, err := r.llm.Generate(ctx, config.TaskReplyer, &llm.Request{Messages: messages})
	// 无论成败都记一笔，失败那次最需要回看。
	r.recordReplyer(ctx, in, resp, err)
	if err != nil {
		return "", fmt.Errorf("Replyer 请求失败: %w", err)
	}

	text := chat.CleanReplyText(resp.Content)
	if text == "" {
		return "", fmt.Errorf("Replyer 返回空内容")
	}
	return text, nil
}

// recordReplyer 记录一次 Replyer 生成事件。
//
// 参考实现把 Replyer 的响应作为独立事件展示（ReplierResponseCard，
// maisaka-monitor.tsx:2056）：正文、思考、模型、输入/输出/总 token、耗时、成败。
// 原来我们完全没有这一环 —— 时间线上只能看到 Planner 与工具，
// 看不到「实际说出来的话是怎么生成的」。
func (r *Replyer) recordReplyer(ctx context.Context, in ReplyInput, resp *llm.Response, err error) {
	if r.db == nil {
		return
	}
	payload := struct {
		Content  string `json:"content,omitempty"`
		Thinking string `json:"thinking,omitempty"`
		Model    string `json:"model,omitempty"`
		Prompt   int    `json:"prompt_tokens,omitempty"`
		Complete int    `json:"completion_tokens,omitempty"`
		Total    int    `json:"total_tokens,omitempty"`
		Error    string `json:"error,omitempty"`
	}{}
	var duration int64
	if resp != nil {
		payload.Content = resp.Content
		payload.Thinking = resp.Reasoning
		payload.Model = resp.Model
		payload.Prompt = resp.Usage.PromptTokens
		payload.Complete = resp.Usage.CompletionTokens
		payload.Total = resp.Usage.TotalTokens
		duration = resp.Duration.Milliseconds()
	}
	if err != nil {
		payload.Error = err.Error()
	}
	raw, jsonErr := json.Marshal(payload)
	if jsonErr != nil {
		return
	}

	// 用独立的短超时：调用方的 ctx 可能因为发送失败已被取消。
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()

	now := time.Now()
	rec := &store.ReasoningRecord{
		SessionID:   in.SessionID,
		TurnID:      in.TurnID,
		Kind:        "replyer",
		Name:        string(config.TaskReplyer),
		Failed:      err != nil,
		DurationMS:  duration,
		Payload:     clipText(string(raw)),
		CreatedAt:   now.Unix(),
		CreatedAtMS: now.UnixMilli(),
	}
	if saveErr := r.db.Reasoning().Save(wctx, rec); saveErr != nil {
		r.log.Warn("写入 Replyer 事件失败", "err", saveErr)
	}
}

// insertBeforeLast 已删除。
//
// 它原来被用来把表达习惯与关键词反应插到「倒数第一条之前」。
// 当时末尾只有一条（ReplyerFinalUserMessage），看着没问题；
// 现在末尾后面还跟着篇幅要求，再「插到倒数第一条之前」就会把
// 补充材料塞到末尾块之后 —— 而那格是模型最看重的位置。
// 补充材料的插入位置现在写在 BuildReplyerMessages 里，顺序显式可控。

// mapStyleToPrompt 已删除。
//
// 它原来把 Planner 的篇幅要求拼进 replyer 模板的 {reply_style}，
// 也就是拿到 system 里去。两个问题：
//   1. 覆盖掉了配置里的基础表达风格（system 里本该是 personality.reply_style）
//   2. 权重太弱 —— 拼在 system 里的「这次要简短」压不住模型铺开写的倾向
//
// 现在两件事分开：
//   - 基础风格 → BuildReplyerSystemPrompt 的 {reply_style}（配置值）
//   - 篇幅要求 → prompt.ReplyerStyleHint，一条独立的末尾 user 消息
// 与参考实现 maisaka_generator_base.py:611 _build_requested_reply_style_message 一致。
