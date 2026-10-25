package store

import (
	"context"
	"testing"
	"time"
)

// TestSaveAndSearchSummary 验证「写入回想 → 按关键词检索」全链路。
//
// 这个测试的价值是守住 FTS 索引的同步：SaveSummary 里如果漏了写 memory_fts，
// 检索会永远返回空，而且不报错 —— 属于最难排查的一类问题。
func TestSaveAndSearchSummary(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	s := &MemorySummary{
		SessionID:    "sess-1",
		GroupID:      "group-1",
		Summary:      "他们讨论了很久火锅和延迟问题，最后决定换模型",
		RecallCues:   []string{"讨论晚餐吃什么", "抱怨推理模型太慢"},
		Participants: []string{"小明", "小红"},
		TimeRange:    "2026/10/21 20:00 - 21:00",
		SourceCount:  12,
		CoveredUntil: 1000,
		CreatedAt:    time.Now().Unix(),
	}
	if err := db.Memory().SaveSummary(ctx, s); err != nil {
		t.Fatalf("写入回想失败: %v", err)
	}
	if s.ID == 0 {
		t.Fatal("写入后 ID 应该被回填")
	}

	cases := []struct {
		name  string
		query string
		want  bool
	}{
		{"命中摘要里的词", "火锅", true},
		{"命中召回线索里的词", "晚餐", true},
		{"命中英文与数字", "延迟", true},
		{"不相关的词", "股票", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hits, err := db.Memory().SearchSummaries(ctx, "", tc.query, 5)
			if err != nil {
				t.Fatalf("检索失败: %v", err)
			}
			got := len(hits) > 0
			if got != tc.want {
				t.Errorf("查询 %q 命中 = %v，期望 %v（返回 %d 条）", tc.query, got, tc.want, len(hits))
			}
		})
	}
}

// TestSearchSummariesSessionScoped 验证会话内检索会带上全局记忆。
//
// 「群里认识的人，私聊时也该认得」这条产品要求就落在这里。
func TestSearchSummariesSessionScoped(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	now := time.Now().Unix()
	for _, s := range []*MemorySummary{
		{SessionID: "sess-a", Summary: "群A聊了摄影", CreatedAt: now},
		{SessionID: "sess-b", Summary: "群B也在聊摄影", CreatedAt: now},
		{SessionID: "", Summary: "全局记忆：摄影是他的爱好", CreatedAt: now},
	} {
		if err := db.Memory().SaveSummary(ctx, s); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
	}

	hits, err := db.Memory().SearchSummaries(ctx, "sess-a", "摄影", 10)
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}

	var sessions []string
	for _, h := range hits {
		sessions = append(sessions, h.SessionID)
	}

	// 必须包含 sess-a 与全局，不能包含 sess-b。
	hasA, hasGlobal, hasB := false, false, false
	for _, s := range sessions {
		switch s {
		case "sess-a":
			hasA = true
		case "":
			hasGlobal = true
		case "sess-b":
			hasB = true
		}
	}
	if !hasA {
		t.Error("应命中本会话的记忆")
	}
	if !hasGlobal {
		t.Error("应命中全局记忆（session_id 为空）")
	}
	if hasB {
		t.Error("不应命中其它会话的记忆")
	}
}

// TestSearchSummariesByTime 验证按时间范围检索。
func TestSearchSummariesByTime(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	base := time.Now().Unix()
	for i, s := range []*MemorySummary{
		{SessionID: "s", Summary: "昨天的事", CreatedAt: base - 86400},
		{SessionID: "s", Summary: "一小时前的事", CreatedAt: base - 3600},
		{SessionID: "s", Summary: "刚刚的事", CreatedAt: base},
	} {
		s.CoveredUntil = int64(i)
		if err := db.Memory().SaveSummary(ctx, s); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
	}

	hits, err := db.Memory().SearchSummariesByTime(ctx, "s", base-7200, 0, 10)
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("命中 %d 条，期望 2（排除昨天那条）", len(hits))
	}
}

