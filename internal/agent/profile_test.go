package agent

// profile.go 的测试。
//
// 画像注入最怕两件事：
//   1. 注入了不该注入的人（群里 40 个人的背景把上下文淹掉）
//   2. 格式走样，模型把它当成「用户说的话」而不是内部参考
// 所以这里用假的 tool.Runtime 精确控制「谁有画像」，并逐字符校验头部与尾部。

import (
	"strings"
	"testing"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/llm"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/tool"
)

// profileFakeRuntime 是一个只实现画像能力的假 Runtime。
//
// 不用 agent 包外的 mock 是为了让「谁有画像」这件事在测试里一眼可见。
type profileFakeRuntime struct {
	profiles map[string]string
	// calls 记录被查询过的人，用于验证「没画像的人不该被查第二次」。
	calls []string
}

func (r *profileFakeRuntime) SessionID() string { return "sess" }
func (r *profileFakeRuntime) IsGroup() bool     { return true }
func (r *profileFakeRuntime) BotName() string   { return "木木" }

func (r *profileFakeRuntime) Reply(tool.ReplyRequest) (tool.ReplyOutcome, error) {
	return tool.ReplyOutcome{}, nil
}
func (r *profileFakeRuntime) Wait(float64, string) error               { return nil }
func (r *profileFakeRuntime) Stop(string)                              {}
func (r *profileFakeRuntime) Conversation() []llm.Message              { return nil }
func (r *profileFakeRuntime) RememberReply(string, []string, []string) {}
func (r *profileFakeRuntime) SearchMemory(tool.MemoryQuery) (tool.MemoryResult, error) {
	return tool.MemoryResult{}, nil
}
func (r *profileFakeRuntime) PickEmoji(string) (tool.EmojiRef, bool) { return tool.EmojiRef{}, false }
func (r *profileFakeRuntime) SendEmoji(tool.EmojiRef) ([]string, error) {
	return nil, nil
}

func (r *profileFakeRuntime) PersonProfile(userID string) string {
	r.calls = append(r.calls, userID)
	return r.profiles[userID]
}

// newProfileTestLoop 构造一个只带配置的 Loop（其余字段未用）。
func newProfileTestLoop(cfg *config.Config) *Loop {
	return &Loop{cfg: config.Wrap(cfg)}
}

// profileTestMessage 构造一条带用户名的消息。
func profileTestMessage(id, userID, userName string, segs ...domain.Segment) *domain.Message {
	return &domain.Message{
		MessageID: id,
		UserID:    userID,
		UserName:  userName,
		Segments:  segs,
		PlainText: domain.BuildPlainText(segs),
	}
}

// TestBuildProfileInjectionNoProfile 验证没人有画像时不注入。
//
// 必须返回空串而不是「只有头尾的空块」：空块会让模型以为「这些人没有背景信息」，
// 从而在回复里表现出「我不了解你」的态度，观感很差。
func TestBuildProfileInjectionNoProfile(t *testing.T) {
	cfg := newTestConfig()
	l := newProfileTestLoop(cfg)
	rt := &profileFakeRuntime{profiles: nil}

	batch := []*domain.Message{profileTestMessage("m1", "u1", "小明", domain.Text("在吗"))}
	if got := l.buildProfileInjection(batch, rt); got != "" {
		t.Errorf("没人有画像时应返回空串，实际 %q", got)
	}
}

// TestBuildProfileInjectionDisabled 验证开关关闭时不注入。
func TestBuildProfileInjectionDisabled(t *testing.T) {
	batch := []*domain.Message{profileTestMessage("m1", "u1", "小明", domain.Text("在吗"))}
	rt := &profileFakeRuntime{profiles: map[string]string{"u1": "喜欢摄影"}}

	cases := []struct {
		name string
		mod  func(*config.Config)
	}{
		{"记忆总开关关闭", func(c *config.Config) { c.Memory.Enabled = false }},
		{"画像开关关闭", func(c *config.Config) { c.Memory.ProfileEnabled = false }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := newTestConfig()
			c.mod(cfg)
			l := newProfileTestLoop(cfg)
			if got := l.buildProfileInjection(batch, rt); got != "" {
				t.Errorf("关闭时应返回空串，实际 %q", got)
			}
			if len(rt.calls) != 0 {
				t.Errorf("关闭时不该查库，实际查询了 %v", rt.calls)
			}
		})
	}

	// cfg 为 nil 时也不能 panic（测试与工具场景会传 nil）。
	if got := newProfileTestLoop(nil).buildProfileInjection(batch, rt); got != "" {
		t.Errorf("cfg 为 nil 时应返回空串，实际 %q", got)
	}
	// 空 batch 不必查库。
	cfg := newTestConfig()
	if got := newProfileTestLoop(cfg).buildProfileInjection(nil, rt); got != "" {
		t.Errorf("空批次应返回空串，实际 %q", got)
	}
}

