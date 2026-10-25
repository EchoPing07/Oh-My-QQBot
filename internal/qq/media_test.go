package qq

// 入站附件处理的测试。
//
// 防的是「有人发图但机器人当成空消息丢掉」以及「SSRF 白名单形同虚设」。

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/store"
)

// openStoreForTest 开一个临时 SQLite，用完自动关。
func openStoreForTest(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestDecodeImageOnlyMessageKept 防：只有图片没有文字的消息被丢弃。
//
// 这是真实踩过的坑：codec 完全不看 attachments，
// 于是纯图片消息在「消息不包含文本或可处理的附件」这一关被扔掉，
// 日志里只留一句警告，看起来像 QQ 根本没推消息。
func TestDecodeImageOnlyMessageKept(t *testing.T) {
	payload := `{
		"id": "msg-img",
		"content": "",
		"group_openid": "group-abc",
		"author": {"member_openid": "user-1", "username": "小明"},
		"attachments": [{
			"content_type": "image/png",
			"url": "https://grouppro.grouppro.qq.com/a.png",
			"filename": "a.png",
			"size": "1234"
		}]
	}`

	got, err := Decode(GroupMessageEvent, json.RawMessage(payload), "bot-openid", "木木")
	if err != nil {
		t.Fatalf("纯图片消息不应解码失败: %v", err)
	}

	if len(got.Message.Segments) != 1 {
		t.Fatalf("段数 = %d，期望 1", len(got.Message.Segments))
	}
	seg := got.Message.Segments[0]
	if seg.Type != domain.SegmentImage {
		t.Errorf("段类型 = %q，期望 image", seg.Type)
	}
	if seg.URL != "https://grouppro.grouppro.qq.com/a.png" {
		t.Errorf("段 URL = %q，期望保留原始地址", seg.URL)
	}
	if got.Message.PlainText != "[图片]" {
		t.Errorf("正文 = %q，期望 [图片] 占位", got.Message.PlainText)
	}
}

// TestDecodeImageWithTextKeepsBoth 防：图文消息丢了文字或丢了图。
func TestDecodeImageWithTextKeepsBoth(t *testing.T) {
	payload := `{
		"id": "msg-img-text",
		"content": "看这个",
		"group_openid": "group-abc",
		"author": {"member_openid": "user-1", "username": "小明"},
		"attachments": [{
			"content_type": "image/jpeg",
			"url": "https://grouppro.grouppro.qq.com/b.jpg"
		}]
	}`

	got, err := Decode(GroupMessageEvent, json.RawMessage(payload), "bot-openid", "木木")
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}

	var hasText, hasImage bool
	for _, seg := range got.Message.Segments {
		switch seg.Type {
		case domain.SegmentText:
			hasText = hasText || strings.Contains(seg.Text, "看这个")
		case domain.SegmentImage:
			hasImage = true
		}
	}
	if !hasText {
		t.Error("应保留文本段")
	}
	if !hasImage {
		t.Error("应保留图片段")
	}
	if !strings.Contains(got.Message.PlainText, "看这个") {
		t.Errorf("正文 = %q，应包含原文", got.Message.PlainText)
	}
	if !strings.Contains(got.Message.PlainText, "[图片]") {
		t.Errorf("正文 = %q，应包含图片占位", got.Message.PlainText)
	}
}

// TestDecodeAnimatedEmojiBecomesEmojiSegment 防：QQ 的动画表情被当成普通图片，
// 于是「纯表情包」判定失效，门控里「表情包可以提前结束防抖」那套逻辑跟着废掉。
func TestDecodeAnimatedEmojiBecomesEmojiSegment(t *testing.T) {
	payload := `{
		"id": "msg-emoji",
		"content": "<faceType=1,faceId=\"1\",ext=\"abc\">",
		"group_openid": "group-abc",
		"author": {"member_openid": "user-1", "username": "小明"},
		"attachments": [{
			"content_type": "image/gif",
			"url": "https://grouppro.grouppro.qq.com/c.gif"
		}]
	}`

	got, err := Decode(GroupMessageEvent, json.RawMessage(payload), "bot-openid", "木木")
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}

	var emoji, image int
	for _, seg := range got.Message.Segments {
		switch seg.Type {
		case domain.SegmentEmoji:
			emoji++
		case domain.SegmentImage:
			image++
		}
	}
	if emoji != 1 {
		t.Errorf("表情段数 = %d，期望 1", emoji)
	}
	if image != 0 {
		t.Errorf("图片段数 = %d，期望 0（应识别为表情包）", image)
	}
	// 正文不应再留一个 [表情] 文本段与表情包重复。
	if strings.Contains(got.Message.PlainText, "[表情] ") || got.Message.PlainText == "[表情]" {
		t.Errorf("正文 = %q，表情占位应被清掉", got.Message.PlainText)
	}
}

