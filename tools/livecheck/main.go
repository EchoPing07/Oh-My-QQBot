// Command livecheck 用真实 LLM 端点跑一遍完整链路，
// 不需要人手动发 QQ 消息。
//
// 用法：
//
//	go run ./tools/livecheck
//
// 它会：
//  1. 读 config.toml 的模型配置
//  2. 起一个模拟 QQ 发送器
//  3. 注入几条「模拟群聊消息」
//  4. 打印每一步的耗时、模型原始响应、最终发出的内容
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/agent"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/chat"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/llm"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/logx"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/prompt"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/router"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/store"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/tool"
)

func main() {
	configPath := flag.String("config", "config.toml", "配置文件")
	turns := flag.Int("turns", 3, "模拟几轮对话")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载配置失败: %v\n", err)
		os.Exit(1)
	}

	// 日志开着但只输出 WARN，避免刷屏；关键信息我们自己打印。
	if err := logx.Init(logx.Options{Level: logx.LevelWarn, Console: true, Color: true}); err != nil {
		fmt.Fprintf(os.Stderr, "初始化日志失败: %v\n", err)
		os.Exit(1)
	}

	// 用独立数据库，不污染正式数据。
	dbPath := filepath.Join("data", "livecheck.db")
	_ = os.Remove(dbPath)

	ctx := context.Background()
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "打开数据库失败: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	prompts, err := prompt.New("", "zh-CN")
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载提示词失败: %v\n", err)
		os.Exit(1)
	}

	cfgStore := config.Wrap(cfg)
	client := llm.New(cfgStore)

	// 落库用量，让最后的汇总有意义。
	client.OnComplete(func(task string, resp *llm.Response, err error) {
		if err != nil || resp == nil {
			return
		}
		var cost float64
		if model, mErr := cfg.Model(resp.Model); mErr == nil {
			cost = llm.CostCalculator(
				resp.Usage.PromptTokens, resp.Usage.CompletionTokens,
				resp.Usage.CachedTokens, model.PriceIn, model.PriceOut, 0)
		}
		_ = db.Usage().Record(context.Background(), &store.UsageRecord{
			Task:             task,
			Model:            resp.Model,
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
			Cost:             cost,
			DurationMS:       resp.Duration.Milliseconds(),
			CreatedAt:        time.Now(),
		})
	})

	// 打印每次模型调用的耗时与摘要。
	client.OnComplete(func(task string, resp *llm.Response, err error) {
		if err != nil {
			fmt.Printf("  \033[31m✗ %s 失败\033[0m %v\n", task, err)
			return
		}
		reasonLen := len([]rune(resp.Reasoning))
		contentLen := len([]rune(resp.Content))
		toolNames := []string{}
		for _, tc := range resp.ToolCalls {
			toolNames = append(toolNames, tc.Function.Name)
		}
		tools := "-"
		if len(toolNames) > 0 {
			tools = strings.Join(toolNames, ",")
		}
		fmt.Printf("  \033[36m▸ %-8s\033[0m %6.1fs  in=%d out=%d  reason=%d content=%d  tool=%s\n",
			task, resp.Duration.Seconds(),
			resp.Usage.PromptTokens, resp.Usage.CompletionTokens,
			reasonLen, contentLen, tools)
	})

	registry := tool.NewRegistry()
	registry.MustRegister(tool.NewReplyToolFunc(func() bool { return cfgStore.Get().Chat.EnableReplyAt }))
	registry.MustRegister(tool.NewWaitTool())
	registry.MustRegister(tool.NewStopTool())

	filter, err := chat.NewFilter(nil, nil, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "创建过滤器失败: %v\n", err)
		os.Exit(1)
	}
	filter.SetBotIDs([]string{"bot-openid"})

	sender := &printSender{}

	cfg.Web.Enabled = false
	cfg.Chat.Timing.DebounceMode = "fast" // 检查用，别等太久

	ctx2, cancel := context.WithCancel(ctx)
	defer cancel()

	hub := router.NewHub(ctx2, router.HubOptions{
		Config:   cfgStore,
		Store:    db,
		LLM:      client,
		Prompts:  prompts,
		Registry: registry,
		Sender:   sender,
		Filter:   filter,
	})
	defer hub.Close()

	fmt.Printf("\n\033[1m配置\033[0m\n")
	fmt.Printf("  planner 模型: %v\n", cfg.ResolveModels(config.TaskPlanner))
	fmt.Printf("  replyer 模型: %v\n", cfg.ResolveModels(config.TaskReplyer))
	fmt.Printf("  端点: %s\n", cfg.LLM.Providers[0].BaseURL)
	fmt.Printf("\n\033[1m模拟 %d 轮群聊对话\033[0m\n\n", *turns)

	userLines := []string{
		"木木在吗，今天好累啊",
		"刚开完会，脑子都木了",
		"你说我该不该请个假",
		"算了不说了，你随便说点什么吧",
		"嗯嗯",
	}

	for i := 0; i < *turns; i++ {
		text := userLines[i%len(userLines)]

		fmt.Printf("\033[33m[用户]\033[0m %s\n", text)

		msg := &domain.Message{
			ID:         fmt.Sprintf("live-%d-%d", time.Now().UnixNano(), i),
			Platform:   "qq",
			MessageID:  fmt.Sprintf("msg-%d-%d", time.Now().UnixNano(), i),
			UserID:     "user-openid-xiaoming",
			UserName:   "小明",
			GroupID:    "group-live-1",
			GroupName:  "测试群",
			IsGroup:    true,
			IsAt:       i == 0, // 第一条 @，其余靠门控
			Segments:   buildSegments(text, i == 0),
			ReceivedAt: time.Now(),
		}
		msg.PlainText = domain.BuildPlainText(msg.Segments)

		before := len(sender.texts())
		hub.Dispatch(msg, domain.ScopeGroup, "group-live-1", "bot-openid")

		// 等待回复或超时。
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			if len(sender.texts()) > before {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}

		got := sender.texts()
		if len(got) > before {
			for _, line := range got[before:] {
				fmt.Printf("\033[32m[木木]\033[0m %s\n", line)
			}
		} else {
			fmt.Printf("\033[90m[木木] (没说话)\033[0m\n")
		}
		fmt.Println()

		// 轮次之间留点间隔，让防抖和门控状态自然推进。
		time.Sleep(1500 * time.Millisecond)
	}

	// 汇总。
	fmt.Printf("\n\033[1m汇总\033[0m\n")
	fmt.Printf("  共发出 %d 条消息\n", len(sender.texts()))
	for i, line := range sender.texts() {
		fmt.Printf("    %d. %s\n", i+1, line)
	}

	// 用量。
	sum, _ := db.Usage().Summary(ctx, time.Now().Add(-time.Hour))
	fmt.Printf("  模型调用 %d 次，token %d，花费 %.4f\n", sum.Calls, sum.TotalTokens, sum.Cost)
}

