// Package config 负责加载、校验与热重载 TOML 配置。
// 对应 MaiBot 的 src/config，但只保留精简版需要的配置节。
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// Config 是 bot_config.toml 的根结构。
type Config struct {
	Bot         BotConfig         `toml:"bot"`
	Personality PersonalityConfig `toml:"personality"`
	Chat        ChatConfig        `toml:"chat"`
	LLM         LLMConfig         `toml:"llm"`
	QQ          QQConfig          `toml:"qq"`
	Message     MessageConfig     `toml:"message"`
	Response    ResponseConfig    `toml:"response"`
	Log         LogConfig         `toml:"log"`
	Web         WebConfig         `toml:"web"`
	Database    DatabaseConfig    `toml:"database"`
	Memory      MemoryConfig      `toml:"memory"`
	Emoji       EmojiConfig       `toml:"emoji"`
	Learning    LearningConfig    `toml:"learning"`
	Keyword     KeywordConfig     `toml:"keyword"`
}

// MemoryConfig 是长期记忆配置。
type MemoryConfig struct {
	// Enabled 总开关。关闭后不摘要、不注入画像、不注册 query_memory。
	Enabled bool `toml:"enabled"`
	// SummaryEnabled 控制历史裁切时是否生成聊天回想。
	SummaryEnabled bool `toml:"summary_enabled"`
	// ProfileEnabled 控制是否自动注入人物画像。
	ProfileEnabled bool `toml:"profile_enabled"`
	// SummaryMinMessages 是触发摘要所需的最少被裁切消息数。
	// 太少不值得花一次模型调用。
	SummaryMinMessages int `toml:"summary_min_messages"`
	// SummaryMaxInputChars 是摘要请求的输入上限，超出部分截断。
	// 对应参考实现的 MAX_SUMMARY_INPUT_CHARS = 16000。
	SummaryMaxInputChars int `toml:"summary_max_input_chars"`
	// QueryDefaultLimit 是 query_memory 的默认返回条数。
	QueryDefaultLimit int `toml:"query_default_limit"`
	// QueryMaxLimit 是 query_memory 允许的上限，防止模型一键拉全库。
	QueryMaxLimit int `toml:"query_max_limit"`
	// ProfileMaxChars 是单份画像注入的最大字数。
	// 对应参考实现的 PROFILE_TEXT_MAX_CHARS = 900。
	ProfileMaxChars int `toml:"profile_max_chars"`
	// ProfileMaxPersons 是一次最多注入几个人物的画像。
	ProfileMaxPersons int `toml:"profile_max_persons"`
	// ProfileUpdateIntervalMessages 是同一人积累多少条消息后更新一次画像。
	ProfileUpdateIntervalMessages int `toml:"profile_update_interval_messages"`

	// VectorCandidateLimit 是向量检索时一次载入多少条候选记忆。
	//
	// SQLite 没有向量索引，只能全表扫后逐个算余弦。这个值决定了
	// 「最多扫多少条」，直接影响检索延迟。1000 条约 4MB 内存、几毫秒 CPU，
	// 够用很久；真的撑不住了再考虑引入 sqlite-vec 之类的扩展。
	VectorCandidateLimit int `toml:"vector_candidate_limit"`
	// VectorMinScore 是余弦相似度的最低门槛。
	//
	// 没有门槛的后果：问一句完全无关的话，也会因为「总得返回点什么」
	// 把分数 0.1 的记忆塞进上下文，反而干扰模型判断。
	// 0.35 来自实测：真正相关的记忆一般 0.45~0.65，
	// 无关的落在 0.1~0.3。
	VectorMinScore float64 `toml:"vector_min_score"`
}

// EmojiConfig 是表情包配置。
type EmojiConfig struct {
	// Enabled 总开关。
	Enabled bool `toml:"enabled"`
	// CollectEnabled 是否自动收藏别人发的表情包。
	CollectEnabled bool `toml:"collect_enabled"`
	// MaxSizeKB 是单个表情包的大小上限，超出不收藏。
	MaxSizeKB int `toml:"max_size_kb"`
	// MaxCount 是表情包库容量上限，满了不再收藏新的。
	MaxCount int `toml:"max_count"`
	// Dir 是表情包文件存放目录（相对工作目录）。
	Dir string `toml:"dir"`
	// SendProbability 是 Planner 被提示可以发表情包的概率，0 表示不提示。
	SendProbability float64 `toml:"send_probability"`
}