// TestDecodeVoiceKeepsASRText 防：语音消息丢掉平台给的语音转文字结果。
func TestDecodeVoiceKeepsASRText(t *testing.T) {
	payload := `{
		"id": "msg-voice",
		"content": "",
		"group_openid": "group-abc",
		"author": {"member_openid": "user-1", "username": "小明"},
		"attachments": [{
			"content_type": "voice",
			"voice_wav_url": "https://grouppro.grouppro.qq.com/v.wav",
			"asr_refer_text": "今天天气不错"
		}]
	}`

	got, err := Decode(GroupMessageEvent, json.RawMessage(payload), "bot-openid", "木木")
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}

	if !strings.Contains(got.Message.PlainText, "今天天气不错") {
		t.Errorf("正文 = %q，应包含语音转文字结果", got.Message.PlainText)
	}
	// 语音应优先用 wav 地址。
	for _, seg := range got.Message.Segments {
		if seg.Type == domain.SegmentVoice && !strings.HasSuffix(seg.URL, ".wav") {
			t.Errorf("语音 URL = %q，期望使用 wav 地址", seg.URL)
		}
	}
}

// TestDecodeFileAttachmentKeptAsPlaceholder 防：文件/视频附件让整条消息变成空消息。
func TestDecodeFileAttachmentKeptAsPlaceholder(t *testing.T) {
	payload := `{
		"id": "msg-file",
		"content": "",
		"group_openid": "group-abc",
		"author": {"member_openid": "user-1", "username": "小明"},
		"attachments": [{
			"content_type": "application/pdf",
			"url": "https://grouppro.grouppro.qq.com/d.pdf",
			"filename": "合同.pdf"
		}]
	}`

	got, err := Decode(GroupMessageEvent, json.RawMessage(payload), "bot-openid", "木木")
	if err != nil {
		t.Fatalf("文件消息不应解码失败: %v", err)
	}
	if !strings.Contains(got.Message.PlainText, "合同.pdf") {
		t.Errorf("正文 = %q，应包含文件名", got.Message.PlainText)
	}
}

// TestValidateMediaURLRejects 防：入站 URL 白名单形同虚设（SSRF）。
func TestValidateMediaURLRejects(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{"非 https", "http://grouppro.grouppro.qq.com/a.png"},
		{"内网地址", "https://127.0.0.1/a.png"},
		{"云元数据", "https://169.254.169.254/latest/meta-data"},
		{"陌生域名", "https://evil.example.com/a.png"},
		{"带凭据", "https://user:pw@grouppro.grouppro.qq.com/a.png"},
		{"非 443 端口", "https://grouppro.grouppro.qq.com:8080/a.png"},
		{"控制字符", "https://grouppro.grouppro.qq.com/a\n.png"},
		{"空地址", ""},
		{"只有前缀的伪装域名", "https://nt.qq.com.evil.com/a.png"},
	}
	for _, c := range cases {
		if err := validateMediaURL(c.url); err == nil {
			t.Errorf("%s: %q 应被拒绝", c.name, c.url)
		}
	}
}

// TestValidateMediaURLAccepts 防：白名单写得太严，正常地址也被拒。
func TestValidateMediaURLAccepts(t *testing.T) {
	cases := []string{
		"https://grouppro.grouppro.qq.com/a.png",
		"https://multimedia.nt.qq.com.cn/download?appid=1407&fileid=abc",
		"https://x.nt.qq.com/a.jpg",
		"https://nt.qq.com/a.jpg",
		"https://GROUPPRO.grouppro.qq.com/a.png",
	}
	for _, u := range cases {
		if err := validateMediaURL(u); err != nil {
			t.Errorf("%q 应被接受，实际报错: %v", u, err)
		}
	}
}

// TestHydrateInboundMediaSkipsUntrusted 防：下载失败时把整条消息搞没，
// 或者在没有二进制时仍然清掉 URL 导致后续彻底没有线索。
func TestHydrateInboundMediaSkipsUntrusted(t *testing.T) {
	msg := &domain.Message{
		MessageID: "m1",
		Segments: []domain.Segment{
			{Type: domain.SegmentImage, URL: "https://evil.example.com/x.png"},
			{Type: domain.SegmentText, Text: "你好"},
		},
	}

	hydrateInboundMedia(t.Context(), msg, nopLogger{})

	if len(msg.Segments[0].Binary) != 0 {
		t.Error("不可信域名不应被下载")
	}
	if len(msg.Segments) != 2 {
		t.Errorf("段数 = %d，期望 2（下载失败不改结构）", len(msg.Segments))
	}
	if msg.Segments[1].Text != "你好" {
		t.Error("文本段不应被改动")
	}
}