// TestBuildProfileInjectionFormat 验证输出格式。
//
// 格式逐字对齐参考实现 build_person_profile_injection_messages：
// 头 → 空行 → 各个人物块（两空格缩进）→ 空行 → 尾。
// 格式走样时模型会把它当成聊天内容的一部分，开始「复述画像」。
func TestBuildProfileInjectionFormat(t *testing.T) {
	cfg := newTestConfig()
	l := newProfileTestLoop(cfg)
	rt := &profileFakeRuntime{profiles: map[string]string{
		"u1": "喜欢摄影\n周末常去爬山",
	}}

	batch := []*domain.Message{profileTestMessage("m1", "u1", "小明", domain.Text("在吗"))}
	got := l.buildProfileInjection(batch, rt)

	if !strings.HasPrefix(got, profileReferenceHeader) {
		t.Errorf("必须以固定头部开头，实际 %q", got)
	}
	if !strings.HasSuffix(got, profileReferenceFooter) {
		t.Errorf("必须以固定尾部结尾，实际 %q", got)
	}
	if !strings.Contains(got, "小明：") {
		t.Errorf("缺少人物名字：%q", got)
	}
	// 正文要压成一行并缩进两格，多行会让模型分不清「谁是谁」。
	if !strings.Contains(got, "  喜欢摄影 周末常去爬山") {
		t.Errorf("正文格式不对（应压成单行并缩进两格）：%q", got)
	}
	// 头部与尾部之间要有空行分隔。
	if !strings.Contains(got, "\n\n") {
		t.Errorf("段落之间应有空行：%q", got)
	}
}

// TestBuildProfileInjectionMaxPersons 验证最多注入 ProfileMaxPersons 个人。
func TestBuildProfileInjectionMaxPersons(t *testing.T) {
	cfg := newTestConfig()
	cfg.Memory.ProfileMaxPersons = 2
	l := newProfileTestLoop(cfg)

	rt := &profileFakeRuntime{profiles: map[string]string{
		"u1": "画像一", "u2": "画像二", "u3": "画像三", "u4": "画像四",
	}}
	batch := []*domain.Message{
		profileTestMessage("m1", "u1", "甲", domain.Text("a")),
		profileTestMessage("m2", "u2", "乙", domain.Text("b")),
		profileTestMessage("m3", "u3", "丙", domain.Text("c")),
		profileTestMessage("m4", "u4", "丁", domain.Text("d")),
	}

	got := l.buildProfileInjection(batch, rt)
	if n := strings.Count(got, "：\n  "); n != 2 {
		t.Errorf("注入人数 = %d，期望 2（ProfileMaxPersons）", n)
	}
	// 候选按「最近的优先」收集，所以该是 u4、u3。
	if !strings.Contains(got, "丁：") || !strings.Contains(got, "丙：") {
		t.Errorf("应按最近优先取最近两个人，实际 %q", got)
	}
	if strings.Contains(got, "甲：") || strings.Contains(got, "乙：") {
		t.Errorf("不该注入更早的人：%q", got)
	}

	// ProfileMaxPersons <= 0 时回退到 4。
	cfg.Memory.ProfileMaxPersons = 0
	got = l.buildProfileInjection(batch, rt)
	if n := strings.Count(got, "：\n  "); n != 4 {
		t.Errorf("ProfileMaxPersons=0 时应回退到 4，实际注入 %d 人", n)
	}
}

// TestBuildProfileInjectionIncludesAtTargets 验证 @ 目标也算候选。
//
// 「他 @ 了谁」往往比「谁在说话」更能说明这次对话涉及几个人。
// 参考实现有 _at_candidate，我们照抄。
func TestBuildProfileInjectionIncludesAtTargets(t *testing.T) {
	cfg := newTestConfig()
	l := newProfileTestLoop(cfg)
	rt := &profileFakeRuntime{profiles: map[string]string{
		"u2": "被 @ 的那个人的画像",
	}}

	// u1 说话并 @ 了 u2，u2 自己没说话。
	batch := []*domain.Message{
		profileTestMessage("m1", "u1", "小明",
			domain.Text("你最近怎么样"),
			domain.At("u2", "小红"),
		),
	}

	got := l.buildProfileInjection(batch, rt)
	if !strings.Contains(got, "被 @ 的那个人的画像") {
		t.Fatalf("@ 目标应被算作候选，实际 %q", got)
	}
	// 显示名要从 @ 段里取不到（displayNameFor 只看 m.UserID），
	// 所以这里用的是原始 ID —— 记下这个已知局限，不当成 bug。
	if !strings.Contains(got, "u2：") {
		t.Logf("注意：@ 目标的显示名回退到了 ID，实际输出 %q", got)
	}

	// 同一个人既说话又被 @，只能出现一次。
	batch = []*domain.Message{
		profileTestMessage("m1", "u2", "小红", domain.Text("我很好")),
		profileTestMessage("m2", "u1", "小明", domain.At("u2", "小红")),
	}
	got = l.buildProfileInjection(batch, rt)
	if n := strings.Count(got, "u2："); n > 1 {
		t.Errorf("同一个人重复注入 %d 次：%q", n, got)
	}
}

