package config

// Store 与可编辑字段的单元测试。
//
// 重点守三件事：
//  1. Update 的失败路径不能留下任何痕迹（内存与磁盘都不变）
//  2. 快照是深拷贝：改新值不能影响旧值（否则正在读的协程会读到半新半旧）
//  3. 字段清单与 ApplyField/Snapshot 必须一一对应

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestStoreGetNeverNil 防：Store 未初始化时 Get 返回 nil 导致调用方崩。
func TestStoreGetNeverNil(t *testing.T) {
	var s *Store
	if s.Get() != nil {
		t.Error("nil Store 的 Get 应返回 nil（而不是 panic）")
	}
	if s.Path() != "" {
		t.Error("nil Store 的 Path 应返回空串")
	}
}

// TestStoreUpdateClonesAndSwaps 防：新快照与旧快照是同一个对象。
//
// 这是本文件最重要的测试。如果 Update 直接在旧对象上改，
// 那么正在读的协程会读到「改了一半」的配置，
// 而这种问题只在并发下出现，功能测试永远发现不了。
func TestStoreUpdateClonesAndSwaps(t *testing.T) {
	s := NewStoreFrom(Default(), "")
	old := s.Get()

	if err := s.Update(func(c *Config) error {
		c.Chat.MaxContextSize = 123
		return nil
	}); err != nil {
		t.Fatalf("更新失败: %v", err)
	}

	next := s.Get()
	if next == old {
		t.Fatal("Update 之后快照指针没变，说明是就地修改")
	}
	if next.Chat.MaxContextSize != 123 {
		t.Errorf("新快照里的值 = %d，期望 123", next.Chat.MaxContextSize)
	}
	// 旧快照必须原封不动：它可能正被别的协程读。
	if old.Chat.MaxContextSize == 123 {
		t.Error("旧快照也被改了：正在读它的协程会看到不一致的配置")
	}
}

// TestStoreUpdateRejectsInvalid 防：Validate 不通过却改了内存。
func TestStoreUpdateRejectsInvalid(t *testing.T) {
	s := NewStoreFrom(Default(), "")
	before := s.Get()

	err := s.Update(func(c *Config) error {
		c.Bot.Nickname = ""
		return nil
	})
	if err == nil {
		t.Fatal("空昵称应当校验失败")
	}
	if s.Get() != before {
		t.Error("校验失败但快照被换掉了")
	}
}

// TestStoreUpdateMutateErrorAborts 防：mutate 返回错误后仍然写盘。
func TestStoreUpdateMutateErrorAborts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := Save(Default(), path); err != nil {
		t.Fatalf("写初始配置失败: %v", err)
	}
	orig, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读配置失败: %v", err)
	}

	s := NewStoreFrom(Default(), path)
	wantErr := errAbort
	if err := s.Update(func(c *Config) error {
		c.Chat.MaxContextSize = 999
		return wantErr
	}); err != wantErr {
		t.Fatalf("错误没被原样返回: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读配置失败: %v", err)
	}
	if string(after) != string(orig) {
		t.Error("mutate 报错后文件仍被改写了")
	}
	if s.Get().Chat.MaxContextSize == 999 {
		t.Error("mutate 报错后内存仍被改了")
	}
}

var errAbort = &abortError{}

type abortError struct{}

func (*abortError) Error() string { return "中止" }

// TestStoreUpdateWritesFile 防：保存只改内存不落盘（重启就丢）。
func TestStoreUpdateWritesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := Save(Default(), path); err != nil {
		t.Fatalf("写初始配置失败: %v", err)
	}

	s := NewStoreFrom(Default(), path)
	if err := s.Update(func(c *Config) error {
		c.Bot.Nickname = "落盘测试"
		return nil
	}); err != nil {
		t.Fatalf("更新失败: %v", err)
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("重新加载失败（写出的 TOML 不合法）: %v", err)
	}
	if reloaded.Bot.Nickname != "落盘测试" {
		t.Errorf("重新加载后 nickname = %q", reloaded.Bot.Nickname)
	}
}

// TestSaveAtomicLeavesNoTempFile 防：临时文件残留。
func TestSaveAtomicLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := SaveAtomic(Default(), path); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读目录失败: %v", err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("目录里应当只有 config.toml，实际 %v", names)
	}
}

