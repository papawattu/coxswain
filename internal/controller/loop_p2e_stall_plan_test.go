// Copyright 2026 papawattu.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

// P2e stall gate plan specs (TDD-PLAN-PHASE2 P2e "Envtest-first tests").
// These are the plan's numbered envtest specs 3, 5, 8, 9, 10, 11, 12, 13
// and 15, driven through the REAL reconciler (full Reconcile per plan spec):
//
//   - spec 3: stallAction=Pause + resume re-fires on identical output
//     (item 6: pausedFrom=Implementing, the iterate bookkeeping ran first,
//     the run is KEPT, the re-fire at k=4).
//   - spec 5: stallAction=Continue (the phase keeps iterating, Stalled=True,
//     the next iteration is allowed).
//   - spec 8: a normalisation-version change resets the consecutive count
//     (a v2 entry with the same hash does NOT extend a v1 run).
//   - spec 9/10: in-progress (I49) — a verify Job pod whose check is Running
//     or whose inits are not yet terminated gets NO decision (no entry, no
//     fire, a requeue).
//   - spec 11: per-Job sticky — a terminal Failed fire does not re-fire on a
//     re-reconcile with the same evidence (no second Event).
//   - spec 12: the cluster-wide coxswain-stall-defaults ConfigMap override
//     (Loop field > ConfigMap > built-in).
//   - spec 13: same-Loop update (I43) — a spec.loop.stallAfter edit after a
//     Continue fire neither re-fires nor clears the Stalled condition.
//   - spec 15: stall/budget precedence against the REAL P2d budget (item 8)
//     — a 3rd identical failure that ALSO pushes the tokens past
//     spec.budget.maxTokens in the SAME reconcile is Failed:Stalled, not
//     Failed:BudgetExceeded (the stall decision is evaluated before the
//     budget step; moving applyBudgetStep before the stall gate in Reconcile
//     must FAIL this spec).
//
// The plan's Gate mutations (dedup, version, non-consecutive, non-terminal,
// ConfigMap, re-fire within a Job, k==stallAfter, budget-before-stall,
// reset-on-pause, Pause-before-iterate, Continue-sets-Paused) are applied
// exactly as worded in a scratch worktree and recorded in
// .samples/p2e/mutations.md (the I49/R21 I55 norm).
package controller

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/cni"
	"github.com/papawattu/coxswain/internal/proxy"
)

const (
	p2eModelSecret   = "p2e-model-creds"
	p2eModelEndpoint = "10.0.0.9:9200"
	p2eBaseCommit    = "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
	p2eHeadCommit    = "b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1"
)

