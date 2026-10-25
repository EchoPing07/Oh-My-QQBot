package mock_test

// 「WebUI 改配置之后，下一轮请求真的用新值」的端到端验证。
//
// 这是配置编辑功能唯一有意义的验收标准：
// 界面上点保存成功、配置文件也写了，但机器人还是按老配置跑，
// 那这个功能等于没做 —— 而且用户完全看不出来（没有任何报错）。
//
// 走 harness 而不是手写 Runtime：harness 与生产启动路径一致
// （见 AGENTS.md 第 34 条），用它才能证明「真实链路」生效。

import (
	"strings"
	"testing"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/mock"
)

// TestConfigUpdateAffectsNextTurn 验证改行为风格后下一轮 Planner 请求就变了。
//
// 选 behavior_style 而不是 personality：Planner 的 system prompt 里
// 只有 behavior_style（见 internal/prompt/embedded/zh-CN/planner.prompt），
// personality 是 Replyer 那一侧才用的。断言写错字段会得到
// 「看起来像配置没生效」的假失败 —— 这个坑当场就踩到了。
func TestConfigUpdateAffectsNextTurn(t *testing.T) {
	h := mock.NewHarness(t, mock.HarnessOptions{
		SetupConfig: func(cfg *config.Config) {
			cfg.Bot.Nickname = "木木"
			cfg.Personality.BehaviorStyle = "第一版风格：喜欢猫"
		},
		Planner: func(_ *mock.RecordedRequest, _ int) *mock.Reply {
			return mock.ToolReply("stop", `{}`)
		},
	})

	// 第一轮：确认提示词里是第一版人格。
	h.SendMessage("第一句话", true)
	if !h.WaitForLLMCalls(1, 10*time.Second) {
		t.Fatal("第一轮没有触发模型调用")
	}
	first := h.PlannerRequests()
	if len(first) == 0 {
		t.Fatal("没有记录到 Planner 请求")
	}
	if !strings.Contains(first[0].SystemText(), "第一版风格") {
		t.Fatalf("第一轮的系统提示里没有初始风格，实际：%s", truncate(first[0].SystemText(), 400))
	}

	// 改配置（等价于 WebUI 上点保存）。
	if err := h.Store.Update(func(c *config.Config) error {
		c.Personality.BehaviorStyle = "第二版风格：喜欢狗"
		return nil
	}); err != nil {
		t.Fatalf("改配置失败: %v", err)
	}

	// 第二轮：提示词必须换成新人格。
	h.SendMessage("第二句话", true)
	if !h.WaitForLLMCalls(2, 10*time.Second) {
		t.Fatal("第二轮没有触发模型调用")
	}
	all := h.PlannerRequests()
	latest := all[len(all)-1]
	if !strings.Contains(latest.SystemText(), "第二版风格") {
		t.Errorf("改配置后下一轮没有用新风格，实际：%s", truncate(latest.SystemText(), 400))
	}
	if strings.Contains(latest.SystemText(), "第一版风格") {
		t.Error("改配置后仍在用旧风格")
	}
}

