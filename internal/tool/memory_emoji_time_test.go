package tool

// 本文件覆盖 send_emoji / show_emoji_list 与时间解析。

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestSendEmojiToolValidation 覆盖参数校验。
func TestSendEmojiToolValidation(t *testing.T) {
	cases := []struct {
		name         string
		args         string
		wantContains string
	}{
		{"emotion 为空", `{"emotion":"   "}`, "emotion 不能为空"},
		{"缺 emotion 字段", `{}`, "emotion 不能为空"},
		{"参数非法", `{`, "参数解析失败"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rt := newFakeRuntime()
			res, err := NewSendEmojiTool().Execute(context.Background(), invJSON("send_emoji", c.args), rt)
			if err != nil {
				t.Fatalf("不应返回 error: %v", err)
			}
			if !res.IsError {
				t.Fatalf("应返回错误结果，实际 %+v", res)
			}
			if !strings.Contains(res.Content, c.wantContains) {
				t.Errorf("错误内容 = %q，期望包含 %q", res.Content, c.wantContains)
			}
			if len(rt.pickEmojiCalls) != 0 {
				t.Error("校验失败时不应去挑选表情")
			}
		})
	}
}

// TestSendEmojiToolPickNotFoundIsSuccess 是这条测试的重点：
// 选不到表情包必须返回成功结果，否则模型会以为自己操作错了然后反复重试。
func TestSendEmojiToolPickNotFoundIsSuccess(t *testing.T) {
	rt := newFakeRuntime()
	rt.pickEmojiFn = func(string) (EmojiRef, bool) { return EmojiRef{}, false }

	res, err := NewSendEmojiTool().Execute(context.Background(),
		invJSON("send_emoji", `{"emotion":"无语"}`), rt)
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if res.IsError {
		t.Fatalf("选不到表情不应是错误结果，实际 %+v", res)
	}
	if !strings.Contains(res.Content, "本次就不发了") {
		t.Errorf("结果内容 = %q，期望说明本次不发", res.Content)
	}
	if len(rt.sendEmojiCalls) != 0 {
		t.Error("选不到时不应调用 SendEmoji")
	}
}

// TestSendEmojiToolSuccess 覆盖正常发送路径。
func TestSendEmojiToolSuccess(t *testing.T) {
	rt := newFakeRuntime()
	rt.pickEmojiFn = func(emotion string) (EmojiRef, bool) {
		return EmojiRef{Hash: "h1", Path: "data/emoji/h1.gif", Description: "无语猫猫"}, true
	}
	rt.sendEmojiFn = func(EmojiRef) ([]string, error) { return []string{"m-e1", "m-e2"}, nil }

	res, err := NewSendEmojiTool().Execute(context.Background(),
		invJSON("send_emoji", `{"emotion":" 无语 "}`), rt)
	if err != nil || res.IsError {
		t.Fatalf("不应失败: %v / %+v", err, res)
	}
	if len(rt.pickEmojiCalls) != 1 || rt.pickEmojiCalls[0] != "无语" {
		t.Errorf("PickEmoji 调用 = %v，期望传入去空白的情绪词", rt.pickEmojiCalls)
	}
	if len(rt.sendEmojiCalls) != 1 || rt.sendEmojiCalls[0].Hash != "h1" {
		t.Errorf("SendEmoji 调用 = %+v，期望传入选中的表情", rt.sendEmojiCalls)
	}
	for _, want := range []string{"无语猫猫", "m-e1", "m-e2"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("结果内容 = %q，期望包含 %q", res.Content, want)
		}
	}
}

// TestSendEmojiToolDescriptionFallback 校验描述为空时用情绪词。
func TestSendEmojiToolDescriptionFallback(t *testing.T) {
	rt := newFakeRuntime()
	rt.pickEmojiFn = func(string) (EmojiRef, bool) { return EmojiRef{Hash: "h1"}, true }
	rt.sendEmojiFn = func(EmojiRef) ([]string, error) { return nil, nil }

	res, err := NewSendEmojiTool().Execute(context.Background(),
		invJSON("send_emoji", `{"emotion":"乐"}`), rt)
	if err != nil || res.IsError {
		t.Fatalf("不应失败: %v / %+v", err, res)
	}
	if !strings.Contains(res.Content, "已发送表情包：乐") {
		t.Errorf("结果内容 = %q，期望用情绪词兜底", res.Content)
	}
}

// TestSendEmojiToolSendError 校验发送失败报错结果。
func TestSendEmojiToolSendError(t *testing.T) {
	rt := newFakeRuntime()
	rt.pickEmojiFn = func(string) (EmojiRef, bool) { return EmojiRef{Hash: "h1"}, true }
	rt.sendEmojiFn = func(EmojiRef) ([]string, error) { return nil, errFake }

	res, err := NewSendEmojiTool().Execute(context.Background(),
		invJSON("send_emoji", `{"emotion":"无语"}`), rt)
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Content, "发送表情包失败") {
		t.Errorf("结果 = %+v，期望发送失败的错误结果", res)
	}
}

