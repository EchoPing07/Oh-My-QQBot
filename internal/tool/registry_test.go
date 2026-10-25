package tool

// 本文件覆盖工具注册表：注册、查找、可见性过滤、执行与 panic 恢复。

import (
	"context"
	"strings"
	"testing"
)

// stubTool 是一个可配置的最小工具实现，供注册表测试使用。
type stubTool struct {
	name    string
	visible bool
	exec    func(ctx context.Context, inv Invocation, rt Runtime) (Result, error)
	desc    string
}

func (s *stubTool) Name() string        { return s.name }
func (s *stubTool) Description() string { return s.desc }
func (s *stubTool) Schema() map[string]any {
	return objectSchema(map[string]any{"x": map[string]any{"type": "string"}}, []string{"x"})
}
func (s *stubTool) Visible(Availability) bool { return s.visible }
func (s *stubTool) Execute(ctx context.Context, inv Invocation, rt Runtime) (Result, error) {
	if s.exec != nil {
		return s.exec(ctx, inv, rt)
	}
	return TextResult("ok"), nil
}

func newStub(name string, visible bool) *stubTool {
	return &stubTool{name: name, visible: visible, desc: name + " 的说明"}
}

// TestRegistryRegisterDuplicate 防的是「同名工具覆盖掉已有实现」：
// 后注册的静默生效会让内置工具被意外替换。
func TestRegistryRegisterDuplicate(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(newStub("reply", true)); err != nil {
		t.Fatalf("首次注册失败: %v", err)
	}
	err := r.Register(newStub("reply", true))
	if err == nil {
		t.Fatal("重复注册同名工具应报错")
	}
	if !strings.Contains(err.Error(), "重复注册") {
		t.Errorf("错误信息 = %q，期望包含 %q", err.Error(), "重复注册")
	}
	if names := r.Names(); len(names) != 1 {
		t.Errorf("Names() = %v，期望只有 1 项", names)
	}
}

// TestRegistryRegisterEmptyName 校验空名工具被拒绝。
func TestRegistryRegisterEmptyName(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(newStub("", true)); err == nil {
		t.Fatal("空工具名应报错")
	}
	if len(r.Names()) != 0 {
		t.Errorf("失败注册不应留下记录，Names() = %v", r.Names())
	}
}

// TestRegistryGet 校验按名字取工具的成功与失败。
func TestRegistryGet(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(newStub("reply", true))

	got, ok := r.Get("reply")
	if !ok || got == nil || got.Name() != "reply" {
		t.Fatalf("Get(reply) = %v, %v，期望取到工具", got, ok)
	}
	if _, ok := r.Get("nope"); ok {
		t.Error("不存在的工具应返回 false")
	}
}

// TestRegistryNamesKeepOrder 校验 Names 按注册顺序返回，
// 顺序变化会让提示词里的工具列表抖动。
func TestRegistryNamesKeepOrder(t *testing.T) {
	r := NewRegistry()
	for _, name := range []string{"reply", "wait", "stop", "query_memory"} {
		r.MustRegister(newStub(name, true))
	}

	want := []string{"reply", "wait", "stop", "query_memory"}
	names := r.Names()
	if len(names) != len(want) {
		t.Fatalf("Names() = %v，期望 %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("Names() = %v，期望 %v（顺序必须保持注册顺序）", names, want)
		}
	}

	// 返回的必须是副本，外部改动不能污染注册表。
	names[0] = "被改了"
	if r.Names()[0] != "reply" {
		t.Error("Names() 应返回副本，外部修改不应影响注册表")
	}

	sorted := r.SortedNames()
	wantSorted := []string{"query_memory", "reply", "stop", "wait"}
	for i := range wantSorted {
		if sorted[i] != wantSorted[i] {
			t.Fatalf("SortedNames() = %v，期望 %v", sorted, wantSorted)
		}
	}
}

// TestRegistryDefinitionsFiltersInvisible 防的是「不该暴露的工具被模型看见」，
// 模型调用不存在的工具会白掉一轮。
func TestRegistryDefinitionsFiltersInvisible(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(newStub("always", true))
	r.MustRegister(newStub("never", false))

	defs := r.Definitions(Availability{IsGroup: true, GroupID: "g1"})
	if len(defs) != 1 {
		t.Fatalf("Definitions() 返回 %d 项，期望 1 项（不可见的应被过滤）", len(defs))
	}
	if defs[0].Function.Name != "always" {
		t.Errorf("可见工具名 = %q，期望 %q", defs[0].Function.Name, "always")
	}
	if defs[0].Type != "function" {
		t.Errorf("工具类型 = %q，期望 %q", defs[0].Type, "function")
	}
	if defs[0].Function.Description != "always 的说明" {
		t.Errorf("描述 = %q，期望 %q", defs[0].Function.Description, "always 的说明")
	}
	if defs[0].Function.Parameters == nil {
		t.Error("Parameters 不应为空")
	}
}

// TestRegistryDefinitionsRespectAvailability 校验 Visible 真的拿到了场景信息。
func TestRegistryDefinitionsRespectAvailability(t *testing.T) {
	var seen Availability
	r := NewRegistry()
	r.MustRegister(&availabilityTool{name: "group_only", groupOnly: true, seen: &seen})

	av := Availability{SessionID: "s1", IsGroup: true, GroupID: "g1", UserID: "u1", Platform: "qq"}
	if defs := r.Definitions(av); len(defs) != 1 {
		t.Fatalf("群聊场景应可见，实际 %d 项", len(defs))
	}
	if seen.SessionID != "s1" || !seen.IsGroup || seen.GroupID != "g1" || seen.Platform != "qq" {
		t.Errorf("Visible 收到的场景 = %+v，期望与传入一致", seen)
	}

	av.IsGroup = false
	if defs := r.Definitions(av); len(defs) != 0 {
		t.Errorf("私聊场景应不可见，实际 %d 项", len(defs))
	}
}

