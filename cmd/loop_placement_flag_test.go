// C2 + D101 (Phase 6, PLAN-PHASE6.md C2): the operator's Loop node-placement
// flags (--loop-node-selector / --loop-tolerations). The parsing helpers live
// in a NON-test source (loop_placement_flag.go) so they compile into the
// operator binary; a malformed token is a STARTUP error that names the bad
// token — never a silent drop. A helper that silently dropped a malformed
// token would fail the malformed-input cases below; one that rejected a valid
// token would fail the good-input cases.
package main

import (
	"strings"
	"testing"
)

func TestParseLoopNodeSelector(t *testing.T) {
	good := []struct {
		in   string
		want map[string]string
	}{
		{"", map[string]string{}},
		{" ", map[string]string{}},
		{"k=v", map[string]string{"k": "v"}},
		{"k=v,k2=v2", map[string]string{"k": "v", "k2": "v2"}},
		{"k=v, k2=v2", map[string]string{"k": "v", "k2": "v2"}},
		// An empty entry between commas is a blank, not a malformed token
		// (a trailing comma must not kill the operator at startup).
		{"k=v,,k2=v2", map[string]string{"k": "v", "k2": "v2"}},
	}
	for _, tc := range good {
		got, err := parseLoopNodeSelector(tc.in)
		if err != nil {
			t.Fatalf("parseLoopNodeSelector(%q) error: %v", tc.in, err)
		}
		if len(got) != len(tc.want) {
			t.Fatalf("parseLoopNodeSelector(%q) = %v (len %d), want %v", tc.in, got, len(got), tc.want)
		}
		for k, v := range tc.want {
			if got[k] != v {
				t.Fatalf("parseLoopNodeSelector(%q) = %v, want %s=%q", tc.in, got, k, v)
			}
		}
	}

	// Every malformed shape is a startup error that NAMES the bad token
	// (the spec: a malformed flag value surfaces at startup with the bad
	// token named, never silently dropped).
	bad := []string{
		"justakey",        // no =
		"=v",              // empty key
		"k=",              // empty value
		" =v",             // whitespace-collapsing empty key
		"k=v no=op",       // whitespace inside the token, not a separator
		" a=v, b2",        // the bad token is the second one
		"k=v=extra",       // too many =
		"k1=v1,,k2",       // a malformed entry, not a blank one
		"k=v:colon",       // a colon is not a key/value separator
	}
	for _, in := range bad {
		_, err := parseLoopNodeSelector(in)
		if err == nil {
			t.Fatalf("parseLoopNodeSelector(%q) = nil error, want an error naming the bad token", in)
		}
		// The error must name the offending token verbatim so the operator
		// can fix it without guessing which entry broke the flag.
		found := false
		for _, candidate := range strings.Split(in, ",") {
			if strings.Contains(err.Error(), strings.TrimSpace(candidate)) && strings.TrimSpace(candidate) != "" {
				found = true
			}
		}
		if !found {
			t.Fatalf("parseLoopNodeSelector(%q) error = %q, want the bad token named", in, err.Error())
		}
	}
}

func TestParseLoopTolerations(t *testing.T) {
	if _, err := parseLoopTolerations(""); err != nil {
		t.Fatalf("empty flag = no tolerations, got error: %v", err)
	}
	tol, err := parseLoopTolerations("k:NoSchedule")
	if err != nil {
		t.Fatalf("parse k:NoSchedule: %v", err)
	}
	if tol.Key != "k" || tol.Operator != "Equal" || tol.Effect != "NoSchedule" {
		t.Fatalf("k:NoSchedule = %+v, want {k Equal NoSchedule} (op defaults to Equal)", tol)
	}

	tol, err = parseLoopTolerations("k:v:Exists")
	if err != nil {
		t.Fatalf("parse k:v:Exists: %v", err)
	}
	if tol.Key != "k" || tol.Value != "v" || tol.Operator != "Equal" || tol.Effect != "Exists" {
		t.Fatalf("k:v:Exists = %+v, want {k Equal v Exists}", tol)
	}

	tol, err = parseLoopTolerations("k:v:NotEqual:NoSchedule")
	if err != nil {
		t.Fatalf("parse k:v:NotEqual:NoSchedule: %v", err)
	}
	if tol.Operator != "NotEqual" || tol.Effect != "NoSchedule" {
		t.Fatalf("k:v:NotEqual:NoSchedule = %+v, want op NotEqual effect NoSchedule", tol)
	}

	// The seconds form: the trailing token is the tolerationSeconds value.
	tol, err = parseLoopTolerations("k:NoSchedule:300")
	if err != nil {
		t.Fatalf("parse k:NoSchedule:300: %v", err)
	}
	if tol.TolerationSeconds == nil || *tol.TolerationSeconds != 300 {
		t.Fatalf("k:NoSchedule:300 TolerationSeconds = %v, want 300", tol.TolerationSeconds)
	}

	// Multiple tolerations.
	tols, err := parseLoopTolerations("a:NoSchedule,b:Exists:120")
	if err != nil {
		t.Fatalf("parse two tolerations: %v", err)
	}
	if len(tols) != 2 || tols[0].Key != "a" || tols[1].Key != "b" {
		t.Fatalf("two tolerations = %+v, want a then b", tols)
	}

	bad := []string{
		"justkey",            // no colon at all
		":NoSchedule",        // empty key
		"k:",                 // empty effect
		"k:NoSchedule:bad",   // trailing seconds that is not an int
		"k:a:b:c:d",          // five fields: neither a value nor a seconds shape
		"k:NoSchedule:1:2",   // value+seconds AND extra fields
		"k:BogusOp:NoSchedule", // a non-default operator that is not Equal/Exists/NotEqual
		"k:NoSchedule:300 extra",
		"a:NoSchedule, b2:NoSchedule:xyz",
	}
	for _, in := range bad {
		_, err := parseLoopTolerations(in)
		if err == nil {
			t.Fatalf("parseLoopTolerations(%q) = nil error, want an error naming the bad token", in)
		}
		found := false
		for _, candidate := range strings.Split(in, ",") {
			if strings.Contains(err.Error(), strings.TrimSpace(candidate)) && strings.TrimSpace(candidate) != "" {
				found = true
			}
		}
		if !found {
			t.Fatalf("parseLoopTolerations(%q) error = %q, want the bad token named", in, err.Error())
		}
	}
}
