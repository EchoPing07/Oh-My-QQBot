package agent

// prompt.go 的测试。
//
// 这一层最容易出的错不是崩溃，而是「提示词里出现了不该出现的规则」——
// 比如照抄了 query_memory 的说明但工具没注册，模型会调用一个不存在的工具、
// 失败、然后 wait，白白浪费一轮（AGENTS.md 第 14 条）。
// 所以这里的断言以「某段文字存在 / 不存在」为主。

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/llm"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/prompt"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/tool"
)

// newTestPromptManager 返回一个加载了内嵌模板的 PromptManager。
func newTestPromptManager(t *testing.T) *prompt.Manager {
	t.Helper()
	pm, err := prompt.New("", "zh-CN")
	if err != nil {
		t.Fatalf("加载提示词模板失败: %v", err)
	}
	return pm
}

// newTestPromptBuilder 构造一个可用的 PromptBuilder。
//
// prompts 目录传空串，走二进制内嵌模板 —— 测试不该依赖工作目录下的 prompts/。
func newTestPromptBuilder(t *testing.T, cfg *config.Config) *PromptBuilder {
	t.Helper()
	return NewPromptBuilder(config.Wrap(cfg), newTestPromptManager(t))
}

// newTestConfig 返回一份默认可用的配置。
//
// 默认配置里模型列表为空，ModelIsVisual 会返回 false —— 这对大多数测试是想要的
// （多模态默认关闭）。
func newTestConfig() *config.Config {
	cfg := config.Default()
	cfg.Bot.Nickname = "木木"
	cfg.Emoji.SendProbability = 0
	return cfg
}

// testAvailability 是群聊场景的工具可用性。
func testAvailability() tool.Availability {
	return tool.Availability{IsGroup: true, Platform: "qq"}
}

// testPNG 返回一张合法的 1x1 PNG。
//
// 不能拿随机字节冒充图片：imagesOf 会先过 imgtx.Normalize，
// 认不出来的字节会被丢弃 —— 这是刻意的，避免把模型吃不下的格式发出去。
func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("构造测试 PNG 失败: %v", err)
	}
	return buf.Bytes()
}

// TestBuildSystemPromptMemoryRuleFollowsRegistry 验证「提示词只描述真的注册过的工具」。
func TestBuildSystemPromptMemoryRuleFollowsRegistry(t *testing.T) {
	cfg := newTestConfig()
	b := newTestPromptBuilder(t, cfg)
	av := testAvailability()

	// 没注册：不该出现 query_memory 的规则。
	empty := tool.NewRegistry()
	empty.MustRegister(tool.NewWaitTool())

	got, err := b.BuildSystemPrompt(true, av, empty)
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	if strings.Contains(got, "query_memory()") {
		t.Errorf("没注册 query_memory 时不该出现它的说明")
	}

	// 注册了：群聊措辞里必须有「不要把私聊或个人隐私记忆带到群聊里」。
	withMemory := tool.NewRegistry()
	withMemory.MustRegister(tool.NewQueryMemoryTool())

	got, err = b.BuildSystemPrompt(true, av, withMemory)
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	if !strings.Contains(got, "query_memory()") {
		t.Errorf("注册了 query_memory 却没有说明")
	}
	if !strings.Contains(got, "不要把私聊或个人隐私记忆带到群聊里") {
		t.Errorf("群聊措辞不对")
	}
}

// TestBuildSystemPromptNilRegistry 验证 registry 为 nil 时不 panic 且不含记忆规则。
func TestBuildSystemPromptNilRegistry(t *testing.T) {
	b := newTestPromptBuilder(t, newTestConfig())

	got, err := b.BuildSystemPrompt(false, testAvailability(), nil)
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	if strings.Contains(got, "query_memory()") {
		t.Errorf("registry 为 nil 时不该出现记忆规则")
	}
	if !strings.Contains(got, "木木") {
		t.Errorf("系统提示词里应出现机器人名字")
	}
}

