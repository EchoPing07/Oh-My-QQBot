package memory

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/llm"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/logx"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/store"
)

// 人物画像生成。
//
// 与参考实现的差异（AGENTS.md 要求写清）：
//
//	参考实现：人物画像由插件 A_memorix 维护（profile_admin / person_profile_service），
//	          有完整的增删改查、证据消息、合并策略，还有独立的模糊修正流程。
//	本项目：  只保留「攒够消息 → 生成一份结构化画像 → 覆盖写入」这条最小链路。
//	          没有做：画像合并（多次生成取交集）、证据溯源、人工修正。
//
// 为什么还要做：没有生成方的话，person_profiles 表永远是空的，
// 画像注入那块代码就是死代码 —— 那比不做更糟，因为看起来像做了。
type ProfileBuilder struct {
	cfg   *config.Store
	llm   *llm.Client
	store *store.DB
	log   *logx.Logger
}

// NewProfileBuilder 创建画像生成器。
func NewProfileBuilder(cfg *config.Store, client *llm.Client, db *store.DB) *ProfileBuilder {
	return &ProfileBuilder{
		cfg:   cfg,
		llm:   client,
		store: db,
		log:   logx.Get("memory.profile"),
	}
}

// profileSectionTitles 是画像的固定分节。
//
// 逐字照抄参考实现 A_memorix/core/utils/profile_text.py 的 PROFILE_SECTION_TITLES。
// 分节固定而不是让模型自由发挥，是因为注入时只取前 5 节
// （PROFILE_INJECTION_SECTION_TITLES），后面几节是给人看的维护信息。
var profileSectionTitles = []string{
	"身份设定",
	"关系设定",
	"稳定了解",
	"相处偏好",
	"近期互动",
	"不确定信息",
	"维护备注",
}

// profileInjectionSections 是注入给 Planner 的分节（前 5 个）。
var profileInjectionSections = map[string]bool{
	"身份设定": true,
	"关系设定": true,
	"稳定了解": true,
	"相处偏好": true,
	"近期互动": true,
}

// UpdateIfNeeded 在消息攒够阈值时更新某人的画像。
//
// 返回 true 表示这次真的更新了。调用方（runtime）在每轮思考后调用，
// 不满足条件时立即返回，不产生任何模型调用。
func (b *ProfileBuilder) UpdateIfNeeded(ctx context.Context, platform, userID, sessionID, displayName string, msgs []*domain.Message) (bool, error) {
	if !b.cfg.Get().Memory.Enabled || !b.cfg.Get().Memory.ProfileEnabled {
		return false, nil
	}
	if b.store == nil || userID == "" || len(msgs) == 0 {
		return false, nil
	}

	// 只统计这个人自己说的话。
	// 不按「本轮批次里有多少条消息」判断：一个活跃的群里，
	// 某一轮可能带进来 20 条别人的消息，但这个人只说了 1 句 ——
	// 用总条数当阈值会让画像更新得过于频繁且内容空洞。
	his := make([]*domain.Message, 0, len(msgs))
	for _, m := range msgs {
		if m != nil && m.UserID == userID && hasInformativeText(m) {
			his = append(his, m)
		}
	}
	if len(his) == 0 {
		return false, nil
	}

	existing, err := b.store.Memory().GetProfile(ctx, platform, userID, sessionID)
	if err != nil {
		return false, fmt.Errorf("读取已有画像失败: %w", err)
	}

	// 阈值判断：靠一个持久化的计数器累积「自上次更新以来新增了多少条」。
	//
	// 为什么不能直接用 len(his) 与 existing.MessageCount 比：
	// his 只是「本批次能看到的」条数（通常 1~3 条），而阈值是 20。
	// 拿 1 去和 20 比永远不够，画像就再也不会更新 ——
	// 这是个很容易写错的地方，表现是「第一次生成后就再也不变了」。
	//
	// 计数器存在 kv 表，每轮把本批次条数加进去，
	// 攒够阈值才真的调模型，然后清零。
	threshold := b.cfg.Get().Memory.ProfileUpdateIntervalMessages
	if threshold < 1 {
		threshold = 20
	}

	counterKey := profileCounterKey(platform, userID, sessionID)
	acc, err := b.accumulate(ctx, counterKey, len(his))
	if err != nil {
		return false, fmt.Errorf("累加画像计数器失败: %w", err)
	}
	if acc < threshold {
		return false, nil
	}

	profileText, err := b.generate(ctx, displayName, his, existing)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(profileText) == "" {
		return false, nil
	}

	// 合并进已有画像，而不是直接覆盖。
	//
	// 覆盖的后果：模型这次输出比上次少了一条正确信息，
	// 那个信息就永久丢了，而且没有任何报错 ——
	// 表现是「机器人对他的了解忽深忽浅」。
	// 合并策略见 merge.go。
	if existing != nil && existing.SessionID == sessionID {
		profileText = MergeProfile(existing.ProfileText, profileText)
	}

	rec := &store.PersonProfile{
		Platform:      platform,
		UserID:        userID,
		SessionID:     sessionID,
		DisplayName:   displayName,
		ProfileText:   profileText,
		MessageCount:  len(his),
		LastMessageAt: his[len(his)-1].ReceivedAt.Unix(),
		UpdatedAt:     time.Now().Unix(),
	}
	if existing != nil && existing.SessionID == sessionID {
		// 累计计数：否则每轮都会因为「新增条数不够」而反复重算。
		rec.MessageCount = existing.MessageCount + len(his)
	}

	if err := b.store.Memory().UpsertProfile(ctx, rec); err != nil {
		return false, fmt.Errorf("保存人物画像失败: %w", err)
	}

	// 更新成功后清零计数器，开始攒下一轮。
	if err := b.store.KV().Set(ctx, counterKey, "0"); err != nil {
		// 清不掉不影响本次结果，但下一轮可能立刻又触发一次更新。
		b.log.Warn("清零画像计数器失败", "key", counterKey, "err", err)
	}

	b.log.Info("已更新人物画像",
		"user", displayName,
		"user_id", userID,
		"messages", rec.MessageCount,
		"chars", len([]rune(profileText)),
	)
	return true, nil
}

