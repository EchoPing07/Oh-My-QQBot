package app

import (
	"context"
	"time"
)

// 保留期清理。
//
// 两张只增不减的表需要定期收缩：
//   - reasoning_records：每轮 Planner 请求与响应，单条上限 64KB
//   - messages：聊天记录本体
//
// 不清理的后果不是「慢慢变慢」而是「某天突然写不进去」：
// 一个活跃群跑几个月，推理记录能到几 GB，而 SQLite 单文件
// 到几十 GB 之后 VACUUM 与索引重建都会变得不可行。
//
// 策略：启动时跑一次，之后每 6 小时跑一次。
// 不在这里做 VACUUM —— 它要独占整个库且耗时与文件大小成正比，
// 放后台定时任务里只会和正常收发抢锁。

const retentionSweepInterval = 6 * time.Hour

// startRetentionSweeper 启动保留期清理协程。
func (a *App) startRetentionSweeper() {
	days := a.cfg.Get().Database.RetentionDays
	if days <= 0 {
		a.log.Info("未开启数据保留期清理", "提示", "把 database.retention_days 设为正数可自动清理旧数据")
		return
	}

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()

		ticker := time.NewTicker(retentionSweepInterval)
		defer ticker.Stop()

		// 启动时先跑一次：可能是停了很久之后才重启的。
		a.sweepRetention(days)

		for {
			select {
			case <-a.ctx.Done():
				return
			case <-ticker.C:
				a.sweepRetention(days)
			}
		}
	}()
}

// sweepRetention 清理一次超出保留期的数据。
//
// days <= 0 直接返回：AddDate(0,0,-0) 得到的是「现在」，
// 那样会把今天之前的所有数据全删掉 —— 一个参数越界就清库，
// 必须在入口挡住，不能依赖调用方自觉。
func (a *App) sweepRetention(days int) {
	if days <= 0 {
		return
	}

	cutoff := time.Now().AddDate(0, 0, -days)

	ctx, cancel := context.WithTimeout(a.ctx, 2*time.Minute)
	defer cancel()

	if n, err := a.db.Reasoning().Cleanup(ctx, cutoff.Unix()); err != nil {
		a.log.Warn("清理推理记录失败", "err", err)
	} else if n > 0 {
		a.log.Info("已清理过期推理记录", "count", n, "before", cutoff.Format(time.DateOnly))
	}

	if n, err := a.db.Messages().DeleteBefore(ctx, cutoff); err != nil {
		a.log.Warn("清理历史消息失败", "err", err)
	} else if n > 0 {
		a.log.Info("已清理过期消息", "count", n, "before", cutoff.Format(time.DateOnly))
	}
}
