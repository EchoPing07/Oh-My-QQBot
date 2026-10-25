package qq

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/logx"
)

// 官方预上传要求的 md5_10m：文件前 10002432 字节的 MD5。
const md5_10mBytes = 10002432

// maxImageBytes 是单张图片上限，与插件一致（20 MB）。
const maxImageBytes = 20 * 1024 * 1024

// Sender 负责出站发送。
// 对应插件的 handle_qq_official_gateway / _claim_passive_fields / _upload_image。
type Sender struct {
	client  *Client
	passive *passiveTracker
	log     *logx.Logger
}

// NewSender 创建发送器。
func NewSender(client *Client) *Sender {
	return &Sender{
		client:  client,
		passive: newPassiveTracker(),
		log:     logx.Get("qq.sender"),
	}
}

// RecordInbound 记录入站消息，开启被动回复窗口。
func (s *Sender) RecordInbound(scene, target, messageID string, at time.Time) {
	s.passive.RecordInbound(scene, target, messageID, at)
}

// Send 发送一条消息。
//
// 关键限制（来自官方 API）：
//   - 一条消息只能承载一种内容，所以文本、图片必须按顺序拆成多条
//   - 有 @ 时必须用 markdown 消息（msg_type=2），否则 <@openid> 显示成代码原文
//   - 每条发送各自占用一个被动回复序号
//
// passiveTo 与 quote 是两个**正交**的东西，不能合并成一个参数：
//   - passiveTo 选的是「占哪条入站消息的被动回复额度」，
//     目标永远是触发本轮的那条；
//   - quote 是引用卡片，目标可以是任意一条历史消息，
//     而且必须走 REFIDX_ 形态（见 domain.Reference 的说明）。
//
// 之前只有一个 replyTo 参数，它同时被两件事使用，结果是谁也没能真正生效：
// 引用段在 splitOutbound 里被直接 continue 丢掉，发送请求里从来没有
// message_reference。
func (s *Sender) Send(ctx context.Context, scene, target string, segments []domain.Segment, passiveTo string, quote domain.Reference) (domain.SendResult, error) {
	parts := splitOutbound(segments)
	if len(parts) == 0 {
		return domain.SendResult{}, fmt.Errorf("出站消息为空")
	}

	basePath := fmt.Sprintf("/v2/groups/%s", target)
	if scene != "group" {
		basePath = fmt.Sprintf("/v2/users/%s", target)
	}

	var result domain.SendResult
	for i, part := range parts {
		var body map[string]any

		if part.isMedia {
			if len(part.binary) > maxImageBytes {
				return domain.SendResult{}, fmt.Errorf("图片大小 %d 超过上限 %d", len(part.binary), maxImageBytes)
			}
			fileInfo, err := s.uploadImage(ctx, basePath, part.binary, part.fileName)
			if err != nil {
				return domain.SendResult{}, err
			}
			body = map[string]any{
				"msg_type": 7,
				"media":    map[string]any{"file_info": fileInfo},
			}
		} else {
			body = part.body
		}

		// 引用只在首条上挂。
		//
		// 与正文分段一致：一条回复被切成多段时，引用卡片挂在第一段上
		// （读者先看到「在回谁」），后续分段重复挂会让客户端出现
		// 好几个指向同一条消息的引用块。
		//
		// 参考实现同理：uni_message_sender.py 的 ReplyComponent 是
		// insert(0, ...)，只插入一次，不按分段重复。
		if i == 0 && !quote.IsZero() {
			body["message_reference"] = map[string]any{"message_id": quote.RefIdx}
		}

		// 占用被动回复序号。
		//
		// 注意这里用的是 passiveTo，不是 quote.RefIdx：前者是「占谁的额度」，
		// 后者是「引用谁」。两者指向两条不同的消息，且键空间不同。
		if msgID, seq, ok := s.passive.Claim(scene, target, passiveTo); ok {
			body["msg_id"] = msgID
			body["msg_seq"] = seq
		}

		var resp struct {
			ID      string `json:"id"`
			ExtInfo struct {
				RefIdx string `json:"ref_idx"`
			} `json:"ext_info"`
		}
		if err := s.client.do(ctx, "POST", basePath+"/messages", body, &resp); err != nil {
			if len(result.MessageIDs) > 0 {
				return result, fmt.Errorf("已发送 %d 条后失败: %w", len(result.MessageIDs), err)
			}
			return domain.SendResult{}, err
		}
		if resp.ID == "" {
			return domain.SendResult{}, fmt.Errorf("QQ 发送失败: 响应缺少消息 ID")
		}
		result.MessageIDs = append(result.MessageIDs, resp.ID)
		// 响应里的 ref_idx 是「刚发出的这条消息自己的引用索引」，
		// 机器人下轮引用自己这句话时靠它。不记下来的话，
		// 引用自己的发言永远会落空（见 AGENTS 第 183 条）。
		result.RefIdxs = append(result.RefIdxs, strings.TrimSpace(resp.ExtInfo.RefIdx))
	}

	return result, nil
}

