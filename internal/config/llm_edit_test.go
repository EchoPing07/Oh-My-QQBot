package config

// 模型管理的结构化读写测试。
//
// 这一块的错误有个共同特征：**保存成功、很久以后才在调用时炸**。
// 典型的三种：
//   - 任务引用了不存在的模型 → 第一次调用才报「没有可用模型」
//   - 服务商改名但模型没跟着改 → 模型看得见、用不了
//   - 删服务商留下悬空模型 → 同上
// 所以这里的断言全部落在「引用完整性」上，而不只是「字段写进去了」。

import (
	"testing"
)

// TestLLMSnapshotHasNoAPIKey 防：快照把密钥带出去。
func TestLLMSnapshotHasNoAPIKey(t *testing.T) {
	cfg := Default()
	cfg.LLM.Providers = []ProviderConfig{{
		Name: "p", BaseURL: "https://x/v1", APIKey: "sk-DO-NOT-LEAK",
	}}
	cfg.LLM.Models = []ModelConfig{{Name: "m", ModelIdentifier: "i", Provider: "p"}}

	view := cfg.LLMSnapshot()
	if !view.Providers[0].HasAPIKey {
		t.Error("has_api_key 应为 true")
	}
	// ProviderView 结构体里根本没有 APIKey 字段，所以这里断言的是
	// 「模型化之后拿不到密钥」这一性质，编译期就已经保证了。
	if view.Providers[0].ModelCount != 1 {
		t.Errorf("model_count = %d", view.Providers[0].ModelCount)
	}
}

// TestLLMTaskOrderCoversAllTasks 防：加了新任务但忘了加进顺序表。
//
// 漏加的表现是「那个任务在界面上根本不存在」——不报错、不警告，
// 只是永远配不到它的模型。
func TestLLMTaskOrderCoversAllTasks(t *testing.T) {
	cfg := Default()
	seen := map[string]bool{}
	for _, name := range TaskOrder() {
		if seen[name] {
			t.Errorf("重复的任务名 %s", name)
		}
		seen[name] = true
		if cfg.LLM.Tasks.byName(name) == nil {
			t.Errorf("taskOrder 里的 %s 在 TasksConfig 里不存在", name)
		}
	}

	// 反向：TasksConfig 的每个字段都要在 taskOrder 里。
	for _, name := range []string{"planner", "replyer", "utils", "learner", "vlm", "embedding"} {
		if !seen[name] {
			t.Errorf("TasksConfig 有 %s，但 taskOrder 里没有（界面上会看不到）", name)
		}
	}
}

// TestApplyProviderPatchAdds 覆盖新增分支的默认值补齐。
func TestApplyProviderPatchAdds(t *testing.T) {
	cfg := Default()
	if err := cfg.ApplyProviderPatch(ProviderPatch{
		Name: "newp", BaseURL: "https://x/v1", APIKey: "sk-1",
	}); err != nil {
		t.Fatalf("新增失败: %v", err)
	}
	p := cfg.LLM.Providers[0]
	if p.AuthType != "bearer" {
		t.Errorf("auth_type 默认值 = %q，期望 bearer", p.AuthType)
	}
	// 超时/重试没填时要有合理默认，否则 0 秒超时会让请求立刻失败。
	if p.Timeout <= 0 || p.MaxRetry <= 0 || p.RetryInterval <= 0 {
		t.Errorf("默认值没补齐: %+v", p)
	}
}

// TestApplyProviderPatchRejectsDuplicate 防：两个同名服务商。
//
// 重名会让 ResolveModels 取到错的密钥，表现是「认证失败」，
// 而两份配置看起来都正常。
func TestApplyProviderPatchRejectsDuplicate(t *testing.T) {
	cfg := Default()
	cfg.LLM.Providers = []ProviderConfig{
		{Name: "a", BaseURL: "https://a/v1"},
		{Name: "b", BaseURL: "https://b/v1"},
	}
	if err := cfg.ApplyProviderPatch(ProviderPatch{Name: "a", BaseURL: "https://c/v1"}); err == nil {
		t.Error("新增同名服务商应当报错")
	}
	// 改名撞上另一个也要拒。
	if err := cfg.ApplyProviderPatch(ProviderPatch{
		Original: "b", Name: "a", BaseURL: "https://b/v1",
	}); err == nil {
		t.Error("改名撞上已有服务商应当报错")
	}
}

