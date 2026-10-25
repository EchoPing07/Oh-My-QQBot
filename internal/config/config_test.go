package config

// 本文件覆盖配置的默认值、校验、模型查找与加载。
// 只新建测试文件，不修改生产代码。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// validConfig 返回一份能通过 Validate 的配置，方便各测试按需改坏某一项。
func validConfig() *Config {
	c := Default()
	c.Bot.Nickname = "木木"
	c.QQ.AppID = "app-id"
	c.QQ.AppSecret = "app-secret"
	return c
}

// TestDefaultKeyValues 钉住几个关键默认值。
//
// 这些数字与 MaiBot 官方配置对齐，改错会让模型的输出质量明显变化，
// 所以用测试固定住，改动必须先改测试。
func TestDefaultKeyValues(t *testing.T) {
	c := Default()

	if got := c.LLM.Tasks.Planner.MaxTokens; got != 4096 {
		t.Errorf("planner max_tokens = %d，期望 4096", got)
	}
	if got := c.Chat.Timing.TalkValue; got != 1 {
		t.Errorf("群聊 talk_value = %v，期望 1", got)
	}
	if got := c.Chat.Timing.PrivateTalkValue; got != 1 {
		t.Errorf("私聊 private_talk_value = %v，期望 1", got)
	}
	if got := c.Web.Port; got != 7800 {
		t.Errorf("web port = %d，期望 7800", got)
	}

	// 其余几项一起钉住，避免默认值悄悄漂移。
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"bot.platform", c.Bot.Platform, "qq"},
		{"bot.nickname", c.Bot.Nickname, "木木"},
		{"qq.api_base_url", c.QQ.APIBaseURL, "https://api.bot.qq.com"},
		{"qq.intents", c.QQ.Intents, 1 << 25},
		{"qq.reconnect_delay_sec", c.QQ.ReconnectDelaySec, 5.0},
		{"chat.max_context_size", c.Chat.MaxContextSize, 40},
		{"chat.max_private_context_size", c.Chat.MaxPrivateContextSize, 60},
		{"chat.timing.max_consecutive_wait_count", c.Chat.Timing.MaxConsecutiveWaitCount, 3},
		{"response.max_split_num", c.Response.MaxSplitNum, 3},
		{"web.enabled", c.Web.Enabled, true},
		{"web.host", c.Web.Host, "127.0.0.1"},
		{"database.path", c.Database.Path, "data/oh-my-qqbot.db"},
		{"memory.query_default_limit", c.Memory.QueryDefaultLimit, 5},
		{"emoji.max_count", c.Emoji.MaxCount, 500},
		{"learning.interval_minutes", c.Learning.IntervalMinutes, 30},
	}
	for _, ck := range checks {
		if ck.got != ck.want {
			t.Errorf("%s = %v，期望 %v", ck.name, ck.got, ck.want)
		}
	}

	// 默认关闭学习：它要花模型调用。
	if c.Learning.ExpressionEnabled || c.Learning.JargonEnabled {
		t.Error("学习开关应默认关闭")
	}
}

// TestValidateMissingFields 校验必填项缺失时报错。
func TestValidateMissingFields(t *testing.T) {
	cases := []struct {
		name         string
		mutate       func(*Config)
		wantContains string
	}{
		{
			name:         "缺 nickname",
			mutate:       func(c *Config) { c.Bot.Nickname = "" },
			wantContains: "bot.nickname 不能为空",
		},
		{
			name: "web 开启但密码为空",
			mutate: func(c *Config) {
				c.Web.Enabled = true
				c.Web.Password = ""
			},
			wantContains: "web.password 不能为空",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := validConfig()
			c.mutate(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatal("期望报错，实际通过校验")
			}
			if !strings.Contains(err.Error(), c.wantContains) {
				t.Errorf("错误信息 = %q，期望包含 %q", err.Error(), c.wantContains)
			}
		})
	}
}

