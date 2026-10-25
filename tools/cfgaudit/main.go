// Command cfgaudit 检查每个配置项是否真的被生产代码读取。
//
// 存在理由：AGENTS.md 第 50 / 61 条反复踩过同一个坑 ——
// 配置项写在 struct 里、config.toml 里能改，但**没有任何代码读它**，
// 表现是「改了配置完全没反应，日志也不报错」。这种问题编译期看不出来，
// 单测也不会失败（因为没人测「这个配置有没有用」）。
//
// 做法：解析 config 包的 struct tag 得到全部点分路径，再到生产代码里
// 找最后一次出现的字段名。
//
// 已知误报（手工确认过，改动这几处时记得同步）：
//   - llm.providers.{timeout,max_retry,retry_interval}
//     通过 ProviderConfig.TimeoutDuration() / Retry() /
//     RetryIntervalDuration() 三个方法读取（internal/config/config.go），
//     而方法体属于配置包本身，被扫描排除掉了。
//   - chat.timing.private_talk_value
//     通过 Config.TalkValue(isGroup) 读取，同理。
//
// 这类「经由配置包内的方法读取」的字段，本工具数不到 —— 它只认
// `.Field` 形式的选择器。把它们列进 waivedPaths 而不是改判据：
// 判据改成「也算上配置包自身」就再也查不出「只有 getter 没有调用方」了。
//
//	go run ./tools/cfgaudit
//
// 退出码非 0 表示存在零使用的配置项。
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// waivedPaths 是「已确认经由配置包内方法读取」的字段，不算漏接线。
var waivedPaths = map[string]bool{
	"llm.providers.timeout":          true,
	"llm.providers.max_retry":        true,
	"llm.providers.retry_interval":   true,
	"chat.timing.private_talk_value": true,
}

func main() {
	root, err := repoRoot()
	if err != nil {
		fail("定位仓库根目录失败: %v", err)
	}

	fields, err := parseConfigFields(filepath.Join(root, "internal", "config"))
	if err != nil {
		fail("解析配置结构失败: %v", err)
	}
	if len(fields) == 0 {
		fail("没有解析到任何配置字段")
	}

	used, err := usedFieldNames(root)
	if err != nil {
		fail("扫描生产代码失败: %v", err)
	}

	type row struct {
		path  string
		field string
		hits  []string
	}
	rows := make([]row, 0, len(fields))
	for _, f := range fields {
		rows = append(rows, row{path: f.Path, field: f.Field, hits: used[f.Field]})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].path < rows[j].path })

	zero := 0
	one := 0
	for _, r := range rows {
		switch len(r.hits) {
		case 0:
			if waivedPaths[r.path] {
				continue
			}
			zero++
			fmt.Printf("  ✗ 零使用：%s  （字段 %s）\n", r.path, r.field)
		case 1:
			one++
			fmt.Printf("  · 仅一处：%s  →  %s\n", r.path, r.hits[0])
		}
	}

	fmt.Printf("\n共 %d 个配置项；零使用 %d 个，单点使用 %d 个\n", len(rows), zero, one)
	if zero > 0 {
		os.Exit(1)
	}
}

// configField 是一个配置项。
type configField struct {
	Path  string // 点分路径，如 chat.timing.talk_value
	Field string // Go 字段名，如 TalkValue
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("找不到 go.mod")
		}
		dir = parent
	}
}

// parseConfigFields 解析 internal/config 下所有 struct，展开成点分路径。
func parseConfigFields(dir string) ([]configField, error) {
	// structName -> 字段列表
	type sfield struct {
		name string
		typ  string
		toml string
	}
	structs := map[string][]sfield{}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") ||
			strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, 0)
		if err != nil {
			return nil, err
		}
		ast.Inspect(file, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				return true
			}
			var list []sfield
			for _, f := range st.Fields.List {
				if f.Tag == nil || len(f.Names) == 0 {
					continue
				}
				tag, err := strconv.Unquote(f.Tag.Value)
				if err != nil {
					continue
				}
				tomlTag := reflect.StructTag(tag).Get("toml")
				if tomlTag == "" || tomlTag == "-" {
					continue
				}
				typ := typeName(f.Type)
				for _, name := range f.Names {
					list = append(list, sfield{name: name.Name, typ: typ, toml: tomlTag})
				}
			}
			structs[ts.Name.Name] = list
			return true
		})
	}

	var out []configField
	var walk func(name, prefix string, depth int)
	walk = func(name, prefix string, depth int) {
		if depth > 4 {
			return
		}
		for _, f := range structs[name] {
			p := f.toml
			if prefix != "" {
				p = prefix + "." + f.toml
			}
			if _, isStruct := structs[f.typ]; isStruct {
				walk(f.typ, p, depth+1)
				continue
			}
			out = append(out, configField{Path: p, Field: f.name})
		}
	}
	walk("Config", "", 0)
	return out, nil
}

// typeName 取出一个类型的名字（去掉指针与切片）。
func typeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return typeName(t.X)
	case *ast.ArrayType:
		return typeName(t.Elt)
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return t.Sel.Name
	}
	return ""
}

// usedFieldNames 扫描生产代码，统计每个「.字段名」在哪些文件里出现。
//
// 只认 `.Foo` 这种选择器表达式，不认声明与赋值目标 ——
// 那正是「配置写了但没人读」的形态。
func usedFieldNames(root string) (map[string][]string, error) {
	out := map[string][]string{}
	seen := map[string]map[string]bool{}

	fset := token.NewFileSet()
	err := filepath.Walk(filepath.Join(root, "internal"), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		norm := filepath.ToSlash(path)
		if strings.HasPrefix(norm[strings.Index(norm, "internal/"):], "internal/config/") {
			return nil // 配置包自身不算「使用」
		}
		if strings.Contains(norm, "internal/mock/") {
			return nil // 测试脚手架不算
		}

		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			name := sel.Sel.Name
			if seen[name] == nil {
				seen[name] = map[string]bool{}
			}
			if !seen[name][norm] {
				seen[name][norm] = true
				rel, _ := filepath.Rel(root, path)
				out[name] = append(out[name], filepath.ToSlash(rel))
			}
			return true
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
