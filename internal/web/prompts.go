package web

// Prompt 管理与数据备份。
//
// 与参考实现的对照：
//
//	参考实现                                        我们
//	----------------------------------------------  ---------------------------
//	GET    /api/config/prompts                       GET    /api/prompts
//	GET    /api/config/prompts/{lang}/{file}         GET    /api/prompts?name=
//	GET    .../default                               （同上一并返回 default 字段）
//	PUT    /api/config/prompts/{lang}/{file}         PUT    /api/prompts
//	DELETE /api/config/prompts/{lang}/{file}         DELETE /api/prompts?name=
//	POST   /api/data-transfer/export                 POST   /api/backup/export
//	POST   /api/data-transfer/import                 POST   /api/backup/import
//
// 刻意不做的（参考实现有、我们不需要）：
//   - 提示词版本历史（versions/activate）：那是模型反复调参的场景，
//     我们只有 11 个模板，用 git 管更合适
//   - 异步任务队列 + 进度轮询：我们的数据量在几十 MB 以内，
//     同步导出几秒就完事，加任务队列只是增加失败面
//   - 可选包含 plugins / logs：我们没有插件，日志另有保留期清理
//
// 提示词清单以 internal/prompt/embedded/zh-CN/*.prompt 为全集：
// 规划、回复、附件、图片描述、表情包打标、聊天回想、人物画像、
// 表达学习、黑话提取、黑话含义推断（两份）。
// 新增一份时只需放一个 .prompt 文件 + 在 promptDescriptions 里写一句说明，
// TestPromptCatalogCoversAllCallers 会检查每份被代码读取的模板都已暴露。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/prompt"
)

// promptFileView 是提示词列表里的一项。
type promptFileView struct {
	Name string `json:"name"`
	// Size 是当前生效版本的大小（磁盘覆盖优先，否则内嵌）。
	Size int64 `json:"size"`
	// Customized 表示磁盘上有覆盖版本。
	//
	// 界面靠它区分「改过」和「还是默认」——
	// 不显示这个的话，用户看到的内容和别人不一样却完全不知道原因。
	Customized bool `json:"customized"`
	// DisplayName / Description 帮用户认出这是哪一段。
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
	// Advanced 与参考实现的 .meta.toml 对应：高级项照旧可改，
	// 但前端默认收起来 —— 11 份提示词平铺会让“改哪份”变得难以判断。
	Advanced bool `json:"advanced"`
	// Placeholders 是模板里出现的占位符。
	//
	// 界面把它列出来，是为了让用户知道删掉哪个会让那段内容变空 ——
	// 少了 {bot_name} 模型就不知道自己叫什么，而保存时会报错，
	// 报错之前先看一眼比改完再被拒好。
	Placeholders []string `json:"placeholders"`
}

// promptDescriptions 是内置模板的说明，写在代码里而不是模板文件里：
// 模板本身要能被用户整体替换，说明文字跟着模板走会在替换后消失。
//
// 字段含义对齐参考实现的 .meta.toml（display_name / advanced / description）：
// advanced 的几项不是“危险”，而是“改了容易出奇怪问题、平时不需要动”，
// 前端把它们收在一个可展开的区域里。
var promptDescriptions = map[string]struct {
	name     string
	desc     string
	advanced bool
}{
	"planner": {
		name: "规划提示词",
		desc: "决定机器人什么时候回复、调用哪个工具。改动风险最高的一段。",
	},
	"replyer": {
		name: "回复提示词",
		desc: "把 Planner 的决定写成真正发出去的那句话。",
	},
	"reply_attachments": {
		name:     "附件回复提示词",
		desc:     "带图或表情包时追加给回复模型的说明。",
		advanced: true,
	},
	"emoji_content_analysis": {
		name:     "表情包打标签提示词",
		desc:     "把一张表情包提成情绪/场景标签，决定之后能不能检索到它。",
		advanced: true,
	},
	"mid_term_memory_summary": {
		name: "聊天回想提示词",
		desc: "上下文被裁切时，把即将丢掉的消息压缩成一条长期记忆。",
	},
	"person_profile": {
		name: "人物画像提示词",
		desc: "按固定分节整理某个人是谁、聊过什么、什么关系。",
	},
	"learn_style": {
		name: "表达学习提示词",
		desc: "从群聊里提炼“什么情境下可以怎么说”。",
	},
	"learn_jargon": {
		name: "黑话提取提示词",
		desc: "从群聊里找出看不懂的黑话候选词。",
	},
	"jargon_inference_with_context": {
		name:     "黑话含义推断（带上下文）",
		desc:     "结合用法猜出黑话是什么意思，猜不准时才用下一份。",
		advanced: true,
	},
	"jargon_inference_content_only": {
		name:     "黑话含义推断（只看词条）",
		desc:     "上下文不足时的兑底：只凭词条本身猜含义。",
		advanced: true,
	},
}

