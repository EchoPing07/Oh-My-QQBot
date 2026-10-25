package memory

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/llm"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/store"
)

// TestNormalizeProfileText 验证模型输出的规范化。
//
// 模型经常加代码块、少分节、写前言。规范化保证注入格式稳定。
func TestNormalizeProfileText(t *testing.T) {
	raw := "好的，这是画像：\n```markdown\n# 人物画像\n## 身份设定\n- 大二学生\n## 稳定了解\n- 喜欢摄影\n```\n希望有帮助。"

	got := normalizeProfileText(raw, "小明")

	// 必须包含全部固定分节，缺的补「- 暂无」。
	for _, title := range profileSectionTitles {
		if !strings.Contains(got, "## "+title) {
			t.Errorf("缺少分节 %q", title)
		}
	}
	// 模型给的内容要保留。
	if !strings.Contains(got, "大二学生") {
		t.Errorf("丢失了身份设定内容：%s", got)
	}
	if !strings.Contains(got, "喜欢摄影") {
		t.Errorf("丢失了稳定了解内容：%s", got)
	}
	// 代码块与前后的废话要去掉。
	if strings.Contains(got, "```") {
		t.Errorf("代码块没被去掉：%s", got)
	}
	if strings.Contains(got, "希望有帮助") {
		t.Errorf("结尾废话没被去掉：%s", got)
	}
	// 必须以一级标题开头。
	if !strings.HasPrefix(strings.TrimSpace(got), "# 人物画像") {
		t.Errorf("应以 # 人物画像 开头：%s", got)
	}
	// 缺失的分节要有占位。
	if !strings.Contains(got, "- 暂无") {
		t.Errorf("缺失分节应补 - 暂无")
	}
}

// TestProfileInjectionText 验证只取前 5 节、丢掉占位。
func TestProfileInjectionText(t *testing.T) {
	full := `# 人物画像
## 身份设定
- 大二学生
## 关系设定
- 暂无
## 稳定了解
- 喜欢摄影
## 相处偏好
- 说话喜欢简短
## 近期互动
- 昨天聊了火锅
## 不确定信息
- 也许住在南方
## 维护备注
- 自动画像仅供内部参考`

	got := ProfileInjectionText(full)

	// 前 5 节的内容要保留。
	for _, want := range []string{"大二学生", "喜欢摄影", "说话喜欢简短", "昨天聊了火锅"} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少 %q，实际：%s", want, got)
		}
	}
	// 后 2 节不该出现。
	for _, bad := range []string{"也许住在南方", "自动画像仅供内部参考"} {
		if strings.Contains(got, bad) {
			t.Errorf("不该包含 %q（属于维护信息），实际：%s", bad, got)
		}
	}
	// 「- 暂无」是占位不是信息。
	if strings.Contains(got, "暂无") {
		t.Errorf("占位行不该被注入：%s", got)
	}
}

// TestProfileInjectionTextEmpty 验证空输入。
func TestProfileInjectionTextEmpty(t *testing.T) {
	if got := ProfileInjectionText(""); got != "" {
		t.Errorf("空输入应返回空串，实际 %q", got)
	}
}

// TestUpdateIfNeededRespectsThreshold 验证未达阈值不调用模型。
//
// 这是成本控制的关键：群里每轮都有人在说话，
// 如果每轮都更新画像，模型调用量会爆炸。
func TestUpdateIfNeededRespectsThreshold(t *testing.T) {
	var calls int
	srv, _ := newProfileServer(t, func() string {
		calls++
		return "## 身份设定\n- 测试"
	})

	db, cfg := openProfileTestDB(t)
	cfg.Memory.ProfileUpdateIntervalMessages = 5
	builder := NewProfileBuilder(config.Wrap(cfg), llm.New(config.Wrap(cfg)), db)

	// 只有 2 条消息，不够 5 条。
	msgs := []*domain.Message{
		{UserID: "u1", UserName: "小明", Segments: []domain.Segment{domain.Text("你好")}, ReceivedAt: time.Now()},
		{UserID: "u1", UserName: "小明", Segments: []domain.Segment{domain.Text("在吗")}, ReceivedAt: time.Now()},
	}

	updated, err := builder.UpdateIfNeeded(t.Context(), "qq", "u1", "sess-1", "小明", msgs)
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if updated {
		t.Error("未达阈值不该更新")
	}
	if calls != 0 {
		t.Errorf("未达阈值不该调用模型，实际调用 %d 次", calls)
	}

	_ = srv
}

