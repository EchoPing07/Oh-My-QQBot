package mock_test

// 本文件是对 docs/audit/11-infra.md「mock（测试 harness）」小节的回归测试：
// P1-7（向量链路接通）、P2-22（自身消息过滤）、P2-23（磁盘模板覆盖）、
// P2-24（流式响应）、P2-25（WaitForSend 语义）、P2-26（图片格式转码/拒绝）。

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/gif"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/embedding"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/mock"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/store"
)

// TestEmbeddingPipelineWired 回归 [P1-7]：harness 必须把 Embedding 客户端
// 传给 Hub，假服务必须提供 /embeddings —— 以前这条链路在所有 harness
// 测试里都不存在，「配置了向量模型」的场景永远测不到。
//
// 判据是工具结果里的「（模式：vector）」而不是「命中了记忆」：
// 向量检索失败时会静默退化关键词（模式显示 keyword），
// 只有模式标记能证明真的走向量。
func TestEmbeddingPipelineWired(t *testing.T) {
	var plannerCalls int
	// 文本与查询一致：假服务的向量按文本哈希生成（没有真实语义），
	// 同文本才有余弦 1.0，否则随机相似度过不了 min_score、退化关键词模式。
	const docText = "上次聊到火锅很好吃，约好周末再去那家店"

	h := mock.NewHarness(t, mock.HarnessOptions{
		SetupConfig: func(cfg *config.Config) {
			cfg.Memory.Enabled = true
			cfg.LLM.Tasks.Embedding.Models = []string{"mock-model"}
		},
		EmbeddingDim: 4,
		Planner: func(req *mock.RecordedRequest, _ int) *mock.Reply {
			plannerCalls++
			if plannerCalls == 1 {
				// 查询文本与文档向量用的文本一致，保证假向量余弦 1.0。
				return mock.ToolReply("query_memory", `{"query":"`+docText+`","mode":"search"}`)
			}
			return mock.ToolReply("stop", `{}`)
		},
		Replyer: func(_ *mock.RecordedRequest, _ int) *mock.Reply {
			return mock.TextReply("好")
		},
	})

	if !h.Embedding.Enabled() {
		t.Fatal("配置了 embedding 任务，但客户端未启用：说明 SetupConfig 的任务分配没生效")
	}

	// 先用假服务生成一条真实向量写进库 —— 检索侧的 embed_dim 过滤
	// 要求「写入维度 == 检索维度」，用同一个客户端生成就不会错位。
	ctx := context.Background()
	vecs, err := h.Embedding.Embed(ctx, []string{docText})
	if err != nil {
		t.Fatalf("调用假 embedding 服务失败: %v", err)
	}
	if len(vecs) != 1 || len(vecs[0]) != 4 {
		t.Fatalf("期望 1 条 4 维向量，实际 %d 条、首条 %d 维", len(vecs), len(vecs[0]))
	}

	if err := h.DB.Memory().SaveSummary(ctx, &store.MemorySummary{
		SessionID:  "",
		Summary:    docText,
		RecallCues: []string{"讨论火锅"},
		TimeRange:  "2026/10/20 20:00",
		CreatedAt:  time.Now().Unix(),
		EmbedModel: "mock-model",
		EmbedDim:   4,
		Embedding:  embedding.Pack(vecs[0]),
	}); err != nil {
		t.Fatalf("写入带向量的记忆失败: %v", err)
	}

	h.SendMessage("还记得上次吃的火锅吗", true)
	if !h.WaitForLLMCalls(2, 15*time.Second) {
		t.Fatalf("Planner 只被调用了 %d 次，期望至少 2（query_memory 后应再思考一轮）", plannerCalls)
	}

	// 第二轮请求里应有工具结果，且模式必须是 vector（关键词退化会显示 keyword）。
	var sawVectorMode bool
	for _, req := range h.PlannerRequests() {
		if strings.Contains(req.FullText(), "（模式：vector）") {
			sawVectorMode = true
		}
		if strings.Contains(req.FullText(), "（模式：keyword）") {
			t.Error("检索退化成了关键词模式：向量链路没有真正生效")
		}
	}
	if !sawVectorMode {
		t.Error("工具结果里没有「（模式：vector）」：向量检索路径没走到")
	}
}

