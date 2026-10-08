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
	"github.com/jqhelper/jq-mcp/internal/jq"
	"github.com/jqhelper/jq-mcp/internal/mcp"
	"github.com/jqhelper/jq-mcp/internal/task"
)

// Version is the server version reported during MCP initialize.
const Version = "0.1.0"

// Options are the resolved command-line overrides.
type Options struct {
	ConfigPath     string
	WSAddr         string
	Token          string
	BrowserMode    string
	Headless       *bool
	ChromePath     string
	UserDataDir    string
	ExtensionDir   string
	TaskFile       string
	RequestTimeout time.Duration
	LogLevel       slog.Level
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

	server := mcp.NewServer("jq-mcp", Version, logger)
	registerTools(server, deps{jq: client, tasks: tasks, hub: hub})

	return server.Run(ctx, os.Stdin, os.Stdout)
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
