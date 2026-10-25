package agent

import (
	"regexp"
	"strings"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
)

// keywordMatcher 是编译好的关键词反应规则集。
//
// 对应 MaiBot 的 KeywordReactionConfig + maisaka_generator_base.py 的
// _build_keyword_reaction_prompt。
//
// 编译一次、运行时只做匹配：正则在配置校验阶段就编译过一遍（config.Validate），
// 这里再编一次是为了拿到 *regexp.Regexp；编不出来就跳过该条规则，
// 不让一条坏规则拖垮整个回复。
type keywordMatcher struct {
	rules []compiledRule
}

type compiledRule struct {
	keywords []string
	regex    []*regexp.Regexp
	reaction string
}

// newKeywordMatcher 编译规则集。cfg 为空时返回 nil（调用方需容忍 nil）。
func newKeywordMatcher(rules []config.KeywordRule) *keywordMatcher {
	if len(rules) == 0 {
		return nil
	}

	m := &keywordMatcher{}
	for _, r := range rules {
		reaction := strings.TrimSpace(r.Reaction)
		if reaction == "" {
			continue
		}
		compiled := compiledRule{reaction: reaction}

		for _, kw := range r.Keywords {
			if kw = strings.TrimSpace(kw); kw != "" {
				compiled.keywords = append(compiled.keywords, kw)
			}
		}
		for _, pattern := range r.Regex {
			pattern = strings.TrimSpace(pattern)
			if pattern == "" {
				continue
			}
			// 配置校验已经检查过一遍，这里失败说明是运行期才出现的极端情况
			// （例如配置被外力改过），跳过即可。
			if re, err := regexp.Compile(pattern); err == nil {
				compiled.regex = append(compiled.regex, re)
			}
		}

		if len(compiled.keywords) == 0 && len(compiled.regex) == 0 {
			continue
		}
		m.rules = append(m.rules, compiled)
	}

	if len(m.rules) == 0 {
		return nil
	}
	return m
}

// match 返回命中规则拼成的提示文本，没命中返回空串。
//
// 输出格式逐字照抄参考实现：
//
//	【关键词反应】
//	最新消息命中了预设反应规则，请在回复时优先参考以下要求：
//	- <reaction>
func (m *keywordMatcher) match(text string) string {
	if m == nil || strings.TrimSpace(text) == "" {
		return ""
	}

	var reactions []string
	for _, rule := range m.rules {
		// 关键词走子串匹配，与参考实现一致：它用的是 `keyword in match_text`。
		for _, kw := range rule.keywords {
			if strings.Contains(text, kw) {
				reactions = append(reactions, rule.reaction)
				break
			}
		}
		// 正则与关键词是 OR 关系：命中了关键词就不必再跑正则。
		already := len(reactions) > 0 && reactions[len(reactions)-1] == rule.reaction
		if already {
			continue
		}
		for _, re := range rule.regex {
			found := re.FindStringSubmatch(text)
			if found == nil {
				continue
			}
			// 支持 $1 捕获组替换，与参考实现的 _replace_regex_capture_groups 等价。
			reactions = append(reactions, expandCaptures(rule.reaction, found))
			break
		}
	}

	if len(reactions) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("【关键词反应】\n")
	b.WriteString("最新消息命中了预设反应规则，请在回复时优先参考以下要求：\n")
	for _, r := range reactions {
		b.WriteString("- ")
		b.WriteString(r)
		b.WriteString("\n")
	}
	return b.String()
}

// expandCaptures 把 $1 / $name 替换成捕获组内容。
func expandCaptures(reaction string, groups []string) string {
	if len(groups) <= 1 {
		return reaction
	}
	out := reaction
	for i := len(groups) - 1; i >= 1; i-- {
		out = strings.ReplaceAll(out, "$"+itoa(i), groups[i])
	}
	return out
}

// itoa 是 strconv.Itoa 的最小替代，避免为了一个数字引入 import。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [4]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// lastUserText 返回最近一条用户消息的纯文本，用于关键词匹配。
//
// 参考实现取的是「最新一条 source_kind == user 的消息」，
// 这里保持一致：机器人自己的发言不参与匹配。
func lastUserText(msgs []*domain.Message, botIDs map[string]struct{}) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m == nil {
			continue
		}
		if _, isBot := botIDs[m.UserID]; isBot {
			continue
		}
		text := strings.TrimSpace(m.PlainText)
		if text != "" {
			return text
		}
	}
	return ""
}

// buildExpressionHabits 把学到的表达方式拼成注入文本。
//
// 对应参考实现 maisaka_expression_selector 产出的 expression_habits。
// 参考实现的措辞我们照抄 —— 它明确要求「不要直接复述」，这条约束很重要，
// 否则模型会把学到的例句原样贴出来，一眼假。
func buildExpressionHabits(items []string) string {
	if len(items) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("【表达习惯参考】\n")
	b.WriteString("以下是你在类似场景下常用的说话方式，可以参考它的语气和句式，但不要直接照搬原句：\n")
	for _, it := range items {
		it = strings.TrimSpace(it)
		if it == "" {
			continue
		}
		b.WriteString("- ")
		b.WriteString(it)
		b.WriteString("\n")
	}
	if b.Len() == 0 {
		return ""
	}
	return b.String()
}

// formatExpression 把一条表达渲染成「当 X 时，可以 Y」。
func formatExpression(situation, style string) string {
	situation = strings.TrimSpace(situation)
	style = strings.TrimSpace(style)
	switch {
	case situation == "" && style == "":
		return ""
	case situation == "":
		return style
	case style == "":
		return situation
	}
	return "当「" + situation + "」时，可以「" + style + "」"
}

// nowUnix 返回当前 Unix 秒，抽出来便于测试替换。
func nowUnix() int64 { return time.Now().Unix() }
