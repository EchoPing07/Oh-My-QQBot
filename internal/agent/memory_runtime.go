package agent

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
	embeddingPkg "github.com/ohmyqqbot/oh-my-qqbot/internal/embedding"
	memoryPkg "github.com/ohmyqqbot/oh-my-qqbot/internal/memory"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/store"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/tool"
)

// 本文件是 runtime 对「长期记忆 / 表情包」两个能力的实现。
//
// 对应参考实现里 tool_ctx.runtime 暴露的那批能力：
//   - query_memory.py 用到 memory_service.search
//   - send_emoji.py 用到 emoji_manager
//
// 放在 agent 包而不是 tool 包，是因为它们要么依赖 store（落库），
// 要么依赖 sender（发图片），tool 包直接依赖会形成循环。

// SearchMemory 检索长期记忆。
func (r *Runtime) SearchMemory(q tool.MemoryQuery) (tool.MemoryResult, error) {
	res := tool.MemoryResult{Mode: q.Mode}
	if r.store == nil {
		return res, fmt.Errorf("数据库不可用")
	}

	limit := q.Limit
	if limit <= 0 {
		limit = r.cfg.Get().Memory.QueryDefaultLimit
	}
	if max := r.cfg.Get().Memory.QueryMaxLimit; max > 0 && limit > max {
		limit = max
	}

	ctx, cancel := context.WithTimeout(r.ctx, 10*time.Second)
	defer cancel()

	// 检索策略：向量优先，关键词兜底。
	//
	// 为什么是这个顺序：向量能理解「我们之前说过要一起去吃好吃的」这类
	// 与原文没有任何字面重合的问法，而 FTS5 对这种查询是 0 命中（实测）。
	// 但向量依赖外部服务，可能没配、可能挂了，所以必须有兜底。
	var (
		hits     []*store.MemorySummary
		err      error
		fallback bool
	)

	switch q.Mode {
	case "time":
		hits, err = r.store.Memory().SearchSummariesByTime(ctx, r.sessionID, q.TimeStart, q.TimeEnd, limit)
		res.Mode = "time"

	case "hybrid":
		// hybrid = 时间范围 + 语义。先按时间圈定，再在圈内按相似度排。
		hits, err = r.store.Memory().SearchSummariesByTime(ctx, r.sessionID, q.TimeStart, q.TimeEnd, limit*4)
		if err == nil && len(hits) > 0 && strings.TrimSpace(q.Query) != "" {
			if ranked, rerr := r.rankByVector(ctx, q.Query, hits, limit); rerr == nil && len(ranked) > 0 {
				hits = ranked
			}
		}
		if len(hits) > limit {
			hits = hits[:limit]
		}
		res.Mode = "hybrid"

	default:
		hits, fallback, err = r.searchByVectorOrKeyword(ctx, q.Query, limit)
		if err != nil {
			return res, err
		}
		res.Mode = "vector"
		if fallback {
			res.Mode = "keyword"
		}
	}
	if err != nil {
		return res, err
	}

	// 向量也没捞到东西时，如果给了时间范围，退一步按时间查。
	// 宁可给点相关背景，也不要空手而归。
	if len(hits) == 0 && q.Mode == "search" && (q.TimeStart > 0 || q.TimeEnd > 0) {
		if alt, aerr := r.store.Memory().SearchSummariesByTime(ctx, r.sessionID, q.TimeStart, q.TimeEnd, limit); aerr == nil && len(alt) > 0 {
			hits = alt
			fallback = true
			res.Mode = "time"
		}
	}
	res.Fallback = fallback

	if name := strings.TrimSpace(q.PersonName); name != "" {
		hits = filterByPerson(hits, name)
	}

	for _, h := range hits {
		res.Hits = append(res.Hits, tool.MemoryHit{
			Summary:      h.Summary,
			TimeRange:    h.TimeRange,
			Participants: h.Participants,
			CreatedAt:    h.CreatedAt,
		})
	}
	return res, nil
}

