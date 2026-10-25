package emoji

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/llm"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/store"
)

// TestCollectSavesFileAndRecord 验证收藏会同时落盘与落库。
func TestCollectSavesFileAndRecord(t *testing.T) {
	m, db, dir := newTestManager(t, nil)
	ctx := context.Background()

	binary := []byte("fake-png-bytes")
	created, err := m.Collect(ctx, binary, "image/png", "sess-1", "user-1")
	if err != nil {
		t.Fatalf("收藏失败: %v", err)
	}
	if !created {
		t.Fatal("首次收藏应返回 true")
	}

	// 库里应有一条记录。
	list, err := db.Emojis().Random(ctx, 10)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("库里有 %d 条，期望 1", len(list))
	}

	// 文件应真的存在。
	//
	// FilePath 已经包含 emoji 目录（形如 <dir>/3c/<hash>.png），
	// 是相对工作目录的路径，不能再拼一次 dir —— 拼了会得到
	// "<dir>/<dir>/3c/..." 这种非法路径。
	_ = dir
	data, err := os.ReadFile(list[0].FilePath)
	if err != nil {
		t.Fatalf("表情包文件不存在 %s: %v", list[0].FilePath, err)
	}
	if string(data) != string(binary) {
		t.Errorf("文件内容不匹配")
	}
}

// TestCollectDedupesByContent 验证同一张图重复收只存一份。
func TestCollectDedupesByContent(t *testing.T) {
	m, db, _ := newTestManager(t, nil)
	ctx := context.Background()

	binary := []byte("same-bytes")
	for i := 0; i < 3; i++ {
		created, err := m.Collect(ctx, binary, "image/png", "s", "u")
		if err != nil {
			t.Fatalf("第 %d 次收藏失败: %v", i+1, err)
		}
		if i == 0 && !created {
			t.Error("首次应返回 true")
		}
		if i > 0 && created {
			t.Errorf("第 %d 次应返回 false（内容重复）", i+1)
		}
	}

	n, err := db.Emojis().Count(ctx)
	if err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if n != 1 {
		t.Errorf("库里有 %d 条，期望 1", n)
	}
}

// TestCollectSkipsOversize 验证超限文件不收藏。
func TestCollectSkipsOversize(t *testing.T) {
	m, db, _ := newTestManager(t, func(cfg *config.Config) {
		cfg.Emoji.MaxSizeKB = 1
	})
	ctx := context.Background()

	// 2KB 超过 1KB 上限。
	big := make([]byte, 2048)
	created, err := m.Collect(ctx, big, "image/png", "s", "u")
	if err != nil {
		t.Fatalf("不该报错（静默跳过）: %v", err)
	}
	if created {
		t.Error("超限文件不该被收藏")
	}

	n, _ := db.Emojis().Count(ctx)
	if n != 0 {
		t.Errorf("库里 %d 条，期望 0", n)
	}
}

// TestCollectSkipsWhenDisabled 验证两个开关各自生效。
func TestCollectSkipsWhenDisabled(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*config.Config)
	}{
		{"功能关闭", func(c *config.Config) { c.Emoji.Enabled = false }},
		{"收藏关闭", func(c *config.Config) { c.Emoji.CollectEnabled = false }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, db, _ := newTestManager(t, tc.mutate)

			created, err := m.Collect(context.Background(), []byte("x"), "image/png", "s", "u")
			if err != nil {
				t.Fatalf("不该报错: %v", err)
			}
			if created {
				t.Error("关闭时不该收藏")
			}
			if n, _ := db.Emojis().Count(context.Background()); n != 0 {
				t.Errorf("库里 %d 条，期望 0", n)
			}
		})
	}
}

// TestCollectStopsAtMaxCount 验证库满后不再收藏。
func TestCollectStopsAtMaxCount(t *testing.T) {
	m, db, _ := newTestManager(t, func(cfg *config.Config) {
		cfg.Emoji.MaxCount = 2
	})
	ctx := context.Background()

	for i := 0; i < 4; i++ {
		binary := []byte{byte(i)}
		if _, err := m.Collect(ctx, binary, "image/png", "s", "u"); err != nil {
			t.Fatalf("第 %d 次收藏失败: %v", i+1, err)
		}
	}

	n, err := db.Emojis().Count(ctx)
	if err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if n != 2 {
		t.Errorf("库里有 %d 条，期望上限 2", n)
	}
}

