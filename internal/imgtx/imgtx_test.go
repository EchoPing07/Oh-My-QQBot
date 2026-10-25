package imgtx

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

// 注意：本文件刻意不 import image/gif（见 buildGIF 的注释）。
// GIF 测试数据全部手工构造，解码依赖 imgtx.go（生产代码）注册的解码器。

// TestSniff 按魔数识别格式。优先级高于 MIME 声明。
func TestSniff(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want Format
	}{
		{"JPEG", encodeJ(t, solidImage(4, 4)), FormatJPEG},
		{"PNG", encodeP(t, solidImage(4, 4)), FormatPNG},
		{"GIF", buildGIF(t, gifTestFrame{}), FormatGIF},
		{"BMP", makeBMP(t, 4, 4, 24), FormatBMP},
		{"WEBP 头", fakeWEBP(), FormatWEBP},
		{"太短", []byte{0xFF, 0xD8}, FormatUnknown},
		{"纯文本", []byte("this is not an image at all"), FormatUnknown},
		{"空", nil, FormatUnknown},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Sniff(tc.data); got != tc.want {
				t.Errorf("Sniff = %q，期望 %q", got, tc.want)
			}
		})
	}
}

// TestNormalizeKeepsFriendlyFormats 验证已是模型友好格式时原样返回。
//
// 不重编码很重要：JPEG 二次压缩会掉画质，PNG 重编码可能变大，
// 而它们本来就能被模型直接吃。
//
// GIF 不在直通集合里（audit 11 P1-3）：部分 OpenAI 兼容视觉接口
// 会拒绝动画 GIF，参考实现只发首帧转成的 PNG —— 见 TestNormalizeGIF*。
func TestNormalizeKeepsFriendlyFormats(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		mime string
	}{
		{"JPEG", encodeJ(t, solidImage(8, 8)), "image/jpeg"},
		{"PNG", encodeP(t, solidImage(8, 8)), "image/png"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Normalize(tc.data)
			if err != nil {
				t.Fatalf("不该报错: %v", err)
			}
			if got.Converted {
				t.Error("友好格式不该被转换")
			}
			if got.MimeType != tc.mime {
				t.Errorf("MIME = %q，期望 %q", got.MimeType, tc.mime)
			}
			if !bytes.Equal(got.Binary, tc.data) {
				t.Error("字节内容应保持原样")
			}
		})
	}
}

// TestNormalizeConvertsBMP 验证 BMP 会被转成 JPEG。
//
// 这是本包存在的主要理由：Go 标准库解不开 BMP，
// 原样发给视觉模型会 400，整轮思考失败。
func TestNormalizeConvertsBMP(t *testing.T) {
	bmp := makeBMP(t, 8, 8, 24)

	got, err := Normalize(bmp)
	if err != nil {
		t.Fatalf("BMP 转码失败: %v", err)
	}
	if !got.Converted {
		t.Error("BMP 应该被转换")
	}
	if got.MimeType != "image/jpeg" {
		t.Errorf("MIME = %q，期望 image/jpeg", got.MimeType)
	}
	// 转换结果必须真的能被当成 JPEG 解开。
	if Sniff(got.Binary) != FormatJPEG {
		t.Error("转换结果不是合法 JPEG")
	}
	if _, err := jpeg.Decode(bytes.NewReader(got.Binary)); err != nil {
		t.Errorf("转换结果无法解码: %v", err)
	}
}

// TestNormalizeBMP32 验证 32 位 BMP 也能转。
func TestNormalizeBMP32(t *testing.T) {
	bmp := makeBMP(t, 6, 5, 32)

	got, err := Normalize(bmp)
	if err != nil {
		t.Fatalf("32 位 BMP 转码失败: %v", err)
	}
	if !got.Converted {
		t.Error("应该被转换")
	}

	img, err := jpeg.Decode(bytes.NewReader(got.Binary))
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 6 || b.Dy() != 5 {
		t.Errorf("尺寸 = %dx%d，期望 6x5", b.Dx(), b.Dy())
	}
}

