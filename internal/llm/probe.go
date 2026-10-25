package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
)

// ProbeResult 是一次模型连通性测试的结果。
//
// 对应 MaiBot 的 ModelTestResponse（webui/routers/model.py:60）。
// 只保留我们真的用得到的字段 —— 那边还有 embedding 相似度对比，
// 我们不做向量模型的可视化测试，不搬那一半。
type ProbeResult struct {
	// OK 表示整条链路通（建连 + 鉴权 + 真的生成出了东西）。
	OK bool `json:"ok"`

	// LatencyMS 是端到端耗时（毫秒）。
	LatencyMS int64 `json:"latency_ms"`

	// Reply 是模型真的回出来的正文，用来判断「通了但不会说人话」。
	Reply string `json:"reply,omitempty"`

	// Model 是实际请求的模型标识符。
	Model string `json:"model,omitempty"`

	// Provider 是实际使用的服务商名。
	Provider string `json:"provider,omitempty"`

	// HTTPStatus 是服务端返回的状态码，0 表示没拿到响应（连不上/超时）。
	HTTPStatus int `json:"http_status,omitempty"`

	// Tokens 是本次消耗的 token 数（服务端给了才有）。
	Tokens int `json:"tokens,omitempty"`

	// Error 是失败原因，成功时为空。
	Error string `json:"error,omitempty"`

	// Hint 是针对这个错误的具体排查建议。
	//
	// 单独一个字段而不是拼进 Error：错误原文要保留（用户可能拿去搜），
	// 而「接下来该改哪里」是另一件事。
	Hint string `json:"hint,omitempty"`

	// Kind 是这次测试走的路径：chat / embedding。
	//
	// 界面上要显示它 —— 用户看到「我测的是向量模型」才知道
	// 不该拿它去配 Replyer。
	Kind string `json:"kind,omitempty"`

	// Dimension 是向量模型实际返回的维度；chat 模型为空。
	//
	// 这个数字很关键：memory_summaries 按「模型 + 维度」匹配向量
	// （AGENTS.md 第 45 条），配错维度会让检索永远空。
	Dimension int `json:"dimension,omitempty"`
}

