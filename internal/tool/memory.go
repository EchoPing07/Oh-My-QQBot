package tool

import (
	"context"
	"strings"
)

// QueryMemoryTool 检索长期记忆。
//
// 对应 MaiBot builtin_tool/query_memory.py。
//
// schema 与参考实现对齐，但 mode 枚举只有 search / time / hybrid：
// 参考实现还有 episode / aggregate，那两种依赖向量库的「经历」与「综合」检索，
// 本项目用 FTS5，没有等价实现。宁可少一个枚举值，也不接受"传了但静默降级"。
type QueryMemoryTool struct{}

// NewQueryMemoryTool 创建长期记忆检索工具。
func NewQueryMemoryTool() *QueryMemoryTool { return &QueryMemoryTool{} }

func (t *QueryMemoryTool) Name() string { return "query_memory" }

func (t *QueryMemoryTool) Description() string {
	return "检索长期记忆（之前的聊天回想）。只在回复明显依赖过去对话、共同经历、约定或长期偏好时使用。"
}

func (t *QueryMemoryTool) Schema() map[string]any {
	return objectSchema(map[string]any{
		"query": map[string]any{
			"type":        "string",
			"description": "关键词或问题；mode=time 时可省略。",
		},
		"mode": map[string]any{
			"type":        "string",
			"description": "检索模式：search 按关键词检索；time 按时间范围检索；hybrid 同时使用两者。",
			"enum":        []string{"search", "time", "hybrid"},
			"default":     "search",
		},
		"limit": map[string]any{
			"type":        "integer",
			"description": "返回条数，默认 5。",
		},
		"person_name": map[string]any{
			"type":        "string",
			"description": "人物名，用于定向过滤。",
		},
		"time_start": map[string]any{
			"type":        "string",
			"description": "起始时间，格式 YYYY/MM/DD 或 YYYY/MM/DD HH:mm。",
		},
		"time_end": map[string]any{
			"type":        "string",
			"description": "结束时间，格式 YYYY/MM/DD 或 YYYY/MM/DD HH:mm。",
		},
	}, nil)
}

func (t *QueryMemoryTool) Visible(Availability) bool { return true }

func (t *QueryMemoryTool) Execute(_ context.Context, inv Invocation, rt Runtime) (Result, error) {
	var args struct {
		Query      string `json:"query"`
		Mode       string `json:"mode"`
		Limit      int    `json:"limit"`
		PersonName string `json:"person_name"`
		TimeStart  string `json:"time_start"`
		TimeEnd    string `json:"time_end"`
	}
	if err := inv.Decode(&args); err != nil {
		return ErrorResult("参数解析失败: %v", err), nil
	}

	mode := strings.ToLower(strings.TrimSpace(args.Mode))
	if mode == "" {
		mode = "search"
	}
	switch mode {
	case "search", "time", "hybrid":
	default:
		return ErrorResult("不支持的检索模式：%s。可选值：search / time / hybrid。", mode), nil
	}

	// 时间解析放在工具层：模型给的是人类可读日期，runtime 只认 Unix 秒。
	start, err := parseQueryTime(args.TimeStart, false)
	if err != nil {
		return ErrorResult("time_start 格式无效：%v", err), nil
	}
	end, err := parseQueryTime(args.TimeEnd, true)
	if err != nil {
		return ErrorResult("time_end 格式无效：%v", err), nil
	}

	query := strings.TrimSpace(args.Query)
	// 与参考实现一致的校验：既没关键词又没时间范围时直接拒绝，
	// 不要退化成"查全库"——那会把无关记忆灌进上下文。
	if query == "" && start == 0 && end == 0 {
		return ErrorResult("query_memory 需要提供 query，或至少提供 time_start / time_end 中的一个。"), nil
	}
	if mode == "time" && start == 0 && end == 0 {
		return ErrorResult("mode=time 时必须提供 time_start 或 time_end。"), nil
	}

	result, err := rt.SearchMemory(MemoryQuery{
		Query:      query,
		Mode:       mode,
		TimeStart:  start,
		TimeEnd:    end,
		PersonName: strings.TrimSpace(args.PersonName),
		Limit:      args.Limit,
	})
	if err != nil {
		return ErrorResult("长期记忆检索失败：%v", err), nil
	}

	if len(result.Hits) == 0 {
		if result.Fallback {
			return TextResult("未找到匹配的长期记忆（已尝试降级检索）。"), nil
		}
		return TextResult("未找到匹配的长期记忆。"), nil
	}

	return TextResult(FormatMemoryHits(result)), nil
}

// FormatMemoryHits 把检索结果排成给模型看的文本。
func FormatMemoryHits(result MemoryResult) string {
	var b strings.Builder
	b.WriteString("【长期记忆-内部参考】\n")
	b.WriteString("以下内容来自更早的聊天记录，仅供理解背景，不要向用户逐字复述。\n")

	count := len(result.Hits)
	b.WriteString(printf("命中：%d 条", count))
	if result.Mode != "" {
		b.WriteString(printf("（模式：%s）", result.Mode))
	}
	b.WriteString("\n")

	for i, hit := range result.Hits {
		b.WriteString(printf("%d. ", i+1))
		if hit.TimeRange != "" {
			b.WriteString(printf("（%s）", hit.TimeRange))
		}
		b.WriteString(collapseWhitespace(hit.Summary))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// collapseWhitespace 把连续空白压成单个空格，避免摘要里的换行撑爆提示词。
func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
