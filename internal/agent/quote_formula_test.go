package agent

// 引用判定公式的逐条对照测试。
//
// 为什么要「另写一份公式再对比」而不是只测几个抽样值：
// 引用概率完全由一堆常数决定，抄错一位小数不会崩、也不会报错，
// 只会让引用频率悄悄偏掉（参考实现的系数来自离线试验，不能自创）。
// 这里把 refs/MaiBot/src/maisaka/quote/rules.py 的公式原样再实现一遍，
// 与 internal/chat 的实现逐点比对，任何常数抄错都会立刻暴露。

import (
	"math"
	"testing"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/chat"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/logx"
)

// refQuoteProbability 是 rules.py:quote_probability 的逐行转写。
//
// 刻意不复用 chat 包的任何辅助函数：那样就成了「用实现验证实现」。
func refQuoteProbability(textLength int, previousSenderSelf bool, previousSeconds float64, freq chat.QuoteFrequency, targetDistance int) float64 {
	var p float64
	switch {
	case textLength == 0 && previousSenderSelf:
		if previousSeconds <= 3 {
			p = 0.005913471588878362
		} else {
			p = 0.013812173277286514
		}
	case textLength == 0:
		if previousSeconds <= 60 {
			p = 0.025472825783009516
		} else {
			p = 0.0108627046866078
		}
	case previousSenderSelf:
		if previousSeconds <= 3 {
			p = 0.011765162067321502
		} else {
			p = 0.07760369512696548
		}
	default:
		if textLength <= 50 {
			p = 0.20934408508581265
		} else {
			p = 0.08614950202149084
		}
	}

	var distanceMultiplier float64
	switch {
	case targetDistance == 0:
		distanceMultiplier = 0.5
	case targetDistance <= 2:
		distanceMultiplier = 1.0
	case targetDistance <= 5:
		distanceMultiplier = 1.1
	default:
		distanceMultiplier = 1.5
	}

	freqMultiplier := 1.0
	if freq == chat.QuoteOften {
		freqMultiplier = 2.0
	}
	return math.Min(1.0, p*freqMultiplier*distanceMultiplier)
}

// refPrivateQuoteProbability 是 rules.py:private_quote_probability 的逐行转写。
func refPrivateQuoteProbability(textLength int, previousSenderSelf bool, previousSeconds float64, previousBurst, targetDistance int, freq chat.QuoteFrequency) float64 {
	z := -4.176576611204103
	if previousSenderSelf {
		if previousSeconds <= 3 {
			z += -2.0390866101745555
		} else {
			z += 0.407259409803155
		}
	} else {
		// Python 的 max(1, min(previous_burst, 16))：下界 1，上界 16。
		burst := previousBurst
		if burst < 1 {
			burst = 1
		}
		if burst > 16 {
			burst = 16
		}
		z += 0.6811570734750046 * math.Log2(float64(burst))
	}
	if textLength == 0 {
		z -= 3.4527659205203576
	}
	// Python 的 min(target_distance + 1, 32)。
	dist := targetDistance + 1
	if dist > 32 {
		dist = 32
	}
	z += 0.521800209819322 * math.Log2(float64(dist))

	p := 1 / (1 + math.Exp(-z))
	freqMultiplier := 1.0
	if freq == chat.QuoteOften {
		freqMultiplier = 2.0
	}
	return math.Min(1.0, p*freqMultiplier)
}

// TestQuoteProbabilityMatchesReference 群聊公式逐点对照。
//
// 覆盖八类基础概率 × 距离四档 × 两个频率档位，
// 这是引用行为最核心的一张表。
func TestQuoteProbabilityMatchesReference(t *testing.T) {
	textLengths := []int{0, 10, 50, 51, 200}
	seconds := []float64{0, 3, 3.5, 60, 60.5, 300}
	distances := []int{0, 1, 2, 3, 5, 6, 50}
	freqs := []chat.QuoteFrequency{chat.QuoteOccasionally, chat.QuoteOften}

	for _, textLength := range textLengths {
		for _, prevSelf := range []bool{true, false} {
			for _, sec := range seconds {
				for _, dist := range distances {
					for _, freq := range freqs {
						want := refQuoteProbability(textLength, prevSelf, sec, freq, dist)
						got, err := chat.QuoteProbability(textLength, prevSelf, sec, freq, dist)
						if err != nil {
							t.Fatalf("不应报错: %v", err)
						}
						// 浮点误差容忍到 1e-12：这里只关心常数是否抄对。
						if math.Abs(got-want) > 1e-12 {
							t.Errorf("len=%d self=%v sec=%v dist=%d freq=%s: got=%.15f want=%.15f",
								textLength, prevSelf, sec, dist, freq, got, want)
						}
					}
				}
			}
		}
	}
}

