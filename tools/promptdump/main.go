package main

import (
	"fmt"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/agent"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/prompt"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/tool"
)

// 打印 Planner 实际收到的提示词，用于调优。
func main() {
	cfg := config.Default()
	cfg.Bot.Nickname = "木木"

	pm, err := prompt.New("", "zh-CN")
	if err != nil {
		panic(err)
	}

	registry := tool.NewRegistry()
	registry.MustRegister(tool.NewReplyTool(true))
	registry.MustRegister(tool.NewWaitTool())
	registry.MustRegister(tool.NewStopTool())

	// 按配置注册可选工具，与 app.go 保持一致。
	//
	// 这里必须同步：这个工具存在的意义就是「看 Planner 真实收到的提示词」，
	// 一旦它注册的工具比生产少，打出来的提示词就缺了对应的规则块，
	// 调提示词时会对着一个不存在的现象调。
	if cfg.Memory.Enabled {
		registry.MustRegister(tool.NewQueryMemoryTool())
	}
	if cfg.Emoji.Enabled {
		registry.MustRegister(tool.NewSendEmojiTool())
		registry.MustRegister(tool.NewShowEmojiListTool())
	}

	builder := agent.NewPromptBuilder(config.Wrap(cfg), pm)

	av := tool.Availability{IsGroup: false, Platform: "qq"}
	historyForReplyer := []*domain.Message{
		{
			MessageID:  "msg-1",
			UserName:   "小明",
			UserID:     "u1",
			PlainText:  "第三次",
			IsGroup:    false,
			ReceivedAt: time.Now(),
		},
	}
	msgs, err := builder.BuildPlannerMessages(false, historyForReplyer, nil, time.Now(), av, registry)
	if err != nil {
		panic(err)
	}

	fmt.Println("========== SYSTEM ==========")
	for _, m := range msgs {
		if m.Role == "system" {
			fmt.Println(m.Content)
		}
	}
	fmt.Println()
	fmt.Println("========== MESSAGES ==========")
	for _, m := range msgs {
		if m.Role == "system" {
			continue
		}
		fmt.Printf("[%s]\n%s\n\n", m.Role, m.Content)
	}

	fmt.Println("========== TOOLS ==========")
	for _, t := range registry.Definitions(av) {
		fmt.Printf("- %s: %s\n", t.Function.Name, t.Function.Description)
	}

	// Replyer 那一段单独打。
	//
	// 它曾经是 bug 现场：末尾块误用了 Planner 的提醒（带「不调用工具」），
	// 模型照抄成正文开头并真的发进群里。所以要有一条命令能直接看到
	// 「Replyer 到底收到什么」，而不是只能去数据库里翻 reasoning_records。
	replyerMsgs, _, err := builder.BuildReplyerMessages(agent.ReplyerPromptInput{
		IsGroup:     false,
		History:     historyForReplyer,
		Reference:   "小明在打招呼，友好回应",
		Style:       "简短表达",
		BaseStyle:   cfg.Personality.ReplyStyle,
		Now:         time.Now(),
		TargetMsgID: "msg-1",
	})
	if err != nil {
		panic(err)
	}
	fmt.Println()
	fmt.Println("========== REPLYER SYSTEM ==========")
	fmt.Println(replyerMsgs[0].Content)
	fmt.Println()
	fmt.Println("========== REPLYER MESSAGES ==========")
	for _, m := range replyerMsgs[1:] {
		fmt.Printf("[%s]\n%s\n\n", m.Role, m.Content)
	}
}
