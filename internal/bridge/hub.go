// Package bridge implements the local WebSocket bridge between jq-mcp and the
// jqhelper browser extension. The server never talks to JoinQuant itself; it
// forwards high-level commands to a connected extension and awaits the result.
package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/jqhelper/jq-mcp/internal/apierr"
)

// Hello is the registration payload sent by the extension on connect.
type Hello struct {
	Client      string    `json:"client"`
	Version     string    `json:"version"`
	Browser     string    `json:"browser"`
	LoggedIn    bool      `json:"loggedIn"`
	UserAgent   string    `json:"userAgent"`
	ConnectedAt time.Time `json:"connectedAt"`
}

// Event is a spontaneous notification from the extension (e.g. auth changes).
type Event struct {
	Type string          `json:"type"`
	At   time.Time       `json:"at"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Status is a snapshot of bridge connectivity for diagnostics tools.
type Status struct {
	Connected bool    `json:"connected"`
	Clients   []Hello `json:"clients"`
	LastEvent *Event  `json:"lastEvent,omitempty"`
}

// Hub tracks connected extensions and routes requests to them.
type Hub struct {
	token  string
	path   string
	logger *slog.Logger

	upgrader websocket.Upgrader
	seq      atomic.Uint64

	mu        sync.Mutex
	clients   map[*client]struct{}
	lastHello *Hello
	lastEvent *Event
	eventFn   func(Event)
}

// NewHub creates a hub. token must match the extension's configured token.
func NewHub(token, path string, logger *slog.Logger) *Hub {
	if logger == nil {
		logger = slog.Default()
	}
	return &Hub{
		token:   token,
		path:    path,
		logger:  logger,
		clients: make(map[*client]struct{}),
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1 << 16,
			WriteBufferSize: 1 << 16,
			// The socket only ever binds to loopback, so the default same-origin
			// check (which browsers do not send for extensions) is relaxed.
			CheckOrigin: func(*http.Request) bool { return true },
		},
	}
}

// SetEventHandler registers a callback for extension events.
func (h *Hub) SetEventHandler(fn func(Event)) {
	h.mu.Lock()
	h.eventFn = fn
	h.mu.Unlock()
}

// Path returns the configured websocket path.
func (h *Hub) Path() string { return h.path }

// ServeWS is the http.HandlerFunc performing the websocket upgrade.
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != h.path {
		http.NotFound(w, r)
		return
	}
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		h.logger.Warn("websocket 升级失败", "err", err)
		return
	}
	c := newClient(h, conn)
	h.add(c)
	go c.writePump()
	go c.readPump()
}

func (h *Hub) add(c *client) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	n := len(h.clients)
	h.mu.Unlock()
	h.logger.Info("插件已连接", "clients", n)
}

func (h *Hub) remove(c *client) {
	h.mu.Lock()
	if _, ok := h.clients[c]; !ok {
		h.mu.Unlock()
		return
	}
	delete(h.clients, c)
	n := len(h.clients)
	h.mu.Unlock()
	c.shutdown()
	h.logger.Info("插件已断开", "clients", n)
}

func (h *Hub) setHello(raw json.RawMessage) {
	var hello Hello
	if err := json.Unmarshal(raw, &hello); err != nil {
		h.logger.Warn("hello 解析失败", "err", err)
		return
	}
	hello.ConnectedAt = time.Now()
	h.mu.Lock()
	h.lastHello = &hello
	h.mu.Unlock()
	h.logger.Info("插件注册", "client", hello.Client, "version", hello.Version, "browser", hello.Browser, "loggedIn", hello.LoggedIn)
}

func (h *Hub) emitEvent(raw json.RawMessage) {
	var ev Event
	if err := json.Unmarshal(raw, &ev); err != nil {
		return
	}
	ev.At = time.Now()
	h.mu.Lock()
	h.lastEvent = &ev
	fn := h.eventFn
	h.mu.Unlock()
	if fn != nil {
		fn(ev)
	}
}

func (h *Hub) pick() *client {
	h.mu.Lock()
	defer h.mu.Unlock()
	// Prefer the most recently added live client. Map iteration is random, so
	// fall back to any client when only one is present.
	var chosen *client
	for c := range h.clients {
		if chosen == nil || c.seq > chosen.seq {
			chosen = c
		}
	}
	return chosen
}

// Connected reports whether at least one extension is attached.
func (h *Hub) Connected() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients) > 0
}

// Status returns a diagnostic snapshot.
func (h *Hub) Status() Status {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := Status{Connected: len(h.clients) > 0, LastEvent: h.lastEvent}
	if h.lastHello != nil {
		st.Clients = append(st.Clients, *h.lastHello)
	}
	return st
}

// VerifyToken reports whether the token supplied by the extension is valid.
func (h *Hub) VerifyToken(provided string) bool {
	return provided != "" && provided == h.token
}

// Call forwards a command to a connected extension and waits for the response.
// It returns apierr.NoExtension when no extension is attached.
func (h *Hub) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c := h.pick()
	if c == nil {
		return nil, apierr.NoExtension()
	}
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, apierr.Wrap(apierr.CodeInternal, fmt.Errorf("序列化参数失败: %w", err))
		}
		raw = b
	} else {
		raw = json.RawMessage("{}")
	}
	id := fmt.Sprintf("s-%d", h.seq.Add(1))
	return c.call(ctx, id, method, raw)
}