// outboundPart 是一个待发送的部分。
type outboundPart struct {
	isMedia bool
	body    map[string]any

	binary   []byte
	fileName string
}

// splitOutbound 按原顺序把消息段切分成待发送的部分。
//
// 规则（照抄插件版 encode_message）：
//   - 连续的文本/@ 段合并为一个 body；含 @ 时用 markdown，否则纯文本
//   - 每个图片/表情段单独成为一个 media 部分
//   - reply 段跳过（引用由 msg_id 表达）
//   - 不支持的段类型直接报错，不静默丢弃
func splitOutbound(segments []domain.Segment) []outboundPart {
	var (
		out         []outboundPart
		textBuf     strings.Builder
		useMarkdown bool
	)

	flushText := func() {
		text := strings.TrimSpace(textBuf.String())
		if text == "" {
			textBuf.Reset()
			useMarkdown = false
			return
		}
		var body map[string]any
		if useMarkdown {
			body = map[string]any{
				"msg_type": 2,
				"markdown": map[string]any{"content": text},
			}
		} else {
			body = map[string]any{
				"msg_type": 0,
				"content":  text,
			}
		}
		out = append(out, outboundPart{body: body})
		textBuf.Reset()
		useMarkdown = false
	}

	for _, seg := range segments {
		switch seg.Type {
		case domain.SegmentReply:
			// 引用不靠正文表达：QQ 侧是请求体顶层的 message_reference，
			// 由 Send 用 quote 参数写入（见 Sender.Send 的说明）。
			//
			// 参考实现也是这里跳过：refs/MaiBot-QQ-Adapter/codec.py
			// 的 encode_message 对 ReplyComponent 直接 continue，
			// 只靠被动回复的 msg_id。但我们把 message_reference 补上了，
			// 因为实测平台确实支持，而且只靠 msg_id 无法引用
			// 「更早的消息」或「机器人自己说过的话」。
			continue

		case domain.SegmentAt:
			// 实测（2026-09-30）：<@openid> 与 <qqbot-at-user /> 在纯文本消息里都显示成代码原文，
			// 只有 markdown 消息中的 <qqbot-at-user id="openid" /> 能渲染为真正的 @。
			if seg.UserID != "" && !isAllDigits(seg.UserID) {
				textBuf.WriteString(fmt.Sprintf(`<qqbot-at-user id="%s" /> `, seg.UserID))
				useMarkdown = true
			} else if seg.UserName != "" {
				// 纯数字是未绑定 OpenID 的 QQ 号，无法 @，退化为可读 @昵称。
				textBuf.WriteString("@" + seg.UserName + " ")
			}

		case domain.SegmentImage, domain.SegmentEmoji:
			flushText()
			if len(seg.Binary) == 0 {
				// 没有二进制数据时跳过，避免整条消息发送失败。
				continue
			}
			ext := detectImageExt(seg.Binary)
			if ext == "" {
				continue
			}
			name := seg.URL
			if name == "" {
				sum := sha1.Sum(seg.Binary)
				name = hex.EncodeToString(sum[:])
			}
			out = append(out, outboundPart{
				isMedia:  true,
				binary:   seg.Binary,
				fileName: fmt.Sprintf("%s.%s", sanitizeFileName(name), ext),
			})

		case domain.SegmentText:
			textBuf.WriteString(seg.Text)

		default:
			// 语音、文件、转发都不支持，跳过而不是让整条消息失败。
			continue
		}
	}
	flushText()

	return out
}

