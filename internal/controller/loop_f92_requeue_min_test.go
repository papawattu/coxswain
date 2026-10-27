// pi3 prereview-308 finding 2 (and finding 5, the same rule): the F92
// CRD-missing window used to take the whole trailing requeue — it returned
// BEFORE the earlier-of comparison the memory window introduced, justified by a
// comment claiming it "is the shortest by construction". It is not: a token- or
// cost-only budget returns 30s (F52 C6), and a nearly-elapsed wall clock can
// return less than a minute, so a held Loop that also had a budget window waited
// on the longer timer. The three windows now take part in the same comparison.
package controller

import (
	"testing"
	"time"
)

// TestReconcileRequeueAfterF92TakesTheEarlierWindow pins the comparison for
// every pairing: the F92 window alone, a sooner budget window, a later budget
// window, a sooner memory window, a later memory window, and the no-flag
// controls.
func TestReconcileRequeueAfterF92TakesTheEarlierWindow(t *testing.T) {
	r := &LoopReconciler{CNIProber: testCNIProber()}
	cases := []struct {
		name           string
		budget, memory time.Duration
		kaptCRDMissing bool
		want           time.Duration
	}{
		{"the F92 window alone", 0, 0, true, kaptCRDMissingRequeue},
		{"a sooner budget window beats the F92 window", 30 * time.Second, 0, true, 30 * time.Second},
		{"a later budget window loses to the F92 window", 10 * time.Minute, 0, true, kaptCRDMissingRequeue},
		{"a sooner memory window beats both", 30 * time.Second, 10 * time.Second, true, 10 * time.Second},
		{"a later memory window loses to the F92 window", 0, 4 * time.Minute, true, kaptCRDMissingRequeue},
		{"no F92 flag keeps the budget window", 30 * time.Second, 0, false, 30 * time.Second},
		{"nothing to wait for", 0, 0, false, 0},
	}
	for _, tc := range cases {
		got := r.reconcileRequeueAfter(true, false, false, false, false, tc.budget, tc.memory, tc.kaptCRDMissing)
		if got.RequeueAfter != tc.want {
			t.Errorf("%s: RequeueAfter = %v, want %v", tc.name, got.RequeueAfter, tc.want)
		}
	}
	// The 5s work requeue still wins over every window: pending work is more
	// urgent than a reminder.
	if got := r.reconcileRequeueAfter(false, true, false, false, false, 30*time.Second, 0, true); got.RequeueAfter != 5*time.Second {
		t.Errorf("pending work must keep the 5s requeue, got %v", got.RequeueAfter)
	}
}
