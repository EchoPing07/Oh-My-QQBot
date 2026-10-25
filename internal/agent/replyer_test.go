package agent

// replyer.go 的测试。
//
// Replyer 是唯一「把话真正说出来」的环节，它有两个容易出错的地方：
//   1. 补充材料（表达习惯 / 关键词反应）插错位置 —— 会被末尾的格式要求盖过
//   2. 空内容被当成正常回复发出去 —— 群里会看到一条空消息
// 所以这里既测纯函数，也用一个 httptest 假服务把 Generate 的请求体抓下来断言。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/llm"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/logx"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/prompt"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/tool"
)

// TestMain 把日志压到只报错，避免测试输出被日志刷屏。
func TestMain(m *testing.M) {
	_ = logx.Init(logx.Options{Level: logx.LevelError, Console: false})
	m.Run()
}

// TestReplyerStyleHintDelegation 验证篇幅要求的映射已上提到 prompt 包。
//
// 原来叫 mapStyleToPrompt，把「这次回复要简短」拼进 replyer 模板的 {reply_style}
// 也就是拿到 system 里去；那样既覆盖了配置里的基础风格，权重又太弱。
// 现在改成一条独立的末尾 user 消息（prompt.ReplyerStyleHint），
// 与参考实现 maisaka_generator_base.py:611 一致。
func TestReplyerStyleHintDelegation(t *testing.T) {
	if got := prompt.ReplyerStyleHint("简短表达"); !strings.Contains(got, "简短的回复") {
		t.Errorf("简短表达映射不对：%q", got)
	}
	if got := prompt.ReplyerStyleHint("长回复"); !strings.Contains(got, "较为详细的评论") {
		t.Errorf("长回复映射不对：%q", got)
	}
	// 正常回复 / 未知 / 空都返回空串，表示不追加这条消息。
	for _, s := range []string{"正常回复", "不知所云", "", "  简短表达  \n"} {
		if s == "  简短表达  \n" {
			// 带空白的也要能识别。
			if got := prompt.ReplyerStyleHint(s); got == "" {
				t.Errorf("%q 应被识别成简短表达", s)
			}
			continue
		}
		if got := prompt.ReplyerStyleHint(s); got != "" {
			t.Errorf("%q 应返回空串，实际 %q", s, got)
		}
	}
}

// TestReplyerMessageOrder 验证补充材料的插入顺序。
//
// 顺序有讲究（每条都对应一次真实的错误排序）：
//
//	system → 历史 → 表达习惯 → 回复参考 → 关键词反应 → 末尾块 → 篇幅要求
//
// 早期版本靠 insertBeforeLast 插「倒数第一条之前」，在末尾只有一条时是对的；
// 加了篇幅要求之后就会把补充材料塞到末尾块后面 —— 那一格是模型最看重的
// 位置，被占掉后格式约束就失效了。所以顺序现在写在 BuildReplyerMessages 里，
// 这个测试就是那条顺序的守护者。
func TestReplyerMessageOrder(t *testing.T) {
	b := newTestPromptBuilder(t, newTestConfig())
	base := time.Date(2026, 10, 21, 13, 0, 0, 0, time.UTC)

	history := []*domain.Message{{
		MessageID: "m-1", UserID: "u1", UserName: "小明",
		PlainText: "历史消息", ReceivedAt: base,
	}}

	msgs, _, err := b.BuildReplyerMessages(ReplyerPromptInput{
		IsGroup:          true,
		History:          history,
		Reference:        "回应他的问题",
		Style:            "简短表达",
		BaseStyle:        "基础风格",
		Now:              base,
		TargetMsgID:      "m-1",
		ExpressionHabits: "【表达习惯参考】\n不要直接照搬原句",
		KeywordReaction:  "【关键词反应】\n先安抚再问细节",
	})
	if err != nil {
		t.Fatalf("组装失败: %v", err)
	}

	// 只查 user 消息：replyer 模板的 system 段本身就写着
	// 「你可以参考【回复信息参考】中的信息」，那是模板文字、不是注入块。
	idx := func(needle string) int {
		for i, m := range msgs {
			if m.Role == llm.RoleUser && strings.Contains(m.Content, needle) {
				return i
			}
		}
		return -1
	}

	habits := idx("【表达习惯参考】")
	reference := idx("【回复信息参考】")
	reaction := idx("【关键词反应】")
	final := idx("当前时间：")
	style := idx(prompt.ReplyerStyleHint("简短表达"))

	order := []struct {
		name string
		at   int
	}{
		{"表达习惯", habits}, {"回复参考", reference}, {"关键词反应", reaction},
		{"末尾块", final}, {"篇幅要求", style},
	}
	for _, o := range order {
		if o.at < 0 {
			t.Fatalf("%s 没被注入", o.name)
		}
	}
	for i := 1; i < len(order); i++ {
		if order[i].at < order[i-1].at {
			t.Errorf("%s(第%d) 不该排在 %s(第%d) 之前", order[i].name, order[i].at, order[i-1].name, order[i-1].at)
		}
	}

	// 末尾块必须在末尾两格之一，不能被补充材料挤走 —— 本次重构的核心约束。
	if final < len(msgs)-2 {
		t.Errorf("末尾块位置 = %d，应在末尾两格之一（共 %d 条）", final, len(msgs))
	}
}

