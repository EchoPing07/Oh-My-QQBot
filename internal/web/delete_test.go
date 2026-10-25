package web

// 删除类接口与 Prompt / 备份接口的测试。
//
// 本文件的两个重点：
//
//  1. **每个删除接口都必须限定 DELETE 方法。**
//     只认 query 参数不看方法的接口，一次浏览器预取、一次爬虫扫链接、
//     甚至一个 <img src="/api/xxx/delete?id=1"> 都会触发删除。
//     这是 AGENTS.md 第 64 条记过的真实教训。
//
//  2. **删除必须连带清理。**
//     删会话不删消息 → 消息页还看得到孤儿数据；
//     删表情包不删文件 → 磁盘一涨不降，用户「删了几百张，占用没变」。
//     这类问题不报错，只是数据慢慢不一致。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/store"
)

// seedMessage 往库里塞一条消息。
func seedMessage(t *testing.T, db *store.DB, sessionID, msgID, text string, at time.Time) {
	t.Helper()
	m := &domain.Message{
		// ID 必须唯一：库里 ON CONFLICT(id) DO NOTHING，
		// 全传空串时后写的会被静默忽略，测试会以为「消息没存进去」。
		ID:         msgID,
		MessageID:  msgID,
		SessionID:  sessionID,
		Platform:   "qq",
		AccountID:  "bot-1",
		GroupID:    "group-1",
		IsGroup:    true,
		UserID:     "user-1",
		UserName:   "测试用户",
		PlainText:  text,
		Segments:   []domain.Segment{{Type: domain.SegmentText, Text: text}},
		ReceivedAt: at,
	}
	if err := db.Messages().Save(context.Background(), m); err != nil {
		t.Fatalf("写入消息失败: %v", err)
	}
}

// seedSession 往库里塞一个会话。
func seedSession(t *testing.T, db *store.DB, id string) {
	t.Helper()
	err := db.Sessions().Upsert(context.Background(), &domain.Session{
		ID: id, Platform: "qq", AccountID: "bot-1",
		Scope: domain.ScopeGroup, TargetID: "group-1",
		DisplayName: "测试群", MessageCount: 1,
		LastActiveAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("写入会话失败: %v", err)
	}
}

// TestDeleteEndpointsRequireDeleteMethod 守「删除接口必须限定 DELETE」。
//
// 这是本文件最重要的一条：用 GET 调删除接口必须返回 405，
// 而不是「删成功了」。返回 200 意味着任何人都能通过一个链接删数据。
func TestDeleteEndpointsRequireDeleteMethod(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	env.loginOK()

	paths := []string{
		"/api/sessions/delete?id=s1",
		"/api/messages/delete?session_id=s1",
		"/api/expressions/delete?id=1",
		"/api/jargons/delete?id=1",
		"/api/emojis/delete?id=1",
		"/api/profiles/delete?user_id=u1",
	}
	for _, p := range paths {
		resp, body := env.do(http.MethodGet, p, "")
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("GET %s 状态码 = %d（期望 405），body=%s", p, resp.StatusCode, body)
		}
	}
}

// TestDeleteEndpointsRequireAuth 守「删除接口必须登录」。
func TestDeleteEndpointsRequireAuth(t *testing.T) {
	env := newTestEnv(t, envOptions{})

	paths := []string{
		"/api/sessions/delete?id=s1",
		"/api/messages/delete?session_id=s1",
		"/api/expressions/delete?id=1",
		"/api/jargons/delete?id=1",
		"/api/emojis/delete?id=1",
		"/api/profiles/delete?user_id=u1",
		"/api/prompts",
		"/api/backup",
	}
	for _, p := range paths {
		resp, _ := env.do(http.MethodDelete, p, "")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("DELETE %s 未鉴权时 = %d，期望 401", p, resp.StatusCode)
		}
	}
}

