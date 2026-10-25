package web

// 服务器层测试：静态资源、SSE 日志流、日志广播的非阻塞语义。
//
// 全部基于 httptest.NewServer，不调用 Server.Start，因此不占用真实端口。

import (
	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/logx"
)

// TestStaticIndexReturnsHTML 防：embed 路径写错或 FileServer 被中间件挡住，
// 导致打开 WebUI 是 404 / 空白。
func TestStaticIndexReturnsHTML(t *testing.T) {
	env := newTestEnv(t, envOptions{})

	for _, path := range []string{"/", "/index.html"} {
		resp, body := env.do(http.MethodGet, path, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s 状态码 = %d，期望 200，body=%s", path, resp.StatusCode, body)
		}
		ct := resp.Header.Get("Content-Type")
		if !strings.Contains(ct, "text/html") {
			t.Errorf("GET %s Content-Type = %q，期望 text/html", path, ct)
		}
		if !strings.Contains(strings.ToLower(string(body)), "<!doctype html") {
			t.Errorf("GET %s 响应不像 HTML 页面: %.120s", path, body)
		}
	}
}

// TestStaticAssetsServed 防：前端依赖的 js/css 没被 embed 进去，页面裸奔。
func TestStaticAssetsServed(t *testing.T) {
	env := newTestEnv(t, envOptions{})

	cases := []struct {
		Path        string
		ContentType string
	}{
		{Path: "/app.js", ContentType: "javascript"},
		{Path: "/style.css", ContentType: "text/css"},
	}
	for _, c := range cases {
		resp, body := env.do(http.MethodGet, c.Path, "")
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s 状态码 = %d，期望 200", c.Path, resp.StatusCode)
			continue
		}
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, c.ContentType) {
			t.Errorf("GET %s Content-Type = %q，期望包含 %q", c.Path, ct, c.ContentType)
		}
		if len(body) == 0 {
			t.Errorf("GET %s 响应体为空", c.Path)
		}
	}
}

// TestStylesheetHonoursHiddenAttribute 防：「登录成功但界面不切换」。
//
// index.html 靠 hidden 属性切换登录页与主界面。浏览器默认的
// [hidden] { display: none } 只是一个普通声明，样式表里 .app { display: grid }
// 与 .login-wrap { display: grid } 同优先级又写在后面，会把它覆盖掉 ——
// 于是 hidden=true 的元素依然显示，登录后主界面和登录卡片重叠在一起。
//
// 这个 bug 纯单测很难发现（没有任何 Go 代码出错），只能靠检查 CSS。
func TestStylesheetHonoursHiddenAttribute(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	_, body := env.do(http.MethodGet, "/style.css", "")
	css := string(body)

	if !strings.Contains(css, "[hidden]") {
		t.Fatal("style.css 缺少 [hidden] 规则，登录后界面不会切换")
	}

	// 找到 [hidden] 规则体（行首出现才算规则，注释里提到的那个不算），
	// 必须带 !important，否则压不过 .app / .login-wrap。
	i := strings.Index(css, "\n[hidden]")
	if i < 0 {
		t.Fatal("style.css 里找不到行首的 [hidden] 规则")
	}
	i++
	end := strings.Index(css[i:], "}")
	if end < 0 {
		t.Fatal("style.css 的 [hidden] 规则没有闭合")
	}
	rule := css[i : i+end]
	if !strings.Contains(rule, "display: none") {
		t.Errorf("[hidden] 规则里没有 display: none: %q", rule)
	}
	if !strings.Contains(rule, "!important") {
		t.Errorf("[hidden] 规则缺少 !important，会被 .app / .login-wrap 的 display 覆盖: %q", rule)
	}
}

// TestLoginAndAppUseHiddenAttribute 防：index.html 忘了给隐藏区加 hidden，
// 或者登录卡片与主界面同时可见。
func TestLoginAndAppUseHiddenAttribute(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	_, body := env.do(http.MethodGet, "/", "")
	html := string(body)

	if !strings.Contains(html, `id="app" class="app" hidden`) {
		t.Errorf("index.html 的 #app 初始应为 hidden，实际片段: %s", excerpt(html, "id=\"app\""))
	}
	if !strings.Contains(html, `id="login"`) {
		t.Fatal("index.html 缺少 #login 容器")
	}
}

