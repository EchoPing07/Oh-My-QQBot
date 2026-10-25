// Package tool 是工具注册与执行层。
//
// 对应 MaiBot 的 src/core/tooling.py + src/maisaka/builtin_tool/。
//
// 设计要点：加功能 = 加工具，不动主循环。
// 每个工具自己声明 schema、自己判断可见性、自己解析参数。
package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/llm"
)

// Availability 是判断工具是否可用的上下文。
// 对应 MaiBot 的 ToolAvailabilityContext。
type Availability struct {
	SessionID string
	IsGroup   bool
	GroupID   string
	UserID    string
	Platform  string
}

// Result 是一次工具执行结果。
// 对应 MaiBot 的 ToolExecutionResult。
type Result struct {
	// Content 是要写回上下文给模型看的内容。
	Content string
	// IsError 标记执行失败。
	IsError bool
	// Images 是工具返回的图片 URL，会作为多模态附件写回。
	Images []string
	// Metadata 供上层做展示用的附加信息。
	Metadata map[string]string
}

// TextResult 构造一个纯文本成功结果。
func TextResult(content string) Result { return Result{Content: content} }

// ErrorResult 构造一个失败结果。
func ErrorResult(format string, args ...any) Result {
	return Result{Content: fmt.Sprintf(format, args...), IsError: true}
}

// Invocation 是一次工具调用。
type Invocation struct {
	// Name 是工具名。
	Name string
	// Arguments 是原始 JSON 参数。
	Arguments json.RawMessage
	// CallID 是模型给出的调用 ID，写回上下文时要用。
	CallID string
	// Reasoning 是 Planner 当时的思考内容，某些工具（reply）需要它。
	Reasoning string
	// TurnID 标识这一轮思考，工具调用产生的下游事件要靠它归到同一轮。
	//
	// 为什么放在这里而不是让 runtime 自己去查：runtime 是长期存活的，
	// 它不知道「当前是第几轮」。只有发起这次循环的 Loop 知道。
	TurnID string
}

// Decode 把参数解到目标结构。
func (inv Invocation) Decode(dst any) error {
	if len(inv.Arguments) == 0 {
		return nil
	}
	return json.Unmarshal(inv.Arguments, dst)
}

// Tool 是所有能力必须实现的接口。
type Tool interface {
	// Name 是工具名，必须唯一。
	Name() string
	// Description 是给模型看的说明。
	Description() string
	// Schema 是参数的 JSON Schema。
	Schema() map[string]any
	// Visible 判断当前场景是否暴露这个工具。
	Visible(av Availability) bool
	// Execute 执行工具。
	Execute(ctx context.Context, inv Invocation, rt Runtime) (Result, error)
}

// Runtime 是工具能访问到的运行时能力。
//
// 这里用接口而不是具体类型，避免 tool 包反向依赖 agent 包造成循环引用。
type Runtime interface {
	// SessionID 返回当前聊天流 ID。
	SessionID() string
	// IsGroup 返回是否群聊。
	IsGroup() bool
	// BotName 返回机器人名字。
	BotName() string
	// Reply 触发一次真实的发送。工具只管请求，发送由 runtime 负责。
	Reply(req ReplyRequest) (ReplyOutcome, error)
	// Wait 请求等待一段时间。
	Wait(seconds float64, reason string) error
	// Stop 结束本轮思考。
	Stop(reason string)
	// Conversation 返回最近的对话历史，供工具读取。
	Conversation() []llm.Message
	// RememberReply 记录本次回复，用于后续引用判断。
	//
	// 除了文本，还要带上出站消息的 ID 与引用索引：
	// 群里有人问「你刚才那句」时，模型要引用的是木木自己发过的那条，
	// 而引用索引只存在于发送响应里，事后没有别的来源。
	RememberReply(text string, messageIDs, refIdxs []string)
	// SearchMemory 检索长期记忆。
	//
	// 对应参考实现 query_memory.py 调用的 memory_service.search。
	// 本项目的检索是「分词 + FTS5」而非向量（端点没有 embedding 服务），
	// 所以 mode 只支持 search / time，hybrid 退化为 time 优先再补 search。
	SearchMemory(req MemoryQuery) (MemoryResult, error)
	// PersonProfile 返回某个用户的画像文本，没有时返回空串。
	PersonProfile(userID string) string
	// PickEmoji 按情绪描述选一个表情包。
	PickEmoji(emotion string) (EmojiRef, bool)
	// SendEmoji 发送一个已选中的表情包，返回平台消息 ID。
	SendEmoji(ref EmojiRef) ([]string, error)
}

// MemoryQuery 是一次长期记忆检索请求。
type MemoryQuery struct {
	// Query 是关键词或问题。mode=time 时可为空。
	Query string
	// Mode 是检索模式：search / time / hybrid。
	Mode string
	// TimeStart / TimeEnd 是 Unix 秒，仅 time / hybrid 使用。
	TimeStart int64
	TimeEnd   int64
	// PersonName 用于人物定向过滤，为空表示不过滤。
	PersonName string
	// Limit 是返回条数。
	Limit int
}

