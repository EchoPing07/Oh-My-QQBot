// Package emoji 负责表情包的收集、打标签与选图。
//
// 对应 MaiBot 的 src/emoji_system/emoji_manager.py，但砍掉了几块：
//   - 没有拼图给视觉模型选（不做子代理，见 AGENTS.md）
//   - 没有定期清理与淘汰策略（库不大时无意义）
//   - 没有「替换已有表情」的 LLM 决策
//
// 保留的三件事是真正有用的：收到就收藏、用 VLM 打情绪标签、按标签检索。
package emoji

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/llm"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/logx"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/store"
)

// Manager 管理表情包库。
type Manager struct {
	cfg   *config.Store
	store *store.DB
	llm   *llm.Client
	log   *logx.Logger

	// 后台打标签任务的并发闸门。
	// VLM 调用很贵，同一时刻只跑一个，排队由 channel 承担。
	jobs chan struct{}
	wg   sync.WaitGroup
}

// New 创建管理器。
func New(cfg *config.Store, db *store.DB, client *llm.Client) *Manager {
	m := &Manager{
		cfg:   cfg,
		store: db,
		llm:   client,
		log:   logx.Get("emoji"),
		jobs:  make(chan struct{}, 1),
	}
	return m
}

// Close 等待后台任务收尾。
func (m *Manager) Close() { m.wg.Wait() }

// Collect 尝试把一个收到的表情包收进库。
//
// 对应参考实现的 ensure_emoji_saved + register_emoji_to_db。
//
// 返回 true 表示这次确实新增了一条。以下情况静默跳过（返回 false, nil）：
//   - 功能关闭
//   - 二进制为空或太大
//   - 库里已有同样的内容（按哈希去重）
//   - 库已满
//
// 只有真正写盘/写库失败才返回错误。
func (m *Manager) Collect(ctx context.Context, binary []byte, mimeType, sessionID, userID string) (bool, error) {
	if !m.cfg.Get().Emoji.Enabled || !m.cfg.Get().Emoji.CollectEnabled {
		return false, nil
	}
	if len(binary) == 0 {
		return false, nil
	}

	maxBytes := m.cfg.Get().Emoji.MaxSizeKB * 1024
	if maxBytes > 0 && len(binary) > maxBytes {
		return false, nil
	}

	hash := hashBytes(binary)
	if existing, err := m.store.Emojis().GetByHash(ctx, hash); err != nil {
		return false, fmt.Errorf("查询表情包失败: %w", err)
	} else if existing != nil {
		return false, nil
	}

	count, err := m.store.Emojis().Count(ctx)
	if err != nil {
		return false, fmt.Errorf("统计表情包数量失败: %w", err)
	}
	if max := m.cfg.Get().Emoji.MaxCount; max > 0 && count >= int64(max) {
		m.log.Debug("表情包库已满，不再收藏", "count", count, "max", max)
		return false, nil
	}

	// 落盘。目录按哈希前两位分桶，避免单目录下堆几万个文件。
	ext := extensionFor(mimeType)
	relPath := filepath.Join(m.cfg.Get().Emoji.Dir, hash[:2], hash+ext)
	absPath := relPath
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return false, fmt.Errorf("创建表情包目录失败: %w", err)
	}
	if err := os.WriteFile(absPath, binary, 0o644); err != nil {
		return false, fmt.Errorf("写入表情包文件失败: %w", err)
	}

	record := &store.Emoji{
		FileHash:      hash,
		FilePath:      relPath,
		SourceSession: sessionID,
		SourceUser:    userID,
		CreatedAt:     time.Now().Unix(),
	}
	created, err := m.store.Emojis().Save(ctx, record)
	if err != nil {
		// 写库失败时把刚落的盘也清掉，避免留下孤儿文件。
		_ = os.Remove(absPath)
		return false, fmt.Errorf("保存表情包记录失败: %w", err)
	}
	if !created {
		// 并发情况下别人先写进去了，删掉自己这份。
		_ = os.Remove(absPath)
		return false, nil
	}

	m.log.Info("已收藏表情包", "hash", hash[:12], "size", len(binary), "user", userID)

	// 打标签是异步的：VLM 调用慢，不能拖住消息处理。
	m.describeAsync(record.ID, binary, mimeType)

	return true, nil
}

// describeAsync 在后台用 VLM 生成情绪标签。
func (m *Manager) describeAsync(id int64, binary []byte, mimeType string) {
	// 没有配置视觉模型就不用排队了，描述留空。
	// 留空的表情包仍然可以被 Random 选中发送，只是没法按情绪检索。
	if len(m.cfg.Get().ResolveModels(config.TaskVLM)) == 0 {
		return
	}

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()

		select {
		case m.jobs <- struct{}{}:
			defer func() { <-m.jobs }()
		case <-time.After(10 * time.Second):
			// 排队太久就放弃：表情包描述不是关键路径，
			// 积压一长串 VLM 调用只会拖慢整个系统。
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()

		desc, err := m.describe(ctx, binary, mimeType)
		if err != nil {
			m.log.Warn("生成表情包描述失败", "id", id, "err", err)
			return
		}
		if desc == "" {
			return
		}
		if err := m.store.Emojis().UpdateDescription(ctx, id, desc); err != nil {
			m.log.Warn("写入表情包描述失败", "id", id, "err", err)
			return
		}
		m.log.Debug("表情包已打标签", "id", id, "desc", desc)
	}()
}

// describe 调 VLM 生成情绪标签。
//
// 提示词逐字照抄 refs/MaiBot/prompts/zh-CN/emoji_content_analysis.prompt。
// 参考实现还会先把 GIF 抽帧拼图，这里不做 —— 直接发原图，
// 大多数 VLM 能直接处理动图首帧。
func (m *Manager) describe(ctx context.Context, binary []byte, mimeType string) (string, error) {
	imageType := "STATIC"
	if strings.Contains(strings.ToLower(mimeType), "gif") {
		imageType = "GIF"
	}

	promptText := renderPrompt(promptNameAnalysis, map[string]string{
		"image_type": imageType,
	})

	mime := strings.TrimSpace(mimeType)
	if mime == "" {
		mime = "image/png"
	}
	images := []string{"data:" + mime + ";base64," + base64Encode(binary)}

	resp, err := m.llm.Generate(ctx, config.TaskVLM, &llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: promptText, Images: images},
		},
	})
	if err != nil {
		return "", err
	}

	// 只取第一行、压掉空白：模型经常多输出解释。
	text := strings.TrimSpace(resp.Content)
	if idx := strings.IndexByte(text, '\n'); idx >= 0 {
		text = strings.TrimSpace(text[:idx])
	}
	if len(text) > 120 {
		text = text[:120]
	}
	return text, nil
}

// 表情包打标签的提示词在 internal/prompt/embedded/zh-CN/
// emoji_content_analysis.prompt（可在 WebUI 里改），默认内容逐字照抄
// refs/MaiBot/prompts/zh-CN/emoji_content_analysis.prompt。
// hashBytes 返回内容哈希。
func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// extensionFor 按 MIME 猜扩展名。
func extensionFor(mimeType string) string {
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/bmp":
		return ".bmp"
	default:
		return ".png"
	}
}

// base64Encode 是 base64 编码的短别名，避免主文件顶部 import 过多。
func base64Encode(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}
