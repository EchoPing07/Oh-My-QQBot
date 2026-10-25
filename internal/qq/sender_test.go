package qq

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
)

// 出站引用测试。
//
// 这一整块以前**不存在**：sender.go 零测试文件，
// 所以「引用到底有没有写进请求体」从来没有被验证过。
// 实际状态是 splitOutbound 里 `case domain.SegmentReply: continue`
// 把引用段直接丢掉，请求体里从来没有 message_reference ——
// 而机器人还是会说「我引用了你那条」，因为没人检查。

// captureServer 记录所有发往 /messages 的请求体。
type captureServer struct {
	mu     sync.Mutex
	bodies []map[string]any
	// refIdx 是响应里要返回的 ext_info.ref_idx，按序取用。
	refIdx []string
}

func newSenderForTest(t *testing.T) (*Sender, *captureServer) {
	t.Helper()

	cap := &captureServer{}

	// 预签名分片上传的目标地址（QQ 真实场景是对象存储）。
	//
	// 必须用 TLS：sender 会拒绝非 https 的预签名地址
	// （那是防「平台被伪造时把内容 PUT 到明文地址」的校验，不该为了测试放开）。
	presignSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(presignSrv.Close)
	presignURL := presignSrv.URL + "/part"

	mux := http.NewServeMux()
	// 一个 handler 处理全部路径：Go 的 ServeMux 里同前缀重复注册会 panic。
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/messages"):
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}

			cap.mu.Lock()
			cap.bodies = append(cap.bodies, body)
			idx := len(cap.bodies)
			var refIdx string
			if idx-1 < len(cap.refIdx) {
				refIdx = cap.refIdx[idx-1]
			}
			cap.mu.Unlock()

			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"ROBOT1.0_sent_%d","timestamp":"2026-10-24T12:00:00+08:00","ext_info":{"ref_idx":%q}}`,
				idx, refIdx)

		// 图片分片上传的三个端点（最小实现，只为让「文本 + 图片」能拆条）。
		case strings.HasSuffix(r.URL.Path, "/upload_prepare"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"upload_id":"up1","block_size":"1024","parts":[{"index":1,"presigned_url":"`+presignURL+`"}]}`)
		case strings.HasSuffix(r.URL.Path, "/upload_part_finish"):
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{}`)
		case strings.HasSuffix(r.URL.Path, "/files"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"file_info":"FILEINFO"}`)

		default:
			http.NotFound(w, r)
		}
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client := NewClient(srv.URL, "app", "secret")
	// 直接注入 token，跳过鉴权请求。
	client.accessToken = "test-token"
	client.tokenExpires = time.Now().Add(time.Hour)
	// 预签名上传走的是自签证书的 TLS server，需要它的客户端。
	client.http = presignSrv.Client()

	return NewSender(client), cap
}

func (c *captureServer) snapshot() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]map[string]any(nil), c.bodies...)
}

// pngBytes 是一个最小的合法 PNG（1x1 透明），
// 只为让 detectImageExt 认出来并走富媒体分支。
var pngBytes = []byte{
	0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
	0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
	0x89, 0x00, 0x00, 0x00, 0x0A, 0x49, 0x44, 0x41,
	0x54, 0x78, 0x9C, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0D, 0x0A, 0x2D, 0xB4, 0x00,
	0x00, 0x00, 0x00, 0x49, 0x45, 0x4E, 0x44, 0xAE,
	0x42, 0x60, 0x82,
}

// TestSendWritesMessageReference 是出站引用的核心回归。
//
// 必须断言「请求体里真的有 message_reference.message_id」。
// 只断言发送成功是不够的：改造前发送一直成功，引用一次都没发出去。
func TestSendWritesMessageReference(t *testing.T) {
	sender, cap := newSenderForTest(t)

	_, err := sender.Send(context.Background(), "group", "group-1",
		[]domain.Segment{domain.Text("收到")}, "", domain.Ref("REFIDX_target"))
	if err != nil {
		t.Fatalf("发送失败: %v", err)
	}

	bodies := cap.snapshot()
	if len(bodies) != 1 {
		t.Fatalf("请求数 = %d，期望 1", len(bodies))
	}

	ref, ok := bodies[0]["message_reference"].(map[string]any)
	if !ok {
		t.Fatalf("请求体缺少 message_reference，实际 body = %v", bodies[0])
	}
	if got := ref["message_id"]; got != "REFIDX_target" {
		t.Errorf("message_reference.message_id = %v，期望 REFIDX_target", got)
	}
}

// TestSendWithoutQuoteOmitsMessageReference 守对照组：
// 未开启引用时请求体里不能出现 message_reference。
func TestSendWithoutQuoteOmitsMessageReference(t *testing.T) {
	sender, cap := newSenderForTest(t)

	if _, err := sender.Send(context.Background(), "group", "group-1",
		[]domain.Segment{domain.Text("收到")}, "", domain.Reference{}); err != nil {
		t.Fatalf("发送失败: %v", err)
	}

	for i, body := range cap.snapshot() {
		if _, ok := body["message_reference"]; ok {
			t.Errorf("第 %d 条请求不该带 message_reference: %v", i+1, body)
		}
	}
}

