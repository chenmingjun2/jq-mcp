package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jqhelper/jq-mcp/internal/mcp"
)

func newRPCFixture(t *testing.T) (*httptest.Server, *mcp.Server) {
	t.Helper()
	srv := mcp.NewServer("jq-mcp", "test", slog.Default())
	srv.Register(mcp.Tool{
		Name:        "echo_session",
		Description: "returns the caller session",
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			return map[string]any{"session": mcp.SessionIDFromContext(ctx)}, nil
		},
	})
	h := New(Options{
		Version:   "test",
		DaemonID:  "default-1",
		Protocol:  2,
		Token:     "tok",
		Scheduler: newFakeScheduler(),
		Tools:     srv,
		MCP:       srv,
		Connected: func() bool { return true },
	})
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts, srv
}

func TestRPCPipeCarriesSession(t *testing.T) {
	ts, _ := newRPCFixture(t)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo_session","arguments":{},"_meta":{"ai.opencode/sessionID":"ses_abc"}}}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/rpc", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), `ses_abc`) {
		t.Fatalf("session not propagated: %s", raw)
	}
}

func TestRPCRequiresToken(t *testing.T) {
	ts, _ := newRPCFixture(t)
	resp, err := http.Post(ts.URL+"/api/rpc", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestMCPInitializeAndOrigin(t *testing.T) {
	ts, _ := newRPCFixture(t)
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize status=%d", resp.StatusCode)
	}
	if resp.Header.Get("Mcp-Session-Id") == "" {
		t.Fatal("missing Mcp-Session-Id on initialize")
	}

	req2, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(body))
	req2.Header.Set("Authorization", "Bearer tok")
	req2.Header.Set("Origin", "http://evil.example")
	bad, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Body.Close()
	if bad.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for foreign origin, got %d", bad.StatusCode)
	}
}

func TestNotificationsSSE(t *testing.T) {
	ts, srv := newRPCFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/notifications", nil)
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Give the handler a moment to subscribe, then push a notification.
	deadline := time.Now().Add(2 * time.Second)
	for srvSubscribers(ts) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	srv.Notify("notifications/message", map[string]any{"hello": "world"})

	lines := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 1<<20), 4<<20)
		for scanner.Scan() {
			text := scanner.Text()
			if strings.HasPrefix(text, "data: ") {
				lines <- text
				return
			}
		}
	}()
	select {
	case line := <-lines:
		if !strings.Contains(line, "hello") {
			t.Fatalf("unexpected SSE line: %q", line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for SSE notification")
	}
}

// srvSubscribers reads the health endpoint to learn whether the SSE handler has
// registered its channel yet.
func srvSubscribers(ts *httptest.Server) int {
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	var h struct {
		Instances int `json:"instances"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&h)
	return h.Instances
}
