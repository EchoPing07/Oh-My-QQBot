package emoji

import (
	"github.com/ohmyqqbot/oh-my-qqbot/internal/prompt"
)

// 本文件是表情包打标签提示词的来源。
//
// 提示词放在 internal/prompt/embedded/zh-CN/emoji_content_analysis.prompt
// （可在 WebUI 里直接改），默认内容逐字照抄
// refs/MaiBot/prompts/zh-CN/emoji_content_analysis.prompt。

// promptNameAnalysis 是表情包打标签用的提示词名。
const promptNameAnalysis = "emoji_content_analysis"

// promptManager 是包级提示词来源。
//
// 用包级变量而不是给 Manager 加字段：包里只有一个 Manager 实例，
// 而测试有若干处在构造它，逐个加参数只为了传同一个指针。
// 未注入（nil）时退回内嵌默认模板，不会 panic 也不会拿到空串。
var promptManager *prompt.Manager

// SetPromptManager 注入提示词管理器。
//
// 它只是个指针，读的时候现取，所以进程启动后随时调用都安全；
// 但要在第一批表情包落库前调用，否则那几张用的是内嵌默认文案。
func SetPromptManager(m *prompt.Manager) { promptManager = m }

// promptTemplate 取模板内容。
//
// 三级兜底，绝不给调用方空串：空提示词会让 VLM 输出完全不可预期的标签，
// 而那些标签会进表情包库、影响之后的检索质量 —— 且没有任何报错。
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