// TestPrivateQuoteProbabilityMatchesReference 私聊 logistic 公式逐点对照。
func TestPrivateQuoteProbabilityMatchesReference(t *testing.T) {
	textLengths := []int{0, 1, 20, 200}
	seconds := []float64{0, 3, 4, 120}
	bursts := []int{0, 1, 2, 8, 16, 17, 100}
	distances := []int{0, 1, 5, 31, 32, 33, 100}
	freqs := []chat.QuoteFrequency{chat.QuoteOccasionally, chat.QuoteOften}

	for _, textLength := range textLengths {
		for _, prevSelf := range []bool{true, false} {
			for _, sec := range seconds {
				for _, burst := range bursts {
					for _, dist := range distances {
						for _, freq := range freqs {
							want := refPrivateQuoteProbability(textLength, prevSelf, sec, burst, dist, freq)
							got, err := chat.PrivateQuoteProbability(textLength, prevSelf, sec, burst, dist, freq)
							if err != nil {
								t.Fatalf("不应报错: %v", err)
							}
							if math.Abs(got-want) > 1e-12 {
								t.Errorf("len=%d self=%v sec=%v burst=%d dist=%d freq=%s: got=%.15f want=%.15f",
									textLength, prevSelf, sec, burst, dist, freq, got, want)
							}
						}
					}
				}
			}
		}
	}
}

// TestQuoteProbabilityEdgeCases 验证边界与错误路径。
func TestQuoteProbabilityEdgeCases(t *testing.T) {
	// never 档位一律 0，不看其它参数。
	if p, err := chat.QuoteProbability(10, false, 1, chat.QuoteNever, 3); err != nil || p != 0 {
		t.Errorf("never 档位应为 0，实际 p=%v err=%v", p, err)
	}
	if p, err := chat.PrivateQuoteProbability(10, false, 1, 1, 3, chat.QuoteNever); err != nil || p != 0 {
		t.Errorf("私聊 never 档位应为 0，实际 p=%v err=%v", p, err)
	}

	// 负距离是非法的，必须报错而不是算出个负数概率。
	if _, err := chat.QuoteProbability(10, false, 1, chat.QuoteOccasionally, -1); err == nil {
		t.Errorf("负距离应报错")
	}
	if _, err := chat.PrivateQuoteProbability(10, false, 1, 1, -1, chat.QuoteOccasionally); err == nil {
		t.Errorf("私聊负距离应报错")
	}

	// 未知档位必须报错，否则会静默按 0 处理、引用功能整体失效。
	bad := chat.QuoteFrequency("sometimes")
	if _, err := chat.QuoteProbability(10, false, 1, bad, 1); err == nil {
		t.Errorf("未知档位应报错")
	}
	if _, err := chat.PrivateQuoteProbability(10, false, 1, 1, 1, bad); err == nil {
		t.Errorf("私聊未知档位应报错")
	}

	// 概率必须被夹在 [0,1]：距离 6 的 1.5 倍系数配 often 的 2 倍很容易超过 1。
	for _, freq := range []chat.QuoteFrequency{chat.QuoteOccasionally, chat.QuoteOften} {
		for dist := 0; dist < 40; dist++ {
			p, err := chat.QuoteProbability(0, false, 1, freq, dist)
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if p < 0 || p > 1 {
				t.Errorf("群聊概率越界 p=%v（dist=%d freq=%s）", p, dist, freq)
			}
			pp, err := chat.PrivateQuoteProbability(0, true, 0, 1, dist, freq)
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if pp < 0 || pp > 1 {
				t.Errorf("私聊概率越界 p=%v（dist=%d freq=%s）", pp, dist, freq)
			}
		}
	}
}

