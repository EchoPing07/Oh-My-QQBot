package llm

// 本文件守住鉴权头拼接的跨层一致性（docs/audit/11-infra.md P1-5）。
//
// llm 层（openai.go 的 applyAuth，probe.go 三处请求都走它）与
// embedding 层必须共用 config.BuildAuthHeaderValue 的语义 ——
// 参考实现 refs/MaiBot/src/llm_models/openai_compat.py:63-76。
// 以前 llm 层无条件补一个空格、embedding 层不补，
// prefix 写成 "Bearer "（带尾空格）时两条链路必有一条 401。

import (
	"net/http"
	"testing"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
)

// TestApplyAuthHeaderMatchesHelper 验证 llm 层发出的头与 helper 完全一致。
//
// probe.go 的三处请求（探活 / 模型级测试）都经 openai.go 的 applyAuth
// 发出，所以守住这一个函数就守住了全部调用点。
func TestApplyAuthHeaderMatchesHelper(t *testing.T) {
	cases := []struct {
		name   string
		prefix string
	}{
		{"常规前缀", "Bearer"},
		{"前缀带尾空格", "Bearer "},
		{"空前缀只发密钥", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &config.ProviderConfig{
				AuthType:         "header",
				AuthHeaderName:   "X-Api-Key",
				AuthHeaderPrefix: tc.prefix,
				APIKey:           "sk-xxx",
			}
			req, _ := http.NewRequest(http.MethodPost, "http://example.invalid", nil)
			applyAuth(req, p)

			want := config.BuildAuthHeaderValue(tc.prefix, "sk-xxx")
			if got := req.Header.Get("X-Api-Key"); got != want {
				t.Errorf("header = %q，期望与 helper 一致 %q", got, want)
			}
			// 带尾空格的前缀绝不能产生双空格形态。
			if got := req.Header.Get("X-Api-Key"); got == "Bearer  sk-xxx" {
				t.Errorf("产生了双空格: %q", got)
			}
		})
	}
}

// TestApplyAuthBearerDefault 验证默认 bearer 分支也走 helper。
func TestApplyAuthBearerDefault(t *testing.T) {
	p := &config.ProviderConfig{APIKey: "sk-xxx"}
	req, _ := http.NewRequest(http.MethodPost, "http://example.invalid", nil)
	applyAuth(req, p)

	if got := req.Header.Get("Authorization"); got != "Bearer sk-xxx" {
		t.Errorf("Authorization = %q，期望 \"Bearer sk-xxx\"", got)
	}
}