// searchByVectorOrKeyword 做一次「向量优先、关键词兜底」的检索。
//
// 返回 (hits, 是否降级, err)。
func (r *Runtime) searchByVectorOrKeyword(ctx context.Context, query string, limit int) ([]*store.MemorySummary, bool, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, false, nil
	}

	// 1) 向量路径。
	if ranked, err := r.searchByVector(ctx, query, limit); err != nil {
		r.log.Warn("向量检索失败，降级为关键词检索", "err", err)
	} else if len(ranked) > 0 {
		return ranked, false, nil
	}

	// 2) 关键词兜底：向量没配、挂了、或者库里还没有向量。
	hits, err := r.store.Memory().SearchSummaries(ctx, r.sessionID, query, limit)
	if err != nil {
		return nil, true, err
	}
	return hits, true, nil
}

// searchByVector 用向量相似度检索。
//
// 没有 embedding 客户端时返回空结果且不报错 —— 那是「未配置」，
// 不是「故障」，调用方会安静地降级。
func (r *Runtime) searchByVector(ctx context.Context, query string, limit int) ([]*store.MemorySummary, error) {
	if r.embedding == nil || !r.embedding.Enabled() {
		return nil, nil
	}

	model, dim, err := r.embeddingModelInfo()
	if err != nil {
		return nil, err
	}

	// 先确认库里有多少可用向量，避免为了一个注定空的结果去调接口。
	// （首次开启、或刚换模型时库里可能全是旧维度向量。）
	candidates, err := r.store.Memory().LoadSummariesForVectorSearch(
		ctx, r.sessionID, model, dim, r.cfg.Get().Memory.VectorCandidateLimit,
	)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	vecs, err := r.embedding.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 {
		return nil, nil
	}
	queryVec := vecs[0]

	// 把候选向量全部归一化，之后用点积代替余弦。
	// 这是「一次查询对 N 条候选」的标准做法，省掉每条的模长计算。
	flat, failed := unpackCandidates(candidates)
	if failed > 0 {
		// 以前损坏的 BLOB 被静默跳过，表现为「某些记忆永远检不到」
		// 且没有任何线索，所以这里必须留下计数。
		r.log.Warn("候选向量解包失败，已跳过", "failed", failed, "total", len(candidates))
	}

	queryNormalized := append([]float32(nil), queryVec...)
	memoryPkg.Normalize(queryNormalized)

	// 有界 top-k：O(n log k) 而不是全排序的 O(n log n)。
	// 记忆上万条时这个差别会体现在回复延迟上。
	top := memoryPkg.TopK(queryNormalized, flat, limit, r.cfg.Get().Memory.VectorMinScore)

	out := make([]*store.MemorySummary, 0, len(top))
	for _, it := range top {
		out = append(out, candidates[it.Index])
	}
	return out, nil
}

// rankByVector 在给定的候选集内按向量相似度重排（hybrid 模式用）。
func (r *Runtime) rankByVector(ctx context.Context, query string, candidates []*store.MemorySummary, limit int) ([]*store.MemorySummary, error) {
	if r.embedding == nil || !r.embedding.Enabled() || len(candidates) == 0 {
		return nil, nil
	}

	vecs, err := r.embedding.Embed(ctx, []string{query})
	if err != nil || len(vecs) == 0 {
		return nil, err
	}
	queryVec := vecs[0]

	flat, failed := unpackCandidates(candidates)
	if failed > 0 {
		r.log.Warn("候选向量解包失败，已跳过", "failed", failed, "total", len(candidates))
	}

	queryNormalized := append([]float32(nil), queryVec...)
	memoryPkg.Normalize(queryNormalized)
	top := memoryPkg.TopK(queryNormalized, flat, limit, r.cfg.Get().Memory.VectorMinScore)

	out := make([]*store.MemorySummary, 0, len(top))
	for _, it := range top {
		out = append(out, candidates[it.Index])
	}
	return out, nil
}

