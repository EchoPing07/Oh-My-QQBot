package config

import (
	"fmt"
	"regexp"
	"strings"
)

// 本文件是「关键词反应规则」的结构化编辑。
//
// 为什么不塞进 EditableFields 的平铺清单：
//
//	keyword.rules 是一个列表，每条有 keywords[] / regex[] / reaction 三个字段。
//	点分键表单只能表达标量，硬塞就得写 "keyword.rules.0.reaction"，
//	既不能增删行，也无法表达「这一条有哪些关键词」。
//	与 llm 那三层嵌套是同一类问题，所以走同样的一套：结构化 JSON。
//
// 与 MaiBot 的对照：它的关键词规则在「麦麦设置」里是一张表（关键词/正则/反应），
// 逐行增删。我们做同一个形态，只是不做它的「导入导出 JSON」。
//
// 提交语义是**整表替换**，这是与 llm 的 action 式提交刻意不同的地方：
//
//	llm 那边一次只改一个服务商/模型，因为它们是三层嵌套、有跨引用；
//	整节覆盖会让「前端少发一个字段」等于把它清空。
//	关键词规则是一张扁平表，用户在页面上看到的就是全部规则，
//	表格编辑器天然是「提交整张表」。这里不做增量动作，
//	但加了一条硬保护：提交的条数必须等于前端拿到的条数 + 显式新增 - 显式删除，
//	这一条由「前端提交完整数组」保证 —— 见 web.handleKeywordSave 的注释。

// KeywordRuleView 是一条规则在界面上的形态。
//
// 字段名与 KeywordRule 的 toml 标签一致：前端不需要在
// 「界面字段」与「配置字段」之间做一次映射，少一处能漂的地方。
type KeywordRuleView struct {
	Keywords []string `json:"keywords"`
	Regex    []string `json:"regex"`
	Reaction string   `json:"reaction"`
}

// KeywordRulesView 返回当前全部规则（拷贝，调用方可以随便改）。
func (c *Config) KeywordRulesView() []KeywordRuleView {
	out := make([]KeywordRuleView, 0, len(c.Keyword.Rules))
	for _, r := range c.Keyword.Rules {
		out = append(out, KeywordRuleView{
			Keywords: append([]string{}, r.Keywords...),
			Regex:    append([]string{}, r.Regex...),
			Reaction: r.Reaction,
		})
	}
	return out
}

// ApplyKeywordRules 用给定的列表整体替换规则。
//
// 校验在这里做而不是只依赖 Validate：
//   - 正则在这里编译一次，报错能指到「第几条」，而 Validate 只会说 "keyword.rules[2]"
//   - 空规则在这里就被剔掉，不会写进配置文件里占一行
//
// 为什么允许「全部删空」：用户可能就是想清空规则。
// 空规则集在运行时表现为 newKeywordMatcher 返回 nil、不追加任何提示，是合法状态。
func (c *Config) ApplyKeywordRules(rules []KeywordRuleView) error {
	next := make([]KeywordRule, 0, len(rules))
	for i, r := range rules {
		reaction := strings.TrimSpace(r.Reaction)
		keywords := trimEmpty(r.Keywords)
		regexes := trimEmpty(r.Regex)

		// 整行都是空的（用户点了「添加」还没填）：跳过而不是报错。
		// 报错会让「点一下添加再加一个」变成不可用操作 ——
		// 中间必然存在一瞬是空行。
		if len(keywords) == 0 && len(regexes) == 0 && reaction == "" {
			continue
		}
		if len(keywords) == 0 && len(regexes) == 0 {
			return fmt.Errorf("第 %d 条规则：关键词与正则至少填一个", i+1)
		}
		if reaction == "" {
			return fmt.Errorf("第 %d 条规则：反应内容不能为空", i+1)
		}
		for _, pattern := range regexes {
			if _, err := regexp.Compile(pattern); err != nil {
				return fmt.Errorf("第 %d 条规则：正则 %q 无效: %w", i+1, pattern, err)
			}
		}

		next = append(next, KeywordRule{
			Keywords: keywords,
			Regex:    regexes,
			Reaction: reaction,
		})
	}

	c.Keyword.Rules = next
	return nil
}
