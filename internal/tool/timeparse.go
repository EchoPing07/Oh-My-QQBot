package tool

import (
	"fmt"
	"strings"
	"time"
)

// timeLayouts 是 query_memory 允许的时间格式。
//
// 与参考实现对齐：它只接受 YYYY/MM/DD 与 YYYY/MM/DD HH:mm，
// 并且会把 ISO 的 YYYY-MM-DD 归一化成斜杠形式再解析。
// 这里直接把两种都收下，省掉一次归一化。
var timeLayouts = []string{
	"2006/01/02 15:04",
	"2006/01/02",
	"2006-01-02 15:04",
	"2006-01-02",
}

// parseQueryTime 解析模型给的时间串，返回 Unix 秒。空串返回 0。
//
// isEnd 为 true 且只给了日期时，取当天 23:59:59 ——
// 否则「查 10 月 21 日」会把当天所有记忆都排除在外，这是个很容易忽略的坑。
func parseQueryTime(raw string, isEnd bool) (int64, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, nil
	}

	for _, layout := range timeLayouts {
		t, err := time.ParseInLocation(layout, s, time.Local)
		if err != nil {
			continue
		}
		if isEnd && !strings.Contains(layout, "15:04") {
			t = t.Add(24*time.Hour - time.Second)
		}
		return t.Unix(), nil
	}
	return 0, fmt.Errorf("%q 不是合法时间，请用 YYYY/MM/DD 或 YYYY/MM/DD HH:mm", raw)
}
