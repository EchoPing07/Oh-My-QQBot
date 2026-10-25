// Package web 提供 WebUI 的 HTTP API 与前端静态资源。
//
// 与 MaiBot 的 webui 完全不同：这里是从零设计的精简版，
// 只保留「能看见机器人在干什么、能改配置、能以人类身份对话」三件事。
package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/llm"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/logx"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/prompt"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/store"
)

//go:embed static/*
var staticFS embed.FS

// Hub 是 WebUI 与会话中心交互的能力。
type Hub interface {
	ActiveCount() int
	// Dispatch 把一条入站消息投进对应会话。
	// 网页对话窗口（/api/chat/send）用它把网页用户的话交给机器人。
	Dispatch(msg *domain.Message, scene, targetID, accountID string)
}

// Options 是创建服务器的参数。
type Options struct {
	Config  *config.Store
	Store   *store.DB
	Hub     Hub
	Prompts *prompt.Manager
	// LLM 用于「测试模型 / 测试服务商」接口。
	//
	// 允许为 nil（测试里常常不建客户端），此时那两个接口返回 503。
	LLM *llm.Client
	// Restart 让 WebUI 能重启整程序（端口、日志级别等改动需要它）。
	// 允许为 nil，此时接口返回 503 并说明不支持。
	Restart Restarter
}

// Server 是 WebUI 服务器。
type Server struct {
	cfg    *config.Store
	store  *store.DB
	hub    Hub
	// prompts 用于提示词管理页的读写。nil 表示未提供（工具与测试场景）。
	prompts *prompt.Manager
	// llm 用于模型探活。nil 表示未提供（测试场景），此时探活接口返回 503。
	llm *llm.Client
	// restart 用于「重启程序」。nil 表示不支持（测试场景）。
	restart Restarter
	// listener 是当前监听器，重启前要先关掉它释放端口。
	listener net.Listener

	http *http.Server
	log  *logx.Logger

	// 日志广播。
	mu      sync.Mutex
	clients map[chan logx.LogLine]struct{}
	// 消息事件广播（/api/events）。与 clients 共用 mu：
	// 同为「给前端的临时状态」，注释见上。
	eventClients map[chan string]struct{}
	auth         *authManager

	// 配置改动后「哪些项要重启才生效」的提示。
	// 与 clients 共用 mu：两者都是「给前端的临时状态」，
	// 单独再加一把锁不值得。
	restartFields []string
	restartAt     time.Time
}

// NewServer 创建服务器。
func NewServer(opts Options) *Server {
	s := &Server{
		cfg:     opts.Config,
		store:   opts.Store,
		hub:     opts.Hub,
		prompts: opts.Prompts,
		llm:     opts.LLM,
		restart: opts.Restart,
		log:         logx.Get("web"),
		clients:     make(map[chan logx.LogLine]struct{}),
		eventClients: make(map[chan string]struct{}),
		auth:        newAuthManager(opts.Config.Get().Web.Password, opts.Config.Get().Web.SessionSecret),
	}
	s.http = &http.Server{
		Addr:              net.JoinHostPort(opts.Config.Get().Web.Host, fmt.Sprint(opts.Config.Get().Web.Port)),
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s
}

// Start 启动服务器（非阻塞）。
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.http.Addr)
	if err != nil {
		return fmt.Errorf("监听 %s 失败: %w", s.http.Addr, err)
	}

	// 存下来：重启时要在起新进程之前先把它关掉，
	// 否则新进程 bind 同一个端口必然失败。
	s.mu.Lock()
	s.listener = ln
	s.mu.Unlock()

	s.log.Info("WebUI 已启动", "url", "http://"+s.http.Addr)

	go func() {
		if err := s.http.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.log.Warn("WebUI 服务退出", "err", err)
		}
	}()
	return nil
}

// ReleaseListener 只关闭监听端口，不关闭已有连接、不停服务。
//
// 重启流程专用：新进程要 bind 同一个端口，旧进程必须先放手。
// 与 Stop 的区别是它不等连接结束、不清理注册表 ——
// 那些事留给随后的 Stop。
//
// 幂等：重复调用不会报错（重启路径上可能被调多次）。
func (s *Server) ReleaseListener() {
	s.mu.Lock()
	ln := s.listener
	s.listener = nil
	s.mu.Unlock()
	if ln != nil {
		if err := ln.Close(); err != nil {
			s.log.Warn("释放 WebUI 端口失败", "err", err)
		}
	}
}