// TestValidateWebDisabledIgnoresPassword 校验 web 关闭时不强制密码。
func TestValidateWebDisabledIgnoresPassword(t *testing.T) {
	cfg := validConfig()
	cfg.Web.Enabled = false
	cfg.Web.Password = ""
	if err := cfg.Validate(); err != nil {
		t.Errorf("web 关闭时不应因密码为空报错: %v", err)
	}
}

// TestValidateFillsDefaults 校验空值兜底填充。
func TestValidateFillsDefaults(t *testing.T) {
	cfg := validConfig()
	cfg.QQ.APIBaseURL = ""
	cfg.QQ.ReconnectDelaySec = 0
	cfg.QQ.Intents = 0
	cfg.Response.MaxSplitNum = 0
	cfg.Memory.QueryDefaultLimit = 0
	cfg.Memory.QueryMaxLimit = 0
	cfg.Memory.SummaryMaxInputChars = 0
	cfg.Memory.ProfileMaxChars = 0
	cfg.Emoji.MaxSizeKB = 0
	cfg.Emoji.MaxCount = 0
	cfg.Emoji.Dir = ""
	cfg.Learning.IntervalMinutes = 0
	cfg.Learning.MinMessages = 0
	cfg.Learning.ExpressionMaxUsed = 0

	if err := cfg.Validate(); err != nil {
		t.Fatalf("兜底填充不应报错: %v", err)
	}

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"qq.api_base_url", cfg.QQ.APIBaseURL, "https://api.bot.qq.com"},
		{"qq.reconnect_delay_sec", cfg.QQ.ReconnectDelaySec, 5.0},
		{"qq.intents", cfg.QQ.Intents, 1 << 25},
		{"response.max_split_num", cfg.Response.MaxSplitNum, 3},
		{"memory.query_default_limit", cfg.Memory.QueryDefaultLimit, 5},
		{"memory.query_max_limit", cfg.Memory.QueryMaxLimit, 5},
		{"memory.summary_max_input_chars", cfg.Memory.SummaryMaxInputChars, 16000},
		{"memory.profile_max_chars", cfg.Memory.ProfileMaxChars, 900},
		{"emoji.max_size_kb", cfg.Emoji.MaxSizeKB, 1024},
		{"emoji.max_count", cfg.Emoji.MaxCount, 500},
		{"emoji.dir", cfg.Emoji.Dir, "data/emoji"},
		{"learning.interval_minutes", cfg.Learning.IntervalMinutes, 30},
		{"learning.min_messages", cfg.Learning.MinMessages, 30},
		{"learning.expression_max_used", cfg.Learning.ExpressionMaxUsed, 5},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v，期望兜底为 %v", c.name, c.got, c.want)
		}
	}
}

// TestValidateProviderRequiredFields 校验服务商缺 name / base_url 时报错。
func TestValidateProviderRequiredFields(t *testing.T) {
	cases := []struct {
		name         string
		provider     ProviderConfig
		wantContains string
	}{
		{"缺 name", ProviderConfig{BaseURL: "http://x"}, "llm.providers[0].name 不能为空"},
		{"缺 base_url", ProviderConfig{Name: "p1"}, "llm.providers[0].base_url 不能为空"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.LLM.Providers = []ProviderConfig{c.provider}
			err := cfg.Validate()
			if err == nil {
				t.Fatal("期望报错")
			}
			if !strings.Contains(err.Error(), c.wantContains) {
				t.Errorf("错误信息 = %q，期望包含 %q", err.Error(), c.wantContains)
			}
		})
	}
}

