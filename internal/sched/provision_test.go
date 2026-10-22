package sched

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/jqhelper/jq-mcp/internal/apierr"
	"github.com/jqhelper/jq-mcp/internal/jq"
)

type fakeStrategyClient struct {
	mu       sync.Mutex
	creates  int
	saves    int
	nextID   int
	missing  bool
	lastCode string
}

func (f *fakeStrategyClient) StrategyCreate(_ context.Context, in jq.StrategyCreateInput) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates++
	f.nextID++
	f.lastCode = in.Code
	return json.Marshal(map[string]string{"id": "sid-" + string(rune('0'+f.nextID))})
}

func (f *fakeStrategyClient) StrategySave(_ context.Context, in jq.StrategySaveInput) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saves++
	if f.missing {
		return nil, apierr.New(apierr.CodeNotFound, "策略不存在")
	}
	f.lastCode = in.Code
	return json.Marshal(map[string]string{"id": in.StrategyID})
}

func TestProvisionReusesAndUpdates(t *testing.T) {
	dir := t.TempDir()
	p := newProvisioner(slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.setDir(dir)
	client := &fakeStrategyClient{}
	ctx := context.Background()

	id1, err := p.ensure(ctx, client, "a1", "key", "name", "print(1)")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := p.ensure(ctx, client, "a1", "key", "name", "print(1)")
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 || client.creates != 1 || client.saves != 0 {
		t.Fatalf("id1=%s id2=%s creates=%d saves=%d", id1, id2, client.creates, client.saves)
	}

	id3, err := p.ensure(ctx, client, "a1", "key", "name", "print(2)")
	if err != nil {
		t.Fatal(err)
	}
	if id3 != id1 || client.saves != 1 {
		t.Fatalf("expected save on changed code: id=%s saves=%d", id3, client.saves)
	}

	// Cached id disappeared remotely -> recreate once.
	client.missing = true
	id4, err := p.ensure(ctx, client, "a1", "key", "name", "print(3)")
	if err != nil {
		t.Fatal(err)
	}
	if id4 == id1 || client.creates != 2 {
		t.Fatalf("expected recreate: id=%s creates=%d", id4, client.creates)
	}
}

func TestProvisionPerAccountIsolation(t *testing.T) {
	p := newProvisioner(nil)
	p.setDir(t.TempDir())
	client := &fakeStrategyClient{}
	ctx := context.Background()
	a, err := p.ensure(ctx, client, "a1", "key", "name", "code")
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.ensure(ctx, client, "a2", "key", "name", "code")
	if err != nil {
		t.Fatal(err)
	}
	if a == b || client.creates != 2 {
		t.Fatalf("accounts shared a copy: a=%s b=%s creates=%d", a, b, client.creates)
	}
}
