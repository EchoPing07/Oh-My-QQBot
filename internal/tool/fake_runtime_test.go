package tool

// 本文件提供测试用的假 Runtime。
// 只新建测试文件，不修改生产代码。

import (
	"errors"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/llm"
)

// fakeRuntime 实现 tool.Runtime 接口，所有能力都可按需注入。
//
// 刻意记录每次调用的参数，让工具测试能断言「传进去的是什么」，
// 而不是只断言「没报错」。
type fakeRuntime struct {
	sessionID string
	isGroup   bool
	botName   string

	// 可注入的行为。
	replyFn        func(ReplyRequest) (ReplyOutcome, error)
	waitFn         func(seconds float64, reason string) error
	searchFn       func(MemoryQuery) (MemoryResult, error)
	personFn       func(userID string) string
	pickEmojiFn    func(emotion string) (EmojiRef, bool)
	sendEmojiFn    func(ref EmojiRef) ([]string, error)
	conversationFn func() []llm.Message

	// 调用记录。
	replyCalls      []ReplyRequest
	waitCalls       []waitCall
	stopCalls       []string
	rememberCalls   []string
	rememberRefIdxs [][]string
	searchCalls     []MemoryQuery
	pickEmojiCalls  []string
	sendEmojiCalls  []EmojiRef
}

type waitCall struct {
	seconds float64
	reason  string
}

func newFakeRuntime() *fakeRuntime {
	return &fakeRuntime{botName: "木木"}
}

func (r *fakeRuntime) SessionID() string { return r.sessionID }
func (r *fakeRuntime) IsGroup() bool     { return r.isGroup }
func (r *fakeRuntime) BotName() string   { return r.botName }

func (r *fakeRuntime) Reply(req ReplyRequest) (ReplyOutcome, error) {
	r.replyCalls = append(r.replyCalls, req)
	if r.replyFn != nil {
		return r.replyFn(req)
	}
	return ReplyOutcome{Text: "默认回复", MessageIDs: []string{"m-out"}}, nil
}

func (r *fakeRuntime) Wait(seconds float64, reason string) error {
	r.waitCalls = append(r.waitCalls, waitCall{seconds: seconds, reason: reason})
	if r.waitFn != nil {
		return r.waitFn(seconds, reason)
	}
	return nil
}

func (r *fakeRuntime) Stop(reason string) { r.stopCalls = append(r.stopCalls, reason) }

func (r *fakeRuntime) Conversation() []llm.Message {
	if r.conversationFn != nil {
		return r.conversationFn()
	}
	return nil
}

func (r *fakeRuntime) RememberReply(text string, messageIDs, refIdxs []string) {
	r.rememberCalls = append(r.rememberCalls, text)
	r.rememberRefIdxs = append(r.rememberRefIdxs, refIdxs)
}

func (r *fakeRuntime) SearchMemory(req MemoryQuery) (MemoryResult, error) {
	r.searchCalls = append(r.searchCalls, req)
	if r.searchFn != nil {
		return r.searchFn(req)
	}
	return MemoryResult{}, nil
}

func (r *fakeRuntime) PersonProfile(userID string) string {
	if r.personFn != nil {
		return r.personFn(userID)
	}
	return ""
}

func (r *fakeRuntime) PickEmoji(emotion string) (EmojiRef, bool) {
	r.pickEmojiCalls = append(r.pickEmojiCalls, emotion)
	if r.pickEmojiFn != nil {
		return r.pickEmojiFn(emotion)
	}
	return EmojiRef{}, false
}

func (r *fakeRuntime) SendEmoji(ref EmojiRef) ([]string, error) {
	r.sendEmojiCalls = append(r.sendEmojiCalls, ref)
	if r.sendEmojiFn != nil {
		return r.sendEmojiFn(ref)
	}
	return []string{"m-emoji"}, nil
}

// errFake 是通用的假错误，避免每处都 errors.New。
var errFake = errors.New("假错误")

// invJSON 构造一个参数为给定 JSON 的调用。
func invJSON(name, args string) Invocation {
	return Invocation{Name: name, Arguments: []byte(args)}
}
