package prompt

// 本文件覆盖提示词渲染与 Manager 的加载/覆盖/重载。
// 只新建测试文件，不修改生产代码。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRender 表驱动覆盖占位符替换的边界情况。
func TestRender(t *testing.T) {
	cases := []struct {
		name   string
		tmpl   string
		values map[string]string
		want   string
	}{
		{
			name:   "普通占位符",
			tmpl:   "你的名字是{bot_name}。",
			values: map[string]string{"bot_name": "木木"},
			want:   "你的名字是木木。",
		},
		{
			name:   "多个占位符",
			tmpl:   "{a}-{b}-{a}",
			values: map[string]string{"a": "1", "b": "2"},
			want:   "1-2-1",
		},
		{
			// 很多块按配置开关是可选的，缺失不能报错。
			name:   "缺失占位符替换为空",
			tmpl:   "开始{missing}结束",
			values: map[string]string{"other": "x"},
			want:   "开始结束",
		},
		{
			name:   "values 为空时清掉全部占位符",
			tmpl:   "a{one}b{two}c",
			values: nil,
			want:   "abc",
		},
		{
			// JSON 示例里的花括号不是占位符，不能被吞掉。
			name:   "JSON 花括号原样保留",
			tmpl:   `参数示例：{"a":1}`,
			values: map[string]string{"a": "被替换就错了"},
			want:   `参数示例：{"a":1}`,
		},
		{
			name:   "双花括号 JSON 原样保留",
			tmpl:   `示例 {{"a":1}} 结束`,
			values: map[string]string{"a": "x"},
			want:   `示例 {{"a":1}} 结束`,
		},
		{
			// 值里含 { 时不能被二次替换，否则值里的花括号会被当成占位符。
			name:   "值里的花括号不被二次替换",
			tmpl:   "前缀 {value} 后缀",
			values: map[string]string{"value": `{"raw":{nested}}`},
			want:   `前缀 {"raw":{nested}} 后缀`,
		},
		{
			name:   "未闭合的花括号原样保留",
			tmpl:   "前缀 {未闭合",
			values: map[string]string{"未闭合": "x"},
			want:   "前缀 {未闭合",
		},
		{
			name:   "空模板",
			tmpl:   "",
			values: map[string]string{"a": "1"},
			want:   "",
		},
		{
			name:   "没有占位符时原样返回",
			tmpl:   "纯文本",
			values: map[string]string{"a": "1"},
			want:   "纯文本",
		},
		{
			name:   "值可以是空串",
			tmpl:   "前{v}后",
			values: map[string]string{"v": ""},
			want:   "前后",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Render(c.tmpl, c.values); got != c.want {
				t.Errorf("Render() = %q，期望 %q", got, c.want)
			}
		})
	}
}

// TestRenderNoDoubleSubstitution 单独强调「值里的占位符形状不被再展开」：
// 用户输入或数据库内容进了提示词时，二次替换会变成提示词注入。
func TestRenderNoDoubleSubstitution(t *testing.T) {
	got := Render("{outer}", map[string]string{
		"outer": "{inner}",
		"inner": "不该出现",
	})
	if got != "{inner}" {
		t.Errorf("Render() = %q，期望 %q（单遍替换）", got, "{inner}")
	}
	if strings.Contains(got, "不该出现") {
		t.Error("值里的占位符被二次替换了")
	}
}

// TestNewLoadsEmbeddedTemplates 校验内嵌模板一定能加载到。
func TestNewLoadsEmbeddedTemplates(t *testing.T) {
	m, err := New("", "")
	if err != nil {
		t.Fatalf("创建 Manager 失败: %v", err)
	}

	// locale 为空时默认 zh-CN。
	if m.locale != "zh-CN" {
		t.Errorf("locale = %q，期望 zh-CN", m.locale)
	}

	want := []string{"emoji_content_analysis", "planner", "reply_attachments", "replyer"}
	for _, name := range want {
		if _, ok := m.Get(name); !ok {
			t.Errorf("缺少内嵌模板 %q", name)
		}
	}

	names := m.Names()
	if len(names) < len(want) {
		t.Fatalf("Names() = %v，期望至少包含 %v", names, want)
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] >= names[i] {
			t.Fatalf("Names() 未按字典序排列: %v", names)
		}
	}

	plan, err := m.MustGet("planner")
	if err != nil {
		t.Fatalf("MustGet(planner) 失败: %v", err)
	}
	if !strings.Contains(plan, "{bot_name}") {
		t.Error("planner 模板应包含 {bot_name} 占位符")
	}
}

