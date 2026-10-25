package tool

// 本文件覆盖 query_memory 与 FormatMemoryHits。

import (
	"context"
	"strings"
	"testing"
)

// TestQueryMemoryToolValidation 表驱动覆盖参数校验：
// 模型最容易犯的就是「什么参数都不给就调用」，必须当场拒绝而不是查全库。
func TestQueryMemoryToolValidation(t *testing.T) {
	cases := []struct {
		name         string
		args         string
		wantContains string
	}{
		{
			name:         "缺 query 且缺时间",
			args:         `{}`,
			wantContains: "需要提供 query",
		},
		{
			name:         "只有空白的 query",
			args:         `{"query":"   "}`,
			wantContains: "需要提供 query",
		},
		{
			name:         "mode=time 无时间",
			args:         `{"mode":"time","query":"上次说的"}`,
			wantContains: "mode=time 时必须提供 time_start 或 time_end",
		},
		{
			name:         "mode 枚举非法",
			args:         `{"mode":"episode","query":"上次说的"}`,
			wantContains: "不支持的检索模式",
		},
		{
			name:         "time_start 格式非法",
			args:         `{"query":"x","time_start":"昨天"}`,
			wantContains: "time_start 格式无效",
		},
		{
			name:         "time_end 格式非法",
			args:         `{"query":"x","time_end":"2026/13/45"}`,
			wantContains: "time_end 格式无效",
		},
		{
			name:         "参数不是合法 JSON",
			args:         `{`,
			wantContains: "参数解析失败",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rt := newFakeRuntime()
			res, err := NewQueryMemoryTool().Execute(context.Background(), invJSON("query_memory", c.args), rt)
			if err != nil {
				t.Fatalf("工具错误应体现在结果里: %v", err)
			}
			if !res.IsError {
				t.Fatalf("应返回错误结果，实际 %+v", res)
			}
			if !strings.Contains(res.Content, c.wantContains) {
				t.Errorf("错误内容 = %q，期望包含 %q", res.Content, c.wantContains)
			}
			if len(rt.searchCalls) != 0 {
				t.Error("校验失败时不应真的去检索")
			}
		})
	}
}

// TestQueryMemoryToolPassesQuery 覆盖正常路径的参数透传。
func TestQueryMemoryToolPassesQuery(t *testing.T) {
	rt := newFakeRuntime()
	rt.searchFn = func(MemoryQuery) (MemoryResult, error) {
		return MemoryResult{Mode: "search", Hits: []MemoryHit{{Summary: "上次说想学吉他"}}}, nil
	}

	args := `{
		"query": " 吉他 ",
		"mode": " SEARCH ",
		"limit": 3,
		"person_name": " 小明 ",
		"time_start": "2026/10/21",
		"time_end": "2026/10/21"
	}`
	res, err := NewQueryMemoryTool().Execute(context.Background(), invJSON("query_memory", args), rt)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if res.IsError {
		t.Fatalf("不应失败: %q", res.Content)
	}
	if len(rt.searchCalls) != 1 {
		t.Fatalf("SearchMemory 应被调用 1 次，实际 %d 次", len(rt.searchCalls))
	}

	req := rt.searchCalls[0]
	if req.Query != "吉他" {
		t.Errorf("Query = %q，期望 %q", req.Query, "吉他")
	}
	if req.Mode != "search" {
		t.Errorf("Mode = %q，期望 %q（应小写化）", req.Mode, "search")
	}
	if req.Limit != 3 {
		t.Errorf("Limit = %d，期望 3", req.Limit)
	}
	if req.PersonName != "小明" {
		t.Errorf("PersonName = %q，期望 %q", req.PersonName, "小明")
	}
	if req.TimeStart == 0 || req.TimeEnd == 0 {
		t.Errorf("时间范围未解析: start=%d end=%d", req.TimeStart, req.TimeEnd)
	}
	if req.TimeEnd <= req.TimeStart {
		t.Errorf("结束时间 %d 应晚于起始时间 %d（只给日期时结束要取当天末）", req.TimeEnd, req.TimeStart)
	}
	if !strings.Contains(res.Content, "想学吉他") {
		t.Errorf("结果内容 = %q，期望包含命中摘要", res.Content)
	}
}

