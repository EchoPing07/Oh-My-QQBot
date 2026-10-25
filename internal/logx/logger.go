// Package logx 提供统一日志。
// 对应 MaiBot 的 src/common/logger.py，行为对齐：终端彩色 + 文件 + WebSocket 广播。
package logx

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Level 是日志级别名。
type Level = slog.Level

const (
	LevelDebug = slog.LevelDebug
	LevelInfo  = slog.LevelInfo
	LevelWarn  = slog.LevelWarn
	LevelError = slog.LevelError
)

// ParseLevel 把配置里的级别字符串转成 slog 级别。
func ParseLevel(s string) slog.Level {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "DEBUG":
		return slog.LevelDebug
	case "INFO":
		return slog.LevelInfo
	case "WARN", "WARNING":
		return slog.LevelWarn
	case "ERROR", "CRITICAL":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// ANSI 颜色，只在终端输出时使用。
const (
	ansiReset  = "\x1b[0m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[36m"
	ansiGreen  = "\x1b[32m"
	ansiGray   = "\x1b[90m"
)

func levelColor(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return ansiRed
	case l >= slog.LevelWarn:
		return ansiYellow
	case l >= slog.LevelInfo:
		return ansiCyan
	default:
		return ansiGray
	}
}

func levelLabel(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "ERROR"
	case l >= slog.LevelWarn:
		return "WARN "
	case l >= slog.LevelInfo:
		return "INFO "
	default:
		return "DEBUG"
	}
}

// Broadcaster 是日志广播接口，WebUI 用它推送实时日志。
type Broadcaster interface {
	Broadcast(line LogLine)
}

// LogLine 是一条广播出去的日志。
type LogLine struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"`
	Logger  string    `json:"logger"`
	Message string    `json:"message"`
}

// Options 是初始化日志的参数。
type Options struct {
	// Level 是全局最低级别。
	Level slog.Level
	// FileLevel 是文件输出的最低级别。
	FileLevel slog.Level
	// FileLevelSet 表示 FileLevel 是否被显式配置。
	//
	// 「未配置」必须用这个独立 bool 表达：slog.LevelInfo 恰好等于 0，
	// 用 `FileLevel != 0` 判断会把「显式配 INFO」当成「未配置」，
	// 重启后文件门槛静默退回终端门槛。未配置时文件门槛跟随 Level。
	FileLevelSet bool
	// Dir 是日志目录。
	Dir string
	// MaxSizeMB 是单文件大小上限。
	MaxSizeMB int
	// MaxBackups 是保留的历史文件数。
	MaxBackups int
	// Console 是否输出到终端。
	Console bool
	// Color 是否在终端使用颜色。
	Color bool
}

// Logger 包装 slog.Logger 并附带广播能力。
type Logger struct {
	*slog.Logger
	name string
}

// With 返回带字段的子 logger。
func (l *Logger) With(args ...any) *Logger {
	return &Logger{Logger: l.Logger.With(args...), name: l.name}
}

// Named 返回带名字的子 logger。
//
// 名字以 `.` 派生（agent.loop → agent.loop.sub），不会叠出两个 logger= 字段：
// handler 把 `logger` 属性当名字状态而不是普通字段，后设置的生效。
func (l *Logger) Named(name string) *Logger {
	full := name
	if l.name != "" {
		full = l.name + "." + name
	}
	return &Logger{Logger: l.Logger.With("logger", full), name: full}
}

var (
	mu          sync.RWMutex
	rootLogger  *slog.Logger
	broadcaster Broadcaster
	logFile     *rotatingFile
	initOnce    sync.Once
	initErr     error
	// pendingLevel 存「Init 之前设置的级别」，Init 时优先采用。
	pendingLevel *slog.Level
	// pendingFileLevel 同上，对应文件门槛。
	pendingFileLevel *slog.Level
	// levelVar 是终端（与 WebUI 广播）的级别，由 log.level 控制。
	//
	// 单独拎出来是为了让 WebUI 能改 log.level 之后立刻生效 ——
	// 它的引用同时被 handler 持有，SetLevel 一改，下一次写日志就按新级别过滤。
	levelVar *slog.LevelVar
	// fileLevelVar 是写文件的级别，由 log.file_level 控制。
	//
	// 与 levelVar 分开是必要的：配置文件默认就是 level=INFO、file_level=DEBUG，
	// 合成一个门槛会让终端也跟着刷 DEBUG。
	fileLevelVar *slog.LevelVar
)

// SetLevel 运行期调整日志级别。
//
// 只影响「级别过滤」，不改输出目标 —— 换日志目录/文件轮转仍然要重启。
//
// 返回是否真的改了：未 Init 时没有 LevelVar 可改，此时返回 false。
// 必须让调用方知道 —— 静默失败的表现是「WebUI 上说改成功了，
// 日志里还是什么都没有」，这比直接报错难查得多。
func SetLevel(l slog.Level) bool {
	mu.Lock()
	if levelVar == nil {
		// 允许在 Init 之前就设级别：把值记下来，Init 时生效。
		pendingLevel = &l
		mu.Unlock()
		return false
	}
	lv := levelVar
	mu.Unlock()
	lv.Set(l)
	return true
}

