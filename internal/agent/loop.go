package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/gate"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/llm"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/logx"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/store"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/tool"
)

// DefaultMaxInternalRounds 是一轮思考内最多的工具循环轮次。
// 对应 MaiBot runtime.py 的 MAX_INTERNAL_ROUNDS = 16。
const DefaultMaxInternalRounds = 16

// Loop 是 Planner 的多轮工具调用循环。
//
// 对应 MaiBot 的 reasoning_engine.py。
//
// 一轮内部循环 = 请求模型 → 解析工具调用 → 执行工具 → 把结果写回 → 再请求，
// 直到模型不再调用工具、或调用了 reply/stop/wait、或达到轮次上限。
type Loop struct {
	cfg     *config.Store
	llm     *llm.Client
	prompts *PromptBuilder
	store   *store.DB
	log     *logx.Logger
}

// NewLoop 创建 Planner 循环。
//
// store 可以为 nil（测试与工具场景），此时不写推理记录。
func NewLoop(cfg *config.Store, client *llm.Client, prompts *PromptBuilder, db *store.DB) *Loop {
	return &Loop{
		cfg:     cfg,
		store:   db,
		llm:     client,
		prompts: prompts,
		log:     logx.Get("agent.loop"),
	}
}

// CycleEnd 描述一轮循环是怎么结束的。
//
// 这个值非常关键：空闲退避靠它累计。
// 如果漏了它，退避机制永远不生效。
type CycleEnd struct {
	// Reason 是结束原因，取值见下。
	Reason string
	// Text 是回复的文本（如果有）。
	Text string
}

// 结束原因常量。前四个属于「空闲」，会累计退避次数。
const (
	// CycleEndNoTool 是 Planner 没调工具直接结束。
	CycleEndNoTool = "planner_no_tool_end"
	// CycleEndWaitRest 是 Planner 决定休息。
	CycleEndWaitRest = "planner_wait_rest"
	// CycleEndWaitTool 是调用了 wait 工具。
	CycleEndWaitTool = "tool_pause:wait"
	// CycleEndStopTool 是调用了 stop 工具。
	CycleEndStopTool = "tool_stop_after_execution"
	// CycleEndReplied 是成功回复。
	CycleEndReplied = "reply"
	// CycleEndInterrupted 是被新消息打断。
	CycleEndInterrupted = "planner_interrupted"
	// CycleEndMaxRounds 是达到最大轮次。
	CycleEndMaxRounds = "max_rounds"
	// CycleEndError 是出错。
	CycleEndError = "error"
)

// TurnInput 是一轮思考的输入。
type TurnInput struct {
	// SessionID 是聊天流。
	SessionID string
	// IsGroup 表示群聊。
	IsGroup bool
	// GroupID / UserID 用于工具上下文。
	GroupID string
	UserID  string
	// Messages 是触发本次思考的消息批次。
	Messages []*domain.Message
	// History 是更早的上下文。
	History []*domain.Message
	// BotReplies 是机器人自己说过的话。
	BotReplies []BotReply
	// Now 是当前时间。
	Now time.Time
}

// TurnResult 是一轮思考的结果。
type TurnResult struct {
	End CycleEnd
	// ReplyText 是生成并已发送的回复。
	ReplyText string
	// Rounds 是实际执行的内部轮次。
	Rounds int
	// LastReasoning 是最后一轮模型的思考内容。
	LastReasoning string
	// Interrupted 表示被新消息打断。
	Interrupted bool
}

// State 是单轮思考的可变状态。
type State struct {
	// maxRounds 是内部轮次上限。
	// 与 MaiBot 的 MAX_INTERNAL_ROUNDS 对齐（runtime.py:85 = 16）。
	maxRounds int

	consecutiveWaits int
	stopped          bool
	stopReason       string
	waitRequested    bool
	waitSeconds      float64
	waitReason       string

	replyTool *tool.ReplyTool
	registry  *tool.Registry
	avail     tool.Availability
	botName   string
}