// TestConfigUpdateAffectsMaxContextSize 验证改上下文条数后下一轮就按新值取。
//
// 这条比人格更「结构性」：上下文条数是回复时真正取多少条历史，
// 改错了不会报错，只会表现为「机器人好像记不住更早的话」。
func TestConfigUpdateAffectsMaxContextSize(t *testing.T) {
	h := mock.NewHarness(t, mock.HarnessOptions{
		SetupConfig: func(cfg *config.Config) {
			cfg.Chat.MaxContextSize = 40
		},
		Planner: func(_ *mock.RecordedRequest, _ int) *mock.Reply {
			return mock.ToolReply("stop", `{}`)
		},
	})

	if err := h.Store.Update(func(c *config.Config) error {
		c.Chat.MaxContextSize = 2
		return nil
	}); err != nil {
		t.Fatalf("改配置失败: %v", err)
	}

	h.SendMessage("你好", true)
	if !h.WaitForLLMCalls(1, 10*time.Second) {
		t.Fatal("没有触发模型调用")
	}
	if got := h.Store.Get().Chat.MaxContextSize; got != 2 {
		t.Errorf("配置里的上下文条数 = %d，期望 2", got)
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// TestConfigUpdateAffectsReplyAt 验证改 enable_reply_at 后工具 schema 立刻变。
//
// 这个字段早先是「启动时读一次、塞进 ReplyTool 的 bool」，而 WebUI 上
// 标的是「即时生效」—— 用户改完发现 @ 参数还是出不来，会以为是 bug。
// 现在 ReplyTool 存的是取值函数，每次取 schema 时重新求值。
func TestConfigUpdateAffectsReplyAt(t *testing.T) {
	h := mock.NewHarness(t, mock.HarnessOptions{
		SetupConfig: func(cfg *config.Config) { cfg.Chat.EnableReplyAt = false },
		Planner: func(_ *mock.RecordedRequest, _ int) *mock.Reply {
			return mock.ToolReply("stop", `{}`)
		},
	})

	// hasAttachAt 从最后一次 Planner 请求里找 reply 工具的 attach_at 参数。
	hasAttachAt := func() bool {
		reqs := h.PlannerRequests()
		if len(reqs) == 0 {
			t.Fatal("没有记录到 Planner 请求")
		}
		for _, tl := range reqs[len(reqs)-1].Tools {
			if tl.Function.Name != "reply" {
				continue
			}
			props, ok := tl.Function.Parameters["properties"].(map[string]any)
			if !ok {
				return false
			}
			_, ok = props["attach_at"]
			return ok
		}
		t.Fatal("请求里没有 reply 工具")
		return false
	}

	h.SendMessage("第一句", true)
	if !h.WaitForLLMCalls(1, 10*time.Second) {
		t.Fatal("第一轮没有触发模型调用")
	}
	if hasAttachAt() {
		t.Fatal("EnableReplyAt=false 时不该暴露 attach_at")
	}

	if err := h.Store.Update(func(c *config.Config) error {
		c.Chat.EnableReplyAt = true
		return nil
	}); err != nil {
		t.Fatalf("改配置失败: %v", err)
	}

	h.SendMessage("第二句", true)
	if !h.WaitForLLMCalls(2, 10*time.Second) {
		t.Fatal("第二轮没有触发模型调用")
	}
	if !hasAttachAt() {
		t.Error("改 enable_reply_at 之后仍没暴露 attach_at：说明 schema 被缓存在工具里")
	}
}

// TestConfigUpdateAffectsBanWords 验证改屏蔽词后过滤器立刻生效。
//
// 过滤器缓存了屏蔽词与编译好的正则，换快照不会自动重建，
// 靠 config.OnSave 钩子刷新（app.go 里接线）。
// harness 现在接了同样的钩子（audit 11 P2-21），所以这里**不再手动推**
// filter —— 这条测试同时守着钩子接线本身：哪天 app.go 或 harness
// 的 OnSave 被删掉，它就会红。
func TestConfigUpdateAffectsBanWords(t *testing.T) {
	h := mock.NewHarness(t, mock.HarnessOptions{
		SetupConfig: func(cfg *config.Config) {
			cfg.Message.BanWords = nil
		},
		Planner: func(_ *mock.RecordedRequest, _ int) *mock.Reply {
			return mock.ToolReply("stop", `{}`)
		},
	})

	// 注意：这里不调用 h.Filter.Update —— 过滤器必须由 OnSave 钩子自动刷新。
	h.Update(func(c *config.Config) {
		c.Message.BanWords = []string{"测试屏蔽词"}
	})

	h.SendMessage("这条含测试屏蔽词", true)

	// 被过滤就不该有模型调用。
	if h.WaitForLLMCalls(1, 2*time.Second) {
		t.Error("命中屏蔽词的消息仍进入了 Planner（过滤器没跟着配置更新）")
	}
}