// LearningConfig 是表达学习与黑话学习配置。
type LearningConfig struct {
	// ExpressionEnabled 开启表达学习（从群聊里学说话方式）。
	ExpressionEnabled bool `toml:"expression_enabled"`
	// JargonEnabled 开启黑话学习。
	JargonEnabled bool `toml:"jargon_enabled"`
	// IntervalMinutes 是学习任务的触发间隔。
	// 学习要花模型调用，别设得太勤。
	IntervalMinutes int `toml:"interval_minutes"`
	// MinMessages 是触发一次学习所需的最少新消息数。
	MinMessages int `toml:"min_messages"`
	// ExpressionMaxUsed 是注入给 Replyer 的表达条数上限。
	ExpressionMaxUsed int `toml:"expression_max_used"`
}

// KeywordRule 是一条关键词反应规则。
// 字段与 MaiBot KeywordRuleConfig 对齐。
type KeywordRule struct {
	// Keywords 命中任意一个即触发。
	Keywords []string `toml:"keywords"`
	// Regex 命中任意一个即触发。compile 失败在 Validate 里报错。
	Regex []string `toml:"regex"`
	// Reaction 是命中后追加给 Replyer 的提示，不是直接发送的内容。
	Reaction string `toml:"reaction"`
}

// KeywordConfig 是关键词反应配置。
type KeywordConfig struct {
	// Rules 是全部规则。
	Rules []KeywordRule `toml:"rules"`
}

// BotConfig 是机器人的身份配置。
type BotConfig struct {
	// Platform 固定为 qq，保留字段以便未来扩展。
	Platform string `toml:"platform"`
	// AccountID 是机器人自己的 openid，由网关 READY 事件回填。
	AccountID string `toml:"account_id"`
	// Nickname 是机器人在群里显示和自称时使用的名字。
	// 默认值只是「开箱即用」的随手一个名字，用户改成任何名字都不应影响程序行为。
	Nickname string `toml:"nickname"`
	// AliasNames 是别人可能用来称呼机器人的名字，用于辅助识别提及。
	AliasNames []string `toml:"alias_names"`
}

// PersonalityConfig 是人格配置。
type PersonalityConfig struct {
	// Personality 是机器人的身份设定。
	Personality string `toml:"personality"`
	// BehaviorStyle 是 Planner 使用的行动准则。
	BehaviorStyle string `toml:"behavior_style"`
	// ReplyStyle 是平时说话的风格。
	ReplyStyle string `toml:"reply_style"`
	// MultipleReplyStyle 是备用表达风格，触发后只影响本次回复。
	MultipleReplyStyle []string `toml:"multiple_reply_style"`
	// MultipleProbability 是随机启用备用风格的概率。
	MultipleProbability float64 `toml:"multiple_probability"`
}

// ChatConfig 是聊天行为配置。
type ChatConfig struct {
	// MaxContextSize 是群聊回复时参考的最近消息数量。
	MaxContextSize int `toml:"max_context_size"`
	// MaxPrivateContextSize 是私聊上下文数量。
	MaxPrivateContextSize int `toml:"max_private_context_size"`
	// EnableReplyAt 允许回复时 @ 用户。
	EnableReplyAt bool `toml:"enable_reply_at"`
	// Timing 是回复时机配置。
	Timing ReplyTimingConfig `toml:"timing"`
	// Style 是回复方式配置。
	Style ReplyStyleConfig `toml:"style"`
}

