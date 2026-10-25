package chat

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// 回复分割。
//
// 对应 MaiBot 的 chat/utils/llm_sentence_splitter.py。
//
// 目的：模型经常一次吐出很长一段，直接发出去不像人。
// 真人聊天是一句一句发的，所以要把长回复按句子切开、分多条发出去。
// 但也不能切得太碎，所以要限制单条长度、单条句数、总条数。

// SplitConfig 是分割配置。
type SplitConfig struct {
	// Enable 允许把过长回复拆成多条。
	Enable bool
	// MaxLength 是单条最大长度（字符数）。
	MaxLength int
	// MaxSentenceNum 是单条最多包含多少句。
	MaxSentenceNum int
	// MaxSplitNum 是一次回复最多拆成几条。
	MaxSplitNum int
	// ProtectKaomoji 尽量避免把颜文字从中间拆开。
	ProtectKaomoji bool
	// OverflowReturnAll 句子太多时直接保留完整回复，不再强行截断。
	OverflowReturnAll bool
}

// DefaultSplitConfig 返回与 MaiBot 默认配置一致的分割参数。
func DefaultSplitConfig() SplitConfig {
	return SplitConfig{
		Enable:         true,
		MaxLength:      512,
		MaxSentenceNum: 8,
		MaxSplitNum:    3,
		ProtectKaomoji: false,
	}
}

// 句子边界标点。
var sentenceEnders = []rune{'。', '！', '？', '!', '?', '…', '\n'}

// 颜文字候选模式，用于保护不被切开。
var kaomojiPattern = regexp.MustCompile(`[（(][^（()）\x01\x02]{0,20}[)）]`)

// 受保护片段的占位符边界。
// 用不可见控制字符，正常文本里不会出现。
const (
	protectOpen  = '\x01'
	protectClose = '\x02'
)

// SplitResponse 把一段回复切成多条消息。
//
// 行为：
//   - 关闭分割时原样返回
//   - 先按句子边界切
//   - 再按最大长度合并成条
//   - 超过 MaxSplitNum 时把剩余内容并到最后一条（不丢内容）
//   - 空段落被丢弃
//   - 开启颜文字保护时，颜文字作为整体不会被切断
func SplitResponse(text string, cfg SplitConfig) []string {
	s := strings.TrimSpace(text)
	if s == "" {
		return nil
	}
	if !cfg.Enable {
		return []string{s}
	}
	if cfg.MaxLength <= 0 {
		cfg.MaxLength = 512
	}
	if cfg.MaxSplitNum <= 0 {
		cfg.MaxSplitNum = 3
	}
	if cfg.MaxSentenceNum <= 0 {
		cfg.MaxSentenceNum = 8
	}

	// 已经足够短，不用切。
	if utf8.RuneCountInString(s) <= cfg.MaxLength && countSentences(s) <= cfg.MaxSentenceNum {
		return []string{s}
	}

	// 颜文字先替换成占位符，切完再还原。
	// 这样两个切割阶段都不会把颜文字切断。
	protected, originals := protectKaomoji(s, cfg.ProtectKaomoji)

	sentences := splitSentences(protected)
	if len(sentences) == 0 {
		return []string{s}
	}

	var (
		chunks  []string
		current strings.Builder
		curLen  int
		curSent int
	)

	flush := func() {
		if current.Len() > 0 {
			chunks = append(chunks, strings.TrimSpace(current.String()))
			current.Reset()
			curLen = 0
			curSent = 0
		}
	}

	for _, sent := range sentences {
		sentLen := utf8.RuneCountInString(sent)

		// 单句就超长，硬切（占位符不会被打断）。
		if sentLen > cfg.MaxLength {
			flush()
			chunks = append(chunks, hardSplitProtected(sent, cfg.MaxLength)...)
			continue
		}

		// 加入这句会超限就换条。
		if curLen+sentLen > cfg.MaxLength || curSent >= cfg.MaxSentenceNum {
			flush()
		}
		current.WriteString(sent)
		curLen += sentLen
		curSent++
	}
	flush()

	if len(chunks) == 0 {
		return []string{s}
	}

	// 条数超限：把多余的并进最后一条，保证不丢内容。
	if len(chunks) > cfg.MaxSplitNum {
		if cfg.OverflowReturnAll {
			return []string{s}
		}
		head := chunks[:cfg.MaxSplitNum-1]
		tail := strings.Join(chunks[cfg.MaxSplitNum-1:], "")
		chunks = append(append([]string(nil), head...), strings.TrimSpace(tail))
	}

	// 还原颜文字并过滤空串。
	out := make([]string, 0, len(chunks))
	for _, c := range chunks {
		c = restoreKaomoji(strings.TrimSpace(c), originals)
		if c != "" {
			out = append(out, c)
		}
	}
	return out
}

