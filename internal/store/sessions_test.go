package store

// 本文件覆盖 SessionRepo 与 PersonRepo。
// 重点防线：Upsert 的"部分更新"语义（空值不覆盖、计数自增）。

import (
	"testing"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
)

// newSession 构造一条聊天流记录。
func newSession(id string, lastActive, created int64) *domain.Session {
	return &domain.Session{
		ID:           id,
		Platform:     "qq",
		AccountID:    "bot-1",
		Scope:        domain.ScopeGroup,
		TargetID:     "g-1",
		DisplayName:  "测试群",
		MessageCount: 0,
		LastActiveAt: unix(lastActive),
		CreatedAt:    unix(created),
	}
}

// TestSessionRepoUpsertInsert 防的是：首次插入时字段错位或 message_count 被多加一次。
func TestSessionRepoUpsertInsert(t *testing.T) {
	repo := openTestDB(t).Sessions()
	ctx := t.Context()

	s := newSession("sess-1", 200, 100)
	s.MessageCount = 1
	if err := repo.Upsert(ctx, s); err != nil {
		t.Fatalf("Upsert 插入失败: %v", err)
	}

	got, err := repo.Get(ctx, "sess-1")
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if got == nil {
		t.Fatal("Get 返回 nil，期望命中刚插入的会话")
	}
	if got.Platform != s.Platform || got.AccountID != s.AccountID ||
		got.Scope != s.Scope || got.TargetID != s.TargetID || got.DisplayName != s.DisplayName {
		t.Errorf("字段不一致: got=%+v want=%+v", got, s)
	}
	// 插入分支直接使用传入的 MessageCount，不自增。
	if got.MessageCount != 1 {
		t.Errorf("首次插入 MessageCount = %d，期望 1", got.MessageCount)
	}
	if !got.LastActiveAt.Equal(s.LastActiveAt) || !got.CreatedAt.Equal(s.CreatedAt) {
		t.Errorf("时间字段不一致: last=%v created=%v", got.LastActiveAt, got.CreatedAt)
	}
}

// TestSessionRepoUpsertConflictIncrementsCount 防的是：同一会话每来一条消息
// message_count 没自增（统计偏小），或本次传入的计数覆盖了历史累计值。
func TestSessionRepoUpsertConflictIncrementsCount(t *testing.T) {
	repo := openTestDB(t).Sessions()
	ctx := t.Context()

	first := newSession("sess-c", 200, 100)
	first.MessageCount = 1
	if err := repo.Upsert(ctx, first); err != nil {
		t.Fatalf("首次 Upsert 失败: %v", err)
	}

	// 冲突分支按传入的 MessageCount 累加，不再硬编码 +1。
	//
	// 这个行为以前是 +1，导致批量补录（例如出站消息补历史）少算：
	// 补 99 条只会加 1，而 WebUI 的条数长期偏小是不报错的。
	// 传入 0 时按 1 处理（调用方只是「碰一下」会话）。
	for i, incoming := range []int{0, 1, 99} {
		s := newSession("sess-c", int64(300+i*100), 999)
		s.MessageCount = incoming
		if err := repo.Upsert(ctx, s); err != nil {
			t.Fatalf("第 %d 次冲突 Upsert 失败: %v", i+2, err)
		}
	}

	got, err := repo.Get(ctx, "sess-c")
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if got == nil {
		t.Fatal("Get 返回 nil")
	}
	// 首次 1 + 0(按1) + 1 + 99 = 102
	if got.MessageCount != 102 {
		t.Errorf("MessageCount = %d，期望 102（首次 1 + 1 + 1 + 99）", got.MessageCount)
	}
	// created_at 不应被冲突分支改写。
	if got.CreatedAt.Unix() != 100 {
		t.Errorf("CreatedAt 被覆盖为 %d，期望保持 100", got.CreatedAt.Unix())
	}
	// last_active_at 应跟随最新一次写入。
	if got.LastActiveAt.Unix() != 500 {
		t.Errorf("LastActiveAt = %d，期望 500", got.LastActiveAt.Unix())
	}
}

