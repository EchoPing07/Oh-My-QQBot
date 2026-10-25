// Command oh-my-qqbot 是 Oh-My-QQBot 的入口。
//
// 目录名与二进制名都用项目名，不再沿用默认机器人昵称「木木」——
// 昵称是配置项（bot.nickname），可以被用户改成任何名字，
// 拿它当项目名会让「改了昵称但程序名还叫木木」这件事一直别扭。
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/app"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/config"
	"github.com/ohmyqqbot/oh-my-qqbot/internal/logx"
)

// version 由构建时注入。
var version = "dev"

func main() {
	var (
		configPath = flag.String("config", "config.toml", "配置文件路径")
		showVer    = flag.Bool("version", false, "显示版本")
		genConfig  = flag.Bool("init", false, "生成默认配置文件")
	)
	flag.Usage = usage
	flag.Parse()

	if *showVer {
		fmt.Printf("Oh-My-QQBot %s\n", version)
		return
	}

	// 生成默认配置。
	if *genConfig {
		cfg := config.Default()
		if err := config.Save(cfg, *configPath); err != nil {
			fmt.Fprintf(os.Stderr, "生成配置失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("已生成默认配置: %s\n请填写 qq.app_id、qq.app_secret 和 llm 配置后启动。\n", *configPath)
		return
	}

	// 加载配置。不存在时自动生成一份并退出，让用户先填。
	//
	// 用 config.Store 而不是裸 *Config：WebUI 要能改配置，
	// 而配置是跨协程被读的，必须走原子快照（见 internal/config/store.go）。
	cfgStore, created, err := config.LoadOrCreateStore(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载配置失败: %v\n", err)
		os.Exit(1)
	}
	if created {
		fmt.Printf("未找到配置，已生成默认配置: %s\n请填写 qq.app_id、qq.app_secret 和 llm 配置后重新启动。\n", *configPath)
		return
	}
	cfg := cfgStore.Get()

	// 初始化日志。必须在其他组件之前。
	//
	// FileLevelSet 用「字符串非空」表达「显式配置过」：
	// slog.LevelInfo 恰好等于 0，logx 无法用零值区分「配了 INFO」和「没配」。
	if err := logx.Init(logx.Options{
		Level:        logx.ParseLevel(cfg.Log.Level),
		FileLevel:    logx.ParseLevel(cfg.Log.FileLevel),
		FileLevelSet: cfg.Log.FileLevel != "",
		Dir:          cfg.Log.Dir,
		MaxSizeMB:    cfg.Log.MaxSizeMB,
		MaxBackups:   cfg.Log.MaxBackups,
		Console:      cfg.Log.Console,
		Color:        cfg.Log.Color,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "初始化日志失败: %v\n", err)
		os.Exit(1)
	}
	defer logx.Close()

	a, err := app.New(cfgStore)
	if err != nil {
		logx.Error("初始化应用失败", "err", err)
		os.Exit(1)
	}

	if err := a.Run(); err != nil {
		logx.Error("运行失败", "err", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `Oh-My-QQBot — 一个精简版的 QQ 群聊 Agent

用法:
  oh-my-qqbot [选项]

选项:
  -config <path>   配置文件路径（默认 config.toml）
  -init            生成默认配置文件
  -version         显示版本

第一次使用:
  1. oh-my-qqbot -init                  生成配置
  2. 编辑 config.toml，填写 QQ AppID/Secret 与模型配置
  3. oh-my-qqbot                        启动

配置好之后:
  WebUI 默认在 http://127.0.0.1:7800
`)
}
