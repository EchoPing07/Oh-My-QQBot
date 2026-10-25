package domain

import (
	"strings"
	"unicode"
)

// 媒体占位前缀，用于判断一条消息是否只有媒体没有文字。
// 与 MaiBot reply_likelihood.py 的 MEDIA_PLACEHOLDER_PREFIXES 一致。
var mediaPlaceholderPrefixes = []string{
	"[表情包", "[图片", "[消息类型]", "[CQ:image", "[语音", "[文件]", "[卡片",
}

// BuildPlainText 把消息段渲染成纯文本。
// 规则与 MaiBot 的 SessionMessage 渲染一致：
//   - text 直接拼接
//   - at 渲染为 @名字
//   - image/emoji/voice 渲染为占位文本
func BuildPlainText(segments []Segment) string {
	var parts []string
	for _, seg := range segments {
		switch seg.Type {
		case SegmentText:
			if s := strings.TrimSpace(seg.Text); s != "" {
				parts = append(parts, s)
			}
		case SegmentAt:
			name := seg.UserName
			if name == "" {
				name = seg.UserID
			}
			parts = append(parts, "@"+name)
		case SegmentImage:
			parts = append(parts, "[图片]")
		case SegmentEmoji:
			parts = append(parts, "[表情包]")
		case SegmentVoice:
			// 平台给了语音转文字就用它，比一个 [语音] 占位信息量大得多。
			if t := strings.TrimSpace(seg.Text); t != "" {
				parts = append(parts, t)
			} else {
				parts = append(parts, "[语音]")
			}
		case SegmentFile:
			if t := strings.TrimSpace(seg.Text); t != "" {
				parts = append(parts, t)
			} else {
				parts = append(parts, "[文件]")
			}
		case SegmentForward:
			parts = append(parts, "[合并转发]")
		case SegmentReply:
			// 引用不进入正文。
		}
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}

// IsMediaPlaceholderOnly 判断纯文本是否只有媒体占位。
//
// 只要整串能被占位前缀一个个吃完就算 true，例如 "[图片] [表情包]"。
// 注意不能只看第一个前缀就 return —— 后面的每一段都必须是占位。
func IsMediaPlaceholderOnly(text string) bool {
	rest := strings.TrimSpace(text)
	if rest == "" {
		return false
	}

	// 允许整体由多个占位拼接，每轮吃掉一个。
	for rest != "" {
		if !hasMediaPlaceholderPrefix(rest) {
			return false
		}
		// 占位必须闭合，否则是「[图片」这种残片。
		idx := strings.Index(rest, "]")
		if idx < 0 {
			return false
		}
		rest = strings.TrimSpace(rest[idx+1:])
	}
	return true
}

// hasMediaPlaceholderPrefix 判断 s 是否以某个媒体占位前缀开头。
func hasMediaPlaceholderPrefix(s string) bool {
	for _, p := range mediaPlaceholderPrefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// ContainsQuestion 判断文本是否带问号。
func ContainsQuestion(text string) bool {
	return strings.ContainsAny(text, "?？")
}

// ContainsAtOther 判断文本里是否出现了 @ 但不是在 @ 机器人。
func ContainsAtOther(text string, hasAtBot bool) bool {
	if hasAtBot {
		return false
	}
	return strings.Contains(text, "@")
}

// RenderAt 把 at 段渲染成可读文本。
func RenderAt(seg Segment) string {
	name := seg.UserName
	if name == "" {
		name = seg.UserID
	}
	return "@" + name
}

// NormalizeSpace 去掉首尾空白并合并连续空白。
func NormalizeSpace(s string) string {
	return strings.Join(strings.FieldsFunc(s, unicode.IsSpace), " ")
}
