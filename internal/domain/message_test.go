package domain

// 本文件覆盖 domain 的消息模型：会话 ID、作用域、发送者判定与渲染。
// 只新建测试文件，不修改生产代码。

import (
	"strings"
	"testing"
	"time"
)

// TestSessionKeyIDStable 防的是「同样输入得到不同 session_id」，
// 那会导致每次重启都新建一条聊天流，上下文全部丢失。
func TestSessionKeyIDStable(t *testing.T) {
	key := SessionKey{
		Platform:  "qq",
		AccountID: "bot-1",
		Scope:     ScopeGroup,
		TargetID:  "group-1",
	}

	first := key.ID()
	for i := 0; i < 5; i++ {
		if got := key.ID(); got != first {
			t.Fatalf("第 %d 次计算结果 %q 与首次 %q 不一致", i+1, got, first)
		}
	}
	if len(first) != 32 {
		t.Errorf("session_id 长度 = %d，期望 32（16 字节 hex）", len(first))
	}
}

// TestSessionKeyIDUniquePerField 防的是「改了任何一段路由信息还撞到同一个 ID」，
// 那会把两个聊天流合并成一条，群聊和私聊互相串台。
func TestSessionKeyIDUniquePerField(t *testing.T) {
	base := SessionKey{Platform: "qq", AccountID: "bot-1", Scope: ScopeGroup, TargetID: "g1"}

	cases := []struct {
		name string
		key  SessionKey
	}{
		{"换平台", SessionKey{Platform: "discord", AccountID: "bot-1", Scope: ScopeGroup, TargetID: "g1"}},
		{"换账号", SessionKey{Platform: "qq", AccountID: "bot-2", Scope: ScopeGroup, TargetID: "g1"}},
		{"换作用域", SessionKey{Platform: "qq", AccountID: "bot-1", Scope: ScopePrivate, TargetID: "g1"}},
		{"换目标", SessionKey{Platform: "qq", AccountID: "bot-1", Scope: ScopeGroup, TargetID: "g2"}},
	}

	seen := map[string]string{base.ID(): "基准"}
	for _, c := range cases {
		id := c.key.ID()
		if id == base.ID() {
			t.Errorf("%s 与基准得到同一个 session_id %q", c.name, id)
		}
		if prev, dup := seen[id]; dup {
			t.Errorf("%s 与 %s 撞了 session_id %q", c.name, prev, id)
		}
		seen[id] = c.name
	}
}

// TestSessionKeyString 校验路由键的可读形式，日志排查要用。
func TestSessionKeyString(t *testing.T) {
	key := SessionKey{Platform: "qq", AccountID: "bot-1", Scope: ScopePrivate, TargetID: "u1"}
	if got, want := key.String(), "qq/bot-1/private/u1"; got != want {
		t.Errorf("String() = %q，期望 %q", got, want)
	}
}

// TestMessageScopeAndTargetID 校验群聊/私聊的判定与出站目标。
func TestMessageScopeAndTargetID(t *testing.T) {
	cases := []struct {
		name       string
		msg        Message
		wantScope  string
		wantTarget string
	}{
		{
			name:       "群聊取群号",
			msg:        Message{IsGroup: true, GroupID: "g1", UserID: "u1"},
			wantScope:  ScopeGroup,
			wantTarget: "g1",
		},
		{
			name:       "私聊取用户 ID",
			msg:        Message{IsGroup: false, GroupID: "g1", UserID: "u1"},
			wantScope:  ScopePrivate,
			wantTarget: "u1",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.msg.Scope(); got != c.wantScope {
				t.Errorf("Scope() = %q，期望 %q", got, c.wantScope)
			}
			if got := c.msg.TargetID(); got != c.wantTarget {
				t.Errorf("TargetID() = %q，期望 %q", got, c.wantTarget)
			}
		})
	}
}

// TestMessageIsSelf 防的是「机器人把自己的消息当成外部消息」，
// 那会自问自答。
func TestMessageIsSelf(t *testing.T) {
	botIDs := map[string]struct{}{"bot-1": {}}

	cases := []struct {
		name   string
		userID string
		want   bool
	}{
		{"机器人自己发的", "bot-1", true},
		{"别人发的", "u1", false},
		{"空 ID 不视为自己", "", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := &Message{UserID: c.userID}
			if got := msg.IsFromBot(botIDs); got != c.want {
				t.Errorf("IsSelf() = %v，期望 %v", got, c.want)
			}
		})
	}

	// 空集合不能把任何消息当成自己。
	if (&Message{UserID: "u1"}).IsFromBot(nil) {
		t.Error("botIDs 为空时不应判定为自己")
	}

	// IsSelf 标记优先于账号比对。
	//
	// 这条覆盖「重启恢复」的路径：从库里读回来的出站消息
	// 靠 IsSelf 字段识别，而不是靠账号 —— 账号可能换过，
	// 但这条记录确实是机器人发的。
	if !(&Message{UserID: "old-bot-id", IsSelf: true}).IsFromBot(map[string]struct{}{"new-bot-id": {}}) {
		t.Error("IsSelf 标记为 true 时应判定为自己，即使账号对不上")
	}
}

