package gate

import (
	"fmt"
	"math"
)

// sprintf 是 fmt.Sprintf 的短别名，避免每个文件都 import fmt。
func sprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }

// math_max 是 math.Max 的别名，保持与 Python 语义的对应关系易读。
func math_max(a, b float64) float64 { return math.Max(a, b) }