// capturedRequest 是一次被抓下来的请求（只保留测试要断言的字段）。
type capturedRequest struct {
	Messages []capturedMessage `json:"messages"`
	Tools    []struct {
		Type     string `json:"type"`
		Function struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"function"`
	} `json:"tools"`
}

// capturedMessage 是一条被抓下来的消息。
//
// Content 用 RawMessage：多模态时它是数组，直接当字符串解析会失败，
// 而那时恰恰是测试最需要看清内容的时候。
type capturedMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"tool_call_id"`
	Name       string          `json:"name"`
}

// text 把 content 归一化成可读文本，图片部分记成 <image> 占位。
func (m capturedMessage) text() string { return contentText(m.Content) }

// contentText 把 OpenAI 的 content 字段转成可读文本。
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL *struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return string(raw)
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Text != "" {
			b.WriteString(p.Text)
		}
		if p.Type == "image_url" {
			b.WriteString("<image>")
		}
	}
	return b.String()
}

// newReplyerServer 起一个假的 OpenAI 兼容服务。
//
// content 是返回给调用方的正文，可以是空串（用来测空内容报错）。
func newReplyerServer(t *testing.T, content string) (*httptest.Server, func() []capturedRequest) {
	t.Helper()

	var (
		mu   sync.Mutex
		reqs []capturedRequest
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req capturedRequest
		_ = json.Unmarshal(raw, &req)
		mu.Lock()
		reqs = append(reqs, req)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"},
			},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		})
	}))
	t.Cleanup(srv.Close)

	return srv, func() []capturedRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]capturedRequest(nil), reqs...)
	}
}

// newReplyerForTest 构造一个指向假服务的 Replyer。
func newReplyerForTest(t *testing.T, baseURL string) *Replyer {
	t.Helper()

	cfg := newTestConfig()
	cfg.LLM.Providers = []config.ProviderConfig{{
		Name: "mock", BaseURL: baseURL, APIKey: "mock", Timeout: 5, MaxRetry: 1,
	}}
	cfg.LLM.Models = []config.ModelConfig{{
		Name: "mock", ModelIdentifier: "mock", Provider: "mock",
	}}
	cfg.LLM.Tasks.Replyer.Models = []string{"mock"}

	return NewReplyer(config.Wrap(cfg), llm.New(config.Wrap(cfg)), newTestPromptBuilder(t, cfg), nil)
}

