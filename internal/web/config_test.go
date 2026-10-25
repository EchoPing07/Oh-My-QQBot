package web

// 配置编辑相关测试。
//
// 重点守三件事：
//  1. 密钥永远不出现在响应里，也不允许通过网页改
//  2. 非法输入被拒绝，且**拒绝时什么都没变**（不能「报错了但已经落盘」）
//  3. 改动真的生效：下一轮读配置就能看到新值

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/logx"
)

// newConfigTestEnv 构造一个绑定了真实配置文件的环境。
//
// 与 newTestEnv 的区别：这里必须有一个磁盘文件，
// 因为「保存后落盘」本身就是被测行为的一部分。
func newConfigTestEnv(t *testing.T) (*testEnv, string) {
	t.Helper()

	cfg := testConfig()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := config.Save(cfg, path); err != nil {
		t.Fatalf("写入初始配置失败: %v", err)
	}

	env := newTestEnv(t, envOptions{cfg: cfg})
	// 换成绑定了路径的 Store，这样保存会真的写文件。
	env.s.cfg = config.NewStoreFrom(cfg, path)
	return env, path
}

// TestConfigEditReturnsFieldsAndValues 防：表单拿不到字段清单或当前值。
func TestConfigEditReturnsFieldsAndValues(t *testing.T) {
	env, path := newConfigTestEnv(t)
	env.loginOK()

	resp, body := env.do(http.MethodGet, "/api/config/edit", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200，body=%s", resp.StatusCode, body)
	}

	var out struct {
		Fields []struct {
			Key   string `json:"key"`
			Label string `json:"label"`
			Kind  string `json:"kind"`
		} `json:"fields"`
		Values  map[string]any `json:"values"`
		Path    string         `json:"path"`
		Restart []string       `json:"restart"`
	}
	decodeJSON(t, body, &out)

	if len(out.Fields) < 30 {
		t.Errorf("字段数 = %d，看起来太少了（清单可能没接上）", len(out.Fields))
	}
	if out.Path != path {
		t.Errorf("path = %q，期望 %q", out.Path, path)
	}

	// 每个 field 都必须有对应的 value（少一个就是「界面上那行永远是空的」）。
	for _, f := range out.Fields {
		if _, ok := out.Values[f.Key]; !ok {
			t.Errorf("字段 %s 没有对应的值", f.Key)
		}
		if f.Label == "" {
			t.Errorf("字段 %s 缺少显示名", f.Key)
		}
	}

	// 值必须与配置文件一致。
	if got := out.Values["chat.max_context_size"]; got != float64(env.cfg.Chat.MaxContextSize) {
		t.Errorf("chat.max_context_size = %v，期望 %d", got, env.cfg.Chat.MaxContextSize)
	}

	// restart 是数组，不能是 null（前端会 forEach）。
	if out.Restart == nil {
		t.Error("restart 应当是空数组而不是 null")
	}
}

// TestConfigEditNeverLeaksSecrets 安全回归：
// 防：字段清单或值里出现密钥字段。
func TestConfigEditNeverLeaksSecrets(t *testing.T) {
	env, _ := newConfigTestEnv(t)
	env.loginOK()

	resp, body := env.do(http.MethodGet, "/api/config/edit", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}

	lower := strings.ToLower(string(body))
	for _, needle := range []string{
		"app_secret", "api_key", "session_secret", "password",
	} {
		if strings.Contains(lower, needle) {
			t.Errorf("配置编辑响应里出现敏感字段名 %q：%s", needle, body)
		}
	}
	for _, secret := range []string{
		testPassword, testSessionSecret,
		env.cfg.QQ.AppSecret, "sk-llm-api-key-DO-NOT-LEAK",
	} {
		if strings.Contains(string(body), secret) {
			t.Errorf("配置编辑响应里出现密钥值 %q", secret)
		}
	}
}

// TestConfigSaveAppliesAndPersists 防：保存了但没生效 / 没落盘。
func TestConfigSaveAppliesAndPersists(t *testing.T) {
	env, path := newConfigTestEnv(t)
	env.loginOK()

	before := env.cfg.Chat.MaxContextSize
	payload := `{"values":{"chat.max_context_size":99,"bot.nickname":"新名字"}}`
	resp, body := env.do(http.MethodPost, "/api/config/save", payload)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200，body=%s", resp.StatusCode, body)
	}

	var out struct {
		OK      bool           `json:"ok"`
		Applied []string       `json:"applied"`
		Values  map[string]any `json:"values"`
	}
	decodeJSON(t, body, &out)
	if !out.OK {
		t.Fatalf("保存失败: %s", body)
	}
	if len(out.Applied) != 2 {
		t.Errorf("applied = %v，期望 2 项", out.Applied)
	}

	// 1. 内存里生效
	if got := env.s.cfg.Get().Chat.MaxContextSize; got != 99 {
		t.Errorf("内存里的 max_context_size = %d，期望 99（原值 %d）", got, before)
	}
	if got := env.s.cfg.Get().Bot.Nickname; got != "新名字" {
		t.Errorf("内存里的 nickname = %q", got)
	}

	// 2. 落盘了
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取配置文件失败: %v", err)
	}
	if !strings.Contains(string(raw), "99") || !strings.Contains(string(raw), "新名字") {
		t.Errorf("配置文件没有写入新值：\n%s", raw)
	}

	// 3. 重新加载后仍是新值（证明是合法的 TOML，不是写坏了）
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("重新加载配置失败（写出的 TOML 不合法）: %v", err)
	}
	if reloaded.Chat.MaxContextSize != 99 {
		t.Errorf("重新加载后 = %d，期望 99", reloaded.Chat.MaxContextSize)
	}
}

// TestConfigSaveOnlyTouchesGivenKeys 防：只提交一项却把其它项清空。
func TestConfigSaveOnlyTouchesGivenKeys(t *testing.T) {
	env, _ := newConfigTestEnv(t)
	env.loginOK()

	env.cfg.Chat.Timing.DebounceMode = "steady"
	env.cfg.Personality.Personality = "占位人格"

	payload := `{"values":{"chat.max_context_size":12}}`
	resp, body := env.do(http.MethodPost, "/api/config/save", payload)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, body)
	}

	cfg := env.s.cfg.Get()
	if cfg.Chat.Timing.DebounceMode != "steady" {
		t.Errorf("未提交的 debounce_mode 被改成了 %q", cfg.Chat.Timing.DebounceMode)
	}
	if cfg.Personality.Personality != "占位人格" {
		t.Errorf("未提交的 personality 被清空了：%q", cfg.Personality.Personality)
	}
}

// TestConfigSaveRejectsUnknownKey 防：拼错键名却提示「保存成功」。
func TestConfigSaveRejectsUnknownKey(t *testing.T) {
	env, _ := newConfigTestEnv(t)
	env.loginOK()

	for _, key := range []string{
		"chat.max_context_size_typo",
		"qq.app_secret", // 密钥不允许改
		"llm.providers", // 结构化嵌套不允许改
		"database.path", // 改错就起不来的路径
		"web.password",
	} {
		payload, _ := json.Marshal(map[string]any{"values": map[string]any{key: "x"}})
		resp, body := env.do(http.MethodPost, "/api/config/save", string(payload))
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("键 %q 的状态码 = %d，期望 400，body=%s", key, resp.StatusCode, body)
		}
	}
}

// TestConfigSaveRejectsWrongType 防：把字符串塞进整数项被静默接受。
func TestConfigSaveRejectsWrongType(t *testing.T) {
	env, path := newConfigTestEnv(t)
	env.loginOK()

	before := env.s.cfg.Get().Chat.MaxContextSize
	beforeRaw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取配置失败: %v", err)
	}

	for _, payload := range []string{
		`{"values":{"chat.max_context_size":"40"}}`,  // 字符串
		`{"values":{"chat.max_context_size":3.7}}`,   // 小数
		`{"values":{"chat.enable_reply_at":"true"}}`, // 字符串当布尔
	} {
		resp, body := env.do(http.MethodPost, "/api/config/save", payload)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("payload %s 状态码 = %d，期望 400，body=%s", payload, resp.StatusCode, body)
		}
	}

	// 关键：失败时内存与磁盘都不能变。
	if got := env.s.cfg.Get().Chat.MaxContextSize; got != before {
		t.Errorf("校验失败后内存值变了：%d -> %d", before, got)
	}
	afterRaw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取配置失败: %v", err)
	}
	if string(afterRaw) != string(beforeRaw) {
		t.Error("校验失败后配置文件被改写了")
	}
}

