package sched

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jqhelper/jq-mcp/internal/apierr"
	"github.com/jqhelper/jq-mcp/internal/jq"
)

type poolStats struct {
	mu         sync.Mutex
	runs       map[string]int
	failures   map[string]int
	active     map[string]int
	maxActive  map[string]int
	lastSID    map[string]string
	lastCredit map[string]bool
}

func newPoolStats() *poolStats {
	return &poolStats{runs: map[string]int{}, failures: map[string]int{}, active: map[string]int{}, maxActive: map[string]int{}, lastSID: map[string]string{}, lastCredit: map[string]bool{}}
}

type poolClient struct {
	alias string
	stats *poolStats
	err   error
	delay time.Duration
}

func (c *poolClient) BacktestRun(ctx context.Context, in jq.BacktestRunInput) (jq.BacktestRunResult, error) {
	if c.err != nil {
		c.stats.mu.Lock()
		c.stats.failures[c.alias]++
		c.stats.mu.Unlock()
		return jq.BacktestRunResult{}, c.err
	}
	c.stats.mu.Lock()
	c.stats.runs[c.alias]++
	c.stats.lastSID[c.alias] = in.StrategyID
	c.stats.lastCredit[c.alias] = in.UseCredit
	c.stats.active[c.alias]++
	if c.stats.active[c.alias] > c.stats.maxActive[c.alias] {
		c.stats.maxActive[c.alias] = c.stats.active[c.alias]
	}
	n := c.stats.runs[c.alias]
	c.stats.mu.Unlock()
	if c.delay > 0 {
		time.Sleep(c.delay)
	}
	c.stats.mu.Lock()
	c.stats.active[c.alias]--
	c.stats.mu.Unlock()
	return jq.BacktestRunResult{ID: c.alias + "-" + itoa(n), ListID: "list-" + c.alias}, nil
}

func (c *poolClient) BacktestStats(ctx context.Context, id string) (json.RawMessage, error) {
	return json.RawMessage(`{"metrics":{"sharpe":"1.0"}}`), nil
}

func (c *poolClient) BacktestList(ctx context.Context, in jq.BacktestListInput) (json.RawMessage, error) {
	return json.RawMessage(`{"items":[]}`), nil
}

func (c *poolClient) BacktestLogs(ctx context.Context, in jq.BacktestLogsInput) (json.RawMessage, error) {
	return json.RawMessage(`{"logs":[]}`), nil
}

func (c *poolClient) StrategyCreate(ctx context.Context, in jq.StrategyCreateInput) (json.RawMessage, error) {
	return json.Marshal(map[string]string{"id": "sid-" + c.alias})
}

func (c *poolClient) StrategySave(ctx context.Context, in jq.StrategySaveInput) (json.RawMessage, error) {
	return json.Marshal(map[string]string{"id": in.StrategyID})
}

