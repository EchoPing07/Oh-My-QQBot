package app

// 本文件覆盖 app 包里的纯逻辑部分：
//   - 执行命令的管理员校验（qq.assign_admin_ids）
//   - 保留期清理（database.retention_days）
//
// 不覆盖 init / Run：那是组装层，需要把整条链路（网关、模型、DB）立起来，
// 已有 e2e_test.go 的 httptest 假服务兜住。

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/logx"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/store"
)

// openAppTestDB 打开一个临时库并返回配置。
func openAppTestDB(t *testing.T) (*store.DB, *config.Config) {
	t.Helper()

	cfg := config.Default()
	cfg.Database.Path = filepath.Join(t.TempDir(), "test.db")

	db, err := store.Open(context.Background(), cfg.Database.Path)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, cfg
}

// TestCommandAdminGate 守的是 qq.assign_admin_ids 真的生效。
//
// 这个配置以前只存在于 struct 里，零处使用 —— 改它不生效。
// 不设限的后果：绑定命令会在群里公开回复「已把本群绑定到 xxx」，
// 任何人都能改映射，等于任何人都能污染数据关联。
func TestCommandAdminGate(t *testing.T) {
	db, cfg := openAppTestDB(t)

	t.Run("名单为空时不设限", func(t *testing.T) {
		cfg.QQ.AssignAdminIDs = nil
		h := newCommandHandler(config.Wrap(cfg), db)
		msg := &domain.Message{UserID: "anyone", PlainText: "/assign_id 12345"}
		if !h.allowed(msg) {
			t.Error("名单为空时任何人都应被允许（单人群自用场景）")
		}
	})

	t.Run("名单非空时只认名单", func(t *testing.T) {
		cfg.QQ.AssignAdminIDs = []string{"admin-openid", "other-admin"}
		h := newCommandHandler(config.Wrap(cfg), db)

		if !h.allowed(&domain.Message{UserID: "admin-openid"}) {
			t.Error("名单内用户应被允许")
		}
		if h.allowed(&domain.Message{UserID: "stranger"}) {
			t.Error("名单外用户不应被允许")
		}
	})
}

// TestCommandRejectsNonAdmin 守的是「非管理员发命令不产生任何副作用」。
func TestCommandRejectsNonAdmin(t *testing.T) {
	db, cfg := openAppTestDB(t)
	cfg.QQ.AssignAdminIDs = []string{"admin-openid"}

	h := newCommandHandler(config.Wrap(cfg), db)
	msg := &domain.Message{
		UserID:    "stranger",
		GroupID:   "g-openid",
		PlainText: "/assign_group_id 123456",
	}

	// 返回 true 表示「已处理」——消息不进聊天流，但也没有真的绑定。
	if !h.Handle(msg, domain.ScopeGroup, "g-openid") {
		t.Fatal("被拒绝的命令也应标记为已处理，否则会流进聊天流")
	}

	// 库里不该出现任何映射。
	raw, _, err := db.KV().Lookup(context.Background(), idMapKey)
	if err != nil {
		t.Fatalf("读取映射失败: %v", err)
	}
	if raw != "" {
		t.Errorf("非管理员不该写入映射，实际: %s", raw)
	}
}

// TestCommandBindsMapping 守的是管理员发命令能真的写入映射。
func TestCommandBindsMapping(t *testing.T) {
	db, cfg := openAppTestDB(t)
	cfg.QQ.AssignAdminIDs = []string{"admin-openid"}

	h := newCommandHandler(config.Wrap(cfg), db)
	msg := &domain.Message{
		UserID:    "admin-openid",
		GroupID:   "g-openid",
		PlainText: "/assign_group_id 123456",
	}
	if !h.Handle(msg, domain.ScopeGroup, "g-openid") {
		t.Fatal("命令应被处理")
	}

	raw, _, err := db.KV().Lookup(context.Background(), idMapKey)
	if err != nil {
		t.Fatalf("读取映射失败: %v", err)
	}
	if raw == "" {
		t.Fatal("管理员执行命令后应有映射")
	}

	var m idMap
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("解析映射失败: %v", err)
	}
	if m.Group["g-openid"] != "123456" {
		t.Errorf("群映射 = %q，期望 123456", m.Group["g-openid"])
	}
}