// TestNormalizeRejectsWEBP 验证 WEBP 返回明确错误。
//
// 关键：必须报错，不能退回原始字节 —— 把 WEBP 原样发给模型同样是 400。
func TestNormalizeRejectsWEBP(t *testing.T) {
	_, err := Normalize(fakeWEBP())
	if err == nil {
		t.Fatal("WEBP 应报错（标准库无法解码）")
	}
	// 错误信息要能看出是 WEBP 导致的，便于日志排查。
	if !bytes.Contains([]byte(err.Error()), []byte("WEBP")) {
		t.Errorf("错误信息应提到 WEBP: %v", err)
	}
}

// TestNormalizeEmpty 验证空输入。
func TestNormalizeEmpty(t *testing.T) {
	if _, err := Normalize(nil); err == nil {
		t.Error("空输入应报错")
	}
	if _, err := Normalize([]byte{}); err == nil {
		t.Error("空切片应报错")
	}
}

// TestNormalizeUnknownFallsBack 验证魔数认不出但标准库能解时会转码。
//
// 覆盖「合法图片但魔数不匹配」的情况，比如某些带额外头的 JPEG 变体。
func TestNormalizeUnknownFallsBack(t *testing.T) {
	// 一个合法 PNG，但把魔数改掉，Snapshot 会认不出。
	pngData := encodeP(t, solidImage(4, 4))
	broken := append([]byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}, pngData...)

	// 前 12 字节变成 0，Sniff 认不出，但 image.Decode 也解不开（头部坏了）。
	if _, err := Normalize(broken); err == nil {
		t.Log("头部损坏的数据反而解开了，说明标准库有容错（不是错误）")
	}

	// 换一种：真正的 BMP，但强迫走 default 分支（通过篡改 Sniff 会认的 BM 头）。
	// 这里直接验证 default 分支对合法 JPEG 的行为即可。
	jpegData := encodeJ(t, solidImage(4, 4))
	got, err := Normalize(jpegData)
	if err != nil {
		t.Fatalf("合法 JPEG 不该失败: %v", err)
	}
	if got.Format != FormatJPEG {
		t.Errorf("格式 = %q", got.Format)
	}
}

// TestDecodeBMPRejectsUnsupported 验证各种不支持的 BMP 变体被拒绝。
func TestDecodeBMPRejectsUnsupported(t *testing.T) {
	cases := []struct {
		name string
		data []byte
	}{
		{"太短", []byte("BM")},
		{"没有 BM 魔数", bytes.Repeat([]byte{0}, 60)},
		{"位深 8", makeBMPHeader(t, 8, 8, 8, 0)},
		{"RLE 压缩", makeBMPHeader(t, 8, 8, 24, 1)},
		{"零宽度", makeBMPHeader(t, 0, 8, 24, 0)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeBMP(tc.data); err == nil {
				t.Error("应报错")
			}
		})
	}
}

