package controller

import (
	"strings"
	"testing"

	"github.com/onsi/gomega"
)

// TestDerivedNameShortUnchanged pins the D20 non-regression: every existing
// Loop name + suffix that already fits the 63-char DNS-1035 budget is
// returned UNCHANGED (an upgrade must not orphan the existing
// '<loop>-egress-proxy' Deployment/Service/netpol by renaming it).
func TestDerivedNameShortUnchanged(t *testing.T) {
	// A short Loop name: the full name fits, so it is returned as-is.
	if got := derivedName("gocli-task1", "-egress-proxy"); got != "gocli-task1-egress-proxy" {
		t.Fatalf("short name must be unchanged, got %q", got)
	}
	if got := derivedName("gocli-task1", "-deliver"); got != "gocli-task1-deliver" {
		t.Fatalf("short deliver name must be unchanged, got %q", got)
	}
	if got := derivedName("gocli-task1", "-deliver-np"); got != "gocli-task1-deliver-np" {
		t.Fatalf("short deliver-np name must be unchanged, got %q", got)
	}
	// Exactly 63 chars is still "fits" (<= budget) and must be unchanged.
	// 63 - len("-egress-proxy")(13) = 50-char loop name.
	var loop50 strings.Builder
	for range 50 {
		loop50.WriteByte('a')
	}
	l50 := loop50.String()
	full := l50 + "-egress-proxy"
	if len(full) != 63 {
		t.Fatalf("test-setup: expected 63-char full name, got %d", len(full))
	}
	if got := derivedName(l50, "-egress-proxy"); got != full {
		t.Fatalf("exactly-63 name must be unchanged, got %q", got)
	}
}

// TestDerivedNameLongTruncatedAndDeterministic pins the D20 near-max case: a
// 55-char (the max valid Loop) name + "-egress-proxy" = 69 > 63, so the name
// is truncated + hashed to stay <= 63, and the result is deterministic
// (identical across two calls) so the reconciler's create/update idempotency
// holds.
func TestDerivedNameLongTruncatedAndDeterministic(t *testing.T) {
	var b strings.Builder
	for range 55 {
		b.WriteByte('b')
	}
	l55 := b.String()
	if len(l55) != 55 {
		t.Fatalf("test-setup: expected 55-char loop name, got %d", len(l55))
	}

	for _, suffix := range []string{"-egress-proxy", "-deliver-np"} {
		got := derivedName(l55, suffix)
		// (1) <= 63.
		if len(got) > 63 {
			t.Fatalf("derived name %q (suffix %q) exceeds 63: %d", got, suffix, len(got))
		}
		// (2) deterministic.
		if again := derivedName(l55, suffix); again != got {
			t.Fatalf("derived name not deterministic for suffix %q: %q vs %q", suffix, got, again)
		}
		// (3) distinct from the full (too-long) name — it was truncated.
		if got == l55+suffix {
			t.Fatalf("derived name %q must not equal the too-long full name", got)
		}
	}

	// Distinct loop names produce distinct truncated names (the hash keeps
	// them unique even after truncation).
	l55c := "c" + l55[1:]
	if a, b2 := derivedName(l55, "-egress-proxy"), derivedName(l55c, "-egress-proxy"); a == b2 {
		t.Fatalf("distinct loop names must produce distinct truncated names (both %q)", a)
	}
	_ = gomega.NewWithT(t)
}
