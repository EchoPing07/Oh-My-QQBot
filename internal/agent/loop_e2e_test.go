package agent

// Loop 的端到端测试。
//
// Loop 是整条链路的骨架：请求模型 → 解析工具调用 → 执行工具 → 把结果写回。
// 它自己不碰网络也不碰数据库，所以可以用 httptest + 假 Runtime 完整跑通，
// 不需要动 QQ，也不需要真模型。
//
// 重点覆盖 CycleEnd：上层的空闲退避完全依赖它，
// 漏了或写错一个 Reason，退避机制就永远不生效，而且不会报错。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/llm"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/store"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/tool"
)

// fakePlannerStep 描述假 Planner 第 N 轮要返回什么。
type fakePlannerStep struct {
	content   string
	toolCalls []llm.ToolCall
	// status 非 0 时直接返回这个 HTTP 状态码，用于测失败路径。
	status int
}

// newFakePlannerServer 起一个假的 OpenAI 兼容服务，按轮次返回预设结果。
//
// 同时把每次请求体抓下来，便于断言「工具结果有没有被写回上下文」。
func newFakePlannerServer(t *testing.T, steps []fakePlannerStep) (*httptest.Server, func() []capturedRequest) {
	t.Helper()

	var (
		mu    sync.Mutex
		reqs  []capturedRequest
		index int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req capturedRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		mu.Lock()
		reqs = append(reqs, req)
		step := fakePlannerStep{}
		if index < len(steps) {
			step = steps[index]
		}
		index++
		mu.Unlock()

		if step.status != 0 {
			http.Error(w, `{"error":{"message":"mock failure"}}`, step.status)
			return
		}

		msg := map[string]any{"role": "assistant", "content": step.content}
		if len(step.toolCalls) > 0 {
			msg["tool_calls"] = step.toolCalls
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "mock",
			"choices": []map[string]any{
				{"message": msg, "finish_reason": finishOf(step)},
			},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		})
	}))
	t.Cleanup(srv.Close)

	return srv, func() []capturedRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]capturedRequest(nil), reqs...)
	}
}

func finishOf(step fakePlannerStep) string {
	if len(step.toolCalls) > 0 {
		return "tool_calls"
	}
	return "stop"
}

// replyCall 构造一个 reply 工具调用。
func replyCall(id, reference string) llm.ToolCall {
	args, _ := json.Marshal(map[string]string{
		"msg_id":          "m-1",
		"reply_reference": reference,
	})
	return llm.ToolCall{
		ID: id, Type: "function",
		Function: llm.ToolCallFunction{Name: "reply", Arguments: string(args)},
	}
}

// simpleCall 构造一个无参数工具调用（wait / stop / query_memory）。
func simpleCall(id, name, args string) llm.ToolCall {
	return llm.ToolCall{
		ID: id, Type: "function",
		Function: llm.ToolCallFunction{Name: name, Arguments: args},
	}
}

// loopFakeRuntime 记录工具对 Runtime 的副作用调用。
type loopFakeRuntime struct {
	mu         sync.Mutex
	replies    []tool.ReplyRequest
	waits      []float64
	stops      []string
	remembered []string
	replyErr   error
}

func (r *loopFakeRuntime) SessionID() string { return "sess-1" }
func (r *loopFakeRuntime) IsGroup() bool     { return true }
func (r *loopFakeRuntime) BotName() string   { return "木木" }

func (r *loopFakeRuntime) Reply(req tool.ReplyRequest) (tool.ReplyOutcome, error) {
	r.mu.Lock()
	r.replies = append(r.replies, req)
	r.mu.Unlock()
	if r.replyErr != nil {
		return tool.ReplyOutcome{}, r.replyErr
	}
	return tool.ReplyOutcome{Text: "已经说了", MessageIDs: []string{"out-1"}}, nil
}

func (r *loopFakeRuntime) Wait(seconds float64, _ string) error {
	r.mu.Lock()
	r.waits = append(r.waits, seconds)
	r.mu.Unlock()
	return nil
}

func (r *loopFakeRuntime) Stop(reason string) {
	r.mu.Lock()
	r.stops = append(r.stops, reason)
	r.mu.Unlock()
}

