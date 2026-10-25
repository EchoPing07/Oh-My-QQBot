package qq

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/logx"
)

// 网关 opcode，与 QQ 官方文档一致。
const (
	opDispatch       = 0  // 事件分发
	opHeartbeat      = 1  // 客户端心跳
	opIdentify       = 2  // 首次鉴权
	opResume         = 6  // 恢复会话
	opReconnect      = 7  // 服务端要求重连
	opInvalidSession = 9  // 会话无效，需重新 identify
	opHello          = 10 // 服务端下发心跳间隔
	opHeartbeatACK   = 11 // 心跳确认
)

// 连接状态事件。
const (
	eventReady   = "READY"
	eventResumed = "RESUMED"
)

// GatewayEvent 是网关推送的一条消息事件。
type GatewayEvent struct {
	// Type 是事件类型，例如 GROUP_AT_MESSAGE_CREATE。
	Type string
	// Data 是原始 payload 的 d 字段。
	Data json.RawMessage
	// Sequence 是事件序号，用于 resume。
	Sequence int64
}

// Gateway 是 QQ WebSocket 网关连接。
// 对应插件里的 _run / _connect / _handle_gateway_payload / _heartbeat。
type Gateway struct {
	client  *Client
	log     *logx.Logger
	onEvent func(GatewayEvent)

	// intents 是订阅的事件位图。
	//
	// 以前这里硬编码 1<<25（GROUP_AND_C2C_EVENT），配置里的
	// qq.intents 完全没人读 —— 改配置不生效，是最坑的那种陷阱。
	// 现在从配置注入，0 表示用默认值。
	intents int

	mu        sync.Mutex
	conn      *websocket.Conn
	sessionID string
	seq       int64
	botID     string
	botName   string
	ready     bool

	hbCancel context.CancelFunc
	wg       sync.WaitGroup
}

// DefaultIntents 是 QQ 官方机器人订阅群与私聊消息所需的事件位。
//
// 1<<25 = GROUP_AND_C2C_EVENT，覆盖群 @ 消息与私聊消息。
// 申请了全量群消息权限后这个位仍然是对的 —— 权限影响的是能收到
// 哪些消息，不改变事件分类的位定义。
const DefaultIntents = 1 << 25

// NewGateway 创建网关。
//
// intents <= 0 时用 DefaultIntents。
func NewGateway(client *Client, intents int, onEvent func(GatewayEvent)) *Gateway {
	if intents <= 0 {
		intents = DefaultIntents
	}
	return &Gateway{
		client:  client,
		log:     logx.Get("qq.gateway"),
		onEvent: onEvent,
		intents: intents,
	}
}

// BotID 返回机器人自己的 openid，由 READY 事件回填。
func (g *Gateway) BotID() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.botID
}

// BotName 返回机器人自己的名字。
func (g *Gateway) BotName() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.botName
}

// Ready 返回连接是否已就绪。
func (g *Gateway) Ready() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.ready
}

// Run 持续运行网关，直到 ctx 取消。
// 对应插件 _run：断线后按 reconnectDelaySec 重试。
func (g *Gateway) Run(ctx context.Context, reconnectDelay time.Duration) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		err := g.connect(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			g.log.Warn("QQ 网关连接失败，将重试", "err", err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(reconnectDelay):
		}
	}
}

// connect 建立一次连接并处理消息，直到连接断开。
func (g *Gateway) connect(ctx context.Context) error {
	url, err := g.client.GatewayURL(ctx)
	if err != nil {
		return err
	}

	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		// QQ 网关单帧可能较大（含图片消息），放宽读取上限。
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return fmt.Errorf("连接 QQ 网关失败: %w", err)
	}
	conn.SetReadLimit(8 * 1024 * 1024)

	g.mu.Lock()
	g.conn = conn
	g.mu.Unlock()

	// 连接断开时释放心跳。
	defer func() {
		g.mu.Lock()
		if g.hbCancel != nil {
			g.hbCancel()
			g.hbCancel = nil
		}
		g.conn = nil
		g.ready = false
		g.mu.Unlock()
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		readCtx, cancel := context.WithCancel(ctx)
		_, data, err := conn.Read(readCtx)
		cancel()

		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("读取网关消息失败: %w", err)
		}

		if err := g.handlePayload(ctx, data); err != nil {
			g.log.Warn("处理网关消息失败", "err", err)
		}
	}
}

// gatewayPayload 是网关帧结构。
type gatewayPayload struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
	S  int64           `json:"s"`
	T  string          `json:"t"`
}

