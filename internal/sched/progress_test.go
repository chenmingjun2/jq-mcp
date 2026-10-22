package sched

import (
	"testing"
	"time"
)

func TestProgressEstimateFromCompletedJobs(t *testing.T) {
	s := newPoolTestScheduler(newPoolStats(), nil)

	// Two completed one-year jobs: 400s each => 400 s per simulated year.
	hist := &Batch{ID: "hist"}
	for i := 0; i < 2; i++ {
		st := time.Now().Add(-time.Hour)
		hist.Jobs = append(hist.Jobs, &JobState{
			Job:       Job{Tag: "h", Start: "2024-01-01", End: "2024-12-31", StrategyKey: "k"},
			Status:    StDone,
			StartedAt: st,
			EndedAt:   st.Add(400 * time.Second),
		})
	}
	s.mu.Lock()
	s.batches["hist"] = hist
	s.batches["b"] = &Batch{ID: "b", Jobs: []*JobState{{
		Job:       Job{Tag: "r", Start: "2023-01-01", End: "2024-12-31", StrategyKey: "k"},
		Status:    StRunning,
		StartedAt: time.Now().Add(-200 * time.Second),
	}}}
	s.mu.Unlock()

	snap, err := s.Status("b")
	if err != nil {
		t.Fatal(err)
	}
	p := snap.Jobs[0].Progress
	if p == nil {
		t.Fatal("expected a progress estimate")
	}
	if p.Basis != "strategyKey" || p.Samples != 2 {
		t.Fatalf("basis=%s samples=%d, want strategyKey/2", p.Basis, p.Samples)
	}
	// Two simulated years at 400 s/yr => ~800s total; 200s elapsed => ~25%.
	if p.EstimatedSeconds < 700 || p.EstimatedSeconds > 900 {
		t.Fatalf("estimated=%v, want ~800", p.EstimatedSeconds)
	}
	if p.Percent < 20 || p.Percent > 30 {
		t.Fatalf("percent=%v, want ~25", p.Percent)
	}
	if p.EtaSeconds < 500 || p.EtaSeconds > 700 {
		t.Fatalf("eta=%v, want ~600", p.EtaSeconds)
	}
}

func TestProgressEstimateNeedsHistory(t *testing.T) {
	s := newPoolTestScheduler(newPoolStats(), nil)
	s.mu.Lock()
	s.batches["b"] = &Batch{ID: "b", Jobs: []*JobState{{
		Job:       Job{Tag: "r", Start: "2024-01-01", End: "2024-12-31", StrategyKey: "k"},
		Status:    StRunning,
		StartedAt: time.Now().Add(-60 * time.Second),
	}}}
	s.mu.Unlock()
	snap, err := s.Status("b")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Jobs[0].Progress != nil {
		t.Fatalf("expected no estimate without history, got %+v", snap.Jobs[0].Progress)
	}
}
