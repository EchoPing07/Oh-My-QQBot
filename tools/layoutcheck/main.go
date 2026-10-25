// Command layoutcheck 用真实浏览器找布局问题：文字溢出、元素重叠、越界。
//
// 与 uicheck 的分工：
//
//	uicheck     —— 「功能对不对」（点按钮能不能存、页签切了没有内容）
//	layoutcheck —— 「长得对不对」（文字有没有被切、两块有没有压在一起、
//	                元素有没有跑到容器外面）
//
// 为什么需要独立一个：这类问题在字符串断言里完全不可见，在 uicheck 的
// DOM 断言里也不可见（元素存在、文本正确，只是被 `overflow: hidden`
// 裁掉了一半，或者被绝对定位的兄弟盖住）。只有拿到 getBoundingClientRect
// 与 scrollWidth/clientWidth 才知道。
//
// 检测的四类问题：
//
//  1. 文字被裁切：scrollWidth > clientWidth + 1 且 overflow 不是 visible
//     （省略号是显式设计，会检查 text-overflow: ellipsis 后放过）
//  2. 文字溢出容器：元素的 rect 超出最近的 overflow:hidden 祖先
//  3. 元素重叠：两个可见的交互元素 rect 有交叠（包含关系除外）
//  4. 横向溢出：文档整体 scrollWidth > clientWidth（出现横向滚动条）
//
// 用法（需要本机装了 Chrome）：
//
//	go run ./tools/layoutcheck
//	go run ./tools/layoutcheck -width 390   # 只看某个视口宽度
//
// 退出码非 0 表示发现问题。
package main

import (
	"context"
	"encoding/json"
	"flag"
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
	"github.com/ohmyqqbot/oh-my-qqbot/internal/prompt"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/store"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/web"
)

const (
	uiPort  = 7812
	cdpPort = 9334
)

type fakeHub struct{}

func (fakeHub) ActiveCount() int { return 2 }

// Dispatch 满足 web.Hub 接口。两个检查工具都不投递消息，给空实现。
func (fakeHub) Dispatch(*domain.Message, string, string, string) {}