// ReplyTimingConfig 是「什么时候回复」的配置。
// 字段与 MaiBot ChatReplyTimingConfig 对齐。
type ReplyTimingConfig struct {
	// DebounceMode 是消息防抖档位：off / fast / natural / steady。
	DebounceMode string `toml:"debounce_mode"`

	// TalkValue 是群聊发言频率，0 更安静，1 按正常频率。
	TalkValue float64 `toml:"talk_value"`
	// PrivateTalkValue 是私聊发言频率。
	PrivateTalkValue float64 `toml:"private_talk_value"`

	// MentionedBotReply 开启后提到机器人名字更容易回复。
	MentionedBotReply bool `toml:"mentioned_bot_reply"`
	// InevitableAtReply 开启后被 @ 时尽量回复。
	InevitableAtReply bool `toml:"inevitable_at_reply"`

	// ReplyTriggerMode 是回复触发模式：frequency / dynamic。
	ReplyTriggerMode string `toml:"reply_trigger_mode"`

	// PlannerInterruptMaxConsecutiveCount 是思考时来新消息最多重新思考多少次。
	PlannerInterruptMaxConsecutiveCount int `toml:"planner_interrupt_max_consecutive_count"`

	// MaxConsecutiveWaitCount 是 Planner 最多连续调用 wait 多少次。
	MaxConsecutiveWaitCount int `toml:"max_consecutive_wait_count"`

	// 空闲退避。
	NoActionBackoffBaseSeconds   float64 `toml:"no_action_backoff_base_seconds"`
	NoActionBackoffCapSeconds    float64 `toml:"no_action_backoff_cap_seconds"`
	NoActionBackoffStartCount    int     `toml:"no_action_backoff_start_count"`
	NoActionBackoffBypassPending int     `toml:"no_action_backoff_bypass_pending_count"`
}

// ReplyStyleConfig 是「怎么回复」的配置。
type ReplyStyleConfig struct {
	// GroupQuoteFrequency 是群聊引用频率：never / occasionally / often。
	GroupQuoteFrequency string `toml:"group_quote_frequency"`
	// PrivateQuoteFrequency 是私聊引用频率。
	PrivateQuoteFrequency string `toml:"private_quote_frequency"`

	// GroupChatPrompt 是群聊通用提示词。
	GroupChatPrompt string `toml:"group_chat_prompt"`
	// PrivateChatPrompt 是私聊通用提示词。
	PrivateChatPrompt string `toml:"private_chat_prompt"`
}

// LLMConfig 是模型配置。
type LLMConfig struct {
	// Providers 是 API 服务商列表。
	Providers []ProviderConfig `toml:"providers"`
	// Models 是模型列表。
	Models []ModelConfig `toml:"models"`
	// Tasks 是各任务使用的模型绑定。
	Tasks TasksConfig `toml:"tasks"`
}

// ProviderConfig 是一个 OpenAI 兼容服务商。
type ProviderConfig struct {
	Name    string `toml:"name"`
	BaseURL string `toml:"base_url"`
	APIKey  string `toml:"api_key"`

	// AuthType 是鉴权方式：bearer / header / query / none。
	AuthType string `toml:"auth_type"`
	// AuthHeaderName 在 auth_type=header 时使用。
	AuthHeaderName string `toml:"auth_header_name"`
	// AuthHeaderPrefix 在 auth_type=header 时使用。
	AuthHeaderPrefix string `toml:"auth_header_prefix"`
	// AuthQueryName 在 auth_type=query 时使用。
	AuthQueryName string `toml:"auth_query_name"`

	// DefaultHeaders 是所有请求默认附带的 HTTP Header。
	DefaultHeaders map[string]string `toml:"default_headers"`
	// DefaultQuery 是所有请求默认附带的查询参数。
	DefaultQuery map[string]string `toml:"default_query"`

	// Timeout 是单次请求超时秒数。
	Timeout int `toml:"timeout"`
	// MaxRetry 是最大尝试次数（含首次）。
	MaxRetry int `toml:"max_retry"`
	// RetryInterval 是重试间隔秒数。
	RetryInterval int `toml:"retry_interval"`
}

