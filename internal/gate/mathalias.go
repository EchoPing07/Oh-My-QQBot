package gate

import "math"

// 数学函数别名，避免 util.go 里再引一批名字。
func math_Ceil(f float64) float64   { return math.Ceil(f) }
func math_Min(a, b float64) float64 { return math.Min(a, b) }