// 检查的目标页面。顺序按侧栏。
//
// prep 是「打开页面后先做一件事」的可选脚本，用来检查交互后的状态：
// 抽屉、展开面板、hover 才出现的按钮 —— 这些元素在首屏 DOM 里
// 要么不存在、要么是 display:none，不打开就永远查不到它们的布局。
var pages = []struct {
	name string
	hash string
	prep string
}{
	{"概览", "#/overview", ""},
	{"会话", "#/chats", `(function(){
		// 点开第一条会话，再打开详情抽屉：抽屉是绝对定位的浮层，
		// 最容易在窄屏上盖住内容或超出容器。
		const item = document.querySelector('.session-item');
		if (item) item.click();
		return 'ok';
	})()`},
	{"会话-抽屉", "#/chats", `(function(){
		const item = document.querySelector('.session-item');
		if (item) item.click();
		setTimeout(function(){
			const b = document.querySelector('.chat-more');
			if (b) b.click();
		}, 200);
		return 'ok';
	})()`},
	// 对话窗口有输入区与消息流，是布局问题高发区（气泡宽度、输入框高度）。
	{"对话", "#/talk", ""},
	{"对话-有消息", "#/talk", `(function(){
		// 预放两条消息再量布局：空流与有消息的流，气泡的 max-width
		// 表现完全不同。头像样式类与会话页共用 avatar 体系。
		const box = document.querySelector('.talk-msgs');
		if (!box) return 'NO_BOX';
		function add(cls, text) {
			const row = document.createElement('div');
			row.className = 'talk-msg' + (cls ? ' ' + cls : '');
			const col = document.createElement('div');
			col.className = 'talk-col';
			const b = document.createElement('div');
			b.className = 'talk-bubble';
			b.textContent = text;
			col.appendChild(b);
			row.appendChild(col);
			box.appendChild(row);
		}
		add('', '木木的一句话，用来量左侧气泡的宽度与换行。'.repeat(3));
		add('mine', '我的一句比较长的话，用来量右侧气泡在窄屏下是否越界。'.repeat(3));
		box.scrollTop = box.scrollHeight;
		return 'ok';
	})()`},
	{"配置-连接", "#/config?section=connection", ""},
	{"配置-身份", "#/config?section=identity", ""},
	{"配置-人格", "#/config?section=personality", ""},
	{"配置-聊天", "#/config?section=chat", ""},
	{"配置-引用", "#/config?section=quote", ""},
	{"配置-回复", "#/config?section=response", ""},
	{"配置-过滤", "#/config?section=filter", ""},
	{"配置-记忆", "#/config?section=memory", ""},
	{"配置-表情包", "#/config?section=emoji", ""},
	{"配置-学习", "#/config?section=learning", ""},
	{"配置-关键词", "#/config?section=keywords", ""},
	{"配置-命令", "#/config?section=command", ""},
	{"配置-运行", "#/config?section=runtime", ""},
	{"配置-信息", "#/config?section=info", ""},
	{"模型-服务商", "#/models", ""},
	// 探活结果卡是动态插入的 DOM，不点一次「测试」就永远不存在。
	// 它有错误原文、建议、模型列表三块文字，是最容易溢出的那种内容。
	{"模型-探活结果", "#/models", `(async function(){
		// 等列表渲染完再点。
		for (let i = 0; i < 40; i++) {
			const b = Array.from(document.querySelectorAll('.mdl-card-head button'))
				.find(x => x.textContent === '测试');
			if (b) { b.click(); return 'clicked'; }
			await new Promise(r => setTimeout(r, 50));
		}
		return 'no-button';
	})()`},
	{"模型-任务", "#/models?tab=tasks", ""},
	{"提示词", "#/prompts", ""},
	{"数据", "#/data", ""},
	{"记忆-回想", "#/memory?tab=recall", ""},
	{"记忆-画像", "#/memory?tab=profiles", ""},
	{"学习-表达", "#/learning?tab=expression", ""},
	{"学习-黑话", "#/learning?tab=jargon", ""},
	{"学习-表情包", "#/learning?tab=emoji", ""},
	{"日志", "#/logs", ""},
	// 提示词页有两个容易溢出的动态块：
	//   1. 「显示高级项」开关展开后侧栏变成 11 条（窄屏是横向滚动）
	//   2. 占位符 chip 里最长的 {group_chat_attention_block} 在手机上很宽
	{"提示词-高级项", "#/prompts", `(function(){
		const cb = document.querySelector('.mdl-side-toggle input');
		if (!cb) return 'no-toggle';
		cb.click();
		return 'clicked';
	})()`},
	// 默认选中 planner，它的占位符最多（7 个），是最容易撑破卡片的情况。
	{"提示词-占位符", "#/prompts", ""},
	// 用量统计已归并进概览页：它的「按任务」表格是首页里唯一一个表格，
	// 而表格正是窄屏最容易横向溢出的东西。
	{"概览-模型调用", "#/overview", ""},
}

// 视口。桌面宽度取常见的 1440，窄屏取 iPhone 的 390。
var viewports = []struct {
	name  string
	width int
	// mobile=true 时同时打开触摸模拟：CSS 里的 @media (hover: none)
	// 会因此生效，而它决定了不少「常驻显示的删除按钮」。
	mobile bool
}{
	{"桌面 1440", 1440, false},
	{"窄屏 1024", 1024, false},
	{"手机 390", 390, true},
}

var (
	onlyWidth = flag.Int("width", 0, "只检查这个视口宽度（0 表示全部）")
	onlyPage  = flag.String("page", "", "只检查名字包含该字符串的页面")
	verbose   = flag.Bool("v", false, "打印每个页面检查了多少个元素")
)

