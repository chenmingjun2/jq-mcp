package sched

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/jqhelper/jq-mcp/internal/apierr"
)

// Lifecycle event types. Every event is appended to a per-batch log and fanned
// out to the MCP notification sink and (optionally) an HTTP webhook.
const (
	EvJobStarted = "job.started"
	EvJobDone    = "job.done"
	EvJobFailed  = "job.failed"
	EvJobTimeout = "job.timeout"
	EvBatchDone  = "batch.done"
)

// Event is one entry in a batch's lifecycle log. Job is set for job.* events;
// Jobs is set for batch.done. Both are snapshots taken at emit time.
type Event struct {
	ID        int64       `json:"eventId"`
	Type      string      `json:"type"`
	BatchID   string      `json:"batchId"`
	SessionID string      `json:"sessionId,omitempty"`
	At        time.Time   `json:"at"`
	Job       *JobState   `json:"job,omitempty"`
	Jobs      []*JobState `json:"jobs,omitempty"`
}

// Events returns the events for a batch with ID > since, oldest first. limit<=0
// means no limit. It also returns the cursor to pass on the next call and
// whether more matching events remain.
func (s *Scheduler) Events(batchID string, since int64, limit int) ([]Event, int64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.batches[batchID]; !ok {
		return nil, since, false, apierr.New(apierr.CodeNotFound, "批次 %s 不存在", batchID)
	}
	all := s.events[batchID]
	out := make([]Event, 0, len(all))
	for _, ev := range all {
		if ev.ID > since {
			out = append(out, ev)
		}
	}
	next := since
	more := false
	if limit > 0 && len(out) > limit {
		out = out[:limit]
		more = true
	}
	if len(out) > 0 {
		next = out[len(out)-1].ID
	}
	return out, next, more, nil
}