func (r *loopFakeRuntime) Conversation() []llm.Message { return nil }

func (r *loopFakeRuntime) RememberReply(text string, messageIDs, refIdxs []string) {
	r.mu.Lock()
	r.remembered = append(r.remembered, text)
	r.mu.Unlock()
}

func (r *loopFakeRuntime) SearchMemory(tool.MemoryQuery) (tool.MemoryResult, error) {
	return tool.MemoryResult{}, nil
}

func (r *loopFakeRuntime) PersonProfile(userID string) string {
	// 返回画像让 buildProfileInjection 也走一遍真实路径。
	if userID == "u-profiled" {
		return "喜欢摄影"
	}
	return ""
}

func (r *loopFakeRuntime) PickEmoji(string) (tool.EmojiRef, bool) { return tool.EmojiRef{}, false }

func (r *loopFakeRuntime) SendEmoji(tool.EmojiRef) ([]string, error) { return nil, nil }

// newLoopForTest 组装一个可跑完整轮的 Loop。
//
// db 为 nil 时不写推理记录；传库时顺带验证记录落库。
func newLoopForTest(t *testing.T, baseURL string, db *store.DB) (*Loop, *State) {
	t.Helper()

	cfg := newTestConfig()
	cfg.LLM.Providers = []config.ProviderConfig{{
		Name: "mock", BaseURL: baseURL, APIKey: "mock", Timeout: 5, MaxRetry: 1,
	}}
	cfg.LLM.Models = []config.ModelConfig{{
		Name: "mock", ModelIdentifier: "mock", Provider: "mock",
	}}
	cfg.LLM.Tasks.Planner.Models = []string{"mock"}

	reg := tool.NewRegistry()
	reg.MustRegister(tool.NewReplyTool(true))
	reg.MustRegister(tool.NewWaitTool())
	reg.MustRegister(tool.NewStopTool())
	reg.MustRegister(tool.NewQueryMemoryTool())

	pb := NewPromptBuilder(config.Wrap(cfg), newTestPromptManager(t))
	st := &State{
		registry: reg,
		avail:    testAvailability(),
		botName:  "木木",
	}
	return NewLoop(config.Wrap(cfg), llm.New(config.Wrap(cfg)), pb, db), st
}

// loopTurnInput 构造一轮输入。
func loopTurnInput() TurnInput {
	base := time.Date(2026, 10, 21, 13, 0, 0, 0, time.UTC)
	return TurnInput{
		SessionID: "sess-1",
		IsGroup:   true,
		GroupID:   "g1",
		Messages: []*domain.Message{{
			MessageID: "m-1", UserID: "u-profiled", UserName: "小明",
			PlainText: "在吗", ReceivedAt: base.Add(time.Minute),
		}},
		History: []*domain.Message{{
			MessageID: "m-0", UserID: "u-1", UserName: "小红",
			PlainText: "有人吗", ReceivedAt: base,
		}},
		Now: base.Add(2 * time.Minute),
	}
}

// TestRunTurnNoTool 验证 Planner 不调工具时结束为 CycleEndNoTool。
func TestRunTurnNoTool(t *testing.T) {
	srv, _ := newFakePlannerServer(t, []fakePlannerStep{{content: "先看看情况，暂不回复"}})
	l, st := newLoopForTest(t, srv.URL, nil)

	res, err := l.RunTurn(context.Background(), loopTurnInput(), &loopFakeRuntime{}, st)
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if res.End.Reason != CycleEndNoTool {
		t.Errorf("结束原因 = %q，期望 %q", res.End.Reason, CycleEndNoTool)
	}
	if res.Rounds != 1 {
		t.Errorf("轮次 = %d，期望 1", res.Rounds)
	}
	// Planner 正文要留下来作为 LastReasoning（但不能是原生 reasoning）。
	if res.LastReasoning != "先看看情况，暂不回复" {
		t.Errorf("LastReasoning = %q", res.LastReasoning)
	}
}