// TestDeleteSessionCascades 防：删了会话，消息与学习数据还留在库里。
//
// 参考实现的 delete_chat_session 会连带清理该会话的表达、黑话、
// 记忆摘要、画像（chat/routes.py:1165 _delete_chat_session_scope）。
// 不清理的表现是「会话没了，记忆页里还挂着一堆属于不存在会话的条目」。
func TestDeleteSessionCascades(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	env.loginOK()
	ctx := context.Background()

	seedSession(t, env.db, "s1")
	seedSession(t, env.db, "s2")
	seedMessage(t, env.db, "s1", "m1", "第一条", time.Now())
	seedMessage(t, env.db, "s2", "m2", "另一会话", time.Now())

	if _, err := env.db.Memory().SaveExpression(ctx, &store.Expression{
		SessionID: "s1", Situation: "打招呼", Style: "嗨",
	}); err != nil {
		t.Fatalf("写入表达失败: %v", err)
	}
	if err := env.db.Memory().SaveSummary(ctx, &store.MemorySummary{
		SessionID: "s1", Summary: "聊了会儿天", CreatedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("写入记忆失败: %v", err)
	}

	resp, body := env.do(http.MethodDelete, "/api/sessions/delete?id=s1", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, body)
	}

	// s1 的消息必须没了。
	msgs, err := env.db.Messages().Recent(ctx, "s1", 10)
	if err != nil {
		t.Fatalf("查消息失败: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("会话删了但消息还在: %d 条", len(msgs))
	}

	// s2 必须完好，否则说明删除范围超出了目标会话。
	msgs2, err := env.db.Messages().Recent(ctx, "s2", 10)
	if err != nil {
		t.Fatalf("查消息失败: %v", err)
	}
	if len(msgs2) != 1 {
		t.Errorf("另一个会话的消息被误删了: %d 条", len(msgs2))
	}

	// 学习数据也要清。
	exprs, err := env.db.Memory().ListExpressions(ctx, "s1", 100)
	if err != nil {
		t.Fatalf("查表达失败: %v", err)
	}
	if len(exprs) != 0 {
		t.Errorf("会话删了但表达还在: %d 条", len(exprs))
	}
}

// TestDeleteSessionNotFound 防：删不存在的会话返回 200。
func TestDeleteSessionNotFound(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	env.loginOK()

	resp, body := env.do(http.MethodDelete, "/api/sessions/delete?id=不存在", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("状态码 = %d（期望 404），body=%s", resp.StatusCode, body)
	}
}

// TestDeleteMessagesScopedToSession 防：删消息误删别人的。
func TestDeleteMessagesScopedToSession(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	env.loginOK()
	ctx := context.Background()

	seedSession(t, env.db, "s1")
	seedSession(t, env.db, "s2")
	seedMessage(t, env.db, "s1", "m1", "a", time.Now())
	seedMessage(t, env.db, "s1", "m2", "b", time.Now())
	seedMessage(t, env.db, "s2", "m3", "c", time.Now())

	resp, body := env.do(http.MethodDelete, "/api/messages/delete?session_id=s1", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, body)
	}

	got, err := env.db.Messages().Recent(ctx, "s2", 10)
	if err != nil {
		t.Fatalf("查消息失败: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("另一个会话的消息被误删: %d 条", len(got))
	}
}

// TestDeleteMessagesRequiresScope 防：漏传参数就清库。
//
// 这是最危险的一条：只传 limit 不传 session_id 时如果退化成「删全部」，
// 一次请求就能把整库消息清空，而界面上只是「点了删除」。
func TestDeleteMessagesRequiresScope(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	env.loginOK()
	ctx := context.Background()

	seedSession(t, env.db, "s1")
	seedMessage(t, env.db, "s1", "m1", "a", time.Now())

	// 不带 session_id 也不带 all=1 → 必须拒绝。
	resp, body := env.do(http.MethodDelete, "/api/messages/delete", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码 = %d（期望 400），body=%s", resp.StatusCode, body)
	}

	// 消息必须还在。
	got, err := env.db.Messages().Recent(ctx, "s1", 10)
	if err != nil {
		t.Fatalf("查消息失败: %v", err)
	}
	if len(got) != 1 {
		t.Error("参数缺失时消息被删了")
	}
}

// TestDeleteExpressionAndJargon 覆盖两条学习数据的删除。
func TestDeleteExpressionAndJargon(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	env.loginOK()
	ctx := context.Background()

	if _, err := env.db.Memory().SaveExpression(ctx, &store.Expression{
		SessionID: "s1", Situation: "被夸", Style: "嘿嘿",
	}); err != nil {
		t.Fatalf("写入表达失败: %v", err)
	}
	exprs, err := env.db.Memory().ListAllExpressions(ctx, 10)
	if err != nil || len(exprs) != 1 {
		t.Fatalf("读取表达失败: %v, n=%d", err, len(exprs))
	}

	resp, body := env.do(http.MethodDelete,
		"/api/expressions/delete?id="+itoa(exprs[0].ID), "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("删除表达状态码 = %d，body=%s", resp.StatusCode, body)
	}

	left, err := env.db.Memory().ListAllExpressions(ctx, 10)
	if err != nil {
		t.Fatalf("读取表达失败: %v", err)
	}
	if len(left) != 0 {
		t.Errorf("表达没被删掉: %d 条", len(left))
	}

	// 黑话同理。
	if err := env.db.Memory().SaveJargon(ctx, &store.Jargon{
		Content: "破防", Count: 1, CreatedAt: time.Now().Unix(), UpdatedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("写入黑话失败: %v", err)
	}
	js, err := env.db.Memory().ListJargons(ctx, 10)
	if err != nil || len(js) != 1 {
		t.Fatalf("读取黑话失败: %v, n=%d", err, len(js))
	}
	resp, body = env.do(http.MethodDelete, "/api/jargons/delete?id="+itoa(js[0].ID), "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("删除黑话状态码 = %d，body=%s", resp.StatusCode, body)
	}
	jsLeft, err := env.db.Memory().ListJargons(ctx, 10)
	if err != nil {
		t.Fatalf("读取黑话失败: %v", err)
	}
	if len(jsLeft) != 0 {
		t.Errorf("黑话没被删掉: %d 条", len(jsLeft))
	}
}

// TestDeleteRejectsBadID 防：非法 id 被当成 0 处理。
func TestDeleteRejectsBadID(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	env.loginOK()

	for _, p := range []string{
		"/api/expressions/delete",
		"/api/expressions/delete?id=",
		"/api/expressions/delete?id=abc",
		"/api/expressions/delete?id=0",
		"/api/expressions/delete?id=-3",
	} {
		resp, body := env.do(http.MethodDelete, p, "")
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("DELETE %s 状态码 = %d（期望 400），body=%s", p, resp.StatusCode, body)
		}
	}
}

// TestDeleteMissingRecordIs404 防：删不存在的东西返回 200。
func TestDeleteMissingRecordIs404(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	env.loginOK()

	for _, p := range []string{
		"/api/expressions/delete?id=99999",
		"/api/jargons/delete?id=99999",
		"/api/emojis/delete?id=99999",
	} {
		resp, body := env.do(http.MethodDelete, p, "")
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("DELETE %s 状态码 = %d（期望 404），body=%s", p, resp.StatusCode, body)
		}
	}
}

