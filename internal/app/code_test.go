package app

import (
	"strings"
	"testing"
)

func TestStandardizeCodeInjectsIntoInitialize(t *testing.T) {
	src := "def initialize(context):\n    g.security = '000001.XSHE'\n"
	out := StandardizeCode(src)
	if !strings.Contains(out, standardBegin) || !strings.Contains(out, standardEnd) {
		t.Fatalf("marker block missing:\n%s", out)
	}
	if !strings.Contains(out, "set_slippage(FixedSlippage(0.002)") {
		t.Fatalf("slippage call missing:\n%s", out)
	}
	// The injected block must sit inside initialize (4-space indent, after def line).
	if !strings.Contains(out, "def initialize(context):\n    "+standardBegin) {
		t.Fatalf("injected block not indented under initialize:\n%s", out)
	}
}

func TestStandardizeCodeIsIdempotent(t *testing.T) {
	src := "def initialize(context):\n    pass\n"
	once := StandardizeCode(src)
	twice := StandardizeCode(once)
	if once != twice {
		t.Fatalf("standardize not idempotent:\n--- once ---\n%s\n--- twice ---\n%s", once, twice)
	}
	if strings.Count(twice, standardBegin) != 1 {
		t.Fatalf("expected exactly one marker block, got %d", strings.Count(twice, standardBegin))
	}
}

func TestStandardizeCodePrependsWhenNoInitialize(t *testing.T) {
	src := "import numpy as np\n"
	out := StandardizeCode(src)
	if !strings.HasPrefix(out, "def initialize(context):\n") {
		t.Fatalf("expected new initialize at top:\n%s", out)
	}
	if !strings.Contains(out, "import numpy as np") {
		t.Fatalf("original body lost:\n%s", out)
	}
}
