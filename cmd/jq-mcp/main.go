// Command jq-mcp is the MCP server that drives the jqhelper browser extension
// to operate a JoinQuant account.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jqhelper/jq-mcp/internal/app"
)

func main() {
	var (
		configPath    string
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
	if err != nil {
		fmt.Fprintf(os.Stderr, "jq-mcp 退出: %v\n", err)
		os.Exit(1)
	}
}