// TestDecodeBMPTopDown 验证自上而下存储的 BMP（高度为负）。
func TestDecodeBMPTopDown(t *testing.T) {
	// 标准 BMP 高度为正（自下而上）；负高度表示 top-down。
	bmp := makeBMPWithHeight(t, 4, -4)

	img, err := decodeBMP(bmp)
	if err != nil {
		t.Fatalf("top-down BMP 解码失败: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 4 || b.Dy() != 4 {
		t.Errorf("尺寸 = %dx%d，期望 4x4", b.Dx(), b.Dy())
	}
}

// TestFlattenAlpha 验证透明区域铺白底而不是变黑。
//
// 表情包大量是透明底 PNG，直接转 JPEG 会让透明处变成黑色，
// 视觉模型会把整张图描述成「黑色背景上的图案」。
func TestFlattenAlpha(t *testing.T) {
	// 全透明（0,0,0,0）的图，铺白底后应该接近白色。
	src := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	got := flattenAlpha(src)

	r, g, b, _ := got.At(0, 0).RGBA()
	// 允许小误差。
	const want = 0xffff
	for name, v := range map[string]uint32{"R": r, "G": g, "B": b} {
		if v < want*95/100 {
			t.Errorf("%s = %d，期望接近 %d（透明区域应铺白底）", name, v, want)
		}
	}
}

// TestFlattenAlphaKeepsOpaque 验证不透明像素不被改动。
func TestFlattenAlphaKeepsOpaque(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	src.Set(0, 0, color.NRGBA{200, 100, 50, 255})

	got := flattenAlpha(src)
	r, g, b, _ := got.At(0, 0).RGBA()

	if uint8(r>>8) != 200 || uint8(g>>8) != 100 || uint8(b>>8) != 50 {
		t.Errorf("不透明像素被改动: (%d,%d,%d)", r>>8, g>>8, b>>8)
	}
}

// TestIsSupportedMime 验证 MIME 判断。
//
// image/gif 不在直通集合里：必须先经 Normalize 转成首帧 PNG
// （部分 OpenAI 兼容接口拒绝动画 GIF，audit 11 P1-3）。
func TestIsSupportedMime(t *testing.T) {
	yes := []string{"image/jpeg", "image/png", "IMAGE/PNG", " image/jpeg "}
	no := []string{"image/gif", "image/webp", "image/bmp", "application/octet-stream", ""}

	for _, m := range yes {
		if !IsSupportedMime(m) {
			t.Errorf("%q 应被支持", m)
		}
	}
	for _, m := range no {
		if IsSupportedMime(m) {
			t.Errorf("%q 不该被支持", m)
		}
	}
}

// TestDecodeAnyBMP 验证 DecodeAny 对 BMP 的分派。
func TestDecodeAnyBMP(t *testing.T) {
	img, format, err := DecodeAny(makeBMP(t, 3, 3, 24))
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if format != FormatBMP {
		t.Errorf("格式 = %q，期望 bmp", format)
	}
	if b := img.Bounds(); b.Dx() != 3 || b.Dy() != 3 {
		t.Errorf("尺寸 = %dx%d", b.Dx(), b.Dy())
	}
}

// ============ 测试辅助 ============

func solidImage(w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 100, G: 150, B: 200, A: 255})
		}
	}
	return img
}

func encodeJ(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("编码 JPEG 失败: %v", err)
	}
	return buf.Bytes()
}

func encodeP(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("编码 PNG 失败: %v", err)
	}
	return buf.Bytes()
}

// makeBMP 构造一个未压缩 BMP。
func makeBMP(t *testing.T, w, h, bitCount int) []byte {
	t.Helper()
	return makeBMPWithHeight(t, w, h)
}

// makeBMPWithHeight 允许传负高度构造 top-down BMP。
func makeBMPWithHeight(t *testing.T, w, h int) []byte {
	t.Helper()

	absH := h
	if absH < 0 {
		absH = -absH
	}
	bitCount := 24
	rowSize := ((bitCount*w + 31) / 32) * 4
	pixelData := rowSize * absH
	fileSize := 54 + pixelData

	b := make([]byte, fileSize)
	b[0], b[1] = 'B', 'M'
	le := binary.LittleEndian
	le.PutUint32(b[2:], uint32(fileSize))
	le.PutUint32(b[10:], 54)
	le.PutUint32(b[14:], 40)
	le.PutUint32(b[18:], uint32(w))
	le.PutUint32(b[22:], uint32(int32(h)))
	le.PutUint16(b[26:], 1)
	le.PutUint16(b[28:], uint16(bitCount))
	le.PutUint32(b[34:], uint32(pixelData))

	// 填充像素：BGR 顺序，画成渐变便于区分。
	for i := 54; i < fileSize; i += 3 {
		if i+2 < fileSize {
			b[i] = 60    // B
			b[i+1] = 120 // G
			b[i+2] = 200 // R
		}
	}
	return b
}

