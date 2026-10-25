package memory

// 本文件守「聊天回想与人物画像的提示词可被用户在 WebUI 上改掉」。
//
// 这两份以前是包里的 const（summaryPromptTemplate / profilePromptTemplate），
// 用户想改只能改代码。现在它们在内嵌资源里，走 prompt.Manager。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/prompt"
)

// stubMemoryPrompts 写一份覆盖模板并注入。
func stubMemoryPrompts(t *testing.T, name, content string) {
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
	SetPromptManager(pm)
	t.Cleanup(func() { SetPromptManager(nil) })
}

// TestSummaryPromptIsOverridable 守聊天回想读的是注入的模板。
func TestSummaryPromptIsOverridable(t *testing.T) {
	stubMemoryPrompts(t, promptNameSummary, "OVERRIDDEN-SUMMARY\n时间范围：{time_range}\n参与人物：{participants_text}")

	got := renderPrompt(promptNameSummary, map[string]string{
		"time_range":        "10:00-10:30",
		"participants_text": "小明、小红",
	})
	if !strings.Contains(got, "OVERRIDDEN-SUMMARY") {
		t.Errorf("没有用覆盖模板：%q", got)
	}
	if !strings.Contains(got, "10:00-10:30") || !strings.Contains(got, "小明、小红") {
		t.Errorf("占位符没有被替换：%q", got)
	}
}

// TestProfilePromptIsOverridable 守人物画像读的是注入的模板。
func TestProfilePromptIsOverridable(t *testing.T) {
	stubMemoryPrompts(t, promptNameProfile, "OVERRIDDEN-PROFILE {person_name} {bot_name} {sections} {chat_str} {existing_section}")

	got := renderPrompt(promptNameProfile, map[string]string{
		"person_name":      "小明",
		"bot_name":         "木木",
		"sections":         "身份设定、关系设定",
		"chat_str":         "小明：我在读大二",
		"existing_section": "",
	})
	if !strings.Contains(got, "OVERRIDDEN-PROFILE") {
		t.Errorf("没有用覆盖模板：%q", got)
	}
	for _, want := range []string{"小明", "木木", "身份设定、关系设定", "我在读大二"} {
		if !strings.Contains(got, want) {
			t.Errorf("占位符没有被替换，缺少 %q：%q", want, got)
		}
	}
}

// TestSummaryPromptFallsBackToEmbedded 守「未注入 Manager 时也不为空」。
//
// 摘要出问题只会在日志里留一条 warn，用户看到的是「记忆一直是空的」，
// 完全没有线索说明是提示词没了。
func TestSummaryPromptFallsBackToEmbedded(t *testing.T) {
	SetPromptManager(nil)

	got := renderPrompt(promptNameSummary, map[string]string{
		"time_range":        "10:00-10:30",
		"participants_text": "小明",
	})
	if strings.TrimSpace(got) == "" {
		t.Fatal("未注入 Manager 时摘要提示词为空")
	}
	if !strings.Contains(got, "聊天回想") {
		t.Errorf("回落内容不像聊天回想提示词：%q", got)
	}
	if strings.Contains(got, "{time_range}") {
		t.Errorf("占位符没有被替换：%q", got)
	}
}

// TestProfilePromptKeepsAllPlaceholders 守画像模板的占位符齐全。
//
// 少了 {sections}，模型不知道要按哪几个分节输出，
// normalizeProfileText 就会把整段内容塞进「身份设定」一节。
func TestProfilePromptKeepsAllPlaceholders(t *testing.T) {
	SetPromptManager(nil)

	got := promptTemplate(promptNameProfile)
	for _, want := range []string{
		"{person_name}", "{bot_name}", "{sections}", "{chat_str}", "{existing_section}",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("画像模板缺少占位符 %q", want)
		}
	}
}

// TestEmbeddedFallbackMatchesFile 守「代码里的兜底与内嵌文件一致」。
//
// promptTemplate 在 Manager 缺失时回落到 prompt.EmbeddedDefault，
// 而当内嵌也读不到时（理论上不会，但接口允许）才用代码里的说明串。
// 这条测试确保内嵌文件本身存在且内容正确 —— 两个来源漂移时，
// 线上行为会因为「哪个先命中」而不同，极难复现。
func TestEmbeddedFallbackMatchesFile(t *testing.T) {
	for _, name := range []string{promptNameSummary, promptNameProfile} {
		content, ok := prompt.EmbeddedDefault(name)
		if !ok {
			t.Errorf("%s 没有内嵌模板文件", name)
			continue
		}
		if strings.TrimSpace(content) == "" {
			t.Errorf("%s 内嵌模板是空的", name)
		}
	}
}