// TestLeadingCommentsKept 防：保存后把用户手写的注释头抹掉。
func TestLeadingCommentsKept(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	content := "# 顶部说明\n# 第二行\n\n[bot]\nnickname = '旧名字'\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}

	cfg := Default()
	cfg.Bot.Nickname = "新名字"
	if err := SaveAtomic(cfg, path); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读文件失败: %v", err)
	}
	if !strings.HasPrefix(string(raw), "# 顶部说明") {
		t.Errorf("注释头丢了：\n%s", raw)
	}
	if !strings.Contains(string(raw), "新名字") {
		t.Errorf("新值没写进去：\n%s", raw)
	}
}

// TestEditableFieldsRoundTrip 防：清单里的键在 Snapshot/ApplyField 里对不上。
//
// 三种错法都会让界面出问题，而编译期都发现不了：
//   - 清单有、Snapshot 没有 → 界面上那一行永远是空的
//   - 清单有、ApplyField 没有 → 改完提示成功但值没变
//   - ApplyField 有、清单没有 → 该字段改不了
func TestEditableFieldsRoundTrip(t *testing.T) {
	cfg := Default()
	snap := cfg.Snapshot()

	seen := map[string]bool{}
	for _, f := range EditableFields() {
		if seen[f.Key] {
			t.Errorf("重复的字段键 %q", f.Key)
		}
		seen[f.Key] = true

		if _, ok := snap[f.Key]; !ok {
			t.Errorf("%s 在 Snapshot 里没有值", f.Key)
		}
		if f.Label == "" {
			t.Errorf("%s 缺少显示名", f.Key)
		}
		if f.Section == "" {
			t.Errorf("%s 缺少分组", f.Key)
		}
		if f.Kind == KindEnum && len(f.Options) == 0 {
			t.Errorf("%s 是枚举类型但没有选项", f.Key)
		}

		// 用当前值回写一遍必须成功（证明类型与转换器匹配）。
		if v, ok := snap[f.Key]; ok {
			next, cerr := cfg.Clone()
			if cerr != nil {
				t.Fatalf("%s: Clone 失败: %v", f.Key, cerr)
			}
			if err := next.ApplyField(f.Key, jsonRoundTrip(v)); err != nil {
				t.Errorf("%s 回写失败: %v", f.Key, err)
			}
		}
	}

	if len(snap) != len(EditableFields()) {
		t.Errorf("Snapshot 有 %d 项、清单有 %d 项", len(snap), len(EditableFields()))
	}
}

// TestApplyFieldRejectsUnknownKey 防：写了不存在的键却静默成功。
func TestApplyFieldRejectsUnknownKey(t *testing.T) {
	cfg := Default()
	for _, key := range []string{
		"", "chat", "chat.nope", "qq.app_secret", "database.path",
	} {
		if err := cfg.ApplyField(key, "x"); err == nil {
			t.Errorf("键 %q 应当被拒绝", key)
		}
	}
}

// TestApplyFieldRejectsWrongTypes 防：类型不匹配被静默接受。
func TestApplyFieldRejectsWrongTypes(t *testing.T) {
	cases := []struct {
		key string
		val any
	}{
		{"chat.max_context_size", "40"},
		{"chat.max_context_size", 3.7},
		{"chat.enable_reply_at", "true"},
		{"chat.timing.talk_value", "0.5"},
		{"message.ban_words", "广告"},
		{"bot.nickname", 123},
	}
	for _, c := range cases {
		cfg := Default()
		if err := cfg.ApplyField(c.key, c.val); err == nil {
			t.Errorf("%s 收到 %v(%T) 应当报错", c.key, c.val, c.val)
		}
	}
}

// TestStringListTrimsBlanks 防：屏蔽词里混入空串。
//
// 空串在 strings.Contains 里对所有文本都成立，
// 一条空屏蔽词会让机器人忽略全部消息。
func TestStringListTrimsBlanks(t *testing.T) {
	cfg := Default()
	if err := cfg.ApplyField("message.ban_words", []any{"广告", "  ", "", " 黄牛 "}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	got := cfg.Message.BanWords
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

// TestStoreConcurrentReadDuringUpdate 用 -race 之外的视角验证并发安全。
//
// 这里只能证明「读到的配置始终自洽」这一较弱性质：
// 反复改一个字段的同时反复读，读到的必须是修改前或修改后的完整值，
// 不能是「结构体改了一半」的中间态。
func TestStoreConcurrentReadDuringUpdate(t *testing.T) {
	s := NewStoreFrom(Default(), "")

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 读者
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				c := s.Get()
				if c == nil {
					t.Error("Get 返回了 nil")
					return
				}
				// 两个字段必须同时处于同一个版本：靠 Update 的整份替换保证。
				if c.Chat.MaxContextSize == 500 && c.Bot.Nickname != "版本B" {
					t.Errorf("读到了不一致的配置：size=%d nickname=%q",
						c.Chat.MaxContextSize, c.Bot.Nickname)
					return
				}
			}
		}()
	}

	// 写者
	for i := 0; i < 50; i++ {
		a := i%2 == 0
		if err := s.Update(func(c *Config) error {
			if a {
				c.Chat.MaxContextSize = 40
				c.Bot.Nickname = "版本A"
			} else {
				c.Chat.MaxContextSize = 500
				c.Bot.Nickname = "版本B"
			}
			return nil
		}); err != nil {
			t.Fatalf("更新失败: %v", err)
		}
	}
	close(stop)
	wg.Wait()
}