// TestQuoteProbabilityOftenDoubles 单独验证 often 档位就是乘 2（封顶前）。
func TestQuoteProbabilityOftenDoubles(t *testing.T) {
	// 选一个乘 2 之后不会触到 1.0 上限的组合。
	const (
		textLength = 0
		distance   = 1
	)
	occ, err := chat.QuoteProbability(textLength, false, 120, chat.QuoteOccasionally, distance)
	if err != nil {
		t.Fatal(err)
	}
	often, err := chat.QuoteProbability(textLength, false, 120, chat.QuoteOften, distance)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(often-occ*2) > 1e-12 {
		t.Errorf("often 应为 occasionally 的两倍：%v vs %v", often, occ*2)
	}
}

// TestQuoteFrequencyOf 验证配置字符串到档位的转换，非法值按 never 且返回 false。
func TestQuoteFrequencyOf(t *testing.T) {
	cases := []struct {
		raw  string
		want chat.QuoteFrequency
		ok   bool
	}{
		{"never", chat.QuoteNever, true},
		{"occasionally", chat.QuoteOccasionally, true},
		{"often", chat.QuoteOften, true},
		{"", chat.QuoteNever, false},
		{"sometimes", chat.QuoteNever, false},
		{"OFTEN", chat.QuoteNever, false},
	}
	for _, c := range cases {
		got, ok := chat.QuoteFrequencyOf(c.raw)
		if got != c.want || ok != c.ok {
			t.Errorf("QuoteFrequencyOf(%q) = (%v,%v)，期望 (%v,%v)", c.raw, got, ok, c.want, c.ok)
		}
	}
}

// TestQuoteCounterRecordAndSnapshot 验证计数器的记录与快照。
//
// 这是 agent/quote.go 依赖的唯一数据结构，字段含义错一个，
// 引用概率就会算在错误的档位上。
func TestQuoteCounterRecordAndSnapshot(t *testing.T) {
	counter := chat.NewQuoteCounter()
	base := time.Unix(1700000000, 0)

	counter.Record("m1", "user-a", base.UnixNano())
	counter.Record("m2", "user-a", base.Add(1*time.Second).UnixNano())
	counter.Record("m3", "bot", base.Add(2*time.Second).UnixNano())

	snap, ok := counter.Snapshot("m1", base.Add(4*time.Second).UnixNano())
	if !ok {
		t.Fatal("m1 应在窗口内")
	}
	if snap.TargetDistance == nil || *snap.TargetDistance != 2 {
		t.Errorf("m1 距离 = %v，期望 2", snap.TargetDistance)
	}
	if snap.PreviousSenderID != "bot" {
		t.Errorf("上一条发送者 = %q，期望 bot", snap.PreviousSenderID)
	}
	// 距最后一条（m3）2 秒。
	if math.Abs(snap.PreviousSeconds-2) > 1e-9 {
		t.Errorf("间隔 = %v 秒，期望 2", snap.PreviousSeconds)
	}
	if snap.PreviousBurst != 1 {
		t.Errorf("连发 = %d，期望 1（最后一条换了人）", snap.PreviousBurst)
	}

	// 目标就是最新一条时距离为 0（公式里对应 0.5 倍系数）。
	snap, ok = counter.Snapshot("m3", base.Add(3*time.Second).UnixNano())
	if !ok || snap.TargetDistance == nil || *snap.TargetDistance != 0 {
		t.Errorf("最新一条的距离应为 0，实际 %v", snap.TargetDistance)
	}

	// 从未记录过的 ID：ok=false，调用方据此「直接引用」。
	if _, ok := counter.Snapshot("不存在", base.UnixNano()); ok {
		t.Errorf("未记录的目标不该返回 ok")
	}
}

// TestQuoteCounterDedupesSameMessage 验证同一 ID 只记一次。
//
// 历史回填会重复投递同一条消息。不去重的话序号会涨、距离会算错，
// 表现是「引用总是引到隔壁那条」。
func TestQuoteCounterDedupesSameMessage(t *testing.T) {
	counter := chat.NewQuoteCounter()
	base := time.Unix(1700000000, 0).UnixNano()

	counter.Record("m1", "a", base)
	counter.Record("m2", "b", base+1)
	counter.Record("m1", "a", base+2) // 重复投递
	counter.Record("m3", "c", base+3)

	snap, ok := counter.Snapshot("m1", base+10)
	if !ok {
		t.Fatal("m1 应存在")
	}
	// 序号仍是 1，总序号是 3（重复那次不该占用序号），所以距离 2。
	if snap.TargetDistance == nil || *snap.TargetDistance != 2 {
		t.Errorf("重复投递后距离 = %v，期望 2", snap.TargetDistance)
	}
	// 重复投递不该改变连发状态：最后记录的仍是 c，连发 1。
	if snap.PreviousSenderID != "c" || snap.PreviousBurst != 1 {
		t.Errorf("重复投递污染了连发状态：sender=%q burst=%d", snap.PreviousSenderID, snap.PreviousBurst)
	}
}

