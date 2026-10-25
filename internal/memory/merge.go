package memory

import (
	"strings"
)

// 画像合并。
//
// 为什么需要合并（而不是每次覆盖写）：
//
//	画像生成是一次模型调用，而模型输出天然有抖动。同一个人、
//	同一批消息连跑两次，可能一次写出「喜欢摄影」，一次写「常拍照」，
//	也可能第二次把第一次的某条正确信息漏掉。
//	如果直接覆盖，画像会来回跳，注入给 Planner 的信息不稳定 ——
//	表现是「机器人对这个人的了解忽深忽浅」，而且没有任何报错。
//
//	参考实现的做法（A_memorix/core/utils/person_profile_service.py:_merge_fact_claim_buckets）：
//	把事实账本里的条目放在分类结果之前合并，保证「已有稳定事实」不会被
//	某一次 top-k 抖动挤出画像。
//
// 这里做同一件事的简化版：按分节把「旧要点」与「新要点」合并去重，
// 旧条目优先（先出现），新增条目追加在后，每节有条数上限。
// 没有做：事实账本、时间衰减、冲突消解（同一件事写了两遍不同说法时取旧的）。

// mergeLimits 是各分节的要点条数上限。
//
// 为什么要限制：不限制的话，同一个人聊上几个月，某一节会积到几十条，
// 注入提示词时把上下文挤爆。上限按「注入价值」排：
// 稳定了解与近期互动最有用，给得多；关系设定通常只有一两条，给得少。
var mergeLimits = map[string]int{
	"身份设定":  5,
	"关系设定":  3,
	"稳定了解":  8,
	"相处偏好":  6,
	"近期互动":  6,
	"不确定信息": 3,
	"维护备注":  2,
}

// defaultMergeLimit 是未在表中列出的分节的默认上限。
const defaultMergeLimit = 5

// MergeProfile 把新生成的画像合并进已有画像。
//
// 规则：
//   - 按分节合并，旧条目在前、新条目在后
//   - 去重（中文标点归一化后比较，避免「喜欢摄影。」与「喜欢摄影」被当成两条）
//   - 「- 暂无」不是信息，永远不进结果
//   - 每节按 mergeLimits 截断
//
// existing 为空时等价于 normalize：直接返回 normalized 的分节结构。
func MergeProfile(existing, normalized string) string {
	oldSections := parseProfileSections(existing)
	newSections := parseProfileSections(normalized)

	var out strings.Builder
	out.WriteString("# 人物画像\n")

	for _, title := range profileSectionTitles {
		merged := mergeBullets(oldSections[title], newSections[title], limitFor(title))

		out.WriteString("## ")
		out.WriteString(title)
		out.WriteString("\n")
		if len(merged) == 0 {
			out.WriteString("- 暂无\n")
			continue
		}
		for _, line := range merged {
			out.WriteString("- ")
			out.WriteString(line)
			out.WriteString("\n")
		}
	}

	return strings.TrimRight(out.String(), "\n")
}

// parseProfileSections 把画像文本解析成「分节 → 要点列表」。
func parseProfileSections(text string) map[string][]string {
	out := make(map[string][]string)
	if strings.TrimSpace(text) == "" {
		return out
	}

	current := ""
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# ") && !strings.HasPrefix(trimmed, "## ") {
			continue
		}
		if strings.HasPrefix(trimmed, "## ") {
			title := strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "## ")), "*：: ")
			current = title
			continue
		}
		if current == "" {
			continue
		}
		// 只收要点行。
		if !strings.HasPrefix(trimmed, "-") && !strings.HasPrefix(trimmed, "*") {
			continue
		}
		bullet := cleanBullet(trimmed)
		if bullet == "" || bullet == "暂无" {
			continue
		}
		out[current] = append(out[current], bullet)
	}
	return out
}

// cleanBullet 去掉要点前缀与首尾装饰。
func cleanBullet(line string) string {
	s := strings.TrimSpace(line)
	s = strings.TrimPrefix(s, "-")
	s = strings.TrimPrefix(s, "*")
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "**")
	return strings.TrimSpace(s)
}

// mergeBullets 合并两组要点，去重并截断。
//
// 旧条目优先保留：这是「稳定性优先」的取舍 ——
// 已经确认过的信息不该因为某次模型输出变化而消失。
func mergeBullets(oldBullets, newBullets []string, limit int) []string {
	if limit <= 0 {
		limit = defaultMergeLimit
	}

	seen := make(map[string]struct{}, len(oldBullets)+len(newBullets))
	out := make([]string, 0, len(oldBullets)+len(newBullets))

	add := func(bullet string) {
		cleaned := cleanBullet(bullet)
		if cleaned == "" || cleaned == "暂无" {
			return
		}
		key := normalizeBulletKey(cleaned)
		if _, dup := seen[key]; dup {
			return
		}
		seen[key] = struct{}{}
		out = append(out, cleaned)
	}

	for _, b := range oldBullets {
		add(b)
	}
	for _, b := range newBullets {
		add(b)
	}

	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// normalizeBulletKey 归一化用于去重比较的键。
//
// 只做保守归一化：去空白、统一中英文标点、去掉句末句号。
// 不做同义词归并 —— 「喜欢摄影」与「常拍照」是两条不同的信息，
// 强行合并会丢掉细节，而判断它们是否等价本身就是模型该做的事。
func normalizeBulletKey(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r':
			continue
		case '，', ',':
			b.WriteRune(',')
		case '。', '.':
			b.WriteRune('.')
		case '：', ':':
			b.WriteRune(':')
		case '；', ';':
			b.WriteRune(';')
		case '！', '!':
			b.WriteRune('!')
		case '？', '?':
			b.WriteRune('?')
		case '“', '”', '"':
			b.WriteRune('"')
		case '‘', '’', '\'':
			b.WriteRune('\'')
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSuffix(strings.ToLower(b.String()), ".")
}

// limitFor 返回某分节的要点上限。
func limitFor(title string) int {
	if n, ok := mergeLimits[title]; ok {
		return n
	}
	return defaultMergeLimit
}

// ProfileStats 返回画像的简要统计，用于日志与 WebUI。
func ProfileStats(profileText string) (sections, bullets int) {
	parsed := parseProfileSections(profileText)
	for _, title := range profileSectionTitles {
		if n := len(parsed[title]); n > 0 {
			sections++
			bullets += n
		}
	}
	return sections, bullets
}
