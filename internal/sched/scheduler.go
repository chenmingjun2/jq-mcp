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
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jqhelper/jq-mcp/internal/apierr"
	"github.com/jqhelper/jq-mcp/internal/jq"
	"github.com/jqhelper/jq-mcp/internal/task"
)

// Job status values.
const (
	StPending     = "pending"
	StRunning     = "running"
	StDone        = "done"
	StFailed      = "failed"
	StTimeout     = "timeout"
	StNeedsCredit = "needs_confirmation"
)

// Job is one backtest to run.
type Job struct {
	Tag        string  `json:"tag,omitempty"`
	StrategyID string  `json:"strategyId,omitempty"`
	Start      string  `json:"start"`
	End        string  `json:"end,omitempty"`
	Capital    float64 `json:"capital,omitempty"`
	Frequency  string  `json:"frequency,omitempty"`
	Compile    bool    `json:"compile,omitempty"`
	UseCredit  bool    `json:"useCredit,omitempty"`
	Priority   int     `json:"priority,omitempty"`

	// Pool fields. Supplying code (or codePath) makes the job portable: the
	// scheduler may place it on any account and upload a prefixed copy there.
	StrategyKey string `json:"strategyKey,omitempty"`
	Name        string `json:"name,omitempty"`
	Code        string `json:"code,omitempty"`
	CodePath    string `json:"codePath,omitempty"`
	Account     string `json:"account,omitempty"`   // pin to one alias
	Placement   string `json:"placement,omitempty"` // auto|pinned|sticky
}

// JobState is a Job plus its live status/result.
type JobState struct {
	Job
	BatchID    string `json:"batchId,omitempty"`
	Status     string `json:"status"`
	TaskID     string `json:"taskId,omitempty"`
	RemoteID   string `json:"id,omitempty"`
	ListID     string `json:"listId,omitempty"`
	Account    string `json:"account,omitempty"`
	AccountUID string `json:"accountUid,omitempty"`
	Attempts   int    `json:"attempts,omitempty"`
	// CreditRequired is set when the free quota is exhausted and the job needs
	// explicit user approval before spending paid credits.
	CreditRequired bool            `json:"creditRequired,omitempty"`
	CreditReason   string          `json:"creditReason,omitempty"`
	CreditAccount  string          `json:"creditAccount,omitempty"`
	Metrics        json.RawMessage `json:"metrics,omitempty"`
	Error          string          `json:"error,omitempty"`
	ErrorLogs      []string        `json:"errorLogs,omitempty"`
	StartedAt      time.Time       `json:"startedAt,omitempty"`
	EndedAt        time.Time       `json:"endedAt,omitempty"`
	// Progress is a best-effort ETA for running jobs; nil when there is not
	// enough history to learn a rate.
	Progress *ProgressEstimate `json:"progress,omitempty"`

	assigned         *acctState
	queuedAt         time.Time
	order            int64
	portable         bool
	pinned           bool
	needsCreditPause bool
}

// ProgressEstimate is a best-effort progress/ETA for a running job. JoinQuant
// exposes no simulation-date watermark while a backtest runs (the result and
// dayResult endpoints are empty, and the page's progress bar is computed
// client-side), so the only real signal is elapsed time. This estimate learns
// median seconds per simulated year from the daemon's completed jobs,
// preferring the same strategyKey, and extrapolates to the job's window.
type ProgressEstimate struct {
	ElapsedSeconds   float64 `json:"elapsedSeconds"`
	EstimatedSeconds float64 `json:"estimatedSeconds"`
	Percent          float64 `json:"percent"`
	EtaSeconds       float64 `json:"etaSeconds"`
	Basis            string  `json:"basis"` // strategyKey | global
	Samples          int     `json:"samples"`
}

// Batch groups submitted jobs.
type Batch struct {
	ID        string      `json:"batchId"`
	CreatedAt time.Time   `json:"createdAt"`
	SessionID string      `json:"sessionId,omitempty"` // originating OpenCode session (MCP _meta)
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
	SessionID      string
}

// WaitResult is returned by WaitEvents: a batch snapshot plus the events that
// happened after the caller's cursor.
type WaitResult struct {
	Batch             *Batch      `json:"batch"`
	Events            []Event     `json:"events"`
	NeedsConfirmation []*JobState `json:"needsConfirmation,omitempty"`
	NextEventID       int64       `json:"nextEventId"`
	Done              bool        `json:"done"`
	HasMore           bool        `json:"hasMore,omitempty"`
}

// AccountRuntime is the scheduler's view of one pool member. UID/nickname come
// from the registry; Connected/LoggedIn/Cap/Credits are refreshed by the daemon.
type AccountRuntime struct {
	Alias        string   `json:"alias"`
	UID          string   `json:"uid,omitempty"`
	Nickname     string   `json:"nickname,omitempty"`
	Enabled      bool     `json:"enabled"`
	Weight       int      `json:"weight,omitempty"`
	Cap          int      `json:"cap,omitempty"`
	Connected    bool     `json:"connected"`
	LoggedIn     bool     `json:"loggedIn"`
	Credits      int      `json:"credits,omitempty"`
	CreditsKnown bool     `json:"creditsKnown,omitempty"`
	State        string   `json:"state,omitempty"`
	Tags         []string `json:"tags,omitempty"`
}

// AccountView is the scheduler's live account snapshot for jq_accounts.
type AccountView struct {
	Alias         string    `json:"alias"`
	UID           string    `json:"uid,omitempty"`
	Nickname      string    `json:"nickname,omitempty"`
	Enabled       bool      `json:"enabled"`
	Weight        int       `json:"weight,omitempty"`
	State         string    `json:"state"`
	Cap           int       `json:"cap"`
	InFlight      int       `json:"inFlight"`
	Queued        int       `json:"queued"`
	Credits       int       `json:"credits"`
	CreditsKnown  bool      `json:"creditsKnown"`
	CooldownUntil time.Time `json:"cooldownUntil,omitempty"`
	LastError     string    `json:"lastError,omitempty"`
	LastUsedAt    time.Time `json:"lastUsedAt,omitempty"`
}