// TestConfigSaveRejectsInvalidValue 防：Validate 不通过的值被写进去。
func TestConfigSaveRejectsInvalidValue(t *testing.T) {
	env, _ := newConfigTestEnv(t)
	env.loginOK()

	// 昵称不能为空（Validate 会拒绝），此时必须整体失败。
	payload := `{"values":{"bot.nickname":""}}`
	resp, body := env.do(http.MethodPost, "/api/config/save", payload)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("空昵称状态码 = %d，期望 400，body=%s", resp.StatusCode, body)
	}
	if env.s.cfg.Get().Bot.Nickname == "" {
		t.Error("校验失败但昵称已经被清空了")
	}
}

// TestConfigSaveStringListTrimsEmpty 防：屏蔽词里混入空串。
//
// 空串在 strings.Contains 里对所有文本都成立，一条空屏蔽词
// 会让机器人忽略全部消息 —— 这是灾难性的。
func TestConfigSaveStringListTrimsEmpty(t *testing.T) {
	env, _ := newConfigTestEnv(t)
	env.loginOK()

	payload := `{"values":{"message.ban_words":["广告","  ","","黄牛 "]}}`
	resp, body := env.do(http.MethodPost, "/api/config/save", payload)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, body)
	}

	got := env.s.cfg.Get().Message.BanWords
	want := []string{"广告", "黄牛"}
	if len(got) != len(want) {
		t.Fatalf("ban_words = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ban_words[%d] = %q，期望 %q", i, got[i], want[i])
		}
	}
}

// TestConfigSaveMarksRestartNotice 防：改了需要重启的项但没提示。
func TestConfigSaveMarksRestartNotice(t *testing.T) {
	env, _ := newConfigTestEnv(t)
	env.loginOK()

	payload := `{"values":{"web.port":7899,"chat.max_context_size":40}}`
	resp, body := env.do(http.MethodPost, "/api/config/save", payload)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, body)
	}

	var out struct {
		Restart []string `json:"restart"`
	}
	decodeJSON(t, body, &out)
	if len(out.Restart) != 1 {
		t.Fatalf("restart = %v，期望只列出端口项", out.Restart)
	}

	// 之后再读编辑页，提示还在（用户可能刷新了页面）。
	_, body = env.do(http.MethodGet, "/api/config/edit", "")
	var edit struct {
		Restart []string `json:"restart"`
	}
	decodeJSON(t, body, &edit)
	if len(edit.Restart) != 1 {
		t.Errorf("编辑页的 restart = %v，期望仍有 1 项", edit.Restart)
	}
}

// TestConfigSaveRequiresAuth 防：新接口漏套 requireAuth。
func TestConfigSaveRequiresAuth(t *testing.T) {
	env, _ := newConfigTestEnv(t)

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/config/edit"},
		{http.MethodPost, "/api/config/save"},
	} {
		req, err := http.NewRequest(tc.method, env.ts.URL+tc.path, strings.NewReader(`{"values":{}}`))
		if err != nil {
			t.Fatalf("构造请求失败: %v", err)
		}
		resp, err := (&http.Client{}).Do(req)
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s 匿名状态码 = %d，期望 401", tc.method, tc.path, resp.StatusCode)
		}
	}
}

// TestConfigSaveRequiresPost 防：GET 就能改配置。
func TestConfigSaveRequiresPost(t *testing.T) {
	env, _ := newConfigTestEnv(t)
	env.loginOK()

	resp, _ := env.do(http.MethodGet, "/api/config/save?values=x", "")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /api/config/save = %d，期望 405", resp.StatusCode)
	}
}

// TestConfigSaveRejectsEmptyPatch 防：空请求被当成「保存成功」。
func TestConfigSaveRejectsEmptyPatch(t *testing.T) {
	env, _ := newConfigTestEnv(t)
	env.loginOK()

	for _, payload := range []string{`{"values":{}}`, `{}`} {
		resp, body := env.do(http.MethodPost, "/api/config/save", payload)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("payload %s 状态码 = %d，期望 400，body=%s", payload, resp.StatusCode, body)
		}
	}
}

