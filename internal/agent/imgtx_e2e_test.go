package agent

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"strings"
	"sync"
	"testing"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/logx"
)

// TestImagesOfConvertsBMPToJPEG 是 imgtx 接入的端到端保证。
//
// 防的是：转码函数写好了但没接进 imagesOf，
// 于是 BMP 仍然被原样当 image_url 发出去，服务端 400、整轮失败。
func TestImagesOfConvertsBMPToJPEG(t *testing.T) {
	bmp := buildBMP(8, 8)

	msg := &domain.Message{
		Segments: []domain.Segment{{
			Type:     domain.SegmentImage,
			Binary:   bmp,
			MimeType: "image/bmp",
		}},
	}

	got := imagesOf(msg)
	if len(got) != 1 {
		t.Fatalf("返回 %d 张，期望 1", len(got))
	}

	// 必须是 JPEG 的 data URL，不能是 bmp。
	if !strings.HasPrefix(got[0], "data:image/jpeg;base64,") {
		t.Fatalf("前缀不对（BMP 没被转码）：%.60s", got[0])
	}

	// 解出 base64 后要真的是 JPEG。
	raw := strings.TrimPrefix(got[0], "data:image/jpeg;base64,")
	data, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		t.Fatalf("base64 解码失败: %v", err)
	}
	if _, err := jpeg.Decode(bytes.NewReader(data)); err != nil {
		t.Errorf("不是合法 JPEG: %v", err)
	}
}

// TestImagesOfSkipsWEBP 验证 WEBP 被跳过而不是原样发出。
//
// 这是「宁可看不到图，也不能 400」的落地：
// 把 WEBP 原样发给视觉模型会让整轮思考失败。
func TestImagesOfSkipsWEBP(t *testing.T) {
	webp := make([]byte, 40)
	copy(webp[0:4], "RIFF")
	copy(webp[8:12], "WEBP")
	copy(webp[12:16], "VP8 ")

	msg := &domain.Message{
		Segments: []domain.Segment{{
			Type:     domain.SegmentImage,
			Binary:   webp,
			MimeType: "image/webp",
		}},
	}

	got := imagesOf(msg)
	if len(got) != 0 {
		t.Errorf("WEBP 应被跳过，实际返回 %d 张", len(got))
	}
}

// TestImagesOfWarnsOnSkippedImage 验证转码失败丢弃时有 Warn 日志（audit 11 P2-13）。
//
// 以前 WEBP 被静默跳过：模型看不到图、日志里一个字都没有，
// 用户完全无法知道「为什么机器人对这张图没反应」。
// QQ 表情包里 WEBP 占比高，丢弃本身是正确决策（不能发 400 字节），
// 但必须有可观测性。
func TestImagesOfWarnsOnSkippedImage(t *testing.T) {
	// logx 是全局状态：确保已初始化（sync.Once，多次调用无害）、
	// 级别放宽到 Warn、注册捕获广播器，结束时全部恢复。
	_ = logx.Init(logx.Options{Level: logx.LevelWarn, Console: false, Dir: t.TempDir()})
	prev := logx.CurrentLevel()
	logx.SetLevel(logx.LevelWarn)
	defer logx.SetLevel(prev)

	capLog := &captureBroadcast{}
	logx.SetBroadcaster(capLog)
	defer logx.SetBroadcaster(nil)

	webp := make([]byte, 40)
	copy(webp[0:4], "RIFF")
	copy(webp[8:12], "WEBP")
	copy(webp[12:16], "VP8 ")

	msg := &domain.Message{Segments: []domain.Segment{
		{Type: domain.SegmentImage, Binary: webp, MimeType: "image/webp"},
		{Type: domain.SegmentImage, Binary: testPNG(t)},
	}}

	got := imagesOf(msg)
	if len(got) != 1 {
		t.Fatalf("应只保留正常 PNG，实际 %d 张", len(got))
	}
	if !strings.HasPrefix(got[0], "data:image/png;base64,") {
		t.Errorf("正常图应继续进 Images: %.40s", got[0])
	}

	// 必须有一条 WARN，且消息里能看出是 WEBP 导致的。
	warned := false
	for _, l := range capLog.lines {
		// levelLabel 输出定宽（"WARN " 带尾空格），用前缀判断。
		if strings.HasPrefix(l.Level, "WARN") {
			warned = true
			if !strings.Contains(l.Message, "WEBP") {
				t.Errorf("Warn 消息应提到 WEBP: %q", l.Message)
			}
		}
	}
	if !warned {
		t.Errorf("跳过图片时应有一条 WARN 日志，实际广播 %d 条", len(capLog.lines))
	}
}

// captureBroadcast 捕获广播日志，用于断言「某条日志真的打了」。
type captureBroadcast struct {
	mu    sync.Mutex
	lines []logx.LogLine
}

func (c *captureBroadcast) Broadcast(l logx.LogLine) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, l)
}

// buildBMP 构造一个最小合法 BMP（24 位未压缩）。
func buildBMP(w, h int) []byte {
	rowSize := ((24*w + 31) / 32) * 4
	pixelData := rowSize * h
	fileSize := 54 + pixelData
	b := make([]byte, fileSize)
	b[0], b[1] = 'B', 'M'
	put := func(off int, v uint32) {
		b[off] = byte(v)
		b[off+1] = byte(v >> 8)
		b[off+2] = byte(v >> 16)
		b[off+3] = byte(v >> 24)
	}
	put(2, uint32(fileSize))
	put(10, 54)
	put(14, 40)
	put(18, uint32(w))
	put(22, uint32(h))
	b[26] = 1
	b[28] = 24
	put(34, uint32(pixelData))
	// 填充像素（BGR）。
	for i := 54; i+2 < fileSize; i += 3 {
		b[i], b[i+1], b[i+2] = 200, 120, 60
	}
	return b
}

var _ = color.RGBA{}
var _ = image.Rect