// unpackCandidates 把候选记忆的向量解包并归一化。
//
// 返回 (向量集, 解包失败数)。失败数 > 0 说明库里存在损坏的 BLOB
// （长度不是 4 的倍数等），调用方应打一次 Warn。
func unpackCandidates(candidates []*store.MemorySummary) ([][]float32, int) {
	flat := make([][]float32, len(candidates))
	failed := 0
	for i, c := range candidates {
		v := embeddingPkg.Unpack(c.Embedding)
		if len(v) == 0 {
			failed++
			continue
		}
		memoryPkg.Normalize(v)
		flat[i] = v
	}
	return flat, failed
}

// embeddingModelInfo 返回当前 embedding 模型的标识与维度。
//
// 维度口径：实测优先 —— 客户端成功调用过一次（Dimension()>0）就用实测值，
// 否则用配置 dimension 作初值。配置维度的角色限定为「发给端点的请求参数」
// （对齐参考实现 openai_client.py:1994-2011：dimensions 只是请求参数），
// 不作为库过滤条件 —— 端点忽略 dimensions 时（很多中转站如此），
// 旧口径会用配置维度去过滤实测维度的库，候选集恒空且无任何日志。
// 配置维度与实测不一致时，embedding 客户端会在下一次调用里报明确错误。
func (r *Runtime) embeddingModelInfo() (string, int, error) {
	models := r.cfg.Get().ResolveModels(config.TaskEmbedding)
	if len(models) == 0 {
		return "", 0, fmt.Errorf("未配置 embedding 模型")
	}
	m, err := r.cfg.Get().Model(models[0])
	if err != nil {
		return "", 0, err
	}
	dim := 0
	if r.embedding != nil {
		// 实测维度优先。
		dim = r.embedding.Dimension()
	}
	if dim == 0 && m.Dimension != nil {
		// 还没成功调用过时，用配置维度作初值；
		// 与实测是否一致由 Embed 的写入校验兜底。
		dim = *m.Dimension
	}
	if dim == 0 {
		return "", 0, fmt.Errorf("embedding 维度未知，请先在配置里指定 dimension 或等待一次成功调用")
	}
	return m.ModelIdentifier, dim, nil
}

// dedupeSummaries 按 ID 去重，保持原顺序。
func dedupeSummaries(in []*store.MemorySummary) []*store.MemorySummary {
	seen := make(map[int64]struct{}, len(in))
	out := make([]*store.MemorySummary, 0, len(in))
	for _, s := range in {
		if s == nil {
			continue
		}
		if _, dup := seen[s.ID]; dup {
			continue
		}
		seen[s.ID] = struct{}{}
		out = append(out, s)
	}
	return out
}

// filterByPerson 只保留提到某人的记忆。
func filterByPerson(hits []*store.MemorySummary, name string) []*store.MemorySummary {
	out := make([]*store.MemorySummary, 0, len(hits))
	for _, h := range hits {
		matched := false
		for _, p := range h.Participants {
			if p == name {
				matched = true
				break
			}
		}
		if !matched && strings.Contains(h.Summary, name) {
			matched = true
		}
		if matched {
			out = append(out, h)
		}
	}
	return out
}

// PersonProfile 返回某个用户的画像文本，没有时返回空串。
func (r *Runtime) PersonProfile(userID string) string {
	if r.store == nil || !r.cfg.Get().Memory.Enabled || !r.cfg.Get().Memory.ProfileEnabled {
		return ""
	}
	if userID == "" {
		return ""
	}

	ctx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
	defer cancel()

	got, err := r.store.Memory().GetProfile(ctx, r.key.Platform, userID, r.sessionID)
	if err != nil {
		r.log.Warn("读取人物画像失败", "user", userID, "err", err)
		return ""
	}
	if got == nil {
		return ""
	}

	// 只取可注入的分节：完整画像里的「不确定信息」是模型自己的猜测，
	// 「维护备注」是给人看的，塞进提示词只会增加噪声。
	text := memoryPkg.ProfileInjectionText(got.ProfileText)
	if text == "" {
		return ""
	}
	if max := r.cfg.Get().Memory.ProfileMaxChars; max > 0 {
		text = truncateRunes(text, max)
	}
	return text
}

