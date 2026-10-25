package store

// 本文件覆盖 UsageRepo、KVRepo、ReasoningRepo。
// 重点防线：聚合数字口径、KV 未命中语义、推理记录的排序与清理边界。

import (
	"errors"
	"testing"
)

// usage 构造一条调用记录。
func usage(task, model string, at int64, prompt, completion int, cost float64) *UsageRecord {
	return &UsageRecord{
		Task:             task,
		Model:            model,
		SessionID:        "sess",
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      prompt + completion,
		Cost:             cost,
		DurationMS:       42,
		CreatedAt:        unix(at),
	}
}

// TestUsageRepoSummaryAggregates 防的是：统计面板数字算错（漏行、SUM 口径不一致），
// 以及空结果集时 COALESCE 缺失导致 Scan 失败。
func TestUsageRepoSummaryAggregates(t *testing.T) {
	repo := openTestDB(t).Usage()
	ctx := t.Context()

	// 空表也应返回零值而不是错误。
	empty, err := repo.Summary(ctx, unix(0))
	if err != nil {
		t.Fatalf("空表 Summary 失败: %v", err)
	}
	if empty.Calls != 0 || empty.TotalTokens != 0 || empty.Cost != 0 {
		t.Errorf("空表 Summary = %+v，期望全零", empty)
	}

	records := []*UsageRecord{
		usage("planner", "m1", 100, 10, 5, 0.01),
		usage("planner", "m1", 200, 20, 10, 0.02),
		usage("reply", "m2", 300, 30, 15, 0.03),
		usage("reply", "m2", 400, 40, 20, 0.04),
		// 早于 since 的记录不应进入汇总。
		usage("planner", "m1", 50, 1000, 1000, 10),
	}
	for i, r := range records {
		if err := repo.Record(ctx, r); err != nil {
			t.Fatalf("Record 第 %d 条失败: %v", i, err)
		}
	}

	got, err := repo.Summary(ctx, unix(100))
	if err != nil {
		t.Fatalf("Summary 失败: %v", err)
	}
	want := UsageSummary{
		Calls:            4,
		PromptTokens:     10 + 20 + 30 + 40,
		CompletionTokens: 5 + 10 + 15 + 20,
		TotalTokens:      15 + 30 + 45 + 60,
		Cost:             0.01 + 0.02 + 0.03 + 0.04,
	}
	if got.Calls != want.Calls {
		t.Errorf("Calls = %d，期望 %d", got.Calls, want.Calls)
	}
	if got.PromptTokens != want.PromptTokens {
		t.Errorf("PromptTokens = %d，期望 %d", got.PromptTokens, want.PromptTokens)
	}
	if got.CompletionTokens != want.CompletionTokens {
		t.Errorf("CompletionTokens = %d，期望 %d", got.CompletionTokens, want.CompletionTokens)
	}
	if got.TotalTokens != want.TotalTokens {
		t.Errorf("TotalTokens = %d，期望 %d", got.TotalTokens, want.TotalTokens)
	}
	// 浮点累加用容差比较。
	if diff := got.Cost - want.Cost; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("Cost = %v，期望 %v", got.Cost, want.Cost)
	}

	// since 为未来时间时应归零。
	future, err := repo.Summary(ctx, unix(10_000))
	if err != nil {
		t.Fatalf("Summary(未来) 失败: %v", err)
	}
	if future.Calls != 0 {
		t.Errorf("Summary(未来).Calls = %d，期望 0", future.Calls)
	}
}

