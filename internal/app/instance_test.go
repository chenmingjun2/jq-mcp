package app

import (
	"errors"
	"os"
	"testing"
)

func TestAcquireInstanceIsExclusive(t *testing.T) {
	t.Setenv("JQ_MCP_HOME", t.TempDir())

	l1, err := AcquireInstance("t")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer l1.Close()

	if _, err := AcquireInstance("t"); err == nil {
		t.Fatal("second acquire should fail while the first is held")
	} else {
		var running *ErrAlreadyRunning
		if !errors.As(err, &running) {
			t.Fatalf("expected ErrAlreadyRunning, got %v", err)
		}
	}

	// A different profile is independent.
	l2, err := AcquireInstance("other")
	if err != nil {
		t.Fatalf("other profile should not conflict: %v", err)
	}
	_ = l2.Close()
}

func TestPublishAndCloseRemovesDescriptor(t *testing.T) {
	t.Setenv("JQ_MCP_HOME", t.TempDir())
	l, err := AcquireInstance("t")
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Publish(Descriptor{ID: "t-1", Profile: "t", HTTPURL: "http://127.0.0.1:9999"}); err != nil {
		t.Fatal(err)
	}
	d, ok := ReadDescriptor("t")
	if !ok || d.HTTPURL != "http://127.0.0.1:9999" || d.Protocol != Protocol {
		t.Fatalf("descriptor not written: %+v ok=%v", d, ok)
	}
	if fi, err := os.Stat(descriptorPath("t")); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Fatalf("descriptor mode = %v, want 0600", fi.Mode().Perm())
	}
	_ = l.Close()
	if _, ok := ReadDescriptor("t"); ok {
		t.Fatal("descriptor should be removed on close")
	}
}