// CurrentLevel 返回当前生效的终端日志级别。
func CurrentLevel() slog.Level {
	mu.RLock()
	lv, pending := levelVar, pendingLevel
	mu.RUnlock()
	if lv != nil {
		return lv.Level()
	}
	if pending != nil {
		return *pending
	}
	return slog.LevelInfo
}

// SetFileLevel 运行期调整**写文件**的日志级别。
//
// 与 SetLevel 分开：终端与文件本来就是两个门槛
// （默认 level=INFO 写终端、file_level=DEBUG 写文件）。
// 未 Init 时同样先暂存，Init 时生效。
func SetFileLevel(l slog.Level) bool {
	mu.Lock()
	if fileLevelVar == nil {
		pendingFileLevel = &l
		mu.Unlock()
		return false
	}
	lv := fileLevelVar
	mu.Unlock()
	lv.Set(l)
	return true
}

// CurrentFileLevel 返回当前生效的文件日志级别。
//
// 未 Init 时返回 Info：调用方（WebUI 只读信息、测试）需要与
// CurrentLevel 一样有一个确定值，不能是「未定义」。
func CurrentFileLevel() slog.Level {
	mu.RLock()
	lv, pending := fileLevelVar, pendingFileLevel
	mu.RUnlock()
	if lv != nil {
		return lv.Level()
	}
	if pending != nil {
		return *pending
	}
	return slog.LevelInfo
}

// Init 初始化全局日志。
func Init(opts Options) error {
	initOnce.Do(func() {
		initErr = initLocked(opts)
	})
	return initErr
}

// Reset 清空全部全局日志状态，把 Init 恢复成「未执行过」。
//
// 仅测试用：Init 由 sync.Once 守着，同一进程里第二次 Init 会静默无效
// （audit 11 P2-30）。测试 harness 需要反复构造时必须先 Reset 再 Init，
// 否则「通过选项控制日志级别」在第二个 harness 里永远不生效。
//
// 会关闭已打开的日志文件；不还原 slog.Default —— 下一次 Init 会重新设置，
// 两次调用之间的日志走旧的兜底路径（Get 未初始化时退化为 slog.Default）。
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	if logFile != nil {
		_ = logFile.Close()
		logFile = nil
	}
	rootLogger = nil
	broadcaster = nil
	levelVar = nil
	fileLevelVar = nil
	pendingLevel = nil
	pendingFileLevel = nil
	initErr = nil
	initOnce = sync.Once{}
}

func initLocked(opts Options) error {
	var console, file io.Writer

	if opts.Console {
		console = &consoleWriter{color: opts.Color}
	}

	if opts.Dir != "" {
		if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
			return fmt.Errorf("创建日志目录失败: %w", err)
		}
		// 文件出口失效（打开/轮转失败）时的一次性告警走终端出口：
		// 直接写 console writer 而不是再走 slog —— 文件出口已经坏了，
		// 走 handler 只会再次失败。console 为 nil（未开终端）时无处可说，静默。
		warn := func(err error) {
			if console == nil {
				return
			}
			msg := time.Now().Format(logTimeFormat) + " " + levelLabel(slog.LevelWarn) + " [logx] 文件日志已失效（只告警一次，恢复后自动清除）: " + err.Error() + "\n"
			_, _ = io.WriteString(console, msg)
		}
		f, err := newRotatingFile(filepath.Join(opts.Dir, "oh-my-qqbot.log"), opts.MaxSizeMB, opts.MaxBackups, warn)
		if err != nil {
			return fmt.Errorf("打开日志文件失败: %w", err)
		}
		logFile = f
		file = f
	}

	// 两个出口都没有时别把日志丢掉：退化成终端输出。
	// （以前写成 io.Discard，于是「Console=false 且没配 Dir」时日志静默消失。）
	if console == nil && file == nil {
		console = &consoleWriter{color: opts.Color}
	}

	consoleLv := &slog.LevelVar{}
	consoleLv.Set(opts.Level)

	// file_level 是独立门槛，语义就是「写文件可以比终端更详细」。
	//
	// 「是否配置过」必须看 FileLevelSet：slog.LevelInfo 恰好等于 0，
	// 用 `FileLevel != 0` 判断会把显式配的 INFO 当成「未配置」，
	// 于是 file_level=INFO + level=DEBUG 时文件里全是 DEBUG（实测）。
	fileLv := &slog.LevelVar{}
	if opts.FileLevelSet {
		fileLv.Set(opts.FileLevel)
	} else {
		fileLv.Set(opts.Level)
	}

	// Init 之前调 SetLevel / SetFileLevel 的场景（测试、提前初始化）以它为最终值。
	if pendingLevel != nil {
		consoleLv.Set(*pendingLevel)
	}
	if pendingFileLevel != nil {
		fileLv.Set(*pendingFileLevel)
	}

	handler := newDualHandler(console, file, consoleLv, fileLv)

	// 先登记级别变量、再设默认 logger：反过来会留一个「SetDefault 已生效、
	// levelVar 还没登记」的窗口，此时并发的 SetLevel 只能暂存进 pending
	// —— 而 pending 已经在上面读过了，值会静默丢失。生产是单线程启动
	// 不可达，测试并发初始化时会踩到。
	mu.Lock()
	levelVar = consoleLv
	fileLevelVar = fileLv
	mu.Unlock()

	rootLogger = slog.New(handler)
	slog.SetDefault(rootLogger)
	return nil
}

