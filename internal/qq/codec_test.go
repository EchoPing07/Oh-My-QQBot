package qq

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
)

// TestDecodeGroupAtMessage 验证群 @ 消息解码。
func TestDecodeGroupAtMessage(t *testing.T) {
	payload := `{
		"id": "msg-1",
		"content": " 木木 你好呀 ",
		"group_openid": "group-abc",
		"author": {"member_openid": "user-1", "username": "小明"},
		"timestamp": "2026-10-21T10:00:00Z"
	}`

	got, err := Decode(GroupAtMessageEvent, json.RawMessage(payload), "bot-openid", "木木")
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}

	if got.Scene != "group" {
		t.Errorf("场景 = %q，期望 group", got.Scene)
	}
	if got.TargetID != "group-abc" {
		t.Errorf("目标 = %q，期望 group-abc", got.TargetID)
	}
	if got.Message.MessageID != "msg-1" {
		t.Errorf("消息 ID = %q", got.Message.MessageID)
	}
	// 群 @ 消息天然 is_at。
	if !got.Message.IsAt {
		t.Error("群 @ 消息应标记 is_at")
	}
	if got.Message.GroupID != "group-abc" {
		t.Errorf("群 ID = %q", got.Message.GroupID)
	}
	if got.Message.UserID != "user-1" {
		t.Errorf("用户 ID = %q", got.Message.UserID)
	}
	if got.Message.UserName != "小明" {
		t.Errorf("用户名 = %q", got.Message.UserName)
	}
}

// TestDecodePrivateMessage 验证单聊消息解码。
func TestDecodePrivateMessage(t *testing.T) {
	payload := `{
		"id": "msg-2",
		"content": "在吗",
		"author": {"user_openid": "user-open-2"},
		"timestamp": "2026-10-21T10:00:00Z"
	}`

	got, err := Decode(C2CMessageEvent, json.RawMessage(payload), "bot-openid", "木木")
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}

	if got.Scene != "private" {
		t.Errorf("场景 = %q，期望 private", got.Scene)
	}
	if got.TargetID != "user-open-2" {
		t.Errorf("目标 = %q，期望 user-open-2", got.TargetID)
	}
	if got.Message.IsAt {
		t.Error("单聊消息不应标记 is_at")
	}
	if got.Message.IsGroup {
		t.Error("单聊 is_group 应为 false")
	}
}

// TestDecodeGroupMessageWithoutAt 验证全量群消息未 @ 机器人。
func TestDecodeGroupMessageWithoutAt(t *testing.T) {
	payload := `{
		"id": "msg-3",
		"content": "今天天气不错",
		"group_openid": "group-abc",
		"author": {"member_openid": "user-1", "username": "小明"},
		"timestamp": "2026-10-21T10:00:00Z"
	}`

	got, err := Decode(GroupMessageEvent, json.RawMessage(payload), "bot-openid", "木木")
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}

	if got.Message.IsAt {
		t.Error("全量群消息未 @ 时不应标记 is_at")
	}
}

// TestDecodeGroupMessageWithMention 验证全量群消息通过 mentions 检测 @。
func TestDecodeGroupMessageWithMention(t *testing.T) {
	payload := `{
		"id": "msg-4",
		"content": "你好",
		"group_openid": "group-abc",
		"author": {"member_openid": "user-1", "username": "小明"},
		"mentions": [{"id": "bot-openid", "username": "木木"}],
		"timestamp": "2026-10-21T10:00:00Z"
	}`

	got, err := Decode(GroupMessageEvent, json.RawMessage(payload), "bot-openid", "木木")
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}

	if !got.Message.IsAt {
		t.Error("mentions 里有机器人时应标记 is_at")
	}
}

