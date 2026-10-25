package logx

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestConsoleWriterWriteReturnsInputLen 回归：着色时 Write 必须返回 len(p)。
//
// 曾经返回实际写出的字节数（含 ANSI 转义，比入参长），
// io.MultiWriter 因此判定 ErrShortWrite 并中止，
// 排在 consoleWriter 之后的文件 writer 一条都收不到，
// 现象是 logs/oh-my-qqbot.log 恒为 0 字节。
func TestConsoleWriterWriteReturnsInputLen(t *testing.T) {
	w := &consoleWriter{color: false}
	p := []byte("2026-10-21 20:00:00.123 INFO  [app] hi\n")

	// 无着色路径校验。
	stdout := os.Stdout
	r, wpipe, err := os.Pipe()
	if err != nil {
		t.Fatalf("创建管道失败: %v", err)
	}
	os.Stdout = wpipe
	n, werr := w.Write(p)
	os.Stdout = stdout
	_ = wpipe.Close()
	_, _ = io.Copy(io.Discard, r)
	_ = r.Close()

	if werr != nil {
		t.Fatalf("Write 返回错误: %v", werr)
	}
	if n != len(p) {
		t.Errorf("Write 返回 %d，期望 %d", n, len(p))
	}

	// 着色路径校验：这是真正触发 bug 的分支。
	cw := &consoleWriter{color: true}
	r2, wpipe2, err := os.Pipe()
	if err != nil {
		t.Fatalf("创建管道失败: %v", err)
	}
	os.Stdout = wpipe2
	n2, werr2 := cw.Write(p)
	os.Stdout = stdout
	_ = wpipe2.Close()
	_, _ = io.Copy(io.Discard, r2)
	_ = r2.Close()

	if werr2 != nil {
		t.Fatalf("着色 Write 返回错误: %v", werr2)
	}
	if n2 != len(p) {
		t.Errorf("着色 Write 返回 %d，期望 %d（否则 MultiWriter 会 ErrShortWrite）", n2, len(p))
	}
}

