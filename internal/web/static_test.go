package web

// 前端静态一致性检查。
//
// 这里不跑浏览器，只做「字符串层面能查出、但浏览器里报错很难定位」的检查：
//   - JS 里 icon('xxx') 引用的图标在 sprite 里没定义 → 界面上是一个空白方块
//   - 导航项没有对应路由 → 点进去是空白页
//   - 路由没有导航入口 → 页面存在但用户永远到不了
//   - 调用了后端没注册的接口 → 控制台 404，页面显示「加载失败」
//   - class 名写错了 → 样式完全不生效，但 DOM 结构看起来是对的
//
// 这些在真实浏览器里都能查，但每次都要人工点一遍；而它们在字符串
// 层面是确定性的，直接写成测试成本更低、回归更稳。

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// 后端注册过的 /api 路径（与 server.go:routes 保持一致）。
//
// 之所以手工维护而不是解析 server.go：解析出来的结果和实际注册行为
// 可能不一致（比如某个 handler 内部又 return 了），手写清单更能表达
// 「这是接口契约」。加新接口时忘了同步，这个测试会失败提醒。
var registeredAPIs = map[string]bool{
	"/api/login":                     true,
	"/api/logout":                    true,
	"/api/status":                    true,
	"/api/overview":                  true,
	"/api/sessions":                  true,
	"/api/sessions/delete":           true,
	"/api/messages/delete":           true,
	"/api/expressions/delete":        true,
	"/api/jargons/delete":            true,
	"/api/emojis/delete":             true,
	"/api/profiles/delete":           true,
	"/api/messages":                  true,
	"/api/chat/session":              true,
	"/api/chat/send":                 true,
	"/api/config":                    true,
	"/api/config/edit":               true,
	"/api/config/save":               true,
	"/api/config/llm":                true,
	"/api/config/llm/save":           true,
	"/api/config/qq":                 true,
	"/api/config/qq/save":            true,
	"/api/config/keywords":           true,
	"/api/config/keywords/save":      true,
	"/api/config/llm/probe":          true,
	"/api/config/llm/probe-provider": true,
	"/api/restart":                   true,
	"/api/sessions/rename":           true,
	"/api/prompts":                   true,
	"/api/backup":                    true,
	"/api/usage":                     true,
	"/api/memory":                    true,
	"/api/memory/delete":             true,
	"/api/profiles":                  true,
	"/api/emojis":                    true,
	"/api/expressions":               true,
	"/api/jargons":                   true,
	"/api/emoji/image":               true,
	"/api/media":                     true,
	"/api/logs":                      true,
	"/api/events":                    true,
}

// readStatic 读一个静态文件的内容。
//
// 统一把 CRLF 折成 LF 再返回：仓库里这几个文件是 CRLF 换行的，
// 而测试里的多行断言写成 "\n.xxx {\n  display: none;"。
// 不折的话跨行断言永远失效 —— 表现是「断言看起来正确，但是一直失败」，
// 而单行断言（".xxx {"）照常通过，很容易误以为检查项本身写错了。
func readStatic(t *testing.T, name string) string {
	t.Helper()
	data, err := staticFS.ReadFile("static/" + name)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", name, err)
	}
	return strings.ReplaceAll(string(data), "\r\n", "\n")
}

// stripHTMLComments 去掉 HTML 注释。
//
// 检查图标引用时必须先去掉注释：注释里会举例写「用 use 引用 #i-xxx」，
// 那会被当成真的引用，报出「引用了不存在的图标」这种假失败。
func stripHTMLComments(s string) string {
	re := regexp.MustCompile(`(?s)<!--.*?-->`)
	return re.ReplaceAllString(s, "")
}

// TestStaticIconReferencesResolve 防：JS 引用了未定义的图标。
//
// 图标缺失的表现是「那个位置什么也没有」，浏览器不报错、控制台干净，
// 只能靠肉眼看出来少了东西。用过 sprite 的地方几乎都踩过这个坑。
func TestStaticIconReferencesResolve(t *testing.T) {
	html := readStatic(t, "index.html")
	js := readStatic(t, "app.js")

	defined := map[string]bool{}
	for _, m := range regexp.MustCompile(`<symbol id="i-([a-zA-Z0-9-]+)"`).FindAllStringSubmatch(html, -1) {
		defined[m[1]] = true
	}
	if len(defined) == 0 {
		t.Fatal("index.html 里没有找到任何图标定义")
	}

	// JS 侧：icon('xxx')
	for _, m := range regexp.MustCompile(`icon\('([a-zA-Z0-9-]+)'`).FindAllStringSubmatch(js, -1) {
		if !defined[m[1]] {
			t.Errorf("app.js 引用了未定义的图标 #i-%s", m[1])
		}
	}

	// HTML 侧：<use href="#i-xxx">
	clean := stripHTMLComments(html)
	for _, m := range regexp.MustCompile(`href="#i-([a-zA-Z0-9-]+)"`).FindAllStringSubmatch(clean, -1) {
		if !defined[m[1]] {
			t.Errorf("index.html 引用了未定义的图标 #i-%s", m[1])
		}
	}
}

// TestStaticRoutesMatchNav 防：导航与路由两边对不上。
//
// 两种情况都会让用户走到「空白页」：
//   - 有导航项但 ROUTES 里没这个键 → navigate() 拿不到 route，直接崩
//   - 有路由但没导航项 → 页面真实存在，但没有任何入口
//
// 结构上现在是单一导航层：侧栏分组菜单 + 页内页签。
//
// 两种情况都会让用户走到「空白页」：
//   - 有导航项但 ROUTES 里没这个键 → navigate() 拿不到 route，直接崩
//   - 有路由但没导航项 → 页面真实存在，但没有任何入口
//
// 日志与用量统计以前走「顶栏工作区页签」这条第二类入口（侧栏切过去
// 就消失），现在被收进侧栏的「运行记录」组，所以**每个路由都必须在
// 侧栏里有一条 data-nav**，不再有例外。
func TestStaticRoutesMatchNav(t *testing.T) {
	html := readStatic(t, "index.html")
	js := readStatic(t, "app.js")

	// 只从 ROUTES 对象里解析路由名。
	//
	// 早先的正则是「4 个空格缩进的 key: {」，它会连带匹配到
	// TASK_META 那种同样是 4 空格的对象字面量，把 planner / replyer
	// 这些任务名当成路由，报出一堆「没有入口」的假失败。
	routes := map[string]bool{}
	if m := regexp.MustCompile(`(?s)const ROUTES = \{(.*?)
  \};`).FindStringSubmatch(js); m != nil {
		for _, r := range regexp.MustCompile(`(?m)^    ([a-z]+): \{`).FindAllStringSubmatch(m[1], -1) {
			routes[r[1]] = true
		}
	}
	if len(routes) == 0 {
		t.Fatal("app.js 里没有解析到任何路由（ROUTES 结构可能改了）")
	}

	navs := map[string]bool{}
	for _, m := range regexp.MustCompile(`data-nav="([a-z]+)"`).FindAllStringSubmatch(html, -1) {
		navs[m[1]] = true
	}

	for n := range navs {
		if !routes[n] {
			t.Errorf("导航项 %q 没有对应的路由", n)
		}
	}
	for r := range routes {
		if !navs[r] {
			t.Errorf("路由 %q 没有任何入口（侧栏里没有 data-nav=\"%s\"）", r, r)
		}
	}
}

// TestStaticAPICallsAreRegistered 防：前端调了后端不存在的接口。
//
// 表现是页面上「加载失败：HTTP 404」，而后端日志干干净净 ——
// 排查时容易以为是网络或权限问题。
func TestStaticAPICallsAreRegistered(t *testing.T) {
	js := readStatic(t, "app.js")

	// 抓 api('/api/xxx') 与 api(`/api/xxx?...`) 两种写法
	re := regexp.MustCompile("api\\(`?'?(/api/[a-z/]+)")
	found := 0
	for _, m := range re.FindAllStringSubmatch(js, -1) {
		found++
		if !registeredAPIs[m[1]] {
			t.Errorf("app.js 调用了未注册的接口 %s", m[1])
		}
	}
	if found == 0 {
		t.Fatal("app.js 里没有解析到任何 api() 调用（写法可能变了）")
	}
}