// TestDecodeMentionToken 验证 <@id> 协议标记转写。
//
// 这是原版一个关键处理：OpenID 原文不能进入聊天流，要转成可读文本。
func TestDecodeMentionToken(t *testing.T) {
	payload := `{
		"id": "msg-5",
		"content": "<@!bot-openid> 你好 <@other-openid>",
		"group_openid": "group-abc",
		"author": {"member_openid": "user-1", "username": "小明"},
		"mentions": [
			{"id": "bot-openid", "username": "木木"},
			{"id": "other-openid", "username": "小红"}
		],
		"timestamp": "2026-10-21T10:00:00Z"
	}`

	got, err := Decode(GroupMessageEvent, json.RawMessage(payload), "bot-openid", "木木")
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}

	// OpenID 原文不应出现在纯文本里。
	if strings.Contains(got.Message.PlainText, "bot-openid") {
		t.Errorf("纯文本里残留 OpenID: %q", got.Message.PlainText)
	}
	if strings.Contains(got.Message.PlainText, "other-openid") {
		t.Errorf("纯文本里残留 OpenID: %q", got.Message.PlainText)
	}
	// 应转成可读的 @名字。
	if !strings.Contains(got.Message.PlainText, "@木木") {
		t.Errorf("纯文本 = %q，应含 @木木", got.Message.PlainText)
	}
	if !strings.Contains(got.Message.PlainText, "@小红") {
		t.Errorf("纯文本 = %q，应含 @小红", got.Message.PlainText)
	}
	// 应有对应的 at 段。
	var atCount int
	for _, seg := range got.Message.Segments {
		if seg.Type == domain.SegmentAt {
			atCount++
		}
	}
	if atCount != 2 {
		t.Errorf("at 段数量 = %d，期望 2", atCount)
	}
}

// TestDecodeFaceTag 验证表情标签转写。
func TestDecodeFaceTag(t *testing.T) {
	payload := `{
		"id": "msg-6",
		"content": "哈哈<faceType=1,faceId=\"0\",ext=\"\">",
		"group_openid": "group-abc",
		"author": {"member_openid": "user-1"},
		"timestamp": "2026-10-21T10:00:00Z"
	}`

	got, err := Decode(GroupMessageEvent, json.RawMessage(payload), "bot-openid", "木木")
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}

	if strings.Contains(got.Message.PlainText, "faceType") {
		t.Errorf("纯文本里残留表情协议标签: %q", got.Message.PlainText)
	}
	if !strings.Contains(got.Message.PlainText, "[表情]") {
		t.Errorf("纯文本 = %q，应含 [表情]", got.Message.PlainText)
	}
}

// TestDecodeMissingID 验证缺 id 报错。
func TestDecodeMissingID(t *testing.T) {
	payload := `{"content":"hi","group_openid":"g","author":{"member_openid":"u"}}`
	if _, err := Decode(GroupMessageEvent, json.RawMessage(payload), "bot", "木木"); err == nil {
		t.Error("缺 id 应报错")
	}
}

// TestDecodeMissingGroup 验证群消息缺 group_openid 报错。
func TestDecodeMissingGroup(t *testing.T) {
	payload := `{"id":"m","content":"hi","author":{"member_openid":"u"}}`
	if _, err := Decode(GroupMessageEvent, json.RawMessage(payload), "bot", "木木"); err == nil {
		t.Error("群消息缺 group_openid 应报错")
	}
}

// TestSplitOutboundText 验证出站纯文本。
func TestSplitOutboundText(t *testing.T) {
	parts := splitOutbound([]domain.Segment{domain.Text("你好")})
	if len(parts) != 1 {
		t.Fatalf("应为 1 部分，实际 %d", len(parts))
	}
	if parts[0].isMedia {
		t.Error("不应是媒体")
	}
	if parts[0].body["msg_type"] != 0 {
		t.Errorf("msg_type = %v，期望 0（纯文本）", parts[0].body["msg_type"])
	}
	if parts[0].body["content"] != "你好" {
		t.Errorf("content = %v", parts[0].body["content"])
	}
}

