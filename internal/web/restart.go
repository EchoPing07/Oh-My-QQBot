package web

// 本文件是「重启程序」接口。
//
// 存在的理由：配置分两类 —— 即时生效的（热更新快照）与需要重启的
// （端口、日志级别、QQ 凭据、数据库路径）。后者改完之后，界面上
// 已经明确标了「需重启」，但用户只能自己去任务管理器结束进程再启动，
// 而这只在「知道可执行文件在哪、用什么参数启动的」时才做得到。
//
// 对应 MaiBot 的 POST /api/webui/system/restart（routers/system.py:1481）。
//
// 安全边界：
//   - 必须显式带 {"confirm":"restart"}，避免误触（尤其是移动端）
//   - 只重启自己：用 os.Executable() + 原工作目录 + 原 os.Args
//   - 新进程启动失败会立刻回报，不会静默留下一个死掉的机器人

import (
	"net/http"
	"os"
	"os/exec"
	"time"
)

// restartSpawnDelay 是「回完 HTTP 响应再启动新进程」的延迟。
//
// 为什么一定要延迟：如果先启动新进程再返回响应，
// 新进程会立刻去 bind 同一个端口，而旧进程还没释放 ——
// 表现是「重启后 WebUI 打不开，要再点一次」。
// 反过来（先返回再退出）也不行：连接被旧进程直接切断，
// 浏览器看到的是网络错误而不是「正在重启」。
//
// 1.2 秒足够把响应刷进 socket，又短到用户不会以为卡了。
const restartSpawnDelay = 1200 * time.Millisecond

// handleRestart 重启主程序。
func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxProbeBodyBytes)

	var req struct {
		Confirm string `json:"confirm"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是合法 JSON"})
		return
	}
	// 显式确认。这不是安全机制（能登录的人本来就能改配置），
	// 而是防止误触：手机上这个按钮挨着「保存」，点错的代价是机器人掉线。
	if req.Confirm != "restart" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "需要 confirm=restart 才能重启",
		})
		return
	}
	if s.restart == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "重启功能未启用（可能是测试环境）",
		})
		return
	}

	// 先把「要重启」这件事排进队列，再回响应。
	//
	// 顺序不能反：如果先回响应再排任务，进程可能在任务被排上之前
	// 就被别的东西关掉了，用户看到「重启成功」但什么都没发生。
	if err := s.restart.Schedule(restartSpawnDelay); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "无法重启: " + err.Error(),
			"hint":  "手动结束进程后重新启动即可；若是打包方式限制（例如作为服务运行），请用服务管理器重启",
		})
		return
	}

	s.log.Info("WebUI 触发重启")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"message": "正在重启，几秒后刷新页面",
		// 前端用这个数字决定「等多久再开始轮询」。
		"delay_ms": restartSpawnDelay.Milliseconds(),
	})
}

// Restarter 负责真正把进程换掉。
//
// 抽成接口而不是在 web 包里直接 os.Exit：
//   - web 包不该管进程生命周期（它连 app 包都不该依赖）
//   - 测试里能塞一个假实现，断言「点了按钮确实排了重启」
type Restarter interface {
	// Schedule 在 delay 之后重启。返回错误表示这个环境不支持重启
	// （例如找不到自己的可执行文件）。
	Schedule(delay time.Duration) error
}

// ExecRestarter 用「起一个新进程 + 自己退出」的方式重启。
//
// 顺序是这里最关键的一件事，写反了就会出现「重启后 WebUI 打不开」：
//
//	延迟 → 释放端口(Release) → 起新进程 → 本进程退出(Exit)
//
// 为什么必须先 Release 再起新进程：新进程启动后立刻会去 bind
// 同一个端口（7800）。Windows 上监听端口是独占的，旧进程不放手，
// 新进程 bind 就失败 —— 而 app.go 里「WebUI 启动失败」只记一条
// 警告、不阻塞主程序，于是机器人还在跑但网页永远打不开，
// 用户只会看到「重启之后界面没了」。
//
// 为什么不能干等进程完全退出再起：那样得让新进程在没人拉起它的
// 情况下自己出现，做不到。「先放端口、再起新进程、最后自己退」
// 是唯一可行的顺序。
type ExecRestarter struct {
	// Args 是启动参数，应包含 -config 等原始参数。
	Args []string
	// Dir 是工作目录。必须保留原目录，否则相对路径的
	// config.toml / data/ / prompts/ 全部找不到。
	Dir string
	// Env 是环境变量。
	Env []string
	// Release 释放监听端口（停 WebUI 的 listener），由 app 包注入。
	//
	// 只停监听、不做完整关闭：新进程起来之后本进程才真正退出。
	Release func()
	// Exit 是「关闭本进程」的回调，由 app 包注入。
	Exit func()
	// Log 用于记录失败原因（走结构化日志）。
	Log func(msg string, fields ...any)
}

// Schedule 实现 Restarter。
func (r *ExecRestarter) Schedule(delay time.Duration) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if delay <= 0 {
		delay = time.Second
	}

	go func() {
		time.Sleep(delay)

		// 1. 先释放端口。没有这一步，新进程 bind 必然失败。
		if r.Release != nil {
			r.Release()
		}

		// 起新进程。
		//
		// 用 exec.Command 而不是 os.StartProcess：前者会处理
		// Windows 上的引号与路径分隔符，跨平台行为一致。
		cmd := exec.Command(exe, r.Args...)
		cmd.Dir = r.Dir
		cmd.Env = append(os.Environ(), r.Env...)
		// 把新进程的输出接到旧进程的 stdout/stderr，否则
		// 双击启动（无控制台）时新进程的日志直接丢失。
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		if err := cmd.Start(); err != nil {
			if r.Log != nil {
				r.Log("重启失败：无法启动新进程，本进程继续运行（WebUI 需手动重启）",
					"err", err.Error())
			}
			// 起不来就不退自己：留一个还能收消息的实例，
			// 总比留下一个什么都不跑的僵尸好。端口已经放掉了，
			// 所以日志里必须说清「WebUI 需手动重启」。
			return
		}

		// 2. 新进程起来了，放掉它。不 Wait：它要活过我们。
		_ = cmd.Process.Release()

		if r.Log != nil {
			r.Log("新进程已启动，本进程退出", "pid", cmd.Process.Pid)
		}

		// 3. 自己退出。给新进程一点时间进入监听状态，
		//    否则短暂窗口内两个进程都没在监听。
		time.Sleep(300 * time.Millisecond)
		if r.Exit != nil {
			r.Exit()
		}
	}()
	return nil
}
