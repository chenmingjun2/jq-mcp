// Package mcp implements the subset of the Model Context Protocol needed by
// jq-mcp: initialize, tools/list, tools/call and ping over stdio using
// newline-delimited JSON-RPC 2.0.
package mcp

import (
	"bufio"
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

// Run processes messages until the reader is exhausted or ctx is cancelled.
func (s *Server) Run(ctx context.Context, in io.Reader, out io.Writer) error {
	s.outMu.Lock()
	s.out = out
	s.outMu.Unlock()
	reader := bufio.NewReaderSize(in, 1<<20)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line, err := reader.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			s.handleLine(ctx, line, out)
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
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
		ProgressToken any `json:"progressToken"`
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
	if out == nil {
		return
	}
	s.writeRaw(out, b)
}

func (s *Server) writeRaw(out io.Writer, b []byte) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	b = append(b, '\n')
	if _, err := out.Write(b); err != nil {
		s.log.Error("写入 MCP 响应失败", "err", err)
	}
}
