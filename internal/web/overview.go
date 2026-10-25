package web

import (
	"context"
	"net/http"
	"time"
)

// 本文件提供「首页一次拿全」的聚合接口。
//
// 为什么需要：前端首页要显示运行时状态、消息总量、活跃会话、
// 各功能的数据量、最近 7 天消息趋势 —— 分开请求是 8 次往返，
// 而且每一次都要单独处理 500 与超时，前端代码会膨胀一倍。
//
// 一处失败不让整体失败：每个区块失败时填零值并在 errors 里列出来，
// 首页不该因为「表情包表查不到」就整页白屏。

// overviewError 是某个区块的查询失败。
type overviewError struct {
	Section string `json:"section"`
	Message string `json:"message"`
}

// handleOverview 返回首页需要的全部数据。
func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	// 显式初始化成空切片：nil 序列化成 null，前端 forEach 直接抛错。
	// 这个坑在 byTask / items 上已经踩过两次了。
	errs := []overviewError{}
	note := func(section string, err error) {
		if err == nil {
			return
		}
		s.log.Warn("概览查询失败", "section", section, "err", err)
		errs = append(errs, overviewError{Section: section, Message: err.Error()})
	}

	// 基础计数。
	var msgCount, sessionCount, personCount int64
	countInto := func(section string, fn func() (int64, error), dst *int64) {
		n, err := fn()
		if err != nil {
			note(section, err)
			return
		}
		*dst = n
	}

	countInto("messages", func() (int64, error) { return s.store.Messages().Count(ctx) }, &msgCount)
	countInto("sessions", func() (int64, error) { return s.store.Sessions().Count(ctx) }, &sessionCount)
	countInto("persons", func() (int64, error) { return s.store.Persons().Count(ctx) }, &personCount)

	// 今日消息数。
	var todayCount int64
	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	countInto("today", func() (int64, error) { return s.store.Messages().CountSince(ctx, todayStart) }, &todayCount)

	// 各功能数据量。
	var memoryCount, profileCount, emojiCount, exprCount, jargonCount int64
	if s.cfg.Get().Memory.Enabled {
		countInto("memory", func() (int64, error) { return s.store.Memory().CountSummaries(ctx) }, &memoryCount)
		countInto("profiles", func() (int64, error) { return s.store.Memory().CountProfiles(ctx) }, &profileCount)
	}
	if s.cfg.Get().Emoji.Enabled {
		countInto("emojis", func() (int64, error) { return s.store.Emojis().Count(ctx) }, &emojiCount)
	}
	if s.cfg.Get().Learning.ExpressionEnabled {
		countInto("expressions", func() (int64, error) { return s.store.Memory().CountExpressions(ctx) }, &exprCount)
	}
	if s.cfg.Get().Learning.JargonEnabled {
		countInto("jargons", func() (int64, error) { return s.store.Memory().CountJargons(ctx) }, &jargonCount)
	}

	// 最近 7 天消息趋势。
	daily, err := s.store.Messages().Daily(ctx, 7)
	note("daily", err)

	var active int
	if s.hub != nil {
		active = s.hub.ActiveCount()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"bot": map[string]any{
			"nickname":   s.cfg.Get().Bot.Nickname,
			"platform":   s.cfg.Get().Bot.Platform,
			"account_id": s.cfg.Get().Bot.AccountID,
		},
		"runtime": map[string]any{
			"active_chats":   active,
			"uptime_seconds": int(time.Since(startedAt).Seconds()),
			"started_at":     startedAt.Unix(),
		},
		"counts": map[string]any{
			"messages":       msgCount,
			"sessions":       sessionCount,
			"persons":        personCount,
			"messages_today": todayCount,
			"memory":         memoryCount,
			"profiles":       profileCount,
			"emojis":         emojiCount,
			"expressions":    exprCount,
			"jargons":        jargonCount,
		},
		"features": map[string]any{
			"memory":     s.cfg.Get().Memory.Enabled,
			"emoji":      s.cfg.Get().Emoji.Enabled,
			"expression": s.cfg.Get().Learning.ExpressionEnabled,
			"jargon":     s.cfg.Get().Learning.JargonEnabled,
		},
		"daily":  emptyIfNil(daily),
		"errors": errs,
	})
}

// emptyIfNil 让 nil 切片序列化成 []，避免前端 forEach 直接抛错。
func emptyIfNil[T any](in []T) []T {
	if in == nil {
		return []T{}
	}
	return in
}
