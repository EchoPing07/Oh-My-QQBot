// Package memory 的摘要生成部分。
//
// 对应参考实现的 maisaka/memory/mid_term.py:build_mid_term_memory_message。
//
// 参考实现把「即将被裁切出上下文的消息」压缩成一条「聊天回想」插入历史，
// 这样上下文窗口能继续往前滚，但早期信息不会真的丢失。
// 我们的做法一致，只是检索端换成 FTS5（见 tokenize.go 的说明）。
package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/embedding"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/llm"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/logx"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/store"
)

// Summarizer 负责把一批消息压成一条聊天回想。
type Summarizer struct {
	cfg       *config.Store
	llm       *llm.Client
	store     *store.DB
	embedding *embedding.Client
	log       *logx.Logger
}

// NewSummarizer 创建摘要器。
//
// emb 可以为 nil：此时回想照样生成，只是没有向量，
// 检索会走关键词兜底。不能因为没配向量就把整块记忆功能关掉。
func NewSummarizer(cfg *config.Store, client *llm.Client, db *store.DB, emb *embedding.Client) *Summarizer {
	return &Summarizer{
		cfg:       cfg,
		llm:       client,
		store:     db,
		embedding: emb,
		log:       logx.Get("memory.summarizer"),
	}
}

// SummaryPayload 是模型返回的摘要结构。
// 与参考实现的 MidTermMemorySummaryModel 对齐。
type SummaryPayload struct {
	Summary    string   `json:"summary"`
	RecallCues []string `json:"recall_cues"`
}

// Summarize 把一批消息压成回想并落库。
//
// sessionID / groupID 用于之后的定向检索。返回 nil 表示跳过（未达门槛或解析失败），
// 调用方不应把它当成错误 —— 摘要失败不该影响正常回复。
func (s *Summarizer) Summarize(ctx context.Context, sessionID, groupID string, msgs []*domain.Message) (*store.MemorySummary, error) {
	if !s.cfg.Get().Memory.Enabled || !s.cfg.Get().Memory.SummaryEnabled {
		return nil, nil
	}
	if s.store == nil {
		return nil, nil
	}

	selected := selectSummaryMessages(msgs, s.cfg.Get().Memory.SummaryMinMessages)
	if len(selected) == 0 {
		return nil, nil
	}

	promptMsgs := s.buildPromptMessages(selected)
	if len(promptMsgs) <= 1 {
		// 只有 system 没有正文，说明消息里全是图片占位之类，不值得调模型。
		return nil, nil
	}

	resp, err := s.llm.Generate(ctx, config.TaskUtils, &llm.Request{Messages: promptMsgs})
	if err != nil {
		return nil, fmt.Errorf("生成聊天回想失败: %w", err)
	}

	payload, err := ParseSummaryResponse(resp.Content)
	if err != nil {
		return nil, fmt.Errorf("解析聊天回想失败: %w", err)
	}
	if strings.TrimSpace(payload.Summary) == "" {
		return nil, nil
	}

	timeRange, participants, coveredUntil := describeMessages(selected)

	summary := &store.MemorySummary{
		SessionID:    sessionID,
		GroupID:      groupID,
		Summary:      strings.TrimSpace(payload.Summary),
		RecallCues:   normalizeCues(payload.RecallCues),
		Participants: participants,
		TimeRange:    timeRange,
		SourceCount:  len(selected),
		CoveredUntil: coveredUntil,
		CreatedAt:    time.Now().Unix(),
	}

	// 先算向量再落库：这样写入的就是「可检索」状态，
	// 不会出现「刚存下的记忆查不到、要等回填任务跑过才行」的空窗。
	if err := s.attachEmbedding(ctx, summary); err != nil {
		// 向量化失败不阻断落库：回想本身是有价值的，
		// 之后回填任务会补上。
		s.log.Warn("聊天回想向量化失败，将由回填任务补齐", "err", err)
	}

	if err := s.store.Memory().SaveSummary(ctx, summary); err != nil {
		return nil, fmt.Errorf("保存聊天回想失败: %w", err)
	}

	s.log.Info("已生成聊天回想",
		"session", sessionID,
		"source", len(selected),
		"cues", len(summary.RecallCues),
		"range", timeRange,
	)
	return summary, nil
}