// TestTextHandlerWritesFile 验证 handler 能真的把日志写进文件。
func TestTextHandlerWritesFile(t *testing.T) {
	dir := t.TempDir()
	rf, err := newRotatingFile(filepath.Join(dir, "oh-my-qqbot.log"), 16, 3, nil)
	if err != nil {
		t.Fatalf("创建文件 writer 失败: %v", err)
	}
	defer func() { _ = rf.Close() }()

	lv := &slog.LevelVar{}
	lv.Set(slog.LevelDebug)
	h := newTextHandler(rf, lv)

	rec := slog.NewRecord(time.Now(), slog.LevelInfo, "文件里应该有这行", 0)
	if err := h.Handle(nil, rec); err != nil { //nolint:staticcheck // context 未使用
		t.Fatalf("Handle 失败: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "oh-my-qqbot.log"))
	if err != nil {
		t.Fatalf("读日志文件失败: %v", err)
	}
	if !strings.Contains(string(raw), "文件里应该有这行") {
		t.Errorf("日志文件内容 = %q，应含写入的消息", string(raw))
	}
}

// TestMultiWriterConsoleThenFile 复现原始组合：控制台在前、文件在后。
func TestMultiWriterConsoleThenFile(t *testing.T) {
	var fileBuf bytes.Buffer
	mw := io.MultiWriter(&consoleWriter{color: true}, &fileBuf)

	p := []byte("2026-10-21 20:00:00.123 INFO  [app] 组合写入\n")
	n, err := mw.Write(p)
	if err != nil {
		t.Fatalf("MultiWriter.Write 失败: %v", err)
	}
	if n != len(p) {
		t.Errorf("MultiWriter 返回 %d，期望 %d", n, len(p))
	}
	if fileBuf.Len() == 0 {
		t.Error("文件 writer 收不到任何字节：着色 writer 的返回值破坏了 MultiWriter")
	}
}

// TestDualHandlerSeparatesLevels 防：file_level 与 level 被合并成一个门槛。
//
// 默认配置是 level=INFO、file_level=DEBUG，语义很明确：
// 终端只看到 INFO 以上，文件里多出 DEBUG。合成一个门槛后
// 要么终端被刷成 DEBUG（早先的实现就是这样），要么文件丢掉 DEBUG。
func TestDualHandlerSeparatesLevels(t *testing.T) {
	consoleLv := &slog.LevelVar{}
	consoleLv.Set(slog.LevelInfo)
	fileLv := &slog.LevelVar{}
	fileLv.Set(slog.LevelDebug)

	var console, file bytes.Buffer
	h := newDualHandler(&console, &file, consoleLv, fileLv)
	log := slog.New(h)

	log.Debug("只有文件能看到")
	log.Info("两边都能看到")

	if got := console.String(); strings.Contains(got, "只有文件能看到") {
		t.Errorf("终端不该出现 DEBUG 日志：\n%s", got)
	}
	if !strings.Contains(console.String(), "两边都能看到") {
		t.Errorf("终端缺少 INFO 日志：\n%s", console.String())
	}
	if !strings.Contains(file.String(), "只有文件能看到") {
		t.Errorf("文件缺少 DEBUG 日志（file_level=DEBUG 没生效）：\n%s", file.String())
	}
	if !strings.Contains(file.String(), "两边都能看到") {
		t.Errorf("文件缺少 INFO 日志：\n%s", file.String())
	}
}

// TestDualHandlerEnabledIsUnion 防：Enabled 收窄成单侧级别。
//
// 只要任一出入口需要这条记录就必须放行。收窄成 console 级别会让
// 「终端 INFO、文件 DEBUG」变成「文件也只有 INFO」。
func TestDualHandlerEnabledIsUnion(t *testing.T) {
	consoleLv := &slog.LevelVar{}
	consoleLv.Set(slog.LevelError)
	fileLv := &slog.LevelVar{}
	fileLv.Set(slog.LevelDebug)

	h := newDualHandler(io.Discard, io.Discard, consoleLv, fileLv)
	if !h.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("只要文件需要，Enabled 就必须放行 DEBUG")
	}

	// 反过来：文件门槛更严、终端更松。
	h2 := newDualHandler(io.Discard, io.Discard, consoleLvAt(slog.LevelDebug), consoleLvAt(slog.LevelError))
	if !h2.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("只要终端需要，Enabled 就必须放行 DEBUG")
	}

	// 两边都要 ERROR 以上时，DEBUG 必须被拒。
	if h2.Enabled(context.Background(), slog.LevelDebug) && !h2.Enabled(context.Background(), slog.LevelError) {
		t.Error("ERROR 一定应该放行")
	}
}

// TestDualHandlerNoConsoleNoFile 防：两个出口都空时 panic。
func TestDualHandlerNoConsoleNoFile(t *testing.T) {
	h := newDualHandler(nil, nil, consoleLvAt(slog.LevelDebug), consoleLvAt(slog.LevelDebug))
	if h.Enabled(context.Background(), slog.LevelError) {
		t.Error("没有任何出口时不该 Enabled")
	}
	slog.New(h).Info("不该崩") // 不应 panic
}

func consoleLvAt(l slog.Level) *slog.LevelVar {
	lv := &slog.LevelVar{}
	lv.Set(l)
	return lv
}

// TestParseLevel 防：配置里的级别字符串解析错。
//
// 这个函数的默认分支返回 Info，所以「写错了」不会报错 ——
// 只会让用户以为设了 DEBUG 却没看到 DEBUG 日志。
func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"DEBUG":    slog.LevelDebug,
		"debug":    slog.LevelDebug,
		" Info ":   slog.LevelInfo,
		"WARN":     slog.LevelWarn,
		"WARNING":  slog.LevelWarn,
		"ERROR":    slog.LevelError,
		"CRITICAL": slog.LevelError,
		"":         slog.LevelInfo,
		"VERBOSE":  slog.LevelInfo,
	}
	for in, want := range cases {
		if got := ParseLevel(in); got != want {
			t.Errorf("ParseLevel(%q) = %v，期望 %v", in, got, want)
		}
	}
}

