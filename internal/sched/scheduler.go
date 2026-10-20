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
	"strings"
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
	BatchID   string          `json:"batchId,omitempty"`
	Status    string          `json:"status"`
	TaskID    string          `json:"taskId,omitempty"`
	RemoteID  string          `json:"id,omitempty"`
	ListID    string          `json:"listId,omitempty"`
	Metrics   json.RawMessage `json:"metrics,omitempty"`
	Error     string          `json:"error,omitempty"`
	ErrorLogs []string        `json:"errorLogs,omitempty"`
	StartedAt time.Time       `json:"startedAt,omitempty"`
	EndedAt   time.Time       `json:"endedAt,omitempty"`
}

// Batch groups submitted jobs.
type Batch struct {
	ID        string      `json:"batchId"`
	CreatedAt time.Time   `json:"createdAt"`
	Jobs      []*JobState `json:"jobs"`
	Callback  *Callback   `json:"callback,omitempty"`
}

// Callback configures a webhook delivered when jobs/batches reach a terminal
// state. Secret is never serialized back to clients; it is only used to sign
// the outgoing request body.
type Callback struct {
	URL    string `json:"url"`
	Secret string `json:"-"`
	On     string `json:"on,omitempty"` // batch (default) | job | both
}

// SubmitOptions carries optional per-batch behaviour.
type SubmitOptions struct {
	CallbackURL    string
	CallbackSecret string
	CallbackOn     string
}

// WaitResult is returned by WaitEvents: a batch snapshot plus the events that
// happened after the caller's cursor.
type WaitResult struct {
	Batch       *Batch  `json:"batch"`
	Events      []Event `json:"events"`
	NextEventID int64   `json:"nextEventId"`
	Done        bool    `json:"done"`
	HasMore     bool    `json:"hasMore,omitempty"`
}

// backtestClient is the slice of jq.Client the scheduler needs. Keeping it
// narrow lets tests drive the polling loop without a live browser bridge.
type backtestClient interface {
	BacktestRun(ctx context.Context, in jq.BacktestRunInput) (jq.BacktestRunResult, error)
	BacktestList(ctx context.Context, in jq.BacktestListInput) (json.RawMessage, error)
	BacktestStats(ctx context.Context, backtestID string) (json.RawMessage, error)
	BacktestLogs(ctx context.Context, in jq.BacktestLogsInput) (json.RawMessage, error)
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
	jq            backtestClient
	tasks         *task.Manager
	log           *slog.Logger
	stop          chan struct{}
	stopOnce      sync.Once
	wg            sync.WaitGroup
	seq           int
	pollInterval  time.Duration
	jobTimeout    time.Duration
	compileGrace  time.Duration

	// Event log and fan-out. events is per batch; eventSeq is global so a
	// cursor stays valid even if it is carried across batches.
	events    map[string][]Event
	eventSeq  int64
	eventCh   chan struct{}
	eventSink func(Event)
	callbacks *callbackSender
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
		tasks:         tasks,
		log:           logger,
		stop:          make(chan struct{}),
		pollInterval:  3 * time.Second,
		jobTimeout:    2 * time.Hour,
		compileGrace:  10 * time.Second,
		events:        map[string][]Event{},
		eventCh:       make(chan struct{}),
		callbacks:     newCallbackSender(logger),
	}
	// Keep a nil *jq.Client as a nil interface so a client-less scheduler
	// (tests, event-only use) never holds a typed-nil interface.
	if client != nil {
		s.jq = client
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
	return s.SubmitWithOptions(jobs, SubmitOptions{})
}

