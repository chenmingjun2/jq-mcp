package bridge

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/jqhelper/jq-mcp/internal/apierr"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 90 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 32 << 20 // 32 MiB: backtest source/stats payloads can be large
	sendBuffer     = 64
)

// envelope is the JSON-RPC style frame exchanged over the bridge. It doubles as
// request (ID+Method+Params), response (ID+Result/Error) and notification
// (Method+Params, no ID).
type envelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      string          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *apierr.Error   `json:"error,omitempty"`
}

type client struct {
	hub  *Hub
	conn *websocket.Conn
	send chan []byte
	seq  uint64

	closeOnce sync.Once
	closeCh   chan struct{}

	mu      sync.Mutex
	pending map[string]chan envelope
	authed  bool
}

func newClient(h *Hub, conn *websocket.Conn) *client {
	return &client{
		hub:     h,
		conn:    conn,
		send:    make(chan []byte, sendBuffer),
		seq:     h.seq.Add(1),
		closeCh: make(chan struct{}),
		pending: make(map[string]chan envelope),
	}
}

func (c *client) shutdown() {
	c.closeOnce.Do(func() {
		close(c.closeCh)
		_ = c.conn.Close()
	})
}

// call sends a request and blocks until the matching response, context
// cancellation or disconnect.
func (c *client) call(ctx context.Context, id, method string, params json.RawMessage) (json.RawMessage, error) {
	ch := make(chan envelope, 1)
	c.mu.Lock()
	select {
	case <-c.closeCh:
		c.mu.Unlock()
		return nil, apierr.NoExtension()
	default:
	}
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	frame, err := json.Marshal(envelope{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return nil, apierr.Wrap(apierr.CodeInternal, err)
	}
	select {
	case c.send <- frame:
	case <-c.closeCh:
		return nil, apierr.NoExtension()
	case <-ctx.Done():
		return nil, apierr.Timeout("发送请求超时: %s", method)
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	case <-c.closeCh:
		return nil, apierr.NoExtension()
	case <-ctx.Done():
		return nil, apierr.Timeout("等待插件响应超时: %s", method)
	}
}

func (c *client) readPump() {
	defer c.hub.remove(c)
	c.conn.SetReadLimit(maxMessageSize)
	_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(pongWait))
	})
	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		var env envelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		switch {
		case env.ID != "":
			c.mu.Lock()
			ch := c.pending[env.ID]
			c.mu.Unlock()
			if ch != nil {
				ch <- env
			}
		case env.Method == "hello":
			c.handleHello(env.Params)
		case env.Method == "event":
			c.hub.emitEvent(env.Params)
		case env.Method == "pong":
			// keepalive only
		}
	}
}

func (c *client) handleHello(raw json.RawMessage) {
	// Peek at the token without persisting it anywhere.
	var probe struct {
		Token   string `json:"token"`
		Client  string `json:"client"`
		Version string `json:"version"`
		Browser string `json:"browser"`
	}
	_ = json.Unmarshal(raw, &probe)
	if !c.hub.VerifyToken(probe.Token) {
		c.hub.logger.Warn("插件 token 校验失败，关闭连接",
			"client", probe.Client, "version", probe.Version, "browser", probe.Browser,
			"providedLen", len(probe.Token), "expectedLen", len(c.hub.token))
		c.shutdown()
		return
	}
	c.mu.Lock()
	c.authed = true
	c.mu.Unlock()
	c.hub.setHello(raw)
}

func (c *client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.shutdown()
	}()
	for {
		select {
		case <-c.closeCh:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			_ = c.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			return
		case msg := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
