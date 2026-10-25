// Package embedding 是文本向量化客户端。
//
// 为什么单独成一个包，而不是塞进 internal/llm：
//
//	llm.Client 的设计是「按任务选模型 + 失败切换 + 重试」，
//	面向的是 chat completions 那一套（消息列表、工具调用、流式增量）。
//	embedding 的契约完全不同：输入是一批字符串、输出是一批向量、
//	没有工具、没有流式、失败重试策略也不同（批量太大会被拒，要拆）。
//	硬塞进 llm 会让两边都变脏。
package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/logx"
)

// ErrNotConfigured 表示没有配置 embedding 模型。
var ErrNotConfigured = fmt.Errorf("embedding: 未配置模型")

// ErrRateLimited 表示端点返回 429（限流）。
//
// 单独一个哨兵错误是为了让重试逻辑能区分「限流要退避重试」
// 和别的失败（错误信息里仍带服务端原文）。
var ErrRateLimited = errors.New("embedding: 端点限流（429）")

// 单批上限有两个维度，先到先切：
//
//   - 条数上限：DashScope 类端点对单次 input 数组长度有限制，
//     超了报 InvalidParameter 而不是明确说"太多"，所以自己先切。
//   - 字符数上限：按条数切挡不住「10 条各 8k 字符的记忆摘要」这种
//     单请求几十万 token 的批，端点直接 400。字符数与 token 数
//     大致成正比，8k 字符对常见端点是安全值。
const (
	embeddingBatchMaxItems  = 10
	embeddingBatchCharLimit = 8000
)

// Client 是向量化客户端。
type Client struct {
	cfg  *config.Store
	http *http.Client
	log  *logx.Logger

	// dim 是首次请求实际拿到的维度，用于缓存与校验。
	// 维度必须全局一致：库里混着不同维度的向量就没法算余弦。
	//
	// 并发说明：attachEmbedding（每轮摘要）与 searchByVector（每次
	// query_memory）在不同协程里都会写它，同时 Dimension() 会被别的
	// 协程读，所以必须用 atomic 而不是裸变量。维度一旦确定不再变
	// （后续遇到不同维度直接报错），因此只有 0 → N 这一次有效写入。
	dim atomic.Int64
}

// New 创建客户端。
//
// 不再给 http.Client 设固定超时：超时改由每次请求的 context 承担
// （口径与 internal/llm/client.go 一致，取任务硬超时与服务商超时的较小值）。
func New(cfg *config.Store) *Client {
	return &Client{
		cfg:  cfg,
		http: &http.Client{},
		log:  logx.Get("embedding"),
	}
}

// Enabled 判断是否配置了 embedding 模型。
func (c *Client) Enabled() bool {
	if c == nil || c.cfg == nil {
		return false
	}
	return len(c.cfg.Get().ResolveModels(config.TaskEmbedding)) > 0
}

// Dimension 返回向量维度；未知时返回 0。
func (c *Client) Dimension() int {
	if c == nil {
		return 0
	}
	return int(c.dim.Load())
}

// Embed 把一批文本转成向量。
//
// 返回的切片与入参一一对应、顺序一致。空文本会得到零向量占位，
// 因为调用方（记忆检索）是按索引对账的，跳过一个会导致整体错位。
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if !c.Enabled() {
		return nil, ErrNotConfigured
	}

	models := c.cfg.Get().ResolveModels(config.TaskEmbedding)
	modelName := models[0]
	model, err := c.cfg.Get().Model(modelName)
	if err != nil {
		return nil, fmt.Errorf("查找 embedding 模型 %q 失败: %w", modelName, err)
	}
	provider, err := c.cfg.Get().Provider(model.Provider)
	if err != nil {
		return nil, fmt.Errorf("查找 provider %q 失败: %w", providerName(model), err)
	}

	// 超时口径对齐 internal/llm/client.go：取「任务硬超时」与
	// 「服务商 timeout」里较小的那个，两个都 <= 0 时才真正不限时。
	// 以前 provider.timeout 完全没被读取，固定 60 秒。
	hard := time.Duration(c.cfg.Get().Task(config.TaskEmbedding).HardTimeout * float64(time.Second))
	timeout := minPositive(hard, provider.TimeoutDuration())

	out := make([][]float32, 0, len(texts))
	for _, batch := range splitEmbedBatches(texts) {
		vecs, err := c.embedBatch(ctx, provider, model, batch, timeout)
		if err != nil {
			return nil, err
		}
		out = append(out, vecs...)
	}

	return out, nil
}

