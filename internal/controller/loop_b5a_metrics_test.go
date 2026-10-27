// B5a: unit tests for the Prometheus metric emission points. These pin the
// contract that the counters/gauges ARE registered on the process-wide
// controller-runtime metrics registry (the one the manager serves at
// /metrics), and that a metric bump at a real call site is observable.
// Mutation: drop a registration must fail (the metric is missing from the
// registry). The bump-site tests drive the REAL production call sites
// (recordPhaseClaim, applyStallGate, applyUsageReading, bumpVerifyResult,
// bumpLoopCompletion, noteUnmetered) and assert a delta.
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

// readMetricLabel reads the value of a named metric for a specific label
// combination. Returns (value, found).
func readMetricLabel(t *testing.T, name string, labels map[string]string) (float64, bool) {
	t.Helper()
	mfs, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			match := true
			for _, lp := range m.GetLabel() {
				if v, ok := labels[lp.GetName()]; ok && v != lp.GetValue() {
					match = false
					break
				}
			}
			if !match {
				continue
			}
			// Check that all requested labels are present.
			allPresent := true
			for _, lp := range m.GetLabel() {
				if _, ok := labels[lp.GetName()]; !ok {
					allPresent = false
					break
				}
			}
			if !allPresent {
				continue
			}
			if c := m.GetCounter(); c != nil {
				return c.GetValue(), true
			}
			if g := m.GetGauge(); g != nil {
				return g.GetValue(), true
			}
			return 0, true
		}
	}
	return 0, false
}

// TestMetricsRegistered: all B5a metrics are registered on the process-wide
// registry (the one the manager serves at /metrics). A CounterVec/GaugeVec
// only appears in the registry's Gather() output once it has at least one
// label sample, so this test bumps each metric once with a test-only label
// value to materialise it, then reads it back. Mutation: drop a registration
// must fail (the metric is missing from the registry even after a bump).
func TestMetricsRegistered(t *testing.T) {
	// Materialise each metric (a labelled metric is absent from Gather() until
	// it has a label sample; the manager's /metrics endpoint serves the same
	// registry, so this matches what the endpoint exposes).
	loopPhaseTransitionsTotal.WithLabelValues("__test__").Inc()
	loopIterationsTotal.Inc()
	verifyResultsTotal.WithLabelValues("__test__").Inc()
	loopsCompletedTotal.WithLabelValues("__test__", "__test__").Inc()
	loopStallsTotal.Inc()
	loopTokensTotal.WithLabelValues("__test__").Inc()
	loopUnmeteredRequestsTotal.Inc()
	memoryWritesTotal.WithLabelValues("__test__").Inc()
	memoryLessonsTotal.WithLabelValues("__test__").Inc()
	// B5a: loops_active is now a custom Collector (loops_active_collector.go),
	// registered in SetupWithManager. It is not tested here (it requires a
	// client with Loops in the cache). The envtest covers it.
	for _, name := range []string{
		"coxswain_loop_phase_transitions_total",
		"coxswain_loop_iterations_total",
		"coxswain_verify_results_total",
		"coxswain_loops_completed_total",
		"coxswain_loop_stalls_total",
		"coxswain_loop_tokens_total",
		"coxswain_unmetered_requests_total",
		"coxswain_memory_writes_total",
		"coxswain_memory_lessons_total",
	} {
		if _, ok := readMetric(t, name); !ok {
			t.Errorf("%s is not registered on the process-wide metrics registry", name)
		}
	}
}

