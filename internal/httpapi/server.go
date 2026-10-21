// Package httpapi exposes jq-mcp's scheduler and tool registry over a small
// local HTTP/JSON API.
//
// It exists so long-lived consumers (the OpenCode jq-loop plugin, the mjs
// client in attach mode, dashboards) can share the one jq-mcp process that owns
// the browser bridge. The extension still connects to exactly one bridge; the
// HTTP API is a read/control surface next to MCP stdio, not a second bridge.
package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jqhelper/jq-mcp/internal/apierr"
	"github.com/jqhelper/jq-mcp/internal/mcp"
	"github.com/jqhelper/jq-mcp/internal/sched"
)

// Scheduler is the slice of *sched.Scheduler the API needs.
type Scheduler interface {
	Cap() int
	SetCap(n int)
	SubmitWithOptions(jobs []sched.Job, opt sched.SubmitOptions) *sched.Batch
	Status(batchID string) (*sched.Batch, error)
	List(limit int) []*sched.Batch
	AllEvents(since int64, limit int) ([]sched.Event, int64, bool)
	NextAllEvents(ctx context.Context, since int64, timeout time.Duration, limit int) ([]sched.Event, int64, bool)
	NextEvents(ctx context.Context, batchID string, since int64, timeout time.Duration, limit int) (*sched.WaitResult, error)
	LastEventID() int64
}

// Tools is the in-process tool registry (mcp.Server.CallTool).
type Tools interface {
	CallTool(ctx context.Context, name string, args json.RawMessage) (any, error)
}

// Options wires the API to the running server.
type Options struct {
	Version   string
	DaemonID  string
	Protocol  int
	StartedAt time.Time
	Token     string // required on /api/* when non-empty
	Scheduler Scheduler
	Tools     Tools
	MCP       *mcp.Server // optional: enables /api/rpc, /api/notifications and /mcp
	Connected func() bool
	Logger    *slog.Logger
	MaxWait   time.Duration // long-poll cap, defaults to 60s
}

// New builds the HTTP handler. The caller owns the listener.
func New(opt Options) http.Handler {
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	if opt.MaxWait <= 0 {
		opt.MaxWait = 60 * time.Second
	}
	s := &server{opt: opt, hub: newNotifyHub()}
	if opt.MCP != nil {
		opt.MCP.AddNotificationSink(s.hub.publish)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.Handle("POST /api/tools/{name}", s.auth(s.callTool))
	mux.Handle("POST /api/batches", s.auth(s.submit))
	mux.Handle("GET /api/batches", s.auth(s.listBatches))
	mux.Handle("GET /api/batches/{id}", s.auth(s.getBatch))
	mux.Handle("GET /api/batches/{id}/events", s.auth(s.batchEvents))
	mux.Handle("GET /api/events", s.auth(s.allEvents))
	if opt.MCP != nil {
		mux.Handle("POST /api/rpc", s.auth(s.rpc))
		mux.Handle("GET /api/notifications", s.auth(s.notifications))
		mux.HandleFunc("POST /mcp", s.mcpPost)
		mux.HandleFunc("GET /mcp", s.mcpGet)
	}
	return mux
}

type server struct {
	opt Options
	hub *notifyHub
}

// ------------------------------------------------------------------ handlers

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	connected := false
	if s.opt.Connected != nil {
		connected = s.opt.Connected()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"version":     s.opt.Version,
		"protocol":    s.opt.Protocol,
		"daemonId":    s.opt.DaemonID,
		"startedAt":   s.opt.StartedAt,
		"connected":   connected,
		"cap":         s.opt.Scheduler.Cap(),
		"lastEventId": s.opt.Scheduler.LastEventID(),
		"instances":   s.hub.subscribers(),
	})
}