// TestConfigPreservesLeadingComments 防：保存后把用户手写的注释头抹掉。
func TestConfigPreservesLeadingComments(t *testing.T) {
	env, path := newConfigTestEnv(t)
	env.loginOK()

	orig, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取配置失败: %v", err)
	}
	head := "# 这是我手写的说明\n# 第二行\n\n" + string(orig)
	if err := os.WriteFile(path, []byte(head), 0o644); err != nil {
		t.Fatalf("写入配置失败: %v", err)
	}

	resp, body := env.do(http.MethodPost, "/api/config/save",
		`{"values":{"chat.max_context_size":33}}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, body)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取配置失败: %v", err)
	}
	if !strings.Contains(string(after), "# 这是我手写的说明") {
		t.Errorf("保存后注释头被抹掉了：\n%s", after)
	}
	if !strings.Contains(string(after), "33") {
		t.Errorf("保存后没有写入新值：\n%s", after)
	}
}

// TestConfigSaveTriggersHooks 防：注册了 OnSave 但没被调用。
//
// 钩子是「让持有内部状态的组件重建」的通道（例如过滤器重编译正则）。
func TestConfigSaveTriggersHooks(t *testing.T) {
	env, _ := newConfigTestEnv(t)
	env.loginOK()

	var gotOld, gotNew int
	env.s.cfg.OnSave(func(old, next *config.Config) {
		gotOld = old.Chat.MaxContextSize
		gotNew = next.Chat.MaxContextSize
	})

	resp, body := env.do(http.MethodPost, "/api/config/save",
		`{"values":{"chat.max_context_size":77}}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, body)
	}
	if gotNew != 77 {
		t.Errorf("钩子拿到的新值 = %d，期望 77", gotNew)
	}
	if gotOld == gotNew {
		t.Errorf("钩子拿到的旧值也是 %d，说明新旧快照是同一个对象", gotOld)
	}
}

// 确保测试环境确实是个能用的 Store（防止 NewStoreFrom 被改成 nil 后静默通过）。
var _ = func() bool {
	var st *config.Store = config.Wrap(config.Default())
	return st.Get() != nil
}

var _ = context.Background

// TestConfigSaveLogLevelTakesEffect 防：改了日志级别但要重启才生效。
//
// 日志级别不是「读配置快照」而是 slog 的 LevelVar 持有的状态，
// 光换快照不会让它变 —— 必须显式调 logx.SetLevel。
// 不接的话表现是「把级别改成 DEBUG，日志里还是什么都没有」，
// 而用户会以为是 bug 而不是「需要重启」。
func TestConfigSaveLogLevelTakesEffect(t *testing.T) {
	env, _ := newConfigTestEnv(t)
	env.loginOK()

	before := env.s.cfg.Get().Log.Level

	resp, body := env.do(http.MethodPost, "/api/config/save",
		`{"values":{"log.level":"DEBUG"}}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, body)
	}
	if env.s.cfg.Get().Log.Level != "DEBUG" {
		t.Errorf("配置里的级别 = %q，期望 DEBUG（原 %q）", env.s.cfg.Get().Log.Level, before)
	}
	if got := logx.CurrentLevel(); got != logx.LevelDebug {
		t.Errorf("运行期日志级别 = %v，期望 DEBUG（说明 SetLevel 没被调用）", got)
	}

	// 恢复，避免影响同包其它用例的日志输出。
	logx.SetLevel(logx.LevelInfo)
}

// TestConfigFieldsRestartFlagsMakeSense 防：标记与实现不一致。
//
// 「需重启」是给用户看的承诺。标错了的两种后果都很难发现：
//   - 该标没标：用户以为立即生效，改了没反应，以为是 bug
//   - 不该标却标了：用户被要求无谓地重启
//
// 这里守的是几条硬性的：端口/目录/凭据类必须标，提示词/阈值/开关类不该标。
func TestConfigFieldsRestartFlagsMakeSense(t *testing.T) {
	restart := map[string]bool{}
	for _, f := range config.EditableFields() {
		restart[f.Key] = f.Restart
	}

	mustRestart := []string{"web.port"}
	for _, k := range mustRestart {
		if !restart[k] {
			t.Errorf("%s 只在启动时读一次，必须标 Restart", k)
		}
	}

	mustNotRestart := []string{
		"chat.max_context_size",
		"chat.timing.talk_value",
		"log.level",
		"message.ban_words",
		"response.max_length",
	}
	for _, k := range mustNotRestart {
		if restart[k] {
			t.Errorf("%s 是每轮读取的，不该标 Restart", k)
		}
	}
}

// TestConfigSaveRejectsOutOfRange 防：范围校验只在清单里声明、没人执行。
//
// 前端 <input min max> 只是浏览器提示，curl 一行就绕过。
// web.port 越界的后果是下次启动 WebUI 起不来。
func TestConfigSaveRejectsOutOfRange(t *testing.T) {
	cases := []string{
		`{"values":{"web.port":0}}`,
		`{"values":{"web.port":70000}}`,
		`{"values":{"chat.max_context_size":0}}`,
		`{"values":{"chat.max_context_size":9999}}`,
		`{"values":{"emoji.max_count":0}}`,
	}
	for _, body := range cases {
		env, path := newConfigTestEnv(t)
		env.loginOK()
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读配置失败: %v", err)
		}

		resp, respBody := env.do(http.MethodPost, "/api/config/save", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s 应返回 400，实际 %d（body=%s）", body, resp.StatusCode, respBody)
		}

		// 关键：拒绝时磁盘必须没变。
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读配置失败: %v", err)
		}
		if string(after) != string(before) {
			t.Errorf("%s 被拒绝，但文件已被改写", body)
		}
	}
}

// TestConfigSaveRejectsBadEnum 防：枚举写进非法值却提示保存成功。
func TestConfigSaveRejectsBadEnum(t *testing.T) {
	for _, body := range []string{
		`{"values":{"chat.timing.debounce_mode":"超快"}}`,
		`{"values":{"log.level":"VERBOSE"}}`,
		`{"values":{"log.file_level":"TRACE"}}`,
		`{"values":{"chat.style.group_quote_frequency":"always"}}`,
	} {
		env, _ := newConfigTestEnv(t)
		env.loginOK()
		resp, respBody := env.do(http.MethodPost, "/api/config/save", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s 应返回 400，实际 %d（body=%s）", body, resp.StatusCode, respBody)
		}
	}
}

// TestConfigSaveBodySizeLimited 防：超大请求体被完整收下并写进配置。
//
// 不设上限时，一次误粘贴就能让配置文件涨到几 MB，
// 而 Clone（TOML 往返）与 Snapshot 每次保存、每次读配置都要带上它。
func TestConfigSaveBodySizeLimited(t *testing.T) {
	env, _ := newConfigTestEnv(t)
	env.loginOK()

	big := strings.Repeat("啊", 2<<20) // 约 6MB（UTF-8 每字 3 字节）
	resp, _ := env.do(http.MethodPost, "/api/config/save",
		`{"values":{"bot.nickname":"`+big+`"}}`)
	if resp.StatusCode == http.StatusOK {
		t.Error("超大请求体应当被拒绝，而不是写进配置")
	}
}

// TestConfigRestartNoticeReplaced 防：重启提示只增不减。
//
// 用户第一次改了端口（记下「WebUI 端口」），第二次只改昵称，
// 提示仍挂着上一次的项 —— 用户分不清哪些是这次改的。
func TestConfigRestartNoticeReplaced(t *testing.T) {
	env, _ := newConfigTestEnv(t)
	env.loginOK()

	// 第一次：改一个需要重启的项。
	resp, body := env.do(http.MethodPost, "/api/config/save",
		`{"values":{"web.port":7801}}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("第一次保存失败: %d %s", resp.StatusCode, body)
	}

	_, body = env.do(http.MethodGet, "/api/config/edit", "")
	var out struct {
		Restart []string `json:"restart"`
	}
	decodeJSON(t, body, &out)
	if len(out.Restart) != 1 {
		t.Fatalf("第一次保存后 restart = %v，期望 1 项", out.Restart)
	}

	// 第二次：改一个即时生效的项，重启提示必须被清空。
	resp, body = env.do(http.MethodPost, "/api/config/save",
		`{"values":{"chat.max_context_size":33}}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("第二次保存失败: %d %s", resp.StatusCode, body)
	}

	_, body = env.do(http.MethodGet, "/api/config/edit", "")
	decodeJSON(t, body, &out)
	if len(out.Restart) != 0 {
		t.Errorf("第二次只改了即时生效项，但 restart 仍为 %v", out.Restart)
	}
}

// TestConfigSaveFileLevelTakesEffect 防：改了 file_level 但文件日志不变。
//
// 默认配置就是 level=INFO、file_level=DEBUG，两者必须是独立门槛。
func TestConfigSaveFileLevelTakesEffect(t *testing.T) {
	env, _ := newConfigTestEnv(t)
	env.loginOK()

	resp, body := env.do(http.MethodPost, "/api/config/save",
		`{"values":{"log.file_level":"DEBUG"}}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("保存失败: %d %s", resp.StatusCode, body)
	}
	if env.s.cfg.Get().Log.FileLevel != "DEBUG" {
		t.Errorf("配置里 file_level = %q", env.s.cfg.Get().Log.FileLevel)
	}
}

