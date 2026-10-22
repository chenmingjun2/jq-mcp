package browser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRandomUAHasConsistentTokens(t *testing.T) {
	for i := 0; i < 200; i++ {
		spec := RandomUA([]string{"windows", "linux", "android"}, []string{"chrome", "chromium", "firefox"})
		if spec.UserAgent == "" || spec.Platform == "" || spec.Browser == "" {
			t.Fatalf("empty spec: %+v", spec)
		}
		switch spec.Browser {
		case "firefox":
			if !strings.Contains(spec.UserAgent, "Firefox/") || strings.Contains(spec.UserAgent, "Chrome/") {
				t.Fatalf("bad firefox UA: %s", spec.UserAgent)
			}
		default:
			if !strings.Contains(spec.UserAgent, "Chrome/") {
				t.Fatalf("bad chromium UA: %s", spec.UserAgent)
			}
		}
		if spec.Platform == "android" && !strings.Contains(spec.UserAgent, "Android") {
			t.Fatalf("android UA missing Android: %s", spec.UserAgent)
		}
	}
}

func TestRandomUASafariFallsBackToPlausibleOS(t *testing.T) {
	for i := 0; i < 50; i++ {
		spec := RandomUA([]string{"android"}, []string{"safari"})
		if spec.Browser != "safari" {
			t.Fatalf("browser = %s", spec.Browser)
		}
		if spec.Platform == "android" || spec.Platform == "windows" || spec.Platform == "linux" {
			t.Fatalf("Safari on %s is not plausible: %s", spec.Platform, spec.UserAgent)
		}
		if !strings.Contains(spec.UserAgent, "Safari/") {
			t.Fatalf("bad safari UA: %s", spec.UserAgent)
		}
	}
}

func TestRandomUAFiltersAreCaseInsensitive(t *testing.T) {
	for i := 0; i < 30; i++ {
		spec := RandomUA([]string{"WINDOWS"}, []string{"FIREFOX"})
		if spec.Platform != "windows" || spec.Browser != "firefox" {
			t.Fatalf("spec = %+v", spec)
		}
	}
}

func TestPrepareProfileKeepsUA(t *testing.T) {
	home := t.TempDir()
	t.Setenv("JQ_MCP_HOME", home)
	ext := t.TempDir()
	if err := os.WriteFile(filepath.Join(ext, "manifest.json"), []byte(`{"version":"1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(ext, "lib"), 0o700)
	os.WriteFile(filepath.Join(ext, "lib", "protocol.js"), []byte("export const VERSION='1'"), 0o600)

	p1, err := PrepareProfile("a1", ext, "ws://127.0.0.1:1/ws", "t")
	if err != nil {
		t.Fatal(err)
	}
	first := p1.EnsureUA("random", []string{"windows"}, []string{"chrome"})
	p2, err := PrepareProfile("a1", ext, "ws://127.0.0.1:1/ws", "t")
	if err != nil {
		t.Fatal(err)
	}
	second := p2.EnsureUA("random", []string{"linux"}, []string{"firefox"})
	if first == "" || second != first {
		t.Fatalf("UA not persisted: %q vs %q", first, second)
	}
}

func TestEnsureUAIsStableUntilRerolled(t *testing.T) {
	p := &Profile{Dir: t.TempDir()}
	first := p.EnsureUA("random", []string{"windows"}, []string{"chrome"})
	if first == "" || p.UAPlatform != "windows" {
		t.Fatalf("ua = %q platform=%s", first, p.UAPlatform)
	}
	second := p.EnsureUA("random", []string{"linux"}, []string{"firefox"})
	if second != first {
		t.Fatalf("UA changed on second launch: %q -> %q", first, second)
	}
	p.SetUA("", "", "")
	if got := p.EnsureUA("off", nil, nil); got != "" {
		t.Fatalf("off should clear UA, got %q", got)
	}
}
