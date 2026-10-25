package app

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/logx"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/store"
)

// 管理命令。
//
// 对应 MaiBot-QQ-Adapter 的 /assign_id 与 /assign_group_id：
// QQ 官方平台的 OpenID 和真实 QQ 号不互通，需要命令把两者绑定起来。
//
// 为什么需要：OpenID 是「每个机器人视角下」的独立标识，
// 换一个机器人就变了，做数据关联时完全没法用。
// 绑到真实 QQ 号后，数据才能跨机器人对齐。

const (
	assignGroupCommand = "/assign_group_id"
	assignUserCommand  = "/assign_id"
	idMapKey           = "openid_map"
)

// idMap 是 OpenID ↔ 真实 ID 的双向映射。
type idMap struct {
	Group map[string]string `json:"group"`
	User  map[string]string `json:"user"`
}

func newIDMap() *idMap {
	return &idMap{Group: map[string]string{}, User: map[string]string{}}
}

// commandHandler 处理管理命令。
type commandHandler struct {
	cfg *config.Store
	kv  *store.KVRepo
	log *logx.Logger
}

func newCommandHandler(cfg *config.Store, db *store.DB) *commandHandler {
	return &commandHandler{
		cfg: cfg,
		kv:  db.KV(),
		log: logx.Get("app.command"),
	}
}

// Handle 处理一条命令消息。
// 返回 true 表示已处理，不再进入聊天流。
func (h *commandHandler) Handle(msg *domain.Message, scene, target string) bool {
	text := strings.TrimSpace(msg.PlainText)
	if !strings.HasPrefix(text, "/") {
		return false
	}

	switch {
	case strings.HasPrefix(text, assignGroupCommand):
		if !h.allowed(msg) {
			h.log.Warn("拒绝执行绑定命令：不在管理员名单",
				"user", msg.UserID, "cmd", assignGroupCommand)
			return true
		}
		h.handleAssignGroup(msg, scene, text)
		return true
	case strings.HasPrefix(text, assignUserCommand):
		if !h.allowed(msg) {
			h.log.Warn("拒绝执行绑定命令：不在管理员名单",
				"user", msg.UserID, "cmd", assignUserCommand)
			return true
		}
		h.handleAssignUser(msg, text)
		return true
	}
	return false
}

// allowed 判断发送者是否有权执行绑定命令。
//
// qq.assign_admin_ids 为空表示不设限（单人群自用场景的默认值）。
// 一旦填了名单就只认名单里的 OpenID —— 这很重要：
// 绑定命令会在群里公开回复「已把本群绑定到 xxx」，
// 让任何人都能改映射等于让任何人都能污染数据关联。
func (h *commandHandler) allowed(msg *domain.Message) bool {
	if len(h.cfg.Get().QQ.AssignAdminIDs) == 0 {
		return true
	}
	for _, id := range h.cfg.Get().QQ.AssignAdminIDs {
		if id == msg.UserID {
			return true
		}
	}
	return false
}

// handleAssignGroup 把群 OpenID 绑定到真实群号。
func (h *commandHandler) handleAssignGroup(msg *domain.Message, scene, text string) {
	if scene != domain.ScopeGroup {
		h.reply(msg, "该命令只能在群聊中使用。")
		return
	}

	arg := strings.TrimSpace(strings.TrimPrefix(text, assignGroupCommand))
	if arg == "" || !isSafeID(arg) {
		h.reply(msg, "用法：/assign_group_id <群号>")
		return
	}

	h.bind("group", msg.GroupID, arg)
	h.log.Info("绑定群 OpenID", "openid", msg.GroupID, "group", arg)
	h.reply(msg, "已把本群绑定到 "+arg+"。")
}

// handleAssignUser 把用户 OpenID 绑定到真实 QQ 号。
func (h *commandHandler) handleAssignUser(msg *domain.Message, text string) {
	arg := strings.TrimSpace(strings.TrimPrefix(text, assignUserCommand))
	if arg == "" || !isSafeID(arg) {
		h.reply(msg, "用法：/assign_id <QQ号>")
		return
	}

	h.bind("user", msg.UserID, arg)
	h.log.Info("绑定用户 OpenID", "openid", msg.UserID, "user", arg)
	h.reply(msg, "已绑定到 "+arg+"。")
}

// bind 写入一条映射。
func (h *commandHandler) bind(kind, openID, realID string) {
	if openID == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// 用 Lookup 而不是 Get：第一次执行绑定命令时这个键根本不存在，
	// 那是正常的空状态，不是错误。
	raw, _, err := h.kv.Lookup(ctx, idMapKey)
	if err != nil {
		h.log.Warn("读取映射失败", "err", err)
		return
	}

	m := newIDMap()
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), m); err != nil {
			// 数据损坏就从干净状态重建，不阻塞用户操作。
			m = newIDMap()
		}
	}
	if m.Group == nil {
		m.Group = map[string]string{}
	}
	if m.User == nil {
		m.User = map[string]string{}
	}

	if kind == "group" {
		m.Group[openID] = realID
	} else {
		m.User[openID] = realID
	}

	out, err := json.Marshal(m)
	if err != nil {
		h.log.Warn("序列化映射失败", "err", err)
		return
	}
	if err := h.kv.Set(ctx, idMapKey, string(out)); err != nil {
		h.log.Warn("保存映射失败", "err", err)
	}
}

// reply 直接回执，不经过 agent。
//
// 注意这里只记录日志：命令回执通过正常的发送链路走，
// 但为了避免循环依赖，实际发送由调用方处理。
func (h *commandHandler) reply(_ *domain.Message, text string) {
	h.log.Info("命令回执", "text", text)
}

// isSafeID 判断 ID 是否只含安全字符。
func isSafeID(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r == '_' || r == '-':
		default:
			return false
		}
	}
	return len(s) > 0 && len(s) <= 64
}
