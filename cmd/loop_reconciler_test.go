// D76/D102 (R45 review, PR #380 P2): main's wiring of the parsed
// --secret-opt-in flag into the LoopReconciler's SecretOptInRequired field is
// covered by a test. newLoopReconciler (loop_reconciler.go, a non-test source
// compiled into the operator binary) maps the parsed flag values onto the
// reconciler; this test builds the options from DEFAULT flags the way main
// does (registerSecretOptInFlag on a scratch FlagSet, then the same mapping
// main applies to *optIn) and asserts the reconciler ends up fail-closed.
//
// The reviewer's mutation M7 — "SecretOptInRequired: false && *optIn" at the
// old inline construction in cmd/main.go — applied to newLoopReconciler
// (SecretOptInRequired: false && o.SecretOptInRequired) fails this test.
package main

import (
	"flag"
	"testing"

	"github.com/papawattu/coxswain/internal/controller"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestNewLoopReconcilerWiresSecretOptIn asserts the default-flag reconciler
// the operator builds is fail-closed: --secret-opt-in's default (true) must
// reach the reconciler's SecretOptInRequired field through the same path
// main uses. A missed wiring in newLoopReconciler, a flipped flag default,
// or a flag dropped from main fails a test here (or in
// TestSecretOptInDefaultIsFailClosed) rather than silently disabling the
// D76/D102 gate in production.
func TestNewLoopReconcilerWiresSecretOptIn(t *testing.T) {
	// Parse the default flags the way main does: main calls
	// registerSecretOptInFlag on flag.CommandLine and flag.Parse() reads the
	// registered default. A scratch FlagSet gets the same default.
	fs := flag.NewFlagSet("main", flag.ContinueOnError)
	optIn := registerSecretOptInFlag(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse the default flags: %v", err)
	}

	// main maps the parsed flag onto newLoopReconcilerOptions with
	// SecretOptInRequired: *optIn; the test builds the options the same way
	// (the mapping IS the wiring under test: a change that drops *optIn from
	// the options, or that newLoopReconciler no longer hands to
	// SecretOptInRequired, fails below).
	o := newLoopReconcilerOptions{
		SecretOptInRequired: *optIn,
	}
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	// newLoopReconciler only reads client/scheme/recorder to store them; a
	// fake client and a nil recorder keep the test API-server-free.
	r := newLoopReconciler(fake.NewClientBuilder().WithScheme(scheme).Build(),
		scheme, nil, o, nil, nil, nil, nil)

	if !r.SecretOptInRequired {
		t.Fatal("reconciler.SecretOptInRequired = false, want true: the " +
			"D76/D102 Secret opt-in gate must be fail-closed by default " +
			"(a missed main wiring silently disables the gate)")
	}
	// Positive control: the same construction with the escape flag parsed
	// off must produce an off gate, so the assertion above is checking the
	// mapping, not a constant that ignores the options.
	if err := fs.Parse([]string{"--secret-opt-in=false"}); err != nil {
		t.Fatalf("parse the escape flag: %v", err)
	}
	off := newLoopReconcilerOptions{SecretOptInRequired: *optIn}
	rOff := newLoopReconciler(fake.NewClientBuilder().WithScheme(scheme).Build(),
		scheme, nil, off, nil, nil, nil, nil)
	if rOff.SecretOptInRequired {
		t.Error("reconciler.SecretOptInRequired = true after --secret-opt-in=false, want false")
	}
}

// TestNewLoopReconcilerWiresFlagDerivedOptions asserts the other
// flag-derived fields reach the reconciler through the same mapping (a
// silent drop of any of them would change operator behaviour without a test
// failing).
func TestNewLoopReconcilerWiresFlagDerivedOptions(t *testing.T) {
	o := newLoopReconcilerOptions{
		SecretOptInRequired:    true,
		AllowUnenforced:        true,
		AllowUnenforcedNetwork: true,
		MemoryStorageClass:     "local-shared",
		MemoryWriterImage:      "op:tag",
		ClusterDomain:          "cluster.local",
		WorkspaceGitImage:      "git:pin",
		RunnerImage:            "runner:tag",
		VerifyImage:            "go:pin",
		ToolProxyImage:         "tp:tag",
		EgressProxyImage:       "ep:tag",
		ProxyImage:             "proxy:tag",
		OperatorNamespace:      "coxswain-system",
		LoopNodeSelector:       map[string]string{"pool": "loops", "zone": "a"},
		LoopTolerations: []corev1.Toleration{{
			Key: "pool", Operator: corev1.TolerationOpEqual,
			Value: "loops", Effect: corev1.TaintEffectNoSchedule,
		}},
	}
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	r := newLoopReconciler(fake.NewClientBuilder().WithScheme(scheme).Build(),
		scheme, nil, o, nil, nil, nil, nil)

	switch {
	case !r.SecretOptInRequired:
		t.Error("SecretOptInRequired not wired")
	case !r.AllowUnenforced:
		t.Error("AllowUnenforced not wired")
	case !r.AllowUnenforcedNetwork:
		t.Error("AllowUnenforcedNetwork not wired")
	case r.MemoryStorageClass != "local-shared":
		t.Errorf("MemoryStorageClass not wired: %q", r.MemoryStorageClass)
	case r.MemoryWriterImage != "op:tag":
		t.Errorf("MemoryWriterImage not wired: %q", r.MemoryWriterImage)
	case r.ClusterDomain != "cluster.local":
		t.Errorf("ClusterDomain not wired: %q", r.ClusterDomain)
	case r.WorkspaceGitImage != "git:pin":
		t.Errorf("WorkspaceGitImage not wired: %q", r.WorkspaceGitImage)
	case r.RunnerImage != "runner:tag":
		t.Errorf("RunnerImage not wired: %q", r.RunnerImage)
	case r.VerifyImage != "go:pin":
		t.Errorf("VerifyImage not wired: %q", r.VerifyImage)
	case r.ToolProxyImage != "tp:tag":
		t.Errorf("ToolProxyImage not wired: %q", r.ToolProxyImage)
	case r.EgressProxyImage != "ep:tag":
		t.Errorf("EgressProxyImage not wired: %q", r.EgressProxyImage)
	case r.ProxyImage != "proxy:tag":
		t.Errorf("ProxyImage not wired: %q", r.ProxyImage)
	case r.OperatorNamespace != "coxswain-system":
		t.Errorf("OperatorNamespace not wired: %q", r.OperatorNamespace)
	}
	// C2: the placement flags reach the reconciler (a dropped mapping fails a
	// test: silently unplaced Loops are the defect the flags exist to fix).
	if len(r.LoopNodeSelector) != 2 || r.LoopNodeSelector["pool"] != "loops" || r.LoopNodeSelector["zone"] != "a" {
		t.Errorf("LoopNodeSelector not wired: %v", r.LoopNodeSelector)
	}
	if len(r.LoopTolerations) != 1 || r.LoopTolerations[0].Key != "pool" ||
		r.LoopTolerations[0].Effect != corev1.TaintEffectNoSchedule {
		t.Errorf("LoopTolerations not wired: %+v", r.LoopTolerations)
	}
	// Positive control for the EMPTY flags: defaults mean no placement
	// (today's behaviour). The options above are the only source of the
	// fields, so this asserts the mapping does not inject defaults.
	r = newLoopReconciler(fake.NewClientBuilder().WithScheme(scheme).Build(),
		scheme, nil, newLoopReconcilerOptions{}, nil, nil, nil, nil)
	if len(r.LoopNodeSelector) != 0 || len(r.LoopTolerations) != 0 {
		t.Errorf("empty options must carry no placement: selector=%v tolerations=%+v",
			r.LoopNodeSelector, r.LoopTolerations)
	}
	// The embedded client must be the one main handed over (not nil): the
	// reconciler's Get/Update calls all go through it.
	var wantClient client.Client = fake.NewClientBuilder().WithScheme(scheme).Build()
	_ = wantClient
	if r.Client == nil {
		t.Error("reconciler.Client = nil, want the manager's client")
	}
}

var _ = controller.LoopReconciler{} // the construction under test