// TestSplitOutboundAtUsesMarkdown 验证 @ 走 markdown。
//
// 实测：纯文本消息里的 <@openid> 显示成代码原文，
// 只有 markdown 的 <qqbot-at-user id="..." /> 能渲染成真正的 @。
func TestSplitOutboundAtUsesMarkdown(t *testing.T) {
	parts := splitOutbound([]domain.Segment{
		domain.Text("你好 "),
		domain.At("openid-abc", "小明"),
	})
	if len(parts) != 1 {
		t.Fatalf("应为 1 部分，实际 %d", len(parts))
	}
	if parts[0].body["msg_type"] != 2 {
		t.Errorf("含 @ 时 msg_type = %v，期望 2（markdown）", parts[0].body["msg_type"])
	}
	md, ok := parts[0].body["markdown"].(map[string]any)
	if !ok {
		t.Fatal("应使用 markdown 结构")
	}
	content, _ := md["content"].(string)
	if !strings.Contains(content, `<qqbot-at-user id="openid-abc" />`) {
		t.Errorf("markdown 内容 = %q，应含 qqbot-at-user 标签", content)
	}
}

// TestSplitOutboundNumericAtDegrades 验证纯数字 ID 无法 @，退化为昵称。
func TestSplitOutboundNumericAtDegrades(t *testing.T) {
	parts := splitOutbound([]domain.Segment{
		domain.Text("你好 "),
		domain.At("123456", "小明"),
	})
	content, _ := parts[0].body["content"].(string)
	if !strings.Contains(content, "@小明") {
		t.Errorf("内容 = %q，纯数字 ID 应退化为 @昵称", content)
	}
	if parts[0].body["msg_type"] != 0 {
		t.Errorf("纯数字 @ 应保持纯文本，msg_type = %v", parts[0].body["msg_type"])
	}
}

// TestSplitOutboundMixed 验证混合消息按顺序拆条。
//
// 官方限制：一条消息只能承载一种内容。
func TestSplitOutboundMixed(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("x", 20))
	parts := splitOutbound([]domain.Segment{
		domain.Text("看这个"),
		{Type: domain.SegmentImage, Binary: png},
		domain.Text("怎么样"),
	})

	if len(parts) != 3 {
		t.Fatalf("应拆成 3 部分，实际 %d", len(parts))
	}
	if parts[0].isMedia || parts[1].isMedia != true || parts[2].isMedia {
		t.Error("中间部分应是媒体，两侧是文本")
	}
	if !strings.HasSuffix(parts[1].fileName, ".png") {
		t.Errorf("文件名 = %q，应以 .png 结尾", parts[1].fileName)
	}
}

// TestSplitOutboundReplySkipped 验证 reply 段被跳过。
func TestSplitOutboundReplySkipped(t *testing.T) {
	parts := splitOutbound([]domain.Segment{
		domain.Reply("msg-old"),
		domain.Text("回复内容"),
	})
	if len(parts) != 1 {
		t.Fatalf("reply 段应被跳过，实际 %d 部分", len(parts))
	}
	if parts[0].body["content"] != "回复内容" {
		t.Errorf("content = %v", parts[0].body["content"])
	}
}

// TestSplitOutboundEmpty 验证空消息。
func TestSplitOutboundEmpty(t *testing.T) {
	parts := splitOutbound(nil)
	if len(parts) != 0 {
		t.Errorf("空消息应返回 0 部分，实际 %d", len(parts))
	}
}

// TestDetectImageExt 验证图片格式识别。
func TestDetectImageExt(t *testing.T) {
	cases := []struct {
		data []byte
		want string
	}{
		{[]byte{0xFF, 0xD8, 0xFF, 0xE0}, "jpg"},
		{[]byte("\x89PNG\r\n\x1a\n"), "png"},
		{[]byte("GIF89a"), "gif"},
		{[]byte("RIFF0000WEBP"), "webp"},
		{[]byte("BM1234"), "bmp"},
		{[]byte("not-an-image"), ""},
	}

	for _, c := range cases {
		if got := detectImageExt(c.data); got != c.want {
			t.Errorf("detectImageExt(%q) = %q，期望 %q", c.data, got, c.want)
		}
	}
}

