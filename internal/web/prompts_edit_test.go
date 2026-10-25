package web

// 提示词管理接口的回归测试（本文件只测「全部提示词都可编辑」这条能力）。
//
// 原有的 TestPromptsListAndSave / TestPromptsRejectTraversal 等
// 在 delete_test.go 里，覆盖的是同一套接口的基础行为。

import (
	"net/http"
	"strings"
	"testing"
)

// TestPromptCatalogExposesEveryTemplate 守「列表里能看到全部提示词」。
//
// 早期只有 4 份是暴露的，其余 7 份（聊天回想、画像、学习、表情包打标、
// 两份含义推断）写死在各自包里 —— 用户想调一句措辞只能改代码。
// 这条测试逐个点名，少一个就红。
func TestPromptCatalogExposesEveryTemplate(t *testing.T) {
	env := newTestEnv(t, envOptions{promptsDir: t.TempDir()})
	env.loginOK()

	resp, body := env.do(http.MethodGet, "/api/prompts", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, body)
	}

	want := []string{
		"planner", "replyer", "reply_attachments",
		"emoji_content_analysis", "mid_term_memory_summary", "person_profile",
		"learn_style", "learn_jargon",
		"jargon_inference_with_context", "jargon_inference_content_only",
	}
	for _, name := range want {
		if !strings.Contains(string(body), `"name":"`+name+`"`) {
			t.Errorf("提示词列表里缺少 %s", name)
		}
	}

	// 每一份都必须有中文显示名：界面上直接显示 name 会让人看不懂这是干什么的。
	if strings.Contains(string(body), `"display_name":""`) {
		t.Errorf("有提示词缺少 display_name：%s", truncateStr(string(body), 800))
	}
}

// TestPromptCatalogMarksAdvanced 守「高级项被标记出来」。
//
// 11 份平铺会让「该改哪一份」变难判断，日常会调的其实只有规划/回复。
// advanced 由后端给（不是前端硬编码），这样加模板时只改一处。
func TestPromptCatalogMarksAdvanced(t *testing.T) {
	env := newTestEnv(t, envOptions{promptsDir: t.TempDir()})
	env.loginOK()

	_, body := env.do(http.MethodGet, "/api/prompts", "")
	raw := string(body)

	// 至少要有一项是 advanced（否则前端那个开关永远不显示）。
	if !strings.Contains(raw, `"advanced":true`) {
		t.Error("没有任何提示词被标记为 advanced")
	}
	// 规划与回复是最常调的，不该被收进高级区。
	for _, essential := range []string{"planner", "replyer"} {
		idx := strings.Index(raw, `"name":"`+essential+`"`)
		if idx < 0 {
			t.Fatalf("找不到 %s", essential)
		}
		// 该项的对象在 name 之后若干字符内，粗查这一段里的 advanced 字段。
		seg := raw[idx:]
		if end := strings.Index(seg, "},"); end > 0 {
			seg = seg[:end]
		}
		if strings.Contains(seg, `"advanced":true`) {
			t.Errorf("%s 不应当被标记为 advanced", essential)
		}
	}
}

// TestPromptGetReturnsPlaceholders 守「读单条时带占位符清单」。
//
// 界面靠它渲染可点击的占位符按钮。没有这个字段，用户只能靠保存失败
// 去猜哪几个 {xxx} 必须保留。
func TestPromptGetReturnsPlaceholders(t *testing.T) {
	env := newTestEnv(t, envOptions{promptsDir: t.TempDir()})
	env.loginOK()

	_, body := env.do(http.MethodGet, "/api/prompts?name=planner", "")
	raw := string(body)
	if !strings.Contains(raw, `"placeholders":[`) {
		t.Fatalf("响应缺少 placeholders：%s", truncateStr(raw, 400))
	}
	for _, ph := range []string{`"bot_name"`, `"behavior_style"`, `"query_memory_rule"`} {
		if !strings.Contains(raw, ph) {
			t.Errorf("placeholders 里缺少 %s：%s", ph, truncateStr(raw, 400))
		}
	}
}

