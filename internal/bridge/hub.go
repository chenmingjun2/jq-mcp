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
	"sort"
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

	// Pool fields. Alias is a display hint from the profile's bootstrap config;
	// UID is filled in by the daemon after auth.identity succeeds.
	ID            string `json:"id,omitempty"`
	Alias         string `json:"alias,omitempty"`
	DaemonProfile string `json:"daemonProfile,omitempty"`
	UID           string `json:"uid,omitempty"`
	Nickname      string `json:"nickname,omitempty"`
}

// ClientInfo is a live extension connection. The daemon uses it for account
// binding and diagnostics; identity fields are authoritative only after
// BindAccount.
type ClientInfo struct {
	ID          string    `json:"id"`
	Alias       string    `json:"alias,omitempty"`
	UID         string    `json:"uid,omitempty"`
	Nickname    string    `json:"nickname,omitempty"`
	Connected   bool      `json:"connected"`
	LoggedIn    bool      `json:"loggedIn"`
	Stale       bool      `json:"stale,omitempty"`
	Version     string    `json:"version,omitempty"`
	Browser     string    `json:"browser,omitempty"`
	ConnectedAt time.Time `json:"connectedAt,omitempty"`
	LastSeen    time.Time `json:"lastSeen,omitempty"`
}

// Event is a spontaneous notification from the extension (e.g. auth changes).
type Event struct {
	Type string          `json:"type"`
	At   time.Time       `json:"at"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Status is a snapshot of bridge connectivity for diagnostics tools.
type Status struct {
	Connected bool         `json:"connected"`
	Clients   []Hello      `json:"clients"`
	Live      []ClientInfo `json:"live,omitempty"`
	LastEvent *Event       `json:"lastEvent,omitempty"`
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
	clientFn  func(ClientInfo)
	defaultID string
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

// SetClientHandler registers a callback for connection lifecycle changes. It is
// called in its own goroutine: callers may issue bridge requests from it.
func (h *Hub) SetClientHandler(fn func(ClientInfo)) {
	h.mu.Lock()
	h.clientFn = fn
	h.mu.Unlock()
}

// SetDefaultAccount selects the account used by calls that do not name one. It
// accepts an alias or a uid.
func (h *Hub) SetDefaultAccount(account string) {
	h.mu.Lock()
	h.defaultID = account
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
	h.logger.Info("插件已连接", "clients", n, "id", c.id)
}

func (h *Hub) remove(c *client) {
	h.mu.Lock()
	if _, ok := h.clients[c]; !ok {
		h.mu.Unlock()
		return
	}
	delete(h.clients, c)
	n := len(h.clients)
	info := h.infoLocked(c, false)
	fn := h.clientFn
	h.mu.Unlock()
	c.shutdown()
	h.logger.Info("插件已断开", "clients", n, "id", c.id, "alias", info.Alias)
	if fn != nil {
		go fn(info)
	}
}

func (h *Hub) setHello(c *client, raw json.RawMessage) {
	var hello Hello
	if err := json.Unmarshal(raw, &hello); err != nil {
		h.logger.Warn("hello 解析失败", "err", err)
		return
	}
	hello.ConnectedAt = time.Now()
	hello.ID = c.id
	h.mu.Lock()
	c.hello = hello
	h.lastHello = &hello
	info := h.infoLocked(c, true)
	fn := h.clientFn
	h.mu.Unlock()
	h.logger.Info("插件注册", "client", hello.Client, "version", hello.Version,
		"browser", hello.Browser, "loggedIn", hello.LoggedIn, "id", c.id, "alias", hello.Alias)
	if fn != nil {
		go fn(info)
	}
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

func (h *Hub) infoLocked(c *client, connected bool) ClientInfo {
	info := ClientInfo{
		ID:          c.id,
		Alias:       c.alias,
		UID:         c.uid,
		Nickname:    c.nickname,
		Connected:   connected,
		LoggedIn:    c.hello.LoggedIn,
		Stale:       c.stale,
		Version:     c.hello.Version,
		Browser:     c.hello.Browser,
		ConnectedAt: c.hello.ConnectedAt,
		LastSeen:    time.Now(),
	}
	if info.Alias == "" {
		info.Alias = c.hello.Alias
	}
	if info.Nickname == "" {
		info.Nickname = c.hello.Nickname
	}
	return info
}

func (h *Hub) pickLocked() *client {
	if h.defaultID != "" {
		if c := h.findLocked(h.defaultID); c != nil {
			return c
		}
	}
	var chosen *client
	for c := range h.clients {
		if c.stale {
			continue
		}
		if chosen == nil || c.seq > chosen.seq {
			chosen = c
		}
	}
	return chosen
}

func (h *Hub) findLocked(account string) *client {
	var newest *client
	for c := range h.clients {
		if c.stale {
			continue
		}
		if c.alias == account || c.uid == account {
			if newest == nil || c.seq > newest.seq {
				newest = c
			}
		}
	}
	return newest
}

func (h *Hub) pick() *client {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.pickLocked()
}

// Clients returns every live connection, newest first.
func (h *Hub) Clients() []ClientInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]ClientInfo, 0, len(h.clients))
	for c := range h.clients {
		out = append(out, h.infoLocked(c, true))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ConnectedAt.After(out[j].ConnectedAt) })
	return out
}

// BindAccount attaches a verified identity to a connection. It never trusts the
// hello payload: callers must have obtained the uid from auth.identity.
func (h *Hub) BindAccount(clientID, alias, uid, nickname string) error {
	h.mu.Lock()
	var target *client
	for c := range h.clients {
		if c.id == clientID {
			target = c
			break
		}
	}
	if target == nil {
		h.mu.Unlock()
		return apierr.New(apierr.CodeNoExtension, "连接 %s 已断开", clientID)
	}
	for c := range h.clients {
		if c == target || uid == "" || c.uid != uid {
			continue
		}
		c.stale = true
		h.logger.Warn("同一账号出现重复连接，旧连接标记为 stale", "uid", uid, "old", c.id, "new", target.id)
	}
	target.alias = alias
	target.uid = uid
	target.nickname = nickname
	target.stale = false
	target.hello.Alias = alias
	target.hello.UID = uid
	target.hello.Nickname = nickname
	target.hello.LoggedIn = true
	h.mu.Unlock()
	return nil
}

// Connected reports whether at least one extension is attached.
func (h *Hub) Connected() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if !c.stale {
			return true
		}
	}
	return false
}

// Status returns a diagnostic snapshot.
func (h *Hub) Status() Status {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := Status{Connected: false, LastEvent: h.lastEvent}
	for c := range h.clients {
		hello := c.hello
		hello.ID = c.id
		if c.alias != "" {
			hello.Alias = c.alias
		}
		if c.uid != "" {
			hello.UID = c.uid
		}
		if c.nickname != "" {
			hello.Nickname = c.nickname
		}
		st.Clients = append(st.Clients, hello)
		st.Live = append(st.Live, h.infoLocked(c, true))
		if !c.stale {
			st.Connected = true
		}
	}
	return st
}

// VerifyToken reports whether the token supplied by the extension is valid.
func (h *Hub) VerifyToken(provided string) bool {
	return provided != "" && provided == h.token
}

// Call forwards a command to the default account's extension.
func (h *Hub) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return h.CallAccount(ctx, "", method, params)
}

// CallClient sends a command to one specific connection (used for identity
// probing before an account is bound).
func (h *Hub) CallClient(ctx context.Context, clientID, method string, params any) (json.RawMessage, error) {
	h.mu.Lock()
	var c *client
	for cand := range h.clients {
		if cand.id == clientID {
			c = cand
			break
		}
	}
	h.mu.Unlock()
	if c == nil {
		return nil, apierr.NoExtension()
	}
	return h.callOn(ctx, c, method, params)
}

// CallAccount forwards a command to a named account (alias or uid). An empty
// account uses the configured default, then the newest connection.
func (h *Hub) CallAccount(ctx context.Context, account, method string, params any) (json.RawMessage, error) {
	h.mu.Lock()
	var c *client
	if account == "" {
		c = h.pickLocked()
	} else {
		c = h.findLocked(account)
	}
	h.mu.Unlock()
	if c == nil {
		if account == "" {
			return nil, apierr.NoExtension()
		}
		return nil, apierr.New(apierr.CodeNoExtension, "账号 %s 未连接", account).WithDetail("account", account)
	}
	return h.callOn(ctx, c, method, params)
}

func (h *Hub) callOn(ctx context.Context, c *client, method string, params any) (json.RawMessage, error) {
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