// ProbeModel 用指定模型发一次最小请求，判断「这个配置能不能用」。
//
// 为什么不复用 Generate：Generate 会按任务策略挑模型、失败自动切下一个。
// 而这里要测的恰恰是**某一个具体模型**（用户刚在界面上填的那个），
// 切走了就测不出问题。
//
// 超时是硬性的：默认 20 秒。配置错误最常见的原因就是地址写错指向一个
// 黑洞，不设上限的话界面上转圈到天荒地老。
func (c *Client) ProbeModel(ctx context.Context, modelName string, timeout time.Duration) *ProbeResult {
	start := time.Now()
	res := &ProbeResult{Model: modelName}

	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cfg := c.cfg.Get()
	model, err := cfg.Model(modelName)
	if err != nil {
		res.LatencyMS = time.Since(start).Milliseconds()
		res.Error = fmt.Sprintf("找不到模型 %q", modelName)
		res.Hint = "先在「模型」页签把模型加进列表"
		return res
	}
	provider, err := cfg.Provider(model.Provider)
	if err != nil {
		res.LatencyMS = time.Since(start).Milliseconds()
		res.Error = fmt.Sprintf("模型 %q 引用的服务商 %q 不存在", modelName, model.Provider)
		res.Hint = "在「模型」页修正该模型的服务商，或者把服务商加回来"
		return res
	}
	res.Provider = provider.Name
	res.Model = model.ModelIdentifier
	if res.Model == "" {
		res.Model = model.Name
	}

	if strings.TrimSpace(provider.BaseURL) == "" {
		res.LatencyMS = time.Since(start).Milliseconds()
		res.Error = "服务商 BaseURL 为空"
		res.Hint = "填完整地址，通常以 /v1 结尾，例如 http://127.0.0.1:10082/v1"
		return res
	}

	// 向量模型走另一条路径。
	//
	// 这一步不能省：embedding 模型调 /chat/completions 会返回 404
	// "Unsupported model for OpenAI compatibility mode"，
	// 看起来像「模型名写错了」，实际是端点根本不对。
	// 以前不区分，于是所有向量模型在测试里永远是红的 ——
	// 而它们在实际调用（embedding.Client）里是好的。
	if isEmbeddingModel(model) {
		return c.probeEmbedding(ctx, model, provider, res, start)
	}
	res.Kind = "chat"

	// 最小请求：不挂工具、不要求 JSON。
	//
	// 参数刻意留空，交给 mergeRequest 填 —— 这一步很关键。
	//
	// 以前这里写死 temperature=0 / max_tokens=64，绕过了模型级配置。
	// 两个后果：
	//   - 有的模型不接受太小的 max_tokens，实际调用（用配置里的值）
	//     能通，测试却报错 —— 用户看到的是「测试失败但调用正常」
	//   - force_stream 的模型在非流式请求下会被服务端拒绝
	// 探活必须走与真实调用完全相同的参数合并路径，否则它测的
	// 不是「这个配置能不能用」，而是「我另造的一套参数能不能用」。
	req := &Request{
		Model:    res.Model,
		Messages: []Message{{Role: "user", Content: "回复「ok」两个字，不要别的内容。"}},
	}
	// 走与 generate→tryModel 相同的合并逻辑。
	merged := c.mergeRequest(req, c.cfg.Get().Task(config.TaskUtils), model)

	// 强制流式的模型必须走流式请求。
	//
	// mergeRequest 会把 model.ForceStream 合进 req.Stream，但 probeOnce
	// 调 buildBody(req, false) 时第一个参数覆盖了这个标记 ——
	// 于是 force_stream 的模型在探活里仍发非流式请求，被服务端拒绝，
	// 而真实调用（走 Generate 的流式分支）是好的。
	stream := merged.Stream
	resp, status, err := c.probeOnce(ctx, provider, merged, stream)
	res.LatencyMS = time.Since(start).Milliseconds()
	res.HTTPStatus = status

	if err != nil {
		res.Error, res.Hint = explainProbeError(err, status)
		return res
	}

	res.Reply = strings.TrimSpace(resp.Content)
	res.Tokens = resp.Usage.TotalTokens

	// 通了但正文为空：这是「能用，但这个模型不适合当回复器」。
	// 不算失败（链路是好的），但必须说出来 —— 否则用户会拿它配
	// Replyer，然后在真机上看到机器人「不说话」。
	if res.Reply == "" {
		if resp.Reasoning != "" {
			res.Hint = "模型只输出了思考没有正文，通常是推理模型 + max_tokens 太小；Replyer 不要用推理模型"
		} else {
			res.Hint = "服务端返回成功但没有正文，检查模型是否支持 /chat/completions"
		}
	}

	res.OK = true
	return res
}

