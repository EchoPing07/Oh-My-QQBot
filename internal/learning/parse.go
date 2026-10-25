package learning

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ExpressionItem 是一条学到的表达方式。
//
// 字段与参考实现 parse_expression_response 返回的 (situation, style, source_id) 三元组对应。
type ExpressionItem struct {
	Situation string `json:"situation"`
	Style     string `json:"style"`
	SourceID  string `json:"source_id"`
}

// JargonItem 是一条候选黑话。
//
// 字段与参考实现 parse_jargon_response 返回的 (content, source_id) 二元组对应。
type JargonItem struct {
	Content  string `json:"content"`
	SourceID string `json:"source_id"`
}

// ParseExpressions 解析模型返回的表达方式 JSON 数组。
//
// 模型经常把数组包在 ```json 代码块里，或者前后加一句「好的，结果如下」。
// 参考实现用 json_repair 兜底（依赖库里没有第三方包），这里退一步：
// 剥掉围栏 → 取第一个 '[' 到最后一个 ']' → 严格 json.Unmarshal。
// 解析不出来就报错，不静默返回空 —— 空数组是模型的合法输出，
// 而"解析失败"和"没学到东西"必须能区分，否则日志里永远看不出模型是不是在乱说。
func ParseExpressions(raw string) ([]ExpressionItem, error) {
	items, err := parseArray[ExpressionItem](raw)
	if err != nil {
		return nil, fmt.Errorf("解析表达方式失败: %w", err)
	}

	// 参考实现 parse_expression_response 会丢掉字段不全的条目，这里保持同样的宽容度：
	// 模型偶尔会混进一两条残项，不该让整批学习失败。
	out := make([]ExpressionItem, 0, len(items))
	for _, it := range items {
		it.Situation = strings.TrimSpace(it.Situation)
		it.Style = normalizeStyle(it.Style)
		it.SourceID = strings.TrimSpace(it.SourceID)
		if it.Situation == "" || it.Style == "" {
			continue
		}
		out = append(out, it)
	}
	return out, nil
}

// ParseJargons 解析模型返回的黑话 JSON 数组。
func ParseJargons(raw string) ([]JargonItem, error) {
	items, err := parseArray[JargonItem](raw)
	if err != nil {
		return nil, fmt.Errorf("解析黑话失败: %w", err)
	}

	out := make([]JargonItem, 0, len(items))
	for _, it := range items {
		it.Content = strings.TrimSpace(it.Content)
		it.SourceID = strings.TrimSpace(it.SourceID)
		if it.Content == "" {
			continue
		}
		out = append(out, it)
	}
	return out, nil
}

// parseArray 是两种解析共用的骨架：剥壳、截取数组、反序列化。
func parseArray[T any](raw string) ([]T, error) {
	cleaned := stripCodeFence(raw)
	if strings.TrimSpace(cleaned) == "" {
		return nil, fmt.Errorf("模型响应为空")
	}

	start := strings.Index(cleaned, "[")
	end := strings.LastIndex(cleaned, "]")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("响应中没有 JSON 数组: %s", preview(cleaned))
	}
	body := cleaned[start : end+1]

	var out []T
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		return nil, fmt.Errorf("JSON 无效: %w（原文: %s）", err, preview(body))
	}
	return out, nil
}

// stripCodeFence 去掉 markdown 代码块围栏。
//
// 只处理最常见两种形态：```json ... ``` 整块包裹，以及 ``` 后面直接换行。
// 不做更花哨的猜测 —— 截取 [ ... ] 那一步已经能把绝大多数废话刨掉。
func stripCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	// 拆掉开头那一行（``` 或 ```json）。
	if nl := strings.IndexByte(s, '\n'); nl >= 0 {
		s = s[nl+1:]
	} else {
		s = strings.TrimPrefix(s, "```")
	}
	if idx := strings.LastIndex(s, "```"); idx >= 0 {
		s = s[:idx]
	}
	return strings.TrimSpace(s)
}

// normalizeStyle 去掉模型从提示词示例里带出来的「使用」前缀。
//
// 逐字对齐参考实现 expression_style_utils.normalize_expression_style_for_learning：
// 提示词示例写的是 "使用 我嘞个xxxx"，模型会照抄这个前缀，
// 但入库的 style 应该是"我嘞个xxxx"，否则注入给 Replyer 时会读成"使用 使用 ..."。
func normalizeStyle(style string) string {
	s := strings.TrimSpace(style)
	if strings.HasPrefix(s, "使用") {
		return strings.TrimSpace(strings.TrimPrefix(s, "使用"))
	}
	return s
}

// preview 截断超长响应，避免把整段模型输出刷进日志和错误信息。
func preview(s string) string {
	const limit = 200
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "..."
}