func buildSegments(text string, at bool) []domain.Segment {
	segs := []domain.Segment{domain.Text(text)}
	if at {
		segs = append([]domain.Segment{domain.At("bot-openid", "木木")}, segs...)
	}
	return segs
}

// printSender 把发出的消息打到终端。
type printSender struct {
	mu  sync.Mutex
	all []string
}

func (p *printSender) SendText(ctx context.Context, scene, target, text, passiveTo string, quote domain.Reference) (domain.SendResult, error) {
	return p.SendSegments(ctx, scene, target, []domain.Segment{domain.Text(text)}, passiveTo, quote)
}

// SendSegments 把段拼成可读文本打印，体检时能看出 @ 与图片有没有真的带上。
func (p *printSender) SendSegments(_ context.Context, _, _ string, segments []domain.Segment, _ string, _ domain.Reference) (domain.SendResult, error) {
	var b strings.Builder
	for _, seg := range segments {
		switch seg.Type {
		case domain.SegmentText:
			b.WriteString(seg.Text)
		case domain.SegmentAt:
			b.WriteString("@" + seg.UserName + " ")
		case domain.SegmentImage, domain.SegmentEmoji:
			b.WriteString("[图片]")
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.all = append(p.all, b.String())
	idx := len(p.all)
	return domain.SendResult{
		MessageIDs: []string{fmt.Sprintf("sent-%d", idx)},
		RefIdxs:    []string{fmt.Sprintf("REFIDX_sent_%d", idx)},
	}, nil
}

func (p *printSender) texts() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.all...)
}

var _ agent.Sender = (*printSender)(nil)