// TestPassiveWindowGroup 验证群聊被动窗口。
func TestPassiveWindowGroup(t *testing.T) {
	pt := newPassiveTracker()
	now := timeNow()
	pt.RecordInbound("group", "g1", "msg-1", now)

	// 群聊最多 5 条。
	for i := 1; i <= 5; i++ {
		msgID, seq, ok := pt.Claim("group", "g1", "msg-1")
		if !ok {
			t.Fatalf("第 %d 次应能占用", i)
		}
		if msgID != "msg-1" {
			t.Errorf("msgID = %q", msgID)
		}
		if seq != i {
			t.Errorf("seq = %d，期望 %d", seq, i)
		}
	}

	// 第 6 次应失败。
	if _, _, ok := pt.Claim("group", "g1", "msg-1"); ok {
		t.Error("群聊第 6 次应超出限制")
	}
}

// TestPassiveWindowExpired 验证窗口过期。
func TestPassiveWindowExpired(t *testing.T) {
	pt := newPassiveTracker()
	// 记录一个 10 分钟前的入站消息，群聊窗口是 5 分钟。
	pt.RecordInbound("group", "g1", "old-msg", timeNow().Add(-10*60*1e9))

	if _, _, ok := pt.Claim("group", "g1", "old-msg"); ok {
		t.Error("过期消息不应能占用被动窗口")
	}
}

// TestPassiveWindowNoInbound 验证无入站记录时失败。
func TestPassiveWindowNoInbound(t *testing.T) {
	pt := newPassiveTracker()
	if _, _, ok := pt.Claim("group", "unknown", ""); ok {
		t.Error("没有入站记录时不应能占用")
	}
}

// TestIsAllDigits 验证数字判断。
func TestIsAllDigits(t *testing.T) {
	cases := map[string]bool{
		"123456": true,
		"abc123": false,
		"":       false,
		"12a":    false,
		"000":    true,
	}
	for in, want := range cases {
		if got := isAllDigits(in); got != want {
			t.Errorf("isAllDigits(%q) = %v，期望 %v", in, got, want)
		}
	}
}

// TestLooseIntHandlesStringAndNumber 防：block_size / index 被下发成
// 字符串时整条上传链路失败。
//
// 真实现象（logs/oh-my-qqbot.log）：
//
//	发送表情包失败: 图片预上传失败: 解析 QQ 接口响应失败
//	  /v2/users/xxx/upload_prepare: json: cannot unmarshal string
//	  into Go struct field .block_size of type int
//
// 后果是表情包与图片根本发不出去，而日志里只有一句「预上传失败」。
func TestLooseIntHandlesStringAndNumber(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want int
	}{
		{"数字", float64(1024), 1024},
		{"字符串", "1024", 1024},
		{"带空白的字符串", " 1024 ", 1024},
		{"int", 512, 512},
		{"nil", nil, 0},
		{"空串", "", 0},
		{"非数字串", "abc", 0},
		{"布尔true", true, 1},
		{"布尔false", false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := looseInt(c.in); got != c.want {
				t.Errorf("looseInt(%v) = %d，期望 %d", c.in, got, c.want)
			}
		})
	}
}