// ModelConfig 是一个模型。
type ModelConfig struct {
	Name string `toml:"name"`
	// ModelIdentifier 是服务商提供的模型标识符。
	ModelIdentifier string `toml:"model_identifier"`
	// Provider 对应 ProviderConfig.Name。
	Provider string `toml:"provider"`
	// Visual 标记是否为多模态模型。
	Visual bool `toml:"visual"`
	// ForceStream 强制流式输出。
	ForceStream bool `toml:"force_stream"`
	// Temperature 覆盖任务级温度。
	Temperature *float64 `toml:"temperature"`
	// MaxTokens 覆盖任务级最大 token。
	MaxTokens *int `toml:"max_tokens"`
	// Dimension 是 embedding 模型的向量维度。
	// 只有 embedding 任务用得到；留空表示由服务端决定。
	// 显式指定会在请求里带上 dimensions 字段 ——
	// 部分端点不认这个字段，所以默认不发。
	Dimension *int `toml:"dimension"`
	// PriceIn / PriceOut 用于花费统计，单位元 / M token。
	PriceIn  float64 `toml:"price_in"`
	PriceOut float64 `toml:"price_out"`
}

// TaskModelConfig 是单个任务的模型绑定。
type TaskModelConfig struct {
	// Models 是候选模型名列表，为空时按回退规则取。
	Models []string `toml:"models"`
	// MaxTokens 是该任务最大输出 token。
	MaxTokens int `toml:"max_tokens"`
	// Temperature 是该任务温度。
	Temperature float64 `toml:"temperature"`
	// SelectionStrategy 是模型选择策略：balance / random / sequential。
	SelectionStrategy string `toml:"selection_strategy"`
	// HardTimeout 是任务硬超时秒数。
	HardTimeout float64 `toml:"hard_timeout"`
}

// TasksConfig 是各任务配置。
type TasksConfig struct {
	Planner   TaskModelConfig `toml:"planner"`
	Replyer   TaskModelConfig `toml:"replyer"`
	Utils     TaskModelConfig `toml:"utils"`
	Learner   TaskModelConfig `toml:"learner"`
	VLM       TaskModelConfig `toml:"vlm"`
	Embedding TaskModelConfig `toml:"embedding"`
}

// QQConfig 是 QQ 官方机器人适配配置。
type QQConfig struct {
	// AppID 是 QQ 开放平台的 AppID。
	AppID string `toml:"app_id"`
	// AppSecret 是 AppSecret。
	AppSecret string `toml:"app_secret"`
	// APIBaseURL 是 OpenAPI 地址。
	APIBaseURL string `toml:"api_base_url"`
	// ReconnectDelaySec 是断线重连间隔秒数。
	ReconnectDelaySec float64 `toml:"reconnect_delay_sec"`
	// Intents 是订阅的网关事件位图。
	Intents int `toml:"intents"`
	// UnifiedAccountID 是统一账号 ID，非空时聊天流按此归属。
	UnifiedAccountID string `toml:"unified_account_id"`
	// AssignAdminIDs 是允许执行绑定命令的用户。
	AssignAdminIDs []string `toml:"assign_admin_ids"`
}

// MessageConfig 是消息接收过滤配置。
type MessageConfig struct {
	// BanWords 是包含即过滤的词。
	BanWords []string `toml:"ban_words"`
	// BanRegexes 是用正则过滤的模式。
	BanRegexes []string `toml:"ban_regexes"`
	// GroupWhitelist 是允许响应的群，空表示不限制。
	GroupWhitelist []string `toml:"group_whitelist"`
}

// ResponseConfig 是回复后处理配置。
type ResponseConfig struct {
	// EnableSplit 允许把过长回复拆成多条。
	EnableSplit bool `toml:"enable_split"`
	// MaxLength 是单条最大长度。
	MaxLength int `toml:"max_length"`
	// MaxSentenceNum 是单条最大句数。
	MaxSentenceNum int `toml:"max_sentence_num"`
	// MaxSplitNum 是一次回复最多拆成几条。
	MaxSplitNum int `toml:"max_split_num"`

	// TypingSpeed 是模拟打字速度，0 最快，1 默认，2 更慢。
	TypingSpeed float64 `toml:"typing_speed"`
}

