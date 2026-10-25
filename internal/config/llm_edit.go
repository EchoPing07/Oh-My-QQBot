package config

// 本文件实现「模型管理」这一块的结构化读写。
//
// 为什么不复用 fields.go 的平铺表单：
// LLM 这一节是三层嵌套（providers[] / models[] / tasks{}），
// 平铺点分键表达不了「第 2 个厂商的 base_url」这种定位，
// 而且模型列表会增删，不能靠固定字段名。
//
// 与 MaiBot 的对应关系（refs/MaiBot/dashboard/src/routes/config/model）：
//   - 「模型设置」页签 = 厂商侧栏 + 模型列表（ProviderSidebar + ModelTable）
//   - 「功能分配」页签 = 任务列表 + 任务配置卡（TaskConfigCard）
// 我们照抄这个两页签结构，字段按自己 config 的实际定义来。
//
// 安全边界：api_key 永远不出现在响应里（只回 has_api_key 布尔），
// 提交时留空表示「不修改」，填了新值才覆盖。

import (
	"fmt"
	"strings"
)

// ProviderView 是返回给前端的服务商视图。
//
// 刻意不含 api_key：前端只需要知道「有没有配」，
// 回显密钥等于把密钥写进浏览器缓存与开发者工具的历史里。
// 对应的写入路径见 ApplyProviderPatch，用 ApiKey 是否为空区分
// 「保留原值」与「改成新值」。
type ProviderView struct {
	Name             string `json:"name"`
	BaseURL          string `json:"base_url"`
	AuthType         string `json:"auth_type"`
	AuthHeaderName   string `json:"auth_header_name"`
	AuthHeaderPrefix string `json:"auth_header_prefix"`
	AuthQueryName    string `json:"auth_query_name"`
	Timeout          int    `json:"timeout"`
	MaxRetry         int    `json:"max_retry"`
	RetryInterval    int    `json:"retry_interval"`
	// HasAPIKey 只表示「已配置」，不回显值。
	HasAPIKey bool `json:"has_api_key"`
	// ModelCount 是该厂商下的模型数，前端用来在侧栏显示徽标。
	ModelCount int `json:"model_count"`
}

// ModelView 是返回给前端的模型视图。
//
// Temperature / MaxTokens / Dimension 用指针：
// nil 表示「沿用任务级默认」，与 0 是两个不同的意思
// （temperature=0 是合法的确定性输出）。
type ModelView struct {
	Name            string   `json:"name"`
	ModelIdentifier string   `json:"model_identifier"`
	Provider        string   `json:"provider"`
	Visual          bool     `json:"visual"`
	ForceStream     bool     `json:"force_stream"`
	Temperature     *float64 `json:"temperature"`
	MaxTokens       *int     `json:"max_tokens"`
	Dimension       *int     `json:"dimension"`
	PriceIn         float64  `json:"price_in"`
	PriceOut        float64  `json:"price_out"`
}

// TaskView 是返回给前端的任务绑定视图。
type TaskView struct {
	Name              string   `json:"name"`
	Models            []string `json:"models"`
	MaxTokens         int      `json:"max_tokens"`
	Temperature       float64  `json:"temperature"`
	SelectionStrategy string   `json:"selection_strategy"`
	HardTimeout       float64  `json:"hard_timeout"`
}

// LLMView 是「模型管理」页需要的全部数据，一次返回。
type LLMView struct {
	Providers []ProviderView `json:"providers"`
	Models    []ModelView    `json:"models"`
	Tasks     []TaskView     `json:"tasks"`
	// TaskOrder 固定任务顺序，前端按它排列侧栏。
	// 不从 map 里取：Go 的 map 遍历顺序随机，界面每次刷新都会换位置。
	TaskOrder []string `json:"task_order"`
}

// taskOrder 是任务的展示顺序，与 MaiBot 的 TASK_CONFIGS 顺序一致（去掉了我们没做的）。
//
// 顺序有讲究：planner / replyer 最常用放最前，
// embedding 是「配错了整套记忆检索就不可用」，放最后但要显眼。
var taskOrder = []string{"planner", "replyer", "utils", "learner", "vlm", "embedding"}

// TaskOrder 返回任务展示顺序。
func TaskOrder() []string { return append([]string(nil), taskOrder...) }