// Account states.
const (
	AcctReady     = "ready"
	AcctBusy      = "busy"
	AcctCooling   = "cooling"
	AcctQuota     = "quota_exhausted"
	AcctFreeOut   = "free_exhausted"
	AcctLoggedOut = "logged_out"
	AcctDisabled  = "disabled"
	AcctPending   = "pending_login"
)

type acctState struct {
	alias         string
	uid           string
	nickname      string
	enabled       bool
	weight        int
	cap           int
	state         string
	inFlight      int
	cooldownUntil time.Time
	credits       int
	creditsKnown  bool
	lastError     string
	lastUsedAt    time.Time
	tags          []string
}

func (a *acctState) ready(now time.Time) bool {
	if a == nil || !a.enabled {
		return false
	}
	switch a.state {
	case AcctReady, AcctBusy:
	case AcctCooling:
		if now.Before(a.cooldownUntil) {
			return false
		}
	default:
		return false
	}
	return a.inFlight < a.cap
}

// usable is ready() plus the credit policy: a free-quota-exhausted account can
// still run when the user has explicitly approved paid credits.
func (a *acctState) usable(now time.Time, needCredit bool) bool {
	if a == nil || !a.enabled || a.inFlight >= a.cap {
		return false
	}
	if a.state == AcctFreeOut {
		if !needCredit {
			return false
		}
		if a.creditsKnown && a.credits <= 0 {
			return false
		}
		return true
	}
	return a.ready(now)
}

// hasCreditCapacityLocked reports whether an approved credit retry has any
// chance of running: either a ready account (it can still use free quota) or a
// free-exhausted account with known/unknown credits.
func (s *Scheduler) hasCreditCapacityLocked(now time.Time) bool {
	for _, a := range s.accounts {
		if !a.enabled {
			continue
		}
		if a.state == AcctFreeOut {
			if !a.creditsKnown || a.credits > 0 {
				return true
			}
			continue
		}
		if a.inFlight < a.cap && a.ready(now) {
			return true
		}
	}
	return false
}

// backtestClient is the slice of jq.Client the scheduler needs. Keeping it
// narrow lets tests drive the polling loop without a live browser bridge.
type backtestClient interface {
	BacktestRun(ctx context.Context, in jq.BacktestRunInput) (jq.BacktestRunResult, error)
	BacktestList(ctx context.Context, in jq.BacktestListInput) (json.RawMessage, error)
	BacktestStats(ctx context.Context, backtestID string) (json.RawMessage, error)
	BacktestLogs(ctx context.Context, in jq.BacktestLogsInput) (json.RawMessage, error)
}

// strategyClient is implemented by jq.Client and used by the provisioner.
type strategyClient interface {
	StrategyCreate(ctx context.Context, in jq.StrategyCreateInput) (json.RawMessage, error)
	StrategySave(ctx context.Context, in jq.StrategySaveInput) (json.RawMessage, error)
}

// accountScoper lets the scheduler obtain a per-account client view. Tests use
// the same client for every account.
type accountScoper interface {
	WithAccount(alias string) *jq.Client
}

// Scheduler owns the queue, the workers and per-account concurrency slots.
type Scheduler struct {
	mu            sync.Mutex
	cond          *sync.Cond
	batches       map[string]*Batch
	order         []string
	pending       []*JobState
	dispatch      chan *JobState
	workers       int
	maxConcurrent int // default cap for accounts without an override
	maxFailovers  int
	jobSubmitWait time.Duration
	accounts      map[string]*acctState
	defaultAlias  string
	sticky        map[string]string
	jq            backtestClient
	tasks         *task.Manager
	log           *slog.Logger
	stop          chan struct{}
	stopOnce      sync.Once
	wg            sync.WaitGroup
	seq           int
	jobSeq        int64
	pollInterval  time.Duration
	jobTimeout    time.Duration
	compileGrace  time.Duration
	provisionDir  string
	prov          *provisioner
	clientFactory func(alias string) backtestClient

	// Event log and fan-out. events is per batch; eventSeq is global so a
	// cursor stays valid even if it is carried across batches.
	events    map[string][]Event
	eventSeq  int64
	daemonID  string
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
		dispatch:      make(chan *JobState, 1024),
		workers:       workers,
		maxConcurrent: maxConcurrent,
		maxFailovers:  1,
		jobSubmitWait: 10 * time.Minute,
		tasks:         tasks,
		log:           logger,
		stop:          make(chan struct{}),
		pollInterval:  3 * time.Second,
		jobTimeout:    2 * time.Hour,
		compileGrace:  10 * time.Second,
		events:        map[string][]Event{},
		eventCh:       make(chan struct{}),
		callbacks:     newCallbackSender(logger),
		accounts:      map[string]*acctState{},
		sticky:        map[string]string{},
	}
	s.accounts["default"] = &acctState{
		alias: "default", enabled: true, weight: 1, cap: maxConcurrent, state: AcctReady,
	}
	s.defaultAlias = "default"
	s.prov = newProvisioner(logger)
	// Keep a nil *jq.Client as a nil interface so a client-less scheduler
	// (tests, event-only use) never holds a typed-nil interface.
	if client != nil {
		s.jq = client
	}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// Start launches the dispatcher, the worker pool and the pending sweeper.
func (s *Scheduler) Start() {
	s.wg.Add(1)
	go s.dispatchLoop()
	for i := 0; i < s.workers; i++ {
		s.wg.Add(1)
		go s.worker()
	}
	s.wg.Add(1)
	go s.sweeper()
}