// RunTurn 执行一轮完整的思考。
//
// 返回的 TurnResult.End.Reason 会被上层用于累计空闲退避。
func (l *Loop) RunTurn(ctx context.Context, in TurnInput, rt tool.Runtime, st *State) (TurnResult, error) {
	res := TurnResult{}

	registry := st.registry
	if registry == nil {
		return res, fmt.Errorf("工具注册表未初始化")
	}

	// 本轮所有内部循环共用一个 turn_id，WebUI 据此把多轮串成一次思考。
	turnID := newTurnID(in.SessionID)

	// 组装初始消息。
	//
	// 人物画像按本轮 batch 里的人聚合：只注入「正在说话的人」的画像。
	// 注入全员画像是错的做法 —— 群里 40 个人的背景会把上下文淹掉。
	profiles := l.buildProfileInjection(in.Messages, rt)
	messages, err := l.prompts.BuildPlannerMessagesWithProfiles(
		in.IsGroup, in.History, in.BotReplies, in.Now, st.avail, registry, profiles,
	)
	if err != nil {
		return res, fmt.Errorf("组装请求失败: %w", err)
	}

	// 把本批触发消息作为最新内容追加。
	//
	// 带图的消息必须单独成条并挂上 Images：拼成一大段的 RenderMessages
	// 只产出文本，图片会在这里被静默丢掉 —— 用户刚发的图模型看不见，
	// 而历史里的图却看得见，排查起来非常迷惑。
	if len(in.Messages) > 0 {
		visual := l.cfg.Get().ModelIsVisual(config.TaskPlanner)
		var textOnly []*domain.Message
		for _, m := range in.Messages {
			imgs := imagesOf(m)
			if !visual || len(imgs) == 0 {
				textOnly = append(textOnly, m)
				continue
			}
			// 先把攒下的纯文本消息刷出去，保证顺序不乱。
			if len(textOnly) > 0 {
				messages = append(messages, llm.Message{
					Role:    llm.RoleUser,
					Content: RenderMessages(textOnly),
				})
				textOnly = nil
			}
			messages = append(messages, llm.Message{
				Role:    llm.RoleUser,
				Content: RenderMessage(m),
				Images:  imgs,
			})
		}
		if len(textOnly) > 0 {
			messages = append(messages, llm.Message{
				Role:    llm.RoleUser,
				Content: RenderMessages(textOnly),
			})
		}
	}

	tools := registry.Definitions(st.avail)
	maxRounds := st.maxRounds
	if maxRounds <= 0 {
		maxRounds = DefaultMaxInternalRounds
	}

	for round := 0; round < maxRounds; round++ {
		res.Rounds = round + 1

		// 被新消息打断。
		if ctx.Err() != nil {
			res.End = CycleEnd{Reason: CycleEndInterrupted}
			res.Interrupted = true
			return res, nil
		}

		req := &llm.Request{
			Messages: messages,
			Tools:    tools,
		}
		if len(tools) == 0 {
			req.Tools = nil
		}

		resp, err := l.llm.Generate(ctx, config.TaskPlanner, req)
		// 无论成功失败都记一笔：失败的那次恰恰是最需要回看的。
		roundRecID := l.saveReasoning(ctx, &store.ReasoningRecord{
			SessionID:  in.SessionID,
			TurnID:     turnID,
			Round:      round + 1,
			Kind:       "planner",
			Name:       string(config.TaskPlanner),
			Failed:     err != nil,
			DurationMS: durationOf(resp),
			Request:    buildReasoningRequest(req),
			Response:   buildReasoningResponse(resp) + errSuffix(err),
		})
		if err != nil {
			if ctx.Err() != nil {
				res.End = CycleEnd{Reason: CycleEndInterrupted}
				res.Interrupted = true
				return res, nil
			}
			res.End = CycleEnd{Reason: CycleEndError}
			return res, fmt.Errorf("Planner 请求失败: %w", err)
		}

		// Planner 正文。
		//
		// 对应 MaiBot 的 _get_planner_content：「只提取 Planner 正文，
		// 不将 Provider 原生 reasoning 传给工具和 replyer」。
		//
		// 区别很重要：原生 reasoning 是模型的内部思考（可能几万字符、
		// 英文、自我纠结），拿它当 reply_reference 会让 Replyer 收到一堆噪音。
		// 正文才是模型「打算怎么做」的表达。
		plannerContent := strings.TrimSpace(resp.Content)
		if plannerContent != "" {
			res.LastReasoning = plannerContent
		}

		// 没有工具调用：结束本轮思考。
		if len(resp.ToolCalls) == 0 {
			res.End = CycleEnd{Reason: CycleEndNoTool}
			return res, nil
		}

		// 把 assistant 的工具调用写回历史。
		messages = append(messages, llm.Message{
			Role:      llm.RoleAssistant,
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		})

		l.log.Info("Planner 返回工具调用",
			"round", round+1,
			"count", len(resp.ToolCalls),
			"tools", toolCallNames(resp.ToolCalls),
			"finish", resp.FinishReason,
		)

		// 逐个执行工具。工具之间可能有依赖，所以顺序执行 —— 这与提示词里
		// 「无依赖可并行」的说明不冲突：并行是给模型的建议，我们这里保守串行，
		// 因为 reply 有副作用，乱序执行会导致发送顺序错乱。
		for _, call := range resp.ToolCalls {
			l.log.Info("执行工具", "tool", call.Function.Name, "args", truncateForLog(call.Function.Arguments, 200))
			toolStart := time.Now()

			result, execErr := registry.Execute(ctx, tool.Invocation{
				Name:      call.Function.Name,
				Arguments: []byte(call.Function.Arguments),
				CallID:    call.ID,
				// 只传 Planner 正文，不传 Provider 原生 reasoning（同 MaiBot）。
				Reasoning: plannerContent,
				TurnID:    turnID,
			}, rt)

			// 工具结果写回上下文。
			content := result.Content
			if execErr != nil {
				l.log.Warn("工具执行失败", "tool", call.Function.Name, "err", execErr)
				if content == "" {
					content = "工具执行失败: " + execErr.Error()
				}
			}
			l.log.Info("工具执行完成",
				"tool", call.Function.Name,
				"is_error", result.IsError,
				"content", truncateForLog(content, 200),
			)

			// 记录工具执行事件。
			//
			// 这是原来完全缺失的一环：时间线上只有「Planner 决定调用 reply」，
			// 没有「reply 实际返回了什么」。参考实现的账本里
			// tool.execution 是独立事件（maisaka-monitor-client.ts:211），
			// 工具卡片显示参数、结果摘要、成功/失败、耗时。
			//
			// 关联到所属 Planner round（Parent），这样前端能把工具
			// 挂到正确的轮次下 —— 只靠时间戳邻近在并发时会串轮次。
			l.saveEvent(ctx, &store.ReasoningRecord{
				SessionID:  in.SessionID,
				TurnID:     turnID,
				Round:      round + 1,
				Kind:       "tool",
				Name:       call.Function.Name,
				Failed:     execErr != nil || result.IsError,
				DurationMS: time.Since(toolStart).Milliseconds(),
				Parent:     roundRecID,
				Payload: buildToolPayload(
					call.Function.Arguments, content, result.IsError || execErr != nil),
			})

			messages = append(messages, llm.Message{
				Role:       llm.RoleTool,
				Content:    content,
				ToolCallID: call.ID,
				Name:       call.Function.Name,
			})

			// 处理工具产生的副作用。
			switch call.Function.Name {
			case "reply":
				if replyText, ok := extractReplyText(result); ok {
					res.ReplyText = replyText
					res.End = CycleEnd{Reason: CycleEndReplied, Text: replyText}
					return res, nil
				}

			case "stop":
				st.stopped = true
				st.stopReason = CycleEndStopTool

			case "wait":
				// 连续 wait 次数在 runtime 里维护，这里只看工具是否成功。
				if !result.IsError {
					st.waitRequested = true
					st.consecutiveWaits++
				}
			}
		}

		// stop 优先于 continue。
		if st.stopped {
			res.End = CycleEnd{Reason: st.stopReason}
			return res, nil
		}

		// wait 请求要中断循环，回到 runtime 层等待。
		if st.waitRequested {
			res.End = CycleEnd{Reason: CycleEndWaitTool}
			return res, nil
		}
	}

	res.End = CycleEnd{Reason: CycleEndMaxRounds}
	return res, nil
}

