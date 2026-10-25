package learning

// 本文件守「学习链路的提示词可被用户在 WebUI 上改掉」。
//
// 这两份模板（表达学习、黑话提取）以前是包里的 const，
// 用户想调一句措辞只能改代码。现在它们在内嵌资源里，
// 走 prompt.Manager，由 SetPromptManager 注入。
//
// 漏接的表现极其隐蔽：WebUI 上改完、保存成功、列表上还标着「已改」，
// 但学出来的东西一点没变 —— 因为代码走的是另一份文案。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/prompt"
)

// stubPromptManager 在临时目录里写一份覆盖模板并返回对应的 Manager。
func stubPromptManager(t *testing.T, name, content string) *prompt.Manager {
	t.Helper()

	dir := t.TempDir()
	localeDir := filepath.Join(dir, "zh-CN")
	if err := os.MkdirAll(localeDir, 0o755); err != nil {
		t.Fatalf("创建提示词目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localeDir, name+".prompt"), []byte(content), 0o644); err != nil {
		t.Fatalf("写入提示词失败: %v", err)
	}
	pm, err := prompt.New(dir, "zh-CN")
	if err != nil {
		t.Fatalf("加载提示词失败: %v", err)
	}
	return pm
}

// TestPromptManagerInjectionAffectsRendering 守学习链路真的读注入的 Manager。
//
// 直接比渲染结果，不跑整条 RunOnce：这里要证明的是「取模板这一步」，
// 而端到端链路已经有 TestRunOncePersistsExpressions 覆盖。
func TestPromptManagerInjectionAffectsRendering(t *testing.T) {
	pm := stubPromptManager(t, promptNameLearnStyle, "OVERRIDDEN-STYLE\n{chat_str}")
	SetPromptManager(pm)
	t.Cleanup(func() { SetPromptManager(nil) })

	got := renderPrompt(promptNameLearnStyle, map[string]string{chatStrKey: "小明：你好"})
	if !strings.Contains(got, "OVERRIDDEN-STYLE") {
		t.Errorf("渲染结果没有用覆盖模板：%q", got)
	}
	// {chat_str} 必须真的被换掉：以前用 strings.Replace 是可行的，
	// 但换成 Render 之后如果键名写错（chatStrKey 与实际不一致），
	// 消息内容会静默消失，模型收到一份只有指令没有语料的请求。
	if !strings.Contains(got, "小明：你好") {
		t.Errorf("聊天记录没有被填进去：%q", got)
	}
	if strings.Contains(got, "{chat_str}") {
		t.Errorf("占位符没有被替换：%q", got)
	}
}

// TestRenderPromptFallsBackToEmbedded 守「未注入 Manager 时不返回空串」。
//
// 空 system prompt 会让模型输出完全不可预期的内容，
// 而学习任务是后台跑的，失败只有一条 warn —— 几乎无法排查。
func TestRenderPromptFallsBackToEmbedded(t *testing.T) {
	SetPromptManager(nil)

	got := renderPrompt(promptNameLearnJargon, map[string]string{chatStrKey: "小明：yyds"})
	if strings.TrimSpace(got) == "" {
		t.Fatal("未注入 Manager 时渲染结果为空")
	}
	if !strings.Contains(got, "黑话") {
		t.Errorf("回落的内容不像黑话提示词：%q", truncateForLog(got, 80))
	}
	if !strings.Contains(got, "小明：yyds") {
		t.Errorf("聊天记录没有被填进去：%q", got)
	}
}

// TestJargonPromptTemplatesKeepPlaceholders 守两份含义推断模板的占位符齐全。
//
// 少了 {content} 就变成「请推断这个词条的含义」而没说哪个词，
// 模型会回一句「哪个词？」—— 表现是黑话含义一直是空的。
func TestJargonPromptTemplatesKeepPlaceholders(t *testing.T) {
	cases := map[string][]string{
		promptNameJargonWithContext: {"{content}", "{bot_name}", "{raw_content_list}"},
		promptNameJargonContentOnly: {"{content}"},
	}
	for name, want := range cases {
		content := promptTemplate(name)
		for _, ph := range want {
			if !strings.Contains(content, ph) {
				t.Errorf("%s 缺少占位符 %q", name, ph)
			}
		}
	}
}

// TestInferMeaningRendersContentIntoRequest 守含义推断的请求里真的有词条。
func TestInferMeaningRendersContentIntoRequest(t *testing.T) {
	SetPromptManager(nil)
	t.Cleanup(func() { SetPromptManager(nil) })

	got := renderPrompt(promptNameJargonContentOnly, map[string]string{"content": "yyds"})
	if !strings.Contains(got, "yyds") {
		t.Errorf("词条没有进请求：%q", got)
	}
	if strings.Contains(got, "{content}") {
		t.Errorf("占位符没有被替换：%q", got)
	}
}

// truncateForLog 按 rune 截断，避免失败信息里出现半个汉字。
func truncateForLog(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