// TestSessionRepoUpsertKeepsDisplayNameWhenEmpty 防的是：QQ 官方事件不带群名，
// 空 display_name 把已补齐的群名冲掉。
func TestSessionRepoUpsertKeepsDisplayNameWhenEmpty(t *testing.T) {
	repo := openTestDB(t).Sessions()
	ctx := t.Context()

	s := newSession("sess-name", 100, 100)
	s.DisplayName = "原始群名"
	if err := repo.Upsert(ctx, s); err != nil {
		t.Fatalf("首次 Upsert 失败: %v", err)
	}

	blank := newSession("sess-name", 200, 100)
	blank.DisplayName = ""
	if err := repo.Upsert(ctx, blank); err != nil {
		t.Fatalf("空名 Upsert 失败: %v", err)
	}
	got, err := repo.Get(ctx, "sess-name")
	if err != nil || got == nil {
		t.Fatalf("Get 失败: %v, %+v", err, got)
	}
	if got.DisplayName != "原始群名" {
		t.Errorf("空 display_name 覆盖了原值: %q，期望 %q", got.DisplayName, "原始群名")
	}

	// 非空时应更新。
	updated := newSession("sess-name", 300, 100)
	updated.DisplayName = "新群名"
	if err := repo.Upsert(ctx, updated); err != nil {
		t.Fatalf("更新名 Upsert 失败: %v", err)
	}
	got, err = repo.Get(ctx, "sess-name")
	if err != nil || got == nil {
		t.Fatalf("Get 失败: %v, %+v", err, got)
	}
	if got.DisplayName != "新群名" {
		t.Errorf("display_name = %q，期望 %q", got.DisplayName, "新群名")
	}
}

// TestSessionRepoGetMissingReturnsNil 防的是：未命中被当成错误上报，
// 上层无法用 nil 判断"会话还没建"。
func TestSessionRepoGetMissingReturnsNil(t *testing.T) {
	repo := openTestDB(t).Sessions()

	got, err := repo.Get(t.Context(), "nope")
	if err != nil {
		t.Fatalf("Get(不存在) 返回错误: %v", err)
	}
	if got != nil {
		t.Errorf("Get(不存在) = %+v，期望 nil", got)
	}
}

// TestSessionRepoSetDisplayName 防的是：外部补齐群名失效，
// 尤其是「值未变化时不应产生多余写入」以及「不存在的会话必须报错」。
func TestSessionRepoSetDisplayName(t *testing.T) {
	repo := openTestDB(t).Sessions()
	ctx := t.Context()

	s := newSession("sess-set", 100, 100)
	if err := repo.Upsert(ctx, s); err != nil {
		t.Fatalf("Upsert 失败: %v", err)
	}

	if err := repo.SetDisplayName(ctx, "sess-set", "补齐的群名"); err != nil {
		t.Fatalf("SetDisplayName 失败: %v", err)
	}
	got, err := repo.Get(ctx, "sess-set")
	if err != nil || got == nil {
		t.Fatalf("Get 失败: %v, %+v", err, got)
	}
	if got.DisplayName != "补齐的群名" {
		t.Errorf("DisplayName = %q，期望 %q", got.DisplayName, "补齐的群名")
	}

	// 幂等：再设同一个值不应报错，也不改变结果。
	if err := repo.SetDisplayName(ctx, "sess-set", "补齐的群名"); err != nil {
		t.Fatalf("重复 SetDisplayName 失败: %v", err)
	}
	got, _ = repo.Get(ctx, "sess-set")
	if got.DisplayName != "补齐的群名" {
		t.Errorf("重复设置后 DisplayName = %q", got.DisplayName)
	}

	// 不存在的会话必须报错。
	//
	// 以前是静默成功（0 行受影响不报错），调用方完全无法发现
	// 「会话 ID 拼错了」—— 群名补不上但没有任何信号。
	if err := repo.SetDisplayName(ctx, "ghost", "x"); err == nil {
		t.Error("对不存在会话 SetDisplayName 应返回错误")
	}
	if n, err := repo.Count(ctx); err != nil || n != 1 {
		t.Errorf("Count = %d, err = %v，期望 1（不应创建新行）", n, err)
	}
}