// LogConfig 是日志配置。
type LogConfig struct {
	Level      string `toml:"level"`
	FileLevel  string `toml:"file_level"`
	Dir        string `toml:"dir"`
	MaxSizeMB  int    `toml:"max_size_mb"`
	MaxBackups int    `toml:"max_backups"`
	Console    bool   `toml:"console"`
	Color      bool   `toml:"color"`
}

// WebConfig 是 WebUI 配置。
type WebConfig struct {
	Enabled bool   `toml:"enabled"`
	Host    string `toml:"host"`
	Port    int    `toml:"port"`
	// Password 是登录密码，明文存配置即可。
	Password string `toml:"password"`
	// SessionSecret 是签名 Cookie 用的密钥，留空则自动生成。
	SessionSecret string `toml:"session_secret"`
}

// DatabaseConfig 是数据库配置。
type DatabaseConfig struct {
	Path string `toml:"path"`
	// RetentionDays 是历史数据的保留天数，<=0 表示永久保留。
	//
	// 管的是两类只增不减的表：
	//   - reasoning_records（每轮 Planner 请求与响应，单条可达 64KB）
	//   - messages（聊天记录本体）
	// 这两张表不清理的话，一个活跃群跑几个月就能攒到几个 GB，
	// 而检索与恢复上下文都会因此变慢。
	RetentionDays int `toml:"retention_days"`
}

