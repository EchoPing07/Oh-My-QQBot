// Package learning 负责从聊天记录里学习表达方式与黑话。
// 对应 MaiBot 的 src/learners/expression_learner.py 与 jargon_learner.py。
package learning

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/llm"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/logx"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/store"
)

// cursorKeyPrefix 是每个会话学习游标在 kv 表里的键前缀。
// 用「会话 → 已学到的位置」而不是「记住消息 ID 列表」，
// 因为 ID 列表会随聊天量无限膨胀，而单个位置标记不会。
const cursorKeyPrefix = "learn.cursor."

// sessionScanLimit 是一次 RunOnce 最多扫描多少个会话。
//
// 学习是后台任务，不该把全库会话都过一遍：既费模型调用，
// 也会让一次 RunOnce 跑得比学习间隔还久。取最近活跃的一批就够。
const sessionScanLimit = 20

// messagesPerSession 是单会话一次最多取多少条消息。
//
// 比 MinMessages 的默认值（30）大一些，够覆盖积压又不会把一次请求撑爆。
const messagesPerSession = 100

// Learner 是表达学习与黑话学习的执行者。
type Learner struct {
	cfg   *config.Store
	llm   *llm.Client
	store *store.DB
	log   *logx.Logger

	// running 保证同一进程内不会有两轮学习并发跑。
	// 并发跑不会写坏数据（表达有唯一索引、黑话是 upsert），
	// 但两轮会读到同一个游标，相当于对同一批消息各调一次模型，白花钱。
	running sync.Mutex
}

// Options 是构造 Learner 的依赖。
type Options struct {
	Config *config.Store
	LLM    *llm.Client
	Store  *store.DB
}

// New 创建学习器。
func New(opts Options) *Learner {
	return &Learner{
		cfg:   opts.Config,
		llm:   opts.LLM,
		store: opts.Store,
		log:   logx.Get("learning"),
	}
}

// Start 启动后台学习循环，每 cfg.Learning.IntervalMinutes 分钟尝试一次。
//
// 立即返回，且不在启动瞬间就学第一次：刚起来的机器人往往要先恢复上下文，
// 这时候抢模型配额没有意义。ctx 取消即退出。
func (l *Learner) Start(ctx context.Context) {
	interval := time.Duration(l.cfg.Get().Learning.IntervalMinutes) * time.Minute
	if interval <= 0 {
		interval = 30 * time.Minute
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				l.log.Info("表达学习已停止")
				return
			case <-ticker.C:
				expressions, jargons, err := l.RunOnce(ctx)
				if err != nil {
					l.log.Warn("学习失败", "err", err)
					continue
				}
				if expressions > 0 || jargons > 0 {
					l.log.Info("学习完成", "expressions", expressions, "jargons", jargons)
				}
			}
		}
	}()
}

