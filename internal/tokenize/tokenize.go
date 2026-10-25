// Package tokenize 是中文检索用的轻量分词器。
//
// 单独成包的原因：store 与 memory 都要用它，放在 memory 里会让
// store → memory（分词）与 memory → store（落库）形成 import 循环。
//
// 为什么需要自研分词（AGENTS.md 要求写清偏离理由）：
//
//	参考实现：refs/MaiBot/src/maisaka/memory/mid_term.py 用 embedding 算余弦相似度，
//	          召回线索（recall_cues）与当前上下文做语义匹配。
//	本项目：  部署端点只提供 chat 模型，/v1/embeddings 返回 404
//	          （实测 text-embedding-3-small / bge-m3 / bge-large-zh /
//	          embedding-3 四种模型名全部 not found），没有可用向量服务。
//	          所以退化为「自研中文分词 + FTS5 倒排」。
//
// 为什么不用 SQLite 自带的 trigram 分词器：实测它对中文短词失效。
// trigram 把文本切成连续三字组，查「火锅」时无法匹配到「去吃了火锅」
// （三字组里没有「火锅」这个两字单元）。见 tokenize_test.go。
package tokenize

import (
	"strings"
	"unicode"
)

// normalizeWidth 把全角 ASCII 归一成半角。
//
// 只做两件事：U+FF01–U+FF5E 映射到 U+0021–U+007E（差 0xFEE0）、
// 全角空格 U+3000 映射到半角空格。手写映射、不引入 golang.org/x/text：
// 完整的 NFKC 还会归一组合字符、兼容汉字等大量形态，远超
// 「群里全角输入与半角库文互相检索不到」这个问题需要的范围。
func normalizeWidth(r rune) rune {
	switch {
	case r == 0x3000:
		return ' '
	case r >= 0xFF01 && r <= 0xFF5E:
		return r - 0xFEE0
	}
	return r
}

// Tokenize 把一段中文/混合文本切成检索用的 token 列表。
//
// 规则（刻意简单，够用即可）：
//   - 汉字：每个单字一个 token，且相邻两字再组成一个二元 token
//   - 其他字母数字：连续的一段整体小写后作为一个 token
//   - 标点与空白：只作分隔
//   - 全角 ASCII 与全角空格先归一成半角（见 normalizeWidth）
//
// 为什么既要单字又要二元：
//   - 只保留单字 → 噪声大，「的」「了」到处命中，排序没意义
//   - 只保留二元 → 单字查询（如「卷」）查不到
//   - 两者都留，靠 bm25 让稀有的二元组权重自然变高
func Tokenize(s string) []string {
	// 归一必须放在最前面：否则「ＡＢＣ」与「ABC」是两个不同的 token，
	// 全角输入与半角库文跨形态永远查不到。
	s = strings.Map(normalizeWidth, s)

	var (
		out   []string
		cjk   []rune
		other []rune
	)

	flushCJK := func() {
		for i := 0; i < len(cjk); i++ {
			out = append(out, string(cjk[i]))
			if i+1 < len(cjk) {
				out = append(out, string(cjk[i:i+2]))
			}
		}
		cjk = nil
	}

	flushOther := func() {
		if len(other) > 0 {
			out = append(out, strings.ToLower(string(other)))
			other = nil
		}
	}

	for _, r := range s {
		switch {
		case unicode.Is(unicode.Han, r):
			flushOther()
			cjk = append(cjk, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			flushCJK()
			other = append(other, r)
		default:
			flushCJK()
			flushOther()
		}
	}
	flushCJK()
	flushOther()

	return out
}

// TokenizeJoined 返回空格拼接的分词串，直接写进 FTS 的 tokens 列。
func TokenizeJoined(s string) string {
	return strings.Join(Tokenize(s), " ")
}

// maxQueryTokens 限制 MATCH 表达式里的 token 数上限。
//
// FTS5 对 MATCH 的解析与倒排合并开销与表达式长度成正比，
// 模型偶发传超长「问题」时（实测出现过 600+ token），10 秒检索
// 超时兜不住；64 个 token 已远超正常问题的长度。
const maxQueryTokens = 64

// BuildMatchExpr 把查询串转成 FTS5 的 MATCH 表达式。
//
// 连接词用 OR、排序交给 bm25：
//   - 以前用 AND 连接全部 token，自然语言查询基本 0 命中 ——
//     「还记得上次吃的火锅吗」会切出「得上」「次吃」这类跨词二元组，
//     文档里不存在，AND 立即失败（实测见 docs/audit/11-infra.md P1-6）。
//   - OR 召回后 bm25 天然按相关度排序：命中的 token 越多、越稀有，
//     权重越高。
//
// 单字 token 在构造表达式时丢弃（噪声：「的」「了」到处命中，
// 还让「查锅命中火锅文档」这种同字不同词互撞），除非丢完一个不剩 ——
// 「单字查询」（如「卷」）仍然可用。索引侧 TokenizeJoined 保持不变，
// 已落库的索引不需要因为单字规则重建。
//
// 返回空串表示查询没有可用 token，调用方应跳过检索而不是查全表。
func BuildMatchExpr(query string) string {
	tokens := Tokenize(query)
	if len(tokens) == 0 {
		return ""
	}

	seen := make(map[string]struct{}, len(tokens))
	unique := make([]string, 0, len(tokens))
	for _, tk := range tokens {
		if _, dup := seen[tk]; dup {
			continue
		}
		seen[tk] = struct{}{}
		unique = append(unique, tk)
	}

	// 丢单字 token；全丢光了退回用全部 token，保证单字查询可用。
	multi := make([]string, 0, len(unique))
	for _, tk := range unique {
		if len([]rune(tk)) > 1 {
			multi = append(multi, tk)
		}
	}
	if len(multi) > 0 {
		unique = multi
	}

	// 截断：单字已丢弃，剩下的都是二元组/英文词，按出现顺序取前 N 个。
	if len(unique) > maxQueryTokens {
		unique = unique[:maxQueryTokens]
	}

	parts := make([]string, 0, len(unique))
	for _, tk := range unique {
		// token 已在 Tokenize 里过滤掉标点，这里只需防 FTS 语法字符。
		// 双引号包裹即可安全表达任意 token。
		parts = append(parts, `tokens:"`+strings.ReplaceAll(tk, `"`, `""`)+`"`)
	}
	return strings.Join(parts, " OR ")
}