// Default 返回带默认值的配置。
// 默认值与 MaiBot 的官方配置保持一致。
func Default() *Config {
	return &Config{
		Bot: BotConfig{
			Platform: "qq",
			Nickname: "木木",
		},
		Personality: PersonalityConfig{
			Personality:   "是女大学生，现在正在上网和群友聊天。一般不包容且善良，但有时候有点一反常态，表现出锐利攻击性。善于用人类的角度思考问题，聊天比较日常。",
			BehaviorStyle: "是大二女大学生，现在正在上网和群友聊天。善于用人类的角度思考问题，聊天偏日常。不会没话题硬找话题，具有足够的自主性，会自行搜集信息理解内容。",
			ReplyStyle:    "你的风格平淡简短，可以参考贴吧的回复风格。不滥用比喻或者生硬句子。视情况省略主语或者进行倒装，风格较为随意。",
			MultipleReplyStyle: []string{
				"你的风格平淡但不失讽刺，很简短,很白话。可以参考贴吧，微博的回复风格。",
				"用1-2个字进行回复",
				"用1-2个符号进行回复",
				"带点翻译腔，但不要太长",
			},
			MultipleProbability: 0,
		},
		Chat: ChatConfig{
			MaxContextSize:        40,
			MaxPrivateContextSize: 60,
			EnableReplyAt:         true,
			Timing: ReplyTimingConfig{
				DebounceMode:                        "natural",
				TalkValue:                           1,
				PrivateTalkValue:                    1,
				MentionedBotReply:                   false,
				InevitableAtReply:                   true,
				ReplyTriggerMode:                    "frequency",
				PlannerInterruptMaxConsecutiveCount: 0,
				MaxConsecutiveWaitCount:             3,
				NoActionBackoffBaseSeconds:          15,
				NoActionBackoffCapSeconds:           300,
				NoActionBackoffStartCount:           2,
				NoActionBackoffBypassPending:        6,
			},
			Style: ReplyStyleConfig{
				GroupQuoteFrequency:   "occasionally",
				PrivateQuoteFrequency: "occasionally",
				GroupChatPrompt: "下面是聊天中的的内容，聊天中包含文字，图片和表情包等消息。\n" +
					"回复尽量简短一些。最好一次对一个话题进行回复，但必须考虑不同群友发言之间的交互，免得啰嗦或者回复内容太乱。\n" +
					"根据聊天内容自由发挥，但是要日常不浮夸，不要刻意找话题。\n" +
					"不用刻意回复其他人发送的表情包。你可以适当发送表情包表达情绪。如果有人不喜欢你或者不理你，就不要强行回复，回复前读空气。不要每个人的消息都回复，适当回复其他话题。可以适当回过头回复之前用户的消息。\n",
				PrivateChatPrompt: "你正在聊天，下面是正在聊的内容，其中包含聊天记录和聊天中的图片。\n" +
					"回复尽量简短一些。请注意把握聊天内容。\n" +
					"请考虑对方的发言频率，想法，思考自己何时回复以及回复内容。\n",
			},
		},
		LLM: LLMConfig{
			Tasks: TasksConfig{
				Planner:   TaskModelConfig{MaxTokens: 4096, Temperature: 0.7, SelectionStrategy: "balance", HardTimeout: 240},
				Replyer:   TaskModelConfig{MaxTokens: 4096, Temperature: 0.7, SelectionStrategy: "balance", HardTimeout: 240},
				Utils:     TaskModelConfig{MaxTokens: 4096, Temperature: 0.7, SelectionStrategy: "balance", HardTimeout: 240},
				Learner:   TaskModelConfig{MaxTokens: 4096, Temperature: 0.7, SelectionStrategy: "balance", HardTimeout: 240},
				VLM:       TaskModelConfig{MaxTokens: 4096, Temperature: 0.7, SelectionStrategy: "balance", HardTimeout: 240},
				Embedding: TaskModelConfig{MaxTokens: 4096, Temperature: 0.7, SelectionStrategy: "balance", HardTimeout: 240},
			},
		},
		QQ: QQConfig{
			APIBaseURL:        "https://api.bot.qq.com",
			ReconnectDelaySec: 5,
			// 1 << 25 = 群与私聊消息事件。
			Intents: 1 << 25,
		},
		Response: ResponseConfig{
			EnableSplit:    true,
			MaxLength:      512,
			MaxSentenceNum: 8,
			MaxSplitNum:    3,
			TypingSpeed:    1,
		},
		Log: LogConfig{
			Level:      "INFO",
			FileLevel:  "DEBUG",
			Dir:        "logs",
			MaxSizeMB:  16,
			MaxBackups: 5,
			Console:    true,
			Color:      true,
		},
		Web: WebConfig{
			Enabled:  true,
			Host:     "127.0.0.1",
			Port:     7800,
			Password: "password",
		},
		Database: DatabaseConfig{
			Path: "data/oh-my-qqbot.db",
			// 默认 90 天：够回看一个季度，也不至于把盘塞满。
			RetentionDays: 90,
		},
		Memory: MemoryConfig{
			Enabled:                       true,
			SummaryEnabled:                true,
			ProfileEnabled:                true,
			SummaryMinMessages:            8,
			SummaryMaxInputChars:          16000,
			QueryDefaultLimit:             5,
			QueryMaxLimit:                 20,
			ProfileMaxChars:               900,
			ProfileMaxPersons:             4,
			ProfileUpdateIntervalMessages: 20,
			VectorCandidateLimit:          1000,
			VectorMinScore:                0.35,
		},
		Emoji: EmojiConfig{
			Enabled:         true,
			CollectEnabled:  true,
			MaxSizeKB:       1024,
			MaxCount:        500,
			Dir:             "data/emoji",
			SendProbability: 0,
		},
		Learning: LearningConfig{
			// 默认关闭：学习要花模型调用，且效果需要人工观察后再打开。
			ExpressionEnabled: false,
			JargonEnabled:     false,
			IntervalMinutes:   30,
			MinMessages:       30,
			ExpressionMaxUsed: 5,
		},
	}
}

// Load 从路径读取配置，缺省字段用默认值填充。
func Load(path string) (*Config, error) {
	cfg := Default()

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置 %s 失败: %w", path, err)
	}
	if err := toml.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("解析配置 %s 失败: %w", path, err)
	}
	cfg.applyEnv()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// LoadOrCreate 在配置不存在时写入一份默认配置。
func LoadOrCreate(path string) (*Config, bool, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		cfg := Default()
		if err := Save(cfg, path); err != nil {
			return nil, false, err
		}
		return cfg, true, nil
	}
	cfg, err := Load(path)
	return cfg, false, err
}

// Save 把配置写回 TOML。
func Save(cfg *Config, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := toml.Marshal(cfg)
	if err != nil {
		return err
	}
	header := []byte("# Oh-My-QQBot 配置文件\n# 修改后重启生效\n\n")
	return os.WriteFile(path, append(header, raw...), 0o644)
}