// TestStaticClassNamesDefined 防：JS 里写了 class 但样式表里没有。
//
// 这种错完全静默：DOM 结构正确、文本正确，只是那块区域没有样式。
// 看起来像「样式失效」，实际是类名拼错了。
func TestStaticClassNamesDefined(t *testing.T) {
	html := readStatic(t, "index.html")
	js := readStatic(t, "app.js")
	css := readStatic(t, "style.css")

	// 只检查 el(..., { class: '...' }) 里的静态类名。
	// 模板串拼出来的（class: 'msg' + (x ? ' self' : '')）不在此列，
	// 它们的片段会被下面的循环单独覆盖到 —— 那些片段恰好也都是静态词。
	re := regexp.MustCompile(`class: '([a-zA-Z0-9 _-]+)'`)
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(js, -1) {
		for _, c := range strings.Fields(m[1]) {
			seen[c] = true
		}
	}
	if len(seen) == 0 {
		t.Fatal("app.js 里没有解析到任何静态 class")
	}

	for c := range seen {
		if !strings.Contains(css, "."+c) && !strings.Contains(html, `class="`+c) {
			t.Errorf("class .%s 在 style.css 和 index.html 里都没有定义", c)
		}
	}
}

// TestStaticHiddenRuleHonoured 防：登录后界面不切换。
//
// index.html 用 hidden 属性切换登录页与主界面。浏览器默认的
// [hidden] { display: none } 只是普通声明，被 .app / .login 的
// display 覆盖后就不生效了，于是登录页和主界面叠在一起。
// 这个 bug 纯看 Go 代码发现不了，只能检查 CSS。
func TestStaticHiddenRuleHonoured(t *testing.T) {
	css := readStatic(t, "style.css")

	i := strings.Index(css, "\n[hidden]")
	if i < 0 {
		t.Fatal("style.css 缺少行首的 [hidden] 规则")
	}
	end := strings.Index(css[i:], "}")
	if end < 0 {
		t.Fatal("style.css 的 [hidden] 规则没有闭合")
	}
	rule := css[i : i+end]

	if !strings.Contains(rule, "display: none") {
		t.Errorf("[hidden] 规则里没有 display: none: %q", rule)
	}
	if !strings.Contains(rule, "!important") {
		t.Errorf("[hidden] 缺少 !important，会被 .app / .login 的 display 覆盖: %q", rule)
	}
}

// TestStaticNoInlineHTMLWithUserData 防：把外部数据拼进 innerHTML。
//
// 消息正文、记忆摘要、画像、黑话含义都是群里任何人能影响的内容。
// 一旦拼进 innerHTML，就是存储型 XSS。
//
// 允许 innerHTML 的唯一场景是写入「代码里写死的静态图标」，
// 所以这里要求 innerHTML 的赋值必须是字面量，不能是变量或拼接。
func TestStaticNoInlineHTMLWithUserData(t *testing.T) {
	js := readStatic(t, "app.js")

	// 找 html: 后面跟的不是字符串字面量的用法
	re := regexp.MustCompile(`html:\s*([^,\n]+)`)
	for _, m := range re.FindAllStringSubmatch(js, -1) {
		v := strings.TrimSpace(m[1])
		quoted := (strings.HasPrefix(v, "'") && strings.Contains(v, "'")) ||
			(strings.HasPrefix(v, `"`) && strings.Contains(v, `"`))
		if !quoted {
			t.Errorf("html 赋值不是字面量，可能把外部数据拼进了 innerHTML: %s", v)
		}
	}

	// 直接操作 innerHTML 的地方也不允许带插值
	for _, line := range strings.Split(js, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.Contains(trimmed, "innerHTML") {
			continue
		}
		if strings.Contains(trimmed, "`") || strings.Contains(trimmed, " + ") {
			t.Errorf("innerHTML 里出现插值或拼接，检查是否会注入外部数据: %s", trimmed)
		}
	}
}

// TestStaticHasViewportMeta 防：移动端页面被当成桌面页缩放。
func TestStaticHasViewportMeta(t *testing.T) {
	html := readStatic(t, "index.html")
	if !strings.Contains(html, `name="viewport"`) {
		t.Error("index.html 缺少 viewport meta，移动端会按 980px 渲染再缩放")
	}
	if !strings.Contains(html, "width=device-width") {
		t.Error("viewport 没写 width=device-width")
	}
}

// TestStaticThemeSupportsBothModes 防：只做了一套配色。
//
// 深色模式不是「加个 class」就行：所有颜色变量都要有覆盖，
// 否则深色下会出现白底黑字的区块。这里检查变量集合是否两边一致。
func TestStaticThemeSupportsBothModes(t *testing.T) {
	css := readStatic(t, "style.css")

	rootBlock := extractCSSBlock(css, ":root {")
	darkBlock := extractCSSBlock(css, `[data-theme="dark"] {`)
	if rootBlock == "" {
		t.Fatal("style.css 里没有 :root 变量块")
	}
	if darkBlock == "" {
		t.Fatal(`style.css 里没有 [data-theme="dark"] 变量块`)
	}

	varsOf := func(block string) map[string]bool {
		out := map[string]bool{}
		for _, m := range regexp.MustCompile(`(--[a-z0-9-]+):`).FindAllStringSubmatch(block, -1) {
			out[m[1]] = true
		}
		return out
	}

	light, dark := varsOf(rootBlock), varsOf(darkBlock)
	// 只要求深色覆盖那些「与配色相关」的变量，尺寸/字体类不必重复定义。
	for v := range light {
		if strings.HasPrefix(v, "--sidebar-w") || strings.HasPrefix(v, "--topbar-h") ||
			strings.HasPrefix(v, "--font") || strings.HasPrefix(v, "--mono") ||
			strings.HasPrefix(v, "--radius") {
			continue
		}
		if !dark[v] {
			t.Errorf("深色模式没有覆盖变量 %s", v)
		}
	}
}

// extractCSSBlock 取 css 里以 marker 开头的那段声明，到配对的 } 为止。
func extractCSSBlock(css, marker string) string {
	i := strings.Index(css, marker)
	if i < 0 {
		return ""
	}
	start := i + len(marker)
	depth := 1
	for j := start; j < len(css); j++ {
		switch css[j] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return css[start:j]
			}
		}
	}
	return ""
}

// TestStaticMessageRendersAvatarAndBubble 防：消息渲染退回「一行纯文本」。
//
// 没有头像与气泡时，一屏十几条消息会糊成一片，分不清谁在说话。
// 这类退化不会报任何错，只能在浏览器里肉眼看出来，所以用字符串检查守住。
func TestStaticMessageRendersAvatarAndBubble(t *testing.T) {
	js := readStatic(t, "app.js")
	css := readStatic(t, "style.css")

	// 渲染器必须造头像与气泡。
	for _, want := range []string{"renderAvatar(", "class: 'bubble'", "avatar-gap"} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js 里缺少 %q：消息渲染可能退回了无头像/无气泡的形态", want)
		}
	}

	// 样式必须真的定义了三者。
	for _, want := range []string{".avatar {", ".bubble {", ".avatar-gap {"} {
		if !strings.Contains(css, want) {
			t.Errorf("style.css 里缺少 %s 规则", want)
		}
	}
}

