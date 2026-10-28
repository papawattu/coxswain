// F52 B1 + B2 (queue-b2-after-f30.md:66; design .samples/design-f52-f53.md
// §F52-B). Two P3s in the delivery/approval area:
//
//	B1  a FAILED delivery still counted as "in flight", so spec.suspend was
//	    refused for ever on a Succeeded Loop whose delivery had failed (the
//	    deliver Job's failure path never writes status.delivery — only the
//	    success path does).
//	B2  settleDeliveryHold DELETED the Verifying condition instead of setting
//	    it False, so the record that the Loop passed verify vanished.
//
// Both are plain Go tests on in-memory Loops (the P5f file's style): no
// cluster, no clock, no Job.
package controller

import (
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// f52DeliveryLoop is a Succeeded Loop with delivery enabled and a pinned
// verified commit — the shape the item-F suspension gate and the P5f hold both
// key on.
func f52DeliveryLoop() *coxv1alpha1.Loop {
	loop := &coxv1alpha1.Loop{}
	loop.Status.Phase = coxv1alpha1.LoopPhaseSucceeded
	loop.Spec.Workspace.Repo = "https://example.com/o/r.git"
	loop.Spec.Delivery = &coxv1alpha1.DeliveryConfig{Mode: coxv1alpha1.DeliveryModePullRequest}
	loop.Status.CurrentVerify = &coxv1alpha1.CurrentVerifyStatus{VerifiedCommit: "abc123"}
	return loop
}

// f52Delivered sets the Delivered condition the way the deliver Job's outcome
// paths do (a condition, never status.delivery, for anything but success).
func f52Delivered(loop *coxv1alpha1.Loop, status metav1.ConditionStatus, reason string) {
	setCondition(loop, coxv1alpha1.DeliveredCondition, status, reason, "test fixture")
}

// TestF52FailedDeliveryDoesNotRefuseSuspend is B1's detector: a delivery that
// FAILED is terminal, so it is not "in flight" and suspend is not refused.
func TestF52FailedDeliveryDoesNotRefuseSuspend(t *testing.T) {
	r := &LoopReconciler{}
	loop := f52DeliveryLoop()
	f52Delivered(loop, metav1.ConditionFalse, coxv1alpha1.ReasonDeliveryFailed)
	loop.Spec.Suspend = true

	blocked, changed := r.handleSuspendEntry(loop)
	if blocked || changed {
		t.Fatalf("a FAILED delivery must not count as in flight: blocked=%v changed=%v", blocked, changed)
	}
	if c := apimeta.FindStatusCondition(loop.Status.Conditions, coxv1alpha1.PausedCondition); c != nil &&
		c.Reason == suspendedRefusedCondReason {
		t.Fatalf("suspend was still refused after the delivery failed: reason=%q message=%q", c.Reason, c.Message)
	}
	if got := loop.Status.Phase; got != coxv1alpha1.LoopPhaseSucceeded {
		t.Fatalf("phase = %s, want Succeeded (a terminal phase is not paused)", got)
	}
}

// TestF52InFlightDeliveryStillRefusesSuspend is B1's control: while the
// delivery is NOT terminal, the refusal stands (the whole point of item F).
func TestF52InFlightDeliveryStillRefusesSuspend(t *testing.T) {
	for _, tc := range []struct {
		name      string
		delivered func(*coxv1alpha1.Loop)
	}{
		{"no Delivered condition yet", func(*coxv1alpha1.Loop) {}},
		{"Delivered=False/InProgress", func(l *coxv1alpha1.Loop) {
			f52Delivered(l, metav1.ConditionFalse, coxv1alpha1.ReasonDeliveryInProgress)
		}},
	} {
		r := &LoopReconciler{}
		loop := f52DeliveryLoop()
		tc.delivered(loop)
		loop.Spec.Suspend = true
		blocked, changed := r.handleSuspendEntry(loop)
		if !blocked || !changed {
			t.Fatalf("%s: blocked=%v changed=%v, want true/true (an in-flight delivery refuses suspend)", tc.name, blocked, changed)
		}
		c := apimeta.FindStatusCondition(loop.Status.Conditions, coxv1alpha1.PausedCondition)
		if c == nil || c.Reason != suspendedRefusedCondReason {
			t.Fatalf("%s: the refusal must be recorded as %s, got %+v", tc.name, suspendedRefusedCondReason, c)
		}
	}
}

// TestF52PausedLoopInDeliveryFlight pins the predicate itself: delivery
// enabled, on a delivery-active phase, and the delivery not terminal.
func TestF52PausedLoopInDeliveryFlight(t *testing.T) {
	for _, tc := range []struct {
		name string
		loop func() *coxv1alpha1.Loop
		want bool
	}{
		{"Succeeded, delivery PR, nothing recorded yet", func() *coxv1alpha1.Loop {
			return f52DeliveryLoop()
		}, true},
		{"Succeeded, delivery PR, Delivered=True", func() *coxv1alpha1.Loop {
			l := f52DeliveryLoop()
			f52Delivered(l, metav1.ConditionTrue, coxv1alpha1.ReasonDelivered)
			return l
		}, false},
		{"Succeeded, delivery PR, delivery FAILED", func() *coxv1alpha1.Loop {
			l := f52DeliveryLoop()
			f52Delivered(l, metav1.ConditionFalse, coxv1alpha1.ReasonDeliveryFailed)
			return l
		}, false},
		{"Succeeded, delivery PR, InProgress", func() *coxv1alpha1.Loop {
			l := f52DeliveryLoop()
			f52Delivered(l, metav1.ConditionFalse, coxv1alpha1.ReasonDeliveryInProgress)
			return l
		}, true},
		{"Succeeded, delivery recorded for the pinned commit", func() *coxv1alpha1.Loop {
			l := f52DeliveryLoop()
			l.Status.Delivery = &coxv1alpha1.DeliverStatus{Commit: "abc123"}
			return l
		}, false},
		{"Succeeded, delivery None", func() *coxv1alpha1.Loop {
			l := f52DeliveryLoop()
			l.Spec.Delivery = nil
			return l
		}, false},
		{"Implementing (no delivery phase), delivery PR", func() *coxv1alpha1.Loop {
			l := f52DeliveryLoop()
			l.Status.Phase = coxv1alpha1.LoopPhaseImplementing
			return l
		}, false},
	} {
		if got := pausedLoopInDeliveryFlight(tc.loop()); got != tc.want {
			t.Errorf("%s: pausedLoopInDeliveryFlight = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestF52SettleDeliveryHoldKeepsTheVerifyRecord is B2's detector: settling the
// hold sets the Verifying condition False (carrying the delivery outcome as the
// reason) instead of deleting it, and the hold really is released.
func TestF52SettleDeliveryHoldKeepsTheVerifyRecord(t *testing.T) {
	for _, tc := range []struct {
		name       string
		outcome    func(*coxv1alpha1.Loop)
		wantReason string
	}{
		{"delivered", func(l *coxv1alpha1.Loop) {
			f52Delivered(l, metav1.ConditionTrue, coxv1alpha1.ReasonDelivered)
		}, coxv1alpha1.ReasonDelivered},
		{"delivery failed (D64: still Succeeded)", func(l *coxv1alpha1.Loop) {
			f52Delivered(l, metav1.ConditionFalse, coxv1alpha1.ReasonDeliveryFailed)
		}, coxv1alpha1.ReasonDeliveryFailed},
		{"delivery cleared mid-hold", func(l *coxv1alpha1.Loop) {
			l.Spec.Delivery = nil
		}, verifyHoldSettledReason},
	} {
		loop := f52DeliveryLoop()
		loop.Status.Phase = coxv1alpha1.LoopPhaseVerifying
		loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseVerifying
		setCondition(loop, string(coxv1alpha1.LoopPhaseVerifying), metav1.ConditionTrue,
			coxv1alpha1.ReasonVerifyingDelivering, "verify passed; held until the delivery is terminal")
		tc.outcome(loop)

		if !settleDeliveryHold(loop) {
			t.Fatalf("%s: settleDeliveryHold = false, want true (the hold was released)", tc.name)
		}
		if loop.Status.Phase != coxv1alpha1.LoopPhaseSucceeded || loop.Status.DesiredPhase != coxv1alpha1.LoopPhaseSucceeded {
			t.Fatalf("%s: phase/desiredPhase = %s/%s, want Succeeded/Succeeded",
				tc.name, loop.Status.Phase, loop.Status.DesiredPhase)
		}
		c := apimeta.FindStatusCondition(loop.Status.Conditions, string(coxv1alpha1.LoopPhaseVerifying))
		if c == nil {
			t.Fatalf("%s: the Verifying condition was DELETED — the record that the Loop passed verify is gone (F52 B2)", tc.name)
		}
		if c.Status != metav1.ConditionFalse {
			t.Errorf("%s: Verifying condition = %s/%s, want False (the hold is released by the status, not by deletion)",
				tc.name, c.Status, c.Reason)
		}
		if c.Reason != tc.wantReason {
			t.Errorf("%s: Verifying condition reason = %q, want %q", tc.name, c.Reason, tc.wantReason)
		}
		if loopHoldingForDelivery(loop) {
			t.Errorf("%s: the Loop still reads as holding for delivery after settling", tc.name)
		}
		if settleDeliveryHold(loop) {
			t.Errorf("%s: a settled Loop settled a second time", tc.name)
		}
	}
}
