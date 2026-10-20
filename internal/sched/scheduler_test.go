package sched

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jqhelper/jq-mcp/internal/jq"
)

// fakeJQ serves canned backtest responses so the polling loop can be tested
// without a browser bridge. Error logs appear from the n-th poll on.
type fakeJQ struct {
	mu          sync.Mutex
	errorCalls  int
	errorAfter  int
	errorLogs   []string
	stats       json.RawMessage
	statsCalls  int
	list        json.RawMessage
	listErr     error
	runResult   jq.BacktestRunResult
	runErr      error
	lastErrLogs []string
}

func (f *fakeJQ) BacktestRun(context.Context, jq.BacktestRunInput) (jq.BacktestRunResult, error) {
	if f.runErr != nil {
		return jq.BacktestRunResult{}, f.runErr
	}
	return f.runResult, nil
}

func (f *fakeJQ) BacktestStats(context.Context, string) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statsCalls++
	return f.stats, nil
}

func (f *fakeJQ) BacktestList(context.Context, jq.BacktestListInput) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.list == nil {
		return json.RawMessage(`{"items":[]}`), nil
	}
	return f.list, nil
}

func (f *fakeJQ) BacktestLogs(_ context.Context, in jq.BacktestLogsInput) (json.RawMessage, error) {
	if !in.Error {
		return json.RawMessage(`{"logs":[]}`), nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errorCalls++
	if f.errorAfter > 0 && f.errorCalls < f.errorAfter {
		return json.RawMessage(`{"logs":[]}`), nil
	}
	f.lastErrLogs = f.errorLogs
	out, _ := json.Marshal(map[string]any{"logs": f.errorLogs})
	return out, nil
}

func newPollTestScheduler(client backtestClient) *Scheduler {
	s := New(nil, nil, 1, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.jq = client
	s.pollInterval = 5 * time.Millisecond
	s.jobTimeout = 2 * time.Second
	s.compileGrace = 20 * time.Millisecond
	return s
}

// TestProcessFailsFastOnErrorLogsAndFiresWebhook covers the contract: a formal
// backtest that writes an error log is marked failed immediately (not on
// timeout), and job.failed / batch.done reach the webhook with the logs.
func TestProcessFailsFastOnErrorLogsAndFiresWebhook(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	fake := &fakeJQ{
		errorAfter: 2,
		errorLogs:  []string{"Traceback (most recent call last):", "NameError: name 'foo' is not defined"},
		stats:      json.RawMessage(`{"metrics":{}}`),
		runResult:  jq.BacktestRunResult{ID: "bt-1", ListID: "list-1", Status: "running"},
	}
	s := newPollTestScheduler(fake)
	b := s.SubmitWithOptions(
		[]Job{{Tag: "t1", StrategyID: "sid", Start: "2024-01-01"}},
		SubmitOptions{CallbackURL: srv.URL, CallbackOn: "both"},
	)

	start := time.Now()
	s.process(b.Jobs[0])
	if elapsed := time.Since(start); elapsed >= s.jobTimeout {
		t.Fatalf("job waited %v; error logs should fail it far sooner", elapsed)
	}

	snap, err := s.Status(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	js := snap.Jobs[0]
	if js.Status != StFailed {
		t.Fatalf("status = %q, want %q", js.Status, StFailed)
	}
	if len(js.ErrorLogs) != 2 || js.ErrorLogs[1] != fake.errorLogs[1] {
		t.Fatalf("errorLogs = %v", js.ErrorLogs)
	}
	if js.Error == "" {
		t.Fatal("expected a summarized error")
	}

	evs, _, _, err := s.Events(b.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, ev := range evs {
		types = append(types, ev.Type)
	}
	want := []string{EvJobStarted, EvJobFailed, EvBatchDone}
	if len(types) != len(want) {
		t.Fatalf("events = %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("events = %v, want %v", types, want)
		}
	}

	s.callbacks.wait()
	mu.Lock()
	defer mu.Unlock()
	var failed, batchDone bool
	for _, body := range bodies {
		switch body["type"] {
		case EvJobFailed:
			failed = true
			job, _ := body["job"].(map[string]any)
			if job == nil || job["status"] != StFailed {
				t.Fatalf("job.failed payload = %v", body)
			}
			logs, _ := job["errorLogs"].([]any)
			if len(logs) != 2 {
				t.Fatalf("webhook errorLogs = %v", job["errorLogs"])
			}
		case EvBatchDone:
			batchDone = true
			counts, _ := body["counts"].(map[string]any)
			if counts[StFailed] != float64(1) {
				t.Fatalf("batch.done counts = %v", body["counts"])
			}
		}
	}
	if !failed || !batchDone {
		t.Fatalf("webhooks = %v, want job.failed and batch.done", bodies)
	}
}

// TestProcessCompletesWhenStatsArrive ensures the error-log poll does not get
// in the way of the normal success path.
func TestProcessCompletesWhenStatsArrive(t *testing.T) {
	fake := &fakeJQ{
		stats:     json.RawMessage(`{"metrics":{"sharpe":"1.2"}}`),
		runResult: jq.BacktestRunResult{ID: "bt-2", ListID: "list-2", Status: "running"},
	}
	s := newPollTestScheduler(fake)
	b := s.Submit([]Job{{Tag: "t2", StrategyID: "sid", Start: "2024-01-01"}})

	s.process(b.Jobs[0])

	snap, err := s.Status(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	js := snap.Jobs[0]
	if js.Status != StDone {
		t.Fatalf("status = %q, want %q (error=%q)", js.Status, StDone, js.Error)
	}
	if len(js.ErrorLogs) != 0 {
		t.Fatalf("unexpected errorLogs = %v", js.ErrorLogs)
	}
}

// TestProcessCompileJobFinishesViaBuildList covers the compile path: compile
// runs never produce metrics, so completion must come from the build list (and
// must not wait for the job timeout).
func TestProcessCompileJobFinishesViaBuildList(t *testing.T) {
	fake := &fakeJQ{
		list:      json.RawMessage(`{"items":[{"id":"bt-3","listId":"list-3","status":"done"}]}`),
		runResult: jq.BacktestRunResult{ID: "bt-3", ListID: "list-3", Status: "running"},
	}
	s := newPollTestScheduler(fake)
	b := s.Submit([]Job{{Tag: "c1", StrategyID: "sid", Start: "2024-01-01", Compile: true}})

	s.process(b.Jobs[0])

	snap, err := s.Status(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	js := snap.Jobs[0]
	if js.Status != StDone {
		t.Fatalf("status = %q, want %q (error=%q)", js.Status, StDone, js.Error)
	}
	if fake.statsCalls != 0 {
		t.Fatalf("compile job polled stats %d times, want 0", fake.statsCalls)
	}
}

// TestProcessCompileJobFailsWhenBuildListReportsFailed ensures a remote failed
// status is not reported as a successful compile.
func TestProcessCompileJobFailsWhenBuildListReportsFailed(t *testing.T) {
	fake := &fakeJQ{
		list:      json.RawMessage(`{"items":[{"id":"bt-4","listId":"list-4","status":"failed"}]}`),
		runResult: jq.BacktestRunResult{ID: "bt-4", ListID: "list-4", Status: "running"},
	}
	s := newPollTestScheduler(fake)
	b := s.Submit([]Job{{Tag: "c2", StrategyID: "sid", Start: "2024-01-01", Compile: true}})

	s.process(b.Jobs[0])

	snap, err := s.Status(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	js := snap.Jobs[0]
	if js.Status != StFailed {
		t.Fatalf("status = %q, want %q", js.Status, StFailed)
	}
	if js.Error == "" {
		t.Fatal("expected an error message")
	}
}

// TestProcessCompileJobFallbackWhenIdsRotate covers the fallback heuristic: the
// build list never exposes our ids, but it is quiet and has a finished run.
func TestProcessCompileJobFallbackWhenIdsRotate(t *testing.T) {
	fake := &fakeJQ{
		list:      json.RawMessage(`{"items":[{"id":"other","listId":"other-list","status":"done"}]}`),
		runResult: jq.BacktestRunResult{ID: "bt-5", ListID: "list-5", Status: "running"},
	}
	s := newPollTestScheduler(fake)
	b := s.Submit([]Job{{Tag: "c3", StrategyID: "sid", Start: "2024-01-01", Compile: true}})

	start := time.Now()
	s.process(b.Jobs[0])
	if elapsed := time.Since(start); elapsed >= s.jobTimeout {
		t.Fatalf("compile job hit the timeout (%v) instead of using the list", elapsed)
	}

	snap, err := s.Status(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if js := snap.Jobs[0]; js.Status != StDone {
		t.Fatalf("status = %q, want %q (error=%q)", js.Status, StDone, js.Error)
	}
}