// TestValidateKeywordRules 覆盖关键词规则的三类错误。
//
// 正则在启动时编译一次，配错必须当场报错，而不是等某条消息命中才炸。
func TestValidateKeywordRules(t *testing.T) {
	cases := []struct {
		name         string
		rule         KeywordRule
		wantContains string
	}{
		{
			name:         "正则非法",
			rule:         KeywordRule{Regex: []string{"([a-z"}, Reaction: "装作没听懂"},
			wantContains: "无效",
		},
		{
			name:         "keywords 与 regex 都为空",
			rule:         KeywordRule{Reaction: "装作没听懂"},
			wantContains: "至少需要 keywords 或 regex 之一",
		},
		{
			name:         "reaction 为空",
			rule:         KeywordRule{Keywords: []string{"木木"}},
			wantContains: "reaction 不能为空",
		},
		{
			name:         "reaction 只有空白",
			rule:         KeywordRule{Keywords: []string{"木木"}, Reaction: "   "},
			wantContains: "reaction 不能为空",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Keyword.Rules = []KeywordRule{c.rule}
			err := cfg.Validate()
			if err == nil {
				t.Fatal("期望报错，实际通过校验")
			}
			if !strings.Contains(err.Error(), c.wantContains) {
				t.Errorf("错误信息 = %q，期望包含 %q", err.Error(), c.wantContains)
			}
		})
	}
}

// TestValidateKeywordRuleValid 校验合法规则能通过。
func TestValidateKeywordRuleValid(t *testing.T) {
	cfg := validConfig()
	cfg.Keyword.Rules = []KeywordRule{
		{Keywords: []string{"木木"}, Reaction: "装作没听懂"},
		{Regex: []string{`^在吗[?？]?$`}, Reaction: "回一句在"},
		{Keywords: []string{"早"}, Regex: []string{"早安"}, Reaction: "打个招呼"},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("合法关键词规则不应报错: %v", err)
	}
}

// TestSessionContextSize 校验群聊/私聊上下文条数分流。
func TestSessionContextSize(t *testing.T) {
	c := Default()
	if got := c.SessionContextSize(true); got != c.Chat.MaxContextSize {
		t.Errorf("群聊上下文 = %d，期望 %d", got, c.Chat.MaxContextSize)
	}
	if got := c.SessionContextSize(false); got != c.Chat.MaxPrivateContextSize {
		t.Errorf("私聊上下文 = %d，期望 %d", got, c.Chat.MaxPrivateContextSize)
	}
}

// TestTalkValue 防的是 AGENTS.md 第 15 条：
// talk_value = 0 是合法配置（完全不说话），不能被当成「未设置」。
func TestTalkValue(t *testing.T) {
	c := Default()
	c.Chat.Timing.TalkValue = 0
	c.Chat.Timing.PrivateTalkValue = 0.5

	if got := c.TalkValue(true); got != 0 {
		t.Errorf("群聊 talk_value = %v，期望 0（0 是合法值）", got)
	}
	if got := c.TalkValue(false); got != 0.5 {
		t.Errorf("私聊 talk_value = %v，期望 0.5", got)
	}
}

// TestProviderTimeoutHelpers 校验超时/重试的兜底默认值。
func TestProviderTimeoutHelpers(t *testing.T) {
	// timeout 不写 = 0 = 不限时。
	//
	// 以前这里兜底成 120 秒，于是「不写」和「写 120」是同一件事，
	// 用户没法表达「这个服务商就是很慢，别掐我」。
	// 真正的兜底在 llm 层（取任务硬超时与服务商超时里较小的一个）。
	p := ProviderConfig{}
	if got := p.TimeoutDuration(); got != 0 {
		t.Errorf("默认超时 = %v，期望 0（不限）", got)
	}
	if got := p.Retry(); got != 1 {
		t.Errorf("默认重试次数 = %d，期望 1", got)
	}
	if got := p.RetryIntervalDuration(); got != 4*time.Second {
		t.Errorf("默认重试间隔 = %v，期望 4s", got)
	}

	p = ProviderConfig{Timeout: 30, MaxRetry: 3, RetryInterval: 2}
	if got := p.TimeoutDuration(); got != 30*time.Second {
		t.Errorf("超时 = %v，期望 30s", got)
	}
	if got := p.Retry(); got != 3 {
		t.Errorf("重试次数 = %d，期望 3", got)
	}
	if got := p.RetryIntervalDuration(); got != 2*time.Second {
		t.Errorf("重试间隔 = %v，期望 2s", got)
	}
}

