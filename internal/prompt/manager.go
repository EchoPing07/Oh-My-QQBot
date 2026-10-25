// Package prompt 负责加载与渲染提示词模板。
// 对应 MaiBot 的 src/prompt/prompt_manager.py。
package prompt

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

//go:embed embedded/zh-CN/*.prompt
var embeddedFS embed.FS

// Manager 管理提示词模板。
//
// 加载优先级：
//  1. 磁盘上的 prompts/<locale>/*.prompt（用户可改，重启生效）
//  2. 二进制内嵌的 embedded/zh-CN/*.prompt（兜底，保证永远有模板可用）
//
// 这个设计的意义是：默认能跑起来，同时允用户覆盖。
type Manager struct {
	mu     sync.RWMutex
	locale string
	tmpls  map[string]string
	dir    string
}

// New 创建管理器并从磁盘加载。
// dir 为空或目录不存在时使用内嵌模板。
func New(dir, locale string) (*Manager, error) {
	if locale == "" {
		locale = "zh-CN"
	}
	m := &Manager{
		locale: locale,
		tmpls:  make(map[string]string),
		dir:    dir,
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	return m, nil
}

// load 先装载内嵌模板，再用磁盘上的覆盖。
func (m *Manager) load() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.tmpls = make(map[string]string)

	// 1. 内嵌
	entries, err := embeddedFS.ReadDir("embedded/zh-CN")
	if err != nil {
		return fmt.Errorf("读取内嵌模板失败: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".prompt") {
			continue
		}
		raw, err := embeddedFS.ReadFile("embedded/zh-CN/" + e.Name())
		if err != nil {
			return fmt.Errorf("读取内嵌模板 %s 失败: %w", e.Name(), err)
		}
		name := strings.TrimSuffix(e.Name(), ".prompt")
		m.tmpls[name] = string(raw)
	}

	// 2. 磁盘覆盖
	if m.dir != "" {
		localeDir := filepath.Join(m.dir, m.locale)
		diskEntries, err := os.ReadDir(localeDir)
		if err == nil {
			for _, e := range diskEntries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".prompt") {
					continue
				}
				raw, err := os.ReadFile(filepath.Join(localeDir, e.Name()))
				if err != nil {
					return fmt.Errorf("读取模板 %s 失败: %w", e.Name(), err)
				}
				name := strings.TrimSuffix(e.Name(), ".prompt")
				m.tmpls[name] = string(raw)
			}
		}
	}

	return nil
}

// Reload 重新从磁盘加载，用于热更新提示词。
func (m *Manager) Reload() error { return m.load() }

// Get 返回原始模板。
func (m *Manager) Get(name string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.tmpls[name]
	return t, ok
}

// MustGet 返回模板，不存在时返回错误。
func (m *Manager) MustGet(name string) (string, error) {
	t, ok := m.Get(name)
	if !ok {
		return "", fmt.Errorf("提示词模板 %q 不存在", name)
	}
	return t, nil
}

