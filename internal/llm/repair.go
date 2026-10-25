package llm

import (
	"encoding/json"
	"regexp"
	"strings"
)

// 工具参数修复。
//
// 背景：不少模型在工具调用里返回的 arguments 不是严格 JSON ——
// 常见情况包括尾逗号、单引号、被包成字符串、带 markdown 代码块、前后有解释文字。
// MaiBot 的 ToolArgumentParseMode 提供了 strict / repair / double_decode 三种策略，
// 这里默认走 repair（先试严格解析，失败再修），因为实际效果最稳。

var (
	// 匹配 ```json ... ``` 或 ``` ... ```
	codeFencePattern = regexp.MustCompile("(?s)^```(?:json|JSON)?\\s*(.*?)\\s*```$")
	// 匹配尾逗号：,} 或 ,]
	trailingCommaPattern = regexp.MustCompile(`,(\s*[}\]])`)
	// 匹配未加引号的键名。
	unquotedKeyPattern = regexp.MustCompile(`([{,])\s*([A-Za-z_][A-Za-z0-9_]*)\s*:`)
)

// RepairArguments 尽力把模型返回的参数修正为合法 JSON。
//
// 修复顺序：
//  1. 去空白，空串直接返回 "{}"
//  2. 剥掉 markdown 代码围栏
//  3. 严格解析，成功即返回压缩后的 JSON
//  4. 去尾逗号后重试
//  5. 给键名加引号后重试
//  6. 截取首个 { 到末个 } 之间的部分重试
//  7. 全部失败则返回 "{}"，让工具走默认值
//
// 注意：不吞错误。调用方可以通过 ValidateArguments 判断返回值是否可信。
func RepairArguments(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "{}"
	}

	// 去掉代码围栏。
	if m := codeFencePattern.FindStringSubmatch(s); m != nil {
		s = strings.TrimSpace(m[1])
	}

	// 已经是合法 JSON 就直接压缩返回。
	if compact, ok := compactJSON(s); ok {
		return compact
	}

	// 去尾逗号。
	if fixed, ok := compactJSON(trailingCommaPattern.ReplaceAllString(s, "$1")); ok {
		return fixed
	}

	// 给裸键名补引号。
	quoted := unquotedKeyPattern.ReplaceAllString(s, `$1"$2":`)
	if fixed, ok := compactJSON(quoted); ok {
		return fixed
	}

	// 截取对象部分，去掉前后的解释性文字。
	if obj := extractJSONObject(s); obj != "" {
		if compact, ok := compactJSON(obj); ok {
			return compact
		}
		if fixed, ok := compactJSON(trailingCommaPattern.ReplaceAllString(obj, "$1")); ok {
			return fixed
		}
	}

	// 单引号包裹的整体是 JSON 字符串，尝试再解一层。
	if unquoted, ok := unwrapJSONString(s); ok {
		return RepairArguments(unquoted)
	}

	return "{}"
}

// ValidateArguments 判断参数是否为合法 JSON 对象。
func ValidateArguments(raw string) bool {
	var v map[string]any
	return json.Unmarshal([]byte(raw), &v) == nil
}

func compactJSON(s string) (string, bool) {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return "", false
	}
	// 只接受对象或数组，标量没有意义。
	switch v.(type) {
	case map[string]any, []any:
	default:
		return "", false
	}
	out, err := json.Marshal(v)
	if err != nil {
		return "", false
	}
	return string(out), true
}

// extractJSONObject 截取第一层花括号包住的内容。
func extractJSONObject(s string) string {
	start := strings.Index(s, "{")
	if start < 0 {
		// 也可能是数组。
		start = strings.Index(s, "[")
		if start < 0 {
			return ""
		}
		end := strings.LastIndex(s, "]")
		if end <= start {
			return ""
		}
		return s[start : end+1]
	}
	end := strings.LastIndex(s, "}")
	if end <= start {
		return ""
	}
	return s[start : end+1]
}

// unwrapJSONString 处理被整体包成字符串的参数。
func unwrapJSONString(s string) (string, bool) {
	if !strings.HasPrefix(s, `"`) || !strings.HasSuffix(s, `"`) {
		return "", false
	}
	var str string
	if err := json.Unmarshal([]byte(s), &str); err != nil {
		return "", false
	}
	return str, true
}
