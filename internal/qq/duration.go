package qq

import "time"

// secondsDuration 把秒数转成 time.Duration。
func secondsDuration(seconds int) time.Duration {
	return time.Duration(seconds) * time.Second
}
