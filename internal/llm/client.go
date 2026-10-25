package llm

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/logx"
)

// Handler 是每次调用完成后的回调，用于统计与调试。
type Handler func(task string, resp *Response, err error)

// Client 是模型调用客户端。
// 对应 MaiBot 的 BaseClient + utils_model 的模型选择与重试逻辑。
type Client struct {
	cfg  *config.Store
	http *http.Client
	log  *logx.Logger

	mu        sync.RWMutex
	counters  map[string]int // 模型名 -> 调用次数，用于 balance 策略
	handlerMu sync.RWMutex
	handlers  []Handler
}

// New 创建客户端。
func New(cfg *config.Store) *Client {
	return &Client{
		cfg:      cfg,
		http:     &http.Client{Timeout: 0}, // 超时由每请求的 context 控制
		log:      logx.Get("llm"),
		counters: make(map[string]int),
	}
}

// OnComplete 注册调用完成回调。
func (c *Client) OnComplete(h Handler) {
	c.handlerMu.Lock()
	defer c.handlerMu.Unlock()
	c.handlers = append(c.handlers, h)
}

func (c *Client) emit(task string, resp *Response, err error) {
	c.handlerMu.RLock()
	hs := append([]Handler(nil), c.handlers...)
	c.handlerMu.RUnlock()
	for _, h := range hs {
		h(task, resp, err)
	}
}

// Generate 是上层使用的主入口：按任务选模型，带重试与模型切换。
//
// 与 MaiBot 的行为对齐：
//   - 按 SelectionStrategy 选模型（balance / random / sequential）
//   - 失败的模型在本轮内不再重试，直接切下一个
//   - 每个模型最多尝试 provider.MaxRetry 次
//   - 任务硬超时到点取消并切模型
func (c *Client) Generate(ctx context.Context, task config.TaskName, req *Request) (*Response, error) {
	return c.generate(ctx, task, req, nil)
}

// GenerateStream 是流式版本。
func (c *Client) GenerateStream(ctx context.Context, task config.TaskName, req *Request, fn func(Delta)) (*Response, error) {
	return c.generate(ctx, task, req, fn)
}

func (c *Client) generate(ctx context.Context, task config.TaskName, req *Request, fn func(Delta)) (*Response, error) {
	models := c.cfg.Get().ResolveModels(task)
	if len(models) == 0 {
		err := fmt.Errorf("%w: 任务 %s", ErrNoModel, task)
		c.emit(string(task), nil, err)
		return nil, err
	}

	taskCfg := c.cfg.Get().Task(task)
	order := c.orderModels(models, taskCfg.SelectionStrategy)

	// 任务硬超时。0 表示不限，交给 provider 的 timeout 管。
	//
	// 以前是 <=0 时兜底成 240 秒 —— 那等于让「配置里不写 hard_timeout」
	// 和「写 240」变成同一件事，用户没法表达「不限」。
	// 现在 0 就是 0：由 attemptTimeout 退到 provider.timeout。
	hardTimeout := time.Duration(taskCfg.HardTimeout * float64(time.Second))

	var lastErr error
	for _, modelName := range order {
		model, err := c.cfg.Get().Model(modelName)
		if err != nil {
			lastErr = err
			continue
		}
		provider, err := c.cfg.Get().Provider(model.Provider)
		if err != nil {
			lastErr = err
			continue
		}

		resp, err := c.tryModel(ctx, task, taskCfg, model, provider, req, fn, hardTimeout)
		if err == nil {
			c.emit(string(task), resp, nil)
			return resp, nil
		}
		lastErr = err

		// 上下文取消（含打断）直接返回，不切换模型。
		if ctx.Err() != nil {
			c.emit(string(task), nil, err)
			return nil, err
		}
		c.log.Warn("模型调用失败，尝试下一个", "task", task, "model", modelName, "err", err)
	}

	if lastErr == nil {
		lastErr = ErrNoModel
	}
	c.emit(string(task), nil, lastErr)
	return nil, lastErr
}

