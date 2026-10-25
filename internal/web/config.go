package web

// 本文件实现「WebUI 编辑配置」。
//
// 与 MaiBot 的关系：它的做法是「后端出 schema、前端生成动态表单、
// 提交整节配置」。我们只需要改几十个扁平字段，所以简化成：
//   - GET  /api/config          → 字段清单 + 当前值（已有的只读视图扩展）
//   - POST /api/config          → 提交 {key: value} 的补丁，逐项校验后生效
//
// 刻意不做的事：
//   - 不返回也不允许改密钥（app_secret / api_key / password / session_secret）
//   - 不允许改 database.path、日志目录这类「改错就起不来」的路径
//   - 不允许整节覆盖：整节覆盖会让前端删掉一个字段就等于把它清零
//
// 生效语义分两种，由 config.Field.Restart 标注：
//   - 即时生效：改动落进新的配置快照，各组件下一轮读 Store.Get() 时拿到新值
//   - 需要重启：监听端口、日志目录、QQ 凭据等只在启动时读一次

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/logx"
)

// handleConfigFields 返回可编辑字段的清单与当前值。
//
// 为什么把「清单」也返回：前端不做硬编码表单，加一个配置项只要
// 在 config.EditableFields 里加一行，界面上自动出现。
func (s *Server) handleConfigFields(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Get()
	if cfg == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "配置不可用"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"fields":   config.EditableFields(),
		"values":   cfg.Snapshot(),
		"path":     s.cfg.Path(),
		"restart":  s.takeRestartNotice(),
		"readonly": readOnlySummary(cfg),
	})
}

