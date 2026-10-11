package cli

import (
	"reflect"
	"testing"
)

// TestLogArgs_Agent asserts the agent component targets the agent container in
// the sandbox pod (with the namespace/flag prefix).
func TestLogArgs_Agent(t *testing.T) {
	got := logArgs("samples", "loop1", compAgent, false, 5)
	want := []string{
		"--namespace", "samples",
		"logs",
		"--tail", "5",
		"pod/loop1-sandbox", "-c", compAgent,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("agent args mismatch\ngot  %v\nwant %v", got, want)
	}
}

// TestLogArgs_Proxy asserts the proxy component targets the standalone
// <loop>-proxy pod (not a container in the sandbox).
func TestLogArgs_Proxy(t *testing.T) {
	got := logArgs("samples", "loop1", compProxy, true, 0)
	// follow is set, tail is 0 (omitted).
	if !contains(got, "pod/loop1-proxy") {
		t.Errorf("proxy args should target pod/loop1-proxy, got %v", got)
	}
	if !contains(got, "-f") {
		t.Errorf("proxy args with follow should include -f, got %v", got)
	}
	if contains(got, "--tail") {
		t.Errorf("proxy args with tail=0 should omit --tail, got %v", got)
	}
}

// TestLogArgs_Verify asserts the verify component uses a single
// -l coxswain.io/verify-for=<loop> selector (the controller's pod label) and
// does NOT pass the component name as a separate argv element.
func TestLogArgs_Verify(t *testing.T) {
	got := logArgs("samples", "loop1", compVerify, false, 0)
	wantSel := verifyForLabel + "=loop1"
	found := false
	for i, a := range got {
		if a == "-l" && i+1 < len(got) && got[i+1] == wantSel {
			found = true
		}
	}
	if !found {
		t.Errorf("verify args should be -l %s, got %v", wantSel, got)
	}
	// The component name must NOT appear as a standalone argument.
	for _, a := range got {
		if a == "component=verify" || a == compVerify {
			t.Errorf("verify args must not pass the component name as a separate argument, got %v", got)
		}
	}
}

// TestLogArgs_Deliver asserts the deliver component uses a single
// -l coxswain.io/deliver-for=<loop> selector.
func TestLogArgs_Deliver(t *testing.T) {
	got := logArgs("samples", "loop1", compDeliver, false, 0)
	wantSel := deliverForLabel + "=loop1"
	found := false
	for i, a := range got {
		if a == "-l" && i+1 < len(got) && got[i+1] == wantSel {
			found = true
		}
	}
	if !found {
		t.Errorf("deliver args should be -l %s, got %v", wantSel, got)
	}
	for _, a := range got {
		if a == "component=deliver" || a == compDeliver {
			t.Errorf("deliver args must not pass the component name as a separate argument, got %v", got)
		}
	}
}

// TestLogArgs_Unknown asserts an unknown component yields no args (the streamer
// then returns an error).
func TestLogArgs_Unknown(t *testing.T) {
	if got := logArgs("samples", "loop1", "bogus", false, 0); len(got) != 0 {
		t.Errorf("unknown component should return nil args, got %v", got)
	}
}

// TestLogArgs_ContextFlag asserts --context is threaded through when set.
func TestLogArgs_ContextFlag(t *testing.T) {
	Context = "kind-coxswain-dev"
	defer func() { Context = "" }()
	got := logArgs("samples", "loop1", compAgent, false, 0)
	if !reflect.DeepEqual(got[:2], []string{"--context", "kind-coxswain-dev"}) {
		t.Errorf("args should start with --context <ctx>, got %v", got)
	}
}
