package qq

// 表情包入站链路的端到端测试（不碰真 QQ、不碰真模型）。
//
// 背景：真实日志里出现
//
//	解码消息失败 err=解析消息 payload 失败:
//	  json: cannot unmarshal number into Go struct field attachmentRef.attachments.size of type string
//
// 这条错误把整条消息（连同正文）一起丢掉，表现就是「收不到表情包」。
// 这个文件用「原始 payload → Decode → hydrate → 段类型」的完整链路守着它。

import (
	"encoding/json"
	"testing"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
)

// gifBytes 是一个最小可用的 GIF（1×1 像素）。
func gifBytes() []byte {
	return append([]byte("GIF89a"), 1, 0, 1, 0, 0, 0, 0, ';')
}

// TestEmojiInboundChain 验证真实形状的 QQ 表情包事件能走完解码+下载。
//
// payload 刻意用「size 是数字」+「content 是 [动画表情]」+「没有 type 字段」，
// 这三样组合是 QQ 实际下发动画表情时最常见的形态。
func TestEmojiInboundChain(t *testing.T) {
	gif := gifBytes()

	// 下载路径由 media_test.go 覆盖（https 白名单让 httptest 场景不适用），
	// 这里只覆盖解码 + MIME 修正 + 收藏判定这三段。
	payload := `{
		"id": "msg-emoji-e2e",
		"content": "[动画表情]",
		"group_openid": "group-abc",
		"author": {"member_openid": "user-1", "username": "小明"},
		"timestamp": "2026-10-22T10:00:00Z",
		"attachments": [{
			"content_type": "image/gif",
			"url": "https://grouppro.grouppro.qq.com/e.gif",
			"filename": "e.gif",
			"size": 4321
		}]
	}`

	got, err := Decode(GroupMessageEvent, json.RawMessage(payload), "bot-openid", "木木")
	if err != nil {
		t.Fatalf("表情包消息不应解码失败: %v", err)
	}

	// 段类型必须是 emoji，否则 Hub 的 collectEmoji 根本不会看它。
	if len(got.Message.Segments) != 1 {
		t.Fatalf("段数 = %d，期望 1：%+v", len(got.Message.Segments), got.Message.Segments)
	}
	if got.Message.Segments[0].Type != domain.SegmentEmoji {
		t.Fatalf("段类型 = %q，期望 emoji（否则表情包库永远收不到东西）", got.Message.Segments[0].Type)
	}

	// 模拟 adapter 下载成功后的状态：填二进制 + 修正 MIME。
	msg := got.Message
	msg.Segments[0].Binary = gif
	normalizeSegmentMime(&msg.Segments[0], gif)
	if msg.Segments[0].MimeType != "image/gif" {
		t.Errorf("MimeType = %q，期望 image/gif", msg.Segments[0].MimeType)
	}

	// 收藏的判定条件（Hub.collectEmoji）必须成立。
	if msg.Segments[0].Type != domain.SegmentEmoji || len(msg.Segments[0].Binary) == 0 {
		t.Error("表情段带二进制时应满足收藏条件")
	}
}

// TestDecodeEmojiWithNumericFields 验证附件里所有字段都被当成数字下发时也不炸。
//
// QQ 的附件字段类型不稳定，任何一个类型对不上都会让整条消息丢失 ——
// 这是本轮 bug 的直接来源。
func TestDecodeEmojiWithNumericFields(t *testing.T) {
	payload := `{
		"id": "msg-numeric",
		"content": "",
		"group_openid": "group-abc",
		"author": {"member_openid": "user-1"},
		"attachments": [{
			"content_type": "image/png",
			"type": "emoji",
			"url": "https://grouppro.grouppro.qq.com/e.png",
			"size": 1234,
			"is_emoji": 1
		}]
	}`

	got, err := Decode(GroupMessageEvent, json.RawMessage(payload), "bot-openid", "木木")
	if err != nil {
		t.Fatalf("数字字段不该让解码失败: %v", err)
	}
	if got.Message.Segments[0].Type != domain.SegmentEmoji {
		t.Errorf("段类型 = %q，期望 emoji", got.Message.Segments[0].Type)
	}
}