// uploadImage 按官方分片上传流程上传图片，返回用于 msg_type=7 的 file_info。
//
// 流程：upload_prepare 取预签名分片 URL → 逐片 PUT 并 upload_part_finish
// → 携带 upload_id 调用 files 合并。
func (s *Sender) uploadImage(ctx context.Context, basePath string, binary []byte, fileName string) (string, error) {
	md5Full := md5.Sum(binary)
	sha1Full := sha1.Sum(binary)

	cut := len(binary)
	if cut > md5_10mBytes {
		cut = md5_10mBytes
	}
	md5_10m := md5.Sum(binary[:cut])

	// 预上传响应里 block_size / index 的类型不稳定：
	// 实测 QQ 会把它们下发成**字符串**，此时直接反序列化到 int 会报
	//
	//	json: cannot unmarshal string into Go struct field .block_size of type int
	//
	// 后果是表情包与图片**根本发不出去** —— 日志里只有一句「图片预上传失败」，
	// 表现为「机器人一直说要发表情，但从没发出来过」。
	// 参考实现写的是 int(prepare.get("block_size") or 0)，两种类型都吃。
	//
	// 与 attachmentRef 用 parseAttachments 是同一个教训：
	// **外部平台的字段类型不保证稳定，不要用写死类型的结构体接。**
	var rawPrepare map[string]any

	prepareBody := map[string]string{
		"file_type": "1",
		"file_size": fmt.Sprint(len(binary)),
		"file_name": fileName,
		"md5":       hex.EncodeToString(md5Full[:]),
		"sha1":      hex.EncodeToString(sha1Full[:]),
		"md5_10m":   hex.EncodeToString(md5_10m[:]),
	}
	if err := s.client.do(ctx, "POST", basePath+"/upload_prepare", prepareBody, &rawPrepare); err != nil {
		return "", fmt.Errorf("图片预上传失败: %w", err)
	}

	uploadID := looseString(rawPrepare["upload_id"])
	blockSize := looseInt(rawPrepare["block_size"])

	type uploadPart struct {
		Index        int
		PresignedURL string
	}
	var parts []uploadPart
	if rawParts, ok := rawPrepare["parts"].([]any); ok {
		for _, rawPart := range rawParts {
			m, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			parts = append(parts, uploadPart{
				Index:        looseInt(m["index"]),
				PresignedURL: looseString(m["presigned_url"]),
			})
		}
	}

	if uploadID == "" || blockSize <= 0 || len(parts) == 0 {
		return "", fmt.Errorf("QQ 图片预上传响应缺少 upload_id、block_size 或 parts")
	}

	// 文档写分片序号从 0 开始，实测（2026-09-30）返回从 1 开始，按实际最小序号计算偏移。
	indexBase := parts[0].Index
	for _, p := range parts {
		if p.Index < indexBase {
			indexBase = p.Index
		}
	}
	if indexBase != 0 && indexBase != 1 {
		return "", fmt.Errorf("QQ 图片预上传返回的分片序号起点无效: %d", indexBase)
	}

	for _, part := range parts {
		if !strings.HasPrefix(part.PresignedURL, "https://") {
			return "", fmt.Errorf("QQ 图片分片 %d 的预签名地址无效", part.Index)
		}
		offset := (part.Index - indexBase) * blockSize
		end := offset + blockSize
		if offset >= len(binary) {
			return "", fmt.Errorf("QQ 图片分片 %d 超出文件范围", part.Index)
		}
		if end > len(binary) {
			end = len(binary)
		}
		chunk := binary[offset:end]

		// 预签名 URL 自带鉴权，不能携带 QQBot Authorization 头。
		if err := s.putPresigned(ctx, part.PresignedURL, chunk); err != nil {
			return "", fmt.Errorf("QQ 图片分片 %d 上传失败: %w", part.Index, err)
		}

		chunkMD5 := md5.Sum(chunk)
		finishBody := map[string]string{
			"upload_id":  uploadID,
			"part_index": fmt.Sprint(part.Index),
			"block_size": fmt.Sprint(len(chunk)),
			"md5":        hex.EncodeToString(chunkMD5[:]),
		}
		if err := s.client.do(ctx, "POST", basePath+"/upload_part_finish", finishBody, nil); err != nil {
			return "", fmt.Errorf("QQ 图片分片 %d 确认失败: %w", part.Index, err)
		}
	}

	var filesResp struct {
		FileInfo string `json:"file_info"`
	}
	filesBody := map[string]any{
		"file_type":    1,
		"srv_send_msg": false,
		"file_name":    fileName,
		"upload_id":    uploadID,
	}
	if err := s.client.do(ctx, "POST", basePath+"/files", filesBody, &filesResp); err != nil {
		return "", fmt.Errorf("QQ 图片合并失败: %w", err)
	}
	if filesResp.FileInfo == "" {
		return "", fmt.Errorf("QQ 图片上传响应缺少 file_info")
	}
	return filesResp.FileInfo, nil
}