// TestModelIsVisual 防的是 AGENTS.md 第 19 条：
// 给纯文本模型发 image_url 会被服务端 400，所以判定必须保守。
func TestModelIsVisual(t *testing.T) {
	t.Run("首选模型 visual=true", func(t *testing.T) {
		c := Default()
		c.LLM.Models = []ModelConfig{{Name: "vlm", Visual: true}}
		c.LLM.Tasks.Planner.Models = []string{"vlm"}
		if !c.ModelIsVisual(TaskPlanner) {
			t.Error("visual=true 的模型应判定为多模态")
		}
	})

	t.Run("首选模型 visual=false", func(t *testing.T) {
		c := Default()
		c.LLM.Models = []ModelConfig{{Name: "text", Visual: false}}
		c.LLM.Tasks.Planner.Models = []string{"text"}
		if c.ModelIsVisual(TaskPlanner) {
			t.Error("visual=false 的模型不应判定为多模态")
		}
	})

	t.Run("没有模型时保守返回 false", func(t *testing.T) {
		c := Default()
		if c.ModelIsVisual(TaskPlanner) {
			t.Error("没有配置模型时应返回 false")
		}
	})

	t.Run("模型名查不到时保守返回 false", func(t *testing.T) {
		c := Default()
		c.LLM.Tasks.Planner.Models = []string{"不存在的模型"}
		if c.ModelIsVisual(TaskPlanner) {
			t.Error("查不到模型时应返回 false")
		}
	})
}

// TestLoadFromTempFile 用临时 toml 校验加载与默认值合并。
func TestLoadFromTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	content := `
[bot]
nickname = "测试木木"

[qq]
app_id = "id-from-file"
app_secret = "secret-from-file"

[chat.timing]
talk_value = 0.25

[web]
enabled = false
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写入临时配置失败: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if cfg.Bot.Nickname != "测试木木" {
		t.Errorf("nickname = %q，期望 %q", cfg.Bot.Nickname, "测试木木")
	}
	if cfg.QQ.AppID != "id-from-file" || cfg.QQ.AppSecret != "secret-from-file" {
		t.Errorf("qq 凭据未从文件读取: %q / %q", cfg.QQ.AppID, cfg.QQ.AppSecret)
	}
	if cfg.Chat.Timing.TalkValue != 0.25 {
		t.Errorf("talk_value = %v，期望 0.25", cfg.Chat.Timing.TalkValue)
	}
	// 未出现在文件里的字段应保留默认值。
	if cfg.LLM.Tasks.Planner.MaxTokens != 4096 {
		t.Errorf("未配置的 planner max_tokens = %d，期望保留默认 4096", cfg.LLM.Tasks.Planner.MaxTokens)
	}
	if cfg.Web.Port != 7800 {
		t.Errorf("未配置的 web.port = %d，期望保留默认 7800", cfg.Web.Port)
	}
}

// TestLoadMissingFile 校验文件不存在时报错且错误带路径。
func TestLoadMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope.toml")
	_, err := Load(path)
	if err == nil {
		t.Fatal("文件不存在应报错")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("错误信息 = %q，期望包含路径 %q", err.Error(), path)
	}
}

// TestLoadInvalidTOML 校验坏 TOML 报错。
func TestLoadInvalidTOML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.toml")
	if err := os.WriteFile(path, []byte("[bot\nnickname="), 0o644); err != nil {
		t.Fatalf("写入临时配置失败: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("非法 TOML 应报错")
	}
}

// TestLoadAppliesEnvOverride 校验环境变量能覆盖敏感项。
func TestLoadAppliesEnvOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	content := `
[bot]
nickname = "木木"

[qq]
app_id = "from-file"
app_secret = "from-file-secret"