// TestSweepRetention 守的是保留期清理真的删掉旧数据。
//
// 不清理的后果不是「慢慢变慢」而是「某天突然写不进去」：
// reasoning_records 每轮一条、单条可达 64KB，几个月的活跃群能到几 GB。
func TestSweepRetention(t *testing.T) {
	db, cfg := openAppTestDB(t)
	ctx := context.Background()

	// 一条 100 天前的旧消息 + 一条今天的。
	old := time.Now().AddDate(0, 0, -100)
	newer := time.Now()

	for _, it := range []struct {
		id string
		at time.Time
	}{
		{"old", old},
		{"new", newer},
	} {
		m := &domain.Message{
			ID:         it.id,
			Platform:   "qq",
			MessageID:  "pm-" + it.id,
			SessionID:  "sess-retention",
			UserID:     "u1",
			Segments:   []domain.Segment{domain.Text("x")},
			PlainText:  "x",
			ReceivedAt: it.at,
		}
		if err := db.Messages().Save(ctx, m); err != nil {
			t.Fatalf("播种失败: %v", err)
		}
	}

	// 旧的推理记录。
	if err := db.Reasoning().Save(ctx, &store.ReasoningRecord{
		SessionID: "sess-retention",
		TurnID:    "t1",
		Round:     1,
		Kind:      "planner",
		Request:   "{}",
		Response:  "{}",
		CreatedAt: old.Unix(),
	}); err != nil {
		t.Fatalf("写入推理记录失败: %v", err)
	}

	a := &App{cfg: config.Wrap(cfg), db: db, ctx: ctx, log: logx.Get("app.test")}
	a.sweepRetention(90)

	n, err := db.Messages().Count(ctx)
	if err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if n != 1 {
		t.Errorf("清理后剩 %d 条消息，期望 1（只留今天那条）", n)
	}

	got, err := db.Messages().ByMessageID(ctx, "pm-new")
	if err != nil || got == nil {
		t.Errorf("今天的消息被误删了: %v", err)
	}

	recs, err := db.Reasoning().ListBySession(ctx, "sess-retention", 10)
	if err != nil {
		t.Fatalf("查推理记录失败: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("过期推理记录还剩 %d 条", len(recs))
	}
}

// TestSweepRetentionDisabled 守的是 retention_days <= 0 时不清理。
//
// 「不设保留期」是合法配置，不能被当成「未设置」。
func TestSweepRetentionDisabled(t *testing.T) {
	db, cfg := openAppTestDB(t)
	cfg.Database.RetentionDays = 0

	ctx := context.Background()
	old := &domain.Message{
		ID:         "very-old",
		Platform:   "qq",
		MessageID:  "pm-very-old",
		SessionID:  "sess-r2",
		UserID:     "u1",
		Segments:   []domain.Segment{domain.Text("x")},
		PlainText:  "x",
		ReceivedAt: time.Now().AddDate(-5, 0, 0),
	}
	if err := db.Messages().Save(ctx, old); err != nil {
		t.Fatalf("播种失败: %v", err)
	}

	a := &App{cfg: config.Wrap(cfg), db: db, ctx: ctx, log: logx.Get("app.test")}
	// startRetentionSweeper 在 days<=0 时直接返回，不会启动协程。
	a.startRetentionSweeper()

	// 即使直接调 sweepRetention 传负数也不该删。
	a.sweepRetention(0)

	n, err := db.Messages().Count(ctx)
	if err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if n != 1 {
		t.Errorf("保留期未设置时不该删数据，实际剩 %d 条", n)
	}
}