// TestStaticAtOnlyMessageKeepsSender 防：只 @ 不说话的消息被降级成
// 居中系统事件，于是「谁 @ 了谁」这个信息在界面上消失。
//
// 这条是真实现象：截图里一条「@麦麦」显示成居中小字，没有任何归属。
// 参考实现（MaiBot MessageRenderer）把 at 段当普通消息段渲染进气泡，
// 从不把它当成系统事件。
func TestStaticAtOnlyMessageKeepsSender(t *testing.T) {
	js := readStatic(t, "app.js")

	// 旧的 `msg.event` 分支会把「没有正文」的消息变成居中事件。
	// 只要它还在，就说明逻辑被改回去了。
	if strings.Contains(js, "classList.add('event')") {
		t.Error("app.js 里仍有把消息降级成居中事件的逻辑（msg.event）")
	}
	if strings.Contains(js, "'.msg.event'") {
		t.Error("app.js 里仍在用 .msg.event 样式")
	}

	// 声明的三条约定必须还在，防止后来人「简化」掉合并逻辑。
	for _, want := range []string{"sameAsPrev", "msgInitial(", "avatarTone("} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js 里缺少 %q", want)
		}
	}
}

// TestStaticMediaRendersImage 防：表情包/图片退回显示「[表情]」文字。
//
// 参考实现是把媒体段直接渲染成 <img>（dashboard 的 MessageRenderer.tsx），
// 不是一行占位文字。占位只有在拿不到图时才该出现（老数据没 hash、
// 或文件已被清理）。
func TestStaticMediaRendersImage(t *testing.T) {
	js := readStatic(t, "app.js")
	css := readStatic(t, "style.css")

	for _, want := range []string{"function renderMedia(", "class: 'msg-img", "addEventListener('error'"} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js 里缺少 %q：媒体可能退回了纯文字占位", want)
		}
	}
	for _, want := range []string{".msg-img {", ".msg-img-emoji {"} {
		if !strings.Contains(css, want) {
			t.Errorf("style.css 里缺少 %s 规则", want)
		}
	}
}

// TestStaticReasoningShowsThinking 防：推理页只剩折叠的原始 JSON。
//
// 参考实现（dashboard 的 TraceRound/TraceStep）把一轮思考渲染成
// 「竖线串起来的步骤」：Planner 想了什么、然后调了什么工具。
// 只显示 JSON 的话要逐个展开才能看出流程，等于没有可读性。
//
// 渲染器名字与参考实现对齐：PlannerFinalizedCard（maisaka-monitor.tsx:1188）
// 负责一轮里的 Planner 卡片，TraceStep 负责竖线上的一个节点。
func TestStaticReasoningShowsThinking(t *testing.T) {
	js := readStatic(t, "app.js")
	css := readStatic(t, "style.css")

	for _, want := range []string{"renderPlannerStep(", "trace-think", "trace-tool-name", "summarizeArgs("} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js 里缺少 %q", want)
		}
	}
	for _, want := range []string{".trace-step {", ".trace-think {", ".trace-tool {"} {
		if !strings.Contains(css, want) {
			t.Errorf("style.css 里缺少 %s 规则", want)
		}
	}

	// 原始 JSON 必须还在（排查时需要），但默认折叠。
	if !strings.Contains(js, "trace-raw") {
		t.Error("app.js 里缺少原始请求/响应区域，排查时会看不到完整上下文")
	}
}

// TestStaticReasoningInChatTimeline 防：思考被拆回独立页面。
//
// 参考实现把思考插进消息流（主事件账本 timeline，见
// dashboard/src/routes/monitor/use-maisaka-monitor.ts:23 的 TimelineEntry），
// 不是单独一页。用户原话：「会话和推理一起，不是分开不同页面」。
func TestStaticReasoningInChatTimeline(t *testing.T) {
	js := readStatic(t, "app.js")
	html := readStatic(t, "index.html")
	css := readStatic(t, "style.css")

	for _, want := range []string{"function buildTimeline(", "function renderTraceTurn(", "entry.kind === 'reasoning'"} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js 里缺少 %q：思考可能又被拆到独立页面了", want)
		}
	}
	if !strings.Contains(css, ".trace-round {") {
		t.Error("style.css 里缺少 .trace-round 规则")
	}

	// 独立推理页必须彻底删掉：导航项、路由、视图函数都不能残留。
	// 留一个「请去会话页看」的空壳页也是错的 —— 那仍然是一个多余入口。
	if strings.Contains(html, "data-nav=\"reasoning\"") {
		t.Error("index.html 里还有「推理」导航项，独立推理页应该彻底删掉")
	}
	for _, gone := range []string{"viewReasoning", "reasoning: { title:", "/api/reasoning"} {
		if strings.Contains(js, gone) {
			t.Errorf("app.js 里还残留 %q，独立推理页应该彻底删掉", gone)
		}
	}
}

// TestStaticUsageLivesInOverview 守「用量统计在概览页里，不是独立页面」。
//
// 它原来是一条独立路由（#/usage）。归并进概览的理由：
//   - 参考实现把请求数 / Token / 花费三张趋势卡直接放在首页
//     （refs/MaiBot/dashboard/src/routes/home/StatisticsCards.tsx）；
//   - 它回答的是「这台机器现在跑得怎么样」，与首页其它指标是同一个问题。
//
// 这条测试同时守着**不要又拆回去**：拆回去意味着同类数据两个入口，
// 而「哪一个是完整的」在界面上完全看不出来。
func TestStaticUsageLivesInOverview(t *testing.T) {
	js := readStatic(t, "app.js")
	html := readStatic(t, "index.html")

	// 区块必须真的被挂到概览页上，而不只是定义了一个函数。
	if !strings.Contains(js, "page.append(usageSection());") {
		t.Error("概览页没有挂载 usageSection()：先把函数定义了却没挂上去，界面上看不到")
	}
	// 时间范围切换必须在区块内自带（不能靠整页刷新重置用户的选择）。
	for _, want := range []string{"function usageSection(", "[[1, '今天'], [7, '7 天'], [30, '30 天']]"} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js 里缺少 %q", want)
		}
	}
	// 路由与导航项都不该再有用量。
	if strings.Contains(js, "usage: {") {
		t.Error("app.js 里还有 usage 路由，它应该已归并进概览")
	}
	if strings.Contains(html, `data-nav="usage"`) {
		t.Error("index.html 里还有用量导航项")
	}
	// 图标也要一并清掉：留着就是一个永远没人引用的 symbol。
	// （TestStaticIconReferencesResolve 只查「引用了未定义的图标」，
	// 查不出「定义了没人引用的图标」。）
	if strings.Contains(html, `id="i-usage"`) {
		t.Error("index.html 里还留着 i-usage 图标定义")
	}
}

// TestStaticPromptPageShowsAdvancedToggleAndPlaceholders 守提示词页的两块新交互。
//
//   - 高级项开关：11 份模板平铺会让「该改哪一份」变难判断；
//   - 占位符 chip：后端保存时会校验「不能增删占位符」，
//     提前列出来（且能点一下插入）比让用户靠保存失败去猜好得多。
func TestStaticPromptPageShowsAdvancedToggleAndPlaceholders(t *testing.T) {
	js := readStatic(t, "app.js")
	css := readStatic(t, "style.css")

	for _, want := range []string{
		"mdl-side-toggle", "显示高级项（",
		"prompt-ph-chip", "prompt-ph-label", "function renderPlaceholders(",
		"btn.dataset.prompt = it.name",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js 里缺少 %q", want)
		}
	}
	for _, want := range []string{".mdl-side-toggle {", ".prompt-ph-chip {", ".prompt-ph {"} {
		if !strings.Contains(css, want) {
			t.Errorf("style.css 里缺少 %q", want)
		}
	}
}

