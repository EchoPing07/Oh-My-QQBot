package tool

import (
	"context"
)

// 内置工具的通用 schema 片段。

func objectSchema(properties map[string]any, required []string) map[string]any {
	if properties == nil {
		properties = map[string]any{}
	}
	if required == nil {
		required = []string{}
	}
	return map[string]any{
		"type":       "object",
		"properties": properties,
		"required":   required,
	}
}

// WaitTool 让 Planner 暂停一段时间再判断。
//
// 对应 MaiBot builtin_tool/wait.py。
// 这是「像人」的关键工具之一：不是每条消息都必须回，可以先看看情况。
type WaitTool struct{}

// NewWaitTool 创建 wait 工具。
//
// 连续 wait 的次数上限**不在这里判断**：它由 runtime 每次执行后
// 读配置比对（agent/runtime.go），这样改配置立刻生效。
// 早先这里存了一份 MaxConsecutive 副本，但从来没有人读它 ——
// 那种字段比没有更糟：后来人会以为改它就能调上限。
func NewWaitTool() *WaitTool { return &WaitTool{} }

func (t *WaitTool) Name() string { return "wait" }

func (t *WaitTool) Description() string {
	return "暂停当前对话并等待一段时间；等待期间新消息可提前结束等待。"
}

func (t *WaitTool) Schema() map[string]any {
	return objectSchema(map[string]any{
		"seconds": map[string]any{
			"type":        "integer",
			"description": "等待秒数。",
		},
	}, []string{"seconds"})
}

func (t *WaitTool) Visible(Availability) bool { return true }

func (t *WaitTool) Execute(ctx context.Context, inv Invocation, rt Runtime) (Result, error) {
	var args struct {
		Seconds int `json:"seconds"`
	}
	if err := inv.Decode(&args); err != nil {
		return ErrorResult("参数解析失败: %v", err), nil
	}
	if args.Seconds < 0 {
		args.Seconds = 0
	}

	if err := rt.Wait(float64(args.Seconds), inv.Reasoning); err != nil {
		// 连续 wait 达到上限：视为多次等待后仍无后续，进入休息。
		return TextResult("连续 wait 已达到上限，本次不再进入等待；视为多次等待后仍无后续，当前对话进入休息。"), nil
	}

	return TextResult(printf(
		"当前对话循环进入等待状态，最多等待 %d 秒；新消息可提前结束等待并继续思考。",
		args.Seconds,
	)), nil
}

// StopTool 显式结束本轮思考。
//
// 对应 MaiBot builtin_tool/stop.py。
// 与「不调用任何工具」效果一致，但语义更明确，模型更愿意用。
type StopTool struct{}

// NewStopTool 创建 stop 工具。
func NewStopTool() *StopTool { return &StopTool{} }

func (t *StopTool) Name() string { return "stop" }

func (t *StopTool) Description() string {
	return "结束本轮思考并等待新消息，与不调用任何工具时一致；不发送消息，也不设置定时等待。无需继续行动时调用。"
}

func (t *StopTool) Schema() map[string]any { return objectSchema(nil, nil) }

func (t *StopTool) Visible(Availability) bool { return true }

func (t *StopTool) Execute(_ context.Context, inv Invocation, rt Runtime) (Result, error) {
	rt.Stop("tool_stop_after_execution")
	return TextResult("已结束本轮思考，等待新消息。"), nil
}

// printf 是 fmt.Sprintf 的短别名，减少 import。
func printf(format string, args ...any) string {
	return sprintfImpl(format, args...)
}