// TestMetricsPhaseTransitionBumpedOnNewPhase: recordPhaseClaim bumps
// coxswain_loop_phase_transitions_total when the claim names a phase different
// from the Loop's current phase (a new-phase transition). A re-claim of the
// same phase does not bump. Mutation: drop the bump in recordPhaseClaim must
// fail (the counter stays at its baseline).
func TestMetricsPhaseTransitionBumpedOnNewPhase(t *testing.T) {
	// Reset the metric to a known baseline.
	loopPhaseTransitionsTotal.Reset()
	_, _ = readMetric(t, "coxswain_loop_phase_transitions_total") // re-materialise

	r := &LoopReconciler{CNIProber: testCNIProber(), Recorder: record.NewFakeRecorder(64)}
	now := metav1.Now()

	// A Loop currently in Planning. A claim reporting it just executed
	// Implementing (a new phase) should bump the metric once.
	l := &coxv1alpha1.Loop{}
	l.Name = "lp"
	l.Namespace = "ns"
	l.Status.Phase = coxv1alpha1.LoopPhasePlanning
	claim := &PhaseClaim{ObservedPhase: coxv1alpha1.LoopPhaseImplementing, Status: "success", Iteration: 1}
	_, _ = r.recordPhaseClaim(context.Background(), l, claim, now)

	got, _ := readMetric(t, "coxswain_loop_phase_transitions_total")
	if got != 1 {
		t.Fatalf("coxswain_loop_phase_transitions_total = %v after a new-phase transition (Planning -> Implementing), want 1", got)
	}

	// A re-claim of the SAME phase (Implementing again) does not bump.
	l.Status.Phase = coxv1alpha1.LoopPhaseImplementing
	claim2 := &PhaseClaim{ObservedPhase: coxv1alpha1.LoopPhaseImplementing, Status: "success", Iteration: 2}
	_, _ = r.recordPhaseClaim(context.Background(), l, claim2, now)

	got2, _ := readMetric(t, "coxswain_loop_phase_transitions_total")
	if got2 != 1 {
		t.Fatalf("coxswain_loop_phase_transitions_total = %v after a re-claim of the same phase, want 1 (no bump)", got2)
	}
}

