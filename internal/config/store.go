package config

// 本文件实现「运行时可变配置」。
//
// 需求：WebUI 要能改配置并生效（对齐 MaiBot 的配置编辑）。
//
// 难点不是写文件，而是**并发安全**：配置被 20 多个组件跨协程读取
// （每个会话的 run 协程读 cfg.Chat.Timing、学习器读 cfg.Learning、
// 门控读 cfg.Chat...）。如果直接就地改结构体字段，就是 AGENTS.md
// 第 68 条说的那类「裸字段跨协程共享」——功能测试全绿，几万次调度里
// 可能一次都不出事，一出事就是毫无头绪的脏读。
//
// 做法：
//  1. 所有读者通过 Store.Get() 拿一份**只读快照指针**，
//     改配置时整份复制、改完再原子换掉指针（atomic.Pointer）。
//     旧快照永不修改，正在读的协程读到的永远是一份自洽的配置。
//  2. 写完必须先 Validate 再落盘：校验不过就不换指针、不写文件，
//     把错误返回给 WebUI —— 不允许「写了坏配置但进程还在跑旧配置」
//     这种状态（重启后直接起不来，用户完全不知道为什么）。
//  3. 落盘走「写临时文件 + rename」，避免写到一半断电留下半截 TOML。
//
// 为什么不做细粒度热重载（只重建受影响组件）：
// 那需要每个组件都能安全替换自己的依赖，改动面比这个功能本身大得多。
// 现在的做法是「换快照 + 少量回调」：能就地生效的（提示词、过滤词、
// 门控阈值、上下文条数……）下次读取自动生效；不能就地生效的
// （监听端口、数据库路径、日志目录、QQ 凭据）明确标成需要重启。

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/pelletier/go-toml/v2"
)

// Store 持有当前生效的配置，并支持原子替换。
//
// 零值不可用，必须用 NewStore 构造。
type Store struct {
	path string

	// cur 是当前生效的配置快照。
	// 用 atomic.Pointer 而不是 mutex：读多写极少（读是每个会话每一轮，
	// 写是用户点一次保存），读路径上不该有锁。
	cur atomic.Pointer[Config]

	// writeMu 串行化「保存」这个动作本身。
	// 它不保护读：读走的是 cur 的原子指针。加它是为了避免两次并发保存
	// 各自读到不同基线、后写的把先写的覆盖掉。
	writeMu sync.Mutex

	// hooks 在写盘成功后依次调用，用于让相关组件重建内部状态
	// （例如过滤器重新编译屏蔽词正则）。
	hooks []func(old, next *Config)
	hookM sync.RWMutex
}

// NewStore 从磁盘加载配置并创建 Store。
func NewStore(path string) (*Store, error) {
	cfg, created, err := LoadOrCreate(path)
	if err != nil {
		return nil, err
	}
	_ = created
	s := &Store{path: path}
	s.cur.Store(cfg)
	return s, nil
}

// NewStoreFrom 用一份已有的配置创建 Store（测试与首次启动用）。
func NewStoreFrom(cfg *Config, path string) *Store {
	s := &Store{path: path}
	s.cur.Store(cfg)
	return s
}

// Get 返回当前生效的配置快照。
//
// 返回的指针是只读的：调用方绝不能修改它。修改必须走 Update，
// 那样才会产生新快照。返回值永不为 nil。
func (s *Store) Get() *Config {
	if s == nil {
		return nil
	}
	return s.cur.Load()
}

// Path 返回配置文件路径。
func (s *Store) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// OnSave 注册一个「配置变更后」的回调。
//
// 回调在保存成功后同步调用，拿到新旧两份快照。
// 只能做轻量的事（重建几个小对象），不要在这里做 IO 或阻塞 ——
// 它跑在 HTTP 请求的调用栈里。
func (s *Store) OnSave(fn func(old, next *Config)) {
	if s == nil || fn == nil {
		return
	}
	s.hookM.Lock()
	defer s.hookM.Unlock()
	s.hooks = append(s.hooks, fn)
}

// Update 用 mutate 修改配置，校验通过后落盘并让新配置生效。
//
// mutate 收到的是一份**深拷贝**，可以直接改。它返回错误时整个更新取消。
//
// 顺序是刻意的：校验 → 落盘 → 换快照 → 回调。
//   - 校验不过：什么都不发生，用户看到错误提示
//   - 落盘失败：什么都没变（内存里还是旧配置，磁盘上还是旧文件）
//   - 落盘成功但换快照前进程崩了：磁盘上是新配置，重启后生效，
//     这是可接受的结果（比「内存新、磁盘旧」安全得多）
func (s *Store) Update(mutate func(*Config) error) error {
	// 先在锁外拍一份旧快照。放锁外没有并发问题：
	// 换快照是原子操作，而 read-then-write 之间的竞争由下面的 writeMu 串行化。
	old := s.cur.Load()
	if old == nil {
		return errors.New("配置尚未初始化")
	}

	// next 在锁内构造并落盘，然后释放锁再跑钩子。
	//
	// 钩子必须在锁外跑：钩子理论上可能调 Update（例如「配置变了顺手
	// 存一份派生值」），在 writeMu 持有期间调就是永久死锁，
	// 而且表现是 HTTP 请求永远不返回、后续所有保存全部阻塞。
	next, err := func() (*Config, error) {
		s.writeMu.Lock()
		defer s.writeMu.Unlock()

		// 锁内重读：等在锁外等锁期间可能已经被别的保存换过了，
		// 拿那份最新的做基线，否则后写者会把先写者的改动覆盖掉。
		base := s.cur.Load()
		if base == nil {
			return nil, errors.New("配置尚未初始化")
		}

		n, err := base.Clone()
		if err != nil {
			return nil, err
		}
		if mutate != nil {
			if err := mutate(n); err != nil {
				return nil, err
			}
		}

		// 与应用启动时同一套校验：不允许用 WebUI 写出「重启后会起不来」的配置。
		// Validate 里还会补齐若干缺省值，所以必须在写盘之前调用。
		if err := n.Validate(); err != nil {
			return nil, err
		}

		if s.path != "" {
			if err := SaveAtomic(n, s.path); err != nil {
				return nil, err
			}
		}

		s.cur.Store(n)
		return n, nil
	}()
	if err != nil {
		return err
	}

	s.runHooks(old, next)
	return nil
}