// handlePrompts 处理提示词的列表 / 读取 / 保存 / 恢复默认。
//
// 一个路由四种方法，而不是四个路由：提示词就是「一组具名文本文件」，
// 拆成四个路由只是把同一件事写四遍。
func (s *Server) handlePrompts(w http.ResponseWriter, r *http.Request) {
	if s.prompts == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "提示词未加载"})
		return
	}

	switch r.Method {
	case http.MethodGet:
		name := r.URL.Query().Get("name")
		if name == "" {
			s.listPrompts(w)
			return
		}
		s.getPrompt(w, name)
	case http.MethodPut:
		s.savePrompt(w, r)
	case http.MethodDelete:
		s.resetPrompt(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{
			"error": "只支持 GET / PUT / DELETE 方法",
		})
	}
}

func (s *Server) listPrompts(w http.ResponseWriter) {
	// 以「内嵌模板」为全集：磁盘上多出来的文件不算，
	// 那可能是用户手放进去的备份，列出来会让人以为是能编辑的模板。
	names := prompt.EmbeddedNames()
	items := make([]promptFileView, 0, len(names))

	for _, name := range names {
		content, ok := s.prompts.Get(name)
		if !ok {
			continue
		}
		meta := promptDescriptions[name]
		items = append(items, promptFileView{
			Name:         name,
			Size:         int64(len(content)),
			Customized:   s.prompts.Customized(name),
			DisplayName:  meta.name,
			Description:  meta.desc,
			Advanced:     meta.advanced,
			Placeholders: prompt.Placeholders(content),
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })

	writeJSON(w, http.StatusOK, map[string]any{
		"items": items,
		"total": len(items),
		// dir 为空时界面要提示「保存会失败」，避免用户改完点保存才发现。
		"dir":    s.prompts.Dir(),
		"locale": s.prompts.Locale(),
	})
}

func (s *Server) getPrompt(w http.ResponseWriter, name string) {
	content, ok := s.prompts.Get(name)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "提示词不存在"})
		return
	}
	def, hasDefault := prompt.EmbeddedDefault(name)
	writeJSON(w, http.StatusOK, map[string]any{
		"name":         name,
		"content":      content,
		"default":      def,
		"has_default":  hasDefault,
		"customized":   s.prompts.Customized(name),
		"placeholders": prompt.Placeholders(content),
	})
}