[web]
enabled = false
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写入临时配置失败: %v", err)
	}

	t.Setenv("OMQ_QQ_APP_ID", "from-env")
	t.Setenv("OMQ_QQ_APP_SECRET", "secret-from-env")
	t.Setenv("OMQ_WEB_PASSWORD", "web-pass-from-env")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if cfg.QQ.AppID != "from-env" {
		t.Errorf("app_id = %q，期望被环境变量覆盖", cfg.QQ.AppID)
	}
	if cfg.QQ.AppSecret != "secret-from-env" {
		t.Errorf("app_secret = %q，期望被环境变量覆盖", cfg.QQ.AppSecret)
	}
	if cfg.Web.Password != "web-pass-from-env" {
		t.Errorf("web.password = %q，期望被环境变量覆盖", cfg.Web.Password)
	}
}

// TestSaveFailsOnBadPath 校验 Save 在目录不可写时报错（不静默成功）。
func TestSaveFailsOnBadPath(t *testing.T) {
	dir := t.TempDir()
	// 把一个普通文件当作目录用的父路径，MkdirAll 必然失败。
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("准备阻塞文件失败: %v", err)
	}

	if err := Save(Default(), filepath.Join(blocker, "config.toml")); err == nil {
		t.Fatal("不可写路径应报错")
	}
}

// TestSaveAndReload 校验 Save/Load 往返一致。
func TestSaveAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.toml")
	cfg := validConfig()
	cfg.Bot.AliasNames = []string{"小木", "木木子"}
	cfg.Keyword.Rules = []KeywordRule{{Keywords: []string{"早"}, Reaction: "打个招呼"}}

	if err := Save(cfg, path); err != nil {
		t.Fatalf("保存配置失败: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取保存后的配置失败: %v", err)
	}
	if !strings.Contains(string(raw), "Oh-My-QQBot 配置文件") {
		t.Error("保存的文件应带中文注释头")
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("重新加载失败: %v", err)
	}
	if len(loaded.Bot.AliasNames) != 2 || loaded.Bot.AliasNames[0] != "小木" {
		t.Errorf("别名 = %v，期望 [小木 木木子]", loaded.Bot.AliasNames)
	}
	if len(loaded.Keyword.Rules) != 1 || loaded.Keyword.Rules[0].Reaction != "打个招呼" {
		t.Errorf("关键词规则 = %+v", loaded.Keyword.Rules)
	}
}

// TestLoadOrCreateCreatesMissing 校验文件不存在时创建默认配置。
func TestLoadOrCreateCreatesMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new", "config.toml")

	cfg, created, err := LoadOrCreate(path)
	if err != nil {
		t.Fatalf("LoadOrCreate 失败: %v", err)
	}
	if !created {
		t.Error("文件不存在时 created 应为 true")
	}
	if cfg.Web.Port != 7800 || cfg.LLM.Tasks.Planner.MaxTokens != 4096 {
		t.Errorf("创建的配置不是默认值: port=%d max_tokens=%d", cfg.Web.Port, cfg.LLM.Tasks.Planner.MaxTokens)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("配置文件应被创建: %v", err)
	}

	// 默认配置里 app_id / app_secret 是空串，直接二次 Load 会因校验失败报错。
	// 这里按真实用法把文件里的空值改成真凭据。
	patched := strings.NewReplacer(
		`app_id = ''`, `app_id = 'id'`,
		`app_secret = ''`, `app_secret = 'secret'`,
	).Replace(string(readFile(t, path)))
	if err := os.WriteFile(path, []byte(patched), 0o644); err != nil {
		t.Fatalf("写入凭据失败: %v", err)
	}

	again, createdAgain, err := LoadOrCreate(path)
	if err != nil {
		t.Fatalf("二次 LoadOrCreate 失败: %v", err)
	}
	if createdAgain {
		t.Error("文件已存在时 created 应为 false")
	}
	if again == nil {
		t.Fatal("二次加载不应返回 nil")
	}
	if again.QQ.AppID != "id" {
		t.Errorf("app_id = %q，期望从文件读到 %q", again.QQ.AppID, "id")
	}
}