// TestSessionRepoListOrderLimitAndCount 防的是：WebUI 会话列表顺序错乱、
// limit<=0 没有兜底默认值、Count 与 List 口径不一致。
func TestSessionRepoListOrderLimitAndCount(t *testing.T) {
	repo := openTestDB(t).Sessions()
	ctx := t.Context()

	for _, it := range []struct {
		id string
		at int64
	}{
		{"old", 100},
		{"new", 300},
		{"mid", 200},
	} {
		if err := repo.Upsert(ctx, newSession(it.id, it.at, 50)); err != nil {
			t.Fatalf("Upsert %s 失败: %v", it.id, err)
		}
	}

	list, err := repo.List(ctx, 10)
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("List 返回 %d 条，期望 3", len(list))
	}
	wantOrder := []string{"new", "mid", "old"}
	for i, want := range wantOrder {
		if list[i].ID != want {
			t.Errorf("List[%d].ID = %q，期望 %q（应按最近活跃倒序）", i, list[i].ID, want)
		}
	}

	// limit <= 0 时按实现默认 100 条处理。
	all, err := repo.List(ctx, 0)
	if err != nil {
		t.Fatalf("List(0) 失败: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("List(0) 返回 %d 条，期望 3（默认 limit 应足够大）", len(all))
	}

	top, err := repo.List(ctx, 1)
	if err != nil {
		t.Fatalf("List(1) 失败: %v", err)
	}
	if len(top) != 1 || top[0].ID != "new" {
		t.Errorf("List(1) 结果错误: %+v", top)
	}

	if n, err := repo.Count(ctx); err != nil || n != 3 {
		t.Fatalf("Count = %d, err = %v，期望 3", n, err)
	}
}

// newPerson 构造一条用户记录。
func newPerson(platform, userID, nickname, card string, first, last int64) *domain.Person {
	return &domain.Person{
		Platform:   platform,
		UserID:     userID,
		Nickname:   nickname,
		CardName:   card,
		FirstSeen:  unix(first),
		LastSeenAt: unix(last),
	}
}

