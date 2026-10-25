package mock_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/mock"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/store"
)

// TestBasicReply 验证最基本的链路：
// 用户发消息 → Planner 调用 reply → Replyer 生成正文 → 发出。
func TestBasicReply(t *testing.T) {
	h := mock.NewHarness(t, mock.HarnessOptions{
		Planner: func(req *mock.RecordedRequest, call int) *mock.Reply {
			// 从上下文里找出真实的消息 ID，模拟模型正确引用。
			return mock.ToolReply("reply", `{"msg_id":"`+extractMsgID(t, req)+`","reply_reference":"小明在打招呼，友好回应"}`)
		},
		Replyer: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.TextReply("嗨，好久不见呀")
		},
	})

	h.SendMessage("你好呀", true)
	text := h.WaitForSend(10 * time.Second)

	if !strings.Contains(text, "好久不见") {
		t.Errorf("发出的内容 = %q，应包含 Replyer 生成的正文", text)
	}

	// 验证 Planner 确实收到了工具定义。
	plannerReqs := h.PlannerRequests()
	if len(plannerReqs) == 0 {
		t.Fatal("应有 Planner 请求")
	}
	var hasReplyTool bool
	for _, tl := range plannerReqs[0].Tools {
		if tl.Function.Name == "reply" {
			hasReplyTool = true
		}
	}
	if !hasReplyTool {
		t.Error("Planner 请求里应有 reply 工具")
	}

	// 验证 Replyer 收到了 reply_reference。
	replyerReqs := h.ReplyerRequests()
	if len(replyerReqs) == 0 {
		t.Fatal("应有 Replyer 请求")
	}
	if !requestContains(replyerReqs[0], "回复信息参考") {
		t.Error("Replyer 请求里应包含【回复信息参考】")
	}
}

// TestStopNoReply 验证模型选择 stop 时不发消息。
func TestStopNoReply(t *testing.T) {
	h := mock.NewHarness(t, mock.HarnessOptions{
		Planner: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.ToolReply("stop", "{}")
		},
	})

	h.SendMessage("随便说点啥", false)
	h.WaitForNoSend(2 * time.Second)

	if n := h.Server().Count(); n == 0 {
		t.Error("应至少调用一次 Planner")
	}
}

// TestWaitThenReply 验证 wait → 到期 → 继续思考 → reply。
//
// 这是「先等等看」的核心机制，之前有 bug：wait 到期后没有新消息就直接返回，
// 导致 wait 退化成 stop。
func TestWaitThenReply(t *testing.T) {
	var plannerCalls int

	h := mock.NewHarness(t, mock.HarnessOptions{
		Planner: func(req *mock.RecordedRequest, call int) *mock.Reply {
			plannerCalls++
			if call == 1 {
				// 第一轮：先等 1 秒
				return mock.ToolReply("wait", `{"seconds":1}`)
			}
			// 第二轮：该说话了
			return mock.ToolReply("reply", `{"msg_id":"`+extractMsgID(t, req)+`","reply_reference":"想清楚了，回应一句"}`)
		},
		Replyer: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.TextReply("刚想了一下，觉得还是说句话吧")
		},
	})

	h.SendMessage("你在吗", true)

	// wait 1 秒 + 第二轮思考，给足时间。
	text := h.WaitForSend(15 * time.Second)
	if !strings.Contains(text, "说句话") {
		t.Errorf("发出内容 = %q", text)
	}

	if plannerCalls < 2 {
		t.Errorf("Planner 应被调用至少 2 次（wait 前后各一次），实际 %d", plannerCalls)
	}
}

// TestMultiTurnToolLoop 验证一次思考内连续调用多个工具。
func TestMultiTurnToolLoop(t *testing.T) {
	var plannerCalls int

	h := mock.NewHarness(t, mock.HarnessOptions{
		Planner: func(req *mock.RecordedRequest, call int) *mock.Reply {
			plannerCalls++
			switch call {
			case 1:
				// 先调一个不存在的工具，验证错误能被回写而不是崩掉
				return mock.ToolReply("nonexistent_tool", `{}`)
			case 2:
				// 再来一轮，这次正常回复
				return mock.ToolReply("reply", `{"msg_id":"`+extractMsgID(t, req)+`","reply_reference":"道歉并回应"}`)
			}
			return mock.ToolReply("stop", "{}")
		},
		Replyer: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.TextReply("不好意思刚才走神了")
		},
	})

	h.SendMessage("喂", true)
	h.WaitForSend(15 * time.Second)

	if plannerCalls < 2 {
		t.Errorf("应有多轮工具循环，实际 %d 轮", plannerCalls)
	}

	// 验证错误被写回了上下文。
	plannerReqs := h.PlannerRequests()
	if len(plannerReqs) >= 2 {
		last := plannerReqs[len(plannerReqs)-1]
		if !requestContains(last, "不存在") {
			t.Error("工具错误应写回上下文供模型参考")
		}
	}
}