// TestManagerDiskOverridesEmbedded 防的是「用户改了 prompts/ 目录但不生效」。
func TestManagerDiskOverridesEmbedded(t *testing.T) {
	dir := t.TempDir()
	localeDir := filepath.Join(dir, "zh-CN")
	if err := os.MkdirAll(localeDir, 0o755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localeDir, "planner.prompt"), []byte("磁盘版 {bot_name}"), 0o644); err != nil {
		t.Fatalf("写入模板失败: %v", err)
	}
	// 磁盘新增的模板也要能加载。
	if err := os.WriteFile(filepath.Join(localeDir, "extra.prompt"), []byte("额外模板"), 0o644); err != nil {
		t.Fatalf("写入模板失败: %v", err)
	}
	// 非 .prompt 文件应被忽略。
	if err := os.WriteFile(filepath.Join(localeDir, "note.txt"), []byte("忽略我"), 0o644); err != nil {
		t.Fatalf("写入文件失败: %v", err)
	}

	m, err := New(dir, "zh-CN")
	if err != nil {
		t.Fatalf("创建 Manager 失败: %v", err)
	}

	plan, _ := m.Get("planner")
	if plan != "磁盘版 {bot_name}" {
		t.Errorf("planner = %q，期望磁盘版本覆盖内嵌版本", plan)
	}
	if _, ok := m.Get("extra"); !ok {
		t.Error("磁盘新增的模板应被加载")
	}
	if _, ok := m.Get("note"); ok {
		t.Error("非 .prompt 文件不应被加载")
	}
	// 未被覆盖的内嵌模板仍需保留。
	if _, ok := m.Get("replyer"); !ok {
		t.Error("未被覆盖的内嵌模板不应丢失")
	}
}

// TestManagerReloadPicksUpChanges 校验热更新路径。
func TestManagerReloadPicksUpChanges(t *testing.T) {
	dir := t.TempDir()
	localeDir := filepath.Join(dir, "zh-CN")
	if err := os.MkdirAll(localeDir, 0o755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	path := filepath.Join(localeDir, "planner.prompt")
	if err := os.WriteFile(path, []byte("第一版"), 0o644); err != nil {
		t.Fatalf("写入模板失败: %v", err)
	}

	m, err := New(dir, "zh-CN")
	if err != nil {
		t.Fatalf("创建 Manager 失败: %v", err)
	}
	if got, _ := m.Get("planner"); got != "第一版" {
		t.Fatalf("planner = %q，期望 %q", got, "第一版")
	}

	if err := os.WriteFile(path, []byte("第二版"), 0o644); err != nil {
		t.Fatalf("改写模板失败: %v", err)
	}
	if err := m.Reload(); err != nil {
		t.Fatalf("Reload 失败: %v", err)
	}
	if got, _ := m.Get("planner"); got != "第二版" {
		t.Errorf("Reload 后 planner = %q，期望 %q", got, "第二版")
	}
}

// TestManagerMustGetMissing 校验缺失模板报错。
func TestManagerMustGetMissing(t *testing.T) {
	m, err := New("", "zh-CN")
	if err != nil {
		t.Fatalf("创建 Manager 失败: %v", err)
	}
	if _, err := m.MustGet("nope"); err == nil {
		t.Fatal("缺失模板应报错")
	} else if !strings.Contains(err.Error(), "nope") {
		t.Errorf("错误信息 = %q，期望包含模板名", err.Error())
	}

	if _, ok := m.Get("nope"); ok {
		t.Error("Get 未命中时应返回 false")
	}
}

// TestManagerRenderNamed 校验按名渲染。
func TestManagerRenderNamed(t *testing.T) {
	m, err := New("", "zh-CN")
	if err != nil {
		t.Fatalf("创建 Manager 失败: %v", err)
	}

	got, err := m.RenderNamed("planner", map[string]string{
		"bot_name":       "木木",
		"behavior_style": "平淡简短",
	})
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	if !strings.Contains(got, "你的名字是木木。") {
		t.Errorf("渲染结果缺少名字，实际前 100 字:\n%s", head(got, 100))
	}
	if strings.Contains(got, "{bot_name}") {
		t.Error("渲染结果不应残留未替换的占位符")
	}
	if strings.Contains(got, "{query_memory_rule}") {
		t.Error("未提供的占位符应被清空")
	}

	if _, err := m.RenderNamed("nope", nil); err == nil {
		t.Error("渲染不存在的模板应报错")
	}
}

// TestManagerUnknownLocaleFallsBackToEmbedded 校验磁盘没有该语言目录时仍能用内嵌模板。
func TestManagerUnknownLocaleFallsBackToEmbedded(t *testing.T) {
	m, err := New(t.TempDir(), "en-US")
	if err != nil {
		t.Fatalf("创建 Manager 失败: %v", err)
	}
	if _, ok := m.Get("planner"); !ok {
		t.Error("磁盘没有该语言目录时应回退到内嵌模板")
	}
}

// head 截取前 n 个字节，仅用于报错信息可读。
func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