// TestHarnessFiltersBotOwnMessage 回归 [P2-22]：harness 必须设置 SetBotIDs，
// 否则「机器人自己的消息被过滤」这条路径在 harness 里永远走不到。
func TestHarnessFiltersBotOwnMessage(t *testing.T) {
	h := mock.NewHarness(t, mock.HarnessOptions{
		Planner: func(_ *mock.RecordedRequest, _ int) *mock.Reply {
			return mock.ToolReply("stop", `{}`)
		},
	})

	// SendMessageIn 用的账号 ID 就是 bot-openid；以它为发送者投一条消息。
	h.SendMessageIn("group-1", "bot-openid", "木木", "我自己说的话", false)

	if h.WaitForLLMCalls(1, 2*time.Second) {
		t.Error("机器人自己的消息进入了 Planner：SetBotIDs 没接到 harness 里")
	}
}

// TestPromptsDirOverridesEmbedded 回归 [P2-23]：磁盘模板必须能覆盖内嵌版本。
// 以前 harness 写死 prompt.New("")，这条路径零覆盖。
func TestPromptsDirOverridesEmbedded(t *testing.T) {
	dir := t.TempDir()
	localeDir := filepath.Join(dir, "zh-CN")
	if err := os.MkdirAll(localeDir, 0o755); err != nil {
		t.Fatalf("建模板目录失败: %v", err)
	}
	const marker = "磁盘模板已生效-标记词"
	if err := os.WriteFile(filepath.Join(localeDir, "planner.prompt"),
		[]byte(marker+" 你是木木。"), 0o644); err != nil {
		t.Fatalf("写模板失败: %v", err)
	}

	h := mock.NewHarness(t, mock.HarnessOptions{
		PromptsDir: dir,
		Planner: func(req *mock.RecordedRequest, _ int) *mock.Reply {
			if !strings.Contains(req.SystemText(), marker) {
				t.Errorf("Planner 的 system 提示词没有用磁盘模板（缺少 %q）", marker)
			}
			return mock.ToolReply("stop", `{}`)
		},
		Replyer: func(_ *mock.RecordedRequest, _ int) *mock.Reply {
			return mock.TextReply("好")
		},
	})

	h.SendMessage("你好", true)
	h.WaitForLLMCalls(1, 10*time.Second)
}

// TestForceStreamModelWorks 回归 [P2-24]：假 LLM 必须支持 SSE 流式。
// 以前 stream:true 的请求也回普通 JSON，force_stream 类缺陷在 harness
// 里永远「绿」—— 服务端恰好能解析非流式响应，看起来一切正常。
func TestForceStreamModelWorks(t *testing.T) {
	h := mock.NewHarness(t, mock.HarnessOptions{
		SetupConfig: func(cfg *config.Config) {
			cfg.LLM.Models[0].ForceStream = true
		},
		Planner: func(req *mock.RecordedRequest, _ int) *mock.Reply {
			return mock.ToolReply("reply", `{"msg_id":"`+extractMsgID(t, req)+`","reply_reference":"回应"}`)
		},
		Replyer: func(_ *mock.RecordedRequest, _ int) *mock.Reply {
			return mock.TextReply("流式回复拼回来了")
		},
	})

	h.SendMessage("你好", true)
	got := h.WaitForSend(10 * time.Second)
	if !strings.Contains(got, "流式回复拼回来了") {
		t.Errorf("流式回复没有被正确拼接，实际发出: %q", got)
	}
}

