package config

import (
	"fmt"
	"strconv"
	"strings"
)

// 本文件是「配置项清单」：WebUI 的配置编辑页完全由它生成。
//
// 为什么用清单而不是反射：
//   - 反射能拿到字段，但拿不到「显示名」「说明」「取值范围」「是否需要重启」
//   - 反射会把 api_key / app_secret / password 这类敏感字段一起暴露出去
//   - 加一个字段时，用清单会强迫作者想一遍「它该不该给用户改」
//
// 与 MaiBot 的对比：它走的是 pydantic schema + 动态表单（dashboard/
// routes/config 的 /schema/bot）。我们只需要「几十个字段的简单表单」，
// 手写清单更短也更好读。

// FieldKind 是配置项的类型，决定前端用哪种控件。
type FieldKind string

const (
	KindString     FieldKind = "string"
	KindText       FieldKind = "text" // 多行文本
	KindInt        FieldKind = "int"
	KindFloat      FieldKind = "float"
	KindBool       FieldKind = "bool"
	KindStringList FieldKind = "string_list"
	KindEnum       FieldKind = "enum"
)

// Field 描述一个可编辑配置项。
type Field struct {
	// Key 是前端提交时用的键，形如 "chat.max_context_size"。
	// 用点分路径而不是嵌套 JSON：前端表单是平铺的，
	// 平铺 + 点分键的代码量最少，也不用处理深层合并。
	Key string `json:"key"`
	// Section 是页面上的分组标题。
	Section string `json:"section"`
	// Label 是显示名。
	Label string `json:"label"`
	// Help 是一句话说明。空白表示不显示。
	Help string    `json:"help,omitempty"`
	Kind FieldKind `json:"kind"`
	// Options 仅 KindEnum 使用。
	Options []string `json:"options,omitempty"`
	// Min / Max 仅数值类型使用，nil 表示该侧不限制。
	//
	// 用指针而不是「0 表示不限制」：有好几个字段的合法值包含 0
	// （talk_value=0 表示完全不说话、retention_days=0 表示永久保留），
	// 用 0 当哨兵就没法表达「下限是 0、负数不行」。
	Min *float64 `json:"min,omitempty"`
	Max *float64 `json:"max,omitempty"`
	// Restart 表示改完必须重启才生效。
	//
	// 判定标准很实际：这一项在启动时被读一次、之后不再读（监听端口、
	// 数据库路径、日志目录、QQ 凭据），就是需要重启；每次用到都读
	// （提示词、阈值、开关），就是即时生效。
	Restart bool `json:"restart,omitempty"`
}

// minf / maxf 把字面量包成指针，供 Field 的 Min/Max 使用。
func minf(v float64) *float64 { return &v }
func maxf(v float64) *float64 { return &v }

