package agent

import (
	"strings"
	"testing"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
)

// TestKeywordMatcherHits 验证关键词子串命中。
func TestKeywordMatcherHits(t *testing.T) {
	m := newKeywordMatcher([]config.KeywordRule{
		{Keywords: []string{"报错", "挂了"}, Reaction: "用户遇到技术问题，先安抚再问具体细节"},
	})

	got := m.match("程序又报错了")
	if got == "" {
		t.Fatal("应命中，实际为空")
	}
	// 输出格式照抄参考实现，三个要素都要在。
	for _, want := range []string{"【关键词反应】", "最新消息命中了预设反应规则", "- 用户遇到技术问题"} {
		if !strings.Contains(got, want) {
			t.Errorf("输出缺少 %q，实际 = %q", want, got)
		}
	}
}

// TestKeywordMatcherMiss 验证不命中时返回空串（而不是空块）。
//
// 返回空块会让提示词里出现一个没有内容的「【关键词反应】」标题，
// 模型会疑惑为什么给了个空要求。
func TestKeywordMatcherMiss(t *testing.T) {
	m := newKeywordMatcher([]config.KeywordRule{
		{Keywords: []string{"报错"}, Reaction: "x"},
	})

	if got := m.match("今天天气不错"); got != "" {
		t.Errorf("不该命中，实际 = %q", got)
	}
}

// TestKeywordMatcherRegex 验证正则规则与捕获组替换。
func TestKeywordMatcherRegex(t *testing.T) {
	m := newKeywordMatcher([]config.KeywordRule{
		{
			Regex:    []string{`叫我(.{1,4})就行`},
			Reaction: "对方希望你叫他 $1，回复时用这个称呼",
		},
	})

	got := m.match("以后叫我小明就行")
	if !strings.Contains(got, "叫他 小明") {
		t.Errorf("捕获组未替换，实际 = %q", got)
	}
}

// TestKeywordMatcherSkipsInvalidRegex 验证坏正则不会拖垮其他规则。
//
// 配置校验已经拦过一次，但运行时仍要能容忍 —— 否则一条坏规则
// 会让整个关键词系统失效，而且只有真跑到才报错。
func TestKeywordMatcherSkipsInvalidRegex(t *testing.T) {
	m := newKeywordMatcher([]config.KeywordRule{
		{Regex: []string{`([unclosed`}, Reaction: "坏规则"},
		{Keywords: []string{"好规则"}, Reaction: "有效反应"},
	})

	got := m.match("这是好规则")
	if !strings.Contains(got, "有效反应") {
		t.Errorf("有效规则应生效，实际 = %q", got)
	}
}

// TestKeywordMatcherNil 验证空规则集时返回 nil 且 match 不 panic。
func TestKeywordMatcherNil(t *testing.T) {
	m := newKeywordMatcher(nil)
	if m != nil {
		t.Error("空规则集应返回 nil")
	}
	if got := m.match("随便"); got != "" {
		t.Errorf("nil matcher 应返回空串，实际 = %q", got)
	}
}

// TestKeywordMatcherMultipleRules 验证多条规则同时命中时都输出。
func TestKeywordMatcherMultipleRules(t *testing.T) {
	m := newKeywordMatcher([]config.KeywordRule{
		{Keywords: []string{"报错"}, Reaction: "反应A"},
		{Keywords: []string{"报错"}, Reaction: "反应B"},
	})

	got := m.match("又报错了")
	if !strings.Contains(got, "反应A") || !strings.Contains(got, "反应B") {
		t.Errorf("两条规则都应出现在输出里，实际 = %q", got)
	}
}

// TestKeywordMatcherEmptyReactionSkipped 验证空 reaction 的规则被忽略。
func TestKeywordMatcherEmptyReactionSkipped(t *testing.T) {
	m := newKeywordMatcher([]config.KeywordRule{
		{Keywords: []string{"报错"}, Reaction: "   "},
	})

	if m != nil {
		t.Error("所有规则 reaction 都为空时应返回 nil")
	}
}

// TestBuildExpressionHabits 验证表达注入文本。
func TestBuildExpressionHabits(t *testing.T) {
	got := buildExpressionHabits([]string{"当「表示惊叹」时，可以「我嘞个去」"})

	for _, want := range []string{"【表达习惯参考】", "不要直接照搬原句", "我嘞个去"} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少 %q，实际 = %q", want, got)
		}
	}
}

// TestBuildExpressionHabitsEmpty 验证空输入返回空串。
func TestBuildExpressionHabitsEmpty(t *testing.T) {
	if got := buildExpressionHabits(nil); got != "" {
		t.Errorf("期望空串，实际 = %q", got)
	}
}

// TestFormatExpression 验证「当 X 时，可以 Y」的拼装与缺值处理。
func TestFormatExpression(t *testing.T) {
	cases := []struct {
		name      string
		situation string
		style     string
		want      string
	}{
		{"都齐", "表示惊叹", "我嘞个去", "当「表示惊叹」时，可以「我嘞个去」"},
		{"只有情境", "表示惊叹", "", "表示惊叹"},
		{"只有说法", "", "我嘞个去", "我嘞个去"},
		{"都空", "", "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatExpression(tc.situation, tc.style); got != tc.want {
				t.Errorf("got %q，want %q", got, tc.want)
			}
		})
	}
}