// TestMemoryRuleForWording 验证群聊与私聊措辞不同。
//
// 两套措辞是参考实现刻意分开的：群聊要防隐私外溢，私聊要鼓励主动回忆。
// 若哪天被「统一」成一套，这条测试会失败。
func TestMemoryRuleForWording(t *testing.T) {
	reg := tool.NewRegistry()
	reg.MustRegister(tool.NewQueryMemoryTool())
	av := testAvailability()

	group := memoryRuleFor(reg, av, true)
	private := memoryRuleFor(reg, av, false)

	if group == "" || private == "" {
		t.Fatalf("注册了工具就该有规则，群聊=%q 私聊=%q", group, private)
	}
	if group == private {
		t.Errorf("群聊与私聊措辞不该相同：%q", group)
	}
	if !strings.Contains(group, "隐私") {
		t.Errorf("群聊规则应提到隐私：%q", group)
	}
	if !strings.Contains(private, "更积极检索") {
		t.Errorf("私聊规则应鼓励更积极检索：%q", private)
	}

	// 没注册时必须为空串，不能返回一个空标题块。
	if got := memoryRuleFor(tool.NewRegistry(), av, true); got != "" {
		t.Errorf("未注册时应为空，实际 %q", got)
	}
	if got := memoryRuleFor(nil, av, true); got != "" {
		t.Errorf("registry 为 nil 时应为空，实际 %q", got)
	}
}

// TestBuildReplyerSystemPromptFallsBackToConfig 验证空 replyStyle 回退到配置。
func TestBuildReplyerSystemPromptFallsBackToConfig(t *testing.T) {
	cfg := newTestConfig()
	cfg.Personality.ReplyStyle = "配置里的默认风格"
	b := newTestPromptBuilder(t, cfg)

	got, err := b.BuildReplyerSystemPrompt(true, "")
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	if !strings.Contains(got, "配置里的默认风格") {
		t.Errorf("空 replyStyle 应回退到配置，实际 %q", got)
	}

	// 显式传入时优先用传入值。
	got, err = b.BuildReplyerSystemPrompt(true, "本次的临时风格")
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	if !strings.Contains(got, "本次的临时风格") {
		t.Errorf("应使用传入的 replyStyle，实际 %q", got)
	}
	if strings.Contains(got, "配置里的默认风格") {
		t.Errorf("传入值优先时不该再出现配置风格")
	}

	// 输出格式说明必须始终在末尾，否则模型会带上前后缀。
	if !strings.Contains(got, prompt.ReplyerOutputInstruction) {
		t.Errorf("缺少输出格式说明")
	}
}

// TestSelectReplyStyle 验证概率 0 / 1 时的行为。
func TestSelectReplyStyle(t *testing.T) {
	cfg := newTestConfig()
	cfg.Personality.MultipleReplyStyle = []string{"风格A", "风格B", "风格C"}
	b := newTestPromptBuilder(t, cfg)
	const base = "基础风格"

	// 概率 0：永远返回基础风格。
	cfg.Personality.MultipleProbability = 0
	for i := 0; i < 50; i++ {
		if got := b.selectReplyStyle(base); got != base {
			t.Fatalf("概率 0 时应返回基础风格，第 %d 次得到 %q", i, got)
		}
	}

	// 概率 1：每次都返回某个备用风格，且不能带出 base。
	cfg.Personality.MultipleProbability = 1
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		got := b.selectReplyStyle(base)
		if got == base {
			t.Fatalf("概率 1 时不该返回基础风格")
		}
		if !strings.HasPrefix(got, "风格") {
			t.Fatalf("返回值不是备用风格之一：%q", got)
		}
		seen[got] = true
	}
	// 100 次抽样至少要覆盖到 3 个里的 2 个，否则说明取模写错了。
	if len(seen) < 2 {
		t.Errorf("100 次抽样只出现了 %d 种风格，随机取模可能有问题", len(seen))
	}

	// 备用风格为空时即使概率 1 也回退，避免返回空串把提示词打断。
	cfg.Personality.MultipleReplyStyle = nil
	if got := b.selectReplyStyle(base); got != base {
		t.Errorf("没有备用风格时应回退到基础风格，实际 %q", got)
	}
}

