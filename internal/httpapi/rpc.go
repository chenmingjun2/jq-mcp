// Transport endpoints: raw JSON-RPC over HTTP (/api/rpc) and MCP Streamable
// HTTP (/mcp). Both share mcp.Server.HandleMessage so the tool registry is
// defined exactly once.
package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/jqhelper/jq-mcp/internal/mcp"
)

// ---------------------------------------------------------------- notify hub

// notifyHub fans server-initiated MCP notifications out to SSE subscribers.
type notifyHub struct {
	mu   sync.Mutex
	subs map[chan []byte]struct{}
}

func newNotifyHub() *notifyHub {
	return &notifyHub{subs: map[chan []byte]struct{}{}}
}

func (h *notifyHub) publish(b []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- b:
		default: // slow consumer: drop rather than block the scheduler
		}
	}
}

func (h *notifyHub) subscribe() chan []byte {
	ch := make(chan []byte, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *notifyHub) unsubscribe(ch chan []byte) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
}

func (h *notifyHub) subscribers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// ------------------------------------------------------------------- helpers

// withSession attaches the caller's OpenCode session (if any) to the context.
func withSession(r *http.Request) context.Context {
	sid := r.Header.Get("X-JQ-Session-Id")
	return mcp.WithSessionID(r.Context(), sid)
}

// sessionFromBody pulls _meta["ai.opencode/sessionID"] out of a JSON-RPC
// request. OpenCode includes it on tools/call in both stdio and HTTP modes.
func sessionFromBody(body []byte) string {
	var msg struct {
		Params struct {
			Meta map[string]any `json:"_meta"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		return ""
	}
	if v, ok := msg.Params.Meta["ai.opencode/sessionID"].(string); ok {
		return v
	}
	return ""
}

func originAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // local CLI clients do not send Origin
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	switch u.Hostname() {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

func newSessionID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("mcpses-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func isInitialize(body []byte) bool {
	var msg struct {
		Method string `json:"method"`
	}
	return json.Unmarshal(body, &msg) == nil && msg.Method == "initialize"
}

// ----------------------------------------------------------------- /api/rpc

// rpc is a thin JSON-RPC pipe for the stdio proxy: one request in, one response
// out, no MCP framing. It keeps the proxy stateless.
func (s *server) rpc(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, nil)
		return
	}
	sid := r.Header.Get("X-JQ-Session-Id")
	if sid == "" {
		sid = sessionFromBody(body)
	}
	resp := s.opt.MCP.HandleMessage(r.Context(), body, sid)
	if len(resp) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(resp)
}

// --------------------------------------------------------- /api/notifications

// notifications streams server-initiated messages as SSE. The stdio proxy
// subscribes and re-emits them on stdout so progress/completion notifications
// survive the HTTP hop.
func (s *server) notifications(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, nil)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			_, _ = io.WriteString(w, ": ping\n\n")
			flusher.Flush()
		case msg, ok := <-ch:
			if !ok {
				return
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", msg); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// ---------------------------------------------------------------- /mcp (P2)

// mcpPost implements the POST half of MCP Streamable HTTP. Responses are
// returned as application/json (the spec allows either JSON or SSE here);
// server-initiated messages are delivered on the GET stream.
func (s *server) mcpPost(w http.ResponseWriter, r *http.Request) {
	if !originAllowed(r) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	if !s.tokenOK(r) && s.opt.Token != "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	sid := r.Header.Get("X-JQ-Session-Id")
	if sid == "" {
		sid = sessionFromBody(body)
	}
	resp := s.opt.MCP.HandleMessage(r.Context(), body, sid)
	if len(resp) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if isInitialize(body) {
		w.Header().Set("Mcp-Session-Id", newSessionID())
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(resp)
}

// mcpGet implements the GET half: an SSE stream carrying notifications.
func (s *server) mcpGet(w http.ResponseWriter, r *http.Request) {
	if !originAllowed(r) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	if !s.tokenOK(r) && s.opt.Token != "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	s.notifications(w, r)
}