// TestReplyerGenerateCleansText 验证成功路径返回清理过的正文。
//
// 模型经常自作主张加「回复：」和包裹引号，这些必须被清掉，
// 否则群里真的会看到「回复：」三个字。
func TestReplyerGenerateCleansText(t *testing.T) {
	srv, captured := newReplyerServer(t, "「今天好热」")
	r := newReplyerForTest(t, srv.URL)

	text, err := r.Generate(context.Background(), ReplyInput{
		IsGroup:   true,
		History:   nil,
		Reference: "附和一下天气",
		Style:     "简短表达",
		Now:       time.Date(2026, 10, 21, 13, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if text != "今天好热" {
		t.Errorf("清理后 = %q，期望 %q", text, "今天好热")
	}

	reqs := captured()
	if len(reqs) != 1 {
		t.Fatalf("应该只请求一次，实际 %d 次", len(reqs))
	}
	// 篇幅要求现在是**独立的一条末尾 user 消息**，不再拼进 system。
	// 拼进 system 的权重太弱，「简短表达」压不住模型铺开写
	// （参考实现 maisaka_generator_base.py:611 也是独立一条）。
	var styleMsg int = -1
	for i, m := range reqs[0].Messages {
		if m.Role == llm.RoleUser && strings.Contains(m.text(), "请简短的回复") {
			styleMsg = i
		}
	}
	if styleMsg < 0 {
		t.Errorf("缺少独立的消息篇幅要求")
	}
	// system 里不能再有篇幅要求，它该只放配置的基础风格。
	if strings.Contains(reqs[0].Messages[0].text(), "这次回复要简短") {
		t.Errorf("篇幅要求不该拼进 system")
	}
	// 回归：Replyer 请求里任何一条都不能是 Planner 的提醒。
	for i, m := range reqs[0].Messages {
		if strings.Contains(m.text(), prompt.PlannerFinalReminder) {
			t.Errorf("第 %d 条含 PlannerFinalReminder：%q", i, m.text())
		}
	}
}

// TestReplyerGenerateEmptyContent 验证空内容报错。
//
// 推理模型会把 max_tokens 全花在思考上、正文为空（AGENTS.md 第 2 条）。
// 不报错的话，群里会收到一条空消息。
func TestReplyerGenerateEmptyContent(t *testing.T) {
	cases := []struct{ name, content string }{
		{"空串", ""},
		{"纯空白", "   \n\t "},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, _ := newReplyerServer(t, c.content)
			r := newReplyerForTest(t, srv.URL)

			_, err := r.Generate(context.Background(), ReplyInput{
				IsGroup: true,
				Now:     time.Now(),
			})
			if err == nil {
				t.Fatal("空内容应报错")
			}
			if !strings.Contains(err.Error(), "空内容") {
				t.Errorf("错误信息应说明是空内容，实际 %v", err)
			}
		})
	}
}

// TestReplyerGenerateInjectsHabitsBeforeReaction 验证补充材料的顺序。
//
// 先表达习惯（怎么说话）再关键词反应（这条要怎么说）。
// 反过来会让关键词要求被表达习惯的措辞稀释。
func TestReplyerGenerateInjectsHabitsBeforeReaction(t *testing.T) {
	srv, captured := newReplyerServer(t, "好")
	r := newReplyerForTest(t, srv.URL)

	_, err := r.Generate(context.Background(), ReplyInput{
		IsGroup:          true,
		Reference:        "回应他的问题",
		Now:              time.Now(),
		ExpressionHabits: "【表达习惯参考】\n不要直接照搬原句",
		KeywordReaction:  "【关键词反应】\n先安抚再问细节",
	})
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}

	reqs := captured()
	if len(reqs) != 1 {
		t.Fatalf("应该只请求一次，实际 %d 次", len(reqs))
	}
	msgs := reqs[0].Messages

	habitsIdx, reactionIdx, finalIdx := -1, -1, -1
	for i, m := range msgs {
		if strings.Contains(m.text(), "【表达习惯参考】") {
			habitsIdx = i
		}
		if strings.Contains(m.text(), "【关键词反应】") {
			reactionIdx = i
		}
		if strings.Contains(m.text(), "当前时间：") {
			finalIdx = i
		}
	}
	if habitsIdx < 0 {
		t.Fatal("表达习惯没被注入")
	}
	if reactionIdx < 0 {
		t.Fatal("关键词反应没被注入")
	}
	if finalIdx < 0 {
		t.Fatal("末尾块没被注入")
	}
	if habitsIdx > reactionIdx {
		t.Errorf("表达习惯应在关键词反应之前，习惯=%d 关键词=%d", habitsIdx, reactionIdx)
	}
	// 两块补充材料都不能跑到末尾块后面去 —— 那格要让给格式约束。
	if reactionIdx > finalIdx || habitsIdx > finalIdx {
		t.Errorf("补充材料不应插到末尾块之后：习惯=%d 关键词=%d 末尾块=%d",
			habitsIdx, reactionIdx, finalIdx)
	}
	// 末尾块必须在末尾两格之一（有篇幅要求时会再多一格）。
	if finalIdx < len(msgs)-2 {
		t.Errorf("末尾块位置 = %d，应在末尾两格之一（共 %d 条）", finalIdx, len(msgs))
	}

	// 回归：整条请求里不能出现 Planner 的提醒。
	// 那句带「不调用工具」，Replyer 会照抄成正文开头（实测泄漏 5/10）。
	for i, m := range msgs {
		if strings.Contains(m.text(), prompt.PlannerFinalReminder) {
			t.Errorf("第 %d 条含 PlannerFinalReminder：%q", i, m.text())
		}
	}
}

// TestReplyerGenerateSkipsBlankExtras 验证空白的补充材料不产生空消息。
//
// 空 user 消息会让模型以为「用户说了句话但内容是空的」，容易触发乱答。
func TestReplyerGenerateSkipsBlankExtras(t *testing.T) {
	srv, captured := newReplyerServer(t, "好")
	r := newReplyerForTest(t, srv.URL)

	_, err := r.Generate(context.Background(), ReplyInput{
		IsGroup:          true,
		Reference:        "回应",
		Now:              time.Now(),
		ExpressionHabits: "   ",
		KeywordReaction:  "\n\n",
	})
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}

	for _, m := range captured()[0].Messages {
		if m.Role == llm.RoleUser && strings.TrimSpace(m.text()) == "" {
			t.Errorf("不该出现空的 user 消息")
		}
	}
}