// TestApplyModelPatchClearsNonPositive 防：max_tokens / dimension 存 0。
//
// 0 在请求里是「不限」，与「不填」不同。用户清空输入框时
// 前端传 null，但如果哪天传了 0，这里要当成「不填」。
func TestApplyModelPatchClearsNonPositive(t *testing.T) {
	cfg := Default()
	cfg.LLM.Providers = []ProviderConfig{{Name: "p", BaseURL: "https://x/v1"}}

	zero := 0
	if err := cfg.ApplyModelPatch(ModelView{
		Name: "m", ModelIdentifier: "i", Provider: "p",
		MaxTokens: &zero, Dimension: &zero,
	}, ""); err != nil {
		t.Fatalf("新增失败: %v", err)
	}
	m := cfg.LLM.Models[0]
	if m.MaxTokens != nil {
		t.Errorf("max_tokens=0 应被当成不填，实际 %v", *m.MaxTokens)
	}
	if m.Dimension != nil {
		t.Errorf("dimension=0 应被当成不填，实际 %v", *m.Dimension)
	}
}

// TestApplyTaskPatchRejectsBadStrategy 防：非法选择策略被接受。
//
// 非法值的表现是「静默退回 balance」，用户以为自己配了 random。
func TestApplyTaskPatchRejectsBadStrategy(t *testing.T) {
	cfg := Default()
	err := cfg.ApplyTaskPatch(TaskView{
		Name: "planner", Models: nil, SelectionStrategy: "乱写",
	})
	if err == nil {
		t.Error("非法选择策略应当报错")
	}
}

// TestApplyTaskPatchKeepsZeroTemperature 防：把 temperature=0 当成「没填」。
//
// temperature = 0 是合法且有用的值（确定性输出）。
// 用「0 表示未设置」的哨兵会让这个值永远存不进去。
func TestApplyTaskPatchKeepsZeroTemperature(t *testing.T) {
	cfg := Default()
	cfg.LLM.Tasks.Planner.Temperature = 0.9
	if err := cfg.ApplyTaskPatch(TaskView{
		Name: "planner", Temperature: 0, SelectionStrategy: "balance",
	}); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if cfg.LLM.Tasks.Planner.Temperature != 0 {
		t.Errorf("temperature = %v，期望 0", cfg.LLM.Tasks.Planner.Temperature)
	}
}

// TestDeleteModelTwiceReportsMissing 防：删两次都成功。
func TestDeleteModelTwiceReportsMissing(t *testing.T) {
	cfg := Default()
	cfg.LLM.Providers = []ProviderConfig{{Name: "p", BaseURL: "https://x/v1"}}
	cfg.LLM.Models = []ModelConfig{{Name: "m", ModelIdentifier: "i", Provider: "p"}}

	if err := cfg.DeleteModel("m"); err != nil {
		t.Fatalf("第一次删除失败: %v", err)
	}
	if err := cfg.DeleteModel("m"); err == nil {
		t.Error("删除不存在的模型应当报错")
	}
}

// TestLLMSnapshotTaskOrderStable 防：任务顺序每次都不一样。
//
// 从 map 遍历会得到随机顺序，界面每次刷新都换位置。
func TestLLMSnapshotTaskOrderStable(t *testing.T) {
	cfg := Default()
	first := cfg.LLMSnapshot().TaskOrder
	for i := 0; i < 5; i++ {
		next := cfg.LLMSnapshot().TaskOrder
		if len(next) != len(first) {
			t.Fatal("长度不一致")
		}
		for j := range first {
			if next[j] != first[j] {
				t.Fatalf("第 %d 次顺序变了: %v vs %v", i, next, first)
			}
		}
	}
	// 并且 Tasks 数组的顺序要与 TaskOrder 一致，前端才能按下标对齐。
	view := cfg.LLMSnapshot()
	for i, name := range view.TaskOrder {
		if view.Tasks[i].Name != name {
			t.Errorf("位置 %d: Tasks[%s] 与 TaskOrder[%s] 不一致", i, view.Tasks[i].Name, name)
		}
	}
}

// TestDeleteProviderCleansModelsAndTasks 覆盖删除服务商的全链路。
//
// 三步都必须发生，少任意一步都会留下「看得见、用不了」的残骸：
//  1. 服务商消失
//  2. 它名下的模型一起消失
//  3. 任务里对这些模型的引用被剔除
func TestDeleteProviderCleansModelsAndTasks(t *testing.T) {
	cfg := Default()
	cfg.LLM.Providers = []ProviderConfig{
		{Name: "a", BaseURL: "https://a/v1"},
		{Name: "b", BaseURL: "https://b/v1"},
	}
	cfg.LLM.Models = []ModelConfig{
		{Name: "ma", ModelIdentifier: "i", Provider: "a"},
		{Name: "mb", ModelIdentifier: "i", Provider: "b"},
	}
	cfg.LLM.Tasks.Planner.Models = []string{"ma", "mb"}
	cfg.LLM.Tasks.Embedding.Models = []string{"ma"}

	if err := cfg.DeleteProvider("a"); err != nil {
		t.Fatalf("删除失败: %v", err)
	}

	if len(cfg.LLM.Providers) != 1 || cfg.LLM.Providers[0].Name != "b" {
		t.Errorf("剩下的服务商 = %+v，期望只有 b", cfg.LLM.Providers)
	}
	if len(cfg.LLM.Models) != 1 || cfg.LLM.Models[0].Name != "mb" {
		t.Errorf("剩下的模型 = %+v，期望只有 mb", cfg.LLM.Models)
	}
	if got := cfg.LLM.Tasks.Planner.Models; len(got) != 1 || got[0] != "mb" {
		t.Errorf("planner 引用 = %v，期望只剩 mb", got)
	}
	if got := cfg.LLM.Tasks.Embedding.Models; len(got) != 0 {
		t.Errorf("embedding 引用 = %v，期望清空", got)
	}

	if err := cfg.DeleteProvider("a"); err == nil {
		t.Error("删除不存在的服务商应当报错")
	}
}