// TestSetLevelBeforeInit 防：Init 之前改级别丢失。
//
// 测试环境与「WebUI 比业务先就绪」都会走到这条路径。
// 静默丢失的表现是「界面说保存成功、日志里还是老样子」。
func TestSetLevelBeforeInit(t *testing.T) {
	mu.Lock()
	savedVar, savedPending := levelVar, pendingLevel
	savedFileVar, savedFilePending := fileLevelVar, pendingFileLevel
	levelVar, pendingLevel = nil, nil
	fileLevelVar, pendingFileLevel = nil, nil
	mu.Unlock()

	t.Cleanup(func() {
		mu.Lock()
		levelVar, pendingLevel = savedVar, savedPending
		fileLevelVar, pendingFileLevel = savedFileVar, savedFilePending
		mu.Unlock()
	})

	if SetLevel(slog.LevelError) {
		t.Error("未 Init 时 SetLevel 应返回 false（调用方需要知道没生效）")
	}
	if got := CurrentLevel(); got != slog.LevelError {
		t.Errorf("未 Init 时 CurrentLevel = %v，期望暂存值 ERROR", got)
	}

	if SetFileLevel(slog.LevelDebug) {
		t.Error("未 Init 时 SetFileLevel 应返回 false")
	}
	if got := CurrentFileLevel(); got != slog.LevelDebug {
		t.Errorf("未 Init 时 CurrentFileLevel = %v，期望暂存值 DEBUG", got)
	}
}

// TestSetLevelAfterInit 覆盖 Init 之后的正常路径。
func TestSetLevelAfterInit(t *testing.T) {
	mu.Lock()
	savedVar, savedPending := levelVar, pendingLevel
	savedFileVar, savedFilePending := fileLevelVar, pendingFileLevel
	mu.Unlock()

	lv := &slog.LevelVar{}
	lv.Set(slog.LevelInfo)
	flv := &slog.LevelVar{}
	flv.Set(slog.LevelInfo)
	mu.Lock()
	levelVar, pendingLevel = lv, nil
	fileLevelVar, pendingFileLevel = flv, nil
	mu.Unlock()

	t.Cleanup(func() {
		mu.Lock()
		levelVar, pendingLevel = savedVar, savedPending
		fileLevelVar, pendingFileLevel = savedFileVar, savedFilePending
		mu.Unlock()
	})

	if !SetLevel(slog.LevelDebug) {
		t.Error("Init 之后 SetLevel 应返回 true")
	}
	if got := CurrentLevel(); got != slog.LevelDebug {
		t.Errorf("CurrentLevel = %v，期望 DEBUG", got)
	}

	if !SetFileLevel(slog.LevelError) {
		t.Error("Init 之后 SetFileLevel 应返回 true")
	}
	if got := CurrentFileLevel(); got != slog.LevelError {
		t.Errorf("CurrentFileLevel = %v，期望 ERROR", got)
	}

	// 两个门槛必须互不影响。
	if got := CurrentLevel(); got != slog.LevelDebug {
		t.Errorf("改文件级别把终端级别也带偏了：%v", got)
	}
}

// TestWithAttrsAndGroup 覆盖 slog.Handler 的两个可选方法。
//
// WithGroup 恢复了 slog 的分组语义：组名作为键前缀（`grp.k=v`），
// 不再挪用为 logger 名（`[...]` 列由 `logger` 属性表达，见
// TestLoggerNameFromGetNotDropped）。
func TestWithAttrsAndGroup(t *testing.T) {
	var buf bytes.Buffer
	lv := &slog.LevelVar{}
	lv.Set(slog.LevelDebug)
	h := newDualHandler(&buf, nil, lv, lv)

	log := slog.New(h).With("k", "v").WithGroup("grp")
	log.Info("带字段的消息")

	out := buf.String()
	if !strings.Contains(out, "grp.k=v") {
		t.Errorf("WithGroup 的组前缀丢了：\n%s", out)
	}
	if strings.Contains(out, "[grp]") {
		t.Errorf("组名不该占用 logger 名列：\n%s", out)
	}
}

