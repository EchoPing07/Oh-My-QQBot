// Package imgtx 把图片转成视觉模型能接受的格式。
//
// 为什么要这个包（实测证据，不是照抄）：
//
//	Go 标准库的 image.Decode 只认 jpeg / png / gif。
//	实测（见 imgtx_test.go）：
//	  - BMP  → image: unknown format（解不开）
//	  - WEBP → image: unknown format（解不开）
//	  - MPO  → 能当成 JPEG 解开（MPO 是多张 JPEG 拼接，Go 只读第一张，正好够用）
//	而 QQ 用户发的图里 BMP 与 WEBP 都真实存在。
//	把这类图原样当 image_url 发给视觉模型，服务端会 400，
//	表现是整轮思考失败 —— 比看不到图严重得多（AGENTS.md 第 19 条）。
//
// 解码器注册：png / jpeg / gif 在本文件的生产代码里都有真实调用
// （png.Encode / jpeg.Encode / gif.Decode），import 即注册 ——
// 主二进制只要依赖本包，三种格式就都能解（audit 11 P2-10：
// 以前 image/gif 只出现在字符串里，根本没进主程序的依赖图，
// DecodeAny 对 GIF 永远失败，测试之所以绿是因为测试文件自己 import 了它）。
//
// GIF 不直通：部分 OpenAI 兼容视觉接口会拒绝动画 GIF，
// 参考实现（refs/MaiBot/src/llm_models/model_client/openai_client.py:354-363）
// 明确只取首帧转 PNG，这里照抄。
//
//	参考实现的对应位置：
//
//	refs/MaiBot/src/common/utils/utils_image.py:_prepare_image_for_receive_compression
//	它用 PIL 统一转 RGB 再存 JPEG。我们没有图像库可用（不引入新依赖），
//	所以这里只做「标准库能做的部分」：BMP 手工解码 + 统一转 JPEG 重编码。
//	WEBP 在没有第三方库的前提下无法解码，只能识别出来并标记为不可用，
//	由调用方决定是丢掉还是留占位 —— 绝不把原始字节发给模型。
package imgtx

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
)

// Format 是识别出的图片格式。
type Format string

const (
	FormatJPEG    Format = "jpeg"
	FormatPNG     Format = "png"
	FormatGIF     Format = "gif"
	FormatBMP     Format = "bmp"
	FormatWEBP    Format = "webp"
	FormatUnknown Format = "unknown"
)

// 图片尺寸上限（audit 11 P2-12：解压炸弹防护）。
//
// image.Decode 会按文件头声明的尺寸分配整张位图 ——
// 一个 1 MB 的 PNG 声明 30000x30000，解码就要 ~3.6 GB 内存。
// PIL 默认的 DecompressionBomb 上限约 8900 万像素；这里取更保守的
// 4000 万：发给视觉模型的图根本不需要那么大（参考实现反而会主动
// 缩到 2048 以内，utils_image.py 的 MODEL_MAX_UPSCALED_IMAGE_SIDE），
// 而 4000 万像素的 RGBA 位图已经是 ~160 MB，再大只会拖垮进程。
const (
	maxImageSide   = 16384
	maxImagePixels = 40_000_000
)

// 视觉模型普遍接受的格式（可原样直通）。
//
// GIF 不在直通集合里：部分 OpenAI 兼容视觉接口会拒绝动画 GIF，
// 参考实现 refs/MaiBot/src/llm_models/model_client/openai_client.py:354-363
// 明确写「只传递首帧」并转成 PNG 再发（SUPPORTED_OPENAI_IMAGE_FORMATS
// 也只有 jpeg/png/webp）。Normalize 对 GIF 走「首帧 → 白底 → PNG」，
// 不透传原始字节。
func isModelFriendly(f Format) bool {
	return f == FormatJPEG || f == FormatPNG
}