// TestStaticTraceRoundExpandedByDefault 守「思考块默认展开」。
//
// 参考实现的初始状态是 { all: true }（maisaka-monitor.tsx:2462），
// 也就是所有轮次默认展开。我之前猜成默认折叠，是错的 ——
// 折叠起来只能看到「第 1 轮」四个字，等于把最有信息量的部分藏了。
func TestStaticTraceRoundExpandedByDefault(t *testing.T) {
	js := readStatic(t, "app.js")

	if !strings.Contains(js, "state.traceOpen !== false") {
		t.Error("思考块应当默认展开（state.traceOpen !== false 表示未手动折叠过就展开）")
	}
	if !strings.Contains(js, "body.hidden = !open") {
		t.Error("思考块 body 的可见性应当由 open 决定")
	}

	// 必须用按钮 + aria-expanded，而不是 <details>：
	// 参考实现的 TraceRound 就是一个 button（conversation.tsx:243），
	// 因为它的折叠状态要被外层的「全部展开/收起」统一控制。
	if !strings.Contains(js, "'aria-expanded'") {
		t.Error("思考块应当用 aria-expanded 表达折叠状态（对齐 TraceRound 的 button 实现）")
	}
	if strings.Contains(js, "el('details', { class: 'trace-round' }") {
		t.Error("思考块不该用 <details>，参考实现是 button + aria-expanded")
	}
}

// TestStaticCollapsedToolSummary 守折叠态的摘要形态。
//
// 参考实现折叠时不显示「第 N 轮思考」这种泛泛的标题，
// 而是「工具名 + 状态圆点」的列表（CollapsedPlannerToolSummary，
// maisaka-monitor.tsx:2222）—— 一眼能看出这轮做了什么。
func TestStaticCollapsedToolSummary(t *testing.T) {
	js := readStatic(t, "app.js")

	if !strings.Contains(js, "function collapsedToolSummary(") {
		t.Fatal("app.js 里缺少 collapsedToolSummary")
	}
	if !strings.Contains(js, "trace-dot") {
		t.Error("折叠摘要里应当有状态圆点（对齐 CollapsedPlannerToolSummary）")
	}
}

// TestStaticTimelineRendersAllEventKinds 守：5 类事件都要有渲染器。
//
// 参考实现的时间线渲染器是一个 switch (entry.type)
// （maisaka-monitor.tsx:2273），每种事件有自己的卡片。
// 少一个渲染器的表现是「那一类事件静默消失」，界面上看不出问题，
// 只是信息少了 —— 这种缺失最难发现。
func TestStaticTimelineRendersAllEventKinds(t *testing.T) {
	js := readStatic(t, "app.js")
	css := readStatic(t, "style.css")

	// 步骤级：Planner / 工具 / 回复器
	for _, want := range []string{"renderPlannerStep(", "renderToolStep(", "renderReplyerStep("} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js 里缺少 %q：时间线上少了一类步骤", want)
		}
	}
	// 事件级：门控 / 裁切
	for _, want := range []string{"renderGate(", "renderTrim("} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js 里缺少 %q：这类事件会静默消失", want)
		}
	}
	// 分发器必须真的把四类事件都接上
	for _, want := range []string{"entry.kind === 'gate'", "entry.kind === 'trim'", "entry.kind === 'reasoning'"} {
		if !strings.Contains(js, want) {
			t.Errorf("buildTimeline 里缺少 %q", want)
		}
	}
	// 样式
	for _, want := range []string{".trace-gate {", ".trace-trim {", ".trace-args {", ".trace-status {"} {
		if !strings.Contains(css, want) {
			t.Errorf("style.css 里缺少 %s 规则", want)
		}
	}
}

// TestStaticToolStepShowsResult 守：工具步骤必须显示执行结果。
//
// 「Planner 决定调用 reply」和「reply 实际返回了什么」是两件事。
// 只显示前者的话，工具执行失败时界面上完全看不出来。
func TestStaticToolStepShowsResult(t *testing.T) {
	js := readStatic(t, "app.js")

	if !strings.Contains(js, "trace-result") {
		t.Error("工具步骤缺少执行结果区域（trace-result）")
	}
	if !strings.Contains(js, "执行成功") || !strings.Contains(js, "执行失败") {
		t.Error("工具步骤应当显示成功/失败状态")
	}
}

// TestStaticLogsInSidebar 守「日志与用量是侧栏里的普通条目」。
//
// 这两页原来属于「日志工作区」：顶栏切过去后侧栏整体消失，改由顶栏页签切换；
// 手机上再靠底部标签栏 + 「管理」页中转，一层又一层。
//
// 现在日志就是侧栏「运行记录」组里的一条，任何视口下都一步可达。
// 这个测试同时守着**工作区那套结构不要再回来**。
func TestStaticLogsInSidebar(t *testing.T) {
	html := readStatic(t, "index.html")
	js := readStatic(t, "app.js")
	css := readStatic(t, "style.css")

	// 两条必须在侧栏里，且归在「运行记录」组。
	sidebar := html[strings.Index(html, `class="sidebar"`):]
	if i := strings.Index(sidebar, `class="sidebar-foot"`); i > 0 {
		sidebar = sidebar[:i]
	}
	if !strings.Contains(sidebar, `class="nav-group">运行记录<`) {
		t.Error("侧栏缺少「运行记录」分组")
	}
	if !strings.Contains(sidebar, `data-nav="logs"`) {
		t.Error("侧栏缺少 logs 的导航项")
	}
	if !strings.Contains(js, "group: '运行记录'") {
		t.Error("app.js 里 logs 的路由应当声明 group: '运行记录'")
	}

	// 用量统计不再是独立页面：它归并进概览页（参考实现也是把
	// 请求数 / Token / 花费三张卡放在首页）。
	// 守「不要又拆回两条」：拆回去意味着同一类数据有两个入口。
	for _, gone := range []string{`data-nav="usage"`, `#/usage`} {
		if strings.Contains(html, gone) {
			t.Errorf("index.html 里不应再有 %s（用量已归并进概览）", gone)
		}
	}
	if strings.Contains(js, "function viewUsage(") {
		t.Error("app.js 里不应再有 viewUsage：用量区块由 usageSection() 提供")
	}
	if !strings.Contains(js, "function usageSection(") {
		t.Error("app.js 缺少 usageSection()：概览页的模型调用区块由它渲染")
	}

	// 旧结构不得回来：它们会让下面的东西重新变得不可达。
	for _, gone := range []string{
		`id="workspace-switch"`, `id="workspace-tabs"`,
		`class="tabbar"`, `data-nav="manage"`, `id="i-manage"`,
	} {
		if strings.Contains(html, gone) {
			t.Errorf("index.html 里不应再有 %s", gone)
		}
	}
	for _, gone := range []string{
		"const WORKSPACES =", "const LOGS_TABS =", "function renderWorkspaceTabs(",
		"async function viewManage(", "'ws-logs'",
	} {
		if strings.Contains(js, gone) {
			t.Errorf("app.js 里不应再有 %s", gone)
		}
	}
	for _, gone := range []string{".ws-tab {", ".tabbar {", ".manage-list {", ".app.ws-logs"} {
		if strings.Contains(css, gone) {
			t.Errorf("style.css 里不应再有 %s", gone)
		}
	}
}

// TestStaticPageTabsFoldSubPages 守「记忆 / 学习折成页签」。
//
// 对齐 MaiBot：它的学习页是「表达方式 / 黑话 / 表情包」三个页签，
// 长期记忆页是「记录 / 画像 / 图谱 …」多个页签，而不是在侧栏里
// 各占一条。木木照这个结构把 5 个页面折成 2 个入口。
//
// 少了页签的后果：侧栏重新膨胀成一长串，且同一类数据被拆成多个入口。
func TestStaticPageTabsFoldSubPages(t *testing.T) {
	html := readStatic(t, "index.html")
	js := readStatic(t, "app.js")
	css := readStatic(t, "style.css")

	if !strings.Contains(html, `id="page-tabs"`) {
		t.Error("index.html 缺少页签挂载点 #page-tabs")
	}
	if !strings.Contains(js, "tabs: [{ id: 'recall'") {
		t.Error("记忆页应当声明 recall / profiles 两个页签")
	}
	for _, want := range []string{"id: 'expression'", "id: 'jargon'", "id: 'emoji'"} {
		if !strings.Contains(js, want) {
			t.Errorf("学习页应当声明 %s 页签", want)
		}
	}
	if !strings.Contains(js, "function routeTab(") {
		t.Error("缺少 routeTab()：页签的 URL 解析与默认值回退必须集中在一处")
	}
	if !strings.Contains(css, ".page-tab.active {") {
		t.Error("style.css 缺少 .page-tab.active 规则")
	}
}