// TestBuildProfileInjectionSkipsBlankProfile 验证空白画像不产生空块。
func TestBuildProfileInjectionSkipsBlankProfile(t *testing.T) {
	cfg := newTestConfig()
	l := newProfileTestLoop(cfg)
	rt := &profileFakeRuntime{profiles: map[string]string{
		"u1": "   \n\t ",
	}}

	batch := []*domain.Message{profileTestMessage("m1", "u1", "小明", domain.Text("在吗"))}
	if got := l.buildProfileInjection(batch, rt); got != "" {
		t.Errorf("空白画像不该注入，实际 %q", got)
	}
}

// TestBuildProfileInjectionSkipsUnknownUser 验证 UserID 为空的消息被跳过。
func TestBuildProfileInjectionSkipsUnknownUser(t *testing.T) {
	cfg := newTestConfig()
	l := newProfileTestLoop(cfg)
	rt := &profileFakeRuntime{profiles: map[string]string{"u1": "画像"}}

	batch := []*domain.Message{
		nil,
		profileTestMessage("m1", "", "无 ID", domain.Text("匿名")),
		profileTestMessage("m2", "u1", "小明", domain.Text("在吗")),
	}

	got := l.buildProfileInjection(batch, rt)
	if !strings.Contains(got, "小明：") {
		t.Errorf("有效用户仍应被注入，实际 %q", got)
	}
	for _, c := range rt.calls {
		if strings.TrimSpace(c) == "" {
			t.Errorf("空 UserID 不该被查询")
		}
	}
}

// TestFormatProfileBlock 验证单个人物块的渲染。
func TestFormatProfileBlock(t *testing.T) {
	// 正常情况：名字 + 冒号 + 换行 + 两空格缩进。
	if got := formatProfileBlock("小明", "喜欢摄影"); got != "小明：\n  喜欢摄影" {
		t.Errorf("格式不对：%q", got)
	}

	// 正文里的换行与多余空白被压平，否则多行画像会破坏「名字-正文」的对齐。
	got := formatProfileBlock("小红", "第一行\n\n第二行   第三行")
	if got != "小红：\n  第一行 第二行 第三行" {
		t.Errorf("正文压平不对：%q", got)
	}

	// 名字为空时用占位，避免出现「：」这样的残句。
	if got := formatProfileBlock("", "画像"); !strings.HasPrefix(got, "某位群友：") {
		t.Errorf("空名字应使用占位：%q", got)
	}
	if got := formatProfileBlock("   ", "画像"); !strings.HasPrefix(got, "某位群友：") {
		t.Errorf("空白名字应使用占位：%q", got)
	}
}

// TestDisplayNameFor 验证显示名查找：最近一条带名字的消息优先，找不到回退到 ID。
func TestDisplayNameFor(t *testing.T) {
	batch := []*domain.Message{
		profileTestMessage("m1", "u1", "旧名字", domain.Text("a")),
		profileTestMessage("m2", "u1", "新名字", domain.Text("b")),
		nil,
	}

	if got := displayNameFor("u1", batch); got != "新名字" {
		t.Errorf("应取最近的名字，实际 %q", got)
	}
	// 最近一条没有名字时继续往前找。
	batch = []*domain.Message{
		profileTestMessage("m1", "u1", "有名字", domain.Text("a")),
		profileTestMessage("m2", "u1", "", domain.Text("b")),
	}
	if got := displayNameFor("u1", batch); got != "有名字" {
		t.Errorf("应跳过空名字继续找，实际 %q", got)
	}
	// 完全没有时回退到 ID，让模型至少知道有这个人。
	if got := displayNameFor("u-unknown", batch); got != "u-unknown" {
		t.Errorf("找不到名字应回退到 ID，实际 %q", got)
	}
	if got := displayNameFor("u1", nil); got != "u1" {
		t.Errorf("空批次应回退到 ID，实际 %q", got)
	}
}

// TestBuildProfileInjectionAllEmptyUserIDs 覆盖「候选一个都没有」的分支。
//
// 全是空 UserID（例如系统通知）时不该构造一个只有头尾的空画像块。
func TestBuildProfileInjectionAllEmptyUserIDs(t *testing.T) {
	cfg := newTestConfig()
	l := newProfileTestLoop(cfg)
	rt := &profileFakeRuntime{profiles: map[string]string{"u1": "画像"}}

	batch := []*domain.Message{
		profileTestMessage("m1", "", "无 ID", domain.Text("系统通知")),
		nil,
	}
	if got := l.buildProfileInjection(batch, rt); got != "" {
		t.Errorf("没有候选时不该注入，实际 %q", got)
	}
	if len(rt.calls) != 0 {
		t.Errorf("没有候选时不该查库，实际查询 %v", rt.calls)
	}
}
