package account

import (
	"path/filepath"
	"testing"
)

func TestBindConnectionBootstrapsAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	r, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	acc, err := r.BindConnection("a1", "uid-1", "nick")
	if err != nil {
		t.Fatal(err)
	}
	if acc.Alias != "a1" || acc.UID != "uid-1" || !acc.Enabled {
		t.Fatalf("unexpected account: %+v", acc)
	}
	if r.DefaultAlias() != "a1" {
		t.Fatalf("default = %q", r.DefaultAlias())
	}
	// Reload from disk.
	r2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := r2.ByUID("uid-1"); !ok || got.Alias != "a1" {
		t.Fatalf("reload lost binding: %+v %v", got, ok)
	}
	// Same uid on a new connection reuses the account.
	again, err := r.BindConnection("", "uid-1", "nick2")
	if err != nil {
		t.Fatal(err)
	}
	if again.Alias != "a1" || again.Nickname != "nick2" {
		t.Fatalf("duplicate uid did not reuse: %+v", again)
	}
}

func TestBindConnectionBootstrapWithoutHintUsesDefault(t *testing.T) {
	r, _ := Load(filepath.Join(t.TempDir(), "accounts.json"))
	acc, err := r.BindConnection("", "uid-x", "nick")
	if err != nil {
		t.Fatal(err)
	}
	if acc.Alias != "default" {
		t.Fatalf("bootstrap alias = %q, want default", acc.Alias)
	}
}

func TestBindConnectionRejectsAliasConflict(t *testing.T) {
	r, _ := Load(filepath.Join(t.TempDir(), "accounts.json"))
	if _, err := r.BindConnection("a1", "uid-1", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.BindConnection("a1", "uid-2", ""); err == nil {
		t.Fatal("expected alias conflict")
	}
}

func TestBindConnectionUnknownWithReject(t *testing.T) {
	r, _ := Load(filepath.Join(t.TempDir(), "accounts.json"))
	if _, err := r.BindConnection("a1", "uid-1", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.BindConnection("a2", "uid-2", ""); err == nil {
		t.Fatal("expected rejection for unregistered alias")
	}
	// No hint and reject -> rejected as well.
	if _, err := r.BindConnection("", "uid-3", ""); err == nil {
		t.Fatal("expected rejection for unknown uid")
	}
}

func TestBindConnectionAutoAccept(t *testing.T) {
	r, _ := Load(filepath.Join(t.TempDir(), "accounts.json"))
	r.data.Unregistered = UnregisteredAccept
	if _, err := r.BindConnection("a1", "uid-1", ""); err != nil {
		t.Fatal(err)
	}
	acc, err := r.BindConnection("", "uid-2", "two")
	if err != nil {
		t.Fatal(err)
	}
	if acc.Alias != "acct-uid-2" {
		t.Fatalf("auto alias = %q", acc.Alias)
	}
}

func TestAddCapRemove(t *testing.T) {
	r, _ := Load(filepath.Join(t.TempDir(), "accounts.json"))
	if _, err := r.Add("bad alias!", 1); err == nil {
		t.Fatal("expected alias validation error")
	}
	a, err := r.Add("a1", 2)
	if err != nil {
		t.Fatal(err)
	}
	if a.Cap != 2 || a.ProfileDir == "" {
		t.Fatalf("unexpected account: %+v", a)
	}
	if err := r.SetCap("a1", 5); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Get("a1"); got.Cap != 5 {
		t.Fatalf("cap = %d", got.Cap)
	}
	if err := r.SetEnabled("a1", false); err != nil {
		t.Fatal(err)
	}
	if r.DefaultAlias() != "" {
		t.Fatalf("disabled account should not be default, got %q", r.DefaultAlias())
	}
	if err := r.Remove("a1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Get("a1"); ok {
		t.Fatal("account still present")
	}
}