// TestPromptsListAndSave 覆盖提示词的读取 / 保存 / 恢复默认全链路。
//
// 用临时目录作为提示词根目录，验证「保存 → 磁盘上有文件 → 恢复默认 → 文件消失」。
func TestPromptsListAndSave(t *testing.T) {
	dir := t.TempDir()
	env := newTestEnv(t, envOptions{promptsDir: dir})
	env.loginOK()

	// 列表：必须列出内置的四个模板。
	resp, body := env.do(http.MethodGet, "/api/prompts", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, body)
	}
	for _, want := range []string{"planner", "replyer"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("列表里缺少 %s：%s", want, body)
		}
	}

	// 读单条：默认内容与内嵌一致。
	resp, body = env.do(http.MethodGet, "/api/prompts?name=planner", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "你的名字是{bot_name}") {
		t.Errorf("读到的默认内容不对：%s", truncateStr(string(body), 300))
	}

	// 保存。
	//
	// 内容必须保留默认模板的全部占位符：保存时会校验，
	// 少一个就 400（少了 {bot_name} 模型就不知道自己叫什么，
	// 而这种错不会报错，只会让机器人变得很奇怪）。
	resp, body = env.do(http.MethodPut, "/api/prompts",
		`{"name":"planner","content":"这是测试用的提示词 {bot_name}{bot_alias_notice}{behavior_style}{bot_nickname_notice}{group_chat_attention_block}{query_memory_rule}{emoji_hint_rule}"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("保存状态码 = %d，body=%s", resp.StatusCode, body)
	}
	// 再读一次必须是新内容。
	_, body = env.do(http.MethodGet, "/api/prompts?name=planner", "")
	if !strings.Contains(string(body), "这是测试用的提示词") {
		t.Errorf("保存后读到的还是旧内容：%s", truncateStr(string(body), 300))
	}
	if !strings.Contains(string(body), `"customized":true`) {
		t.Error("保存后 customized 应为 true")
	}

	// 恢复默认。
	resp, body = env.do(http.MethodDelete, "/api/prompts?name=planner", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("恢复状态码 = %d，body=%s", resp.StatusCode, body)
	}
	_, body = env.do(http.MethodGet, "/api/prompts?name=planner", "")
	if strings.Contains(string(body), "这是测试用的提示词") {
		t.Error("恢复默认后读到的还是自定义内容")
	}
	if !strings.Contains(string(body), `"customized":false`) {
		t.Error("恢复默认后 customized 应为 false")
	}
}

// TestPromptsRejectTraversal 防：提示词名字带 ../ 写出目录外。
func TestPromptsRejectTraversal(t *testing.T) {
	env := newTestEnv(t, envOptions{promptsDir: t.TempDir()})
	env.loginOK()

	for _, name := range []string{
		"../../etc/passwd",
		"..\\..\\windows",
		"a/b",
		"a.b",
		"",
	} {
		body := `{"name":"` + name + `","content":"x {bot_name}"}`
		resp, respBody := env.do(http.MethodPut, "/api/prompts", body)
		if resp.StatusCode == http.StatusOK {
			t.Errorf("名字 %q 被接受了：%s", name, respBody)
		}
	}
}

// TestPromptsRejectEmptyContent 防：保存空提示词。
//
// 空的 system prompt 会让模型行为完全不可预期，
// 而且不报错 —— 只是「机器人变得很奇怪」。
func TestPromptsRejectEmptyContent(t *testing.T) {
	env := newTestEnv(t, envOptions{promptsDir: t.TempDir()})
	env.loginOK()

	resp, body := env.do(http.MethodPut, "/api/prompts",
		`{"name":"planner","content":"   "}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码 = %d（期望 400），body=%s", resp.StatusCode, body)
	}
}