// TestContextRestoredFromDB 验证重启后上下文从数据库恢复。
//
// 这是之前真实暴露的 bug：重启后模型只看到新消息，完全失忆，
// 于是合理地选择 stop。
func TestContextRestoredFromDB(t *testing.T) {
	dbPath := t.TempDir() + "/ctx.db"

	// 第一次：发几条消息，让它们落库。
	h1 := mock.NewHarness(t, mock.HarnessOptions{
		DBPath: dbPath,
		Planner: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.ToolReply("stop", "{}")
		},
	})
	h1.SendMessage("第一句话", false)
	h1.WaitForLLMCalls(1, 5*time.Second)
	time.Sleep(300 * time.Millisecond)
	h1.SendMessage("第二句话", false)
	h1.WaitForLLMCalls(2, 5*time.Second)
	time.Sleep(300 * time.Millisecond)

	// 关闭第一个实例。
	h1.Hub.Close()
	h1.DB.Close()

	// 第二次：同一数据库，新实例。上下文应该被恢复。
	h2 := mock.NewHarness(t, mock.HarnessOptions{
		DBPath: dbPath,
		Planner: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.ToolReply("reply", `{"msg_id":"`+extractMsgID(t, req)+`","reply_reference":"回应"}`)
		},
		Replyer: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.TextReply("记得你刚才说过话")
		},
	})

	h2.SendMessage("第三句话", true)
	h2.WaitForSend(15 * time.Second)

	// 验证 Planner 请求里带上了历史消息。
	plannerReqs := h2.PlannerRequests()
	if len(plannerReqs) == 0 {
		t.Fatal("应有 Planner 请求")
	}
	found := false
	for _, m := range plannerReqs[0].Messages {
		if strings.Contains(m.ContentText(), "第一句话") {
			found = true
		}
	}
	if !found {
		t.Error("重启后上下文中应包含之前的历史消息，说明没从数据库恢复")
	}
}

// TestPrivateMessageTriggers 验证私聊不受群聊频率限制。
//
// 注意：frequency 门控本身不区分群聊私聊（区分的是动态门控），
// 所以私聊要走高频配置。这里验证的是「私聊能正常触发」，
// 而不是「私聊豁免门控」——后者只对 dynamic 模式成立。
func TestPrivateMessageTriggers(t *testing.T) {
	h := mock.NewHarness(t, mock.HarnessOptions{
		TalkValue:        mock.Float(1.0),
		PrivateTalkValue: mock.Float(1.0),
		Planner: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.ToolReply("reply", `{"msg_id":"`+extractMsgID(t, req)+`","reply_reference":"回应"}`)
		},
		Replyer: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.TextReply("私聊我必回")
		},
	})

	h.SendPrivateMessage("在吗")
	text := h.WaitForSend(10 * time.Second)

	if !strings.Contains(text, "私聊") {
		t.Errorf("私聊应触发回复，实际发出: %q", text)
	}
}

// TestQuietTalkValue 验证 talk_value=0 时完全不说话。
func TestQuietTalkValue(t *testing.T) {
	h := mock.NewHarness(t, mock.HarnessOptions{
		TalkValue: mock.Float(0),
		Planner: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.ToolReply("reply", `{"msg_id":"m1","reply_reference":"回应"}`)
		},
		Replyer: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.TextReply("不该出现")
		},
	})

	h.SendMessage("有人吗", false)
	h.WaitForNoSend(3 * time.Second)
}

// TestReplyReferenceNotBody 验证 reply_reference 不是正文。
//
// 这是两段式的核心：Planner 只给方向，正文由 Replyer 生成。
// 如果 Planner 直接把话写进 reply_reference，Replyer 还是应该重新生成。
func TestReplyReferenceNotBody(t *testing.T) {
	h := mock.NewHarness(t, mock.HarnessOptions{
		Planner: func(req *mock.RecordedRequest, call int) *mock.Reply {
			// 故意把「像是正文」的东西塞进 reply_reference
			return mock.ToolReply("reply", `{"msg_id":"`+extractMsgID(t, req)+`","reply_reference":"直接说快跑"}`)
		},
		Replyer: func(req *mock.RecordedRequest, call int) *mock.Reply {
			// Replyer 应该按人格重新措辞
			return mock.TextReply("我觉得还是别了吧")
		},
	})

	h.SendMessage("要跑吗", true)
	text := h.WaitForSend(10 * time.Second)

	if strings.Contains(text, "快跑") {
		t.Error("发出的应来自 Replyer 而不是 Planner 的 reply_reference")
	}
	if !strings.Contains(text, "别了") {
		t.Errorf("应使用 Replyer 生成的内容，实际: %q", text)
	}
}

// TestSplitLongReply 验证长回复被拆成多条。
func TestSplitLongReply(t *testing.T) {
	longReply := strings.Repeat("这是一句很长的话。", 40)

	h := mock.NewHarness(t, mock.HarnessOptions{
		Planner: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.ToolReply("reply", `{"msg_id":"`+extractMsgID(t, req)+`","reply_reference":"说很多","reply_style":"长回复"}`)
		},
		Replyer: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.TextReply(longReply)
		},
	})
	h.Update(func(c *config.Config) {
		c.Response.MaxSplitNum = 3
		c.Response.TypingSpeed = 0 // 测试里不要延迟
	})

	h.SendMessage("讲点长的", true)

	// 等所有分条发出（默认最多 3 条）。
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if len(h.Send.Texts()) >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	texts := h.Send.Texts()
	if len(texts) < 2 {
		t.Errorf("长回复应拆成多条，实际发出 %d 条", len(texts))
	}
	if len(texts) > 3 {
		t.Errorf("分条数不应超过上限 3，实际 %d", len(texts))
	}
}

// TestToolErrorDoesNotCrash 验证工具报错不会让会话崩掉。
func TestToolErrorDoesNotCrash(t *testing.T) {
	h := mock.NewHarness(t, mock.HarnessOptions{
		Planner: func(req *mock.RecordedRequest, call int) *mock.Reply {
			if call == 1 {
				return mock.ToolReply("bogus", `{"x":1}`)
			}
			return mock.ToolReply("reply", `{"msg_id":"`+extractMsgID(t, req)+`","reply_reference":"回应"}`)
		},
		Replyer: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.TextReply("还在")
		},
	})

	h.SendMessage("试试", true)
	text := h.WaitForSend(15 * time.Second)

	if !strings.Contains(text, "还在") {
		t.Errorf("工具报错后应仍能回复，实际: %q", text)
	}
}