var failures int

func main() {
	flag.Parse()

	dir, err := os.MkdirTemp("", "omlayout")
	must(err)

	srv, db, stop := startServer(dir)
	defer stop()
	defer db.Close()

	// 端口占用保护：残留的 Chrome 会让本工具连到旧实例，
	// 对着旧代码报一堆假问题（参见 AGENTS.md 第 142 条）。
	if conn, e := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", cdpPort), 400*time.Millisecond); e == nil {
		_ = conn.Close()
		panic(fmt.Sprintf("调试端口 %d 已被占用：很可能有上一轮残留的 Chrome。"+
			"\n先结束它再跑（Windows: taskkill /F /PID <pid>）。", cdpPort))
	}
	_ = srv

	loginOut, err := exec.Command("curl", "-s", "-X", "POST",
		"-H", "Content-Type: application/json",
		"-d", `{"password":"check-pass"}`,
		fmt.Sprintf("http://127.0.0.1:%d/api/login", uiPort),
		"-D", "-", "-o", os.DevNull).CombinedOutput()
	must(err)
	cookie := extractCookie(string(loginOut))
	if cookie == "" {
		panic("登录失败，拿不到 Cookie")
	}

	chrome := exec.Command(`C:\Program Files\Google\Chrome\Application\chrome.exe`,
		"--headless=new", "--disable-gpu", "--no-first-run",
		"--no-default-browser-check",
		"--remote-debugging-port="+fmt.Sprint(cdpPort),
		"--user-data-dir="+filepath.Join(dir, "chrome"),
		"about:blank",
	)
	must(chrome.Start())
	defer func() {
		_ = chrome.Process.Kill()
		_, _ = chrome.Process.Wait()
	}()

	c, err := dialCDP(cookie)
	must(err)
	defer c.close()

	for _, vp := range viewports {
		if *onlyWidth != 0 && vp.width != *onlyWidth {
			continue
		}
		fmt.Printf("\n========== 视口 %s（%dpx）==========\n", vp.name, vp.width)
		c.setViewport(vp.width, vp.mobile)

		for _, p := range pages {
			if *onlyPage != "" && !strings.Contains(p.name, *onlyPage) {
				continue
			}
			c.navigate("http://127.0.0.1:" + fmt.Sprint(uiPort) + "/" + p.hash)
			time.Sleep(350 * time.Millisecond)
			if p.prep != "" {
				c.eval(p.prep)
				time.Sleep(500 * time.Millisecond)
			}

			report := c.audit()
			var probs []problem
			if err := json.Unmarshal([]byte(report), &probs); err != nil {
				fmt.Printf("  %-14s 解析失败: %v\n", p.name, err)
				failures++
				continue
			}

			if len(probs) == 0 {
				if *verbose {
					fmt.Printf("  %-14s OK\n", p.name)
				}
				continue
			}
			fmt.Printf("  %-14s %d 个问题\n", p.name, len(probs))
			for i, pr := range probs {
				if i >= 8 {
					fmt.Printf("      … 还有 %d 个\n", len(probs)-8)
					break
				}
				fmt.Printf("      [%s] %s\n", pr.Kind, pr.Detail)
			}
			failures += len(probs)
		}
	}

	fmt.Println()
	if failures > 0 {
		fmt.Printf("发现 %d 个布局问题\n", failures)
		os.Exit(1)
	}
	fmt.Println("布局检查全部通过")
}

