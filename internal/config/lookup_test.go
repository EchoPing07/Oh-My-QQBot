package config

// 本文件覆盖 Provider / Model / Task / ResolveModels 等查找与回退逻辑。

import (
	"strings"
	"testing"
)

// newLookupConfig 造一份带服务商、模型与任务绑定的配置。
func newLookupConfig() *Config {
	c := validConfig()
	c.LLM.Providers = []ProviderConfig{
		{Name: "p1", BaseURL: "http://p1/v1", APIKey: "k1"},
		{Name: "p2", BaseURL: "http://p2/v1", APIKey: "k2"},
	}
	c.LLM.Models = []ModelConfig{
		{Name: "m1", ModelIdentifier: "id-1", Provider: "p1"},
		{Name: "m2", ModelIdentifier: "id-2", Provider: "p2"},
	}
	return c
}

// TestProviderLookup 校验服务商查找的成功与失败。
func TestProviderLookup(t *testing.T) {
	c := newLookupConfig()

	p, err := c.Provider("p2")
	if err != nil {
		t.Fatalf("查找 p2 失败: %v", err)
	}
	if p.BaseURL != "http://p2/v1" || p.APIKey != "k2" {
		t.Errorf("找到的服务商 = %+v，期望 p2", p)
	}

	if _, err := c.Provider("nope"); err == nil {
		t.Fatal("不存在的服务商应报错")
	} else if !strings.Contains(err.Error(), "nope") {
		t.Errorf("错误信息 = %q，期望包含服务商名", err.Error())
	}
}

// TestModelLookup 校验模型查找的成功与失败。
func TestModelLookup(t *testing.T) {
	c := newLookupConfig()

	m, err := c.Model("m1")
	if err != nil {
		t.Fatalf("查找 m1 失败: %v", err)
	}
	if m.ModelIdentifier != "id-1" || m.Provider != "p1" {
		t.Errorf("找到的模型 = %+v，期望 m1", m)
	}

	if _, err := c.Model("nope"); err == nil {
		t.Fatal("不存在的模型应报错")
	} else if !strings.Contains(err.Error(), "nope") {
		t.Errorf("错误信息 = %q，期望包含模型名", err.Error())
	}

	if !c.HasModel("m2") {
		t.Error("HasModel(m2) 应为 true")
	}
	if c.HasModel("nope") {
		t.Error("HasModel(nope) 应为 false")
	}
}

// TestTaskLookupAllNames 校验每个任务名都映射到正确的配置节。
func TestTaskLookupAllNames(t *testing.T) {
	c := Default()
	c.LLM.Tasks.Planner.MaxTokens = 1001
	c.LLM.Tasks.Replyer.MaxTokens = 1002
	c.LLM.Tasks.Utils.MaxTokens = 1003
	c.LLM.Tasks.Learner.MaxTokens = 1004
	c.LLM.Tasks.VLM.MaxTokens = 1005
	c.LLM.Tasks.Embedding.MaxTokens = 1006

	cases := []struct {
		task TaskName
		want int
	}{
		{TaskPlanner, 1001},
		{TaskReplyer, 1002},
		{TaskUtils, 1003},
		{TaskLearner, 1004},
		{TaskVLM, 1005},
		{TaskEmbedding, 1006},
	}
	for _, ck := range cases {
		if got := c.Task(ck.task).MaxTokens; got != ck.want {
			t.Errorf("Task(%s).MaxTokens = %d，期望 %d", ck.task, got, ck.want)
		}
	}

	// 未知任务名退化为 utils，避免返回零值让调用方拿到空配置。
	if got := c.Task(TaskName("未知")).MaxTokens; got != 1003 {
		t.Errorf("未知任务的 MaxTokens = %d，期望退化为 utils 的 1003", got)
	}
}

