package logx

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// logTimeFormat 是日志行的时间戳格式（含年份与毫秒）。
//
// consoleWriter 的级别识别依赖它是**定长**的（见 lineLevel），改它必须同步那边。
const logTimeFormat = "2006-01-02 15:04:05.000"

// textHandler 是自定义的 slog.Handler。
//
// 选择自己写而不是用 slog.TextHandler 的原因：
//  1. 输出格式要对齐 MaiBot 的 `[时间] [级别] [logger] 消息` 样式
//  2. 需要同时投递到 WebSocket 广播器
//  3. 需要支持运行时调整级别
//  4. 需要让终端与文件走**不同的**级别门槛（log.level / log.file_level）
//
// 第 4 点是这里之所以不用 io.MultiWriter 的原因：MultiWriter 只能共用
// 一个 handler 级别，于是「终端 INFO、文件 DEBUG」表达不出来 ——
// 早先的实现把两级取了更详细的那个，结果终端也跟着刷 DEBUG。
type textHandler struct {
	mu *sync.Mutex

	// console / file 各自可以为 nil（对应「不输出到终端 / 不写文件」）。
	console   io.Writer
	file      io.Writer
	consoleLv *slog.LevelVar
	fileLv    *slog.LevelVar

	// name 是 logger 名（Get/Named 通过 With("logger", …) 传入，
	// 由 WithAttrs 摘出来存在这里），用于 `[...]` 列与 SSE 的 logger 字段。
	// 以前它落在普通 kv 字段里，导致所有行都显示 `[app] ... logger=xxx`。
	name string

	// group 是 WithGroup 的键前缀（slog 的分组语义：组内键加 `group.` 前缀）。
	// 以前它被挪用为 logger 名，slog.Group 的嵌套语义完全失效。
	group string

	attrs []slog.Attr
}

// newTextHandler 只写一个出口（测试与非分级场景用）。
func newTextHandler(out io.Writer, level *slog.LevelVar) *textHandler {
	return newDualHandler(out, nil, level, level)
}

// newDualHandler 让终端与文件各用一个级别门槛。
func newDualHandler(console, file io.Writer, consoleLv, fileLv *slog.LevelVar) *textHandler {
	if consoleLv == nil {
		consoleLv = &slog.LevelVar{}
	}
	if fileLv == nil {
		fileLv = consoleLv
	}
	return &textHandler{
		mu:        &sync.Mutex{},
		console:   console,
		file:      file,
		consoleLv: consoleLv,
		fileLv:    fileLv,
	}
}

// Enabled 只要**任一出入口**要这条记录就必须处理。
//
// 收窄成「console 级别」会让文件丢掉更详细的日志（file_level=DEBUG 就失效），
// 收窄成「file 级别」又会让终端被文件级别拖成更啰嗦。
func (h *textHandler) Enabled(_ context.Context, l slog.Level) bool {
	if h.console != nil && l >= h.consoleLv.Level() {
		return true
	}
	if h.file != nil && l >= h.fileLv.Level() {
		return true
	}
	return false
}

