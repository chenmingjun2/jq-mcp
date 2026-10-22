// Package app wires configuration, the WebSocket bridge, the browser launcher,
// the jq domain client, the task manager and the MCP server into one process.
//
// Two run modes share the same daemon core:
//
//	jq-mcp serve   daemon only (browser bridge + scheduler + HTTP API + /mcp)
//	jq-mcp proxy   per-client stdio MCP that forwards to a running daemon
//	jq-mcp         legacy combined mode (daemon + stdio in one process)
package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jqhelper/jq-mcp/internal/account"
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
const Version = "0.4.0"

// Mode selects how the process participates in the daemon/client split.
type Mode string

const (
	ModeCombined Mode = ""      // legacy: daemon + stdio in one process
	ModeDaemon   Mode = "serve" // daemon only
	ModeProxy    Mode = "proxy" // stdio MCP forwarding to a daemon
)

// Options are the resolved command-line overrides.
type Options struct {
	Mode           Mode
	Profile        string
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

// Run dispatches to the selected mode.
func Run(ctx context.Context, opts Options) error {
	switch opts.Mode {
	case ModeProxy:
		return RunProxy(ctx, opts)
	case ModeDaemon:
		return runDaemon(ctx, opts, false)
	default:
		return runDaemon(ctx, opts, true)
	}
}

// runDaemon starts the singleton and either serves stdio MCP (legacy combined
// mode) or just blocks until ctx is cancelled.
func runDaemon(ctx context.Context, opts Options, serveStdio bool) error {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: opts.LogLevel}))

	cfg, err := config.Load(opts.ConfigPath)
	if err != nil {
		return err
	}
	applyOverrides(cfg, opts)
	if cfg.Browser.Mode != config.BrowserAttach && cfg.Browser.Mode != config.BrowserLaunch {
		return apierr.Usage("browser mode 必须是 attach 或 launch，收到 %q", cfg.Browser.Mode)
	}
	// A daemon without an HTTP API cannot be discovered or driven by clients.
	if !serveStdio && cfg.HTTPAddr == "" {
		cfg.HTTPAddr = "127.0.0.1:8792"
	}
	if err := cfg.Save(opts.ConfigPath); err != nil {
		return err
	}

	profile := normalizeProfile(opts.Profile)
	lease, err := AcquireInstance(profile)
	if err != nil {
		return err
	}
	defer lease.Close()

	acctReg, err := account.Load(config.AccountsPath())
	if err != nil {
		return err
	}

	// Bridge WebSocket server.
	hub := bridge.NewHub(cfg.Token, cfg.WSPath, logger)
	if def := acctReg.DefaultAlias(); def != "" {
		hub.SetDefaultAccount(def)
	}
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

	daemonID := fmt.Sprintf("%s-%d", profile, os.Getpid())
	logger.Info("jq-mcp 已启动",
		"version", Version,
		"daemonId", daemonID,
		"mode", string(opts.Mode),
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
	sch.SetDaemonID(daemonID)
	sch.SetProvisionDir(filepath.Join(config.Dir(), "accounts"))
	sch.SetMaxFailovers(cfg.Pool.MaxFailovers)
	sch.Start()
	defer sch.Stop()

	pool := newPoolRuntime(acctReg, hub, client, sch, tasks, cfg.Pool.DefaultCap, maxConcurrent, logger)
	pool.start(ctx)
	client.SetAccountResolver(pool.resolveAccount)
	client.SetDefaultAccount(pool.defaultAccount)

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
			"eventId":  ev.ID,
			"daemonId": ev.DaemonID,
			"type":     ev.Type,
			"batchId":  ev.BatchID,
			"at":       ev.At,
		}
		level := "info"
		if ev.Job != nil {
			data["tag"] = ev.Job.Tag
			data["status"] = ev.Job.Status
			if ev.Job.Account != "" {
				data["account"] = ev.Job.Account
			}
			if ev.Job.CreditAccount != "" {
				data["creditAccount"] = ev.Job.CreditAccount
			}
			if ev.Job.CreditRequired {
				data["creditRequired"] = true
				data["creditReason"] = ev.Job.CreditReason
			}
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

	d := deps{jq: client, tasks: tasks, hub: hub, sched: sch, pool: pool}
	registerTools(server, d)
	registerBatchTools(server, d)

	// Optional local HTTP API: scheduler control + a cross-batch event feed for
	// long-lived consumers (OpenCode jq-loop plugin, attach-mode clients).
	if cfg.HTTPAddr != "" {
		handler := httpapi.New(httpapi.Options{
			Version:   Version,
			DaemonID:  daemonID,
			Protocol:  Protocol,
			StartedAt: time.Now(),
			Token:     cfg.Token,
			Scheduler: sch,
			Tools:     server,
			MCP:       server,
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
		httpURL := "http://" + apiLn.Addr().String()
		logger.Info("HTTP API 已启动", "addr", apiLn.Addr().String(), "auth", "token")
		if !loopbackAddr(cfg.HTTPAddr) {
			logger.Warn("HTTP API 监听非回环地址，请确认网络边界与 token 安全", "addr", cfg.HTTPAddr)
		}
		if err := lease.Publish(Descriptor{
			ID:        daemonID,
			Profile:   profile,
			HTTPURL:   httpURL,
			WSAddr:    listener.Addr().String(),
			PID:       os.Getpid(),
			Version:   Version,
			StartedAt: time.Now(),
		}); err != nil {
			logger.Warn("写入 descriptor 失败，客户端将无法自动发现本实例", "err", err)
		}
	}

	if serveStdio {
		return server.Run(ctx, os.Stdin, os.Stdout)
	}
	<-ctx.Done()
	return nil
}

// stdoutMu serializes stdout writes: responses and relayed notifications are
// produced by different goroutines, and a large tools/list response must not be
// interleaved with a notification.
var stdoutMu sync.Mutex

func writeStdout(b []byte) error {
	stdoutMu.Lock()
	defer stdoutMu.Unlock()
	_, err := os.Stdout.Write(b)
	return err
}

// RunProxy is a stateless stdio MCP adapter. It forwards every JSON-RPC message
// to the daemon's /api/rpc and relays server notifications back on stdout.
func RunProxy(ctx context.Context, opts Options) error {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: opts.LogLevel}))
	cfg, err := config.Load(opts.ConfigPath)
	if err != nil {
		return err
	}
	base := ResolveHTTPURL(opts.Profile, firstNonEmpty(opts.HTTPAddr, cfg.HTTPAddr))
	if base == "" {
		return fmt.Errorf("找不到 jq-mcp daemon（profile=%s）。先启动：jq-mcp serve 或 systemctl --user start jq-mcp", normalizeProfile(opts.Profile))
	}
	if _, err := Probe(base, cfg.Token); err != nil {
		return fmt.Errorf("jq-mcp daemon 未运行（%s）：%w\n先启动：jq-mcp serve 或 systemctl --user start jq-mcp", base, err)
	}
	logger.Info("jq-mcp proxy 已连接", "daemon", base, "profile", normalizeProfile(opts.Profile))

	go relayNotifications(ctx, base, cfg.Token, logger)

	reader := bufio.NewReaderSize(os.Stdin, 1<<20)
	// Dispatch each JSON-RPC message in its own goroutine: a slow tool call
	// (e.g. jq_backtest_get while the browser is busy) must not block the
	// stdin loop, otherwise every later request (including jq_batch_submit)
	// queues behind it. Responses are matched by JSON-RPC id; writes are
	// serialized by writeStdout.
	sem := make(chan struct{}, 16)
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		line, err := reader.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			body := append([]byte(nil), line...)
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return nil
			}
			go func() {
				defer func() { <-sem }()
				resp, rerr := postRPC(ctx, base, cfg.Token, body)
				if rerr != nil {
					logger.Error("转发失败", "err", rerr)
					if eb := errorResponse(body, rerr); len(eb) > 0 {
						_ = writeStdout(append(eb, '\n'))
					}
					return
				}
				if len(resp) > 0 {
					_ = writeStdout(append(resp, '\n'))
				}
			}()
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// proxyHTTP is shared so concurrent forwards reuse connections instead of
// opening a new TCP connection per call.
var proxyHTTP = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
	},
}

