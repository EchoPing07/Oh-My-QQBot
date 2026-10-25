package web

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/store"
)

// TestMemoryAPI 覆盖 /api/memory 的三种查询方式。
func TestMemoryAPI(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	seedData(t, env.db)
	env.loginOK()

	// 播种三条记忆：本会话、其它会话、全局。
	ctx := context.Background()
	now := time.Now().Unix()
	for _, s := range []*store.MemorySummary{
		{
			SessionID: groupSessionID, Summary: "群A聊了摄影和火锅",
			RecallCues: []string{"讨论摄影"}, Participants: []string{"小明"},
			TimeRange: "2026/10/21 20:00", SourceCount: 8, CreatedAt: now,
		},
		{
			SessionID: privateSessionID, Summary: "私聊聊了天气",
			CreatedAt: now,
		},
		{
			SessionID: "", Summary: "全局：他喜欢摄影",
			CreatedAt: now,
		},
	} {
		if err := env.db.Memory().SaveSummary(ctx, s); err != nil {
			t.Fatalf("播种记忆失败: %v", err)
		}
	}

	t.Run("按会话查", func(t *testing.T) {
		resp, body := env.do(http.MethodGet, "/api/memory?session_id="+groupSessionID, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("状态码 = %d，响应: %s", resp.StatusCode, body)
		}

		var out struct {
			Items []store.MemorySummary `json:"items"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("解析响应失败: %v", err)
		}
		// 应包含本会话与全局，不含私聊。
		if len(out.Items) != 2 {
			t.Fatalf("返回 %d 条，期望 2", len(out.Items))
		}
		for _, it := range out.Items {
			if it.SessionID == privateSessionID {
				t.Error("不该返回其它会话的记忆")
			}
		}
	})

	t.Run("按关键词查", func(t *testing.T) {
		resp, body := env.do(http.MethodGet, "/api/memory?q=摄影", "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("状态码 = %d，响应: %s", resp.StatusCode, body)
		}

		var out struct {
			Items []store.MemorySummary `json:"items"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("解析响应失败: %v", err)
		}
		if len(out.Items) == 0 {
			t.Fatal("按关键词「摄影」应命中至少一条")
		}
	})

	t.Run("空结果是数组不是null", func(t *testing.T) {
		resp, body := env.do(http.MethodGet, "/api/memory?q=完全不存在的词xyz", "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("状态码 = %d", resp.StatusCode)
		}
		// nil 切片会序列化成 null，前端 forEach 会直接抛错。
		if !jsonHasArray(body, "items") {
			t.Errorf("items 应为数组而非 null，响应: %s", body)
		}
	})
}

// TestMemoryDeleteAPI 覆盖删除接口的参数校验与正常路径。
func TestMemoryDeleteAPI(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	seedData(t, env.db)
	env.loginOK()

	ctx := context.Background()
	s := &store.MemorySummary{
		SessionID: groupSessionID,
		Summary:   "待删除的记忆",
		CreatedAt: time.Now().Unix(),
	}
	if err := env.db.Memory().SaveSummary(ctx, s); err != nil {
		t.Fatalf("播种失败: %v", err)
	}

	t.Run("GET 被拒", func(t *testing.T) {
		// 删除是有副作用的操作，只允许 POST / DELETE。
		// 否则浏览器预取或爬虫扫到一个链接就会真的删掉数据。
		resp, _ := env.do(http.MethodGet, "/api/memory/delete?id="+itoa64(s.ID), "")
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("状态码 = %d，期望 405", resp.StatusCode)
		}
	})

	t.Run("缺 id 报 400", func(t *testing.T) {
		resp, _ := env.do(http.MethodPost, "/api/memory/delete", "")
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("状态码 = %d，期望 400", resp.StatusCode)
		}
	})

	t.Run("正常删除", func(t *testing.T) {
		resp, body := env.do(http.MethodPost, "/api/memory/delete?id="+itoa64(s.ID), "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("状态码 = %d，响应: %s", resp.StatusCode, body)
		}

		n, err := env.db.Memory().CountSummaries(ctx)
		if err != nil {
			t.Fatalf("统计失败: %v", err)
		}
		if n != 0 {
			t.Errorf("删除后还剩 %d 条", n)
		}
	})
}

