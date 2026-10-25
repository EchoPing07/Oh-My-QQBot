package prompt

// 提示词的保存 / 恢复默认 / 名字校验测试。
//
// 与 manager_test.go 分开：那个文件测的是「加载与渲染」，
// 这个测的是「编辑与写盘」。混在一起的话，
// manager_test.go 已经有 head() 之类的辅助，新加的东西会撞名。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSaveAndCustomized 覆盖保存 → 已改标记 → 恢复默认 的完整循环。
func TestSaveAndCustomized(t *testing.T) {
	dir := t.TempDir()
	m, err := New(dir, "zh-CN")
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}

	if m.Customized("planner") {
		t.Error("刚创建不该是已改状态")
	}

	if err := m.Save("planner", "新内容 {bot_name}"); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if got, _ := m.Get("planner"); got != "新内容 {bot_name}" {
		t.Errorf("保存后读到 %q", got)
	}
	if !m.Customized("planner") {
		t.Error("保存后应为已改状态")
	}

	// 磁盘上必须真的有这个文件（重启后还在）。
	path := filepath.Join(dir, "zh-CN", "planner.prompt")
	if _, err := os.Stat(path); err != nil {
		t.Errorf("磁盘上没有写入文件: %v", err)
	}

	// 恢复默认。
	if err := m.Reset("planner"); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	if m.Customized("planner") {
		t.Error("恢复后不该是已改状态")
	}
	got, _ := m.Get("planner")
	if !strings.Contains(got, "你的名字是{bot_name}") {
		t.Errorf("恢复后应回到内嵌默认，实际 %q", head(got, 40))
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("恢复默认后磁盘文件应当被删掉")
	}
}

// TestSaveWithoutDirFails 防：没配目录时静默成功。
//
// 静默成功的表现是「界面上说保存了，重启就没了」，
// 这比报错难查得多。
func TestSaveWithoutDirFails(t *testing.T) {
	m, err := New("", "zh-CN")
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if err := m.Save("planner", "x"); err == nil {
		t.Error("未配置目录时保存应当报错")
	}
	if err := m.Reset("planner"); err == nil {
		t.Error("未配置目录时恢复应当报错")
	}
}

// TestValidatePromptName 防：路径穿越与非法字符。
//
// 名字会拼进文件路径，允许 ".." 就等于允许任意文件写入。
func TestValidatePromptName(t *testing.T) {
	bad := []string{"", "..", "../x", "a/b", `a\b`, "a.b", "名字", "a b"}
	for _, name := range bad {
		if err := validatePromptName(name); err == nil {
			t.Errorf("%q 应当被拒绝", name)
		}
	}
	good := []string{"planner", "reply_attachments", "image-description", "a1"}
	for _, name := range good {
		if err := validatePromptName(name); err != nil {
			t.Errorf("%q 应当被接受: %v", name, err)
		}
	}
}

// TestEmbeddedDefault 覆盖默认内容的读取。
func TestEmbeddedDefault(t *testing.T) {
	got, ok := EmbeddedDefault("planner")
	if !ok || got == "" {
		t.Fatal("读不到 planner 的默认内容")
	}
	if _, ok := EmbeddedDefault("不存在的"); ok {
		t.Error("不存在的模板不该返回 ok")
	}

	names := EmbeddedNames()
	if len(names) == 0 {
		t.Fatal("内嵌模板为空")
	}
}

// TestSaveIsAtomic 防：写到一半断电留下半截模板。
//
// 实现是「写 .tmp 再 rename」，所以目录里不该残留 .tmp。
func TestSaveLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	m, err := New(dir, "zh-CN")
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if err := m.Save("planner", "内容"); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	entries, err := os.ReadDir(filepath.Join(dir, "zh-CN"))
	if err != nil {
		t.Fatalf("读目录失败: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("残留临时文件 %s", e.Name())
		}
	}
}

// TestSaveCreatesLocaleDir 防：目标目录不存在时保存失败。
func TestSaveCreatesLocaleDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "不存在", "的", "目录")
	m, err := New(dir, "zh-CN")
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if err := m.Save("planner", "内容"); err != nil {
		t.Fatalf("应当自动创建目录，实际失败: %v", err)
	}
}