// Sniff 按魔数识别格式，优先于 MIME 声明。
//
// 为什么不信任 MimeType：QQ 给的 attachments 里 contentType 经常是
// application/octet-stream，或者干脆与实际内容不符。按字节判断才可靠。
func Sniff(data []byte) Format {
	if len(data) < 12 {
		return FormatUnknown
	}

	// JPEG: FF D8 FF
	if data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF {
		return FormatJPEG
	}
	// PNG: 89 50 4E 47 0D 0A 1A 0A
	if bytes.HasPrefix(data, []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}) {
		return FormatPNG
	}
	// GIF: "GIF87a" / "GIF89a"
	if bytes.HasPrefix(data, []byte("GIF87a")) || bytes.HasPrefix(data, []byte("GIF89a")) {
		return FormatGIF
	}
	// BMP: "BM"
	if data[0] == 'B' && data[1] == 'M' {
		return FormatBMP
	}
	// WEBP: "RIFF" + 4 字节长度 + "WEBP"
	if bytes.HasPrefix(data, []byte("RIFF")) && string(data[8:12]) == "WEBP" {
		return FormatWEBP
	}
	return FormatUnknown
}

// Result 是一次转码的结果。
type Result struct {
	// Binary 是转换后的字节。
	Binary []byte
	// MimeType 是转换后的 MIME。
	MimeType string
	// Converted 表示是否真的做了转换。
	Converted bool
	// Format 是原始格式。
	Format Format
}

// Normalize 把图片整理成视觉模型可用的格式。
//
// 行为：
//   - JPEG / PNG：先只读头校验（可解码 + 尺寸），通过后原样返回
//     （不重编码，避免有损二次压缩）
//   - GIF：取首帧铺白底后转 PNG —— 部分 OpenAI 兼容视觉接口
//     会拒绝动画 GIF，参考实现同样只发首帧（openai_client.py:354-363）
//   - BMP：解码后转 JPEG
//   - WEBP：无法解码，返回错误（调用方应丢弃该段而不是发原始字节）
//   - 未知：先用 DecodeConfig 拦尺寸，标准库能解就转 JPEG
//
// 返回错误时调用方必须丢弃图片，**不能退回原始字节** ——
// 这正是「给纯文本模型发 image_url 会 400」同类问题的变体。
func Normalize(data []byte) (Result, error) {
	if len(data) == 0 {
		return Result{}, fmt.Errorf("图片数据为空")
	}

	format := Sniff(data)

	// 三种标准库格式在直通/转首帧之前，都先只读头校验
	// （audit 11 P2-11/P2-12）：魔数对但内容截断的图、
	// 声明超大尺寸的图都必须在这里拒绝 —— 绝不把坏字节发出去。
	if format == FormatJPEG || format == FormatPNG || format == FormatGIF {
		if err := checkDecodable(data); err != nil {
			return Result{Format: format}, err
		}
	}

	if isModelFriendly(format) {
		return Result{
			Binary:   data,
			MimeType: mimeOf(format),
			Format:   format,
		}, nil
	}

	switch format {
	case FormatGIF:
		// gif.Decode 只解第一帧，正好是参考实现要的行为。
		img, err := gif.Decode(bytes.NewReader(data))
		if err != nil {
			return Result{Format: format}, fmt.Errorf("解码 GIF 首帧失败: %w", err)
		}
		out, err := encodePNG(flattenAlpha(img))
		if err != nil {
			return Result{Format: format}, fmt.Errorf("GIF 首帧转 PNG 失败: %w", err)
		}
		return Result{Binary: out, MimeType: "image/png", Converted: true, Format: format}, nil

	case FormatBMP:
		img, err := decodeBMP(data)
		if err != nil {
			return Result{Format: format}, fmt.Errorf("解码 BMP 失败: %w", err)
		}
		out, err := encodeJPEG(img)
		if err != nil {
			return Result{Format: format}, fmt.Errorf("BMP 转 JPEG 失败: %w", err)
		}
		return Result{Binary: out, MimeType: "image/jpeg", Converted: true, Format: format}, nil

	case FormatWEBP:
		// 标准库没有 WEBP 解码器。不引入新依赖，所以只能明确拒绝。
		// 返回带格式信息的错误，让日志能说清「是 WEBP 导致的」而不是笼统的失败。
		return Result{Format: format}, fmt.Errorf("WEBP 需要额外的解码库，标准库不支持")

	default:
		// 兜底：试着让标准库解一次，能解就转 JPEG。
		// 这条路径覆盖「魔数没认出来但实际是合法图片」的情况。
		// 先 DecodeConfig 只读头拦住超大尺寸（P2-12），再全量解码。
		cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return Result{Format: format}, fmt.Errorf("无法识别的图片格式")
		}
		if err := checkDimensions(cfg.Width, cfg.Height); err != nil {
			return Result{Format: format}, err
		}
		img, _, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			return Result{Format: format}, fmt.Errorf("无法识别的图片格式")
		}
		out, err := encodeJPEG(img)
		if err != nil {
			return Result{Format: format}, fmt.Errorf("转 JPEG 失败: %w", err)
		}
		return Result{Binary: out, MimeType: "image/jpeg", Converted: true, Format: format}, nil
	}
}