// TestUpdateIfNeededGeneratesProfile 验证达标时会生成并落库。
func TestUpdateIfNeededGeneratesProfile(t *testing.T) {
	srv, calls := newProfileServer(t, func() string {
		return "## 身份设定\n- 大二学生\n## 稳定了解\n- 喜欢摄影"
	})

	db, cfg := openProfileTestDB(t)
	cfg.Memory.ProfileUpdateIntervalMessages = 2
	builder := NewProfileBuilder(config.Wrap(cfg), buildLLMClient(cfg, srv.URL), db)

	msgs := []*domain.Message{
		{UserID: "u1", UserName: "小明", Segments: []domain.Segment{domain.Text("我在读大二")}, ReceivedAt: time.Now()},
		{UserID: "u1", UserName: "小明", Segments: []domain.Segment{domain.Text("我周末去拍照")}, ReceivedAt: time.Now()},
	}

	updated, err := builder.UpdateIfNeeded(t.Context(), "qq", "u1", "sess-1", "小明", msgs)
	if err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	if !updated {
		t.Fatal("达标时应更新")
	}
	if calls() == 0 {
		t.Error("应该调用了模型")
	}

	// 落库检查。
	got, err := db.Memory().GetProfile(t.Context(), "qq", "u1", "sess-1")
	if err != nil {
		t.Fatalf("查询画像失败: %v", err)
	}
	if got == nil {
		t.Fatal("画像应该已经落库 —— 这是之前缺的写入方")
	}
	if !strings.Contains(got.ProfileText, "大二学生") {
		t.Errorf("画像内容 = %q", got.ProfileText)
	}
	if got.MessageCount != 2 {
		t.Errorf("消息计数 = %d，期望 2", got.MessageCount)
	}
}

// TestUpdateIfNeededSkipsOthersMessages 验证只统计本人的消息。
//
// 一个活跃群里，一轮可能带进 20 条别人的消息，
// 但这个人只说了一句 —— 按总条数判断会导致画像更新过于频繁。
func TestUpdateIfNeededSkipsOthersMessages(t *testing.T) {
	var calls int
	srv, _ := newProfileServer(t, func() string { calls++; return "x" })

	db, cfg := openProfileTestDB(t)
	cfg.Memory.ProfileUpdateIntervalMessages = 3
	builder := NewProfileBuilder(config.Wrap(cfg), buildLLMClient(cfg, srv.URL), db)

	msgs := []*domain.Message{
		{UserID: "u1", UserName: "小明", Segments: []domain.Segment{domain.Text("我说一句")}, ReceivedAt: time.Now()},
		{UserID: "u2", UserName: "小红", Segments: []domain.Segment{domain.Text("别人说1")}, ReceivedAt: time.Now()},
		{UserID: "u2", UserName: "小红", Segments: []domain.Segment{domain.Text("别人说2")}, ReceivedAt: time.Now()},
		{UserID: "u2", UserName: "小红", Segments: []domain.Segment{domain.Text("别人说3")}, ReceivedAt: time.Now()},
	}

	updated, err := builder.UpdateIfNeeded(t.Context(), "qq", "u1", "sess-1", "小明", msgs)
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	// u1 只说了 1 条，阈值是 3，不该触发。
	if updated {
		t.Error("只统计本人消息，1 条不该触发更新")
	}
	if calls != 0 {
		t.Errorf("不该调用模型，实际 %d 次", calls)
	}
}

// TestUpdateIfNeededSkipsWhenDisabled 验证开关关闭时不工作。
func TestUpdateIfNeededSkipsWhenDisabled(t *testing.T) {
	db, cfg := openProfileTestDB(t)
	cfg.Memory.ProfileEnabled = false

	builder := NewProfileBuilder(config.Wrap(cfg), llm.New(config.Wrap(cfg)), db)
	msgs := []*domain.Message{
		{UserID: "u1", Segments: []domain.Segment{domain.Text("x")}, ReceivedAt: time.Now()},
	}

	updated, err := builder.UpdateIfNeeded(t.Context(), "qq", "u1", "s", "n", msgs)
	if err != nil || updated {
		t.Errorf("关闭时不该更新: updated=%v err=%v", updated, err)
	}
}

// ============ 辅助 ============

func openProfileTestDB(t *testing.T) (*store.DB, *config.Config) {
	t.Helper()

	cfg := config.Default()
	cfg.QQ.AppID = "mock"
	cfg.QQ.AppSecret = "mock"
	cfg.Database.Path = filepath.Join(t.TempDir(), "test.db")

	db, err := store.Open(t.Context(), cfg.Database.Path)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	return db, cfg
}

func buildLLMClient(cfg *config.Config, baseURL string) *llm.Client {
	clone := *cfg
	clone.LLM.Providers = []config.ProviderConfig{{
		Name: "mock", BaseURL: baseURL, APIKey: "mock", Timeout: 5,
	}}
	clone.LLM.Models = []config.ModelConfig{{
		Name: "mock", ModelIdentifier: "mock", Provider: "mock",
	}}
	clone.LLM.Tasks.Utils.Models = []string{"mock"}
	return llm.New(config.Wrap(&clone))
}

func newProfileServer(t *testing.T, gen func() string) (*httptest.Server, func() int) {
	t.Helper()

	var (
		mu sync.Mutex
		n  int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"role": "assistant", "content": gen()}, "finish_reason": "stop"},
			},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 10, "total_tokens": 20},
		})
	}))
	t.Cleanup(srv.Close)

	return srv, func() int {
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}