// TestLoadOrCreateDefaultHasNoCredentials 防的是「首次启动能跑、第二次启动崩」。
//
// 这是真发生过的缺陷：Save 写出的默认配置里 QQ 凭据为空，
// 如果 Validate 硬性拒绝空凭据，第二次启动就会直接报错退出。
// 现在的设计是凭据缺失只警告、不阻断启动，
// 所以这里要求二次加载必须成功，且 MissingCredentials 能报出缺了什么。
func TestLoadOrCreateDefaultHasNoCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if _, _, err := LoadOrCreate(path); err != nil {
		t.Fatalf("首次创建失败: %v", err)
	}

	cfg, _, err := LoadOrCreate(path)
	if err != nil {
		t.Fatalf("二次加载不应失败（凭据缺失只警告）: %v", err)
	}
	if cfg == nil {
		t.Fatal("二次加载不应返回 nil")
	}

	missing := cfg.MissingCredentials()
	if len(missing) != 2 {
		t.Fatalf("缺失凭据 = %v，期望 app_id 与 app_secret 两项", missing)
	}
	if !strings.Contains(strings.Join(missing, ","), "qq.app_id") {
		t.Errorf("缺失项里应包含 qq.app_id，实际 = %v", missing)
	}
}

// TestMissingCredentialsEmptyWhenFilled 验证凭据齐全时没有告警。
func TestMissingCredentialsEmptyWhenFilled(t *testing.T) {
	cfg := validConfig()
	if got := cfg.MissingCredentials(); len(got) != 0 {
		t.Errorf("凭据齐全时不该有缺失项，实际 = %v", got)
	}
}

// TestLoadOrCreatePropagatesSaveError 校验目录不可写时错误被向上抛，而不是静默返回默认值。
func TestLoadOrCreatePropagatesSaveError(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("准备阻塞文件失败: %v", err)
	}

	if _, _, err := LoadOrCreate(filepath.Join(blocker, "config.toml")); err == nil {
		t.Fatal("创建配置失败时应报错")
	}
}

// readFile 读文件内容，失败直接终止测试。
func readFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	return raw
}

// TestKeywordRulesApply 守关键词规则的结构化编辑。
//
// 这是「配置项有数据源但没入口」的补齐：keyword.rules 一直在
// EditableFields 之外（列表类型塞不进点分键表单），于是
// 网页上只能看到一个条数，改一个字都要打开 config.toml。
func TestKeywordRulesApply(t *testing.T) {
	cfg := Default()

	// 1. 正常写入。
	err := cfg.ApplyKeywordRules([]KeywordRuleView{
		{Keywords: []string{" 猫 ", "狗"}, Regex: nil, Reaction: " 聊宠物 "},
		{Keywords: nil, Regex: []string{`(?i)help`}, Reaction: "给做法"},
	})
	if err != nil {
		t.Fatalf("ApplyKeywordRules 失败: %v", err)
	}
	if len(cfg.Keyword.Rules) != 2 {
		t.Fatalf("条数 = %d，期望 2", len(cfg.Keyword.Rules))
	}
	// 空白要被剔除：空串在关键词里会匹配「所有消息」。
	if got := cfg.Keyword.Rules[0].Keywords; len(got) != 2 || got[0] != "猫" {
		t.Errorf("关键词 = %v，期望 [猫 狗]", got)
	}
	// 反应也要 trim。
	if got := cfg.Keyword.Rules[0].Reaction; got != "聊宠物" {
		t.Errorf("反应 = %q，期望「聊宠物」", got)
	}

	// 2. 读回来的视图与写入一致（Snapshot/ApplyField 用的就是它）。
	view := cfg.KeywordRulesView()
	if len(view) != 2 || view[1].Regex[0] != `(?i)help` {
		t.Errorf("KeywordRulesView 与写入不一致: %+v", view)
	}

	// 3. 视图是拷贝：改它不能影响配置。
	view[0].Keywords[0] = "被改了"
	if cfg.Keyword.Rules[0].Keywords[0] != "猫" {
		t.Error("KeywordRulesView 返回的是内部切片，外部修改污染了配置")
	}

	// 4. 空行被跳过（表格编辑器点「添加」必然产生空行）。
	err = cfg.ApplyKeywordRules([]KeywordRuleView{
		{Keywords: []string{"a"}, Reaction: "r"},
		{},
	})
	if err != nil {
		t.Fatalf("含空行时失败: %v", err)
	}
	if len(cfg.Keyword.Rules) != 1 {
		t.Errorf("空行应被跳过，实际 %d 条", len(cfg.Keyword.Rules))
	}

	// 5. 清空是合法的。
	if err := cfg.ApplyKeywordRules(nil); err != nil {
		t.Fatalf("清空失败: %v", err)
	}
	if len(cfg.Keyword.Rules) != 0 {
		t.Error("清空后仍有规则")
	}
}

