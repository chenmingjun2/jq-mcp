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
)

func testScheduler() *Scheduler {
	return New(nil, nil, 1, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestEventLogLifecycleAndCursor(t *testing.T) {
	s := testScheduler()
	b := s.Submit([]Job{{Tag: "t1", StrategyID: "sid", Start: "2024-01-01"}})
	js := b.Jobs[0]

	s.start(js)
	s.finish(js, StDone, "")

	evs, next, more, err := s.Events(b.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if more {
		t.Fatal("unexpected hasMore")
	}
	var types []string
	for _, ev := range evs {
		types = append(types, ev.Type)
	}
	want := []string{EvJobStarted, EvJobDone, EvBatchDone}
	if len(types) != len(want) {
		t.Fatalf("events = %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("events = %v, want %v", types, want)
		}
	}

	// Cursor after the first event returns only the newer ones.
	evs2, next2, _, err := s.Events(b.ID, evs[0].ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs2) != 2 || evs2[0].Type != EvJobDone || next2 != next {
		t.Fatalf("cursor read = %+v next=%d want next=%d", evs2, next2, next)
	}

	res, err := s.Wait(context.Background(), b.ID, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !allTerminal(res) {
		t.Fatal("batch should be terminal")
	}
}

func TestNextEventsWakesOnCompletion(t *testing.T) {
	s := testScheduler()
	b := s.Submit([]Job{{Tag: "t1", StrategyID: "sid", Start: "2024-01-01"}})
	js := b.Jobs[0]
	s.start(js)

	_, cursor, _, err := s.Events(b.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}

	type result struct {
		res *WaitResult
		err error
	}
	ch := make(chan result, 1)
	go func() {
		res, err := s.NextEvents(context.Background(), b.ID, cursor, 2*time.Second, 100)
		ch <- result{res, err}
	}()

	time.Sleep(20 * time.Millisecond)
	s.finish(js, StDone, "")

	select {
	case got := <-ch:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if len(got.res.Events) == 0 || !got.res.Done {
			t.Fatalf("expected events and done, got %+v", got.res)
		}
	case <-time.After(time.Second):
		t.Fatal("NextEvents did not wake on completion")
	}
}

func TestNextEventsTimesOutWithoutEvents(t *testing.T) {
	s := testScheduler()
	b := s.Submit([]Job{{Tag: "t1", StrategyID: "sid", Start: "2024-01-01"}})
	js := b.Jobs[0]
	s.start(js)
	_, cursor, _, err := s.Events(b.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	res, err := s.NextEvents(context.Background(), b.ID, cursor, 60*time.Millisecond, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 0 || res.Done {
		t.Fatalf("unexpected result: %+v", res)
	}
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Fatalf("returned too early: %v", elapsed)
	}
}

func TestWebhookDeliverySignatureAndRetry(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	var headers []http.Header
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, body)
		headers = append(headers, r.Header.Clone())
		mu.Unlock()
		if n == 1 {
			http.Error(w, "try again", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	s := testScheduler()
	s.callbacks = &callbackSender{
		client:   srv.Client(),
		attempts: 2,
		backoff:  time.Millisecond,
		log:      s.log,
		sem:      make(chan struct{}, 1),
	}
	b := s.SubmitWithOptions(
		[]Job{{Tag: "t1", StrategyID: "sid", Start: "2024-01-01"}},
		SubmitOptions{CallbackURL: srv.URL, CallbackSecret: "topsecret", CallbackOn: "batch"},
	)
	js := b.Jobs[0]
	s.start(js)
	s.finish(js, StDone, "")
	s.callbacks.wait()

	mu.Lock()
	defer mu.Unlock()
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if len(bodies) != 2 {
		t.Fatalf("bodies = %d, want 2", len(bodies))
	}
	var payload callbackPayload
	if err := json.Unmarshal(bodies[1], &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Type != EvBatchDone || payload.BatchID != b.ID || payload.Counts[StDone] != 1 {
		t.Fatalf("payload = %+v", payload)
	}
	h := headers[1]
	ts := h.Get("X-JQ-Timestamp")
	if ts == "" || h.Get("X-JQ-Event") != EvBatchDone || h.Get("X-JQ-Delivery") == "" {
		t.Fatalf("missing headers: %v", h)
	}
	if got, want := h.Get("X-JQ-Signature"), "sha256="+sign("topsecret", ts, bodies[1]); got != want {
		t.Fatalf("signature = %q, want %q", got, want)
	}
}

func TestWebhookSkipsJobEventsWhenOnBatch(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Header.Get("X-JQ-Event"))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	s := testScheduler()
	s.callbacks = &callbackSender{
		client:   srv.Client(),
		attempts: 1,
		backoff:  time.Millisecond,
		log:      s.log,
		sem:      make(chan struct{}, 1),
	}
	b := s.SubmitWithOptions(
		[]Job{{Tag: "t1", StrategyID: "sid", Start: "2024-01-01"}},
		SubmitOptions{CallbackURL: srv.URL, CallbackOn: "batch"},
	)
	js := b.Jobs[0]
	s.start(js)
	s.finish(js, StDone, "")
	s.callbacks.wait()

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != EvBatchDone {
		t.Fatalf("events = %v, want [%s]", got, EvBatchDone)
	}
}

func TestWebhookOnJobIncludesStarted(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Header.Get("X-JQ-Event"))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	s := testScheduler()
	s.callbacks = &callbackSender{
		client:   srv.Client(),
		attempts: 1,
		backoff:  time.Millisecond,
		log:      s.log,
		sem:      make(chan struct{}, 1),
	}
	b := s.SubmitWithOptions(
		[]Job{{Tag: "t1", StrategyID: "sid", Start: "2024-01-01"}},
		SubmitOptions{CallbackURL: srv.URL, CallbackOn: "job"},
	)
	js := b.Jobs[0]
	s.start(js)
	s.finish(js, StDone, "")
	s.callbacks.wait()

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("events = %v, want job.started + job.done", got)
	}
	seen := map[string]bool{}
	for _, ev := range got {
		seen[ev] = true
	}
	if !seen[EvJobStarted] || !seen[EvJobDone] || seen[EvBatchDone] {
		t.Fatalf("events = %v", got)
	}
}

func TestWebhookDoesNotRetry4xx(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer srv.Close()

	s := testScheduler()
	s.callbacks = &callbackSender{
		client:   srv.Client(),
		attempts: 3,
		backoff:  time.Millisecond,
		log:      s.log,
		sem:      make(chan struct{}, 1),
	}
	b := s.SubmitWithOptions(
		[]Job{{Tag: "t1", StrategyID: "sid", Start: "2024-01-01"}},
		SubmitOptions{CallbackURL: srv.URL, CallbackOn: "batch"},
	)
	js := b.Jobs[0]
	s.start(js)
	s.finish(js, StDone, "")
	s.callbacks.wait()

	mu.Lock()
	defer mu.Unlock()
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (4xx must not retry)", attempts)
	}
}
