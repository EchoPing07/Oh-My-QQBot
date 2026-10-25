package llm

import "testing"

// TestRepairArgumentsValid 验证合法 JSON 原样通过。
func TestRepairArgumentsValid(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`{"a":1}`, `{"a":1}`},
		{`{"a": "b"}`, `{"a":"b"}`},
		{`{"nested":{"x":[1,2,3]}}`, `{"nested":{"x":[1,2,3]}}`},
		{`[1,2,3]`, `[1,2,3]`},
	}

	for _, c := range cases {
		if got := RepairArguments(c.in); got != c.want {
			t.Errorf("RepairArguments(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// TestRepairArgumentsEmpty 验证空输入返回空对象。
func TestRepairArgumentsEmpty(t *testing.T) {
	for _, in := range []string{"", "  ", "\n"} {
		if got := RepairArguments(in); got != "{}" {
			t.Errorf("RepairArguments(%q) = %q，期望 {}", in, got)
		}
	}
}

// TestRepairArgumentsCodeFence 验证剥离 markdown 代码围栏。
func TestRepairArgumentsCodeFence(t *testing.T) {
	cases := []string{
		"```json\n{\"a\":1}\n```",
		"```\n{\"a\":1}\n```",
		"```JSON\n{\"a\":1}\n```",
	}

	for _, c := range cases {
		if got := RepairArguments(c); got != `{"a":1}` {
			t.Errorf("RepairArguments(%q) = %q，期望 {\"a\":1}", c, got)
		}
	}
}

// TestRepairArgumentsTrailingComma 验证去尾逗号。
func TestRepairArgumentsTrailingComma(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`{"a":1,}`, `{"a":1}`},
		{`{"a":[1,2,]}`, `{"a":[1,2]}`},
	}

	for _, c := range cases {
		if got := RepairArguments(c.in); got != c.want {
			t.Errorf("RepairArguments(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// TestRepairArgumentsUnquotedKey 验证给裸键名补引号。
func TestRepairArgumentsUnquotedKey(t *testing.T) {
	got := RepairArguments(`{a:1,b:2}`)
	if got != `{"a":1,"b":2}` {
		t.Errorf("RepairArguments = %q，期望 {\"a\":1,\"b\":2}", got)
	}
}

// TestRepairArgumentsSurroundingText 验证去掉前后解释文字。
func TestRepairArgumentsSurroundingText(t *testing.T) {
	got := RepairArguments(`好的，参数是 {"a":1} 就这样`)
	if got != `{"a":1}` {
		t.Errorf("RepairArguments = %q，期望 {\"a\":1}", got)
	}
}

// TestRepairArgumentsDoubleEncoded 验证双层编码。
//
// 有些模型会把参数整体包成字符串：`"{\"a\":1}"`。
func TestRepairArgumentsDoubleEncoded(t *testing.T) {
	got := RepairArguments(`"{\"a\":1}"`)
	if got != `{"a":1}` {
		t.Errorf("RepairArguments = %q，期望 {\"a\":1}", got)
	}
}

// TestRepairArgumentsHopeless 验证完全无法修复时返回空对象。
func TestRepairArgumentsHopeless(t *testing.T) {
	cases := []string{
		"这不是 JSON",
		"null",
		"123",
		"true",
	}
	for _, c := range cases {
		got := RepairArguments(c)
		if got != "{}" {
			t.Errorf("RepairArguments(%q) = %q，期望 {}", c, got)
		}
	}
}

// TestValidateArguments 验证参数校验。
func TestValidateArguments(t *testing.T) {
	if !ValidateArguments(`{"a":1}`) {
		t.Error("合法对象应通过校验")
	}
	if ValidateArguments(`not json`) {
		t.Error("非法内容不应通过校验")
	}
	if ValidateArguments(`123`) {
		t.Error("标量不应通过校验")
	}
}

// TestEstimateTokens 验证 token 估算不失控。
func TestEstimateTokens(t *testing.T) {
	// 中文按 1 字 1 token，加 10% 余量。
	cn := EstimateTokens("你好世界")
	if cn < 4 || cn > 8 {
		t.Errorf("4 个中文字符估算 %d，期望 4~8", cn)
	}

	// 英文按 4 字符 1 token。
	en := EstimateTokens("hello world")
	if en < 2 || en > 6 {
		t.Errorf("11 个英文字符估算 %d，期望 2~6", en)
	}

	if EstimateTokens("") != 0 {
		t.Error("空串应为 0")
	}
}

// TestEstimateMessageTokens 验证消息列表估算。
func TestEstimateMessageTokens(t *testing.T) {
	msgs := []Message{
		{Role: RoleSystem, Content: "你是一个助手"},
		{Role: RoleUser, Content: "你好"},
	}
	got := EstimateMessageTokens(msgs)
	if got <= 0 {
		t.Errorf("估算 = %d，应大于 0", got)
	}
}

// TestCostCalculator 验证花费计算。
func TestCostCalculator(t *testing.T) {
	// 100 万 prompt token，单价 1 元/M；100 万 completion，单价 2 元/M。
	cost := CostCalculator(1_000_000, 1_000_000, 0, 1.0, 2.0, 0)
	if cost != 3.0 {
		t.Errorf("花费 = %v，期望 3.0", cost)
	}

	// 缓存命中部分按缓存价。
	cost = CostCalculator(1_000_000, 0, 500_000, 1.0, 2.0, 0.1)
	// 50 万未命中 * 1 + 50 万命中 * 0.1 = 0.5 + 0.05 = 0.55
	if cost < 0.54 || cost > 0.56 {
		t.Errorf("缓存花费 = %v，期望约 0.55", cost)
	}
}
