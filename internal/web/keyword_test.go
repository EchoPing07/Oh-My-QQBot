package web

// 关键词规则编辑接口的测试。
//
// 守三件事：
//  1. 整表替换真的生效，并且写进配置 → 下一轮判定用的是新规则
//  2. 校验在 HTTP 层就把错误指到「第几条」，不是抛一个笼统的 500
//  3. 边界：空表、超上限、非 GET/POST

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
)

// TestKeywordsRoundTrip 守读写一致性与内存生效。
func TestKeywordsRoundTrip(t *testing.T) {
	env, path := newConfigTestEnv(t)
	env.loginOK()

	payload := `{"rules":[
		{"keywords":["猫","狗"],"regex":[],"reaction":"用户可能在说宠物，可以顺着聊"},
		{"keywords":[],"regex":["(?i)help\\s+me"],"reaction":"有人在求助，给出具体做法"}
	]}`
	resp, body := env.do(http.MethodPost, "/api/config/keywords/save", payload)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200，body=%s", resp.StatusCode, body)
	}

	// 1. 内存里生效
	rules := env.s.cfg.Get().Keyword.Rules
	if len(rules) != 2 {
		t.Fatalf("规则条数 = %d，期望 2", len(rules))
	}
	if len(rules[0].Keywords) != 2 || rules[0].Keywords[0] != "猫" {
		t.Errorf("第 1 条关键词 = %v", rules[0].Keywords)
	}
	if rules[1].Regex[0] != `(?i)help\s+me` {
		t.Errorf("第 2 条正则 = %v", rules[1].Regex)
	}

	// 2. 落盘了（重新加载仍是新值 → 证明写出的是合法 TOML）
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("重新加载配置失败（写出的 TOML 不合法）: %v", err)
	}
	if len(reloaded.Keyword.Rules) != 2 {
		t.Errorf("重新加载后规则条数 = %d，期望 2", len(reloaded.Keyword.Rules))
	}

	// 3. GET 拿回来的是同一份（含 max_rules 上限）
	_, body = env.do(http.MethodGet, "/api/config/keywords", "")
	var out struct {
		Rules    []map[string]any `json:"rules"`
		MaxRules int              `json:"max_rules"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("解析失败: %v，body=%s", err, body)
	}
	if len(out.Rules) != 2 {
		t.Errorf("GET 返回 %d 条，期望 2", len(out.Rules))
	}
	if out.MaxRules <= 0 {
		t.Errorf("max_rules = %d，前端无从判断还能不能加", out.MaxRules)
	}
}

// TestKeywordsRejectsInvalid 守校验：错误必须能指到第几条。
func TestKeywordsRejectsInvalid(t *testing.T) {
	env, path := newConfigTestEnv(t)
	env.loginOK()

	before, err := json.Marshal(env.s.cfg.Get().Keyword.Rules)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}

	cases := []struct {
		name    string
		payload string
		wantSub string
	}{
		{
			name:    "只有反应没有触发条件",
			payload: `{"rules":[{"keywords":[],"regex":[],"reaction":"随便说点什么"}]}`,
			wantSub: "第 1 条",
		},
		{
			name:    "反应为空",
			payload: `{"rules":[{"keywords":["猫"],"regex":[],"reaction":""}]}`,
			wantSub: "反应内容不能为空",
		},
		{
			name:    "正则写错",
			payload: `{"rules":[{"keywords":["a"],"regex":["[unclosed"],"reaction":"x"}]}`,
			wantSub: "第 1 条",
		},
		{
			name: "第二条错时要指到第二条",
			payload: `{"rules":[
				{"keywords":["ok"],"regex":[],"reaction":"fine"},
				{"keywords":[],"regex":[],"reaction":"缺触发条件"}
			]}`,
			wantSub: "第 2 条",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := env.do(http.MethodPost, "/api/config/keywords/save", tc.payload)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("状态码 = %d，期望 400，body=%s", resp.StatusCode, body)
			}
			var errResp struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(body, &errResp)
			if !strings.Contains(errResp.Error, tc.wantSub) {
				t.Errorf("错误信息 %q 里没有 %q", errResp.Error, tc.wantSub)
			}
		})
	}

	// 校验失败时配置一个字都不能变。
	after, _ := json.Marshal(env.s.cfg.Get().Keyword.Rules)
	if string(before) != string(after) {
		t.Errorf("校验失败后规则被改了：%s → %s", before, after)
	}
	// 也不该落盘。
	raw, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("读取配置失败: %v", readErr)
	}
	if strings.Contains(string(raw), "缺触发条件") {
		t.Error("校验失败的规则被写进了配置文件")
	}
}

// TestKeywordsEmptyAllowed 守「清空全部规则」是合法操作。
//
// 空规则集在运行时表现为 newKeywordMatcher 返回 nil、不追加任何提示。
// 如果这里报错，用户就没法把配错的规则删干净。
func TestKeywordsEmptyAllowed(t *testing.T) {
	env, _ := newConfigTestEnv(t)
	env.loginOK()

	env.s.cfg.Get().Keyword.Rules = nil
	resp, body := env.do(http.MethodPost, "/api/config/keywords/save", `{"rules":[]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("清空状态码 = %d，期望 200，body=%s", resp.StatusCode, body)
	}
	if n := len(env.s.cfg.Get().Keyword.Rules); n != 0 {
		t.Errorf("清空后条数 = %d，期望 0", n)
	}
}

