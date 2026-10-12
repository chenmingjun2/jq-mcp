// Package sched implements a server-side backtest scheduler with work-stealing
// workers and a concurrency cap that adapts to JoinQuant's parallel limit.
//
// The point: clients declare jobs and read results; the engine owns concurrency,
// back-pressure, terminal waiting and task bookkeeping. This removes the need
// for every client to re-implement "keep the parallel slots full".
package sched

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/jqhelper/jq-mcp/internal/apierr"
	"github.com/jqhelper/jq-mcp/internal/jq"
	"github.com/jqhelper/jq-mcp/internal/task"
)

// Job status values.
const (
	StPending = "pending"
	StRunning = "running"
	StDone    = "done"
	StFailed  = "failed"
	StTimeout = "timeout"
)

// Job is one backtest to run.
type Job struct {
	Tag        string  `json:"tag,omitempty"`
	StrategyID string  `json:"strategyId"`
	Start      string  `json:"start"`
	End        string  `json:"end,omitempty"`
	Capital    float64 `json:"capital,omitempty"`
	Frequency  string  `json:"frequency,omitempty"`
	Compile    bool    `json:"compile,omitempty"`
	UseCredit  bool    `json:"useCredit,omitempty"`
	Priority   int     `json:"priority,omitempty"`
}

// JobState is a Job plus its live status/result.
type JobState struct {
	Job
	Status    string          `json:"status"`
	TaskID    string          `json:"taskId,omitempty"`
	RemoteID  string          `json:"id,omitempty"`
	ListID    string          `json:"listId,omitempty"`
	Metrics   json.RawMessage `json:"metrics,omitempty"`
	Error     string          `json:"error,omitempty"`
	StartedAt time.Time       `json:"startedAt,omitempty"`
	EndedAt   time.Time       `json:"endedAt,omitempty"`
}

// Batch groups submitted jobs.
type Batch struct {
	ID        string      `json:"batchId"`
	CreatedAt time.Time   `json:"createdAt"`
	Jobs      []*JobState `json:"jobs"`
}

// Scheduler owns the queue, the workers and the concurrency cap.
type Scheduler struct {
	mu            sync.Mutex
	cond          *sync.Cond
	batches       map[string]*Batch
	order         []string
	queue         chan *JobState
	workers       int
	maxConcurrent int
	inFlight      int
	jq            *jq.Client
	tasks         *task.Manager
	log           *slog.Logger
	stop          chan struct{}
	stopOnce      sync.Once
	wg            sync.WaitGroup
	seq           int
	pollInterval  time.Duration
	jobTimeout    time.Duration
}

// New builds a scheduler. workers<=0 defaults to 16; maxConcurrent<=0 defaults to 1.
func New(client *jq.Client, tasks *task.Manager, workers, maxConcurrent int, logger *slog.Logger) *Scheduler {
	if workers <= 0 {
		workers = 16
	}
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	if logger == nil {
		logger = slog.Default()
	}
	s := &Scheduler{
		batches:       map[string]*Batch{},
		queue:         make(chan *JobState, 8192),
		workers:       workers,
		maxConcurrent: maxConcurrent,
		jq:            client,
		tasks:         tasks,
		log:           logger,
		stop:          make(chan struct{}),
		pollInterval:  3 * time.Second,
		jobTimeout:    2 * time.Hour,
	}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// Start launches the worker pool.
func (s *Scheduler) Start() {
	for i := 0; i < s.workers; i++ {
		s.wg.Add(1)
		go s.worker()
	}
}

// Stop signals workers to exit and waits for them.
func (s *Scheduler) Stop() {
	s.stopOnce.Do(func() { close(s.stop) })
	s.cond.Broadcast()
	s.wg.Wait()
}

// Cap returns the current concurrency cap.
func (s *Scheduler) Cap() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxConcurrent
}