func (s *server) callTool(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var body struct {
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if len(body.Arguments) == 0 {
		body.Arguments = json.RawMessage("{}")
	}
	result, err := s.opt.Tools.CallTool(withSession(r), name, body.Arguments)
	if err != nil {
		e := apierr.From(err)
		writeErr(w, statusFor(e), e)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": result})
}

func (s *server) submit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Jobs           []sched.Job `json:"jobs"`
		MaxConcurrent  int         `json:"maxConcurrent"`
		CallbackURL    string      `json:"callbackUrl"`
		CallbackSecret string      `json:"callbackSecret"`
		CallbackOn     string      `json:"callbackOn"`
		SessionID      string      `json:"sessionId"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if len(body.Jobs) == 0 {
		writeErr(w, http.StatusBadRequest, apierr.Usage("jobs 不能为空"))
		return
	}
	for i := range body.Jobs {
		if body.Jobs[i].StrategyID == "" || body.Jobs[i].Start == "" {
			writeErr(w, http.StatusBadRequest, apierr.Usage("jobs[%d] 需要 strategyId 与 start", i))
			return
		}
	}
	if err := validateCallback(body.CallbackURL, body.CallbackOn); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	sessionID := body.SessionID
	if sessionID == "" {
		sessionID = r.Header.Get("X-JQ-Session-Id")
	}
	if body.MaxConcurrent > 0 {
		s.opt.Scheduler.SetCap(body.MaxConcurrent)
	}
	b := s.opt.Scheduler.SubmitWithOptions(body.Jobs, sched.SubmitOptions{
		CallbackURL:    body.CallbackURL,
		CallbackSecret: body.CallbackSecret,
		CallbackOn:     body.CallbackOn,
		SessionID:      sessionID,
	})
	snap, err := s.opt.Scheduler.Status(b.ID)
	if err != nil {
		writeErr(w, statusFor(apierr.From(err)), apierr.From(err))
		return
	}
	resp := map[string]any{
		"batchId":   snap.ID,
		"count":     len(snap.Jobs),
		"status":    "submitted",
		"cap":       s.opt.Scheduler.Cap(),
		"jobs":      snap.Jobs,
		"createdAt": snap.CreatedAt,
	}
	if snap.SessionID != "" {
		resp["sessionId"] = snap.SessionID
	}
	if snap.Callback != nil {
		resp["callback"] = callbackViewOf(snap.Callback)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *server) listBatches(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	batches := s.opt.Scheduler.List(limit)
	out := make([]batchView, 0, len(batches))
	for _, b := range batches {
		out = append(out, viewOf(b))
	}
	writeJSON(w, http.StatusOK, map[string]any{"batches": out, "cap": s.opt.Scheduler.Cap()})
}

func (s *server) getBatch(w http.ResponseWriter, r *http.Request) {
	b, err := s.opt.Scheduler.Status(r.PathValue("id"))
	if err != nil {
		e := apierr.From(err)
		writeErr(w, statusFor(e), e)
		return
	}
	writeJSON(w, http.StatusOK, viewOf(b))
}

func (s *server) batchEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	since, wait, limit := eventParams(r, s.opt.MaxWait)
	res, err := s.opt.Scheduler.NextEvents(r.Context(), id, since, wait, limit)
	if err != nil {
		e := apierr.From(err)
		writeErr(w, statusFor(e), e)
		return
	}
	evs := res.Events
	if evs == nil {
		evs = []sched.Event{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"batch":       viewOf(res.Batch),
		"events":      evs,
		"nextEventId": res.NextEventID,
		"done":        res.Done,
		"hasMore":     res.HasMore,
	})
}

func (s *server) allEvents(w http.ResponseWriter, r *http.Request) {
	since, wait, limit := eventParams(r, s.opt.MaxWait)
	var (
		evs  []sched.Event
		next int64
		more bool
	)
	if wait > 0 {
		evs, next, more = s.opt.Scheduler.NextAllEvents(r.Context(), since, wait, limit)
	} else {
		evs, next, more = s.opt.Scheduler.AllEvents(since, limit)
	}
	if evs == nil {
		evs = []sched.Event{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"events":      evs,
		"nextEventId": next,
		"hasMore":     more,
	})
}

func eventParams(r *http.Request, maxWait time.Duration) (since int64, wait time.Duration, limit int) {
	q := r.URL.Query()
	since, _ = strconv.ParseInt(q.Get("since"), 10, 64)
	if since < 0 {
		since = 0
	}
	secs, _ := strconv.Atoi(q.Get("wait"))
	wait = time.Duration(secs) * time.Second
	if wait > maxWait {
		wait = maxWait
	}
	if wait < 0 {
		wait = 0
	}
	limit, _ = strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = 200
	}
	if limit > 500 {
		limit = 500
	}
	return since, wait, limit
}

// -------------------------------------------------------------------- views

type batchView struct {
	BatchID   string            `json:"batchId"`
	CreatedAt time.Time         `json:"createdAt"`
	SessionID string            `json:"sessionId,omitempty"`
	Callback  *callbackView     `json:"callback,omitempty"`
	Total     int               `json:"total"`
	Counts    map[string]int    `json:"counts"`
	Done      bool              `json:"done"`
	Jobs      []*sched.JobState `json:"jobs"`
}

type callbackView struct {
	URL    string `json:"url"`
	On     string `json:"on"`
	Signed bool   `json:"signed"`
}

func callbackViewOf(c *sched.Callback) *callbackView {
	return &callbackView{URL: c.URL, On: c.On, Signed: c.Secret != ""}
}

func viewOf(b *sched.Batch) batchView {
	v := batchView{
		BatchID:   b.ID,
		CreatedAt: b.CreatedAt,
		SessionID: b.SessionID,
		Total:     len(b.Jobs),
		Counts:    map[string]int{},
		Done:      true,
		Jobs:      b.Jobs,
	}
	for _, js := range b.Jobs {
		v.Counts[js.Status]++
		switch js.Status {
		case sched.StDone, sched.StFailed, sched.StTimeout:
		default:
			v.Done = false
		}
	}
	if b.Callback != nil {
		v.Callback = callbackViewOf(b.Callback)
	}
	return v
}

// ------------------------------------------------------------------- helpers

func (s *server) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.tokenOK(r) {
			writeErr(w, http.StatusUnauthorized, apierr.New(apierr.CodeNotAuthenticated, "HTTP API token 无效"))
			return
		}
		next(w, r)
	})
}

func (s *server) tokenOK(r *http.Request) bool {
	if s.opt.Token == "" {
		return true
	}
	token := r.Header.Get("X-JQ-Token")
	if token == "" {
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			token = strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
		}
	}
	if token == "" {
		token = r.URL.Query().Get("token")
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(s.opt.Token)) == 1
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) *apierr.Error {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		return apierr.Usage("请求体不是合法 JSON: %v", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, e *apierr.Error) {
	writeJSON(w, status, map[string]any{"ok": false, "error": e})
}

func statusFor(e *apierr.Error) int {
	switch e.Code {
	case apierr.CodeUsage:
		return http.StatusBadRequest
	case apierr.CodeNotFound:
		return http.StatusNotFound
	case apierr.CodeNotAuthenticated:
		return http.StatusUnauthorized
	case apierr.CodeNoExtension:
		return http.StatusServiceUnavailable
	case apierr.CodeTimeout:
		return http.StatusGatewayTimeout
	case apierr.CodeAPI, apierr.CodeNetwork:
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

func validateCallback(raw, on string) *apierr.Error {
	switch on {
	case "", "batch", "job", "both":
	default:
		return apierr.Usage("callbackOn 必须是 batch、job 或 both")
	}
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return apierr.Usage("callbackUrl 必须是合法的 http/https URL")
	}
	return nil
}