// TestStaticMobileUsesSidebar 守「移动端用抽屉侧栏，没有底部标签栏」。
//
// 两套导航并存时，底部三个入口只覆盖一小部分页面，
// 剩下的还得去「管理」页再点一次 —— 手机上比桌面端多两层。
// 现在抽屉侧栏与桌面分组完全一致，一套走到底。
func TestStaticMobileUsesSidebar(t *testing.T) {
	css := readStatic(t, "style.css")

	i := strings.Index(css, "@media (max-width: 900px)")
	if i < 0 {
		t.Fatal("style.css 里没有移动端断点")
	}
	mobile := css[i:]
	if !strings.Contains(mobile, ".sidebar.open { transform: translateX(0); }") {
		t.Error("移动端断点里应当把抽屉侧栏滑入")
	}
	if !strings.Contains(mobile, ".topbar-menu { display: inline-flex; }") {
		t.Error("移动端断点里应当显示汉堡菜单按钮，否则抽屉没有入口")
	}
}

// TestStaticNavGroupsMatchMaibot 守侧栏分组层级。
//
// 参考实现的分组是「概览 / 配置管理 / 资源管理 / 扩展集成 / 高级工具」。
// 木木只有前三组有对应功能，后两组整组删除 —— 但前三个分组标题
// 与归属关系必须与它一致，否则「配置」会被塞进「资源管理」这类错位里。
// 日志与用量按参考实现归在「运行记录」（它那边的日志工作区页签组）。
func TestStaticNavGroupsMatchMaibot(t *testing.T) {
	js := readStatic(t, "app.js")

	for _, want := range []string{
		"group: '概览'",
		"group: '配置管理'",
		"group: '资源管理'",
		"group: '运行记录'",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js 里缺少侧栏分组 %s", want)
		}
	}

	// 归属关系：会话在概览、配置在配置管理、记忆与学习在资源管理。
	//
	// 用「从这条声明开始到下一个同级声明之间」的文本块判断，
	// 而不是全局搜索「group: '概览'」—— 后者只要别处出现一次就会误判。
	routeBlock := func(name string) string {
		t.Helper()
		marker := "\n    " + name + ": {\n"
		idx := strings.Index(js, marker)
		if idx < 0 {
			t.Fatalf("找不到路由 %s 的声明", name)
		}
		rest := js[idx+len(marker):]
		if end := strings.Index(rest, "\n    },"); end >= 0 {
			return rest[:end]
		}
		t.Fatalf("路由 %s 的声明没有闭合", name)
		return ""
	}

	for _, pair := range []struct{ route, group string }{
		{"chats", "group: '概览'"},
		{"config", "group: '配置管理'"},
		{"memory", "group: '资源管理'"},
		{"learning", "group: '资源管理'"},
		{"logs", "group: '运行记录'"},
	} {
		if block := routeBlock(pair.route); !strings.Contains(block, pair.group) {
			t.Errorf("路由 %s 应当归在 %s，实际块:\n%s", pair.route, pair.group, block)
		}
	}
}

// TestStaticGlobalActionsInTopbar 守「主题/退出在顶栏，不在侧栏底部」。
//
// 这条最初是因为「日志工作区没有侧栏」才发现的，工作区结构现在已去掉，
// 但结论不变：全局动作属于顶栏，不该钉在导航菜单里 —— 参考实现也是
// 把设置与主题放在 Header 右侧。
func TestStaticGlobalActionsInTopbar(t *testing.T) {
	html := readStatic(t, "index.html")
	css := readStatic(t, "style.css")

	iTop := strings.Index(html, `class="topbar-global"`)
	if iTop < 0 {
		t.Fatal("index.html 缺少顶栏全局动作区 .topbar-global")
	}
	if iSide := strings.Index(html, `class="sidebar-foot"`); iSide < 0 {
		t.Error("index.html 缺少 .sidebar-foot（连接状态还在那里）")
	}

	// 两个按钮必须落在 topbar-global 之内（出现在它之后、且不在 sidebar 之内）。
	top := html[iTop:]
	for _, id := range []string{`id="theme-toggle"`, `id="logout"`} {
		if !strings.Contains(top, id) {
			t.Errorf("%s 应当放在顶栏全局动作区里", id)
		}
	}

	// 侧栏里不能再有它们：侧栏在日志工作区会被隐藏。
	if iSide := strings.Index(html, `class="sidebar-foot"`); iSide >= 0 {
		sideBlock := html[iSide:iTop]
		for _, id := range []string{`id="theme-toggle"`, `id="logout"`} {
			if strings.Contains(sideBlock, id) {
				t.Errorf("%s 仍在侧栏里：日志工作区隐藏侧栏时它会一起消失", id)
			}
		}
	}

	if !strings.Contains(css, ".topbar-global {") {
		t.Error("style.css 缺少 .topbar-global 规则")
	}
}

// TestStaticConfigFormIsEditable 守的是「配置页确实可编辑」这件事的结构前提。
//
// 这些点单独看都很琐碎，漏一个的表现都是「界面上看着正常，但某一类
// 字段改不了 / 改了没反应 / 报错看不见」。
func TestStaticConfigFormIsEditable(t *testing.T) {
	js := readStatic(t, "app.js")

	// novalidate：不加的话浏览器内置校验失败时**不触发 submit 事件**，
	// 用户点保存什么都不发生 —— 既不提示也不发请求。
	if !strings.Contains(js, "novalidate") {
		t.Error("配置表单必须加 novalidate，否则越界值会让保存按钮完全没反应")
	}

	// 每种控件类型都要有分支，漏一种就是「那一类字段永远不改生效」。
	for _, kind := range []string{"'bool'", "'text'", "'enum'", "'int'", "'float'", "'string_list'"} {
		if !strings.Contains(js, "case "+kind+":") {
			t.Errorf("renderField 缺少 %s 分支", kind)
		}
	}

	// min/max 必须用 != null 判断：talk_value 的下限是 0，
	// 用真值判断会把合法的 0 当成「没设置」。
	if !strings.Contains(js, "f.min != null") || !strings.Contains(js, "f.max != null") {
		t.Error("min/max 必须用 != null 判断（0 是合法边界）")
	}

	// collectPatch 抛错必须在 try 里：否则异常冒到控制台，界面无提示。
	if !strings.Contains(js, "patch = collectPatch(fields, draft, initial);") {
		t.Fatal("没找到 collectPatch 调用点")
	}
	idx := strings.Index(js, "patch = collectPatch(fields, draft, initial);")
	if !strings.Contains(js[idx-260:idx], "try {") {
		t.Error("collectPatch 必须在 try 块里调用，否则数字框填错时界面无任何反馈")
	}

	// 保存失败要显示后端原因，而不是只把按钮恢复。
	if !strings.Contains(js, "'保存失败：' + err.message") {
		t.Error("保存失败必须显示后端返回的原因")
	}

	// 保存成功但某项没能即时生效时，必须把 warning 显示出来。
	if !strings.Contains(js, "res.warning") {
		t.Error("必须处理后端返回的 warning（写盘成功但未即时生效）")
	}
}

// TestStaticConfigFieldKeysUnique 防：行容器与控件抢同一个 data-key。
//
// querySelector('[data-key=...]') 会先命中行（div，没有 .value），
// 于是测试与脚本拿到一个 undefined，看起来像「控件不存在」。
func TestStaticConfigFieldKeysUnique(t *testing.T) {
	js := readStatic(t, "app.js")
	if !strings.Contains(js, "dataset: { rowKey: f.key }") {
		t.Error("行容器必须用 data-row-key，不能和控件的 data-key 撞名")
	}
}