// TestProfilesAPI 覆盖画像查询。
func TestProfilesAPI(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	seedData(t, env.db)
	env.loginOK()

	ctx := context.Background()
	if err := env.db.Memory().UpsertProfile(ctx, &store.PersonProfile{
		Platform:    "qq",
		UserID:      "u-profile-1",
		SessionID:   "",
		DisplayName: "小明",
		ProfileText: "喜欢摄影，说话简短",
		UpdatedAt:   time.Now().Unix(),
	}); err != nil {
		t.Fatalf("播种画像失败: %v", err)
	}

	t.Run("按 user_id 查", func(t *testing.T) {
		resp, body := env.do(http.MethodGet, "/api/profiles?user_id=u-profile-1", "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("状态码 = %d，响应: %s", resp.StatusCode, body)
		}

		var out struct {
			Items []store.PersonProfile `json:"items"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("解析响应失败: %v", err)
		}
		if len(out.Items) != 1 {
			t.Fatalf("返回 %d 条，期望 1", len(out.Items))
		}
		if out.Items[0].ProfileText != "喜欢摄影，说话简短" {
			t.Errorf("画像内容 = %q", out.Items[0].ProfileText)
		}
	})

	t.Run("不存在的用户返回空数组", func(t *testing.T) {
		resp, body := env.do(http.MethodGet, "/api/profiles?user_id=nobody", "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("状态码 = %d", resp.StatusCode)
		}
		if !jsonHasArray(body, "items") {
			t.Errorf("items 应为数组，响应: %s", body)
		}
	})

	t.Run("不给 user_id 时列出全部画像", func(t *testing.T) {
		// 以前这里刻意不提供列表，理由是隐私。实际后果是管理页
		// 只能靠人肉记住 OpenID 才能查人，等于这个页面没做。
		// 现在返回画像摘要列表（不是原始聊天记录），
		// 用它可以检查画像质量。
		resp, body := env.do(http.MethodGet, "/api/profiles", "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("状态码 = %d", resp.StatusCode)
		}

		var out struct {
			Items []store.PersonProfile `json:"items"`
			Total int                   `json:"total"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("解析响应失败: %v", err)
		}
		if len(out.Items) != 1 || out.Total != 1 {
			t.Fatalf("返回 %d 条（total=%d），期望 1", len(out.Items), out.Total)
		}
		if out.Items[0].UserID != "u-profile-1" {
			t.Errorf("列出的用户 = %q", out.Items[0].UserID)
		}
	})

	t.Run("按 session_id 过滤画像", func(t *testing.T) {
		resp, body := env.do(http.MethodGet, "/api/profiles?session_id=sess-does-not-exist", "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("状态码 = %d", resp.StatusCode)
		}
		var out struct {
			Items []store.PersonProfile `json:"items"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("解析响应失败: %v", err)
		}
		if len(out.Items) != 0 {
			t.Errorf("不属于该会话的画像被返回: %d 条", len(out.Items))
		}
	})
}

// TestEmojisAPI 覆盖表情包列表与按标签检索。
func TestEmojisAPI(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	seedData(t, env.db)
	env.loginOK()

	ctx := context.Background()
	now := time.Now().Unix()
	for i, e := range []*store.Emoji{
		{FileHash: "h1", FilePath: "a.png", Description: "开心，大笑", CreatedAt: now},
		{FileHash: "h2", FilePath: "b.png", Description: "无语", CreatedAt: now},
	} {
		_ = i
		if _, err := env.db.Emojis().Save(ctx, e); err != nil {
			t.Fatalf("播种表情包失败: %v", err)
		}
	}

	t.Run("列全部", func(t *testing.T) {
		resp, body := env.do(http.MethodGet, "/api/emojis", "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("状态码 = %d，响应: %s", resp.StatusCode, body)
		}

		var out struct {
			Items []store.Emoji `json:"items"`
			Total int64         `json:"total"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("解析响应失败: %v", err)
		}
		if out.Total != 2 {
			t.Errorf("total = %d，期望 2", out.Total)
		}
		if len(out.Items) != 2 {
			t.Errorf("返回 %d 条，期望 2", len(out.Items))
		}
	})

	t.Run("按标签检索", func(t *testing.T) {
		resp, body := env.do(http.MethodGet, "/api/emojis?q=开心", "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("状态码 = %d", resp.StatusCode)
		}

		var out struct {
			Items []store.Emoji `json:"items"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("解析响应失败: %v", err)
		}
		if len(out.Items) != 1 {
			t.Fatalf("返回 %d 条，期望 1", len(out.Items))
		}
		if out.Items[0].FileHash != "h1" {
			t.Errorf("命中 = %q", out.Items[0].FileHash)
		}
	})
}

// TestEmojiImageAPIPathTraversal 防的是「通过 hash 参数读到库外文件」。
//
// 攻击面：file_path 存在库里，如果被改成 ../../etc/passwd 之类，
// 直接 os.ReadFile 就会读出去。这里验证越界路径被拒绝。
func TestEmojiImageAPIPathTraversal(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	seedData(t, env.db)
	env.loginOK()

	// 造一个库外的文件，然后把指针指过去。
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("TOP-SECRET"), 0o644); err != nil {
		t.Fatalf("准备文件失败: %v", err)
	}

	ctx := context.Background()
	if _, err := env.db.Emojis().Save(ctx, &store.Emoji{
		FileHash:  "evil",
		FilePath:  outside,
		CreatedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("播种失败: %v", err)
	}

	resp, body := env.do(http.MethodGet, "/api/emoji/image?hash=evil", "")
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("越界路径不该被读取，实际返回了 %d 字节", len(body))
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("状态码 = %d，期望 403", resp.StatusCode)
	}
}

// TestEmojiImageAPIServesFile 验证正常路径能读到图片。
func TestEmojiImageAPIServesFile(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	seedData(t, env.db)
	env.loginOK()

	// 在配置的表情包目录下放一个文件。
	dir := env.cfg.Emoji.Dir
	rel := filepath.Join(dir, "ab", "test.png")
	if err := os.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(rel, []byte("PNGDATA"), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}

	ctx := context.Background()
	if _, err := env.db.Emojis().Save(ctx, &store.Emoji{
		FileHash:  "ok-hash",
		FilePath:  rel,
		CreatedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("播种失败: %v", err)
	}

	resp, body := env.do(http.MethodGet, "/api/emoji/image?hash=ok-hash", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，响应: %s", resp.StatusCode, body)
	}
	if string(body) != "PNGDATA" {
		t.Errorf("内容 = %q", string(body))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q", ct)
	}
}

// TestExpressionsAndJargonsAPI 覆盖表达与黑话列表。
func TestExpressionsAndJargonsAPI(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	seedData(t, env.db)
	env.loginOK()

	ctx := context.Background()
	now := time.Now().Unix()

	created, err := env.db.Memory().SaveExpression(ctx, &store.Expression{
		SessionID: groupSessionID,
		Situation: "表示惊叹",
		Style:     "我嘞个去",
		CreatedAt: now,
	})
	if err != nil || !created {
		t.Fatalf("播种表达失败: created=%v err=%v", created, err)
	}

	if err := env.db.Memory().SaveJargon(ctx, &store.Jargon{
		Content: "yyds", Meaning: "永远的神", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("播种黑话失败: %v", err)
	}

	t.Run("表达列表", func(t *testing.T) {
		resp, body := env.do(http.MethodGet, "/api/expressions", "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("状态码 = %d，响应: %s", resp.StatusCode, body)
		}

		var out struct {
			Items []store.Expression `json:"items"`
			Total int64              `json:"total"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("解析响应失败: %v", err)
		}
		if out.Total != 1 || len(out.Items) != 1 {
			t.Fatalf("total=%d items=%d，期望都是 1", out.Total, len(out.Items))
		}
		if out.Items[0].Style != "我嘞个去" {
			t.Errorf("style = %q", out.Items[0].Style)
		}
	})

	t.Run("黑话列表", func(t *testing.T) {
		resp, body := env.do(http.MethodGet, "/api/jargons", "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("状态码 = %d，响应: %s", resp.StatusCode, body)
		}

		var out struct {
			Items []store.Jargon `json:"items"`
			Total int64          `json:"total"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("解析响应失败: %v", err)
		}
		if out.Total != 1 || len(out.Items) != 1 {
			t.Fatalf("total=%d items=%d，期望都是 1", out.Total, len(out.Items))
		}
		if out.Items[0].Meaning != "永远的神" {
			t.Errorf("meaning = %q", out.Items[0].Meaning)
		}
	})

	t.Run("空库返回数组成员空数组", func(t *testing.T) {
		// 用一个全新环境，验证空列表不是 null。
		fresh := newTestEnv(t, envOptions{})
		seedData(t, fresh.db)
		fresh.loginOK()

		for _, path := range []string{"/api/expressions", "/api/jargons", "/api/emojis"} {
			resp, body := fresh.do(http.MethodGet, path, "")
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s 状态码 = %d，响应: %s", path, resp.StatusCode, body)
			}
			if !jsonHasArray(body, "items") {
				t.Errorf("%s 的 items 应为数组，响应: %s", path, body)
			}
		}
	})
}

// TestNewAPIsRequireAuth 防的是「新接口忘了加鉴权」。
//
// 这是最容易漏的地方：routes() 里手写注册，新增一行忘了包 requireAuth
// 就会把记忆内容、表情包文件全部暴露给未登录访问者。
func TestNewAPIsRequireAuth(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	seedData(t, env.db)
	// 刻意不登录：本测试就是要验证未鉴权访问被拒绝。

	// 不带 Cookie 直接访问。
	paths := []string{
		"/api/memory",
		"/api/memory/delete",
		"/api/profiles",
		"/api/emojis",
		"/api/emoji/image?hash=x",
		"/api/media?hash=x",
		"/api/expressions",
		"/api/jargons",
	}

	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			resp, body := env.doWithCookie(http.MethodGet, p, "", "")
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("状态码 = %d，期望 401（未鉴权）", resp.StatusCode)
			}
			// 未登录时不能泄露任何数据内容。
			if len(body) > 200 {
				t.Errorf("未鉴权响应体过大，可能泄露了数据: %s", body)
			}
		})
	}
}

// ============ 辅助 ============

// jsonHasArray 判断 JSON 顶层里某个 key 是数组。
func jsonHasArray(body []byte, key string) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return false
	}
	raw, ok := m[key]
	if !ok {
		return false
	}
	return len(raw) > 0 && raw[0] == '['
}

// jsonHasField 判断 JSON 顶层存在某个 key。
func jsonHasField(body []byte, key string) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return false
	}
	_, ok := m[key]
	return ok
}

// itoa64 把 int64 转成十进制字符串。
func itoa64(v int64) string {
	return strconv.FormatInt(v, 10)
}