// handleConfigUpdate 接收一个 {key: value} 补丁并应用。
//
// 请求体形如：
//
//	{"values": {"chat.max_context_size": 40, "chat.enable_reply_at": true}}
//
// 只更新出现的键：不在请求体里的项保持原值。
// 这样前端可以只提交被改动的字段，不用整份回传（也就不会因为
// 「某个字段这次没渲染」而把它清空）。
func (s *Server) handleConfigUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST"})
		return
	}

	// 限制请求体大小。不设上限时一个「粘贴了一大段文字」的输入就能
	// 让配置文件涨到几 MB，而 Clone（TOML 往返）与 Snapshot 每次保存、
	// 每次读配置都要带上它 —— 一个人手滑就能把整个进程拖慢。
	r.Body = http.MaxBytesReader(w, r.Body, maxConfigBodyBytes)

	var req struct {
		Values map[string]any `json:"values"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	if len(req.Values) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "没有要修改的字段"})
		return
	}

	// 只允许改清单里列过的键。不认识的键直接拒绝，
	// 而不是静默忽略 —— 拼错一个键名却提示「保存成功」是最坏的结果。
	allowed := make(map[string]bool, len(config.EditableFields()))
	for _, f := range config.EditableFields() {
		allowed[f.Key] = true
	}
	var unknown []string
	for k := range req.Values {
		if !allowed[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "不支持的配置项: " + strings.Join(unknown, ", "),
		})
		return
	}

	var (
		applied []string
		restart []string
	)
	err := s.cfg.Update(func(next *config.Config) error {
		for _, f := range config.EditableFields() {
			raw, ok := req.Values[f.Key]
			if !ok {
				continue
			}
			if err := next.ApplyField(f.Key, raw); err != nil {
				return err
			}
			applied = append(applied, f.Key)
			if f.Restart {
				restart = append(restart, f.Label)
			}
		}
		return nil
	})
	if err != nil {
		// 校验失败：什么都没变（Store.Update 在校验不过时不落盘、不换快照）。
		s.log.Warn("配置保存失败", "err", err)
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	// 日志级别可以就地生效，顺手调一下 —— 否则用户把级别改成 DEBUG
	// 之后要重启才能看到详细日志，体验上等于没做成这个功能。
	//
	// 失败要报出来：静默失败的表现是「界面说保存成功、日志里还是老样子」，
	// 这比直接报错难查得多。
	if err := s.applyLiveEffects(req.Values); err != nil {
		s.log.Warn("配置已保存，但即时生效失败", "err", err)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"applied": applied,
			"restart": restart,
			"values":  s.cfg.Get().Snapshot(),
			"warning": err.Error(),
		})
		return
	}

	// 每次都整体替换（包括替换成空）：否则上一次改端口留下的
	// 「需重启」会一直挂着，用户分不清哪些是这次改的。
	s.setRestartNotice(restart)
	s.log.Info("配置已更新", "fields", applied, "need_restart", restart)

	cfg := s.cfg.Get()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"applied": applied,
		"restart": restart,
		"values":  cfg.Snapshot(),
	})
}

// maxConfigBodyBytes 是配置保存请求体的上限。
//
// 1MB 对「几十个扁平字段」绰绰有余，同时挡住误粘贴大段文本。
const maxConfigBodyBytes = 1 << 20

// applyLiveEffects 处理那些「不是读配置快照、而是持有自己状态」的项。
//
// 目前只有日志级别：它由 slog 的 LevelVar 承载，改配置不会自动生效。
// 其余项都是每次用到才读 Store.Get()，不需要额外动作。
//
// 返回错误而不是静默吞掉：「界面说成功、日志没变」是最难排查的一类问题。
// 注意 SetLevel 返回 false 的语义不是「失败」而是「日志系统尚未初始化，
// 值已暂存、将在 Init 时生效」—— 生产路径不可达（main.go 里 logx.Init
// 早于 WebUI 构造），这是防御性分支，提示文案必须与实际行为一致。
func (s *Server) applyLiveEffects(values map[string]any) error {
	if raw, ok := values["log.level"]; ok {
		lv, ok := raw.(string)
		if !ok {
			return fmt.Errorf("log.level 需要字符串，收到 %T", raw)
		}
		if !logx.SetLevel(logx.ParseLevel(lv)) {
			return fmt.Errorf("终端日志级别未能即时生效（日志系统尚未初始化，值已暂存、初始化时生效）")
		}
	}
	if raw, ok := values["log.file_level"]; ok {
		lv, ok := raw.(string)
		if !ok {
			return fmt.Errorf("log.file_level 需要字符串，收到 %T", raw)
		}
		if !logx.SetFileLevel(logx.ParseLevel(lv)) {
			return fmt.Errorf("文件日志级别未能即时生效（日志系统尚未初始化，值已暂存、初始化时生效）")
		}
	}
	return nil
}

// readOnlySummary 返回「界面上显示但不可改」的信息。
//
// 这些是排查问题必需、又绝不能在网上改的：
// 配置文件路径、当前日志级别（可能是环境变量覆盖的结果）、
// 以及各密钥是否已配置（只报有/无，不回显值）。
func readOnlySummary(cfg *config.Config) map[string]any {
	return map[string]any{
		"log_level":            cfg.Log.Level,
		"log_file_level":       cfg.Log.FileLevel,
		"log_dir":              cfg.Log.Dir,
		"database_path":        cfg.Database.Path,
		"web_host":             cfg.Web.Host,
		"web_port":             cfg.Web.Port,
		"qq_credentials_ready": len(cfg.MissingCredentials()) == 0,
		"llm_provider_count":   len(cfg.LLM.Providers),
		"llm_model_count":      len(cfg.LLM.Models),
		"keyword_rule_count":   len(cfg.Keyword.Rules),
	}
}

// 重启提示的状态。
//
// 存在 Server 上而不是配置里：它只对「当前这几次请求」有意义，
// 重启一次就该消失。用互斥锁保护，因为可能在多个请求里读写。
func (s *Server) setRestartNotice(fields []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.restartFields = append([]string(nil), fields...)
	s.restartAt = time.Now()
}

// takeRestartNotice 取出并保留重启提示（不清空）。
//
// 不清空是刻意的：用户可能关掉页面再打开，此时仍然应该看到
// 「这些项需要重启」。进程重启后它自然消失。
func (s *Server) takeRestartNotice() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 显式造空切片而不是 append(nil, ...)：
	// 后者在没有任何项时返回 nil，序列化成 null，
	// 前端 forEach 直接抛错。这个坑已经踩过三次了（AGENTS.md 第 67 条）。
	out := make([]string, len(s.restartFields))
	copy(out, s.restartFields)
	return out
}

// ---------------------------------------------------------------------------
// 模型管理
//
// 对应 MaiBot 的「模型管理」页（/config/model），两个页签：
//   - 模型设置：厂商侧栏 + 模型列表
//   - 功能分配：任务 → 模型绑定
//
// 与通用配置表单分开的原因：LLM 是三层嵌套且列表会增删，
// 「点分键 + 平铺表单」表达不了。这里用结构化 JSON。
//
// 安全边界：api_key 永远不出现在响应里（只回 has_api_key）。
// 提交时把密钥输入框留空 = 不修改原值。
// ---------------------------------------------------------------------------

// handleLLMGet 返回厂商、模型、任务绑定三部分。
func (s *Server) handleLLMGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 GET"})
		return
	}
	cfg := s.cfg.Get()
	if cfg == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "配置不可用"})
		return
	}
	writeJSON(w, http.StatusOK, cfg.LLMSnapshot())
}

// handleLLMSave 处理厂商 / 模型 / 任务的增删改。
//
// 一次只做一件事（action 决定），而不是整节覆盖：
// 整节覆盖会让「前端少发了一个字段」等价于「把它清空」，
// 而这种丢失完全没有提示。
func (s *Server) handleLLMSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxConfigBodyBytes)

	var req struct {
		Action   string               `json:"action"`
		Provider config.ProviderPatch `json:"provider"`
		Model    config.ModelView     `json:"model"`
		Original string               `json:"original"`
		Task     config.TaskView      `json:"task"`
		Name     string               `json:"name"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}

	if err := s.cfg.Update(func(next *config.Config) error {
		switch req.Action {
		case "provider.save":
			return next.ApplyProviderPatch(req.Provider)
		case "provider.delete":
			return next.DeleteProvider(req.Name)
		case "model.save":
			return next.ApplyModelPatch(req.Model, req.Original)
		case "model.delete":
			return next.DeleteModel(req.Name)
		case "task.save":
			return next.ApplyTaskPatch(req.Task)
		default:
			return fmt.Errorf("未知操作 %q", req.Action)
		}
	}); err != nil {
		s.log.Warn("模型配置保存失败", "action", req.Action, "err", err)
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	s.log.Info("模型配置已更新", "action", req.Action)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"data": s.cfg.Get().LLMSnapshot(),
	})
}