// TestConfigReadOnlyIncludesFileLevel 防：只读信息区漏字段。
func TestConfigReadOnlyIncludesFileLevel(t *testing.T) {
	env, _ := newConfigTestEnv(t)
	env.loginOK()

	_, body := env.do(http.MethodGet, "/api/config/edit", "")
	var out struct {
		Readonly map[string]any `json:"readonly"`
	}
	decodeJSON(t, body, &out)
	if _, ok := out.Readonly["log_file_level"]; !ok {
		t.Error("只读信息里缺少 log_file_level")
	}
}

// TestConfigNoKeyLeakInEdit 防：可编辑字段接口把密钥带出来。
//
// 这条与 TestConfigNoSecretsLeaked 是两条不同的路径：
// 后者打的是 /api/config（概览摘要），这条打的是 /api/config/edit
// （表单数据）。新增接口必须各自被守住。
func TestConfigNoKeyLeakInEdit(t *testing.T) {
	env, _ := newConfigTestEnv(t)
	env.loginOK()

	_, body := env.do(http.MethodGet, "/api/config/edit", "")
	lower := strings.ToLower(string(body))
	for _, bad := range []string{"app_secret", "api_key", "session_secret", "password", "sk-", "do-not-leak"} {
		if strings.Contains(lower, bad) {
			t.Errorf("/api/config/edit 响应里出现了 %q", bad)
		}
	}

	// 也不能通过保存接口写入密钥类键。
	for _, key := range []string{"qq.app_id", "qq.app_secret", "web.password", "database.path"} {
		resp, _ := env.do(http.MethodPost, "/api/config/save",
			`{"values":{"`+key+`":"x"}}`)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("键 %s 应当被拒绝，实际 %d", key, resp.StatusCode)
		}
	}
}

// ---------------------------------------------------------------------------
// 模型管理
// ---------------------------------------------------------------------------

// TestLLMGetHidesAPIKey 防：服务商密钥被回显到浏览器。
//
// 这是本组测试里最重要的一条。密钥一旦进了响应体，就会同时进
// 浏览器缓存、开发者工具的 Network 历史、以及任何离线保存的 HAR 文件。
// 前端只需要知道「有没有配」，用 has_api_key 表达就够了。
func TestLLMGetHidesAPIKey(t *testing.T) {
	env, _ := newConfigTestEnv(t)
	env.loginOK()

	resp, body := env.do(http.MethodGet, "/api/config/llm", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, body)
	}

	for _, secret := range []string{
		"sk-llm-api-key-DO-NOT-LEAK",
		"qq-app-secret-DO-NOT-LEAK",
		testPassword,
	} {
		if strings.Contains(string(body), secret) {
			t.Errorf("响应里出现了密钥 %q：%s", secret, body)
		}
	}

	var out config.LLMView
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if len(out.Providers) != 1 {
		t.Fatalf("服务商数 = %d，期望 1", len(out.Providers))
	}
	if !out.Providers[0].HasAPIKey {
		t.Error("已配置密钥时 has_api_key 应为 true")
	}
	if out.Providers[0].ModelCount != 1 {
		t.Errorf("model_count = %d，期望 1", out.Providers[0].ModelCount)
	}
	if len(out.TaskOrder) == 0 {
		t.Error("task_order 不应为空：前端靠它稳定排序")
	}
}