func newPoolTestScheduler(stats *poolStats, errs map[string]error) *Scheduler {
	s := New(nil, nil, 4, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.pollInterval = time.Millisecond
	s.jobTimeout = 3 * time.Second
	s.SetProvisionDir("")
	s.SetMaxFailovers(2)
	s.SetClientFactory(func(alias string) backtestClient {
		return &poolClient{alias: alias, stats: stats, err: errs[alias], delay: 5 * time.Millisecond}
	})
	return s
}

func TestDispatcherSpreadsJobsAndHonorsPerAccountCap(t *testing.T) {
	stats := newPoolStats()
	s := newPoolTestScheduler(stats, nil)
	s.SyncAccounts([]AccountRuntime{
		{Alias: "a1", Enabled: true, Connected: true, LoggedIn: true, Cap: 1},
		{Alias: "a2", Enabled: true, Connected: true, LoggedIn: true, Cap: 1},
	})
	s.Start()
	defer s.Stop()

	b := s.Submit([]Job{
		{Tag: "1", Code: "print(1)", Start: "2024-01-01"},
		{Tag: "2", Code: "print(1)", Start: "2024-01-01"},
		{Tag: "3", Code: "print(1)", Start: "2024-01-01"},
		{Tag: "4", Code: "print(1)", Start: "2024-01-01"},
	})
	snap, err := s.Wait(context.Background(), b.ID, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, js := range snap.Jobs {
		if js.Status != StDone {
			t.Fatalf("job %s status=%s err=%s", js.Tag, js.Status, js.Error)
		}
		if js.Account == "" {
			t.Fatalf("job %s has no account", js.Tag)
		}
	}
	if stats.runs["a1"] != 2 || stats.runs["a2"] != 2 {
		t.Fatalf("runs = %v, want 2/2", stats.runs)
	}
	for _, a := range []string{"a1", "a2"} {
		if stats.maxActive[a] > 1 {
			t.Fatalf("%s exceeded cap: maxActive=%d", a, stats.maxActive[a])
		}
	}
	if s.Cap() != 2 {
		t.Fatalf("aggregate cap = %d", s.Cap())
	}
}

func TestDispatcherFailsOverToHealthyAccount(t *testing.T) {
	stats := newPoolStats()
	s := newPoolTestScheduler(stats, map[string]error{"a1": apierr.NotAuthenticated()})
	s.SyncAccounts([]AccountRuntime{
		{Alias: "a1", Enabled: true, Connected: true, LoggedIn: true, Cap: 1},
		{Alias: "a2", Enabled: true, Connected: true, LoggedIn: true, Cap: 1},
	})
	s.Start()
	defer s.Stop()

	b := s.Submit([]Job{
		{Tag: "1", Code: "print(1)", Start: "2024-01-01"},
		{Tag: "2", Code: "print(1)", Start: "2024-01-01"},
	})
	snap, err := s.Wait(context.Background(), b.ID, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, js := range snap.Jobs {
		if js.Status != StDone {
			t.Fatalf("job %s status=%s err=%s", js.Tag, js.Status, js.Error)
		}
		if js.Account != "a2" {
			t.Fatalf("job %s ran on %s, want a2", js.Tag, js.Account)
		}
	}
	if stats.failures["a1"] == 0 {
		t.Fatal("expected at least one failure on a1")
	}
	if got := stats.lastSID["a2"]; got != "sid-a2" {
		t.Fatalf("a2 ran with strategyId %q; failover must re-provision per account", got)
	}
	if stats.runs["a2"] != 2 {
		t.Fatalf("a2 runs = %d", stats.runs["a2"])
	}
}

func TestDispatcherHonorsPinnedAccount(t *testing.T) {
	stats := newPoolStats()
	s := newPoolTestScheduler(stats, nil)
	s.SyncAccounts([]AccountRuntime{
		{Alias: "a1", Enabled: true, Connected: true, LoggedIn: true, Cap: 1},
		{Alias: "a2", Enabled: true, Connected: true, LoggedIn: true, Cap: 1},
	})
	s.Start()
	defer s.Stop()

	b := s.Submit([]Job{
		{Tag: "p1", Code: "print(1)", Start: "2024-01-01", Account: "a2"},
		{Tag: "p2", Code: "print(1)", Start: "2024-01-01", Account: "a2"},
	})
	snap, err := s.Wait(context.Background(), b.ID, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, js := range snap.Jobs {
		if js.Status != StDone || js.Account != "a2" {
			t.Fatalf("job %s status=%s account=%s", js.Tag, js.Status, js.Account)
		}
	}
	if stats.runs["a1"] != 0 || stats.runs["a2"] != 2 {
		t.Fatalf("runs = %v", stats.runs)
	}
}

// quotaClient rejects free runs with a credit-required message until UseCredit
// is explicitly approved.
type quotaClient struct {
	*poolClient
}

func (q *quotaClient) BacktestRun(ctx context.Context, in jq.BacktestRunInput) (jq.BacktestRunResult, error) {
	if !in.UseCredit {
		q.stats.mu.Lock()
		q.stats.failures[q.alias]++
		q.stats.mu.Unlock()
		return jq.BacktestRunResult{}, apierr.New(apierr.CodeAPI, "免费回测额度已用完，继续将消耗 50000 积分")
	}
	return q.poolClient.BacktestRun(ctx, in)
}

func TestFreeQuotaPausesForConfirmationAndCreditRetry(t *testing.T) {
	stats := newPoolStats()
	s := New(nil, nil, 2, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.pollInterval = time.Millisecond
	s.jobTimeout = 2 * time.Second
	s.SetClientFactory(func(alias string) backtestClient {
		return &quotaClient{poolClient: &poolClient{alias: alias, stats: stats}}
	})
	s.SyncAccounts([]AccountRuntime{
		{Alias: "a1", Enabled: true, Connected: true, LoggedIn: true, Cap: 1},
		{Alias: "a2", Enabled: true, Connected: true, LoggedIn: true, Cap: 1},
	})
	s.Start()
	defer s.Stop()

	b := s.Submit([]Job{{Tag: "q1", Code: "print(1)", Start: "2024-01-01"}})
	res, err := s.Wait(context.Background(), b.ID, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.Jobs[0].Status != StNeedsCredit {
		t.Fatalf("status = %s, want %s", res.Jobs[0].Status, StNeedsCredit)
	}
	if res.Jobs[0].CreditReason == "" || res.Jobs[0].CreditAccount == "" {
		t.Fatalf("missing credit metadata: %+v", res.Jobs[0])
	}
	if res.Jobs[0].UseCredit {
		t.Fatal("free attempt must not set UseCredit")
	}

	if _, err := s.ConfirmCredit(b.ID, true, nil); err != nil {
		t.Fatal(err)
	}
	final, err := s.Wait(context.Background(), b.ID, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	js := final.Jobs[0]
	if js.Status != StDone || !js.UseCredit {
		t.Fatalf("final = %s useCredit=%v err=%s", js.Status, js.UseCredit, js.Error)
	}
	if stats.failures["a1"]+stats.failures["a2"] == 0 {
		t.Fatal("expected at least one free-quota rejection")
	}
}

func TestFreeQuotaDeclineFailsJob(t *testing.T) {
	stats := newPoolStats()
	s := New(nil, nil, 1, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.pollInterval = time.Millisecond
	s.SetClientFactory(func(alias string) backtestClient {
		return &quotaClient{poolClient: &poolClient{alias: alias, stats: stats}}
	})
	s.SyncAccounts([]AccountRuntime{{Alias: "a1", Enabled: true, Connected: true, LoggedIn: true, Cap: 1}})
	s.Start()
	defer s.Stop()

	b := s.Submit([]Job{{Tag: "q2", Code: "print(1)", Start: "2024-01-01"}})
	res, err := s.Wait(context.Background(), b.ID, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.Jobs[0].Status != StNeedsCredit {
		t.Fatalf("status = %s", res.Jobs[0].Status)
	}
	if _, err := s.ConfirmCredit(b.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	snap, err := s.Status(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Jobs[0].Status != StFailed {
		t.Fatalf("status = %s", snap.Jobs[0].Status)
	}
	if !allTerminal(snap) {
		t.Fatal("batch should be terminal after decline")
	}

	// A new free job must pause immediately on a free-exhausted account instead
	// of waiting for the 10-minute submit timeout.
	b2 := s.Submit([]Job{{Tag: "q3", Code: "print(2)", Start: "2024-01-01"}})
	res2, err := s.Wait(context.Background(), b2.ID, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Jobs[0].Status != StNeedsCredit {
		t.Fatalf("second job status = %s, want needs_confirmation", res2.Jobs[0].Status)
	}
	if _, err := s.ConfirmCredit(b2.ID, true, nil); err != nil {
		t.Fatal(err)
	}
	final2, err := s.Wait(context.Background(), b2.ID, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if final2.Jobs[0].Status != StDone || !final2.Jobs[0].UseCredit {
		t.Fatalf("second job final = %s useCredit=%v", final2.Jobs[0].Status, final2.Jobs[0].UseCredit)
	}
}

func TestJobStateJSONUsesAssignedAccount(t *testing.T) {
	js := &JobState{Job: Job{Account: "requested", Code: "print(1)"}, Account: "assigned"}
	b, err := json.Marshal(js)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if !strings.Contains(got, `"account":"assigned"`) || strings.Contains(got, "requested") {
		t.Fatalf("job JSON = %s", got)
	}
}

func TestDispatcherKeepsLegacyStrategyIDOnDefaultAccount(t *testing.T) {
	stats := newPoolStats()
	s := newPoolTestScheduler(stats, nil)
	s.Start()
	defer s.Stop()

	b := s.Submit([]Job{{Tag: "legacy", StrategyID: "sid", Start: "2024-01-01"}})
	snap, err := s.Wait(context.Background(), b.ID, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	js := snap.Jobs[0]
	if js.Status != StDone || js.Account != "default" {
		t.Fatalf("legacy job status=%s account=%s err=%s", js.Status, js.Account, js.Error)
	}
	if stats.runs["default"] != 1 {
		t.Fatalf("runs = %v", stats.runs)
	}
}
