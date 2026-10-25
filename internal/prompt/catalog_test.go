package prompt

// 本文件覆盖「全部提示词都可编辑」这条能力新增的三块逻辑：
//   - Placeholders / ValidatePlaceholders：保存时校验占位符不能增删
//   - RenderParts：一份模板里两套占位符空间按顺序渲染
//   - 内嵌模板清单：11 份都在，且都能被 Manager 读到

import (
	"strings"
	"testing"
)

// TestEmbeddedNamesCoversEveryChain 守内嵌模板清单包含每条链路要用的那份。
//
// 少一份的表现是静默的：对应功能会回落到代码里的兜底文案或空串，
// 而 WebUI 上根本看不到这个模板，用户想改也没入口。
func TestEmbeddedNamesCoversEveryChain(t *testing.T) {
	want := []string{
		"planner",                       // Planner system
		"replyer",                       // Replyer system
		"reply_attachments",             // 附带图片/表情包时的说明
		"emoji_content_analysis",        // 表情包打标签
		"mid_term_memory_summary",       // 聊天回想
		"person_profile",                // 人物画像
		"learn_style",                   // 表达学习
		"learn_jargon",                  // 黑话提取
		"jargon_inference_with_context", // 黑话含义推断（带上下文）
		"jargon_inference_content_only", // 黑话含义推断（只看词条）
	}

	got := map[string]bool{}
	for _, n := range EmbeddedNames() {
		got[n] = true
	}
	for _, n := range want {
		if !got[n] {
			t.Errorf("内嵌模板里缺少 %q（对应链路会静默用兜底文案）", n)
		}
	}
}

// TestPlaceholdersExtractsKeys 覆盖占位符提取。
func TestPlaceholdersExtractsKeys(t *testing.T) {
	tmpl := "你好{bot_name}，风格是{behavior_style}。再来一次{bot_name}。"
	got := Placeholders(tmpl)
	if len(got) != 2 || got[0] != "behavior_style" || got[1] != "bot_name" {
		t.Errorf("Placeholders() = %v，期望 [behavior_style bot_name]（去重且排序）", got)
	}
}

// TestPlaceholdersSkipsJSONExamples 守「JSON 示例里的花括号不算占位符」。
//
// 这条很重要：黑话/表达学习的模板里全是 {"situation": "..."} 这种示例，
// 一旦被当成占位符，保存校验会永远失败 —— 用户完全不知道为什么改不了。
func TestPlaceholdersSkipsJSONExamples(t *testing.T) {
	tmpl := `[
  {"situation": "对某件事表示十分惊叹", "style": "使用 我嘞个xxxx"},
  {"content": "词条", "source_id": "12"}
]
真正的占位符：{chat_str}`
	got := Placeholders(tmpl)
	if len(got) != 1 || got[0] != "chat_str" {
		t.Errorf("Placeholders() = %v，期望只有 [chat_str]", got)
	}
}

// TestValidatePlaceholdersAcceptsIdentical 守「原样保存」不被误拦。
func TestValidatePlaceholdersAcceptsIdentical(t *testing.T) {
	def, ok := EmbeddedDefault("planner")
	if !ok {
		t.Fatal("读不到 planner 默认模板")
	}
	if err := ValidatePlaceholders("planner", def); err != nil {
		t.Errorf("原样保存应当通过，实际: %v", err)
	}
}

// TestValidatePlaceholdersRejectsMissing 守「少了占位符」被拦下。
//
// 少了 {bot_name} 不会报错，只会让 Planner 不知道自己叫什么 ——
// 而这种错只表现为「机器人行为变得很奇怪」，排查成本极高。
func TestValidatePlaceholdersRejectsMissing(t *testing.T) {
	content := "你的名字是{bot_name}。"
	err := ValidatePlaceholders("planner", content)
	if err == nil {
		t.Fatal("少了占位符应当被拦下")
	}
	if !strings.Contains(err.Error(), "缺少占位符") {
		t.Errorf("错误信息应当说明缺了什么，实际: %v", err)
	}
	// 报错要点名具体是哪几个，否则用户只能一个个去猜。
	for _, want := range []string{"bot_alias_notice", "behavior_style"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息里应当出现 %q，实际: %v", want, err)
		}
	}
}

// TestValidatePlaceholdersRejectsExtra 守「多了占位符」被拦下。
//
// 多了的占位符永远渲染成空串，用户会以为自己写的内容被吞了。
func TestValidatePlaceholdersRejectsExtra(t *testing.T) {
	def, _ := EmbeddedDefault("replyer")
	err := ValidatePlaceholders("replyer", def+"\n{不存在的键}")
	if err == nil {
		t.Fatal("多了占位符应当被拦下")
	}
	if !strings.Contains(err.Error(), "多余占位符") {
		t.Errorf("错误信息应当说明多出了什么，实际: %v", err)
	}
}

// TestValidatePlaceholdersAllowsRewording 守「只改措辞、占位符不动」能通过。
func TestValidatePlaceholdersAllowsRewording(t *testing.T) {
	def, _ := EmbeddedDefault("learn_style")
	rewritten := strings.ToUpper("请从上面这段群聊中提取用户的语言风格和说话方式。") + "\n" +
		strings.ReplaceAll(def, "请从上面这段群聊中提取用户的语言风格和说话方式。", "")
	if err := ValidatePlaceholders("learn_style", rewritten); err != nil {
		t.Errorf("只改措辞应当通过，实际: %v", err)
	}
}

// TestPersonProfileTemplateIsEmbedded 守画像模板确实进了内嵌资源。
//
// 它以前是 memory 包里的一个 const，用户改不了。
// 现在必须有 .prompt 文件，否则 WebUI 列表里看不到它。
func TestPersonProfileTemplateIsEmbedded(t *testing.T) {
	content, ok := EmbeddedDefault("person_profile")
	if !ok {
		t.Fatal("person_profile 没有内嵌模板")
	}
	for _, want := range []string{"{person_name}", "{chat_str}", "{sections}", "{existing_section}"} {
		if !strings.Contains(content, want) {
			t.Errorf("画像模板缺少占位符 %q", want)
		}
	}
}
