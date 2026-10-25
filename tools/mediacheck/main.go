// mediacheck 验证「消息段 hash → /api/media → 显示图片」这条链路。
//
// 为什么不写成单元测试：这条链路跨 qq(采集 hash) → store(落库) →
// web(回查 URL) → 浏览器(<img>) 四层，任何一层漏字段都表现为
// 「表情包显示成 [表情] 文字」，单测里逐层断言会写得又长又碎。
// 这里直接对真实库跑一遍，一眼看出哪一层断了。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/store"
)

func main() {
	dbPath := flag.String("db", "data/oh-my-qqbot.db", "数据库路径")
	flag.Parse()

	ctx := context.Background()
	db, err := store.Open(ctx, *dbPath)
	if err != nil {
		fmt.Println("打开数据库失败:", err)
		os.Exit(1)
	}
	defer db.Close()

	// 1) 段里的 hash 能不能落库并读回。
	hash, err := pickEmojiHash(ctx, db)
	if err != nil {
		fmt.Println("读取表情包库失败:", err)
		os.Exit(1)
	}
	if hash == "" {
		fmt.Println("表情包库是空的，先让机器人收几个表情包再来")
		os.Exit(1)
	}
	fmt.Println("取到的表情哈希:", hash)

	msgID := fmt.Sprintf("mediacheck-%d", time.Now().UnixNano())
	m := &domain.Message{
		ID: msgID, SessionID: "mediacheck", MessageID: msgID,
		Platform: "qq", UserID: "check", UserName: "检查",
		Segments: []domain.Segment{{
			Type: domain.SegmentEmoji, MimeType: "image/jpeg", Hash: hash,
		}},
		PlainText:  "[表情包]",
		ReceivedAt: time.Now(),
	}
	if err := db.Messages().Save(ctx, m); err != nil {
		fmt.Println("落库失败:", err)
		os.Exit(1)
	}
	got, err := db.Messages().ByMessageID(ctx, msgID)
	if err != nil || got == nil {
		fmt.Println("读回失败:", err)
		os.Exit(1)
	}
	raw, _ := json.Marshal(got.Segments)
	fmt.Println("落库并读回的段:", string(raw))

	if len(got.Segments) == 0 || got.Segments[0].Hash != hash {
		fmt.Println("❌ hash 没有落库 —— WebUI 会显示 [表情] 占位")
		os.Exit(1)
	}

	// 2) 库外查询：这个 hash 在 emoji 库里有文件吗。
	existing, err := db.Emojis().ExistingHashes(ctx, []string{hash})
	if err != nil {
		fmt.Println("查询失败:", err)
		os.Exit(1)
	}
	if !existing[hash] {
		fmt.Println("❌ 库里查不到这个 hash —— /api/media 会 404")
		os.Exit(1)
	}
	fmt.Println("✅ 链路通：段带 hash → 库里有文件 → /api/media?hash=" + hash[:12] + "… 能返回图片")

	fmt.Printf("\n浏览器里验证：登录后打开 /api/media?hash=%s\n", hash)

	// 清理刚才造的临时消息，别污染会话列表。
	// 直接用 SQL：这是调试工具，不值得为它往 store 里加一个
	// 只有工具会用的删除方法（AGENTS.md 第 56 条）。
	if _, err := db.SQL().ExecContext(ctx, `DELETE FROM messages WHERE id = ?`, msgID); err != nil {
		fmt.Println("清理临时消息失败（不影响结论）:", err)
	}
}

// pickEmojiHash 从表情包库里随便取一个带文件的哈希。
func pickEmojiHash(ctx context.Context, db *store.DB) (string, error) {
	list, err := db.Emojis().Random(ctx, 1)
	if err != nil || len(list) == 0 {
		return "", err
	}
	return list[0].FileHash, nil
}
