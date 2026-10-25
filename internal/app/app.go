// Package app 负责组装所有组件并管理生命周期。
package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/chat"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/embedding"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/emoji"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/learning"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/llm"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/logx"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/memory"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/prompt"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/qq"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/router"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/store"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/tool"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/web"
)

// App 是整个程序。
type App struct {
	cfg      *config.Store
	configOK bool

	db        *store.DB
	llm       *llm.Client
	prompts   *prompt.Manager
	adapter   *qq.Adapter
	hub       *router.Hub
	web       *web.Server
	usage     *usageRecorder
	emoji     *emoji.Manager
	learner   *learning.Learner
	embedding *embedding.Client

	ctx    context.Context
	cancel context.CancelFunc
	log    *logx.Logger

	// wg 跟踪退出时需要等待的后台协程（保留期清理等）。
	wg sync.WaitGroup
}

// New 创建应用。
func New(cfg *config.Store) (*App, error) {
	ctx, cancel := context.WithCancel(context.Background())

	a := &App{
		cfg:    cfg,
		ctx:    ctx,
		cancel: cancel,
		log:    logx.Get("app"),
	}
	return a, nil
}

// Run 启动并阻塞直到收到退出信号。
func (a *App) Run() error {
	defer a.cancel()

	if err := a.init(); err != nil {
		a.shutdown()
		return err
	}

	// 启动 WebUI。
	if a.cfg.Get().Web.Enabled {
		if err := a.web.Start(); err != nil {
			a.log.Error("WebUI 启动失败", "err", err)
			// WebUI 失败不阻塞主程序运行。
		}
	}

	// 启动 QQ 适配器（阻塞）。
	adapterErr := make(chan error, 1)
	go func() {
		adapterErr <- a.adapter.Run(a.ctx, time.Duration(a.cfg.Get().QQ.ReconnectDelaySec)*time.Second)
	}()

	// 等待退出信号或网关错误。
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-sigCh:
		a.log.Info("收到退出信号，开始关闭")
	case err := <-adapterErr:
		if err != nil && !errors.Is(err, context.Canceled) {
			a.log.Error("QQ 适配器异常退出", "err", err)
			a.cancel()
			a.shutdown()
			return err
		}
	case <-a.ctx.Done():
	}

	a.cancel()
	a.shutdown()
	return nil
}