// accumulate 把本批次消息数加进持久化计数器，返回累加后的值。
func (b *ProfileBuilder) accumulate(ctx context.Context, key string, delta int) (int, error) {
	raw, _, err := b.store.KV().Lookup(ctx, key)
	if err != nil {
		return 0, err
	}

	prev := 0
	if raw != "" {
		if n, perr := strconv.Atoi(raw); perr == nil {
			prev = n
		}
	}

	next := prev + delta
	if err := b.store.KV().Set(ctx, key, strconv.Itoa(next)); err != nil {
		return 0, err
	}
	return next, nil
}

// profileCounterKey 返回某个人的画像计数器键。
//
// 带 session_id：同一个人在不同群里可能有不同的画像，
// 计数器也要分开，否则一个活跃群会把另一个安静群的更新提前触发。
func profileCounterKey(platform, userID, sessionID string) string {
	return "profile.counter." + platform + "." + userID + "." + sessionID
}

// generate 调模型生成结构化画像。
func (b *ProfileBuilder) generate(ctx context.Context, name string, msgs []*domain.Message, existing *store.PersonProfile) (string, error) {
	if len(msgs) == 0 {
		return "", nil
	}

	var chat strings.Builder
	total := 0
	budget := b.cfg.Get().Memory.SummaryMaxInputChars
	if budget <= 0 {
		budget = 16000
	}
	for _, m := range msgs {
		line := renderForSummary(m)
		if strings.TrimSpace(line) == "" {
			continue
		}
		if total+len([]rune(line)) > budget {
			break
		}
		chat.WriteString(line)
		chat.WriteString("\n")
		total += len([]rune(line))
	}

	existingSection := ""
	if existing != nil && strings.TrimSpace(existing.ProfileText) != "" {
		existingSection = "\n**已有画像（在此基础上更新，不要丢弃仍然成立的信息）**\n" +
			strings.TrimSpace(existing.ProfileText) + "\n"
	}

	promptText := renderPrompt(promptNameProfile, map[string]string{
		"person_name":      name,
		"bot_name":         b.cfg.Get().Bot.Nickname,
		"sections":         strings.Join(profileSectionTitles, "、"),
		"chat_str":         chat.String(),
		"existing_section": existingSection,
	})

	resp, err := b.llm.Generate(ctx, config.TaskUtils, &llm.Request{
		Messages: []llm.Message{{Role: llm.RoleSystem, Content: promptText}},
	})
	if err != nil {
		return "", fmt.Errorf("生成人物画像失败: %w", err)
	}

	return normalizeProfileText(resp.Content, name), nil
}

