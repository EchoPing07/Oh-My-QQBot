package memory

import (
	"github.com/ohmyqqbot/oh-my-qqbot/internal/prompt"
)

// 本文件是 memory 包对「可编辑提示词」的统一入口。
//
// 提示词全部放在 internal/prompt/embedded/zh-CN/*.prompt，可在 WebUI 里直接改，
// 代码里不保留副本 —— 两份文本必然漂移，而漂移后的表现是
// 「网页上改了一段、但某条链路还是按旧文案跑」，极难排查。

// 提示词名字。与 embedded/zh-CN/<name>.prompt 一一对应。
const (
	promptNameSummary    = "mid_term_memory_summary"
	promptNameProfile    = "person_profile"
	promptNameAttachment = "reply_attachments"
)

// promptManager 是包级提示词来源。
//
// 用包级变量而不是给每个构造器加参数：memory 包里有 4 个调用点、
// 十几个测试在构造它们，逐个加参数只为了传同一个指针，
// 结果是每个测试都得写一次 "cfg, client, db, nil, prompts"。
// 进程里只有一个 Manager（app.go 启动时创建），所以一次注入即可。
//
// 未注入（nil）时退回内嵌默认模板，不会 panic，也不会拿到空串。
var promptManager *prompt.Manager

// SetPromptManager 注入提示词管理器。
//
// 它只是个指针，读的时候现取，所以进程启动后随时调用都安全；
// 但要在跑第一轮摘要之前调用，否则那一轮用的是内嵌默认文案。
func SetPromptManager(m *prompt.Manager) { promptManager = m }

// promptTemplate 取模板内容。
//
// 三级兜底，绝不给调用方空串：
//  1. 注入的 Manager（用户改过的版本）
//  2. 内嵌默认模板（Manager 里一定也有一份，这里是防御性重复）
//  3. 什么都没有时返回一句能看出问题的说明，而不是空 ——
//     空 system prompt 会让模型行为完全不可预期，且日志里没有任何线索。
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