// init 初始化所有组件。
//
// 顺序很重要：日志 → 配置 → 数据库 → 提示词 → LLM → 工具 → 适配器 → 路由。
func (a *App) init() error {
	// 日志已在外层初始化，这里只确认可用。
	logx.Info("启动中", "nickname", a.cfg.Get().Bot.Nickname)

	// 数据库。
	db, err := store.Open(a.ctx, a.cfg.Get().Database.Path)
	if err != nil {
		return fmt.Errorf("初始化数据库失败: %w", err)
	}
	a.db = db
	a.log.Info("数据库已就绪", "path", a.cfg.Get().Database.Path)

	// 提示词。
	prompts, err := prompt.New("prompts", "zh-CN")
	if err != nil {
		return fmt.Errorf("加载提示词失败: %w", err)
	}
	a.prompts = prompts
	a.log.Info("提示词已加载", "count", len(prompts.Names()))

	// 把同一个 Manager 交给各业务包。
	//
	// 摘要、画像、表达学习、黑话推断、表情包打标签这几处的提示词
	// 以前硬编码在各自包里，用户能改的只有 planner / replyer 两份。
	// 现在全部走同一个 Manager，WebUI 上改一份就真的生效 ——
	// 这里漏接一个包，对应功能就会静默地继续用内嵌默认文案。
	memory.SetPromptManager(prompts)
	learning.SetPromptManager(prompts)
	emoji.SetPromptManager(prompts)

	// LLM。
	a.llm = llm.New(a.cfg)

	// 模型可以后补：先启动验证 QQ 连通性，收到消息时才会真的需要模型。
	// 但这里要明确告诉用户「现在还不能回复」，避免以为已经配好了。
	if models := a.cfg.Get().ResolveModels(config.TaskPlanner); len(models) == 0 {
		a.log.Warn("尚未配置 planner 模型，机器人可以连上 QQ 但无法回复",
			"提示", "在 config.toml 的 [llm] 里填 providers/models，并在 [llm.tasks.planner] 的 models 里引用")
	}

	// 凭据缺失不阻断启动，但必须大声告知 —— 否则现象是「程序在跑但完全不回消息」，
	// 极难排查。这一点是踩过的：Validate 里硬性拒绝会让 LoadOrCreate
	// 写出的默认配置第二次启动就崩，而静默放过又让人找不到原因。
	if missing := a.cfg.Get().MissingCredentials(); len(missing) > 0 {
		a.log.Warn("QQ 凭据不完整，机器人将无法连接 QQ",
			"missing", missing,
			"提示", "在 config.toml 的 [qq] 里填 app_id 与 app_secret，或用 OMQ_QQ_APP_ID / OMQ_QQ_APP_SECRET 环境变量",
		)
	}

	// 用量统计。
	a.usage = newUsageRecorder(a.db, a.cfg)
	a.llm.OnComplete(a.usage.handle)

	// 向量检索客户端。
	//
	// 未配置 [llm.tasks.embedding] 时为「禁用」状态，
	// 记忆检索会自动降级为关键词检索 —— 不会因为没配向量而整个功能不可用。
	a.embedding = embedding.New(a.cfg)
	if a.embedding.Enabled() {
		models := a.cfg.Get().ResolveModels(config.TaskEmbedding)
		a.log.Info("向量检索已启用", "model", models[0])
	} else {
		a.log.Info("未配置 embedding 模型，记忆检索将使用关键词模式",
			"提示", "在 [llm.tasks.embedding] 里填 models 可启用语义检索")
	}

	// 表情包管理器。
	// 数据库或模型不可用时它内部会自行降级（收藏照做、打标签跳过）。
	if a.cfg.Get().Emoji.Enabled {
		a.emoji = emoji.New(a.cfg, a.db, a.llm)
	}

	// 工具注册。
	//
	// 关键约束：提示词里描述的每个工具都必须真的注册过。
	// 曾经照抄了 query_memory 的说明却没实现，模型调用后失败、白烧一轮。
	// 反过来也要注意：功能关闭时不要注册，否则模型会尝试用不存在的库。
	registry := tool.NewRegistry()
	registry.MustRegister(tool.NewReplyToolFunc(func() bool { return a.cfg.Get().Chat.EnableReplyAt }))
	registry.MustRegister(tool.NewWaitTool())
	registry.MustRegister(tool.NewStopTool())
	if a.cfg.Get().Memory.Enabled {
		registry.MustRegister(tool.NewQueryMemoryTool())
	}
	if a.cfg.Get().Emoji.Enabled {
		registry.MustRegister(tool.NewSendEmojiTool())
		registry.MustRegister(tool.NewShowEmojiListTool())
	}
	a.log.Info("工具已注册", "tools", registry.Names())

	// 过滤器。
	filter, err := chat.NewFilter(a.cfg.Get().Message.BanWords, a.cfg.Get().Message.BanRegexes, a.cfg.Get().Message.GroupWhitelist)
	if err != nil {
		return fmt.Errorf("初始化过滤器失败: %w", err)
	}

	// QQ 适配器。
	client := qq.NewClient(a.cfg.Get().QQ.APIBaseURL, a.cfg.Get().QQ.AppID, a.cfg.Get().QQ.AppSecret)
	a.adapter = qq.NewAdapter(client, a.cfg.Get().QQ.Intents)

	// 机器人自身 ID 过滤。
	filter.SetBotIDs([]string{a.cfg.Get().Bot.AccountID, a.cfg.Get().QQ.UnifiedAccountID})

	// 配置热更新：过滤器自己缓存了编译好的正则与白名单集合，
	// 换快照不会自动让它重建，必须在这里推一把。
	// 不接的话表现是「WebUI 上改了屏蔽词，机器人照样回」——
	// 这种「改完没反应」比没有这个配置更糟（AGENTS.md 第 50 条）。
	a.cfg.OnSave(func(_, next *config.Config) {
		filter.Update(next.Message.BanWords, next.Message.BanRegexes, next.Message.GroupWhitelist)
		filter.SetBotIDs([]string{next.Bot.AccountID, next.QQ.UnifiedAccountID})
	})

	// 路由器。
	a.hub = router.NewHub(a.ctx, router.HubOptions{
		Config:    a.cfg,
		Store:     a.db,
		LLM:       a.llm,
		Prompts:   a.prompts,
		Registry:  registry,
		Sender:    a.adapter,
		Filter:    filter,
		Emoji:     emojiCollectorOrNil(a.emoji),
		Embedding: a.embedding,
	})

	// 学习器：表达与黑话。
	// 两个开关都关时它不会发起任何模型调用（Start 内部会立刻返回）。
	if a.cfg.Get().Learning.ExpressionEnabled || a.cfg.Get().Learning.JargonEnabled {
		a.learner = learning.New(learning.Options{
			Config: a.cfg,
			LLM:    a.llm,
			Store:  a.db,
		})
		a.learner.Start(a.ctx)
		a.log.Info("学习器已启动",
			"expression", a.cfg.Get().Learning.ExpressionEnabled,
			"jargon", a.cfg.Get().Learning.JargonEnabled,
			"interval_min", a.cfg.Get().Learning.IntervalMinutes,
		)
	}

	// 入站接线。
	a.adapter.OnMessage(func(msg *domain.Message, scene, target, accountID string) {
		a.hub.Dispatch(msg, scene, target, accountID)
	})

	// 历史记忆向量回填。
	//
	// 两个触发场景：首次启用向量检索、换了 embedding 模型。
	// 放后台跑：回填可能要处理几百条，不能拖慢启动。
	if a.embedding.Enabled() && a.cfg.Get().Memory.Enabled {
		go a.backfillEmbeddings()
	}

	// 管理命令：/assign_id 这类绑定命令在进聊天流之前处理。
	a.adapter.OnCommand(newCommandHandler(a.cfg, a.db).Handle)

	// 保留期清理。老数据不清会拖慢恢复上下文与检索，
	// 而且 reasoning_records 涨得比消息快得多（每轮一条，单条可达 64KB）。
	a.startRetentionSweeper()

	// 机器人身份就绪后，把它加进过滤名单，避免自问自答。
	go a.watchBotIdentity(filter)

	// WebUI。
	if a.cfg.Get().Web.Enabled {
		a.web = web.NewServer(web.Options{
			Config:  a.cfg,
			Store:   a.db,
			Hub:     a.hub,
			Prompts: a.prompts,
			LLM:     a.llm,
			Restart: a.restarter(),
		})
		logx.SetBroadcaster(a.web)
		// 消息落库后推事件给 WebUI（前端实时刷新）。
		// hub 先建、web 后建，所以建好后再注入，注入方式与上面一行同款。
		a.hub.SetNotifier(a.web)
	}

	a.log.Info("初始化完成")
	return nil
}

