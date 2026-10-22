package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jqhelper/jq-mcp/internal/apierr"
	"github.com/jqhelper/jq-mcp/internal/sched"
)

// ------------------------------------------------------------------- fakes

type fakeScheduler struct {
	mu      sync.Mutex
	cap     int
	seq     int
	batches map[string]*sched.Batch
	events  []sched.Event
}

func newFakeScheduler() *fakeScheduler {
	return &fakeScheduler{cap: 10, batches: map[string]*sched.Batch{}}
}

func (f *fakeScheduler) Cap() int { return f.cap }

func (f *fakeScheduler) Accounts() []sched.AccountView { return nil }
func (f *fakeScheduler) SetCap(n int) {
	f.mu.Lock()
	f.cap = n
	f.mu.Unlock()
}

func (f *fakeScheduler) SubmitWithOptions(jobs []sched.Job, opt sched.SubmitOptions) *sched.Batch {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	b := &sched.Batch{ID: fmt.Sprintf("batch-%d", f.seq), CreatedAt: time.Now(), SessionID: opt.SessionID}
	for _, j := range jobs {
		b.Jobs = append(b.Jobs, &sched.JobState{Job: j, BatchID: b.ID, Status: sched.StPending})
	}
	f.batches[b.ID] = b
	f.seq++
	f.events = append(f.events, sched.Event{
		ID: int64(f.seq), Type: sched.EvJobStarted, BatchID: b.ID, SessionID: b.SessionID, At: time.Now(),
		Job: b.Jobs[0],
	})
	f.seq++
	b.Jobs[0].Status = sched.StDone
	f.events = append(f.events, sched.Event{
		ID: int64(f.seq), Type: sched.EvJobDone, BatchID: b.ID, SessionID: b.SessionID, At: time.Now(),
		Job: b.Jobs[0],
	})
	f.seq++
	f.events = append(f.events, sched.Event{
		ID: int64(f.seq), Type: sched.EvBatchDone, BatchID: b.ID, SessionID: b.SessionID, At: time.Now(), Jobs: b.Jobs,
	})
	return b
}

func (f *fakeScheduler) Status(batchID string) (*sched.Batch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.batches[batchID]
	if !ok {
		return nil, apierr.New(apierr.CodeNotFound, "批次 %s 不存在", batchID)
	}
	return b, nil
}

func (f *fakeScheduler) List(limit int) []*sched.Batch {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*sched.Batch, 0, len(f.batches))
	for _, b := range f.batches {
		out = append(out, b)
	}
	return out
}

func (f *fakeScheduler) AllEvents(since int64, limit int) ([]sched.Event, int64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sliceLocked(since, limit)
}

func (f *fakeScheduler) NextAllEvents(ctx context.Context, since int64, timeout time.Duration, limit int) ([]sched.Event, int64, bool) {
	return f.AllEvents(since, limit)
}

func (f *fakeScheduler) NextEvents(ctx context.Context, batchID string, since int64, timeout time.Duration, limit int) (*sched.WaitResult, error) {
	b, err := f.Status(batchID)
	if err != nil {
		return nil, err
	}
	evs, next, more := f.AllEvents(since, limit)
	out := make([]sched.Event, 0, len(evs))
	for _, ev := range evs {
		if ev.BatchID == batchID {
			out = append(out, ev)
		}
	}
	done := true
	for _, js := range b.Jobs {
		if js.Status != sched.StDone && js.Status != sched.StFailed && js.Status != sched.StTimeout {
			done = false
		}
	}
	return &sched.WaitResult{Batch: b, Events: out, NextEventID: next, Done: done, HasMore: more}, nil
}

func (f *fakeScheduler) LastEventID() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int64(f.seq)
}

func (f *fakeScheduler) sliceLocked(since int64, limit int) ([]sched.Event, int64, bool) {
	var out []sched.Event
	for _, ev := range f.events {
		if ev.ID > since {
			out = append(out, ev)
		}
	}
	next := since
	if len(out) > 0 {
		next = out[len(out)-1].ID
	}
	return out, next, false
}

type fakeTools struct{}

func (fakeTools) CallTool(_ context.Context, name string, args json.RawMessage) (any, error) {
	if name == "missing" {
		return nil, apierr.Usage("未知工具: %s", name)
	}
	return map[string]any{"tool": name, "args": json.RawMessage(args)}, nil
}