// TestShowEmojiListTool 覆盖空库与有库两种情况。
func TestShowEmojiListTool(t *testing.T) {
	t.Run("空库", func(t *testing.T) {
		rt := newFakeRuntime()
		res, err := NewShowEmojiListTool().Execute(context.Background(), invJSON("show_emoji_list", `{}`), rt)
		if err != nil || res.IsError {
			t.Fatalf("空库不应报错: %v / %+v", err, res)
		}
		if !strings.Contains(res.Content, "表情包库目前是空的") {
			t.Errorf("结果内容 = %q，期望说明库是空的", res.Content)
		}
	})

	t.Run("有库", func(t *testing.T) {
		rt := newFakeRuntime()
		rt.pickEmojiFn = func(string) (EmojiRef, bool) {
			return EmojiRef{Hash: "h1", Description: "无语"}, true
		}
		res, err := NewShowEmojiListTool().Execute(context.Background(), invJSON("show_emoji_list", `{}`), rt)
		if err != nil || res.IsError {
			t.Fatalf("不应失败: %v / %+v", err, res)
		}
		if !strings.Contains(res.Content, "无语") {
			t.Errorf("结果内容 = %q，期望带上示例标签", res.Content)
		}
	})
}

// TestParseQueryTime 表驱动覆盖时间解析。
func TestParseQueryTime(t *testing.T) {
	loc := time.Local

	cases := []struct {
		name    string
		raw     string
		isEnd   bool
		want    time.Time
		wantErr bool
	}{
		{
			name: "空串返回 0",
			raw:  "",
			want: time.Unix(0, 0),
		},
		{
			name: "纯空白也返回 0",
			raw:  "   ",
			want: time.Unix(0, 0),
		},
		{
			name: "斜杠日期",
			raw:  "2026/10/21",
			want: time.Date(2026, 10, 21, 0, 0, 0, 0, loc),
		},
		{
			name: "斜杠日期带时间",
			raw:  "2026/10/21 13:45",
			want: time.Date(2026, 10, 21, 13, 45, 0, 0, loc),
		},
		{
			name: "ISO 日期",
			raw:  "2026-10-21",
			want: time.Date(2026, 10, 21, 0, 0, 0, 0, loc),
		},
		{
			name: "ISO 日期带时间",
			raw:  "2026-10-21 13:45",
			want: time.Date(2026, 10, 21, 13, 45, 0, 0, loc),
		},
		{
			// 只给日期且是结束边界时必须取当天最后一秒，
			// 否则「查 10 月 21 日」会把当天所有记忆都排除。
			name:  "结束边界只给日期取当天末",
			raw:   "2026/10/21",
			isEnd: true,
			want:  time.Date(2026, 10, 21, 23, 59, 59, 0, loc),
		},
		{
			name:  "结束边界给了时间就不改",
			raw:   "2026/10/21 13:45",
			isEnd: true,
			want:  time.Date(2026, 10, 21, 13, 45, 0, 0, loc),
		},
		{
			name:    "非法格式报错",
			raw:     "昨天",
			wantErr: true,
		},
		{
			name:    "越界日期报错",
			raw:     "2026/13/45",
			wantErr: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseQueryTime(c.raw, c.isEnd)
			if c.wantErr {
				if err == nil {
					t.Fatalf("期望报错，实际得到 %d", got)
				}
				if !strings.Contains(err.Error(), "不是合法时间") {
					t.Errorf("错误信息 = %q，期望说明格式非法", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("解析 %q 失败: %v", c.raw, err)
			}
			if got != c.want.Unix() {
				t.Errorf("parseQueryTime(%q, %v) = %d (%s)，期望 %d (%s)",
					c.raw, c.isEnd, got, time.Unix(got, 0).Format(time.DateTime),
					c.want.Unix(), c.want.Format(time.DateTime))
			}
		})
	}
}

// TestParseQueryTimeEndIsOneSecondBeforeNextDay 单独强调结束边界的算法：
// 23:59:59 而不是 00:00:00（后者会把当天整段排除）。
func TestParseQueryTimeEndIsOneSecondBeforeNextDay(t *testing.T) {
	end, err := parseQueryTime("2026/10/21", true)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	nextDay, err := parseQueryTime("2026/10/22", false)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if nextDay-end != 1 {
		t.Errorf("结束边界 %d 与次日零点 %d 相差 %d 秒，期望 1 秒", end, nextDay, nextDay-end)
	}
}

// TestSprintfImpl 校验统一格式化入口。
func TestSprintfImpl(t *testing.T) {
	if got := sprintfImpl("%s-%d", "a", 2); got != "a-2" {
		t.Errorf("sprintfImpl() = %q，期望 %q", got, "a-2")
	}
	if got := sprintfImpl("无参"); got != "无参" {
		t.Errorf("sprintfImpl() = %q，期望 %q", got, "无参")
	}
}