// TestKeywordRulesValidation 守逐条校验与「第几条」定位。
//
// 错误信息必须指到具体某条：几十条规则里报一句「正则无效」
// 等于什么都没说。
func TestKeywordRulesValidation(t *testing.T) {
	cfg := Default()

	cases := []struct {
		name string
		in   []KeywordRuleView
		want string
	}{
		{
			name: "缺触发条件",
			in:   []KeywordRuleView{{Reaction: "只有反应"}},
			want: "第 1 条",
		},
		{
			name: "反应为空",
			in:   []KeywordRuleView{{Keywords: []string{"猫"}}},
			want: "反应内容不能为空",
		},
		{
			name: "正则非法",
			in:   []KeywordRuleView{{Keywords: []string{"a"}, Regex: []string{"["}, Reaction: "r"}},
			want: "第 1 条",
		},
		{
			name: "第二条出错要指到第二条",
			in: []KeywordRuleView{
				{Keywords: []string{"ok"}, Reaction: "fine"},
				{Reaction: "缺触发"},
			},
			want: "第 2 条",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := cfg.ApplyKeywordRules(tc.in)
			if err == nil {
				t.Fatal("期望报错，实际通过")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误 %q 里没有 %q", err, tc.want)
			}
		})
	}

	// 校验失败时不能把配置改成半截。
	cfg.Keyword.Rules = []KeywordRule{{Keywords: []string{"原"}, Reaction: "原反应"}}
	_ = cfg.ApplyKeywordRules([]KeywordRuleView{{Keywords: []string{"新"}, Reaction: ""}})
	if len(cfg.Keyword.Rules) != 1 || cfg.Keyword.Rules[0].Keywords[0] != "原" {
		t.Errorf("校验失败时旧规则被改动: %+v", cfg.Keyword.Rules)
	}
}

// TestAliasAndStyleFieldsEditable 守两个「有数据源但没入口」的字段。
//
// bot.alias_names 与 personality.multiple_reply_style 都是列表，
// 以前既不在 EditableFields 里、也没有单独的编辑接口，
// 网页上完全改不了。后者更糟：multiple_probability 在界面上，
// 但它依赖的风格列表永远是空的，于是那个概率旋钮永远空转。
func TestAliasAndStyleFieldsEditable(t *testing.T) {
	cfg := Default()
	cfg.Bot.AliasNames = []string{"小木"}
	cfg.Personality.MultipleReplyStyle = []string{"风格A"}

	snap := cfg.Snapshot()
	for _, key := range []string{"bot.alias_names", "personality.multiple_reply_style"} {
		if _, ok := snap[key]; !ok {
			t.Errorf("Snapshot 里缺少 %s（界面上那一行永远是空的）", key)
		}
	}

	// 写回。
	if err := cfg.ApplyField("bot.alias_names", []any{"木宝", "小木头"}); err != nil {
		t.Fatalf("写 alias_names 失败: %v", err)
	}
	if len(cfg.Bot.AliasNames) != 2 || cfg.Bot.AliasNames[0] != "木宝" {
		t.Errorf("AliasNames = %v", cfg.Bot.AliasNames)
	}

	if err := cfg.ApplyField("personality.multiple_reply_style", []any{"简短", "带翻译腔"}); err != nil {
		t.Fatalf("写 multiple_reply_style 失败: %v", err)
	}
	if len(cfg.Personality.MultipleReplyStyle) != 2 {
		t.Errorf("MultipleReplyStyle = %v", cfg.Personality.MultipleReplyStyle)
	}

	// 空项要被剔除：多行文本框很容易多敲一个空行，
	// 而空风格会被 selectReplyStyle 当成一条合法风格选中，产出一句空回复。
	if err := cfg.ApplyField("personality.multiple_reply_style", []any{"  ", ""}); err != nil {
		t.Fatalf("写空列表失败: %v", err)
	}
	if len(cfg.Personality.MultipleReplyStyle) != 0 {
		t.Errorf("空项没被剔除: %v", cfg.Personality.MultipleReplyStyle)
	}
}