// normalizeProfileText 规范化模型输出。
//
// 模型经常：加 markdown 代码块、少几个分节、写多余的解释。
// 这里统一成「# 人物画像 + 固定分节」的形式，
// 保证注入时的格式稳定 —— 格式不稳定会让模型对这块内容的理解也飘忽。
func normalizeProfileText(raw, name string) string {
	text := stripProfileFence(raw)

	// 按「## 分节名」切出各节内容。
	sections := make(map[string]string, len(profileSectionTitles))
	current := ""
	seenSection := false
	var buf strings.Builder

	flush := func() {
		if current != "" {
			sections[current] = strings.TrimSpace(buf.String())
		}
		buf.Reset()
	}

	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		// 跳过一级标题：我们自己会加。
		if strings.HasPrefix(trimmed, "# ") && !strings.HasPrefix(trimmed, "## ") {
			continue
		}
		// 第一个分节之前的内容（「好的，这是画像：」这类前言）一律丢弃。
		if !seenSection && !strings.HasPrefix(trimmed, "## ") {
			continue
		}
		if strings.HasPrefix(trimmed, "## ") {
			seenSection = true
			flush()
			title := strings.TrimSpace(strings.TrimPrefix(trimmed, "## "))
			// 去掉可能的冒号与加粗标记。
			title = strings.Trim(title, "*：: ")
			current = title
			continue
		}
		if current == "" {
			continue
		}
		// 只收「- 」开头的要点行。
		//
		// 后处理要求模型只写要点，但模型经常在最后补一句
		// 「希望有帮助」之类的客套话 —— 它不带「- 」前缀，
		// 会被归到上一个分节里，污染画像内容。
		if !strings.HasPrefix(trimmed, "-") && !strings.HasPrefix(trimmed, "*") {
			continue
		}
		buf.WriteString(line)
		buf.WriteString("\n")
	}
	flush()

	var out strings.Builder
	out.WriteString("# 人物画像\n")
	for _, title := range profileSectionTitles {
		out.WriteString("## ")
		out.WriteString(title)
		out.WriteString("\n")
		body := strings.TrimSpace(sections[title])
		if body == "" {
			out.WriteString("- 暂无\n")
		} else {
			out.WriteString(body)
			out.WriteString("\n")
		}
	}
	return strings.TrimRight(out.String(), "\n")
}

// stripProfileFence 去掉任意位置的 markdown 代码块围栏。
//
// 不能复用 memory.stripCodeFence：那个只处理「整段以 ``` 开头」的情况，
// 而模型经常写成「好的，这是画像：\n```markdown\n...\n```\n希望有帮助」，
// 围栏在中间。残留的围栏行会被当成普通内容写进分节，
// 于是画像里出现一格 ``` 和一个「希望有帮助」。
func stripProfileFence(s string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		// 围栏行本身丢掉；围栏内的内容保留。
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// ProfileInjectionText 从完整画像里截出「可注入」的部分。
//
// 只取前 5 节：后两节是「不确定信息」与「维护备注」，
// 前者是模型自己都不确定的猜测，后者是给人看的，
// 两者塞进提示词只会增加噪声。
func ProfileInjectionText(full string) string {
	if strings.TrimSpace(full) == "" {
		return ""
	}

	var out strings.Builder
	current := ""
	var buf strings.Builder

	flush := func() {
		if current == "" || !profileInjectionSections[current] {
			buf.Reset()
			return
		}
		body := strings.TrimSpace(buf.String())
		// 「- 暂无」是占位，不是信息，丢掉。
		if body == "" || body == "- 暂无" {
			buf.Reset()
			return
		}
		out.WriteString(body)
		out.WriteString("\n")
		buf.Reset()
	}

	for _, line := range strings.Split(full, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# ") && !strings.HasPrefix(trimmed, "## ") {
			continue
		}
		if strings.HasPrefix(trimmed, "## ") {
			flush()
			current = strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "## ")), "*：: ")
			continue
		}
		buf.WriteString(line)
		buf.WriteString("\n")
	}
	flush()

	return strings.TrimSpace(out.String())
}

// 画像提示词已移到 internal/prompt/embedded/zh-CN/person_profile.prompt（可编辑）。
//
// 那段文案是自研的：参考实现没有独立的 profile prompt（它由 A_memorix
// 插件内部拼装），但严格沿用它的分节名与「只做描述、不做人格扮演」的定位。
// 这里刻意不用「人物画像」以外的表述，避免模型写成一个自我介绍 ——
// 画像的用途是让 Planner 理解「这个人是谁、跟我什么关系、聊过什么」，
// 不是生成一段可以拿去扮演这个人的台词。