// splitEmbedBatches 把一批文本切成若干请求批。
//
// 条数与总字符数两个上限先到先切；单条超过字符上限时独占一批 ——
// 不能再切，切开的内容已经不是原文了。
func splitEmbedBatches(texts []string) [][]string {
	var (
		batches [][]string
		cur     []string
		chars   int
	)
	flush := func() {
		if len(cur) > 0 {
			batches = append(batches, cur)
			cur, chars = nil, 0
		}
	}
	for _, t := range texts {
		n := utf8.RuneCountInString(t)
		if len(cur) > 0 && (len(cur) >= embeddingBatchMaxItems || chars+n > embeddingBatchCharLimit) {
			flush()
		}
		cur = append(cur, t)
		chars += n
	}
	flush()
	return batches
}

// embedBatch 发一批请求，带重试与 429 退避。
//
// 重试口径对齐 provider 配置（与 llm 层一致）；429 时把间隔翻倍，
// 对齐参考实现 embedding_rate_limit.py 的做法（最多翻 2 次）。
func (c *Client) embedBatch(ctx context.Context, provider *config.ProviderConfig, model *config.ModelConfig, texts []string, timeout time.Duration) ([][]float32, error) {
	chars := 0
	for _, t := range texts {
		chars += utf8.RuneCountInString(t)
	}

	attempts := provider.Retry()
	interval := provider.RetryIntervalDuration()
	doublings := 0

	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(interval):
			}
		}
		vecs, err := c.embedBatchOnce(ctx, provider, model, texts, timeout)
		if err == nil {
			return vecs, nil
		}
		lastErr = err
		// 上层取消（含检索超时）直接退出，不再重试。
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, ErrRateLimited) && doublings < 2 {
			doublings++
			interval *= 2
		}
	}
	return nil, fmt.Errorf("embedding 批量请求失败（本批 %d 条 / %d 字符，已尝试 %d 次）: %w",
		len(texts), chars, attempts, lastErr)
}

// embedBatchOnce 发一次批量请求（不重试）。
func (c *Client) embedBatchOnce(ctx context.Context, provider *config.ProviderConfig, model *config.ModelConfig, texts []string, timeout time.Duration) ([][]float32, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	// 空串会被服务端拒绝，替换成一个空格占位；
	// 返回时按索引对应，调用方自己决定要不要用。
	inputs := make([]string, len(texts))
	for i, t := range texts {
		if strings.TrimSpace(t) == "" {
			inputs[i] = " "
		} else {
			inputs[i] = t
		}
	}

	body := map[string]any{
		"model":           model.ModelIdentifier,
		"input":           inputs,
		"encoding_format": "float",
	}
	// dimensions 只在配置里显式给了才发：部分端点不认这个字段，
	// 无脑带上会 400。注意它的角色只是「请求参数」—— 端点是否遵守
	// 由下方实测校验兜底（端点忽略 dimensions 时很多中转站返回原生维度）。
	if model.Dimension != nil && *model.Dimension > 0 {
		body["dimensions"] = *model.Dimension
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("编码 embedding 请求失败: %w", err)
	}

	url := strings.TrimRight(provider.BaseURL, "/") + "/embeddings"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("构造 embedding 请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	applyAuth(req, provider)
	for k, v := range provider.DefaultHeaders {
		req.Header.Set(k, v)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 embedding 失败: %w", err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取 embedding 响应失败: %w", err)
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: %s", ErrRateLimited, truncate(string(payload), 300))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("embedding 返回 %d: %s", resp.StatusCode, truncate(string(payload), 300))
	}

	var parsed struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		} `json:"data"`
		Model string `json:"model"`
		Error *struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return nil, fmt.Errorf("解析 embedding 响应失败: %w，原始内容: %s", err, truncate(string(payload), 300))
	}
	// 有的端点即使 200 也会在 body 里带 error 对象。
	if parsed.Error != nil && parsed.Error.Message != "" {
		return nil, fmt.Errorf("embedding 返回错误: %s（%s）", parsed.Error.Message, parsed.Error.Code)
	}
	if len(parsed.Data) != len(texts) {
		return nil, fmt.Errorf("embedding 返回 %d 条，请求了 %d 条", len(parsed.Data), len(texts))
	}

	// 按 index 落位，不依赖返回顺序。
	out := make([][]float32, len(texts))
	for _, item := range parsed.Data {
		if item.Index < 0 || item.Index >= len(out) {
			return nil, fmt.Errorf("embedding 返回的 index %d 越界", item.Index)
		}
		out[item.Index] = item.Embedding
	}

	// 记录维度并校验一致性。
	for i, v := range out {
		if len(v) == 0 {
			return nil, fmt.Errorf("第 %d 条 embedding 为空", i)
		}
		if err := c.observeDimension(len(v), parsed.Model); err != nil {
			return nil, err
		}
	}

	// 配置维度与实测维度不一致 → 明确报错，而不是静默把实测维度写进库。
	// 端点忽略 dimensions 参数时（很多中转站如此），旧口径的后果是
	// 库里存实测维度、检索按配置维度过滤，候选集恒空且无任何日志。
	if model.Dimension != nil && *model.Dimension > 0 {
		if measured := int(c.dim.Load()); measured > 0 && measured != *model.Dimension {
			return nil, fmt.Errorf("端点忽略了 dimensions 配置：配置 %d 维，实际返回 %d 维", *model.Dimension, measured)
		}
	}

	return out, nil
}