// TestDefaultNamingAndPassword 钉住「项目名与默认凭据」这一组默认值。
//
// 背景：项目早期把默认机器人昵称「木木」当成了项目名，二进制、日志文件、
// 环境变量前缀、WebUI 密码全都叫 mumu。昵称是可配置的（bot.nickname），
// 项目名不该跟着它走。这里把纠正后的值固定住，防止有人「顺手改回去」。
func TestDefaultNamingAndPassword(t *testing.T) {
	c := Default()

	if c.Web.Password != "password" {
		t.Errorf("默认密码 = %q，期望 password", c.Web.Password)
	}
	// 库路径跟着项目名走，改名后旧库读不到是预期行为（不做迁移）。
	if c.Database.Path != "data/oh-my-qqbot.db" {
		t.Errorf("默认库路径 = %q", c.Database.Path)
	}
	// 环境变量前缀必须是 OMQ_，不能再是 MUMU_。
	if EnvQQAppID != "OMQ_QQ_APP_ID" || EnvQQAppSecret != "OMQ_QQ_APP_SECRET" {
		t.Errorf("环境变量名 = %q / %q", EnvQQAppID, EnvQQAppSecret)
	}
}

// TestSaveHeaderUsesProjectName 守住配置文件头的项目名。
//
// 这个头会出现在每个新生成的 config.toml 第一行，是用户第一眼看到的东西。
func TestSaveHeaderUsesProjectName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := Save(Default(), path); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	head := string(raw)
	if !strings.Contains(head, "Oh-My-QQBot 配置文件") {
		t.Errorf("配置文件头缺项目名: %q", head[:min(len(head), 80)])
	}
	if strings.Contains(head, "木木 QQBot") {
		t.Error("配置文件头仍在使用旧的「木木 QQBot」品牌名")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestBuildAuthHeaderValue 三态验证鉴权头拼接语义（docs/audit/11-infra.md P1-5）。
//
// 语义逐字对齐参考实现 openai_compat.py:63-76 的 _build_auth_header_value。
// 以前 llm 层无条件补一个空格、embedding 层不补，prefix 写成 "Bearer "
// （带尾空格）时两条链路发出的头必有一条 401。
func TestBuildAuthHeaderValue(t *testing.T) {
	cases := []struct {
		name   string
		prefix string
		want   string
	}{
		{"常规前缀", "Bearer", "Bearer sk-xxx"},
		{"前缀带尾空格", "Bearer ", "Bearer sk-xxx"},
		{"前缀带首尾空格", " Bearer ", "Bearer sk-xxx"},
		{"空前缀只发密钥", "", "sk-xxx"},
		{"纯空白前缀视为空", "   ", "sk-xxx"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := BuildAuthHeaderValue(tc.prefix, "sk-xxx"); got != tc.want {
				t.Errorf("BuildAuthHeaderValue(%q) = %q，期望 %q", tc.prefix, got, tc.want)
			}
		})
	}
}