// TestStaticConfigSaveBarInsideForm 防：保存按钮不在 <form> 里。
//
// 这是「详细设置页改人格保存不了」的真正根因：
// 保存按钮是 type=submit，而 HTML 规定不在 form 内的 submit 按钮
// 不触发提交。之前写成 page.append(form) + page.append(saveBar)，
// 保存栏成了 form 的兄弟节点 —— 点保存什么都不发生：
// 不发请求、不报错、按钮不置灰、控制台干净。
//
// 而 uicheck 当时用 form.requestSubmit() 直接向表单调提交，
// 正好绕开了这条死路，所以断言全绿、功能全废。
// 这条静态检查守的是结构，比浏览器断言更早发现问题。
func TestStaticConfigSaveBarInsideForm(t *testing.T) {
	js := readStatic(t, "app.js")

	// 保存栏必须 append 到 form；一旦有人改回 page.append(...) 就红。
	//
	// 配置页现在是分节渲染，保存栏写在 buildSectionForm 里：
	//   form.append(el('div', { class: 'cfg-save' }, ...))
	// 判定要认这个形态，同时禁止「保存栏被 append 到 page」的写法。
	if !strings.Contains(js, "form.append(el('div', { class: 'cfg-save' },") &&
		!strings.Contains(js, "form.append(saveBar);") {
		t.Error("保存栏必须 append 到 form 里，否则 type=submit 的按钮不会触发提交")
	}
	if strings.Contains(js, "page.append(saveBar);") {
		t.Error("page.append(saveBar) 会让保存按钮落在 form 外，点了没反应")
	}

	// 同一页里不能出现「submit 按钮 + 不在 form 里」的组合。
	// 兜底：所有 type: 'submit' 的按钮所在函数里必须有表单提交监听。
	if !strings.Contains(js, "form.addEventListener('submit'") {
		t.Error("缺少 form 的 submit 监听")
	}
}

// TestStaticToastKindIsError 防：错误 toast 用了 'err'，而 toast() 只认 'error'。
//
// 后果是错误提示渲染成绿色的成功样式、且 2.4 秒就消失（成功态时长），
// 用户看不到「删除失败：xxx」里的原因。
func TestStaticToastKindIsError(t *testing.T) {
	js := readStatic(t, "app.js")
	if strings.Contains(js, ", 'err')") {
		t.Error("toast 的失败类型是 'error' 不是 'err'，写 'err' 会渲染成成功样式")
	}
}

// TestStaticProfileHasDelete 防：画像只能看不能删。
//
// 后端 DELETE /api/profiles/delete 早就写好并有测试，
// 但前端零调用 —— 画像里是群里真人的信息，删不掉等于
// 「机器人记了别人什么由不得你」。
func TestStaticProfileHasDelete(t *testing.T) {
	js := readStatic(t, "app.js")
	if !strings.Contains(js, "/api/profiles/delete") {
		t.Error("画像页必须能删除（DELETE /api/profiles/delete）")
	}
}

// TestStaticSessionSheet 防：会话级操作没有落点。
//
// 清空消息、重命名、删除会话三个动作以前只有删除会话有入口，
// 而且挤在列表行的 hover 垃圾桶上 —— 误触代价很大。
func TestStaticSessionSheet(t *testing.T) {
	js := readStatic(t, "app.js")
	css := readStatic(t, "style.css")

	for _, want := range []string{
		"/api/sessions/rename",
		"/api/messages/delete?session_id=",
		"/api/sessions/delete?id=",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("会话抽屉缺少动作 %s", want)
		}
	}
	if !strings.Contains(js, "renderSheet") {
		t.Error("缺少会话详情抽屉的渲染函数")
	}
	if !strings.Contains(css, ".sheet.open") {
		t.Error("style.css 缺少抽屉展开态规则")
	}
}

// TestStaticKeywordsEditor 防：关键词规则只能在 config.toml 里改。
func TestStaticKeywordsEditor(t *testing.T) {
	js := readStatic(t, "app.js")
	css := readStatic(t, "style.css")

	if !strings.Contains(js, "function keywordPanel(") {
		t.Error("缺少关键词规则编辑器")
	}
	for _, want := range []string{"/api/config/keywords", "/api/config/keywords/save"} {
		if !strings.Contains(js, want) {
			t.Errorf("关键词编辑器缺少接口调用 %s", want)
		}
	}
	// 关键词现在是二级导航里的一节（不再是独立页签）。
	if !strings.Contains(js, "{ slug: 'keywords',") {
		t.Error("配置页二级导航里必须有「关键词」一节")
	}
	if !strings.Contains(css, ".kw-row") {
		t.Error("style.css 缺少关键词表格样式")
	}
}

// TestStaticConfigSectionNav 防：配置页退回「50 字段长表单」。
//
// 一页到底的形态有三个具体坏处：要滚 2000+ 像素才能看完；
// 分区只靠 <h3> 撑着，改一个阈值要在几十个框里找；
// 保存栏是全局的，改完不知道到底存了几处。
func TestStaticConfigSectionNav(t *testing.T) {
	js := readStatic(t, "app.js")
	css := readStatic(t, "style.css")

	if !strings.Contains(js, "const CONFIG_SECTIONS = [") {
		t.Fatal("缺少配置分区表 CONFIG_SECTIONS")
	}
	// 14 个分区必须都在表里，少一个那批字段就在界面上消失。
	for _, slug := range []string{
		"'connection'", "'identity'", "'personality'", "'chat'", "'quote'",
		"'response'", "'filter'", "'memory'", "'emoji'", "'learning'",
		"'keywords'", "'command'", "'runtime'", "'info'",
	} {
		if !strings.Contains(js, "slug: "+slug) {
			t.Errorf("分区表缺少 slug %s", slug)
		}
	}

	// 每节独立保存栏 + 独立 submit 监听。
	if !strings.Contains(js, "function buildSectionForm(") {
		t.Error("缺少分节表单构造函数")
	}
	if !strings.Contains(js, "form.append(el('div', { class: 'cfg-save' },") {
		t.Error("分节保存栏必须 append 到 form 内（否则 submit 按钮点了没反应）")
	}

	// 切节不能靠 location.hash：那会触发 navigate() 从服务端重读，
	// 把其它节里还没保存的编辑静默抹掉。
	if !strings.Contains(js, "history.replaceState(null, '', '#/config?section='") {
		t.Error("切节必须用 replaceState，不能用 location.hash（会丢未保存改动）")
	}

	// 未保存标记。
	if !strings.Contains(js, "subnav-dot") {
		t.Error("缺少「未保存」小圆点：多节编辑后这是唯一的状态线索")
	}

	// 布局与样式。
	if !strings.Contains(css, ".cfg-layout {") {
		t.Error("style.css 缺少两栏布局 .cfg-layout")
	}
	if !strings.Contains(css, ".subnav-item.on {") {
		t.Error("style.css 缺少导航选中态")
	}
	// 手机上导航必须能横向滚动，否则 14 项挤在一行或被截断。
	if !strings.Contains(css, "flex-direction: row;") {
		t.Error("移动端二级导航应改成横向排列")
	}
}