// extractReplyText 从 reply 工具结果里取出发送内容。
func extractReplyText(result tool.Result) (string, bool) {
	if result.IsError {
		return "", false
	}
	if text, ok := result.Metadata["reply_text"]; ok {
		return text, true
	}
	// 没有 metadata 时，工具成功即认为已发送。
	return "", true
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// toolCallNames 提取工具名列表，用于日志。
func toolCallNames(calls []llm.ToolCall) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.Function.Name)
	}
	return out
}

// truncateForLog 截断日志里的长文本，避免刷屏。
func truncateForLog(s string, n int) string {
	// 换行会让单条日志变成多行，先压平。
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// WaitState 返回待处理的 wait 请求。
func (s *State) WaitState() (requested bool, seconds float64, reason string) {
	return s.waitRequested, s.waitSeconds, s.waitReason
}

// ClearWait 清理 wait 状态。
func (s *State) ClearWait() {
	s.waitRequested = false
	s.waitSeconds = 0
	s.waitReason = ""
}

// ResetForNewTurn 为新一轮重置状态。
func (s *State) ResetForNewTurn() {
	s.stopped = false
	s.stopReason = ""
	s.waitRequested = false
	s.waitSeconds = 0
	s.waitReason = ""
}

// ConsecutiveWaits 返回连续 wait 次数。
func (s *State) ConsecutiveWaits() int { return s.consecutiveWaits }

// ResetWaits 清零连续 wait。
func (s *State) ResetWaits() { s.consecutiveWaits = 0 }

// IncrWaits 增加连续 wait 计数。
func (s *State) IncrWaits() { s.consecutiveWaits++ }

// gateInput 从运行时状态构造门控输入。
// 这个函数放在这里是为了让 runtime 不用自己拼装。
func buildGateInput(
	isGroup bool,
	frequency float64,
	pending []*domain.Message,
	history []*domain.Message,
	botReplies []BotReply,
	hasForced bool,
	now time.Time,
	averageInterval, lastExternalAt float64,
) gate.Input {
	return gate.Input{
		PendingCount:      len(pending),
		PendingMessageIDs: messageIDs(pending),
		IsGroup:           isGroup,
		Frequency:         frequency,
		HasForcedTrigger:  hasForced,
		Likelihood:        buildLikelihoodInput(pending, history, botReplies, now),
		AverageInterval:   averageInterval,
		LastExternalAt:    lastExternalAt,
		Now:               now.Sub(time.Unix(0, 0)).Seconds(),
	}
}

func messageIDs(msgs []*domain.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.MessageID)
	}
	return out
}