// EditableFields 返回全部可编辑配置项。
//
// 刻意不包含的（安全考虑，且改了多半会把程序搞坏）：
//   - qq.app_id / qq.app_secret：凭据，用环境变量或手改文件
//   - llm.providers[].api_key：密钥
//   - web.password / web.session_secret：在「安全」页单独处理
//   - database.path、data 目录：改错了下次启动连库都打不开
//   - llm 整节：模型绑定是结构化嵌套（providers/models/tasks），
//     平铺表单表达不了，也不该在网页上随手改
func EditableFields() []Field {
	return []Field{
		// —— 身份 ——
		{Key: "bot.nickname", Section: "身份", Label: "昵称",
			Help: "机器人在群里显示和自称时用的名字", Kind: KindString},
		// 别名校验要真的接上消费方：agent 用它判断「这句话是不是在叫我」。
		// 以前只存在于 struct 里，网页上没入口 —— 用户想把「小木」改成
		// 「木宝」只能手改 config.toml。
		{Key: "bot.alias_names", Section: "身份", Label: "别名",
			Help: "别人称呼它时可能用的名字，一行一个", Kind: KindStringList},

		// —— 人格 ——
		{Key: "personality.personality", Section: "人格", Label: "人格设定",
			Help: "身份设定，每次请求都会注入", Kind: KindText},
		{Key: "personality.behavior_style", Section: "人格", Label: "行为风格",
			Kind: KindText},
		{Key: "personality.reply_style", Section: "人格", Label: "表达风格",
			Kind: KindText},
		// 备用风格文本列表。
		//
		// 以前只暴露了 multiple_probability（触发概率），而它依赖的
		// multiple_reply_style 完全没有入口 —— 默认配置里是空数组，
		// 于是 selectReplyStyle 里 len(styles)==0 直接返回基准风格，
		// 那个概率旋钮永远空转，改完什么都不会发生。
		{Key: "personality.multiple_reply_style", Section: "人格", Label: "备用表达风格",
			Help: "随机启用的风格，一行一条；留空则下面的概率不生效",
			Kind: KindStringList},
		{Key: "personality.multiple_probability", Section: "人格", Label: "备用风格概率",
			Help: "0 表示不启用备用表达风格", Kind: KindFloat, Min: minf(0), Max: maxf(1)},

		// —— 聊天 ——
		{Key: "chat.max_context_size", Section: "聊天", Label: "群聊上下文条数",
			Kind: KindInt, Min: minf(1), Max: maxf(500)},
		{Key: "chat.max_private_context_size", Section: "聊天", Label: "私聊上下文条数",
			Kind: KindInt, Min: minf(1), Max: maxf(500)},
		// schema 每次请求现算（tool.NewReplyToolFunc），所以即时生效。
		{Key: "chat.enable_reply_at", Section: "聊天", Label: "回复时允许 @",
			Kind: KindBool},
		{Key: "chat.timing.debounce_mode", Section: "聊天", Label: "消息防抖档位",
			Kind: KindEnum, Options: []string{"off", "fast", "natural", "steady"}},
		{Key: "chat.timing.talk_value", Section: "聊天", Label: "群聊发言频率",
			Help: "0 更安静，1 按正常频率", Kind: KindFloat, Min: minf(0), Max: maxf(1)},
		{Key: "chat.timing.private_talk_value", Section: "聊天", Label: "私聊发言频率",
			Kind: KindFloat, Min: minf(0), Max: maxf(1)},
		{Key: "chat.timing.reply_trigger_mode", Section: "聊天", Label: "触发模式",
			Kind: KindEnum, Options: []string{"frequency", "dynamic"}},
		{Key: "chat.timing.mentioned_bot_reply", Section: "聊天", Label: "提到名字更容易回复",
			Kind: KindBool},
		{Key: "chat.timing.inevitable_at_reply", Section: "聊天", Label: "被 @ 时尽量回复",
			Kind: KindBool},
		{Key: "chat.timing.planner_interrupt_max_consecutive_count", Section: "聊天",
			Label: "思考被打断上限", Help: "0 表示完全不打断", Kind: KindInt, Min: minf(0), Max: maxf(20)},
		{Key: "chat.timing.max_consecutive_wait_count", Section: "聊天",
			Label: "最多连续 wait 次数", Kind: KindInt, Min: minf(1), Max: maxf(20)},

		{Key: "chat.style.group_quote_frequency", Section: "引用", Label: "群聊引用频率",
			Kind: KindEnum, Options: []string{"never", "occasionally", "often"}},
		{Key: "chat.style.private_quote_frequency", Section: "引用", Label: "私聊引用频率",
			Kind: KindEnum, Options: []string{"never", "occasionally", "often"}},
		{Key: "chat.style.group_chat_prompt", Section: "引用", Label: "群聊注意事项",
			Kind: KindText},
		{Key: "chat.style.private_chat_prompt", Section: "引用", Label: "私聊注意事项",
			Kind: KindText},

		// —— 回复后处理 ——
		{Key: "response.enable_split", Section: "回复", Label: "允许拆成多条",
			Kind: KindBool},
		{Key: "response.max_length", Section: "回复", Label: "单条最大长度",
			Kind: KindInt, Min: minf(10), Max: maxf(4000)},
		{Key: "response.max_sentence_num", Section: "回复", Label: "单条最大句数",
			Kind: KindInt, Min: minf(1), Max: maxf(50)},
		{Key: "response.max_split_num", Section: "回复", Label: "最多拆成几条",
			Kind: KindInt, Min: minf(1), Max: maxf(10)},
		{Key: "response.typing_speed", Section: "回复", Label: "模拟打字速度",
			Help: "0 最快，1 默认，2 更慢", Kind: KindFloat, Min: minf(0), Max: maxf(4)},

		// —— 消息过滤 ——
		{Key: "message.ban_words", Section: "消息过滤", Label: "屏蔽词",
			Help: "包含任一即整条忽略", Kind: KindStringList},
		{Key: "message.ban_regexes", Section: "消息过滤", Label: "屏蔽正则",
			Help: "一行一个，写错的正则会跳过", Kind: KindStringList},
		{Key: "message.group_whitelist", Section: "消息过滤", Label: "群白名单",
			Help: "留空表示不限制", Kind: KindStringList},

		// —— 长期记忆 ——
		// 总开关决定「工具是否注册 / 组件是否构造」，两者都只发生在启动时。
		{Key: "memory.enabled", Section: "长期记忆", Label: "总开关", Kind: KindBool, Restart: true},
		{Key: "memory.summary_enabled", Section: "长期记忆", Label: "生成聊天回想",
			Kind: KindBool},
		{Key: "memory.profile_enabled", Section: "长期记忆", Label: "生成人物画像",
			Kind: KindBool},
		{Key: "memory.summary_min_messages", Section: "长期记忆", Label: "触发摘要的最少条数",
			Kind: KindInt, Min: minf(1), Max: maxf(200)},
		{Key: "memory.query_default_limit", Section: "长期记忆", Label: "检索默认条数",
			Kind: KindInt, Min: minf(1), Max: maxf(50)},
		{Key: "memory.vector_min_score", Section: "长期记忆", Label: "向量相似度门槛",
			Help: "低于此分数的记忆不作为检索结果", Kind: KindFloat, Min: minf(0), Max: maxf(1)},
		{Key: "memory.vector_candidate_limit", Section: "长期记忆", Label: "向量候选上限",
			Help: "一次最多扫描多少条记忆", Kind: KindInt, Min: minf(10), Max: maxf(50000)},
		{Key: "memory.profile_max_persons", Section: "长期记忆", Label: "最多注入几份画像",
			Kind: KindInt, Min: minf(1), Max: maxf(20)},

		// —— 表情包 ——
		// 同 memory.enabled：send_emoji 工具只在启动时注册。
		{Key: "emoji.enabled", Section: "表情包", Label: "总开关", Kind: KindBool, Restart: true},
		{Key: "emoji.collect_enabled", Section: "表情包", Label: "自动收藏", Kind: KindBool},
		{Key: "emoji.max_count", Section: "表情包", Label: "库容量上限",
			Kind: KindInt, Min: minf(1), Max: maxf(100000)},
		{Key: "emoji.max_size_kb", Section: "表情包", Label: "单张大小上限（KB）",
			Kind: KindInt, Min: minf(16), Max: maxf(20480)},
		{Key: "emoji.send_probability", Section: "表情包", Label: "提示发表情的概率",
			Help: "0 表示不在提示词里提表情包", Kind: KindFloat, Min: minf(0), Max: maxf(1)},

		// —— 学习 ——
		// 这两个开关 learner 每次都读 cfg.Get()，改完下一轮学习就生效。
		{Key: "learning.expression_enabled", Section: "学习", Label: "表达学习",
			Kind: KindBool},
		{Key: "learning.jargon_enabled", Section: "学习", Label: "黑话学习",
			Kind: KindBool},
		// interval 只在 Start 时用于 time.NewTicker，改完必须重启。
		{Key: "learning.interval_minutes", Section: "学习", Label: "学习间隔（分钟）",
			Kind: KindInt, Min: minf(1), Max: maxf(1440), Restart: true},
		{Key: "learning.min_messages", Section: "学习", Label: "触发学习的最少新消息数",
			Kind: KindInt, Min: minf(1), Max: maxf(1000)},
		{Key: "learning.expression_max_used", Section: "学习", Label: "注入表达条数上限",
			Kind: KindInt, Min: minf(1), Max: maxf(50)},

		// —— 命令管理 ——
		// 对应 MaiBot 的「麦麦设置 → 命令管理」页。
		// 我们只有两条绑定命令（/assign_group_id、/assign_id），
		// 所以不做它那种「逐命令配允许的人/群」的矩阵，只保留管理员名单。
		// 留空表示不设限（单人群自用场景的默认值）。
		{Key: "qq.assign_admin_ids", Section: "命令", Label: "管理员 OpenID",
			Help: "留空表示任何人都能用绑定命令", Kind: KindStringList},

		// —— 运行 ——
		// log.level 标不标 Restart：能就地生效。
		// 机制是 logx.SetLevel（slog 的 LevelVar），由 web.applyLiveEffects 调用。
		{Key: "log.level", Section: "运行", Label: "终端日志级别",
			Kind: KindEnum, Options: []string{"DEBUG", "INFO", "WARN", "ERROR"}},
		// file_level 独立于 level：前者管写文件、后者管终端。
		// 默认配置就是 INFO 写终端、DEBUG 写文件，所以两级不能合成一个。
		{Key: "log.file_level", Section: "运行", Label: "文件日志级别",
			Help: "写 logs/oh-my-qqbot.log 的级别，可以比终端更详细", Kind: KindEnum,
			Options: []string{"DEBUG", "INFO", "WARN", "ERROR"}},
		{Key: "database.retention_days", Section: "运行", Label: "数据保留天数",
			Help: "0 表示永久保留", Kind: KindInt, Min: minf(0), Max: maxf(3650)},
		{Key: "web.port", Section: "运行", Label: "WebUI 端口",
			Kind: KindInt, Min: minf(1), Max: maxf(65535), Restart: true},
	}
}

