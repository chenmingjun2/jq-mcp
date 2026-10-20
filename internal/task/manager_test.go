package task

import "testing"

func TestFindByRemoteID(t *testing.T) {
	m := NewManager("")
	first := m.Add(Task{RemoteID: "bt-1", ListID: "list-1", Mode: "backtest"})
	m.Add(Task{RemoteID: "bt-2", ListID: "list-2", Mode: "compile"})

	if got := m.FindByRemoteID("bt-1"); got == nil || got.ID != first.ID {
		t.Fatalf("by detail id = %+v", got)
	}
	got := m.FindByRemoteID("list-2")
	if got == nil || got.Mode != "compile" {
		t.Fatalf("by list id = %+v", got)
	}
	if m.FindByRemoteID("missing") != nil || m.FindByRemoteID("") != nil {
		t.Fatal("expected nil for unknown/empty id")
	}
}

func TestFindByRemoteIDPrefersNewest(t *testing.T) {
	m := NewManager("")
	m.Add(Task{RemoteID: "bt-1", Mode: "backtest"})
	newest := m.Add(Task{RemoteID: "bt-1", Mode: "compile"})

	got := m.FindByRemoteID("bt-1")
	if got == nil || got.ID != newest.ID {
		t.Fatalf("got %+v, want newest %s", got, newest.ID)
	}
}