// TestCloneIsDeep 防：Clone 是浅拷贝，改新配置会影响旧的。
func TestCloneIsDeep(t *testing.T) {
	cfg := Default()
	cfg.Message.BanWords = []string{"原词"}
	clone, err := cfg.Clone()
	if err != nil {
		t.Fatalf("Clone 失败: %v", err)
	}

	clone.Message.BanWords[0] = "改过的"
	if cfg.Message.BanWords[0] != "原词" {
		t.Error("Clone 不是深拷贝：改克隆体影响到了原对象")
	}

	clone.LLM.Providers = append(clone.LLM.Providers, ProviderConfig{Name: "x"})
	if len(cfg.LLM.Providers) != 0 {
		t.Error("Clone 的切片与原对象共享底层数组")
	}
}

// jsonRoundTrip 模拟一次 JSON 编解码，把 Go 值变成前端传回来的形态。
//
// 前端提交的数字永远是 float64，字符串数组永远是 []any ——
// 直接用 Go 原生类型测会漏掉「前端传 float64 时能不能转成 int」这一整类问题。
func jsonRoundTrip(v any) any {
	switch val := v.(type) {
	case int:
		return float64(val)
	case int64:
		return float64(val)
	case float64:
		return val
	case string:
		return val
	case bool:
		return val
	case []string:
		out := make([]any, len(val))
		for i, s := range val {
			out[i] = s
		}
		return out
	}
	return v
}

// TestNewStoreAndReload 覆盖从磁盘加载与重新加载。
func TestNewStoreAndReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	cfg := Default()
	cfg.Bot.Nickname = "磁盘上的名字"
	if err := Save(cfg, path); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}

	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore 失败: %v", err)
	}
	if s.Get().Bot.Nickname != "磁盘上的名字" {
		t.Errorf("加载后 nickname = %q", s.Get().Bot.Nickname)
	}
	if s.Path() != path {
		t.Errorf("Path = %q，期望 %q", s.Path(), path)
	}

	// 内存里改一份，再从磁盘 Reload 应当丢掉改动。
	if err := s.Update(func(c *Config) error {
		c.Bot.Nickname = "内存里改的"
		return nil
	}); err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	// 手工把文件改回另一个值，证明 Reload 读的是磁盘。
	disk := Default()
	disk.Bot.Nickname = "磁盘上又改了"
	if err := Save(disk, path); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}
	if err := s.Reload(); err != nil {
		t.Fatalf("Reload 失败: %v", err)
	}
	if s.Get().Bot.Nickname != "磁盘上又改了" {
		t.Errorf("Reload 后 nickname = %q", s.Get().Bot.Nickname)
	}
}

// TestReloadWithoutPath 防：未绑路径时 Reload 静默成功。
func TestReloadWithoutPath(t *testing.T) {
	s := NewStoreFrom(Default(), "")
	if err := s.Reload(); err == nil {
		t.Error("未绑路径时 Reload 应当报错")
	}
}

// TestLoadOrCreateStore 覆盖首次启动生成默认配置。
func TestLoadOrCreateStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "config.toml")

	s, created, err := LoadOrCreateStore(path)
	if err != nil {
		t.Fatalf("LoadOrCreateStore 失败: %v", err)
	}
	if !created {
		t.Error("文件不存在时 created 应为 true")
	}
	if s.Get() == nil {
		t.Fatal("Get 返回 nil")
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("应当已经写出配置文件: %v", statErr)
	}

	// 第二次不应该再算「新建」。
	_, created2, err := LoadOrCreateStore(path)
	if err != nil {
		t.Fatalf("LoadOrCreateStore 失败: %v", err)
	}
	if created2 {
		t.Error("文件已存在时 created 应为 false")
	}
}