// TestBuildAttachmentSegmentsCapsCount 防：一条消息带 50 个附件时全部处理，
// 导致一次入站触发 50 次下载，把网关读取循环拖死。
func TestBuildAttachmentSegmentsCapsCount(t *testing.T) {
	atts := make([]attachmentRef, 50)
	for i := range atts {
		atts[i] = attachmentRef{
			ContentType: "image/png",
			URL:         "https://grouppro.grouppro.qq.com/x.png",
		}
	}

	segments, labels := buildAttachmentSegments(atts, "")
	if len(segments) > maxInboundAttachments {
		t.Errorf("段数 = %d，超过上限 %d", len(segments), maxInboundAttachments)
	}
	if len(labels) != len(segments) {
		t.Errorf("标签数 %d 与段数 %d 不一致", len(labels), len(segments))
	}
}

// nopLogger 是测试用的空日志实现。
type nopLogger struct{}

func (nopLogger) Warn(string, ...any) {}

// TestPersistedSegmentsDropBinary 防：图片二进制被 base64 落库。
//
// 一张 200KB 的图序列化后接近 270KB 文本，
// 而 Recent 每次恢复上下文都要读回来反序列化 —— 聊天记录一多就是几十 MB 无谓 IO。
// 这里从「解码后的消息」一路走到「落库再读回」，确保整条链路都不带二进制。
func TestPersistedSegmentsDropBinary(t *testing.T) {
	payload := `{
		"id": "msg-img-store",
		"content": "",
		"group_openid": "group-abc",
		"author": {"member_openid": "user-1", "username": "小明"},
		"attachments": [{
			"content_type": "image/png",
			"url": "https://grouppro.grouppro.qq.com/a.png"
		}]
	}`

	got, err := Decode(GroupMessageEvent, json.RawMessage(payload), "bot-openid", "木木")
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}

	// 模拟 hydrate 之后的状态：段里带二进制。
	got.Message.Segments[0].Binary = []byte{0x89, 'P', 'N', 'G', 1, 2, 3, 4}
	originalLen := len(got.Message.Segments[0].Binary)

	db := openStoreForTest(t)
	if err := db.Messages().Save(t.Context(), got.Message); err != nil {
		t.Fatalf("落库失败: %v", err)
	}

	// 原始消息不应被落库动作改掉（attach_pic 还要复用这份二进制）。
	if len(got.Message.Segments[0].Binary) != originalLen {
		t.Error("Save 不应修改调用方的消息段")
	}

	msgs, err := db.Messages().Recent(t.Context(), got.Message.SessionID, 10)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("消息数 = %d，期望 1", len(msgs))
	}
	for _, seg := range msgs[0].Segments {
		if len(seg.Binary) != 0 {
			t.Errorf("读回的段 %q 带 %d 字节二进制，不应落库", seg.Type, len(seg.Binary))
		}
	}
}

// TestDecodeAttachmentSizeAsNumberDoesNotDropMessage 防的是本轮真正的那条 bug：
// QQ 下发的 attachments[].size 有时是数字，有时是字符串。
//
// 早期 attachmentRef.Size 声明成 string，于是数字让**整条消息**
// 反序列化失败 —— 日志里只有一句「解析消息 payload 失败」，
// 表现为「群里发的表情包/图片，机器人完全没反应」，而且连正文一起丢。
func TestDecodeAttachmentSizeAsNumberDoesNotDropMessage(t *testing.T) {
	cases := []struct {
		name string
		size string
	}{
		{"size 是数字", `1234`},
		{"size 是字符串", `"1234"`},
		{"size 缺失", ``},
		{"size 是 null", `null`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sizeField := ""
			if tc.size != "" {
				sizeField = `"size": ` + tc.size + `,`
			}
			payload := `{
				"id": "msg-size",
				"content": "看图",
				"group_openid": "group-abc",
				"author": {"member_openid": "user-1", "username": "小明"},
				"attachments": [{
					` + sizeField + `
					"content_type": "image/png",
					"url": "https://grouppro.grouppro.qq.com/a.png"
				}]
			}`

			got, err := Decode(GroupMessageEvent, json.RawMessage(payload), "bot-openid", "木木")
			if err != nil {
				t.Fatalf("附件字段类型变化不该让整条消息解码失败: %v", err)
			}
			if !strings.Contains(got.Message.PlainText, "看图") {
				t.Errorf("正文 = %q，应保留原文", got.Message.PlainText)
			}
			var hasImage bool
			for _, seg := range got.Message.Segments {
				if seg.Type == domain.SegmentImage {
					hasImage = true
				}
			}
			if !hasImage {
				t.Error("应保留图片段")
			}
		})
	}
}