// TestBuildPlannerMessagesWithProfiles 验证画像消息的插入位置。
//
// 位置错位不会报错，只会让模型把画像当成「对所有人的描述」，
// 或者被末尾的 reminder 盖过去 —— 两种都表现为「画像好像没起作用」。
func TestBuildPlannerMessagesWithProfiles(t *testing.T) {
	cfg := newTestConfig()
	b := newTestPromptBuilder(t, cfg)
	now := time.Date(2026, 10, 21, 13, 5, 0, 0, time.UTC)

	history := []*domain.Message{
		newRenderTestMessage("m-1", "小明", domain.Text("在吗")),
	}
	reg := tool.NewRegistry()
	reg.MustRegister(tool.NewWaitTool())

	const block = "【人物画像-内部参考】\n小明：\n  测试画像"

	msgs, err := b.BuildPlannerMessagesWithProfiles(true, history, nil, now, testAvailability(), reg, block)
	if err != nil {
		t.Fatalf("组装失败: %v", err)
	}

	// 顺序应当是：system → 历史 → 画像 → 当前时间 → reminder。
	if msgs[0].Role != llm.RoleSystem {
		t.Fatalf("第一条必须是 system，实际 %q", msgs[0].Role)
	}
	timeIdx, profileIdx, reminderIdx := -1, -1, -1
	for i, m := range msgs {
		if strings.HasPrefix(m.Content, "当前时间：") {
			timeIdx = i
		}
		if strings.HasPrefix(m.Content, "【人物画像-内部参考】") {
			profileIdx = i
		}
		if m.Content == prompt.PlannerFinalReminder {
			reminderIdx = i
		}
	}
	if profileIdx < 0 {
		t.Fatal("画像消息没有被插入")
	}
	if profileIdx >= timeIdx {
		t.Errorf("画像应在当前时间之前，画像=%d 时间=%d", profileIdx, timeIdx)
	}
	if reminderIdx != timeIdx+1 {
		t.Errorf("当前时间之后必须紧跟 reminder，时间=%d reminder=%d", timeIdx, reminderIdx)
	}
	if reminderIdx != len(msgs)-1 {
		t.Errorf("reminder 必须是最后一条，实际 %d/%d", reminderIdx, len(msgs)-1)
	}

	// profiles 为空时不该插入额外消息。
	without, err := b.BuildPlannerMessagesWithProfiles(true, history, nil, now, testAvailability(), reg, "   ")
	if err != nil {
		t.Fatalf("组装失败: %v", err)
	}
	if len(without) != len(msgs)-1 {
		t.Errorf("空画像时不该多出消息，有画像 %d 条，无画像 %d 条", len(msgs), len(without))
	}
	for _, m := range without {
		if strings.Contains(m.Content, "【人物画像-内部参考】") {
			t.Errorf("空画像时不该出现画像块")
		}
	}
}

// TestBuildPlannerMessagesOrdersHistoryAndBotReplies 验证历史与自述按时间合并。
func TestBuildPlannerMessagesOrdersHistoryAndBotReplies(t *testing.T) {
	b := newTestPromptBuilder(t, newTestConfig())
	base := time.Date(2026, 10, 21, 13, 0, 0, 0, time.UTC)

	history := []*domain.Message{
		{MessageID: "m-1", UserID: "u1", UserName: "小明", PlainText: "第一句", ReceivedAt: base},
		{MessageID: "m-3", UserID: "u2", UserName: "小红", PlainText: "第三句", ReceivedAt: base.Add(2 * time.Minute)},
	}
	// 机器人自述夹在两条外部消息之间，排序后应回到中间。
	botReplies := []BotReply{{Text: "第二句", At: base.Add(time.Minute)}}

	msgs, err := b.BuildPlannerMessages(true, history, botReplies, base.Add(3*time.Minute), testAvailability(), tool.NewRegistry())
	if err != nil {
		t.Fatalf("组装失败: %v", err)
	}

	var bodies []string
	for _, m := range msgs {
		if strings.HasPrefix(m.Content, "<message ") {
			bodies = append(bodies, m.Content)
		}
	}
	if len(bodies) != 3 {
		t.Fatalf("应有 3 条聊天消息，实际 %d 条", len(bodies))
	}
	if !strings.Contains(bodies[0], "第一句") || !strings.Contains(bodies[1], "第二句") || !strings.Contains(bodies[2], "第三句") {
		t.Errorf("时间顺序不对：%v", bodies)
	}
	if !strings.Contains(bodies[1], `is_self_message="true"`) {
		t.Errorf("机器人自述应带标记：%q", bodies[1])
	}
}

