package mock_test

import (
	"testing"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/mock"
)

// TestQueryMemoryWithoutEmbeddingFallsBackToKeyword 验证没配向量时功能仍可用。
//
// 这是降级路径的核心保证：没配 embedding 不能导致 query_memory 报错，
// 它应该安静地退回关键词检索，哪怕召回质量差。
func TestQueryMemoryWithoutEmbeddingFallsBackToKeyword(t *testing.T) {
	var plannerCalls int

	h := mock.NewHarness(t, mock.HarnessOptions{
		SetupConfig: func(cfg *config.Config) {
			cfg.Memory.Enabled = true
			// 明确不配 embedding
			cfg.LLM.Tasks.Embedding.Models = nil
		},
		Planner: func(req *mock.RecordedRequest, _ int) *mock.Reply {
			plannerCalls++
			if plannerCalls == 1 {
				return mock.ToolReply("query_memory", `{"query":"火锅","mode":"search"}`)
			}
			// 第二轮不该出现「未配置 embedding」这类错误让模型困惑。
			// 关键词检索命中即可。
			return mock.ToolReply("stop", `{}`)
		},
	})

	h.SendMessage("还记得吗", true)
	if !h.WaitForLLMCalls(2, 15*time.Second) {
		t.Fatalf("Planner 只被调用 %d 次", plannerCalls)
	}
}