func (h *textHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder

	// 时间（含年份与毫秒：跨年日志要能区分，归档名里的年份才对得上行内容）
	b.WriteString(r.Time.Format(logTimeFormat))
	b.WriteString(" ")

	// 级别
	b.WriteString(levelLabel(r.Level))
	b.WriteString(" ")

	// logger 名的解析优先级：记录级 `logger` 属性 > handler 名（Get/Named）> app。
	name := h.name
	var kv []string

	// 先输出通过 With 挂在 handler 上的固定字段。
	//
	// 这一段以前**完全缺失**：WithAttrs 把 attrs 存下来了，
	// 但 Handle 从头到尾没读过它，于是 log.With("k","v") 的字段
	// 全部静默消失 —— 不报错，只是日志里少东西。
	for _, a := range h.attrs {
		kv = append(kv, h.key(a.Key)+"="+formatValue(a.Value))
	}

	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "logger" {
			name = a.Value.String()
			return true
		}
		kv = append(kv, h.key(a.Key)+"="+formatValue(a.Value))
		return true
	})
	if name == "" {
		name = "app"
	}
	b.WriteString("[")
	b.WriteString(name)
	b.WriteString("] ")

	// 消息
	b.WriteString(r.Message)

	// 附加字段
	if len(kv) > 0 {
		b.WriteString(" ")
		b.WriteString(strings.Join(kv, " "))
	}
	b.WriteString("\n")

	line := b.String()

	h.mu.Lock()
	var firstErr error
	if h.console != nil && r.Level >= h.consoleLv.Level() {
		if _, err := io.WriteString(h.console, line); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if h.file != nil && r.Level >= h.fileLv.Level() {
		if _, err := io.WriteString(h.file, line); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	h.mu.Unlock()

	// 广播给 WebUI。放在锁外面，避免订阅者慢时拖住日志写入。
	//
	// 按 console 级别判断：WebUI 的日志页是给人看的实时流，
	// 不该因为 file_level 更详细就把它变成文件的镜像。
	mu.RLock()
	bc := broadcaster
	mu.RUnlock()
	if bc != nil && h.console != nil && r.Level >= h.consoleLv.Level() {
		bc.Broadcast(LogLine{
			Time:    r.Time,
			Level:   levelLabel(r.Level),
			Logger:  name,
			Message: r.Message,
		})
	}

	return firstErr
}

// key 给键加上 WithGroup 前缀（slog 分组语义的扁平化表示）。
func (h *textHandler) key(k string) string {
	if h.group == "" {
		return k
	}
	return h.group + "." + k
}

func (h *textHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	nh := *h
	var extra []slog.Attr
	for _, a := range attrs {
		if a.Key == "logger" {
			// 「logger 名」是 handler 状态而不是普通字段：落在 kv 里的话
			// `[...]` 列与 SSE 的 logger 字段永远拿不到它（曾是所有行都
			// 显示 `[app] ... logger=xxx`）。后设置的生效 —— Named 在已有
			// 名字的基础上派生全名时靠这条覆盖，不会叠出两个 logger= 字段。
			nh.name = a.Value.String()
			continue
		}
		extra = append(extra, a)
	}
	if len(extra) > 0 {
		// 必须复制切片：直接 append 会与父 handler 共享底层数组。
		nh.attrs = append(append([]slog.Attr{}, h.attrs...), extra...)
	}
	return &nh
}

// WithGroup 恢复 slog 的分组语义：组名作为键前缀（`group.key=value`），
// 不再挪用为 logger 名 —— logger 名由 `logger` 属性（Get/Named）表达。
func (h *textHandler) WithGroup(name string) slog.Handler {
	nh := *h
	if name == "" {
		return &nh
	}
	if nh.group == "" {
		nh.group = name
	} else {
		nh.group += "." + name
	}
	return &nh
}

func formatValue(v slog.Value) string {
	switch v.Kind() {
	case slog.KindString:
		s := v.String()
		if strings.ContainsAny(s, " \t\n") {
			return fmt.Sprintf("%q", s)
		}
		return s
	case slog.KindTime:
		return v.Time().Format(time.RFC3339)
	default:
		return v.String()
	}
}

// rotatingFile 是带大小轮转的文件写入器。
type rotatingFile struct {
	mu         sync.Mutex
	path       string
	maxSize    int64
	maxBackups int
	f          *os.File
	size       int64

	// closed 后 Write 一律丢弃，不再重新打开文件：
	// 否则「Close → Info(已关闭)」会把日志文件重新建出来。
	closed bool

	// degraded 表示文件出口当前失效（打开/轮转失败）。
	// 失效期间的每次告警只发一次（见 markDegraded），避免日志风暴。
	degraded bool

	// warn 是「文件出口失效」的一次性告警出口（终端 writer，Init 时注入）。
	// nil 时静默（测试直连 newRotatingFile 的场景）。
	warn func(err error)
}

func newRotatingFile(path string, maxSizeMB, maxBackups int, warn func(error)) (*rotatingFile, error) {
	if maxSizeMB <= 0 {
		maxSizeMB = 16
	}
	if maxBackups <= 0 {
		maxBackups = 5
	}
	rf := &rotatingFile{
		path:       path,
		maxSize:    int64(maxSizeMB) * 1024 * 1024,
		maxBackups: maxBackups,
		warn:       warn,
	}
	if err := rf.open(); err != nil {
		return nil, err
	}
	return rf, nil
}

func (r *rotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	r.f = f
	r.size = info.Size()
	if r.degraded {
		// 恢复：清掉降级标志，下一次失效会再告警一次。
		r.degraded = false
	}
	return nil
}

func (r *rotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		// Close 之后丢弃而不是重新打开：重新打开会让「已关闭」的文件
		// 又被建出来，日志文件语义混乱（进程退出路径每天都会踩）。
		return len(p), nil
	}

	if r.f == nil {
		if err := r.open(); err != nil {
			r.markDegraded(err)
			return 0, err
		}
	}

	if r.size+int64(len(p)) > r.maxSize {
		r.rotate()
		if r.f == nil {
			// rotate 失败且重开也没成功（markDegraded 已告警过一次）。
			return 0, fmt.Errorf("日志文件不可用: %s", r.path)
		}
	}

	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// markDegraded 记录「文件出口失效」并通过 warn 告警一次。
//
// 只告警一次：失效期间每条日志都会走到这里，重复告警会形成风暴；
// 恢复（open 成功）后清标志，下一次失效再告警。
// slog 会忽略 Handle 返回的错误，所以不告警的话这条路径完全静默。
func (r *rotatingFile) markDegraded(err error) {
	if r.degraded {
		return
	}
	r.degraded = true
	if r.warn != nil {
		r.warn(err)
	}
}

// rotate 把当前文件改名归档，并清理超出保留数的旧文件。
//
// 任一步失败都不丢进程、不丢后续日志：见各分支的回退策略。
func (r *rotatingFile) rotate() {
	if r.f != nil {
		_ = r.f.Close()
		r.f = nil
	}

	// 归档名用纳秒精度：秒级名字在同一秒内多次轮转时会互相覆盖
	// （Windows 的 os.Rename 与 POSIX rename(2) 都是覆盖语义），
	// 后一份归档把前一份吃掉，且没有任何错误 —— 实测写 4.3MB 只剩 716KB。
	//
	// 但纳秒在 Windows 上并不够：time.Now 的时钟粒度约 0.5~15.6ms
	// （实测连续 8 次采样 7 次纳秒完全相同），几毫秒内的多次轮转仍会重名。
	// 所以名字已存在时追加递增序号探测，保证绝不覆盖。
	stamp := time.Now().Format("20060102-150405.000000000")
	archived := fmt.Sprintf("%s.%s", r.path, stamp)
	for i := 1; ; i++ {
		if _, err := os.Stat(archived); os.IsNotExist(err) {
			break
		}
		archived = fmt.Sprintf("%s.%s.%d", r.path, stamp, i)
	}
	if err := os.Rename(r.path, archived); err != nil {
		// 改名失败（文件被占用/权限问题/文件消失）：回退为「不轮转、
		// 重新打开原文件继续追加」。open 成功后 r.size 会被修正为
		// 真实文件大小，不会残留「已改名」的脏值。
		r.markDegraded(fmt.Errorf("日志轮转失败，回退为继续追加当前文件: %w", err))
		if err := r.open(); err != nil {
			// 已经 degraded 时 markDegraded 是 no-op，不会重复告警；
			// 等下次 Write 再重试 open。
			r.markDegraded(err)
		}
		return
	}

	if err := r.open(); err != nil {
		// 归档成功但新文件打不开：置降级，等下次 Write 重试 open。
		r.markDegraded(err)
		return
	}

	r.cleanup()
}

func (r *rotatingFile) cleanup() {
	dir := filepath.Dir(r.path)
	base := filepath.Base(r.path)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	var archives []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), base+".") {
			archives = append(archives, filepath.Join(dir, e.Name()))
		}
	}
	if len(archives) <= r.maxBackups {
		return
	}

	// 归档名以定长时间戳开头，字典序即时间序。
	sort.Strings(archives)
	for _, p := range archives[:len(archives)-r.maxBackups] {
		_ = os.Remove(p)
	}
}

func (r *rotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}
