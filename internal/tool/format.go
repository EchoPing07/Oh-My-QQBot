package tool

import "fmt"

// sprintfImpl 是统一格式化入口。
func sprintfImpl(format string, args ...any) string { return fmt.Sprintf(format, args...) }
