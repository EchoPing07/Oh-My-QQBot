package app

import (
	"context"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/llm"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/store"
)

// usageRecorder 把 LLM 调用统计写入数据库。
//
// 对应 MaiBot 的 model_task_usage_service + llm_cache_stats，
// 但只保留最核心的「谁、用了多少 token、花了多少钱」。
type usageRecorder struct {
	repo *store.UsageRepo
	cfg  *config.Store
}

func newUsageRecorder(db *store.DB, cfg *config.Store) *usageRecorder {
	return &usageRecorder{repo: db.Usage(), cfg: cfg}
}

// handle 是 llm.OnComplete 的回调。
func (u *usageRecorder) handle(task string, resp *llm.Response, err error) {
	if err != nil || resp == nil {
		return
	}

	// 查模型价格算花费。查不到模型就记 0，不影响主流程。
	var cost float64
	if model, mErr := u.cfg.Get().Model(resp.Model); mErr == nil {
		cost = llm.CostCalculator(
			resp.Usage.PromptTokens,
			resp.Usage.CompletionTokens,
			resp.Usage.CachedTokens,
			model.PriceIn,
			model.PriceOut,
			0,
		)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_ = u.repo.Record(ctx, &store.UsageRecord{
		Task:             task,
		Model:            resp.Model,
		PromptTokens:     resp.Usage.PromptTokens,
		CompletionTokens: resp.Usage.CompletionTokens,
		TotalTokens:      resp.Usage.TotalTokens,
		Cost:             cost,
		DurationMS:       resp.Duration.Milliseconds(),
		CreatedAt:        time.Now(),
	})
}
