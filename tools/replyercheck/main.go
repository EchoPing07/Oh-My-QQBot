// Command replyercheck 用真实模型反复跑 Replyer，统计「指令回声」泄漏率。
//
// 为什么需要它：
//
//	Replyer 曾经把 Planner 的提醒（"不调用工具……"）当成待回应的指令照抄，
//	产出「不调用工具，直接回就行。\n\n<正文>」并真的发进群里。
//	起手排查时靠肉眼翻群聊很难量化，而这个问题恰好是概率性的
//	（同一份历史 10 次里泄漏 4~5 次），跑一次说明不了修复有效。
//
// 与 livecheck 的分工：
//
//	livecheck   跑完整链路（Planner → 工具 → Replyer → 发送），看整体
//	replyercheck 只跑 Replyer 这一段，N 次重复，给出泄漏率
//
// 它复用生产的 agent.PromptBuilder，而不是手拼提示词 —— 手拼的提示词
// 与线上不一致时，测出来的结论不可信（这是 AGENTS.md 第 34 条的教训）。
//
// 用法：
//
//	go run ./tools/replyercheck -config config.toml -turns 10
//	go run ./tools/replyercheck -config config.toml -historic   # 连脏历史一起测
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/agent"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/chat"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/llm"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/logx"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/prompt"
)

// echoPrefixes 是判定「这次泄漏了」的开头集合。
//
// 刻意比 chat.instructionEchoPrefixes 宽一点：这里要统计「模型倾向于
// 复述指令」，哪怕它把句子改写成「用 reply 回复：…」也算。
var echoPrefixes = []string{
	"不调用工具", "不用调用工具", "不需要调用工具", "不用工具",
	"用reply", "用 reply", "reply回复", "使用reply", "使用 reply",
	"不用搜", "无需检索", "直接回就行", "直接回。", "接一句就行",
}

// reference 是一条真实的 reply_reference 形态（取自线上 reasoning_records）。
const reference = "群鱼提醒火别太旺，延续锅巴记账的玩笑话题。轻松接一句，把火候和锅巴联系起来。"

func main() {
	configPath := flag.String("config", "config.toml", "配置文件")
	turns := flag.Int("turns", 10, "每个场景重复多少次")
	historic := flag.Bool("historic", true, "历史里是否混入「已污染的机器人自发言」")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载配置失败: %v\n", err)
		os.Exit(1)
	}
	if err := logx.Init(logx.Options{Level: logx.LevelError, Console: false}); err != nil {
		fmt.Fprintf(os.Stderr, "初始化日志失败: %v\n", err)
		os.Exit(1)
	}

	cfgStore := config.Wrap(cfg)
	pm, err := prompt.New("", "zh-CN")
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载提示词失败: %v\n", err)
		os.Exit(1)
	}

	builder := agent.NewPromptBuilder(cfgStore, pm)
	client := llm.New(cfgStore)

	// 只看 Replyer 用哪个模型，避免 Planner 的模型配置干扰。
	models := cfg.LLM.Tasks.Replyer.Models
	fmt.Printf("Replyer 模型: %v\n", models)
	fmt.Printf("历史里混入已污染的自发言: %v\n\n", *historic)

	history, botReplies := buildHistory(cfgStore.Get(), *historic)

	total, leaked := 0, 0
	echoSamples := make([]string, 0, *turns)

	for i := 0; i < *turns; i++ {
		msgs, _, err := builder.BuildReplyerMessages(agent.ReplyerPromptInput{
			IsGroup:     true,
			History:     history,
			BotReplies:  botReplies,
			Reference:   reference,
			Style:       "简短表达",
			BaseStyle:   cfgStore.Get().Personality.ReplyStyle,
			Now:         time.Now(),
			TargetMsgID: history[len(history)-1].MessageID,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "组装请求失败: %v\n", err)
			os.Exit(1)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		resp, err := client.Generate(ctx, config.TaskReplyer, &llm.Request{Messages: msgs})
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "第 %d 次调用失败: %v\n", i+1, err)
			continue
		}

		total++
		raw := strings.TrimSpace(resp.Content)
		cleaned := cleanEcho(raw)
		leak := looksLikeEcho(raw)
		if leak {
			leaked++
			if len(echoSamples) < 3 {
				echoSamples = append(echoSamples, raw)
			}
		}

		fmt.Printf("[%2d] 原始=%q\n", i+1, clip(raw))
		if leak {
			fmt.Printf("     ↑ 检测到指令回声，CleanReplyText 后=%q\n", clip(cleaned))
		}
	}

	fmt.Printf("\n===== 结果 =====\n")
	fmt.Printf("模型原始输出带指令回声: %d/%d\n", leaked, total)
	if total > 0 {
		fmt.Printf("泄漏率: %.0f%%\n", float64(leaked)*100/float64(total))
	}
	if leaked > 0 {
		fmt.Println("\n样例（原始输出）:")
		for _, s := range echoSamples {
			fmt.Printf("  %q\n", clip(s))
		}
		fmt.Println("\n注意：代码侧已改用正确的 Replyer 尾注（不含工具字样），")
		fmt.Println("      这里的泄漏只要能被 chat.CleanReplyText 清掉就不会发进群。")
	}
}