var _ = Describe("P2e: stall gate plan specs (envtest-first, plan P2e 3/5/8/9/10/11/12/13/15)", func() {
	ctx := context.Background()

	nsFor := func(prefix string) string {
		ns := prefix + "-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		return ns
	}

	getLoop := func(ns, name string) *coxv1alpha1.Loop {
		got := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, got)).To(Succeed())
		return got
	}

	setAnnotation := func(l *coxv1alpha1.Loop, key, value string) {
		if l.Annotations == nil {
			l.Annotations = map[string]string{}
		}
		l.Annotations[key] = value
		Expect(k8sClient.Update(ctx, l)).To(Succeed())
	}

	cond := func(l *coxv1alpha1.Loop, condType string) *metav1.Condition {
		for i := range l.Status.Conditions {
			if l.Status.Conditions[i].Type == condType {
				return &l.Status.Conditions[i]
			}
		}
		return nil
	}

	drainEvents := func(recorder *record.FakeRecorder) []string {
		var out []string
	Loop:
		for {
			select {
			case e, ok := <-recorder.Events:
				if !ok {
					break Loop
				}
				out = append(out, e)
			default:
				break Loop
			}
		}
		return out
	}

	// newP2ePlanReconciler builds a reconciler whose gates are all satisfied
	// (D30 AllowUnenforced, D38 CNI enforced) with a fixed failing
	// verify-check output (the readCheckOutput seam). The readProxyUsage seam
	// is set per spec where the spec needs a budget reading (spec 15).
	newP2ePlanReconciler := func(recorder *record.FakeRecorder, checkOutput string) *LoopReconciler {
		r := &LoopReconciler{
			Client:          k8sClient,
			Scheme:          k8sClient.Scheme(),
			AllowUnenforced: true,
			CNIProber:       cni.NewFakeProber(),
			Recorder:        recorder,
			readPhaseClaim:  func(context.Context, *coxv1alpha1.Loop) (*PhaseClaim, error) { return nil, nil },
			readCheckOutput: func(pod *corev1.Pod, checkName string) (string, bool) {
				return checkOutput, true
			},
		}
		r.CNIProber.(*cni.FakeProber).SetResult(cni.CNIProbeResult{Reason: cni.ReasonCNIEnforced})
		return r
	}

	// createP2ePlanLoop creates a Loop with a model endpoint, a budget (when
	// maxTokens != 0), one acceptance check, stallAfter, and the given
	// stallAction. status.baseCommit is seeded (the verify Job's clone needs
	// it).
	createP2ePlanLoop := func(ns, name string, stallAfter int32, action coxv1alpha1.StallAction, maxTokens int64) {
		var b *coxv1alpha1.BudgetConfig
		if maxTokens > 0 {
			t := maxTokens
			b = &coxv1alpha1.BudgetConfig{MaxTokens: &t, OnExceeded: coxv1alpha1.BudgetExceededActionPause}
		}
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      "P2e stall plan spec",
				Workspace: testWorkspace(),
				Verify:    coxv1alpha1.VerifyConfig{AcceptanceChecks: []string{loopCheckCmd}},
				Budget:    b,
				Loop: coxv1alpha1.LoopSettings{
					StallAfter:  &stallAfter,
					StallAction: action,
				},
				Agent: coxv1alpha1.AgentConfig{
					Image:             runnerImage,
					Model:             testModel,
					EndpointSecretRef: p2eModelSecret,
					ModelEndpoint:     p2eModelEndpoint,
				},
			},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		l := getLoop(ns, name)
		l.Status.BaseCommit = p2eBaseCommit
		Expect(k8sClient.Status().Update(ctx, l)).To(Succeed())
	}

	// createP2ePlanLoopNoStallAfter is spec 12's shape: a Loop with NO
	// spec.loop.stallAfter (the ConfigMap / built-in resolution).
	createP2ePlanLoopNoStallAfter := func(ns, name string) {
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      "P2e stall plan spec",
				Workspace: testWorkspace(),
				Verify:    coxv1alpha1.VerifyConfig{AcceptanceChecks: []string{loopCheckCmd}},
				Loop:      coxv1alpha1.LoopSettings{StallAction: coxv1alpha1.StallActionFail},
				Agent: coxv1alpha1.AgentConfig{
					Image:             runnerImage,
					Model:             testModel,
					EndpointSecretRef: p2eModelSecret,
					ModelEndpoint:     p2eModelEndpoint,
				},
			},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		l := getLoop(ns, name)
		l.Status.BaseCommit = p2eBaseCommit
		Expect(k8sClient.Status().Update(ctx, l)).To(Succeed())
	}

	// primeP2eProxy creates the model proxy the D35a gate requires and marks
	// it Ready (the P2d fixture shape).
	primeP2eProxy := func(ns, loopName string) {
		_ = k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: p2eModelSecret, Namespace: ns},
			StringData: map[string]string{modelAPIKey: "p2e-dummy", modelBaseURL: p2eModelEndpoint},
		})
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      proxyPodName(loopName),
				Namespace: ns,
				Labels: map[string]string{
					"app.kubernetes.io/managed-by": "coxswain",
					"app.kubernetes.io/loop":       loopName,
					kaptComponentLabel:             p2dModelProxyComponent,
				},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: p2dProxyComponent, Image: p2dProxyComponent}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		_ = k8sClient.Create(ctx, &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: egressProxyServiceName(loopName), Namespace: ns},
			Spec:       corev1.ServiceSpec{ClusterIP: "None"},
		})
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: proxyPodName(loopName)}, pod)).To(Succeed())
		now := metav1.Now()
		pod.Status.Conditions = []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: now},
		}
		pod.Status.PodIP = "127.0.0.1"
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	}

	reconcileOnce := func(r *LoopReconciler, ns, name string) {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		Expect(err).NotTo(HaveOccurred(), "reconcile %s/%s", ns, name)
	}

	// createP2eVerifyPod plants the verify Job's pod for iteration iter with
	// the given init statuses (tamper + artifact terminated exit 0 when
	// initDone; the check-0 state is checkState). A new iteration's pod is a
	// NEW object (the real operator's Job owns the pod; the envtest fixture
	// plants it directly, the S5a pattern).
	createP2eVerifyPod := func(ns, loopName string, iter int, initDone bool, checkState corev1.ContainerState) {
		_ = k8sClient.Delete(ctx, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-verify-pod-%d", loopName, iter), Namespace: ns},
		})
		tamperState := corev1.ContainerState{}
		artifactState := corev1.ContainerState{}
		if initDone {
			tamperState = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}
			artifactState = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}
		}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("%s-verify-pod-%d", loopName, iter),
				Namespace: ns,
				Labels:    map[string]string{"job-name": fmt.Sprintf("%s-verify-%d", loopName, iter), verifyForLabel: loopName},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "busybox"}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{
			{Name: "clone-base", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: "import-agent", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: verifyTamperInit, State: tamperState},
			{Name: verifyArtifactInit, State: artifactState},
			{Name: s5aCheck0, State: checkState},
		}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	}

	// advanceRepin re-seeds the Verifying phase at iteration iter with a pin
	// (the Implementing -> Verifying advance shape, seeded directly): the
	// next verify is the NEW Job <loop>-verify-<iter>.
	advanceRepin := func(ns, name string, iter int, commit string) {
		l := getLoop(ns, name)
		l.Status.Phase = coxv1alpha1.LoopPhaseVerifying
		l.Status.DesiredPhase = coxv1alpha1.LoopPhaseVerifying
		l.Status.Iteration = iter
		l.Status.CurrentVerify = &coxv1alpha1.CurrentVerifyStatus{VerifiedCommit: commit}
		Expect(k8sClient.Status().Update(ctx, l)).To(Succeed())
	}

	// aFailedCheck is a terminated non-zero check-0 (the terminal failure
	// evidence).
	aFailedCheck := corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}

	// --- the plan's specs ---

	It("spec 3: stallAction=Pause + resume re-fires on identical output (item 6: pausedFrom=Implementing, iterate ran first, the run is kept)", func() {
		ns := nsFor("p2e-s3")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2ePlanReconciler(recorder, p2eOutputRepeated)
		createP2ePlanLoop(ns, "stalllp", 3, coxv1alpha1.StallActionPause, 0)
		primeP2eProxy(ns, "stalllp")
		reconcileOnce(r, ns, "stalllp") // bootstrap to Planning

		// Three consecutive identical verify failures (three distinct Jobs:
		// <loop>-verify-1/2/3). The 3rd entry fires the detector (k=3).
		for iter := 1; iter <= 3; iter++ {
			advanceRepin(ns, "stalllp", iter, p2eHeadCommit)
			createP2eVerifyPod(ns, "stalllp", iter, true, aFailedCheck)
			reconcileOnce(r, ns, "stalllp")
			l := getLoop(ns, "stalllp")
			if iter < 3 {
				Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing),
					"iteration %d fails the check: the loop iterates", iter)
			} else {
				Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePaused), "the 3rd identical failure pauses the loop")
			}
		}

		l := getLoop(ns, "stalllp")
		Expect(l.Status.PausedReason).To(Equal(coxv1alpha1.PausedReasonStall), "pausedReason=Stall")
		Expect(l.Status.PausedFrom).To(Equal(coxv1alpha1.LoopPhaseImplementing),
			"pausedFrom=Implementing (item 6: the Pause is entered AFTER the iterate bookkeeping — the phase the iterate would have set)")
		Expect(l.Status.Iteration).To(Equal(4), "the iteration has ALREADY advanced (the iterate ran first: 3 -> 4)")
		Expect(l.Status.StallHistory).To(HaveLen(3), "the run is KEPT (3 entries, not reset)")
		Expect(cond(l, string(coxv1alpha1.StalledCondition))).ToNot(BeNil(), "the Stalled condition is set")

		By("resuming via the annotation: back to Implementing at iteration 4")
		l = getLoop(ns, "stalllp")
		setAnnotation(l, resumeAnnotation, "true")
		reconcileOnce(r, ns, "stalllp")
		l = getLoop(ns, "stalllp")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing), "resumed to the exact pausedFrom phase")
		Expect(l.Status.Iteration).To(Equal(4), "the resume does not re-iterate")
		Expect(l.Status.StallHistory).To(HaveLen(3), "the run is kept across the resume")

		By("the resumed agent produces the SAME failing output: the 4th Job extends the run to k=4 (>=3) and the stall RE-fires")
		advanceRepin(ns, "stalllp", 4, p2eHeadCommit)
		createP2eVerifyPod(ns, "stalllp", 4, true, aFailedCheck)
		reconcileOnce(r, ns, "stalllp")
		l = getLoop(ns, "stalllp")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePaused), "the stall re-fires on the 4th new Job (the detector is not exhausted by one pause)")
		Expect(l.Status.PausedReason).To(Equal(coxv1alpha1.PausedReasonStall))
		Expect(l.Status.StallHistory).To(HaveLen(4), "a 4th entry (the new Job <loop>-verify-4)")
	})

	It("spec 5: stallAction=Continue — the phase keeps iterating (Stalled is set, the next iteration is allowed)", func() {
		ns := nsFor("p2e-s5")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2ePlanReconciler(recorder, p2eOutputRepeated)
		createP2ePlanLoop(ns, "stalllp", 3, coxv1alpha1.StallActionContinue, 0)
		primeP2eProxy(ns, "stalllp")
		reconcileOnce(r, ns, "stalllp")

		for iter := 1; iter <= 3; iter++ {
			advanceRepin(ns, "stalllp", iter, p2eHeadCommit)
			createP2eVerifyPod(ns, "stalllp", iter, true, aFailedCheck)
			reconcileOnce(r, ns, "stalllp")
			l := getLoop(ns, "stalllp")
			Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing),
				"iteration %d: stallAction=Continue keeps the loop iterating (the phase is unchanged by the fire)", iter)
		}
		l := getLoop(ns, "stalllp")
		c := cond(l, string(coxv1alpha1.StalledCondition))
		Expect(c).ToNot(BeNil(), "the Stalled condition is set (a record of the fire)")
		Expect(c.Status).To(Equal(metav1.ConditionTrue))
		events := drainEvents(recorder)
		Expect(events).To(ContainElement(ContainSubstring("stall detector fired")),
			"a stall Event must fire: %v", events)

		By("the 4th iteration is allowed (the cap is the maxIterations one, unchanged)")
		advanceRepin(ns, "stalllp", 4, p2eHeadCommit)
		createP2eVerifyPod(ns, "stalllp", 4, true,
			corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}})
		reconcileOnce(r, ns, "stalllp")
		l = getLoop(ns, "stalllp")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded),
			"the 4th verify (a passing check) succeeds: the loop iterated on")
	})

	It("spec 8: a normalisation-version change resets the consecutive count (a v2 entry with the same hash does NOT extend a v1 run)", func() {
		ns := nsFor("p2e-s8")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2ePlanReconciler(recorder, p2eOutputRepeated)
		createP2ePlanLoop(ns, "stalllp", 3, coxv1alpha1.StallActionFail, 0)
		primeP2eProxy(ns, "stalllp")
		reconcileOnce(r, ns, "stalllp")

		// Two v1 entries (verify-1, verify-2) — then a v2 entry with the SAME
		// hash (verify-3, simulated by writing the entry directly, as the
		// S4/B3 tests do: the v1->v2 upgrade is out of band for this spec).
		// The consecutive count must be 1 (a fresh v1 run), not 3.
		advanceRepin(ns, "stalllp", 1, p2eHeadCommit)
		createP2eVerifyPod(ns, "stalllp", 1, true, aFailedCheck)
		reconcileOnce(r, ns, "stalllp")
		advanceRepin(ns, "stalllp", 2, p2eHeadCommit)
		createP2eVerifyPod(ns, "stalllp", 2, true, aFailedCheck)
		reconcileOnce(r, ns, "stalllp")

		l := getLoop(ns, "stalllp")
		Expect(l.Status.StallHistory).To(HaveLen(2), "two v1 entries from the two Jobs")
		Expect(l.Status.StallHistory[0].NormalisationVersion).To(Equal("v1"))
		Expect(l.Status.StallHistory[1].NormalisationVersion).To(Equal("v1"))
		h := l.Status.StallHistory[1].Hash
		v2Entry := coxv1alpha1.StallEntry{
			Iteration:            3,
			JobName:              "stalllp-verify-3",
			Hash:                 h,
			NormalisationVersion: "v2",
			Check:                s5aCheck0,
		}
		// The At must be set (the CRD's +kubebuilder:validation:Required on
		// status.stallHistory[].at — an envtest fixture has no kubelet to set
		// the finish time).
		now := metav1.Now()
		v2Entry.At = now
		l.Status.StallHistory = append(l.Status.StallHistory, v2Entry)
		Expect(k8sClient.Status().Update(ctx, l)).To(Succeed())

		// A 4th NEW v1 entry (verify-4, the same output): the trailing v1 run
		// is 1 (the v2 entry broke it) — NOT 3 — so no fire.
		advanceRepin(ns, "stalllp", 4, p2eHeadCommit)
		createP2eVerifyPod(ns, "stalllp", 4, true, aFailedCheck)
		reconcileOnce(r, ns, "stalllp")

		l = getLoop(ns, "stalllp")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing),
			"no fire: the v2 entry reset the run (a v1 hash and a v2 hash of the same output are not comparable)")
		Expect(cond(l, string(coxv1alpha1.StalledCondition))).To(BeNil(), "the Stalled condition is not set")
		Expect(l.Status.StallHistory).To(HaveLen(4))
		Expect(l.Status.StallHistory[3].JobName).To(Equal("stalllp-verify-4"))
	})

	It("spec 9: in-progress — a verify Job pod whose check is Running gets no decision (I49)", func() {
		ns := nsFor("p2e-s9")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2ePlanReconciler(recorder, p2eOutputRepeated)
		createP2ePlanLoop(ns, "stalllp", 3, coxv1alpha1.StallActionFail, 0)
		primeP2eProxy(ns, "stalllp")
		reconcileOnce(r, ns, "stalllp")

		advanceRepin(ns, "stalllp", 1, p2eHeadCommit)
		createP2eVerifyPod(ns, "stalllp", 1, true,
			corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}) // the check is RUNNING
		reconcileOnce(r, ns, "stalllp")

		l := getLoop(ns, "stalllp")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"a check container that has not terminated is IN-PROGRESS: no decision (no iterate, no fire)")
		Expect(l.Status.StallHistory).To(BeEmpty(), "no StallEntry while the check is Running (the terminal gate)")
		Expect(cond(l, string(coxv1alpha1.StalledCondition))).To(BeNil())
	})

	It("spec 10: in-progress — a verify Job pod whose inits are not started gets no decision (I49)", func() {
		ns := nsFor("p2e-s10")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2ePlanReconciler(recorder, p2eOutputRepeated)
		createP2ePlanLoop(ns, "stalllp", 3, coxv1alpha1.StallActionFail, 0)
		primeP2eProxy(ns, "stalllp")
		reconcileOnce(r, ns, "stalllp")

		advanceRepin(ns, "stalllp", 1, p2eHeadCommit)
		// The tamper + artifact inits are not terminated (initDone=false):
		// the kubelet has not advanced them yet.
		createP2eVerifyPod(ns, "stalllp", 1, false,
			corev1.ContainerState{Running: &corev1.ContainerStateRunning{}})
		reconcileOnce(r, ns, "stalllp")

		l := getLoop(ns, "stalllp")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"inits not yet started is IN-PROGRESS: no decision")
		Expect(l.Status.StallHistory).To(BeEmpty())
		Expect(cond(l, string(coxv1alpha1.StalledCondition))).To(BeNil())
	})

	It("spec 11: per-Job sticky — a terminal Failed fire does not re-fire on a re-reconcile with the same evidence (no second Event)", func() {
		ns := nsFor("p2e-s11")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2ePlanReconciler(recorder, p2eOutputRepeated)
		createP2ePlanLoop(ns, "stalllp", 1, coxv1alpha1.StallActionFail, 0)
		primeP2eProxy(ns, "stalllp")
		reconcileOnce(r, ns, "stalllp")

		advanceRepin(ns, "stalllp", 1, p2eHeadCommit)
		createP2eVerifyPod(ns, "stalllp", 1, true, aFailedCheck)
		reconcileOnce(r, ns, "stalllp")
		l := getLoop(ns, "stalllp")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseFailed), "the fire lands on the first failure (N=1)")
		fc := cond(l, string(coxv1alpha1.LoopPhaseFailed))
		Expect(fc).ToNot(BeNil())
		Expect(fc.Reason).To(Equal("Stalled"))

		By("re-reconciling with the SAME evidence (the same Job, no new entry): no re-fire")
		_ = drainEvents(recorder)
		reconcileOnce(r, ns, "stalllp")
		events := drainEvents(recorder)
		Expect(events).NotTo(ContainElement(ContainSubstring("stall detector fired")),
			"no second stall Event on the re-reconcile (the per-Job rule: no new Job, no new evaluation): %v", events)
		l = getLoop(ns, "stalllp")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseFailed), "the phase does not bounce (it is already terminal)")
	})

	It("spec 12: the coxswain-stall-defaults ConfigMap override (Loop field > ConfigMap > built-in 3)", func() {
		ns := nsFor("p2e-s12")
		defer deleteNS(ctx, ns)

		// The operator namespace + the ConfigMap (stallAfter=2). The operator
		// namespace is coxswain-system (the LoopReconciler's OperatorNamespace
		// default — the P2d budget prices ConfigMap uses the same default).
		operatorNS := "coxswain-system"
		nsErr := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: operatorNS}})
		Expect(apierrors.IsAlreadyExists(nsErr) || nsErr == nil).To(BeTrue(), "the operator namespace exists or was created")
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "coxswain-stall-defaults", Namespace: operatorNS},
			Data:       map[string]string{"stallAfter": "2"},
		}
		Expect(k8sClient.Create(ctx, cm)).To(Succeed())

		By("a Loop with NO spec.loop.stallAfter fires at 2 (the ConfigMap's value, not the built-in 3)")
		recorder := record.NewFakeRecorder(64)
		r := newP2ePlanReconciler(recorder, p2eOutputRepeated)
		r.OperatorNamespace = operatorNS // the reconciler reads the ConfigMap from the operator namespace
		createP2ePlanLoopNoStallAfter(ns, "stalllp")
		primeP2eProxy(ns, "stalllp")
		reconcileOnce(r, ns, "stalllp")
		advanceRepin(ns, "stalllp", 1, p2eHeadCommit)
		createP2eVerifyPod(ns, "stalllp", 1, true, aFailedCheck)
		reconcileOnce(r, ns, "stalllp")
		Expect(getLoop(ns, "stalllp").Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing), "iteration 1: no fire (k=1 < 2)")
		advanceRepin(ns, "stalllp", 2, p2eHeadCommit)
		createP2eVerifyPod(ns, "stalllp", 2, true, aFailedCheck)
		reconcileOnce(r, ns, "stalllp")
		l := getLoop(ns, "stalllp")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseFailed),
			"the ConfigMap's stallAfter=2 fires at the 2nd consecutive failure (not the built-in 3)")

		By("a Loop WITH spec.loop.stallAfter=5 ignores the ConfigMap (the Loop field wins): no fire at 2 or 3")
		recorder2 := record.NewFakeRecorder(64)
		r2 := newP2ePlanReconciler(recorder2, p2eOutputRepeated)
		createP2ePlanLoop(ns, "stalllp5", 5, coxv1alpha1.StallActionFail, 0)
		primeP2eProxy(ns, "stalllp5")
		reconcileOnce(r2, ns, "stalllp5")
		for iter := 1; iter <= 3; iter++ {
			advanceRepin(ns, "stalllp5", iter, p2eHeadCommit)
			createP2eVerifyPod(ns, "stalllp5", iter, true, aFailedCheck)
			reconcileOnce(r2, ns, "stalllp5")
			l := getLoop(ns, "stalllp5")
			Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing),
				"iteration %d: the ConfigMap is ignored (the Loop's stallAfter=5 wins) — no fire", iter)
		}
	})

	It("spec 13: same-Loop update (I43) — a spec.loop.stallAfter edit after a Continue fire neither re-fires nor clears the Stalled condition", func() {
		ns := nsFor("p2e-s13")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2ePlanReconciler(recorder, p2eOutputRepeated)
		createP2ePlanLoop(ns, "stalllp", 3, coxv1alpha1.StallActionContinue, 0)
		primeP2eProxy(ns, "stalllp")
		reconcileOnce(r, ns, "stalllp")
		for iter := 1; iter <= 3; iter++ {
			advanceRepin(ns, "stalllp", iter, p2eHeadCommit)
			createP2eVerifyPod(ns, "stalllp", iter, true, aFailedCheck)
			reconcileOnce(r, ns, "stalllp")
		}
		l := getLoop(ns, "stalllp")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing))
		Expect(cond(l, string(coxv1alpha1.StalledCondition))).ToNot(BeNil(), "the fire is recorded")

		By("updating spec.loop.stallAfter to a lower value (from the API server) and re-reconciling: the edit does not re-fire or clear the condition")
		l = getLoop(ns, "stalllp")
		lower := int32(1)
		l.Spec.Loop.StallAfter = &lower
		Expect(k8sClient.Update(ctx, l)).To(Succeed())
		reconcileOnce(r, ns, "stalllp")
		events := drainEvents(recorder)
		Expect(events).NotTo(ContainElement(ContainSubstring("stall detector fired")),
			"no NEW stall fire from the edit alone (the condition is a record, not a decision input): %v", events)
		l = getLoop(ns, "stalllp")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing), "the phase is unchanged by the edit")
		Expect(cond(l, string(coxv1alpha1.StalledCondition))).ToNot(BeNil(), "the Stalled condition is NOT cleared by the edit")
	})

	It("spec 15: stall/budget precedence against the REAL budget (item 8) — a 3rd identical failure that also exceeds spec.budget.maxTokens in the SAME reconcile is Failed:Stalled", func() {
		// The plan's spec 15: the 3rd identical verify failure ALSO pushes the
		// tokens past maxTokens (the readProxyUsage seam drives the reading).
		// In the SAME reconcile the stall decision is evaluated BEFORE the
		// budget step (item 8's precedence), so the phase is Failed:Stalled
		// (the stall's decision) — the budget cap hit is recorded (the
		// BudgetExceeded condition, P2d's spec 16 side) but the phase action
		// is inert (the phase is already terminal by the stall decision).
		// Mutation: moving applyBudgetStep before the stall gate in Reconcile
		// must FAIL this spec (the phase becomes Failed:BudgetExceeded /
		// Paused:Budget).
		ns := nsFor("p2e-s15")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2ePlanReconciler(recorder, p2eOutputRepeated)
		createP2ePlanLoop(ns, "stalllp", 3, coxv1alpha1.StallActionFail, 200)
		primeP2eProxy(ns, "stalllp")
		reconcileOnce(r, ns, "stalllp") // bootstrap (the reading is unset: no budget read)

		// The budget reading: the first read adopts the baseline (no delta),
		// the second adds the same-boot delta (250+10=260 >= 200: the cap is
		// exceeded). It is set on the SAME reconcile as the 3rd failure.
		var reads int
		r.readProxyUsage = func(context.Context, *coxv1alpha1.Loop) (proxy.Reading, error) {
			reads++
			if reads == 1 {
				return proxy.Reading{BootID: "B1", PromptTokens: 0, CompletionTokens: 0, Requests: 0}, nil
			}
			return proxy.Reading{BootID: "B1", PromptTokens: 250, CompletionTokens: 10, Requests: 3}, nil
		}

		// Iteration 1: the check fails with the same output (no fire yet —
		// the run is 1). The budget reading is adopted on this reconcile
		// (no delta, no decision).
		advanceRepin(ns, "stalllp", 1, p2eHeadCommit)
		createP2eVerifyPod(ns, "stalllp", 1, true, aFailedCheck)
		reconcileOnce(r, ns, "stalllp")
		Expect(getLoop(ns, "stalllp").Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing), "iteration 1: no fire yet (k=1)")

		// Iteration 2: the SAME reconcile carries the 2nd budget reading
		// (the delta pushes the accumulated sum to 260 >= 200 — the cap is
		// exceeded; the budget decision's onExceeded=Pause is recorded). The
		// stall run is 2 (no fire yet). The budget decision runs AFTER the
		// verify/stall decisions: the phase is still Verifying when it runs,
		// so the Pause action takes it to Paused (pausedReason=Budget).
		// This is the pre-3rd-failure state the plan's spec 15 builds to.
		advanceRepin(ns, "stalllp", 2, p2eHeadCommit)
		createP2eVerifyPod(ns, "stalllp", 2, true, aFailedCheck)
		reconcileOnce(r, ns, "stalllp")
		l := getLoop(ns, "stalllp")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePaused),
			"iteration 2: the budget cap hit (260 >= 200) pauses the loop (onExceeded=Pause) — the stall run is 2, no fire yet")
		Expect(l.Status.Budget.Exceeded).To(BeTrue())
		Expect(l.Status.Budget.ExceededReason).To(Equal(coxv1alpha1.BudgetExceededTokens))

		By("the budget's cap hit is recorded BEFORE the stall fires (the 3rd failure's reconcile carries the budget's sticky cap-hit flag — the budget step has already run)")
		l = getLoop(ns, "stalllp")
		Expect(l.Status.Budget.Exceeded).To(BeTrue(), "the budget cap hit is recorded (the sticky flag from iteration 2)")
		Expect(l.Status.Budget.ExceededReason).To(Equal(coxv1alpha1.BudgetExceededTokens))

		By("the 3rd identical failure's reconcile: the stall decision (k=3) is evaluated BEFORE the budget step's DECISION (item 8's precedence) — Failed:Stalled")
		advanceRepin(ns, "stalllp", 3, p2eHeadCommit)
		createP2eVerifyPod(ns, "stalllp", 3, true, aFailedCheck)
		reconcileOnce(r, ns, "stalllp")

		l = getLoop(ns, "stalllp")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseFailed), "the 3rd identical failure is terminal (the stall fires)")
		fc := cond(l, string(coxv1alpha1.LoopPhaseFailed))
		Expect(fc).ToNot(BeNil(), "the Failed condition is set")
		Expect(fc.Reason).To(Equal("Stalled"),
			"the stall's outcome wins (item 8): Failed:Stalled, not Failed:BudgetExceeded (moving applyBudgetStep before the stall gate must fail this spec)")
		// The Stalled condition is set (the stall's decision): the stall gate's
		// Fail action sets BOTH the Failed condition (the phase outcome, reason
		// Stalled) AND the Stalled condition (the detector's record). Item 8's
		// precedence: the stall's outcome wins over the budget's (Failed:
		// Stalled, not Failed:BudgetExceeded).
		sc := cond(l, string(coxv1alpha1.StalledCondition))
		Expect(sc).ToNot(BeNil(), "the Stalled condition is set (the stall's decision)")
		Expect(sc.Status).To(Equal(metav1.ConditionTrue), "the Stalled condition is True")
		// P2d's spec 16 side: the budget cap hit IS recorded (the sticky flag
		// from iteration 2), the reading is still folded into status.budget.
		Expect(l.Status.Budget).ToNot(BeNil())
		Expect(l.Status.Budget.Exceeded).To(BeTrue(), "the budget cap hit is recorded (the sticky flag is kept)")
		Expect(l.Status.Budget.ExceededReason).To(Equal(coxv1alpha1.BudgetExceededTokens))
	})
})
