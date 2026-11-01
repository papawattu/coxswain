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
	// The approved plan's format (PLAN-PHASE6.md:101-102):
	// key:op:effect[:value[:seconds]], op always field 2.
	if _, err := parseLoopTolerations(""); err != nil {
		t.Fatalf("empty flag = no tolerations, got error: %v", err)
	}

	// 3 fields: key:op:effect (no value, no seconds).
	tols, err := parseLoopTolerations("k:Exists:NoExecute")
	if err != nil {
		t.Fatalf("parse k:Exists:NoExecute: %v", err)
	}
	tol := tols[0]
	if tol.Key != "k" || tol.Operator != "Exists" || tol.Value != "" || tol.Effect != "NoExecute" || tol.TolerationSeconds != nil {
		t.Fatalf("k:Exists:NoExecute = %+v, want {k Exists NoExecute, no value/seconds}", tol)
	}

	// 4 fields: key:Equal:NoSchedule:loops (the reviewer's example).
	tols, err = parseLoopTolerations("coxswain.io/pool:Equal:NoSchedule:loops")
	if err != nil {
		t.Fatalf("parse coxswain.io/pool:Equal:NoSchedule:loops: %v", err)
	}
	tol = tols[0]
	if tol.Key != "coxswain.io/pool" || tol.Operator != "Equal" || tol.Value != "loops" || tol.Effect != "NoSchedule" || tol.TolerationSeconds != nil {
		t.Fatalf("4-field = %+v, want {pool Equal loops NoSchedule}", tol)
	}

	// 5 fields with seconds.
	tols, err = parseLoopTolerations("k:Equal:NoSchedule:loops:120")
	if err != nil {
		t.Fatalf("parse k:Equal:NoSchedule:loops:120: %v", err)
	}
	tol = tols[0]
	if tol.Value != "loops" || tol.TolerationSeconds == nil || *tol.TolerationSeconds != 120 {
		t.Fatalf("5-field = %+v, want seconds 120", tol)
	}

	// Exists with seconds but NO value: the empty value slot keeps seconds in
	// field 5 (key:Exists:NoExecute::30).
	tols, err = parseLoopTolerations("k:Exists:NoExecute::30")
	if err != nil {
		t.Fatalf("parse k:Exists:NoExecute::30: %v", err)
	}
	tol = tols[0]
	if tol.Operator != "Exists" || tol.Value != "" || tol.TolerationSeconds == nil || *tol.TolerationSeconds != 30 {
		t.Fatalf("Exists::30 = %+v, want Exists NoExecute seconds 30, empty value", tol)
	}

	// PreferNoSchedule effect.
	tols, err = parseLoopTolerations("k:Equal:PreferNoSchedule:loops")
	if err != nil {
		t.Fatalf("parse k:Equal:PreferNoSchedule:loops: %v", err)
	}
	tol = tols[0]
	if tol.Effect != "PreferNoSchedule" {
		t.Fatalf("PreferNoSchedule = %+v, want effect PreferNoSchedule", tol)
	}

	// Multiple tolerations, one of each operator.
	tols, err = parseLoopTolerations("a:Equal:NoSchedule:pool,b:Exists:NoExecute")
	if err != nil {
		t.Fatalf("parse two tolerations: %v", err)
	}
	if len(tols) != 2 || tols[0].Key != "a" || tols[1].Key != "b" || tols[1].Operator != "Exists" {
		t.Fatalf("two tolerations = %+v, want a then b (Exists)", tols)
	}

	bad := []string{
		"justkey",              // no colons at all
		"k:NoSchedule",         // two fields: op is missing
		":Equal:NoSchedule",    // empty key
		"k:Equal:",             // empty effect
		"k:Equal",              // missing effect entirely
		"k:Equal:NoSchedule",   // Equal with no value
		"k:Exists:NoExecute:v", // Exists with a value
		"k:NotEqual:NoSchedule:loops", // NotEqual is not a k8s toleration operator
		"k:BogusOp:NoSchedule:loops",  // a non-Equal/Exists operator
		"k:Equal:BogusEffect:loops",   // a bad taint effect
		"k:Equal:NoSchedule:loops:x",  // seconds that is not an int
		"k:Equal:NoSchedule:loops:-1", // negative seconds
		"k:Exists:NoExecute:30",       // seconds in the VALUE slot (value with Exists)
		"k:Equal:NoSchedule:loops:1:2", // six fields
		"a:Equal:NoSchedule:pool, b2:NoSchedule", // the second token is bad
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
