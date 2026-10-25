package memory

import (
	"strings"
	"testing"
)

// TestMergeProfilePreservesOldBullets 是合并逻辑的核心保证。
//
// 场景：第一次生成了「喜欢摄影」，第二次模型漏写了这条但写了「住校」。
// 覆盖会让「喜欢摄影」永久丢失；合并应该两条都在。
func TestMergeProfilePreservesOldBullets(t *testing.T) {
	existing := `# 人物画像
## 身份设定
- 大二学生
## 稳定了解
- 喜欢摄影
- 用旧胶片机`
	fresh := `# 人物画像
## 身份设定
- 大二学生
- 住校
## 稳定了解
- 喜欢摄影`

	got := MergeProfile(existing, fresh)

	// 旧信息必须还在。
	for _, want := range []string{"大二学生", "喜欢摄影", "用旧胶片机"} {
		if !strings.Contains(got, want) {
			t.Errorf("丢失了旧信息 %q：\n%s", want, got)
		}
	}
	// 新信息要合进来。
	if !strings.Contains(got, "住校") {
		t.Errorf("新信息没合进来：\n%s", got)
	}
}

// TestMergeProfileDedupes 验证重复要点只保留一条。
func TestMergeProfileDedupes(t *testing.T) {
	existing := `# 人物画像
## 稳定了解
- 喜欢摄影`
	fresh := `# 人物画像
## 稳定了解
- 喜欢摄影
- 喜欢摄影`

	got := MergeProfile(existing, fresh)

	if n := strings.Count(got, "喜欢摄影"); n != 1 {
		t.Errorf("「喜欢摄影」出现 %d 次，期望 1", n)
	}
}

// TestMergeProfileDedupesPunctuation 验证标点差异被归一化。
//
// 模型有时写「喜欢摄影。」有时写「喜欢摄影」，这是同一条信息。
func TestMergeProfileDedupesPunctuation(t *testing.T) {
	existing := `# 人物画像
## 稳定了解
- 喜欢摄影。`
	fresh := `# 人物画像
## 稳定了解
- 喜欢摄影`

	got := MergeProfile(existing, fresh)
	_, bullets := ProfileStats(got)
	if bullets != 1 {
		t.Errorf("要点数 = %d，期望 1（标点差异不该算两条）", bullets)
	}
}

// TestMergeProfileIgnoresPlaceholder 验证「- 暂无」不参与合并。
//
// 占位符不是信息。如果把它当成真实要点合并进去，
// 之后即使有了真实内容，「暂无」也会一直留在画像里。
func TestMergeProfileIgnoresPlaceholder(t *testing.T) {
	existing := `# 人物画像
## 稳定了解
- 暂无`
	fresh := `# 人物画像
## 稳定了解
- 喜欢摄影`

	got := MergeProfile(existing, fresh)

	// 「稳定了解」这一节有真实内容了，就不该再保留占位。
	// 注意不能断言整个结果里没有「暂无」—— 其它空分节本来就应该写「- 暂无」。
	parsed := parseProfileSections(got)
	if len(parsed["稳定了解"]) != 1 || parsed["稳定了解"][0] != "喜欢摄影" {
		t.Errorf("稳定了解 = %v，期望只有「喜欢摄影」", parsed["稳定了解"])
	}
}

// TestMergeProfileRespectsLimits 验证各分节条数上限。
//
// 不限制的话，同一个人聊几个月，某一节会积到几十条，注入时撑爆上下文。
func TestMergeProfileRespectsLimits(t *testing.T) {
	var existing strings.Builder
	existing.WriteString("# 人物画像\n## 稳定了解\n")
	for i := 0; i < 30; i++ {
		existing.WriteString("- 旧信息")
		existing.WriteString(string(rune('A' + i)))
		existing.WriteString("\n")
	}

	got := MergeProfile(existing.String(), "# 人物画像\n## 稳定了解\n")

	parsed := parseProfileSections(got)
	limit := limitFor("稳定了解")
	if n := len(parsed["稳定了解"]); n > limit {
		t.Errorf("要点数 = %d，超过上限 %d", n, limit)
	}
}