// TestResolveModelsFallback 是这组里最重要的一条：
// 回退规则错了会直接导致「没有可用模型」启动失败或者用错模型。
func TestResolveModelsFallback(t *testing.T) {
	c := newLookupConfig()
	c.LLM.Tasks.Planner.Models = []string{"planner-model"}
	c.LLM.Tasks.Utils.Models = []string{"utils-model"}
	c.LLM.Tasks.Replyer.Models = nil
	c.LLM.Tasks.Learner.Models = nil
	c.LLM.Tasks.VLM.Models = nil
	c.LLM.Tasks.Embedding.Models = nil

	cases := []struct {
		name string
		task TaskName
		want []string
	}{
		{"planner 用自己的", TaskPlanner, []string{"planner-model"}},
		{"replyer 回退到 planner", TaskReplyer, []string{"planner-model"}},
		{"learner 回退到 utils", TaskLearner, []string{"utils-model"}},
		{"utils 用自己的", TaskUtils, []string{"utils-model"}},
		// planner 是最上层，没有回退；vlm 也没有回退。
		{"vlm 无回退", TaskVLM, nil},
		{"embedding 无回退", TaskEmbedding, nil},
	}

	for _, ck := range cases {
		t.Run(ck.name, func(t *testing.T) {
			got := c.ResolveModels(ck.task)
			if len(got) != len(ck.want) {
				t.Fatalf("ResolveModels(%s) = %v，期望 %v", ck.task, got, ck.want)
			}
			for i := range ck.want {
				if got[i] != ck.want[i] {
					t.Fatalf("ResolveModels(%s) = %v，期望 %v", ck.task, got, ck.want)
				}
			}
		})
	}
}

// TestResolveModelsPlannerNoFallback 单独钉住「planner 为空时就是空」。
func TestResolveModelsPlannerNoFallback(t *testing.T) {
	c := newLookupConfig()
	c.LLM.Tasks.Planner.Models = nil
	c.LLM.Tasks.Utils.Models = []string{"utils-model"}

	if got := c.ResolveModels(TaskPlanner); len(got) != 0 {
		t.Errorf("planner 无回退，期望空列表，实际 %v", got)
	}

	// replyer 回退到 planner，planner 为空时 replyer 也为空。
	if got := c.ResolveModels(TaskReplyer); len(got) != 0 {
		t.Errorf("planner 为空时 replyer 也应为空，实际 %v", got)
	}
}

// TestResolveModelsExplicitWins 校验显式配置优先于回退。
func TestResolveModelsExplicitWins(t *testing.T) {
	c := newLookupConfig()
	c.LLM.Tasks.Planner.Models = []string{"planner-model"}
	c.LLM.Tasks.Replyer.Models = []string{"replyer-model"}
	c.LLM.Tasks.Utils.Models = []string{"utils-model"}
	c.LLM.Tasks.Learner.Models = []string{"learner-model"}

	if got := c.ResolveModels(TaskReplyer); len(got) != 1 || got[0] != "replyer-model" {
		t.Errorf("replyer 应使用自己的模型，实际 %v", got)
	}
	if got := c.ResolveModels(TaskLearner); len(got) != 1 || got[0] != "learner-model" {
		t.Errorf("learner 应使用自己的模型，实际 %v", got)
	}
}

// TestPrimaryModel 校验首选模型的取法与空列表报错。
func TestPrimaryModel(t *testing.T) {
	c := newLookupConfig()
	c.LLM.Tasks.Planner.Models = []string{"first", "second"}

	got, err := c.PrimaryModel(TaskPlanner)
	if err != nil {
		t.Fatalf("取首选模型失败: %v", err)
	}
	if got != "first" {
		t.Errorf("PrimaryModel = %q，期望 %q", got, "first")
	}

	if _, err := c.PrimaryModel(TaskVLM); err == nil {
		t.Fatal("没有可用模型时应报错")
	}
}

// TestTaskNameConstants 校验任务名常量值，配置文件与日志都按字符串匹配。
func TestTaskNameConstants(t *testing.T) {
	cases := map[TaskName]string{
		TaskPlanner:   "planner",
		TaskReplyer:   "replyer",
		TaskUtils:     "utils",
		TaskLearner:   "learner",
		TaskVLM:       "vlm",
		TaskEmbedding: "embedding",
	}
	for task, want := range cases {
		if string(task) != want {
			t.Errorf("TaskName %q，期望 %q", string(task), want)
		}
	}
}
