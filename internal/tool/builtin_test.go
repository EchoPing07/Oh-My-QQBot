package tool

// 本文件覆盖内置工具：reply / wait / stop。
// 只新建测试文件，不修改生产代码。

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"unsafe"
)

// TestReplyToolMissingReferenceFallsBackToReasoning
// 防的是「Planner 忘了填 reply_reference 就整轮失败」：
// 工具应该用 Planner 正文兜底，而不是直接拒绝。
func TestReplyToolMissingReferenceFallsBackToReasoning(t *testing.T) {
	rt := newFakeRuntime()
	tl := NewReplyTool(false)

	inv := invJSON("reply", `{"msg_id":"m1","reply_reference":"   "}`)
	inv.Reasoning = "  用户在问我吃了没，我想回一句刚吃完  "

	res, err := tl.Execute(context.Background(), inv, rt)
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if res.IsError {
		t.Fatalf("应走兜底而不是报错，实际内容 %q", res.Content)
	}
	if len(rt.replyCalls) != 1 {
		t.Fatalf("Reply 应被调用 1 次，实际 %d 次", len(rt.replyCalls))
	}
	if got, want := rt.replyCalls[0].Reference, "用户在问我吃了没，我想回一句刚吃完"; got != want {
		t.Errorf("Reference = %q，期望 %q（应取 Reasoning 并去空白）", got, want)
	}
}

// TestReplyToolEmptyReferenceAndReasoning 校验两者都为空时返回错误结果。
func TestReplyToolEmptyReferenceAndReasoning(t *testing.T) {
	rt := newFakeRuntime()
	tl := NewReplyTool(false)

	res, err := tl.Execute(context.Background(), invJSON("reply", `{"msg_id":"m1"}`), rt)
	if err != nil {
		t.Fatalf("工具错误应体现在结果里而不是 error: %v", err)
	}
	if !res.IsError {
		t.Fatal("reply_reference 与 Reasoning 都为空时应返回错误结果")
	}
	if !strings.Contains(res.Content, "reply_reference") {
		t.Errorf("错误内容 = %q，期望提到 reply_reference", res.Content)
	}
	if len(rt.replyCalls) != 0 {
		t.Error("校验失败时不应调用 Reply")
	}
}

// TestReplyToolPassesAttachments 是本文件的重点：
// attach_at / attach_pic / reply_style 必须原样传进 ReplyRequest，
// 传丢了就是 AGENTS.md 里那条「@ 和图片发不出去」的回归。
func TestReplyToolPassesAttachments(t *testing.T) {
	rt := newFakeRuntime()
	rt.replyFn = func(ReplyRequest) (ReplyOutcome, error) {
		return ReplyOutcome{Text: "收到了", MessageIDs: []string{"m-out-1", "m-out-2"}}, nil
	}
	tl := NewReplyTool(true)

	args := `{
		"msg_id": " m1 ",
		"reply_reference": " 用户问我吃了没 ",
		"reply_style": " 简短表达 ",
		"attach_at": ["m1", "m2"],
		"attach_pic": [{"msg_id":"m1","index":0},{"msg_id":"m2","index":2}]
	}`

	res, err := tl.Execute(context.Background(), invJSON("reply", args), rt)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if res.IsError {
		t.Fatalf("不应失败，实际 %q", res.Content)
	}
	if len(rt.replyCalls) != 1 {
		t.Fatalf("Reply 应被调用 1 次，实际 %d 次", len(rt.replyCalls))
	}

	req := rt.replyCalls[0]
	if req.MsgID != "m1" {
		t.Errorf("MsgID = %q，期望 %q（应去空白）", req.MsgID, "m1")
	}
	if req.Reference != "用户问我吃了没" {
		t.Errorf("Reference = %q，期望 %q", req.Reference, "用户问我吃了没")
	}
	if req.Style != "简短表达" {
		t.Errorf("Style = %q，期望 %q", req.Style, "简短表达")
	}
	if len(req.AttachAt) != 2 || req.AttachAt[0] != "m1" || req.AttachAt[1] != "m2" {
		t.Errorf("AttachAt = %v，期望 [m1 m2]", req.AttachAt)
	}
	if len(req.AttachPics) != 2 {
		t.Fatalf("AttachPics 数量 = %d，期望 2", len(req.AttachPics))
	}
	if req.AttachPics[0] != (AttachPic{MsgID: "m1", Index: 0}) {
		t.Errorf("AttachPics[0] = %+v，期望 {m1 0}", req.AttachPics[0])
	}
	if req.AttachPics[1] != (AttachPic{MsgID: "m2", Index: 2}) {
		t.Errorf("AttachPics[1] = %+v，期望 {m2 2}", req.AttachPics[1])
	}

	// 发出去的消息 ID 要回给模型，方便它引用自己刚说的话。
	if !strings.Contains(res.Content, "m-out-1") || !strings.Contains(res.Content, "m-out-2") {
		t.Errorf("结果内容 = %q，期望带上消息编号", res.Content)
	}

	// 回复文本要记进引用计数器。
	if len(rt.rememberCalls) != 1 || rt.rememberCalls[0] != "收到了" {
		t.Errorf("RememberReply 调用 = %v，期望 [收到了]", rt.rememberCalls)
	}
}