// excerpt 截取包含 needle 的一小段文本，便于报错时定位。
func excerpt(s, needle string) string {
	i := strings.Index(s, needle)
	if i < 0 {
		return "<not found>"
	}
	end := i + 80
	if end > len(s) {
		end = len(s)
	}
	return s[i:end]
}

// TestStaticUnknownPathDoesNotLeakAPI 防：静态兜底把未知路径改写成 200，
// 或者把 /api 下不存在的端点吞进静态处理器（应返回 404）。
func TestStaticUnknownPathDoesNotLeakAPI(t *testing.T) {
	env := newTestEnv(t, envOptions{})

	resp, _ := env.do(http.MethodGet, "/not-exist-file.txt", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /not-exist-file.txt = %d，期望 404", resp.StatusCode)
	}

	// 未注册的 /api/* 不应落到静态文件服务器（这里应 404，而不是 HTML）。
	resp, body := env.do(http.MethodGet, "/api/does-not-exist", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /api/does-not-exist = %d，期望 404，body=%s", resp.StatusCode, body)
	}
}

// TestBroadcastNoClientsDoesNotPanic 防：Broadcast 在没有任何客户端时
// 对 nil/空 map 操作或阻塞，把日志写入线程拖死。
func TestBroadcastNoClientsDoesNotPanic(t *testing.T) {
	s := NewServer(Options{Config: config.Wrap(testConfig()), Store: nil})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			s.Broadcast(logx.LogLine{Level: "INFO", Logger: "test", Message: "hi"})
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("无客户端时 Broadcast 阻塞")
	}

	// 客户端集合必须保持为空，不能凭空产生条目。
	s.mu.Lock()
	n := len(s.clients)
	s.mu.Unlock()
	if n != 0 {
		t.Errorf("无客户端时 clients 长度 = %d，期望 0", n)
	}
}

// TestBroadcastNonBlockingOnSlowClient 防：慢客户端（缓冲区满）阻塞日志广播。
// 关键设计：满了就丢，绝不阻塞。
func TestBroadcastNonBlockingOnSlowClient(t *testing.T) {
	s := NewServer(Options{Config: config.Wrap(testConfig()), Store: nil})

	ch := make(chan logx.LogLine, 1)
	s.mu.Lock()
	s.clients[ch] = struct{}{}
	s.mu.Unlock()

	// 第一条能进缓冲区。
	s.Broadcast(logx.LogLine{Message: "first"})

	// 后续必须立刻返回，而不是等待读端。
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			s.Broadcast(logx.LogLine{Message: "overflow"})
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("缓冲区满时 Broadcast 阻塞了")
	}

	// 缓冲区里保留第一条，其余被丢弃。
	if got := <-ch; got.Message != "first" {
		t.Errorf("缓冲区内容 = %q，期望 first", got.Message)
	}
	select {
	case extra := <-ch:
		t.Errorf("缓冲区出现额外日志 %q，期望已丢弃", extra.Message)
	default:
	}
}

// TestBroadcastReachesMultipleClients 防：广播只推给第一个客户端。
func TestBroadcastReachesMultipleClients(t *testing.T) {
	s := NewServer(Options{Config: config.Wrap(testConfig()), Store: nil})

	chans := make([]chan logx.LogLine, 3)
	s.mu.Lock()
	for i := range chans {
		chans[i] = make(chan logx.LogLine, 4)
		s.clients[chans[i]] = struct{}{}
	}
	s.mu.Unlock()

	line := logx.LogLine{Level: "WARN", Logger: "x", Message: "broadcast"}
	s.Broadcast(line)

	for i, ch := range chans {
		select {
		case got := <-ch:
			if got.Message != line.Message || got.Level != line.Level {
				t.Errorf("客户端 %d 收到 %+v，期望 %+v", i, got, line)
			}
		default:
			t.Errorf("客户端 %d 没有收到广播", i)
		}
	}
}