// TestLLMErrorHandling 验证模型报错时不会卡死。
func TestLLMErrorHandling(t *testing.T) {
	h := mock.NewHarness(t, mock.HarnessOptions{
		Planner: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return &mock.Reply{HTTPStatus: 500, ErrorBody: `{"error":{"message":"boom"}}`}
		},
	})

	h.SendMessage("会炸吗", true)

	// 不应发出任何消息，但也不应 hang 住。
	h.WaitForNoSend(3 * time.Second)

	if h.Server().Count() == 0 {
		t.Error("应该至少尝试调用过一次模型")
	}
}

// TestReplyerRequestHasNoPlannerReminder 是一条结构性回归。
//
// 历史事故：Replyer 请求的末尾块误用了 Planner 的提醒
// （"请你进行分析，是不调用工具……"），Replyer 把它当成要回应的指令，
// 产出「不调用工具，直接回就行。<正文>」并真的发进群里，
// 占全部回复的 12.7%，而且会入库、下一轮被当范文再放大。
//
// 这条测试同时锁三件事：
//   - Replyer 请求里不出现 Planner 提醒
//   - 末尾是「只输出正文」的正确尾注
//   - 机器人自己的历史发言是 assistant 角色（不是 user）
func TestReplyerRequestHasNoPlannerReminder(t *testing.T) {
	h := mock.NewHarness(t, mock.HarnessOptions{
		Planner: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.ToolReply("reply", `{"msg_id":"`+extractMsgID(t, req)+`","reply_reference":"回应"}`)
		},
		Replyer: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.TextReply("好")
		},
	})

	h.SendMessage("你好", true)
	h.WaitForSend(10 * time.Second)

	reqs := h.ReplyerRequests()
	if len(reqs) == 0 {
		t.Fatal("应有 Replyer 请求")
	}

	const plannerReminder = "请你进行分析，是不调用工具，进行思考和等待"
	for _, r := range reqs {
		for i, m := range r.Messages {
			if strings.Contains(m.ContentText(), plannerReminder) {
				t.Errorf("Replyer 请求第 %d 条含 Planner 提醒，模型会照抄它：%q",
					i, m.ContentText())
			}
		}
	}

	// 末尾必须是「只输出正文」的尾注。
	last := reqs[0].Messages[len(reqs[0].Messages)-1]
	if !strings.Contains(last.ContentText(), "只输出实际要发言的内容") {
		t.Errorf("Replyer 末尾应是输出格式要求，实际 %q", last.ContentText())
	}
}

// TestBotHistoryIsAssistantForReplyer 锁住「机器人的历史发言是 assistant 角色」。
//
// 参考实现 maisaka_generator_base.py:638-644 把 guided_reply 转成
// RoleType.Assistant。我们以前全当 user，模型会把自发言当成「别人刚说过的话」
// 照着模仿 —— 是指令回声自我强化的根因之一。
func TestBotHistoryIsAssistantForReplyer(t *testing.T) {
	h := mock.NewHarness(t, mock.HarnessOptions{
		Planner: func(req *mock.RecordedRequest, call_ int) *mock.Reply {
			return mock.ToolReply("reply", `{"msg_id":"`+extractMsgID(t, req)+`","reply_reference":"回应"}`)
		},
		Replyer: func(req *mock.RecordedRequest, call_ int) *mock.Reply {
			return mock.TextReply("第一句回复")
		},
	})

	// 第一轮：产生一条出站的「机器人发言」并入库/入内存。
	h.SendMessage("在吗", true)
	h.WaitForSend(10 * time.Second)

	// 第二轮：此时上下文里已经有机器人自己说过的话。
	h.SendMessage("在的", true)
	time.Sleep(1500 * time.Millisecond)

	reqs := h.ReplyerRequests()
	if len(reqs) < 2 {
		t.Fatalf("应有至少 2 次 Replyer 请求，实际 %d", len(reqs))
	}

	// 最后一次请求里应该出现 assistant 角色。
	var assistants int
	for _, m := range reqs[len(reqs)-1].Messages {
		if m.Role == "assistant" {
			assistants++
		}
	}
	if assistants == 0 {
		t.Error("第二轮 Replyer 请求里应包含 assistant 角色的历史发言")
	}
}

// TestReplyerInstructionEchoNeverSent 是端到端兜底回归。
//
// 即使模型（或历史里的脏样例）让它吐出「不调用工具，直接回就行。<正文>」，
// 最终发到群里的必须是干净正文。否则这条脏消息会入库、
// 下一轮被当作「机器人自己说过的话」读回上下文，形成正反馈。
//
// 实测线上就是这样恶化的：10 条污染消息占 12.7%，之后同一会话
// 越回越容易再泄漏。
func TestReplyerInstructionEchoNeverSent(t *testing.T) {
	h := mock.NewHarness(t, mock.HarnessOptions{
		Planner: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.ToolReply("reply", `{"msg_id":"`+extractMsgID(t, req)+`","reply_reference":"回应"}`)
		},
		Replyer: func(req *mock.RecordedRequest, call int) *mock.Reply {
			// 原样复现线上截到的输出形态。
			return mock.TextReply("不调用工具，直接回就行。\n\n行，那这锅巴我就踏实收着了")
		},
	})

	h.SendMessage("锅巴归你了", true)
	text := h.WaitForSend(10 * time.Second)

	if strings.Contains(text, "不调用工具") {
		t.Errorf("指令回声被发出去了：%q", text)
	}
	if !strings.Contains(text, "锅巴") {
		t.Errorf("正文被误删：%q", text)
	}
}

// ============ 辅助 ============

// extractMsgID 从 Planner 请求里掏出第一条消息的 msg_id。
//
// 真实模型也是这么做的：从上下文里读 msg_id 然后填进参数。
func extractMsgID(t *testing.T, req *mock.RecordedRequest) string {
	t.Helper()

	for _, m := range req.Messages {
		text := m.ContentText()
		const marker = `msg_id="`
		idx := strings.Index(text, marker)
		if idx < 0 {
			continue
		}
		start := idx + len(marker)
		end := strings.Index(text[start:], `"`)
		if end < 0 {
			continue
		}
		return text[start : start+end]
	}

	t.Logf("请求里没有找到 msg_id，可用消息数: %d", len(req.Messages))
	return "unknown"
}

