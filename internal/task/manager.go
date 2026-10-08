// Package task records backtest runs that jq-mcp has submitted so clients can
// poll them without re-discovering identifiers.
package task

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/jqhelper/jq-mcp/internal/apierr"
)

// Status values for a tracked backtest task.
const (
	StatusRunning   = "running"
	StatusDone      = "done"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
	StatusTimeout   = "timeout"
)

// Task describes one submitted backtest.
type Task struct {
	ID         string    `json:"id"`
	StrategyID string    `json:"strategyId"`
	Mode       string    `json:"mode"` // backtest | compile
	Start      string    `json:"start"`
	End        string    `json:"end,omitempty"`
	Capital    float64   `json:"capital,omitempty"`
	Frequency  string    `json:"frequency,omitempty"`
	RemoteID   string    `json:"remoteId,omitempty"`
	ListID     string    `json:"listId,omitempty"`
	Status     string    `json:"status"`
	Error      string    `json:"error,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// Manager keeps tasks in memory and optionally mirrors them to a JSON file.
type Manager struct {
	mu    sync.Mutex
	tasks map[string]*Task
	order []string
	file  string
	seq   int
}

// NewManager creates a manager. When file is non-empty the manager loads any
// existing snapshot and persists after every mutation.
func NewManager(file string) *Manager {
	m := &Manager{tasks: make(map[string]*Task), file: file}
	if file != "" {
		m.load()
	}
	return m
}

func (m *Manager) load() {
	raw, err := os.ReadFile(m.file)
	if err != nil {
		return
	}
	var snap struct {
		Seq   int     `json:"seq"`
		Tasks []*Task `json:"tasks"`
	}
	if err := json.Unmarshal(raw, &snap); err != nil {
		return
	}
	m.seq = snap.Seq
	for _, t := range snap.Tasks {
		if t == nil || t.ID == "" {
			continue
		}
		if _, ok := m.tasks[t.ID]; ok {
			continue
		}
		m.tasks[t.ID] = t
		m.order = append(m.order, t.ID)
	}
}

func (m *Manager) persistLocked() {
	if m.file == "" {
		return
	}
	snap := struct {
		Seq   int     `json:"seq"`
		Tasks []*Task `json:"tasks"`
	}{Seq: m.seq}
	for _, id := range m.order {
		snap.Tasks = append(snap.Tasks, m.tasks[id])
	}
	raw, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(m.file), 0o700); err != nil {
		return
	}
	tmp := m.file + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, m.file)
}

// Add registers a new task and returns it.
func (m *Manager) Add(t Task) *Task {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	t.ID = "task-" + time.Now().Format("20060102") + "-" + itoa(m.seq)
	now := time.Now()
	t.CreatedAt = now
	t.UpdatedAt = now
	if t.Status == "" {
		t.Status = StatusRunning
	}
	m.tasks[t.ID] = &t
	m.order = append(m.order, t.ID)
	m.persistLocked()
	cp := t
	return &cp
}

// Get returns a task by ID.
func (m *Manager) Get(id string) (*Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return nil, apierr.New(apierr.CodeNotFound, "任务 %s 不存在", id)
	}
	cp := *t
	return &cp, nil
}

// Update mutates a task via fn.
func (m *Manager) Update(id string, fn func(*Task)) (*Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return nil, apierr.New(apierr.CodeNotFound, "任务 %s 不存在", id)
	}
	fn(t)
	t.UpdatedAt = time.Now()
	m.persistLocked()
	cp := *t
	return &cp, nil
}

// List returns tasks, newest first, capped at limit (limit<=0 means all).
func (m *Manager) List(limit int) []Task {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Task, 0, len(m.order))
	for i := len(m.order) - 1; i >= 0; i-- {
		if t, ok := m.tasks[m.order[i]]; ok {
			out = append(out, *t)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
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