// WaitEvents blocks until the batch is terminal, the timeout elapses or ctx is
// cancelled. When since>0 it also returns as soon as a new event arrives, which
// turns the call into an incremental follow. progress, when non-nil, is called
// once per wakeup with the current done/total counts.
func (s *Scheduler) WaitEvents(ctx context.Context, batchID string, since int64, timeout time.Duration, progress func(done, total int)) (*WaitResult, error) {
	deadline := time.Now().Add(timeout)
	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()
	for {
		b, evs, next, more, ch, err := s.probe(batchID, since)
		if err != nil {
			return nil, err
		}
		done := allTerminal(b)
		if done || (since > 0 && len(evs) > 0) || !time.Now().Before(deadline) {
			return &WaitResult{Batch: b, Events: evs, NextEventID: next, Done: done, HasMore: more}, nil
		}
		if progress != nil {
			d, total := progressCounts(b)
			progress(d, total)
		}
		timer := time.NewTimer(time.Until(deadline))
		select {
		case <-ctx.Done():
			timer.Stop()
			return &WaitResult{Batch: b, Events: evs, NextEventID: next, Done: done, HasMore: more}, ctx.Err()
		case <-ch:
			timer.Stop()
		case <-heartbeat.C:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// NextEvents returns as soon as at least one event newer than since exists;
// otherwise it blocks up to timeout. It never treats "no events" as an error,
// so it doubles as a long-poll for event consumers.
func (s *Scheduler) NextEvents(ctx context.Context, batchID string, since int64, timeout time.Duration, limit int) (*WaitResult, error) {
	deadline := time.Now().Add(timeout)
	for {
		b, evs, next, more, ch, err := s.probe(batchID, since)
		if err != nil {
			return nil, err
		}
		if len(evs) > 0 || !time.Now().Before(deadline) {
			if limit > 0 && len(evs) > limit {
				evs = evs[:limit]
				next = evs[len(evs)-1].ID
				more = true
			}
			return &WaitResult{Batch: b, Events: evs, NextEventID: next, Done: allTerminal(b), HasMore: more}, nil
		}
		timer := time.NewTimer(time.Until(deadline))
		select {
		case <-ctx.Done():
			timer.Stop()
			return &WaitResult{Batch: b, Events: evs, NextEventID: next, Done: allTerminal(b), HasMore: more}, ctx.Err()
		case <-ch:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// probe atomically snapshots the batch, its new events and the wakeup channel.
// Reading the channel under the same lock as the events prevents a missed
// notification between snapshot and wait.
func (s *Scheduler) probe(batchID string, since int64) (*Batch, []Event, int64, bool, <-chan struct{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.batches[batchID]
	if !ok {
		return nil, nil, since, false, nil, apierr.New(apierr.CodeNotFound, "批次 %s 不存在", batchID)
	}
	out := make([]Event, 0, len(s.events[batchID]))
	for _, ev := range s.events[batchID] {
		if ev.ID > since {
			out = append(out, ev)
		}
	}
	next := since
	if len(out) > 0 {
		next = out[len(out)-1].ID
	}
	more := false
	if len(out) > maxWaitEvents {
		out = out[:maxWaitEvents]
		next = out[len(out)-1].ID
		more = true
	}
	return snapshot(b), out, next, more, s.eventCh, nil
}

// maxWaitEvents caps the event payload of a wait/follow response. Batch state
// itself is always complete, so a truncated tail can be fetched by cursor.
const maxWaitEvents = 500

// AllEvents returns events across every batch with ID > since, oldest first.
// It backs the local HTTP event feed used by external orchestrators (the
// OpenCode jq-loop plugin), which must not miss events from any batch.
func (s *Scheduler) AllEvents(since int64, limit int) ([]Event, int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.allEventsLocked(since, limit)
}

// NextAllEvents long-polls the cross-batch event feed: it returns as soon as at
// least one event newer than since exists, or when timeout elapses.
func (s *Scheduler) NextAllEvents(ctx context.Context, since int64, timeout time.Duration, limit int) ([]Event, int64, bool) {
	deadline := time.Now().Add(timeout)
	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()
	for {
		s.mu.Lock()
		evs, next, more := s.allEventsLocked(since, limit)
		ch := s.eventCh
		s.mu.Unlock()
		if len(evs) > 0 || !time.Now().Before(deadline) {
			return evs, next, more
		}
		timer := time.NewTimer(time.Until(deadline))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, since, false
		case <-ch:
			timer.Stop()
		case <-heartbeat.C:
			timer.Stop()
		case <-timer.C:
			timer.Stop()
		}
	}
}

// LastEventID returns the newest event sequence number.
func (s *Scheduler) LastEventID() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.eventSeq
}

func (s *Scheduler) allEventsLocked(since int64, limit int) ([]Event, int64, bool) {
	if limit <= 0 || limit > maxWaitEvents {
		limit = maxWaitEvents
	}
	var all []Event
	for _, evs := range s.events {
		for _, ev := range evs {
			if ev.ID > since {
				all = append(all, ev)
			}
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	next := since
	more := false
	if len(all) > limit {
		all = all[:limit]
		more = true
	}
	if len(all) > 0 {
		next = all[len(all)-1].ID
	}
	return all, next, more
}

func (s *Scheduler) appendEventLocked(typ, batchID string, js *JobState, jobs []*JobState) Event {
	s.eventSeq++
	ev := Event{ID: s.eventSeq, Type: typ, BatchID: batchID, At: time.Now()}
	if b, ok := s.batches[batchID]; ok {
		ev.SessionID = b.SessionID
	}
	if js != nil {
		cp := *js
		ev.Job = &cp
	}
	for _, j := range jobs {
		c := *j
		ev.Jobs = append(ev.Jobs, &c)
	}
	s.events[batchID] = append(s.events[batchID], ev)
	// Wake everyone waiting on this batch (or on any batch).
	close(s.eventCh)
	s.eventCh = make(chan struct{})
	return ev
}

func (s *Scheduler) publish(ev Event, cb *Callback) {
	s.mu.Lock()
	sink := s.eventSink
	s.mu.Unlock()
	if sink != nil {
		sink(ev)
	}
	if cb != nil && cb.URL != "" && cb.wants(ev.Type) {
		s.callbacks.enqueue(ev, cb)
	}
}

func (c *Callback) wants(typ string) bool {
	switch c.On {
	case "both":
		return true
	case "job":
		return strings.HasPrefix(typ, "job.")
	default: // "batch" (also covers the empty value)
		return typ == EvBatchDone
	}
}

func eventSuffix(status string) string {
	switch status {
	case StDone:
		return "done"
	case StFailed:
		return "failed"
	case StTimeout:
		return "timeout"
	default:
		return status
	}
}

func progressCounts(b *Batch) (done, total int) {
	total = len(b.Jobs)
	for _, js := range b.Jobs {
		switch js.Status {
		case StDone, StFailed, StTimeout:
			done++
		}
	}
	return done, total
}
