package web

// 本文件守「前端读的字段名与后端实际返回的形状一致」。
//
// 为什么需要它：这类错误**从来不报错**。
// 前后端任何一侧笔误，另一侧读到 undefined，界面上表现为
// 「某个数字永远是 0」「按钮点了永远显示失败」——
// 功能测试全绿（接口确实返回了正确数据），只有真的在浏览器里
// 点一下才看得见。
//
// 已踩过的（AGENTS.md 第 144、167 条）：
//   - overview 后端返回 counts.memory，前端读 counts.memories
//   - probe 后端返回裸对象，前端读 res.data（恒 undefined）
//
// 做法：把断言写成「前端源码里读的是 X」与「后端源码里写的是 Y」
// 的对照，两边都改才通过。这比跑一次浏览器便宜，也比没有早得多。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/llm"
)

// TestProbeResponseIsNotWrapped 守探活接口返回的是裸对象。
//
// 后端 handleModelProbe / handleProviderProbe 直接 writeJSON(res)，
// 没有 data 包裹。前端若读 res.data 就永远拿不到东西 ——
// 而兜底文案恰好是「失败：没有返回结果」，看起来像模型真的不通。
func TestProbeResponseIsNotWrapped(t *testing.T) {
	js := readStatic(t, "app.js")

	// 前端必须从顶层读 ok / latency_ms，不能过一层 data。
	for _, want := range []string{
		"showProbe(btn, res || { ok: false, error: '没有返回结果' })",
	} {
		if strings.Count(js, want) != 2 {
			t.Errorf("探活结果必须从顶层对象读取（两个探活入口各一次）：%s，实际出现 %d 次",
				want, strings.Count(js, want))
		}
	}
	if strings.Contains(js, "showProbe(btn, res.data") {
		t.Error("探活接口返回裸对象，前端不能读 res.data —— 那会恒为 undefined，" +
			"界面上永远显示「失败：没有返回结果」，与模型通不通无关")
	}
}

// TestProbeHandlerReturnsBareObject 从后端一侧守住同一件事。
//
// 只测前端源码不够：有人可能「修」成后端包一层 data，
// 那样前端就对了但所有其他调用方都要跟着改。
// 这条断言把「探活是裸对象」变成契约。
func TestProbeHandlerReturnsBareObject(t *testing.T) {
	cfg := config.NewStoreFrom(config.Default(), "")
	s := NewServer(Options{Config: cfg, LLM: llm.New(cfg)})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/config/llm/probe",
		strings.NewReader(`{"model":"__nope__"}`))
	s.handleModelProbe(rec, req)

	body := rec.Body.String()
	if !strings.HasPrefix(strings.TrimSpace(body), "{") {
		t.Fatalf("响应应是 JSON 对象: %s", body)
	}
	// 顶层必须有 ok，且不能包在 data 里。
	if !strings.Contains(body, `"ok":`) {
		t.Errorf("响应顶层必须有 ok 字段: %s", body)
	}
	if strings.Contains(body, `"data":{`) && strings.Contains(body, `"data":{"ok"`) {
		t.Errorf("探活响应不应包 data 层: %s", body)
	}
}

// TestRestartDelayReadFromTopLevel 守重启的 delay_ms 读取口径。
//
// 同样的问题：原来 `(res.data && res.data.delay_ms) || 1200`，
// res.data 恒 undefined 于是永远取兜底 1200 —— 恰好等于后端的真值，
// 所以这个 bug 看不出来，直到有人改 restartSpawnDelay。
func TestRestartDelayReadFromTopLevel(t *testing.T) {
	js := readStatic(t, "app.js")
	if !strings.Contains(js, "res && res.delay_ms") {
		t.Error("delay_ms 在响应顶层，不能读 res.data.delay_ms")
	}
	if strings.Contains(js, "res.data && res.data.delay_ms") {
		t.Error("restart 响应没有 data 包裹，读 res.data.delay_ms 永远是 undefined")
	}
}