// applyEnv 允许用环境变量覆盖敏感项。
func (c *Config) applyEnv() {
	if v := os.Getenv("OMQ_QQ_APP_ID"); v != "" {
		c.QQ.AppID = v
	}
	if v := os.Getenv("OMQ_QQ_APP_SECRET"); v != "" {
		c.QQ.AppSecret = v
	}
	if v := os.Getenv("OMQ_WEB_PASSWORD"); v != "" {
		c.Web.Password = v
	}
}

// Validate 校验配置的合法性。
//
// 注意：必须先把「写文件后重新加载」这条链路打通。
// Save 写出的默认配置里 QQ 凭据是空的，如果 Validate 在这里硬性拒绝，
// LoadOrCreate 首次启动能成（因为它不 Validate 自己刚写的那份），
// 第二次启动却会直接报「qq.app_id 不能为空」而启不了。
// 所以凭据缺失只当警告，不阻断启动 —— 没有凭据时机器人连不上 QQ，
// 但它能跑起来并告诉用户问题在哪，比直接崩掉好用。
func (c *Config) Validate() error {
	if c.Bot.Nickname == "" {
		return fmt.Errorf("bot.nickname 不能为空")
	}
	if c.QQ.APIBaseURL == "" {
		c.QQ.APIBaseURL = "https://api.bot.qq.com"
	}
	if len(c.LLM.Providers) > 0 {
		for i, p := range c.LLM.Providers {
			if p.Name == "" {
				return fmt.Errorf("llm.providers[%d].name 不能为空", i)
			}
			if p.BaseURL == "" {
				return fmt.Errorf("llm.providers[%d].base_url 不能为空", i)
			}
		}
	}

	// 模型配置不在这里强制校验。
	//
	// 原因：没有模型时程序仍应能启动并连上 QQ，用于验证凭据与网关是否可用；
	// 真正的「没有可用模型」错误在 llm.Generate 里返回，那时才是有意义的失败点。
	// 启动时若检测到模型缺失会打印警告，提示补齐配置。
	if c.Web.Enabled && c.Web.Password == "" {
		return fmt.Errorf("web.password 不能为空")
	}
	if c.QQ.ReconnectDelaySec <= 0 {
		c.QQ.ReconnectDelaySec = 5
	}
	if c.QQ.Intents == 0 {
		c.QQ.Intents = 1 << 25
	}
	if c.Response.MaxSplitNum < 1 {
		c.Response.MaxSplitNum = 3
	}

	// 关键词规则的正则在启动时编译一次，配错了当场报错而不是等到命中才炸。
	for i, rule := range c.Keyword.Rules {
		if len(rule.Keywords) == 0 && len(rule.Regex) == 0 {
			return fmt.Errorf("keyword.rules[%d] 至少需要 keywords 或 regex 之一", i)
		}
		if strings.TrimSpace(rule.Reaction) == "" {
			return fmt.Errorf("keyword.rules[%d].reaction 不能为空", i)
		}
		for _, pattern := range rule.Regex {
			if _, err := regexp.Compile(pattern); err != nil {
				return fmt.Errorf("keyword.rules[%d] 的正则 %q 无效: %w", i, pattern, err)
			}
		}
	}

	if c.Memory.Enabled {
		if c.Memory.QueryDefaultLimit < 1 {
			c.Memory.QueryDefaultLimit = 5
		}
		if c.Memory.QueryMaxLimit < c.Memory.QueryDefaultLimit {
			c.Memory.QueryMaxLimit = c.Memory.QueryDefaultLimit
		}
		if c.Memory.SummaryMaxInputChars < 1000 {
			c.Memory.SummaryMaxInputChars = 16000
		}
		if c.Memory.ProfileMaxChars < 100 {
			c.Memory.ProfileMaxChars = 900
		}
		if c.Memory.VectorCandidateLimit < 10 {
			c.Memory.VectorCandidateLimit = 1000
		}
		if c.Memory.VectorMinScore <= 0 || c.Memory.VectorMinScore >= 1 {
			c.Memory.VectorMinScore = 0.35
		}
	}

	if c.Emoji.Enabled {
		if c.Emoji.MaxSizeKB < 16 {
			c.Emoji.MaxSizeKB = 1024
		}
		if c.Emoji.MaxCount < 1 {
			c.Emoji.MaxCount = 500
		}
		if c.Emoji.Dir == "" {
			c.Emoji.Dir = "data/emoji"
		}
	}

	if c.Learning.IntervalMinutes < 1 {
		c.Learning.IntervalMinutes = 30
	}
	if c.Learning.MinMessages < 1 {
		c.Learning.MinMessages = 30
	}
	if c.Learning.ExpressionMaxUsed < 1 {
		c.Learning.ExpressionMaxUsed = 5
	}

	return nil
}