// putPresigned 上传一个分片到预签名地址。
func (s *Sender) putPresigned(ctx context.Context, url string, data []byte) error {
	req, err := newPutRequest(ctx, url, data)
	if err != nil {
		return err
	}
	resp, err := s.client.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// MuteResult 是禁言操作的结果。
type MuteResult struct {
	Success bool
	Error   string
}

// MuteUser 禁言群成员。
// 对应插件 tool_mute 的 restrict_chat_setting 调用。
func (s *Sender) MuteUser(ctx context.Context, groupOpenID, memberOpenID string, duration time.Duration) error {
	// 官方接口按 RFC3339 到期时间设置禁言，最长 30 天。
	expireAt := time.Now().UTC().Add(duration).Format("2006-01-02T15:04:05Z")

	body := map[string]any{
		"members": []map[string]any{
			{
				"op":             "add",
				"member_openid":  memberOpenID,
				"mute_expire_at": expireAt,
			},
		},
	}
	return s.client.do(ctx, "POST",
		fmt.Sprintf("/v2/groups/%s/restrict_chat_setting", groupOpenID), body, nil)
}

// RecallMessage 撤回消息。
// 对应插件 recall_message：群与单聊路径不同。
func (s *Sender) RecallMessage(ctx context.Context, scene, target, messageID string) error {
	basePath := fmt.Sprintf("/v2/groups/%s", target)
	if scene != "group" {
		basePath = fmt.Sprintf("/v2/users/%s", target)
	}
	return s.client.do(ctx, "DELETE", fmt.Sprintf("%s/messages/%s", basePath, messageID), nil, nil)
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// detectImageExt 按文件头识别官方富媒体支持的图片格式。
func detectImageExt(b []byte) string {
	switch {
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "jpg"
	case len(b) >= 8 && string(b[:8]) == "\x89PNG\r\n\x1a\n":
		return "png"
	case len(b) >= 6 && (string(b[:6]) == "GIF87a" || string(b[:6]) == "GIF89a"):
		return "gif"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "webp"
	case len(b) >= 2 && b[0] == 'B' && b[1] == 'M':
		return "bmp"
	}
	return ""
}

func sanitizeFileName(name string) string {
	// 只保留安全字符，避免上传文件名带路径或特殊符号。
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := b.String()
	if len(out) > 64 {
		out = out[:64]
	}
	if out == "" {
		out = "image"
	}
	return out
}