// TestKeywordsBlankRowSkipped 守「点了添加但还没填」的空行不算错。
//
// 表格编辑器里点「添加一条」必然产生一个三字段全空的行。
// 这里报错会让「加一条再填」变成不可用操作。
func TestKeywordsBlankRowSkipped(t *testing.T) {
	env, _ := newConfigTestEnv(t)
	env.loginOK()

	payload := `{"rules":[
		{"keywords":["猫"],"regex":[],"reaction":"聊宠物"},
		{"keywords":[],"regex":[],"reaction":""}
	]}`
	resp, body := env.do(http.MethodPost, "/api/config/keywords/save", payload)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200，body=%s", resp.StatusCode, body)
	}
	if n := len(env.s.cfg.Get().Keyword.Rules); n != 1 {
		t.Errorf("空行应被跳过，实际条数 = %d", n)
	}
}

// TestKeywordsTooMany 守条数上限。
//
// 规则是每条消息都要逐条匹配的，条数无上限会让处理里
// 多出几百次子串/正则匹配 —— 表现是「机器人越用越慢」。
func TestKeywordsTooMany(t *testing.T) {
	env, _ := newConfigTestEnv(t)
	env.loginOK()

	rules := make([]map[string]any, maxKeywordRules+1)
	for i := range rules {
		rules[i] = map[string]any{"keywords": []string{"k"}, "regex": []string{}, "reaction": "r"}
	}
	raw, _ := json.Marshal(map[string]any{"rules": rules})

	resp, body := env.do(http.MethodPost, "/api/config/keywords/save", string(raw))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("超上限状态码 = %d，期望 400，body=%s", resp.StatusCode, body)
	}
}

// TestKeywordsMethods 守只接受 GET / POST。
func TestKeywordsMethods(t *testing.T) {
	env, _ := newConfigTestEnv(t)
	env.loginOK()

	if resp, _ := env.do(http.MethodDelete, "/api/config/keywords", ""); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("DELETE /api/config/keywords = %d，期望 405", resp.StatusCode)
	}
	if resp, _ := env.do(http.MethodGet, "/api/config/keywords/save", ""); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /api/config/keywords/save = %d，期望 405", resp.StatusCode)
	}
}

// TestKeywordsRequireAuth 守未登录不能读写规则。
func TestKeywordsRequireAuth(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	if resp, _ := env.do(http.MethodGet, "/api/config/keywords", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET 未鉴权 = %d，期望 401", resp.StatusCode)
	}
	if resp, _ := env.do(http.MethodPost, "/api/config/keywords/save", `{"rules":[]}`); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("POST 未鉴权 = %d，期望 401", resp.StatusCode)
	}
}