func postRPC(ctx context.Context, base, token string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/rpc", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := proxyHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("daemon 返回 %s: %s", resp.Status, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// errorResponse builds a JSON-RPC error frame for a request we could not
// forward, so the client fails fast instead of waiting for its own timeout.
func errorResponse(line []byte, err error) []byte {
	var req struct {
		ID any `json:"id"`
	}
	if json.Unmarshal(line, &req) != nil || req.ID == nil {
		return nil
	}
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      req.ID,
		"error":   map[string]any{"code": -32603, "message": err.Error()},
	})
	return b
}

// relayNotifications keeps an SSE subscription open and re-emits every message
// on the proxy's stdout so the MCP client still receives progress/completion
// notifications.
func relayNotifications(ctx context.Context, base, token string, logger *slog.Logger) {
	backoff := time.Second
	for ctx.Err() == nil {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/notifications", nil)
		if err != nil {
			return
		}
		req.Header.Set("Accept", "text/event-stream")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := (&http.Client{}).Do(req)
		if err != nil {
			time.Sleep(backoff)
			backoff = minDuration(backoff*2, 15*time.Second)
			continue
		}
		backoff = time.Second
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 1<<20), 4<<20)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			payload := strings.TrimPrefix(line, "data: ") + "\n"
			if err := writeStdout([]byte(payload)); err != nil {
				return
			}
		}
		_ = resp.Body.Close()
		if ctx.Err() != nil {
			return
		}
		logger.Warn("通知流断开，重连中")
		time.Sleep(backoff)
	}
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
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
