package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

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

// TestEndToEndReply 端到端验证：消息进 → Planner → reply 工具 → Replyer → 发出。
//
// 用 httptest 起一个假的 OpenAI 兼容服务，验证整条链路真的能跑通。
func TestEndToEndReply(t *testing.T) {
	var (
		mu       sync.Mutex
		requests []llm.Request
		callSeq  int
	)

	// 假 LLM：第一次调用返回 reply 工具调用，第二次返回回复正文。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		var req llm.Request
		_ = json.Unmarshal(body, &req)

		mu.Lock()
		requests = append(requests, req)
		callSeq++
		seq := callSeq
		mu.Unlock()

		// 判断是 Planner 还是 Replyer：Planner 请求带 tools。
		var raw map[string]any
		_ = json.Unmarshal(body, &raw)
		_, hasTools := raw["tools"]

		w.Header().Set("Content-Type", "application/json")

		if hasTools {
			// Planner：请求调用 reply 工具。
			fmt.Fprint(w, `{
				"model": "test-model",
				"choices": [{
					"index": 0,
					"message": {
						"role": "assistant",
						"content": "小明在打招呼，我该回一句。",
						"reasoning_content": "对方在打招呼，礼貌回应即可",
						"tool_calls": [{
							"id": "call_1",
							"type": "function",
							"function": {
								"name": "reply",
								"arguments": "{\"msg_id\":\"msg-1\",\"reply_reference\":\"小明在打招呼，回一句友好的话\",\"reply_style\":\"简短表达\"}"
							}
						}]
					},
					"finish_reason": "tool_calls"
				}],
				"usage": {"prompt_tokens": 100, "completion_tokens": 30, "total_tokens": 130}
			}`)
			return
		}

		// Replyer：返回正文。
		_ = seq
		fmt.Fprint(w, `{
			"model": "test-model",
			"choices": [{
				"index": 0,
				"message": {"role": "assistant", "content": "嗨，好久不见！"},
				"finish_reason": "stop"
			}],
			"usage": {"prompt_tokens": 50, "completion_tokens": 10, "total_tokens": 60}
		}`)
	}))
	defer srv.Close()

	// 组装一个最小可用的运行时环境。
	cfg := config.Default()
	cfg.Bot.Nickname = "木木"
	cfg.QQ.AppID = "test"
	cfg.QQ.AppSecret = "test"
	cfg.LLM.Providers = []config.ProviderConfig{{
		Name:    "test",
		BaseURL: srv.URL,
		APIKey:  "sk-test",
	}}
	cfg.LLM.Models = []config.ModelConfig{{
		Name:            "test-model",
		ModelIdentifier: "test-model",
		Provider:        "test",
	}}
	cfg.LLM.Tasks.Planner.Models = []string{"test-model"}
	cfg.LLM.Tasks.Replyer.Models = []string{"test-model"}
	cfg.Web.Enabled = false
	cfg.Database.Path = t.TempDir() + "/test.db"
	cfg.Log.Console = false
	cfg.Log.Dir = ""

	// 日志。
	if err := logx.Init(logx.Options{Level: logx.LevelDebug, Console: false}); err != nil {
		t.Fatalf("初始化日志失败: %v", err)
	}

	// 数据库。
	db, err := store.Open(t.Context(), cfg.Database.Path)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer db.Close()

	// 提示词。
	prompts, err := prompt.New("", "zh-CN")
	if err != nil {
		t.Fatalf("加载提示词失败: %v", err)
	}

	// LLM。
	client := llm.New(config.Wrap(cfg))
	usage := newUsageRecorder(db, config.Wrap(cfg))
	client.OnComplete(usage.handle)

	// 工具。
	registry := tool.NewRegistry()
	registry.MustRegister(tool.NewReplyTool(true))
	registry.MustRegister(tool.NewWaitTool())
	registry.MustRegister(tool.NewStopTool())

	// 假发送器，记录发出的内容。
	var sent []string
	sender := &fakeSender{onSend: func(text string) { sent = append(sent, text) }}

	// 过滤器。
	filter, err := chat.NewFilter(nil, nil, nil)
	if err != nil {
		t.Fatalf("创建过滤器失败: %v", err)
	}

	// Hub。
	hub := router.NewHub(t.Context(), router.HubOptions{
		Config:   config.Wrap(cfg),
		Store:    db,
		LLM:      client,
		Prompts:  prompts,
		Registry: registry,
		Sender:   sender,
		Filter:   filter,
	})
	defer hub.Close()

	// 投递一条 @ 机器人的消息。
	msg := &domain.Message{
		ID:         "local-1",
		Platform:   "qq",
		MessageID:  "msg-1",
		UserID:     "user-1",
		UserName:   "小明",
		GroupID:    "group-1",
		GroupName:  "测试群",
		IsGroup:    true,
		IsAt:       true,
		Segments:   []domain.Segment{domain.At("bot", "木木"), domain.Text("你好呀")},
		PlainText:  "@木木 你好呀",
		ReceivedAt: time.Now(),
	}

	hub.Dispatch(msg, domain.ScopeGroup, "group-1", "bot")

	// 等链路跑完。防抖 1 秒（@ 上限）+ 模型调用。
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(requests)
		mu.Unlock()

		if n >= 2 && len(sent) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	reqCount := len(requests)
	mu.Unlock()

	if reqCount < 2 {
		t.Fatalf("应至少调用模型 2 次（Planner + Replyer），实际 %d 次", reqCount)
	}

	if len(sent) == 0 {
		t.Fatal("应该发出发送请求")
	}

	// 验证发出去的内容是 Replyer 的正文，而不是 Planner 的思考。
	found := false
	for _, s := range sent {
		if strings.Contains(s, "好久不见") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("发出的内容应含 Replyer 正文，实际发出: %v", sent)
	}

	// 验证 Planner 请求里确实带了工具。
	mu.Lock()
	firstReq := requests[0]
	mu.Unlock()

	if len(firstReq.Tools) == 0 {
		t.Error("Planner 请求里应包含工具定义")
	}

	// 验证工具名。
	var hasReply bool
	for _, tl := range firstReq.Tools {
		if tl.Function.Name == "reply" {
			hasReply = true
		}
	}
	if !hasReply {
		t.Error("工具列表里应有 reply")
	}

	// 验证 system prompt 里带上了人格。
	var systemFound bool
	for _, m := range firstReq.Messages {
		if m.Role == llm.RoleSystem {
			systemFound = true
			if !strings.Contains(m.Content, "木木") {
				t.Error("system prompt 里应包含机器人名字")
			}
		}
	}
	if !systemFound {
		t.Error("应有 system 消息")
	}

	// 验证用量落库。
	summary, err := db.Usage().Summary(t.Context(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("查询用量失败: %v", err)
	}
	if summary.Calls < 2 {
		t.Errorf("用量记录 = %d 次，期望至少 2 次", summary.Calls)
	}
}

// fakeSender 是测试用的发送器，实现 agent.Sender。
type fakeSender struct {
	mu     sync.Mutex
	onSend func(text string)
}

func (f *fakeSender) SendText(ctx context.Context, scene, target, text, passiveTo string, quote domain.Reference) (domain.SendResult, error) {
	return f.SendSegments(ctx, scene, target, []domain.Segment{domain.Text(text)}, passiveTo, quote)
}

// SendSegments 实现 agent.Sender，把段拼成文本回传给断言。
func (f *fakeSender) SendSegments(_ context.Context, _, _ string, segments []domain.Segment, _ string, _ domain.Reference) (domain.SendResult, error) {
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
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.onSend != nil {
		f.onSend(b.String())
	}
	return domain.SendResult{
		MessageIDs: []string{"sent-1"},
		RefIdxs:    []string{"REFIDX_sent_1"},
	}, nil
}