// probeOnce 发一次非流式请求，返回正文与 HTTP 状态码。
//
// 与 doOpenAI 的区别：不吞状态码。doOpenAI 把非 2xx 折成一个 error
// 字符串，而这里要把状态码单独拿出来判断（401 与 404 的排查建议完全不同）。
func (c *Client) probeOnce(
	ctx context.Context, provider *config.ProviderConfig, req *Request, stream bool,
) (*Response, int, error) {
	body, err := c.buildBody(req, stream)
	if err != nil {
		return nil, 0, err
	}

	endpoint := strings.TrimRight(provider.BaseURL, "/") + "/chat/completions"
	if q := buildQuery(provider.DefaultQuery); q != "" {
		endpoint += "?" + q
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return nil, 0, fmt.Errorf("构造请求: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	applyAuth(httpReq, provider)
	for k, v := range provider.DefaultHeaders {
		httpReq.Header.Set(k, v)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	// 流式响应是 SSE，不能当成一个 JSON 读。
	//
	// 这条分支以前不存在：探活永远发非流式请求，所以 force_stream
	// 的模型在测试里必然被拒，而真实调用（GenerateStream）是好的。
	if stream {
		out, err := c.readStream(resp.Body, func(Delta) {}, time.Now())
		if err != nil {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return nil, resp.StatusCode, fmt.Errorf("%w: %s", err, summarizeBody(raw))
		}
		return out, resp.StatusCode, nil
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("读取响应: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("%w: %s", ErrNotOK, summarizeBody(raw))
	}

	var parsed openAIResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("解析响应: %w（原文前 200 字符: %s）", err, clip(raw, 200))
	}
	if len(parsed.Choices) == 0 {
		return nil, resp.StatusCode, errors.New("响应里没有 choices")
	}

	choice := parsed.Choices[0]
	out := &Response{
		Content:   choice.Message.Content,
		Reasoning: choice.Message.ReasoningContent,
		Usage: Usage{
			PromptTokens:     parsed.Usage.PromptTokens,
			CompletionTokens: parsed.Usage.CompletionTokens,
			TotalTokens:      parsed.Usage.TotalTokens,
		},
	}
	return out, resp.StatusCode, nil
}

// explainProbeError 把错误翻译成「哪里改」。
//
// 这条映射表是本功能的全部价值：单纯显示 "HTTP 401" 用户还是不知道
// 该去改哪个框。每一行都对着一个真实踩过的场景写的。
func explainProbeError(err error, status int) (msg, hint string) {
	msg = err.Error()

	switch {
	case status == 401 || status == 403:
		return msg, "API Key 无效或已过期。确认这个服务商下的 api_key 填的是这一家的"
	case status == 404:
		// 这一条最值得单独说：404 是「模型名不存在」，
		// 不是「端点不支持这个能力」（AGENTS.md 第 41 条）。
		return msg, "404 通常是模型标识符写错，或 BaseURL 少了/多了 /v1。先确认模型标识符与服务商控制台一致"
	case status == 400:
		return msg, "请求被拒绝。常见原因：模型不支持 max_tokens 参数、模型标识符不对、或该模型需要额外参数"
	case status == 429:
		return msg, "被限流或余额不足。等一会儿再试，或换服务商"
	case status >= 500:
		return msg, "服务商侧故障。稍后重试；持续出现就换服务商"
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return msg, "请求超时。检查 BaseURL 是否可达（本机地址确认端口对）、以及服务商是否真的在跑"
	}
	if errors.Is(err, ErrNotOK) {
		return msg, "服务端返回了错误，详见错误原文"
	}
	// 没拿到响应：连不上。
	return msg, "连不上服务端。检查 BaseURL 的协议、主机、端口，以及是否需要代理"
}

// clip 按 rune 截断，避免把汉字劈成非法 UTF-8（AGENTS.md 第 21 条）。
func clip(raw []byte, n int) string {
	rs := []rune(string(raw))
	if len(rs) <= n {
		return string(rs)
	}
	return string(rs[:n]) + "…"
}

// ProviderProbeResult 是一次服务商级探活的结果。
//
// 与 ProbeResult 的区别：这里不生成内容，只调 GET /models。
// 好处是「还没配模型」时也能验证地址与密钥 —— 而那正是最容易错的阶段。
type ProviderProbeResult struct {
	OK         bool     `json:"ok"`
	LatencyMS  int64    `json:"latency_ms"`
	Provider   string   `json:"provider,omitempty"`
	BaseURL    string   `json:"base_url,omitempty"`
	HTTPStatus int      `json:"http_status,omitempty"`
	// Models 是服务端返回的模型标识符列表（最多 50 个，够看就行）。
	Models []string `json:"models,omitempty"`
	Error  string   `json:"error,omitempty"`
	Hint   string   `json:"hint,omitempty"`
}

// ProbeProvider 用 GET /models 验证服务商的地址与鉴权。
//
// 为什么不复用 ProbeModel：配服务商时模型列表经常还是空的，
// 没有模型名可测。这个接口专门覆盖「地址对了没、key 对了没」。
//
// 有些 OpenAI 兼容端点不实现 /models（只给 /chat/completions），
// 所以 404 在这里不能直接判死 —— 会提示「端点不提供 /models，
// 请改用模型级测试」。这一条很重要：否则用户会以为配置错了。
func (c *Client) ProbeProvider(ctx context.Context, providerName string, timeout time.Duration) *ProviderProbeResult {
	start := time.Now()
	res := &ProviderProbeResult{Provider: providerName}

	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cfg := c.cfg.Get()
	provider, err := cfg.Provider(providerName)
	if err != nil {
		res.LatencyMS = time.Since(start).Milliseconds()
		res.Error = "找不到服务商 " + providerName
		res.Hint = "先在「模型设置」页签把它加进列表"
		return res
	}
	res.BaseURL = provider.BaseURL
	if strings.TrimSpace(provider.BaseURL) == "" {
		res.LatencyMS = time.Since(start).Milliseconds()
		res.Error = "服务商 BaseURL 为空"
		res.Hint = "填完整地址，通常以 /v1 结尾"
		return res
	}

	endpoint := strings.TrimRight(provider.BaseURL, "/") + "/models"
	if q := buildQuery(provider.DefaultQuery); q != "" {
		endpoint += "?" + q
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		res.LatencyMS = time.Since(start).Milliseconds()
		res.Error = "构造请求失败: " + err.Error()
		res.Hint = "BaseURL 可能不是合法 URL"
		return res
	}
	applyAuth(httpReq, provider)
	for k, v := range provider.DefaultHeaders {
		httpReq.Header.Set(k, v)
	}

	resp, err := c.http.Do(httpReq)
	res.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		res.Error, res.Hint = explainProbeError(err, 0)
		return res
	}
	defer resp.Body.Close()
	res.HTTPStatus = resp.StatusCode

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512<<10))

	switch {
	case resp.StatusCode == http.StatusNotFound:
		// 端点没实现 /models，不代表配置错。
		res.Error = fmt.Sprintf("HTTP 404: %s", summarizeBody(raw))
		res.Hint = "这个端点不提供 /models，无法用它验证。请改用「测试模型」直接发一次生成请求"
		return res
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		res.Error = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, summarizeBody(raw))
		res.Hint = "API Key 无效。确认这个服务商下的 api_key 填的是这一家的"
		return res
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		res.Error = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, summarizeBody(raw))
		res.Hint = "服务端返回错误，详见错误原文"
		return res
	}

	// 解析 data[].id。用宽松方式：拿不到就当成「通了但没有列表」，
	// 不因为解析失败就判定配置有问题。
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	ids := []string{}
	if err := json.Unmarshal(raw, &parsed); err == nil {
		for _, d := range parsed.Data {
			if d.ID != "" {
				ids = append(ids, d.ID)
			}
			if len(ids) >= 50 {
				break
			}
		}
	}
	res.Models = ids
	res.OK = true
	if len(ids) == 0 {
		res.Hint = "地址与鉴权都通过，但服务端没有返回模型列表；用「测试模型」逐个验证"
	}
	return res
}