// TestApplyProviderPatchRejectsEmpty 覆盖必填校验。
func TestApplyProviderPatchRejectsEmpty(t *testing.T) {
	cfg := Default()
	for _, p := range []ProviderPatch{
		{Name: "", BaseURL: "https://x/v1"},
		{Name: "  ", BaseURL: "https://x/v1"},
		{Name: "p", BaseURL: ""},
		{Name: "p", BaseURL: "   "},
	} {
		if err := cfg.ApplyProviderPatch(p); err == nil {
			t.Errorf("%+v 应当被拒绝", p)
		}
	}
	// 修改不存在的厂商也要报错。
	if err := cfg.ApplyProviderPatch(ProviderPatch{
		Original: "nope", Name: "p", BaseURL: "https://x/v1",
	}); err == nil {
		t.Error("修改不存在的服务商应当报错")
	}
}

// TestApplyModelPatchGuards 覆盖模型的必填与唯一性校验。
func TestApplyModelPatchGuards(t *testing.T) {
	base := func() *Config {
		c := Default()
		c.LLM.Providers = []ProviderConfig{{Name: "p", BaseURL: "https://x/v1"}}
		c.LLM.Models = []ModelConfig{
			{Name: "m1", ModelIdentifier: "i1", Provider: "p"},
			{Name: "m2", ModelIdentifier: "i2", Provider: "p"},
		}
		return c
	}

	cases := []struct {
		name  string
		patch ModelView
		orig  string
	}{
		{"空名称", ModelView{ModelIdentifier: "i", Provider: "p"}, ""},
		{"空标识符", ModelView{Name: "x", Provider: "p"}, ""},
		{"空厂商", ModelView{Name: "x", ModelIdentifier: "i"}, ""},
		{"重复名称", ModelView{Name: "m1", ModelIdentifier: "i", Provider: "p"}, ""},
		{"修改不存在的模型", ModelView{Name: "x", ModelIdentifier: "i", Provider: "p"}, "nope"},
	}
	for _, c := range cases {
		cfg := base()
		if err := cfg.ApplyModelPatch(c.patch, c.orig); err == nil {
			t.Errorf("%s 应当被拒绝", c.name)
		}
	}
}

// TestRenameModelUpdatesTaskRefs 防：改了模型名，任务还指着旧名字。
//
// 后果是「任务里空了一个位置」，表现为随机挑不到模型，
// 而配置界面上那个任务看起来是配好的。
func TestRenameModelUpdatesTaskRefs(t *testing.T) {
	cfg := Default()
	cfg.LLM.Providers = []ProviderConfig{{Name: "p", BaseURL: "https://x/v1"}}
	cfg.LLM.Models = []ModelConfig{{Name: "old", ModelIdentifier: "i", Provider: "p"}}
	cfg.LLM.Tasks.Planner.Models = []string{"old"}

	if err := cfg.ApplyModelPatch(ModelView{
		Name: "renamed", ModelIdentifier: "i", Provider: "p",
	}, "old"); err != nil {
		t.Fatalf("改名失败: %v", err)
	}
	if got := cfg.LLM.Tasks.Planner.Models; len(got) != 1 || got[0] != "renamed" {
		t.Errorf("planner 引用 = %v，期望 [renamed]", got)
	}
}

// TestApplyTaskPatchUnknownTask 覆盖未知任务名。
func TestApplyTaskPatchUnknownTask(t *testing.T) {
	cfg := Default()
	if err := cfg.ApplyTaskPatch(TaskView{Name: "nope"}); err == nil {
		t.Error("未知任务应当报错")
	}
}