// SetCap sets the concurrency cap (used by clients to force a known limit).
func (s *Scheduler) SetCap(n int) {
	s.mu.Lock()
	if n > 0 {
		s.maxConcurrent = n
	}
	s.mu.Unlock()
	s.cond.Broadcast()
}

// tighten lowers the cap when the platform rejects with "最多N个".
func (s *Scheduler) tighten(n int) {
	if n <= 0 {
		return
	}
	s.mu.Lock()
	changed := n < s.maxConcurrent
	if changed {
		s.maxConcurrent = n
	}
	s.mu.Unlock()
	if changed {
		s.log.Warn("并发上限收紧（平台返回）", "maxConcurrent", n)
		s.cond.Broadcast()
	}
}

func (s *Scheduler) acquire() {
	s.mu.Lock()
	for s.inFlight >= s.maxConcurrent {
		s.cond.Wait()
	}
	s.inFlight++
	s.mu.Unlock()
}

func (s *Scheduler) release() {
	s.mu.Lock()
	s.inFlight--
	s.mu.Unlock()
	s.cond.Broadcast()
}

// Submit enqueues a batch and returns immediately.
func (s *Scheduler) Submit(jobs []Job) *Batch {
	s.mu.Lock()
	s.seq++
	b := &Batch{ID: "batch-" + time.Now().Format("20060102") + "-" + itoa(s.seq), CreatedAt: time.Now()}
	for i := range jobs {
		js := &JobState{Job: jobs[i], Status: StPending}
		if js.Frequency == "" {
			js.Frequency = "day"
		}
		b.Jobs = append(b.Jobs, js)
	}
	s.batches[b.ID] = b
	s.order = append(s.order, b.ID)
	s.mu.Unlock()
	for _, js := range b.Jobs {
		s.queue <- js
	}
	return b
}

// Status returns a snapshot of a batch.
func (s *Scheduler) Status(batchID string) (*Batch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.batches[batchID]
	if !ok {
		return nil, apierr.New(apierr.CodeNotFound, "批次 %s 不存在", batchID)
	}
	return snapshot(b), nil
}