// TestLooseStringHandlesNumber 是上一条的对称情形（attachments[].size）。
func TestLooseStringHandlesNumber(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{float64(4321), "4321"},
		{"4321", "4321"},
		{nil, ""},
		{true, "true"},
	}
	for _, c := range cases {
		if got := looseString(c.in); got != c.want {
			t.Errorf("looseString(%v) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// TestDecodeUsesLocalTimeNotPlatformTimestamp 防：时间线倒挂。
//
// 库里数据 + 日志对比（真实证据）：
//
//	日志收到 14:47:42  库里记录 14:49:19  偏差 +97s
//	日志收到 16:50:50  库里记录 16:52:26  偏差 +96s
//
// 原因是入站用了平台 timestamp（比本机晚 96 秒），而出站用本机 time.Now()。
// 两者混在一起排序，用户先发的表情包会排在机器人后发的回复**后面**。
// 现在统一用本机接收时间。
func TestDecodeUsesLocalTimeNotPlatformTimestamp(t *testing.T) {
	// 平台时间戳刻意给一个明显的过去时间，如果代码还在读它就会暴露。
	payload := `{
		"id": "msg-time",
		"content": "你好",
		"group_openid": "group-abc",
		"author": {"member_openid": "user-1", "username": "小明"},
		"timestamp": "2020-01-01T00:00:00Z"
	}`

	before := time.Now()
	got, err := Decode(GroupMessageEvent, json.RawMessage(payload), "bot-openid", "木木")
	after := time.Now()
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}

	ts := got.Message.ReceivedAt
	if ts.Before(before.Add(-time.Second)) || ts.After(after.Add(time.Second)) {
		t.Errorf("ReceivedAt = %v，应为本机当前时间（%v ~ %v），不是平台时间戳",
			ts, before, after)
	}
}

// decodeRaw 是引用相关测试的公共辅助：把一段 JSON 当成事件 payload 解码。
func decodeRaw(t *testing.T, eventType, payload, accountID string) *domain.Message {
	t.Helper()
	res, err := Decode(eventType, json.RawMessage(payload), accountID, "木木")
	if err != nil {
		t.Fatalf("Decode 失败: %v", err)
	}
	return res.Message
}

// TestDecodeQuoteWithText 验证「带字引用」解析出引用段与被引用正文。
//
// 报文取自真实抓包（bot/qq-probe/captures/run-20261001-214147），
// 只把中文乱码还原、去掉了脱敏的 auth_token。
func TestDecodeQuoteWithText(t *testing.T) {
	payload := `{
		"id": "ROBOT1.0_quote",
		"content": " 这条信息引用了上一条消息",
		"group_openid": "G1",
		"author": {"id": "U1", "member_openid": "U1", "username": "犭者"},
		"message_scene": {"ext": [
			"ref_msg_idx=REFIDX_target",
			"msg_idx=REFIDX_self",
			"auth_token=t"
		], "source": "default"},
		"message_type": 103,
		"msg_elements": [
			{"content": "这是一条即将被引用的信息", "message_type": 103, "msg_idx": "REFIDX_target"}
		],
		"parallel_message": {"msg_nodes": [{"content": "这是一条即将被引用的信息", "message_type": 0}]}
	}`

	msg := decodeRaw(t, GroupMessageEvent, payload, "7246569821734997014")

	// 正文只该是用户本轮输入，不能混进被引用内容。
	if msg.PlainText != "这条信息引用了上一条消息" {
		t.Errorf("PlainText = %q，不该包含被引用内容", msg.PlainText)
	}
	// 引用段必须排在首位（对齐 uni_message_sender.py:301 的 insert(0, …)）。
	if len(msg.Segments) == 0 || msg.Segments[0].Type != domain.SegmentReply {
		t.Fatalf("首段应为 reply，实际 %+v", msg.Segments)
	}
	if got := msg.Segments[0].MessageID; got != "REFIDX_target" {
		t.Errorf("引用段 MessageID = %q，期望 REFIDX_target", got)
	}
	if got := msg.Additional[domain.AdditionalQuotedText]; got != "这是一条即将被引用的信息" {
		t.Errorf("quoted_text = %q", got)
	}
	if got := msg.Additional[domain.AdditionalRefMsgIdx]; got != "REFIDX_target" {
		t.Errorf("ref_msg_idx = %q", got)
	}
	if got := msg.RefMsgIdx; got != "REFIDX_self" {
		t.Errorf("RefMsgIdx = %q，期望 REFIDX_self", got)
	}
}

// TestDecodeQuoteWithoutText 守最严重的那条回归：
// 只引用、不打字时，被引用的旧消息**不能**被当成用户这轮说的话。
//
// 旧实现的 extractContent 在顶层 content 为空时回落到 msg_elements，
// 于是别人历史上说的那句直接变成「当前用户说的」。错误归因不报任何错。
func TestDecodeQuoteWithoutText(t *testing.T) {
	payload := `{
		"id": "ROBOT1.0_quote_only",
		"content": "",
		"group_openid": "G1",
		"author": {"id": "U1", "username": "犭者"},
		"message_scene": {"ext": ["ref_msg_idx=REFIDX_target", "msg_idx=REFIDX_self"]},
		"message_type": 103,
		"msg_elements": [{"content": "别人以前说的话", "message_type": 103, "msg_idx": "REFIDX_target"}]
	}`

	msg := decodeRaw(t, GroupMessageEvent, payload, "7246569821734997014")

	if msg.PlainText != "" {
		t.Errorf("PlainText = %q，被引用内容绝不能变成用户发言", msg.PlainText)
	}
	if got := msg.Additional[domain.AdditionalQuotedText]; got != "别人以前说的话" {
		t.Errorf("被引正文应进 quoted_text，实际 %q", got)
	}
	// 整条消息不能因为「正文为空」被丢弃。
	if len(msg.Segments) != 1 || msg.Segments[0].Type != domain.SegmentReply {
		t.Fatalf("应只剩一个 reply 段，实际 %+v", msg.Segments)
	}
}

// TestDecodeQuoteMediaUsesParallelMessage 验证媒体类引用靠 parallel_message 取正文。
//
// 实测：引用图片/语音/文件时 msg_elements[].content 是空串，
// 可读占位只在 parallel_message.msg_nodes 里。
func TestDecodeQuoteMediaUsesParallelMessage(t *testing.T) {
	payload := `{
		"id": "ROBOT1.0_quote_media",
		"content": "这个图好看",
		"group_openid": "G1",
		"author": {"id": "U1", "username": "犭者"},
		"message_scene": {"ext": ["ref_msg_idx=REFIDX_img", "msg_idx=REFIDX_self"]},
		"message_type": 103,
		"msg_elements": [{
			"content": "", "message_type": 103, "msg_idx": "REFIDX_img",
			"attachments": [{"content_type": "image/jpeg", "filename": "a.jpg", "size": "86029", "url": "https://x/a.jpg"}]
		}],
		"parallel_message": {"msg_nodes": [{"content": "[图片]", "message_type": 7}]}
	}`

	msg := decodeRaw(t, GroupMessageEvent, payload, "7246569821734997014")

	if got := msg.Additional[domain.AdditionalQuotedText]; got != "[图片]" {
		t.Errorf("quoted_text = %q，期望从 parallel_message 取到 [图片]", got)
	}
	if msg.PlainText != "这个图好看" {
		t.Errorf("PlainText = %q", msg.PlainText)
	}
}

// TestDecodeQuoteChatRecord 验证引用聊天记录（TMP_ 键空间）。
//
// 这类引用的 msg_elements[0].msg_idx 为 null，只能靠 ref_msg_idx 认出这是引用。
func TestDecodeQuoteChatRecord(t *testing.T) {
	payload := `{
		"id": "ROBOT1.0_quote_fwd",
		"content": "看这个",
		"group_openid": "G1",
		"author": {"id": "U1", "username": "犭者"},
		"message_scene": {"ext": ["ref_msg_idx=TMP_f01d7008-df53-42f0-840e-7d9a6ffc60b4", "msg_idx=REFIDX_self"]},
		"message_type": 103,
		"msg_elements": [{"content": "=== 消息 1 ===\n[消息内容] 合并信息测试"}]
	}`

	msg := decodeRaw(t, GroupMessageEvent, payload, "7246569821734997014")

	if len(msg.Segments) == 0 || msg.Segments[0].Type != domain.SegmentReply {
		t.Fatalf("聊天记录引用同样要产出 reply 段，实际 %+v", msg.Segments)
	}
	if got := msg.Segments[0].MessageID; got != "TMP_f01d7008-df53-42f0-840e-7d9a6ffc60b4" {
		t.Errorf("引用段 MessageID = %q", got)
	}
}

// TestDecodeNonQuoteKeepsMsgElementsFallback 守「修 bug 不能过头」：
// 非引用消息（message_type=0）仍然要走 msg_elements 正文兜底。
func TestDecodeNonQuoteKeepsMsgElementsFallback(t *testing.T) {
	payload := `{
		"id": "ROBOT1.0_plain",
		"content": "",
		"group_openid": "G1",
		"author": {"id": "U1", "username": "犭者"},
		"message_type": 0,
		"msg_elements": [{"content": "文本在元素里"}]
	}`

	msg := decodeRaw(t, GroupMessageEvent, payload, "7246569821734997014")
	if msg.PlainText != "文本在元素里" {
		t.Errorf("PlainText = %q，非引用消息仍应吃 msg_elements 兜底", msg.PlainText)
	}
	if len(msg.Segments) != 1 || msg.Segments[0].Type != domain.SegmentText {
		t.Errorf("不该产出 reply 段，实际 %+v", msg.Segments)
	}
}

// TestDecodeMentionIsYou 守全量群模式下 @ 判定失效的回归。
//
// 实测 mentions[].id 是 openid 形态（E4E7B42E…），与 READY 下发的
// 19 位纯数字 self_id 永远不相等。只比 id 的实现在全量群消息模式下
// is_at 恒为 false（旧日志：96 条群消息、40 条带 @、at=true 零命中）。
func TestDecodeMentionIsYou(t *testing.T) {
	payload := `{
		"id": "ROBOT1.0_at",
		"content": " 在吗",
		"group_openid": "G1",
		"author": {"id": "U1", "member_openid": "U1", "username": "犭者"},
		"mentions": [{"bot": true, "id": "E4E7B42EBF5C7495156FC910340C576B", "is_you": true, "username": "木木"}]
	}`

	msg := decodeRaw(t, GroupMessageEvent, payload, "7246569821734997014")
	if !msg.IsAt {
		t.Error("is_you=true 时应判定为 @ 了机器人")
	}

	// is_you=false 的其他机器人不能被误判成 @ 自己。
	payload = strings.Replace(payload, `"is_you": true`, `"is_you": false`, 1)
	msg = decodeRaw(t, GroupMessageEvent, payload, "7246569821734997014")
	if msg.IsAt {
		t.Error("is_you=false 不该判定为 @ 了机器人")
	}
}

// TestMessageSceneExtValue 验证 ext 是 "key=value" 数组、value 里可能带 '='。
func TestMessageSceneExtValue(t *testing.T) {
	ext := []string{"msg_idx=REFIDX_a+b/c==", "ref_msg_idx=TMP_x", "auth_token=t"}
	if got := messageSceneExtValue(ext, "msg_idx"); got != "REFIDX_a+b/c==" {
		t.Errorf("msg_idx = %q，value 里的 '=' 不能被截断", got)
	}
	if got := messageSceneExtValue(ext, "ref_msg_idx"); got != "TMP_x" {
		t.Errorf("ref_msg_idx = %q", got)
	}
	if got := messageSceneExtValue(ext, "没有这个键"); got != "" {
		t.Errorf("不存在的键应返回空串，实际 %q", got)
	}
	if got := messageSceneExtValue(nil, "msg_idx"); got != "" {
		t.Errorf("nil 应返回空串，实际 %q", got)
	}
	// 键名必须整段匹配，不能是前缀匹配（msg_idx 不该命中 ref_msg_idx 的后缀）。
	if got := messageSceneExtValue([]string{"ref_msg_idx=X"}, "msg_idx"); got != "" {
		t.Errorf("不该前缀/后缀误匹配，实际 %q", got)
	}
}