// availabilityTool 是「只在群聊可见」的假工具。
type availabilityTool struct {
	name      string
	groupOnly bool
	seen      *Availability
}

func (a *availabilityTool) Name() string        { return a.name }
func (a *availabilityTool) Description() string { return "按场景可见" }
func (a *availabilityTool) Schema() map[string]any {
	return objectSchema(nil, nil)
}
func (a *availabilityTool) Visible(av Availability) bool {
	*a.seen = av
	return !a.groupOnly || av.IsGroup
}
func (a *availabilityTool) Execute(context.Context, Invocation, Runtime) (Result, error) {
	return TextResult("ok"), nil
}

// TestRegistryExecuteUnknownTool 校验未知工具返回错误结果而不是静默成功。
func TestRegistryExecuteUnknownTool(t *testing.T) {
	r := NewRegistry()
	res, err := r.Execute(context.Background(), invJSON("nope", `{}`), newFakeRuntime())
	if err == nil {
		t.Fatal("未知工具应返回错误")
	}
	if !res.IsError {
		t.Error("未知工具的结果应标记 IsError")
	}
	if !strings.Contains(res.Content, "nope") {
		t.Errorf("结果内容 = %q，期望包含工具名", res.Content)
	}
}

// TestRegistryExecutePanicRecovered 是本文件最重要的一条：
// 工具内部 panic 必须被 safeExecute 吃掉，否则整个会话 goroutine 直接崩。
func TestRegistryExecutePanicRecovered(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(&stubTool{
		name:    "boom",
		visible: true,
		exec: func(context.Context, Invocation, Runtime) (Result, error) {
			panic("模拟炸了")
		},
	})

	res, err := r.Execute(context.Background(), invJSON("boom", `{}`), newFakeRuntime())
	if err == nil {
		t.Fatal("panic 应被转成错误返回")
	}
	if !strings.Contains(err.Error(), "panic") {
		t.Errorf("错误信息 = %q，期望包含 panic", err.Error())
	}
	if !res.IsError {
		t.Error("panic 结果应标记 IsError")
	}
	if !strings.Contains(res.Content, "模拟炸了") {
		t.Errorf("结果内容 = %q，期望带上 panic 值", res.Content)
	}
}

// TestRegistryExecuteSuccess 校验正常路径原样返回结果与 nil 错误。
func TestRegistryExecuteSuccess(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(&stubTool{
		name:    "ok",
		visible: true,
		exec: func(_ context.Context, inv Invocation, _ Runtime) (Result, error) {
			return TextResult("收到参数 " + string(inv.Arguments)), nil
		},
	})

	res, err := r.Execute(context.Background(), invJSON("ok", `{"x":"1"}`), newFakeRuntime())
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if res.IsError {
		t.Error("成功结果不应标记 IsError")
	}
	if res.Content != `收到参数 {"x":"1"}` {
		t.Errorf("结果内容 = %q", res.Content)
	}
}

// TestRegistryBuildToolsDescription 校验提示词里的工具说明。
func TestRegistryBuildToolsDescription(t *testing.T) {
	r := NewRegistry()
	if got := r.BuildToolsDescription(Availability{}); got != "" {
		t.Errorf("空注册表应返回空串，实际 %q", got)
	}

	r.MustRegister(newStub("reply", true))
	r.MustRegister(newStub("hidden", false))
	got := r.BuildToolsDescription(Availability{})
	want := "- reply: reply 的说明"
	if got != want {
		t.Errorf("BuildToolsDescription() = %q，期望 %q", got, want)
	}
}

// TestRegistryMustRegisterPanics 校验内置工具注册失败时会 panic，
// 启动阶段应该当场暴露而不是静默少一个工具。
func TestRegistryMustRegisterPanics(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(newStub("reply", true))

	defer func() {
		if recover() == nil {
			t.Error("重复注册应 panic")
		}
	}()
	r.MustRegister(newStub("reply", true))
}

// TestResultHelpers 校验结果构造辅助函数。
func TestResultHelpers(t *testing.T) {
	ok := TextResult("好了")
	if ok.IsError || ok.Content != "好了" {
		t.Errorf("TextResult() = %+v", ok)
	}

	bad := ErrorResult("坏了：%s", "原因")
	if !bad.IsError || bad.Content != "坏了：原因" {
		t.Errorf("ErrorResult() = %+v", bad)
	}
}

// TestInvocationDecode 校验空参数与非法参数的处理。
func TestInvocationDecode(t *testing.T) {
	var dst struct {
		A string `json:"a"`
	}
	if err := (Invocation{}).Decode(&dst); err != nil {
		t.Errorf("空参数不应报错，实际 %v", err)
	}
	if err := (Invocation{Arguments: []byte(`{"a":"1"}`)}).Decode(&dst); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if dst.A != "1" {
		t.Errorf("A = %q，期望 %q", dst.A, "1")
	}
	if err := (Invocation{Arguments: []byte(`{`)}).Decode(&dst); err == nil {
		t.Error("非法 JSON 应报错")
	}
}