// truncateRunes 按 rune 截断，不切断 UTF-8。
//
// 这个约束踩过坑：按 byte 截断汉字会产出非法 UTF-8，
// 整个 JSON 请求都发不出去（见 AGENTS.md 第 21 条）。
func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "..."
}

// PickEmoji 按情绪描述选一个表情包。
//
// emotion 为空时退化为「随机取一个」，用于 show_emoji_list 这类探索场景。
func (r *Runtime) PickEmoji(emotion string) (tool.EmojiRef, bool) {
	if r.store == nil || !r.cfg.Get().Emoji.Enabled {
		return tool.EmojiRef{}, false
	}

	ctx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
	defer cancel()

	var (
		list []*store.Emoji
		err  error
	)
	if strings.TrimSpace(emotion) == "" {
		list, err = r.store.Emojis().Random(ctx, 1)
	} else {
		list, err = r.store.Emojis().Search(ctx, emotion, 5)
		if err == nil && len(list) == 0 {
			// 情绪描述没命中就随机兜底：表情包描述是 VLM 生成的，
			// 模型填的情绪词未必与标签字面重合。
			list, err = r.store.Emojis().Random(ctx, 1)
		}
	}
	if err != nil {
		r.log.Warn("检索表情包失败", "emotion", emotion, "err", err)
		return tool.EmojiRef{}, false
	}
	if len(list) == 0 {
		return tool.EmojiRef{}, false
	}

	// 多条命中时随机挑一个，避免每次都是同一张。
	pick := list[0]
	if len(list) > 1 {
		pick = list[r.randN(len(list))]
	}

	return tool.EmojiRef{
		Hash:        pick.FileHash,
		Path:        pick.FilePath,
		Description: pick.Description,
	}, true
}

// SendEmoji 发送一个表情包。
func (r *Runtime) SendEmoji(ref tool.EmojiRef) ([]string, error) {
	if ref.Path == "" {
		return nil, fmt.Errorf("表情包路径为空")
	}

	// 路径是相对工作目录的（emoji.dir 与 file_path 都这么写），
	// 所以直接读，不要拼数据库路径的父目录 —— 那会得到 data/data/emoji/...
	binary, err := os.ReadFile(ref.Path)
	if err != nil {
		return nil, fmt.Errorf("读取表情包文件 %s 失败: %w", ref.Path, err)
	}

	ctx, cancel := context.WithTimeout(r.ctx, 30*time.Second)
	defer cancel()

	segments := []domain.Segment{{
		Type:   domain.SegmentImage,
		Binary: binary,
	}}
	sent, err := r.sender.SendSegments(ctx, r.scene(), r.targetID, segments, "", domain.Reference{})
	if err != nil {
		return nil, err
	}

	storeCtx, cancelStore := outboundContext(r.ctx)
	r.recordOutbound(storeCtx, segments, sent, "")
	cancelStore()

	// 记一次使用，供之后「常用优先」排序。
	if r.store != nil {
		if id := r.emojiIDByHash(ref.Hash); id > 0 {
			if err := r.store.Emojis().MarkUsed(ctx, id, time.Now()); err != nil {
				r.log.Warn("记录表情包使用失败", "hash", ref.Hash, "err", err)
			}
		}
	}
	return sent.MessageIDs, nil
}

// emojiIDByHash 按哈希取表情包 ID。
func (r *Runtime) emojiIDByHash(hash string) int64 {
	if hash == "" || r.store == nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
	defer cancel()

	e, err := r.store.Emojis().GetByHash(ctx, hash)
	if err != nil || e == nil {
		return 0
	}
	return e.ID
}

// randN 返回 [0,n) 的随机整数。
func (r *Runtime) randN(n int) int {
	if n <= 1 {
		return 0
	}
	return int(randFloat() * float64(n))
}