// Stop 关闭服务器。
func (s *Server) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := s.http.Shutdown(ctx); err != nil {
		s.log.Warn("WebUI 关闭超时", "err", err)
	}

	// 这里不 close 通道，只把注册表清空。
	//
	// 通道的所有者是各自的 SSE handler（handleLogsStream /
	// handleEventsStream，它们自己 defer close），
	// 由 Stop 再 close 一次会 panic: close of closed channel。
	// net/http 会 recover 所以进程不崩，但每个在线 SSE 连接
	// 在服务关闭时都会往日志里刷一段 panic 堆栈。
	//
	// 连接本身由 http.Shutdown 断开，客户端 context 随之取消，
	// handler 会正常返回并自己关通道。
	s.mu.Lock()
	s.clients = make(map[chan logx.LogLine]struct{})
	s.eventClients = make(map[chan string]struct{})
	s.mu.Unlock()
}

// Broadcast 实现 logx.Broadcaster，把日志推给所有 WebSocket 客户端。
//
// 关键设计：非阻塞投递。慢客户端不该拖住日志写入。
func (s *Server) Broadcast(line logx.LogLine) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for ch := range s.clients {
		select {
		case ch <- line:
		default:
			// 客户端处理不过来，丢掉这条日志而不是阻塞。
		}
	}
}