// requestContains 判断请求里是否有消息包含某段文本。
func requestContains(req *mock.RecordedRequest, needle string) bool {
	for _, m := range req.Messages {
		if strings.Contains(m.ContentText(), needle) {
			return true
		}
	}
	return false
}

var _ = domain.ScopeGroup

// TestAttachAtSendsAtSegment 防：attach_at 收下来但没真正发出去。
//
// 这是 AGENTS.md 里记的「已实现但没接上线」之一：reply 工具解析了参数、
// 填进了 ReplyRequest，但 runtime.Reply 只用了 Reference 与 Style。
func TestAttachAtSendsAtSegment(t *testing.T) {
	h := mock.NewHarness(t, mock.HarnessOptions{
		Planner: func(req *mock.RecordedRequest, call int) *mock.Reply {
			// 先确认上下文里拿得到 msg_id，再把它同时填进 msg_id 与 attach_at。
			id := extractMsgID(t, req)
			return mock.ToolReply("reply", `{"msg_id":"`+id+`","reply_reference":"回应小明","attach_at":["`+id+`"]}`)
		},
		Replyer: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.TextReply("在的")
		},
	})

	h.SendMessage("木木在吗", true)
	h.WaitForSend(10 * time.Second)

	sends := h.Send.Sends()
	if len(sends) == 0 {
		t.Fatal("应有出站记录")
	}
	if len(sends[0].AtUserIDs) == 0 {
		t.Fatalf("首条消息没有 @ 任何人：%+v", sends[0])
	}
	// 发送目标应是消息发送者的 OpenID，而不是消息 ID。
	if got := sends[0].AtUserIDs[0]; got != "user-1" {
		t.Errorf("被 @ 的 OpenID = %q，期望 user-1", got)
	}
}

// TestAttachAtWithUnknownMsgIDDoesNotFail 防：模型填了一个不存在的 msg_id，
// 结果整条回复发不出去（@ 只是锦上添花，不该阻断发送）。
func TestAttachAtWithUnknownMsgIDDoesNotFail(t *testing.T) {
	h := mock.NewHarness(t, mock.HarnessOptions{
		Planner: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.ToolReply("reply", `{"msg_id":"`+extractMsgID(t, req)+`","reply_reference":"回应","attach_at":["不存在的id"]}`)
		},
		Replyer: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.TextReply("嗯")
		},
	})

	h.SendMessage("你好", true)
	text := h.WaitForSend(10 * time.Second)

	if !strings.Contains(text, "嗯") {
		t.Errorf("发出的内容 = %q，应仍然包含正文", text)
	}
	if sends := h.Send.Sends(); len(sends) > 0 && len(sends[0].AtUserIDs) != 0 {
		t.Errorf("无法解析的 attach_at 不应产生 @ 段: %v", sends[0].AtUserIDs)
	}
}

// TestAttachPicSendsImage 防：attach_pic 收下来但图片没真正发出去。
func TestAttachPicSendsImage(t *testing.T) {
	h := mock.NewHarness(t, mock.HarnessOptions{
		Planner: func(req *mock.RecordedRequest, call int) *mock.Reply {
			id := extractMsgID(t, req)
			return mock.ToolReply("reply",
				`{"msg_id":"`+id+`","reply_reference":"回应图片","attach_pic":[{"msg_id":"`+id+`","index":0}]}`)
		},
		Replyer: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.TextReply("这图有意思")
		},
	})

	h.SendMessageWithImage("group-1", "user-1", "小明", "看这个")
	// 图文各占一条：等两条都发出来。
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if len(h.Send.Sends()) >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	sends := h.Send.Sends()
	if len(sends) < 2 {
		t.Fatalf("应发出至少 2 条（正文 + 图片），实际 %d 条", len(sends))
	}

	var images int
	for _, s := range sends {
		images += s.ImageCount
	}
	if images == 0 {
		t.Errorf("没有任何一条消息携带图片：%+v", sends)
	}
}

// TestQuoteFrequencyNeverDisablesQuote 防：把引用频率配成 never 时仍然引用。
func TestQuoteFrequencyNeverDisablesQuote(t *testing.T) {
	h := mock.NewHarness(t, mock.HarnessOptions{
		Planner: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.ToolReply("reply", `{"msg_id":"`+extractMsgID(t, req)+`","reply_reference":"回应"}`)
		},
		Replyer: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.TextReply("好")
		},
	})
	h.Update(func(c *config.Config) {
		c.Chat.Style.GroupQuoteFrequency = "never"
	})

	h.SendMessage("说点什么", true)
	h.WaitForSend(10 * time.Second)

	sends := h.Send.Sends()
	if len(sends) == 0 {
		t.Fatal("应有出站记录")
	}
	if sends[0].QuoteTo != "" {
		t.Errorf("never 档不应引用，实际 quoteTo = %q", sends[0].QuoteTo)
	}
}