// TestBuildPlannerMessagesAttachesImagesOnlyWhenVisual 守住多模态开关。
//
// 给纯文本模型塞 image_url 会被服务端 400，整轮思考失败 ——
// 比看不到图严重得多（AGENTS.md 第 19 条）。
func TestBuildPlannerMessagesAttachesImagesOnlyWhenVisual(t *testing.T) {
	cfg := newTestConfig()
	// 显式配置一个视觉模型。
	cfg.LLM.Providers = []config.ProviderConfig{{Name: "p", BaseURL: "http://127.0.0.1:1"}}
	cfg.LLM.Models = []config.ModelConfig{{Name: "m", Provider: "p", Visual: true}}
	cfg.LLM.Tasks.Planner.Models = []string{"m"}

	history := []*domain.Message{{
		MessageID:  "m-img",
		UserID:     "u1",
		UserName:   "小明",
		ReceivedAt: time.Now(),
		Segments:   []domain.Segment{{Type: domain.SegmentImage, Binary: testPNG(t)}},
	}}

	if !cfg.ModelIsVisual(config.TaskPlanner) {
		t.Fatal("配置应当是视觉模型")
	}
	b := newTestPromptBuilder(t, cfg)
	msgs, err := b.BuildPlannerMessages(true, history, nil, time.Now(), testAvailability(), tool.NewRegistry())
	if err != nil {
		t.Fatalf("组装失败: %v", err)
	}
	if !anyImages(msgs) {
		t.Errorf("视觉模型应收到图片")
	}

	// 关掉 visual：不该再挂图片。
	cfg.LLM.Models[0].Visual = false
	b = newTestPromptBuilder(t, cfg)
	msgs, err = b.BuildPlannerMessages(true, history, nil, time.Now(), testAvailability(), tool.NewRegistry())
	if err != nil {
		t.Fatalf("组装失败: %v", err)
	}
	if anyImages(msgs) {
		t.Errorf("纯文本模型不该收到图片")
	}
}

// anyImages 判断消息列表里是否有任意一条挂了图。
func anyImages(msgs []llm.Message) bool {
	for _, m := range msgs {
		if len(m.Images) > 0 {
			return true
		}
	}
	return false
}

// TestImagesOf 验证图片转 data URL 的细节。
func TestImagesOf(t *testing.T) {
	pngBytes := testPNG(t)

	if got := imagesOf(nil); got != nil {
		t.Errorf("nil 消息应返回 nil，实际 %v", got)
	}
	if got := imagesOf(&domain.Message{PlainText: "只有文字"}); len(got) != 0 {
		t.Errorf("无图应返回空，实际 %v", got)
	}

	// PNG 原样直传：不重编码，避免有损二次压缩。
	// 断言的是按魔数识别出的 MIME，而不是段里写的 MimeType ——
	// QQ 给的 contentType 经常是 application/octet-stream，不可信。
	got := imagesOf(&domain.Message{Segments: []domain.Segment{{
		Type: domain.SegmentImage, Binary: pngBytes,
	}}})
	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngBytes)
	if len(got) != 1 || got[0] != want {
		t.Errorf("PNG 直传处理不对，实际 %v", got)
	}

	// 表情包走同一条通道。
	got = imagesOf(&domain.Message{Segments: []domain.Segment{{
		Type: domain.SegmentEmoji, Binary: pngBytes,
	}}})
	if len(got) != 1 || got[0] != want {
		t.Errorf("表情包也应被转成 data URL，实际 %v", got)
	}

	// 没有 Binary 的图（只给了 URL 还没下载）要跳过，不能发空 data URL。
	got = imagesOf(&domain.Message{Segments: []domain.Segment{{
		Type: domain.SegmentImage, URL: "https://example.com/a.png",
	}}})
	if len(got) != 0 {
		t.Errorf("无二进制应跳过，实际 %v", got)
	}

	// 模型吃不下的格式必须丢弃，绝不能退回原始字节 ——
	// 那会让服务端 400，整轮思考失败。
	got = imagesOf(&domain.Message{Segments: []domain.Segment{{
		Type: domain.SegmentImage, Binary: []byte("not an image at all"),
	}}})
	if len(got) != 0 {
		t.Errorf("无法识别的格式应丢弃，实际 %v", got)
	}

	// 最多 4 张：再多会显著拉高 token 与延迟。
	segs := make([]domain.Segment, 0, 7)
	for i := 0; i < 7; i++ {
		segs = append(segs, domain.Segment{Type: domain.SegmentImage, Binary: pngBytes})
	}
	got = imagesOf(&domain.Message{Segments: segs})
	if len(got) != 4 {
		t.Errorf("最多 4 张，实际 %d 张", len(got))
	}
}

