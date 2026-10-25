package tokenize

import (
	"strings"
	"testing"
)

// TestTokenizeCJK 验证中文既能按单字命中，也能按相邻二字命中。
//
// 这是整个检索能用的前提：只留单字噪声太大，只留二元查不到单字，
// 两者都必须有。
func TestTokenizeCJK(t *testing.T) {
	got := Tokenize("火锅")

	want := []string{"火", "火锅", "锅"}
	if len(got) != len(want) {
		t.Fatalf("token 数 = %d (%v)，期望 %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("token[%d] = %q，期望 %q", i, got[i], want[i])
		}
	}
}

// TestTokenizeMixed 验证中英混排与标点分隔。
func TestTokenizeMixed(t *testing.T) {
	got := Tokenize("用 DeepSeek 还是 glm")

	joined := strings.Join(got, ",")
	for _, want := range []string{"deepseek", "glm", "还", "还是", "是"} {
		if !strings.Contains(joined, want) {
			t.Errorf("缺少 token %q，实际 = %s", want, joined)
		}
	}
	// 标点与空格不应成为 token。
	for _, bad := range []string{" ", "，", ","} {
		for _, tk := range got {
			if tk == bad {
				t.Errorf("token 里不应包含分隔符 %q", bad)
			}
		}
	}
}

// TestTokenizeEmpty 验证空输入与纯标点不产出 token。
func TestTokenizeEmpty(t *testing.T) {
	for _, in := range []string{"", "   ", "，。！？", "..."} {
		if got := Tokenize(in); len(got) != 0 {
			t.Errorf("Tokenize(%q) = %v，期望空", in, got)
		}
	}
}

// TestBuildMatchExpr 验证 MATCH 表达式的构造。
//
// 连接词是 OR（以前是 AND，自然语言查询 0 命中，见 P1-6）；
// 有二元组可用时单字 token 被丢弃（P2-18）。
func TestBuildMatchExpr(t *testing.T) {
	// 「火锅」只有单字 + 一个二元组，丢掉单字后仍剩「火锅」可用。
	got := BuildMatchExpr("火锅")
	if got != `tokens:"火锅"` {
		t.Errorf("表达式 = %s，期望只保留二元组 tokens:\"火锅\"", got)
	}

	// 「今天天气」有多个二元组，单字「今」「天」「气」必须被丢弃。
	got = BuildMatchExpr("今天天气")
	for _, part := range []string{`tokens:"今天"`, `tokens:"天天"`, `tokens:"天气"`} {
		if !strings.Contains(got, part) {
			t.Errorf("表达式缺少 %q，实际 = %s", part, got)
		}
	}
	for _, bad := range []string{`tokens:"今"`, `tokens:"天"`, `tokens:"气"`} {
		if strings.Contains(got, bad) {
			t.Errorf("表达式不应包含单字 %q，实际 = %s", bad, got)
		}
	}
	if !strings.Contains(got, " OR ") {
		t.Errorf("表达式应该用 OR 连接，实际 = %s", got)
	}
}

// TestBuildMatchExprDedupes 验证重复 token 只出现一次。
func TestBuildMatchExprDedupes(t *testing.T) {
	// 「哈哈哈」会切出两个「哈哈」，去重后只能出现一次。
	got := BuildMatchExpr("哈哈哈")

	if strings.Count(got, `tokens:"哈哈"`) != 1 {
		t.Errorf("重复 token 未被去重，实际 = %s", got)
	}
}

// TestBuildMatchExprKeepsSingleCharQuery 验证纯单字查询仍可用。
//
// 丢单字规则的边界：如果丢完一个不剩（查询本来就只有一个字），
// 必须退回用全部 token，否则「卷」这类单字查询会返回空表达式、
// 调用方直接跳过检索。
func TestBuildMatchExprKeepsSingleCharQuery(t *testing.T) {
	if got := BuildMatchExpr("卷"); got != `tokens:"卷"` {
		t.Errorf("单字查询 = %q，期望 tokens:\"卷\"", got)
	}
}

// TestBuildMatchExprCapsTokens 验证表达式长度有上限（P2-20）。
//
// FTS5 对 MATCH 的解析与倒排合并开销与表达式长度成正比，
// 模型偶发传超长「问题」时（实测出现过 600+ token）会把检索拖到超时。
func TestBuildMatchExprCapsTokens(t *testing.T) {
	// 构造 66 个不同汉字：切出 66 个单字 + 65 个二元组，
	// 丢单字后剩 65 个二元组，应被截到 maxQueryTokens（64）。
	runes := make([]rune, 66)
	for i := range runes {
		runes[i] = rune(0x4E00 + i)
	}
	got := BuildMatchExpr(string(runes))

	if n := strings.Count(got, `tokens:"`); n != maxQueryTokens {
		t.Errorf("表达式含 %d 个 token，期望截断到 %d", n, maxQueryTokens)
	}
}

// TestTokenizeNormalizesWidth 验证全角/半角产出一致（P2-19）。
//
// 手机输入法常出全角；不归一的话「ＡＢＣ」与「ABC」是两个不同 token，
// 全角输入与半角库文跨形态永远查不到。
func TestTokenizeNormalizesWidth(t *testing.T) {
	cases := []struct{ full, half string }{
		{"ＡＢＣ", "ABC"},
		{"１２３", "123"},
		{"ａｂｃ", "abc"},
	}
	for _, tc := range cases {
		gotFull := Tokenize(tc.full)
		gotHalf := Tokenize(tc.half)
		if strings.Join(gotFull, ",") != strings.Join(gotHalf, ",") {
			t.Errorf("Tokenize(%q) = %v，与 Tokenize(%q) = %v 不一致", tc.full, gotFull, tc.half, gotHalf)
		}
	}

	// 全角空格与半角空格同样只作分隔符。
	if got := Tokenize("你　好"); strings.Contains(strings.Join(got, ","), "\u3000") {
		t.Errorf("全角空格不应进入 token，实际 = %v", got)
	}
}

// TestBuildMatchExprEmpty 验证无 token 时返回空串。
//
// 调用方靠这个判断「不要检索」，如果返回空表达式会退化成查全表。
func TestBuildMatchExprEmpty(t *testing.T) {
	if got := BuildMatchExpr("，。！"); got != "" {
		t.Errorf("期望空串，实际 = %q", got)
	}
}

// TestBuildMatchExprQuotesSafe 验证 token 里的引号不会破坏 FTS 语法。
func TestBuildMatchExprQuotesSafe(t *testing.T) {
	// 引号在 Tokenize 阶段已被当作分隔符丢弃，这里确认不会漏出裸引号。
	got := BuildMatchExpr(`he said "hi"`)
	if strings.Count(got, `"`)%2 != 0 {
		t.Errorf("引号不成对，表达式 = %s", got)
	}
}
