package chat

import (
	"strings"
	"time"
	"unicode/utf8"
)

// 拟人化后处理。
//
// 对应 MaiBot 的：
//   - chat/utils/utils.py 的 calculate_typing_time / truncate_message
//   - services/send_service.py 的引用判断逻辑
//
// 不做错别字生成（配置里已明确不要）。

// TypingConfig 是打字延迟配置。
type TypingConfig struct {
	// Speed 是打字速度，0 最快，1 默认，2 更慢。
	Speed float64
	// Enabled 是否启用打字延迟。
	Enabled bool
}

// CalculateTypingTime 模拟打字耗时。
//
// 思路与 MaiBot 一致：
//   - 有基础反应时间（看到消息到开始打字）
//   - 每个字有一定耗时
//   - 加一点随机抖动，避免每次都一样
//
// 返回的是「额外等待时间」，不包含模型生成耗时。
func CalculateTypingTime(text string, cfg TypingConfig) time.Duration {
	if !cfg.Enabled {
		return 0
	}

	speed := cfg.Speed
	if speed < 0 {
		speed = 0
	}
	if speed > 2 {
		speed = 2
	}

	n := utf8.RuneCountInString(text)
	if n == 0 {
		return 0
	}

	// 基准：单字 50ms，基础反应 300ms。
	perChar := 50 * time.Millisecond
	base := 300 * time.Millisecond

	// speed=0 时最快（0.25 倍），speed=1 正常（1 倍），speed=2 最慢（2.5 倍）。
	scale := 0.25 + speed*1.125

	total := time.Duration(float64(base)+float64(perChar)*float64(n)) * time.Duration(scale*10) / 10

	// 上限保护：再长也不超过 6 秒，否则聊天节奏就断了。
	const maxTyping = 6 * time.Second
	if total > maxTyping {
		total = maxTyping
	}
	// 下限：至少 200ms，避免秒回。
	const minTyping = 200 * time.Millisecond
	if total < minTyping {
		total = minTyping
	}
	return total
}

// TruncateMessage 截断过长文本，保留可读性。
// 对应 MaiBot truncate_message（chat/utils/utils.py:739），固定补 "..."。
func TruncateMessage(text string, maxLen int) string {
	return TruncateMessageWith(text, maxLen, "...")
}

// TruncateMessageWith 截断文本并追加自定义替换文案。
//
// 参考实现的 cut 是按 Python 的字符下标切，
// 这里换算成 rune 而非 byte —— 按 byte 切会把一个汉字劈成两半，
// 生成非法 UTF-8，最终整条 JSON 都发不出去。
func TruncateMessageWith(text string, maxLen int, replacement string) string {
	if maxLen <= 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) <= maxLen {
		return text
	}
	return string(runes[:maxLen]) + replacement
}

// CleanReplyText 清理模型输出里常见的前后缀噪声。
//
// 模型经常自作主张加「回复：」、引号、括号动作描写等。
// 这里做保守清理，只处理最明显的情况。
func CleanReplyText(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}

	s = stripInstructionEcho(s)
	if s == "" {
		return s
	}

	// 去掉整体包裹的引号。
	pairs := [][2]string{
		{`"`, `"`},
		{"“", "”"},
		{"「", "」"},
		{"『", "』"},
	}
	for _, p := range pairs {
		if strings.HasPrefix(s, p[0]) && strings.HasSuffix(s, p[1]) &&
			utf8.RuneCountInString(s) > 2 {
			s = strings.TrimSpace(s[len(p[0]) : len(s)-len(p[1])])
		}
	}

	// 下去掉 "回复：" / "回答：" 这类前缀。
	//
	// 刻意不处理「昵称：」形式：昵称是配置项（bot.nickname），
	// 在 chat 包里拿不到它，而且模型本来也不该在正文前自报名字。
	for _, prefix := range []string{"回复：", "回复:", "回答：", "回答:"} {
		if strings.HasPrefix(s, prefix) {
			s = strings.TrimSpace(strings.TrimPrefix(s, prefix))
		}
	}

	return s
}

// instructionEchoPrefixes 是模型会当成「待回应指令」而照抄下来的开头。
//
// 来源：Replyer 请求的末尾块曾经误用 Planner 的提醒，里面有
// 「不调用工具」「使用reply回复」，模型于是先吐一句「不调用工具，直接回。」
// 再接正文。代码侧已经改用正确尾注（见 prompt.ReplyerReplyInstruction），
// 这里是最后一道兜底：已有脏数据、或者其他模型偶然模仿，也不让它发到群里。
//
// 刻意严格：只认这几种完整开头，不做模糊匹配 ——
// 群里真的有人会聊「不调用工具」这件事，那属于正文，不能删。
var instructionEchoPrefixes = []string{
	"不调用工具",
	"不用调用工具",
	"不需要调用工具",
	"不需要工具",
	"不用工具",
	"用reply回复",
	"用 reply 回复",
	"无需检索",
	"不用搜",
}

// stripInstructionEcho 丢弃开头那行指令回声，只留下正文。
//
// 只在两个条件同时成立时才动手：
//  1. 第一行以已知回声开头（instructionEchoPrefixes）
//  2. 后面确实还有非空正文（否则删了就什么都不剩，那也不是回声，
//     而是模型只说了这一句 —— 这种情况保留原文，让上层报「空内容」或者照发）
func stripInstructionEcho(s string) string {
	first, rest, found := strings.Cut(s, "\n")
	if !found || strings.TrimSpace(rest) == "" {
		return s
	}

	head := strings.TrimSpace(first)
	matched := false
	for _, p := range instructionEchoPrefixes {
		if strings.HasPrefix(head, p) {
			matched = true
			break
		}
	}
	if !matched {
		return s
	}

	// 第一行可能还带着「，直接回就行。」这类尾巴，整行丢掉即可。
	return strings.TrimSpace(rest)
}