// TestWaitForSendReturnsLatest 回归 [P2-25]：WaitForSend 必须返回**最新**一条。
// 以前返回最早一条，多轮测试对着上一轮的旧内容断言也能通过（假绿）。
func TestWaitForSendReturnsLatest(t *testing.T) {
	h := mock.NewHarness(t, mock.HarnessOptions{
		Planner: func(req *mock.RecordedRequest, _ int) *mock.Reply {
			return mock.ToolReply("reply", `{"msg_id":"`+extractMsgID(t, req)+`","reply_reference":"回应"}`)
		},
		Replyer: func(_ *mock.RecordedRequest, call int) *mock.Reply {
			if call == 1 {
				return mock.TextReply("第一轮的内容")
			}
			return mock.TextReply("第二轮的内容")
		},
	})

	h.SendMessage("第一句", true)
	if got := h.WaitForSend(10 * time.Second); !strings.Contains(got, "第一轮的内容") {
		t.Fatalf("第一轮应收到第一轮内容，实际: %q", got)
	}

	h.SendMessage("第二句", true)
	// 第二轮受防抖延迟，先等满两条再断言 —— WaitForSend 只等「至少一条」，
	// 立刻调用会命中还没过防抖窗口的第一轮记录。
	texts := h.WaitForSendCount(2, 15*time.Second)
	if len(texts) != 2 {
		t.Fatalf("期望 2 条出站，实际 %d 条: %v", len(texts), texts)
	}
	// WaitForSend 必须返回最新一条，而不是最早一条。
	if got := h.WaitForSend(time.Second); !strings.Contains(got, "第二轮的内容") {
		t.Errorf("WaitForSend 应返回最新一条，实际: %q", got)
	}
	if !strings.Contains(texts[0], "第一轮的内容") || !strings.Contains(texts[1], "第二轮的内容") {
		t.Errorf("出站顺序不对: %v", texts)
	}
}

// TestImageFormatsTranscodeAndReject 回归 [P2-26]：harness 要能投递
// 任意格式的图片段，覆盖 imgtx 的转码（GIF→首帧 PNG）与拒绝（WEBP）路径。
func TestImageFormatsTranscodeAndReject(t *testing.T) {
	h := mock.NewHarness(t, mock.HarnessOptions{
		SetupConfig: func(cfg *config.Config) {
			cfg.LLM.Models[0].Visual = true
		},
		Planner: func(_ *mock.RecordedRequest, _ int) *mock.Reply {
			return mock.ToolReply("stop", `{}`)
		},
		Replyer: func(_ *mock.RecordedRequest, _ int) *mock.Reply {
			return mock.TextReply("看到了")
		},
	})

	// 真 GIF（image/gif 编码，能被解码）+ WEBP 假头（只有魔数，不可解码）。
	gifBytes := buildTestGIF(t)
	webpBytes := append([]byte("RIFF\x00\x00\x00\x00WEBP"), make([]byte, 16)...)

	h.SendMessageWithSegments("group-1", "user-1", "小明", "看这两张",
		domain.Segment{Type: domain.SegmentImage, Binary: gifBytes, MimeType: "image/gif"},
		domain.Segment{Type: domain.SegmentImage, Binary: webpBytes, MimeType: "image/webp"},
	)

	if !h.WaitForLLMCalls(1, 10*time.Second) {
		t.Fatal("没有触发模型调用")
	}

	reqs := h.PlannerRequests()
	if len(reqs) == 0 {
		t.Fatal("应有 Planner 请求")
	}
	var urls []string
	for _, m := range reqs[0].Messages {
		urls = append(urls, m.Images()...)
	}
	if len(urls) != 1 {
		t.Fatalf("期望恰好 1 张图（GIF 转码保留、WEBP 拒绝丢弃），实际 %d 张", len(urls))
	}
	if !strings.HasPrefix(urls[0], "data:image/png;") {
		t.Errorf("GIF 应被转成 PNG 首帧，实际: %.60s", urls[0])
	}
}

// buildTestGIF 用标准库编码一张 1x1 的真 GIF。
func buildTestGIF(t *testing.T) []byte {
	t.Helper()
	img := image.NewPaletted(image.Rect(0, 0, 1, 1), color.Palette{color.RGBA{R: 200, G: 100, B: 50, A: 255}})
	var buf bytes.Buffer
	if err := gif.Encode(&buf, img, nil); err != nil {
		t.Fatalf("构造测试 GIF 失败: %v", err)
	}
	return buf.Bytes()
}