// Names 返回所有模板名，按字典序。
func (m *Manager) Names() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.tmpls))
	for k := range m.tmpls {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Render 渲染模板，把 {key} 替换为 values[key]。
//
// 与 MaiBot 的模板语法一致：使用 {name} 占位符。
// 未提供的占位符会被替换为空串，不报错 —— 因为很多块按配置开关是可选的。
func Render(tmpl string, values map[string]string) string {
	if len(values) == 0 {
		return stripPlaceholders(tmpl)
	}
	return replacePlaceholders(tmpl, values)
}

// RenderNamed 按名字查模板并渲染。
func (m *Manager) RenderNamed(name string, values map[string]string) (string, error) {
	tmpl, err := m.MustGet(name)
	if err != nil {
		return "", err
	}
	return Render(tmpl, values), nil
}

// replacePlaceholders 单遍扫描，避免值里含 { 时被二次替换。
func replacePlaceholders(tmpl string, values map[string]string) string {
	var b strings.Builder
	b.Grow(len(tmpl))

	i := 0
	for i < len(tmpl) {
		open := strings.IndexByte(tmpl[i:], '{')
		if open < 0 {
			b.WriteString(tmpl[i:])
			break
		}
		open += i
		b.WriteString(tmpl[i:open])

		closeIdx := strings.IndexByte(tmpl[open:], '}')
		if closeIdx < 0 {
			b.WriteString(tmpl[open:])
			break
		}
		closeIdx += open

		key := tmpl[open+1 : closeIdx]

		// JSON 示例里的 {{"a":1}} 不是占位符，双花括号要原样保留。
		if strings.HasPrefix(key, "{") || strings.HasSuffix(key, "}") || strings.ContainsAny(key, " \n\t\":,") {
			b.WriteString(tmpl[open : closeIdx+1])
			i = closeIdx + 1
			continue
		}

		if v, ok := values[key]; ok {
			b.WriteString(v)
		}
		// 未提供的占位符替换为空。
		i = closeIdx + 1
	}
	return b.String()
}

// stripPlaceholders 在没有提供任何值时清掉合法占位符。
func stripPlaceholders(tmpl string) string {
	return replacePlaceholders(tmpl, nil)
}

// Dir 返回磁盘上提示词目录（可能为空，表示全用内嵌模板）。
func (m *Manager) Dir() string { return m.dir }

// Locale 返回当前语言。
func (m *Manager) Locale() string { return m.locale }

// EmbeddedDefault 返回内嵌的默认模板内容。
//
// 用途是「恢复默认」：用户改坏了可以对照原版，
// 也用于在编辑页显示「这一段原本是什么」。
func EmbeddedDefault(name string) (string, bool) {
	raw, err := embeddedFS.ReadFile("embedded/zh-CN/" + name + ".prompt")
	if err != nil {
		return "", false
	}
	return string(raw), true
}

// Placeholders 返回模板里出现的占位符名集合（去重、排好序）。
//
// 用途是保存时校验「自定义版本不能增删占位符」：
// 少了占位符，那段内容就会静默变空（例如 planner 少了 {bot_name}，
// 模型就不知道自己叫什么）；多了占位符则永远渲染成空串。
// 两种情况都不会报错，只表现为「机器人行为变得很奇怪」。
func Placeholders(tmpl string) []string {
	seen := make(map[string]struct{})
	i := 0
	for i < len(tmpl) {
		open := strings.IndexByte(tmpl[i:], '{')
		if open < 0 {
			break
		}
		open += i
		closeIdx := strings.IndexByte(tmpl[open:], '}')
		if closeIdx < 0 {
			break
		}
		closeIdx += open
		key := tmpl[open+1 : closeIdx]
		i = closeIdx + 1

		// JSON 示例里的 {{ 不是占位符：整段跳过，不做收集。
		if strings.HasPrefix(key, "{") || strings.HasSuffix(key, "}") || strings.ContainsAny(key, " \n\t\":,") {
			continue
		}
		seen[key] = struct{}{}
	}

	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ValidatePlaceholders 校验自定义内容与默认模板的占位符是否完全一致。
//
// 只比集合，不比顺序：模型看到的顺序由模板正文决定，
// 而正文本身可以随便改。少一个占位符会让那段内容静默变空，
// 多一个则永远渲染成空串 —— 两种都不会报错。
func ValidatePlaceholders(name, content string) error {
	def, ok := EmbeddedDefault(name)
	if !ok {
		return fmt.Errorf("提示词 %q 没有内置默认版本，无法校验占位符", name)
	}
	want := Placeholders(def)
	got := Placeholders(content)

	missing := diffStrings(want, got)
	extra := diffStrings(got, want)
	if len(missing) == 0 && len(extra) == 0 {
		return nil
	}

	parts := make([]string, 0, 2)
	if len(missing) > 0 {
		parts = append(parts, "缺少占位符: "+strings.Join(missing, ", "))
	}
	if len(extra) > 0 {
		parts = append(parts, "多余占位符: "+strings.Join(extra, ", "))
	}
	return fmt.Errorf("自定义提示词的占位符必须与默认版本完全一致（%s）", strings.Join(parts, "；"))
}

// diffStrings 返回 a 里有、b 里没有的元素（保持 a 的顺序）。
func diffStrings(a, b []string) []string {
	set := make(map[string]struct{}, len(b))
	for _, s := range b {
		set[s] = struct{}{}
	}
	var out []string
	for _, s := range a {
		if _, ok := set[s]; !ok {
			out = append(out, s)
		}
	}
	return out
}

// EmbeddedNames 返回内嵌模板名（即「全部内置提示词」）。
func EmbeddedNames() []string {
	entries, err := embeddedFS.ReadDir("embedded/zh-CN")
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".prompt") {
			continue
		}
		out = append(out, strings.TrimSuffix(e.Name(), ".prompt"))
	}
	sort.Strings(out)
	return out
}

// Save 把模板写到磁盘目录并重新加载。
//
// 写磁盘而不是只改内存：提示词是用户资产，重启后必须还在。
// 目录为空（全用内嵌模板）时返回错误而不是静默成功 ——
// 「点了保存、界面说成功、重启就没了」是最不能接受的一种失败。
func (m *Manager) Save(name, content string) error {
	if m.dir == "" {
		return fmt.Errorf("提示词目录未配置，无法保存（请在配置里指定 prompts 目录）")
	}
	// 名字来自用户输入，必须校验：带 ../ 就能写到目录外。
	if err := validatePromptName(name); err != nil {
		return err
	}

	localeDir := filepath.Join(m.dir, m.locale)
	if err := os.MkdirAll(localeDir, 0o755); err != nil {
		return fmt.Errorf("创建提示词目录失败: %w", err)
	}
	path := filepath.Join(localeDir, name+".prompt")

	// 临时文件 + rename：写到一半断电不会留下半截模板，
	// 那会让程序下次启动时读到语法完整但内容残缺的提示词。
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return fmt.Errorf("写入提示词失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("替换提示词失败: %w", err)
	}
	return m.load()
}

// Reset 删除磁盘上的覆盖版本，退回内嵌默认模板。
func (m *Manager) Reset(name string) error {
	if m.dir == "" {
		return fmt.Errorf("提示词目录未配置")
	}
	if err := validatePromptName(name); err != nil {
		return err
	}
	path := filepath.Join(m.dir, m.locale, name+".prompt")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除提示词失败: %w", err)
	}
	return m.load()
}

// Customized 判断某个模板是否被磁盘版本覆盖。
func (m *Manager) Customized(name string) bool {
	if m.dir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(m.dir, m.locale, name+".prompt"))
	return err == nil
}

// validatePromptName 挡住路径穿越。
//
// 名字会拼进文件路径，允许 ".." 与分隔符就等于允许任意文件写入。
// 模板名只可能是 planner / replyer 这类标识符。
func validatePromptName(name string) error {
	if name == "" {
		return fmt.Errorf("提示词名称不能为空")
	}
	if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return fmt.Errorf("提示词名称 %q 非法", name)
	}
	for _, r := range name {
		ok := r == '_' || r == '-' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !ok {
			return fmt.Errorf("提示词名称 %q 含非法字符", name)
		}
	}
	return nil
}