// Stop signals workers to exit and waits for them.
func (s *Scheduler) Stop() {
	s.stopOnce.Do(func() { close(s.stop) })
	s.cond.Broadcast()
	s.wg.Wait()
}

// Cap returns the aggregate effective concurrency cap.
func (s *Scheduler) Cap() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	for _, a := range s.accounts {
		if a.enabled {
			total += a.cap
		}
	}
	if total == 0 {
		return s.maxConcurrent
	}
	return total
}

// CapFor returns one account's effective cap.
func (s *Scheduler) CapFor(alias string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.accounts[alias]; ok {
		return a.cap
	}
	return 0
}

// SetCap sets a ceiling for the pool. It only tightens: an account's
// configured cap (accounts cap / defaultCap) is never raised by a batch's
// maxConcurrent. Use `jq-mcp accounts cap <alias> N` to change it.
func (s *Scheduler) SetCap(n int) { s.SetCapFor("", n) }

// SetCapFor applies a ceiling to one account ("" = every account).
func (s *Scheduler) SetCapFor(alias string, n int) {
	if n <= 0 {
		return
	}
	s.mu.Lock()
	clamp := func(cur int) int {
		if cur <= 0 || n < cur {
			return n
		}
		return cur
	}
	if alias == "" {
		s.maxConcurrent = n
		for _, a := range s.accounts {
			a.cap = clamp(a.cap)
		}
	} else if a, ok := s.accounts[alias]; ok {
		a.cap = clamp(a.cap)
	}
	s.mu.Unlock()
	s.cond.Broadcast()
}

// tighten lowers one account's cap when the platform rejects with "最多N个".
func (s *Scheduler) tighten(alias string, n int) {
	if n <= 0 || alias == "" {
		return
	}
	s.mu.Lock()
	a, ok := s.accounts[alias]
	changed := ok && n < a.cap
	if changed {
		a.cap = n
	}
	s.mu.Unlock()
	if changed {
		s.log.Warn("账号并发上限收紧（平台返回）", "account", alias, "maxConcurrent", n)
		s.cond.Broadcast()
	}
}

func (s *Scheduler) releaseAccount(a *acctState) {
	if a == nil {
		return
	}
	s.mu.Lock()
	// SyncAccounts may have replaced the account map while the job was running;
	// always decrement the current entry so capacity is not leaked.
	cur := s.accounts[a.alias]
	if cur == nil {
		cur = a
	}
	if cur.inFlight > 0 {
		cur.inFlight--
	}
	if cur.state == AcctBusy && cur.inFlight == 0 {
		cur.state = AcctReady
	}
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
	b := &Batch{
		ID:        "batch-" + time.Now().Format("20060102") + "-" + itoa(s.seq),
		CreatedAt: time.Now(),
		SessionID: opt.SessionID,
	}
	if opt.CallbackURL != "" {
		on := opt.CallbackOn
		if on == "" {
			on = "batch"
		}
		b.Callback = &Callback{URL: opt.CallbackURL, Secret: opt.CallbackSecret, On: on}
	}
	for i := range jobs {
		js := &JobState{Job: jobs[i], Status: StPending, BatchID: b.ID, queuedAt: time.Now()}
		js.Account = jobs[i].Account
		if js.Frequency == "" {
			js.Frequency = "day"
		}
		js.portable = js.Code != "" || js.CodePath != ""
		js.pinned = js.Account != ""
		s.jobSeq++
		js.order = s.jobSeq
		b.Jobs = append(b.Jobs, js)
		s.enqueueLocked(js)
	}
	s.batches[b.ID] = b
	s.order = append(s.order, b.ID)
	s.mu.Unlock()
	s.cond.Broadcast()
	return b
}

// SetEventSink installs a listener invoked for every job/batch lifecycle event.
// The sink runs outside the scheduler lock and must not block for long.
func (s *Scheduler) SetEventSink(fn func(Event)) {
	s.mu.Lock()
	s.eventSink = fn
	s.mu.Unlock()
}

// SetDefaultAccount selects the account used when a job does not name one and
// auto placement cannot decide (legacy strategyId jobs).
func (s *Scheduler) SetDefaultAccount(alias string) {
	s.mu.Lock()
	if _, ok := s.accounts[alias]; ok {
		s.defaultAlias = alias
	}
	s.mu.Unlock()
	s.cond.Broadcast()
}

// DefaultAccount returns the current default alias.
func (s *Scheduler) DefaultAccount() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.defaultAlias
}

// SetProvisionDir enables on-disk caching of per-account strategy copies.
func (s *Scheduler) SetProvisionDir(dir string) {
	s.mu.Lock()
	s.provisionDir = dir
	s.mu.Unlock()
	s.prov.setDir(dir)
}

// SetMaxFailovers bounds how many times a portable job may move accounts.
func (s *Scheduler) SetMaxFailovers(n int) {
	s.mu.Lock()
	if n >= 0 {
		s.maxFailovers = n
	}
	s.mu.Unlock()
}