// SetBroadcaster 注册日志广播器，用于把日志推给 WebUI。
func SetBroadcaster(b Broadcaster) {
	mu.Lock()
	defer mu.Unlock()
	broadcaster = b
}

// Get 返回指定名字的 logger。
//
// 名字通过 With("logger", name) 传入，handler 把它识别为名字状态
// （显示在 `[...]` 列与 SSE 的 logger 字段），不会重复出现在 kv 字段里。
func Get(name string) *Logger {
	mu.RLock()
	base := rootLogger
	mu.RUnlock()

	if base == nil {
		// 未初始化时退化为默认 stderr 输出，避免 panic。
		base = slog.Default()
	}
	if name == "" {
		return &Logger{Logger: base, name: ""}
	}
	return &Logger{Logger: base.With("logger", name), name: name}
}

// Debug/Info/Warn/Error 是便捷函数，使用默认 logger。

func Debug(msg string, args ...any) { Get("").Logger.Debug(msg, args...) }
func Info(msg string, args ...any)  { Get("").Logger.Info(msg, args...) }
func Warn(msg string, args ...any)  { Get("").Logger.Warn(msg, args...) }
func Error(msg string, args ...any) { Get("").Logger.Error(msg, args...) }

// Close 关闭日志文件。
//
// 关闭后文件出口进入「关闭态」：后续日志不再重新打开文件（直接丢弃），
// 否则「Close → Info(已关闭)」会把日志文件重新建出来。
func Close() {
	mu.Lock()
	defer mu.Unlock()
	if logFile != nil {
		_ = logFile.Close()
		logFile = nil
	}
}

// consoleWriter 负责把日志行写成带颜色的终端输出。
type consoleWriter struct {
	color bool
}

// lineLevel 从格式化后的日志行里取级别。
//
// 行格式是 `<logTimeFormat> <级别（定宽 5）> [<logger>] <消息>`，
// 时间戳与级别标签都是定宽的，直接按偏移取级别字段 ——
// 不会把消息文本里出现的「ERROR」之类的字样误判成级别。
func lineLevel(s string) slog.Level {
	i := len(logTimeFormat) + 1
	if len(s) < i+5 {
		return slog.LevelInfo
	}
	switch strings.TrimSpace(s[i : i+5]) {
	case "ERROR":
		return slog.LevelError
	case "WARN":
		return slog.LevelWarn
	case "DEBUG":
		return slog.LevelDebug
	default:
		return slog.LevelInfo
	}
}

// Write 把着色后的行写到 stdout。
//
// 返回值必须恒为 len(p)，不能返回实际写出的字节数：
// 着色会插入 ANSI 转义，写出长度大于 len(p)，而 io.MultiWriter 只要看到
// 某个 writer 返回 n != len(p) 就判定 ErrShortWrite 并中止，
// 于是排在后面的文件 writer 一条都收不到（曾导致 logs/oh-my-qqbot.log 恒为 0 字节）。
func (w *consoleWriter) Write(p []byte) (int, error) {
	s := string(p)
	if w.color {
		// 按行内级别字段着色。曾经用 HasPrefix(s, "ERROR") 判断，
		// 但行首是时间戳，永远不命中 —— 彩色输出实际只有灰色一种。
		//
		// 复位码插在最后一个换行符**之前**：留在 \n 之后的话颜色状态
		// 会跨行残留，按行切割输出的终端/日志采集器会把 reset 归到下一行。
		c := levelColor(lineLevel(s))
		if i := strings.LastIndexByte(s, '\n'); i >= 0 {
			s = c + s[:i] + ansiReset + s[i:]
		} else {
			s = c + s + ansiReset
		}
	}
	if _, err := os.Stdout.Write([]byte(s)); err != nil {
		return 0, err
	}
	return len(p), nil
}

// contextKey 用于把字段塞到 context 里。
type contextKey struct{}

// WithContext 把 logger 放进 context，方便跨层传递。
func WithContext(ctx context.Context, l *Logger) context.Context {
	return context.WithValue(ctx, contextKey{}, l)
}

// FromContext 取出 context 里的 logger，没有则返回默认。
func FromContext(ctx context.Context) *Logger {
	if l, ok := ctx.Value(contextKey{}).(*Logger); ok {
		return l
	}
	return Get("")
}