// buildLikelihoodInput 构造回复概率估算的输入。
func buildLikelihoodInput(
	pending, history []*domain.Message,
	botReplies []BotReply,
	now time.Time,
) gate.LikelihoodInput {
	var (
		mentionBot      bool
		hasQuestionMark bool
		allPlaceholder  = len(pending) > 0
		anyAtOther      bool
	)

	for _, m := range pending {
		if m.IsMentioned || m.IsAt {
			mentionBot = true
		}
		if m.PlainText != "" && (strings.Contains(m.PlainText, "?") || strings.Contains(m.PlainText, "？")) {
			hasQuestionMark = true
		}
		if !domain.IsMediaPlaceholderOnly(m.PlainText) {
			allPlaceholder = false
		}
		if !m.IsAt && strings.Contains(m.PlainText, "@") {
			anyAtOther = true
		}
	}

	// 最近 5 分钟的活跃度。
	const recentWindow = 5 * time.Minute
	var recentTotal, recentSelf int
	var sinceBot float64
	foundBot := false

	for _, m := range history {
		if now.Sub(m.ReceivedAt) <= recentWindow {
			recentTotal++
		}
	}
	for i := len(botReplies) - 1; i >= 0; i-- {
		r := botReplies[i]
		if now.Sub(r.At) <= recentWindow {
			recentTotal++
			recentSelf++
		}
		if !foundBot {
			sinceBot = now.Sub(r.At).Seconds()
			foundBot = true
		}
	}
	if !foundBot {
		sinceBot = 3600.0
	}

	ratio := 0.0
	if recentTotal > 0 {
		ratio = float64(recentSelf) / float64(recentTotal)
	}

	return gate.LikelihoodInput{
		MentionBot:             mentionBot,
		AtOther:                anyAtOther,
		HasQuestionMark:        hasQuestionMark,
		PlaceholderOnly:        allPlaceholder,
		RecentSelfRatio:        ratio,
		RecentMessageCount:     recentTotal,
		SecondsSinceBotMessage: sinceBot,
		PendingCount:           len(pending),
	}
}