// runHooks 在锁外依次调用钩子。
func (s *Store) runHooks(old, next *Config) {
	s.hookM.RLock()
	hooks := append([]func(old, next *Config){}, s.hooks...)
	s.hookM.RUnlock()
	for _, fn := range hooks {
		fn(old, next)
	}
}

// Reload 从磁盘重新加载，丢弃内存里的改动。
func (s *Store) Reload() error {
	if s.path == "" {
		return errors.New("未绑定配置文件路径")
	}

	var old, next *Config
	err := func() error {
		s.writeMu.Lock()
		defer s.writeMu.Unlock()

		n, err := Load(s.path)
		if err != nil {
			return err
		}
		old = s.cur.Load()
		next = n
		s.cur.Store(n)
		return nil
	}()
	if err != nil {
		return err
	}

	s.runHooks(old, next)
	return nil
}

// Clone 深拷贝一份配置。
//
// 手写而不是用反射/序列化：字段数量固定、改动频率低，
// 手写一份能保证「加了字段忘了拷贝」在编译期就被发现不了 ——
// 不，恰恰相反，手写反而容易漏。所以用 TOML 往返：
// 慢一点，但加字段时不需要动这里。
//
// 失败时返回错误而不是「退化成 Default()」：
// Update 的下一步就是 Validate + SaveAtomic，拿一份默认值去过校验
// 当然能通过，于是用户辛苦配的东西会被默认值默默覆盖，接口还回 200。
// 宁可不保存，也不能把配置写丢。
func (c *Config) Clone() (*Config, error) {
	if c == nil {
		return nil, nil
	}
	raw, err := toml.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("复制配置失败（序列化）: %w", err)
	}
	out := Default()
	if err := toml.Unmarshal(raw, out); err != nil {
		return nil, fmt.Errorf("复制配置失败（反序列化）: %w", err)
	}
	return out, nil
}

// SaveAtomic 先写临时文件再 rename，避免留下半截配置。
//
// 直接 os.WriteFile 的问题是：写到一半进程被杀/断电，
// 磁盘上就是一个语法不完整的 TOML，下次启动直接报解析失败。
func SaveAtomic(cfg *Config, path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建配置目录 %s 失败: %w", dir, err)
	}

	raw, err := toml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}

	// 保留原有的注释头（用户手写的说明不要每次保存都被抹掉）。
	content := raw
	if prev, err := os.ReadFile(path); err == nil {
		if header := leadingComments(prev); len(header) > 0 {
			content = append(header, raw...)
		}
	}

	tmp, err := os.CreateTemp(dir, ".config-*.toml")
	if err != nil {
		return fmt.Errorf("创建临时配置失败: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // rename 成功后这里是无操作（文件已不存在）

	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return fmt.Errorf("写入临时配置失败: %w", err)
	}
	// 先 sync 再 rename：否则 rename 成功但内容还在页缓存里，
	// 断电后得到的是空文件。
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("刷新配置到磁盘失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时配置失败: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("替换配置 %s 失败: %w", path, err)
	}
	return nil
}

// leadingComments 取出文件开头连续的注释行与空行。
//
// 只取「文件顶部、在第一个非注释行之前」的那一段：
// 中间散落的注释不属于头部，跟着 rename 一起重排反而会错位。
func leadingComments(raw []byte) []byte {
	lines := strings.Split(string(raw), "\n")
	var out []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			break
		}
		out = append(out, line)
	}
	if len(out) == 0 {
		return nil
	}
	// 保证头部与正文之间有空行分隔。
	joined := strings.Join(out, "\n")
	joined = strings.TrimRight(joined, "\n") + "\n\n"
	return []byte(joined)
}

// Wrap 把一份现成的配置包成 Store（不绑文件路径）。
//
// 测试里到处需要「给定一份配置」的 Store：`config.Wrap(cfg)` 比
// `config.NewStoreFrom(cfg, "")` 短，而且读起来意图更清楚。
func Wrap(cfg *Config) *Store {
	if cfg == nil {
		// 返回真正的 nil 而不是「包着 nil 的 Store」：
		// 调用方普遍用 `if x.cfg == nil` 判断「没有配置」，
		// 包一层会让那个判断失效，然后在下一行解引用崩掉。
		return nil
	}
	return NewStoreFrom(cfg, "")
}

// LoadOrCreateStore 是 LoadOrCreate 的 Store 版本。
//
// 返回 created=true 表示刚生成了默认配置（此时调用方通常应该提示用户去填凭据）。
func LoadOrCreateStore(path string) (s *Store, created bool, err error) {
	if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
		cfg := Default()
		if saveErr := Save(cfg, path); saveErr != nil {
			return nil, false, saveErr
		}
		return NewStoreFrom(cfg, path), true, nil
	}
	cfg, err := Load(path)
	if err != nil {
		return nil, false, err
	}
	return NewStoreFrom(cfg, path), false, nil
}
