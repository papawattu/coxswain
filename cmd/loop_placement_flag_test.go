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

	corev1 "k8s.io/api/core/v1"
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
		"justakey",  // no =
		"=v",        // empty key
		"k=",        // empty value
		" =v",       // whitespace-collapsing empty key
		"k=v no=op", // whitespace inside the token, not a separator
		" a=v, b2",  // the bad token is the second one
		"k=v=extra", // too many =
		"k1=v1,,k2", // a malformed entry, not a blank one
	}
	for _, in := range bad {
		_, err := parseLoopNodeSelector(in)
		if err == nil {
			t.Fatalf("parseLoopNodeSelector(%q) = nil error, want an error naming the bad token", in)
		}
		// The error must name the offending token verbatim so the operator
		// can fix it without guessing which entry broke the flag.
		found := false
		for candidate := range strings.SplitSeq(in, ",") {
			if strings.Contains(err.Error(), strings.TrimSpace(candidate)) && strings.TrimSpace(candidate) != "" {
				found = true
			}
		}
		if !found {
			t.Fatalf("parseLoopNodeSelector(%q) error = %q, want the bad token named", in, err.Error())
		}
	}
}

// errNamesToken reports whether err names one of the trimmed non-blank
// comma-separated tokens of in verbatim (the spec: a malformed flag value
// surfaces at startup with the bad token named, never silently dropped).
func errNamesToken(t *testing.T, in string, err error) bool {
	t.Helper()
	for candidate := range strings.SplitSeq(in, ",") {
		cand := strings.TrimSpace(candidate)
		if cand != "" && strings.Contains(err.Error(), cand) {
			return true
		}
	}
	return false
}

// tolerationMatches reports whether two tolerations are field-for-field
// equal (TolerationSeconds is dereferenced, since struct == compares the
// pointer's address, not its pointee).
func tolerationMatches(got, want corev1.Toleration) bool {
	if got.Key != want.Key || got.Operator != want.Operator ||
		got.Value != want.Value || got.Effect != want.Effect {
		return false
	}
	if (got.TolerationSeconds == nil) != (want.TolerationSeconds == nil) {
		return false
	}
	return got.TolerationSeconds == nil || *got.TolerationSeconds == *want.TolerationSeconds
}

// assertToleration matches every field of one parsed toleration (a failure
// message names the actual value so a changed parser is easy to spot).
func assertToleration(t *testing.T, in string, want corev1.Toleration) {
	t.Helper()
	tols, err := parseLoopTolerations(in)
	if err != nil {
		t.Fatalf("parse %q: %v", in, err)
	}
	if len(tols) != 1 {
		t.Fatalf("parse %q = %d tolerations, want 1", in, len(tols))
	}
	if !tolerationMatches(tols[0], want) {
		t.Fatalf("parse %q = %+v, want %+v", in, tols[0], want)
	}
}

// TestParseLoopTolerationsGood pins the valid shapes of the approved plan's
// format (PLAN-PHASE6.md:101-102): key:op:effect[:value[:seconds]], op
// always field 2.
func TestParseLoopTolerationsGood(t *testing.T) {
	// Empty flag = no tolerations, not an error.
	tols, err := parseLoopTolerations("")
	if err != nil {
		t.Fatalf("empty flag = no tolerations, got error: %v", err)
	}
	if len(tols) != 0 {
		t.Fatalf("empty flag = %v tolerations, want none", tols)
	}

	// 3 fields: key:op:effect (no value, no seconds).
	assertToleration(t, "k:Exists:NoExecute", corev1.Toleration{
		Key: "k", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute,
	})

	// 4 fields: key:Equal:NoSchedule:loops (the reviewer's example).
	assertToleration(t, "coxswain.io/pool:Equal:NoSchedule:loops", corev1.Toleration{
		Key: "coxswain.io/pool", Operator: corev1.TolerationOpEqual,
		Value: "loops", Effect: corev1.TaintEffectNoSchedule,
	})

	// 5 fields with seconds.
	secs := int64(120)
	assertToleration(t, "k:Equal:NoSchedule:loops:120", corev1.Toleration{
		Key: "k", Operator: corev1.TolerationOpEqual, Value: "loops",
		Effect: corev1.TaintEffectNoSchedule, TolerationSeconds: &secs,
	})

	// Exists with seconds but NO value: the empty value slot keeps seconds in
	// field 5 (key:Exists:NoExecute::30).
	secs30 := int64(30)
	assertToleration(t, "k:Exists:NoExecute::30", corev1.Toleration{
		Key: "k", Operator: corev1.TolerationOpExists,
		Effect: corev1.TaintEffectNoExecute, TolerationSeconds: &secs30,
	})

	// PreferNoSchedule effect.
	assertToleration(t, "k:Equal:PreferNoSchedule:loops", corev1.Toleration{
		Key: "k", Operator: corev1.TolerationOpEqual, Value: "loops",
		Effect: corev1.TaintEffectPreferNoSchedule,
	})

	// Multiple tolerations, one of each operator.
	tols, err = parseLoopTolerations("a:Equal:NoSchedule:pool,b:Exists:NoExecute")
	if err != nil {
		t.Fatalf("parse two tolerations: %v", err)
	}
	wantTwo := []corev1.Toleration{
		{Key: "a", Operator: corev1.TolerationOpEqual, Value: "pool", Effect: corev1.TaintEffectNoSchedule},
		{Key: "b", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute},
	}
	if len(tols) != len(wantTwo) {
		t.Fatalf("two tolerations = %v, want %v", tols, wantTwo)
	}
	for i := range wantTwo {
		if !tolerationMatches(tols[i], wantTwo[i]) {
			t.Fatalf("two tolerations[%d] = %+v, want %+v", i, tols[i], wantTwo[i])
		}
	}
}

// TestParseLoopTolerationsMalformed pins the malformed-token contract: every
// bad shape is a startup error that names the offending token verbatim (a
// helper that silently dropped a bad token fails this test).
func TestParseLoopTolerationsMalformed(t *testing.T) {
	in := []string{
		"justkey",                                // no colons at all
		"k:NoSchedule",                           // two fields: op is missing
		":Equal:NoSchedule",                      // empty key
		"k:Equal:",                               // empty effect
		"k:Equal",                                // missing effect entirely
		"k:Equal:NoSchedule",                     // Equal with no value
		"k:Exists:NoExecute:v",                   // Exists with a value
		"k:NotEqual:NoSchedule:loops",            // NotEqual is not a k8s toleration operator
		"k:BogusOp:NoSchedule:loops",             // a non-Equal/Exists operator
		"k:Equal:BogusEffect:loops",              // a bad taint effect
		"k:Equal:NoSchedule:loops:x",             // seconds that is not an int
		"k:Equal:NoSchedule:loops:-1",            // negative seconds
		"k:Exists:NoExecute:30",                  // seconds in the VALUE slot (value with Exists)
		"k:Equal:NoSchedule:loops:1:2",           // six fields
		"a:Equal:NoSchedule:pool, b2:NoSchedule", // the second token is bad
	}
	for _, in := range in {
		_, err := parseLoopTolerations(in)
		if err == nil {
			t.Fatalf("parseLoopTolerations(%q) = nil error, want an error naming the bad token", in)
		}
		if !errNamesToken(t, in, err) {
			t.Fatalf("parseLoopTolerations(%q) error = %q, want the bad token named", in, err.Error())
		}
	}
}
