package controller

// F83d: what the tamper record still got wrong after F83c deleted the
// status-driven gate. Three things are pinned here:
//
//   - the record cannot decide anything from STATUS, in the one window where a
//     re-added status reader would see exactly what a user wrote (the
//     delivery hold, where applyVerifyOutcome returns before the F83 writer
//     runs) — .samples/design-f83d.md §B;
//   - the record is per-PIN, so a new pin clears it (P3-5) instead of leaving
//     the previous iteration's tamper verdict on display for the next run and
//     for the delivered PR body;
//   - the VerifyInfraFailed message carries the kubelet's Terminated.Reason
//     (so an OOMKilled tamper init is readable as such, not as "exit 137").

import (
	"context"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cov "github.com/papawattu/coxswain/api/v1alpha1"
)

var _ = Describe("F83d: the tamper record is provenance, per pin, and readable", func() {
	ctx := context.Background()

	newNS := func(prefix string) string {
		ns := prefix + "-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		})
		return ns
	}

	It("a forged tamper verdict cannot fail a Loop held for delivery (nothing decides on status)", func() {
		ns := newNS("f83d-forge")
		const name = "f83d-forge"
		// The driven fixture: a real Loop at a PASSING verify, held for delivery
		// (spec.delivery.mode PullRequest). In the hold applyVerifyOutcome returns
		// before recordTamperEvidence, so the F83 writer does not run — the one
		// window where a re-added status reader would see exactly what a user
		// wrote (the F83 forge spec cannot catch it: the writer overwrites the
		// forge in the same pass).
		r := p5fToVerifyPassed(name, ns, true, false)
		s6Reconcile(r, ns, name)
		l := p5fGet(ns, name)
		Expect(loopHoldingForDelivery(l)).To(BeTrue(), "setup: the Loop is Verifying + Delivering")
		Expect(l.Status.CurrentVerify).NotTo(BeNil(), "setup: the pin is written on entering Verifying")
		Expect(l.Status.Verify).NotTo(BeNil(), "setup: the F83 writer recorded the passing tamper init")

		By("forging a tampered verdict that matches the real pin and the real Job UID")
		one := int32(1)
		l.Status.Verify.TamperExitCode = &one
		l.Status.Verify.VerifiedCommit = l.Status.CurrentVerify.VerifiedCommit
		Expect(k8sClient.Status().Update(ctx, l)).To(Succeed())

		got := s6Reconcile(r, ns, name)
		Expect(got.Status.Phase).To(Equal(cov.LoopPhaseVerifying),
			"a forged tamper verdict must not fail a Loop in the delivery hold (F83d)")
		Expect(findConditionFn(got.Status.Conditions, string(cov.LoopPhaseFailed))).To(BeNil())
		// The forge SURVIVES: the writer does not run under the hold, so the
		// immunity is "nothing reads status", not "the writer corrects it". If
		// the writer is ever extended to the hold, change this line deliberately.
		Expect(got.Status.Verify.TamperExitCode).NotTo(BeNil())
		Expect(*got.Status.Verify.TamperExitCode).To(Equal(int32(1)))
	})

	It("a new pin clears the previous pin's tamper record (P3-5)", func() {
		// The pin advance is the operator's own transition (Implementing ->
		// Verifying with a valid headCommit), so the fixture is the in-memory
		// Loop plus the claim seam: no cluster round-trip is needed to see the
		// clear.
		loop := &cov.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: "f83d-newpin", Namespace: "default"},
			Status: cov.LoopStatus{
				Phase:        cov.LoopPhaseImplementing,
				DesiredPhase: cov.LoopPhaseImplementing,
				Iteration:    2,
				Verify: &cov.VerifyStatus{
					TamperExitCode: func() *int32 { z := int32(0); return &z }(),
					VerifiedCommit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
					TamperJobUID:   "old-verify-job-uid",
					InfraAttempts:  2,
					InfraJobUID:    "old-verify-job-uid",
				},
			},
		}
		r := &LoopReconciler{
			readPhaseClaim: func(context.Context, *cov.Loop) (*PhaseClaim, error) {
				return &PhaseClaim{
					ObservedPhase: cov.LoopPhaseImplementing, Status: claimSuccess, Iteration: 2,
					HeadCommit: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
				}, nil
			},
		}
		_, changed, _ := r.advancePhaseFromClaim(ctx, loop)
		Expect(changed).To(BeTrue(), "the pin advance is a status change")
		Expect(loop.Status.Phase).To(Equal(cov.LoopPhaseVerifying), "setup: the claim advanced into Verifying")
		Expect(loop.Status.CurrentVerify).NotTo(BeNil())
		Expect(loop.Status.CurrentVerify.VerifiedCommit).To(Equal("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"), "the new pin")

		Expect(loop.Status.Verify).NotTo(BeNil())
		Expect(loop.Status.Verify.TamperExitCode).To(BeNil(),
			"F83d (P3-5): the previous pin's tamper verdict must not describe the new pin")
		Expect(loop.Status.Verify.VerifiedCommit).To(BeEmpty())
		Expect(loop.Status.Verify.TamperJobUID).To(BeEmpty())
		// The infra counters are the same per-pin reset (I65 P1-B), unchanged.
		Expect(loop.Status.Verify.InfraAttempts).To(BeZero())
		Expect(loop.Status.Verify.InfraJobUID).To(BeEmpty())
	})
})

// F83d (D.1): the VerifyInfraFailed message names the kubelet's reason, so an
// OOM-killed tamper init (the F83c mapping) is readable without knowing that
// exit 137 is SIGKILL.
func TestVerifyInfraFailureMessageCarriesTheKubeletReason(t *testing.T) {
	pod := &corev1.Pod{Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{
		{Name: verifyTamperInit, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled"}}},
		{Name: "check-0", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
	}}}

	msg := verifyInfraFailureMessage(pod, verifyTamperInit, 137)
	if !strings.Contains(msg, "reason OOMKilled") {
		t.Errorf("the message must carry the kubelet's reason:\n%s", msg)
	}
	if !strings.Contains(msg, "exit 137") {
		t.Errorf("the message must still carry the exit code:\n%s", msg)
	}

	// A check init that failed without a reason (the API allows it — F89) keeps
	// the old shape: no dangling ", reason )".
	noReason := &corev1.Pod{Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{
		{Name: "check-0", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}},
	}}}
	msg = verifyInfraFailureMessage(noReason, "check-0", 1)
	if strings.Contains(msg, "reason") {
		t.Errorf("no reason was recorded, so none may be rendered:\n%s", msg)
	}

	// The no-check case (a Failed Job with no terminated init) is unchanged.
	if got := verifyInfraFailureMessage(nil, "", 0); !strings.Contains(got, "no check init terminated") {
		t.Errorf("the no-check message changed: %s", got)
	}
}

// F83d (D.1): initTerminatedReason's four shapes.
func TestInitTerminatedReason(t *testing.T) {
	pod := &corev1.Pod{Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{
		{Name: "terminated", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"}}},
		{Name: "no-reason", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}},
		{Name: "running", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
	}}}
	for _, tc := range []struct{ name, want string }{
		{"terminated", "OOMKilled"},
		{"no-reason", ""},
		{"running", ""},
		{"absent", ""},
	} {
		if got := initTerminatedReason(pod, tc.name); got != tc.want {
			t.Errorf("initTerminatedReason(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := initTerminatedReason(nil, "terminated"); got != "" {
		t.Errorf("initTerminatedReason(nil) = %q, want \"\"", got)
	}
}
