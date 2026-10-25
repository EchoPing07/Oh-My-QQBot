package app

import (
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/chat"
)

// watchBotIdentity 等待适配器拿到机器人自己的 openid，然后加进过滤器名单。
//
// 为什么要等：QQ 网关的 READY 事件才带机器人 openid，
// 配置里可能没填，所以只能运行期补。
func (a *App) watchBotIdentity(filter *chat.Filter) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
			id := a.adapter.AccountID()
			if id == "" {
				continue
			}

			ids := []string{id}
			if a.cfg.Get().Bot.AccountID != "" && a.cfg.Get().Bot.AccountID != id {
				ids = append(ids, a.cfg.Get().Bot.AccountID)
			}
			if a.cfg.Get().QQ.UnifiedAccountID != "" {
				ids = append(ids, a.cfg.Get().QQ.UnifiedAccountID)
			}

			filter.SetBotIDs(ids)
			a.log.Info("机器人身份已加入过滤名单", "ids", ids)
			return
		}
	}
}