// TestSendQuoteOnlyOnFirstPart 防：正文被切成多条时每段都挂引用。
//
// QQ 客户端会把每条都渲染成一个引用块，一条回复出现三四个
// 指向同一消息的引用块，看起来像刷屏。
//
// 用「文本 + 图片 + 文本」构造真正的拆条：连续的文本段会被合并成一条，
// 只有遇到图片才会切断（一条消息只能承载一种内容）。
func TestSendQuoteOnlyOnFirstPart(t *testing.T) {
	sender, cap := newSenderForTest(t)

	_, err := sender.Send(context.Background(), "group", "group-1",
		[]domain.Segment{
			domain.Text("第一段"),
			{Type: domain.SegmentImage, Binary: pngBytes},
			domain.Text("第二段"),
		}, "", domain.Ref("REFIDX_target"))
	if err != nil {
		t.Fatalf("发送失败: %v", err)
	}

	bodies := cap.snapshot()
	if len(bodies) < 2 {
		t.Fatalf("请求数 = %d，期望至少 2（文本与图片各一条）", len(bodies))
	}

	var quoted int
	for i, body := range bodies {
		if _, ok := body["message_reference"]; ok {
			quoted++
			if i != 0 {
				t.Errorf("第 %d 条也带了引用，应只有首条挂引用", i+1)
			}
		}
	}
	if quoted != 1 {
		t.Errorf("带引用的请求数 = %d，期望 1", quoted)
	}
}

// TestSendSeparatesPassiveAndQuote 守两个参数不能混用。
//
// passiveTo 是「占谁的被动回复额度」，quote 是「引用卡片指向谁」。
// 指向不同的消息，键空间也不同（前者 ROBOT1.0_，后者 REFIDX_）。
// 混用的表现是：引用块指向了自己正在回复的那条消息，
// 而那本来就是被动回复默认的语义 —— 等于引用没起作用。
func TestSendSeparatesPassiveAndQuote(t *testing.T) {
	sender, cap := newSenderForTest(t)

	passive := "ROBOT1.0_inbound_event"
	sender.RecordInbound("group", "group-1", passive, time.Now())

	_, err := sender.Send(context.Background(), "group", "group-1",
		[]domain.Segment{domain.Text("收到")}, passive, domain.Ref("REFIDX_earlier"))
	if err != nil {
		t.Fatalf("发送失败: %v", err)
	}

	body := cap.snapshot()[0]

	// 被动回复的目标是入站消息 ID。
	if got := body["msg_id"]; got != passive {
		t.Errorf("msg_id = %v，期望 %v（被动回复目标）", got, passive)
	}
	if _, ok := body["msg_seq"]; !ok {
		t.Error("应带 msg_seq")
	}

	// 引用目标是另一条消息的引用索引。
	ref, ok := body["message_reference"].(map[string]any)
	if !ok {
		t.Fatalf("缺少 message_reference: %v", body)
	}
	if got := ref["message_id"]; got != "REFIDX_earlier" {
		t.Errorf("message_reference.message_id = %v，期望 REFIDX_earlier", got)
	}
	if ref["message_id"] == passive {
		t.Error("引用目标等于被动回复目标，两者混用了")
	}
}

// TestSendReturnsRefIdx 守「机器人能引用自己上一句话」的数据来源。
//
// 发送响应里的 ext_info.ref_idx 是机器人自己那条消息的引用索引，
// 入站事件里根本没有这个值。不解析它，「引用自己的发言」永远落空。
func TestSendReturnsRefIdx(t *testing.T) {
	sender, cap := newSenderForTest(t)
	cap.mu.Lock()
	cap.refIdx = []string{"REFIDX_self_1", "REFIDX_self_2"}
	cap.mu.Unlock()

	res, err := sender.Send(context.Background(), "group", "group-1",
		[]domain.Segment{domain.Text("收到")}, "", domain.Reference{})
	if err != nil {
		t.Fatalf("发送失败: %v", err)
	}

	if len(res.MessageIDs) != 1 || res.MessageIDs[0] == "" {
		t.Fatalf("MessageIDs = %v", res.MessageIDs)
	}
	if len(res.RefIdxs) != 1 {
		t.Fatalf("RefIdxs 长度 = %d，期望 1（与 MessageIDs 等长）", len(res.RefIdxs))
	}
	if res.RefIdxs[0] != "REFIDX_self_1" {
		t.Errorf("RefIdxs[0] = %q，期望 REFIDX_self_1", res.RefIdxs[0])
	}
}

// TestSendRefIdxsAlignWithMessageIDs 防：平台没返回 ref_idx 时按下标配对错位。
//
// 错位的表现是「引用自己那句话」引到了另一句，且完全无声。
func TestSendRefIdxsAlignWithMessageIDs(t *testing.T) {
	sender, cap := newSenderForTest(t)
	cap.mu.Lock()
	// 故意只给第一条响应带 ref_idx。
	cap.refIdx = []string{"REFIDX_only_first"}
	cap.mu.Unlock()

	res, err := sender.Send(context.Background(), "group", "group-1",
		[]domain.Segment{domain.Text("收到")}, "", domain.Reference{})
	if err != nil {
		t.Fatalf("发送失败: %v", err)
	}

	if len(res.RefIdxs) != len(res.MessageIDs) {
		t.Fatalf("RefIdxs=%v 与 MessageIDs=%v 长度不一致", res.RefIdxs, res.MessageIDs)
	}
}

// TestSendRefusesEmptySegments 守空消息不发送。
func TestSendRefusesEmptySegments(t *testing.T) {
	sender, _ := newSenderForTest(t)
	if _, err := sender.Send(context.Background(), "group", "g", nil, "", domain.Reference{}); err == nil {
		t.Fatal("空消息应报错")
	}
}