// TestFileLevelIndependentOfConsole 是 file_level 的端到端验证。
//
// 默认配置是 level=INFO、file_level=DEBUG。这条测试直接落盘验证：
// 文件里必须有 DEBUG，终端（这里用 buffer 代替）必须没有。
func TestFileLevelIndependentOfConsole(t *testing.T) {
	var console, file bytes.Buffer
	consoleLv := consoleLvAt(slog.LevelInfo)
	fileLv := consoleLvAt(slog.LevelDebug)

	log := slog.New(newDualHandler(&console, &file, consoleLv, fileLv))
	log.Debug("debug-only")
	log.Warn("both")

	if strings.Contains(console.String(), "debug-only") {
		t.Error("终端出现了 DEBUG（file_level 把终端级别带偏了）")
	}
	if !strings.Contains(file.String(), "debug-only") {
		t.Error("文件缺少 DEBUG（file_level 没生效）")
	}
	if !strings.Contains(console.String(), "both") || !strings.Contains(file.String(), "both") {
		t.Error("WARN 应该两边都有")
	}
}

// TestSetFileLevelDoesNotAffectConsole 防：两个级别被写到同一个变量上。
func TestSetFileLevelDoesNotAffectConsole(t *testing.T) {
	mu.Lock()
	savedVar, savedPending := levelVar, pendingLevel
	savedFileVar, savedFilePending := fileLevelVar, pendingFileLevel
	mu.Unlock()

	lv := consoleLvAt(slog.LevelInfo)
	flv := consoleLvAt(slog.LevelInfo)
	mu.Lock()
	levelVar, pendingLevel = lv, nil
	fileLevelVar, pendingFileLevel = flv, nil
	mu.Unlock()

	t.Cleanup(func() {
		mu.Lock()
		levelVar, pendingLevel = savedVar, savedPending
		fileLevelVar, pendingFileLevel = savedFileVar, savedFilePending
		mu.Unlock()
	})

	SetFileLevel(slog.LevelDebug)
	if CurrentLevel() != slog.LevelInfo {
		t.Errorf("改文件级别影响了终端级别：%v", CurrentLevel())
	}
	if CurrentFileLevel() != slog.LevelDebug {
		t.Errorf("文件级别 = %v，期望 DEBUG", CurrentFileLevel())
	}
}

// captureStdout 在 fn 执行期间捕获 os.Stdout 的输出。
//
// consoleWriter.Write 在调用时读包级变量 os.Stdout，所以替换它可以截流。
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("创建管道失败: %v", err)
	}
	old := os.Stdout
	os.Stdout = w
	fn()
	os.Stdout = old
	_ = w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	_ = r.Close()
	return buf.String()
}

// withFreshInit 直接调 initLocked（绕过 initOnce），并保存/恢复全部全局。
//
// preInit 在全局清零之后、initLocked 之前执行，用来布置
// 「Init 之前 SetLevel 暂存」这类前置状态。
func withFreshInit(t *testing.T, opts Options, preInit func()) {
	t.Helper()
	mu.Lock()
	savedRoot, savedFile := rootLogger, logFile
	savedLv, savedFlv := levelVar, fileLevelVar
	savedPL, savedPFL := pendingLevel, pendingFileLevel
	rootLogger, logFile = nil, nil
	levelVar, fileLevelVar = nil, nil
	pendingLevel, pendingFileLevel = nil, nil
	mu.Unlock()

	savedDefault := slog.Default()

	if preInit != nil {
		preInit()
	}
	if err := initLocked(opts); err != nil {
		t.Fatalf("initLocked 失败: %v", err)
	}

	t.Cleanup(func() {
		Close()
		mu.Lock()
		rootLogger, logFile = savedRoot, savedFile
		levelVar, fileLevelVar = savedLv, savedFlv
		pendingLevel, pendingFileLevel = savedPL, savedPFL
		mu.Unlock()
		if savedFile != nil {
			_ = savedFile.Close()
		}
		slog.SetDefault(savedDefault)
	})
}