// List returns the most recent batches (newest first).
func (s *Scheduler) List(limit int) []*Batch {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Batch, 0, len(s.order))
	for i := len(s.order) - 1; i >= 0; i-- {
		if b, ok := s.batches[s.order[i]]; ok {
			out = append(out, snapshot(b))
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Wait blocks until every job in the batch is terminal, the timeout elapses or
// ctx is cancelled. It always returns the latest snapshot.
func (s *Scheduler) Wait(ctx context.Context, batchID string, timeout time.Duration) (*Batch, error) {
	deadline := time.Now().Add(timeout)
	for {
		b, err := s.Status(batchID)
		if err != nil {
			return nil, err
		}
		if allTerminal(b) || time.Now().After(deadline) {
			return b, nil
		}
		select {
		case <-ctx.Done():
			return b, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (s *Scheduler) worker() {
	defer s.wg.Done()
	for {
		select {
		case <-s.stop:
			return
		default:
		}
		select {
		case <-s.stop:
			return
		case js := <-s.queue:
			s.process(js)
		}
	}
}

func (s *Scheduler) process(js *JobState) {
	s.acquire()
	defer s.release()
	ctx := context.Background()

	s.setStatus(js, StRunning, "")
	js.StartedAt = time.Now()

	in := jq.BacktestRunInput{
		StrategyID: js.StrategyID, Start: js.Start, End: js.End,
		Capital: js.Capital, Frequency: js.Frequency, Compile: js.Compile, UseCredit: js.UseCredit,
	}
	var res jq.BacktestRunResult
	submitted := false
	backoff := 5 * time.Second
	for attempt := 0; attempt < 240; attempt++ {
		r, err := s.jq.BacktestRun(ctx, in)
		if err == nil {
			res, submitted = r, true
			break
		}
		e := apierr.From(err)
		switch e.Code {
		case apierr.CodeNoExtension, apierr.CodeNotAuthenticated:
			s.finish(js, StFailed, err.Error())
			return
		}
		if n := concurrentFrom(e); n > 0 {
			s.tighten(n)
		}
		// api_error / network_error / timeout: back off and retry (back-pressure).
		select {
		case <-s.stop:
			return
		case <-time.After(backoff):
		}
	}
	if !submitted {
		s.finish(js, StFailed, "提交重试耗尽")
		return
	}

	js.RemoteID, js.ListID = res.ID, res.ListID
	if s.tasks != nil {
		t := s.tasks.Add(task.Task{
			StrategyID: js.StrategyID, Mode: mode(js.Compile), Start: js.Start, End: js.End,
			Capital: js.Capital, Frequency: js.Frequency, RemoteID: res.ID, ListID: res.ListID,
			Status: task.StatusRunning,
		})
		js.TaskID = t.ID
	}

	deadline := time.Now().Add(s.jobTimeout)
	for time.Now().Before(deadline) {
		raw, err := s.jq.BacktestStats(ctx, js.RemoteID)
		if err == nil {
			js.Metrics = raw
			if statsComplete(raw) {
				s.finish(js, StDone, "")
				return
			}
		} else {
			e := apierr.From(err)
			if e.Code == apierr.CodeNoExtension || e.Code == apierr.CodeNotAuthenticated {
				s.finish(js, StFailed, err.Error())
				return
			}
		}
		select {
		case <-s.stop:
			return
		case <-time.After(s.pollInterval):
		}
	}
	s.finish(js, StTimeout, "")
}

func (s *Scheduler) setStatus(js *JobState, st, errMsg string) {
	s.mu.Lock()
	js.Status = st
	if errMsg != "" {
		js.Error = errMsg
	}
	s.mu.Unlock()
}

func (s *Scheduler) finish(js *JobState, st, errMsg string) {
	s.mu.Lock()
	js.Status = st
	if errMsg != "" {
		js.Error = errMsg
	}
	js.EndedAt = time.Now()
	taskID := js.TaskID
	s.mu.Unlock()
	if s.tasks != nil && taskID != "" {
		tStatus := task.StatusDone
		switch st {
		case StFailed:
			tStatus = task.StatusFailed
		case StTimeout:
			tStatus = task.StatusTimeout
		}
		_, _ = s.tasks.Update(taskID, func(t *task.Task) {
			t.Status = tStatus
			if errMsg != "" {
				t.Error = errMsg
			}
		})
	}
	s.log.Info("批次任务完成", "tag", js.Tag, "status", st, "id", js.RemoteID)
}

func snapshot(b *Batch) *Batch {
	cp := &Batch{ID: b.ID, CreatedAt: b.CreatedAt}
	for _, js := range b.Jobs {
		c := *js
		cp.Jobs = append(cp.Jobs, &c)
	}
	return cp
}

func allTerminal(b *Batch) bool {
	for _, js := range b.Jobs {
		switch js.Status {
		case StDone, StFailed, StTimeout:
		default:
			return false
		}
	}
	return true
}

// statsComplete mirrors the MCP wait helper: core metrics present means done.
func statsComplete(raw json.RawMessage) bool {
	var p struct {
		Metrics map[string]any `json:"metrics"`
	}
	if err := json.Unmarshal(raw, &p); err != nil || p.Metrics == nil {
		return false
	}
	for _, key := range []string{"sharpe", "annual_algo_return", "algorithm_return", "max_drawdown"} {
		v, ok := p.Metrics[key]
		if !ok || v == nil {
			continue
		}
		if s, isStr := v.(string); isStr && (s == "" || s == "--") {
			continue
		}
		return true
	}
	return false
}

func concurrentFrom(e *apierr.Error) int {
	if e == nil || e.Details == nil {
		return 0
	}
	switch v := e.Details["maxConcurrent"].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	}
	return 0
}

func mode(compile bool) string {
	if compile {
		return "compile"
	}
	return "backtest"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
