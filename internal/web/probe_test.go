package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/llm"
)

// fakeRestarter 记录「有没有被排上重启」，不真的动进程。
type fakeRestarter struct {
	calls atomic.Int32
	err   error
	delay time.Duration
}

func (f *fakeRestarter) Schedule(delay time.Duration) error {
	f.calls.Add(1)
	f.delay = delay
	return f.err
}

// newProbeServer 构造一个带 LLM 客户端的测试服务器。
func newProbeServer(t *testing.T, opts Options) *Server {
	t.Helper()
	if opts.Config == nil {
		opts.Config = config.NewStoreFrom(config.Default(), "")
	}
	return NewServer(opts)
}

func TestModelProbeRequiresPost(t *testing.T) {
	s := newProbeServer(t, Options{})
	rec := httptest.NewRecorder()
	s.handleModelProbe(rec, httptest.NewRequest(http.MethodGet, "/api/config/llm/probe", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET 应返回 405，实际 %d", rec.Code)
	}
}

func TestModelProbeWithoutLLMReturns503(t *testing.T) {
	s := newProbeServer(t, Options{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/config/llm/probe",
		strings.NewReader(`{"model":"m1"}`))
	s.handleModelProbe(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("没有 LLM 客户端时应 503，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

func TestModelProbeMissingModel(t *testing.T) {
	cfg := config.NewStoreFrom(config.Default(), "")
	s := newProbeServer(t, Options{Config: cfg, LLM: llm.New(cfg)})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/config/llm/probe",
		strings.NewReader(`{}`))
	s.handleModelProbe(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("缺 model 应 400，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "model") {
		t.Errorf("错误信息应指出缺 model: %s", rec.Body.String())
	}
}

// TestModelProbeReturnsHint 守「失败时必须给出可执行建议」。
//
// 这个功能的全部价值就在 Hint：只回一个 "HTTP 404" 用户还是不知道怎么改。
func TestModelProbeReturnsHint(t *testing.T) {
	cfg := config.Default()
	// 指向一个必然连不上的地址，快速失败。
	cfg.LLM.Providers = []config.ProviderConfig{{
		Name: "p1", BaseURL: "http://127.0.0.1:1/v1", AuthType: "bearer",
	}}
	cfg.LLM.Models = []config.ModelConfig{{Name: "m1", ModelIdentifier: "x", Provider: "p1"}}
	store := config.NewStoreFrom(cfg, "")
	s := newProbeServer(t, Options{Config: store, LLM: llm.New(store)})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/config/llm/probe",
		strings.NewReader(`{"model":"m1"}`))
	s.handleModelProbe(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("探活本身成功执行就该 200（结论在 body 里），实际 %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"ok":false`) {
		t.Errorf("连不上时 ok 应为 false: %s", body)
	}
	if !strings.Contains(body, "hint") {
		t.Errorf("失败时必须给 hint: %s", body)
	}
}

func TestProviderProbeMissingProvider(t *testing.T) {
	cfg := config.NewStoreFrom(config.Default(), "")
	s := newProbeServer(t, Options{Config: cfg, LLM: llm.New(cfg)})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/config/llm/probe-provider",
		strings.NewReader(`{}`))
	s.handleProviderProbe(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("缺 provider 应 400，实际 %d", rec.Code)
	}
}

func TestProviderProbeUnknownProvider(t *testing.T) {
	cfg := config.NewStoreFrom(config.Default(), "")
	s := newProbeServer(t, Options{Config: cfg, LLM: llm.New(cfg)})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/config/llm/probe-provider",
		strings.NewReader(`{"provider":"不存在"}`))
	s.handleProviderProbe(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `"ok":false`) {
		t.Errorf("不存在的服务商应 ok=false: %s", body)
	}
	if !strings.Contains(body, "hint") {
		t.Errorf("必须给 hint: %s", body)
	}
}

// ---------------------------------------------------------------------------
// 重启
// ---------------------------------------------------------------------------

func TestRestartRequiresConfirm(t *testing.T) {
	r := &fakeRestarter{}
	s := newProbeServer(t, Options{Restart: r})

	for _, body := range []string{`{}`, `{"confirm":"yes"}`, `{"confirm":""}`} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/restart", strings.NewReader(body))
		s.handleRestart(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body=%s 应 400，实际 %d", body, rec.Code)
		}
	}
	if r.calls.Load() != 0 {
		t.Error("没有确认时不能真的排重启")
	}
}

func TestRestartRequiresPost(t *testing.T) {
	s := newProbeServer(t, Options{Restart: &fakeRestarter{}})
	rec := httptest.NewRecorder()
	s.handleRestart(rec, httptest.NewRequest(http.MethodGet, "/api/restart", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET 应 405，实际 %d", rec.Code)
	}
}

func TestRestartUnsupportedReturns503(t *testing.T) {
	s := newProbeServer(t, Options{}) // 没有 Restart
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/restart",
		strings.NewReader(`{"confirm":"restart"}`))
	s.handleRestart(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("不支持重启时应 503，实际 %d", rec.Code)
	}
}

func TestRestartSchedules(t *testing.T) {
	r := &fakeRestarter{}
	s := newProbeServer(t, Options{Restart: r})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/restart",
		strings.NewReader(`{"confirm":"restart"}`))
	s.handleRestart(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if r.calls.Load() != 1 {
		t.Errorf("应排 1 次重启，实际 %d", r.calls.Load())
	}
	// 延迟必须 >0：立刻重启会让浏览器收到网络错误而不是「正在重启」。
	if r.delay <= 0 {
		t.Errorf("延迟应为正，实际 %v", r.delay)
	}
	if !strings.Contains(rec.Body.String(), "delay_ms") {
		t.Error("响应应带 delay_ms，前端要靠它决定等多久开始轮询")
	}
}

// TestRestartScheduleErrorSurfaces 守「起不来时不能假装成功」。
func TestRestartScheduleErrorSurfaces(t *testing.T) {
	r := &fakeRestarter{err: errTestSchedule}
	s := newProbeServer(t, Options{Restart: r})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/restart",
		strings.NewReader(`{"confirm":"restart"}`))
	s.handleRestart(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("排重启失败应 500，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "hint") {
		t.Error("失败时要告诉用户怎么办")
	}
}

var errTestSchedule = &testError{"无法定位可执行文件"}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }
