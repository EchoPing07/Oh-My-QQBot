package qq

import (
	"context"
	"sync"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/logx"
)

// Adapter 把 QQ 网关、编解码、发送器组装成一个可用的适配器。
//
// 对应 MaiBot-QQ-Adapter 插件里的 handle_qq_official_gateway。
type Adapter struct {
	client  *Client
	gateway *Gateway
	sender  *Sender
	log     *logx.Logger

	// onMessage 是入站回调，由 app 层注入。
	onMessage func(msg *domain.Message, scene, target, accountID string)

	mu      sync.Mutex
	account string
	name    string

	// 管理命令回调，处理 /assign_id 这类命令。
	onCommand func(msg *domain.Message, scene, target string) bool

	// baseCtx 是事件回调可用的生命周期 context，由 Run 注入。
	// 入站附件下载要挂一个能随进程退出而取消的 context，
	// 但 handleEvent 本身没有 ctx 参数。
	baseCtx context.Context

	wg sync.WaitGroup
}

// NewAdapter 创建适配器。
//
// intents 来自配置 qq.intents；<= 0 时网关自己取默认值。
func NewAdapter(client *Client, intents int) *Adapter {
	a := &Adapter{
		client:  client,
		sender:  NewSender(client),
		log:     logx.Get("qq.adapter"),
		baseCtx: context.Background(),
	}
	a.gateway = NewGateway(client, intents, a.handleEvent)
	return a
}

// OnMessage 注册入站消息回调。
func (a *Adapter) OnMessage(fn func(msg *domain.Message, scene, target, accountID string)) {
	a.onMessage = fn
}

// OnCommand 注册管理命令回调。
// 返回 true 表示已处理，消息不再进入聊天流。
func (a *Adapter) OnCommand(fn func(msg *domain.Message, scene, target string) bool) {
	a.onCommand = fn
}

// Sender 返回发送器，供 agent 层使用。
func (a *Adapter) Sender() *Sender { return a.sender }

// AccountID 返回机器人账号。
func (a *Adapter) AccountID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.account
}

// BotName 返回机器人名字。
func (a *Adapter) BotName() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.name
}

// Run 启动网关。阻塞直到 ctx 取消。
func (a *Adapter) Run(ctx context.Context, reconnectDelay time.Duration) error {
	// 等 READY 事件回填账号后，把账号同步给发送器与上层。
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.syncAccountLoop(ctx)
	}()

	hctx, cancel := context.WithCancel(ctx)
	defer cancel()

	a.mu.Lock()
	a.baseCtx = hctx
	a.mu.Unlock()

	return a.gateway.Run(hctx, reconnectDelay)
}

// syncAccountLoop 轮询等待 READY，把账号信息同步出来。
func (a *Adapter) syncAccountLoop(ctx context.Context) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !a.gateway.Ready() {
				continue
			}
			id := a.gateway.BotID()
			name := a.gateway.BotName()
			if id == "" {
				continue
			}

			a.mu.Lock()
			changed := a.account != id
			a.account = id
			a.name = name
			a.mu.Unlock()

			if changed {
				a.log.Info("QQ 账号已就绪", "account", id, "name", name)
			}
			return
		}
	}
}

// Stop 停止适配器。
func (a *Adapter) Stop() {
	a.gateway.Stop()
	a.wg.Wait()
}

// handleEvent 处理网关推送的事件。
func (a *Adapter) handleEvent(ev GatewayEvent) {
	// 先无差别记录事件类型，用于确认「网关到底推送了哪些事件」。
	// intents 配得对不对，看这一行日志就知道。
	a.log.Debug("网关事件", "type", ev.Type, "seq", ev.Sequence)

	// 需要账号 ID 才能判断是否 @ 了机器人。没就绪时丢弃，README 的行为也是如此。
	accountID := a.gateway.BotID()
	if accountID == "" {
		a.log.Warn("收到消息事件但机器人身份未就绪，丢弃", "type", ev.Type)
		return
	}

	result, err := Decode(ev.Type, ev.Data, accountID, a.gateway.BotName())
	if err != nil {
		a.log.Warn("解码消息失败", "err", err, "type", ev.Type)
		return
	}

	msg := result.Message

	// 立刻下载图片/表情附件。
	//
	// 必须在投递进聊天流之前做完：QQ 给的附件地址是有时效的签名 URL，
	// 晚点再取大概率 403。下载失败的段保留占位文本，不影响消息本身。
	a.mu.Lock()
	baseCtx := a.baseCtx
	a.mu.Unlock()
	hydrateInboundMedia(baseCtx, msg, a.log)

	// 记录被动回复窗口，后续发送可以用 msg_id 免额度。
	a.sender.RecordInbound(result.Scene, result.TargetID, msg.MessageID, time.Now())

	// 入站日志。排查「消息到底有没有到」时这是第一手证据。
	a.log.Info("收到消息",
		"scene", result.Scene,
		"group", msg.GroupID,
		"user", msg.UserName,
		"user_id", msg.UserID,
		"at", msg.IsAt,
		"text", msg.PlainText,
	)

	// 管理命令优先处理。
	if a.onCommand != nil && a.onCommand(msg, result.Scene, result.TargetID) {
		return
	}

	if a.onMessage != nil {
		a.onMessage(msg, result.Scene, result.TargetID, accountID)
	}
}