// TestRunTurnReply 验证 reply 工具成功后的结束原因与副作用。
func TestRunTurnReply(t *testing.T) {
	srv, captured := newFakePlannerServer(t, []fakePlannerStep{{
		content:   "该回一句",
		toolCalls: []llm.ToolCall{replyCall("call-1", "附和一下")},
	}})
	l, st := newLoopForTest(t, srv.URL, nil)
	rt := &loopFakeRuntime{}

	res, err := l.RunTurn(context.Background(), loopTurnInput(), rt, st)
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if res.End.Reason != CycleEndReplied {
		t.Errorf("结束原因 = %q，期望 %q", res.End.Reason, CycleEndReplied)
	}
	if len(rt.replies) != 1 {
		t.Fatalf("reply 工具应被调用一次，实际 %d 次", len(rt.replies))
	}
	// reply_reference 必须原样传到 Runtime（它不是回复正文）。
	if rt.replies[0].Reference != "附和一下" {
		t.Errorf("Reference = %q", rt.replies[0].Reference)
	}
	if rt.replies[0].MsgID != "m-1" {
		t.Errorf("MsgID = %q", rt.replies[0].MsgID)
	}
	// 回复成功后 Runtime 会 RememberReply，工具侧负责调用。
	if len(rt.remembered) != 1 || rt.remembered[0] != "已经说了" {
		t.Errorf("RememberReply 记录 = %v", rt.remembered)
	}
	_ = captured
}

// TestRunTurnStop 验证 stop 工具优先于继续循环。
func TestRunTurnStop(t *testing.T) {
	srv, _ := newFakePlannerServer(t, []fakePlannerStep{{
		content:   "没什么可说的",
		toolCalls: []llm.ToolCall{simpleCall("call-1", "stop", "{}")},
	}})
	l, st := newLoopForTest(t, srv.URL, nil)
	rt := &loopFakeRuntime{}

	res, err := l.RunTurn(context.Background(), loopTurnInput(), rt, st)
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if res.End.Reason != CycleEndStopTool {
		t.Errorf("结束原因 = %q，期望 %q", res.End.Reason, CycleEndStopTool)
	}
	if len(rt.stops) != 1 {
		t.Errorf("Stop 应被调用一次，实际 %d 次", len(rt.stops))
	}
	if !st.stopped {
		t.Errorf("State.stopped 应被置位")
	}
}

// TestRunTurnWait 验证 wait 工具会中断循环并累计连续次数。
func TestRunTurnWait(t *testing.T) {
	srv, _ := newFakePlannerServer(t, []fakePlannerStep{{
		content:   "再等等",
		toolCalls: []llm.ToolCall{simpleCall("call-1", "wait", `{"seconds":30}`)},
	}})
	l, st := newLoopForTest(t, srv.URL, nil)
	rt := &loopFakeRuntime{}

	res, err := l.RunTurn(context.Background(), loopTurnInput(), rt, st)
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if res.End.Reason != CycleEndWaitTool {
		t.Errorf("结束原因 = %q，期望 %q", res.End.Reason, CycleEndWaitTool)
	}
	if !st.waitRequested {
		t.Errorf("waitRequested 应被置位（否则 wait 退化成 stop）")
	}
	if st.ConsecutiveWaits() != 1 {
		t.Errorf("连续 wait = %d，期望 1", st.ConsecutiveWaits())
	}
	// 秒数只交给工具层，Runtime.Wait 由工具在 Execute 里调用。
	if len(rt.waits) != 1 || rt.waits[0] != 30 {
		t.Errorf("Runtime.Wait 调用 = %v，期望 [30]", rt.waits)
	}
}