// TestMetricsStallFires: applyStallGate firing (a new Job at k=N) bumps
// coxswain_loop_stalls_total exactly once. A non-fire (k < N) bumps nothing.
// Also checks that the terminal stall-fire transition bumps
// coxswain_loops_completed_total{result=failed,reason=stalled}.
// Mutation: drop the bump must fail.
func TestMetricsStallFires(t *testing.T) {
	before, ok := readMetric(t, "coxswain_loop_stalls_total")
	if !ok {
		t.Fatalf("coxswain_loop_stalls_total is not registered")
	}
	// Reset the completion metric to a known baseline.
	loopsCompletedTotal.Reset()
	_, _ = readMetricLabel(t, "coxswain_loops_completed_total",
		map[string]string{"result": "failed", "reason": "stalled"}) // re-materialise

	_, h := normalizeCheckOutput(p2eStallSeamOutput)
	r := &LoopReconciler{CNIProber: testCNIProber(), readCheckOutput: func(*corev1.Pod, string) (string, bool) { return p2eStallSeamOutput, true }}
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
	// Check the loopsCompletedTotal bump for the stall-fire terminal
	// transition.
	completed, found := readMetricLabel(t, "coxswain_loops_completed_total",
		map[string]string{"result": "failed", "reason": "stalled"})
	if !found {
		t.Fatalf("coxswain_loops_completed_total{result=failed,reason=stalled} not found after a stall fire")
	}
	if completed != 1 {
		t.Fatalf("coxswain_loops_completed_total{result=failed,reason=stalled} = %v after a stall fire, want 1", completed)
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

	r := &LoopReconciler{CNIProber: testCNIProber(), Recorder: record.NewFakeRecorder(64)}
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

// TestMetricsVerifyResults: bumpVerifyResult bumps coxswain_verify_results_total
// for each outcome. Mutation: drop the bump must fail.
func TestMetricsVerifyResults(t *testing.T) {
	// Reset the metric to a known baseline.
	verifyResultsTotal.Reset()

	// Test each outcome.
	bumpVerifyResult(verifySucceeded)
	passCount, found := readMetricLabel(t, "coxswain_verify_results_total", map[string]string{"result": "pass"})
	if !found || passCount != 1 {
		t.Fatalf("coxswain_verify_results_total{result=pass} = %v (found=%v) after bumpVerifyResult(verifySucceeded), want 1", passCount, found)
	}

	bumpVerifyResult(verifyIterate)
	failCount, found := readMetricLabel(t, "coxswain_verify_results_total", map[string]string{"result": "fail"})
	if !found || failCount != 1 {
		t.Fatalf("coxswain_verify_results_total{result=fail} = %v (found=%v) after bumpVerifyResult(verifyIterate), want 1", failCount, found)
	}

	bumpVerifyResult(verifyInfraFailed)
	infraCount, found := readMetricLabel(t, "coxswain_verify_results_total", map[string]string{"result": "infra"})
	if !found || infraCount != 1 {
		t.Fatalf("coxswain_verify_results_total{result=infra} = %v (found=%v) after bumpVerifyResult(verifyInfraFailed), want 1", infraCount, found)
	}
}

// TestMetricsLoopsCompleted: bumpLoopCompletion bumps coxswain_loops_completed_total.
// Mutation: drop the bump must fail.
func TestMetricsLoopsCompleted(t *testing.T) {
	// Reset the metrics to a known baseline.
	loopsCompletedTotal.Reset()

	// Test a succeeded completion.
	bumpLoopCompletion("succeeded", "verify_passed")
	succCount, found := readMetricLabel(t, "coxswain_loops_completed_total",
		map[string]string{"result": "succeeded", "reason": "verify_passed"})
	if !found || succCount != 1 {
		t.Fatalf("coxswain_loops_completed_total{result=succeeded,reason=verify_passed} = %v (found=%v), want 1", succCount, found)
	}

	// Test a failed completion.
	bumpLoopCompletion("failed", "stalled")
	failCount, found := readMetricLabel(t, "coxswain_loops_completed_total",
		map[string]string{"result": "failed", "reason": "stalled"})
	if !found || failCount != 1 {
		t.Fatalf("coxswain_loops_completed_total{result=failed,reason=stalled} = %v (found=%v), want 1", failCount, found)
	}
}

// TestMetricsIterationBumped: bumpIteration bumps coxswain_loop_iterations_total.
// Mutation: drop the bump must fail.
func TestMetricsIterationBumped(t *testing.T) {
	// Read the baseline (a plain Counter has no Reset; we read the current
	// value and assert the delta).
	before, ok := readMetric(t, "coxswain_loop_iterations_total")
	if !ok {
		t.Fatalf("coxswain_loop_iterations_total is not registered")
	}
	bumpIteration()
	after, _ := readMetric(t, "coxswain_loop_iterations_total")
	if after != before+1 {
		t.Fatalf("coxswain_loop_iterations_total = %v after bumpIteration(), want %v (before+1)", after, before+1)
	}
}

// TestMetricsUnmeteredTotal: noteUnmetered bumps coxswain_unmetered_requests_total
// by the delta it folds into status.budget (F82: the bump at the noteUnmetered
// call site was the one line of the path with no spec).
// Mutation: delete `loopUnmeteredRequestsTotal.Add(...)` in noteUnmetered.
func TestMetricsUnmeteredTotal(t *testing.T) {
	before, ok := readMetric(t, "coxswain_unmetered_requests_total")
	if !ok {
		t.Fatalf("coxswain_unmetered_requests_total is not registered")
	}
	// noteUnmetered reads and writes status.budget: the Loop needs a Budget.
	loop := &coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: "unmetered-metric", Namespace: "default"}}
	loop.Status.Budget = &coxv1alpha1.BudgetStatus{}
	r := &LoopReconciler{}
	r.noteUnmetered(loop, 2)
	after, _ := readMetric(t, "coxswain_unmetered_requests_total")
	if after != before+2 {
		t.Fatalf("coxswain_unmetered_requests_total = %v after noteUnmetered(2), want %v (before+2)", after, before+2)
	}
	// A zero delta (a clean same-boot read) leaves it alone. This is a sanity
	// check, not evidence for the `delta <= 0` guard: adding 0 would be a no-op
	// even if the Add had moved above the guard (early-341 P3-3).
	r.noteUnmetered(loop, 0)
	if got, _ := readMetric(t, "coxswain_unmetered_requests_total"); got != before+2 {
		t.Fatalf("coxswain_unmetered_requests_total = %v after noteUnmetered(0), want %v (unchanged)", got, before+2)
	}
}