// observeDimension 记录首个实测维度，并校验后续维度一致。
//
// 用 CAS 保证并发下只有一个 0 → N 的写入；维度一旦确定不再变，
// 之后任何不同维度都直接报错。
func (c *Client) observeDimension(n int, model string) error {
	for {
		old := c.dim.Load()
		if old == 0 {
			if c.dim.CompareAndSwap(0, int64(n)) {
				c.log.Info("embedding 维度已确定", "dim", n, "model", model)
				return nil
			}
			// 并发下别的协程先定了维度，重读一次做校验。
			continue
		}
		if int64(n) != old {
			return fmt.Errorf("embedding 维度不一致：已有 %d，本次 %d", old, n)
		}
		return nil
	}
}

// Cosine 计算两个向量的余弦相似度。
//
// 维度不一致时返回 0 而不是报错：库里的旧向量可能是别的维度，
// 检索时安静跳过比整轮失败好。
func Cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// Pack 把向量序列化成 float32 小端字节串，用于存 BLOB。
//
// 为什么不用 JSON：1024 维的 JSON 约 12KB，float32 二进制是 4KB，
// 而记忆检索每次要读回几十条，差值直接体现在响应时间上。
func Pack(v []float32) []byte {
	if len(v) == 0 {
		return nil
	}
	buf := make([]byte, len(v)*4)
	for i, f := range v {
		bits := math.Float32bits(f)
		buf[i*4] = byte(bits)
		buf[i*4+1] = byte(bits >> 8)
		buf[i*4+2] = byte(bits >> 16)
		buf[i*4+3] = byte(bits >> 24)
	}
	return buf
}

// Unpack 从字节串还原向量。
func Unpack(b []byte) []float32 {
	if len(b) == 0 || len(b)%4 != 0 {
		return nil
	}
	out := make([]float32, len(b)/4)
	for i := range out {
		bits := uint32(b[i*4]) | uint32(b[i*4+1])<<8 | uint32(b[i*4+2])<<16 | uint32(b[i*4+3])<<24
		out[i] = math.Float32frombits(bits)
	}
	return out
}

// applyAuth 按 provider 配置附加鉴权头。
//
// header 值的拼接统一走 config.BuildAuthHeaderValue（与 llm 层共用同一语义，
// 对齐参考实现 openai_compat.py 的 _build_auth_header_value）。
func applyAuth(req *http.Request, p *config.ProviderConfig) {
	authType := p.AuthType
	if authType == "" {
		authType = "bearer"
	}
	switch authType {
	case "none":
		// 不附加任何鉴权
	case "header":
		name := p.AuthHeaderName
		if name == "" {
			name = "Authorization"
		}
		req.Header.Set(name, config.BuildAuthHeaderValue(p.AuthHeaderPrefix, p.APIKey))
	case "query":
		q := req.URL.Query()
		name := p.AuthQueryName
		if name == "" {
			name = "api_key"
		}
		q.Set(name, p.APIKey)
		req.URL.RawQuery = q.Encode()
	default:
		// bearer 是默认方式。
		req.Header.Set("Authorization", config.BuildAuthHeaderValue("Bearer", p.APIKey))
	}
}

// minPositive 返回两个时长里较小且为正的那个，都为 0 时返回 0。
// 与 internal/llm/client.go 的同名私有函数语义一致。
func minPositive(a, b time.Duration) time.Duration {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}

func providerName(m *config.ModelConfig) string {
	if m == nil {
		return ""
	}
	return m.Provider
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
