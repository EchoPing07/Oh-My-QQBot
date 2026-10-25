package chat

import (
	"regexp"
	"strings"
	"sync"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
)

// 消息过滤。
// 对应 MaiBot 的 MessageReceiveConfig：ban_words 与 ban_msgs_regex。

// Filter 负责判断消息是否要丢弃。
type Filter struct {
	mu sync.RWMutex

	banWords   []string
	banRegexes []*regexp.Regexp
	// groupWhitelist 为空表示不限制。
	groupWhitelist map[string]struct{}
	// botIDs 是机器人自己的 ID，用于过滤自己发的消息。
	botIDs map[string]struct{}
}

// NewFilter 创建过滤器。
func NewFilter(banWords, banRegexes, groupWhitelist []string) (*Filter, error) {
	f := &Filter{
		groupWhitelist: make(map[string]struct{}),
		botIDs:         make(map[string]struct{}),
	}
	f.Update(banWords, banRegexes, groupWhitelist)
	return f, nil
}

// Update 更新过滤规则。
func (f *Filter) Update(banWords, banRegexes, groupWhitelist []string) {
	var compiled []*regexp.Regexp
	for _, pattern := range banRegexes {
		re, err := regexp.Compile(pattern)
		if err != nil {
			// 非法正则跳过并记录，不让一条坏配置炸掉整个启动。
			continue
		}
		compiled = append(compiled, re)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.banWords = append([]string(nil), banWords...)
	f.banRegexes = compiled
	f.groupWhitelist = make(map[string]struct{}, len(groupWhitelist))
	for _, g := range groupWhitelist {
		if s := strings.TrimSpace(g); s != "" {
			f.groupWhitelist[s] = struct{}{}
		}
	}
}

// SetBotIDs 设置机器人自己的 ID，用于过滤自身消息。
func (f *Filter) SetBotIDs(ids []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.botIDs = make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if s := strings.TrimSpace(id); s != "" {
			f.botIDs[s] = struct{}{}
		}
	}
}

// BanReason 返回消息被过滤的原因；空串表示放行。
func (f *Filter) BanReason(m *domain.Message) string {
	f.mu.RLock()
	defer f.mu.RUnlock()

	// 自己发的消息不处理，避免自问自答。
	//
	// 两条路都查：IsFromBot 先看 IsSelf 标记（从库里恢复的出站消息
	// 靠它识别），再按账号比对（网关推送的实时消息靠它识别）。
	// 只查一条都会漏：重启恢复的历史没有实时事件，而实时事件里
	// 机器人账号可能还没从 READY 回填。
	if m.IsFromBot(f.botIDs) {
		return "自身消息"
	}

	// 群白名单。
	if m.IsGroup && len(f.groupWhitelist) > 0 {
		if _, ok := f.groupWhitelist[m.GroupID]; !ok {
			return "群不在白名单"
		}
	}

	text := m.PlainText
	if text == "" {
		return ""
	}

	// 屏蔽词。
	for _, w := range f.banWords {
		if w == "" {
			continue
		}
		if strings.Contains(text, w) {
			return "命中屏蔽词: " + w
		}
	}

	// 屏蔽正则。
	for _, re := range f.banRegexes {
		if re.MatchString(text) {
			return "命中屏蔽正则: " + re.String()
		}
	}

	return ""
}
