// B5a: unit tests for the Prometheus metric emission points. These pin the
// contract that the counters/gauges ARE registered on the process-wide
// controller-runtime metrics registry (the one the manager serves at
// /metrics), and that a metric bump at a real call site is observable.
// Mutation: drop a registration must fail (the metric is missing from the
// registry). The bump-site tests drive the REAL production call sites
// (applyStallGate, applyUsageReading) and assert a delta.
package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/proxy"
)

// readMetric reads the value of a named metric from the process-wide
// controller-runtime metrics registry (the one the manager serves at
// /metrics). For CounterVec / GaugeVec metrics, the sum is taken across ALL
// label samples (so a labelled counter is read as its total across all
// labels). Returns (value, found).
func readMetric(t *testing.T, name string) (float64, bool) {
	t.Helper()
	mfs, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		var sum float64
		for _, m := range mf.GetMetric() {
			if c := m.GetCounter(); c != nil {
				sum += c.GetValue()
			}
			if g := m.GetGauge(); g != nil {
				sum += g.GetValue()
			}
		}
		return sum, true
	}
	return 0, false
}

// TestMetricsRegistered: all four B5a metrics are registered on the
// process-wide registry (the one the manager serves at /metrics). A
// CounterVec/GaugeVec only appears in the registry's Gather() output once it
// has at least one label sample, so this test bumps each metric once with a
// test-only label value to materialise it, then reads it back. Mutation: drop
// a registration must fail (the metric is missing from the registry even
// after a bump).
func TestMetricsRegistered(t *testing.T) {
	// Materialise each metric (a labelled metric is absent from Gather() until
	// it has a label sample; the manager's /metrics endpoint serves the same
	// registry, so this matches what the endpoint exposes).
	loopIterationsTotal.WithLabelValues("__test__").Inc()
	loopStallsTotal.Inc()
	loopTokensTotal.WithLabelValues("__test__").Inc()
	loopsActive.WithLabelValues("__test__").Set(1)
	for _, name := range []string{
		"coxswain_loop_iterations_total",
		"coxswain_loop_stalls_total",
		"coxswain_loop_tokens_total",
		"coxswain_loops_active",
	} {
		if _, ok := readMetric(t, name); !ok {
			t.Errorf("%s is not registered on the process-wide metrics registry", name)
		}
	}
}

// TestMetricsStallFires: applyStallGate firing (a new Job at k=N) bumps
// coxswain_loop_stalls_total exactly once. A non-fire (k < N) bumps nothing.
// Mutation: drop the bump must fail.
func TestMetricsStallFires(t *testing.T) {
	before, ok := readMetric(t, "coxswain_loop_stalls_total")
	if !ok {
		t.Fatalf("coxswain_loop_stalls_total is not registered")
	}
	_, h := normalizeCheckOutput(p2eStallSeamOutput)
	r := &LoopReconciler{readCheckOutput: func(*corev1.Pod, string) (string, bool) { return p2eStallSeamOutput, true }}
	terminatedState := corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		ExitCode:   1,
		FinishedAt: metav1.Now(),
	}}
	pod := &corev1.Pod{
		Status: corev1.PodStatus{
			InitContainerStatuses: []corev1.ContainerStatus{
				{Name: s5aCheck0, State: terminatedState,
					LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
						ExitCode:   1,
						Message:    "out",
						FinishedAt: metav1.Now(),
					}}},
			},
		},
	}
	// Fire case: a new Job at k=N.
	n := int32(3)
	loop := &coxv1alpha1.Loop{}
	loop.Name = "lp"
	loop.Namespace = p2eStallNS
	loop.Status.Phase = coxv1alpha1.LoopPhaseVerifying
	loop.Status.Iteration = 2
	loop.Spec.Loop.StallAfter = &n
	loop.Spec.Loop.StallAction = coxv1alpha1.StallActionFail
	loop.Status.StallHistory = append(loop.Status.StallHistory,
		coxv1alpha1.StallEntry{JobName: p2eStallSeededJob0, Hash: h, NormalisationVersion: "v1"},
		coxv1alpha1.StallEntry{JobName: p2eStallSeededJob1, Hash: h, NormalisationVersion: "v1"})
	if !r.applyStallGate(context.Background(), loop, pod, s5aCheck0) {
		t.Fatalf("applyStallGate: a new Job at k=N must fire")
	}
	after, _ := readMetric(t, "coxswain_loop_stalls_total")
	if after != before+1 {
		t.Fatalf("coxswain_loop_stalls_total = %v after a fire, want %v (before+1)", after, before+1)
	}
	// Non-fire case: k < N.
	n2 := int32(3)
	loop2 := &coxv1alpha1.Loop{}
	loop2.Name = "lp2"
	loop2.Namespace = p2eStallNS
	loop2.Status.Phase = coxv1alpha1.LoopPhaseVerifying
	loop2.Status.Iteration = 1
	loop2.Spec.Loop.StallAfter = &n2
	loop2.Spec.Loop.StallAction = coxv1alpha1.StallActionFail
	loop2.Status.StallHistory = append(loop2.Status.StallHistory,
		coxv1alpha1.StallEntry{JobName: p2eStallSeededJob0, Hash: h, NormalisationVersion: "v1"})
	if r.applyStallGate(context.Background(), loop2, pod, s5aCheck0) {
		t.Fatalf("applyStallGate: k < N must NOT fire")
	}
	after2, _ := readMetric(t, "coxswain_loop_stalls_total")
	if after2 != before+1 {
		t.Fatalf("coxswain_loop_stalls_total = %v after a non-fire, want %v (unchanged)", after2, before+1)
	}
}

