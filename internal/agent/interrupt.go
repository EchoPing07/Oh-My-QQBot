package agent

import "sync"

// PlannerInterruptController 维护当前可打断的 Planner 请求与连续打断计数。
//
// 对应 MaiBot 的 runtime.py:103 PlannerInterruptController，行为逐条对齐：
//
//	request() 返回四种结果
//	  "idle"      —— 当前没有在跑的请求，无需打断
//	  "duplicate" —— 这次请求已经发起过一次打断，不重复打断
//	  "limit"     —— 连续打断次数已达上限，等当前请求自然完成
//	  "request"   —— 真正发起打断，计数 +1
//
//	unbind(interrupted=false) —— 一次「自然完成」会把计数清零。
//
// 为什么需要上限（此前是无限打断）：群里连续轰炸时，
// 每次新消息都取消正在进行的模型请求，模型永远跑不完一轮，
// 表现是「机器人完全不理人」而日志里全是重复的请求。
//
// 默认值 0 的语义照抄参考实现（official_configs.py:576，
// `_consecutive_count >= max_consecutive_count` 先于打断判断）：
// **配置为 0 时不打断正在进行的思考**，让它自然跑完再处理新消息。
type PlannerInterruptController struct {
	mu sync.Mutex

	// active 表示当前是否有一个可打断的请求在跑。
	active bool
	// requested 表示这一轮是否已经发起过打断。
	requested bool
	// consecutive 是连续打断次数。
	consecutive int

	// cancel 是当前请求的取消函数，由 bind 注入。
	cancel func()
}

// NewPlannerInterruptController 创建控制器。
func NewPlannerInterruptController() *PlannerInterruptController {
	return &PlannerInterruptController{}
}

// Bind 标记「现在有一个可被打断的请求在跑」。
//
// cancel 为 nil 时也算已绑定：单测里会这么构造，
// 打断时只需要不出错，不需要真的取消什么。
func (c *PlannerInterruptController) Bind(cancel func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.active = true
	c.requested = false
	c.cancel = cancel
}

// Unbind 标记请求结束，并维护连续打断计数。
//
// interrupted 为 false（自然完成）时计数清零 —— 这是「上限」能生效的前提：
// 只有连续被打断才累计，中间只要成功跑完一轮就重新计数。
func (c *PlannerInterruptController) Unbind(interrupted bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.active = false
	c.requested = false
	c.cancel = nil
	if !interrupted {
		c.consecutive = 0
	}
}

// Consecutive 返回连续打断次数。
func (c *PlannerInterruptController) Consecutive() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.consecutive
}

// Request 尝试发起一次打断，返回 idle / duplicate / limit / request。
//
// maxConsecutiveCount 语义与参考实现一致：
// 值为 0 时永远返回 "limit"（不打断），<0 视为不限制。
func (c *PlannerInterruptController) Request(maxConsecutiveCount int) string {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.active {
		return InterruptIdle
	}
	if c.requested {
		return InterruptDuplicate
	}
	if maxConsecutiveCount >= 0 && c.consecutive >= maxConsecutiveCount {
		return InterruptLimit
	}

	c.requested = true
	c.consecutive++
	if c.cancel != nil {
		c.cancel()
	}
	return InterruptRequest
}

// Request() 的返回值常量。
const (
	InterruptIdle      = "idle"
	InterruptDuplicate = "duplicate"
	InterruptLimit     = "limit"
	InterruptRequest   = "request"
)