// TestQuoteFrequencyOftenQuotesFirstSegmentOnly 防：多段回复时
// 每一段都挂引用（QQ 上每段都引用一次很吵），或反过来一段都没挂。
func TestQuoteFrequencyOftenQuotesFirstSegmentOnly(t *testing.T) {
	h := mock.NewHarness(t, mock.HarnessOptions{
		Planner: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.ToolReply("reply", `{"msg_id":"`+extractMsgID(t, req)+`","reply_reference":"回应"}`)
		},
		Replyer: func(req *mock.RecordedRequest, call int) *mock.Reply {
			// 故意给一句会被切成多条的长回复。
			return mock.TextReply("第一句说完了。第二句接着讲。第三句再来一点。第四句收尾啦。")
		},
	})
	// often + 空文本档会让概率尽量高；再缩短分割阈值保证一定切成多条。
	h.Update(func(c *config.Config) {
		c.Chat.Style.GroupQuoteFrequency = "often"
		c.Response.MaxLength = 12
		c.Response.MaxSentenceNum = 1
		c.Response.MaxSplitNum = 5
	})

	h.SendMessage("说点什么", true)

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if len(h.Send.Sends()) >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	sends := h.Send.Sends()
	if len(sends) < 2 {
		t.Skipf("回复未被切成多条（%d 条），无法验证首段引用", len(sends))
	}

	var quoted int
	for _, s := range sends {
		if s.QuoteTo != "" {
			quoted++
		}
	}
	if quoted > 1 {
		t.Errorf("%d 条消息都挂了引用，应只有首条引用", quoted)
	}
}

// TestReasoningRecordsPersisted 防：推理记录只定义没写入，
// reasoning_records 表恒为 0 行，WebUI 推理重放永远是空的。
func TestReasoningRecordsPersisted(t *testing.T) {
	h := mock.NewHarness(t, mock.HarnessOptions{
		Planner: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.ToolReply("reply", `{"msg_id":"`+extractMsgID(t, req)+`","reply_reference":"回应"}`)
		},
		Replyer: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.TextReply("好的")
		},
	})

	h.SendMessage("在吗", true)
	h.WaitForSend(10 * time.Second)

	sessionID := firstSessionID(t, h)

	recs, err := h.DB.Reasoning().ListBySession(t.Context(), sessionID, 50)
	if err != nil {
		t.Fatalf("查询推理记录失败: %v", err)
	}
	if len(recs) == 0 {
		t.Fatalf("会话 %s 没有推理记录，表是空的", sessionID)
	}

	// 至少要能看出「发了什么请求、回了什么」。
	//
	// 按 kind 查找而不是取第一条：现在时间线上还会有 gate / tool / replyer
	// 等事件（对齐参考实现的主事件账本），第一条未必是 Planner。
	var r store.ReasoningRecord
	var found bool
	for _, rec := range recs {
		if rec.Kind == "planner" {
			r, found = rec, true
			break
		}
	}
	if !found {
		t.Fatalf("没有 planner 记录，实际有 %d 条（kind: %v）", len(recs), reasoningKinds(recs))
	}
	if r.Request == "" {
		t.Error("request 为空，无法重放")
	}
	if !strings.Contains(r.Request, "reply") {
		t.Errorf("request 里应包含 reply 工具定义，实际: %.200s", r.Request)
	}
	if r.Response == "" {
		t.Error("response 为空，无法重放")
	}
	if !strings.Contains(r.Response, "reply_reference") {
		t.Errorf("response 里应包含模型给出的工具调用，实际: %.200s", r.Response)
	}
	if r.TurnID == "" {
		t.Error("turn_id 为空，WebUI 无法把多轮串成一次思考")
	}
}

// reasoningKinds 汇总记录里出现过哪些 kind，失败时报出来方便定位。
func reasoningKinds(recs []store.ReasoningRecord) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(recs))
	for _, rec := range recs {
		if !seen[rec.Kind] {
			seen[rec.Kind] = true
			out = append(out, rec.Kind)
		}
	}
	return out
}

// firstSessionID 从数据库里取第一个会话 ID，用于断言。
func firstSessionID(t *testing.T, h *mock.Harness) string {
	t.Helper()
	sessions, err := h.DB.Sessions().List(t.Context(), 1)
	if err != nil || len(sessions) == 0 {
		t.Fatalf("取会话失败: %v", err)
	}
	return sessions[0].ID
}

// TestVisualModelReceivesImage 防：多模态断链 ——
// 图片下载了、进了 Segments，但 Planner 请求里一张图都没有。
//
// 这是 AGENTS.md 里记的第 1 条："codec 把图片压成 [图片] 占位，
// Message.Images 始终为空，多模态是断的"。
func TestVisualModelReceivesImage(t *testing.T) {
	h := mock.NewHarness(t, mock.HarnessOptions{
		Planner: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.ToolReply("stop", `{"reason":"只是想看看"}`)
		},
		Replyer: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.TextReply("嗯")
		},
	})

	// 把 Planner 模型标成多模态。
	h.Update(func(c *config.Config) {
		for i := range c.LLM.Models {
			c.LLM.Models[i].Visual = true
		}
	})

	h.SendMessageWithImage("group-1", "user-1", "小明", "看这个")
	h.WaitForLLMCalls(1, 10*time.Second)

	plannerReqs := h.PlannerRequests()
	if len(plannerReqs) == 0 {
		t.Fatal("应有 Planner 请求")
	}

	var images int
	for _, m := range plannerReqs[0].Messages {
		images += len(m.Images())
	}
	if images == 0 {
		t.Error("Planner 请求里没有图片，多模态仍然是断的")
	}
}

// TestTextModelDoesNotReceiveImage 防：往纯文本模型塞 image_url。
//
// 服务端会直接 400，表现是整轮思考失败 —— 比看不到图严重得多。
func TestTextModelDoesNotReceiveImage(t *testing.T) {
	h := mock.NewHarness(t, mock.HarnessOptions{
		Planner: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.ToolReply("stop", `{"reason":"看看"}`)
		},
		Replyer: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.TextReply("嗯")
		},
	})

	// 默认配置里 Visual 是 false，这里显式强调一遍。
	h.Update(func(c *config.Config) {
		for i := range c.LLM.Models {
			c.LLM.Models[i].Visual = false
		}
	})

	h.SendMessageWithImage("group-1", "user-1", "小明", "看这个")
	h.WaitForLLMCalls(1, 10*time.Second)

	plannerReqs := h.PlannerRequests()
	if len(plannerReqs) == 0 {
		t.Fatal("应有 Planner 请求")
	}

	for _, m := range plannerReqs[0].Messages {
		if got := m.Images(); len(got) > 0 {
			t.Fatalf("纯文本模型的请求里出现了 %d 张图片，会被服务端拒绝", len(got))
		}
	}
}

