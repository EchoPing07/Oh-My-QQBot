package config

// 本文件实现「QQ 机器人凭据」的网页读写。
//
// 为什么不放进 EditableFields 的平铺清单里：
//   - AppSecret 是密钥，平铺清单的 Snapshot 会把值一起返回给前端
//   - 字段太少（3 个），为它单独造一个平铺分类不值得
//   - 它和 LLM 的 api_key 是同一类问题，共用一套「留空=不改」规则更好维护
//
// 参考实现的对照：MaiBot 把 QQ 凭据放在「适配器管理」页
// （dashboard 的 adapter 配置），因为它支持多个适配器。
// 我们只有一个官方机器人适配器，所以放在「详细设置」页的开头，
// 不单独造一个入口 —— 造了也没有第二种适配器可以选。
//
// 三个必须守住的点：
//  1. **AppSecret 永不出现在任何响应里**（连掩码都不给：掩码会泄露长度，
//     而长度对暴力猜测是有用信息）。
//  2. 前端留空 = 保留原值，`clear_app_secret: true` 才是显式清空。
//     不回显密钥就必然要区分「空」与「不改」（AGENTS.md 第 128 条）。
//  3. 改了必须重启才生效：网关客户端在 app.Start 里用当时的凭据构造，
//     之后只在重连时复用同一个 client，不会重新读配置。

import (
	"fmt"
	"os"
	"strings"
)

// 环境变量名。applyEnv 会用它们覆盖配置文件里的值，
// 所以网页上改完之后如果环境变量还在，重启时会被环境变量盖掉。
const (
	EnvQQAppID     = "OMQ_QQ_APP_ID"
	EnvQQAppSecret = "OMQ_QQ_APP_SECRET"
)

// QQView 是 QQ 凭据的只读视图，可以安全地返回给前端。
//
// 刻意没有 AppSecret 字段：结构体里不存在，就不存在「忘了置空」的可能。
type QQView struct {
	AppID            string `json:"app_id"`
	SecretSet        bool   `json:"secret_set"`
	UnifiedAccountID string `json:"unified_account_id"`
	APIBaseURL       string `json:"api_base_url"`
	Intents          int    `json:"intents"`
	ReconnectDelay   int    `json:"reconnect_delay_sec"`
	// EnvAppID / EnvAppSecret 表示「环境变量里有同名覆盖」。
	//
	// 这一项不告诉用户的话，会得到一个几乎无法自查的现象：
	// 网页上改了凭据、提示保存成功、重启后还是连不上旧账号 ——
	// 因为 applyEnv 在启动时又把环境变量盖回去了。
	EnvAppID     bool `json:"env_app_id"`
	EnvAppSecret bool `json:"env_app_secret"`
	// Ready 表示凭据齐全，缺的部分在 Missing 里。
	Ready bool     `json:"ready"`
	Missing []string `json:"missing"`
}

// QQSnapshot 返回可安全回传的 QQ 配置视图。
func (c *Config) QQSnapshot() QQView {
	if c == nil {
		return QQView{Missing: []string{}}
	}
	missing := c.MissingCredentials()
	if missing == nil {
		missing = []string{}
	}
	return QQView{
		AppID:            c.QQ.AppID,
		SecretSet:        c.QQ.AppSecret != "",
		UnifiedAccountID: c.QQ.UnifiedAccountID,
		APIBaseURL:       c.QQ.APIBaseURL,
		Intents:          c.QQ.Intents,
		ReconnectDelay:   int(c.QQ.ReconnectDelaySec),
		EnvAppID:         os.Getenv(EnvQQAppID) != "",
		EnvAppSecret:     os.Getenv(EnvQQAppSecret) != "",
		Ready:            len(missing) == 0,
		Missing:          missing,
	}
}

// QQPatch 是一次凭据修改请求。
type QQPatch struct {
	AppID string `json:"app_id"`
	// AppSecret 为空表示「不修改原密钥」。
	AppSecret string `json:"app_secret"`
	// ClearAppSecret 显式清空密钥，与「留空不改」区分开。
	ClearAppSecret   bool   `json:"clear_app_secret"`
	UnifiedAccountID string `json:"unified_account_id"`
}

// ApplyQQPatch 应用一次凭据修改。
func (c *Config) ApplyQQPatch(p QQPatch) error {
	appID := strings.TrimSpace(p.AppID)
	if appID != "" && !isDigits(appID) {
		// 最常见的填错是把 AppSecret 粘进了 AppID 框：
		// 两者都是长串，肉眼很难分辨，而错误的 AppID 只会表现为
		// 「鉴权失败」，报错点离填错的地方很远。
		return fmt.Errorf("AppID 只能是数字，当前值里有非数字字符")
	}

	secret := strings.TrimSpace(p.AppSecret)
	if secret != "" && strings.ContainsAny(secret, " \t\r\n") {
		return fmt.Errorf("AppSecret 里不能有空白字符")
	}

	c.QQ.AppID = appID
	switch {
	case p.ClearAppSecret:
		c.QQ.AppSecret = ""
	case secret != "":
		c.QQ.AppSecret = secret
	}
	c.QQ.UnifiedAccountID = strings.TrimSpace(p.UnifiedAccountID)
	return nil
}

// isDigits 判断字符串是否全为数字且非空。
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