// TestFileLevelInfoExplicitIsRespected 回归 [P1-2]：显式配 file_level=INFO 必须生效。
//
// slog.LevelInfo 恰好等于 0，旧实现用 `FileLevel != 0` 判断「是否配置过」，
// 于是显式的 INFO 被当成「未配置」、文件门槛退回终端门槛：
// level=DEBUG + file_level=INFO 时文件里出现 DEBUG（实测），重启后静默失效。
func TestFileLevelInfoExplicitIsRespected(t *testing.T) {
	dir := t.TempDir()
	withFreshInit(t, Options{
		Level:        slog.LevelDebug,
		FileLevel:    slog.LevelInfo,
		FileLevelSet: true,
		Dir:          dir,
		Console:      false,
	}, nil)

	Debug("DEBUG-SHOULD-NOT-BE-IN-FILE")
	Info("INFO-SHOULD-BE-IN-FILE")

	raw, err := os.ReadFile(filepath.Join(dir, "oh-my-qqbot.log"))
	if err != nil {
		t.Fatalf("读日志文件失败: %v", err)
	}
	if strings.Contains(string(raw), "DEBUG-SHOULD-NOT-BE-IN-FILE") {
		t.Errorf("显式 file_level=INFO 没生效，文件里出现了 DEBUG:\n%s", raw)
	}
	if !strings.Contains(string(raw), "INFO-SHOULD-BE-IN-FILE") {
		t.Errorf("文件缺少 INFO 日志:\n%s", raw)
	}
}

// TestFileLevelUnsetFollowsConsoleLevel 验证未配置时的既有语义：文件门槛跟随 level。
func TestFileLevelUnsetFollowsConsoleLevel(t *testing.T) {
	dir := t.TempDir()
	withFreshInit(t, Options{
		Level:   slog.LevelDebug,
		Dir:     dir,
		Console: false,
		// FileLevelSet 缺省 = 未配置，文件应跟随 DEBUG。
	}, nil)

	Debug("FOLLOWS-LEVEL")
	raw, err := os.ReadFile(filepath.Join(dir, "oh-my-qqbot.log"))
	if err != nil {
		t.Fatalf("读日志文件失败: %v", err)
	}
	if !strings.Contains(string(raw), "FOLLOWS-LEVEL") {
		t.Errorf("未配置 file_level 时文件应跟随 level=DEBUG:\n%s", raw)
	}
}

// TestCloseThenLogDoesNotRecreateFile 回归 [P2-4] 的完整路径：
// Init → Info → Close → 删文件 → Info，文件不许被重新创建。
func TestCloseThenLogDoesNotRecreateFile(t *testing.T) {
	dir := t.TempDir()
	withFreshInit(t, Options{Level: slog.LevelInfo, Dir: dir, Console: false}, nil)

	Info("before-close")
	Close()
	path := filepath.Join(dir, "oh-my-qqbot.log")
	if err := os.Remove(path); err != nil {
		t.Fatalf("删除日志文件失败: %v", err)
	}

	Info("after-close")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("Close 之后日志文件被重新创建了: %v", err)
	}
}

// TestPendingLevelAppliedByInit 覆盖 [P2-6] 保护的语义链：
// Init 之前 SetLevel 的暂存值必须在 initLocked 里被采用。
//
// P2-6 修的是「SetDefault 已生效、levelVar 还没登记」的并发窗口 ——
// 窗口里的 SetLevel 只能暂存进 pending，而 pending 已被读过，值会丢失。
// 这里验证暂存链路端到端可用（顺序版）。
func TestPendingLevelAppliedByInit(t *testing.T) {
	withFreshInit(t, Options{Level: slog.LevelInfo, Console: false}, func() {
		if SetLevel(slog.LevelError) {
			t.Error("未 Init 时 SetLevel 应返回 false")
		}
	})
	if got := CurrentLevel(); got != slog.LevelError {
		t.Errorf("暂存级别没被 initLocked 采用: %v", got)
	}
	// Init 完成后 SetLevel 必须立刻可用（levelVar 已登记）。
	if !SetLevel(slog.LevelDebug) {
		t.Error("Init 之后 SetLevel 应返回 true")
	}
	if got := CurrentLevel(); got != slog.LevelDebug {
		t.Errorf("CurrentLevel = %v，期望 DEBUG", got)
	}
}