// TestQueryMemoryToolDefaultMode 校验 mode 缺省为 search。
func TestQueryMemoryToolDefaultMode(t *testing.T) {
	rt := newFakeRuntime()
	rt.searchFn = func(MemoryQuery) (MemoryResult, error) {
		return MemoryResult{}, nil
	}
	if _, err := NewQueryMemoryTool().Execute(context.Background(),
		invJSON("query_memory", `{"query":"吉他"}`), rt); err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if len(rt.searchCalls) != 1 || rt.searchCalls[0].Mode != "search" {
		t.Errorf("SearchMemory 调用 = %+v，期望 Mode=search", rt.searchCalls)
	}
}

// TestQueryMemoryToolEmptyHits 校验空命中与降级的文案不同。
func TestQueryMemoryToolEmptyHits(t *testing.T) {
	cases := []struct {
		name         string
		fallback     bool
		wantContains string
	}{
		{"无降级", false, "未找到匹配的长期记忆。"},
		{"有降级", true, "已尝试降级检索"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rt := newFakeRuntime()
			rt.searchFn = func(MemoryQuery) (MemoryResult, error) {
				return MemoryResult{Mode: "time", Fallback: c.fallback}, nil
			}
			res, err := NewQueryMemoryTool().Execute(context.Background(),
				invJSON("query_memory", `{"query":"吉他"}`), rt)
			if err != nil || res.IsError {
				t.Fatalf("空命中不是错误: %v / %+v", err, res)
			}
			if !strings.Contains(res.Content, c.wantContains) {
				t.Errorf("结果内容 = %q，期望包含 %q", res.Content, c.wantContains)
			}
		})
	}
}

// TestQueryMemoryToolRuntimeError 校验检索失败被转成错误结果。
func TestQueryMemoryToolRuntimeError(t *testing.T) {
	rt := newFakeRuntime()
	rt.searchFn = func(MemoryQuery) (MemoryResult, error) { return MemoryResult{}, errFake }

	res, err := NewQueryMemoryTool().Execute(context.Background(),
		invJSON("query_memory", `{"query":"吉他"}`), rt)
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Content, "长期记忆检索失败") {
		t.Errorf("结果 = %+v，期望检索失败的错误结果", res)
	}
}

// TestFormatMemoryHits 覆盖空命中、多条命中、TimeRange 为空三种情况。
func TestFormatMemoryHits(t *testing.T) {
	cases := []struct {
		name         string
		result       MemoryResult
		wantContains []string
		wantMissing  []string
	}{
		{
			name:         "空 hits",
			result:       MemoryResult{Mode: "search"},
			wantContains: []string{"【长期记忆-内部参考】", "命中：0 条", "模式：search"},
		},
		{
			name: "多条 hits",
			result: MemoryResult{
				Mode: "hybrid",
				Hits: []MemoryHit{
					{Summary: "小明喜欢吉他", TimeRange: "2026-10-01 至 2026-10-02"},
					{Summary: "约好周末去看电影", TimeRange: ""},
				},
			},
			wantContains: []string{
				"命中：2 条（模式：hybrid）",
				"1. （2026-10-01 至 2026-10-02）小明喜欢吉他",
				"2. 约好周末去看电影",
			},
			// TimeRange 为空时不能出现空括号。
			wantMissing: []string{"2. （）"},
		},
		{
			name:         "Mode 为空时不输出模式括号",
			result:       MemoryResult{Hits: []MemoryHit{{Summary: "一条记忆"}}},
			wantContains: []string{"命中：1 条\n"},
			wantMissing:  []string{"模式："},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := FormatMemoryHits(c.result)
			for _, want := range c.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("结果缺少 %q，实际:\n%s", want, got)
				}
			}
			for _, bad := range c.wantMissing {
				if strings.Contains(got, bad) {
					t.Errorf("结果不应包含 %q，实际:\n%s", bad, got)
				}
			}
			if strings.HasSuffix(got, "\n") {
				t.Errorf("结果不应以换行结尾:\n%q", got)
			}
		})
	}
}

// TestFormatMemoryHitsCollapsesWhitespace 防的是「摘要里的换行撑爆提示词结构」。
func TestFormatMemoryHitsCollapsesWhitespace(t *testing.T) {
	got := FormatMemoryHits(MemoryResult{
		Hits: []MemoryHit{{Summary: "第一行\n\n第二行\t第三行"}},
	})
	if strings.Contains(got, "\n\n") && !strings.Contains(got, "命中：1 条") {
		t.Fatalf("摘要里的换行未被压缩:\n%q", got)
	}
	if !strings.Contains(got, "1. 第一行 第二行 第三行") {
		t.Errorf("摘要空白未压成单空格，实际:\n%q", got)
	}
}