// isEmbeddingModel 判断一个模型是不是向量模型。
//
// 判据是 Dimension != nil：这是我们配置里唯一的向量模型标志。
//
// 刻意不看「模型名里有没有 embed」—— 模型名是服务商的自由文本，
// 叫 `text-embedding-3-small` 的有，叫 `bge-m3` 的也有，
// 还有个叫 `qwen3.7-text-embedding-flash` 的。用名字猜必然漏。
// 参考实现是靠显式 model_type 字段（refs/MaiBot model.py:171），
// 我们用 Dimension，因为那本来就是为向量模型引入的配置项。
func isEmbeddingModel(m *config.ModelConfig) bool {
	return m.Dimension != nil
}

// probeEmbedding 测试向量模型：调 /embeddings，检查维度。
//
// 与 embedding.Client.embedBatch 走同一个端点、同一个请求体，
// 但不复用它 —— 那个函数要 providers/models 列表、要批量、
// 要落维度缓存，而这里只需要「对某一个模型发一条」。
// 复用的代价是给它加参数，反而更难读。
func (c *Client) probeEmbedding(
	ctx context.Context,
	model *config.ModelConfig,
	provider *config.ProviderConfig,
	res *ProbeResult,
	start time.Time,
) *ProbeResult {
	res.Kind = "embedding"

	// 探活不需要语义上有意义的文本，只要一段非空中文即可：
	// 空串在部分端点会被当作非法输入，英文又测不出中文分词的问题。
	// 不写具体昵称——昵称是配置项，这里白拿它当探针字符串只会在改名后变成误导。
	body := map[string]any{
		"model":           model.ModelIdentifier,
		"input":           []string{"连接测试"},
		"encoding_format": "float",
	}
	// dimensions 只在配置里显式给了才发：部分端点不认这个字段。
	if model.Dimension != nil && *model.Dimension > 0 {
		body["dimensions"] = *model.Dimension
	}

	raw, err := json.Marshal(body)
	if err != nil {
		res.LatencyMS = time.Since(start).Milliseconds()
		res.Error = "编码请求失败: " + err.Error()
		res.Hint = "模型配置里有非法字段"
		return res
	}

	endpoint := strings.TrimRight(provider.BaseURL, "/") + "/embeddings"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		res.LatencyMS = time.Since(start).Milliseconds()
		res.Error = "构造请求失败: " + err.Error()
		res.Hint = "BaseURL 可能不是合法 URL"
		return res
	}
	httpReq.Header.Set("Content-Type", "application/json")
	applyAuth(httpReq, provider)
	for k, v := range provider.DefaultHeaders {
		httpReq.Header.Set(k, v)
	}

	resp, err := c.http.Do(httpReq)
	res.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		res.Error, res.Hint = explainProbeError(err, 0)
		return res
	}
	defer resp.Body.Close()
	res.HTTPStatus = resp.StatusCode

	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		res.Error = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, summarizeBody(payload))
		_, res.Hint = explainProbeError(fmt.Errorf("%w: x", ErrNotOK), resp.StatusCode)
		return res
	}

	var parsed struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		} `json:"data"`
		Error *struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &parsed); err != nil {
		res.Error = fmt.Sprintf("解析响应失败: %v（原文前 200 字符: %s）", err, clip(payload, 200))
		res.Hint = "端点可能不是 OpenAI 兼容格式。常见原因：BaseURL 少了 /v1，或指向了一个网页"
		return res
	}
	// 有的端点即使 200 也会在 body 里带 error 对象。
	if parsed.Error != nil && parsed.Error.Message != "" {
		res.Error = "服务端返回错误: " + parsed.Error.Message
		res.Hint = "端点不接受这个模型做向量化。确认它是 embedding 模型，而不是聊天模型"
		return res
	}
	if len(parsed.Data) == 0 {
		res.Error = "响应里没有 data"
		res.Hint = "端点返回了合法 JSON 但没有向量。确认这个模型支持 /embeddings"
		return res
	}

	dim := len(parsed.Data[0].Embedding)
	res.Dimension = dim
	if dim == 0 {
		res.Error = "返回的向量是空的"
		res.Hint = "端点行为异常，换一个模型或服务商试试"
		return res
	}

	// 维度与配置不一致是必报的：memory_summaries 按「模型 + 维度」
	// 匹配（AGENTS.md 第 45 条），维度错了检索会永远是空的，
	// 而且不报任何错 —— 表现只是「记忆功能没效果」。
	if model.Dimension != nil && *model.Dimension > 0 && dim != *model.Dimension {
		res.OK = true // 链路是通的
		res.Error = fmt.Sprintf("维度不一致：配置写 %d，服务端返回 %d", *model.Dimension, dim)
		res.Hint = "把模型的 dimension 改成服务端实际返回的值，否则向量检索会全部落空"
		return res
	}

	res.OK = true
	return res
}