// TestRunTurnMultiRound 验证多轮工具循环：第一轮查记忆，第二轮回复。
func TestRunTurnMultiRound(t *testing.T) {
	srv, captured := newFakePlannerServer(t, []fakePlannerStep{
		{
			content: "先查一下记忆",
			toolCalls: []llm.ToolCall{
				simpleCall("call-1", "query_memory", `{"query":"火锅","mode":"search"}`),
			},
		},
		{
			content:   "查到了，可以回",
			toolCalls: []llm.ToolCall{replyCall("call-2", "提起上次的火锅")},
		},
	})
	l, st := newLoopForTest(t, srv.URL, nil)

	res, err := l.RunTurn(context.Background(), loopTurnInput(), &loopFakeRuntime{}, st)
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if res.Rounds != 2 {
		t.Errorf("轮次 = %d，期望 2", res.Rounds)
	}
	if res.End.Reason != CycleEndReplied {
		t.Errorf("结束原因 = %q，期望 %q", res.End.Reason, CycleEndReplied)
	}

	// 第二轮请求里必须能看到「assistant 发起调用 → tool 返回结果」这一对消息，
	// 否则模型会重复调用同一个工具（这是最常见的死循环来源）。
	reqs := captured()
	if len(reqs) != 2 {
		t.Fatalf("应请求两次，实际 %d 次", len(reqs))
	}
	var sawAssistantCall, sawToolResult bool
	for _, m := range reqs[1].Messages {
		if m.Role == llm.RoleAssistant {
			sawAssistantCall = true
		}
		if m.Role == llm.RoleTool && m.ToolCallID == "call-1" {
			sawToolResult = true
		}
	}
	if !sawToolResult {
		t.Errorf("第二轮请求里缺少工具结果消息")
	}
	if !sawAssistantCall {
		t.Errorf("第二轮请求里缺少 assistant 的工具调用消息")
	}
}

// TestRunTurnMaxRounds 验证达到轮次上限时结束为 CycleEndMaxRounds。
//
// 上限存在是因为模型可能反复调用同一个无效工具。没有上限就是无限循环。
func TestRunTurnMaxRounds(t *testing.T) {
	steps := make([]fakePlannerStep, 0, 3)
	for i := 0; i < 3; i++ {
		steps = append(steps, fakePlannerStep{
			content:   "再查一次",
			toolCalls: []llm.ToolCall{simpleCall("call-"+itoa(i), "query_memory", `{"query":"x"}`)},
		})
	}
	srv, _ := newFakePlannerServer(t, steps)
	l, st := newLoopForTest(t, srv.URL, nil)
	st.maxRounds = 3

	res, err := l.RunTurn(context.Background(), loopTurnInput(), &loopFakeRuntime{}, st)
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if res.End.Reason != CycleEndMaxRounds {
		t.Errorf("结束原因 = %q，期望 %q", res.End.Reason, CycleEndMaxRounds)
	}
	if res.Rounds != 3 {
		t.Errorf("轮次 = %d，期望 3", res.Rounds)
	}
}

// TestRunTurnPlannerError 验证模型调用失败时的结束原因。
func TestRunTurnPlannerError(t *testing.T) {
	srv, _ := newFakePlannerServer(t, []fakePlannerStep{{status: http.StatusInternalServerError}})
	l, st := newLoopForTest(t, srv.URL, nil)

	res, err := l.RunTurn(context.Background(), loopTurnInput(), &loopFakeRuntime{}, st)
	if err == nil {
		t.Fatal("模型失败应报错")
	}
	if res.End.Reason != CycleEndError {
		t.Errorf("结束原因 = %q，期望 %q", res.End.Reason, CycleEndError)
	}
	if !strings.Contains(err.Error(), "Planner") {
		t.Errorf("错误信息应带上下文，实际 %v", err)
	}
}