// TestPromptSaveRejectsMissingPlaceholder 守「少占位符被拦下」。
//
// 这条是本次最关键的校验：少了 {bot_name} 不会报错，
// 只会让 Planner 不知道自己叫什么 —— 表现为「机器人行为变得很奇怪」。
func TestPromptSaveRejectsMissingPlaceholder(t *testing.T) {
	env := newTestEnv(t, envOptions{promptsDir: t.TempDir()})
	env.loginOK()

	resp, body := env.do(http.MethodPut, "/api/prompts",
		`{"name":"planner","content":"你的名字是{bot_name}。"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码 = %d（期望 400），body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "缺少占位符") {
		t.Errorf("错误信息应当说明缺了什么：%s", body)
	}
	// hint 要指出去改哪里，不能只给一句技术描述。
	if !strings.Contains(string(body), `"hint"`) {
		t.Errorf("响应缺少 hint：%s", body)
	}
}

// TestPromptSaveRejectsExtraPlaceholder 守「多占位符被拦下」。
//
// 多了的占位符永远渲染成空串，用户会以为自己写的内容被吞掉了。
func TestPromptSaveRejectsExtraPlaceholder(t *testing.T) {
	env := newTestEnv(t, envOptions{promptsDir: t.TempDir()})
	env.loginOK()

	// 先拿到默认内容，再在末尾追加一个不存在的占位符。
	_, body := env.do(http.MethodGet, "/api/prompts?name=replyer", "")
	def, ok := extractJSONField(string(body), "content")
	if !ok {
		t.Fatalf("读不到 replyer 默认内容：%s", truncateStr(string(body), 300))
	}

	content := def + "\n{不存在的键}"
	payload := `{"name":"replyer","content":` + jsonString(content) + `}`
	resp, respBody := env.do(http.MethodPut, "/api/prompts", payload)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码 = %d（期望 400），body=%s", resp.StatusCode, respBody)
	}
	if !strings.Contains(string(respBody), "多余占位符") {
		t.Errorf("错误信息应当说明多出了什么：%s", respBody)
	}
}

// TestPromptSaveAcceptsRewording 守「只改措辞能保存」。
//
// 校验只比占位符集合，不比对全文 —— 否则「提示词管理」就等于没用。
func TestPromptSaveAcceptsRewording(t *testing.T) {
	env := newTestEnv(t, envOptions{promptsDir: t.TempDir()})
	env.loginOK()

	_, body := env.do(http.MethodGet, "/api/prompts?name=learn_style", "")
	def, ok := extractJSONField(string(body), "content")
	if !ok {
		t.Fatalf("读不到 learn_style 默认内容：%s", truncateStr(string(body), 300))
	}

	// 在最前面加一句自己的话，占位符原样保留。
	content := "【自定义前缀】\n" + def
	payload := `{"name":"learn_style","content":` + jsonString(content) + `}`
	resp, respBody := env.do(http.MethodPut, "/api/prompts", payload)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d（期望 200），body=%s", resp.StatusCode, respBody)
	}

	// 保存后读回来必须是新内容。
	_, body = env.do(http.MethodGet, "/api/prompts?name=learn_style", "")
	if !strings.Contains(string(body), "【自定义前缀】") {
		t.Errorf("保存后读到的还是旧内容：%s", truncateStr(string(body), 300))
	}
}

// TestPromptSaveRejectsUnknownName 守「保存一个不存在的模板被拦下」。
//
// 没有默认版本就没有占位符基线可比，保存进去也永远不会被渲染到 ——
// 只会让用户以为「改好了」，实际什么都没发生。
func TestPromptSaveRejectsUnknownName(t *testing.T) {
	env := newTestEnv(t, envOptions{promptsDir: t.TempDir()})
	env.loginOK()

	resp, body := env.do(http.MethodPut, "/api/prompts",
		`{"name":"my_custom_prompt","content":"随便写点什么"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码 = %d（期望 400），body=%s", resp.StatusCode, body)
	}
}

// extractJSONField 从 JSON 文本里取一个字符串字段（不解析整份结构）。
//
// 不用完整 json.Unmarshal 到 map：测试只需要一个值，
// 而提示词内容本身带大量转义字符，解析成 map 后还要再取一次。
func extractJSONField(raw, field string) (string, bool) {
	marker := `"` + field + `":"`
	idx := strings.Index(raw, marker)
	if idx < 0 {
		return "", false
	}
	rest := raw[idx+len(marker):]
	var b strings.Builder
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		switch c {
		case '\\':
			if i+1 >= len(rest) {
				return b.String(), true
			}
			i++
			switch rest[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			default:
				b.WriteByte(rest[i])
			}
		case '"':
			return b.String(), true
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), true
}

// jsonString 把文本编码成 JSON 字符串字面量（带引号）。
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