// SyncAccounts replaces the account set (registry changes, startup). Runtime
// counters and cooldowns survive when the alias still exists.
func (s *Scheduler) SyncAccounts(list []AccountRuntime) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(list) == 0 {
		if len(s.accounts) == 0 {
			s.accounts["default"] = &acctState{alias: "default", enabled: true, weight: 1, cap: s.maxConcurrent, state: AcctReady}
			s.defaultAlias = "default"
		}
		return
	}
	next := make(map[string]*acctState, len(list))
	for _, rt := range list {
		if rt.Alias == "" {
			continue
		}
		cap := rt.Cap
		if cap <= 0 {
			cap = s.maxConcurrent
		}
		st := &acctState{
			alias: rt.Alias, uid: rt.UID, nickname: rt.Nickname,
			enabled: rt.Enabled, weight: rt.Weight, cap: cap,
			credits: rt.Credits, creditsKnown: rt.CreditsKnown,
			tags: append([]string(nil), rt.Tags...),
		}
		if st.weight <= 0 {
			st.weight = 1
		}
		if old, ok := s.accounts[rt.Alias]; ok {
			st.inFlight = old.inFlight
			st.cooldownUntil = old.cooldownUntil
			st.lastError = old.lastError
			st.lastUsedAt = old.lastUsedAt
			if !rt.CreditsKnown {
				st.credits, st.creditsKnown = old.credits, old.creditsKnown
			}
			if st.uid == "" {
				st.uid = old.uid
			}
			if st.nickname == "" {
				st.nickname = old.nickname
			}
			if old.state == AcctCooling && time.Now().Before(old.cooldownUntil) {
				st.state = AcctCooling
			}
			if old.state == AcctQuota && time.Now().Before(old.cooldownUntil) {
				st.state = AcctQuota
			}
			if old.state == AcctFreeOut && time.Now().Before(old.cooldownUntil) {
				st.state = AcctFreeOut
			}
			// A live WebSocket does not prove the JoinQuant session is valid.
			// Keep a submit-time auth failure quarantined until a fresh
			// identity probe succeeds (SetAccountIdentity resets it).
			if old.state == AcctLoggedOut && rt.Connected {
				st.state = AcctLoggedOut
			}
		}
		if st.state == "" {
			switch {
			case !st.enabled:
				st.state = AcctDisabled
			case rt.State != "":
				st.state = rt.State
			case !rt.Connected || !rt.LoggedIn:
				st.state = AcctPending
			default:
				st.state = AcctReady
			}
		}
		next[rt.Alias] = st
	}
	if len(next) > 0 {
		s.accounts = next
		if _, ok := s.accounts[s.defaultAlias]; !ok {
			s.defaultAlias = ""
			for _, rt := range list {
				if rt.Enabled {
					s.defaultAlias = rt.Alias
					break
				}
			}
		}
	}
	s.cond.Broadcast()
}

// SetAccountIdentity applies a verified identity and login state to one account.
func (s *Scheduler) SetAccountIdentity(alias, uid, nickname string, credits int, creditsKnown bool) {
	if alias == "" {
		return
	}
	s.mu.Lock()
	a, ok := s.accounts[alias]
	if !ok {
		a = &acctState{alias: alias, enabled: true, weight: 1, cap: s.maxConcurrent}
		s.accounts[alias] = a
		if s.defaultAlias == "" || s.defaultAlias == "default" {
			s.defaultAlias = alias
		}
	}
	a.uid, a.nickname, a.credits, a.creditsKnown = uid, nickname, credits, creditsKnown
	if !a.enabled {
		a.state = AcctDisabled
	} else if a.state == AcctLoggedOut || a.state == AcctPending || a.state == "" || a.state == "default" {
		a.state = AcctReady
	}
	a.lastError = ""
	s.mu.Unlock()
	s.cond.Broadcast()
}

// MarkAccountDisconnected quarantines an account whose bridge connection went
// away. Jobs already running keep polling; queued jobs move elsewhere.
func (s *Scheduler) MarkAccountDisconnected(alias string) {
	if alias == "" {
		return
	}
	s.mu.Lock()
	if a, ok := s.accounts[alias]; ok {
		a.state = AcctLoggedOut
		a.lastError = "浏览器连接已断开"
	}
	s.mu.Unlock()
	s.cond.Broadcast()
}

// AccountForRemote returns the account that owns a backtest id, if it was run
// through this scheduler.
func (s *Scheduler) AccountForRemote(remoteID string) string {
	if remoteID == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.batches {
		for _, js := range b.Jobs {
			if js.RemoteID == remoteID || js.ListID == remoteID {
				return js.Account
			}
		}
	}
	return ""
}

// Accounts returns a stable snapshot for jq_accounts.
func (s *Scheduler) Accounts() []AccountView {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]AccountView, 0, len(s.accounts))
	for _, a := range s.accounts {
		v := AccountView{
			Alias: a.alias, UID: a.uid, Nickname: a.nickname, Enabled: a.enabled,
			Weight: a.weight, State: a.state, Cap: a.cap, InFlight: a.inFlight,
			Credits: a.credits, CreditsKnown: a.creditsKnown,
			CooldownUntil: a.cooldownUntil, LastError: a.lastError, LastUsedAt: a.lastUsedAt,
		}
		for _, js := range s.pending {
			if js.Account == a.alias {
				v.Queued++
			}
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Alias < out[j].Alias })
	return out
}

// SetDaemonID stamps every event with the daemon that produced it. Consumers
// use (daemonId, eventId) as a globally unique cursor: a daemon restart resets
// eventId, so the pair must change together.
func (s *Scheduler) SetDaemonID(id string) {
	s.mu.Lock()
	s.daemonID = id
	s.mu.Unlock()
}

// DaemonID returns the configured daemon identity.
func (s *Scheduler) DaemonID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.daemonID
}

// Status returns a snapshot of a batch.
func (s *Scheduler) Status(batchID string) (*Batch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.batches[batchID]
	if !ok {
		return nil, apierr.New(apierr.CodeNotFound, "批次 %s 不存在", batchID)
	}
	return s.snapshotWithProgressLocked(b), nil
}

// List returns the most recent batches (newest first).
func (s *Scheduler) List(limit int) []*Batch {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Batch, 0, len(s.order))
	for i := len(s.order) - 1; i >= 0; i-- {
		if b, ok := s.batches[s.order[i]]; ok {
			out = append(out, s.snapshotWithProgressLocked(b))
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
		case js := <-s.dispatch:
			s.process(js)
		}
	}
}