// TestRunTurnInterrupted 验证 context 取消后的结束原因。
//
// 被打断必须区别于出错：出错会累计退避，被打断不该。
func TestRunTurnInterrupted(t *testing.T) {
	srv, _ := newFakePlannerServer(t, []fakePlannerStep{{content: "x"}})
	l, st := newLoopForTest(t, srv.URL, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := l.RunTurn(ctx, loopTurnInput(), &loopFakeRuntime{}, st)
	if err != nil {
		t.Fatalf("被打断不该报错: %v", err)
	}
	if res.End.Reason != CycleEndInterrupted {
		t.Errorf("结束原因 = %q，期望 %q", res.End.Reason, CycleEndInterrupted)
	}
	if !res.Interrupted {
		t.Errorf("Interrupted 应置位")
	}
}

// TestRunTurnWritesReasoningRecords 验证每轮请求与响应都落库。
//
// 失败的那次也要记：它恰恰是最需要回看的。
func TestRunTurnWritesReasoningRecords(t *testing.T) {
	db, _ := openAgentTestDB(t)
	srv, _ := newFakePlannerServer(t, []fakePlannerStep{
		{
			content:   "先查记忆",
			toolCalls: []llm.ToolCall{simpleCall("call-1", "query_memory", `{"query":"x"}`)},
		},
		{
			content:   "回一句",
			toolCalls: []llm.ToolCall{replyCall("call-2", "回应")},
		},
	})
	l, st := newLoopForTest(t, srv.URL, db)

	if _, err := l.RunTurn(context.Background(), loopTurnInput(), &loopFakeRuntime{}, st); err != nil {
		t.Fatalf("不该报错: %v", err)
	}

	records, err := db.Reasoning().ListBySession(context.Background(), "sess-1", 10)
	if err != nil {
		t.Fatalf("查询推理记录失败: %v", err)
	}

	// 时间线上要同时有「模型想了什么」和「工具实际做了什么」。
	//
	// 参考实现的账本里 planner.finalized 与 tool.execution 是两类事件
	// （maisaka-monitor-client.ts:211 / 439）。原来我们只记前者，
	// 界面上看不到工具的执行结果 —— 而这恰恰是最需要看的东西。
	byKind := map[string][]store.ReasoningRecord{}
	for _, rec := range records {
		byKind[rec.Kind] = append(byKind[rec.Kind], rec)
	}

	planners := byKind["planner"]
	if len(planners) != 2 {
		t.Fatalf("应有 2 条 planner 记录，实际 %d 条（kind 分布: %v）", len(planners), kindCounts(byKind))
	}
	tools := byKind["tool"]
	if len(tools) != 2 {
		t.Fatalf("应有 2 条 tool 记录（query_memory + reply），实际 %d 条（kind 分布: %v）",
			len(tools), kindCounts(byKind))
	}

	// 同一轮的多个循环共用一个 turn_id，WebUI 据此串成一次思考。
	for _, rec := range records {
		if rec.TurnID != planners[0].TurnID {
			t.Errorf("同一轮的 turn_id 应相同：%q vs %q", rec.TurnID, planners[0].TurnID)
		}
	}
	if planners[0].Round != 1 || planners[1].Round != 2 {
		t.Errorf("轮次记录不对：%d %d", planners[0].Round, planners[1].Round)
	}

	// 请求里要有完整的消息列表，响应里要有正文与工具调用。
	if !strings.Contains(planners[0].Request, `"messages"`) {
		t.Errorf("请求没被记录：%s", planners[0].Request)
	}
	if !strings.Contains(planners[0].Request, "query_memory") {
		t.Errorf("请求里应包含工具定义")
	}
	if !strings.Contains(planners[0].Response, "先查记忆") {
		t.Errorf("响应正文没被记录：%s", planners[0].Response)
	}
	if !strings.Contains(planners[0].Response, "tool_calls") {
		t.Errorf("工具调用没被记录：%s", planners[0].Response)
	}

	// 工具事件要带名字、参数与结果摘要，并挂到所属 Planner round 上。
	var sawQueryMemory, sawReply bool
	for _, rec := range tools {
		switch rec.Name {
		case "query_memory":
			sawQueryMemory = true
			if !strings.Contains(rec.Payload, `"query"`) {
				t.Errorf("工具参数没被记录：%s", rec.Payload)
			}
		case "reply":
			sawReply = true
		}
		if rec.Parent == 0 {
			t.Errorf("工具记录 %q 没有挂到所属 Planner round 上（Parent=0）", rec.Name)
		}
		if rec.Failed {
			t.Errorf("工具 %q 不该被标记为失败", rec.Name)
		}
	}
	if !sawQueryMemory {
		t.Error("没有 query_memory 的工具执行记录")
	}
	if !sawReply {
		t.Error("没有 reply 的工具执行记录")
	}
}

// kindCounts 把按 kind 分组的记录数压成一行，失败时报出来方便定位。
func kindCounts(byKind map[string][]store.ReasoningRecord) map[string]int {
	out := make(map[string]int, len(byKind))
	for k, v := range byKind {
		out[k] = len(v)
	}
	return out
}

// TestRunTurnFailureStillRecordsReasoning 验证失败也留下记录。
func TestRunTurnFailureStillRecordsReasoning(t *testing.T) {
	db, _ := openAgentTestDB(t)
	srv, _ := newFakePlannerServer(t, []fakePlannerStep{{status: http.StatusBadGateway}})
	l, st := newLoopForTest(t, srv.URL, db)

	if _, err := l.RunTurn(context.Background(), loopTurnInput(), &loopFakeRuntime{}, st); err == nil {
		t.Fatal("应报错")
	}

	records, err := db.Reasoning().ListBySession(context.Background(), "sess-1", 10)
	if err != nil {
		t.Fatalf("查询推理记录失败: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("失败也要记一笔，实际 %d 条", len(records))
	}
	if !strings.Contains(records[0].Response, "调用失败") {
		t.Errorf("失败原因应写进记录：%s", records[0].Response)
	}
}

// TestRunTurnIncludesProfileInjection 验证画像被拼进 Planner 请求。
//
// 只注入「本轮正在说话的人」的画像：群里 40 个人的背景会把上下文淹掉。
func TestRunTurnIncludesProfileInjection(t *testing.T) {
	srv, captured := newFakePlannerServer(t, []fakePlannerStep{{content: "x"}})
	l, st := newLoopForTest(t, srv.URL, nil)

	if _, err := l.RunTurn(context.Background(), loopTurnInput(), &loopFakeRuntime{}, st); err != nil {
		t.Fatalf("不该报错: %v", err)
	}

	reqs := captured()
	found := false
	for _, m := range reqs[0].Messages {
		if strings.Contains(m.text(), "【人物画像-内部参考】") && strings.Contains(m.text(), "喜欢摄影") {
			found = true
		}
	}
	if !found {
		t.Errorf("本轮说话人的画像应被注入")
	}
}

// TestRunTurnAppendsBatchMessages 验证本批触发消息被追加到请求里。
//
// 本批消息必须出现在请求中，否则模型看不到「刚说的话」。
func TestRunTurnAppendsBatchMessages(t *testing.T) {
	srv, captured := newFakePlannerServer(t, []fakePlannerStep{{content: "x"}})
	l, st := newLoopForTest(t, srv.URL, nil)

	in := loopTurnInput()
	if _, err := l.RunTurn(context.Background(), in, &loopFakeRuntime{}, st); err != nil {
		t.Fatalf("不该报错: %v", err)
	}

	reqs := captured()
	var lastUser string
	for _, m := range reqs[0].Messages {
		if m.Role == llm.RoleUser {
			lastUser = m.text()
		}
	}
	// 最后一条 user 应该是 reminder，所以往前找「在吗」。
	var sawBatch bool
	for _, m := range reqs[0].Messages {
		if strings.Contains(m.text(), "在吗") && strings.Contains(m.text(), `msg_id="m-1"`) {
			sawBatch = true
		}
	}
	if !sawBatch {
		t.Errorf("本批消息没被追加到请求里（last user=%q）", lastUser)
	}
}

// TestRunTurnHasToolsInRequest 验证工具定义真的发给模型了。
//
// 没有工具定义时模型只能输出纯文本，永远调不到 reply。
func TestRunTurnHasToolsInRequest(t *testing.T) {
	srv, captured := newFakePlannerServer(t, []fakePlannerStep{{content: "x"}})
	l, st := newLoopForTest(t, srv.URL, nil)

	if _, err := l.RunTurn(context.Background(), loopTurnInput(), &loopFakeRuntime{}, st); err != nil {
		t.Fatalf("不该报错: %v", err)
	}

	tools := captured()[0].Tools
	if len(tools) == 0 {
		t.Fatal("请求里没有工具定义，模型永远调不到 reply")
	}
	var names []string
	for _, toolDef := range tools {
		names = append(names, toolDef.Function.Name)
		if toolDef.Type != "function" {
			t.Errorf("工具类型 = %q，期望 function", toolDef.Type)
		}
	}
	joined := strings.Join(names, ",")
	// reply 是唯一的可见输出通道，它必须在。
	if !strings.Contains(joined, "reply") {
		t.Errorf("工具列表里缺少 reply：%v", names)
	}
}

// TestRunTurnUnknownToolWritesErrorBackToContext 验证工具不存在时把错误写回上下文。
//
// 不写回的话模型会一直重复调用同一个不存在的工具，直到轮次上限。
func TestRunTurnUnknownToolWritesErrorBackToContext(t *testing.T) {
	srv, captured := newFakePlannerServer(t, []fakePlannerStep{
		{
			content:   "试试这个工具",
			toolCalls: []llm.ToolCall{simpleCall("call-1", "不存在的工具", "{}")},
		},
		{content: "算了"},
	})
	l, st := newLoopForTest(t, srv.URL, nil)

	res, err := l.RunTurn(context.Background(), loopTurnInput(), &loopFakeRuntime{}, st)
	if err != nil {
		t.Fatalf("工具失败不该让整轮报错: %v", err)
	}
	// 第二轮没有工具调用，所以结束于 NoTool。
	if res.End.Reason != CycleEndNoTool {
		t.Errorf("结束原因 = %q，期望 %q", res.End.Reason, CycleEndNoTool)
	}

	reqs := captured()
	if len(reqs) != 2 {
		t.Fatalf("应请求两次，实际 %d", len(reqs))
	}
	var toolContent string
	for _, m := range reqs[1].Messages {
		if m.Role == llm.RoleTool {
			toolContent = m.text()
		}
	}
	if toolContent == "" {
		t.Fatal("工具错误没有写回上下文")
	}
	if !strings.Contains(toolContent, "工具执行失败") && !strings.Contains(toolContent, "不存在") {
		t.Errorf("写回的内容应说明失败原因，实际 %q", toolContent)
	}
}

// TestRunTurnSplitsImageMessages 验证带图消息单独成条并挂 Images。
//
// 这是 AGENTS.md 第 23 条的回归：本批新消息里的图如果只走 RenderMessages，
// 图片会被静默丢掉，表现为「历史里的图看得见，刚发的那张看不见」。
func TestRunTurnSplitsImageMessages(t *testing.T) {
	srv, captured := newFakePlannerServer(t, []fakePlannerStep{{content: "x"}})

	cfg := newTestConfig()
	cfg.LLM.Providers = []config.ProviderConfig{{Name: "mock", BaseURL: srv.URL, APIKey: "k", Timeout: 5, MaxRetry: 1}}
	cfg.LLM.Models = []config.ModelConfig{{
		Name: "mock", ModelIdentifier: "mock", Provider: "mock", Visual: true,
	}}
	cfg.LLM.Tasks.Planner.Models = []string{"mock"}

	reg := tool.NewRegistry()
	reg.MustRegister(tool.NewReplyTool(true))
	st := &State{registry: reg, avail: testAvailability(), botName: "木木"}
	l := NewLoop(config.Wrap(cfg), llm.New(config.Wrap(cfg)), NewPromptBuilder(config.Wrap(cfg), newTestPromptManager(t)), nil)

	base := time.Date(2026, 10, 21, 13, 0, 0, 0, time.UTC)
	in := TurnInput{
		SessionID: "sess-1",
		IsGroup:   true,
		Messages: []*domain.Message{
			{MessageID: "m-1", UserID: "u1", UserName: "小明", PlainText: "看这个", ReceivedAt: base},
			{MessageID: "m-2", UserID: "u2", UserName: "小红", ReceivedAt: base.Add(time.Second),
				Segments: []domain.Segment{{Type: domain.SegmentImage, Binary: testPNG(t)}}},
			{MessageID: "m-3", UserID: "u3", UserName: "小刚", PlainText: "嗯", ReceivedAt: base.Add(2 * time.Second)},
		},
		Now: base.Add(time.Minute),
	}

	if _, err := l.RunTurn(context.Background(), in, &loopFakeRuntime{}, st); err != nil {
		t.Fatalf("不该报错: %v", err)
	}

	// 抓原始请求体确认 image_url 存在。
	reqs := captured()
	if len(reqs) != 1 {
		t.Fatalf("应请求一次，实际 %d", len(reqs))
	}
	// capturedRequest 不解析 images（content 会是数组），这里换个方式：
	// 直接数 content 里能看到的文字，确认三段消息都在。
	var joined strings.Builder
	for _, m := range reqs[0].Messages {
		joined.WriteString(m.text())
	}
	for _, want := range []string{"看这个", "嗯"} {
		if !strings.Contains(joined.String(), want) {
			t.Errorf("消息 %q 丢失", want)
		}
	}
}