// attachEmbedding 给回想算向量并填进结构体。
//
// 检索用的文本是「摘要 + 召回线索」：召回线索本身就是模型写的
// 「什么情景下会需要这条记忆」，把两者一起向量化比只向量化摘要
// 更贴近「以后会被怎么问」。
func (s *Summarizer) attachEmbedding(ctx context.Context, summary *store.MemorySummary) error {
	if s.embedding == nil || !s.embedding.Enabled() {
		return nil
	}

	// 检索用的文本是摘要 + 召回线索，两者用换行分隔。
	text := summary.Summary + "\n" + strings.Join(summary.RecallCues, "\n")
	vecs, err := s.embedding.Embed(ctx, []string{text})
	if err != nil {
		return err
	}
	if len(vecs) == 0 {
		return nil
	}

	model, err := s.embeddingModelName()
	if err != nil {
		return err
	}

	summary.Embedding = embedding.Pack(vecs[0])
	summary.EmbedModel = model
	summary.EmbedDim = len(vecs[0])
	return nil
}

// embeddingModelName 返回当前 embedding 模型的标识符。
func (s *Summarizer) embeddingModelName() (string, error) {
	models := s.cfg.Get().ResolveModels(config.TaskEmbedding)
	if len(models) == 0 {
		return "", fmt.Errorf("未配置 embedding 模型")
	}
	m, err := s.cfg.Get().Model(models[0])
	if err != nil {
		return "", err
	}
	return m.ModelIdentifier, nil
}

// Backfill 给历史记忆补向量。
//
// 两个用途：
//   - 首次启用向量检索时，把之前存的记忆补上
//   - 换了 embedding 模型后，旧维度向量全部作废，重新算
//
// 返回处理条数。每次最多处理 limit 条，由调用方循环或定时任务推进。
func (s *Summarizer) Backfill(ctx context.Context, limit int) (int, error) {
	if s.embedding == nil || !s.embedding.Enabled() {
		return 0, nil
	}

	model, err := s.embeddingModelName()
	if err != nil {
		return 0, err
	}

	// 维度口径与检索侧（agent.embeddingModelInfo）一致：实测优先，
	// 配置维度只作初值（还没成功调用过时用一个起点）。
	// 配置维度与端点实测不一致时，Embed 会报明确错误，
	// 不会把错维度的向量写进库。
	dim := s.embedding.Dimension()
	if dim == 0 {
		if models := s.cfg.Get().ResolveModels(config.TaskEmbedding); len(models) > 0 {
			if m, err := s.cfg.Get().Model(models[0]); err == nil && m.Dimension != nil {
				dim = *m.Dimension
			}
		}
	}
	if dim == 0 {
		return 0, nil
	}

	pending, err := s.store.Memory().PendingEmbedding(ctx, model, dim, limit)
	if err != nil {
		return 0, err
	}
	if len(pending) == 0 {
		return 0, nil
	}

	texts := make([]string, len(pending))
	for i, p := range pending {
		texts[i] = p.Summary + "\n" + strings.Join(p.RecallCues, "\n")
	}

	vecs, err := s.embedding.Embed(ctx, texts)
	if err != nil {
		return 0, fmt.Errorf("批量向量化记忆失败: %w", err)
	}

	done := 0
	for i, v := range vecs {
		if len(v) == 0 {
			continue
		}
		if err := s.store.Memory().UpdateEmbedding(ctx, pending[i].ID, embedding.Pack(v), model, len(v)); err != nil {
			s.log.Warn("写入记忆向量失败", "id", pending[i].ID, "err", err)
			continue
		}
		done++
	}

	s.log.Info("记忆向量回填完成", "done", done, "total_pending", len(pending))
	return done, nil
}

