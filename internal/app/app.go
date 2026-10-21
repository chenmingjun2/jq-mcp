// Package app wires configuration, the WebSocket bridge, the browser launcher,
// the jq domain client, the task manager and the MCP server into one process.
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/jqhelper/jq-mcp/internal/apierr"
	"github.com/jqhelper/jq-mcp/internal/bridge"
	"github.com/jqhelper/jq-mcp/internal/browser"
	"github.com/jqhelper/jq-mcp/internal/config"
	"github.com/jqhelper/jq-mcp/internal/httpapi"
	"github.com/jqhelper/jq-mcp/internal/jq"
	"github.com/jqhelper/jq-mcp/internal/mcp"
	"github.com/jqhelper/jq-mcp/internal/sched"
	"github.com/jqhelper/jq-mcp/internal/task"
)

// Version is the server version reported during MCP initialize.
const Version = "0.3.0"

// Options are the resolved command-line overrides.
type Options struct {
	ConfigPath     string
	WSAddr         string
	HTTPAddr       string
	Token          string
	BrowserMode    string
	Headless       *bool
	ChromePath     string
	UserDataDir    string
	ExtensionDir   string
	TaskFile       string
	RequestTimeout time.Duration
	LogLevel       slog.Level
	Workers        int // scheduler worker threads
	MaxConcurrent  int // scheduler concurrency cap
}

