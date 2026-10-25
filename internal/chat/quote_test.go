package chat

import (
	"math"
	"testing"
	"unicode/utf8"
)

// 本文件验证引用判定的概率公式。
//
// 数值来自 refs/MaiBot/src/maisaka/quote/rules.py，是原版离线试验的拟合结果，
// 这里只验证「实现没有抄错」，不验证「数值本身合不合理」。

// TestQuoteBaseProbabilityMatchesReference 防：抄公式时漏掉某个分档。
func TestQuoteBaseProbabilityMatchesReference(t *testing.T) {
	cases := []struct {
		name        string
		textLength  int
		prevSelf    bool
		prevSeconds float64
		want        float64
	}{
		{"空文本-自己-3秒内", 0, true, 1.0, 0.005913471588878362},
		{"空文本-自己-3秒外", 0, true, 10.0, 0.013812173277286514},
		{"空文本-他人-1分钟内", 0, false, 30.0, 0.025472825783009516},
		{"空文本-他人-1分钟外", 0, false, 300.0, 0.0108627046866078},
		{"有文本-自己-3秒内", 5, true, 1.0, 0.011765162067321502},
		{"有文本-自己-3秒外", 5, true, 10.0, 0.07760369512696548},
		{"有文本-他人-短", 10, false, 5.0, 0.20934408508581265},
		{"有文本-他人-长", 100, false, 5.0, 0.08614950202149084},
	}
	for _, c := range cases {
		got := quoteBaseProbability(c.textLength, c.prevSelf, c.prevSeconds)
		if math.Abs(got-c.want) > 1e-15 {
			t.Errorf("%s: 概率 = %v，期望 %v", c.name, got, c.want)
		}
	}
}

// TestQuoteDistanceMultiplier 防：距离系数档位抄反。
func TestQuoteDistanceMultiplier(t *testing.T) {
	cases := map[int]float64{
		0: 0.5, 1: 1.0, 2: 1.0, 3: 1.1, 5: 1.1, 6: 1.5, 99: 1.5,
	}
	for distance, want := range cases {
		if got := quoteDistanceMultiplier(distance); got != want {
			t.Errorf("距离 %d 系数 = %v，期望 %v", distance, got, want)
		}
	}
}

// TestQuoteProbabilityFrequencyAndDistance 防：often 没有翻倍、或没有按距离缩放。
func TestQuoteProbabilityFrequencyAndDistance(t *testing.T) {
	occ, err := QuoteProbability(10, false, 5, QuoteOccasionally, 2)
	if err != nil {
		t.Fatalf("occasionally 返回错误: %v", err)
	}
	often, err := QuoteProbability(10, false, 5, QuoteOften, 2)
	if err != nil {
		t.Fatalf("often 返回错误: %v", err)
	}
	if math.Abs(often-occ*2) > 1e-15 {
		t.Errorf("often(%v) 应为 occasionally(%v) 的两倍", often, occ)
	}

	far, err := QuoteProbability(10, false, 5, QuoteOccasionally, 6)
	if err != nil {
		t.Fatalf("远距离返回错误: %v", err)
	}
	if far <= occ {
		t.Errorf("距离 6 (%v) 的概率应高于距离 2 (%v)", far, occ)
	}

	never, err := QuoteProbability(10, false, 5, QuoteNever, 2)
	if err != nil || never != 0 {
		t.Errorf("never 概率 = %v, err = %v，期望 0, nil", never, err)
	}

	if _, err := QuoteProbability(10, false, 5, QuoteFrequency("bogus"), 2); err == nil {
		t.Error("非法频率应返回错误")
	}
	if _, err := QuoteProbability(10, false, 5, QuoteOften, -1); err == nil {
		t.Error("负距离应返回错误")
	}
}

// TestQuoteProbabilityCapsAtOne 防：概率没有封顶 1.0（often + 远距离会溢出）。
func TestQuoteProbabilityCapsAtOne(t *testing.T) {
	// 有文本 + 他人 + 长文本 = 0.209，often 翻倍再乘 1.5 = 0.628，还不到 1。
	// 用短文本档（0.209）配 often(2) 与距离 1.5 虽然不超 1，
	// 但私聊的 logistic 会接近 1，所以这里验证群聊确实做了一次 Min。
	p, err := QuoteProbability(10, false, 5, QuoteOften, 6)
	if err != nil {
		t.Fatalf("返回错误: %v", err)
	}
	if p > 1.0 {
		t.Errorf("概率 = %v，超过了 1.0", p)
	}
}