// TestPromptsReadOnlyEndpoints 守列表接口不被写方法误用。
func TestPromptsRejectsBadMethod(t *testing.T) {
	env := newTestEnv(t, envOptions{promptsDir: t.TempDir()})
	env.loginOK()

	resp, _ := env.do(http.MethodPost, "/api/prompts", `{"name":"planner","content":"x"}`)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST 状态码 = %d（期望 405）", resp.StatusCode)
	}
}

// TestBackupExportImport 覆盖备份的导出与导入。
func TestBackupExportImport(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	env.loginOK()

	seedSession(t, env.db, "s1")
	seedMessage(t, env.db, "s1", "m1", "备份前的内容", time.Now())

	// 导出。
	resp, body := env.do(http.MethodGet, "/api/backup", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("导出状态码 = %d，body=%s", resp.StatusCode, body)
	}
	if len(body) < 100 {
		t.Fatalf("导出的文件太小: %d 字节", len(body))
	}
	// SQLite 文件头。
	if string(body[:16]) != "SQLite format 3\x00" {
		t.Errorf("导出内容不是 SQLite 文件: %q", body[:16])
	}

	// 不带 confirm 的导入必须被拒。
	resp, respBody := env.do(http.MethodPost, "/api/backup", string(body))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("无 confirm 的导入状态码 = %d（期望 400），body=%s",
			resp.StatusCode, respBody)
	}

	// 上传的不是数据库也要拒。
	resp, respBody = env.do(http.MethodPost, "/api/backup?confirm=replace", "这不是数据库")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法文件的导入状态码 = %d（期望 400），body=%s",
			resp.StatusCode, respBody)
	}

	// 正常导入。
	resp, respBody = env.do(http.MethodPost, "/api/backup?confirm=replace", string(body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("导入状态码 = %d，body=%s", resp.StatusCode, respBody)
	}
	if !strings.Contains(string(respBody), `"restart":true`) {
		t.Errorf("导入后应提示需要重启：%s", respBody)
	}
}

