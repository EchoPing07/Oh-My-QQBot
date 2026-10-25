package qq

import (
	"sync"
	"time"
)

// 被动回复窗口。
//
// QQ 官方限制：在收到一条入站消息后的一段时间内，可以用 msg_id + msg_seq
// 回复若干条消息，不消耗主动消息额度。超出窗口或次数后必须走主动消息。
//
// 限制（见插件 README）：
//   - 群聊：300 秒内最多 5 条
//   - 私聊：3600 秒内最多 4 条
const (
	groupReplyTTL     = 300 * time.Second
	groupReplyLimit   = 5
	privateReplyTTL   = 3600 * time.Second
	privateReplyLimit = 4
)

// passiveWindow 记录某个聊天目标的被动回复窗口。
type passiveWindow struct {
	// 按入站消息 ID 记录：收到时刻 + 已用序号。
	entries map[string]*passiveEntry
}

type passiveEntry struct {
	receivedAt time.Time
	used       int
}

// passiveTracker 管理所有聊天目标的被动回复窗口。
// 对应插件 _claim_passive_fields。
type passiveTracker struct {
	mu      sync.Mutex
	windows map[string]*passiveWindow // key: scene + ":" + target
}

func newPassiveTracker() *passiveTracker {
	return &passiveTracker{windows: make(map[string]*passiveWindow)}
}

func (t *passiveTracker) key(scene, target string) string {
	return scene + ":" + target
}

// RecordInbound 记录一条入站消息，开启/刷新被动回复窗口。
func (t *passiveTracker) RecordInbound(scene, target, messageID string, at time.Time) {
	if messageID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	k := t.key(scene, target)
	w, ok := t.windows[k]
	if !ok {
		w = &passiveWindow{entries: make(map[string]*passiveEntry)}
		t.windows[k] = w
	}
	w.entries[messageID] = &passiveEntry{receivedAt: at}

	// 顺手清掉过期条目，避免 map 无限增长。
	t.pruneLocked(w, scene, at)
}

// Claim 为一次发送占用一个被动回复序号。
//
// 返回的 msgID 与 seq 直接塞进请求体；返回空串表示没有可用的被动窗口，
// 调用方应改用主动消息发送。
//
// 参数 replyTo 是要回复的消息 ID；为空时取该目标最近的一条入站消息。
func (t *passiveTracker) Claim(scene, target, replyTo string) (msgID string, seq int, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	k := t.key(scene, target)
	w, exists := t.windows[k]
	if !exists {
		return "", 0, false
	}

	ttl, limit := groupReplyTTL, groupReplyLimit
	if scene != "group" {
		ttl, limit = privateReplyTTL, privateReplyLimit
	}

	var entry *passiveEntry
	chosenID := replyTo

	if replyTo != "" {
		if e, ok := w.entries[replyTo]; ok {
			entry = e
		}
	} else {
		// 取最近的一条。
		var latest time.Time
		for id, e := range w.entries {
			if e.receivedAt.After(latest) {
				latest = e.receivedAt
				chosenID = id
				entry = e
			}
		}
	}

	if entry == nil {
		return "", 0, false
	}

	// 窗口过期或次数用尽。
	if now.Sub(entry.receivedAt) >= ttl || entry.used >= limit {
		return "", 0, false
	}

	entry.used++
	// 发送后不再复用该序号，即便 HTTP 超时无法证明平台未接受。
	return chosenID, entry.used, true
}

// pruneLocked 清理过期条目。
func (t *passiveTracker) pruneLocked(w *passiveWindow, scene string, now time.Time) {
	ttl := groupReplyTTL
	if scene != "group" {
		ttl = privateReplyTTL
	}
	for id, e := range w.entries {
		if now.Sub(e.receivedAt) >= ttl {
			delete(w.entries, id)
		}
	}
}

// Cleanup 定期清理所有过期窗口。
func (t *passiveTracker) Cleanup() {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	for k, w := range t.windows {
		scene := "group"
		if len(k) > 6 && k[:6] == "private" {
			scene = "private"
		}
		t.pruneLocked(w, scene, now)
		if len(w.entries) == 0 {
			delete(t.windows, k)
		}
	}
}
