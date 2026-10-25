package learning

import (
	"github.com/ohmyqqbot/oh-my-qqbot/internal/prompt"
)

// 本文件是表达学习与黑话学习的提示词来源。
//
// 提示词本身放在 internal/prompt/embedded/zh-CN/*.prompt（可在 WebUI 里直接改），
// 默认内容逐字照抄 refs/MaiBot/prompts/zh-CN/ 里对应的两份 —— 唯一差异是
// 原文里的 JSON 示例写成 {{"situation": ...}}，那是 Python 模板引擎的转义，
// 内嵌副本里已经还原成单花括号。
//
// 为什么这些模板也走 prompt.Render 而不是 strings.Replace：
// 早期担心 JSON 示例里的花括号被当成占位符，于是绕开 Render 用 Replace。
// 但 Render 本来就会跳过 `{{`、以及含引号/冒号/逗号/空白的花括号片段
// （见 replacePlaceholders 的判定），所以 JSON 示例是安全的。
// 绕开 Render 的代价是用户在 WebUI 里改了模板也不生效 —— 那才是最糟的。

// 提示词名字。与 embedded/zh-CN/<name>.prompt 一一对应。
const (
	promptNameLearnStyle = "learn_style"
	promptNameLearnJargon = "learn_jargon"
	promptNameJargonWithContext = "jargon_inference_with_context"
	promptNameJargonContentOnly = "jargon_inference_content_only"
)

// promptManager 是包级提示词来源。
//
// 用包级变量而不是给 Learner 加字段：包里只有一个 Learner 实例，
// 而测试有十几个在构造它，逐个加参数只为了传同一个指针。
// 未注入（nil）时退回内嵌默认模板，不会 panic 也不会拿到空串。
var promptManager *prompt.Manager

// SetPromptManager 注入提示词管理器。
//
// 它只是个指针，读的时候现取，所以进程启动后随时调用都安全；
// 但要在第一次学习之前调用，否则那一轮用的是内嵌默认文案。
func SetPromptManager(m *prompt.Manager) { promptManager = m }

// promptTemplate 取模板内容。
//
// 三级兜底，绝不给调用方空串：
//  1. 注入的 Manager（用户改过的版本）
//  2. 内嵌默认模板
//  3. 一句能看出问题的说明
//
// 空 system prompt 不会报错，只会让模型输出完全不可预期的内容 ——
// 而学习任务是后台跑的，失败只有一条 warn，几乎无法排查。
func promptTemplate(name string) string {
	if promptManager != nil {
		if tmpl, ok := promptManager.Get(name); ok && tmpl != "" {
			return tmpl
		}
	}
	if tmpl, ok := prompt.EmbeddedDefault(name); ok && tmpl != "" {
		return tmpl
	}
	return "（提示词 " + name + " 缺失，请检查 prompts 目录）"
}

// renderPrompt 渲染一个模板。
func renderPrompt(name string, values map[string]string) string {
	return prompt.Render(promptTemplate(name), values)
}

// chatStrKey 是两份提取类提示词里唯一的占位符名（模板里写作 {chat_str}）。
//
// 聊天记录的渲染格式统一成 `[source_id:N] 用户名: 内容` 一行一条：
// 参考实现（expression_learner.py:_build_multi_learning_messages）用了
// `<message ...>` 标签 + speaker/time 元信息，但黑话提示词的字段说明里
// 同时承认 `[source_id:N]` 和 `<message source_id="N">` 两种写法，
// 说明模型对两种格式都能对账。我们选 `[source_id:N]` 的原因：
//  1. 一行一条，拼接简单，不会因为标签没闭合让模型误判消息边界；
//  2. 参考实现自己也用 `[source_id:N]` 在标签体系之外标识来源行；
//  3. 单行格式更容易在日志和测试里肉眼核对。
const chatStrKey = "chat_str"