// TestBackupRejectsBadMethod 守导入导出方法限制。
func TestBackupRejectsBadMethod(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	env.loginOK()

	resp, _ := env.do(http.MethodPut, "/api/backup", "")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("PUT 状态码 = %d（期望 405）", resp.StatusCode)
	}
}

// itoa 转 int64 字符串，测试里用得多。
func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// truncateStr 截断长文本，避免失败信息刷屏。
func truncateStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// TestDeleteProfile 覆盖画像删除（含参数缺失与不存在）。
func TestDeleteProfile(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	env.loginOK()
	ctx := context.Background()

	if err := env.db.Memory().UpsertProfile(ctx, &store.PersonProfile{
		Platform: "qq", UserID: "u1", SessionID: "s1",
		DisplayName: "小明", ProfileText: "喜欢摄影",
		MessageCount: 3, LastMessageAt: time.Now().Unix(), UpdatedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("写入画像失败: %v", err)
	}

	// 缺 user_id。
	resp, _ := env.do(http.MethodDelete, "/api/profiles/delete", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("缺 user_id 时状态码 = %d，期望 400", resp.StatusCode)
	}

	// 正常删除。
	resp, body := env.do(http.MethodDelete, "/api/profiles/delete?user_id=u1&session_id=s1", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, body)
	}

	// 再删一次应当是 404。
	resp, body = env.do(http.MethodDelete, "/api/profiles/delete?user_id=u1&session_id=s1", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("重复删除状态码 = %d（期望 404），body=%s", resp.StatusCode, body)
	}
}

// TestDeleteMessagesAll 覆盖 all=1 清空全库。
func TestDeleteMessagesAll(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	env.loginOK()
	ctx := context.Background()

	seedSession(t, env.db, "s1")
	seedMessage(t, env.db, "s1", "m1", "a", time.Now())
	seedMessage(t, env.db, "s1", "m2", "b", time.Now())

	resp, body := env.do(http.MethodDelete, "/api/messages/delete?all=1", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, body)
	}

	total, err := env.db.Messages().Count(ctx)
	if err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if total != 0 {
		t.Errorf("清空后仍有 %d 条", total)
	}
}

// TestDeleteEmojiRemovesFile 防：删了表情包记录但文件还在。
//
// 只删库不删文件的表现是「删了几百张，磁盘占用一点没变」，
// 用户完全无法理解这些文件是什么。
func TestDeleteEmojiRemovesFile(t *testing.T) {
	dir := t.TempDir()
	env := newTestEnv(t, envOptions{})
	env.loginOK()
	ctx := context.Background()

	// 真的建一个文件。
	filePath := filepath.Join(dir, "x.png")
	if err := os.WriteFile(filePath, []byte("fake image"), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}

	e := &store.Emoji{FileHash: "hh", FilePath: filePath, Description: "猫", CreatedAt: time.Now().Unix()}
	if _, err := env.db.Emojis().Save(ctx, e); err != nil {
		t.Fatalf("写入表情包失败: %v", err)
	}

	resp, body := env.do(http.MethodDelete, "/api/emojis/delete?id="+itoa(e.ID), "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, body)
	}
	if _, err := os.Stat(filePath); !os.IsNotExist(err) {
		t.Error("文件没被删掉")
	}
}