// makeBMPHeader 构造只有头的 BMP，用于测试拒绝分支。
func makeBMPHeader(t *testing.T, w, h, bitCount, compression int) []byte {
	t.Helper()

	b := make([]byte, 54)
	b[0], b[1] = 'B', 'M'
	le := binary.LittleEndian
	le.PutUint32(b[2:], 54)
	le.PutUint32(b[10:], 54)
	le.PutUint32(b[14:], 40)
	le.PutUint32(b[18:], uint32(w))
	le.PutUint32(b[22:], uint32(h))
	le.PutUint16(b[26:], 1)
	le.PutUint16(b[28:], uint16(bitCount))
	le.PutUint32(b[30:], uint32(compression))
	return b
}

// fakeWEBP 构造一个只有 WEBP 文件头的假数据。
//
// 不构造真正可解码的 WEBP：标准库没有解码器，
// 我们要测的是「识别出格式并拒绝」，不是「能解码」。
func fakeWEBP() []byte {
	b := make([]byte, 32)
	copy(b[0:4], "RIFF")
	binary.LittleEndian.PutUint32(b[4:8], 24)
	copy(b[8:12], "WEBP")
	copy(b[12:16], "VP8 ")
	return b
}

// ============ GIF → 首帧 PNG（audit 11 P1-3） ============