// TestReplyToolNoMessageIDs 校验发送成功但没有消息 ID 时的文案。
func TestReplyToolNoMessageIDs(t *testing.T) {
	rt := newFakeRuntime()
	rt.replyFn = func(ReplyRequest) (ReplyOutcome, error) {
		return ReplyOutcome{Text: "嗯"}, nil
	}
	tl := NewReplyTool(false)

	res, err := tl.Execute(context.Background(), invJSON("reply", `{"msg_id":"m1","reply_reference":"随便回"}`), rt)
	if err != nil || res.IsError {
		t.Fatalf("不应失败: %v / %+v", err, res)
	}
	if res.Content != "已发送回复。" {
		t.Errorf("结果内容 = %q，期望 %q", res.Content, "已发送回复。")
	}
}

// TestReplyToolSendError 校验发送失败被转成错误结果（而不是 panic 或 error）。
func TestReplyToolSendError(t *testing.T) {
	rt := newFakeRuntime()
	rt.replyFn = func(ReplyRequest) (ReplyOutcome, error) { return ReplyOutcome{}, errFake }
	tl := NewReplyTool(false)

	res, err := tl.Execute(context.Background(), invJSON("reply", `{"msg_id":"m1","reply_reference":"回应"}`), rt)
	if err != nil {
		t.Fatalf("工具错误应体现在结果里: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Content, "发送失败") {
		t.Errorf("结果 = %+v，期望是发送失败的错误结果", res)
	}
	if len(rt.rememberCalls) != 0 {
		t.Error("发送失败时不应调用 RememberReply")
	}
}

// TestReplyToolBadArgs 校验参数不是合法 JSON 时报错结果。
func TestReplyToolBadArgs(t *testing.T) {
	tl := NewReplyTool(false)
	res, err := tl.Execute(context.Background(), invJSON("reply", `{`), newFakeRuntime())
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Content, "参数解析失败") {
		t.Errorf("结果 = %+v，期望参数解析失败", res)
	}
}

// TestReplyToolSchemaAttachAtGated 防的是「enable_reply_at=false 时仍然告诉模型可以 @」。
func TestReplyToolSchemaAttachAtGated(t *testing.T) {
	on := NewReplyTool(true)
	off := NewReplyTool(false)

	propsOf := func(t *testing.T, tl *ReplyTool) map[string]any {
		t.Helper()
		props, ok := tl.Schema()["properties"].(map[string]any)
		if !ok {
			t.Fatal("schema 缺少 properties")
		}
		return props
	}

	if _, ok := propsOf(t, on)["attach_at"]; !ok {
		t.Error("enable_reply_at=true 时 schema 应包含 attach_at")
	}
	if _, ok := propsOf(t, off)["attach_at"]; ok {
		t.Error("enable_reply_at=false 时 schema 不应包含 attach_at")
	}
	for _, key := range []string{"msg_id", "reply_reference", "reply_style", "attach_pic"} {
		if _, ok := propsOf(t, off)[key]; !ok {
			t.Errorf("schema 缺少 %s", key)
		}
	}

	required, ok := off.Schema()["required"].([]string)
	if !ok || len(required) != 2 {
		t.Fatalf("required = %v，期望 2 项", off.Schema()["required"])
	}
}