// TestPromptsGetMissing 防：读不存在的提示词返回 200。
func TestPromptsGetMissing(t *testing.T) {
	env := newTestEnv(t, envOptions{promptsDir: t.TempDir()})
	env.loginOK()

	resp, _ := env.do(http.MethodGet, "/api/prompts?name=不存在的", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("状态码 = %d，期望 404", resp.StatusCode)
	}
}

// TestPromptsResetMissingName 防：恢复默认时不传名字。
func TestPromptsResetMissingName(t *testing.T) {
	env := newTestEnv(t, envOptions{promptsDir: t.TempDir()})
	env.loginOK()

	resp, _ := env.do(http.MethodDelete, "/api/prompts", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("状态码 = %d，期望 400", resp.StatusCode)
	}
}

// TestPromptsWithoutManager 防：未提供 Prompts 时崩掉而不是报 503。
func TestPromptsWithoutManager(t *testing.T) {
	cfg := testConfig()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer db.Close()

	// 不传 Prompts。
	s := NewServer(Options{Config: config.Wrap(cfg), Store: db})
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/prompts", nil)
	req.AddCookie(&http.Cookie{Name: "omq_session", Value: s.auth.Issue()})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("状态码 = %d，期望 503", resp.StatusCode)
	}
}

// TestSessionRename 守会话重命名这条链路。
//
// 数据源 SessionRepo.SetDisplayName 早就存在，缺的只是一个写入入口 ——
// 于是界面上的会话名永远是从 target_id 拼出来的。
func TestSessionRename(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	seedData(t, env.db)
	env.loginOK()

	// 1. 正常改名。
	payload := `{"id":"` + groupSessionID + `","name":"家人群"}`
	resp, body := env.do(http.MethodPatch, "/api/sessions/rename", payload)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200，body=%s", resp.StatusCode, body)
	}

	sess, err := env.db.Sessions().Get(context.Background(), groupSessionID)
	if err != nil || sess == nil {
		t.Fatalf("读取会话失败: %v", err)
	}
	if sess.DisplayName != "家人群" {
		t.Errorf("DisplayName = %q，期望「家人群」", sess.DisplayName)
	}

	// 2. 空名字必须被拒：否则列表里出现一行空白。
	//    这一点在 HTTP 层拦就够，不必依赖 store 的 trim 后静默返回。
	resp, _ = env.do(http.MethodPatch, "/api/sessions/rename",
		`{"id":"`+groupSessionID+`","name":"   "}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("空名字状态码 = %d，期望 400", resp.StatusCode)
	}

	// 3. 不存在的会话 → 404（不是 500，也不是静默成功）。
	resp, _ = env.do(http.MethodPatch, "/api/sessions/rename",
		`{"id":"ghost-session","name":"x"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("不存在会话状态码 = %d，期望 404", resp.StatusCode)
	}

	// 4. GET 必须被拒：改名是写操作，不该能用链接触发。
	resp, _ = env.do(http.MethodGet, "/api/sessions/rename", "")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET 状态码 = %d，期望 405", resp.StatusCode)
	}
}

// TestSessionRenameRequiresAuth 守未登录不能改名。
func TestSessionRenameRequiresAuth(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	resp, _ := env.do(http.MethodPatch, "/api/sessions/rename", `{"id":"s1","name":"x"}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("未鉴权状态码 = %d，期望 401", resp.StatusCode)
	}
}