// withRootLogger 把 rootLogger 换成指定 handler 的 logger，测完恢复。
func withRootLogger(t *testing.T, h *textHandler) {
	t.Helper()
	mu.Lock()
	saved := rootLogger
	rootLogger = slog.New(h)
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		rootLogger = saved
		mu.Unlock()
	})
}

// TestLoggerNameFromGetNotDropped 回归 [P2-1]：Get 的名字必须出现在 `[...]` 列。
//
// 旧实现把 With("logger", name) 落进普通字段，`[...]` 列永远是 [app]：
// 每行都是 `INFO [app] 网关已就绪 logger=qq.gateway`，
// 终端与 WebUI 都看不出这条日志来自哪个子系统。
func TestLoggerNameFromGetNotDropped(t *testing.T) {
	var buf bytes.Buffer
	lv := consoleLvAt(slog.LevelDebug)
	withRootLogger(t, newDualHandler(&buf, nil, lv, lv))

	Get("qq.gateway").Info("网关已就绪")

	out := buf.String()
	if !strings.Contains(out, "[qq.gateway] 网关已就绪") {
		t.Errorf("行里缺少真实子系统名：\n%s", out)
	}
	if strings.Contains(out, "logger=qq.gateway") {
		t.Errorf("名字不该重复出现在字段里：\n%s", out)
	}
}

// TestNamedDerivesWithoutDuplicating 回归 [P2-1] 连带问题：Named 不叠两个 logger=。
//
// 旧实现里 Named 在已有名字上再 With 一层，出现
// `logger=agent.loop logger=agent.loop.sub` 两个重复字段。
func TestNamedDerivesWithoutDuplicating(t *testing.T) {
	var buf bytes.Buffer
	lv := consoleLvAt(slog.LevelDebug)
	withRootLogger(t, newDualHandler(&buf, nil, lv, lv))

	Get("agent.loop").Named("sub").Warn("派生名字")

	out := buf.String()
	if !strings.Contains(out, "[agent.loop.sub] 派生名字") {
		t.Errorf("Named 的派生名没进 `[...]` 列：\n%s", out)
	}
	if got := strings.Count(out, "logger="); got != 0 {
		t.Errorf("出现了 %d 个 logger= 字段，期望 0：\n%s", got, out)
	}
}

// TestGetWithKeepsFieldsAndName 验证 With 的字段照常输出、logger 名不丢。
func TestGetWithKeepsFieldsAndName(t *testing.T) {
	var buf bytes.Buffer
	lv := consoleLvAt(slog.LevelDebug)
	withRootLogger(t, newDualHandler(&buf, nil, lv, lv))

	Get("emoji").With("hash", "abc123").Info("收藏表情")

	out := buf.String()
	if !strings.Contains(out, "[emoji] 收藏表情 hash=abc123") {
		t.Errorf("名字或 With 字段丢失：\n%s", out)
	}
}

// TestLineTimestampHasYearAndMillis 回归 [P2-5]：行时间戳含年份与毫秒。
//
// 旧格式 `01-02 15:04:05` 跨年日志无法区分，且与归档名里的年份对不上。
func TestLineTimestampHasYearAndMillis(t *testing.T) {
	var buf bytes.Buffer
	lv := consoleLvAt(slog.LevelDebug)
	h := newDualHandler(&buf, nil, lv, lv)

	rec := slog.NewRecord(time.Date(2026, 10, 25, 13, 21, 45, 123456789, time.Local), slog.LevelInfo, "时间戳行", 0)
	if err := h.Handle(nil, rec); err != nil { //nolint:staticcheck // context 未使用
		t.Fatalf("Handle 失败: %v", err)
	}

	out := buf.String()
	if !strings.HasPrefix(out, "2026-10-25 13:21:45.123 INFO  [app] 时间戳行") {
		t.Errorf("时间戳格式不对（应含年份与毫秒）：\n%s", out)
	}
}

