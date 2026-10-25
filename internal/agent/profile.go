package agent

import (
	"strings"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/tool"
)

// 人物画像注入。
//
// 对应参考实现 maisaka/memory/person_profile.py:
// build_person_profile_injection_messages。
//
// 参考实现的格式（逐字对齐，改之前先看它）：
//
//	【人物画像-内部参考】
//	以下内容仅供内部推理，不要向用户逐字复述。
//
//	<名字>：
//	  <画像正文>
//
//	使用时把它当作对当前人物的背景理解；若与当前对话冲突，以当前对话为准。
const (
	profileReferenceHeader = "【人物画像-内部参考】\n以下内容仅供内部推理，不要向用户逐字复述。"
	profileReferenceFooter = "使用时把它当作对当前人物的背景理解；若与当前对话冲突，以当前对话为准。"
)

// buildProfileInjection 拼出本轮要注入的人物画像块。
//
// 候选来源与参考实现一致：本轮批次里的发言者。参考实现还会把 @ 目标与引用目标
// 也算进去（_at_candidate / _reply_candidate），这里同样处理 ——
// 「他 @ 了谁」往往比「谁在说话」更能说明这次对话涉及几个人。
func (l *Loop) buildProfileInjection(batch []*domain.Message, rt tool.Runtime) string {
	if l.cfg == nil || !l.cfg.Get().Memory.Enabled || !l.cfg.Get().Memory.ProfileEnabled {
		return ""
	}
	if rt == nil || len(batch) == 0 {
		return ""
	}

	maxPersons := l.cfg.Get().Memory.ProfileMaxPersons
	if maxPersons <= 0 {
		maxPersons = 4
	}

	// 候选按「最近的优先」收集，与参考实现的 _messages_current_first 一致。
	var (
		order []string
		seen  = make(map[string]struct{})
		add   = func(uid string) {
			uid = strings.TrimSpace(uid)
			if uid == "" {
				return
			}
			if _, dup := seen[uid]; dup {
				return
			}
			seen[uid] = struct{}{}
			order = append(order, uid)
		}
	)

	for i := len(batch) - 1; i >= 0; i-- {
		m := batch[i]
		if m == nil {
			continue
		}
		add(m.UserID)
		// @ 目标也算候选。
		for _, seg := range m.Segments {
			if seg.Type == domain.SegmentAt {
				add(seg.UserID)
			}
		}
	}

	if len(order) == 0 {
		return ""
	}

	var blocks []string
	for _, uid := range order {
		if len(blocks) >= maxPersons {
			break
		}
		text := rt.PersonProfile(uid)
		if strings.TrimSpace(text) == "" {
			continue
		}
		name := displayNameFor(uid, batch)
		blocks = append(blocks, formatProfileBlock(name, text))
	}

	if len(blocks) == 0 {
		return ""
	}

	return profileReferenceHeader + "\n\n" +
		strings.Join(blocks, "\n\n") + "\n\n" +
		profileReferenceFooter
}

// formatProfileBlock 渲染单个人物的画像块。
//
// 缩进两格是照抄参考实现的 _format_profile_person_block，
// 作用是在视觉上把「名字」与「正文」分开，模型对齐关系时更稳。
func formatProfileBlock(name, text string) string {
	if strings.TrimSpace(name) == "" {
		name = "某位群友"
	}
	return name + "：\n  " + strings.Join(strings.Fields(text), " ")
}

// displayNameFor 在本轮消息里找某人的显示名，找不到就用 ID。
func displayNameFor(userID string, batch []*domain.Message) string {
	for i := len(batch) - 1; i >= 0; i-- {
		m := batch[i]
		if m == nil || m.UserID != userID {
			continue
		}
		if name := strings.TrimSpace(m.UserName); name != "" {
			return name
		}
	}
	return userID
}