// Snapshot 把当前配置摊平成「点分键 → 值」，供 WebUI 表单回填。
//
// 只输出 EditableFields 里列过的键：清单之外的字段（密钥、模型绑定）
// 不该出现在响应里，哪怕前端不显示也不行 —— 密钥泄露只需要一次大意。
func (c *Config) Snapshot() map[string]any {
	out := make(map[string]any, len(EditableFields()))
	for _, f := range EditableFields() {
		v, ok := c.fieldValue(f.Key)
		if !ok {
			continue
		}
		out[f.Key] = v
	}
	return out
}

// fieldValue 按点分键取一个字段的值。
//
// 手写 switch 而不是反射：清单里只有几十项，而且这样能在编译期
// 发现「清单里写了个不存在的键」——反射版只会安静地返回 nil，
// 表现是「表单上那一行永远是空的」。
func (c *Config) fieldValue(key string) (any, bool) {
	switch key {
	case "bot.nickname":
		return c.Bot.Nickname, true
	case "bot.alias_names":
		return append([]string{}, c.Bot.AliasNames...), true

	case "personality.personality":
		return c.Personality.Personality, true
	case "personality.behavior_style":
		return c.Personality.BehaviorStyle, true
	case "personality.reply_style":
		return c.Personality.ReplyStyle, true
	case "personality.multiple_reply_style":
		return append([]string{}, c.Personality.MultipleReplyStyle...), true
	case "personality.multiple_probability":
		return c.Personality.MultipleProbability, true

	case "chat.max_context_size":
		return c.Chat.MaxContextSize, true
	case "chat.max_private_context_size":
		return c.Chat.MaxPrivateContextSize, true
	case "chat.enable_reply_at":
		return c.Chat.EnableReplyAt, true
	case "chat.timing.debounce_mode":
		return c.Chat.Timing.DebounceMode, true
	case "chat.timing.talk_value":
		return c.Chat.Timing.TalkValue, true
	case "chat.timing.private_talk_value":
		return c.Chat.Timing.PrivateTalkValue, true
	case "chat.timing.reply_trigger_mode":
		return c.Chat.Timing.ReplyTriggerMode, true
	case "chat.timing.mentioned_bot_reply":
		return c.Chat.Timing.MentionedBotReply, true
	case "chat.timing.inevitable_at_reply":
		return c.Chat.Timing.InevitableAtReply, true
	case "chat.timing.planner_interrupt_max_consecutive_count":
		return c.Chat.Timing.PlannerInterruptMaxConsecutiveCount, true
	case "chat.timing.max_consecutive_wait_count":
		return c.Chat.Timing.MaxConsecutiveWaitCount, true
	case "chat.style.group_quote_frequency":
		return c.Chat.Style.GroupQuoteFrequency, true
	case "chat.style.private_quote_frequency":
		return c.Chat.Style.PrivateQuoteFrequency, true
	case "chat.style.group_chat_prompt":
		return c.Chat.Style.GroupChatPrompt, true
	case "chat.style.private_chat_prompt":
		return c.Chat.Style.PrivateChatPrompt, true

	case "response.enable_split":
		return c.Response.EnableSplit, true
	case "response.max_length":
		return c.Response.MaxLength, true
	case "response.max_sentence_num":
		return c.Response.MaxSentenceNum, true
	case "response.max_split_num":
		return c.Response.MaxSplitNum, true
	case "response.typing_speed":
		return c.Response.TypingSpeed, true

	case "message.ban_words":
		return c.Message.BanWords, true
	case "message.ban_regexes":
		return c.Message.BanRegexes, true
	case "message.group_whitelist":
		return c.Message.GroupWhitelist, true

	case "qq.assign_admin_ids":
		return append([]string{}, c.QQ.AssignAdminIDs...), true

	case "memory.enabled":
		return c.Memory.Enabled, true
	case "memory.summary_enabled":
		return c.Memory.SummaryEnabled, true
	case "memory.profile_enabled":
		return c.Memory.ProfileEnabled, true
	case "memory.summary_min_messages":
		return c.Memory.SummaryMinMessages, true
	case "memory.query_default_limit":
		return c.Memory.QueryDefaultLimit, true
	case "memory.vector_min_score":
		return c.Memory.VectorMinScore, true
	case "memory.vector_candidate_limit":
		return c.Memory.VectorCandidateLimit, true
	case "memory.profile_max_persons":
		return c.Memory.ProfileMaxPersons, true

	case "emoji.enabled":
		return c.Emoji.Enabled, true
	case "emoji.collect_enabled":
		return c.Emoji.CollectEnabled, true
	case "emoji.max_count":
		return c.Emoji.MaxCount, true
	case "emoji.max_size_kb":
		return c.Emoji.MaxSizeKB, true
	case "emoji.send_probability":
		return c.Emoji.SendProbability, true

	case "learning.expression_enabled":
		return c.Learning.ExpressionEnabled, true
	case "learning.jargon_enabled":
		return c.Learning.JargonEnabled, true
	case "learning.interval_minutes":
		return c.Learning.IntervalMinutes, true
	case "learning.min_messages":
		return c.Learning.MinMessages, true
	case "learning.expression_max_used":
		return c.Learning.ExpressionMaxUsed, true

	case "log.level":
		return c.Log.Level, true
	case "log.file_level":
		return c.Log.FileLevel, true
	case "database.retention_days":
		return c.Database.RetentionDays, true
	case "web.port":
		return c.Web.Port, true
	}
	return nil, false
}