// TestLogsStreamDeliversBroadcast 防：SSE 端点连不上、收到日志不推送。
//
// 两个关键实现细节决定了这个用例必须并发发起广播：
//  1. Go 的 http.Server 在首次 Flush 前不会真正把响应头写给客户端，
//     而 handler 只在有日志/心跳时才 Flush —— 所以 client.Do 会阻塞，
//     必须放到 goroutine 里；
//  2. 客户端注册发生在 WriteHeader 之后，需要轮询 s.clients 确认已注册。
//
// 全程用 context 超时 + 显式 cancel 收尾，不会挂住。
func TestLogsStreamDeliversBroadcast(t *testing.T) {
	env := newTestEnv(t, envOptions{})
	token := env.loginOK()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, env.ts.URL+"/api/logs", nil)
	if err != nil {
		t.Fatalf("构造 SSE 请求失败: %v", err)
	}
	req.AddCookie(&http.Cookie{Name: cookieName, Value: token})

	// Do 会一直阻塞到服务端首次 Flush，因此必须异步。
	type sseResult struct {
		resp *http.Response
		err  error
	}
	resultCh := make(chan sseResult, 1)
	go func() {
		resp, err := (&http.Client{}).Do(req)
		resultCh <- sseResult{resp: resp, err: err}
	}()

	// 等 handler 把客户端注册进广播表（最多 5 秒）。
	registerDeadline := time.Now().Add(5 * time.Second)
	for {
		env.s.mu.Lock()
		n := len(env.s.clients)
		env.s.mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(registerDeadline) {
			t.Fatal("SSE 连接建立后未注册到广播表")
		}
		time.Sleep(5 * time.Millisecond)
	}

	want := "sse-test-line"
	env.s.Broadcast(logx.LogLine{
		Time:    time.Now(),
		Level:   "INFO",
		Logger:  "sse-test",
		Message: want,
	})

	var resp *http.Response
	select {
	case r := <-resultCh:
		if r.err != nil {
			t.Fatalf("连接 SSE 失败: %v", r.err)
		}
		resp = r.resp
	case <-time.After(5 * time.Second):
		t.Fatal("连接 SSE 超时")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("SSE 状态码 = %d，期望 200，body=%s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("SSE Content-Type = %q，期望 text/event-stream", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Errorf("SSE Cache-Control = %q，期望包含 no-cache", cc)
	}
	if xab := resp.Header.Get("X-Accel-Buffering"); xab != "no" {
		t.Errorf("SSE X-Accel-Buffering = %q，期望 no", xab)
	}

	// 从流里读首条 data 行。
	lines := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data: ") {
				lines <- line
				return
			}
		}
		close(lines)
	}()

	select {
	case line, ok := <-lines:
		if !ok {
			t.Fatal("SSE 流在收到广播前就关闭了")
		}
		if !strings.Contains(line, want) {
			t.Errorf("SSE 数据行 = %q，期望包含 %q", line, want)
		}
		if !strings.Contains(line, `"level":"INFO"`) {
			t.Errorf("SSE 数据行缺少 level 字段: %q", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("等待 SSE 首条日志超时")
	}

	// 取消请求后，客户端必须从广播表移除（否则内存泄漏）。
	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for {
		env.s.mu.Lock()
		n := len(env.s.clients)
		env.s.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("SSE 断开后 clients 仍有 %d 个条目，泄漏", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestLogsStreamRequiresAuth 防：SSE 绕过认证，匿名可窃听全部日志。
func TestLogsStreamRequiresAuth(t *testing.T) {
	env := newTestEnv(t, envOptions{})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, env.ts.URL+"/api/logs", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("匿名 SSE 状态码 = %d，期望 401", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "未登录") {
		t.Errorf("匿名 SSE 响应 = %s，期望包含 未登录", body)
	}
}

// TestClientIPAndTrimPrefix 防：这两个被多处复用的小工具改写后
// 日志里的 IP 变成 host:port、或者路由前缀处理错位。
func TestClientIPAndTrimPrefix(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)

	req.RemoteAddr = "192.0.2.10:5678"
	if got := clientIP(req); got != "192.0.2.10" {
		t.Errorf("clientIP(%q) = %q，期望 192.0.2.10", req.RemoteAddr, got)
	}

	// 没有端口的 RemoteAddr 原样返回，不能变成空串。
	req.RemoteAddr = "192.0.2.11"
	if got := clientIP(req); got != "192.0.2.11" {
		t.Errorf("clientIP(%q) = %q，期望原样返回", req.RemoteAddr, got)
	}

	// 实际语义是剥掉 "/api/" 前缀（不是原样返回）。
	cases := map[string]string{
		"/api/status": "status",
		"/api/usage":  "usage",
		"status":      "status",
		"":            "",
	}
	for in, want := range cases {
		if got := TrimPrefix(in); got != want {
			t.Errorf("TrimPrefix(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// TestLogsStreamWithoutFlusherReturns500 防：handler 拿不到 Flusher 时
// 不报错而是「假装建立连接」，前端永远收不到日志。
func TestLogsStreamWithoutFlusherReturns500(t *testing.T) {
	s := NewServer(Options{Config: config.Wrap(testConfig()), Store: nil})

	// httptest.ResponseRecorder 本身实现了 http.Flusher，
	// 所以必须包一层把 Flush 屏蔽掉，否则 handler 会真的进 SSE 循环。
	rec := httptest.NewRecorder()
	s.handleLogsStream(noFlusherWriter{rec}, httptest.NewRequest(http.MethodGet, "/api/logs", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("无 Flusher 状态码 = %d，期望 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "不支持流式响应") {
		t.Errorf("无 Flusher 响应体 = %q，期望包含错误说明", rec.Body.String())
	}

	s.mu.Lock()
	n := len(s.clients)
	s.mu.Unlock()
	if n != 0 {
		t.Errorf("响应失败后 clients 长度 = %d，期望 0", n)
	}
}

// TestNewServerWithEmptySecretGeneratesOne 防：SessionSecret 留空时
// authManager 不生成随机密钥（secret 为空字节串会让签名退化）。
func TestNewServerWithEmptySecretGeneratesOne(t *testing.T) {
	cfg := testConfig()
	cfg.Web.SessionSecret = ""

	s := NewServer(Options{Config: config.Wrap(cfg), Store: nil})

	if len(s.auth.secret) == 0 {
		t.Fatal("SessionSecret 留空时未生成随机密钥")
	}

	// 能够正常签发与校验。
	token := s.auth.Issue()
	if !s.auth.Verify(token) {
		t.Fatalf("自动生成密钥后签发/校验不一致: %q", token)
	}

	// 两个实例各自生成不同密钥，互相不能验证（避免硬编码常量式退化）。
	s2 := NewServer(Options{Config: config.Wrap(cfg), Store: nil})
	if string(s.auth.secret) == string(s2.auth.secret) {
		t.Error("两个实例生成了相同的随机密钥")
	}
	if s2.auth.Verify(token) {
		t.Error("另一个实例验证了不属于它的 token")
	}
}

// TestStartStopLifecycle 覆盖 Start / Stop 的真实生命周期。
//
// 用 Port = 0 让内核分配临时端口，不影响其他进程；
// 用例结束立即 Stop，并验证客户端通道被关闭、广播表被清空。
func TestStartStopLifecycle(t *testing.T) {
	cfg := testConfig()
	cfg.Web.Port = 0 // 内核分配临时端口，不占用固定端口。

	s := NewServer(Options{Config: config.Wrap(cfg), Store: nil})
	if err := s.Start(); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}

	// 模拟一个已连接的 SSE 客户端。
	ch := make(chan logx.LogLine, 1)
	s.mu.Lock()
	s.clients[ch] = struct{}{}
	s.mu.Unlock()

	s.Stop()

	// Stop 只清空注册表，不关通道 —— 通道的所有者是 handleLogsStream，
	// 它自己 defer close。两边都 close 会 panic: close of closed channel。
	//
	// 这里断言两点：
	//   1. 注册表被清空（否则 Broadcast 会往已断开的连接投递）
	//   2. 通道没有被 Stop 关掉（关了就是那个 panic 的成因）
	s.mu.Lock()
	n := len(s.clients)
	s.mu.Unlock()
	if n != 0 {
		t.Errorf("Stop 后 clients 长度 = %d，期望 0", n)
	}

	select {
	case _, ok := <-ch:
		if !ok {
			t.Error("Stop 不应关闭客户端通道（应由 handler 独占所有权）")
		}
	default:
		// 没有数据也没有被关闭，符合预期。
	}

	// 不存在的端口 / 非法地址应当返回错误而不是 panic。
	badCfg := testConfig()
	badCfg.Web.Host = "256.256.256.256"
	if err := NewServer(Options{Config: config.Wrap(badCfg), Store: nil}).Start(); err == nil {
		t.Error("非法监听地址时 Start 未返回错误")
	}
}

// TestSSEChannelOwnedByHandler 防：Stop 与 handler 重复 close 同一通道。
//
// 原来 Stop() 会 close 所有客户端通道，而 handleLogsStream 的 defer 又
// 对同一个通道再 close 一次，结果是每个在线 SSE 连接在关闭时都 panic：
// "close of closed channel"（net/http 会 recover，进程不崩，但日志里刷堆栈）。
//
// 现在的约定：通道的所有者是 handleLogsStream，它自己 defer close；
// Stop 只清空注册表，只让 http.Shutdown 断开连接、由 handler 自行收尾。
func TestSSEChannelOwnedByHandler(t *testing.T) {
	s := NewServer(Options{Config: config.Wrap(testConfig()), Store: nil})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/api/logs", nil).WithContext(ctx)
	w := httptest.NewRecorder()

	// ResponseRecorder 实现了 Flusher，因此会进 SSE 循环。
	handlerDone := make(chan any, 1)
	go func() {
		defer func() { handlerDone <- recover() }()
		s.handleLogsStream(w, req)
	}()

	// 等 handler 注册客户端。
	deadline := time.Now().Add(3 * time.Second)
	for {
		s.mu.Lock()
		n := len(s.clients)
		s.mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("SSE handler 未注册客户端")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Stop 只清空注册表，不关通道。
	s.Stop()

	s.mu.Lock()
	n := len(s.clients)
	s.mu.Unlock()
	if n != 0 {
		t.Errorf("Stop 后 clients 长度 = %d，期望 0", n)
	}

	// 取消请求 context，handler 应自行退出且不 panic。
	cancel()

	select {
	case recovered := <-handlerDone:
		if recovered != nil {
			t.Errorf("SSE handler 退出时 panic: %v（通道所有权应只属于 handler）", recovered)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("取消 context 后 SSE handler 未退出")
	}
}

// TestStopIsIdempotentWithoutClients 防：Stop 在无客户端时 panic
// （对空 map / 未启动的 http.Server 操作）。
func TestStopIsIdempotentWithoutClients(t *testing.T) {
	s := NewServer(Options{Config: config.Wrap(testConfig()), Store: nil})

	// 不调用 Start：http.Server 从未监听，Shutdown 应当安全返回。
	s.Stop()
	s.Stop()

	s.mu.Lock()
	n := len(s.clients)
	s.mu.Unlock()
	if n != 0 {
		t.Errorf("Stop 后 clients 长度 = %d，期望 0", n)
	}
}

// noFlusherWriter 只暴露 http.ResponseWriter，隐藏底层可能存在的 Flush 方法。
type noFlusherWriter struct {
	http.ResponseWriter
}

// TestStaticServedWithNoCache 防：重启后浏览器仍用旧的 app.js / style.css。
//
// 资源被 embed 进二进制，二进制重启后内容变了但 URL 没变，
// 浏览器会一直拿缓存里的旧版本。真实现象：改完消息渲染重新编译，
// 界面还是老样子，连手动刷新都不一定好使。
func TestStaticServedWithNoCache(t *testing.T) {
	env := newTestEnv(t, envOptions{})

	for _, path := range []string{"/app.js", "/style.css", "/"} {
		resp, _ := env.do(http.MethodGet, path, "")
		got := resp.Header.Get("Cache-Control")
		if !strings.Contains(got, "no-cache") {
			t.Errorf("%s 的 Cache-Control = %q，期望含 no-cache", path, got)
		}
	}
}