func (s *Server) savePrompt(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxPromptBodyBytes)

	var req struct {
		Name    string `json:"name"`
		Content string `json:"content"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	if strings.TrimSpace(req.Content) == "" {
		// 空模板会让 Planner 收到一个空的 system prompt，
		// 模型行为完全不可预期。宁可拒绝。
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "提示词内容不能为空（想恢复默认请用「恢复默认」）",
		})
		return
	}

	// 占位符必须与默认版本一致。参考实现同样校验（config.py:611
	// _build_prompt_validation），理由是少一个占位符不会报错：
	// 那段内容就静默变空（少了 {bot_name} 模型就不知道自己叫什么），
	// 多一个则永远渲染成空串。两种都只表现为「机器人变得很奇怪」。
	if err := prompt.ValidatePlaceholders(req.Name, req.Content); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": err.Error(),
			"hint":  "如果只是想改措辞，请保留原模板里的全部 {xxxx} 占位符；想恢复原样请用「恢复默认」",
		})
		return
	}

	if err := s.prompts.Save(req.Name, req.Content); err != nil {
		s.log.Warn("保存提示词失败", "name", req.Name, "err", err)
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	// 提示词换了要清掉 Planner 的缓存（如果哪一层缓存了渲染结果）。
	// 目前没有缓存，Render 每次现算，所以这里只需要记日志。
	s.log.Info("提示词已更新", "name", req.Name, "bytes", len(req.Content))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"name":    req.Name,
		"content": req.Content,
	})
}

func (s *Server) resetPrompt(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "name 不能为空"})
		return
	}
	if err := s.prompts.Reset(name); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	def, _ := prompt.EmbeddedDefault(name)
	s.log.Info("提示词已恢复默认", "name", name)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"name":    name,
		"content": def,
	})
}

// maxPromptBodyBytes 是提示词保存的请求体上限。
//
// Planner 提示词正文不到 3KB，留 1MB 足够，
// 同时挡住「误把整个日志粘进去」。
const maxPromptBodyBytes = 1 << 20

// ---------------------------------------------------------------------------
// 数据备份
// ---------------------------------------------------------------------------

// handleBackup 导出 / 导入一份数据库快照。
//
// 导出的是 SQLite 文件本身，不是 JSON：
// 我们的「数据」全在库里（消息、记忆、画像、表情包索引、表达、黑话），
// 直接用 VACUUM INTO 拿一份一致的快照，比逐表导 JSON 少一半代码、
// 也不会因为加了新表忘记同步导出逻辑。
//
// 表情包图片在 data/emoji 下，不在库里 —— 导出包里不含它们。
// 这一点必须在界面上写清楚，否则用户以为备份完整。
func (s *Server) handleBackup(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.exportBackup(w, r)
	case http.MethodPost:
		s.importBackup(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{
			"error": "只支持 GET（导出）/ POST（导入）",
		})
	}
}

func (s *Server) exportBackup(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	// 写到临时目录而不是内存：库可能有几百 MB，
	// 一次性读进内存会让整个进程抖一下，而且下载要等全部读完才开始。
	dir, err := os.MkdirTemp("", "omq-backup-*")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer os.RemoveAll(dir)

	name := "oh-my-qqbot-" + time.Now().Format("20060102-150405") + ".db"
	path := filepath.Join(dir, name)
	if err := s.store.BackupTo(ctx, path); err != nil {
		s.log.Error("导出备份失败", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	f, err := os.Open(path)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer f.Close()

	s.log.Info("已导出备份", "name", name)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	if _, err := io.Copy(w, f); err != nil {
		// 已经开始写响应体了，没法改状态码，只能记日志。
		s.log.Warn("写出备份流失败", "err", err)
	}
}

// importBackup 用上传的数据库覆盖当前库。
//
// 这是本项目里最危险的一个操作，所以做了三道保护：
//  1. 必须 POST 且带 confirm=replace（GET 不可能触发）
//  2. 覆盖前先把当前库备份成 .before-import 文件
//  3. 校验上传内容是不是合法的 SQLite（有文件头且不是空文件）
//
// 仍然不做的是「热替换正在使用的库」：连接池还开着，
// 直接覆盖文件会让 SQLite 拿到损坏的句柄。
// 所以导入成功后要求重启 —— 这一点必须如实告诉用户。
func (s *Server) importBackup(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("confirm") != "replace" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "导入会覆盖当前全部数据，需要 confirm=replace 确认",
		})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBackupBodyBytes)

	dir, err := os.MkdirTemp("", "omq-import-*")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer os.RemoveAll(dir)

	tmp := filepath.Join(dir, "upload.db")
	f, err := os.Create(tmp)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if _, err := io.Copy(f, r.Body); err != nil {
		f.Close()
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "读取上传内容失败"})
		return
	}
	f.Close()

	if err := validateSQLiteFile(tmp); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	backupPath, err := s.store.ReplaceFrom(ctx, tmp)
	if err != nil {
		s.log.Error("导入备份失败", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	s.log.Warn("已导入备份，需要重启才能生效", "old_backup", backupPath)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"message": "已写入，重启程序后生效（当前正在使用的库还是旧的）",
		"backup":  backupPath,
		"restart": true,
	})
}

// maxBackupBodyBytes 是导入时的上限。库再大也不该超过 512MB。
const maxBackupBodyBytes = 512 << 20

// validateSQLiteFile 检查前 16 字节是不是 SQLite 文件头。
//
// 只看文件头不做完整校验：真正的完整性检查由后续 open 完成。
// 但文件头能挡住「传了个 zip / 传了个空文件」这类最常见的错。
func validateSQLiteFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("打开上传文件失败: %w", err)
	}
	defer f.Close()

	var head [16]byte
	n, err := io.ReadFull(f, head[:])
	if err != nil || n < 16 {
		return errors.New("上传的文件太小，不是数据库文件")
	}
	if string(head[:]) != "SQLite format 3\x00" {
		return errors.New("上传的文件不是 SQLite 数据库")
	}
	return nil
}