// TestReplyerGenerateConnectionError 验证请求失败能往上抛。
func TestReplyerGenerateConnectionError(t *testing.T) {
	// 指向一个必然连不上的地址。
	r := newReplyerForTest(t, "http://127.0.0.1:1")

	_, err := r.Generate(context.Background(), ReplyInput{IsGroup: true, Now: time.Now()})
	if err == nil {
		t.Fatal("连接失败应报错")
	}
	if !strings.Contains(err.Error(), "Replyer") {
		t.Errorf("错误信息应带上下文，实际 %v", err)
	}
}

// TestReplyerTellsModelAboutAttachments 守「附件要告诉 Replyer」。
//
// 不说的后果很具体：模型不知道那些图/表情会被系统单独发出去，
// 于是正文里写「（图）」「[表情]」，或者干脆描述一张它自己杜撰的图。
// 参考实现对应 maisaka_generator_base.py:351 _build_reply_attachment_prompt。
func TestReplyerTellsModelAboutAttachments(t *testing.T) {
	b := newTestPromptBuilder(t, newTestConfig())
	base := time.Date(2026, 10, 24, 13, 0, 0, 0, time.UTC)

	history := []*domain.Message{{
		MessageID: "m-1", UserID: "u1", UserName: "小明",
		PlainText: "发张图", ReceivedAt: base,
	}}

	msgs, _, err := b.BuildReplyerMessages(ReplyerPromptInput{
		IsGroup:   true,
		History:   history,
		Reference: "回应他的请求",
		Now:       base,
		AttachPics: []tool.AttachPic{
			{MsgID: "m-1", Index: 0},
			{MsgID: "m-1", Index: 1},
		},
		AttachEmoji: "无语",
	})
	if err != nil {
		t.Fatalf("组装失败: %v", err)
	}

	// 找末尾块那条 user 消息。
	final := ""
	for _, m := range msgs {
		if m.Role == llm.RoleUser && strings.Contains(m.Content, "当前时间：") {
			final = m.Content
		}
	}
	if final == "" {
		t.Fatal("没找到末尾块")
	}

	// 三件事都要说出来：有两张图、有表情包、这是系统另外发的。
	for _, want := range []string{
		"【额外发送内容参考】",
		"图片",
		"第 1 张图片", "第 2 张图片",
		"和",
		"无语",
	} {
		if !strings.Contains(final, want) {
			t.Errorf("末尾块里缺少 %q：%s", want, final)
		}
	}

	// 附件段必须在「只输出正文」之前 —— 那格是模型最看重的格式约束。
	if strings.Index(final, "【额外发送内容参考】") > strings.Index(final, prompt.ReplyerReplyInstruction) {
		t.Errorf("附件段跑到格式约束之后了：%s", final)
	}
}

// TestReplyerWithoutAttachmentsHasNoAttachmentSection 守「没附件时不出现空标题」。
//
// 留一个「【额外发送内容参考】」下面什么都没有，模型会以为这次要发点什么，
// 然后凭空描述一个附件。
func TestReplyerWithoutAttachmentsHasNoAttachmentSection(t *testing.T) {
	b := newTestPromptBuilder(t, newTestConfig())
	base := time.Date(2026, 10, 24, 13, 0, 0, 0, time.UTC)

	msgs, _, err := b.BuildReplyerMessages(ReplyerPromptInput{
		IsGroup:   true,
		History:   []*domain.Message{{MessageID: "m-1", UserID: "u1", UserName: "小明", PlainText: "hi", ReceivedAt: base}},
		Reference: "回应",
		Now:       base,
	})
	if err != nil {
		t.Fatalf("组装失败: %v", err)
	}
	for _, m := range msgs {
		if strings.Contains(m.Content, "【额外发送内容参考】") {
			t.Errorf("没附件时不该出现附件段：%s", m.Content)
		}
	}
}

// TestAttachmentPromptFallsBackWhenTemplateMissing 守模板缺失时的兜底。
//
// 这里宁可少一层措辞，也不能返回空串：空串意味着模型完全不知道
// 有附件要发，就会在正文里开始描述一张自己杜撰的图。
func TestAttachmentPromptFallsBackWhenTemplateMissing(t *testing.T) {
	b := &PromptBuilder{cfg: config.Wrap(newTestConfig())} // prompts 为 nil

	got := b.renderAttachmentPrompt("当前文字回复后还会另外发送图片。")
	if got == "" {
		t.Fatal("模板缺失时不该返回空串")
	}
	if !strings.Contains(got, "还会另外发送图片") {
		t.Errorf("兜底内容应当是 attachments 原文：%q", got)
	}
}

// TestAttachmentBlockWithoutAttachments 守空输入不产出任何段落。
func TestAttachmentBlockWithoutAttachments(t *testing.T) {
	b := newTestPromptBuilder(t, newTestConfig())
	if got := b.attachmentBlock(nil, "  "); got != "" {
		t.Errorf("没有附件时不该产出内容：%q", got)
	}
}