func testServer(token string) http.Handler {
	return New(Options{
		Version:   "test",
		Token:     token,
		Scheduler: newFakeScheduler(),
		Tools:     fakeTools{},
		Connected: func() bool { return true },
	})
}

// -------------------------------------------------------------------- tests

func TestHealthIsUnauthenticated(t *testing.T) {
	rec := do(t, testServer("secret"), http.MethodGet, "/healthz", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["ok"] != true || out["connected"] != true || out["version"] != "test" {
		t.Fatalf("health = %v", out)
	}
}

func TestAuthToken(t *testing.T) {
	h := testServer("secret")
	if rec := do(t, h, http.MethodGet, "/api/batches", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token status = %d", rec.Code)
	}
	if rec := do(t, h, http.MethodGet, "/api/batches", "Bearer secret", ""); rec.Code != http.StatusOK {
		t.Fatalf("bearer status = %d", rec.Code)
	}
	if rec := do(t, h, http.MethodGet, "/api/batches?token=secret", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("query token status = %d", rec.Code)
	}
}

func TestSubmitAndReadBatch(t *testing.T) {
	h := testServer("")
	body := `{"jobs":[{"tag":"v1","strategyId":"sid","start":"2024-01-01"}],"sessionId":"ses_1"}`
	rec := do(t, h, http.MethodPost, "/api/batches", "", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("submit status = %d: %s", rec.Code, rec.Body)
	}
	var sub map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &sub)
	batchID, _ := sub["batchId"].(string)
	if batchID == "" || sub["count"] != float64(1) || sub["sessionId"] != "ses_1" {
		t.Fatalf("submit = %v", sub)
	}

	rec = do(t, h, http.MethodGet, "/api/batches/"+batchID, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d", rec.Code)
	}
	var bv map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &bv)
	if bv["done"] != true || bv["counts"].(map[string]any)["done"] != float64(1) {
		t.Fatalf("batch view = %v", bv)
	}

	if rec := do(t, h, http.MethodGet, "/api/batches/nope", "", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("missing batch status = %d", rec.Code)
	}
}

func TestSubmitValidation(t *testing.T) {
	h := testServer("")
	for _, body := range []string{
		`{}`,
		`{"jobs":[{"start":"2024-01-01"}]}`,
		`{"jobs":[{"strategyId":"s","start":"2024-01-01"}],"callbackUrl":"ftp://x"}`,
	} {
		if rec := do(t, h, http.MethodPost, "/api/batches", "", body); rec.Code != http.StatusBadRequest {
			t.Fatalf("body %s -> status %d", body, rec.Code)
		}
	}
}

func TestEventFeeds(t *testing.T) {
	h := testServer("")
	body := `{"jobs":[{"tag":"v1","strategyId":"sid","start":"2024-01-01"}],"sessionId":"ses_9"}`
	rec := do(t, h, http.MethodPost, "/api/batches", "", body)
	var sub map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &sub)
	batchID := sub["batchId"].(string)

	rec = do(t, h, http.MethodGet, "/api/events?since=0&wait=0", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("events status = %d", rec.Code)
	}
	var all map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &all)
	evs, _ := all["events"].([]any)
	if len(evs) != 3 {
		t.Fatalf("events = %d, want 3", len(evs))
	}
	first := evs[0].(map[string]any)
	if first["sessionId"] != "ses_9" {
		t.Fatalf("sessionId missing: %v", first)
	}

	rec = do(t, h, http.MethodGet, "/api/batches/"+batchID+"/events?since=0", "", "")
	var one map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &one)
	if one["done"] != true || one["nextEventId"].(float64) <= 0 {
		t.Fatalf("batch events = %v", one)
	}
}

func TestToolProxy(t *testing.T) {
	h := testServer("")
	rec := do(t, h, http.MethodPost, "/api/tools/jq_health", "", `{"arguments":{}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("tool status = %d: %s", rec.Code, rec.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["ok"] != true || out["result"].(map[string]any)["tool"] != "jq_health" {
		t.Fatalf("tool result = %v", out)
	}
	if rec := do(t, h, http.MethodPost, "/api/tools/missing", "", `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing tool status = %d", rec.Code)
	}
}

func do(t *testing.T, h http.Handler, method, path, auth, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}