// TestPersonRepoUpsertAndGet 防的是：card_name 与 nickname 互相覆盖、
// 空值把已有昵称/群名片清空、首次见到时间被后续写入改写。
func TestPersonRepoUpsertAndGet(t *testing.T) {
	repo := openTestDB(t).Persons()
	ctx := t.Context()

	p := newPerson("qq", "u-1", "昵称A", "群名片A", 100, 200)
	if err := repo.Upsert(ctx, p); err != nil {
		t.Fatalf("Upsert 插入失败: %v", err)
	}

	got, err := repo.Get(ctx, "qq", "u-1")
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if got == nil {
		t.Fatal("Get 返回 nil，期望命中刚插入的用户")
	}
	if got.Nickname != "昵称A" || got.CardName != "群名片A" {
		t.Errorf("昵称/群名片不一致: nickname=%q card=%q", got.Nickname, got.CardName)
	}
	if !got.FirstSeen.Equal(unix(100)) || !got.LastSeenAt.Equal(unix(200)) {
		t.Errorf("时间不一致: first=%v last=%v", got.FirstSeen, got.LastSeenAt)
	}

	// 冲突更新：只改 last_seen，nickname/card_name 为空时保留原值。
	blank := newPerson("qq", "u-1", "", "", 999, 300)
	if err := repo.Upsert(ctx, blank); err != nil {
		t.Fatalf("空值 Upsert 失败: %v", err)
	}
	got, _ = repo.Get(ctx, "qq", "u-1")
	if got.Nickname != "昵称A" || got.CardName != "群名片A" {
		t.Errorf("空值覆盖了已有字段: nickname=%q card=%q", got.Nickname, got.CardName)
	}
	if !got.LastSeenAt.Equal(unix(300)) {
		t.Errorf("LastSeenAt = %v，期望 300", got.LastSeenAt)
	}
	// first_seen 在 UPDATE 分支未赋值，应保持首次值（这是当前实现的语义）。
	if !got.FirstSeen.Equal(unix(100)) {
		t.Errorf("FirstSeen = %v，期望保持 100", got.FirstSeen)
	}

	// 非空值应分别更新各自字段，互不干扰。
	changed := newPerson("qq", "u-1", "昵称B", "群名片B", 999, 400)
	if err := repo.Upsert(ctx, changed); err != nil {
		t.Fatalf("更新 Upsert 失败: %v", err)
	}
	got, _ = repo.Get(ctx, "qq", "u-1")
	if got.Nickname != "昵称B" || got.CardName != "群名片B" {
		t.Errorf("更新失败: nickname=%q card=%q", got.Nickname, got.CardName)
	}

	// 只给群名片时，昵称必须保留。
	cardOnly := newPerson("qq", "u-1", "", "群名片C", 999, 500)
	if err := repo.Upsert(ctx, cardOnly); err != nil {
		t.Fatalf("仅群名片 Upsert 失败: %v", err)
	}
	got, _ = repo.Get(ctx, "qq", "u-1")
	if got.Nickname != "昵称B" || got.CardName != "群名片C" {
		t.Errorf("仅改群名片时字段错误: nickname=%q card=%q", got.Nickname, got.CardName)
	}
}

// TestPersonRepoPlatformIsolationAndCount 防的是：不同平台同名 user_id 被合并，
// 以及 Get 未命中的返回值语义。
func TestPersonRepoPlatformIsolationAndCount(t *testing.T) {
	repo := openTestDB(t).Persons()
	ctx := t.Context()

	if n, err := repo.Count(ctx); err != nil || n != 0 {
		t.Fatalf("空库 Count = %d, err = %v", n, err)
	}
	if got, err := repo.Get(ctx, "qq", "absent"); err != nil || got != nil {
		t.Fatalf("Get(未命中) = %+v, err = %v，期望 nil, nil", got, err)
	}

	// 同一 user_id 在两个平台应各自独立。
	if err := repo.Upsert(ctx, newPerson("qq", "u-9", "QQ昵称", "", 100, 100)); err != nil {
		t.Fatalf("Upsert qq 失败: %v", err)
	}
	if err := repo.Upsert(ctx, newPerson("discord", "u-9", "DC昵称", "", 100, 100)); err != nil {
		t.Fatalf("Upsert discord 失败: %v", err)
	}

	if n, err := repo.Count(ctx); err != nil || n != 2 {
		t.Fatalf("Count = %d, err = %v，期望 2", n, err)
	}
	qq, _ := repo.Get(ctx, "qq", "u-9")
	dc, _ := repo.Get(ctx, "discord", "u-9")
	if qq == nil || dc == nil {
		t.Fatalf("平台隔离失败: qq=%+v dc=%+v", qq, dc)
	}
	if qq.Nickname != "QQ昵称" || dc.Nickname != "DC昵称" {
		t.Errorf("跨平台串数据: qq=%q dc=%q", qq.Nickname, dc.Nickname)
	}

	// card_name 为空时 DisplayName 回退到 nickname（业务侧展示口径）。
	if qq.DisplayName() != "QQ昵称" {
		t.Errorf("DisplayName() = %q，期望回退到昵称", qq.DisplayName())
	}
}