// TestWaitTool 覆盖 wait 的正常路径、负数归零与连续上限。
func TestWaitTool(t *testing.T) {
	cases := []struct {
		name         string
		seconds      int
		waitErr      error
		wantSeconds  float64
		wantContains string
	}{
		{
			name:         "正常等待",
			seconds:      5,
			wantSeconds:  5,
			wantContains: "最多等待 5 秒",
		},
		{
			name:         "负数归零",
			seconds:      -3,
			wantSeconds:  0,
			wantContains: "最多等待 0 秒",
		},
		{
			name:         "达到连续上限",
			seconds:      10,
			waitErr:      errFake,
			wantSeconds:  10,
			wantContains: "连续 wait 已达到上限",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rt := newFakeRuntime()
			if c.waitErr != nil {
				rt.waitFn = func(float64, string) error { return c.waitErr }
			}
			tl := NewWaitTool()

			inv := invJSON("wait", `{"seconds":`+strconv.Itoa(c.seconds)+`}`)
			inv.Reasoning = "还在等对方补充"
			res, err := tl.Execute(context.Background(), inv, rt)
			if err != nil {
				t.Fatalf("不应返回 error: %v", err)
			}
			if res.IsError {
				// 达到上限是提示文本而不是错误，模型据此进入休息。
				t.Errorf("达到上限应返回成功结果，实际 %+v", res)
			}
			if !strings.Contains(res.Content, c.wantContains) {
				t.Errorf("结果内容 = %q，期望包含 %q", res.Content, c.wantContains)
			}
			if len(rt.waitCalls) != 1 {
				t.Fatalf("Wait 应被调用 1 次，实际 %d 次", len(rt.waitCalls))
			}
			if rt.waitCalls[0].seconds != c.wantSeconds {
				t.Errorf("等待秒数 = %v，期望 %v", rt.waitCalls[0].seconds, c.wantSeconds)
			}
			if rt.waitCalls[0].reason != "还在等对方补充" {
				t.Errorf("等待原因 = %q，期望取 Reasoning", rt.waitCalls[0].reason)
			}
		})
	}
}

// TestWaitToolBadArgs 校验非法参数被拒绝。
func TestWaitToolBadArgs(t *testing.T) {
	tl := NewWaitTool()
	res, err := tl.Execute(context.Background(), invJSON("wait", `{"seconds":"五"}`), newFakeRuntime())
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Content, "参数解析失败") {
		t.Errorf("结果 = %+v，期望参数解析失败", res)
	}
}

// TestWaitToolHasNoCachedLimit 守的是「没有冗余字段」。
//
// 连续 wait 的上限由 runtime 每次读配置比对，WaitTool 里不该再存一份
// 副本 —— 那个字段从来没人读，却会让维护者以为改它就能调上限
// （AGENTS.md 第 56 条）。
func TestWaitToolHasNoCachedLimit(t *testing.T) {
	tl := NewWaitTool()
	if tl.Name() != "wait" {
		t.Errorf("名字 = %q", tl.Name())
	}
	// 结构体里只剩零字段：任何人想加回缓存字段都要先改这条断言。
	if size := unsafe.Sizeof(*tl); size != 0 {
		t.Errorf("WaitTool 大小 = %d，不该再持有字段（缓存上限会与配置脱钩）", size)
	}
}

// TestStopToolCallsRuntimeStop 防的是「stop 不真的结束本轮」，
// 那会让模型以为结束了但循环继续转。
func TestStopToolCallsRuntimeStop(t *testing.T) {
	rt := newFakeRuntime()
	res, err := NewStopTool().Execute(context.Background(), invJSON("stop", `{}`), rt)
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if res.IsError {
		t.Errorf("stop 应成功，实际 %+v", res)
	}
	if len(rt.stopCalls) != 1 {
		t.Fatalf("Stop 应被调用 1 次，实际 %d 次", len(rt.stopCalls))
	}
	if rt.stopCalls[0] != "tool_stop_after_execution" {
		t.Errorf("Stop 原因 = %q，期望 %q", rt.stopCalls[0], "tool_stop_after_execution")
	}
	if !strings.Contains(res.Content, "已结束本轮思考") {
		t.Errorf("结果内容 = %q，期望提示已结束", res.Content)
	}
}

// TestBuiltinToolNames 校验内置工具名稳定，提示词与注册表都依赖这些字符串。
func TestBuiltinToolNames(t *testing.T) {
	cases := []struct {
		tool Tool
		want string
	}{
		{NewReplyTool(false), "reply"},
		{NewWaitTool(), "wait"},
		{NewStopTool(), "stop"},
		{NewQueryMemoryTool(), "query_memory"},
		{NewSendEmojiTool(), "send_emoji"},
		{NewShowEmojiListTool(), "show_emoji_list"},
	}
	for _, c := range cases {
		if got := c.tool.Name(); got != c.want {
			t.Errorf("工具名 = %q，期望 %q", got, c.want)
		}
		if c.tool.Description() == "" {
			t.Errorf("工具 %s 的说明不应为空", c.want)
		}
		if c.tool.Schema()["type"] != "object" {
			t.Errorf("工具 %s 的 schema type 应为 object", c.want)
		}
		if !c.tool.Visible(Availability{}) {
			t.Errorf("工具 %s 应默认可见", c.want)
		}
	}
}