// TestCollectDescribesViaVLM 验证配了视觉模型时会异步补描述。
//
// 这是「表情包能按情绪检索」的前提：没有描述，Search 永远空。
func TestCollectDescribesViaVLM(t *testing.T) {
	srv, calls := newVLMServer(t, "开心，大笑，嘲讽")

	m, db, _ := newTestManager(t, func(cfg *config.Config) {
		cfg.LLM.Tasks.VLM.Models = []string{"mock-vlm"}
		cfg.LLM.Models = append(cfg.LLM.Models, config.ModelConfig{
			Name:            "mock-vlm",
			ModelIdentifier: "mock-vlm",
			Provider:        "mock",
			Visual:          true,
		})
	})
	m.srvURL = srv.URL
	cloneForLLM, err := m.cfg.Get().Clone()
	if err != nil {
		t.Fatalf("复制配置失败: %v", err)
	}
	m.llm = buildClient(cloneForLLM, srv.URL)

	ctx := context.Background()
	if _, err := m.Collect(ctx, []byte("png"), "image/png", "s", "u"); err != nil {
		t.Fatalf("收藏失败: %v", err)
	}

	// 等后台打标签完成。
	deadline := time.Now().Add(5 * time.Second)
	var desc string
	for time.Now().Before(deadline) {
		list, err := db.Emojis().Random(ctx, 1)
		if err == nil && len(list) == 1 && list[0].Description != "" {
			desc = list[0].Description
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if desc == "" {
		t.Fatal("描述没有被补上（VLM 打标签可能没跑）")
	}
	if !strings.Contains(desc, "开心") {
		t.Errorf("描述 = %q", desc)
	}
	if calls() == 0 {
		t.Error("VLM 没有被调用")
	}

	// 打完标签后应该能按情绪检索到。
	hits, err := db.Emojis().Search(ctx, "开心", 5)
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if len(hits) != 1 {
		t.Errorf("按「开心」检索命中 %d 条，期望 1", len(hits))
	}

	m.Close()
}

// TestCollectSkipsVLMWhenNotConfigured 验证没配视觉模型时不调用模型。
//
// 关键：给未配置 VLM 的部署塞图片请求会 400，而收藏本身应该照常成功。
func TestCollectSkipsVLMWhenNotConfigured(t *testing.T) {
	m, db, _ := newTestManager(t, nil)

	ctx := context.Background()
	if _, err := m.Collect(ctx, []byte("png"), "image/png", "s", "u"); err != nil {
		t.Fatalf("收藏失败: %v", err)
	}
	m.Close()

	list, err := db.Emojis().Random(ctx, 1)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(list) != 1 {
		t.Fatal("应有一条记录")
	}
	if list[0].Description != "" {
		t.Errorf("没配 VLM 时描述应为空，实际 = %q", list[0].Description)
	}
}

// TestExtensionFor 验证扩展名映射。
func TestExtensionFor(t *testing.T) {
	cases := map[string]string{
		"image/jpeg": ".jpg",
		"image/JPG":  ".jpg",
		"image/gif":  ".gif",
		"image/webp": ".webp",
		"image/bmp":  ".bmp",
		"image/png":  ".png",
		"":           ".png",
		"怪格式":        ".png",
	}
	for in, want := range cases {
		if got := extensionFor(in); got != want {
			t.Errorf("extensionFor(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// TestHashBytesStable 验证哈希的稳定性与区分度。
func TestHashBytesStable(t *testing.T) {
	a := hashBytes([]byte("abc"))
	b := hashBytes([]byte("abc"))
	c := hashBytes([]byte("abd"))

	if a != b {
		t.Error("同内容应得同哈希")
	}
	if a == c {
		t.Error("不同内容应得不同哈希")
	}
	if len(a) != 64 {
		t.Errorf("应返回 64 位十六进制，实际 %d", len(a))
	}
}

// ============ 测试辅助 ============

// testManager 是对 Manager 的包装，暴露测试需要的内部字段。
type testManager struct {
	*Manager
	srvURL string
}

func newTestManager(t *testing.T, mutate func(*config.Config)) (*testManager, *store.DB, string) {
	t.Helper()

	dir := t.TempDir()
	cfg := config.Default()
	cfg.QQ.AppID = "mock"
	cfg.QQ.AppSecret = "mock"
	cfg.Database.Path = filepath.Join(dir, "test.db")
	cfg.Emoji.Dir = filepath.Join(dir, "emoji")
	cfg.LLM.Providers = []config.ProviderConfig{{
		Name: "mock", BaseURL: "http://127.0.0.1:1", APIKey: "mock", Timeout: 2,
	}}
	cfg.LLM.Models = []config.ModelConfig{{
		Name: "mock-model", ModelIdentifier: "mock-model", Provider: "mock",
	}}
	cfg.LLM.Tasks.VLM.Models = nil // 默认不配 VLM

	if mutate != nil {
		mutate(cfg)
	}

	db, err := store.Open(t.Context(), cfg.Database.Path)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	cloneForLLM, err := cfg.Clone()
	if err != nil {
		t.Fatalf("复制配置失败: %v", err)
	}
	m := New(config.Wrap(cfg), db, buildClient(cloneForLLM, cfg.LLM.Providers[0].BaseURL))
	return &testManager{Manager: m}, db, dir
}

func buildClient(cfg *config.Config, baseURL string) *llm.Client {
	// 深拷贝 provider 列表，避免多个测试共用同一份配置指针互相污染。
	clone := *cfg
	clone.LLM.Providers = make([]config.ProviderConfig, len(cfg.LLM.Providers))
	copy(clone.LLM.Providers, cfg.LLM.Providers)
	if len(clone.LLM.Providers) > 0 {
		clone.LLM.Providers[0].BaseURL = baseURL
	}
	return llm.New(config.Wrap(&clone))
}

func newVLMServer(t *testing.T, reply string) (*httptest.Server, func() int) {
	t.Helper()

	var (
		mu sync.Mutex
		n  int
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		mu.Unlock()

		body := map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"role": "assistant", "content": reply}, "finish_reason": "stop"},
			},
			"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 5, "total_tokens": 10},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)

	return srv, func() int {
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}