// TestStaticGridColumnsHaveZeroMin 防：grid 的 1fr 被长内容撑破。
//
// CSS Grid 的 `1fr` 隐含最小值是 auto（= min-content），
// 一个长群名会把 268px+1fr 的两栏布局撑到 435px，
// 手机上（390px）直接横向溢出、把右侧对话区挤出屏幕。
// 必须写成 minmax(0, 1fr)。flex 里的 min-width:0 拦不住这个 ——
// 撑宽发生在网格轨道计算阶段，早于子元素布局。
func TestStaticGridColumnsHaveZeroMin(t *testing.T) {
	css := readStatic(t, "style.css")

	bare := regexp.MustCompile(`grid-template-columns:\s*[^;]*\b1fr\b`)
	for _, line := range strings.Split(css, "\n") {
		tl := strings.TrimSpace(line)
		if !strings.HasPrefix(tl, "grid-template-columns:") {
			continue
		}
		// 单列 1fr 不会溢出（没有第二个轨道可争），跳过。
		if strings.HasSuffix(tl, "columns: 1fr;") {
			continue
		}
		// 多列时必须每个 1fr 都带 minmax(最小值, 1fr)。
		//
		// 裸 1fr 的隐含最小值是 auto（= min-content），会被长内容撑破。
		// 允许 minmax(90px, 1fr) 这种「有明确下限」的写法。
		for _, tok := range strings.Split(tl, " ") {
			if tok != "1fr" && tok != "1fr;" {
				continue
			}
			// 裸 1fr（前面不是 minmax(...) 的逗号）
			idx := strings.Index(tl, tok)
			prefix := tl[:idx]
			if strings.HasSuffix(strings.TrimSpace(prefix), ",") &&
				strings.Contains(prefix, "minmax(") &&
				strings.Count(prefix, "minmax(") > strings.Count(prefix, ")") {
				continue // 在 minmax(...) 内部
			}
			// auto-fit / auto-fill 的 repeat 自带 minmax 下限
			if strings.Contains(tl, "auto-fit") || strings.Contains(tl, "auto-fill") {
				continue
			}
			t.Errorf("多列 grid 用裸 1fr 会被长内容撑破，应写 minmax(0, 1fr)：%q", tl)
			break
		}
	}
	_ = bare
}

// TestStaticSheetTriggerHidden 防：抽屉打开后触发器与关闭按钮重叠。
//
// 抽屉是 right:0 的整高浮层，关闭按钮在右上角；「会话操作」触发器
// 也在右上角。两者实测 28x28px 完全重合 —— 点哪个都说不清，
// 而且触发器被盖住后用户会以为「点不回去了」。
func TestStaticSheetTriggerHidden(t *testing.T) {
	js := readStatic(t, "app.js")
	css := readStatic(t, "style.css")

	if !strings.Contains(js, "classList.toggle('sheet-open', open)") {
		t.Error("打开抽屉时必须给容器打 sheet-open 标记")
	}
	if !strings.Contains(css, ".chats.sheet-open .chat-more") {
		t.Error("抽屉打开时要把 .chat-more 藏起来，否则两个按钮叠在一起")
	}
}

// TestStaticModelProbe 防：探活入口消失。
//
// 模型配置是唯一一类「填错了不报错、只表现为机器人不说话」的配置。
// 没有这个按钮，用户只能去命令行跑 tools/livecheck，
// 而真正会填错的人恰恰是在网页上改配置的人。
func TestStaticModelProbe(t *testing.T) {
	js := readStatic(t, "app.js")
	css := readStatic(t, "style.css")

	for _, want := range []string{
		"async function probeModel(",
		"async function probeProvider(",
		"/api/config/llm/probe",
		"/api/config/llm/probe-provider",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("缺少模型探活实现：%s", want)
		}
	}

	// 探活结果必须展示 hint 与 models。
	//
	// 只显示「失败」等于没做这个功能 —— 用户还是不知道该改哪个框。
	// 服务商的模型列表同样重要：模型名填错时，它就是答案。
	if !strings.Contains(js, "res.hint") {
		t.Error("探活结果必须展示排查建议（hint）")
	}
	if !strings.Contains(js, "res.models") {
		t.Error("服务商探活必须展示服务端返回的模型列表")
	}

	// 测试按钮不能只在代码里存在，必须真的挂在界面上。
	if strings.Count(js, "probeModel(m.name") < 1 {
		t.Error("模型行缺少「测试」按钮")
	}
	if strings.Count(js, "probeProvider(current.name") < 1 {
		t.Error("服务商卡缺少「测试」按钮")
	}

	// 连点保护：请求期间必须禁用按钮。
	if !strings.Contains(js, "async function withBusy(") {
		t.Error("探活必须防连点（请求期间禁用按钮）")
	}

	// 结果样式。
	for _, want := range []string{".probe {", ".probe.bad", ".probe-hint", ".probe-models"} {
		if !strings.Contains(css, want) {
			t.Errorf("style.css 缺少探活样式 %s", want)
		}
	}

	// 结果卡必须允许长内容断行。
	//
	// 错误原文里经常带完整 URL 或服务端返回的 JSON，中间没有空格。
	// 不写 overflow-wrap: anywhere 的话，一个长串会把卡片撑出
	// 横向滚动条 —— 这在 layoutcheck 里能测到，但静态断言更早。
	if !strings.Contains(css, "overflow-wrap: anywhere") {
		t.Error("探活结果卡必须允许长串断行（否则被 URL 撑破）")
	}

	// 必须标出测的是「对话」还是「向量」。
	//
	// 两种模型走完全不同的端点（/chat/completions vs /embeddings）。
	// 不标的话「向量模型测试通过」会被误读成「它可以用作回复器」——
	// 而给 Replyer 配向量模型是本项目最典型的一类配错。
	for _, want := range []string{"res.kind === 'embedding'", "res.dimension"} {
		if !strings.Contains(js, want) {
			t.Errorf("探活结果必须展示模型类型与维度：%s", want)
		}
	}
}

// TestStaticRestart 防：重启入口消失或变成「假装重启」。
func TestStaticRestart(t *testing.T) {
	js := readStatic(t, "app.js")

	if !strings.Contains(js, "function restartCard()") {
		t.Fatal("缺少重启卡片")
	}
	if !strings.Contains(js, "/api/restart") {
		t.Fatal("重启卡片没有调用 /api/restart")
	}
	// 必须带显式确认：服务端要求 confirm=restart，否则 400。
	if !strings.Contains(js, "confirm: 'restart'") {
		t.Error("重启请求必须带 confirm=restart")
	}
	// 必须有二次确认弹窗：手机上这个按钮挨着「保存」，点错的代价是掉线。
	if !strings.Contains(js, "确定重启？") {
		t.Error("重启前必须二次确认")
	}
	// 重启后不能直接 reload：新进程起得慢时浏览器会显示「无法访问此网站」，
	// 用户会以为重启把程序弄挂了。
	if !strings.Contains(js, "waitBack") {
		t.Error("重启后必须轮询等新进程恢复，不能直接 reload")
	}
	// 只出现在「运行」一节：别处的字段都不需要重启。
	if !strings.Contains(js, "if (active === 'runtime') bodyBox.append(restartCard());") {
		t.Error("重启卡应只出现在「运行」一节")
	}
}

// TestRestartHasNoHardcodedPort 守：重启不能自己拼端口/路径。
//
// 端口来自配置，路径来自 os.Getwd()。写死任何一个是「换台机器就坏」。
func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

func TestRestartDoesNotHardcodePath(t *testing.T) {
	src := readFileString(t, "../../internal/web/restart.go")
	// 逐行检查，跳过注释 —— 注释里举例提到 7800 是正常的，
	// 只有出现在代码里才算写死。
	for _, line := range strings.Split(src, "\n") {
		tl := strings.TrimSpace(line)
		if tl == "" || strings.HasPrefix(tl, "//") || strings.HasPrefix(tl, "/*") ||
			strings.HasPrefix(tl, "*") {
			continue
		}
		if strings.Contains(tl, "7800") {
			t.Errorf("重启逻辑不能写死默认端口，端口必须来自配置：%q", tl)
		}
	}
	if !strings.Contains(src, "os.Executable()") {
		t.Error("重启必须用 os.Executable() 定位自己，不能假设文件名")
	}
	if !strings.Contains(src, "Release()") {
		t.Error("重启必须先释放端口，否则新进程 bind 会失败")
	}
}