// LLMSnapshot 导出模型相关配置，供 WebUI 使用。
func (c *Config) LLMSnapshot() LLMView {
	// 先数一遍每个厂商下的模型数，避免前端再做一次分组。
	counts := make(map[string]int, len(c.LLM.Providers))
	for _, m := range c.LLM.Models {
		counts[m.Provider]++
	}

	out := LLMView{
		Providers: make([]ProviderView, 0, len(c.LLM.Providers)),
		Models:    make([]ModelView, 0, len(c.LLM.Models)),
		Tasks:     make([]TaskView, 0, len(taskOrder)),
		TaskOrder: TaskOrder(),
	}

	for _, p := range c.LLM.Providers {
		out.Providers = append(out.Providers, ProviderView{
			Name:             p.Name,
			BaseURL:          p.BaseURL,
			AuthType:         p.AuthType,
			AuthHeaderName:   p.AuthHeaderName,
			AuthHeaderPrefix: p.AuthHeaderPrefix,
			AuthQueryName:    p.AuthQueryName,
			Timeout:          p.Timeout,
			MaxRetry:         p.MaxRetry,
			RetryInterval:    p.RetryInterval,
			HasAPIKey:        p.APIKey != "",
			ModelCount:       counts[p.Name],
		})
	}

	for _, m := range c.LLM.Models {
		out.Models = append(out.Models, ModelView{
			Name:            m.Name,
			ModelIdentifier: m.ModelIdentifier,
			Provider:        m.Provider,
			Visual:          m.Visual,
			ForceStream:     m.ForceStream,
			Temperature:     m.Temperature,
			MaxTokens:       m.MaxTokens,
			Dimension:       m.Dimension,
			PriceIn:         m.PriceIn,
			PriceOut:        m.PriceOut,
		})
	}

	for _, name := range taskOrder {
		t := c.LLM.Tasks.byName(name)
		out.Tasks = append(out.Tasks, TaskView{
			Name:              name,
			Models:            append([]string{}, t.Models...),
			MaxTokens:         t.MaxTokens,
			Temperature:       t.Temperature,
			SelectionStrategy: t.SelectionStrategy,
			HardTimeout:       t.HardTimeout,
		})
	}
	return out
}

// taskByName 返回指向内部字段的指针，供读与写共用。
//
// 返回指针而不是值：写路径必须改到 c.LLM.Tasks 里的真实字段，
// 返回值拷贝的话「保存成功但没生效」——而且完全不报错。
func (t *TasksConfig) byName(name string) *TaskModelConfig {
	switch name {
	case "planner":
		return &t.Planner
	case "replyer":
		return &t.Replyer
	case "utils":
		return &t.Utils
	case "learner":
		return &t.Learner
	case "vlm":
		return &t.VLM
	case "embedding":
		return &t.Embedding
	}
	return nil
}

// ProviderPatch 是厂商的一次修改。
//
// 用「整条替换 + 密钥特例」而不是逐字段补丁：
// 厂商表单本来就是一次填一整条，逐字段补丁会让前端为了
// 「改了个超时」而拼出三种不同的请求体。
type ProviderPatch struct {
	// Original 是修改前的 name，用于定位；空表示新增。
	Original string `json:"original"`
	Name     string `json:"name"`
	BaseURL  string `json:"base_url"`
	AuthType string `json:"auth_type"`
	// AuthHeaderName / Prefix / QueryName 仅对应 auth_type 使用。
	AuthHeaderName   string `json:"auth_header_name"`
	AuthHeaderPrefix string `json:"auth_header_prefix"`
	AuthQueryName    string `json:"auth_query_name"`
	Timeout          int    `json:"timeout"`
	MaxRetry         int    `json:"max_retry"`
	RetryInterval    int    `json:"retry_interval"`
	// APIKey 为空表示「不修改原密钥」。
	//
	// 这是唯一能让「不回显密钥」与「能改密钥」共存的做法：
	// 前端把输入框留空提交，后端就知道要保留原值。
	// 想清空密钥得显式传 clear_api_key。
	APIKey      string `json:"api_key"`
	ClearAPIKey bool   `json:"clear_api_key"`
}