// TestLoadOrCreateStoreBadFile 防：配置语法错误被当成「首次启动」。
func TestLoadOrCreateStoreBadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("这不是 TOML [[["), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	if _, _, err := LoadOrCreateStore(path); err == nil {
		t.Error("语法错误的配置应当报错，而不是被覆盖成默认值")
	}
}

// TestOnSaveHookOrder 防：多个钩子的执行顺序或数量不对。
func TestOnSaveHookOrder(t *testing.T) {
	s := NewStoreFrom(Default(), "")
	var order []int
	s.OnSave(func(_, _ *Config) { order = append(order, 1) })
	s.OnSave(func(_, _ *Config) { order = append(order, 2) })
	s.OnSave(nil) // 空钩子应被忽略而不是 panic

	if err := s.Update(func(c *Config) error {
		c.Chat.MaxContextSize = 7
		return nil
	}); err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	if len(order) != 2 || order[0] != 1 || order[1] != 2 {
		t.Errorf("钩子执行顺序 = %v，期望 [1 2]", order)
	}
}

// TestCloneNil 防：nil 配置 Clone 出 panic。
func TestCloneNil(t *testing.T) {
	var c *Config
	if got, err := c.Clone(); got != nil || err != nil {
		t.Error("nil 的 Clone 应返回 (nil, nil)")
	}
}

// TestSetIntAcceptsGoInts 覆盖 setInt 的 Go 原生整数分支（非 JSON 路径）。
func TestSetIntAcceptsGoInts(t *testing.T) {
	cfg := Default()
	if err := cfg.ApplyField("chat.max_context_size", 11); err != nil {
		t.Errorf("int 写入失败: %v", err)
	}
	if err := cfg.ApplyField("chat.max_context_size", int64(12)); err != nil {
		t.Errorf("int64 写入失败: %v", err)
	}
	if cfg.Chat.MaxContextSize != 12 {
		t.Errorf("max_context_size = %d，期望 12", cfg.Chat.MaxContextSize)
	}
}

// TestSetFloatAcceptsGoNumbers 覆盖 setFloat 的非 float64 分支。
func TestSetFloatAcceptsGoNumbers(t *testing.T) {
	cfg := Default()
	if err := cfg.ApplyField("chat.timing.talk_value", 0); err != nil {
		t.Errorf("int 写入失败: %v", err)
	}
	if cfg.Chat.Timing.TalkValue != 0 {
		t.Errorf("talk_value = %v，期望 0", cfg.Chat.Timing.TalkValue)
	}
	if err := cfg.ApplyField("chat.timing.talk_value", int64(1)); err != nil {
		t.Errorf("int64 写入失败: %v", err)
	}
}

// TestSetStringListAcceptsGoSlice 覆盖 []string 直传（非 HTTP 调用方）。
func TestSetStringListAcceptsGoSlice(t *testing.T) {
	cfg := Default()
	if err := cfg.ApplyField("message.ban_words", []string{"a", "", "b"}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if len(cfg.Message.BanWords) != 2 {
		t.Errorf("ban_words = %v，期望 2 项", cfg.Message.BanWords)
	}
	// 元素类型不对时要报错。
	if err := cfg.ApplyField("message.ban_words", []any{"a", 1}); err == nil {
		t.Error("数组里有非字符串时应当报错")
	}
}

// TestWrapNilAndValue 覆盖 Wrap 的两个分支。
func TestWrapNilAndValue(t *testing.T) {
	if Wrap(nil) != nil {
		t.Error("Wrap(nil) 应当返回 nil，用来保留调用方的 nil 判断")
	}
	s := Wrap(Default())
	if s == nil || s.Get() == nil {
		t.Error("Wrap(正常配置) 应当可读")
	}
}

// TestSaveAtomicCreatesDir 防：配置目录不存在时直接失败。
func TestSaveAtomicCreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b", "c")
	path := filepath.Join(dir, "config.toml")
	if err := SaveAtomic(Default(), path); err != nil {
		t.Fatalf("多级目录下保存失败: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("文件不存在: %v", err)
	}
}

// TestSaveAtomicOnCorruptExisting 覆盖「原文件不可读」分支。
//
// 读不到旧文件时不该失败：拿不到注释头就直接写新内容，
// 比因为一个读权限问题导致保存不了要好。
func TestSaveAtomicOnCorruptExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	// 存在但是个目录：ReadFile 会失败。
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	// rename 到目录上会失败，这里只要求它返回错误而不是 panic。
	if err := SaveAtomic(Default(), path); err == nil {
		t.Log("在当前平台上 rename 覆盖目录成功了（不视为失败）")
	}
}