// restarter 构造「重启自己」的实现。
//
// 三个回调的分工必须说清楚，否则重启会以各种方式失败：
//   - Release：只放掉 WebUI 端口。必须在起新进程之前，否则新进程
//     bind 失败，而 app.go 对 WebUI 启动失败只记警告 —— 结果是
//     机器人活着但网页永远打不开。
//   - Exit：走正常的 shutdown + os.Exit，让网关断开、库关闭。
//   - Args/Dir：完整保留启动方式。丢了 -config，新进程会去读
//     默认的 config.toml（可能在另一个目录），表现是「重启后配置全变默认」。
func (a *App) restarter() web.Restarter {
	dir, err := os.Getwd()
	if err != nil {
		a.log.Warn("取工作目录失败，重启功能将不可用", "err", err)
		return nil
	}
	return &web.ExecRestarter{
		Args: os.Args[1:],
		Dir:  dir,
		Release: func() {
			if a.web != nil {
				a.web.ReleaseListener()
			}
		},
		Exit: func() {
			a.cancel()
			a.shutdown()
			os.Exit(0)
		},
		Log: func(msg string, fields ...any) {
			a.log.Info(msg, fields...)
		},
	}
}

// shutdown 优雅关闭。
//
// 顺序与 MaiBot 一致：先断消息入口，再停会话，再关资源。
func (a *App) shutdown() {
	a.log.Info("开始关闭")

	// 1. 停适配器，不再收新消息。
	if a.adapter != nil {
		a.adapter.Stop()
	}

	// 2. 停所有会话。
	if a.hub != nil {
		a.hub.Close()
	}

	// 3. 停 WebUI。
	if a.web != nil {
		a.web.Stop()
	}

	// 4. 等后台任务收尾（表情包打标签、学习任务）。
	if a.emoji != nil {
		a.emoji.Close()
	}

	// 4.5 等后台清理协程收尾，再关库
	// （否则它会拿着已关闭的 *sql.DB 报错刷屏）。
	a.wg.Wait()

	// 5. 关数据库。
	if a.db != nil {
		if err := a.db.Close(); err != nil {
			a.log.Warn("关闭数据库失败", "err", err)
		}
	}

	// 先说完「已关闭」再关日志：logx.Close 之后文件出口是关闭态，
	// 反过来的话这条 Info 不会落盘，旧实现还会把文件重新建出来。
	a.log.Info("已关闭")
	logx.Close()
}

// emojiCollectorOrNil 把具体类型转成接口，nil 时返回真正的 nil 接口值。
//
// 直接写 Emoji: a.emoji 会得到一个「有具体类型的 nil 接口」，
// 在 Hub 里 `if h.emoji == nil` 判断不出来 —— 这是 Go 接口最经典的坑。
func emojiCollectorOrNil(m *emoji.Manager) router.EmojiCollector {
	if m == nil {
		return nil
	}
	return m
}

// backfillEmbeddings 给历史记忆补向量。
//
// 分批推进直到没有待处理的：首次启用时库里可能有几百条旧记忆，
// 一次性全塞进一个请求会被服务端拒绝，也容易超时。
func (a *App) backfillEmbeddings() {
	summarizer := memory.NewSummarizer(a.cfg, a.llm, a.db, a.embedding)

	ctx, cancel := context.WithTimeout(a.ctx, 10*time.Minute)
	defer cancel()

	total := 0
	for {
		n, err := summarizer.Backfill(ctx, 50)
		if err != nil {
			a.log.Warn("记忆向量回填中断", "done", total, "err", err)
			return
		}
		total += n
		if n == 0 {
			break
		}
		// 防止异常情况下无限循环。
		if total > 10000 {
			a.log.Warn("记忆向量回填已达上限，剩余部分下次启动继续", "done", total)
			break
		}
	}

	if total > 0 {
		a.log.Info("记忆向量回填全部完成", "count", total)
	}
}