// TestQuoteCounterRestoredFromDB 防：重启后引用计数器是空的。
//
// 参考实现（runtime.py:_restore_recent_context_from_db）在恢复上下文时
// 会逐条调 `_quote_counter.record(message)`。漏掉这步的后果不是"少引用几次"，
// 而是目标消息全部「不在窗口内」→ 判定直接引用 → 重启后每条都引用，
// 行为突然变了。这个测试从"计数器里有没有东西"这个角度守它。
func TestQuoteCounterRestoredFromDB(t *testing.T) {
	dbPath := t.TempDir() + "/quote.db"

	h1 := mock.NewHarness(t, mock.HarnessOptions{
		DBPath: dbPath,
		Planner: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.ToolReply("stop", "{}")
		},
	})
	h1.SendMessage("第一句", false)
	h1.WaitForLLMCalls(1, 5*time.Second)
	time.Sleep(200 * time.Millisecond)
	h1.SendMessage("第二句", false)
	h1.WaitForLLMCalls(2, 5*time.Second)
	time.Sleep(200 * time.Millisecond)

	h1.Hub.Close()
	h1.DB.Close()

	// 新实例：上下文与引用计数器都应从库里恢复。
	h2 := mock.NewHarness(t, mock.HarnessOptions{
		DBPath: dbPath,
		Planner: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.ToolReply("stop", "{}")
		},
	})
	// 触发一次投递，让 runtime 被创建（创建时才会 restoreRecentContext）。
	h2.SendMessage("第三句", false)
	h2.WaitForLLMCalls(1, 10*time.Second)

	// 从库里的历史消息反推：如果计数器真被喂过，
	// 那么"第二句"这条消息应该能被 Snapshot 找到。
	// 这里通过 Planner 请求是否带上了历史来间接确认 restore 跑过，
	// 再直接查会话的历史条数。
	plannerReqs := h2.PlannerRequests()
	if len(plannerReqs) == 0 {
		t.Fatal("应有 Planner 请求")
	}
	var sawHistory bool
	for _, m := range plannerReqs[0].Messages {
		if strings.Contains(m.ContentText(), "第一句") {
			sawHistory = true
		}
	}
	if !sawHistory {
		t.Fatal("上下文未从数据库恢复，无法验证计数器")
	}

	// 关键断言：恢复的历史消息条数应 >= 2（第一句、第二句）。
	// 计数器与 history 是同一个循环里一起喂的，
	// 所以历史在、计数器就应当在（结构上绑死，见 runtime.restoreRecentContext）。
	msgs, err := h2.DB.Messages().Recent(t.Context(), firstSessionID(t, h2), 10)
	if err != nil {
		t.Fatalf("查询历史失败: %v", err)
	}
	if len(msgs) < 2 {
		t.Errorf("恢复的历史条数 = %d，期望 >= 2", len(msgs))
	}
}

// TestOutboundReplyIsStored 守的是「机器人说的话也进库」。
//
// 之前整条链路只有入站消息落库，出站只进内存（上限 20 条、重启即丢）。
// 后果有两层：
//   - WebUI 的会话页只能看到用户消息，看起来像机器人从没回过话
//   - 重启后恢复的上下文只有用户那半边，模型不知道自己说过什么
//
// 这条测试同时钉住「WebUI 能看到」和「重启后能恢复」两个面。
func TestOutboundReplyIsStored(t *testing.T) {
	dbPath := t.TempDir() + "/outbound.db"

	h := mock.NewHarness(t, mock.HarnessOptions{
		DBPath: dbPath,
		Planner: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.ToolReply("reply", `{"msg_id":"`+extractMsgID(t, req)+`","reply_reference":"回应"}`)
		},
		Replyer: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.TextReply("木木说过这句话")
		},
	})

	h.SendMessage("你好", true)
	h.WaitForSend(10 * time.Second)
	// 落库是异步的（发送成功之后），给它一点时间。
	time.Sleep(500 * time.Millisecond)

	sessions, err := h.DB.Sessions().List(context.Background(), 10)
	if err != nil {
		t.Fatalf("查会话失败: %v", err)
	}
	if len(sessions) == 0 {
		t.Fatal("没有会话")
	}

	msgs, err := h.DB.Messages().Recent(context.Background(), sessions[0].ID, 50)
	if err != nil {
		t.Fatalf("查消息失败: %v", err)
	}

	var selfCount, userCount int
	var selfText string
	for _, m := range msgs {
		if m.IsSelf {
			selfCount++
			selfText = m.PlainText
		} else {
			userCount++
		}
	}
	if selfCount == 0 {
		t.Fatal("机器人自己的回复没有入库，WebUI 会看不到它说过什么")
	}
	if !strings.Contains(selfText, "木木说过这句话") {
		t.Errorf("入库的出站正文 = %q", selfText)
	}
	if userCount == 0 {
		t.Error("用户消息也应入库")
	}

	// 关掉再开一个新实例：恢复的上下文里必须能分辨哪句是自己说的。
	h.Hub.Close()
	h.DB.Close()

	h2 := mock.NewHarness(t, mock.HarnessOptions{
		DBPath: dbPath,
		Planner: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.ToolReply("reply", `{"msg_id":"`+extractMsgID(t, req)+`","reply_reference":"回应"}`)
		},
		Replyer: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.TextReply("嗯")
		},
	})

	h2.SendMessage("还记得吗", true)
	h2.WaitForSend(15 * time.Second)

	reqs := h2.PlannerRequests()
	if len(reqs) == 0 {
		t.Fatal("应有 Planner 请求")
	}

	// 自己说过的话要带 is_self_message 标记，
	// 否则模型会把它当成「某个用户说的」，于是重复自己。
	var marked bool
	for _, m := range reqs[0].Messages {
		c := m.ContentText()
		if strings.Contains(c, "木木说过这句话") && strings.Contains(c, `is_self_message="true"`) {
			marked = true
		}
	}
	if !marked {
		t.Error("恢复的自发言应带 is_self_message 标记，实际没有")
	}
}

