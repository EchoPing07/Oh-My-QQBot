package qq

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
)

// 入站附件下载。
//
// 为什么必须即时下载：QQ 给的 attachments[].url 是带签名的临时地址，
// 有效期很短，且过期后平台不保证还能申请到新的。
// 如果只把 URL 存下来晚点再取，大概率拿到 403。
//
// 参考实现：refs/MaiBot-QQ-Adapter/plugin.py 的 _download_attachment / _validate_media_url。

const (
	// maxInboundAttachments 是单条消息最多处理的附件数，与插件一致。
	maxInboundAttachments = 10

	// maxInboundMediaBytes 是单个附件的上限，与插件一致（20 MiB）。
	maxInboundMediaBytes = 20 * 1024 * 1024

	// inboundMediaTimeout 是单次下载超时。
	// 图片通常几百 KB，给 20 秒足够；太长会拖住网关读取循环。
	inboundMediaTimeout = 20 * time.Second
)

// 受信任的 QQ 媒体域名。
//
// 为什么要白名单：入站 URL 来自外部消息，如果模型或注入方能让它指向内网地址，
// 就会变成一次 SSRF。参考实现（plugin.py:_validate_media_url）也是这么做的。
var (
	trustedMediaHosts = map[string]struct{}{
		"grouppro.grouppro.qq.com": {},
	}
	trustedMediaSuffixes = []string{
		"nt.qq.com",
		"nt.qq.com.cn",
	}
)

// mediaHTTPClient 专用于下载入站附件。
//
// 不复用 Client.http（那个带 Authorization 头且超时 30 秒）：
// 附件下载不该带 QQ 的 access token，也不该发到非 QQ 域名上。
var mediaHTTPClient = &http.Client{
	Timeout: inboundMediaTimeout,
	// 不跟随重定向：受信任域名不该往回跳，跳了就说明有问题。
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// validateMediaURL 检查附件地址是否可以安全下载。
func validateMediaURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("附件地址为空")
	}
	if len(raw) > 4096 {
		return fmt.Errorf("附件地址过长")
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("附件地址包含控制字符")
		}
	}

	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("附件地址格式无效: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("附件地址必须是 https，实际 %q", u.Scheme)
	}
	if u.User != nil {
		return fmt.Errorf("附件地址不能包含用户凭据")
	}
	if port := u.Port(); port != "" && port != "443" {
		return fmt.Errorf("附件地址端口必须是 443，实际 %q", port)
	}

	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" {
		return fmt.Errorf("附件地址缺少主机名")
	}
	if _, ok := trustedMediaHosts[host]; ok {
		return nil
	}
	for _, suffix := range trustedMediaSuffixes {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return nil
		}
	}
	return fmt.Errorf("附件地址不属于受信任的 QQ 媒体域名: %q", host)
}

// downloadMedia 下载一个入站附件。
//
// 失败返回 error，由调用方决定降级方式（保留占位段即可，不要让整条消息丢失）。
func downloadMedia(ctx context.Context, rawURL string) ([]byte, error) {
	if err := validateMediaURL(rawURL); err != nil {
		return nil, err
	}

	dctx, cancel := context.WithTimeout(ctx, inboundMediaTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(dctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := mediaHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("下载附件失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, fmt.Errorf("附件地址返回重定向: HTTP %d", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("附件下载失败: HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxInboundMediaBytes {
		return nil, fmt.Errorf("附件大小 %d 超过上限 %d", resp.ContentLength, maxInboundMediaBytes)
	}

	// 多读 1 字节，用来判断是否超限 —— 只信 Content-Length 不够，
	// 服务端可以不返回它，也可以给个假值。
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxInboundMediaBytes+1))
	if err != nil {
		return nil, fmt.Errorf("读取附件失败: %w", err)
	}
	if len(data) > maxInboundMediaBytes {
		return nil, fmt.Errorf("附件超过 %d 字节上限", maxInboundMediaBytes)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("附件内容为空")
	}
	return data, nil
}

// hydrateInboundMedia 下载消息里的图片/表情段，把二进制填进去。
//
// 为什么要填二进制而不是留 URL：
//   - URL 会过期，后面 attach_pic 想复用就取不到了
//   - 多模态请求需要 base64，本地有二进制才发得出去
//
// 下载失败的段保留 URL、丢掉二进制，正文里的占位仍然成立，
// 所以调用方不需要区分成功与失败。
func hydrateInboundMedia(ctx context.Context, msg *domain.Message, log logger) {
	if msg == nil || len(msg.Segments) == 0 {
		return
	}

	for i := range msg.Segments {
		seg := &msg.Segments[i]
		if seg.Type != domain.SegmentImage && seg.Type != domain.SegmentEmoji {
			continue
		}
		if len(seg.Binary) > 0 || seg.URL == "" {
			continue
		}

		data, err := downloadMedia(ctx, seg.URL)
		if err != nil {
			log.Warn("入站图片下载失败，保留占位",
				"msg_id", msg.MessageID,
				"kind", string(seg.Type),
				"host", safeURLHost(seg.URL),
				"err", err,
			)
			// URL 不留在内存里常驻：它有时效，留着反而误导后续逻辑。
			seg.URL = ""
			continue
		}
		seg.Binary = data
		normalizeSegmentMime(seg, data)
		// 同时记下内容哈希：二进制不落库，但 WebUI 要靠这个 hash
		// 回查 data/emoji 里的文件，才能把图片/表情直接显示出来
		// 而不是一个「[表情]」占位字。
		//
		// 必须用 sha256，与 emoji.Manager.Collect 落盘用的哈希一致，
		// 否则算出来的 key 对不上库里记录。
		seg.Hash = sha256Hex(data)
		// 下载成功也清掉 URL：后续链路一律用二进制，避免有人误用过期地址。
		seg.URL = ""
	}

	// 多模态用：把图片二进制挂到 Message.Images 之外的路径由 llm 层负责，
	// 这里只需要保证 Segments 里的 Binary 是齐的。
}

// sha256Hex 返回内容的 sha256 十六进制串。
//
// 与 emoji.Manager 落盘用的哈希必须完全一致：
// 那里是 hashBytes() 算 sha256 后 hex，这边如果换个算法（比如 sha1），
// 算出来的 key 就永远对不上库里的 file_hash，图片显示不出来。
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// normalizeSegmentMime 按文件头修正消息段的 MIME。
//
// 为什么必须覆盖而不是只补空值：QQ 的 attachments[].content_type
// 经常是 application/octet-stream，或者与实际内容不符。
// 只补空值的后果是表情包落盘时用了错的扩展名（全变成 .png），
// 之后 WebUI 预览与 VLM 上传都会对不上，而且不报任何错误。
//
// 嗅探不出具体类型时（application/octet-stream）保留原声明：
// 至少比一个「啥也不是」的类型有用。
func normalizeSegmentMime(seg *domain.Segment, data []byte) {
	if seg == nil || len(data) == 0 {
		return
	}
	if detected := http.DetectContentType(data); detected != "" && detected != "application/octet-stream" {
		seg.MimeType = detected
	}
}

// logger 是 hydrateInboundMedia 需要的最小日志能力。
// 用接口而不是直接依赖 logx，测试里能塞个空实现。
type logger interface {
	Warn(msg string, kv ...any)
}

// safeURLHost 提取主机名用于日志，不回显查询参数（签名在 query 里）。
func safeURLHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<无效>"
	}
	return u.Hostname()
}