// TestPrivateQuoteProbabilityMonotonic 防：私聊公式符号抄反（距离越远概率反而越低）。
func TestPrivateQuoteProbabilityMonotonic(t *testing.T) {
	near, err := PrivateQuoteProbability(20, false, 5, 1, 0, QuoteOccasionally)
	if err != nil {
		t.Fatalf("返回错误: %v", err)
	}
	far, err := PrivateQuoteProbability(20, false, 5, 1, 10, QuoteOccasionally)
	if err != nil {
		t.Fatalf("返回错误: %v", err)
	}
	if far <= near {
		t.Errorf("距离 10 的概率 (%v) 应高于距离 0 (%v)", far, near)
	}

	// 空文本会显著降低概率（z 减 3.45）。
	empty, err := PrivateQuoteProbability(0, false, 5, 1, 3, QuoteOccasionally)
	if err != nil {
		t.Fatalf("返回错误: %v", err)
	}
	if empty >= near {
		t.Errorf("空文本概率 (%v) 应低于有文本 (%v)", empty, near)
	}

	// 距离封顶 32：再远也不会继续涨。
	a, _ := PrivateQuoteProbability(20, false, 5, 1, 31, QuoteOccasionally)
	b, _ := PrivateQuoteProbability(20, false, 5, 1, 5000, QuoteOccasionally)
	if math.Abs(a-b) > 1e-15 {
		t.Errorf("距离 31 (%v) 与 5000 (%v) 应一致（封顶 32）", a, b)
	}
}

// TestDecideQuoteOutsideWindowAlwaysQuotes 防：目标离开窗口时没有直接引用。
func TestDecideQuoteOutsideWindowAlwaysQuotes(t *testing.T) {
	quoted, prob, err := decideQuoteWithRoll(nil, "bot", "你好", QuoteOccasionally, true, 0.99)
	if err != nil {
		t.Fatalf("返回错误: %v", err)
	}
	if !quoted {
		t.Error("目标不在窗口内时应直接引用")
	}
	if prob != 1.0 {
		t.Errorf("概率 = %v，期望 1.0", prob)
	}

	// never 档无论窗口内外都不引用。
	quoted, prob, err = decideQuoteWithRoll(nil, "bot", "你好", QuoteNever, true, 0.0)
	if err != nil || quoted || prob != 0 {
		t.Errorf("never: quoted=%v prob=%v err=%v，期望 false/0/nil", quoted, prob, err)
	}
}

// TestDecideQuoteRollBoundary 防：把 roll < p 写成 roll <= p，
// 或干脆把概率当布尔用。
func TestDecideQuoteRollBoundary(t *testing.T) {
	distance := 2
	ctx := &QuoteContext{
		TargetDistance:   &distance,
		PreviousSenderID: "user",
		PreviousSeconds:  5,
		PreviousBurst:    1,
	}

	// 先算出概率，再用概率本身作为 roll：roll == p 不应命中。
	_, prob, err := decideQuoteWithRoll(ctx, "bot", "这是一句普通长度的话", QuoteOccasionally, true, 0)
	if err != nil {
		t.Fatalf("返回错误: %v", err)
	}
	if prob <= 0 || prob >= 1 {
		t.Skipf("概率 %v 落在端点，无法验证边界", prob)
	}

	hit, _, err := decideQuoteWithRoll(ctx, "bot", "这是一句普通长度的话", QuoteOccasionally, true, prob-1e-9)
	if err != nil {
		t.Fatalf("返回错误: %v", err)
	}
	if !hit {
		t.Errorf("roll 略小于概率 (%v) 时应命中", prob)
	}

	miss, _, err := decideQuoteWithRoll(ctx, "bot", "这是一句普通长度的话", QuoteOccasionally, true, prob)
	if err != nil {
		t.Fatalf("返回错误: %v", err)
	}
	if miss {
		t.Errorf("roll 等于概率 (%v) 时不应命中（严格小于）", prob)
	}
}

// TestQuoteCounterRecordsAndSnapshots 防：计数器不记录、或距离算反。
func TestQuoteCounterRecordsAndSnapshots(t *testing.T) {
	c := NewQuoteCounter()
	base := int64(1_000_000_000)

	c.Record("m1", "user-a", base)
	c.Record("m2", "user-a", base+int64(1e9))
	c.Record("m3", "bot", base+int64(2e9))

	// m1 距最新一条的距离是 2。
	snap, ok := c.Snapshot("m1", base+int64(3e9))
	if !ok {
		t.Fatal("m1 应在窗口内")
	}
	if snap.TargetDistance == nil || *snap.TargetDistance != 2 {
		t.Errorf("m1 距离 = %v，期望 2", snap.TargetDistance)
	}
	if snap.PreviousSenderID != "bot" {
		t.Errorf("上一条发送者 = %q，期望 bot", snap.PreviousSenderID)
	}
	if math.Abs(snap.PreviousSeconds-1.0) > 1e-9 {
		t.Errorf("间隔 = %v 秒，期望 1.0", snap.PreviousSeconds)
	}
	if snap.PreviousBurst != 1 {
		t.Errorf("连发 = %d，期望 1（上一条换了人）", snap.PreviousBurst)
	}
}

// TestQuoteCounterDedupesSameMessage 防：同一条消息被投递两次时重复计数，
// 导致距离和连发条数都失真。
func TestQuoteCounterDedupesSameMessage(t *testing.T) {
	c := NewQuoteCounter()
	c.Record("m1", "user-a", 1)
	c.Record("m1", "user-a", 2) // 重复投递
	c.Record("m2", "user-a", 3)

	snap, ok := c.Snapshot("m1", 4)
	if !ok {
		t.Fatal("m1 应在窗口内")
	}
	if *snap.TargetDistance != 1 {
		t.Errorf("距离 = %d，期望 1（重复投递不应计数）", *snap.TargetDistance)
	}
	if snap.PreviousBurst != 2 {
		t.Errorf("连发 = %d，期望 2", snap.PreviousBurst)
	}
}

// TestQuoteCounterBurstCap 防：连发条数没有按 16 封顶，
// 私聊公式里 log2 会因此算错。
func TestQuoteCounterBurstCap(t *testing.T) {
	c := NewQuoteCounter()
	for i := 0; i < 50; i++ {
		c.Record(string(rune('a'+i%26))+string(rune('0'+i/26)), "same-user", int64(i))
	}
	snap, ok := c.Snapshot(string(rune('a'+49%26))+string(rune('0'+49/26)), 100)
	if !ok {
		t.Fatal("目标应在窗口内")
	}
	if snap.PreviousBurst > quoteMaxBurst {
		t.Errorf("连发 = %d，超过上限 %d", snap.PreviousBurst, quoteMaxBurst)
	}
}

// TestQuoteCounterWindowEviction 防：窗口无限增长（长跑必然内存泄漏），
// 以及「离开窗口」这条分支没有被触发。
func TestQuoteCounterWindowEviction(t *testing.T) {
	c := NewQuoteCounter()
	for i := 0; i < quoteWindowSize+50; i++ {
		c.Record("m"+itoa(i), "user", int64(i))
	}

	if _, ok := c.Snapshot("m0", int64(quoteWindowSize+50)); ok {
		t.Error("最早的消息应已被淘汰，Snapshot 应返回 false")
	}
	if _, ok := c.Snapshot("m"+itoa(quoteWindowSize+49), int64(quoteWindowSize+50)); !ok {
		t.Error("最新的消息应在窗口内")
	}
	if len(c.positions) != quoteWindowSize {
		t.Errorf("窗口内 ID 数 = %d，期望 %d", len(c.positions), quoteWindowSize)
	}
}

// TestQuoteFrequencyOf 防：配置里写错频率档位时被当成合法值。
func TestQuoteFrequencyOf(t *testing.T) {
	for _, valid := range []QuoteFrequency{QuoteNever, QuoteOccasionally, QuoteOften} {
		got, ok := QuoteFrequencyOf(string(valid))
		if !ok || got != valid {
			t.Errorf("QuoteFrequencyOf(%q) = %v, %v，期望识别为合法", valid, got, ok)
		}
	}
	for _, bad := range []string{"", "always", "Never", "偶尔"} {
		if got, ok := QuoteFrequencyOf(bad); ok {
			t.Errorf("QuoteFrequencyOf(%q) 应为非法，实际返回 %v", bad, got)
		}
	}
}

// itoa 避免为了一个数字引入 strconv。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// TestTruncateMessageWithUsesRunes 防：按 byte 截断把汉字劈成非法 UTF-8。
//
// 这是真实会炸的地方：截断结果会进 JSON 请求体，
// 一个非法字节就让整次模型调用失败。
func TestTruncateMessageWithUsesRunes(t *testing.T) {
	// 每个汉字 3 字节，按 byte 切 5 一定落在字符中间。
	text := "中文测试一段很长的文本"
	got := TruncateMessageWith(text, 5, "...")
	if got != "中文测试一..." {
		t.Errorf("截断结果 = %q，期望 %q", got, "中文测试一...")
	}
	if !utf8.ValidString(got) {
		t.Errorf("截断结果不是合法 UTF-8: %q", got)
	}

	// 不超长时原样返回，不加尾巴。
	if got := TruncateMessageWith("短", 5, "..."); got != "短" {
		t.Errorf("未超长时 = %q，期望原样返回", got)
	}

	// maxLen <= 0 表示不截断。
	if got := TruncateMessageWith(text, 0, "..."); got != text {
		t.Errorf("maxLen=0 时 = %q，期望原样返回", got)
	}
}

// TestTruncateMessageAppendsEllipsis 防：默认替换文案被改掉。
func TestTruncateMessageAppendsEllipsis(t *testing.T) {
	if got := TruncateMessage("abcdefgh", 3); got != "abc..." {
		t.Errorf("TruncateMessage = %q，期望 %q", got, "abc...")
	}
}