// TestUsageRepoByTask 防的是：按任务分组时漏任务、分组数字算错，
// 或排序没按 total_tokens 倒序（前端默认展示顺序依赖它）。
func TestUsageRepoByTask(t *testing.T) {
	repo := openTestDB(t).Usage()
	ctx := t.Context()

	// 空表应返回空切片且不报错。
	empty, err := repo.ByTask(ctx, unix(0))
	if err != nil {
		t.Fatalf("空表 ByTask 失败: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("空表 ByTask 返回 %d 条，期望 0", len(empty))
	}

	for _, r := range []*UsageRecord{
		usage("small", "m", 100, 1, 1, 0.001), // 2 tokens
		usage("big", "m", 200, 50, 50, 0.5),   // 100 tokens
		usage("big", "m", 300, 25, 25, 0.25),  // 50 tokens，累计 150
		usage("small", "m", 400, 2, 2, 0.002), // 累计 2+4=6 tokens
		usage("tiny", "m", 500, 1, 0, 0.0005), // 1 token
		usage("tiny", "m", 10, 1000, 0, 10.0), // 早于 since，应被过滤
	} {
		if err := repo.Record(ctx, r); err != nil {
			t.Fatalf("Record 失败: %v", err)
		}
	}

	got, err := repo.ByTask(ctx, unix(100))
	if err != nil {
		t.Fatalf("ByTask 失败: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("ByTask 返回 %d 组，期望 3: %+v", len(got), got)
	}

	// 按 total_tokens 倒序：big(150) > small(4) > tiny(1)。
	wantOrder := []string{"big", "small", "tiny"}
	for i, want := range wantOrder {
		if got[i].Task != want {
			t.Errorf("ByTask[%d].Task = %q，期望 %q（应按 total_tokens 倒序）", i, got[i].Task, want)
		}
	}

	byTask := map[string]TaskUsage{}
	for _, t2 := range got {
		byTask[t2.Task] = t2
	}

	if v := byTask["big"]; v.Calls != 2 || v.TotalTokens != 150 {
		t.Errorf("big = %+v，期望 calls=2 total=150", v)
	}
	if v := byTask["small"]; v.Calls != 2 || v.TotalTokens != 6 {
		t.Errorf("small = %+v，期望 calls=2 total=6", v)
	}
	if v := byTask["tiny"]; v.Calls != 1 || v.TotalTokens != 1 {
		t.Errorf("tiny = %+v，期望 calls=1 total=1（旧记录应被 since 过滤）", v)
	}
	// 费用聚合校验（容差）。
	if v := byTask["big"]; v.Cost < 0.7499 || v.Cost > 0.7501 {
		t.Errorf("big.Cost = %v，期望 0.75", v.Cost)
	}
}

// TestKVRepoSetGetDelete 防的是：键值状态（OpenID 映射、运行时开关）读写不一致，
// 以及「键不存在」与「值就是空串」分不清。
//
// 旧实现两者都返回 ("", nil)，于是想区分「删除过」和「清空过」根本做不到。
// 现在 Get 未命中返回 ErrKVNotFound，Lookup 保留「没有就从头开始」的旧语义。
func TestKVRepoSetGetDelete(t *testing.T) {
	repo := openTestDB(t).KV()
	ctx := t.Context()

	// Get：未命中必须显式报错，不能与空串混为一谈。
	if _, err := repo.Get(ctx, "absent"); !errors.Is(err, ErrKVNotFound) {
		t.Fatalf("Get(不存在) err = %v，期望 ErrKVNotFound", err)
	}

	// Lookup：未命中是正常的空状态。
	if v, ok, err := repo.Lookup(ctx, "absent"); err != nil || ok || v != "" {
		t.Errorf("Lookup(不存在) = (%q,%v,%v)，期望 (\"\",false,nil)", v, ok, err)
	}

	// 存空串之后：Get 成功返回空串，Lookup 的 ok 为 true。
	if err := repo.Set(ctx, "empty", ""); err != nil {
		t.Fatalf("Set 空串失败: %v", err)
	}
	if v, err := repo.Get(ctx, "empty"); err != nil || v != "" {
		t.Errorf("Get(空串) = %q, err = %v，期望 (\"\",nil)", v, err)
	}
	if v, ok, err := repo.Lookup(ctx, "empty"); err != nil || !ok || v != "" {
		t.Errorf("Lookup(空串) = (%q,%v,%v)，期望 (\"\",true,nil)", v, ok, err)
	}

	if err := repo.Set(ctx, "k1", "v1"); err != nil {
		t.Fatalf("Set 失败: %v", err)
	}
	if got, err := repo.Get(ctx, "k1"); err != nil || got != "v1" {
		t.Fatalf("Get = %q, err = %v，期望 v1", got, err)
	}

	// 覆盖写。
	if err := repo.Set(ctx, "k1", "v2"); err != nil {
		t.Fatalf("覆盖 Set 失败: %v", err)
	}
	if got, err := repo.Get(ctx, "k1"); err != nil || got != "v2" {
		t.Fatalf("覆盖后 Get = %q, err = %v，期望 v2", got, err)
	}

	// 空值也应能正常存取（表中 NOT NULL 但允许空串）。
	if err := repo.Set(ctx, "empty", ""); err != nil {
		t.Fatalf("Set 空值失败: %v", err)
	}
	if got, err := repo.Get(ctx, "empty"); err != nil || got != "" {
		t.Fatalf("空值 Get = %q, err = %v", got, err)
	}

	// 删除。
	if err := repo.Delete(ctx, "k1"); err != nil {
		t.Fatalf("Delete 失败: %v", err)
	}
	if _, err := repo.Get(ctx, "k1"); !errors.Is(err, ErrKVNotFound) {
		t.Fatalf("删除后 Get err = %v，期望 ErrKVNotFound", err)
	}

	// 删除不存在的键应静默成功。
	if err := repo.Delete(ctx, "ghost"); err != nil {
		t.Errorf("Delete(不存在) 返回错误: %v", err)
	}

	// updated_at 应随写入落地（秒级）。直接取同包的底层句柄查询，不重复打开数据库。
	var updatedAt int64
	if err := repo.db.SQL().QueryRowContext(ctx,
		`SELECT updated_at FROM kv WHERE key = 'empty'`).Scan(&updatedAt); err != nil {
		t.Fatalf("查询 updated_at 失败: %v", err)
	}
	if updatedAt == 0 {
		t.Errorf("updated_at 未写入")
	}
}

// TestReasoningRepoSaveAndListOrder 防的是：推理回放顺序错乱（前端按返回顺序渲染）、
// limit<=0 没有默认值导致只返回 0 条，以及 LastInsertId 没回填。
func TestReasoningRepoSaveAndListOrder(t *testing.T) {
	repo := openTestDB(t).Reasoning()
	ctx := t.Context()

	// 空会话返回空切片。
	empty, err := repo.ListBySession(ctx, "s-none", 10)
	if err != nil {
		t.Fatalf("空会话 ListBySession 失败: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("空会话返回 %d 条，期望 0", len(empty))
	}

	// 写入 5 条，穿插另一会话以验证过滤。
	for i := 0; i < 5; i++ {
		rec := &ReasoningRecord{
			SessionID: "s-r",
			TurnID:    "turn-1",
			Round:     i,
			Kind:      "planner",
			Request:   "req",
			Response:  "resp",
			CreatedAt: int64(1000 + i),
		}
		if err := repo.Save(ctx, rec); err != nil {
			t.Fatalf("Save 第 %d 条失败: %v", i, err)
		}
		if rec.ID == 0 {
			t.Errorf("Save 未回填 ID（第 %d 条）", i)
		}
	}
	if err := repo.Save(ctx, &ReasoningRecord{
		SessionID: "s-other", Kind: "reply", CreatedAt: 1500,
	}); err != nil {
		t.Fatalf("Save 其他会话失败: %v", err)
	}

	got, err := repo.ListBySession(ctx, "s-r", 10)
	if err != nil {
		t.Fatalf("ListBySession 失败: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("返回 %d 条，期望 5（有跨会话泄漏）", len(got))
	}
	for i, rec := range got {
		if rec.SessionID != "s-r" {
			t.Errorf("返回了其他会话的记录: %q", rec.SessionID)
		}
		if rec.Round != i {
			t.Errorf("第 %d 条 Round = %d，期望 %d（应为正序）", i, rec.Round, i)
		}
		if rec.Request != "req" || rec.Response != "resp" || rec.Kind != "planner" {
			t.Errorf("第 %d 条内容不一致: %+v", i, rec)
		}
	}

	// limit 应取"最近的 N 条"，但仍按正序返回。
	limited, err := repo.ListBySession(ctx, "s-r", 2)
	if err != nil {
		t.Fatalf("带 limit 的 ListBySession 失败: %v", err)
	}
	if len(limited) != 2 {
		t.Fatalf("limit=2 返回 %d 条", len(limited))
	}
	if limited[0].Round != 3 || limited[1].Round != 4 {
		t.Errorf("limit 截取错误: rounds=%d,%d，期望 3,4", limited[0].Round, limited[1].Round)
	}

	// limit<=0 时实现默认 200，这里只验证"不会返回 0 条"。
	defaulted, err := repo.ListBySession(ctx, "s-r", 0)
	if err != nil {
		t.Fatalf("ListBySession(0) 失败: %v", err)
	}
	if len(defaulted) != 5 {
		t.Errorf("ListBySession(0) 返回 %d 条，期望 5（默认 limit 应足够大）", len(defaulted))
	}
}

// TestReasoningRepoCleanup 防的是：清理任务算错边界（< vs <=），
// 造成该留的被删或该删的留下，以及返回的删除行数不准。
func TestReasoningRepoCleanup(t *testing.T) {
	repo := openTestDB(t).Reasoning()
	ctx := t.Context()

	for _, at := range []int64{100, 200, 300, 400} {
		if err := repo.Save(ctx, &ReasoningRecord{
			SessionID: "s-c", Kind: "planner", CreatedAt: at,
		}); err != nil {
			t.Fatalf("Save(at=%d) 失败: %v", at, err)
		}
	}

	// before=300 删除 created_at < 300，即 100 和 200 两条。
	n, err := repo.Cleanup(ctx, 300)
	if err != nil {
		t.Fatalf("Cleanup 失败: %v", err)
	}
	if n != 2 {
		t.Errorf("Cleanup 删除 %d 行，期望 2", n)
	}

	left, err := repo.ListBySession(ctx, "s-c", 10)
	if err != nil {
		t.Fatalf("清理后 ListBySession 失败: %v", err)
	}
	if len(left) != 2 {
		t.Fatalf("剩余 %d 条，期望 2", len(left))
	}
	if left[0].CreatedAt != 300 || left[1].CreatedAt != 400 {
		t.Errorf("剩余记录边界错误: %d,%d，期望 300,400（边界值应保留）",
			left[0].CreatedAt, left[1].CreatedAt)
	}

	// 再清一次同一时间点：没有可删的，应返回 0 且不报错。
	again, err := repo.Cleanup(ctx, 300)
	if err != nil {
		t.Fatalf("重复 Cleanup 失败: %v", err)
	}
	if again != 0 {
		t.Errorf("重复 Cleanup 删除 %d 行，期望 0", again)
	}

	// 清理到未来：全部删除。
	all, err := repo.Cleanup(ctx, 10_000)
	if err != nil {
		t.Fatalf("Cleanup(未来) 失败: %v", err)
	}
	if all != 2 {
		t.Errorf("Cleanup(未来) 删除 %d 行，期望 2", all)
	}
	if left, _ := repo.ListBySession(ctx, "s-c", 10); len(left) != 0 {
		t.Errorf("全清后仍有 %d 条", len(left))
	}
}

// TestReasoningListBySessionOrdersByTime 防：ListBySession 按自增 id 排序，
// 导致补录历史（created_at 早于已有记录）时列出来的顺序是错的，
// 而且和 Cleanup 的 created_at 口径对不上。
func TestReasoningListBySessionOrdersByTime(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()

	const sessionID = "sess-order"

	// 故意乱序写入：先写「晚的」，再写「早的」。
	// 如果按 id 排序，「早的」会被排到后面；按时间排序才对。
	late := &ReasoningRecord{
		SessionID: sessionID, TurnID: "turn-2", Round: 2, Kind: "planner",
		Request: "late", Response: "late", CreatedAt: 2000,
	}
	early := &ReasoningRecord{
		SessionID: sessionID, TurnID: "turn-1", Round: 1, Kind: "planner",
		Request: "early", Response: "early", CreatedAt: 1000,
	}
	if err := db.Reasoning().Save(ctx, late); err != nil {
		t.Fatalf("写入 late 失败: %v", err)
	}
	if err := db.Reasoning().Save(ctx, early); err != nil {
		t.Fatalf("写入 early 失败: %v", err)
	}

	recs, err := db.Reasoning().ListBySession(ctx, sessionID, 10)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("记录数 = %d，期望 2", len(recs))
	}
	if recs[0].CreatedAt != 1000 {
		t.Errorf("首条 created_at = %d，期望 1000（按时间正序）", recs[0].CreatedAt)
	}
	if recs[1].CreatedAt != 2000 {
		t.Errorf("次条 created_at = %d，期望 2000", recs[1].CreatedAt)
	}
}

// TestReasoningListBySessionLimitsToLatest 防：limit 截取的不是最近的记录。
func TestReasoningListBySessionLimitsToLatest(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()

	const sessionID = "sess-limit"
	for i := 1; i <= 5; i++ {
		rec := &ReasoningRecord{
			SessionID: sessionID, TurnID: "t", Round: i, Kind: "planner",
			Request: "r", Response: "r", CreatedAt: int64(i * 100),
		}
		if err := db.Reasoning().Save(ctx, rec); err != nil {
			t.Fatalf("写入第 %d 条失败: %v", i, err)
		}
	}

	recs, err := db.Reasoning().ListBySession(ctx, sessionID, 2)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("记录数 = %d，期望 2", len(recs))
	}
	// 应取最近两条（created_at 400 与 500），并按正序返回。
	if recs[0].CreatedAt != 400 || recs[1].CreatedAt != 500 {
		t.Errorf("取到 created_at = [%d %d]，期望 [400 500]", recs[0].CreatedAt, recs[1].CreatedAt)
	}
}
