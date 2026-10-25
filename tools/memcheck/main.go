// Command memcheck 验证长期记忆的端到端检索质量。
//
// 为什么需要这个工具（而不是只靠单元测试）：
//
//	单元测试用的是假向量，只能证明「代码路径走得通」，
//	证明不了「语义检索真的比关键词强」。这个工具打真端点，
//	把两种检索方式放在同一批数据上对比，用真实的相似度数值说话。
//
// 用法：
//
//	go run ./tools/memcheck
//	go run ./tools/memcheck -query "我们之前约好要做什么"
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/embedding"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/store"
)

// corpus 是一批典型的聊天回想。
// 刻意设计成「查询几乎不会与原文有字面重合」，
// 这才是真实使用场景，而不是搜自己刚输入过的词。
var corpus = []struct {
	summary string
	cues    []string
}{
	{
		summary: "上次聊到火锅很好吃，约好周末一起去吃",
		cues:    []string{"讨论周末聚餐安排", "提到想吃火锅"},
	},
	{
		summary: "抱怨推理模型太慢，回复经常是空的，最后换了 flash 模型",
		cues:    []string{"讨论模型响应延迟", "排查正文为空的原因"},
	},
	{
		summary: "讨论换哪个 planner 模型，比较了三家的工具调用稳定性，最后选了 glm",
		cues:    []string{"模型选型对比"},
	},
	{
		summary: "小明说他喜欢摄影，周末常去老城区拍街景，用的是一台旧胶片机",
		cues:    []string{"了解小明平时的兴趣爱好"},
	},
	{
		summary: "群里在聊新显卡的价格，有人觉得溢价太严重决定再等等",
		cues:    []string{"讨论硬件价格行情"},
	},
	{
		summary: "商量了项目分工，决定先做最小可用版本，长期记忆往后放",
		cues:    []string{"确定开发计划与优先级"},
	},
}

// queries 是检索请求，每条附上「期望命中的 corpus 下标」。
var queries = []struct {
	text string
	want int
}{
	{"我们之前说过要一起去吃好吃的", 0},
	{"模型响应太慢怎么办", 1},
	{"他有什么爱好", 3},
	{"那个硬件太贵了要不要买", 4},
	{"接下来先做什么功能", 5},
}

func main() {
	queryFlag := flag.String("query", "", "只跑这一条查询")
	flag.Parse()

	cfg, err := config.Load("config.toml")
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取配置失败: %v\n", err)
		os.Exit(1)
	}

	if len(cfg.ResolveModels(config.TaskEmbedding)) == 0 {
		fmt.Fprintln(os.Stderr, "未配置 embedding 模型，无法验证向量检索")
		fmt.Fprintln(os.Stderr, "请在 config.toml 的 [llm.tasks.embedding] 里填 models")
		os.Exit(1)
	}

	ctx := context.Background()
	emb := embedding.New(config.Wrap(cfg))

	// 用临时库，不污染真实数据。
	path := "data/memcheck.db"
	_ = os.Remove(path)
	db, err := store.Open(ctx, path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "打开数据库失败: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()
	defer os.Remove(path)

	fmt.Println("\n配置")
	models := cfg.ResolveModels(config.TaskEmbedding)
	m, _ := cfg.Model(models[0])
	fmt.Printf("  embedding 模型: %s (%s)\n", m.ModelIdentifier, m.Provider)

	// 播种并向量化。
	fmt.Printf("\n播种 %d 条记忆并向量化\n", len(corpus))
	start := time.Now()
	for _, item := range corpus {
		text := item.summary + "\n" + strings.Join(item.cues, "\n")
		vecs, err := emb.Embed(ctx, []string{text})
		if err != nil {
			fmt.Fprintf(os.Stderr, "向量化失败: %v\n", err)
			os.Exit(1)
		}

		s := &store.MemorySummary{
			SessionID:  "",
			Summary:    item.summary,
			RecallCues: item.cues,
			CreatedAt:  time.Now().Unix(),
			Embedding:  embedding.Pack(vecs[0]),
			EmbedModel: m.ModelIdentifier,
			EmbedDim:   len(vecs[0]),
		}
		if err := db.Memory().SaveSummary(ctx, s); err != nil {
			fmt.Fprintf(os.Stderr, "写入失败: %v\n", err)
			os.Exit(1)
		}
	}
	fmt.Printf("  完成，耗时 %v，维度 %d\n", time.Since(start).Round(time.Millisecond), emb.Dimension())

	// 跑对比。
	runQueries := queries
	if *queryFlag != "" {
		runQueries = []struct {
			text string
			want int
		}{{*queryFlag, -1}}
	}

	fmt.Println("\n检索对比")
	var vecOK, kwOK, total int

	for _, q := range runQueries {
		total++
		fmt.Printf("\n  查询：%s\n", q.text)

		// 关键词检索（FTS5）。
		kwHits, err := db.Memory().SearchSummaries(ctx, "", q.text, 3)
		if err != nil {
			fmt.Printf("    关键词: 失败 %v\n", err)
		}
		kwHit := q.want >= 0 && len(kwHits) > 0 && kwHits[0].Summary == corpus[q.want].summary
		if q.want >= 0 && kwHit {
			kwOK++
		}
		fmt.Printf("    关键词  %s\n", hitLabel(len(kwHits), kwHit, q.want >= 0))

		// 向量检索。
		vecs, err := emb.Embed(ctx, []string{q.text})
		if err != nil {
			fmt.Printf("    向量: 失败 %v\n", err)
			continue
		}
		candidates, err := db.Memory().LoadSummariesForVectorSearch(ctx, "", m.ModelIdentifier, len(vecs[0]), 100)
		if err != nil {
			fmt.Printf("    向量: 载入失败 %v\n", err)
			continue
		}

		type scored struct {
			s     *store.MemorySummary
			score float64
		}
		var ranked []scored
		for _, c := range candidates {
			v := embedding.Unpack(c.Embedding)
			ranked = append(ranked, scored{s: c, score: embedding.Cosine(vecs[0], v)})
		}
		// 简单插入排序，只用前 3。
		for i := 1; i < len(ranked); i++ {
			for j := i; j > 0 && ranked[j].score > ranked[j-1].score; j-- {
				ranked[j], ranked[j-1] = ranked[j-1], ranked[j]
			}
		}
		if len(ranked) > 3 {
			ranked = ranked[:3]
		}

		vecHit := q.want >= 0 && len(ranked) > 0 && ranked[0].s.Summary == corpus[q.want].summary
		if q.want >= 0 && vecHit {
			vecOK++
		}
		fmt.Printf("    向量    %s\n", hitLabel(len(ranked), vecHit, q.want >= 0))
		for _, r := range ranked {
			mark := " "
			if q.want >= 0 && r.s.Summary == corpus[q.want].summary {
				mark = "✓"
			}
			fmt.Printf("      %s %.3f  %s\n", mark, r.score, trunc(r.s.Summary, 34))
		}
	}

	if *queryFlag == "" {
		fmt.Printf("\n命中率\n")
		fmt.Printf("  关键词: %d/%d\n", kwOK, total)
		fmt.Printf("  向量:   %d/%d\n", vecOK, total)
	}
	fmt.Println()
}

func hitLabel(n int, correct bool, hasExpectation bool) string {
	if !hasExpectation {
		return fmt.Sprintf("返回 %d 条", n)
	}
	if correct {
		return "✓ 命中正确答案"
	}
	if n == 0 {
		return "✗ 0 命中"
	}
	return "✗ 命中了别的"
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