// SubmitWithOptions enqueues a batch with an optional completion callback.
func (s *Scheduler) SubmitWithOptions(jobs []Job, opt SubmitOptions) *Batch {
	s.mu.Lock()
	s.seq++
	b := &Batch{ID: "batch-" + time.Now().Format("20060102") + "-" + itoa(s.seq), CreatedAt: time.Now()}
	if opt.CallbackURL != "" {
		on := opt.CallbackOn
		if on == "" {
			on = "batch"
		}
		b.Callback = &Callback{URL: opt.CallbackURL, Secret: opt.CallbackSecret, On: on}
	}
	for i := range jobs {
		js := &JobState{Job: jobs[i], Status: StPending, BatchID: b.ID}
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

// SetEventSink installs a listener invoked for every job/batch lifecycle event.
// The sink runs outside the scheduler lock and must not block for long.
func (s *Scheduler) SetEventSink(fn func(Event)) {
	s.mu.Lock()
	s.eventSink = fn
	s.mu.Unlock()
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
	res, err := s.WaitEvents(ctx, batchID, 0, timeout, nil)
	if err != nil {
		if res != nil {
			return res.Batch, err
		}
		return nil, err
	}
	return res.Batch, nil
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

	s.start(js)

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

	s.mu.Lock()
	js.RemoteID, js.ListID = res.ID, res.ListID
	s.mu.Unlock()
	if s.tasks != nil {
		t := s.tasks.Add(task.Task{
			StrategyID: js.StrategyID, Mode: mode(js.Compile), Start: js.Start, End: js.End,
			Capital: js.Capital, Frequency: js.Frequency, RemoteID: res.ID, ListID: res.ListID,
			Status: task.StatusRunning,
		})
		s.mu.Lock()
		js.TaskID = t.ID
		s.mu.Unlock()
	}

	deadline := time.Now().Add(s.jobTimeout)
	submittedAt := time.Now()
	for time.Now().Before(deadline) {
		// Poll the error log first: a runtime or compile error never heals, so
		// the run will never produce useful output. Failing here (instead of
		// waiting for jobTimeout) emits job.failed + batch.done immediately,
		// which is what fires the webhook.
		if errs := s.errorLogs(ctx, js.RemoteID); len(errs) > 0 {
			s.finishWithLogs(js, StFailed, summarizeErrorLogs(errs), errs)
			return
		}
		if js.Compile {
			// Compile runs produce no metrics, so they are tracked through the
			// build list instead of backtest.stats.
			st := s.compileStatus(ctx, js, time.Since(submittedAt))
			switch st {
			case StDone:
				s.finish(js, StDone, "")
				return
			case StFailed:
				s.finish(js, StFailed, "聚宽侧编译失败（无错误日志）")
				return
			case "cancelled":
				s.finish(js, StFailed, "聚宽侧编译已取消")
				return
			}
		} else {
			raw, err := s.jq.BacktestStats(ctx, js.RemoteID)
			if err == nil {
				s.mu.Lock()
				js.Metrics = raw
				done := statsComplete(raw)
				s.mu.Unlock()
				if done {
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
		}
		select {
		case <-s.stop:
			return
		case <-time.After(s.pollInterval):
		}
	}
	s.finish(js, StTimeout, "")
}

// buildItem is one row of the backtest/build list.
type buildItem struct {
	ID     string `json:"id"`
	ListID string `json:"listId"`
	Status string `json:"status"`
}

// compileStatus reports the terminal state of a compile (build) run, or "" when
// it is still running or cannot be determined yet.
//
// The row ids are matched exactly against the ids returned by backtest.run. If
// JoinQuant hands out ids that do not line up, it falls back — after a grace
// period — to the heuristic jq_strategy_compile uses: no run in flight for the
// strategy plus at least one finished run.
func (s *Scheduler) compileStatus(ctx context.Context, js *JobState, elapsed time.Duration) string {
	if s.jq == nil {
		return ""
	}
	raw, err := s.jq.BacktestList(ctx, jq.BacktestListInput{
		StrategyID: js.StrategyID,
		Limit:      20,
		Compile:    true,
	})
	if err != nil {
		return ""
	}
	var p struct {
		Items []buildItem `json:"items"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return ""
	}
	running, terminal, matched, matchedStatus := false, false, false, ""
	for _, it := range p.Items {
		if it.Status == "running" {
			running = true
		}
		switch it.Status {
		case "done", "failed", "cancelled":
			terminal = true
		}
		if matchesRun(it, js.RemoteID, js.ListID) {
			matched, matchedStatus = true, it.Status
		}
	}
	if matched {
		return matchedStatus
	}
	// Give a freshly submitted run time to show up before trusting the list.
	if terminal && !running && elapsed >= s.compileGrace {
		return StDone
	}
	return ""
}

func matchesRun(it buildItem, ids ...string) bool {
	for _, id := range ids {
		if id != "" && (it.ID == id || it.ListID == id) {
			return true
		}
	}
	return false
}

// errorLogs returns the run's error log entries. A transport/parse failure is
// reported as nil on purpose: transient bridge errors must not fail the job,
// the next poll retries.
func (s *Scheduler) errorLogs(ctx context.Context, backtestID string) []string {
	if s.jq == nil || backtestID == "" {
		return nil
	}
	raw, err := s.jq.BacktestLogs(ctx, jq.BacktestLogsInput{BacktestID: backtestID, Error: true})
	if err != nil {
		return nil
	}
	var p struct {
		Logs []string `json:"logs"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil
	}
	return p.Logs
}

// summarizeErrorLogs keeps the tail of the error log as the job's one-line
// error; the full list travels in JobState.ErrorLogs (and thus in the webhook).
func summarizeErrorLogs(logs []string) string {
	const maxLines = 5
	start := 0
	if len(logs) > maxLines {
		start = len(logs) - maxLines
	}
	return "回测错误日志: " + strings.Join(logs[start:], "\n")
}

func (s *Scheduler) start(js *JobState) {
	var ev Event
	var cb *Callback
	s.mu.Lock()
	js.Status = StRunning
	js.StartedAt = time.Now()
	ev = s.appendEventLocked("job.started", js.BatchID, js, nil)
	if b, ok := s.batches[js.BatchID]; ok && b.Callback != nil {
		c := *b.Callback
		cb = &c
	}
	s.mu.Unlock()
	s.publish(ev, cb)
}

func (s *Scheduler) finish(js *JobState, st, errMsg string) {
	s.finishWithLogs(js, st, errMsg, nil)
}

func (s *Scheduler) finishWithLogs(js *JobState, st, errMsg string, errLogs []string) {
	var evs []Event
	var cb *Callback
	var taskID string
	s.mu.Lock()
	js.Status = st
	if errMsg != "" {
		js.Error = errMsg
	}
	if len(errLogs) > 0 {
		js.ErrorLogs = errLogs
	}
	js.EndedAt = time.Now()
	taskID = js.TaskID
	evs = append(evs, s.appendEventLocked("job."+eventSuffix(st), js.BatchID, js, nil))
	if b, ok := s.batches[js.BatchID]; ok && allTerminal(b) {
		evs = append(evs, s.appendEventLocked("batch.done", b.ID, nil, b.Jobs))
		if b.Callback != nil {
			c := *b.Callback
			cb = &c
		}
	}
	s.mu.Unlock()

	for _, ev := range evs {
		s.publish(ev, cb)
	}

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
			if len(errLogs) > 0 {
				t.ErrorLogs = errLogs
			}
		})
	}
	s.log.Info("批次任务完成", "tag", js.Tag, "status", st, "id", js.RemoteID)
}

func snapshot(b *Batch) *Batch {
	cp := &Batch{ID: b.ID, CreatedAt: b.CreatedAt}
	if b.Callback != nil {
		c := *b.Callback
		cp.Callback = &c
	}
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