// TestBuildReplyerMessages 验证 Replyer 请求的结构。
//
// 关键约定（每一条都对应过一次真实 bug）：
//   - 历史不截断，Replyer 要靠完整措辞
//   - 机器人自己的发言是 assistant 角色（不是 user）
//   - 末尾块是「当前时间 + 目标消息 + 只输出正文」，**不能是 Planner 提醒**
//   - 篇幅要求作为最后一条独立 user 消息
func TestBuildReplyerMessages(t *testing.T) {
	b := newTestPromptBuilder(t, newTestConfig())
	base := time.Date(2026, 10, 21, 13, 0, 0, 0, time.UTC)

	long := strings.Repeat("很长的历史", 100)
	history := []*domain.Message{{
		MessageID: "m-1", UserID: "u1", UserName: "小明",
		PlainText: long, ReceivedAt: base,
	}}

	msgs, count, err := b.BuildReplyerMessages(ReplyerPromptInput{
		IsGroup:     true,
		History:     history,
		Reference:   "回应他的问题",
		Style:       "简短表达",
		BaseStyle:   "配置里的基础风格",
		Now:         base,
		TargetMsgID: "m-1",
	})
	if err != nil {
		t.Fatalf("组装失败: %v", err)
	}
	if count != 1 {
		t.Errorf("历史条数 = %d，期望 1", count)
	}
	if msgs[0].Role != llm.RoleSystem {
		t.Errorf("第一条必须是 system")
	}

	var body string
	for _, m := range msgs {
		if strings.Contains(m.Content, long) {
			body = m.Content
		}
	}
	if body == "" {
		t.Fatal("Replyer 不该截断历史，完整正文必须出现")
	}

	// 回归：末尾绝不能是 Planner 的决策提醒。
	// 那句里带「不调用工具」「使用reply回复」，Replyer 会照抄成正文开头。
	for i, m := range msgs {
		if strings.Contains(m.Content, prompt.PlannerFinalReminder) {
			t.Errorf("第 %d 条消息含 PlannerFinalReminder，Replyer 不该看到它：%q", i, m.Content)
		}
	}

	// 末尾块：当前时间 → 目标消息 → 只输出正文。
	last := msgs[len(msgs)-1]
	if last.Role != llm.RoleUser {
		t.Errorf("末尾必须是 user 消息，实际 %q", last.Role)
	}
	if last.Content != prompt.ReplyerStyleHint("简短表达") {
		t.Errorf("有篇幅要求时最后一条应是它，实际 %q", last.Content)
	}

	final := msgs[len(msgs)-2]
	for _, want := range []string{
		"当前时间：",
		"你想要回复的消息是 小明 发送的 msg_id为 m-1 的消息",
		prompt.ReplyerReplyInstruction,
	} {
		if !strings.Contains(final.Content, want) {
			t.Errorf("末尾块缺少 %q，实际 %q", want, final.Content)
		}
	}

	// 回复指引在末尾块之前。
	if !strings.Contains(msgs[len(msgs)-3].Content, "【回复信息参考】") {
		t.Errorf("回复指引应在末尾块之前")
	}

	// system 里必须是配置的基础风格，不能被篇幅要求覆盖。
	if !strings.Contains(msgs[0].Content, "配置里的基础风格") {
		t.Errorf("system 应使用基础风格：%q", msgs[0].Content)
	}

	// 没有回复指引时不该凭空造一个空的参考块。
	// 注意只看 user 消息：replyer 模板的 system 段本身就提了一句
	// 「你可以参考【回复信息参考】中的信息」，那是模板文字、不是注入块。
	msgs, _, err = b.BuildReplyerMessages(ReplyerPromptInput{
		IsGroup: true, History: history, Now: base,
	})
	if err != nil {
		t.Fatalf("组装失败: %v", err)
	}
	for _, m := range msgs {
		if m.Role == llm.RoleUser && strings.Contains(m.Content, "【回复信息参考】") {
			t.Errorf("空指引时不该出现参考块：%q", m.Content)
		}
	}
	// 「正常回复」/空 style 不该追加篇幅消息。
	if got := msgs[len(msgs)-1].Content; strings.Contains(got, "请简短的回复") {
		t.Errorf("空 style 不该带篇幅要求：%q", got)
	}
}