// TestConsoleWriterColorsByLevelField 回归 [P2-3]：颜色按行内级别字段命中。
//
// 旧行首是时间戳，HasPrefix(s, "ERROR") 永远不命中，
// 于是 log.color 实际只有「全灰」一种效果。
func TestConsoleWriterColorsByLevelField(t *testing.T) {
	cases := []struct {
		line string
		want string
	}{
		{"2026-10-25 13:21:45.123 ERROR [app] boom\n", ansiRed},
		{"2026-10-25 13:21:45.123 WARN  [app] careful\n", ansiYellow},
		{"2026-10-25 13:21:45.123 INFO  [app] hi\n", ansiCyan},
		{"2026-10-25 13:21:45.123 DEBUG [app] detail\n", ansiGray},
	}
	for _, c := range cases {
		got := captureStdout(t, func() {
			w := &consoleWriter{color: true}
			if _, err := w.Write([]byte(c.line)); err != nil {
				t.Errorf("写入失败: %v", err)
			}
		})
		if !strings.HasPrefix(got, c.want) {
			t.Errorf("行 %q 着色错误：want 前缀 %q，got %q", c.line, c.want, got)
		}
		if !strings.HasSuffix(got, ansiReset+"\n") {
			t.Errorf("行 %q 缺少颜色复位：%q", c.line, got)
		}
	}

	// 消息文本里出现的「ERROR」字样不该把 INFO 行误染成红色。
	got := captureStdout(t, func() {
		w := &consoleWriter{color: true}
		_, _ = w.Write([]byte("2026-10-25 13:21:45.123 INFO  [app] handle ERROR failed\n"))
	})
	if !strings.HasPrefix(got, ansiCyan) {
		t.Errorf("含 ERROR 字样的 INFO 行被误染色: %q", got)
	}
}

// TestDegradedWarnGoesToConsole 回归 [P2-2] 的接线：
// 文件出口失效的一次性告警必须真的出现在终端出口。
func TestDegradedWarnGoesToConsole(t *testing.T) {
	dir := t.TempDir()
	withFreshInit(t, Options{Level: slog.LevelDebug, Dir: dir, Console: true}, nil)

	// 模拟文件失效：把文件出口置为未打开，并把路径换成目录（OpenFile 必失败）。
	mu.RLock()
	lf := logFile
	mu.RUnlock()
	if lf == nil {
		t.Fatal("initLocked 没有创建 logFile")
	}
	path := filepath.Join(dir, "oh-my-qqbot.log")
	lf.mu.Lock()
	_ = lf.f.Close()
	lf.f = nil
	lf.mu.Unlock()
	if err := os.Remove(path); err != nil {
		t.Fatalf("删除日志文件失败: %v", err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("占位目录创建失败: %v", err)
	}

	out := captureStdout(t, func() { Debug("这条会写文件失败") })
	if !strings.Contains(out, "文件日志已失效") {
		t.Errorf("终端出口缺少一次性告警:\n%s", out)
	}
}

// TestResetAllowsRepeatedInit 回归 [P2-30]：Reset 必须让第二次 Init 真的执行。
//
// Init 是 sync.Once，同一进程里第二次调用静默无效 —— 测试 harness
// 反复构造时全靠 Reset 恢复。这条测试自包含：先 Reset 排除别的测试
// 可能留下的状态，结尾再 Reset 恢复成「未初始化」基线。
func TestResetAllowsRepeatedInit(t *testing.T) {
	Reset()
	if err := Init(Options{Level: slog.LevelError, Console: false}); err != nil {
		t.Fatalf("第一次 Init 失败: %v", err)
	}
	if got := CurrentLevel(); got != slog.LevelError {
		t.Fatalf("第一次 Init 后级别 = %v，期望 ERROR", got)
	}
	if !SetLevel(slog.LevelError) {
		// 上面 Init 成功后 levelVar 必须已登记，SetLevel 必须可用。
		t.Error("Init 之后 SetLevel 应返回 true")
	}

	Reset()
	if err := Init(Options{Level: slog.LevelDebug, Console: false}); err != nil {
		t.Fatalf("Reset 后第二次 Init 失败: %v", err)
	}
	if got := CurrentLevel(); got != slog.LevelDebug {
		t.Errorf("Reset 后第二次 Init 的级别 = %v，期望 DEBUG（sync.Once 没被重置？）", got)
	}

	t.Cleanup(func() {
		// 恢复成「未初始化」基线，不把本测试的 handler 留给后面的测试。
		Reset()
	})
}