// tryModel 在单个模型上带重试地执行请求。
func (c *Client) tryModel(
	ctx context.Context,
	task config.TaskName,
	taskCfg config.TaskModelConfig,
	model *config.ModelConfig,
	provider *config.ProviderConfig,
	req *Request,
	fn func(Delta),
	hardTimeout time.Duration,
) (*Response, error) {
	attempts := provider.Retry()

	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(provider.RetryIntervalDuration()):
			}
		}

		// 每次尝试独立超时，到点取消。
		//
		// 取「任务硬超时」与「服务商 timeout」里较小的那个：
		// 以前 provider.timeout 完全没被读取，改配置不生效。
		// 两个都 <= 0 时才真正不限时。
		//
		// 先算出超时再建 ctx：以前是先建一个 WithCancel 再立刻 cancel 掉换成
		// WithTimeout，功能正确但每次请求都白建一个 context。
		var (
			ctxWithDeadline context.Context
			cancel          context.CancelFunc
			timeout         = minPositive(hardTimeout, provider.TimeoutDuration())
		)
		if timeout > 0 {
			ctxWithDeadline, cancel = context.WithTimeout(ctx, timeout)
		} else {
			// 两边都没设超时时仍然要能被打断，所以还是建一个可取消的 ctx。
			ctxWithDeadline, cancel = context.WithCancel(ctx)
		}

		// 把任务级配置合到请求上，模型级配置优先。
		merged := c.mergeRequest(req, taskCfg, model)

		resp, err := c.doOpenAI(ctxWithDeadline, provider, merged, fn)

		// 必须在 cancel 之前读超时状态，否则永远是 Canceled。
		timedOut := ctxWithDeadline.Err() == context.DeadlineExceeded
		cancel()

		if err == nil {
			c.bumpCounter(model.Name)
			return resp, nil
		}
		lastErr = err

		// 上层主动取消（含打断）直接退出，不再重试或切换模型。
		if ctx.Err() != nil {
			return nil, ErrAborted
		}
		if timedOut {
			// 不要把底层错误拼进去：超时时底层拿到的必然是「被取消」，
			// 拼出来是「请求超时: 请求被中断」，两层意思互相抵消，
			// 排查的人会以为是自己主动打断的。真正的原因只有一个：到点了。
			lastErr = fmt.Errorf("%w（超过 %v）", ErrTimeout, timeout)
		}
	}
	return nil, lastErr
}

// mergeRequest 把任务级与模型级默认值合入请求。
func (c *Client) mergeRequest(req *Request, taskCfg config.TaskModelConfig, model *config.ModelConfig) *Request {
	out := *req

	// 模型标识符由配置决定，调用方不需要知道。
	if model.ModelIdentifier != "" {
		out.Model = model.ModelIdentifier
	}

	// 温度：模型级 > 任务级。
	if out.Temperature == nil {
		if model.Temperature != nil {
			out.Temperature = model.Temperature
		} else {
			t := taskCfg.Temperature
			out.Temperature = &t
		}
	}

	// MaxTokens：模型级 > 任务级。
	if out.MaxTokens == nil {
		if model.MaxTokens != nil {
			out.MaxTokens = model.MaxTokens
		} else if taskCfg.MaxTokens > 0 {
			m := taskCfg.MaxTokens
			out.MaxTokens = &m
		}
	}

	// 强制流式优先于调用方。
	if model.ForceStream {
		out.Stream = true
	}
	return &out
}

// orderModels 按选择策略决定尝试顺序。
func (c *Client) orderModels(models []string, strategy string) []string {
	out := append([]string(nil), models...)

	switch strategy {
	case "random":
		rand.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	case "balance":
		c.mu.RLock()
		counts := make(map[string]int, len(out))
		for _, m := range out {
			counts[m] = c.counters[m]
		}
		c.mu.RUnlock()

		// 按调用次数升序排序，次数相同则保持配置顺序。
		stableSortBy(out, func(a, b string) bool { return counts[a] < counts[b] })
	case "sequential":
		// 保持配置顺序。
	}
	return out
}

func (c *Client) bumpCounter(model string) {
	c.mu.Lock()
	c.counters[model]++
	c.mu.Unlock()
}

// stableSortBy 是插入排序，模型数量很小，够用且保持稳定性。
func stableSortBy(s []string, less func(a, b string) bool) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && less(s[j], s[j-1]); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// minPositive 返回两个时长里较小且为正的那个，都为 0 时返回 0。
func minPositive(a, b time.Duration) time.Duration {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}