// RunOnce 执行一次学习：取最近未学习过的消息，提取表达与黑话并落库。
//
// 返回本轮新写入的表达条数与黑话条数。
// 单个会话出错不影响其它会话，最后汇总成一个错误返回 ——
// 学习是尽力而为的后台任务，一个会话的脏数据不该让整轮白跑。
func (l *Learner) RunOnce(ctx context.Context) (expressions int, jargons int, err error) {
	if !l.cfg.Get().Learning.ExpressionEnabled && !l.cfg.Get().Learning.JargonEnabled {
		// 两个开关都关时连数据库都不碰，避免后台任务空转。
		return 0, 0, nil
	}
	if l.store == nil || l.llm == nil {
		return 0, 0, fmt.Errorf("学习失败: 依赖未初始化（store 或 llm 为空）")
	}

	// 上一轮还在跑就跳过本轮而不是排队：排队只会让积压消息学得更晚。
	if !l.running.TryLock() {
		l.log.Debug("上一轮学习尚未结束，跳过本轮")
		return 0, 0, nil
	}
	defer l.running.Unlock()

	sessions, err := l.store.Sessions().List(ctx, sessionScanLimit)
	if err != nil {
		return 0, 0, fmt.Errorf("学习失败: 读取会话列表: %w", err)
	}

	var errs []error
	for _, sess := range sessions {
		if ctx.Err() != nil {
			return expressions, jargons, ctx.Err()
		}

		exp, jar, err := l.learnSession(ctx, sess.ID)
		expressions += exp
		jargons += jar
		if err != nil {
			l.log.Warn("会话学习失败", "session", sess.ID, "err", err)
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return expressions, jargons, fmt.Errorf("学习部分失败（已写入表达 %d 条、黑话 %d 条）: %w",
			expressions, jargons, errors.Join(errs...))
	}
	return expressions, jargons, nil
}

// learnSession 处理单个会话的一批新消息。
func (l *Learner) learnSession(ctx context.Context, sessionID string) (expressions int, jargons int, err error) {
	cur, err := l.loadCursor(ctx, sessionID)
	if err != nil {
		return 0, 0, fmt.Errorf("读取学习游标失败: %w", err)
	}

	fresh, lastSeen, err := l.fetchFreshMessages(ctx, sessionID, cur)
	if err != nil {
		return 0, 0, err
	}
	if len(fresh) < l.minMessages() {
		// 素材不够就等下一轮。这里不推进游标是故意的：
		// 攒到 MinMessages 条才学，正是「不要为几条消息花一次模型调用」的意思。
		return 0, 0, nil
	}

	chatStr, sourceByID, ok := buildChatString(fresh)
	if !ok {
		// 防御分支：isLearnable 已经保证每条都有文字，正常情况下走不到。
		// 留着是防以后改 textOnly 时忘了这里，顺便把游标推过这批消息。
		if lastSeen != nil {
			_ = l.saveCursor(ctx, sessionID, cursor{at: lastSeen.ReceivedAt, lastID: lastSeen.ID})
		}
		return 0, 0, nil
	}

	// 游标推进到「读到的最后一条消息」，而不是「最后一条可学消息」：
	// 否则夹在中间的机器人发言/图片消息会被每轮重新读出来，
	// 一旦它们占了 minMessages 的大头，这个会话就永远学不动了。
	//
	// 模型调用失败时也推进：同一批文本再问一次多半得到同样结果，重试只是重复花钱；
	// 更糟的是永远解析失败的窗口会让后来的新消息再也学不到。
	// 代价是这一批丢一次学习机会，但消息还在库里，游标也能手工重置。
	// 带上内部 ID 作为同秒并列时的决胜键（见 cursor 的说明）。
	last := fresh[len(fresh)-1]
	if lastSeen != nil {
		last = lastSeen
	}
	next := cursor{at: last.ReceivedAt, lastID: last.ID}

	var errs []error
	if l.cfg.Get().Learning.ExpressionEnabled {
		n, err := l.learnExpressions(ctx, sessionID, chatStr, sourceByID)
		if err != nil {
			// 表达失败不阻断黑话：两个提示词独立、互不依赖。
			errs = append(errs, err)
		}
		expressions = n
	}

	if l.cfg.Get().Learning.JargonEnabled {
		n, err := l.learnJargons(ctx, sessionID, chatStr)
		if err != nil {
			errs = append(errs, fmt.Errorf("学习黑话失败: %w", err))
		}
		jargons = n

		// 顺带把够格的黑话推进一步：推断含义。
		// 放在学完之后而不是单独一轮，是因为推断的触发条件就是「又见到一次」，
		// 刚累加完次数正是判断的最佳时机。
		if merr := l.inferPendingMeanings(ctx, chatStr); merr != nil {
			errs = append(errs, fmt.Errorf("推断黑话含义失败: %w", merr))
		}
	}

	if err := l.saveCursor(ctx, sessionID, next); err != nil {
		errs = append(errs, fmt.Errorf("保存学习游标失败: %w", err))
	}
	return expressions, jargons, errors.Join(errs...)
}

// learnExpressions 调模型提取表达并写入。
func (l *Learner) learnExpressions(ctx context.Context, sessionID, chatStr string, sourceByID map[string]string) (int, error) {
	promptText := renderPrompt(promptNameLearnStyle, map[string]string{chatStrKey: chatStr})
	resp, err := l.llm.Generate(ctx, config.TaskLearner, &llm.Request{
		Messages: []llm.Message{{Role: llm.RoleSystem, Content: promptText}},
	})
	if err != nil {
		return 0, fmt.Errorf("学习表达失败: 调用模型: %w", err)
	}

	items, err := ParseExpressions(resp.Content)
	if err != nil {
		return 0, fmt.Errorf("学习表达失败: %w", err)
	}
	if len(items) == 0 {
		l.log.Debug("模型没有提取出表达方式", "session", sessionID)
		return 0, nil
	}

	// 单批上限 20 条，逐字对齐 expression_learner.py:432。
	// 超过直接整批丢弃而不是截断：能吐出 20 条以上的多半是模型在重复造词，
	// 截前 20 条只是把噪声写进库。
	const maxExpressionsPerBatch = 20
	if len(items) > maxExpressionsPerBatch {
		l.log.Info("表达方式数量超过限制，整批丢弃",
			"session", sessionID, "count", len(items), "limit", maxExpressionsPerBatch)
		return 0, nil
	}

	filtered := l.filterExpressions(items, sourceByID)
	if len(filtered) == 0 {
		l.log.Debug("没有表达方式通过过滤", "session", sessionID, "candidates", len(items))
		return 0, nil
	}

	now := time.Now().Unix()
	saved := 0
	for _, it := range filtered {
		e := &store.Expression{
			SessionID:  sessionID,
			Situation:  it.Situation,
			Style:      it.Style,
			SourceText: sourceByID[it.SourceID],
			CreatedAt:  now,
		}
		isNew, err := l.store.Memory().SaveExpression(ctx, e)
		if err != nil {
			return saved, fmt.Errorf("学习表达失败: 写入 %q: %w", it.Situation, err)
		}
		if isNew {
			saved++
		}
	}
	return saved, nil
}

// filterExpressions 丢掉不该入库的表达候选。
//
// 规则逐字对齐参考实现 expression_learner.py:_filter_expressions（第 640-712 行）：
//   - source_id 必须是数字且落在本批范围内（否则无法溯源到真人发言）；
//   - 不学 style 与机器人名字一致的内容；
//   - situation/style 里出现 SELF、[表情包、[图片 一律丢掉。
//
// 参考实现还检查了来源消息本身是不是机器人发言、是否为空 —— 我们已经在
// isLearnable / textOnly 阶段把这类消息挤出素材了，所以不重复。
func (l *Learner) filterExpressions(items []ExpressionItem, sourceByID map[string]string) []ExpressionItem {
	banned := make(map[string]struct{}, 1+len(l.cfg.Get().Bot.AliasNames))
	addBanned := func(name string) {
		if name = strings.TrimSpace(name); name != "" {
			banned[strings.ToLower(name)] = struct{}{}
		}
	}
	addBanned(l.cfg.Get().Bot.Nickname)
	for _, alias := range l.cfg.Get().Bot.AliasNames {
		addBanned(alias)
	}

	out := make([]ExpressionItem, 0, len(items))
	for _, it := range items {
		if _, ok := sourceByID[it.SourceID]; !ok {
			l.log.Debug("丢弃无法溯源的表达", "situation", it.Situation, "source_id", it.SourceID)
			continue
		}
		if _, dup := banned[strings.ToLower(it.Style)]; dup {
			l.log.Debug("丢弃与机器人名字一致的表达", "style", it.Style)
			continue
		}
		joined := it.Situation + "\x00" + it.Style
		if strings.Contains(joined, "SELF") ||
			strings.Contains(joined, "[表情包") ||
			strings.Contains(joined, "[图片") {
			l.log.Debug("丢弃含非文字标记的表达", "situation", it.Situation, "style", it.Style)
			continue
		}
		out = append(out, it)
	}
	return out
}

// inferPendingMeanings 给达到阈值的黑话推断含义。
//
// 每轮最多推断 3 条：推断要单独调模型（不合格还要调第二次），
// 一轮里把所有到阈值的都推一遍会让一次学习任务跑上几分钟。
func (l *Learner) inferPendingMeanings(ctx context.Context, chatStr string) error {
	list, err := l.store.Memory().ListJargons(ctx, 50)
	if err != nil {
		return fmt.Errorf("读取黑话列表: %w", err)
	}

	const maxPerRound = 3
	done := 0
	var errs []error

	for _, j := range list {
		if done >= maxPerRound {
			break
		}
		if !shouldInferMeaning(j.Count, j.LastInferenceCount, j.Meaning) {
			continue
		}

		meaning, ierr := l.inferMeaning(ctx, j.Content, chatStr, j.Meaning)
		if ierr != nil {
			errs = append(errs, fmt.Errorf("推断 %q 失败: %w", j.Content, ierr))
			continue
		}
		// 模型说信息不足（两种问法都没给出含义）时，
		// 也要把 last_inference_count 推上去 —— 否则下一批消息还会再问一次
		// 同样的问题，而那时上下文并没有多出什么有价值的信息。
		if err := l.store.Memory().UpdateJargonMeaning(ctx, j.ID, meaning, j.Count, time.Now().Unix()); err != nil {
			errs = append(errs, fmt.Errorf("保存 %q 的含义失败: %w", j.Content, err))
			continue
		}
		done++

		if meaning == "" {
			l.log.Debug("黑话含义推断无结果", "content", j.Content, "count", j.Count)
		} else {
			l.log.Info("推断出黑话含义", "content", j.Content, "meaning", meaning, "count", j.Count)
		}
	}
	return errors.Join(errs...)
}

// learnJargons 调模型提取黑话并写入。
func (l *Learner) learnJargons(ctx context.Context, sessionID, chatStr string) (int, error) {
	promptText := renderPrompt(promptNameLearnJargon, map[string]string{chatStrKey: chatStr})
	resp, err := l.llm.Generate(ctx, config.TaskLearner, &llm.Request{
		Messages: []llm.Message{{Role: llm.RoleSystem, Content: promptText}},
	})
	if err != nil {
		return 0, fmt.Errorf("调用模型: %w", err)
	}

	items, err := ParseJargons(resp.Content)
	if err != nil {
		return 0, err
	}
	if len(items) == 0 {
		l.log.Debug("模型没有提取出黑话", "session", sessionID)
		return 0, nil
	}

	now := time.Now().Unix()
	for _, it := range items {
		j := &store.Jargon{
			Content: it.Content,
			// Context 记会话 ID：含义还没推断，先留下它是从哪个群学来的。
			Context:   sessionID,
			CreatedAt: now,
			UpdatedAt: now,
		}
		if err := l.store.Memory().SaveJargon(ctx, j); err != nil {
			return 0, fmt.Errorf("写入黑话 %q 失败: %w", it.Content, err)
		}
	}
	// SaveJargon 的 upsert 不回报是新增还是更新，
	// 这里按模型给出的候选条数计，语义是「本轮学到的黑话条数」。
	return len(items), nil
}

// fetchFreshMessages 取本会话在游标之后的新消息，并按学习要求过滤。
//
// 第二个返回值是「读到的最后一条原始消息」（不论是否被过滤掉），
// 供调用方在没拿到可用素材时也能把游标推过去。
func (l *Learner) fetchFreshMessages(ctx context.Context, sessionID string, cur cursor) ([]*domain.Message, *domain.Message, error) {
	all, err := l.store.Messages().Since(ctx, sessionID, cur.at, messagesPerSession)
	if err != nil {
		return nil, nil, fmt.Errorf("读取会话消息失败: %w", err)
	}

	// Messages().Since 是 >= 语义，同一秒内已经学过的那几条会被再读出来。
	// 用上一轮最后一条的内部 ID 作为决胜键把它们切掉：
	// 时间只精确到秒，单靠时间戳无法区分「同秒但已学」和「同秒且未学」。
	start := 0
	if cur.lastID != "" {
		for i, m := range all {
			if m.ID == cur.lastID {
				start = i + 1
				break
			}
		}
	}
	all = all[start:]

	botIDs := l.botIDs()
	out := make([]*domain.Message, 0, len(all))
	for _, m := range all {
		if isLearnable(m, botIDs) {
			out = append(out, m)
		}
	}

	var lastSeen *domain.Message
	if len(all) > 0 {
		lastSeen = all[len(all)-1]
	}
	return out, lastSeen, nil
}

// isLearnable 判断一条消息是否该进学习素材。
//
// 三条硬规则都来自参考实现：
//   - learn_style.prompt 第 2 条、learn_jargon.prompt「只从 USER 来源中提取」：
//     不学机器人自己的发言；
//   - learn_style.prompt 第 1 条「只考虑文字」：图片/表情包不算；
//   - 命令消息不是聊天内容，学了只会污染风格。
func isLearnable(m *domain.Message, botIDs map[string]struct{}) bool {
	if m == nil || m.IsCommand {
		return false
	}
	if _, self := botIDs[m.UserID]; self {
		return false
	}
	return textOnly(m) != ""
}

// textOnly 只拼 text 段，忽略图片/表情包/语音/文件。
//
// 与 memory.renderForSummary 的区别：那边会给图片补 [图片] 占位，
// 这里必须真的丢掉 —— 学习提示词明确要求只看文字，
// 占位符进提示词会让模型把"图片"当成一种可学的语言特征。
func textOnly(m *domain.Message) string {
	// 历史数据或测试构造可能只有 PlainText 没有 Segments。
	if len(m.Segments) == 0 {
		return strings.TrimSpace(m.PlainText)
	}

	var b strings.Builder
	for _, seg := range m.Segments {
		if seg.Type != domain.SegmentText {
			continue
		}
		b.WriteString(strings.TrimSpace(seg.Text))
	}
	return strings.TrimSpace(b.String())
}

// buildChatString 把消息渲染成提示词里的聊天记录，并返回 source_id → 原文的映射。
//
// 格式 `[source_id:N] 用户名: 内容` 一行一条，N 从 1 开始，
// 与这两份提示词里 source_id 字段说明的对账方式一致（见 prompts.go 的常量注释）。
func buildChatString(msgs []*domain.Message) (string, map[string]string, bool) {
	var b strings.Builder
	sourceByID := make(map[string]string, len(msgs))

	idx := 0
	for _, m := range msgs {
		text := textOnly(m)
		if text == "" {
			continue
		}
		idx++

		name := m.UserName
		if name == "" {
			name = m.UserID
		}
		if name == "" {
			name = "未知用户"
		}

		// 内容里的换行会破坏「一行一条」的结构，压成空格。
		text = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(text)
		line := fmt.Sprintf("[source_id:%d] %s: %s", idx, name, text)
		b.WriteString(line)
		b.WriteByte('\n')
		sourceByID[strconv.Itoa(idx)] = line
	}

	if idx == 0 {
		return "", nil, false
	}
	return b.String(), sourceByID, true
}

// botIDs 收集机器人自己的 openid。
//
// cfg.Bot.AccountID 由网关 READY 回填，cfg.QQ.UnifiedAccountID 是手动配置的统一账号 ID。
// 历史上这两个字段都承载过机器人身份（见 app/filter 的 SetBotIDs），所以都算。
func (l *Learner) botIDs() map[string]struct{} {
	out := make(map[string]struct{}, 2)
	for _, id := range []string{l.cfg.Get().Bot.AccountID, l.cfg.Get().QQ.UnifiedAccountID} {
		if id = strings.TrimSpace(id); id != "" {
			out[id] = struct{}{}
		}
	}
	return out
}

// minMessages 返回学习门槛。
func (l *Learner) minMessages() int {
	if l.cfg.Get().Learning.MinMessages > 0 {
		return l.cfg.Get().Learning.MinMessages
	}
	return 30
}

// cursor 是一个会话的学习进度。
//
// 为什么不只存时间戳：messages.created_at 是 INTEGER 秒精度，
// 同一秒里的多条消息无法用单个时间戳区分"学过"和"没学过"。
// 只用秒级时间戳会两种翻车法：要么重学同一批（同一秒内消息数 ≥ MinMessages 时
// 会无限重学），要么丢掉同一秒的后半批。所以再存一个最后一条消息的内部 ID 当决胜键。
type cursor struct {
	// at 是上一轮最后一条消息的接收时刻。
	at time.Time
	// lastID 是那条消息在本系统内的 ID，可能为空（首次学习或旧格式游标）。
	lastID string
}

// loadCursor 读取会话的学习游标。没有游标（从未学过）时从 Unix 0 开始。
func (l *Learner) loadCursor(ctx context.Context, sessionID string) (cursor, error) {
	raw, _, err := l.store.KV().Lookup(ctx, cursorKeyPrefix+sessionID)
	if err != nil {
		return cursor{}, err
	}

	raw = strings.TrimSpace(raw)
	if raw == "" {
		return cursor{at: time.Unix(0, 0)}, nil
	}

	atRaw, id, _ := strings.Cut(raw, "|")
	sec, err := strconv.ParseInt(strings.TrimSpace(atRaw), 10, 64)
	if err != nil || sec <= 0 {
		// 值损坏时当作从未学过：宁可重学一遍，也不要因为一个坏值永久停止学习。
		l.log.Warn("学习游标值损坏，按未学习处理", "session", sessionID, "value", raw)
		return cursor{at: time.Unix(0, 0)}, nil
	}
	return cursor{at: time.Unix(sec, 0), lastID: id}, nil
}

// saveCursor 保存会话的学习游标。
func (l *Learner) saveCursor(ctx context.Context, sessionID string, cur cursor) error {
	value := strconv.FormatInt(cur.at.Unix(), 10)
	if cur.lastID != "" {
		value += "|" + cur.lastID
	}
	return l.store.KV().Set(ctx, cursorKeyPrefix+sessionID, value)
}