// problem 是一条布局问题。
type problem struct {
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

// ---------------------------------------------------------------------------
// 服务端与数据
// ---------------------------------------------------------------------------

func startServer(dir string) (*web.Server, *store.DB, func()) {
	cfg := config.Default()
	cfg.Web.Password = "check-pass"
	cfg.Web.Host = "127.0.0.1"
	cfg.Web.Port = uiPort
	cfg.Bot.Nickname = "木木"
	// 长昵称与长别名：最容易撑破布局的就是长文本，
	// 用短名字测等于没测。
	cfg.Bot.AliasNames = []string{"小木", "omq", "木木同学", "一个特别特别长的别名用来测试溢出"}
	cfg.LLM.Providers = []config.ProviderConfig{{
		Name:    "check-provider-with-a-very-long-name-for-overflow-test",
		BaseURL: "https://example.invalid/v1",
		APIKey:  "sk-uicheck", Timeout: 60, MaxRetry: 2, RetryInterval: 3,
	}}
	cfg.LLM.Models = []config.ModelConfig{{
		Name: "check-model", ModelIdentifier: "check-ident", Provider: "check-provider-with-a-very-long-name-for-overflow-test",
	}}
	cfg.Keyword.Rules = []config.KeywordRule{{
		Keywords: []string{"猫", "狗"},
		Reaction: "用户可能在说宠物，可以顺着聊这个话题，但不要问太多问题",
	}}

	db, err := store.Open(context.Background(), filepath.Join(dir, "c.db"))
	must(err)

	ctx := context.Background()
	now := time.Now()
	// 会话名故意放长的中文：列表项必须能省略而不是撑破。
	must(db.Sessions().Upsert(ctx, &domain.Session{
		ID: "s1", Platform: "qq", Scope: domain.ScopeGroup, TargetID: "g1",
		DisplayName:  "一个非常非常长的群名称用来测试会话列表的溢出处理是否正确",
		MessageCount: 42, LastActiveAt: now, CreatedAt: now,
	}))
	for i := 0; i < 3; i++ {
		must(db.Messages().Save(ctx, &domain.Message{
			ID: fmt.Sprintf("m%d", i), SessionID: "s1", MessageID: fmt.Sprintf("p%d", i),
			Platform: "qq", UserID: "u1", UserName: "阿一", IsGroup: true,
			PlainText:  "这是一条比较长的消息用来测试气泡里的文字是否会溢出容器边界或者把布局撑破",
			Segments:   []domain.Segment{domain.Text("这是一条比较长的消息用来测试气泡里的文字是否会溢出容器边界或者把布局撑破")},
			ReceivedAt: now.Add(time.Duration(i) * time.Second),
		}))
	}
	must(db.Memory().UpsertProfile(ctx, &store.PersonProfile{
		Platform: "qq", UserID: "u1", SessionID: "s1", DisplayName: "阿一",
		ProfileText:  strings.Repeat("喜欢摄影，经常在周末出门拍照。", 8),
		MessageCount: 12, UpdatedAt: now.Unix(),
	}))
	_, err = db.Memory().SaveExpression(ctx, &store.Expression{
		SessionID: "s1", Situation: "被夸奖", Style: "嘿嘿谢啦",
	})
	must(err)
	must(db.Memory().SaveJargon(ctx, &store.Jargon{
		Content: "破防", Meaning: "情绪崩溃", Count: 5,
		CreatedAt: now.Unix(), UpdatedAt: now.Unix(),
	}))

	pm, err := prompt.New(filepath.Join(dir, "prompts"), "zh-CN")
	must(err)

	s := web.NewServer(web.Options{
		Config: config.Wrap(cfg), Store: db, Hub: fakeHub{}, Prompts: pm,
	})
	must(s.Start())
	time.Sleep(300 * time.Millisecond)

	return s, db, func() { s.Stop() }
}

// ---------------------------------------------------------------------------
// CDP
// ---------------------------------------------------------------------------

type cdp struct {
	ws   *websocket.Conn
	mu   sync.Mutex
	next int
}

// dialCDP 连上 **page 级** 的调试端点。
//
// 不能连 /json/version 给的那个 browser 级地址：Runtime.evaluate
// 在 browser target 上不可用，而且**不报错**，所有 evaluate 都返回空。
// 那个现象特别像「页面没渲染」，实际是连错了 target。
func dialCDP(cookie string) (*cdp, error) {
	var wsURL string
	for i := 0; i < 60; i++ {
		out, _ := exec.Command("curl", "-s",
			fmt.Sprintf("http://127.0.0.1:%d/json/list", cdpPort)).Output()
		var list []struct {
			Type                 string `json:"type"`
			WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
		}
		if json.Unmarshal(out, &list) == nil {
			for _, t := range list {
				if t.Type == "page" && t.WebSocketDebuggerURL != "" {
					wsURL = t.WebSocketDebuggerURL
				}
			}
		}
		if wsURL != "" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if wsURL == "" {
		return nil, fmt.Errorf("连不上 Chrome CDP（端口 %d）", cdpPort)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(64 << 20)
	c := &cdp{ws: conn}
	c.mustCall("Page.enable", nil)
	c.mustCall("Runtime.enable", nil)

	// 注入 Cookie，跳过登录页。Path 必须是 /，否则只对某个前缀生效。
	c.mustCall("Network.enable", nil)
	c.mustCall("Network.setCookie", map[string]any{
		"name": "omq_session", "value": cookie,
		"domain": "127.0.0.1", "path": "/",
	})
	return c, nil
}

func (c *cdp) close() { _ = c.ws.Close(websocket.StatusNormalClosure, "") }

func (c *cdp) call(method string, params any) (map[string]any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.next++
	id := c.next
	msg := map[string]any{"id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	raw, _ := json.Marshal(msg)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := c.ws.Write(ctx, websocket.MessageText, raw); err != nil {
		return nil, err
	}
	for {
		_, data, err := c.ws.Read(ctx)
		if err != nil {
			return nil, err
		}
		var resp map[string]any
		if json.Unmarshal(data, &resp) != nil {
			continue
		}
		if got, ok := resp["id"].(float64); !ok || int(got) != id {
			continue // 事件通知，跳过
		}
		return resp, nil
	}
}

func (c *cdp) mustCall(method string, params any) map[string]any {
	resp, err := c.call(method, params)
	if err != nil {
		panic(method + ": " + err.Error())
	}
	return resp
}

func (c *cdp) eval(expr string) string {
	resp := c.mustCall("Runtime.evaluate", map[string]any{
		"expression":    expr,
		"returnByValue": true,
		"awaitPromise":  true,
	})
	res, _ := resp["result"].(map[string]any)
	if res == nil {
		return ""
	}
	if ex, ok := res["exceptionDetails"]; ok {
		b, _ := json.Marshal(ex)
		return "JS_ERROR: " + string(b)
	}
	inner, _ := res["result"].(map[string]any)
	raw, _ := json.Marshal(inner["value"])
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

func (c *cdp) navigate(url string) {
	c.mustCall("Page.navigate", map[string]any{"url": url})
	deadline := time.Now().Add(6 * time.Second)
	var prev string
	for time.Now().Before(deadline) {
		time.Sleep(150 * time.Millisecond)
		dom := c.eval(`document.documentElement.outerHTML`)
		if dom == prev && len(dom) > 10000 && !strings.Contains(dom, "加载中") {
			return
		}
		prev = dom
	}
}

func (c *cdp) setViewport(width int, mobile bool) {
	c.mustCall("Emulation.setDeviceMetricsOverride", map[string]any{
		"width": width, "height": 900, "deviceScaleFactor": 1, "mobile": mobile,
	})
}

// audit 在当前页面上跑一轮布局检查，返回 JSON 数组。
//
// 检查逻辑全部在页面内跑（一次 evaluate 往返）：逐个节点算 rect
// 比反复往返快得多，而且「哪些元素互相重叠」必须能同时看到双方。
func (c *cdp) audit() string {
	return c.eval(auditJS())
}

// auditJS 是页面内执行的检查脚本。
//
// 四个刻意的判断：
//
//  1. 只检查「可见且有尺寸」的元素。display:none / 0x0 的跳过，
//     否则每个隐藏的模板节点都会报一堆问题。
//  2. 文字裁切用 scrollWidth - clientWidth > 1，并放过显式的
//     text-overflow: ellipsis（省略号是有意的，不是 bug）。
//  3. 重叠只比较「交互元素」（a / button / input / select / textarea）
//     与「有文字的块」。全量两两比较是 O(n²)，一个页面几百个节点
//     会很慢，而且大量重叠是合法的（父子、装饰层）。
//  4. 包含关系不算重叠：父元素的 rect 天然包住子元素。
func auditJS() string {
	return `(function(){
  const problems = [];
  const seen = new Set();
  function add(kind, detail) {
    const k = kind + '|' + detail;
    if (seen.has(k)) return;
    seen.add(k);
    problems.push({kind: kind, detail: detail});
  }

  function desc(el) {
    const cls = (el.className && typeof el.className === 'string')
      ? '.' + el.className.trim().split(/\s+/).slice(0, 2).join('.') : '';
    const txt = (el.textContent || '').trim().replace(/\s+/g, ' ').slice(0, 26);
    return el.tagName.toLowerCase() + cls + (txt ? ' "' + txt + '"' : '');
  }

  function visible(el) {
    const st = getComputedStyle(el);
    if (st.display === 'none' || st.visibility === 'hidden') return false;
    if (Number(st.opacity) === 0) return false;
    const r = el.getBoundingClientRect();
    return r.width > 1 && r.height > 1;
  }

  // —— 1. 文档横向溢出 ——
  const de = document.documentElement;
  if (de.scrollWidth > de.clientWidth + 1) {
    add('横向溢出', '页面出现横向滚动条（' + de.scrollWidth + ' > ' + de.clientWidth + '）');
  }

  // —— 2. 文字被裁切 ——
  // 只查「自己的文字比盒子宽」的元素，跳过有子元素的容器
  // （容器的 scrollWidth 会被子元素撑大，那种情况下面第 3 条会管）。
  const textTargets = document.querySelectorAll(
    '.cfg-help, .hint, .k, .v, .metric-k, .metric-sub, .subnav-name, .session-name, ' +
    '.session-meta, .mdl-side-name, .mdl-side-meta, .profile-name, .profile-id, ' +
    '.tag, .btn, .page-tab, .ws-tab, .msg-who, .quote-text, .bubble-text, .memory-head, ' +
    '.emoji-figcaption, .kw-cell > span, .trace-step-title, .trace-arg-val'
  );
  for (const el of textTargets) {
    if (!visible(el)) continue;
    const st = getComputedStyle(el);
    // 显式省略号是有意设计，不算问题。
    if (st.textOverflow === 'ellipsis' && st.overflow === 'hidden') continue;
    if (st.overflowX === 'hidden' || st.overflow === 'hidden') {
      if (el.scrollWidth > el.clientWidth + 1) {
        add('文字被裁切', desc(el) + ' 内容宽 ' + el.scrollWidth + ' 超出可视 ' + el.clientWidth
          + '（overflow:hidden 且没有省略号）');
      }
    }
  }

  // —— 3. 元素越出最近的可裁祖先 ——
  // clipAncestor 找最近一个「会裁切且不能滚动」的祖先。
  //
  // 两个关键判断：
  //
  // 1. overflow-x: auto/scroll 是**故意**做成横向滚动的容器
  //    （手机上的配置二级导航、模型服务商列表）。里面的子元素超出边界
  //    是设计意图 —— 而且**一旦遇到滚动容器就要停止上溯**：再往上那些
  //    祖先的 overflow:hidden 由滚动容器负责，子元素超出它们是正常的，
  //    继续上溯会把「可以横向滚动的列表」全报成有溢出问题。
  // 2. 只有 overflow:hidden 且不可滚动的祖先才算「裁切者」。
  function clipAncestor(el) {
    let p = el.parentElement;
    while (p && p !== document.body) {
      const st = getComputedStyle(p);
      if (st.overflowX === 'auto' || st.overflowX === 'scroll' ||
          st.overflowY === 'auto' || st.overflowY === 'scroll') {
        // 交给滚动容器管，不再上溯。
        return null;
      }
      if (st.overflowX === 'hidden' || st.overflowY === 'hidden' || st.overflow === 'hidden') {
        return p;
      }
      p = p.parentElement;
    }
    return null;
  }

  const all = Array.from(document.querySelectorAll('#content *')).filter(visible);

  for (const el of all) {
    const r = el.getBoundingClientRect();
    const clip = clipAncestor(el);
    if (!clip) continue;
    const cr = clip.getBoundingClientRect();
    // 允许 1px 的亚像素误差。
    if (r.right > cr.right + 1 || r.left < cr.left - 1) {
      add('超出容器', desc(el) + ' 横向超出 ' + desc(clip) +
        '（' + Math.round(r.left) + '..' + Math.round(r.right) +
        ' vs ' + Math.round(cr.left) + '..' + Math.round(cr.right) + '）');
    }
  }

  // —— 4. 交互元素重叠 ——
  const interactive = all.filter(el => {
    const t = el.tagName.toLowerCase();
    return t === 'a' || t === 'button' || t === 'input' || t === 'select' || t === 'textarea';
  });

  function contains(a, b) { return a !== b && a.contains(b); }

  for (let i = 0; i < interactive.length; i++) {
    for (let j = i + 1; j < interactive.length; j++) {
      const a = interactive[i], b = interactive[j];
      if (contains(a, b) || contains(b, a)) continue;
      const ra = a.getBoundingClientRect(), rb = b.getBoundingClientRect();
      const ox = Math.min(ra.right, rb.right) - Math.max(ra.left, rb.left);
      const oy = Math.min(ra.bottom, rb.bottom) - Math.max(ra.top, rb.top);
      // 只报「明显重叠」：2px 以上。相邻按钮各有 1px 抗锯齿差是正常的。
      if (ox > 2 && oy > 2) {
        const area = ox * oy;
        const minArea = Math.min(ra.width * ra.height, rb.width * rb.height);
        // 一个按钮被另一个盖住一大半才算问题；轻微交叠（比如
        // 浮层边缘压住一点点）不报，否则误报太多没人看。
        if (area > minArea * 0.25) {
          add('元素重叠', desc(a) + ' 与 ' + desc(b) +
            ' 重叠 ' + Math.round(ox) + 'x' + Math.round(oy) + 'px');
        }
      }
    }
  }

  // —— 5. 元素跑到视口外（左侧/上方为负） ——
  // 右侧超出交给第 1 条（横向滚动条）与第 3 条（超出容器）。
  // 这里抓的是「被负 margin / translate 推到看不见的地方」。
  for (const el of all) {
    const r = el.getBoundingClientRect();
    if (r.width < 2 || r.height < 2) continue;
    if (r.right < 0 || r.bottom < 0) {
      add('跑出视口', desc(el) + ' 位于可视区之外（right=' + Math.round(r.right) +
        ', bottom=' + Math.round(r.bottom) + '）');
    }
  }

  return JSON.stringify(problems);
})()`
}

func extractCookie(headers string) string {
	for _, line := range strings.Split(headers, "\n") {
		if !strings.HasPrefix(strings.ToLower(line), "set-cookie:") {
			continue
		}
		v := strings.TrimSpace(line[len("set-cookie:"):])
		if i := strings.Index(v, "="); i > 0 {
			name := v[:i]
			rest := v[i+1:]
			if j := strings.Index(rest, ";"); j >= 0 {
				rest = rest[:j]
			}
			if name == "omq_session" {
				return rest
			}
		}
	}
	return ""
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