// TestDecodeEmojiTagOnlyContentBecomesEmojiSegment 防：
// QQ 对「动画表情」只下发字面量 content="[表情]"，不带任何表情类型标记。
//
// 参考实现（plugin.py:_build_attachments）第四条条件是
// `content_hint in {"[表情]", "[动画表情]"}`；漏掉它的话这类表情包
// 会被当成普通图片存进 image 段，于是表情包库永远收不到东西。
func TestDecodeEmojiTagOnlyContentBecomesEmojiSegment(t *testing.T) {
	for _, content := range []string{"[表情]", "[动画表情]"} {
		t.Run(content, func(t *testing.T) {
			payload := `{
				"id": "msg-face-literal",
				"content": "` + content + `",
				"group_openid": "group-abc",
				"author": {"member_openid": "user-1", "username": "小明"},
				"attachments": [{
					"content_type": "image/png",
					"url": "https://grouppro.grouppro.qq.com/e.png"
				}]
			}`

			got, err := Decode(GroupMessageEvent, json.RawMessage(payload), "bot-openid", "木木")
			if err != nil {
				t.Fatalf("解码失败: %v", err)
			}
			if len(got.Message.Segments) != 1 || got.Message.Segments[0].Type != domain.SegmentEmoji {
				t.Fatalf("段 = %+v，期望只有一个 emoji 段", got.Message.Segments)
			}
		})
	}
}

// TestDecodeEmojiAttachmentTypeIsEmoji 防：附件类型标了 emoji/face/sticker 时
// 却被当成普通图片。
func TestDecodeEmojiAttachmentTypeIsEmoji(t *testing.T) {
	for _, typ := range []string{"emoji", "face", "sticker"} {
		t.Run(typ, func(t *testing.T) {
			payload := `{
				"id": "msg-typed",
				"content": "",
				"group_openid": "group-abc",
				"author": {"member_openid": "user-1", "username": "小明"},
				"attachments": [{
					"type": "` + typ + `",
					"content_type": "image/png",
					"url": "https://grouppro.grouppro.qq.com/e.png"
				}]
			}`

			got, err := Decode(GroupMessageEvent, json.RawMessage(payload), "bot-openid", "木木")
			if err != nil {
				t.Fatalf("解码失败: %v", err)
			}
			if got.Message.Segments[0].Type != domain.SegmentEmoji {
				t.Errorf("段类型 = %q，期望 emoji", got.Message.Segments[0].Type)
			}
		})
	}
}

// TestNormalizeSegmentMimeFixesWrongType 防：QQ 把 content_type 报成
// application/octet-stream 时，表情包按错误扩展名落盘（全变成 .png）。
//
// 表情包落盘用的扩展名来自 seg.MimeType，错了之后 WebUI 预览与实际格式不符，
// 而且这种错误不报任何日志，只能靠断言抓到。
func TestNormalizeSegmentMimeFixesWrongType(t *testing.T) {
	gif := append([]byte("GIF89a"), 1, 0, 1, 0, 0, 0, 0, ';')

	cases := []struct {
		name string
		decl string
		want string
	}{
		{"octet-stream 按文件头改正", "application/octet-stream", "image/gif"},
		{"空值补上", "", "image/gif"},
		{"错误声明也按文件头改正", "image/png", "image/gif"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seg := &domain.Segment{Type: domain.SegmentEmoji, MimeType: tc.decl}
			normalizeSegmentMime(seg, gif)
			if seg.MimeType != tc.want {
				t.Errorf("MimeType = %q，期望 %q", seg.MimeType, tc.want)
			}
		})
	}

	// 嗅探不出具体类型时保留原声明，不要覆盖成 octet-stream 把信息弄丢。
	seg := &domain.Segment{MimeType: "image/gif"}
	normalizeSegmentMime(seg, make([]byte, 64))
	if seg.MimeType != "image/gif" {
		t.Errorf("嗅探不出类型时应保留原声明，实际 %q", seg.MimeType)
	}
}

// TestDetectContentTypeRecognizesGIF 守住上一条测试依赖的前提。
func TestDetectContentTypeRecognizesGIF(t *testing.T) {
	gif := []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;")

	// mediaClient 不跟随重定向且校验白名单，所以这里直接测修正逻辑
	// 所依赖的前提：http.DetectContentType 能认出 GIF。
	if got := http.DetectContentType(gif); got != "image/gif" {
		t.Fatalf("前提不成立：DetectContentType = %q，期望 image/gif", got)
	}
}