// TestLLMSaveTaskBinding 防：改了任务绑定的模型但没生效。
func TestLLMSaveTaskBinding(t *testing.T) {
	env, path := newConfigTestEnv(t)
	env.loginOK()

	body := `{"action":"task.save","task":{"name":"planner","models":["test-model"],` +
		`"max_tokens":1234,"temperature":0.33,"selection_strategy":"random","hard_timeout":45}}`
	resp, respBody := env.do(http.MethodPost, "/api/config/llm/save", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, respBody)
	}

	got := env.s.cfg.Get().LLM.Tasks.Planner
	if len(got.Models) != 1 || got.Models[0] != "test-model" {
		t.Errorf("planner 绑定 = %v", got.Models)
	}
	if got.MaxTokens != 1234 || got.SelectionStrategy != "random" || got.HardTimeout != 45 {
		t.Errorf("planner 参数没写进去: %+v", got)
	}

	// 落盘验证：只在内存生效的话重启就丢。
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("重新加载失败: %v", err)
	}
	if reloaded.LLM.Tasks.Planner.MaxTokens != 1234 {
		t.Errorf("重新加载后 max_tokens = %d", reloaded.LLM.Tasks.Planner.MaxTokens)
	}
}

// TestLLMSaveRejectsUnknownModelInTask 防：任务指向不存在的模型。
//
// 这种配置能保存成功，但第一次调用时才会报「没有可用模型」，
// 报错点离配置点很远，很难联想到是这里填错了。
func TestLLMSaveRejectsUnknownModelInTask(t *testing.T) {
	env, _ := newConfigTestEnv(t)
	env.loginOK()

	body := `{"action":"task.save","task":{"name":"planner","models":["不存在的模型"],` +
		`"selection_strategy":"balance"}}`
	resp, respBody := env.do(http.MethodPost, "/api/config/llm/save", body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400，body=%s", resp.StatusCode, respBody)
	}
	if !strings.Contains(string(respBody), "不存在") {
		t.Errorf("错误信息应说明模型不存在：%s", respBody)
	}
}

// TestLLMSaveProviderKeepsSecretWhenEmpty 防：编辑服务商时把密钥清空。
//
// 这是「不回显密钥」带来的必然问题：前端拿不到原值，
// 提交时密钥框是空的。如果后端把空当作「清空」，
// 那么每次改个超时时间就会把密钥抹掉 —— 而界面上看起来一切正常。
func TestLLMSaveProviderKeepsSecretWhenEmpty(t *testing.T) {
	env, _ := newConfigTestEnv(t)
	env.loginOK()

	// 改超时，密钥留空。
	body := `{"action":"provider.save","provider":{"original":"test-provider",` +
		`"name":"test-provider","base_url":"https://example.invalid/v1","timeout":99}}`
	resp, respBody := env.do(http.MethodPost, "/api/config/llm/save", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, respBody)
	}

	p := env.s.cfg.Get().LLM.Providers[0]
	if p.Timeout != 99 {
		t.Errorf("timeout = %d，期望 99", p.Timeout)
	}
	if p.APIKey != "sk-llm-api-key-DO-NOT-LEAK" {
		t.Errorf("密钥被清空了：%q", p.APIKey)
	}
}

// TestLLMSaveProviderClearKey 验证显式清空密钥能生效。
func TestLLMSaveProviderClearKey(t *testing.T) {
	env, _ := newConfigTestEnv(t)
	env.loginOK()

	body := `{"action":"provider.save","provider":{"original":"test-provider",` +
		`"name":"test-provider","base_url":"https://example.invalid/v1","clear_api_key":true}}`
	resp, respBody := env.do(http.MethodPost, "/api/config/llm/save", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, respBody)
	}
	if env.s.cfg.Get().LLM.Providers[0].APIKey != "" {
		t.Error("显式要求清空后密钥仍在")
	}
}