// ApplyProviderPatch 新增或修改一个厂商。
func (c *Config) ApplyProviderPatch(p ProviderPatch) error {
	name := strings.TrimSpace(p.Name)
	if name == "" {
		return fmt.Errorf("厂商名称不能为空")
	}
	base := strings.TrimSpace(p.BaseURL)
	if base == "" {
		return fmt.Errorf("厂商 %s 的 BaseURL 不能为空", name)
	}

	idx := -1
	if p.Original != "" {
		for i := range c.LLM.Providers {
			if c.LLM.Providers[i].Name == p.Original {
				idx = i
				break
			}
		}
		if idx < 0 {
			return fmt.Errorf("厂商 %s 不存在", p.Original)
		}
	}

	// 改名时不允许撞上另一个厂商：重名会让 ResolveModels 取到错的密钥。
	if p.Original != "" && p.Original != name {
		for i, q := range c.LLM.Providers {
			if i != idx && q.Name == name {
				return fmt.Errorf("已存在同名厂商 %s", name)
			}
		}
	}

	next := ProviderConfig{
		Name:             name,
		BaseURL:          base,
		AuthType:         orDefault(p.AuthType, "bearer"),
		AuthHeaderName:   p.AuthHeaderName,
		AuthHeaderPrefix: p.AuthHeaderPrefix,
		AuthQueryName:    p.AuthQueryName,
		Timeout:          p.Timeout,
		MaxRetry:         p.MaxRetry,
		RetryInterval:    p.RetryInterval,
	}
	if next.Timeout <= 0 {
		next.Timeout = 120
	}
	if next.MaxRetry <= 0 {
		next.MaxRetry = 2
	}
	if next.RetryInterval <= 0 {
		next.RetryInterval = 3
	}

	switch {
	case p.ClearAPIKey:
		// 显式清空。
	case strings.TrimSpace(p.APIKey) != "":
		next.APIKey = strings.TrimSpace(p.APIKey)
	case idx >= 0:
		next.APIKey = c.LLM.Providers[idx].APIKey
	}

	if idx >= 0 {
		old := c.LLM.Providers[idx].Name
		c.LLM.Providers[idx] = next
		if old != name {
			// 厂商改名后，引用它的模型要跟着改，否则模型会指向一个
			// 不存在的厂商，表现为「模型明明在列表里却报没有可用模型」。
			for i := range c.LLM.Models {
				if c.LLM.Models[i].Provider == old {
					c.LLM.Models[i].Provider = name
				}
			}
		}
		return nil
	}

	for _, q := range c.LLM.Providers {
		if q.Name == name {
			return fmt.Errorf("已存在同名厂商 %s", name)
		}
	}
	c.LLM.Providers = append(c.LLM.Providers, next)
	return nil
}

// DeleteProvider 删除一个厂商。
//
// 引用它的模型一并删除：留下「指向不存在厂商的模型」只会让
// 排查变得困难（列表里看得见、调用时报没有可用模型）。
func (c *Config) DeleteProvider(name string) error {
	idx := -1
	for i := range c.LLM.Providers {
		if c.LLM.Providers[i].Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("厂商 %s 不存在", name)
	}
	c.LLM.Providers = append(c.LLM.Providers[:idx], c.LLM.Providers[idx+1:]...)

	kept := make([]ModelConfig, 0, len(c.LLM.Models))
	for _, m := range c.LLM.Models {
		if m.Provider != name {
			kept = append(kept, m)
		}
	}
	c.LLM.Models = kept

	// 任务里对已删模型的引用也要清掉，否则 ResolveModels 会取到空。
	c.removeModelsFromTasks(name, nil)
	return nil
}

// removeModelsFromTasks 从所有任务里剔除指定模型名。
//
// deleted 为 nil 时按「已不存在的模型」全量清理：
// 删厂商会连带删模型，逐个传名字容易漏。
func (c *Config) removeModelsFromTasks(providerName string, _ []string) {
	alive := make(map[string]bool, len(c.LLM.Models))
	for _, m := range c.LLM.Models {
		if providerName == "" || m.Provider != providerName {
			alive[m.Name] = true
		}
	}
	for _, name := range taskOrder {
		t := c.LLM.Tasks.byName(name)
		if t == nil {
			continue
		}
		kept := make([]string, 0, len(t.Models))
		for _, mn := range t.Models {
			if alive[mn] {
				kept = append(kept, mn)
			}
		}
		t.Models = kept
	}
}

