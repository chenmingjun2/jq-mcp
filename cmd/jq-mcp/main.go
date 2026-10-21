// Command jq-mcp is the MCP server that drives the jqhelper browser extension
// to operate a JoinQuant account.
//
// Usage:
//
//	jq-mcp                 legacy: daemon + stdio in one process
//	jq-mcp serve           daemon only (the singleton that owns the browser)
//	jq-mcp proxy           stdio MCP adapter forwarding to the daemon
//	jq-mcp status          show the running daemon for a profile
//	jq-mcp stop            stop the daemon for a profile
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jqhelper/jq-mcp/internal/app"
)

func main() {
	args := os.Args[1:]
	mode := app.ModeCombined
	if len(args) > 0 {
		switch args[0] {
		case "serve":
			mode, args = app.ModeDaemon, args[1:]
		case "proxy":
			mode, args = app.ModeProxy, args[1:]
		case "status", "stop":
			cmd, rest := args[0], args[1:]
			profile := argValue(rest, "profile", "default")
			var err error
			if cmd == "status" {
				err = app.Status(profile)
			} else {
				err = app.Stop(profile)
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "jq-mcp %s: %v\n", cmd, err)
				os.Exit(1)
			}
			return
		}
	}
	os.Args = append([]string{os.Args[0]}, args...)

	var (
		configPath    string
		profile       string
		wsAddr        string
		httpAddr      string
		token         string
		browserMode   string
		headless      bool
		chromePath    string
		userDataDir   string
		extensionDir  string
		taskFile      string
		timeout       time.Duration
		verbose       bool
		showVersion   bool
		workers       int
		maxConcurrent int
	)

	flag.StringVar(&profile, "profile", "default", "实例名（一个浏览器 profile 一个 daemon）")
	flag.StringVar(&configPath, "config", "", "配置文件路径（默认 ~/.config/jq-mcp/config.json）")
	flag.StringVar(&wsAddr, "ws-addr", "", "WebSocket 监听地址，例如 127.0.0.1:8790")
	flag.StringVar(&httpAddr, "http-addr", "", "可选 HTTP API 监听地址，例如 127.0.0.1:8792（供 jq-loop 插件/attach 客户端使用）")
	flag.StringVar(&token, "token", "", "与插件共享的访问 token（默认自动生成）")
	flag.StringVar(&browserMode, "browser-mode", "", "浏览器模式：attach（复用已开浏览器）或 launch（由本程序启动）")
	flag.BoolVar(&headless, "headless", false, "launch 模式下是否无头运行（首次登录请用 --headless=false）")
	flag.StringVar(&chromePath, "chrome-path", "", "Chrome/Chromium 可执行文件路径")
	flag.StringVar(&userDataDir, "user-data-dir", "", "受管浏览器的独立配置目录")
	flag.StringVar(&extensionDir, "extension-dir", "", "jqhelper 插件目录（包含 manifest.json）")
	flag.StringVar(&taskFile, "task-file", "", "回测任务快照文件（可选，用于重启恢复）")
	flag.DurationVar(&timeout, "request-timeout", 60*time.Second, "单次桥请求超时")
	flag.BoolVar(&verbose, "verbose", false, "输出调试日志")
	flag.BoolVar(&showVersion, "version", false, "打印版本号后退出")
	flag.IntVar(&workers, "workers", 16, "批量调度器的工作线程数")
	flag.IntVar(&maxConcurrent, "max-concurrent", 10, "批量调度器的平台并发上限（被拒时自动收紧）")
	flag.Parse()

	if showVersion {
		fmt.Println("jq-mcp", app.Version)
		return
	}

	// Only treat --headless as an override when it was explicitly provided.
	headlessSet := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "headless" {
			headlessSet = true
		}
	})
	var headlessPtr *bool
	if headlessSet {
		headlessPtr = &headless
	}

	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := app.Run(ctx, app.Options{
		Mode:           mode,
		Profile:        profile,
		ConfigPath:     configPath,
		WSAddr:         wsAddr,
		HTTPAddr:       httpAddr,
		Token:          token,
		BrowserMode:    browserMode,
		Headless:       headlessPtr,
		ChromePath:     chromePath,
		UserDataDir:    userDataDir,
		ExtensionDir:   extensionDir,
		TaskFile:       taskFile,
		RequestTimeout: timeout,
		LogLevel:       level,
		Workers:        workers,
		MaxConcurrent:  maxConcurrent,
	})
	var running *app.ErrAlreadyRunning
	if errors.As(err, &running) {
		fmt.Fprintln(os.Stderr, running.Error())
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "jq-mcp 退出: %v\n", err)
		os.Exit(1)
	}
}

// argValue extracts --name value / --name=value from a small arg list.
func argValue(args []string, name, fallback string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--"+name || a == "-"+name {
			if i+1 < len(args) {
				return args[i+1]
			}
		}
		if strings.HasPrefix(a, "--"+name+"=") {
			return strings.TrimPrefix(a, "--"+name+"=")
		}
		if strings.HasPrefix(a, "-"+name+"=") {
			return strings.TrimPrefix(a, "-"+name+"=")
		}
	}
	return fallback
}