// buildPromptMessages 组装摘要请求。
//
// 提示词来自 prompts/<locale>/mid_term_memory_summary.prompt，
// 默认内容逐字照抄 refs/MaiBot/prompts/zh-CN/mid_term_memory_summary.prompt。
// 正文里带 JSON 示例，所以用 RenderNamed（单遍扫描）而不是 Sprintf。
func (s *Summarizer) buildPromptMessages(msgs []*domain.Message) []llm.Message {
	timeRange, participants, _ := describeMessages(msgs)
	participantsText := "未知"
	if len(participants) > 0 {
		participantsText = strings.Join(participants, "、")
	}

	// 提示词来自 prompts/<locale>/mid_term_memory_summary.prompt（可编辑），
	// 默认内容逐字照抄 refs/MaiBot/prompts/zh-CN/mid_term_memory_summary.prompt。
	instruction := renderPrompt(promptNameSummary, map[string]string{
		"time_range":        timeRange,
		"participants_text": participantsText,
	})
	out := []llm.Message{{Role: llm.RoleSystem, Content: instruction}}

	// 预算控制：超出上限直接截断剩余消息。
	// 参考实现用 MAX_SUMMARY_INPUT_CHARS = 16000，同样的理由 ——
	// 摘要输入过长时首尾都会失控，而且很贵。
	budget := s.cfg.Get().Memory.SummaryMaxInputChars
	total := 0
	for _, m := range msgs {
		text := renderForSummary(m)
		if strings.TrimSpace(text) == "" {
			continue
		}
		if budget > 0 && total+len([]rune(text)) > budget {
			break
		}
		out = append(out, llm.Message{Role: llm.RoleUser, Content: text})
		total += len([]rune(text))
	}

	return out
}

// selectSummaryMessages 挑出值得摘要的消息。
//
// 除了完全渲染不出内容的，还要筛掉「只有媒体占位、没有任何文字」的消息。
//
// 为什么：群里常有纯图连发、纯表情包互动，它们的渲染结果是 `[图片]`
// 这样有内容但无信息的文本。放进摘要后，模型会拿一大串 `[图片]`
// 去「总结话题脉络」，产出的回想基本是废话。
// 这与参考实现不冲突 —— 它过滤的是「无文本且无视觉内容」，
// 而我们这里连视觉模型都没接，媒体对它同样是零信息。
func selectSummaryMessages(msgs []*domain.Message, minCount int) []*domain.Message {
	out := make([]*domain.Message, 0, len(msgs))
	for _, m := range msgs {
		if m == nil {
			continue
		}
		if !hasInformativeText(m) {
			continue
		}
		out = append(out, m)
	}
	if len(out) < minCount {
		return nil
	}
	return out
}

// hasInformativeText 判断消息里有没有真正可摘要的文字。
//
// 有 text 段内容，或者语音/文件段带了平台给的转写/文件名，才算有信息。
func hasInformativeText(m *domain.Message) bool {
	for _, seg := range m.Segments {
		switch seg.Type {
		case domain.SegmentText:
			if strings.TrimSpace(seg.Text) != "" {
				return true
			}
		case domain.SegmentVoice, domain.SegmentFile:
			// 这两个类型的内容存在 seg.Text 里，是真正的信息而非占位符。
			if strings.TrimSpace(seg.Text) != "" {
				return true
			}
		}
	}
	// 历史数据可能只有 PlainText（Segments 为空）。
	// 注意这里不能用渲染结果判断：渲染会对图片产出 `[图片]` 这种占位符。
	if len(m.Segments) == 0 && strings.TrimSpace(m.PlainText) != "" {
		return true
	}
	return false
}

