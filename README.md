# Oh-My-QQBot

用 Go 写的一个精简版 MaiBot。配合 QQ 官方机器人，做群聊里的拟人 Agent。

## 特性

- **会决定要不要开口**：不是每条消息都回。三层决策 —— 消息防抖、触发门控、空闲退避
- **Planner / Replyer 两段式**：想用推理模型，说用文风模型，提示词彻底独立
- **单二进制部署**：`go build` 出一个 exe，带 WebUI，无运行时依赖
- **纯 Go SQLite**：无 cgo，交叉编译零痛苦
- **WebUI 自研**：极简设计，看会话、发消息、读日志、查统计

## 快速开始

```bash
# 1. 生成配置
go run ./cmd/oh-my-qqbot -init

# 2. 编辑 config.toml，填三处：
#    [qq]  app_id / app_secret
#    [llm] providers 和 models
#    [llm.tasks.planner] models = ['你的模型名']

# 3. 启动
go run ./cmd/oh-my-qqbot
```

打开 http://127.0.0.1:7800 ，默认密码 `password`（配置里可改，建议第一次登录就改掉）。

## 配置要点

### QQ 官方机器人

在 [QQ 开放平台](https://q.qq.com/) 创建机器人，拿 AppID 和 AppSecret。
需要开通**群聊全量消息**权限（`GROUP_MESSAGE_CREATE`），否则只能收到 @ 机器人的消息。

```toml
[qq]
app_id = '你的 AppID'
app_secret = '你的 AppSecret'
```

### 模型

任何 OpenAI 兼容端点都能用：

```toml
[llm]
providers = [{ name = 'deepseek', base_url = 'https://api.deepseek.com/v1', api_key = 'sk-xxx' }]
models = [
  { name = 'planner', model_identifier = 'deepseek-chat', provider = 'deepseek' },
  { name = 'replyer', model_identifier = 'deepseek-chat', provider = 'deepseek' },
]

[llm.tasks.planner]
models = ['planner']   # 思考用，必须支持工具调用

[llm.tasks.replyer]
models = ['replyer']   # 说话用，可以换个更快的
```

**Planner 必须支持 function calling**，否则无法调用 `reply` / `wait` / `stop`，整条链路走不通。

**别用纯推理模型做 Replyer。** 实测某个推理模型回一句话会先输出 1.4 万字符思考、
吃满 `max_tokens` 导致正文为空，单次耗时 90 秒以上。Replyer 要的是「快 + 会说人话」，
不需要深度推理。Planner 可以用推理模型，但也要留意 `max_tokens` 别被思考吃光。

分段配置能明显改善体验：Planner 用强模型（可靠性优先），Replyer 用快模型（延迟优先）。

### 调性格

```toml
[personality]
personality = '是女大学生，现在正在上网和群友聊天。'
behavior_style = '是大二女大学生，聊天偏日常。'
reply_style = '你的风格平淡简短。'
```

### 调话多话少

```toml
[chat.timing]
talk_value = 1.0          # 0 = 完全不说话，1 = 正常频率
debounce_mode = 'natural' # off / fast / natural / steady
reply_trigger_mode = 'frequency'  # 或 'dynamic'，更精细地控制回复次数
```

## 架构

```
QQ 网关 WS
   │  解码、去重、落库
   ▼
chat.Filter ──► router.Hub ──► agent.Runtime（每会话一个 goroutine）
                                   │
                     ┌─────────────┴──────────────┐
                     │  ① 防抖：等一小会儿看有没有后续  │
                     │  ② 门控：这批消息值得思考吗     │
                     │  ③ Planner：LLM + 工具循环     │
                     │  ④ Replyer：把方向变成具体的措辞  │
                     └─────────────┬──────────────┘
                                   ▼
                          分割 / 打字延迟 ──► QQ OpenAPI
```

### 目录

| 包 | 职责 |
|---|---|
| `internal/qq` | QQ 官方机器人适配（网关、编解码、发送、被动窗口） |
| `internal/agent` | Agent 核心（运行时、Planner 循环、提示词、Replyer） |
| `internal/gate` | 三层开口决策 |
| `internal/chat` | 防抖、分割、拟人化后处理、过滤 |
| `internal/tool` | 工具注册表与内置工具 |
| `internal/llm` | OpenAI 兼容客户端（流式、工具调用、参数修复） |
| `internal/store` | SQLite 与迁移 |
| `internal/router` | 会话路由与生命周期 |
| `internal/web` | WebUI 后端与内嵌前端 |
| `internal/prompt` | 提示词模板加载与渲染 |

## 关键设计

### 三层开口决策

**防抖**：消息不立刻处理，等一小段时间看有没有后续。四档规则：

| 档位 | 安静开批 | 活跃开批 | 同人续发 | 换人 |
|---|---|---|---|---|
| fast | 1s | 3s | 1s | 0.5s |
| natural | 2s | 5s | 2s | 0.5s |
| steady | 4s | 7s | 4s | 2s |

额外规则：裸图至少 4 秒、纯表情包只提前不延后、整批 8 秒硬上限、@ 后 1 秒上限、**已到期批次不被新消息延长**。

**门控**：两种模式。
- 频率阈值：`阈值 = ceil(1 / 频率)`，配合空窗补偿（沉默时长折算成消息数，但封顶 `阈值-1` 且 `pending=0` 时永不触发）
- 动态门控：逻辑回归估算回复概率，按 1 小时滑动窗口的实际回复数动态调阈值

**空闲退避**：连续几次想完了没说话，检查间隔指数退避 `min(300s, 15s × 2^(n-2))`，攒够 6 条新消息立即绕过。

### Planner / Replyer 两段式

Planner 调用 `reply` 工具时，传的不是回复正文，而是 `reply_reference` —— 描述「这次想表达什么方向、基于什么上下文」。

Replyer 拿着这个方向和人格提示词，生成真正的措辞。

好处：思考和说话彻底解耦，可以各配模型、各自调优。

### 会话 actor

每个聊天流一个 goroutine 独占状态，外界通过 channel 投递消息。无锁、无竞态。闲置 30 分钟回收，上下文从数据库重建。

## 已知限制

QQ 官方平台：

- 一条消息只能承载一种内容，文本+图片+@ 必须拆成多条发
- 被动回复窗口：群聊 300 秒内 5 条、私聊 3600 秒内 4 条
- `@` 只能在 markdown 消息里生效（纯文本里的 `<@openid>` 会显示成代码）
- OpenID ≠ QQ 号，跨机器人不通用
- 不支持戳一戳、语音、文件出站

## 测试

```bash
go test ./... -timeout 240s
```

三类测试：

| 测试 | 内容 |
|---|---|
| `internal/chat` | 防抖四档规则、8 秒/1 秒上限、分割器、拟人化后处理 |
| `internal/gate` | 门控权重、频率阈值、空窗补偿、动态门控、空闲退避 |
| `internal/qq` | 事件解码、@ 转写、出站拆条、被动窗口 |
| `internal/mock` | **完整链路场景测试** |

### 模拟测试

`internal/mock` 提供一个假的 OpenAI 兼容服务 + 完整的运行时 harness，
不用连 QQ、不用真的调模型、也不用人工发消息，就能验证整条链路：

```go
h := mock.NewHarness(t, mock.HarnessOptions{
    Planner: func(req *mock.RecordedRequest, call int) *mock.Reply {
        return mock.ToolReply("reply", `{"msg_id":"m1","reply_reference":"回应"}`)
    },
    Replyer: func(req *mock.RecordedRequest, call int) *mock.Reply {
        return mock.TextReply("嗨")
    },
})

h.SendMessage("你好", true)          // 注入一条消息
text := h.WaitForSend(10*time.Second) // 等它发出
```

覆盖的场景包括：基础回复、`stop` 不说话、`wait` 后继续、多轮工具循环、
重启后上下文恢复、私聊触发、静默频率、工具报错不崩、模型报错不卡死、长回复分条。

### 真实端点体检

想验证真实模型能不能用，不用发 QQ 消息：

```bash
go run ./tools/livecheck -turns 4
```

它会跑一遍完整对话，打印每次模型调用的耗时、token、推理长度、调用了哪个工具，
以及最终发出的内容。调模型选型时很有用。

## 与 MaiBot 的关系

架构、数值、提示词都来自 [MaiBot](https://github.com/Mai-with-u/MaiBot)，用 Go 重新实现。

明确不做：插件系统、多平台适配器、配置迁移、错别字生成、电脑操作 Agent、MCP、联网搜索。

## License

GPL-3.0