// buildHistory 构造一段真实形态的群聊历史。
//
// polluted=true 时混入「已被污染的机器人自发言」—— 这正是线上恶化的路径：
// 一旦有一条带前缀的回复入库，下一轮它就以 is_self_message 出现在上下文里，
// 模型照着模仿的概率显著上升。这个开关就是为了复现那条正反馈。
func buildHistory(cfg *config.Config, polluted bool) ([]*domain.Message, []agent.BotReply) {
	now := time.Now()
	mk := func(n int, id, user, text string) *domain.Message {
		return &domain.Message{
			MessageID: id, UserName: user, PlainText: text,
			ReceivedAt: now.Add(time.Duration(n) * time.Second),
			IsGroup:    true, SessionID: "replyercheck",
		}
	}

	history := []*domain.Message{
		mk(1, "m-1", "群鱼", "记米粒上，这活儿我熟"),
		mk(2, "m-2", "Momo", "跟电费并一行，年底一汇总正好一起清算"),
		mk(3, "m-3", "群鱼", "火别太旺啊"),
		mk(4, "m-4", "群鱼", "脆的那层糊了可就没法对账了喵"),
	}

	replies := []agent.BotReply{
		{Text: "行，锅都快烧好了，账本慢慢摊", At: now.Add(2500 * time.Millisecond)},
	}
	if polluted {
		// 线上真实截到的那两条形态。
		replies = append(replies,
			agent.BotReply{Text: "不调用工具，直接回就行。\n\n锅里锅外全是账，年底一数，我这米饭都吃出利息了", At: now.Add(3 * time.Second)},
			agent.BotReply{Text: "不调用工具，直接回。\n\n一粒顶三行，那我这账本得煮一锅才够写", At: now.Add(3500 * time.Millisecond)},
		)
	}
	return history, replies
}

// looksLikeEcho 判断模型原始输出是否以指令回声开头。
func looksLikeEcho(s string) bool {
	head := strings.TrimSpace(s)
	for _, p := range echoPrefixes {
		if strings.HasPrefix(head, p) {
			return true
		}
	}
	return false
}

// cleanEcho 复刻生产路径的清理，用来看「最终会发出去什么」。
//
// 直接调 chat.CleanReplyText 而不是在这里重写一份 —— 重写的那份一定会漂。
func cleanEcho(s string) string {
	return chat.CleanReplyText(s)
}

// clip 截断用于打印，按 rune 不劈汉字。
func clip(s string) string {
	r := []rune(strings.ReplaceAll(s, "\n", "\\n"))
	if len(r) <= 90 {
		return string(r)
	}
	return string(r[:90]) + "…"
}
