package chat

import "strconv"

// strconvFormatFloat 按最短形式格式化浮点数。
// 与 Python 的 :g 格式接近：1.0 -> "1"，0.5 -> "0.5"，4.0 -> "4"。
func strconvFormatFloat(f float64) string {
	if f == float64(int64(f)) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}