// MissingCredentials 返回缺失的 QQ 凭据名，用于启动时打印警告。
//
// 单独提供而不是在 Validate 里报错：凭据缺失不影响程序启动与 WebUI 使用，
// 只是连不上 QQ。让调用方自己决定要多大声音。
func (c *Config) MissingCredentials() []string {
	var missing []string
	if c.QQ.AppID == "" {
		missing = append(missing, "qq.app_id")
	}
	if c.QQ.AppSecret == "" {
		missing = append(missing, "qq.app_secret")
	}
	return missing
}

// SessionContextSize 返回指定聊天类型的上下文条数。
func (c *Config) SessionContextSize(isGroup bool) int {
	if isGroup {
		return c.Chat.MaxContextSize
	}
	return c.Chat.MaxPrivateContextSize
}

// TalkValue 返回指定聊天类型的发言频率。
func (c *Config) TalkValue(isGroup bool) float64 {
	if isGroup {
		return c.Chat.Timing.TalkValue
	}
	return c.Chat.Timing.PrivateTalkValue
}

// TimeoutDuration 返回服务商的请求超时。
//
// 返回 0 表示不设超时。以前 <=0 时兜底成 120 秒，
// 于是「不写 timeout」和「写 120」变成同一件事，
// 用户没法表达「这个服务商就是很慢，别掐我」。
// 真正的兜底放在调用方（llm 层取任务硬超时与服务商超时里较小的一个），
// 两边都表达 0 时才真的不限时。
func (p *ProviderConfig) TimeoutDuration() time.Duration {
	if p.Timeout <= 0 {
		return 0
	}
	return time.Duration(p.Timeout) * time.Second
}

// Retry 返回最大尝试次数。
func (p *ProviderConfig) Retry() int {
	if p.MaxRetry <= 0 {
		return 1
	}
	return p.MaxRetry
}

// RetryIntervalDuration 返回重试间隔。
func (p *ProviderConfig) RetryIntervalDuration() time.Duration {
	if p.RetryInterval <= 0 {
		return 4 * time.Second
	}
	return time.Duration(p.RetryInterval) * time.Second
}

// BuildAuthHeaderValue 构造鉴权请求头的值。
//
// 语义逐字对齐参考实现 refs/MaiBot/src/llm_models/openai_compat.py:63-76
// 的 _build_auth_header_value：prefix 去掉首尾空白后为空 → 只返回 key
// （留空表示直接发送原始密钥）；否则「prefix + 单个空格 + key」。
//
// 必须两层共用这一个函数：以前 llm 层无条件补一个空格、
// embedding 层不补空格，同一个 provider 配置 auth_header_prefix = "Bearer "
// （带尾空格）时，两条链路发出的头必有一条 401。
func BuildAuthHeaderValue(prefix, key string) string {
	normalized := strings.TrimSpace(prefix)
	if normalized == "" {
		return key
	}
	return normalized + " " + key
}

// ModelIsVisual 判断某个任务的首选模型是否支持图片输入。
//
// 用途：决定要不要在请求里挂 image_url。给纯文本模型发图片会被服务端 400，
// 所以这里宁可保守 —— 没配置或查不到就当作不支持。
func (c *Config) ModelIsVisual(task TaskName) bool {
	names := c.ResolveModels(task)
	if len(names) == 0 {
		return false
	}
	m, err := c.Model(names[0])
	if err != nil {
		return false
	}
	return m.Visual
}