// protectKaomoji 把颜文字替换成等长占位符，返回替换后的文本与还原表。
func protectKaomoji(s string, enabled bool) (string, []string) {
	if !enabled {
		return s, nil
	}

	var originals []string
	out := kaomojiPattern.ReplaceAllStringFunc(s, func(m string) string {
		idx := len(originals)
		originals = append(originals, m)
		return tokenFor(idx)
	})
	return out, originals
}

// tokenFor 生成第 idx 个占位符。
//
// 长度固定为 4 个 rune（\x01 数字 \x02），足够覆盖到 4 位数。
// 用固定长度是为了让长度计算保持稳定。
func tokenFor(idx int) string {
	digits := "0123456789"
	d0 := digits[idx%10]
	d1 := digits[(idx/10)%10]
	d2 := digits[(idx/100)%10]
	d3 := digits[(idx/1000)%10]
	return string([]rune{protectOpen, rune(d0), rune(d1), rune(d2), rune(d3), protectClose})
}

// restoreKaomoji 把占位符换回原颜文字。
func restoreKaomoji(s string, originals []string) string {
	if len(originals) == 0 {
		return s
	}
	runes := []rune(s)
	var b strings.Builder

	for i := 0; i < len(runes); {
		if runes[i] == protectOpen && i+5 < len(runes) && runes[i+5] == protectClose {
			digits := string(runes[i+1 : i+5])
			idx := 0
			for _, d := range digits {
				idx = idx*10 + int(d-'0')
			}
			if idx < len(originals) {
				b.WriteString(originals[idx])
				i += 6
				continue
			}
		}
		b.WriteRune(runes[i])
		i++
	}
	return b.String()
}

// splitSentences 按句子边界切分，保留标点。
//
// 占位符里不含句子边界字符（tokenFor 只用数字），所以不会被误切。
func splitSentences(s string) []string {
	var (
		out     []string
		current strings.Builder
	)

	for _, r := range s {
		current.WriteRune(r)

		for _, e := range sentenceEnders {
			if r == e {
				out = append(out, current.String())
				current.Reset()
				break
			}
		}
	}
	if current.Len() > 0 {
		out = append(out, current.String())
	}
	return out
}

// hardSplitProtected 把超长句子硬切，但不切断占位符。
//
// 如果单个占位符本身比 maxLen 还长（颜文字很长而限制很严的极端情况），
// 把它整体作为一个片段输出 —— 宁可超长，也不破坏颜文字的完整性。
func hardSplitProtected(s string, maxLen int) []string {
	runes := []rune(s)
	spans := tokenSpans(runes)

	var out []string
	for len(runes) > 0 {
		take := maxLen
		if take > len(runes) {
			take = len(runes)
		}

		// 如果切点落在占位符内部，往前退到占位符起点。
		adjusted := take
		for adjusted > 0 {
			if !insideToken(spans, adjusted) {
				break
			}
			adjusted--
		}

		if adjusted == 0 {
			// 说明从 0 开始就是个占位符，且它比 maxLen 长。
			// 整体取出这个占位符。
			end := tokenEndAt(spans, 0)
			if end <= 0 || end > len(runes) {
				end = take
			}
			out = append(out, string(runes[:end]))
			runes = runes[end:]
			spans = tokenSpans(runes)
			continue
		}

		out = append(out, string(runes[:adjusted]))
		runes = runes[adjusted:]
		spans = tokenSpans(runes)
	}
	return out
}

// span 是一个占位符在 rune 数组里的 [start, end) 区间。
type span struct {
	start int
	end   int
}

// tokenSpans 扫描出所有占位符区间。
func tokenSpans(runes []rune) []span {
	var out []span
	for i := 0; i < len(runes); i++ {
		if runes[i] != protectOpen {
			continue
		}
		for j := i + 1; j < len(runes); j++ {
			if runes[j] == protectClose {
				out = append(out, span{start: i, end: j + 1})
				i = j
				break
			}
		}
	}
	return out
}

// insideToken 判断切点 pos 是否落在某个占位符内部（不含边界）。
func insideToken(spans []span, pos int) bool {
	for _, sp := range spans {
		if pos > sp.start && pos < sp.end {
			return true
		}
	}
	return false
}

// tokenEndAt 返回从 pos 开始的占位符结束位置。
func tokenEndAt(spans []span, pos int) int {
	for _, sp := range spans {
		if sp.start == pos {
			return sp.end
		}
	}
	return -1
}

// countSentences 数一下有多少句。
func countSentences(s string) int {
	n := 1
	for _, r := range s {
		for _, e := range sentenceEnders {
			if r == e {
				n++
				break
			}
		}
	}
	return n
}