// TestApplyTaskPatchIgnoresNonPositive 防：负数覆盖掉合理默认值。
//
// 前端数字框留空时会传 0/负数表示「不填」，
// 直接写进去会让 max_tokens=0（不限）或 hard_timeout=0（不超时）。
func TestApplyTaskPatchIgnoresNonPositive(t *testing.T) {
	cfg := Default()
	cfg.LLM.Tasks.Planner.MaxTokens = 2048
	cfg.LLM.Tasks.Planner.HardTimeout = 90

	if err := cfg.ApplyTaskPatch(TaskView{
		Name: "planner", MaxTokens: -1, HardTimeout: -5,
		Temperature: 3, SelectionStrategy: "balance",
	}); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if cfg.LLM.Tasks.Planner.MaxTokens != 2048 {
		t.Errorf("max_tokens 被负数覆盖: %d", cfg.LLM.Tasks.Planner.MaxTokens)
	}
	if cfg.LLM.Tasks.Planner.HardTimeout != 90 {
		t.Errorf("hard_timeout 被负数覆盖: %v", cfg.LLM.Tasks.Planner.HardTimeout)
	}
	// temperature 越界（>2）也不该写进去。
	if cfg.LLM.Tasks.Planner.Temperature == 3 {
		t.Error("temperature 越界值被接受了")
	}
}

// TestApplyProviderPatchKeepsSecretAndAuthFields 覆盖「编辑时保留原密钥」
// 与鉴权字段的透传。
//
// 这条守的是「不回显密钥」设计的另一半：前端拿不到原值，
// 所以编辑时必须能把原值带回来。带不回来就等于每次改超时都在清密钥。
func TestApplyProviderPatchKeepsSecretAndAuthFields(t *testing.T) {
	cfg := Default()
	if err := cfg.ApplyProviderPatch(ProviderPatch{
		Name: "p", BaseURL: "https://x/v1", APIKey: "sk-original",
		AuthType: "header", AuthHeaderName: "X-Key", AuthHeaderPrefix: "Bearer ",
	}); err != nil {
		t.Fatalf("新增失败: %v", err)
	}

	// 编辑：密钥留空、改地址与鉴权字段。
	if err := cfg.ApplyProviderPatch(ProviderPatch{
		Original: "p", Name: "p", BaseURL: "https://y/v1",
		AuthType: "header", AuthHeaderName: "X-Key", AuthHeaderPrefix: "Token ",
	}); err != nil {
		t.Fatalf("编辑失败: %v", err)
	}

	p := cfg.LLM.Providers[0]
	if p.APIKey != "sk-original" {
		t.Errorf("密钥 = %q，期望保留原值", p.APIKey)
	}
	if p.BaseURL != "https://y/v1" {
		t.Errorf("base_url = %q", p.BaseURL)
	}
	if p.AuthHeaderPrefix != "Token " {
		t.Errorf("auth_header_prefix = %q", p.AuthHeaderPrefix)
	}
	if p.AuthType != "header" {
		t.Errorf("auth_type = %q", p.AuthType)
	}

	// 显式传新密钥要覆盖。
	if err := cfg.ApplyProviderPatch(ProviderPatch{
		Original: "p", Name: "p", BaseURL: "https://y/v1", APIKey: "sk-new",
	}); err != nil {
		t.Fatalf("改密钥失败: %v", err)
	}
	if cfg.LLM.Providers[0].APIKey != "sk-new" {
		t.Errorf("密钥没被覆盖: %q", cfg.LLM.Providers[0].APIKey)
	}
}

// TestApplyTaskPatchWritesAllFields 覆盖任务字段的完整写入。
func TestApplyTaskPatchWritesAllFields(t *testing.T) {
	cfg := Default()
	cfg.LLM.Providers = []ProviderConfig{{Name: "p", BaseURL: "https://x/v1"}}
	cfg.LLM.Models = []ModelConfig{
		{Name: "m1", ModelIdentifier: "i", Provider: "p"},
		{Name: "m2", ModelIdentifier: "i", Provider: "p"},
	}

	if err := cfg.ApplyTaskPatch(TaskView{
		Name: "replyer", Models: []string{"m1", "m2"},
		MaxTokens: 512, Temperature: 0.5,
		SelectionStrategy: "sequential", HardTimeout: 30,
	}); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	got := cfg.LLM.Tasks.Replyer
	if len(got.Models) != 2 {
		t.Errorf("models = %v", got.Models)
	}
	if got.MaxTokens != 512 || got.Temperature != 0.5 ||
		got.SelectionStrategy != "sequential" || got.HardTimeout != 30 {
		t.Errorf("字段没写全: %+v", got)
	}
}

// TestDeleteModelOnEmptyConfig 覆盖「配置里一个模型都没有」时删除。
func TestDeleteModelOnEmptyConfig(t *testing.T) {
	cfg := Default()
	if err := cfg.DeleteModel("whatever"); err == nil {
		t.Error("空配置里删除模型应当报错")
	}
}
