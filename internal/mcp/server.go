// Package mcp implements the subset of the Model Context Protocol needed by
// jq-mcp: initialize, tools/list, tools/call and ping over stdio using
// newline-delimited JSON-RPC 2.0.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"

	"github.com/jqhelper/jq-mcp/internal/apierr"
)

// defaultProtocolVersion is used when the client does not request one.
const defaultProtocolVersion = "2024-11-05"

// Tool is a single MCP tool.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
	Handler     func(ctx context.Context, args json.RawMessage) (any, error)
}

// Server dispatches MCP requests to registered tools.
type Server struct {
	name    string
	version string
	log     *slog.Logger

	mu    sync.RWMutex
	tools map[string]*Tool
	order []string

	outMu   sync.RWMutex
	out     io.Writer
	writeMu sync.Mutex

	sinkMu sync.RWMutex
	sinks  []func([]byte)
}

// NewServer creates an empty server.
func NewServer(name, version string, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{name: name, version: version, log: logger, tools: map[string]*Tool{}}
}

// Register adds a tool. Registration order is preserved in tools/list.
func (s *Server) Register(t Tool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.tools[t.Name]; !exists {
		s.order = append(s.order, t.Name)
	}
	s.tools[t.Name] = &t
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int            `json:"code"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// ProgressFunc reports progress for the current tools/call. Clients that send
// _meta.progressToken get notifications/progress; others get a no-op.
type ProgressFunc func(progress, total float64, message string)

type progressKey struct{}

// ProgressFromContext returns the caller's progress reporter (never nil).
func ProgressFromContext(ctx context.Context) ProgressFunc {
	if f, ok := ctx.Value(progressKey{}).(ProgressFunc); ok {
		return f
	}
	return func(float64, float64, string) {}
}

type sessionKey struct{}

// SessionIDFromContext returns the invoking client's session identifier when
// the transport provides one. OpenCode sends it in MCP
// tools/call._meta["ai.opencode/sessionID"].
func SessionIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(sessionKey{}).(string)
	return v
}

// WithSessionID returns a context carrying the client session identifier.
func WithSessionID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionKey{}, id)
}

// Run processes messages until the reader is exhausted or ctx is cancelled.
//
// The reader runs in its own goroutine: a stdio client keeps stdin open
// forever, so a blocking ReadBytes would otherwise ignore SIGTERM and the
// process would only die on SIGKILL.
func (s *Server) Run(ctx context.Context, in io.Reader, out io.Writer) error {
	s.outMu.Lock()
	s.out = out
	s.outMu.Unlock()
	reader := bufio.NewReaderSize(in, 1<<20)

	type chunk struct {
		line []byte
		err  error
	}
	ch := make(chan chunk, 1)
	go func() {
		defer close(ch)
		for {
			line, err := reader.ReadBytes('\n')
			ch <- chunk{line: line, err: err}
			if err != nil {
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case c, ok := <-ch:
			if !ok {
				return nil
			}
			if len(strings.TrimSpace(string(c.line))) > 0 {
				s.handleLine(ctx, c.line, out)
			}
			if c.err != nil {
				if c.err == io.EOF {
					return nil
				}
				return c.err
			}
		}
	}
}

// HandleMessage processes one JSON-RPC message synchronously and returns the
// encoded response (nil for notifications). It is the transport-neutral entry
// point used by the HTTP API (POST /api/rpc, POST /mcp) and by the stdio proxy.
//
// sessionID is attached to the context so tools can route results back to the
// originating client session; it may be empty.
func (s *Server) HandleMessage(ctx context.Context, line []byte, sessionID string) []byte {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		b, _ := json.Marshal(rpcResponse{
			JSONRPC: "2.0",
			Error:   &rpcError{Code: -32700, Message: "无法解析 JSON-RPC 消息"},
		})
		return b
	}
	if len(req.ID) == 0 {
		return nil // notification: no response
	}
	if sessionID != "" {
		ctx = context.WithValue(ctx, sessionKey{}, sessionID)
	}
	var buf bytes.Buffer
	s.dispatch(ctx, req, &buf)
	return bytes.TrimSpace(buf.Bytes())
}

func (s *Server) handleLine(ctx context.Context, line []byte, out io.Writer) {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		s.log.Warn("无法解析 MCP 消息", "err", err)
		return
	}
	// Notifications carry no id and expect no response.
	isNotification := len(req.ID) == 0
	if isNotification {
		if req.Method == "notifications/initialized" || req.Method == "initialized" {
			return
		}
		s.log.Debug("忽略通知", "method", req.Method)
		return
	}

	// Dispatch each request concurrently so a blocking tool (e.g. jq_batch_wait
	// or a long jq_backtest_wait) never stalls the stdio loop and starves other
	// requests. Responses are matched by id; writes are serialized by writeMu.
	go s.dispatch(ctx, req, out)
}

func (s *Server) dispatch(ctx context.Context, req rpcRequest, out io.Writer) {
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = s.handleInitialize(req.Params)
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = s.handleToolsList()
	case "tools/call":
		result, rerr := s.handleToolsCall(ctx, req.Params)
		if rerr != nil {
			resp.Error = rerr
		} else {
			resp.Result = result
		}
	default:
		resp.Error = &rpcError{Code: -32601, Message: fmt.Sprintf("未知方法: %s", req.Method)}
	}
	s.write(out, resp)
}

func (s *Server) handleInitialize(params json.RawMessage) any {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &p)
	version := p.ProtocolVersion
	if version == "" {
		version = defaultProtocolVersion
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities": map[string]any{
			"tools":   map[string]any{"listChanged": false},
			"logging": map[string]any{},
		},
		"serverInfo": map[string]any{"name": s.name, "version": s.version},
	}
}

func (s *Server) handleToolsList() any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	tools := make([]map[string]any, 0, len(s.order))
	for _, name := range s.order {
		t := s.tools[name]
		schema := t.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		tools = append(tools, map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": schema,
		})
	}
	return map[string]any{"tools": tools}
}

func (s *Server) handleToolsCall(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Meta      json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{Code: -32602, Message: "tools/call 参数无效"}
	}
	var meta struct {
		ProgressToken any    `json:"progressToken"`
		SessionID     string `json:"ai.opencode/sessionID"`
	}
	_ = json.Unmarshal(p.Meta, &meta)
	if meta.ProgressToken != nil {
		ctx = context.WithValue(ctx, progressKey{}, ProgressFunc(func(progress, total float64, message string) {
			s.Notify("notifications/progress", map[string]any{
				"progressToken": meta.ProgressToken,
				"progress":      progress,
				"total":         total,
				"message":       message,
			})
		}))
	}
	if meta.SessionID != "" {
		ctx = context.WithValue(ctx, sessionKey{}, meta.SessionID)
	}
	s.mu.RLock()
	tool := s.tools[p.Name]
	s.mu.RUnlock()
	if tool == nil {
		return toolError(apierr.Usage("未知工具: %s", p.Name)), nil
	}
	args := p.Arguments
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	result, err := tool.Handler(ctx, args)
	if err != nil {
		return toolError(apierr.From(err)), nil
	}
	return toolSuccess(result), nil
}

// CallTool invokes a registered tool synchronously. The stdio loop dispatches
// asynchronously; this entry point is for local in-process transports such as
// the HTTP API.
func (s *Server) CallTool(ctx context.Context, name string, args json.RawMessage) (any, error) {
	s.mu.RLock()
	tool := s.tools[name]
	s.mu.RUnlock()
	if tool == nil {
		return nil, apierr.Usage("未知工具: %s", name)
	}
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	return tool.Handler(ctx, args)
}

func toolSuccess(result any) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": resultText(result)}},
		"isError": false,
	}
}

func toolError(e *apierr.Error) map[string]any {
	payload, _ := json.Marshal(map[string]any{"error": e})
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(payload)}},
		"isError": true,
	}
}

func resultText(result any) string {
	switch v := result.(type) {
	case nil:
		return "{}"
	case json.RawMessage:
		if len(v) == 0 {
			return "{}"
		}
		return string(v)
	case []byte:
		return string(v)
	case string:
		return v
	default:
		b, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return fmt.Sprintf("%v", result)
		}
		return string(b)
	}
}

func (s *Server) write(out io.Writer, resp rpcResponse) {
	b, err := json.Marshal(resp)
	if err != nil {
		s.log.Error("序列化 MCP 响应失败", "err", err)
		return
	}
	s.writeRaw(out, b)
}

// Notify sends a server-initiated JSON-RPC notification. It is used for
// completion events (notifications/message) and progress reporting. MCP has no
// server->client tool call, so this is the only push channel available.
//
// Notifications are delivered to the stdio writer (if any) and to every
// registered sink, which lets the HTTP API fan them out over SSE while a stdio
// client is attached at the same time.
func (s *Server) Notify(method string, params any) {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	b, err := json.Marshal(msg)
	if err != nil {
		s.log.Error("序列化 MCP 通知失败", "err", err)
		return
	}
	s.outMu.RLock()
	out := s.out
	s.outMu.RUnlock()
	if out != nil {
		s.writeRaw(out, b)
	}
	s.sinkMu.RLock()
	sinks := append([]func([]byte){}, s.sinks...)
	s.sinkMu.RUnlock()
	for _, fn := range sinks {
		fn(b)
	}
}

// AddNotificationSink registers a transport-specific notification consumer.
// Sinks receive the raw JSON message (no trailing newline).
func (s *Server) AddNotificationSink(fn func([]byte)) {
	if fn == nil {
		return
	}
	s.sinkMu.Lock()
	s.sinks = append(s.sinks, fn)
	s.sinkMu.Unlock()
}

func (s *Server) writeRaw(out io.Writer, b []byte) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	b = append(b, '\n')
	if _, err := out.Write(b); err != nil {
		s.log.Error("写入 MCP 响应失败", "err", err)
	}
}