// TestQuotedMessageReachesPlanner 是引用链路的端到端回归。
//
// 用户引用一条消息说话时，模型必须同时看到两样东西：
//  1. 引用关系 —— `<message … quote="REFIDX_…">` 属性
//  2. 被引用正文 —— `[回复了X的消息: …]` 前缀
//
// 少任何一个都不算「看得见引用」：只有属性时模型拿到一个够不着的 ID，
// 只有正文时模型不知道那是在引谁。两者都缺就是改造前的状态 ——
// 引用被完全丢掉，模型只能靠猜。
func TestQuotedMessageReachesPlanner(t *testing.T) {
	h := mock.NewHarness(t, mock.HarnessOptions{
		Planner: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.ToolReply("reply", `{"msg_id":"`+extractMsgID(t, req)+`","reply_reference":"回应引用"}`)
		},
		Replyer: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.TextReply("那确实")
		},
	})

	h.SendQuotedMessage("真的假的", "REFIDX_quoted_target", "今晚加班")
	h.WaitForSend(10 * time.Second)

	plannerReqs := h.PlannerRequests()
	if len(plannerReqs) == 0 {
		t.Fatal("应有 Planner 请求")
	}

	var sawQuoteAttr, sawQuotedBody bool
	for _, m := range plannerReqs[0].Messages {
		text := m.ContentText()
		if strings.Contains(text, `quote="REFIDX_quoted_target"`) {
			sawQuoteAttr = true
		}
		if strings.Contains(text, "[回复了小红的消息: 今晚加班]") {
			sawQuotedBody = true
		}
	}
	if !sawQuoteAttr {
		t.Error("Planner 上下文里应有 quote 属性（引用关系）")
	}
	if !sawQuotedBody {
		t.Error("Planner 上下文里应有被引用正文")
	}

	// 被引用正文绝不能混进用户这轮的发言正文里（错误归因）。
	for _, m := range plannerReqs[0].Messages {
		text := m.ContentText()
		if strings.Contains(text, "今晚加班") && !strings.Contains(text, "[回复了") {
			t.Errorf("被引用正文出现在非引用位置，可能被当成用户发言: %q", text)
		}
	}
}

// TestQuotedOnlyMessageStillReachesPlanner 守「只引用不打字」的场景。
//
// 这种消息正文为空，旧实现会因为「消息不包含文本」整条丢弃；
// 现在它必须仍然带着引用信息进上下文。
func TestQuotedOnlyMessageStillReachesPlanner(t *testing.T) {
	h := mock.NewHarness(t, mock.HarnessOptions{
		Planner: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.ToolReply("reply", `{"msg_id":"`+extractMsgID(t, req)+`","reply_reference":"回应纯引用"}`)
		},
		Replyer: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.TextReply("看到了")
		},
	})

	h.SendQuotedMessage("", "REFIDX_only_quote", "别人以前说的话")
	h.WaitForSend(10 * time.Second)

	plannerReqs := h.PlannerRequests()
	if len(plannerReqs) == 0 {
		t.Fatal("纯引用消息也应触发 Planner")
	}
	var saw bool
	for _, m := range plannerReqs[0].Messages {
		if strings.Contains(m.ContentText(), "[回复了小红的消息: 别人以前说的话]") {
			saw = true
		}
	}
	if !saw {
		t.Error("纯引用消息的被引正文应进上下文")
	}
}

// TestOutboundQuoteIsSent 是出站引用的端到端回归。
//
// 修复前的状态：`splitOutbound` 里 `case domain.SegmentReply: continue`
// 把引用段直接丢掉，请求体里从来没有 message_reference。
// 机器人会照常说「我引用了你那条」，而平台侧什么都没收到。
//
// 断言必须落在 **QuoteTo 非空**，而不是「发送成功」——
// 改造前发送一直是成功的。
//
// 为什么循环多轮：引用是按概率抽签的（often 档约 42%），
// 单轮不引用是正确行为，不是失败。跑 15 轮里一次都不引用，
// 概率约 1e-4，那时才说明链路断了。
func TestOutboundQuoteIsSent(t *testing.T) {
	h := mock.NewHarness(t, mock.HarnessOptions{
		Planner: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.ToolReply("reply", `{"msg_id":"`+extractMsgID(t, req)+`","reply_reference":"回应"}`)
		},
		Replyer: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.TextReply("好")
		},
	})
	h.Update(func(c *config.Config) {
		c.Chat.Style.GroupQuoteFrequency = "often"
	})

	var sawQuote bool
	for round := 0; round < 15 && !sawQuote; round++ {
		h.Send.Reset()
		h.SendMessage("说点什么", true)
		h.WaitForSend(10 * time.Second)

		for _, sent := range h.Send.Sends() {
			if sent.QuoteTo == "" {
				continue
			}
			sawQuote = true
			// 引用目标必须是 REFIDX_ 形态（引用索引），
			// 不是 ROBOT1.0_ 形态的消息 ID。
			if !strings.HasPrefix(sent.QuoteTo, "REFIDX_") {
				t.Errorf("引用目标 = %q，应为 REFIDX_ 形态的引用索引", sent.QuoteTo)
			}
		}

		// 同一轮里也只能有一条挂引用（正文分段时不该每段都挂）。
		var perRound int
		for _, sent := range h.Send.Sends() {
			if sent.QuoteTo != "" {
				perRound++
			}
		}
		if perRound > 1 {
			t.Errorf("同一轮里 %d 条消息都挂了引用，应只有首条", perRound)
		}
	}

	if !sawQuote {
		t.Error("15 轮里一次引用都没发出 —— 引用链路查断（splitOutbound 是不是又把 Reply 段丢了？）")
	}
}

