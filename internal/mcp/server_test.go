package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *syncBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *syncBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

func waitContains(t *testing.T, out *syncBuffer, sub string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), sub) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q in output: %s", sub, out.String())
}

func startTestServer(t *testing.T) (*Server, io.WriteCloser, *syncBuffer, func()) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := NewServer("test", "0", logger)
	pr, pw := io.Pipe()
	out := &syncBuffer{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Run(context.Background(), pr, out)
	}()
	fmt.Fprintln(pw, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	waitContains(t, out, "serverInfo")
	stop := func() {
		_ = pw.Close()
		<-done
	}
	return s, pw, out, stop
}

func TestNotifyWritesLoggingNotification(t *testing.T) {
	s, _, out, stop := startTestServer(t)
	defer stop()

	s.Notify("notifications/message", map[string]any{
		"level": "info", "logger": "jq-mcp", "data": map[string]any{"type": "batch.done"},
	})
	waitContains(t, out, `"method":"notifications/message"`)
	if !strings.Contains(out.String(), `"batch.done"`) {
		t.Fatalf("notification missing data: %s", out.String())
	}
}

func TestToolsCallProgressNotification(t *testing.T) {
	s, pw, out, stop := startTestServer(t)
	defer stop()

	s.Register(Tool{
		Name: "probe",
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			ProgressFromContext(ctx)(1, 2, "half way")
			return map[string]any{"ok": true}, nil
		},
	})
	fmt.Fprintln(pw, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"probe","arguments":{},"_meta":{"progressToken":"tok-1"}}}`)
	waitContains(t, out, `"method":"notifications/progress"`)
	if !strings.Contains(out.String(), `"progressToken":"tok-1"`) || !strings.Contains(out.String(), "half way") {
		t.Fatalf("bad progress notification: %s", out.String())
	}
}

func TestSessionIDFromMCPMeta(t *testing.T) {
	s, pw, out, stop := startTestServer(t)
	defer stop()

	s.Register(Tool{
		Name: "probe",
		Handler: func(ctx context.Context, _ json.RawMessage) (any, error) {
			return map[string]string{"sessionId": SessionIDFromContext(ctx)}, nil
		},
	})
	fmt.Fprintln(pw, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"probe","arguments":{},"_meta":{"ai.opencode/sessionID":"ses_123"}}}`)
	waitContains(t, out, `ses_123`)
}

func TestCallToolSynchronous(t *testing.T) {
	s := NewServer("test", "0", slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.Register(Tool{
		Name: "echo",
		Handler: func(_ context.Context, args json.RawMessage) (any, error) {
			return map[string]any{"args": string(args)}, nil
		},
	})
	out, err := s.CallTool(context.Background(), "echo", json.RawMessage(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if out.(map[string]any)["args"] != `{"x":1}` {
		t.Fatalf("result = %v", out)
	}
	if _, err := s.CallTool(context.Background(), "missing", nil); err == nil {
		t.Fatal("expected an error for an unknown tool")
	}
}
