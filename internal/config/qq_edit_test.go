package config

// QQ 凭据的网页读写测试。
//
// 这个文件守的核心是一件事：**AppSecret 永不出现在响应里**。
// 它不是「顺手检查」—— secret 一旦跟着 Snapshot 出去，
// 浏览器历史、开发者工具、代理日志里都会留一份。

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestQQSnapshotHasNoSecret 守：视图结构体里没有密钥。
func TestQQSnapshotHasNoSecret(t *testing.T) {
	cfg := Default()
	cfg.QQ.AppID = "102000000"
	cfg.QQ.AppSecret = "sk-do-not-leak-aaaaaaaaaaaa"

	view := cfg.QQSnapshot()
	if !view.SecretSet {
		t.Error("SecretSet 应为 true")
	}

	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if strings.Contains(string(raw), "sk-do-not-leak") {
		t.Errorf("响应里出现了密钥: %s", raw)
	}
	// 精确查找字段名，不能用子串匹配：env_app_secret 与 secret_set
	// 都包含 "app_secret" 这三个词根，子串判断会假失败。
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if _, ok := probe["app_secret"]; ok {
		t.Errorf("响应里出现了 app_secret 字段: %s", raw)
	}
}

// TestQQSnapshotMissing 守：缺凭据时能说清缺哪个。
func TestQQSnapshotMissing(t *testing.T) {
	cfg := Default()
	cfg.QQ.AppID = ""
	cfg.QQ.AppSecret = ""

	view := cfg.QQSnapshot()
	if view.Ready {
		t.Error("凭据为空时 Ready 应为 false")
	}
	if len(view.Missing) != 2 {
		t.Errorf("Missing = %v，期望 2 项", view.Missing)
	}
	// 空切片而不是 nil：nil 序列化成 null，前端 forEach 会抛错（第 67 条）。
	raw, _ := json.Marshal(view)
	if strings.Contains(string(raw), `"missing":null`) {
		t.Errorf("missing 不能是 null: %s", raw)
	}
}

// TestApplyQQPatchKeepsSecretWhenEmpty 守：留空 = 不修改。
//
// 这是「不回显密钥」的必然代价：如果留空被当成清空，
// 用户每改一次只改 AppID 就会把密钥抹掉，而界面上看起来一切正常。
func TestApplyQQPatchKeepsSecretWhenEmpty(t *testing.T) {
	cfg := Default()
	cfg.QQ.AppID = "111"
	cfg.QQ.AppSecret = "old-secret"

	if err := cfg.ApplyQQPatch(QQPatch{AppID: "222"}); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if cfg.QQ.AppID != "222" {
		t.Errorf("AppID = %q", cfg.QQ.AppID)
	}
	if cfg.QQ.AppSecret != "old-secret" {
		t.Errorf("留空时密钥不该变，实际 %q", cfg.QQ.AppSecret)
	}
}

// TestApplyQQPatchClearSecret 守：显式清空。
func TestApplyQQPatchClearSecret(t *testing.T) {
	cfg := Default()
	cfg.QQ.AppSecret = "old-secret"

	if err := cfg.ApplyQQPatch(QQPatch{AppID: "111", ClearAppSecret: true}); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if cfg.QQ.AppSecret != "" {
		t.Errorf("应当被清空，实际 %q", cfg.QQ.AppSecret)
	}
}

// TestApplyQQPatchOverwritesSecret 守：填了新值就覆盖。
func TestApplyQQPatchOverwritesSecret(t *testing.T) {
	cfg := Default()
	cfg.QQ.AppSecret = "old"

	if err := cfg.ApplyQQPatch(QQPatch{AppID: "111", AppSecret: "new"}); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if cfg.QQ.AppSecret != "new" {
		t.Errorf("密钥 = %q，期望 new", cfg.QQ.AppSecret)
	}
}

// TestApplyQQPatchRejectsNonDigitAppID 守：AppID 只能是数字。
//
// 最常见的填错是把 AppSecret 粘进 AppID 框 —— 两者都是长串，
// 肉眼很难分辨，而错误的 AppID 只表现为「鉴权失败」，
// 报错点离填错的地方很远。
func TestApplyQQPatchRejectsNonDigitAppID(t *testing.T) {
	cfg := Default()
	err := cfg.ApplyQQPatch(QQPatch{AppID: "abc123"})
	if err == nil {
		t.Fatal("非数字 AppID 应当被拒绝")
	}
	if !strings.Contains(err.Error(), "数字") {
		t.Errorf("错误信息应说明原因，实际: %v", err)
	}
}

// TestApplyQQPatchRejectsWhitespaceInSecret 守：密钥里不能有空白。
//
// 从网页复制粘贴时很容易带上首尾空白或内部换行，
// 而这类值会让鉴权莫名其妙失败。
func TestApplyQQPatchRejectsWhitespaceInSecret(t *testing.T) {
	cfg := Default()
	err := cfg.ApplyQQPatch(QQPatch{AppID: "111", AppSecret: "a b"})
	if err == nil {
		t.Fatal("含空白的密钥应当被拒绝")
	}
}

// TestApplyQQPatchTrimsValues 守：首尾空白会被去掉（而不是报错）。
//
// AppID 从平台复制过来常常带一个尾随空格，
// 为这个报错太苛刻，直接 trim 掉更好。
func TestApplyQQPatchTrimsValues(t *testing.T) {
	cfg := Default()
	if err := cfg.ApplyQQPatch(QQPatch{AppID: "  111  ", AppSecret: "  secret  "}); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if cfg.QQ.AppID != "111" {
		t.Errorf("AppID = %q", cfg.QQ.AppID)
	}
	if cfg.QQ.AppSecret != "secret" {
		t.Errorf("AppSecret = %q", cfg.QQ.AppSecret)
	}
}

// TestQQSnapshotReportsEnvOverride 守：环境变量覆盖要说出来。
//
// 不说的话会得到一个几乎无法自查的现象：网页上改了凭据、
// 提示保存成功、重启后还是连不上旧账号。
func TestQQSnapshotReportsEnvOverride(t *testing.T) {
	t.Setenv(EnvQQAppID, "999")
	cfg := Default()
	cfg.QQ.AppID = "111"

	view := cfg.QQSnapshot()
	if !view.EnvAppID {
		t.Error("EnvAppID 应为 true")
	}
	if view.EnvAppSecret {
		t.Error("没设环境变量时 EnvAppSecret 应为 false")
	}
}

// TestQQSnapshotChangesPersistThroughClone 守：凭据能被 Clone 带过去。
//
// Store.Update 走的是 Clone（TOML 往返），如果某个字段的 toml tag
// 写错，表现就是「界面上保存成功，重启后凭据回到空」。
func TestQQCredentialsSurviveClone(t *testing.T) {
	cfg := Default()
	cfg.QQ.AppID = "102000000"
	cfg.QQ.AppSecret = "secret"
	cfg.QQ.UnifiedAccountID = "u1"

	clone, err := cfg.Clone()
	if err != nil {
		t.Fatalf("Clone 失败: %v", err)
	}
	if clone.QQ.AppID != "102000000" {
		t.Errorf("AppID = %q", clone.QQ.AppID)
	}
	if clone.QQ.AppSecret != "secret" {
		t.Errorf("AppSecret = %q", clone.QQ.AppSecret)
	}
	if clone.QQ.UnifiedAccountID != "u1" {
		t.Errorf("UnifiedAccountID = %q", clone.QQ.UnifiedAccountID)
	}
}