// ApplyField 按点分键写入一个值。
//
// 类型必须匹配清单里声明的 Kind：前端传来的是 JSON，
// 数字一律是 float64、布尔是 bool、字符串是 string。
// 这里刻意做显式转换而不是「尽量兼容」——把 "true" 当 true 处理，
// 用户写错了一个字符就会得到沉默的错误行为。
//
// 写完立刻按清单里的 Min/Max/Options 校验。这一步不能省：
// 前端 <input min max> 只是浏览器提示，curl 一行就绕过了，
// 而 range 外的值会真的写进配置文件 —— web.port = -1 的后果是
// 下次启动 WebUI 起不来，用户完全不知道为什么。
func (c *Config) ApplyField(key string, raw any) error {
	if err := c.applyFieldRaw(key, raw); err != nil {
		return err
	}
	return c.checkFieldRange(key, raw)
}

// checkFieldRange 按清单校验取值范围与枚举。
//
// 单独一个函数（而不是写进每个 setXxx）是为了让它无法被忘记：
// 新增字段时只要在 EditableFields 里写好 Min/Max/Options，
// 这里自动生效，不需要改转换器。
func (c *Config) checkFieldRange(key string, raw any) error {
	var field *Field
	for _, f := range EditableFields() {
		if f.Key == key {
			ff := f
			field = &ff
			break
		}
	}
	if field == nil {
		// 清单里没有的键，applyFieldRaw 已经拒掉了，走到这里说明是内部不一致。
		return nil
	}

	if field.Kind == KindEnum {
		want, ok := raw.(string)
		if !ok {
			// 类型错误已在 applyFieldRaw 里报过，这里不重复报。
			return nil
		}
		for _, opt := range field.Options {
			if opt == want {
				return nil
			}
		}
		return fmt.Errorf("%s 只能取 %s，收到 %q", key,
			strings.Join(field.Options, " / "), want)
	}

	if field.Kind != KindInt && field.Kind != KindFloat {
		return nil
	}

	// 取写入后的实际值来比对，而不是 raw —— 这样 int/float 的
	// 各种输入形态都能统一处理，也不会因为 float64 精度丢了判定。
	actual := float64(0)
	if v, ok := c.fieldValue(key); ok {
		switch n := v.(type) {
		case int:
			actual = float64(n)
		case float64:
			actual = n
		}
	}

	if field.Min != nil && actual < *field.Min {
		return fmt.Errorf("%s 不能小于 %v，收到 %v", key, trimFloat(*field.Min), trimFloat(actual))
	}
	if field.Max != nil && actual > *field.Max {
		return fmt.Errorf("%s 不能大于 %v，收到 %v", key, trimFloat(*field.Max), trimFloat(actual))
	}
	return nil
}

