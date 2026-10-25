package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/llm"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/store"
)

// 推理记录落库。
//
// 之前 ReasoningRepo 只有 Save/ListBySession，没有任何生产调用点，
// reasoning_records 表恒为 0 行，WebUI 的推理重放自然是空的。
// 这里把每轮 Planner 的请求与响应真正写进去。
//
// 与参考实现的差异：MaiBot 有 1129 行的完整请求快照，我们只存
// 「发出去的消息 + 返回的正文/工具调用」，够重放判断即可。
// 这属于 docs/plan.md 第 7 节偏离清单里的「请求快照：极简」。

// 单条记录的长度上限。
//
// 不设上限的话，一次带满上下文的 Planner 请求能有几百 KB，
// 长跑几天就能把数据库撑到 GB 级，而这个表的用途只是「事后看看模型怎么想的」。
const reasoningFieldLimit = 64 * 1024

// newTurnID 生成一轮思考的编号。
//
// 同一轮内的多次工具循环共用一个 turn_id，WebUI 可以据此把多轮串成一次思考。
func newTurnID(sessionID string) string {
	return fmt.Sprintf("%s-%d", sessionID, time.Now().UnixNano())
}

// saveReasoning 写入一条推理记录，返回它的自增 ID。
//
// 出错只记日志：记录是调试辅助，不该因为写不进去就让回复失败。
// 返回 ID 是为了让工具执行记录能挂到它所属的 Planner round 上（Parent）——
// 时间线里「Planner 想了什么」与「工具实际做了什么」必须能对上，
// 否则只靠时间戳邻近来判断，并发时就会串到别的轮次。
func (l *Loop) saveReasoning(ctx context.Context, rec *store.ReasoningRecord) int64 {
	if l.store == nil || rec == nil {
		return 0
	}
	// 复用调用方的 ctx 可能已被取消（打断），单独给一个短超时更稳。
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()

	// 两个口径都写：created_at 供清理与旧查询，created_at_ms 供时间线排序。
	now := time.Now()
	rec.CreatedAt = now.Unix()
	rec.CreatedAtMS = now.UnixMilli()
	if err := l.store.Reasoning().Save(wctx, rec); err != nil {
		l.log.Warn("写入推理记录失败", "session", rec.SessionID, "round", rec.Round, "err", err)
		return 0
	}
	return rec.ID
}

// saveEvent 记录一个「非模型调用」的时间线事件。
//
// 参考实现的监控账本里有 8 类事件（maisaka-monitor.tsx:2273 的 switch），
// 我们原来只记了 Planner 请求一种，于是界面上只有「模型想了什么」，
// 看不到「工具实际做了什么」「门控为什么放行」「Replyer 生成了什么」。
//
// 这几类统一走这个函数，靠 kind 区分：
//
//	kind=tool    工具执行（name=工具名，ok=是否成功，payload=参数与结果）
//	kind=replyer Replyer 生成（name="replyer"，payload=正文与 token）
//	kind=gate    门控决策（name=动作，payload=输入与结论）
//	kind=trim    上下文裁切（name="context.trimmed"，payload=裁切前后条数）
func (l *Loop) saveEvent(ctx context.Context, rec *store.ReasoningRecord) int64 {
	if rec.Kind == "" {
		return 0
	}
	return l.saveReasoning(ctx, rec)
}

// buildReasoningRequest 把请求序列化成可回看的文本。
func buildReasoningRequest(req *llm.Request) string {
	type toolView struct {
		Name string `json:"name"`
		Desc string `json:"description"`
	}
	payload := struct {
		Messages []llm.Message `json:"messages"`
		Tools    []toolView    `json:"tools,omitempty"`
	}{
		Messages: req.Messages,
	}
	for _, t := range req.Tools {
		payload.Tools = append(payload.Tools, toolView{
			Name: t.Function.Name,
			Desc: t.Function.Description,
		})
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Sprintf("<序列化失败: %v>", err)
	}
	return clipText(string(raw))
}

// buildReasoningResponse 把响应序列化成可回看的文本。
func buildReasoningResponse(resp *llm.Response) string {
	if resp == nil {
		return ""
	}
	payload := struct {
		Content    string         `json:"content"`
		Reasoning  string         `json:"reasoning,omitempty"`
		ToolCalls  []llm.ToolCall `json:"tool_calls,omitempty"`
		Finish     string         `json:"finish_reason"`
		Model      string         `json:"model"`
		DurationMS int64          `json:"duration_ms"`
	}{
		Content:    resp.Content,
		Reasoning:  resp.Reasoning,
		ToolCalls:  resp.ToolCalls,
		Finish:     resp.FinishReason,
		Model:      resp.Model,
		DurationMS: resp.Duration.Milliseconds(),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Sprintf("<序列化失败: %v>", err)
	}
	return clipText(string(raw))
}

// clipText 按字节上限截断，并保证不切断 UTF-8 字符。
func clipText(s string) string {
	if len(s) <= reasoningFieldLimit {
		return s
	}
	// 回退到最近的合法 rune 边界，避免存进非法 UTF-8。
	cut := reasoningFieldLimit
	for cut > 0 && !utf8StartByte(s[cut]) {
		cut--
	}
	return s[:cut] + "…（已截断）"
}

// utf8StartByte 判断一个字节是否是 UTF-8 字符的起始字节。
func utf8StartByte(b byte) bool { return b&0xC0 != 0x80 }

// errSuffix 把错误信息拼进记录，方便回看时知道那次为什么失败。
func errSuffix(err error) string {
	if err == nil {
		return ""
	}
	return "\n\n<调用失败: " + err.Error() + ">"
}

// durationOf 取一次模型调用的耗时。
//
// resp 为 nil（请求失败）时返回 0：那次的时间由调用方的超时/重试逻辑体现，
// 在这里猜一个数字没有意义。
func durationOf(resp *llm.Response) int64 {
	if resp == nil {
		return 0
	}
	return resp.Duration.Milliseconds()
}

// buildToolPayload 把工具调用的参数与结果拼成一段 JSON。
//
// 为什么分开放参数和结果：前端要分别展示（参考实现的
// ToolArgumentBlock 显示参数，PlannerToolResultCard 显示结果摘要），
// 拼成一段纯文本就只能原样贴出来。
func buildToolPayload(rawArgs, resultContent string, failed bool) string {
	payload := struct {
		Args   json.RawMessage `json:"args,omitempty"`
		Result string          `json:"result,omitempty"`
		Failed bool            `json:"failed,omitempty"`
	}{
		Result: clipText(resultContent),
		Failed: failed,
	}
	// 参数本身就是 JSON 文本，合法时原样嵌入（前端拿到的是对象而不是字符串）。
	trimmed := strings.TrimSpace(rawArgs)
	if trimmed != "" && json.Valid([]byte(trimmed)) {
		payload.Args = json.RawMessage(trimmed)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return clipText(string(raw))
}
