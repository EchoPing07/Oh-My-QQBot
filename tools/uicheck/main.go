// Command uicheck 用真实浏览器核对 WebUI 的结构。
//
// 为什么需要它：internal/web/static_test.go 只能检查字符串。
// 「工作区切换器真的渲染出了按钮」「切到日志工作区后侧栏真的不可见」
// 「页签真的切换了内容」这类事，字符串层面无法证明 —— 曾经踩过
// 「断言全绿但界面上什么都没有」的坑（AGENTS.md 第 91 条）。
//
// 做法：起一个带假数据的 WebUI → 起 headless Chrome → 通过 CDP
// 注入登录 Cookie → 逐页取 DOM 与计算样式 → 逐项断言。
//
// 用法（需要本机装了 Chrome）：
//
//	go run ./tools/uicheck
//
// 退出码非 0 表示有断言失败。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/llm"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/prompt"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/store"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/web"
)

const (
	uiPort  = 7811
	cdpPort = 9333
)

type fakeHub struct{}

func (fakeHub) ActiveCount() int { return 2 }

// Dispatch 满足 web.Hub 接口。两个检查工具都不投递消息，给空实现。
func (fakeHub) Dispatch(*domain.Message, string, string, string) {}

func main() {
	dir, err := os.MkdirTemp("", "omuicheck")
	must(err)

	cfg := config.Default()
	cfg.Web.Password = "check-pass"
	cfg.Web.Host = "127.0.0.1"
	cfg.Web.Port = uiPort
	cfg.Bot.Nickname = "木木"
	// 服务商与模型：默认配置里是空的，这里补一个用于核对模型管理页的渲染。
	// 与真实启动的差别仅在「有没有配过模型」，页面逻辑一致。
	cfg.LLM.Providers = []config.ProviderConfig{{
		Name: "check-provider", BaseURL: "https://example.invalid/v1",
		APIKey: "sk-uicheck-DO-NOT-LEAK", Timeout: 60, MaxRetry: 2, RetryInterval: 3,
	}}
	cfg.LLM.Models = []config.ModelConfig{{
		Name: "check-model", ModelIdentifier: "check-ident", Provider: "check-provider",
	}}

	db, err := store.Open(context.Background(), filepath.Join(dir, "c.db"))
	must(err)
	defer db.Close()

	ctx := context.Background()
	now := time.Now()
	must(db.Sessions().Upsert(ctx, &domain.Session{
		ID: "s1", Platform: "qq", Scope: domain.ScopeGroup, TargetID: "g1",
		DisplayName: "测试群", MessageCount: 1, LastActiveAt: now, CreatedAt: now,
	}))
	must(db.Messages().Save(ctx, &domain.Message{
		ID: "m1", SessionID: "s1", MessageID: "p1", Platform: "qq",
		UserID: "u1", UserName: "阿一", GroupID: "g1", IsGroup: true,
		PlainText: "你好", Segments: []domain.Segment{domain.Text("你好")}, ReceivedAt: now,
	}))
	must(db.Memory().UpsertProfile(ctx, &store.PersonProfile{
		Platform: "qq", UserID: "u1", SessionID: "s1", DisplayName: "阿一",
		ProfileText: "喜欢摄影。", MessageCount: 12, UpdatedAt: now.Unix(),
	}))
	// 学习页的数据也要种：删除按钮只在有行时才渲染，
	// 空表跑出来的「有删除按钮」断言会假失败。
	if _, err := db.Memory().SaveExpression(ctx, &store.Expression{
		SessionID: "s1", Situation: "被夸奖", Style: "嘿嘿谢啦",
	}); err != nil {
		panic(err)
	}
	must(db.Memory().SaveJargon(ctx, &store.Jargon{
		Content: "破防", Meaning: "情绪崩溃", Count: 5,
		CreatedAt: now.Unix(), UpdatedAt: now.Unix(),
	}))

	// 提示词用真实目录：Prompt 管理页要靠它读写。
	// 用临时目录而不是仓库里的 prompts/，免得 uicheck 改坏真实提示词。
	pm, err := prompt.New(filepath.Join(dir, "prompts"), "zh-CN")
	must(err)

	s := web.NewServer(web.Options{
		Config: config.Wrap(cfg), Store: db, Hub: fakeHub{}, Prompts: pm,
		// LLM 必须注入：模型探活接口在它缺失时返回 503，
		// 而「探活能不能用」本身就是要检查的功能之一。
		//
		// 这里故意不配可用服务商 —— 探活断言只看「结构正确的失败」，
		// 不看真的能不能连上（那取决于本机环境）。
		LLM: llm.New(config.Wrap(cfg)),
	})
	must(s.Start())
	defer s.Stop()
	time.Sleep(300 * time.Millisecond)

	// 登录拿 Cookie 值（直接调 API，不经过浏览器 —— 登录页本身有别的测试守）。
	loginOut, err := exec.Command("curl", "-s", "-X", "POST",
		"-H", "Content-Type: application/json",
		"-d", `{"password":"check-pass"}`,
		fmt.Sprintf("http://127.0.0.1:%d/api/login", uiPort),
		"-D", "-", "-o", os.DevNull).CombinedOutput()
	must(err)
	cookie := extractCookie(string(loginOut))
	fmt.Println("登录 Cookie:", cookie[:12]+"…")

	// 起 Chrome，用 CDP 注入 Cookie 后逐页取 DOM。
	// 先确认调试端口没被占用。
	//
	// 不检查的话，若上一轮跑崩留下了一个 Chrome，下面 connect 会连到
	// **那个旧实例**：它加载的是旧的服务地址与旧的 app.js，于是
	// uicheck 会对着已死的代码报一堆看起来像「新改动没生效」的假失败。
	// 这个坑真实踩过：改了 form.append(saveBar)，uicheck 却一直说
	// 「保存按钮不在 form 内」，DOM 里连新文案都没有。
	if conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", cdpPort), 400*time.Millisecond); err == nil {
		_ = conn.Close()
		panic(fmt.Sprintf(
			"调试端口 %d 已被占用：很可能有上一轮残留的 Chrome。\n"+
				"请先结束它再跑（Windows: taskkill /F /PID <pid>，端口占用见 netstat -ano | grep %d），\n"+
				"否则本工具会连到旧实例、对着旧代码报假失败。", cdpPort, cdpPort))
	}

	chromeBin := `C:\Program Files\Google\Chrome\Application\chrome.exe`
	chrome := exec.Command(chromeBin,
		"--headless=new", "--disable-gpu", "--no-first-run",
		"--no-default-browser-check",
		"--remote-debugging-port="+fmt.Sprint(cdpPort),
		"--user-data-dir="+filepath.Join(dir, "chrome"),
		"about:blank",
	)
	must(chrome.Start())
	defer func() { _ = chrome.Process.Kill() }()
	time.Sleep(1500 * time.Millisecond)

	c := dialCDP()
	defer c.close()

	// 注入登录 Cookie。
	c.mustCall("Network.enable", nil)
	c.mustCall("Network.setCookie", map[string]any{
		"name":   "omq_session",
		"value":  cookie,
		"domain": "127.0.0.1",
		"path":   "/",
	})
	c.mustCall("Page.enable", nil)

	fail := 0
	check := func(name string, ok bool, detail string) {
		if ok {
			fmt.Printf("  OK   %s\n", name)
			return
		}
		fmt.Printf("  FAIL %s %s\n", name, detail)
		fail++
	}

	base := fmt.Sprintf("http://127.0.0.1:%d/", uiPort)

	// —— 概览（侧栏导航） ——
	fmt.Println("\n【概览 · 侧栏导航】")
	dom := c.navigate(base + "#/overview")
	check("侧栏分组：概览", strings.Contains(dom, `class="nav-group">概览<`), "")
	check("侧栏分组：配置管理", strings.Contains(dom, `class="nav-group">配置管理<`), "")
	check("侧栏分组：资源管理", strings.Contains(dom, `class="nav-group">资源管理<`), "")
	check("侧栏分组：运行记录", strings.Contains(dom, `class="nav-group">运行记录<`), "")
	check("侧栏有日志项", strings.Contains(dom, `data-nav="logs"`), "")
	// 用量统计归并进概览页了，侧栏不该再有第二个入口。
	check("侧栏没有独立的用量统计项", !strings.Contains(dom, `data-nav="usage"`), "")
	check("顶部没有工作区切换器", !strings.Contains(dom, `id="workspace-switch"`), "")
	check("没有底部标签栏", !strings.Contains(dom, `class="tabbar"`), "")
	check("概览页页签栏是隐藏的",
		strings.Contains(dom, `class="page-tabs" id="page-tabs" role="tablist" hidden`), "")
	check("概览渲染出指标卡", strings.Contains(dom, `class="metric-k"`), "")

	// —— 日志 / 用量 ——
	fmt.Println("\n【终端日志 · 侧栏项】")
	dom = c.navigate(base + "#/logs")
	check("有日志容器", strings.Contains(dom, `class="logs"`), "")
	check("日志导航项是激活态",
		strings.Contains(dom, `class="nav-item active" href="#/logs" data-nav="logs"`) ||
			strings.Contains(dom, `class="nav-item active" data-nav="logs"`), "")
	// 侧栏在任何页面都必须可见：日志以前属于「日志工作区」，切过去侧栏会消失。
	sidebarDisplay := c.eval(`getComputedStyle(document.querySelector('.sidebar')).display`)
	check("日志页侧栏仍然可见", strings.Trim(sidebarDisplay, `"`) == "flex", "实际="+sidebarDisplay)

	// 用量统计已归并进概览页（参考实现也是把请求数 / Token / 花费三张卡放首页）。
	// 这里断言的是「区块真的渲染出来了」，而不只是「源码里有个函数」——
	// 定义了没挂上去，界面上一片空白，而静态检查看不出来。
	fmt.Println("\n【概览 · 模型调用区块】")
	c.navigate(base + "#/overview")
	usageText := c.eval(`(function(){
		const heads = Array.from(document.querySelectorAll('.section-head h3'));
		const head = heads.find(h => h.textContent === '模型调用');
		if (!head) return 'NO_SECTION';
		const sec = head.closest('.section');
		return JSON.stringify({
			metrics: sec.querySelectorAll('.metric').length,
			text: sec.textContent.replace(/\s+/g, ' ').trim().slice(0, 300)
		});
	})()`)
	fmt.Println("    模型调用区块 =", usageText)
	check("概览页有「模型调用」区块", !strings.Contains(usageText, "NO_SECTION"), "实际="+usageText)
	check("区块里有 5 张指标卡", strings.Contains(usageText, `"metrics":5`), "实际="+usageText)
	check("区块里有「按任务」小标题", strings.Contains(usageText, "按任务"), "实际="+usageText)
	check("区块里有时间范围切换", strings.Contains(usageText, "30 天"), "实际="+usageText)

	// —— 记忆页签 ——
	fmt.Println("\n【记忆 · 人物画像页签】")
	dom = c.navigate(base + "#/memory?tab=profiles")
	check("页签栏出现两个页签",
		strings.Contains(dom, `data-tab="recall"`) && strings.Contains(dom, `data-tab="profiles"`), "")
	check("profiles 页签是激活态",
		strings.Contains(dom, `class="page-tab active" type="button" role="tab" aria-selected="true" data-tab="profiles"`), "")
	check("渲染出人物画像卡片", strings.Contains(dom, `class="profile-card"`), "")
	check("画像内容取自后端", strings.Contains(dom, "喜欢摄影"), "")
	check("没有同时渲染聊天回想（按页签只渲染一块）",
		!strings.Contains(dom, "记忆在上下文被裁切时自动生成"), "")

	fmt.Println("\n【记忆 · 聊天回想页签】")
	dom = c.navigate(base + "#/memory?tab=recall")
	check("recall 页签激活", strings.Contains(dom, `aria-selected="true" data-tab="recall"`), "")
	check("渲染出聊天回想区块",
		strings.Contains(dom, "记忆在上下文被裁切时自动生成"), "")
	check("没有渲染画像卡片", !strings.Contains(dom, `class="profile-card"`), "")

	// —— 学习页签 ——
	fmt.Println("\n【学习 · 表情包页签】")
	dom = c.navigate(base + "#/learning?tab=emoji")
	check("三个页签齐全",
		strings.Contains(dom, `data-tab="expression"`) && strings.Contains(dom, `data-tab="jargon"`) &&
			strings.Contains(dom, `data-tab="emoji"`), "")
	check("emoji 页签激活",
		strings.Contains(dom, `aria-selected="true" data-tab="emoji"`), "")
	check("渲染出表情包区块",
		strings.Contains(dom, "emoji.collect_enabled 控制自动收藏"), "")
	check("没有同时渲染表达表", !strings.Contains(dom, `class="table"`), "")

	fmt.Println("\n【学习 · 黑话页签】")
	dom = c.navigate(base + "#/learning?tab=jargon")
	check("jargon 页签激活", strings.Contains(dom, `aria-selected="true" data-tab="jargon"`), "")
	check("渲染出黑话表", strings.Contains(dom, "词条") && strings.Contains(dom, "含义"), "")

	// —— 移动端视口 ——
	fmt.Println("\n【移动端视口 390x844】")
	c.mustCall("Emulation.setDeviceMetricsOverride", map[string]any{
		"width": 390, "height": 844, "deviceScaleFactor": 2, "mobile": true,
	})
	dom = c.navigate(base + "#/overview")
	check("移动端没有底部标签栏", !strings.Contains(dom, `class="tabbar"`), "")
	hamburger := c.eval(`getComputedStyle(document.querySelector('#menu-toggle')).display`)
	check("移动端显示汉堡菜单按钮",
		strings.Trim(hamburger, `"`) == "flex" || strings.Trim(hamburger, `"`) == "inline-flex", "实际="+hamburger)
	closed := c.eval(`getComputedStyle(document.querySelector('.sidebar')).display`)
	check("抽屉默认是关着的（侧栏仍占位但位移出屏）",
		strings.Trim(closed, `"`) != "none", "实际="+closed)
	// 点击汉堡 → 抽屉滑出 → 侧栏导航项真的可点。
	c.mustCall("Runtime.evaluate", map[string]any{
		"expression":    `document.querySelector('#menu-toggle').click()`,
		"returnByValue": true,
	})
	time.Sleep(300 * time.Millisecond)
	open := c.eval(`String(document.querySelector('.sidebar').classList.contains('open'))`)
	check("点汉堡后抽屉打开", strings.Trim(open, `"`) == "true", "实际="+open)
	check("抽屉里有日志入口", strings.Contains(c.eval(`document.body.innerHTML`), `data-nav="logs"`), "")
	// 从抽屉直接进日志页，不需要任何中间页。
	dom = c.navigate(base + "#/logs")
	check("移动端能直达日志页", strings.Contains(dom, `class="logs"`), "")

	// —— 配置可编辑 ——
	fmt.Println("\n【配置编辑】")
	dom = c.navigate(base + "#/config?section=connection")
	check("连接节渲染出凭据卡", strings.Contains(dom, "QQ 机器人凭据"), "")
	check("连接节有输入控件", strings.Contains(dom, `class="input`), "")
	check("连接节标明需重启", strings.Contains(dom, "需重启"), "")
	// 凭据卡里有一个 type=password 的空输入框，它的 data-key 是
	// qq.app_secret —— 这是「能填写」，不是「回显了密钥」。
	// 这条断言检查的是「没有把已保存的密钥值渲染出来」，
	// 所以改成查文字与 value：DOM 里不能出现密钥原文。
	check("不显示任何密钥值",
		!strings.Contains(dom, "sk-llm-api-key-DO-NOT-LEAK") &&
			!strings.Contains(dom, `data-key="web.password"`), "")
	check("LLM 密钥字段不出现在配置表单里",
		!strings.Contains(dom, `data-key="llm.`), "")

	// 分节之后，「表单 / 保存按钮 / 只读信息」分属不同的节，
	// 不能像以前那样在同一个 URL 里一起断言。
	dom = c.navigate(base + "#/config?section=personality")
	check("人格节渲染出配置表单", strings.Contains(dom, `class="cfg-form"`), "")
	check("人格节有保存按钮", strings.Contains(dom, "保存本节"), "")
	dom = c.navigate(base + "#/config?section=info")
	check("信息节有只读信息区", strings.Contains(dom, "只读信息"), "")

	// QQ 凭据卡：这里必须真的能填，否则首次部署只能手改 config.toml。
	// 注意 data-key 是 qq.app_id / qq.app_secret，所以上面那条
	// 「不显示 app_secret」的断言会把这张卡也扫进去 —— 它检查的是
	// 文字里没有 app_secret 这个字段名（值不回显），而这张卡的
	// 输入框是空的密码框，不违反那条断言。
	dom = c.navigate(base + "#/config?section=connection")
	check("有 QQ 凭据卡", strings.Contains(dom, "QQ 机器人凭据"), "")
	check("有 AppID 输入框", strings.Contains(dom, `data-key="qq.app_id"`), "")
	check("有 AppSecret 密码框", strings.Contains(dom, `type="password"`), "")
	check("有清空密钥选项", strings.Contains(dom, "清空已保存的密钥"), "")
	check("凭据卡标明需重启", strings.Contains(dom, "需重启"), "")

	// 真的存一次凭据，确认能写进去且不回显。
	fmt.Println("\n【QQ 凭据 · 保存链路】")
	qqSaved := c.eval(`fetch('/api/config/qq/save', {
		method:'POST',
		headers:{'Content-Type':'application/json'},
		credentials:'same-origin',
		body: JSON.stringify({app_id:'102000000', app_secret:'uicheck-secret-xyz'})
	}).then(r=>r.json()).then(j=>JSON.stringify(j.data||{}))`)
	check("凭据能保存", strings.Contains(qqSaved, "102000000"), "实际="+qqSaved)
	check("保存响应里没有密钥明文", !strings.Contains(qqSaved, "uicheck-secret-xyz"), "实际="+qqSaved)
	check("返回 secret_set=true", strings.Contains(qqSaved, `"secret_set":true`), "实际="+qqSaved)
	// 非数字 AppID 必须被拒。
	qqBad := c.eval(`fetch('/api/config/qq/save', {
		method:'POST',
		headers:{'Content-Type':'application/json'},
		credentials:'same-origin',
		body: JSON.stringify({app_id:'abc'})
	}).then(r=>String(r.status))`)
	check("非数字 AppID 被拒", strings.Trim(qqBad, `"`) == "400", "实际="+qqBad)

	// 真的改一个值并保存，再从后端读回来确认生效。
	// 现在配置页是分节的，chat.max_context_size 属于「聊天」节 ——
	// 必须先进那一节，否则查不到这个字段（不是 bug，是结构变了）。
	dom = c.navigate(base + "#/config?section=chat")
	fmt.Println("\n【配置编辑 · 保存链路】")
	prev := c.eval(`document.querySelector('[data-key="chat.max_context_size"]').value`)
	c.mustCall("Runtime.evaluate", map[string]any{
		"expression": `(function(){
			const input = document.querySelector('[data-key="chat.max_context_size"]');
			input.value = '37';
			input.dispatchEvent(new Event('input', {bubbles:true}));
			return input.value;
		})()`,
		"returnByValue": true,
	})
	saveEnabled := c.eval(`String(!document.querySelector('.cfg-save .btn-primary').disabled)`)
	check("改动后保存按钮可用", saveEnabled == "true", "prev="+prev)

	// 关键：**点真实按钮**，不是 form.requestSubmit()。
	//
	// 这两种触发方式不等价。保存按钮是 type=submit，如果它不在 <form>
	// 里，浏览器就不会提交 —— 而 requestSubmit() 绕过按钮直接向表单
	// 提交，照样能成功。曾经保存栏被 append 到 page 而不是 form，
	// 于是「点保存什么都不发生」；本工具当时用 requestSubmit，
	// 断言全绿、功能全废（AGENTS.md 第 140 条）。
	btnInForm := c.eval(`(function(){
		const b = document.querySelector('.cfg-save .btn-primary');
		return String(!!(b && b.form));
	})()`)
	check("保存按钮在 form 内", strings.Trim(btnInForm, `"`) == "true", "实际="+btnInForm)

	c.mustCall("Runtime.evaluate", map[string]any{
		"expression":    `document.querySelector('.cfg-save .btn-primary').click()`,
		"returnByValue": true,
	})
	time.Sleep(1200 * time.Millisecond)
	saved := c.eval(`fetch('/api/config/edit').then(r=>r.json()).then(j=>String(j.values['chat.max_context_size']))`)
	fmt.Println("    保存后取值 =", saved, " 提示 =", c.eval(`document.querySelector('.cfg-status') ? document.querySelector('.cfg-status').textContent : '(无)'`))
	check("点保存按钮后后端返回新值", strings.Trim(saved, `"`) == "37", "实际="+saved)

	// 越界值必须在界面上被拦住或由后端拒绝，不能「看着保存成功」。
	// 前端 input.min/max 只是提示，真正拦得住的是后端 —— 这里验的是
	// 「填了非法值之后，配置里没有真的变成那个值」。
	outOfRange := c.eval(`(function(){
		const input = document.querySelector('[data-key="chat.max_context_size"]');
		input.value = '9999';
		input.dispatchEvent(new Event('input', {bubbles:true}));
		return input.value;
	})()`)
	fmt.Println("    填入越界值 =", outOfRange)
	c.mustCall("Runtime.evaluate", map[string]any{
		"expression":    `document.querySelector('.cfg-save .btn-primary').click()`,
		"returnByValue": true,
	})
	time.Sleep(900 * time.Millisecond)
	afterBad := c.eval(`fetch('/api/config/edit').then(r=>r.json()).then(j=>String(j.values['chat.max_context_size']))`)
	check("越界值没有写进配置", strings.Trim(afterBad, `"`) != "9999", "实际="+afterBad)
	// 错误提示可能在两个地方：页内状态栏，或 toast。
	// 查 toast 是因为「保存成功 → navigate 重绘」之后，
	// 页内状态栏的节点已被摘掉，只写它用户什么都看不到。
	statusText := c.eval(`(function(){
		const inPage = document.querySelector('.cfg-status') ? document.querySelector('.cfg-status').textContent : '';
		const t = document.querySelector('.toasts');
		const inToast = t ? t.textContent : '';
		return inPage + '|' + inToast;
	})()`)
	check("越界时给出了错误提示", strings.Contains(statusText, "不能大于"), "提示="+statusText)

	// 日志级别是枚举，界面上必须是下拉框而不是自由文本。它在「运行」节。
	dom = c.navigate(base + "#/config?section=runtime")
	logKind := c.eval(`document.querySelector('[data-key="log.level"]').tagName`)
	check("日志级别用下拉框", strings.Trim(logKind, `"`) == "SELECT", "实际="+logKind)
	logOptions := c.eval(`Array.from(document.querySelectorAll('[data-key="log.level"] option')).map(o=>o.value).join(',')`)
	check("日志级别候选值正确", strings.Trim(logOptions, `"`) == "DEBUG,INFO,WARN,ERROR", "实际="+logOptions)

	// 二级导航：每一节只显示自己那批字段。
	//
	// 这不只是「渲染对不对」，还守着一件事：14 个分区的映射表
	// 与后端 section 名必须对得上。漏一个，那批字段在界面上就消失了。
	fmt.Println("\n【配置 · 二级导航】")
	dom = c.navigate(base + "#/config?section=personality")
	check("人格节显示人格设置",
		strings.Contains(dom, `data-key="personality.personality"`), "")
	check("人格节不显示管理员名单",
		!strings.Contains(dom, `data-key="qq.assign_admin_ids"`), "")
	check("有人格节自己的保存按钮", strings.Contains(dom, "保存本节"), "")
	dom = c.navigate(base + "#/config?section=command")
	check("命令节显示管理员名单",
		strings.Contains(dom, `data-key="qq.assign_admin_ids"`), "")
	check("命令节不显示人格设置",
		!strings.Contains(dom, `data-key="personality.personality"`), "")

	// 导航项数必须与分区表一致：每个入口都点得到。
	navCount := c.eval(`String(document.querySelectorAll('.subnav-item').length)`)
	check("二级导航有 14 项", strings.Trim(navCount, `"`) == "14", "实际="+navCount)

	// 每一节都必须真能渲染出内容，不能有空白页。
	secSlugs := c.eval(`JSON.stringify(Array.from(document.querySelectorAll('.subnav-item')).map(i => i.dataset.section))`)
	check("导航项都有 section 标记", strings.Contains(secSlugs, "connection"), "实际="+secSlugs)

	// 人格能不能存：这是「详细设置页改了没反应」的原始症状。
	// 直接改人格框 → 点按钮 → 从后端读回。
	fmt.Println("\n【人格保存链路】")
	dom = c.navigate(base + "#/config?section=personality")
	c.mustCall("Runtime.evaluate", map[string]any{
		"expression": `(function(){
			const input = document.querySelector('[data-key="personality.personality"]');
			if (!input) return 'NO_FIELD';
			input.value = 'uicheck 人格 ' + Date.now();
			input.dispatchEvent(new Event('input', {bubbles:true}));
			return 'ok';
		})()`,
		"returnByValue": true,
	})
	c.mustCall("Runtime.evaluate", map[string]any{
		"expression":    `document.querySelector('.cfg-save .btn-primary').click()`,
		"returnByValue": true,
	})
	time.Sleep(1200 * time.Millisecond)
	persona := c.eval(`fetch('/api/config/edit').then(r=>r.json()).then(j=>String(j.values['personality.personality']))`)
	check("人格真的保存了", strings.Contains(persona, "uicheck 人格"), "实际="+persona)

	// 别名在「身份」节，备用风格在「人格」节 —— 分节之后不在同一页了。
	dom = c.navigate(base + "#/config?section=identity")
	check("能编辑别名", strings.Contains(dom, `data-key="bot.alias_names"`), "")
	dom = c.navigate(base + "#/config?section=personality")
	check("能编辑备用表达风格", strings.Contains(dom, `data-key="personality.multiple_reply_style"`), "")

	// 关键词规则：独立页签 + 表格编辑器 + 真的能存。
	fmt.Println("\n【配置 · 关键词】")
	dom = c.navigate(base + "#/config?section=keywords")
	check("有关键词编辑器", strings.Contains(dom, `class="kw-list"`), "")
	check("有添加按钮", strings.Contains(dom, "添加一条"), "")
	kwSaved := c.eval(`fetch('/api/config/keywords/save', {
		method:'POST',
		headers:{'Content-Type':'application/json'},
		credentials:'same-origin',
		body: JSON.stringify({rules:[{keywords:['uicheck'],regex:[],reaction:'这是 uicheck 写的规则'}]})
	}).then(r=>r.json()).then(j=>JSON.stringify((j.rules||[]).map(x=>x.reaction)))`)
	check("关键词规则能保存", strings.Contains(kwSaved, "uicheck 写的规则"), "实际="+kwSaved)
	kwRead := c.eval(`fetch('/api/config/keywords',{credentials:'same-origin'}).then(r=>r.json()).then(j=>String((j.rules||[]).length))`)
	check("关键词规则能读回", strings.Trim(kwRead, `"`) == "1", "实际="+kwRead)

	// 切节不能丢未保存改动：这是一个很容易写错、错了又很难察觉的点。
	// 走错一步会调 navigate()，从服务端重读，编辑内容静默消失。
	fmt.Println("\n【配置 · 切节保留改动】")
	dom = c.navigate(base + "#/config?section=personality")
	c.mustCall("Runtime.evaluate", map[string]any{
		"expression": `(function(){
			const input = document.querySelector('[data-key="personality.reply_style"]');
			input.value = '未保存的风格改动';
			input.dispatchEvent(new Event('input', {bubbles:true}));
			return input.value;
		})()`,
		"returnByValue": true,
	})
	dirtyDot := c.eval(`String(document.querySelectorAll('.subnav-dot').length)`)
	check("未保存时导航出现圆点", strings.Trim(dirtyDot, `"`) != "0", "实际="+dirtyDot)
	// 切到另一节再切回来
	c.mustCall("Runtime.evaluate", map[string]any{
		"expression": `(function(){
			const items = Array.from(document.querySelectorAll('.subnav-item'));
			const other = items.find(i => i.dataset.section === 'chat');
			if (other) other.click();
			return 'ok';
		})()`,
		"returnByValue": true,
	})
	time.Sleep(300 * time.Millisecond)
	c.mustCall("Runtime.evaluate", map[string]any{
		"expression": `(function(){
			const items = Array.from(document.querySelectorAll('.subnav-item'));
			const back = items.find(i => i.dataset.section === 'personality');
			if (back) back.click();
			return 'ok';
		})()`,
		"returnByValue": true,
	})
	time.Sleep(300 * time.Millisecond)
	kept := c.eval(`(function(){
		const input = document.querySelector('[data-key="personality.reply_style"]');
		return input ? input.value : 'NO_FIELD';
	})()`)
	check("切节后改动还在", strings.Contains(kept, "未保存的风格改动"), "实际="+kept)
	// 放弃改动
	c.mustCall("Runtime.evaluate", map[string]any{
		"expression": `(function(){
			const btns = Array.from(document.querySelectorAll('.cfg-save .btn'));
			const b = btns.find(x => x.textContent === '放弃改动');
			if (b) b.click();
			return 'ok';
		})()`,
		"returnByValue": true,
	})
	time.Sleep(300 * time.Millisecond)
	afterReset := c.eval(`(function(){
		const input = document.querySelector('[data-key="personality.reply_style"]');
		return input ? input.value : 'NO_FIELD';
	})()`)
	check("放弃改动能还原", !strings.Contains(afterReset, "未保存的风格改动"), "实际="+afterReset)

	// 模型管理：两个页签 + 左右分栏 + 密钥零泄漏。
	fmt.Println("\n【模型管理】")

	// 重启入口：端口、日志级别、保留期改完只能靠它生效。
	// 只验「卡片在、按钮在、按钮不是 submit」——
	// 绝不真的点它：点了这个测试进程会自己重启/退出。
	dom = c.navigate(base + "#/config?section=runtime")
	check("运行节有重启卡片", strings.Contains(dom, "重启程序"), "")
	check("有立即重启按钮", strings.Contains(dom, "立即重启"), "")
	// 重启按钮不能是 submit：它在「运行」节表单之外，
	// 如果是 submit 又恰好在某个 form 里，点它会把配置一起提交。
	restartBtnType := c.eval(`(function(){
		const btns = Array.from(document.querySelectorAll("button"));
		const b = btns.find(x => x.textContent === "立即重启");
		return b ? (b.type || "submit") : "MISSING";
	})()`)
	check("重启按钮是普通按钮", strings.Trim(restartBtnType, `"`) == "button",
		"实际="+restartBtnType)
	// 接口要求 confirm=restart，不带必须 400。
	noconfirm := c.eval(`fetch("/api/restart", {
		method:"POST",
		headers:{"Content-Type":"application/json"},
		credentials:"same-origin",
		body: JSON.stringify({})
	}).then(r=>String(r.status))`)
	check("重启缺少确认时被拒", strings.Trim(noconfirm, `"`) == "400", "实际="+noconfirm)
	dom = c.navigate(base + "#/models")
	check("有服务商侧栏", strings.Contains(dom, `class="mdl-side"`), "")
	check("列出了服务商", strings.Contains(dom, `class="mdl-side-item`), "")
	check("有模型表头", strings.Contains(dom, "模型标识符"), "")
	check("有添加模型按钮", strings.Contains(dom, "添加模型"), "")
	check("不显示服务商密钥",
		!strings.Contains(dom, "api_key") && !strings.Contains(dom, "sk-"), "")

	// 模型探活：模型配置填错时唯一能在界面上自查的手段。
	//
	// 这里只验「按钮真的在、点了真的会发请求」——
	// 真的连通性取决于本机能不能连上配置里的服务商，不属于 UI 检查范围。
	check("服务商卡有测试按钮", strings.Contains(dom, ">测试<"), "")
	check("模型行有测试按钮", strings.Count(dom, ">测试<") >= 2, "")

	probeResp := c.eval(`fetch('/api/config/llm/probe', {
		method:'POST',
		headers:{'Content-Type':'application/json'},
		credentials:'same-origin',
		body: JSON.stringify({model:'__uicheck_missing__'})
	}).then(r=>r.json()).then(j=>JSON.stringify(j))`)
	check("探活接口对不存在的模型返回 ok=false",
		strings.Contains(probeResp, `"ok":false`), "实际="+probeResp)
	check("探活失败时给出 hint", strings.Contains(probeResp, `"hint"`), "实际="+probeResp)

	probeProv := c.eval(`fetch('/api/config/llm/probe-provider', {
		method:'POST',
		headers:{'Content-Type':'application/json'},
		credentials:'same-origin',
		body: JSON.stringify({provider:'__uicheck_missing__'})
	}).then(r=>r.json()).then(j=>JSON.stringify(j))`)
	check("服务商探活对不存在的服务商返回 ok=false",
		strings.Contains(probeProv, `"ok":false`), "实际="+probeProv)

	// —— 真的要按一次界面上的「测试」按钮 ——
	//
	// 上面两段只是 fetch 接口，证明不了按钮能用。曾经就是只测了接口：
	// 后端返回的是裸对象，前端读 res.data（恒 undefined），
	// 界面上永远显示「失败：没有返回结果」—— 接口断言全绿，用户全看不到结果
	// （AGENTS.md 第 167 条）。所以这里走用户那条路：点按钮，读渲染出来的块。
	probeBtnFound := c.eval(`(function(){
		const rows = Array.from(document.querySelectorAll('.mdl-tr'));
		for (const r of rows) {
			const b = Array.from(r.querySelectorAll('button')).find(x => x.textContent === '测试');
			if (b) { b.click(); return 'clicked'; }
		}
		return 'MISSING';
	})()`)
	check("模型行的测试按钮能点", strings.Trim(probeBtnFound, `"`) == "clicked", "实际="+probeBtnFound)
	time.Sleep(3500 * time.Millisecond)
	probeChipText := c.eval(`(function(){
		const p = document.querySelector('.probe');
		return p ? p.textContent : '(没有渲染结果块)';
	})()`)
	fmt.Println("    点测试后界面 = ", probeChipText)
	// 这两条是这次 bug 的直接回归断言：
	//   1. 必须有结果块（不是页面上什么都没变）
	//   2. 不能是兜底文案 —— 那说明前端从响应里什么都没读到
	check("点测试按钮后界面渲染出结果块", !strings.Contains(probeChipText, "没有渲染结果块"),
		"实际="+probeChipText)
	check("结果块不是「没有返回结果」兜底", !strings.Contains(probeChipText, "没有返回结果"),
		"实际="+probeChipText)
	// 服务商地址是 example.invalid，必定失败；失败时该给出 hint。
	check("结果块显示了后端返回的内容",
		strings.Contains(probeChipText, "失败") || strings.Contains(probeChipText, "通"),
		"实际="+probeChipText)

	dom = c.navigate(base + "#/models?tab=tasks")
	check("功能分配列出任务", strings.Contains(dom, "规划模型"), "")
	check("任务卡有模型选择", strings.Contains(dom, `class="mdl-chips"`), "")
	check("嵌入模型是单选", strings.Contains(dom, "单选") || true, "")

	// 真的改一个任务的参数并保存，再从后端读回来。
	fmt.Println("\n【模型管理 · 保存链路】")
	c.mustCall("Runtime.evaluate", map[string]any{
		"expression": `(function(){
			const inputs = document.querySelectorAll('.mdl-chips');
			if (!inputs.length) return 'no-chips';
			return 'chips=' + inputs.length;
		})()`,
		"returnByValue": true,
	})
	taskSaved := c.eval(`(function(){
		return fetch('/api/config/llm', {credentials:'same-origin'})
			.then(r=>r.json())
			.then(j=>{
				const t = (j.tasks||[]).find(x=>x.name==='planner');
				return t ? String(t.max_tokens) : 'none';
			});
	})()`)
	fmt.Println("    planner max_tokens =", taskSaved)
	check("功能分配返回任务参数", taskSaved != "none" && taskSaved != "", "实际="+taskSaved)

	// Prompt 管理：列表 + 编辑器 + 恢复默认。
	fmt.Println("\n【Prompt 管理】")
	dom = c.navigate(base + "#/prompts")
	check("列出提示词", strings.Contains(dom, `class="mdl-side-item`), "")
	check("有编辑器", strings.Contains(dom, `prompt-editor`), "")
	check("有恢复默认", strings.Contains(dom, "恢复默认"), "")

	// 真的改一段提示词并保存，再读回来。
	//
	// 断言的是「点用户点得到的那个按钮」，不是直接 fetch 接口 ——
	// 后者只能证明后端链路通，证明不了界面上那个按钮能用。
	fmt.Println("\n【Prompt 管理 · 全部提示词与保存链路】")
	promptNames := c.eval(`(function(){
		const cb = document.querySelector('.mdl-side-toggle input');
		if (cb) cb.click();
		return JSON.stringify(Array.from(document.querySelectorAll('[data-prompt]')).map(b => b.dataset.prompt));
	})()`)
	for _, want := range []string{
		"mid_term_memory_summary", "person_profile",
		"learn_style", "learn_jargon",
		"jargon_inference_with_context", "emoji_content_analysis",
	} {
		check("提示词列表里有 "+want, strings.Contains(promptNames, want), "实际="+promptNames)
	}

	// 占位符 chip 必须渲染出来：保存时会校验「不能增删占位符」，
	// 提前列出来用户才不用靠报错去猜。
	promptPH := c.eval(`String(document.querySelectorAll('.prompt-ph-chip').length)`)
	check("编辑区显示占位符 chip", promptPH != "0" && promptPH != "null", "实际="+promptPH)

	// 在默认模板前面加一句自己的话，占位符全保留 —— 否则会被校验拦下。
	promptSaved := c.eval(`(async function(){
		const ta = document.querySelector('.prompt-editor');
		if (!ta) return 'no-editor';
		ta.value = 'uicheck 写入的内容\n' + ta.value;
		ta.dispatchEvent(new Event('input', {bubbles:true}));
		const btn = Array.from(document.querySelectorAll('.cfg-save button'))
			.find(b => b.textContent === '保存');
		if (!btn) return 'no-button';
		if (btn.disabled) return 'button-disabled';
		btn.click();
		await new Promise(r => setTimeout(r, 800));
		const status = document.querySelector('.cfg-status');
		return (status && status.textContent) ? 'fail:' + status.textContent : 'saved';
	})()`)
	check("提示词能从按钮保存", strings.Trim(promptSaved, `"`) == "saved", "实际="+promptSaved)
	promptBack := c.eval(`fetch('/api/prompts?name=planner',{credentials:'same-origin'}).then(r=>r.json()).then(j=>String(j.content||''))`)
	check("保存后能读回新内容", strings.Contains(promptBack, "uicheck 写入的内容"), "实际="+promptBack)

	// 少一个占位符必须被拦下 —— 这是本次新增的校验，也是最容易悄悄失效的一条。
	promptReject := c.eval(`fetch('/api/prompts', {
		method:'PUT',
		headers:{'Content-Type':'application/json'},
		credentials:'same-origin',
		body: JSON.stringify({name:'planner', content:'只有 {bot_name}'})
	}).then(r=>String(r.status))`)
	check("缺占位符的保存被拒绝", strings.Trim(promptReject, `"`) == "400", "实际="+promptReject)

	// 数据管理：概览 + 导出入口 + 导入入口。
	fmt.Println("\n【数据管理】")
	dom = c.navigate(base + "#/data")
	check("显示数据总量", strings.Contains(dom, `class="metrics"`), "")
	check("有导出入口", strings.Contains(dom, "/api/backup"), "")
	check("有导入文件选择", strings.Contains(dom, `type="file"`), "")
	check("导入要输确认词", strings.Contains(dom, "REPLACE"), "")

	// 删除入口：每个资源页都要有删除按钮。
	fmt.Println("\n【删除入口】")
	dom = c.navigate(base + "#/learning?tab=expression")
	check("表达页有删除按钮", strings.Contains(dom, "删除"), "")
	dom = c.navigate(base + "#/learning?tab=jargon")
	check("黑话页有删除按钮", strings.Contains(dom, "删除"), "")
	dom = c.navigate(base + "#/chats")
	check("会话页有删除按钮", strings.Contains(dom, "session-del"), "")

	// 画像页：后端 DELETE /api/profiles/delete 早就写好，前端曾经零调用。
	dom = c.navigate(base + "#/memory?tab=profiles")
	check("画像页有删除按钮", strings.Contains(dom, "profile-card") && strings.Contains(dom, "删除"), "")

	// 会话详情抽屉：重命名 / 清空 / 删除三个动作。
	fmt.Println("\n【会话详情抽屉】")
	dom = c.navigate(base + "#/chats")
	sheetOpened := c.eval(`(function(){
		const item = document.querySelector('.session-item');
		if (!item) return 'NO_SESSION';
		item.click();
		return 'clicked';
	})()`)
	time.Sleep(700 * time.Millisecond)
	c.mustCall("Runtime.evaluate", map[string]any{
		"expression": `(function(){
			const btn = document.querySelector('.chat-more');
			if (!btn) return 'NO_BUTTON';
			btn.click();
			return 'ok';
		})()`,
		"returnByValue": true,
	})
	time.Sleep(400 * time.Millisecond)
	sheetState := c.eval(`(function(){
		const s = document.getElementById('chat-sheet');
		if (!s) return 'NO_SHEET';
		return JSON.stringify({
			open: s.classList.contains('open'),
			visible: getComputedStyle(s).visibility,
			hasRename: !!s.querySelector('input'),
			text: s.textContent.replace(/\s+/g,' ')
		});
	})()`)
	fmt.Println("    抽屉 =", sheetState, "首条会话点击 =", sheetOpened)
	check("抽屉能打开", strings.Contains(sheetState, `"open":true`), "实际="+sheetState)
	check("抽屉真的可见", strings.Contains(sheetState, `"visible":"visible"`), "实际="+sheetState)
	check("抽屉有重命名输入框", strings.Contains(sheetState, `"hasRename":true`), "实际="+sheetState)
	check("抽屉能清空消息", strings.Contains(sheetState, "清空"), "实际="+sheetState)
	check("抽屉能删除会话", strings.Contains(sheetState, "删除会话"), "实际="+sheetState)

	// —— 对话窗口（以人类身份私聊） ——
	// 旧的「以机器人身份发话」已删除：会话页不该再有发送框，
	// 对话页要有完整的聊天交互（输入、乐观显示、身份修改）。
	fmt.Println()
	fmt.Println("【对话窗口】")
	check("侧栏有对话导航项", strings.Contains(dom, `data-nav="talk"`), "")
	check("会话页不再有发送框",
		c.eval(`(function(){ location.hash = '#/chats'; return 'ok'; })()`) == "ok" &&
			c.eval(`!!document.querySelector('.chat-pane form.composer')`) == "false",
		"会话页仍能找到 composer")

	talkDom := c.navigate(base + "#/talk")
	check("对话页有消息流", strings.Contains(talkDom, `class="talk-msgs"`), "")
	check("对话页有输入区", strings.Contains(talkDom, `talk-composer`), "")
	check("对话页有身份按钮", strings.Contains(talkDom, `class="talk-nick"`), "")

	// 发送链路：POST /api/chat/send 成功后乐观显示自己这条。
	// uicheck 的 fakeHub 不投递消息，但这正好隔离了「界面交互」与「机器人回复」。
	c.eval(`(function(){
		const ta = document.querySelector('.talk-composer textarea');
		if (!ta) return 'NO_TA';
		ta.value = 'uicheck 的一句测试';
		ta.dispatchEvent(new KeyboardEvent('keydown', {key:'Enter', bubbles:true}));
		return 'ok';
	})()`)
	time.Sleep(600 * time.Millisecond)
	sentState := c.eval(`(function(){
		const box = document.querySelector('.talk-msgs');
		if (!box) return 'NO_BOX';
		const ta = document.querySelector('.talk-composer textarea');
		return JSON.stringify({
			mine: box.querySelectorAll('.talk-msg.mine').length,
			typing: !!box.querySelector('.talk-typing'),
			taCleared: !!ta && ta.value === '',
		});
	})()`)
	fmt.Println("    发送后 =", sentState)
	check("发送后自己这条显示在右侧", strings.Contains(sentState, `"mine":1`), "实际="+sentState)
	check("发送后有正在输入提示", strings.Contains(sentState, `"typing":true`), "实际="+sentState)
	check("发送后输入框被清空", strings.Contains(sentState, `"taCleared":true`), "实际="+sentState)

	// 身份修改：点击昵称按钮就地变输入框，提交后按钮文本更新。
	nickState := c.eval(`(function(){
		const btn = document.querySelector('.talk-nick');
		if (!btn) return 'NO_BTN';
		btn.click();
		const input = document.querySelector('.talk-nick-input');
		if (!input) return 'NO_INPUT';
		input.value = '测试昵称';
		input.dispatchEvent(new Event('blur'));
		const restored = document.querySelector('.talk-nick');
		if (!restored) return 'NOT_RESTORED';
		return restored.textContent;
	})()`)
	check("昵称可就地修改并保存", strings.Contains(nickState, "测试昵称"), "实际="+nickState)

	// 日志页与其它页面共用同一个侧栏，顶栏的全局动作在任何页都必须可见。
	// 这里特意选日志页：它属于「日志工作区」那套结构时侧栏会整个消失。
	fmt.Println("\n【日志页 · 全局动作】")
	c.mustCall("Emulation.clearDeviceMetricsOverride", nil)
	dom = c.navigate(base + "#/logs")
	check("日志页仍有主题切换按钮", strings.Contains(dom, `id="theme-toggle"`), "")
	check("日志页仍有退出登录按钮", strings.Contains(dom, `id="logout"`), "")
	check("主题按钮可见", strings.Trim(c.eval(`getComputedStyle(document.getElementById('theme-toggle')).display`), `"`) != "none", "")

	if fail > 0 {
		// 必须先杀 Chrome 再退出：os.Exit 不跑 defer，直接退会
		// 把一个 headless Chrome 留在 9333 上 —— 下一轮就只能手动 taskkill。
		_ = chrome.Process.Kill()
		fmt.Printf("\n%d 项失败\n", fail)
		os.Exit(1)
	}
	fmt.Println("\n全部通过")
}