// ApplyModelPatch 新增或修改一个模型。
func (c *Config) ApplyModelPatch(m ModelView, original string) error {
	name := strings.TrimSpace(m.Name)
	if name == "" {
		return fmt.Errorf("模型名称不能为空")
	}
	ident := strings.TrimSpace(m.ModelIdentifier)
	if ident == "" {
		return fmt.Errorf("模型 %s 的标识符不能为空", name)
	}
	if strings.TrimSpace(m.Provider) == "" {
		return fmt.Errorf("模型 %s 必须指定厂商", name)
	}
	ok := false
	for _, p := range c.LLM.Providers {
		if p.Name == m.Provider {
			ok = true
			break
		}
	}
	if !ok {
		return fmt.Errorf("厂商 %s 不存在", m.Provider)
	}

	idx := -1
	if original != "" {
		for i := range c.LLM.Models {
			if c.LLM.Models[i].Name == original {
				idx = i
				break
			}
		}
		if idx < 0 {
			return fmt.Errorf("模型 %s 不存在", original)
		}
	}

	next := ModelConfig{
		Name:            name,
		ModelIdentifier: ident,
		Provider:        m.Provider,
		Visual:          m.Visual,
		ForceStream:     m.ForceStream,
		Temperature:     m.Temperature,
		MaxTokens:       m.MaxTokens,
		Dimension:       m.Dimension,
		PriceIn:         m.PriceIn,
		PriceOut:        m.PriceOut,
	}
	if next.MaxTokens != nil && *next.MaxTokens <= 0 {
		next.MaxTokens = nil
	}
	if next.Dimension != nil && *next.Dimension <= 0 {
		next.Dimension = nil
	}

	if idx >= 0 {
		old := c.LLM.Models[idx].Name
		c.LLM.Models[idx] = next
		if old != name {
			// 模型改名后，任务里的引用要跟着改。
			for _, tn := range taskOrder {
				t := c.LLM.Tasks.byName(tn)
				if t == nil {
					continue
				}
				for i, mn := range t.Models {
					if mn == old {
						t.Models[i] = name
					}
				}
			}
		}
		return nil
	}

	for _, q := range c.LLM.Models {
		if q.Name == name {
			return fmt.Errorf("已存在同名模型 %s", name)
		}
	}
	c.LLM.Models = append(c.LLM.Models, next)
	return nil
}

// DeleteModel 删除一个模型，并从所有任务里剔除。
func (c *Config) DeleteModel(name string) error {
	idx := -1
	for i := range c.LLM.Models {
		if c.LLM.Models[i].Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("模型 %s 不存在", name)
	}
	c.LLM.Models = append(c.LLM.Models[:idx], c.LLM.Models[idx+1:]...)

	for _, tn := range taskOrder {
		t := c.LLM.Tasks.byName(tn)
		if t == nil {
			continue
		}
		kept := make([]string, 0, len(t.Models))
		for _, mn := range t.Models {
			if mn != name {
				kept = append(kept, mn)
			}
		}
		t.Models = kept
	}
	return nil
}

// ApplyTaskPatch 修改一个任务的模型绑定。
func (c *Config) ApplyTaskPatch(t TaskView) error {
	target := c.LLM.Tasks.byName(t.Name)
	if target == nil {
		return fmt.Errorf("未知任务 %s", t.Name)
	}

	// 模型名必须存在：允许填不存在的名字，会让「保存成功、调用时报没有可用模型」，
	// 而报错点离配置点很远，很难联想到是这里写错了。
	for _, mn := range t.Models {
		found := false
		for _, m := range c.LLM.Models {
			if m.Name == mn {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("任务 %s 引用了不存在的模型 %s", t.Name, mn)
		}
	}

	switch t.SelectionStrategy {
	case "", "balance", "random", "sequential":
	default:
		return fmt.Errorf("未知的选择策略 %s", t.SelectionStrategy)
	}

	target.Models = append([]string{}, t.Models...)
	if t.MaxTokens > 0 {
		target.MaxTokens = t.MaxTokens
	}
	if t.Temperature >= 0 && t.Temperature <= 2 {
		target.Temperature = t.Temperature
	}
	if t.SelectionStrategy != "" {
		target.SelectionStrategy = t.SelectionStrategy
	}
	if t.HardTimeout > 0 {
		target.HardTimeout = t.HardTimeout
	}
	return nil
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
