package config

import (
	"fmt"
	"strings"
)

// 本文件是 fields.go 用到的类型转换辅助。
//
// 统一原则：类型不匹配就报错，不做「尽量兼容」。
// 前端把数字传成字符串、把布尔传成字符串时，静默接受会让用户
// 以为自己改成功了，而实际值没变 —— 这种问题排查起来最费劲。

func setString(dst *string, raw any, key string) error {
	v, ok := raw.(string)
	if !ok {
		return fmt.Errorf("%s 需要字符串，收到 %T", key, raw)
	}
	*dst = v
	return nil
}

// setInt 接受 JSON 数字（float64）与 Go 整数。
//
// 不接受字符串："40" 与 40 在前端只差一对引号，但含义完全不同 ——
// 前者会把 max_context_size 变成 0（TOML 里是字符串，解析器会拒绝），
// 后者才是用户想要的。
func setInt(dst *int, raw any, key string) error {
	switch v := raw.(type) {
	case float64:
		// JSON 数字没有整数类型，转之前先确认没有小数部分，
		// 否则 3.7 会被静默截成 3。
		if v != float64(int(v)) {
			return fmt.Errorf("%s 需要整数，收到 %v", key, v)
		}
		*dst = int(v)
		return nil
	case int:
		*dst = v
		return nil
	case int64:
		*dst = int(v)
		return nil
	}
	return fmt.Errorf("%s 需要整数，收到 %T", key, raw)
}

func setFloat(dst *float64, raw any, key string) error {
	switch v := raw.(type) {
	case float64:
		*dst = v
		return nil
	case int:
		*dst = float64(v)
		return nil
	case int64:
		*dst = float64(v)
		return nil
	}
	return fmt.Errorf("%s 需要数字，收到 %T", key, raw)
}

func setBool(dst *bool, raw any, key string) error {
	v, ok := raw.(bool)
	if !ok {
		return fmt.Errorf("%s 需要布尔值，收到 %T", key, raw)
	}
	*dst = v
	return nil
}

// setStringList 接受字符串数组。
//
// 空白项会被剔除：多行文本框里用户很容易多敲一个空行，
// 而空串在屏蔽词里会匹配「所有消息」——那是灾难性的。
func setStringList(dst *[]string, raw any, key string) error {
	arr, ok := raw.([]any)
	if !ok {
		// 也接受 []string（测试与非 HTTP 调用方会直接传）
		if ss, ok := raw.([]string); ok {
			*dst = trimEmpty(ss)
			return nil
		}
		return fmt.Errorf("%s 需要字符串数组，收到 %T", key, raw)
	}
	out := make([]string, 0, len(arr))
	for i, item := range arr {
		s, ok := item.(string)
		if !ok {
			return fmt.Errorf("%s[%d] 需要字符串，收到 %T", key, i, item)
		}
		out = append(out, s)
	}
	*dst = trimEmpty(out)
	return nil
}

func trimEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	return out
}
