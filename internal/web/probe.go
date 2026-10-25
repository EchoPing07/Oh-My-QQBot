package web

// 本文件是「模型连通性测试」接口。
//
// 存在的理由很具体：模型配置是唯一一类「填错了不会报错、只表现为
// 机器人不说话」的配置。以前只能靠 `go run ./tools/livecheck`
// 在命令行验证，而真正会填错的是在网页上改配置的人。
//
// 与 MaiBot 的 /api/webui/models/test-model 对应
// （src/webui/routers/model.py:162）。
//
// 刻意不做的那一半：参考实现还有 embedding 模型的相似度对比测试
// （_test_text_embedding_model）。我们不做，因为那需要构造语料、
// 展示向量相似度，属于「调参」而不是「排错」，收益远小于复杂度。

import (
	"net/http"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/llm"
)

// maxProbeBodyBytes 是探活请求体上限。它只带一个模型名。
const maxProbeBodyBytes = 4 << 10

// probeTimeout 是探活的总超时。
//
// 20 秒是个折中：本机服务商通常 1 秒内就回，公网加速的也不会超过 10 秒。
// 超过 20 秒实际就是「地址不对」或「服务没起来」，与其让用户盯着转圈，
// 不如直接告诉他连不上。
const probeTimeout = 20 * time.Second

// handleModelProbe 测试单个模型能否正常调用。
//
// POST /api/config/llm/probe  {"model":"glm-5.3"}
//
// 返回 llm.ProbeResult，其中 Hint 字段是「接下来该改哪里」。
func (s *Server) handleModelProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST"})
		return
	}
	if s.llm == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "模型客户端不可用"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxProbeBodyBytes)

	var req struct {
		Model string `json:"model"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是合法 JSON"})
		return
	}
	if req.Model == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少 model"})
		return
	}

	// 用请求自己的 ctx：浏览器关掉页面时应该立刻放弃这次探活，
	// 而不是让服务端把 20 秒超时跑完。
	res := s.llm.ProbeModel(r.Context(), req.Model, probeTimeout)
	writeJSON(w, http.StatusOK, res)
}

// handleProviderProbe 测试整个服务商：只验「地址通 + 鉴权过」，
// 不关心具体模型能不能生成。
//
// POST /api/config/llm/probe-provider  {"provider":"local"}
//
// 单独一个接口的价值：填完 BaseURL 与 api_key 后，用户往往还没加模型，
// 这时候没有模型名可测。用 /models 列表端点能立刻分辨
// 「地址错了」「key 错了」「只是模型名还没填」。
func (s *Server) handleProviderProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST"})
		return
	}
	if s.llm == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "模型客户端不可用"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxProbeBodyBytes)

	var req struct {
		Provider string `json:"provider"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是合法 JSON"})
		return
	}
	if req.Provider == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少 provider"})
		return
	}

	res := s.llm.ProbeProvider(r.Context(), req.Provider, probeTimeout)
	writeJSON(w, http.StatusOK, res)
}

// 让 llm.ProbeResult 与 llm.ProviderProbeResult 的字段被静态检查认到，
// 避免有人「优化」掉 Hint 字段时前端悄悄少了一半信息。
var (
	_ = llm.ProbeResult{}.Hint
	_ = llm.ProviderProbeResult{}.Models
)