// TestStaticModelRowFitsThreeButtons 防：加了按钮没加宽列。
//
// 这个 bug 真实发生过：模型行原本只有「编辑 / 删除」两个按钮，
// 操作列固定 108px。加「测试」之后需要 136px，多出来的部分
// 横向溢到列外 —— 被 layoutcheck 抓到（[超出容器] 测试 按钮）。
//
// 静态断言的价值：它比 layoutcheck 早得多，不需要跑浏览器。
func TestStaticModelRowFitsThreeButtons(t *testing.T) {
	css := readStatic(t, "style.css")

	// 找到 .mdl-tr 的 grid-template-columns，最后一列不能是固定小像素。
	re := regexp.MustCompile(`\.mdl-tr \{([^}]*)\}`)
	m := re.FindStringSubmatch(css)
	if m == nil {
		t.Fatal("找不到 .mdl-tr 规则")
	}
	// 只看列定义那一行，别扫到注释（注释里会提到「原来是 108px」）。
	var decl string
	for _, line := range strings.Split(m[1], "\n") {
		tl := strings.TrimSpace(line)
		if strings.HasPrefix(tl, "//") || !strings.Contains(tl, "grid-template-columns") {
			continue
		}
		decl = tl
		break
	}
	if decl == "" {
		t.Fatal(".mdl-tr 缺少 grid-template-columns 声明")
	}
	// 最后一列必须是 auto，不能是 108px 这种定值。
	if strings.Contains(decl, "108px") {
		t.Errorf("操作列不能是固定 108px —— 三个按钮放不下，第一个会溢出：%q", decl)
	}
	if !strings.Contains(decl, "auto)") {
		t.Errorf("操作列应使用 auto 按内容取宽：%q", decl)
	}
}

// TestStaticQuoteRendering 守引用块的两种视觉形态。
//
// 引用块有三条分支，都是「界面上看得见但容易漏」的：
//  1. 有批量预览 → 显示「谁: 内容」
//  2. 只有事件快照（quoted_text）→ 也要显示内容，不能退回 ID。
//     这是 QQ 官方通道的特点：被引正文随事件下发，
//     就算那条消息已经被保留期清掉，内容仍然在。
//  3. 都没有 → quote-raw + 等宽字体的截断 ID
//
// 另外 quote-ref 用来区分「用户之间的引用」与「木木的被动回复」——
// 两者在数据上都是 reply_to，但看聊天记录时意义完全不同。
func TestStaticQuoteRendering(t *testing.T) {
	js := readStatic(t, "app.js")
	css := readStatic(t, "style.css")

	// 快照兜底必须在，否则被清理的消息只剩一串 ID。
	if !strings.Contains(js, "m.quoted_text") {
		t.Error("引用块应能用 quoted_text 快照兜底，否则被引消息清理后只剩 ID")
	}
	if !strings.Contains(js, "m.quoted_sender") {
		t.Error("引用块应显示被引消息的发送者（quoted_sender）")
	}
	// 区分真引用与被动回复。
	if !strings.Contains(js, "is_quote_target") {
		t.Error("引用块应用 is_quote_target 区分真引用与被动回复")
	}
	if !strings.Contains(js, "quote-ref") {
		t.Error("app.js 里应有 quote-ref 分支")
	}
	if !strings.Contains(css, ".quote.quote-ref") {
		t.Error("style.css 里应定义 .quote.quote-ref")
	}
	// 不许退回「只显示 ID」这一条 —— 那是改造前的行为。
	if strings.Contains(js, "text: preview || m.reply_to") {
		t.Error("引用块不该直接把 reply_to 当正文显示")
	}
}

// TestStaticTalkPage 守「对话窗口」页：以人类身份与机器人私聊的入口。
//
// 对应 MaiBot dashboard 的 /chat 聊天页。会话页（观察）与对话页（聊天）
// 必须是两个入口：观察流混进一个输入框，用户分不清自己是在「看」还是「聊」。
func TestStaticTalkPage(t *testing.T) {
	js := readStatic(t, "app.js")
	html := readStatic(t, "index.html")
	css := readStatic(t, "style.css")

	// 路由、视图、导航三处都要在。
	if !strings.Contains(js, "talk: {") {
		t.Error("app.js 里缺少 talk 路由")
	}
	if !strings.Contains(js, "async function viewTalk(") {
		t.Error("app.js 里缺少 viewTalk 函数")
	}
	if !strings.Contains(html, `data-nav="talk"`) {
		t.Error("index.html 里缺少「对话」导航项")
	}
	if !strings.Contains(html, `id="i-talk"`) {
		t.Error("index.html 里缺少 i-talk 图标")
	}

	// 核心交互：身份（昵称）、发送、乐观显示、轮询、正在输入。
	for _, want := range []string{
		"'/api/chat/send'",
		"'/api/chat/session'",
		"function renderTalkMsg(",
		"function makeNickBtn(",
		"function showTalkTyping(",
		"currentRoute() !== 'talk'",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js 里缺少 %q：对话页的核心交互缺了一块", want)
		}
	}
	if !strings.Contains(css, ".talk-bubble {") {
		t.Error("style.css 里缺少 .talk-bubble 规则")
	}
}

// TestStaticBotVoiceRemoved 守「以机器人身份发话」彻底删干净。
//
// 这是被「对话页」替代的旧方向：旧的 /api/send 以机器人身份往真实
// QQ 会话发主动消息，容易在群里造成「管理员手动冒充机器人」的困惑。
// 接口、前端调用、发送框一个都不许回来。
func TestStaticBotVoiceRemoved(t *testing.T) {
	js := readStatic(t, "app.js")
	html := readStatic(t, "index.html")

	for _, gone := range []string{
		"/api/send",
		"以机器人身份发一条消息",
		"以机器人身份发言",
	} {
		if strings.Contains(js, gone) || strings.Contains(html, gone) {
			t.Errorf("前端还残留 %q：「以机器人身份发话」应该彻底移除", gone)
		}
	}
	// 会话页的发送框整块删掉，不留禁用态的空壳。
	if strings.Contains(js, "const composer = el('form', { class: 'composer' }") {
		t.Error("会话页还在构造 composer：发送框应该删掉而不是禁用")
	}
}

// TestStaticEventsDriveAutoRefresh 守「前端实时刷新的事件流接线」。
//
// 对齐 MaiBot dashboard 的行为（unified WS 的 chat 域推送）：消息落库
// 即通知前端拉增量，轮询只做兜底。删掉其中任何一环，页面就退回
// 「等轮询才更新」，而且是静默退化 —— 构建不会报任何错。
func TestStaticEventsDriveAutoRefresh(t *testing.T) {
	js := readStatic(t, "app.js")

	// 事件源：登录后连接，登出后断开。
	for _, want := range []string{
		"new EventSource('/api/events')",
		"function startEvents(",
		"function stopEvents(",
		"stopEvents();",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js 里缺少 %q：事件流接线断了，页面退回纯轮询", want)
		}
	}
	// 事件分发必须驱动三个页面的刷新，且走节流（洪峰下合并拉取）。
	for _, want := range []string{
		"function onMessageEvent(",
		"scheduleRefresh('chats-messages', fetchChatsIncrement)",
		"scheduleRefresh('talk-messages', fetchTalkIncrement)",
		"scheduleRefresh('overview', refreshOverviewSilently",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js 里缺少 %q：对应页面的实时刷新没接上", want)
		}
	}
	// 轮询兜底必须还在：事件流断线期间靠它补数据。
	for _, want := range []string{
		"fetchChatsIncrement();",
		"fetchTalkIncrement();",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js 里缺少 %q：轮询兜底被删了", want)
		}
	}
	// 概览页的静默刷新入口。
	if !strings.Contains(js, "function refreshOverviewSilently(") {
		t.Error("app.js 里缺少 refreshOverviewSilently：概览自动刷新没接上")
	}
}