// TestMetricsTokenTotalBumpedOnFold: applyUsageReading bumps
// coxswain_loop_tokens_total (summed over the prompt/completion kinds) by the
// reading's delta when the reading is a same-bootID, no-drop advance (the
// normal accumulate case). A zero-delta reading (cumulative == last) bumps
// nothing. Mutation: drop the bump must fail.
func TestMetricsTokenTotalBumpedOnFold(t *testing.T) {
	// Reset the metric to a known baseline (other tests in the package may
	// have bumped it; Reset clears all label samples).
	loopTokensTotal.Reset()
	_, _ = readMetric(t, "coxswain_loop_tokens_total") // re-materialise after Reset

	r := &LoopReconciler{Recorder: record.NewFakeRecorder(64)}
	// A Loop with a budget status seeded with a prior reading (boot A,
	// cumulative prompt 5 / completion 4). A new reading on the SAME boot
	// with prompt 11 / completion 5 is a delta of (6, 1) — no drop.
	// ModelPrices is set on spec.budget so modelPricesForLoop does not need
	// a k8s client (it short-circuits on the spec override).
	l := &coxv1alpha1.Loop{}
	l.Name = "lp"
	l.Namespace = "ns"
	l.Spec.Budget = &coxv1alpha1.BudgetConfig{ModelPrices: &coxv1alpha1.ModelPrices{PromptUsdPerMtok: "0.5", CompletionUsdPerMtok: "1.5"}}
	l.Status.Budget = &coxv1alpha1.BudgetStatus{
		LastBootID:           "bootA",
		LastPromptTokens:     5,
		LastCompletionTokens: 4,
		PromptTokens:         5,
		CompletionTokens:     4,
	}
	reading := proxy.Reading{BootID: "bootA", PromptTokens: 11, CompletionTokens: 5, Requests: 1}
	r.applyUsageReading(context.Background(), l, reading)

	before, _ := readMetric(t, "coxswain_loop_tokens_total")
	if before != 7 {
		t.Fatalf("coxswain_loop_tokens_total = %v after a same-bootID delta of (6 prompt, 1 completion), want 7 (6+1)", before)
	}
	// A zero-delta reading (cumulative == last): bumps nothing.
	before2, _ := readMetric(t, "coxswain_loop_tokens_total")
	r.applyUsageReading(context.Background(), l, reading) // same reading again: cumulative == last -> delta 0
	after2, _ := readMetric(t, "coxswain_loop_tokens_total")
	if after2 != before2 {
		t.Fatalf("coxswain_loop_tokens_total bumped on a zero-delta reading (%v -> %v)", before2, after2)
	}
}