// dispatchLoop assigns jobs to accounts and hands them to workers. Assigning
// before dispatch is what prevents head-of-line blocking: a job pinned to a busy
// account never occupies a worker that could run work on another account.
func (s *Scheduler) dispatchLoop() {
	defer s.wg.Done()
	for {
		s.mu.Lock()
		for {
			select {
			case <-s.stop:
				s.mu.Unlock()
				return
			default:
			}
			if js := s.takeLocked(); js != nil {
				s.mu.Unlock()
				if js.needsCreditPause {
					js.needsCreditPause = false
					s.pauseForCredit(js, "免费回测额度已用完")
					break
				}
				select {
				case s.dispatch <- js:
				case <-s.stop:
					return
				}
				break
			}
			s.cond.Wait()
		}
	}
}

// enqueueLocked inserts a job in priority order (smaller runs first), keeping
// FIFO within the same priority.
func (s *Scheduler) enqueueLocked(js *JobState) {
	i := sort.Search(len(s.pending), func(i int) bool {
		p := s.pending[i]
		if p.Priority != js.Priority {
			return p.Priority > js.Priority
		}
		return p.order > js.order
	})
	s.pending = append(s.pending, nil)
	copy(s.pending[i+1:], s.pending[i:])
	s.pending[i] = js
}

// takeLocked removes and reserves the highest-priority assignable job.
func (s *Scheduler) takeLocked() *JobState {
	now := time.Now()
	for i, js := range s.pending {
		a := s.pickAccountLocked(js, now)
		if a == nil {
			if alias := s.freeExhaustedAccountLocked(js, now); alias != "" {
				s.pending = append(s.pending[:i], s.pending[i+1:]...)
				js.needsCreditPause = true
				js.CreditAccount = alias
				return js
			}
			continue
		}
		s.pending = append(s.pending[:i], s.pending[i+1:]...)
		js.assigned = a
		js.Account = a.alias
		js.AccountUID = a.uid
		a.inFlight++
		a.lastUsedAt = now
		if a.state == AcctReady {
			a.state = AcctBusy
		}
		return js
	}
	return nil
}

// pickAccountLocked returns an eligible account for js, or nil.
func (s *Scheduler) pickAccountLocked(js *JobState, now time.Time) *acctState {
	if len(s.accounts) == 0 {
		return nil
	}
	if js.Account != "" {
		a := s.accounts[js.Account]
		if a != nil && a.usable(now, js.UseCredit) {
			return a
		}
		return nil
	}
	// Legacy jobs reference an account-private strategyId. Only the default
	// account (or the single configured account) can be assumed to own it.
	if js.StrategyID != "" && js.Code == "" && js.CodePath == "" {
		a := s.accounts[s.defaultAlias]
		if a != nil && a.usable(now, js.UseCredit) {
			return a
		}
		return nil
	}
	if js.Placement == "pinned" {
		return nil
	}
	if js.Placement == "sticky" && js.StrategyKey != "" {
		if owner, ok := s.sticky[js.StrategyKey]; ok {
			if a := s.accounts[owner]; a != nil && a.usable(now, js.UseCredit) {
				return a
			}
		}
	}
	var best *acctState
	bestFree := -1
	for _, a := range s.accounts {
		if !a.usable(now, js.UseCredit) {
			continue
		}
		free := a.cap - a.inFlight
		if best == nil || free > bestFree ||
			(free == bestFree && a.weight > best.weight) ||
			(free == bestFree && a.weight == best.weight && a.lastUsedAt.Before(best.lastUsedAt)) {
			best, bestFree = a, free
		}
	}
	return best
}

func (s *Scheduler) freeExhaustedAccountLocked(js *JobState, now time.Time) string {
	if js.UseCredit {
		return ""
	}
	isFreeOut := func(a *acctState) bool {
		return a != nil && a.enabled && a.state == AcctFreeOut && now.Before(a.cooldownUntil)
	}
	target := js.Account
	if target == "" && js.StrategyID != "" && js.Code == "" && js.CodePath == "" {
		target = s.defaultAlias
	}
	if target != "" {
		if isFreeOut(s.accounts[target]) {
			return target
		}
		return ""
	}
	// Auto placement: only pause when every enabled account is out of free
	// quota. A busy/cooling/pending account might still take the job soon.
	freeAlias := ""
	enabled := 0
	for _, a := range s.accounts {
		if !a.enabled {
			continue
		}
		enabled++
		if !isFreeOut(a) {
			return ""
		}
		freeAlias = a.alias
	}
	if enabled > 0 {
		return freeAlias
	}
	return ""
}

func (s *Scheduler) clientFor(alias string) backtestClient {
	if s.clientFactory != nil {
		return s.clientFactory(alias)
	}
	if alias == "" || s.jq == nil {
		return s.jq
	}
	if sc, ok := s.jq.(accountScoper); ok {
		return sc.WithAccount(alias)
	}
	return s.jq
}

// SetClientFactory overrides per-account client construction (tests).
func (s *Scheduler) SetClientFactory(fn func(alias string) backtestClient) {
	s.clientFactory = fn
}

func (s *Scheduler) removePending(js *JobState) {
	s.mu.Lock()
	for i, p := range s.pending {
		if p == js {
			s.pending = append(s.pending[:i], s.pending[i+1:]...)
			break
		}
	}
	s.mu.Unlock()
}