// renderForSummary 渲染一条消息用于摘要输入。
//
// 格式与参考实现的「按原始多消息格式逐条提供」一致：
// 每条独立成 user 消息，保留 msg 边界。
func renderForSummary(m *domain.Message) string {
	var b strings.Builder
	name := m.UserName
	if name == "" {
		name = m.UserID
	}
	b.WriteString(name)
	b.WriteString("：")

	for _, seg := range m.Segments {
		switch seg.Type {
		case domain.SegmentText:
			b.WriteString(seg.Text)
		case domain.SegmentImage:
			b.WriteString("[图片]")
		case domain.SegmentEmoji:
			b.WriteString("[表情]")
		case domain.SegmentVoice:
			// 有转写就用转写，没有才退回占位。
			// 这是踩过的坑：硬编码 [语音] 会把平台给的 asr 结果丢掉。
			if strings.TrimSpace(seg.Text) != "" {
				b.WriteString(seg.Text)
			} else {
				b.WriteString("[语音]")
			}
		case domain.SegmentFile:
			if strings.TrimSpace(seg.Text) != "" {
				b.WriteString("[文件 " + seg.Text + "]")
			} else {
				b.WriteString("[文件]")
			}
		}
	}

	// Segments 为空但 PlainText 有内容时（历史数据、测试构造）用 PlainText 兜底。
	if b.Len() == 0 && strings.TrimSpace(m.PlainText) != "" {
		b.WriteString(m.PlainText)
	}
	return b.String()
}

// describeMessages 归纳时间范围、参与人物与覆盖到的时间点。
func describeMessages(msgs []*domain.Message) (timeRange string, participants []string, coveredUntil int64) {
	if len(msgs) == 0 {
		return "", nil, 0
	}

	first := msgs[0].ReceivedAt
	last := msgs[0].ReceivedAt
	seen := make(map[string]struct{}, len(msgs))
	for _, m := range msgs {
		if m.ReceivedAt.Before(first) {
			first = m.ReceivedAt
		}
		if m.ReceivedAt.After(last) {
			last = m.ReceivedAt
		}
		if m.ReceivedAt.Unix() > coveredUntil {
			coveredUntil = m.ReceivedAt.Unix()
		}
		name := m.UserName
		if name == "" {
			name = m.UserID
		}
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		participants = append(participants, name)
	}

	const layout = "2006/01/02 15:04"
	if first.Equal(last) {
		timeRange = first.Format(layout)
	} else {
		timeRange = first.Format(layout) + " - " + last.Format(layout)
	}
	return timeRange, participants, coveredUntil
}

// ParseSummaryResponse 解析模型返回的摘要 JSON。
//
// 模型经常把 JSON 包在 markdown 代码块里，或者前后加一句解释。
// 参考实现用 json_repair 兜底，我们没有那个依赖，所以自己做「剥壳 + 取首尾对象」。
func ParseSummaryResponse(raw string) (*SummaryPayload, error) {
	cleaned := stripCodeFence(raw)
	start := strings.Index(cleaned, "{")
	end := strings.LastIndex(cleaned, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("响应中没有 JSON 对象")
	}

	var payload SummaryPayload
	if err := json.Unmarshal([]byte(cleaned[start:end+1]), &payload); err != nil {
		return nil, fmt.Errorf("JSON 无效: %w", err)
	}
	return &payload, nil
}

// stripCodeFence 去掉 markdown 代码块围栏。
func stripCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	// 去掉开头 ``` 及其后的语言标识那一行。
	if nl := strings.IndexByte(s, '\n'); nl >= 0 {
		s = s[nl+1:]
	} else {
		s = strings.TrimPrefix(s, "```")
	}
	if idx := strings.LastIndex(s, "```"); idx >= 0 {
		s = s[:idx]
	}
	return strings.TrimSpace(s)
}

// normalizeCues 清洗召回线索：去空、去重、限条数。
//
// 参考实现要求 3-5 条，模型有时会多给，限制上限避免污染 FTS 索引。
func normalizeCues(cues []string) []string {
	out := make([]string, 0, len(cues))
	seen := make(map[string]struct{}, len(cues))
	for _, cue := range cues {
		text := strings.TrimSpace(cue)
		if text == "" {
			continue
		}
		if _, dup := seen[text]; dup {
			continue
		}
		seen[text] = struct{}{}
		out = append(out, text)
		if len(out) >= 5 {
			break
		}
	}
	return out
}

// 注意：summaryPromptTemplate 已移到 internal/prompt/embedded/zh-CN/
// mid_term_memory_summary.prompt（可在 WebUI 里直接改）。
// 代码里只留 promptNameSummary 这个名字常量。