// TestMergeProfileKeepsAllSections 验证结果包含全部固定分节。
func TestMergeProfileKeepsAllSections(t *testing.T) {
	got := MergeProfile("", "# 人物画像\n## 稳定了解\n- 喜欢摄影\n")

	for _, title := range profileSectionTitles {
		if !strings.Contains(got, "## "+title) {
			t.Errorf("缺少分节 %q", title)
		}
	}
}

// TestMergeProfileEmptyExisting 验证旧画像为空时等价于规范化。
func TestMergeProfileEmptyExisting(t *testing.T) {
	fresh := "# 人物画像\n## 身份设定\n- 大二学生\n"

	got := MergeProfile("", fresh)

	if !strings.Contains(got, "大二学生") {
		t.Errorf("内容丢失：\n%s", got)
	}
	if !strings.HasPrefix(got, "# 人物画像") {
		t.Errorf("格式不对：\n%s", got)
	}
}

// TestMergeProfileOldFirst 验证旧条目排在新条目之前。
//
// 顺序有意义：注入时如果被截断，先被丢的是新条目，
// 而已经确认过的旧信息应该更稳。
func TestMergeProfileOldFirst(t *testing.T) {
	existing := "# 人物画像\n## 稳定了解\n- 旧要点\n"
	fresh := "# 人物画像\n## 稳定了解\n- 新要点\n"

	got := MergeProfile(existing, fresh)

	oldIdx := strings.Index(got, "旧要点")
	newIdx := strings.Index(got, "新要点")
	if oldIdx < 0 || newIdx < 0 {
		t.Fatalf("两条都该存在：\n%s", got)
	}
	if oldIdx > newIdx {
		t.Errorf("旧要点应排在新要点之前")
	}
}

// TestParseProfileSections 验证分节解析。
func TestParseProfileSections(t *testing.T) {
	text := `# 人物画像
这是前言，应被忽略
## 身份设定
- 大二学生
- 住校
## 稳定了解
- 喜欢摄影`

	got := parseProfileSections(text)

	if len(got["身份设定"]) != 2 {
		t.Errorf("身份设定 = %d 条，期望 2", len(got["身份设定"]))
	}
	if len(got["稳定了解"]) != 1 {
		t.Errorf("稳定了解 = %d 条，期望 1", len(got["稳定了解"]))
	}
}

// TestCleanBullet 验证要点前缀清理。
func TestCleanBullet(t *testing.T) {
	cases := map[string]string{
		"- 喜欢摄影":     "喜欢摄影",
		"* 喜欢摄影":     "喜欢摄影",
		"  - 喜欢摄影  ": "喜欢摄影",
		"- **加粗内容**": "加粗内容",
		"没有前缀":       "没有前缀",
	}

	for in, want := range cases {
		if got := cleanBullet(in); got != want {
			t.Errorf("cleanBullet(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// TestNormalizeBulletKey 验证去重键的归一化。
func TestNormalizeBulletKey(t *testing.T) {
	// 这些应该归一化成同一个键。
	same := []string{"喜欢摄影", "喜欢摄影。", "喜欢 摄影", "喜欢摄影.", "喜欢摄影 "}
	first := normalizeBulletKey(same[0])
	for _, s := range same[1:] {
		if got := normalizeBulletKey(s); got != first {
			t.Errorf("normalizeBulletKey(%q) = %q，期望与 %q 相同", s, got, first)
		}
	}

	// 这些不同，不该被合并。
	diff := []string{"喜欢摄影", "常拍照"}
	if normalizeBulletKey(diff[0]) == normalizeBulletKey(diff[1]) {
		t.Error("不同语义不该被归一化成同一个键")
	}
}

// TestProfileStats 验证画像统计。
func TestProfileStats(t *testing.T) {
	text := `# 人物画像
## 身份设定
- 大二学生
## 稳定了解
- 喜欢摄影
- 用旧胶片机
## 相处偏好
- 暂无`

	sections, bullets := ProfileStats(text)

	// 三个分节都出现过，但「暂无」不算要点，所以是 2 个有效分节、3 条要点。
	if sections != 2 {
		t.Errorf("有效分节 = %d，期望 2（相处偏好只有占位）", sections)
	}
	if bullets != 3 {
		t.Errorf("要点数 = %d，期望 3", bullets)
	}
}