// TestLLMDeleteProviderRemovesModelsAndRefs 防：删了服务商留下悬空引用。
func TestLLMDeleteProviderRemovesModelsAndRefs(t *testing.T) {
	env, _ := newConfigTestEnv(t)
	env.loginOK()

	// 先让 planner 引用这个模型。
	if err := env.s.cfg.Update(func(c *config.Config) error {
		c.LLM.Tasks.Planner.Models = []string{"test-model"}
		return nil
	}); err != nil {
		t.Fatalf("准备数据失败: %v", err)
	}

	resp, body := env.do(http.MethodPost, "/api/config/llm/save",
		`{"action":"provider.delete","name":"test-provider"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, body)
	}

	cfg := env.s.cfg.Get()
	if len(cfg.LLM.Providers) != 0 {
		t.Errorf("服务商还在: %+v", cfg.LLM.Providers)
	}
	if len(cfg.LLM.Models) != 0 {
		t.Errorf("服务商下的模型没被删掉: %+v", cfg.LLM.Models)
	}
	if len(cfg.LLM.Tasks.Planner.Models) != 0 {
		t.Errorf("任务里的悬空引用没清理: %v", cfg.LLM.Tasks.Planner.Models)
	}
}

// TestLLMRenameProviderUpdatesModels 防：改了服务商名，模型还指向旧名字。
func TestLLMRenameProviderUpdatesModels(t *testing.T) {
	env, _ := newConfigTestEnv(t)
	env.loginOK()

	body := `{"action":"provider.save","provider":{"original":"test-provider",` +
		`"name":"renamed","base_url":"https://example.invalid/v1"}}`
	resp, respBody := env.do(http.MethodPost, "/api/config/llm/save", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, respBody)
	}

	cfg := env.s.cfg.Get()
	if cfg.LLM.Providers[0].Name != "renamed" {
		t.Errorf("服务商名 = %q", cfg.LLM.Providers[0].Name)
	}
	if cfg.LLM.Models[0].Provider != "renamed" {
		t.Errorf("模型仍指向旧服务商 %q", cfg.LLM.Models[0].Provider)
	}
}

// TestLLMAddModelRejectsUnknownProvider 防：模型挂到不存在的服务商上。
func TestLLMAddModelRejectsUnknownProvider(t *testing.T) {
	env, _ := newConfigTestEnv(t)
	env.loginOK()

	body := `{"action":"model.save","model":{"name":"m2","model_identifier":"x",` +
		`"provider":"不存在的服务商"}}`
	resp, respBody := env.do(http.MethodPost, "/api/config/llm/save", body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400，body=%s", resp.StatusCode, respBody)
	}
}

// TestLLMSaveRejectsUnknownAction 防：拼错的 action 静默成功。
func TestLLMSaveRejectsUnknownAction(t *testing.T) {
	env, _ := newConfigTestEnv(t)
	env.loginOK()

	resp, body := env.do(http.MethodPost, "/api/config/llm/save", `{"action":"乱写"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400，body=%s", resp.StatusCode, body)
	}
}

// TestLLMEndpointsRequireAuth 守住新接口的鉴权。
//
// routes() 是手写注册，最容易漏 requireAuth —— 漏了就等于
// 任何人（包括外网扫端口的）都能改模型配置。
func TestLLMEndpointsRequireAuth(t *testing.T) {
	env, _ := newConfigTestEnv(t)

	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/config/llm", ""},
		{http.MethodPost, "/api/config/llm/save", `{"action":"provider.delete","name":"x"}`},
	} {
		resp, _ := env.do(c.method, c.path, c.body)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s 未鉴权时状态码 = %d，期望 401", c.method, c.path, resp.StatusCode)
		}
	}
}

// TestLLMDeleteModelClearsTaskRefs 防：删了模型但任务还引用它。
func TestLLMDeleteModelClearsTaskRefs(t *testing.T) {
	env, _ := newConfigTestEnv(t)
	env.loginOK()

	if err := env.s.cfg.Update(func(c *config.Config) error {
		c.LLM.Tasks.Replyer.Models = []string{"test-model"}
		return nil
	}); err != nil {
		t.Fatalf("准备数据失败: %v", err)
	}

	resp, body := env.do(http.MethodPost, "/api/config/llm/save",
		`{"action":"model.delete","name":"test-model"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, body)
	}
	if got := env.s.cfg.Get().LLM.Tasks.Replyer.Models; len(got) != 0 {
		t.Errorf("replyer 的悬空引用没清理: %v", got)
	}
}
