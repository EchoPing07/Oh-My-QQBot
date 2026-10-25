package learning

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/llm"
)

// 黑话含义推断。
//
// 对应参考实现 refs/MaiBot/src/learners/jargon_miner.py 的 infer_meaning
// 与 _should_infer_meaning。
//
// 为什么不「一学到就推断」：
// 黑话的含义往往要靠多次不同语境的用法才能确定。只见过一次就下结论，
// 大概率是错的，而且错的含义会一直留在库里被注入提示词。
// 参考实现在命中次数达到 4 / 8 / 25 / 100 时才推断，
// 次数越多上下文越充分，结论也越准。这里照抄同一套阈值。

// jargonInferenceThresholds 是触发含义推断的命中次数阈值。
// 与参考实现的 JARGON_INFERENCE_THRESHOLDS 一致。
var jargonInferenceThresholds = []int{4, 8, 25, 100}

// shouldInferMeaning 判断某条黑话是否该做（再一次的）含义推断。
//
// 四个条件全都要满足，缺一不可：
//   - 还没推断过（含义为空），或次数超过了上次推断时的次数
//   - 命中次数达到下一个阈值
//   - 不是人工录入的（本项目没有人工录入通道，这个判断保留给未来）
func shouldInferMeaning(count, lastInferredAt int, meaning string) bool {
	if count < jargonInferenceThresholds[0] {
		return false
	}
	// 已经推断过且没到下一次阈值，不重复花钱。
	if count <= lastInferredAt {
		return false
	}

	next := nextThreshold(lastInferredAt)
	if next == 0 {
		return false
	}
	if count < next {
		return false
	}

	// 已经推断出非空含义且次数没再涨，也跳过。
	// 这里不因「已有含义」就永久停止：黑话的用法会变，
	// 到 100 次时应该重新确认一次。
	return strings.TrimSpace(meaning) == "" || count >= next
}

// nextThreshold 返回大于 last 的最小阈值，没有则返回 0。
func nextThreshold(last int) int {
	for _, th := range jargonInferenceThresholds {
		if th > last {
			return th
		}
	}
	return 0
}

// infiniteResult 是模型对「这个词是什么意思」的回答。
type inferenceResult struct {
	Meaning string `json:"meaning"`
	NoInfo  bool   `json:"no_info"`
}

// inferMeaning 调模型推断黑话含义。
//
// 两步，与参考实现一致：
//  1. 带上下文推断（jargon_inference_with_context.prompt）
//  2. 第一步说信息不足时，只凭词条本身再猜一次（jargon_inference_content_only.prompt）
//
// 第二步的价值：很多黑话（yyds、绝绝子）脱离上下文也能猜，
// 而上下文里常常只有一句「笑死」这种毫无信息量的用法。
func (l *Learner) inferMeaning(ctx context.Context, content, contextText, previousMeaning string) (string, error) {
	botName := l.cfg.Get().Bot.Nickname

	previousSection := ""
	previousInstruction := ""
	if strings.TrimSpace(previousMeaning) != "" {
		previousSection = "\n**上一次推断的含义（仅供参考）**\n" + strings.TrimSpace(previousMeaning)
		previousInstruction = "- 请参考上一次推断的含义，结合新的上下文信息，给出更准确或更新的推断结果"
	}

	promptText := renderPrompt(promptNameJargonWithContext, map[string]string{
		"content":                     content,
		"bot_name":                    botName,
		"raw_content_list":            contextText,
		"previous_meaning_section":    previousSection,
		"previous_meaning_instruction": previousInstruction,
	})

	first, err := l.inferOnce(ctx, promptText)
	if err != nil {
		return "", err
	}
	if first != nil && !first.NoInfo && strings.TrimSpace(first.Meaning) != "" {
		return strings.TrimSpace(first.Meaning), nil
	}

	// 上下文不够，退一步只看词条本身。
	prompt2 := renderPrompt(promptNameJargonContentOnly, map[string]string{
		"content": content,
	})

	second, err := l.inferOnce(ctx, prompt2)
	if err != nil {
		return "", err
	}
	if second != nil && strings.TrimSpace(second.Meaning) != "" {
		return strings.TrimSpace(second.Meaning), nil
	}
	return "", nil
}

// inferOnce 发一次推断请求并解析 JSON。
func (l *Learner) inferOnce(ctx context.Context, prompt string) (*inferenceResult, error) {
	resp, err := l.llm.Generate(ctx, config.TaskLearner, &llm.Request{
		Messages: []llm.Message{{Role: llm.RoleSystem, Content: prompt}},
	})
	if err != nil {
		return nil, fmt.Errorf("调用模型推断含义: %w", err)
	}

	raw := strings.TrimSpace(resp.Content)
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start < 0 || end <= start {
		// 模型没给 JSON。可能是直接回了一句「我不确定」，
		// 这不算错误，交给上层走第二步。
		return nil, nil
	}

	var out inferenceResult
	if err := json.Unmarshal([]byte(raw[start:end+1]), &out); err != nil {
		return nil, fmt.Errorf("解析含义推断结果失败: %w", err)
	}
	return &out, nil
}

// 含义推断的提示词在 internal/prompt/embedded/zh-CN/ 下（可在 WebUI 里改）：
//
//	jargon_inference_with_context.prompt    带上下文推断
//	jargon_inference_content_only.prompt    只看词条本身
//
// 默认内容逐字照抄 refs/MaiBot/prompts/zh-CN/ 里对应两份，
// 只把模板转义的双花括号还原成单花括号（JSON 示例部分），其余一字不改。
// 两份都用 prompt.Render 渲染：JSON 示例里的 {{ 与含引号的花括号片段
// 会被 Render 原样跳过（见 prompt.replacePlaceholders 的判定）。

