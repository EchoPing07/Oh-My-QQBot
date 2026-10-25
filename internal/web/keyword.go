package web

// 本文件是「关键词反应规则」的结构化编辑接口。
//
// 与 /api/config/save 分开的原因：keyword.rules 是一张表（每条有
// keywords[] / regex[] / reaction 三个字段），点分键平铺表单表达不了。
// 与 llm 分开的原因：那边是三层嵌套 + 跨引用（改名要连带更新下游），
// 这里是扁平表，语义是「提交整张表」。
//
// 安全边界：
//   - 只接受 POST，且请求体有大小上限（正则可能很长，但不需要 1MB）
//   - 正则在这里就用 regexp.Compile 验一遍，错误信息能指到第几条
//   - 生效是即时的：agent 每次判定都重新 newKeywordMatcher(cfg.Get().Keyword.Rules)，
//     换快照后下一轮就是新规则，不需要重启

import (
	"fmt"
	"net/http"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
)

// handleKeywordsGet 返回全部关键词规则。
//
// 响应里同时给 rules 与 max_rules：前者是数据，后者是上限。
// 前端据此决定「添加」按钮是否还能点 —— 只靠后端拒绝会让用户
// 点了添加、填完、点保存，才发现不能加。
func (s *Server) handleKeywordsGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 GET"})
		return
	}
	cfg := s.cfg.Get()
	if cfg == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "配置不可用"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"rules":     cfg.KeywordRulesView(),
		"max_rules": maxKeywordRules,
	})
}

// handleKeywordsSave 整体替换规则表。
//
// 请求体形如 {"rules":[{"keywords":["猫"],"regex":[],"reaction":"..."}]}。
//
// 为什么是整表替换而不是增删改三个动作：
// 关键词规则在界面上就是一张可以任意增删行的表，用户看到什么就提交什么。
// 不像 llm 那样有跨引用（改名要连带更新任务绑定），
// 增量动作在这里只会让前端多写三倍的编排代码。
func (s *Server) handleKeywordsSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxConfigBodyBytes)

	var req struct {
		Rules []config.KeywordRuleView `json:"rules"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	// 上限保护：规则是每条消息都要逐条匹配的，条数无上限会让
	// 每条消息的处理里多出几百次正则 —— 表现是「机器人越用越慢」。
	if len(req.Rules) > maxKeywordRules {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": fmt.Sprintf("最多 %d 条规则", maxKeywordRules),
		})
		return
	}

	if err := s.cfg.Update(func(next *config.Config) error {
		return next.ApplyKeywordRules(req.Rules)
	}); err != nil {
		s.log.Warn("关键词规则保存失败", "err", err)
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	s.log.Info("关键词规则已更新", "count", len(s.cfg.Get().Keyword.Rules))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":    true,
		"rules": s.cfg.Get().KeywordRulesView(),
	})
}

// maxKeywordRules 是规则条数上限。
//
// 30 条对「关键词反应」这种场景已经很多了：它是逐条子串/正则匹配，
// 而每条消息都要跑一遍。
const maxKeywordRules = 30