// TestBuildReplyerMessagesBotRepliesAreAssistant 锁住「机器人自己的发言是 assistant」。
//
// 参考实现 maisaka_generator_base.py:638-644 把 guided_reply 转成 RoleType.Assistant。
// 我们以前全部当 user，模型于是把自发言当成「别人/自己刚说过的话」照着模仿，
// 是指令回声自我强化的根因之一（实测把自发言改成 assistant 后逐轮不再复制）。
func TestBuildReplyerMessagesBotRepliesAreAssistant(t *testing.T) {
	b := newTestPromptBuilder(t, newTestConfig())
	base := time.Date(2026, 10, 21, 13, 0, 0, 0, time.UTC)

	history := []*domain.Message{{
		MessageID: "m-1", UserID: "u1", UserName: "小明",
		PlainText: "火别太旺", ReceivedAt: base,
	}}
	botReplies := []BotReply{
		{Text: "我盯着呢", At: base.Add(time.Second)},
		{Text: "糊不了", At: base.Add(2 * time.Second)},
	}

	msgs, count, err := b.BuildReplyerMessages(ReplyerPromptInput{
		IsGroup: true, History: history, BotReplies: botReplies, Now: base,
	})
	if err != nil {
		t.Fatalf("组装失败: %v", err)
	}
	if count != 3 {
		t.Errorf("历史条数 = %d，期望 3（1 条外部 + 2 条自发言）", count)
	}

	var assistants []string
	for _, m := range msgs {
		if m.Role == llm.RoleAssistant {
			assistants = append(assistants, m.Content)
		}
	}
	if len(assistants) != 2 {
		t.Fatalf("应有 2 条 assistant 消息，实际 %d：%v", len(assistants), assistants)
	}
	// assistant 消息里只能是人话，不能带 <message> 标签。
	for _, a := range assistants {
		if strings.Contains(a, "<message") || strings.Contains(a, "is_self_message") {
			t.Errorf("assistant 消息不该带 XML 包裹：%q", a)
		}
	}
	if assistants[0] != "我盯着呢" || assistants[1] != "糊不了" {
		t.Errorf("assistant 消息顺序不对：%v", assistants)
	}
}

// TestBuildReplyerMessagesTargetSelf 验证「回复自己发过的消息」走专用分支。
func TestBuildReplyerMessagesTargetSelf(t *testing.T) {
	b := newTestPromptBuilder(t, newTestConfig())
	base := time.Date(2026, 10, 21, 13, 0, 0, 0, time.UTC)

	history := []*domain.Message{{
		MessageID: "m-self", IsSelf: true, UserName: "木木",
		PlainText: "我先说的", ReceivedAt: base,
	}}
	msgs, _, err := b.BuildReplyerMessages(ReplyerPromptInput{
		IsGroup: true, History: history, Now: base, TargetMsgID: "m-self",
	})
	if err != nil {
		t.Fatalf("组装失败: %v", err)
	}

	var final string
	for _, m := range msgs {
		if strings.Contains(m.Content, "当前时间：") {
			final = m.Content
		}
	}
	if !strings.Contains(final, "你想要补充说明你自己（木木）") {
		t.Errorf("回复自己的消息应用 self 分支：%q", final)
	}
}

// TestBuildReplyerMessagesUnknownTarget 验证查不到目标时不编空块。
func TestBuildReplyerMessagesUnknownTarget(t *testing.T) {
	b := newTestPromptBuilder(t, newTestConfig())
	msgs, _, err := b.BuildReplyerMessages(ReplyerPromptInput{
		IsGroup: true, Now: time.Now(), TargetMsgID: "不存在的-id",
	})
	if err != nil {
		t.Fatalf("组装失败: %v", err)
	}
	for _, m := range msgs {
		if strings.Contains(m.Content, "你想要回复的消息是") {
			t.Errorf("查不到目标时不该出现目标块：%q", m.Content)
		}
	}
	// 但末尾块必须还在（否则模型没有格式约束）。
	if !strings.Contains(msgs[len(msgs)-1].Content, prompt.ReplyerReplyInstruction) {
		t.Errorf("末尾块不能丢：%q", msgs[len(msgs)-1].Content)
	}
}

// TestSelectReplyStyleMixedProbability 覆盖「概率介于 0 和 1 之间」的分支。
//
// 概率 0 与 1 之外还有一条「抽签不中就用基础风格」的路径，
// 它不被覆盖时，概率配置写错（例如填了 0.5）可能完全不起作用也没人发现。
func TestSelectReplyStyleMixedProbability(t *testing.T) {
	cfg := newTestConfig()
	cfg.Personality.MultipleReplyStyle = []string{"备用甲", "备用乙"}
	cfg.Personality.MultipleProbability = 0.5
	b := newTestPromptBuilder(t, cfg)
	const base = "基础风格"

	baseCount, altCount := 0, 0
	for i := 0; i < 400; i++ {
		switch got := b.selectReplyStyle(base); got {
		case base:
			baseCount++
		case "备用甲", "备用乙":
			altCount++
		default:
			t.Fatalf("出现了意料之外的风格：%q", got)
		}
	}
	// 宽松区间：只要两边都出现过，说明分支都走到了。
	if baseCount == 0 || altCount == 0 {
		t.Errorf("概率 0.5 时两边都应出现：基础 %d 次、备用 %d 次", baseCount, altCount)
	}
	if baseCount+altCount != 400 {
		t.Errorf("统计漏了调用")
	}
}

// TestImagesOfSkipsNonMediaSegments 验证非图片段被跳过。
func TestImagesOfSkipsNonMediaSegments(t *testing.T) {
	pngBytes := testPNG(t)
	msg := &domain.Message{Segments: []domain.Segment{
		domain.Text("只有文字"),
		domain.At("u1", "小明"),
		{Type: domain.SegmentVoice, Binary: pngBytes},
		{Type: domain.SegmentFile, Binary: pngBytes},
		{Type: domain.SegmentImage, Binary: pngBytes},
	}}

	got := imagesOf(msg)
	if len(got) != 1 {
		t.Errorf("应只取 image/emoji 段，实际得到 %d 张", len(got))
	}
	if !strings.HasPrefix(got[0], "data:image/png;base64,") {
		t.Errorf("返回的不是 data URL：%q", got[0])
	}
}

// TestReplyerPromptNeverUsesPlannerReminder 是源码级硬约束。
//
// 这是本轮 bug 的最短复现路径：BuildReplyerMessages 里一旦出现
// prompt.PlannerFinalReminder，Replyer 就会把「不调用工具」当指令照抄，
// 产出「不调用工具，直接回。\n\n<正文>」并入库、下一轮被当范文放大。
//
// 为什么用源码扫描而不是行为测试：行为测试要跑模型才有概率命中
// （同一份历史 10 次里泄漏 4~5 次），而这里是确定性的 ——
// 只要那句提醒没被写进 Replyer 分支，就永远不会回到这个形态。
func TestReplyerPromptNeverUsesPlannerReminder(t *testing.T) {
	src, err := os.ReadFile("prompt.go")
	if err != nil {
		t.Fatalf("读取 prompt.go 失败: %v", err)
	}
	text := string(src)

	// 定位 BuildReplyerMessages 的函数体：从函数签名到下一个顶层 func。
	start := strings.Index(text, "func (b *PromptBuilder) BuildReplyerMessages(")
	if start < 0 {
		t.Fatal("找不到 BuildReplyerMessages")
	}
	body := text[start:]
	if end := strings.Index(body[1:], "\nfunc "); end >= 0 {
		body = body[:end+1]
	}

	if strings.Contains(body, "PlannerFinalReminder") {
		t.Error("BuildReplyerMessages 里出现了 PlannerFinalReminder —— " +
			"那句带「不调用工具」，Replyer 会照抄成正文开头")
	}
	// 正向断言：必须用正确的尾注。删掉它模型会开始带前后缀。
	if !strings.Contains(body, "ReplyerFinalUserMessage") {
		t.Error("BuildReplyerMessages 必须用 ReplyerFinalUserMessage 作末尾块")
	}
}