// handleQQGet 返回 QQ 凭据的只读视图。
//
// 响应里没有 AppSecret 这个字段（config.QQView 结构体里压根不存在），
// 所以不存在「忘了把它置空」的可能。只给 secret_set 布尔值。
func (s *Server) handleQQGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 GET"})
		return
	}
	cfg := s.cfg.Get()
	if cfg == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "配置不可用"})
		return
	}
	writeJSON(w, http.StatusOK, cfg.QQSnapshot())
}

// handleQQSave 保存 QQ 凭据。
//
// 生效语义是「需重启」：网关客户端在 app.Start 里用当时的凭据构造，
// 之后只在重连时复用同一个 client，不会重新读配置。
// 这一点必须如实告诉用户，否则「填完凭据没反应」会被当成 bug 排查很久。
func (s *Server) handleQQSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxConfigBodyBytes)

	var patch config.QQPatch
	if err := readJSON(r, &patch); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}

	if err := s.cfg.Update(func(next *config.Config) error {
		return next.ApplyQQPatch(patch)
	}); err != nil {
		s.log.Warn("QQ 凭据保存失败", "err", err)
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	s.setRestartNotice([]string{"QQ 凭据"})
	s.log.Info("QQ 凭据已更新", "app_id", patch.AppID, "secret_changed", patch.AppSecret != "" || patch.ClearAppSecret)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"restart": []string{"QQ 凭据"},
		"data":    s.cfg.Get().QQSnapshot(),
	})
}