// TestQuoteCounterBurstCap 验证连发封顶 16。
//
// 私聊公式里 log2(burst) 参与计算，参考实现刻意封顶 16，
// 不封顶的话一个人刷屏时引用概率会失控。
func TestQuoteCounterBurstCap(t *testing.T) {
	counter := chat.NewQuoteCounter()
	base := time.Unix(1700000000, 0).UnixNano()

	for i := 0; i < 40; i++ {
		counter.Record("m"+itoa(i), "same", base+int64(i))
	}
	snap, ok := counter.Snapshot("m39", base+100)
	if !ok {
		t.Fatal("目标应存在")
	}
	if snap.PreviousBurst != 16 {
		t.Errorf("连发 = %d，期望封顶 16", snap.PreviousBurst)
	}
}

// TestQuoteCounterWindowEviction 验证 100 条窗口的淘汰与「已离开窗口」语义。
//
// 淘汰后 Snapshot 返回 ok=false，调用方按「直接引用」处理 ——
// 不能虚构一个距离，否则概率会算在错误档位上。
func TestQuoteCounterWindowEviction(t *testing.T) {
	counter := chat.NewQuoteCounter()
	base := time.Unix(1700000000, 0).UnixNano()

	for i := 0; i < 100; i++ {
		counter.Record("m"+itoa(i), "u", base+int64(i))
	}
	// 第 100 条压掉最老的一条。
	counter.Record("m100", "u", base+100)

	if _, ok := counter.Snapshot("m0", base+200); ok {
		t.Errorf("m0 已被淘汰，不该返回 ok")
	}
	snap, ok := counter.Snapshot("m1", base+200)
	if !ok {
		t.Fatal("m1 仍在窗口内")
	}
	if snap.TargetDistance == nil || *snap.TargetDistance != 99 {
		t.Errorf("m1 距离 = %v，期望 99", snap.TargetDistance)
	}
}

// TestQuoteCounterEmptyMessageIDSkipped 验证空 ID 不占序号。
func TestQuoteCounterEmptyMessageIDSkipped(t *testing.T) {
	counter := chat.NewQuoteCounter()
	base := time.Unix(1700000000, 0).UnixNano()

	counter.Record("", "u", base)
	counter.Record("m1", "u", base+1)

	snap, ok := counter.Snapshot("m1", base+2)
	if !ok {
		t.Fatal("m1 应存在")
	}
	if snap.TargetDistance == nil || *snap.TargetDistance != 0 {
		t.Errorf("空 ID 不该占序号，距离 = %v，期望 0", snap.TargetDistance)
	}
}

// TestDecideQuoteWithRoll 用注入的随机数验证阈值判断，避开随机性。
func TestDecideQuoteWithRoll(t *testing.T) {
	dist := 1
	ctx := &chat.QuoteContext{
		TargetDistance:   &dist,
		PreviousSenderID: "bot",
		PreviousSeconds:  1,
		PreviousBurst:    1,
	}

	// 目标不在窗口内：按参考实现直接引用，概率记 1。
	should, p, err := chat.DecideQuote(nil, "bot", "你好", chat.QuoteOccasionally, false)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if !should || p != 1.0 {
		t.Errorf("窗口外应直接引用，实际 should=%v p=%v", should, p)
	}

	// never 档位：永远不引用，概率 0。
	should, p, err = chat.DecideQuote(ctx, "bot", "你好", chat.QuoteNever, true)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if should || p != 0 {
		t.Errorf("never 档位不该引用，实际 should=%v p=%v", should, p)
	}

	// 未知档位必须报错，而不是静默不引用。
	if _, _, err := chat.DecideQuote(ctx, "bot", "你好", chat.QuoteFrequency("sometimes"), true); err == nil {
		t.Errorf("未知档位应报错")
	}
}

