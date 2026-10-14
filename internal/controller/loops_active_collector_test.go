// loops_active_collector_test.go — B5a: unit test for the loops-active
// custom Collector. The collector counts Loops per non-terminal phase from
// the client at scrape time. This test creates a fake client with Loops in
// various phases and checks that the collector returns the correct counts.
//
// Mutation: remove the collector's List call (or the phase filter) must fail
// this test (the counts will be wrong or the metric will not be emitted).

package controller

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// TestLoopsActiveCollector: the collector counts Loops per non-terminal phase.
// It creates a fake client with Loops in Planning, Implementing, Verifying,
// Paused, Succeeded, and Failed phases, and checks that the collector emits
// the correct counts (Succeeded and Failed are terminal and excluded).
func TestLoopsActiveCollector(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := coxv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	// Create Loops in various phases.
	loops := []coxv1alpha1.Loop{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "loop-planning", Namespace: "ns"},
			Spec:       s5aLoopSpec(),
			Status:     coxv1alpha1.LoopStatus{Phase: coxv1alpha1.LoopPhasePlanning},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "loop-implementing", Namespace: "ns"},
			Spec:       s5aLoopSpec(),
			Status:     coxv1alpha1.LoopStatus{Phase: coxv1alpha1.LoopPhaseImplementing},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "loop-verifying-1", Namespace: "ns"},
			Spec:       s5aLoopSpec(),
			Status:     coxv1alpha1.LoopStatus{Phase: coxv1alpha1.LoopPhaseVerifying},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "loop-verifying-2", Namespace: "ns"},
			Spec:       s5aLoopSpec(),
			Status:     coxv1alpha1.LoopStatus{Phase: coxv1alpha1.LoopPhaseVerifying},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "loop-paused", Namespace: "ns"},
			Spec:       s5aLoopSpec(),
			Status:     coxv1alpha1.LoopStatus{Phase: coxv1alpha1.LoopPhasePaused},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "loop-succeeded", Namespace: "ns"},
			Spec:       s5aLoopSpec(),
			Status:     coxv1alpha1.LoopStatus{Phase: coxv1alpha1.LoopPhaseSucceeded},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "loop-failed", Namespace: "ns"},
			Spec:       s5aLoopSpec(),
			Status:     coxv1alpha1.LoopStatus{Phase: coxv1alpha1.LoopPhaseFailed},
		},
	}
	// Create a fake client with the Loops.
	objs := make([]client.Object, len(loops))
	for i := range loops {
		objs[i] = &loops[i]
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()

	// Create the collector.
	collector := NewLoopsActiveCollector(cl)

	// Register the collector on a test registry.
	reg := prometheus.NewRegistry()
	if err := reg.Register(collector); err != nil {
		t.Fatalf("register collector: %v", err)
	}

	// Gather the metrics.
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	// Find the coxswain_loops_active metric family.
	var activeMF *dto.MetricFamily
	for _, mf := range mfs {
		if mf.GetName() == "coxswain_loops_active" {
			activeMF = mf
			break
		}
	}
	if activeMF == nil {
		t.Fatal("coxswain_loops_active metric family not found")
	}

	// Build a map of phase -> count from the metric family.
	phaseCounts := make(map[string]float64)
	for _, m := range activeMF.GetMetric() {
		var phase string
		var value float64
		for _, lp := range m.GetLabel() {
			if lp.GetName() == "phase" {
				phase = lp.GetValue()
			}
		}
		if g := m.GetGauge(); g != nil {
			value = g.GetValue()
		}
		phaseCounts[phase] = value
	}

	// Check the expected counts.
	expected := map[string]float64{
		"Planning":    1,
		"Implementing": 1,
		"Verifying":    2,
		"Paused":       1,
		// Succeeded and Failed are terminal and should NOT be present.
	}
	for phase, want := range expected {
		got, ok := phaseCounts[phase]
		if !ok {
			t.Errorf("coxswain_loops_active{phase=%q} not found; want %v", phase, want)
			continue
		}
		if got != want {
			t.Errorf("coxswain_loops_active{phase=%q} = %v, want %v", phase, got, want)
		}
	}

	// Check that terminal phases are NOT present.
	for _, phase := range []string{"Succeeded", "Failed"} {
		if _, ok := phaseCounts[phase]; ok {
			t.Errorf("coxswain_loops_active{phase=%q} should not be present (terminal phase)", phase)
		}
	}

	// Also verify via testutil.CollectAndCount (should be 4 phases).
	count := testutil.CollectAndCount(collector)
	if count != 4 {
		t.Errorf("collector emitted %d metrics, want 4 (Planning, Implementing, Verifying, Paused)", count)
	}
}