// handlePayload 处理一条网关帧。
// 对应插件 _handle_gateway_payload。
func (g *Gateway) handlePayload(ctx context.Context, data []byte) error {
	var p gatewayPayload
	if err := json.Unmarshal(data, &p); err != nil {
		return fmt.Errorf("解析网关帧失败: %w", err)
	}

	switch p.Op {
	case opHello:
		var hello struct {
			HeartbeatInterval float64 `json:"heartbeat_interval"`
		}
		if err := json.Unmarshal(p.D, &hello); err != nil {
			return fmt.Errorf("解析 Hello 失败: %w", err)
		}
		if hello.HeartbeatInterval <= 0 {
			return fmt.Errorf("QQ 网关心跳间隔无效: %v", hello.HeartbeatInterval)
		}

		// 先启动心跳，再决定 identify 还是 resume。
		g.startHeartbeat(ctx, time.Duration(hello.HeartbeatInterval)*time.Millisecond)

		g.mu.Lock()
		hasSession := g.sessionID != "" && g.seq > 0
		sessionID := g.sessionID
		seq := g.seq
		g.mu.Unlock()

		if hasSession {
			g.log.Info("QQ 网关尝试恢复会话", "session", sessionID)
			return g.send(ctx, map[string]any{
				"op": opResume,
				"d": map[string]any{
					"token":      "QQBot " + g.token(ctx),
					"session_id": sessionID,
					"seq":        seq,
				},
			})
		}

		return g.send(ctx, map[string]any{
			"op": opIdentify,
			"d": map[string]any{
				"token":   "QQBot " + g.token(ctx),
				"intents": g.intents,
				"shard":   []int{0, 1},
			},
		})

	case opDispatch:
		return g.handleDispatch(p)

	case opHeartbeat:
		// 服务端要求立即心跳。
		return g.sendHeartbeat(ctx)

	case opHeartbeatACK:
		// 正常确认，无需处理。

	case opReconnect:
		g.log.Info("QQ 网关要求重连")
		g.closeConn()
		return nil

	case opInvalidSession:
		g.log.Warn("QQ 网关会话无效，将重新鉴权")
		g.mu.Lock()
		g.sessionID = ""
		g.seq = 0
		g.mu.Unlock()
		g.closeConn()
		return nil
	}
	return nil
}

// handleDispatch 处理事件分发。
func (g *Gateway) handleDispatch(p gatewayPayload) error {
	g.mu.Lock()
	if p.S > 0 {
		g.seq = p.S
	}
	g.mu.Unlock()

	switch p.T {
	case eventReady:
		var ready struct {
			SessionID string `json:"session_id"`
			User      struct {
				ID       string `json:"id"`
				Username string `json:"username"`
			} `json:"user"`
		}
		if err := json.Unmarshal(p.D, &ready); err != nil {
			return fmt.Errorf("解析 READY 失败: %w", err)
		}
		if ready.SessionID == "" || ready.User.ID == "" {
			return fmt.Errorf("QQ 网关 READY 缺少账号或会话 ID")
		}

		g.mu.Lock()
		g.sessionID = ready.SessionID
		g.botID = ready.User.ID
		g.botName = ready.User.Username
		g.ready = true
		g.mu.Unlock()

		g.log.Info("QQ 网关已就绪", "bot", ready.User.Username, "id", ready.User.ID)
		return nil

	case eventResumed:
		g.mu.Lock()
		g.ready = true
		g.mu.Unlock()
		g.log.Info("QQ 网关会话已恢复")
		return nil
	}

	if !IsMessageEvent(p.T) {
		return nil
	}

	g.mu.Lock()
	ready := g.ready
	g.mu.Unlock()
	if !ready {
		// Resume 的补发事件先于 RESUMED 到达，补发事件本身就说明已恢复。
		g.mu.Lock()
		g.ready = true
		g.mu.Unlock()
	}

	if g.onEvent != nil {
		g.onEvent(GatewayEvent{
			Type:     p.T,
			Data:     p.D,
			Sequence: p.S,
		})
	}
	return nil
}

// startHeartbeat 启动心跳协程。
func (g *Gateway) startHeartbeat(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}

	g.mu.Lock()
	if g.hbCancel != nil {
		g.hbCancel()
	}
	hbCtx, cancel := context.WithCancel(ctx)
	g.hbCancel = cancel
	g.mu.Unlock()

	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.C:
				if err := g.sendHeartbeat(hbCtx); err != nil {
					g.log.Debug("发送心跳失败", "err", err)
				}
			}
		}
	}()
}

func (g *Gateway) sendHeartbeat(ctx context.Context) error {
	g.mu.Lock()
	seq := g.seq
	g.mu.Unlock()
	return g.send(ctx, map[string]any{"op": opHeartbeat, "d": seq})
}

// send 发送一帧。
func (g *Gateway) send(ctx context.Context, payload any) error {
	g.mu.Lock()
	conn := g.conn
	g.mu.Unlock()

	if conn == nil {
		return fmt.Errorf("QQ WebSocket 未连接")
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Write(writeCtx, websocket.MessageText, raw)
}

// token 拿访问凭证，失败时返回空串（后续发送会因为鉴权失败暴露问题）。
func (g *Gateway) token(ctx context.Context) string {
	t, err := g.client.Token(ctx)
	if err != nil {
		g.log.Warn("获取网关鉴权 token 失败", "err", err)
		return ""
	}
	return t
}

func (g *Gateway) closeConn() {
	g.mu.Lock()
	conn := g.conn
	g.mu.Unlock()
	if conn != nil {
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}
}

// Stop 停止网关并等待心跳退出。
func (g *Gateway) Stop() {
	g.closeConn()
	g.wg.Wait()
}