func (s *Scheduler) process(js *JobState) {
	acct := js.assigned
	if acct != nil {
		defer s.releaseAccount(acct)
	}
	s.removePending(js)
	// 整个任务最多 15 分钟；避免页面/接口卡住时无限挂起。
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	s.start(js)

	// Resolve portable code into an account-local strategy id. Legacy jobs keep
	// their strategyId and are pinned to the default account.
	if js.StrategyID == "" {
		code, err := s.resolveCode(js)
		if err != nil {
			s.failOrRequeue(js, err.Error())
			return
		}
		alias := js.Account
		client := s.clientFor(alias)
		sc, ok := client.(strategyClient)
		if !ok {
			s.finish(js, StFailed, "当前客户端不支持策略落地")
			return
		}
		key := strategyKey(js, code)
		id, err := s.prov.ensure(ctx, sc, alias, key, strategyName(js, key), code)
		if err != nil {
			s.failOrRequeue(js, "策略落地失败: "+apierr.From(err).Message)
			return
		}
		s.mu.Lock()
		js.StrategyID = id
		if js.StrategyKey == "" {
			js.StrategyKey = key
		}
		s.mu.Unlock()
	}

	client := s.clientFor(js.Account)
	in := jq.BacktestRunInput{
		StrategyID: js.StrategyID, Start: js.Start, End: js.End,
		Capital: js.Capital, Frequency: js.Frequency, Compile: js.Compile,
		// Always try the free allowance first; credits are a fallback.
		UseCredit: false,
	}
	var res jq.BacktestRunResult
	submitted := false
	creditTried := false
	backoff := 5 * time.Second
	// 最多 6 次（约 30 秒）；平台异常时不长时间空转，避免烧光提交次数。
	for attempt := 0; attempt < 6; attempt++ {
		callCtx, callCancel := context.WithTimeout(ctx, 90*time.Second)
		r, err := client.BacktestRun(callCtx, in)
		callCancel()
		if err == nil {
			res, submitted = r, true
			break
		}
		e := apierr.From(err)
		if isDailyCountLimit(e.Message) {
			// 平台按账号限制每日提交次数，积分也不能突破；重试/迁移只会扩大风控。
			s.markQuota(js.Account, e.Message)
			s.finish(js, StFailed, "平台日提交次数已用完（次日 00:00 重置）："+e.Message)
			return
		}
		switch e.Code {
		case apierr.CodeNoExtension, apierr.CodeNotAuthenticated:
			s.markUnavailable(js.Account, err.Error())
			s.failOrRequeue(js, err.Error())
			return
		}
		if n := concurrentFrom(e); n > 0 {
			s.tighten(js.Account, n)
		} else if isQuotaError(e) {
			if !js.UseCredit {
				s.pauseForCredit(js, err.Error())
				return
			}
			if !creditTried {
				// useCredit was pre-approved; retry once with credits after
				// confirming the free allowance is really exhausted.
				s.markFreeExhausted(js.Account, err.Error())
				in.UseCredit = true
				creditTried = true
				continue
			}
			s.markQuota(js.Account, err.Error())
			s.failOrRequeue(js, err.Error())
			return
		}
		// api_error / network_error / timeout: back off and retry (back-pressure).
		select {
		case <-s.stop:
			return
		case <-time.After(backoff):
		}
	}
	if !submitted {
		s.failOrRequeue(js, "提交重试耗尽")
		return
	}
	s.markHealthy(js.Account)
	if js.StrategyKey != "" {
		s.mu.Lock()
		s.sticky[js.StrategyKey] = js.Account
		s.mu.Unlock()
	}

	s.mu.Lock()
	js.RemoteID, js.ListID = res.ID, res.ListID
	s.mu.Unlock()
	if s.tasks != nil {
		t := s.tasks.Add(task.Task{
			StrategyID: js.StrategyID, Mode: mode(js.Compile), Start: js.Start, End: js.End,
			Capital: js.Capital, Frequency: js.Frequency, RemoteID: res.ID, ListID: res.ListID,
			Account: js.Account, AccountUID: js.AccountUID, Status: task.StatusRunning,
		})
		s.mu.Lock()
		js.TaskID = t.ID
		s.mu.Unlock()
	}

	deadline := time.Now().Add(s.jobTimeout)
	submittedAt := time.Now()
	authLostAt := time.Time{}
	for time.Now().Before(deadline) {
		// Poll the error log first: a runtime or compile error never heals, so
		// the run will never produce useful output. Failing here (instead of
		// waiting for jobTimeout) emits job.failed + batch.done immediately,
		// which is what fires the webhook.
		if errs := s.errorLogs(ctx, client, js.RemoteID); len(errs) > 0 {
			s.finishWithLogs(js, StFailed, summarizeErrorLogs(errs), errs)
			return
		}
		if js.Compile {
			// Compile runs produce no metrics, so they are tracked through the
			// build list instead of backtest.stats.
			st := s.compileStatus(ctx, client, js, time.Since(submittedAt))
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
			raw, err := client.BacktestStats(ctx, js.RemoteID)
			if err == nil {
				authLostAt = time.Time{}
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
					// The remote run may still be alive. Re-running elsewhere
					// would burn another quota slot, so wait for the account to
					// come back instead of failing over.
					s.markUnavailable(js.Account, err.Error())
					if authLostAt.IsZero() {
						authLostAt = time.Now()
					}
					if time.Since(authLostAt) > 10*time.Minute {
						s.finish(js, StFailed, "账号掉线超过 10 分钟，远端回测可能仍在运行，id="+js.RemoteID)
						return
					}
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

// pauseForCredit stops a job whose free quota is exhausted until the user
// explicitly approves spending paid credits. It does not fail the batch.
func (s *Scheduler) pauseForCredit(js *JobState, reason string) {
	account := js.Account
	if account == "" {
		account = js.CreditAccount
	}
	s.markFreeExhausted(account, reason)

	var ev Event
	var cb *Callback
	s.mu.Lock()
	js.Status = StNeedsCredit
	js.CreditRequired = true
	js.CreditReason = reason
	js.CreditAccount = account
	js.Account = ""
	js.AccountUID = ""
	js.assigned = nil
	js.Error = ""
	js.ErrorLogs = nil
	ev = s.appendEventLocked(EvJobNeedsCredit, js.BatchID, js, nil)
	if b, ok := s.batches[js.BatchID]; ok && b.Callback != nil {
		c := *b.Callback
		cb = &c
	}
	s.mu.Unlock()
	s.publish(ev, cb)
	s.log.Warn("免费回测额度已用完，等待用户确认是否使用积分", "tag", js.Tag, "account", account)
}

// ConfirmCredit approves or declines paid-credit retries for paused jobs.
func (s *Scheduler) ConfirmCredit(batchID string, approve bool, tags []string) (*Batch, error) {
	filter := map[string]bool{}
	for _, t := range tags {
		filter[t] = true
	}
	s.mu.Lock()
	b, ok := s.batches[batchID]
	if !ok {
		s.mu.Unlock()
		return nil, apierr.New(apierr.CodeNotFound, "批次 %s 不存在", batchID)
	}
	var targets []*JobState
	for _, js := range b.Jobs {
		if js.Status != StNeedsCredit {
			continue
		}
		if len(filter) > 0 && !filter[js.Tag] {
			continue
		}
		targets = append(targets, js)
	}
	if len(targets) == 0 {
		s.mu.Unlock()
		return nil, apierr.Usage("批次 %s 没有待确认的任务", batchID)
	}
	if approve && !s.hasCreditCapacityLocked(time.Now()) {
		s.mu.Unlock()
		return nil, apierr.Usage("所有账号的免费额度都已用完，且没有可用积分；无法继续")
	}
	if approve {
		for _, js := range targets {
			js.Status = StPending
			js.UseCredit = true
			js.CreditRequired = false
			js.CreditReason = ""
			js.Account = ""
			if js.pinned {
				js.Account = js.CreditAccount
			}
			js.AccountUID = ""
			js.assigned = nil
			if js.portable {
				js.StrategyID = ""
			}
			js.queuedAt = time.Now()
			s.jobSeq++
			js.order = s.jobSeq
			s.enqueueLocked(js)
		}
	}
	s.mu.Unlock()

	if !approve {
		for _, js := range targets {
			s.finish(js, StFailed, "用户拒绝使用积分")
		}
	} else {
		s.cond.Broadcast()
	}
	return s.Status(batchID)
}
func (s *Scheduler) failOrRequeue(js *JobState, reason string) {
	if isDailyCountLimit(reason) {
		// 日提交次数上限：迁移只会把其他账号一起拖入风控，直接失败并冷却账号。
		s.markQuota(js.Account, reason)
		s.finish(js, StFailed, reason)
		return
	}
	if s.canFailover(js) {
		s.mu.Lock()
		js.Attempts++
		js.Status = StPending
		js.Account = ""
		js.AccountUID = ""
		js.assigned = nil
		js.StartedAt = time.Time{}
		js.Error = reason
		// Drop the account-local strategy id: the next account needs its own
		// copy (the provisioner will recreate/reuse it there).
		if js.portable {
			js.StrategyID = ""
		}
		js.queuedAt = time.Now()
		s.jobSeq++
		js.order = s.jobSeq
		s.enqueueLocked(js)
		s.mu.Unlock()
		s.log.Warn("任务迁移到其他账号重试", "tag", js.Tag, "attempt", js.Attempts, "reason", reason)
		s.cond.Broadcast()
		return
	}
	s.finish(js, StFailed, reason)
}

func (s *Scheduler) canFailover(js *JobState) bool {
	if js.pinned || !js.portable {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return js.Attempts < s.maxFailovers && len(s.accounts) > 1
}

// AccountPinned reports whether the job was explicitly pinned to an account.
func (js *JobState) AccountPinned() bool { return js.pinned }

func (s *Scheduler) markUnavailable(alias, reason string) {
	s.mu.Lock()
	if a, ok := s.accounts[alias]; ok {
		a.state = AcctLoggedOut
		a.lastError = reason
	}
	s.mu.Unlock()
	s.cond.Broadcast()
}

func (s *Scheduler) markQuota(alias, reason string) {
	s.mu.Lock()
	if a, ok := s.accounts[alias]; ok {
		a.state = AcctQuota
		a.cooldownUntil = quotaCooldown(reason, time.Now())
		a.lastError = reason
	}
	s.mu.Unlock()
	s.cond.Broadcast()
}

// quotaCooldown picks the reset time from the platform message: daily limits
// ("今天/明天") reset at local midnight, other quota failures get a 24h guess.
func quotaCooldown(reason string, now time.Time) time.Time {
	if strings.Contains(reason, "明天") || strings.Contains(reason, "今天") || strings.Contains(reason, "今日") {
		return nextMidnight(now)
	}
	return now.Add(24 * time.Hour)
}

// markFreeExhausted blocks free jobs on this account but still allows jobs the
// user has approved to spend credits. JoinQuant's free allowance is daily, so
// the cooldown ends at the next local midnight rather than 24h from the error.
func (s *Scheduler) markFreeExhausted(alias, reason string) {
	if alias == "" {
		return
	}
	s.mu.Lock()
	if a, ok := s.accounts[alias]; ok {
		a.state = AcctFreeOut
		a.cooldownUntil = nextMidnight(time.Now())
		a.lastError = reason
	}
	s.mu.Unlock()
	s.cond.Broadcast()
}

func nextMidnight(now time.Time) time.Time {
	y, m, d := now.Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, now.Location())
}

func (s *Scheduler) markHealthy(alias string) {
	s.mu.Lock()
	if a, ok := s.accounts[alias]; ok {
		a.cooldownUntil = time.Time{}
		a.lastError = ""
		if a.state == AcctCooling || a.state == AcctBusy || a.state == AcctLoggedOut || a.state == AcctPending {
			a.state = AcctReady
		}
	}
	s.mu.Unlock()
}

// sweeper fails jobs that waited too long for an account and clears expired
// cooldowns.
func (s *Scheduler) sweeper() {
	defer s.wg.Done()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
		}
		now := time.Now()
		var expired []*JobState
		s.mu.Lock()
		keep := s.pending[:0]
		for _, js := range s.pending {
			if now.Sub(js.queuedAt) > s.jobSubmitWait {
				expired = append(expired, js)
				continue
			}
			keep = append(keep, js)
		}
		s.pending = keep
		for _, a := range s.accounts {
			if (a.state == AcctCooling || a.state == AcctQuota || a.state == AcctFreeOut) && !now.Before(a.cooldownUntil) {
				a.state = AcctReady
				a.cooldownUntil = time.Time{}
			}
		}
		s.mu.Unlock()
		for _, js := range expired {
			errMsg := "等待可用账号超时"
			if js.Account != "" {
				errMsg = "账号 " + js.Account + " 不可用，等待超时"
			}
			s.start(js)
			s.finish(js, StFailed, errMsg)
		}
		s.cond.Broadcast()
	}
}

func (s *Scheduler) resolveCode(js *JobState) (string, error) {
	if js.Code != "" {
		return js.Code, nil
	}
	if js.CodePath == "" {
		return "", apierr.Usage("job 需要 strategyId，或 strategyKey/code/codePath 之一")
	}
	raw, err := os.ReadFile(js.CodePath)
	if err != nil {
		return "", apierr.Usage("读取 codePath 失败: %v", err)
	}
	if len(raw) > 2<<20 {
		return "", apierr.Usage("策略源码超过 2 MiB")
	}
	return string(raw), nil
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
func (s *Scheduler) compileStatus(ctx context.Context, client backtestClient, js *JobState, elapsed time.Duration) string {
	if client == nil {
		return ""
	}
	raw, err := client.BacktestList(ctx, jq.BacktestListInput{
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
func (s *Scheduler) errorLogs(ctx context.Context, client backtestClient, backtestID string) []string {
	if client == nil || backtestID == "" {
		return nil
	}
	raw, err := client.BacktestLogs(ctx, jq.BacktestLogsInput{BacktestID: backtestID, Error: true})
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
	js.Error = ""
	js.ErrorLogs = nil
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
	js.Error = errMsg
	if len(errLogs) > 0 {
		js.ErrorLogs = errLogs
	} else if st == StDone {
		js.ErrorLogs = nil
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

// snapshotWithProgressLocked copies a batch and fills ETA estimates for its
// running jobs. Caller must hold s.mu.
func (s *Scheduler) snapshotWithProgressLocked(b *Batch) *Batch {
	cp := snapshot(b)
	type hist struct {
		key  string
		rate float64
	}
	var all []hist
	for _, bb := range s.batches {
		for _, js := range bb.Jobs {
			if js.Status != StDone || js.Compile || js.StartedAt.IsZero() || js.EndedAt.IsZero() {
				// 编译任务只跑 10 天，不能当作“秒/模拟年”的样本
				continue
			}
			years := windowYears(js.Start, js.End)
			dur := js.EndedAt.Sub(js.StartedAt).Seconds()
			if years <= 0 || dur <= 0 {
				continue
			}
			all = append(all, hist{key: js.StrategyKey, rate: dur / years})
		}
	}
	for _, js := range cp.Jobs {
		if js.Status != StRunning || js.StartedAt.IsZero() {
			continue
		}
		years := windowYears(js.Start, js.End)
		if years <= 0 {
			continue
		}
		same := make([]float64, 0, len(all))
		global := make([]float64, 0, len(all))
		for _, h := range all {
			global = append(global, h.rate)
			if js.StrategyKey != "" && h.key == js.StrategyKey {
				same = append(same, h.rate)
			}
		}
		rate, basis, samples := 0.0, "", 0
		if len(same) >= 2 {
			rate, basis, samples = medianFloat(same), "strategyKey", len(same)
		} else if len(global) >= 2 {
			rate, basis, samples = medianFloat(global), "global", len(global)
		}
		if rate <= 0 {
			continue
		}
		elapsed := time.Since(js.StartedAt).Seconds()
		total := rate * years
		pct := elapsed / total * 100
		if pct < 0 {
			pct = 0
		}
		if pct > 99 {
			pct = 99
		}
		eta := total - elapsed
		if eta < 0 {
			eta = 0
		}
		js.Progress = &ProgressEstimate{
			ElapsedSeconds:   math.Round(elapsed),
			EstimatedSeconds: math.Round(total),
			Percent:          math.Round(pct*10) / 10,
			EtaSeconds:       math.Round(eta),
			Basis:            basis,
			Samples:          samples,
		}
	}
	return cp
}

// windowYears converts a backtest window into calendar years.
func windowYears(start, end string) float64 {
	st, err := time.Parse("2006-01-02", start)
	if err != nil {
		return 0
	}
	et := time.Now()
	if end != "" {
		if t, err := time.Parse("2006-01-02", end); err == nil {
			et = t
		}
	}
	if et.Before(st) {
		return 0
	}
	return et.Sub(st).Hours() / 24 / 365.25
}

func medianFloat(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	sort.Float64s(v)
	n := len(v)
	if n%2 == 1 {
		return v[n/2]
	}
	return (v[n/2-1] + v[n/2]) / 2
}

func snapshot(b *Batch) *Batch {
	cp := &Batch{ID: b.ID, CreatedAt: b.CreatedAt, SessionID: b.SessionID}
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

// needsCreditJobs lists jobs paused for paid-credit approval.
func needsCreditJobs(b *Batch) []*JobState {
	var out []*JobState
	for _, js := range b.Jobs {
		if js.Status == StNeedsCredit {
			out = append(out, js)
		}
	}
	return out
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