// Run starts every component and blocks until ctx is cancelled or stdin closes.
func Run(ctx context.Context, opts Options) error {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: opts.LogLevel}))

	cfg, err := config.Load(opts.ConfigPath)
	if err != nil {
		return err
	}
	applyOverrides(cfg, opts)
	if cfg.Browser.Mode != config.BrowserAttach && cfg.Browser.Mode != config.BrowserLaunch {
		return apierr.Usage("browser mode 必须是 attach 或 launch，收到 %q", cfg.Browser.Mode)
	}
	if err := cfg.Save(opts.ConfigPath); err != nil {
		return err
	}

	// Bridge WebSocket server.
	hub := bridge.NewHub(cfg.Token, cfg.WSPath, logger)
	listener, err := net.Listen("tcp", cfg.WSAddr)
	if err != nil {
		return fmt.Errorf("监听 %s 失败: %w", cfg.WSAddr, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc(cfg.WSPath, hub.ServeWS)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(hub.Status())
	})
	httpServer := &http.Server{Handler: mux}
	go func() {
		if err := httpServer.Serve(listener); err != nil && err != http.ErrServerClosed {
			logger.Error("bridge 服务退出", "err", err)
		}
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	logger.Info("jq-mcp 已启动",
		"version", Version,
		"bridge", fmt.Sprintf("ws://%s%s", listener.Addr().String(), cfg.WSPath),
		"browserMode", cfg.Browser.Mode,
		"config", config.DefaultPath(),
	)
	logger.Info("请在 jqhelper 插件选项页填入 bridge 地址与 token", "token", cfg.Token)

	// Managed browser (launch mode only).
	var launcher *browser.Launcher
	if cfg.Browser.Mode == config.BrowserLaunch {
		wsURL := bridgeURL(listener.Addr().String(), cfg.WSPath)
		launcher, err = browser.Launch(cfg.Browser, wsURL, cfg.Token, logger)
		if err != nil {
			// Keep serving: the user may still attach a browser manually.
			logger.Error("启动受管浏览器失败，继续等待插件连接", "err", err)
		} else {
			defer launcher.Stop()
		}
	}

	timeout := opts.RequestTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	client := jq.New(hub, timeout)
	tasks := task.NewManager(cfg.TaskFile)

	// Server-side scheduler: owns concurrency, work-stealing and back-pressure.
	workers := opts.Workers
	if workers <= 0 {
		workers = 16
	}
	maxConcurrent := opts.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = 10
	}
	sch := sched.New(client, tasks, workers, maxConcurrent, logger)
	sch.Start()
	defer sch.Stop()

	server := mcp.NewServer("jq-mcp", Version, logger)

	// Push terminal events to the MCP client as logging notifications. This is
	// the closest thing MCP offers to a server-initiated callback; it lets
	// clients surface completion immediately while jq_batch_wait stays the
	// single blocking primitive for agents.
	sch.SetEventSink(func(ev sched.Event) {
		if ev.Type == sched.EvJobStarted {
			return
		}
		data := map[string]any{
			"eventId": ev.ID,
			"type":    ev.Type,
			"batchId": ev.BatchID,
			"at":      ev.At,
		}
		level := "info"
		if ev.Job != nil {
			data["tag"] = ev.Job.Tag
			data["status"] = ev.Job.Status
			if ev.Job.TaskID != "" {
				data["taskId"] = ev.Job.TaskID
			}
			if ev.Job.RemoteID != "" {
				data["id"] = ev.Job.RemoteID
			}
			if ev.Job.Error != "" {
				data["error"] = ev.Job.Error
			}
			if len(ev.Job.ErrorLogs) > 0 {
				data["errorLogs"] = ev.Job.ErrorLogs
			}
			if ev.Job.Status == sched.StFailed || ev.Job.Status == sched.StTimeout {
				level = "error"
			}
		}
		if len(ev.Jobs) > 0 {
			counts := map[string]int{}
			for _, js := range ev.Jobs {
				counts[js.Status]++
			}
			data["total"] = len(ev.Jobs)
			data["counts"] = counts
		}
		server.Notify("notifications/message", map[string]any{
			"level":  level,
			"logger": "jq-mcp",
			"data":   data,
		})
	})

	d := deps{jq: client, tasks: tasks, hub: hub, sched: sch}
	registerTools(server, d)
	registerBatchTools(server, d)

	// Optional local HTTP API: scheduler control + a cross-batch event feed for
	// long-lived consumers (OpenCode jq-loop plugin, attach-mode clients).
	if cfg.HTTPAddr != "" {
		handler := httpapi.New(httpapi.Options{
			Version:   Version,
			Token:     cfg.Token,
			Scheduler: sch,
			Tools:     server,
			Connected: hub.Connected,
			Logger:    logger,
		})
		apiLn, err := net.Listen("tcp", cfg.HTTPAddr)
		if err != nil {
			return fmt.Errorf("监听 HTTP API %s 失败: %w", cfg.HTTPAddr, err)
		}
		apiServer := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
		go func() {
			if err := apiServer.Serve(apiLn); err != nil && err != http.ErrServerClosed {
				logger.Error("HTTP API 退出", "addr", cfg.HTTPAddr, "err", err)
			}
		}()
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = apiServer.Shutdown(shutdownCtx)
		}()
		logger.Info("HTTP API 已启动", "addr", cfg.HTTPAddr, "auth", "token")
		if !loopbackAddr(cfg.HTTPAddr) {
			logger.Warn("HTTP API 监听非回环地址，请确认网络边界与 token 安全", "addr", cfg.HTTPAddr)
		}
	}

	return server.Run(ctx, os.Stdin, os.Stdout)
}

// loopbackAddr reports whether a listen address is bound to loopback.
func loopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	switch host {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	}
	return false
}

// bridgeURL builds the extension-facing websocket URL from a listen address.
func bridgeURL(addr, path string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		host, port = addr, ""
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	if port == "" {
		return fmt.Sprintf("ws://%s%s", host, path)
	}
	return fmt.Sprintf("ws://%s:%s%s", host, port, path)
}

func applyOverrides(cfg *config.Config, o Options) {
	if o.WSAddr != "" {
		cfg.WSAddr = o.WSAddr
	}
	if o.HTTPAddr != "" {
		cfg.HTTPAddr = o.HTTPAddr
	}
	if o.Token != "" {
		cfg.Token = o.Token
	}
	if o.BrowserMode != "" {
		cfg.Browser.Mode = o.BrowserMode
	}
	if o.Headless != nil {
		cfg.Browser.Headless = *o.Headless
	}
	if o.ChromePath != "" {
		cfg.Browser.ChromePath = o.ChromePath
	}
	if o.UserDataDir != "" {
		cfg.Browser.UserDataDir = o.UserDataDir
	}
	if o.ExtensionDir != "" {
		cfg.Browser.ExtensionDir = o.ExtensionDir
	}
	if o.TaskFile != "" {
		cfg.TaskFile = o.TaskFile
	}
}