// checkDecodable 只读文件头（image.DecodeConfig）做两项校验：
//
//  1. 数据真的可解码 —— 防「魔数对但内容截断/损坏」（audit 11 P2-11）。
//     以前 16 字节的假 PNG（魔数 + 半个 IHDR）会被原样发给模型。
//  2. 头声明的尺寸不超上限 —— image.Decode 按声明尺寸分配内存，
//     必须在解码之前拦截（audit 11 P2-12）。
//
// 只读头、不解像素，代价很小，对 JPEG/PNG/GIF 统一适用。
// BMP 走自己的解码器，自带长度与尺寸校验，不经这里。
func checkDecodable(data []byte) error {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("图片头无法解析（可能已损坏或被截断）: %w", err)
	}
	return checkDimensions(cfg.Width, cfg.Height)
}

// checkDimensions 按上限校验头声明的尺寸。
func checkDimensions(w, h int) error {
	if w <= 0 || h <= 0 {
		return fmt.Errorf("非法图片尺寸 %dx%d", w, h)
	}
	if w > maxImageSide || h > maxImageSide {
		return fmt.Errorf("图片单边尺寸 %dx%d 超过上限 %d", w, h, maxImageSide)
	}
	if w*h > maxImagePixels {
		return fmt.Errorf("图片总像素 %d（%dx%d）超过上限 %d", w*h, w, h, maxImagePixels)
	}
	return nil
}

// mimeOf 返回格式对应的 MIME。
func mimeOf(f Format) string {
	switch f {
	case FormatJPEG:
		return "image/jpeg"
	case FormatPNG:
		return "image/png"
	case FormatGIF:
		return "image/gif"
	case FormatBMP:
		return "image/bmp"
	case FormatWEBP:
		return "image/webp"
	default:
		return "application/octet-stream"
	}
}

// encodeJPEG 把任意 image.Image 编码成 JPEG。
//
// 画质选 90：视觉模型只需要看清内容，90 在体积与清晰度之间比较平衡。
// 带透明通道的图先铺白底 —— 直接转 JPEG 会把透明区域变成黑色，
// 而表情包大量是透明底的 PNG。
func encodeJPEG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, flattenAlpha(img), &jpeg.Options{Quality: 90}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// encodePNG 把任意 image.Image 编码成 PNG（无损，GIF 首帧走这里）。
func encodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// flattenAlpha 把任意图片按标准 alpha 合成铺到白底上。
//
// 为什么不再按类型分支判断「有没有 alpha」（audit 11 P2-8/P2-9）：
//  - 旧实现把 At().RGBA() 的**预乘**值又乘了一次 alpha，半透明像素偏暗；
//  - 旧实现的类型 switch 只认 NRGBA/RGBA 系，Paletted（绝大多数表情包）
//    被跳过，透明区域变黑。
// 现在无条件走标准 draw 合成，对 Paletted/NRGBA/RGBA/Gray 全类型通用，
// 是唯一不会漏类型的写法：
//  1. 先用 draw.Src 铺纯白底；
//  2. 再用 draw.Over 把原图合成上去（标准 alpha 合成，只乘一次）。
//
// 与参考实现一致：utils_image.py:178-185 的
// background.paste(alpha_image, mask=alpha_image.getchannel("A"))
// 做的就是同一种标准合成。
func flattenAlpha(img image.Image) image.Image {
	b := img.Bounds()
	if b.Empty() {
		return img
	}

	dst := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Rect, image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(dst, dst.Rect, img, b.Min, draw.Over)
	return dst
}