// MemoryHit 是一条命中结果。
type MemoryHit struct {
	// Summary 是记忆正文。
	Summary string
	// TimeRange 是这条记忆覆盖的时间范围描述。
	TimeRange string
	// Participants 是涉及的人物。
	Participants []string
	// CreatedAt 是记忆生成时刻（Unix 秒）。
	CreatedAt int64
}

// MemoryResult 是检索结果。
type MemoryResult struct {
	Hits []MemoryHit
	// Mode 是实际使用的模式（可能与请求不同，例如 hybrid 降级）。
	Mode string
	// Fallback 标记是否发生了降级。
	Fallback bool
}

// EmojiRef 指向一个已入库的表情包。
type EmojiRef struct {
	// Hash 是内容哈希，全局唯一。
	Hash string
	// Path 是相对工作目录的文件路径。
	Path string
	// Description 是情绪标签。
	Description string
}

// ReplyRequest 是一次回复请求。
// 参数与 MaiBot 的 reply 工具对齐。
type ReplyRequest struct {
	// MsgID 是要回复的消息 ID。
	MsgID string
	// Reference 是上下文关键信息与回复方向，不是正文。
	// 这是整个设计的点睛之笔：Planner 只说想表达什么方向，Replyer 负责真正措辞。
	Reference string
	// Style 控制篇幅：简短表达 / 正常回复 / 长回复。
	Style string
	// AttachPics 是随回复附带的图片。
	AttachPics []AttachPic
	// AttachAt 是随回复 @ 的目标消息 ID。
	AttachAt []string
	// TurnID 是这次回复所属的思考轮次，用于把 Replyer 事件挂到时间线上。
	TurnID string
	// AttachEmoji 是随回复附带的表情包情绪描述。
	//
	// 用情绪描述而不是哈希：模型看不到表情包库，让它填哈希只会瞎编。
	// 实际选图在 runtime 里按描述检索，选不到就静默跳过。
	AttachEmoji string
}

// AttachPic 指向一张要附带的图片。
type AttachPic struct {
	MsgID string
	Index int
}

// ReplyOutcome 是一次回复的结果。
type ReplyOutcome struct {
	// Text 是最终发出的文本。
	Text string
	// MessageIDs 是发出的消息 ID。
	MessageIDs []string
	// RefIdxs 与 MessageIDs 一一对应，是各条消息自己的引用索引。
	//
	// 由 runtime 从发送响应里取回。工具层把它原样交给 RememberReply，
	// 否则「引用机器人自己的发言」这条链路永远断在这里。
	RefIdxs []string
}

// Registry 是工具注册表。
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
	order []string
}

// NewRegistry 创建空注册表。
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// Register 注册一个工具。
func (r *Registry) Register(t Tool) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	name := t.Name()
	if name == "" {
		return fmt.Errorf("工具名不能为空")
	}
	if _, exists := r.tools[name]; exists {
		return fmt.Errorf("工具 %q 重复注册", name)
	}
	r.tools[name] = t
	r.order = append(r.order, name)
	return nil
}

// MustRegister 注册并 panic，用于内置工具。
func (r *Registry) MustRegister(t Tool) {
	if err := r.Register(t); err != nil {
		panic(err)
	}
}

// Get 按名字取工具。
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Definitions 返回当前场景下可见的工具定义。
func (r *Registry) Definitions(av Availability) []llm.ToolDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]llm.ToolDefinition, 0, len(r.order))
	for _, name := range r.order {
		t := r.tools[name]
		if !t.Visible(av) {
			continue
		}
		out = append(out, llm.ToolDefinition{
			Type: "function",
			Function: llm.ToolFunctionDefine{
				Name:        t.Name(),
				Description: t.Description(),
				Parameters:  t.Schema(),
			},
		})
	}
	return out
}

// Names 返回所有已注册的工具名，按注册顺序。
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.order...)
}

// Execute 执行一次工具调用。
func (r *Registry) Execute(ctx context.Context, inv Invocation, rt Runtime) (Result, error) {
	t, ok := r.Get(inv.Name)
	if !ok {
		return ErrorResult("工具 %q 不存在", inv.Name), fmt.Errorf("工具 %q 不存在", inv.Name)
	}

	// 工具内部 panic 不应该拖垮整个会话。
	result, err := safeExecute(ctx, t, inv, rt)
	if err != nil {
		return result, err
	}
	return result, nil
}

// safeExecute 包一层 panic 恢复。
func safeExecute(ctx context.Context, t Tool, inv Invocation, rt Runtime) (result Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("工具 %s 执行 panic: %v", t.Name(), r)
			result = ErrorResult("工具执行出错: %v", r)
		}
	}()
	return t.Execute(ctx, inv, rt)
}

// BuildToolsDescription 生成给提示词用的工具列表说明。
// 用于在 system prompt 里补充工具用途（除了 JSON schema 之外的说明）。
func (r *Registry) BuildToolsDescription(av Availability) string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var lines []string
	for _, name := range r.order {
		t := r.tools[name]
		if !t.Visible(av) {
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s: %s", t.Name(), t.Description()))
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n")
}

// SortedNames 返回按字典序排列的工具名，用于稳定输出。
func (r *Registry) SortedNames() []string {
	names := r.Names()
	sort.Strings(names)
	return names
}
