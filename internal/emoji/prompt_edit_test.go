package emoji

// 本文件守「表情包打标签的提示词可被用户在 WebUI 上改掉」。
//
// 这份模板以前是包里的 const（analysisPrompt）。它决定表情包被写成什么标签，
// 而标签直接决定之后能不能按情绪检索到那张图 ——
// 用内嵌默认文案时用户完全没有调整余地。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/prompt"
)

// stubEmojiPrompts 写一份覆盖模板并注入。
func stubEmojiPrompts(t *testing.T, content string) {
	t.Helper()

	dir := t.TempDir()
	localeDir := filepath.Join(dir, "zh-CN")
	if err := os.MkdirAll(localeDir, 0o755); err != nil {
		t.Fatalf("创建提示词目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localeDir, promptNameAnalysis+".prompt"), []byte(content), 0o644); err != nil {
		t.Fatalf("写入提示词失败: %v", err)
	}
	pm, err := prompt.New(dir, "zh-CN")
	if err != nil {
		t.Fatalf("加载提示词失败: %v", err)
	}
	SetPromptManager(pm)
	t.Cleanup(func() { SetPromptManager(nil) })
}

// TestAnalysisPromptIsOverridable 守打标签读的是注入的模板。
func TestAnalysisPromptIsOverridable(t *testing.T) {
	stubEmojiPrompts(t, "OVERRIDDEN-EMOJI 类型={image_type}")

	got := renderPrompt(promptNameAnalysis, map[string]string{"image_type": "GIF"})
	if !strings.Contains(got, "OVERRIDDEN-EMOJI") {
		t.Errorf("没有用覆盖模板：%q", got)
	}
	if !strings.Contains(got, "类型=GIF") {
		t.Errorf("占位符没有被替换：%q", got)
	}
}

// TestAnalysisPromptFallsBackToEmbedded 守「未注入 Manager 时也不为空」。
//
// 空提示词会让 VLM 输出完全不可预期的标签，而那些标签会进库、
// 影响之后的检索质量 —— 且没有任何报错。
func TestAnalysisPromptFallsBackToEmbedded(t *testing.T) {
	SetPromptManager(nil)

	got := renderPrompt(promptNameAnalysis, map[string]string{"image_type": "STATIC"})
	if strings.TrimSpace(got) == "" {
		t.Fatal("未注入 Manager 时打标签提示词为空")
	}
	if !strings.Contains(got, "标签") {
		t.Errorf("回落内容不像表情包打标签提示词：%q", got)
	}
	// GIF 与 STATIC 两种取值都要能替换掉占位符。
	if strings.Contains(got, "{image_type}") {
		t.Errorf("占位符没有被替换：%q", got)
	}
}