// decodeBMP 解析未压缩的 BMP。
//
// 只支持最常见的两种：24 位（BGR）与 32 位（BGRA）。
// 不支持 1/4/8 位调色板与 RLE 压缩 —— QQ 用户发的截图基本都是 24/32 位，
// 为了罕见格式写一堆分支不值得（AGENTS.md：不要提前为「以后可能要」买单）。
func decodeBMP(data []byte) (image.Image, error) {
	if len(data) < 54 {
		return nil, fmt.Errorf("数据长度 %d 不足 BMP 头", len(data))
	}
	if data[0] != 'B' || data[1] != 'M' {
		return nil, fmt.Errorf("缺少 BM 魔数")
	}

	le := binary.LittleEndian
	dataOffset := int(le.Uint32(data[10:14]))
	dibSize := int(le.Uint32(data[14:18]))
	if dibSize < 40 {
		return nil, fmt.Errorf("不支持的 DIB 头长度 %d", dibSize)
	}
	if len(data) < 14+dibSize {
		return nil, fmt.Errorf("DIB 头被截断")
	}

	width := int(int32(le.Uint32(data[18:22])))
	rawHeight := int32(le.Uint32(data[22:26]))
	bitCount := int(le.Uint16(data[28:30]))
	compression := int(le.Uint32(data[30:34]))

	if compression != 0 {
		// BI_RGB = 0。RLE 等压缩格式不支持。
		return nil, fmt.Errorf("不支持的 BMP 压缩方式 %d（只支持未压缩）", compression)
	}
	if bitCount != 24 && bitCount != 32 {
		return nil, fmt.Errorf("不支持的位深 %d（只支持 24/32 位）", bitCount)
	}
	if width <= 0 || rawHeight == 0 {
		return nil, fmt.Errorf("非法的 BMP 尺寸 %dx%d", width, rawHeight)
	}

	// 高度为负表示自上而下存储（top-down），这是合法写法。
	topDown := rawHeight < 0
	height := int(rawHeight)
	if topDown {
		height = -height
	}
	if width > 1<<16 || height > 1<<16 {
		return nil, fmt.Errorf("BMP 尺寸过大 %dx%d", width, height)
	}

	bytesPerPixel := bitCount / 8
	// 每行按 4 字节对齐，这是 BMP 规范里最容易算错的地方。
	rowSize := ((bitCount*width + 31) / 32) * 4
	if dataOffset < 0 || dataOffset+rowSize*height > len(data) {
		return nil, fmt.Errorf("BMP 像素数据被截断（需要 %d 字节，实际 %d）",
			dataOffset+rowSize*height, len(data))
	}

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		// 行序：默认是自下而上；top-down 时直接对应。
		srcY := height - 1 - y
		if topDown {
			srcY = y
		}
		rowStart := dataOffset + srcY*rowSize
		for x := 0; x < width; x++ {
			p := rowStart + x*bytesPerPixel
			// BMP 的像素顺序是 BGR(A)，不是 RGB(A)。
			b := data[p]
			g := data[p+1]
			r := data[p+2]
			a := uint8(255)
			if bytesPerPixel == 4 {
				a = data[p+3]
				// 部分 32 位 BMP 的 alpha 通道是垃圾值（写成 0 但实际不透明）。
				// 全 0 时按不透明处理，否则整张图会变全黑。
				if a == 0 {
					a = 255
				}
			}
			img.Set(x, y, color.RGBA{R: r, G: g, B: b, A: a})
		}
	}
	return img, nil
}

// DecodeAny 尽力解码任意可识别格式，主要用于测试与调试。
//
// GIF 依赖生产代码 import image/gif（gif.Decode）注册的解码器；
// 测试包刻意不 import image/gif，防止「包自身没注册」被测试
// 的 import 副作用藏住（audit 11 P2-10）。
func DecodeAny(data []byte) (image.Image, Format, error) {
	format := Sniff(data)
	if format == FormatBMP {
		img, err := decodeBMP(data)
		return img, format, err
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	return img, format, err
}

// SupportedMimeTypes 返回**可原样直通**的 MIME 列表，供调用方做快速判断。
//
// 注意不含 image/gif：GIF 必须先经 Normalize 转成首帧 PNG 才能发
// （部分 OpenAI 兼容接口拒绝动画 GIF，见 isModelFriendly 的注释）。
func SupportedMimeTypes() []string {
	return []string{"image/jpeg", "image/png"}
}

// IsSupportedMime 判断 MIME 是否可直接发给模型。
func IsSupportedMime(mime string) bool {
	mime = strings.ToLower(strings.TrimSpace(mime))
	for _, s := range SupportedMimeTypes() {
		if mime == s {
			return true
		}
	}
	return false
}