// TestRecordQuoteUsesReceivedAt 验证入站消息的时间戳来源。
//
// 间隔用的是消息自带的 received_at，不是「记录时刻」——
// 恢复历史时两者能差好几天，用错会让公式里的「上一句是不是刚说的」永远成立。
func TestRecordQuoteUsesReceivedAt(t *testing.T) {
	counter := chat.NewQuoteCounter()
	at := time.Unix(1700000000, 0)
	recordQuote(counter, &domain.Message{MessageID: "m1", UserID: "u", ReceivedAt: at})
	recordQuote(counter, &domain.Message{MessageID: "m2", UserID: "u2", ReceivedAt: at.Add(5 * time.Second)})

	snap, ok := counter.Snapshot("m1", at.Add(10*time.Second).UnixNano())
	if !ok {
		t.Fatal("m1 应存在")
	}
	// m2 的 received_at 距 now 5 秒。
	if math.Abs(snap.PreviousSeconds-5) > 1e-9 {
		t.Errorf("间隔 = %v 秒，期望 5（说明用的是 received_at）", snap.PreviousSeconds)
	}
}

// TestRecordInboundAndOutboundQuote 验证入站/出站都喂给同一个计数器。
//
// 出站漏记的后果：下一轮算「上一句是不是我说的」永远是否，
// 于是引用概率一直在「别人刚说完」那一档，引用行为整体偏移。
func TestRecordInboundAndOutboundQuote(t *testing.T) {
	r := &Runtime{accountID: "bot-1", quoteCounter: chat.NewQuoteCounter()}
	base := time.Unix(1700000000, 0)

	r.recordInboundQuote(&domain.Message{MessageID: "m1", UserID: "u1", ReceivedAt: base})
	r.recordInboundQuote(&domain.Message{MessageID: "m2", UserID: "u2", ReceivedAt: base.Add(time.Second)})

	if got := r.quoteCounter.Snapshot; got == nil {
		t.Fatal("计数器不该为空")
	}
	snap, ok := r.quoteCounter.Snapshot("m1", base.Add(2*time.Second).UnixNano())
	if !ok || snap.TargetDistance == nil || *snap.TargetDistance != 1 {
		t.Errorf("入站消息没被记录：%+v", snap)
	}

	// 出站消息记在机器人自己名下。
	r.recordOutboundQuote([]string{"out-1", "", "out-2"})
	snap, ok = r.quoteCounter.Snapshot("out-2", time.Now().UnixNano())
	if !ok {
		t.Fatal("出站消息应被记录（空 ID 跳过，其余都记）")
	}
	if snap.PreviousSenderID != "bot-1" {
		t.Errorf("出站消息的发送者 = %q，期望 bot-1", snap.PreviousSenderID)
	}

	// 没有计数器时所有记录函数都必须是空操作（不能 panic）。
	bare := &Runtime{}
	bare.recordInboundQuote(&domain.Message{MessageID: "x"})
	bare.recordOutboundQuote([]string{"y"})
	if got := counterOf(bare); got != nil {
		t.Errorf("counterOf 应返回 nil，实际 %v", got)
	}
}

// TestDecideQuoteInvalidFrequencyLogsAndSkips 覆盖配置非法时的兜底。
//
// 配置写错只能跳过引用并记日志：随机乱引用比不引用难排查得多。
func TestDecideQuoteInvalidFrequencyLogsAndSkips(t *testing.T) {
	r := &Runtime{
		log:          logx.Get("agent.test"),
		accountID:    "bot-1",
		quoteCounter: chat.NewQuoteCounter(),
		history:      []*domain.Message{{MessageID: "m1", UserID: "u1", ReceivedAt: time.Now()}},
		cfg:          config.Wrap(config.Default()),
	}
	// 直接绕过 quoteFrequency 的校验，模拟「配置在运行期被改坏」。
	// 走 Store.Update 而不是就地改字段：就地改会改到共享快照上，
	// 而这个测试之后的其它用例还在用同一份。
	if err := r.cfg.Update(func(c *config.Config) error {
		c.Chat.Style.GroupQuoteFrequency = "sometimes"
		return nil
	}); err != nil {
		t.Fatalf("改配置失败: %v", err)
	}
	r.isGroup = true

	// quoteFrequency 会把非法值收敛成 never，所以这一步不该引用。
	if got := r.decideQuote("m1", "你好"); !got.IsZero() {
		t.Errorf("非法档位不该引用，实际 %q", got.RefIdx)
	}
}