// TestDeleteSummaryRemovesFTS 防的是「删了记录但 FTS 索引没删」。
//
// 外部内容模式的 FTS 表不会自动同步，忘了删就会检索到已经在
// memory_summaries 里不存在的 rowid，JOIN 之后静默丢行。
func TestDeleteSummaryRemovesFTS(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	s := &MemorySummary{
		SessionID: "sess",
		Summary:   "独一无二的关键词异形虫",
		CreatedAt: time.Now().Unix(),
	}
	if err := db.Memory().SaveSummary(ctx, s); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	before, err := db.Memory().SearchSummaries(ctx, "", "异形虫", 5)
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if len(before) != 1 {
		t.Fatalf("删除前命中 %d 条，期望 1", len(before))
	}

	if err := db.Memory().DeleteSummary(ctx, s.ID); err != nil {
		t.Fatalf("删除失败: %v", err)
	}

	after, err := db.Memory().SearchSummaries(ctx, "", "异形虫", 5)
	if err != nil {
		t.Fatalf("删除后检索失败: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("删除后仍命中 %d 条，FTS 索引没清理", len(after))
	}

	// FTS 表里也不该留下孤儿行。
	var orphan int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM memory_fts WHERE rowid = ?`, s.ID).Scan(&orphan); err != nil {
		t.Fatalf("查询 FTS 失败: %v", err)
	}
	if orphan != 0 {
		t.Errorf("memory_fts 里残留 %d 行", orphan)
	}
}

// TestLastCoveredUntil 验证增量摘要的游标。
func TestLastCoveredUntil(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	got, err := db.Memory().LastCoveredUntil(ctx, "sess")
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if got != 0 {
		t.Errorf("空库应返回 0，实际 %d", got)
	}

	for _, covered := range []int64{100, 500, 300} {
		s := &MemorySummary{
			SessionID:    "sess",
			Summary:      "x",
			CoveredUntil: covered,
			CreatedAt:    time.Now().Unix(),
		}
		if err := db.Memory().SaveSummary(ctx, s); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
	}

	got, err = db.Memory().LastCoveredUntil(ctx, "sess")
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if got != 500 {
		t.Errorf("应返回最大值 500，实际 %d", got)
	}
}

// TestProfileFallbackToGlobal 验证「会话画像缺失时回退到跨会话画像」。
func TestProfileFallbackToGlobal(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	now := time.Now().Unix()
	if err := db.Memory().UpsertProfile(ctx, &PersonProfile{
		Platform:     "qq",
		UserID:       "u1",
		SessionID:    "",
		DisplayName:  "小明",
		ProfileText:  "喜欢摄影",
		MessageCount: 10,
		UpdatedAt:    now,
	}); err != nil {
		t.Fatalf("写入全局画像失败: %v", err)
	}

	// 查私聊会话，库里只有全局画像，应该回退。
	got, err := db.Memory().GetProfile(ctx, "qq", "u1", "private-session")
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if got == nil {
		t.Fatal("应回退到全局画像，实际 nil")
	}
	if got.ProfileText != "喜欢摄影" {
		t.Errorf("画像内容 = %q", got.ProfileText)
	}

	// 写入会话专属画像后，应优先返回它。
	if err := db.Memory().UpsertProfile(ctx, &PersonProfile{
		Platform:    "qq",
		UserID:      "u1",
		SessionID:   "private-session",
		DisplayName: "小明",
		ProfileText: "私聊里更放得开",
		UpdatedAt:   now + 10,
	}); err != nil {
		t.Fatalf("写入会话画像失败: %v", err)
	}

	got, err = db.Memory().GetProfile(ctx, "qq", "u1", "private-session")
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if got.ProfileText != "私聊里更放得开" {
		t.Errorf("应优先会话专属画像，实际 = %q", got.ProfileText)
	}
}

// TestSaveExpressionDedupes 验证重复表达不重复入库。
func TestSaveExpressionDedupes(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().Unix()

	e := &Expression{
		SessionID: "s",
		Situation: "表示惊叹",
		Style:     "我嘞个去",
		CreatedAt: now,
	}
	created, err := db.Memory().SaveExpression(ctx, e)
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if !created {
		t.Error("首次写入应返回 true")
	}

	again, err := db.Memory().SaveExpression(ctx, e)
	if err != nil {
		t.Fatalf("二次写入失败: %v", err)
	}
	if again {
		t.Error("重复写入应返回 false")
	}

	n, err := db.Memory().CountExpressions(ctx)
	if err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if n != 1 {
		t.Errorf("总数 = %d，期望 1", n)
	}
}

// TestMarkExpressionsUsed 验证使用次数自增。
func TestMarkExpressionsUsed(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().Unix()

	e := &Expression{SessionID: "s", Situation: "a", Style: "b", CreatedAt: now}
	if _, err := db.Memory().SaveExpression(ctx, e); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	list, err := db.Memory().ListExpressions(ctx, "s", 5)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("条数 = %d", len(list))
	}

	if err := db.Memory().MarkExpressionsUsed(ctx, []int64{list[0].ID}, time.Now()); err != nil {
		t.Fatalf("标记失败: %v", err)
	}

	list, err = db.Memory().ListExpressions(ctx, "s", 5)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if list[0].UsedCount != 1 {
		t.Errorf("使用次数 = %d，期望 1", list[0].UsedCount)
	}
}

// TestSaveJargonUpdatesMeaning 验证黑话含义可以被补全。
func TestSaveJargonUpdatesMeaning(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().Unix()

	if err := db.Memory().SaveJargon(ctx, &Jargon{
		Content: "yyds", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	if err := db.Memory().SaveJargon(ctx, &Jargon{
		Content: "yyds", Meaning: "永远的神", CreatedAt: now, UpdatedAt: now + 1,
	}); err != nil {
		t.Fatalf("更新失败: %v", err)
	}

	list, err := db.Memory().ListJargons(ctx, 10)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("条数 = %d，期望 1（按 content 去重）", len(list))
	}
	if list[0].Meaning != "永远的神" {
		t.Errorf("含义 = %q，期望被补全", list[0].Meaning)
	}
}

// TestSearchSummariesReturnsEmbedMeta 防：列表查询不读 embed_model / embed_dim。
//
// 这两列只在写入侧写过（SaveSummary）、检索侧却没读回来，
// 结果 WebUI 永远显示「仅关键词」——即便向量早就备好了。
// 这条 bug 的特征是「不报错、不丢数据，只是界面上的判断永远是错的」，
// 只有把元信息一起断言才抓得到。
//
// 两条检索路径（FTS 关键词 / 按时间）都要覆盖：
// 它们用不同的 SQL，漏改其中一条同样会静默出错。
func TestSearchSummariesReturnsEmbedMeta(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	base := time.Now().Unix()
	s := &MemorySummary{
		SessionID:  "sess-embed",
		Summary:    "这条记忆里有独一无二的词：向量元信息校验",
		CreatedAt:  base,
		EmbedModel: "qwen-embed",
		EmbedDim:   1024,
		Embedding:  []byte{1, 2, 3, 4},
	}
	if err := db.Memory().SaveSummary(ctx, s); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	check := func(t *testing.T, label string, hits []*MemorySummary) {
		t.Helper()
		if len(hits) == 0 {
			t.Fatalf("%s: 没有命中", label)
		}
		got := hits[0]
		if got.EmbedModel != "qwen-embed" {
			t.Errorf("%s: EmbedModel = %q，期望 qwen-embed", label, got.EmbedModel)
		}
		if got.EmbedDim != 1024 {
			t.Errorf("%s: EmbedDim = %d，期望 1024", label, got.EmbedDim)
		}
	}

	t.Run("按时间", func(t *testing.T) {
		hits, err := db.Memory().SearchSummariesByTime(ctx, "sess-embed", 0, 0, 5)
		if err != nil {
			t.Fatalf("检索失败: %v", err)
		}
		check(t, "按时间", hits)
	})

	t.Run("关键词", func(t *testing.T) {
		hits, err := db.Memory().SearchSummaries(ctx, "sess-embed", "向量元信息校验", 5)
		if err != nil {
			t.Fatalf("检索失败: %v", err)
		}
		check(t, "关键词", hits)
	})
}

// TestSearchSummariesNaturalLanguageQuery 端到端验证自然语言查询能命中短文档
// （docs/audit/11-infra.md P1-6 点名要求的测试）。
//
// 以前查询侧用 AND 连接全部 token：「还记得上次吃的火锅吗」会切出
// 「得上」「次吃」这类跨词二元组，文档里不存在，AND 立即失败 → 0 命中。
// 而 query_memory 的 schema 明确接受「关键词或问题」，模型几乎总是传整句。
// 现在查询侧 OR 连接 + 丢单字 + bm25 排序，句子式提问能命中。
func TestSearchSummariesNaturalLanguageQuery(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	s := &MemorySummary{
		SessionID: "sess-1",
		Summary:   "上次聊到火锅很好吃，约好周末再去那家店",
		CreatedAt: time.Now().Unix(),
	}
	if err := db.Memory().SaveSummary(ctx, s); err != nil {
		t.Fatalf("写入回想失败: %v", err)
	}

	for _, q := range []string{
		"还记得上次吃的火锅吗",   // 命中「上次」「火锅」
		"我们周末约好去吃什么",   // 命中「周末」「约好」
	} {
		hits, err := db.Memory().SearchSummaries(ctx, "", q, 5)
		if err != nil {
			t.Fatalf("查询 %q 失败: %v", q, err)
		}
		if len(hits) == 0 {
			t.Errorf("查询 %q 应命中（OR 连接后至少「上次/火锅」等二元组重合）", q)
		}
	}

	// 不相关查询不该命中：查询切出的二元组（推理/理模/模型/型太/太慢/慢了）
	// 与文档的所有 token 都不相交；单字已在查询侧丢弃，
	// 不会靠「的」「了」这类常用字误命中（这正是丢单字的原因之一）。
	hits, err := db.Memory().SearchSummaries(ctx, "", "推理模型太慢了", 5)
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("查询 %q 不应命中：%v", "推理模型太慢了", summarizeHits(hits))
	}
}

// TestSearchSummariesFullWidthQuery 验证全角查询能命中半角库文（P2-19）。
//
// Tokenize 里做了全角→半角归一，索引与查询两侧同口径后天然一致，
// 不需要这条查询走什么特殊分支。
func TestSearchSummariesFullWidthQuery(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	s := &MemorySummary{
		SessionID: "sess-1",
		Summary:   "他说GPT4很强，速度也不错",
		CreatedAt: time.Now().Unix(),
	}
	if err := db.Memory().SaveSummary(ctx, s); err != nil {
		t.Fatalf("写入回想失败: %v", err)
	}

	// 半角查询（对照组，必须命中）。
	hits, err := db.Memory().SearchSummaries(ctx, "", "gpt4", 5)
	if err != nil || len(hits) == 0 {
		t.Fatalf("半角查询应命中: hits=%d err=%v", len(hits), err)
	}

	// 全角查询：归一后与半角形态一致。
	hits, err = db.Memory().SearchSummaries(ctx, "", "ＧＰＴ４", 5)
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if len(hits) == 0 {
		t.Error("全角查询「ＧＰＴ４」应命中半角库文「GPT4」")
	}
}

// summarizeHits 把命中列表压成一行摘要，方便失败信息阅读。
func summarizeHits(hits []*MemorySummary) string {
	out := ""
	for i, h := range hits {
		if i > 0 {
			out += " / "
		}
		out += h.Summary
	}
	return out
}
