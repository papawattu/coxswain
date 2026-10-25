// Copyright 2026 papawattu.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

func int32PtrEnv(v int32) *int32 {
	p := new(int32)
	*p = v
	return p
}

// A fixed commit SHA the B2 envtest cases use for both the evidence's and the
// operator-pinned verified commit (named to keep goconst quiet).
const b2Commit = "deadbeef"

// B2 (D10/D24) anti-gaming + fail-closed envtest. The operator's TamperedVerify
// decision at Verifying is driven by its OWN evidence (status.verify.*), never
// by the runner's result.json claim. In envtest (no Job controller) the tests
// set status.verify.* directly.
//
// The tri-state (D24): nil tamper evidence is NEVER clean (fail-closed) — the
// Loop stays in Verifying and B3 cannot reach Succeeded on it. Explicit 0 is
// clean (B3's check containers then decide). Non-zero ends Failed:TamperedVerify
// (terminal) even when the runner claims Verifying done.
var _ = Describe("B2 TamperedVerify via base-commit glob diff (D10/D24)", func() {
	ctx := context.Background()

	// ensureB2Sandbox creates the sandbox object if the recycle (or a previous
	// advance) deleted it (the specs' stand-in for the operator's own
	// ensureSandbox on the next reconcile; envtest has no agent-sandbox
	// controller to run the real one). Declared before driveToVerifying so the
	// closure captures it.
	ensureB2Sandbox := func(nn types.NamespacedName) {
		sb := &sandboxv1beta1.Sandbox{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: nn.Name + "-sandbox", Namespace: nn.Namespace}, sb); apierrors.IsNotFound(err) {
			_ = k8sClient.Create(ctx, &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: nn.Name + "-sandbox", Namespace: nn.Namespace},
			})
		}
	}

	// driveToVerifying drives a fresh Loop through the B1 claim path to
	// Verifying. R17 (2026-10-03): the claims are injected via the readPhaseClaim
	// seam (the legacy drive-by-status.observedPhase was deleted with the B1
	// production seam): the runner's claim for the phase it EXECUTED (the current
	// phase, status success) completes that phase and advances the machine one
	// step (claimPhaseForAdvance + nextPhase). The bootstrap moved Pending ->
	// Planning on the first reconcile, so the effective steps are the two
	// completed-phase claims (Planning, then Implementing).
	driveToVerifying := func(nn types.NamespacedName) {
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		ensureB2Sandbox(nn)
		for _, executed := range []coxv1alpha1.LoopPhase{
			coxv1alpha1.LoopPhasePlanning,
			coxv1alpha1.LoopPhaseImplementing,
		} {
			// S5a (B3 MVP gate): the Implementing claim carries the 40-hex headCommit
			// (the post-S5a runner always commits + reports the head on a successful
			// Implementing), so the operator advances to Verifying and pins it.
			r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
				c := &PhaseClaim{ObservedPhase: executed, Status: claimSuccess}
				if executed == coxv1alpha1.LoopPhaseImplementing {
					c.HeadCommit = "d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5" // 40-hex
				}
				return c, nil
			}
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
			r.readPhaseClaim = nil
			ensureB2Sandbox(nn) // the advance recycles the sandbox; recreate it for the next claim
		}
	}

	makeLoop := func(ns, name string) types.NamespacedName {
		nn := types.NamespacedName{Name: name, Namespace: ns}
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       coxv1alpha1.LoopSpec{Goal: loopGoal, Workspace: testWorkspace()},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		return nn
	}

	It("ends Failed:TamperedVerify (terminal) when the tamper code is non-zero, even though the runner claims success (D10 anti-gaming)", func() {
		ns := "b2-tamper-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		nn := makeLoop(ns, "b2-tamper")
		driveToVerifying(nn)
		got := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"setup: the Loop must reach Verifying before the tamper verdict")

		// The operator's tamper evidence: a protected path changed (non-zero),
		// for the current verifiedCommit. Plus the runner's claim that it's done.
		// The operator pinned the current commit (D11) to the SAME SHA the
		// evidence names, so the evidence is bound (D27).
		// F83c: the STATUS says clean (0) and the POD's tamper init exits 1. The
		// decision reads the pod (verifyOutcome), so the status cannot decide —
		// if anyone reintroduced a status-driven verdict, this spec fails.
		got.Status.Verify = &coxv1alpha1.VerifyStatus{
			TamperExitCode: int32PtrEnv(0),
			VerifiedCommit: b2Commit,
			JobName:        "b2-tamper-verify-1",
		}
		got.Status.CurrentVerify = &coxv1alpha1.CurrentVerifyStatus{VerifiedCommit: b2Commit}
		got.Status.ObservedPhase = coxv1alpha1.LoopPhaseVerifying
		Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())
		// The verify Job's pod (operator-only evidence) carries the real tamper
		// exit code; F83c made verifyOutcome its only reader.
		f83SetTamperPod(ctx, ns, nn.Name,
			corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}},
			corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}, nil)

		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, nn, got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseFailed),
			"D10: a non-zero tamper code must end the Loop Failed, not Succeeded")
		var failed bool
		for _, c := range got.Status.Conditions {
			if c.Type == string(coxv1alpha1.LoopPhaseFailed) && c.Status == metav1.ConditionTrue &&
				c.Reason == TamperedVerifyReason {
				failed = true
			}
		}
		Expect(failed).To(BeTrue(),
			"the terminal condition must record reason TamperedVerify (the operator's evidence, not the claim)")
	})

	It("a clean tamper pod lets verify SUCCEED (this fixture has no acceptance checks, so the clean path decides)", func() {
		ns := "b2-clean-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		nn := makeLoop(ns, "b2-clean")
		driveToVerifying(nn)
		got := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, got)).To(Succeed())
		// Clean tamper code (0) bound to the current commit (D27): the evidence
		// names the same SHA the operator pinned, so it counts.
		got.Status.Verify = &coxv1alpha1.VerifyStatus{
			TamperExitCode: int32PtrEnv(0),
			VerifiedCommit: b2Commit,
		}
		got.Status.CurrentVerify = &coxv1alpha1.CurrentVerifyStatus{VerifiedCommit: b2Commit}
		got.Status.ObservedPhase = coxv1alpha1.LoopPhaseVerifying
		Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())
		// F83c: plant the real pod (tamper 0, artifact 0, check-0 still running),
		// so the spec exercises verifyOutcome's clean path instead of the
		// no-pod no-decision path (which would pass for the wrong reason).
		runningCheck := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
		f83SetTamperPod(ctx, ns, nn.Name,
			corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
			corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}, &runningCheck)

		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, nn, got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded),
			"a clean tamper pod must not flip the Loop to Failed: with no acceptance checks the verify passes")
	})

	It("stays Verifying when there is NO tamper evidence at all (no pod, no status) — D24 fail-closed, never clean", func() {
		ns := "b2-nonev-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		nn := makeLoop(ns, "b2-nonev")
		driveToVerifying(nn)
		got := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, got)).To(Succeed())
		// No verify block at all and no verify pod (the Job never ran / crashed
		// before the tamper container finished / status lost). F83c: the decision
		// reads the POD, so this is the no-pod no-decision path. Absence of
		// evidence is NOT a pass.
		got.Status.Verify = nil
		got.Status.ObservedPhase = coxv1alpha1.LoopPhaseVerifying
		Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())

		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, nn, got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"D24: nil tamper evidence must not advance the Loop out of Verifying (fail-closed)")
		Expect(got.Status.Phase).ToNot(Equal(coxv1alpha1.LoopPhaseSucceeded),
			"the Loop must NOT reach Succeeded on no evidence")
		Expect(got.Status.Phase).ToNot(Equal(coxv1alpha1.LoopPhaseFailed),
			"no evidence is Unknown, not Tampered — it must not end Failed either")
	})

	It("stays Verifying when the verify pod belongs to a STALE Job (the freshness binding is the Job UID, F83c)", func() {
		ns := "b2-stale-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		nn := makeLoop(ns, "b2-stale")
		driveToVerifying(nn)
		got := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, got)).To(Succeed())
		Expect(got.Status.CurrentVerify).NotTo(BeNil())
		// F83c: the decision reads the pod, and readVerifyJobPod binds a pod to
		// the CURRENT verify Job by UID. A pod still owned by a previous Job
		// (the infra-retry recreate, the I65 bug) is not this attempt's evidence.
		staleJob := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "b2-stale-verify-0", Namespace: ns, Labels: verifyJobLabels(nn.Name)},
			Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever,
				Containers:    []corev1.Container{{Name: "noop", Image: "busybox"}},
			}}},
		}
		Expect(k8sClient.Create(ctx, staleJob)).To(Succeed())
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      nn.Name + "-verify-pod",
				Namespace: ns,
				Labels:    map[string]string{s5aJobNameLabel: verifyJobName(got), verifyForLabel: nn.Name},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: batchv1.SchemeGroupVersion.String(), Kind: "Job",
					Name: staleJob.Name, UID: staleJob.UID,
					Controller: new(true), BlockOwnerDeletion: new(true),
				}},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "noop", Image: "busybox"}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{
			{Name: s5aTamper, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
		}
		Expect(k8sClient.SubResource("status").Update(ctx, pod)).To(Succeed())

		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, nn, got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"a pod owned by a stale Job must not be this attempt's evidence (no decision)")
		Expect(got.Status.Phase).ToNot(Equal(coxv1alpha1.LoopPhaseSucceeded),
			"stale-Job evidence must not reach Succeeded")
	})
})