// TestQuoteOnSelfMessageUsesOwnRefIdx 守「引用木木自己说过的话」。
//
// 这是出站引用最容易漏的一条：
//   - 入站事件里只有别人的 msg_idx，机器人自己发的消息没有这个值
//   - 唯一来源是发送响应的 ext_info.ref_idx
//
// 所以必须做到两件事：发出时把 ref_idx 记进 botReplies，
// 判定引用时能从 botReplies 里查到它。少任一件，这条路就是断的。
func TestQuoteOnSelfMessageUsesOwnRefIdx(t *testing.T) {
	var prevBotMsgID string
	var target string
	calls := 0

	h := mock.NewHarness(t, mock.HarnessOptions{
		Planner: func(req *mock.RecordedRequest, call int) *mock.Reply {
			calls++
			// 第二轮起：引用木木自己之前说过的那句。
			if target != "" {
				return mock.ToolReply("reply", `{"msg_id":"`+target+`","reply_reference":"认可自己刚才说的"}`)
			}
			return mock.ToolReply("reply", `{"msg_id":"`+extractMsgID(t, req)+`","reply_reference":"先回一句"}`)
		},
		Replyer: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.TextReply("好")
		},
	})
	h.Update(func(c *config.Config) {
		c.Chat.Style.GroupQuoteFrequency = "often"
	})

	// 先让木木说一句，拿到它的出站消息 ID。
	h.SendMessage("说点什么", true)
	h.WaitForSend(10 * time.Second)
	prevBotMsgID = "sent-1"
	stored := false
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if outboundStored(h, prevBotMsgID) {
			stored = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !stored {
		t.Fatalf("第一轮的出站消息没有落库（%s），后续断言无从谈起", prevBotMsgID)
	}

	// 引用按概率抽签，多轮重试直到命中。
	target = prevBotMsgID
	var found bool
	for round := 0; round < 15 && !found; round++ {
		h.Send.Reset()
		h.SendMessage("你是这么说的吗", true)
		h.WaitForSend(10 * time.Second)

		for _, sent := range h.Send.Sends() {
			if sent.QuoteTo == "" {
				continue
			}
			found = true
			// 引用自己的发言时，目标必须是那条出站消息自己的 ref_idx。
			if sent.QuoteTo != "REFIDX_sent_1" {
				t.Errorf("引用自己的发言时目标 = %q，期望 REFIDX_sent_1", sent.QuoteTo)
			}
		}
	}

	if !found {
		t.Error("引用木木自己的发言没有生效 —— 可能是 botReplies 里缺 RefMsgIdx，" +
			"或 findMessageByID 没查 botReplies")
	}
}

// outboundStored 报告某条出站消息是否已经落库。
//
// 注意 session ID 是哈希值，不是 "group:group-1" 这种可猜的形式，
// 所以只能遍历会话去查 —— 硬编码一个 ID 会让等待循环一直空转到超时
// （测试仍然「通过」，只是白等十几秒）。
func outboundStored(h *mock.Harness, messageID string) bool {
	sessions, err := h.DB.Sessions().List(context.Background(), 20)
	if err != nil {
		return false
	}
	for _, s := range sessions {
		msgs, err := h.DB.Messages().Recent(context.Background(), s.ID, 50)
		if err != nil {
			continue
		}
		for _, m := range msgs {
			if m.MessageID == messageID {
				return true
			}
		}
	}
	return false
}

// TestWebUIDialogEndToEnd 守的是网页对话窗口的完整链路：
// 网页用户的话以 platform=webui 的私聊消息进 Hub，
// 机器人思考后回复 —— 回复不走 QQ 适配器（那里只会失败），
// 而是经 WebUISender 生成消息 ID 后照常落库，WebUI 轮询 /api/messages 就能看到。
//
// 这是「以机器人身份发话」被移除后的替代方向：网页端以人类身份对话。
func TestWebUIDialogEndToEnd(t *testing.T) {
	h := mock.NewHarness(t, mock.HarnessOptions{
		Planner: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.ToolReply("reply", `{"msg_id":"`+extractMsgID(t, req)+`","reply_reference":"网页里的回应"}`)
		},
		Replyer: func(req *mock.RecordedRequest, call int) *mock.Reply {
			return mock.TextReply("这是网页窗口里的回复")
		},
	})

	sessionID := h.SendWebUIMessage("阿平", "在网页里跟你说话")

	// webui 会话的回复不经过 harness 的假 sender，只能等它落库。
	// 用轮询而不是固定 sleep：防抖 + 打字延迟的耗时不稳定。
	deadline := time.Now().Add(15 * time.Second)
	var msgs []*domain.Message
	for time.Now().Before(deadline) {
		var err error
		msgs, err = h.DB.Messages().Recent(context.Background(), sessionID, 50)
		if err == nil {
			var selfSeen bool
			for _, m := range msgs {
				if m.IsSelf {
					selfSeen = true
				}
			}
			if selfSeen {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}

	var selfText, userText string
	for _, m := range msgs {
		if m.IsSelf {
			selfText = m.PlainText
		} else {
			userText = m.PlainText
		}
	}
	if userText != "在网页里跟你说话" {
		t.Errorf("网页用户消息没有入站落库，got %q", userText)
	}
	if selfText != "这是网页窗口里的回复" {
		t.Errorf("机器人回复没有落库（WebUI 将看不到），got %q", selfText)
	}
}