// trimFloat 把 1.000000 这类值格式化成 1，错误信息更短。
func trimFloat(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// applyFieldRaw 是真正写字段的地方，范围校验在 ApplyField 里统一做。
func (c *Config) applyFieldRaw(key string, raw any) error {
	switch key {
	case "bot.nickname":
		return setString(&c.Bot.Nickname, raw, key)
	case "bot.alias_names":
		return setStringList(&c.Bot.AliasNames, raw, key)

	case "personality.personality":
		return setString(&c.Personality.Personality, raw, key)
	case "personality.behavior_style":
		return setString(&c.Personality.BehaviorStyle, raw, key)
	case "personality.reply_style":
		return setString(&c.Personality.ReplyStyle, raw, key)
	case "personality.multiple_reply_style":
		return setStringList(&c.Personality.MultipleReplyStyle, raw, key)
	case "personality.multiple_probability":
		return setFloat(&c.Personality.MultipleProbability, raw, key)

	case "chat.max_context_size":
		return setInt(&c.Chat.MaxContextSize, raw, key)
	case "chat.max_private_context_size":
		return setInt(&c.Chat.MaxPrivateContextSize, raw, key)
	case "chat.enable_reply_at":
		return setBool(&c.Chat.EnableReplyAt, raw, key)
	case "chat.timing.debounce_mode":
		return setString(&c.Chat.Timing.DebounceMode, raw, key)
	case "chat.timing.talk_value":
		return setFloat(&c.Chat.Timing.TalkValue, raw, key)
	case "chat.timing.private_talk_value":
		return setFloat(&c.Chat.Timing.PrivateTalkValue, raw, key)
	case "chat.timing.reply_trigger_mode":
		return setString(&c.Chat.Timing.ReplyTriggerMode, raw, key)
	case "chat.timing.mentioned_bot_reply":
		return setBool(&c.Chat.Timing.MentionedBotReply, raw, key)
	case "chat.timing.inevitable_at_reply":
		return setBool(&c.Chat.Timing.InevitableAtReply, raw, key)
	case "chat.timing.planner_interrupt_max_consecutive_count":
		return setInt(&c.Chat.Timing.PlannerInterruptMaxConsecutiveCount, raw, key)
	case "chat.timing.max_consecutive_wait_count":
		return setInt(&c.Chat.Timing.MaxConsecutiveWaitCount, raw, key)
	case "chat.style.group_quote_frequency":
		return setString(&c.Chat.Style.GroupQuoteFrequency, raw, key)
	case "chat.style.private_quote_frequency":
		return setString(&c.Chat.Style.PrivateQuoteFrequency, raw, key)
	case "chat.style.group_chat_prompt":
		return setString(&c.Chat.Style.GroupChatPrompt, raw, key)
	case "chat.style.private_chat_prompt":
		return setString(&c.Chat.Style.PrivateChatPrompt, raw, key)

	case "response.enable_split":
		return setBool(&c.Response.EnableSplit, raw, key)
	case "response.max_length":
		return setInt(&c.Response.MaxLength, raw, key)
	case "response.max_sentence_num":
		return setInt(&c.Response.MaxSentenceNum, raw, key)
	case "response.max_split_num":
		return setInt(&c.Response.MaxSplitNum, raw, key)
	case "response.typing_speed":
		return setFloat(&c.Response.TypingSpeed, raw, key)

	case "message.ban_words":
		return setStringList(&c.Message.BanWords, raw, key)
	case "message.ban_regexes":
		return setStringList(&c.Message.BanRegexes, raw, key)
	case "message.group_whitelist":
		return setStringList(&c.Message.GroupWhitelist, raw, key)

	case "qq.assign_admin_ids":
		return setStringList(&c.QQ.AssignAdminIDs, raw, key)

	case "memory.enabled":
		return setBool(&c.Memory.Enabled, raw, key)
	case "memory.summary_enabled":
		return setBool(&c.Memory.SummaryEnabled, raw, key)
	case "memory.profile_enabled":
		return setBool(&c.Memory.ProfileEnabled, raw, key)
	case "memory.summary_min_messages":
		return setInt(&c.Memory.SummaryMinMessages, raw, key)
	case "memory.query_default_limit":
		return setInt(&c.Memory.QueryDefaultLimit, raw, key)
	case "memory.vector_min_score":
		return setFloat(&c.Memory.VectorMinScore, raw, key)
	case "memory.vector_candidate_limit":
		return setInt(&c.Memory.VectorCandidateLimit, raw, key)
	case "memory.profile_max_persons":
		return setInt(&c.Memory.ProfileMaxPersons, raw, key)

	case "emoji.enabled":
		return setBool(&c.Emoji.Enabled, raw, key)
	case "emoji.collect_enabled":
		return setBool(&c.Emoji.CollectEnabled, raw, key)
	case "emoji.max_count":
		return setInt(&c.Emoji.MaxCount, raw, key)
	case "emoji.max_size_kb":
		return setInt(&c.Emoji.MaxSizeKB, raw, key)
	case "emoji.send_probability":
		return setFloat(&c.Emoji.SendProbability, raw, key)

	case "learning.expression_enabled":
		return setBool(&c.Learning.ExpressionEnabled, raw, key)
	case "learning.jargon_enabled":
		return setBool(&c.Learning.JargonEnabled, raw, key)
	case "learning.interval_minutes":
		return setInt(&c.Learning.IntervalMinutes, raw, key)
	case "learning.min_messages":
		return setInt(&c.Learning.MinMessages, raw, key)
	case "learning.expression_max_used":
		return setInt(&c.Learning.ExpressionMaxUsed, raw, key)

	case "log.level":
		return setString(&c.Log.Level, raw, key)
	case "log.file_level":
		return setString(&c.Log.FileLevel, raw, key)
	case "database.retention_days":
		return setInt(&c.Database.RetentionDays, raw, key)
	case "web.port":
		return setInt(&c.Web.Port, raw, key)
	}
	return fmt.Errorf("不支持的配置项 %q", key)
}