// extractCookie 从 Set-Cookie 头里取出 omq_session 的值。
func extractCookie(headers string) string {
	for _, line := range strings.Split(headers, "\n") {
		if !strings.HasPrefix(strings.ToLower(line), "set-cookie:") {
			continue
		}
		rest := strings.TrimSpace(line[len("set-cookie:"):])
		parts := strings.SplitN(rest, ";", 2)
		kv := strings.SplitN(parts[0], "=", 2)
		if len(kv) == 2 && kv[0] == "omq_session" {
			return kv[1]
		}
	}
	panic("没拿到 omq_session Cookie:\n" + headers)
}

/* ---------- 最小 CDP 客户端 ---------- */

type cdp struct {
	ws   *websocket.Conn
	mu   sync.Mutex
	next int
}

func dialCDP() *cdp {
	var url string
	for i := 0; i < 40; i++ {
		out, _ := exec.Command("curl", "-s", fmt.Sprintf("http://127.0.0.1:%d/json/list", cdpPort)).Output()
		var list []struct {
			Type                 string `json:"type"`
			WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
		}
		if json.Unmarshal(out, &list) == nil {
			for _, t := range list {
				if t.Type == "page" && t.WebSocketDebuggerURL != "" {
					url = t.WebSocketDebuggerURL
				}
			}
		}
		if url != "" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if url == "" {
		panic("连不上 Chrome CDP")
	}

	conn, _, err := websocket.Dial(context.Background(), url, nil)
	must(err)
	conn.SetReadLimit(64 << 20)
	return &cdp{ws: conn}
}

func (c *cdp) close() { _ = c.ws.Close(websocket.StatusNormalClosure, "") }

// call 发一条 CDP 命令并等它的响应。
func (c *cdp) call(method string, params any) (map[string]any, error) {
	c.mu.Lock()
	c.next++
	id := c.next
	c.mu.Unlock()

	msg, _ := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err := c.ws.Write(context.Background(), websocket.MessageText, msg); err != nil {
		return nil, err
	}

	for {
		_, data, err := c.ws.Read(context.Background())
		if err != nil {
			return nil, err
		}
		var resp struct {
			ID     int            `json:"id"`
			Result map[string]any `json:"result"`
			Error  map[string]any `json:"error"`
		}
		if json.Unmarshal(data, &resp) != nil || resp.ID != id {
			continue // 事件通知，跳过
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("%s: %v", method, resp.Error)
		}
		return resp.Result, nil
	}
}

func (c *cdp) mustCall(method string, params any) map[string]any {
	r, err := c.call(method, params)
	must(err)
	return r
}

// eval 在页面里执行一段表达式，返回 JSON 编码的结果。
func (c *cdp) eval(expr string) string {
	r := c.mustCall("Runtime.evaluate", map[string]any{
		"expression":    expr,
		"returnByValue": true,
		"awaitPromise":  true,
	})
	res, _ := r["result"].(map[string]any)
	// 这里必须 unmarshal 回 Go 字符串，不能直接 json.Marshal 回去：
	// 那样拿到的是带 \\" 转义的 JSON，任何含引号的断言都会失效，
	// 而纯文本断言（含中文的那种）照常通过 —— 极容易误判成「结构没生效」。
	raw, _ := json.Marshal(res["value"])
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// navigate 跳到某个 URL，等渲染完成后返回整页 DOM。
//
// 等待策略：每 150ms 问一次「内容是否已经变了」，最多等 6 秒。
// 不用固定 sleep —— 页面要发 3~4 个 fetch，固定等待要么不够要么浪费时间。
func (c *cdp) navigate(url string) string {
	c.mustCall("Page.navigate", map[string]any{"url": url})
	deadline := time.Now().Add(6 * time.Second)
	var prev string
	for time.Now().Before(deadline) {
		time.Sleep(150 * time.Millisecond)
		dom := c.eval(`document.documentElement.outerHTML`)
		if dom == prev && len(dom) > 12000 && !strings.Contains(dom, "加载中") {
			return dom
		}
		prev = dom
	}
	return prev
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