// routes 注册路由。
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// 静态资源。
	//
	// 外面包一层 noCacheStaticSheet 而不是裸 FileServer：
	// 资源被 embed 进二进制，二进制重启后内容变了，但 URL 没变，
	// 浏览器就会拿缓存里的旧 app.js / style.css，表现是
	// 「改了代码重启，界面还是老样子」——而且刷新也不一定有用。
	// 这个坑真实踩过：消息气泡改完重新编译，浏览器仍跑旧渲染。
	sub, err := fs.Sub(staticFS, "static")
	if err == nil {
		mux.Handle("/", noCacheStatic(http.FileServer(http.FS(sub))))
	}

	// 认证。
	mux.HandleFunc("/api/login", s.handleLogin)
	mux.HandleFunc("/api/logout", s.handleLogout)

	// 需要登录的接口。
	mux.Handle("/api/status", s.requireAuth(s.handleStatus))
	mux.Handle("/api/overview", s.requireAuth(s.handleOverview))
	mux.Handle("/api/sessions", s.requireAuth(s.handleSessions))
	mux.Handle("/api/messages", s.requireAuth(s.handleMessages))
	// 网页对话窗口：以人类身份与机器人私聊（对应 MaiBot 的 /chat）。
	// 与旧 /api/send（以机器人身份往 QQ 会话发话，已移除）是两件事。
	mux.Handle("/api/chat/session", s.requireAuth(s.handleChatSession))
	mux.Handle("/api/chat/send", s.requireAuth(s.handleChatSend))
	mux.Handle("/api/config", s.requireAuth(s.handleConfig))
	// 配置编辑：GET 拿字段清单+当前值，POST 提交补丁。
	// 与 /api/config（只读摘要，给概览页用）分开：
	// 那个接口是「看一眼机器人在用什么配置」，这个是「改配置」，
	// 语义不同、权限面也不同，混在一个 handler 里迟早出问题。
	mux.Handle("/api/config/edit", s.requireAuth(s.handleConfigFields))
	mux.Handle("/api/config/save", s.requireAuth(s.handleConfigUpdate))

	// 模型管理与通用配置分开：LLM 是三层嵌套 + 列表增删，
	// 点分键平铺表单表达不了（见 internal/config/llm_edit.go）。
	mux.Handle("/api/config/llm", s.requireAuth(s.handleLLMGet))
	mux.Handle("/api/config/llm/save", s.requireAuth(s.handleLLMSave))
	// 模型探活：填错模型配置时唯一的自查手段。
	mux.Handle("/api/config/llm/probe", s.requireAuth(s.handleModelProbe))
	mux.Handle("/api/config/llm/probe-provider", s.requireAuth(s.handleProviderProbe))
	// 重启程序：端口、日志级别等改动只能靠它生效。
	mux.Handle("/api/restart", s.requireAuth(s.handleRestart))

	// QQ 凭据单独一组：AppSecret 不能进平铺清单（那里的 Snapshot
	// 会把值返回给前端），而且需要「留空=不改」的语义。
	mux.Handle("/api/config/qq", s.requireAuth(s.handleQQGet))
	mux.Handle("/api/config/qq/save", s.requireAuth(s.handleQQSave))

	// 关键词反应规则：一张表，提交整表。
	// 与 /api/config/save 分开是因为列表类型无法用点分键表达。
	mux.Handle("/api/config/keywords", s.requireAuth(s.handleKeywordsGet))
	mux.Handle("/api/config/keywords/save", s.requireAuth(s.handleKeywordsSave))
	mux.Handle("/api/usage", s.requireAuth(s.handleUsage))

	// 长期记忆 / 表情包 / 表达三块新功能。
	mux.Handle("/api/memory", s.requireAuth(s.handleMemory))
	mux.Handle("/api/memory/delete", s.requireAuth(s.handleMemoryDelete))
	mux.Handle("/api/profiles", s.requireAuth(s.handleProfiles))
	mux.Handle("/api/emojis", s.requireAuth(s.handleEmojis))
	mux.Handle("/api/expressions", s.requireAuth(s.handleExpressions))
	mux.Handle("/api/jargons", s.requireAuth(s.handleJargons))
	mux.Handle("/api/emoji/image", s.requireAuth(s.handleEmojiImage))
	// 聊天记录里的图片/表情回查。与 /api/emoji/image 读的是同一批文件，
	// 但语义不同：那个是「表情包库管理页看抽图」，这个是
	// 「会话里某条消息的那个图」。分开是为了以后能各自做缓存策略。
	mux.Handle("/api/media", s.requireAuth(s.handleMedia))

	// 删除类接口。
	//
	// 全部限定 DELETE 方法：只认 GET 的话，浏览器预取、爬虫扫到链接、
	// 甚至一张 <img src> 都能触发删除（AGENTS.md 第 64 条）。
	//
	// 与参考实现的对应：
	//   DELETE /api/chat/sessions/{id}  → DELETE /api/sessions?id=
	//   DELETE /api/chat/history        → DELETE /api/messages
	//   DELETE /api/expression/{id}     → DELETE /api/expressions?id=
	//   DELETE /api/jargon/{id}         → DELETE /api/jargons?id=
	//   DELETE /api/emoji/{id}          → DELETE /api/emojis?id=
	//   DELETE /api/person/{id}         → DELETE /api/profiles
	mux.Handle("/api/sessions/delete", s.requireAuth(s.handleSessionDelete))
	// 重命名不是删除，但同属「会话级写操作」。用 PATCH 而不是 POST：
	// 它是幂等的局部更新，与方法语义对得上。
	mux.Handle("/api/sessions/rename", s.requireAuth(s.handleSessionRename))
	mux.Handle("/api/messages/delete", s.requireAuth(s.handleMessagesDelete))
	mux.Handle("/api/expressions/delete", s.requireAuth(s.handleExpressionDelete))
	mux.Handle("/api/jargons/delete", s.requireAuth(s.handleJargonDelete))
	mux.Handle("/api/emojis/delete", s.requireAuth(s.handleEmojiDelete))
	mux.Handle("/api/profiles/delete", s.requireAuth(s.handleProfileDelete))

	// Prompt 管理（对应参考实现 /api/config/prompts/*）。
	// 一个路由四种方法，而不是四个路由：提示词就是「一组具名文本文件」。
	mux.Handle("/api/prompts", s.requireAuth(s.handlePrompts))

	// 数据管理（对应参考实现 /api/data-transfer）。
	// GET 导出、POST 导入。导出的是数据库快照，不含表情包图片文件。
	mux.Handle("/api/backup", s.requireAuth(s.handleBackup))

	mux.Handle("/api/logs", s.requireAuth(s.handleLogsStream))
	// 消息事件流：消息落库即推送，前端据此实时拉增量。
	mux.Handle("/api/events", s.requireAuth(s.handleEventsStream))

	return mux
}

// noCacheStatic 给静态资源加「每次都要重新验证」的缓存策略。
//
// 用 no-cache 而不是 no-store：no-cache 允许浏览器存副本，
// 但每次使用前必须向服务器验证（这里没有 ETag，所以效果就是重新取）。
// 选它的原因是很小的体积换取「重启即生效」，而开发期最常见的困惑
// 就是「为什么我改的没生效」。
func noCacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		next.ServeHTTP(w, r)
	})
}

// requireAuth 是认证中间件。
func (s *Server) requireAuth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.auth.verify(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "未登录"})
			return
		}
		next(w, r)
	})
}

// writeJSON 写 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// readJSON 读 JSON 请求体。
func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

// clientIP 取客户端地址。
func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// TrimPrefix 用于路由里去掉 /api 前缀。
func TrimPrefix(p string) string { return strings.TrimPrefix(p, "/api/") }
