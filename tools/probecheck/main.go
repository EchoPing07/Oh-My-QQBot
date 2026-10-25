// probecheck 排查「模型测试失败但实际调用正常」这类问题。
//
// 它做的不是「再测一次」，而是把探活与真实调用两条路径**并排打印**：
//   - 探活路径：llm.Client.ProbeModel（WebUI「测试」按钮走的那条）
//   - 真实路径：llm.Client.Generate（Planner / Replyer 走的那条）
//
// 两条路径的参数合并逻辑、请求体、状态码都会打出来，
// 这样「测试红了、调用是绿的」到底差在哪一步，一眼能看出来。
//
// 用法：
//
//	go run ./tools/probecheck -config config.toml
//	go run ./tools/probecheck -config config.toml -model glm
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/llm"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/logx"
)

func main() {
	configPath := flag.String("config", "config.toml", "配置文件")
	modelName := flag.String("model", "", "只测这一个模型（默认测全部非向量模型）")
	timeout := flag.Duration("timeout", 20*time.Second, "探活超时")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取配置失败: %v\n", err)
		os.Exit(1)
	}
	if err := logx.Init(logx.Options{Level: slog.LevelWarn, Console: false}); err != nil {
		fmt.Fprintf(os.Stderr, "初始化日志失败: %v\n", err)
		os.Exit(1)
	}

	store := config.NewStoreFrom(cfg, *configPath)
	client := llm.New(store)

	for _, m := range cfg.LLM.Models {
		if *modelName != "" && m.Name != *modelName {
			continue
		}
		// 向量模型只跑探活，不跑 Generate（任务模型里没有它）。
		if m.Dimension != nil {
			runProbeOnly(client, &m, *timeout)
			continue
		}
		runOne(client, cfg, &m, *timeout)
	}
}

func runProbeOnly(client *llm.Client, m *config.ModelConfig, timeout time.Duration) {
	fmt.Printf("\n================ 模型 %s (%s) ================\n", m.Name, m.ModelIdentifier)
	dump("ProbeModel", client.ProbeModel(context.Background(), m.Name, timeout))
}

func runOne(client *llm.Client, cfg *config.Config, m *config.ModelConfig, timeout time.Duration) {
	fmt.Printf("\n================ 模型 %s (%s) ================\n", m.Name, m.ModelIdentifier)

	// ---- 路径 A：探活（WebUI 测试按钮） ----
	res := client.ProbeModel(context.Background(), m.Name, timeout)
	dump("ProbeModel", res)

	// ---- 路径 B：真实调用（Generate） ----
	// 用真实任务的请求形状：planner 带 tools，replyer 不带。
	for _, task := range []config.TaskName{config.TaskPlanner, config.TaskReplyer} {
		if !hasModel(cfg, task, m.Name) {
			continue
		}
		req := &llm.Request{
			Messages: []llm.Message{{Role: "user", Content: "回复「ok」两个字，不要别的内容。"}},
		}
		if task == config.TaskPlanner {
			req.Tools = []llm.ToolDefinition{{
				Type: "function",
				Function: llm.ToolFunctionDefine{
					Name:        "noop",
					Description: "什么都不做",
					Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
				},
			}}
		}
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		resp, err := client.Generate(ctx, task, req)
		cancel()
		info := map[string]any{
			"task":       string(task),
			"latency_ms": time.Since(start).Milliseconds(),
		}
		if err != nil {
			info["error"] = err.Error()
		} else {
			info["content"] = resp.Content
			info["reasoning_len"] = len([]rune(resp.Reasoning))
			info["total_tokens"] = resp.Usage.TotalTokens
			info["tool_calls"] = len(resp.ToolCalls)
		}
		dump("Generate", info)
	}
}

func hasModel(cfg *config.Config, task config.TaskName, model string) bool {
	for _, name := range cfg.ResolveModels(task) {
		if name == model {
			return true
		}
	}
	return false
}

func dump(label string, v any) {
	raw, _ := json.MarshalIndent(v, "  ", "  ")
	fmt.Printf("--- %s ---\n  %s\n", label, raw)
}