// TestPersonDisplayNameFallback 校验三级回退：群名片 → 昵称 → 用户 ID。
func TestPersonDisplayNameFallback(t *testing.T) {
	cases := []struct {
		name string
		p    Person
		want string
	}{
		{"优先群名片", Person{UserID: "u1", Nickname: "小明", CardName: "小明同学"}, "小明同学"},
		{"无名片用昵称", Person{UserID: "u1", Nickname: "小明"}, "小明"},
		{"都为空用 ID", Person{UserID: "u1"}, "u1"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.p.DisplayName(); got != c.want {
				t.Errorf("DisplayName() = %q，期望 %q", got, c.want)
			}
		})
	}
}

// TestChatMessageRenderText 覆盖工具调用编码与 HasToolCalls，
// 日志与调试依赖它。
func TestChatMessageRenderText(t *testing.T) {
	plain := ChatMessage{Role: RoleUser, Content: "你好"}
	if got, want := plain.RenderText(), "你好"; got != want {
		t.Errorf("RenderText() = %q，期望 %q", got, want)
	}
	if plain.HasToolCalls() {
		t.Error("没有工具调用时 HasToolCalls() 应为 false")
	}

	withCalls := ChatMessage{
		Role:    RoleAssistant,
		Content: "我想回复",
		ToolCalls: []ToolCall{
			{ID: "c1", Name: "reply", Args: `{"msg_id":"m1"}`},
			{ID: "c2", Name: "wait", Args: `{"seconds":3}`},
		},
		Timestamp: time.Unix(0, 0),
	}
	got := withCalls.RenderText()
	for _, want := range []string{"我想回复", "[tool_call reply {\"msg_id\":\"m1\"}]", "[tool_call wait {\"seconds\":3}]"} {
		if !strings.Contains(got, want) {
			t.Errorf("RenderText() = %q，缺少 %q", got, want)
		}
	}
	if !withCalls.HasToolCalls() {
		t.Error("有工具调用时 HasToolCalls() 应为 true")
	}
}

// TestReference 覆盖引用的构造与判断。
//
// Reference 是出站引用的目标类型。它存在的意义是把
// 「REFIDX_ 形态的引用索引」与「ROBOT1.0_ 形态的消息 ID」
// 在类型层面区分开 —— 两者都是长串不透明字符串，混用不会报错，
// 只会让引用链无法反查。
func TestReference(t *testing.T) {
	if !(Reference{}).IsZero() {
		t.Error("零值应为空引用")
	}
	if !Ref("   ").IsZero() {
		t.Error("全空白应视为空引用")
	}
	if Ref("REFIDX_x").IsZero() {
		t.Error("有值不应为空引用")
	}

	// Ref 会 trim：前后空白会让平台匹配不上，且看不出来。
	if got := Ref("  REFIDX_x  "); got.RefIdx != "REFIDX_x" {
		t.Errorf("Ref 应去除空白，实际 %q", got.RefIdx)
	}
}

// TestReferenceOf 覆盖从消息取引用索引。
//
// 只认 RefMsgIdx。**不能**退回 MessageID：那属于另一个键空间，
// 虽然平台也接受，但所有反查索引都按 REFIDX_ 建，混用会让引用关系断掉。
func TestReferenceOf(t *testing.T) {
	if got := ReferenceOf(nil); !got.IsZero() {
		t.Errorf("nil 消息应返回空引用，实际 %q", got.RefIdx)
	}

	// 只有 MessageID（d.id 形态）时不能当引用目标。
	onlyID := &Message{MessageID: "ROBOT1.0_abc"}
	if got := ReferenceOf(onlyID); !got.IsZero() {
		t.Errorf("只有 MessageID 时不该给出引用目标，实际 %q", got.RefIdx)
	}

	withIdx := &Message{MessageID: "ROBOT1.0_abc", RefMsgIdx: "REFIDX_abc"}
	if got := ReferenceOf(withIdx); got.RefIdx != "REFIDX_abc" {
		t.Errorf("应取 RefMsgIdx，实际 %q", got.RefIdx)
	}
}