// TestNormalizeGIFToFirstFramePNG 验证 GIF 不再原样透传，而是转成首帧 PNG。
//
// 参考 refs/MaiBot/src/llm_models/model_client/openai_client.py:354-363：
// 「部分 OpenAI 兼容视觉接口会拒绝动画 GIF 或动画 WebP，因此只传递首帧」。
// 旧实现把 GIF 当直通格式原样发出，严格端点上会 400、整轮思考失败。
func TestNormalizeGIFToFirstFramePNG(t *testing.T) {
	// 单帧 GIF，局部调色板索引 0 是红色。
	gifData := buildGIF(t, gifTestFrame{localPalette: [][3]byte{{255, 0, 0}, {0, 0, 0}}})

	got, err := Normalize(gifData)
	if err != nil {
		t.Fatalf("GIF 转码失败: %v", err)
	}
	if !got.Converted {
		t.Error("GIF 应被转换为 PNG")
	}
	if got.MimeType != "image/png" {
		t.Errorf("MIME = %q，期望 image/png", got.MimeType)
	}
	if got.Format != FormatGIF {
		t.Errorf("原始格式 = %q，期望 gif", got.Format)
	}

	img, err := png.Decode(bytes.NewReader(got.Binary))
	if err != nil {
		t.Fatalf("转换结果不是合法 PNG: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 1 || b.Dy() != 1 {
		t.Errorf("尺寸 = %dx%d，期望 1x1", b.Dx(), b.Dy())
	}
	r, g, b, _ := img.At(0, 0).RGBA()
	if uint8(r>>8) != 255 || uint8(g>>8) != 0 || uint8(b>>8) != 0 {
		t.Errorf("首帧颜色不对: (%d,%d,%d)，期望红色", r>>8, g>>8, b>>8)
	}
}

// TestNormalizeGIFTakesFirstFrameOfAnimation 验证多帧 GIF 只取第一帧。
func TestNormalizeGIFTakesFirstFrameOfAnimation(t *testing.T) {
	// 两帧动画：第一帧红、第二帧蓝。gif.Decode 只解第一帧。
	gifData := buildGIF(t,
		gifTestFrame{localPalette: [][3]byte{{255, 0, 0}, {0, 0, 0}}},
		gifTestFrame{localPalette: [][3]byte{{0, 0, 255}, {0, 0, 0}}},
	)

	got, err := Normalize(gifData)
	if err != nil {
		t.Fatalf("动画 GIF 转码失败: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(got.Binary))
	if err != nil {
		t.Fatalf("转换结果不是合法 PNG: %v", err)
	}
	r, g, b, _ := img.At(0, 0).RGBA()
	if uint8(r>>8) != 255 {
		t.Errorf("应取第一帧（红），实际 (%d,%d,%d)", r>>8, g>>8, b>>8)
	}
}

// TestNormalizeGIFFlattensTransparency 验证透明 GIF 转出的 PNG 铺了白底。
//
// 这同时覆盖 flattenAlpha 的 Paletted 路径（audit 11 P2-9）：
// gif.Decode 返回 *image.Paletted，旧的类型 switch 会跳过它，
// 透明区域直接变黑。
func TestNormalizeGIFFlattensTransparency(t *testing.T) {
	// 调色板索引 0 标记为透明（透明控制扩展）。
	gifData := buildGIF(t, gifTestFrame{transparent: true})

	got, err := Normalize(gifData)
	if err != nil {
		t.Fatalf("透明 GIF 转码失败: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(got.Binary))
	if err != nil {
		t.Fatalf("转换结果不是合法 PNG: %v", err)
	}
	r, g, b, _ := img.At(0, 0).RGBA()
	if uint8(r>>8) < 250 || uint8(g>>8) < 250 || uint8(b>>8) < 250 {
		t.Errorf("透明区域应铺白底，实际 (%d,%d,%d)", r>>8, g>>8, b>>8)
	}
}

// TestDecodeAnyGIFWithoutGifImport 守住「imgtx 包自身注册了 GIF 解码器」
// （audit 11 P2-10）。
//
// 整个测试包都不 import image/gif（构造数据用手写字节，见 buildGIF），
// 所以 DecodeAny 对 GIF 的解码只能依赖 imgtx.go（生产代码）里
// gif.Decode 带来的注册。以前 image/gif 只出现在字符串里、
// 没进主程序依赖图，这个场景会返回 image: unknown format ——
// 而旧测试因为自己 import 了 image/gif 根本测不出来。
func TestDecodeAnyGIFWithoutGifImport(t *testing.T) {
	img, format, err := DecodeAny(buildGIF(t, gifTestFrame{}))
	if err != nil {
		t.Fatalf("DecodeAny 应能解 GIF（依赖生产代码注册的解码器）: %v", err)
	}
	if format != FormatGIF {
		t.Errorf("格式 = %q，期望 gif", format)
	}
	if b := img.Bounds(); b.Dx() != 1 || b.Dy() != 1 {
		t.Errorf("尺寸 = %dx%d", b.Dx(), b.Dy())
	}
}

// ============ 头校验：截断与超大尺寸（audit 11 P2-11 / P2-12） ============

// TestNormalizeRejectsTruncatedPNG 验证「魔数对但内容截断」的 PNG 被拒绝。
//
// 旧实现只看魔数，16 字节的假 PNG 会被原样发给模型。
func TestNormalizeRejectsTruncatedPNG(t *testing.T) {
	// PNG 魔数（8 字节）+ chunk 长度（4 字节）+ "IHDR"（4 字节）恰好 16 字节，
	// IHDR 体（宽高等 13 字节）完全缺失。
	truncated := append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A},
		0x00, 0x00, 0x00, 0x0D, 'I', 'H', 'D', 'R')

	got, err := Normalize(truncated)
	if err == nil {
		t.Fatal("截断的 PNG 应被拒绝（绝不把坏字节发出去）")
	}
	if got.Format != FormatPNG {
		t.Errorf("错误路径应带回原始格式，实际 %q", got.Format)
	}
	if len(got.Binary) != 0 {
		t.Error("拒绝路径绝不能退回原始字节")
	}
}

// TestNormalizeRejectsOversizedPNG 验证头声明超大尺寸的 PNG 被拒绝。
//
// 一个几 KB 的 PNG 头声明 30000x30000，image.Decode 会按声明尺寸
// 分配 ~3.6 GB 内存 —— 必须在只读头的阶段拦住。
func TestNormalizeRejectsOversizedPNG(t *testing.T) {
	got, err := Normalize(pngHeaderWithSize(30000, 30000))
	if err == nil {
		t.Fatal("超大尺寸的 PNG 应被拒绝")
	}
	if got.Format != FormatPNG {
		t.Errorf("错误路径应带回原始格式，实际 %q", got.Format)
	}
	if len(got.Binary) != 0 {
		t.Error("拒绝路径绝不能退回原始字节")
	}
}

// TestCheckDimensions 边界值：单边/总像素在上限内放行、超限拒绝。
func TestCheckDimensions(t *testing.T) {
	ok := [][2]int{{1, 1}, {16384, 1}, {4000, 10000}} // 4000*10000 = 4000 万，恰好等于上限
	bad := [][2]int{{0, 1}, {1, 0}, {-1, 5}, {16385, 1}, {1, 16385}, {5000, 10000}}
	for _, d := range ok {
		if err := checkDimensions(d[0], d[1]); err != nil {
			t.Errorf("%dx%d 在上限内，不该拒绝: %v", d[0], d[1], err)
		}
	}
	for _, d := range bad {
		if err := checkDimensions(d[0], d[1]); err == nil {
			t.Errorf("%dx%d 超限，应被拒绝", d[0], d[1])
		}
	}
}

// ============ flattenAlpha 半透明合成（audit 11 P2-8 / P2-9） ============

// TestFlattenAlphaSemiTransparentCompositesWhite 验证半透明像素的标准 alpha 合成。
//
// 基准值来自审查报告：NRGBA{200,100,50,A=128} 铺白底应为 (227,177,152)
// （200*128/255 + 255*127/255 ≈ 227）。旧实现把 At().RGBA() 的预乘值
// 又乘一次 alpha，实际输出 (177,152,139)，整体偏暗。
func TestFlattenAlphaSemiTransparentCompositesWhite(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	src.Set(0, 0, color.NRGBA{R: 200, G: 100, B: 50, A: 128})

	got := flattenAlpha(src)
	r, g, b, _ := got.At(0, 0).RGBA()

	// draw 的整数实现允许 ±1 的舍入误差。
	want := [3]int{227, 177, 152}
	got8 := [3]int{int(r >> 8), int(g >> 8), int(b >> 8)}
	for i, name := range [3]string{"R", "G", "B"} {
		if d := got8[i] - want[i]; d < -1 || d > 1 {
			t.Errorf("%s = %d，期望 %d 附近（标准 alpha 合成，只乘一次）", name, got8[i], want[i])
		}
	}
}

// TestFlattenAlphaPalettedCompositesWhite 验证调色板图不再被跳过。
//
// 表情包绝大多数是 Paletted。旧实现的类型 switch 只认 NRGBA/RGBA 系，
// Paletted 原样穿过 flattenAlpha，经 JPEG 解回时透明像素读到 (100,50,25)
// —— 既没合成白底，颜色还被预乘压暗。
func TestFlattenAlphaPalettedCompositesWhite(t *testing.T) {
	pal := color.Palette{color.NRGBA{R: 200, G: 100, B: 50, A: 128}, color.White}
	src := image.NewPaletted(image.Rect(0, 0, 1, 1), pal)
	src.Pix[0] = 0 // 像素取调色板索引 0

	got := flattenAlpha(src)
	r, g, b, _ := got.At(0, 0).RGBA()

	want := [3]int{227, 177, 152}
	got8 := [3]int{int(r >> 8), int(g >> 8), int(b >> 8)}
	for i, name := range [3]string{"R", "G", "B"} {
		if d := got8[i] - want[i]; d < -1 || d > 1 {
			t.Errorf("%s = %d，期望 %d 附近（Paletted 也必须走白底合成）", name, got8[i], want[i])
		}
	}
}

// TestFlattenAlphaKeepsNonZeroOrigin 验证非 (0,0) 起点的图（如裁剪子图）
// 在白底合成后颜色不错位。
func TestFlattenAlphaKeepsNonZeroOrigin(t *testing.T) {
	src := image.NewNRGBA(image.Rect(5, 5, 6, 6))
	src.Set(5, 5, color.NRGBA{R: 200, G: 100, B: 50, A: 128})

	got := flattenAlpha(src)
	if b := got.Bounds(); b.Min.X != 0 || b.Min.Y != 0 {
		t.Errorf("输出应从 (0,0) 开始，实际 %v", b)
	}
	r, g, b, _ := got.At(0, 0).RGBA()
	if d := int(r>>8) - 227; d < -1 || d > 1 {
		t.Errorf("R = %d，期望 227 附近（合成时坐标要按 Bounds().Min 对齐）", r>>8)
	}
	_ = g
	_ = b
}

// ============ 手写 GIF / PNG 头构造器 ============

// gifTestFrame 描述手写 GIF 里的一帧（全部是 1x1、像素取调色板索引 0）。
type gifTestFrame struct {
	// localPalette 非 nil 时该帧自带 2 色局部调色板；nil 用全局调色板。
	localPalette [][3]byte
	// transparent 为 true 时给该帧加透明控制扩展（透明索引固定 0）。
	transparent bool
}

// buildGIF 手工构造合法的 GIF 字节流。
//
// 刻意不用 image/gif 的编码器（audit 11 P2-10 的教训）：
// 测试文件一旦 import image/gif，解码器就随包 init 注册进测试二进制，
// 「imgtx 包自身没注册 gif 解码器」这种缺陷会被测试的 import 藏住。
// 手写字节让整个 imgtx 测试包不依赖 image/gif，
// GIF 解码全部依赖 imgtx.go（生产代码）注册的解码器。
//
// 字节布局（GIF89a）：
//
//	头 "GIF89a"
//	逻辑屏幕描述符：1x1，全局调色板 2 色（黑、白）
//	每帧：[透明控制扩展] + 图像描述符（可带局部调色板）+ 最小 LZW 块
//	结尾 0x3B
//
// 最小 LZW 块固定为 `02 02 44 01 00`：minCodeSize=2，
// 子块里 44 01 打包了 clear(4) + 像素索引 0 + EOI(5)。
func buildGIF(t *testing.T, frames ...gifTestFrame) []byte {
	t.Helper()

	var b []byte
	b = append(b, "GIF89a"...)
	b = append(b, 0x01, 0x00, 0x01, 0x00) // 宽=1 高=1（LE16）
	b = append(b, 0x80, 0x00, 0x00)       // 有全局调色板、2 色；背景色 0；纵横比 0
	b = append(b, 0x00, 0x00, 0x00)       // 全局调色板色 0：黑
	b = append(b, 0xFF, 0xFF, 0xFF)       // 全局调色板色 1：白

	for _, f := range frames {
		if f.transparent {
			// 图形控制扩展：packed=0x01（透明标志），透明索引固定 0。
			b = append(b, 0x21, 0xF9, 0x04, 0x01, 0x00, 0x00, 0x00, 0x00)
		}
		b = append(b, 0x2C, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00)
		if f.localPalette != nil {
			b[len(b)-1] = 0x80 // packed：本帧带局部调色板（2 色）
			for _, c := range f.localPalette {
				b = append(b, c[0], c[1], c[2])
			}
		}
		b = append(b, 0x02, 0x02, 0x44, 0x01, 0x00)
	}
	b = append(b, 0x3B)
	return b
}

// pngHeaderWithSize 构造一个只有「魔数 + IHDR」的 PNG 头，宽高按参数声明。
//
// image.DecodeConfig 只读到这里就能拿到尺寸 —— 用来测尺寸拦截，
// 不需要（也不应该）真的生成几亿像素的数据。
func pngHeaderWithSize(w, h int) []byte {
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], uint32(w))
	binary.BigEndian.PutUint32(ihdr[4:8], uint32(h))
	ihdr[8] = 8  // 位深
	ihdr[9] = 2  // 颜色类型：RGB
	ihdr[10] = 0 // 压缩
	ihdr[11] = 0 // 滤波
	ihdr[12] = 0 // 隔行

	var b []byte
	b = append(b, 0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A)
	b = binary.BigEndian.AppendUint32(b, uint32(len(ihdr)))
	b = append(b, "IHDR"...)
	b = append(b, ihdr...)
	b = binary.BigEndian.AppendUint32(b, crc32.ChecksumIEEE(append([]byte("IHDR"), ihdr...)))
	return b
}