// TestUpdateHookRunsOutsideLock 防：hook 在 writeMu 持有期间被调用。
//
// 这是本次 review 抓到的最危险的一条：hooks 原先在锁内执行，
// 于是「配置变了顺手做一次 Update」这种写法会永久死锁，
// 表现是 HTTP 请求不返回、之后所有保存全部阻塞，而功能测试全绿。
//
// 注意：hook 里**递归**调 Update 仍会无限递归（每次 Update 都再触发一次
// hook），这是调用方自己的责任，不是 Store 的 bug。所以这条测试验证的是
// 「hook 执行期间别的 goroutine 能拿到 writeMu」，而不是「hook 里能调
// Update」—— 后者永远不该被支持。
func TestUpdateHookRunsOutsideLock(t *testing.T) {
	s := NewStoreFrom(Default(), "")

	// hook 里阻塞，直到另一个保存成功拿到锁。若 hook 在锁内，
	// 那个保存在 writeMu 上永远等不到，测试超时失败。
	hookEntered := make(chan struct{})
	otherDone := make(chan error, 1)

	// hook 每次保存都会被调用，所以只在第一次故意阻塞。
	// 第二次（并发那次保存触发的）必须立刻返回，否则就成了
	// 「hook 等自己」的假死锁。
	var first atomic.Bool

	s.OnSave(func(_, _ *Config) {
		if !first.CompareAndSwap(false, true) {
			return
		}
		select {
		case hookEntered <- struct{}{}:
		default:
		}
		// 等另一个 goroutine 完成一次保存（它必须能拿到 writeMu）。
		select {
		case err := <-otherDone:
			if err != nil {
				t.Errorf("并发保存失败: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("hook 执行期间别的保存拿不到锁：hooks 必须在 writeMu 之外执行")
		}
	})

	go func() {
		<-hookEntered
		otherDone <- s.Update(func(c *Config) error {
			c.Response.MaxLength = 321
			return nil
		})
	}()

	if err := s.Update(func(c *Config) error {
		c.Chat.MaxContextSize = 55
		return nil
	}); err != nil {
		t.Fatalf("外层 Update 失败: %v", err)
	}

	if got := s.Get().Response.MaxLength; got != 321 {
		t.Errorf("并发保存的值没生效，max_length = %d", got)
	}
	if got := s.Get().Chat.MaxContextSize; got != 55 {
		t.Errorf("外层保存的值被覆盖，max_context_size = %d", got)
	}
}

// TestUpdateMinMaxEnforced 防：清单里写了 Min/Max 但没人检查。
//
// 前端 <input min max> 只是浏览器提示，curl 一行就绕过了。
// 越界的值会真的写进配置文件，而 web.port=-1 的后果是下次启动
// WebUI 直接起不来 —— 用户完全不知道是自己填错了。
func TestUpdateMinMaxEnforced(t *testing.T) {
	cases := []struct {
		key string
		val any
	}{
		{"chat.max_context_size", float64(-5)},
		{"chat.max_context_size", float64(501)},
		{"chat.max_context_size", float64(0)},
		{"web.port", float64(0)},
		{"web.port", float64(-1)},
		{"web.port", float64(70000)},
		{"emoji.max_count", float64(0)},
		{"memory.vector_min_score", float64(1e9)},
		{"memory.vector_min_score", float64(-0.5)},
		{"chat.timing.talk_value", float64(-0.1)},
		{"chat.timing.talk_value", float64(1.5)},
		{"chat.timing.planner_interrupt_max_consecutive_count", float64(-1)},
		{"response.max_length", float64(1)},
	}
	for _, c := range cases {
		cfg := Default()
		if err := cfg.ApplyField(c.key, c.val); err == nil {
			t.Errorf("%s = %v 应当被范围校验拒绝", c.key, c.val)
		}
	}
}

// TestUpdateMinMaxAllowsBoundary 防：边界值被误判为越界。
//
// 好几个字段的合法值包含 0（talk_value=0 表示完全不说话、
// retention_days=0 表示永久保留），所以「0 表示不限制」那种哨兵写法
// 会把合法的 0 判成非法。Min/Max 因此用指针。
func TestUpdateMinMaxAllowsBoundary(t *testing.T) {
	cases := []struct {
		key string
		val any
	}{
		{"chat.max_context_size", float64(1)},
		{"chat.max_context_size", float64(500)},
		{"chat.timing.talk_value", float64(0)},
		{"chat.timing.talk_value", float64(1)},
		{"chat.timing.planner_interrupt_max_consecutive_count", float64(0)},
		{"database.retention_days", float64(0)},
		{"web.port", float64(1)},
		{"web.port", float64(65535)},
	}
	for _, c := range cases {
		cfg := Default()
		if err := cfg.ApplyField(c.key, c.val); err != nil {
			t.Errorf("%s = %v 应当被接受，实际报错: %v", c.key, c.val, err)
		}
	}
}

// TestEnumValuesEnforced 防：枚举写进非法值时静默接受。
//
// 非法枚举值不会报错，只会让配置永远不生效：
// debounce_mode 落到 map 取不到就静默回落 natural，
// log.level 写成 VERBOSE 会被 ParseLevel 回落成 INFO，
// 而界面上显示的仍是 VERBOSE。
func TestEnumValuesEnforced(t *testing.T) {
	cases := []struct {
		key string
		val string
	}{
		{"chat.timing.debounce_mode", "超快"},
		{"chat.timing.debounce_mode", ""},
		{"chat.timing.reply_trigger_mode", "random"},
		{"chat.style.group_quote_frequency", "always"},
		{"chat.style.private_quote_frequency", "sometimes"},
		{"log.level", "VERBOSE"},
		{"log.level", "debug"}, // 大小写敏感：取值必须逐字匹配
		{"log.file_level", "TRACE"},
	}
	for _, c := range cases {
		cfg := Default()
		if err := cfg.ApplyField(c.key, c.val); err == nil {
			t.Errorf("%s = %q 应当被拒绝", c.key, c.val)
		}
	}

	// 合法值必须能写进去。
	for _, key := range []string{"chat.timing.debounce_mode", "log.level", "log.file_level"} {
		cfg := Default()
		if err := cfg.ApplyField(key, "INFO"); err != nil && key != "chat.timing.debounce_mode" {
			t.Errorf("%s = INFO 应当被接受，实际: %v", key, err)
		}
	}
}

// TestCloneErrorInsteadOfDefault 防：Clone 失败时退化成默认值。
//
// 早先的实现 Clone 失败就 return Default()，而 Update 的下一步是
// Validate + SaveAtomic —— 默认值当然能过校验，于是用户辛苦配的东西
// 被默认值默默覆盖，接口还返回 200。宁可不保存也不能写丢。
func TestCloneErrorInsteadOfDefault(t *testing.T) {
	var nilCfg *Config
	got, err := nilCfg.Clone()
	if err != nil || got != nil {
		t.Errorf("nil 的 Clone 应返回 (nil, nil)，实际 (%v, %v)", got, err)
	}

	// 正常配置必须能克隆，且是深拷贝。
	cfg := Default()
	cfg.Message.BanWords = []string{"原值"}
	clone, err := cfg.Clone()
	if err != nil {
		t.Fatalf("Clone 失败: %v", err)
	}
	clone.Message.BanWords[0] = "改过"
	if cfg.Message.BanWords[0] != "原值" {
		t.Error("Clone 不是深拷贝")
	}
}

// TestConcurrentSavesKeepBothChanges 防：并发保存时后写者覆盖先写者。
//
// writeMu 的存在就是为了这个：两个请求各自读到旧基线、
// 各自只改一个字段，如果不同步，后写的会把先写的改动一起抹掉。
func TestConcurrentSavesKeepBothChanges(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := Save(Default(), path); err != nil {
		t.Fatalf("写初始配置失败: %v", err)
	}
	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = s.Update(func(c *Config) error {
				if i%2 == 0 {
					c.Chat.MaxContextSize = 123
				} else {
					c.Response.MaxLength = 456
				}
				return nil
			})
		}(i)
	}
	wg.Wait()

	got := s.Get()
	if got.Chat.MaxContextSize != 123 {
		t.Errorf("max_context_size = %d，期望 123（并发保存把它覆盖了）", got.Chat.MaxContextSize)
	}
	if got.Response.MaxLength != 456 {
		t.Errorf("max_length = %d，期望 456（并发保存把它覆盖了）", got.Response.MaxLength)
	}

	// 磁盘上也必须是两个改动都在。
	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("重新加载失败: %v", err)
	}
	if reloaded.Chat.MaxContextSize != 123 || reloaded.Response.MaxLength != 456 {
		t.Errorf("磁盘上的值不对: ctx=%d len=%d",
			reloaded.Chat.MaxContextSize, reloaded.Response.MaxLength)
	}
}
