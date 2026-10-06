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

// P2g (TDD-PLAN-PHASE2): the auditability sweep. Every P2 phase/budget/stall/
// pause transition must emit BOTH a condition (on status.conditions, with the
// right type/reason/status and a NON-EMPTY message) and an Event (the right
// reason, a non-empty message). This slice is the enforcement: if P2d/P2e/P2f
// forgot an Event or left a condition's message empty, these specs fail. The
// gate mutation (drop one Event from its transition site) makes specs 4 and 10
// fail; per-transition mutations are run in a scratch worktree (I49 norm) and
// recorded in .samples/p2g/mutations.md.
package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrl "sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/cni"
	"github.com/papawattu/coxswain/internal/proxy"
)

const (
	// p2gOutputRepeated is a single raw check output repeated across
	// iterations (the "N consecutive identical" drive for the stall specs).
	p2gOutputRepeated = "FAIL: TestBuild\nmain.go:10: assertion failed (got 1, want 2)\n"
	// p2gModelSecret / p2gModelEndpoint are the P2g fixture's model proxy
	// credentials (IP-literal: the D35a proxy gate peer is an ipBlock).
	p2gModelSecret   = "p2g-model-creds"
	p2gModelEndpoint = "10.0.0.8:9200"
	// p2gHeadCommit is a 40-hex commit (the verify Job pin shape).
	p2gHeadCommit = "f1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0"
)

var _ = Describe("P2g: conditions + events for every P2 transition (the auditability sweep)", func() {
	ctx := context.Background()

	// newP2gReconciler builds a reconciler whose gates are all satisfied (the
	// P2d/P2f fixture shape) so the full Reconcile drives the transition the
	// way production does. The readProxyUsage / readCheckOutput seams are set
	// per-It.
	newP2gReconciler := func(recorder *record.FakeRecorder) *LoopReconciler {
		r := &LoopReconciler{
			Client:          k8sClient,
			Scheme:          k8sClient.Scheme(),
			AllowUnenforced: true,
			CNIProber:       cni.NewFakeProber(),
			Recorder:        recorder,
			readPhaseClaim:  func(context.Context, *coxv1alpha1.Loop) (*PhaseClaim, error) { return nil, nil },
			readBaseCommit:  func(context.Context, *coxv1alpha1.Loop) (string, bool, error) { return p2gHeadCommit, true, nil },
		}
		// The clone-pending timer is suppressed: the fixture seeds
		// status.baseCommit itself (createP2gLoop).
		r.baseCommitSeeded = true
		r.CNIProber.(*cni.FakeProber).SetResult(cni.CNIProbeResult{Reason: cni.ReasonCNIEnforced})
		return r
	}

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

	// createP2gLoop creates a Loop with a model endpoint (the D35a gate is on
	// the path) and seeds status.baseCommit (no clone-pending timer).
	createP2gLoop := func(ns, name string, mutate func(*coxv1alpha1.Loop)) *coxv1alpha1.Loop {
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      "P2g audit spec",
				Workspace: testWorkspace(),
				Agent: coxv1alpha1.AgentConfig{
					Image:             runnerImage,
					Model:             testModel,
					EndpointSecretRef: p2gModelSecret,
					ModelEndpoint:     p2gModelEndpoint,
				},
			},
		}
		if mutate != nil {
			mutate(loop)
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		l := getLoop(ns, name)
		l.Status.BaseCommit = p2gHeadCommit
		Expect(k8sClient.Status().Update(ctx, l)).To(Succeed())
		return l
	}

	// primeP2gProxy creates the model proxy the D35a gate requires (the P2d
	// fixture shape: the pod directly, a headless egress service, a Ready pod
	// status) and the secret.
	primeP2gProxy := func(loop *coxv1alpha1.Loop) {
		_ = k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: p2gModelSecret, Namespace: loop.Namespace},
			StringData: map[string]string{modelAPIKey: "p2g-dummy", modelBaseURL: p2gModelEndpoint},
		})
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      proxyPodName(loop.Name),
				Namespace: loop.Namespace,
				Labels: map[string]string{
					"app.kubernetes.io/managed-by": "coxswain",
					"app.kubernetes.io/loop":       loop.Name,
					kaptComponentLabel:             p2dModelProxyComponent,
				},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: p2dProxyComponent, Image: p2dProxyComponent}},
			},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		_ = k8sClient.Create(ctx, &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      egressProxyServiceName(loop.Name),
				Namespace: loop.Namespace,
			},
			Spec: corev1.ServiceSpec{ClusterIP: "None"},
		})
		pod = &corev1.Pod{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: loop.Namespace, Name: proxyPodName(loop.Name)}, pod)).To(Succeed())
		now := metav1.Now()
		pod.Status.Conditions = []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: now},
		}
		pod.Status.PodIP = "127.0.0.1"
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	}

	seedPhase := func(l *coxv1alpha1.Loop, phase coxv1alpha1.LoopPhase) {
		l.Status.Phase = phase
		l.Status.DesiredPhase = phase
		Expect(k8sClient.Status().Update(ctx, l)).To(Succeed())
	}

	reconcile := func(r *LoopReconciler, ns, name string) {
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		Expect(err).NotTo(HaveOccurred(), "reconcile %s/%s", ns, name)
	}

	setSuspend := func(ns, name string, suspend bool) {
		l := getLoop(ns, name)
		l.Spec.Suspend = suspend
		Expect(k8sClient.Update(ctx, l)).To(Succeed())
	}

	setResumeAnnotation := func(ns, name string) {
		l := getLoop(ns, name)
		if l.Annotations == nil {
			l.Annotations = map[string]string{}
		}
		l.Annotations[resumeAnnotation] = unstructuredTrue
		Expect(k8sClient.Update(ctx, l)).To(Succeed())
	}

	seedBudgetP2g := func(l *coxv1alpha1.Loop, mutate func(*coxv1alpha1.BudgetStatus)) {
		if l.Status.Budget == nil {
			l.Status.Budget = &coxv1alpha1.BudgetStatus{}
		}
		mutate(l.Status.Budget)
		Expect(k8sClient.Status().Update(ctx, l)).To(Succeed())
	}

	// aP2gFailingPod returns a verify pod with check-0 failed (exit 1) and a
	// finish time (the StallEntry's At), driving the stall gate through the
	// readCheckOutput seam.
	aP2gFailingPod := func() *corev1.Pod {
		fin := metav1.Now()
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "p2g-stall-verify-1", Namespace: ""},
			Status: corev1.PodStatus{
				InitContainerStatuses: []corev1.ContainerStatus{
					{Name: verifyTamperInit, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
					{Name: verifyArtifactInit, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
					{Name: s5aCheck0, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, FinishedAt: fin}}},
				},
			},
		}
	}

	// runStallFailures drives applyStallGate for iterations [1..n] with a
	// fixed output (the P2e envtest shape), returning whether the last
	// iteration fired.
	runStallFailures := func(r *LoopReconciler, ns string, n int) bool {
		var fired bool
		for iter := 1; iter <= n; iter++ {
			loop := getLoop(ns, "p2g-stall")
			loop.Status.Phase = coxv1alpha1.LoopPhaseVerifying
			loop.Status.Iteration = iter
			Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
			fired = r.applyStallGate(ctx, loop, aP2gFailingPod(), s5aCheck0)
			Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		}
		return fired
	}

	// eventWithReason returns a recorded event string of the given reason (""
	// when none). The FakeRecorder formats "Reason: <reason>; <message>".
	eventWithReason := func(events []string, reason string) string {
		prefix := "Reason: " + reason + "; "
		for _, e := range events {
			if len(e) >= len(prefix) && e[:len(prefix)] == prefix {
				return e
			}
		}
		return ""
	}

	// condition returns the named condition (nil when absent).
	condition := func(l *coxv1alpha1.Loop, condType string) *metav1.Condition {
		for i := range l.Status.Conditions {
			if l.Status.Conditions[i].Type == condType {
				return &l.Status.Conditions[i]
			}
		}
		return nil
	}

	It("spec 1: -> Paused (suspend source): the Paused condition names the source; the Paused Event names it", func() {
		ns := nsFor("p2g-s1")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2gReconciler(recorder)
		loop := createP2gLoop(ns, "p2g-s1", nil)
		primeP2gProxy(loop)
		reconcile(r, ns, "p2g-s1") // bootstrap
		seedPhase(getLoop(ns, "p2g-s1"), coxv1alpha1.LoopPhaseImplementing)
		reconcile(r, ns, "p2g-s1")

		By("flipping suspend=true and re-reconciling")
		setSuspend(ns, "p2g-s1", true)
		reconcile(r, ns, "p2g-s1")

		l := getLoop(ns, "p2g-s1")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePaused))
		Expect(l.Status.PausedReason).To(Equal(coxv1alpha1.PausedReasonSuspend))
		c := condition(l, coxv1alpha1.PausedCondition)
		Expect(c).NotTo(BeNil(), "the Paused condition must be set")
		Expect(c.Status).To(Equal(metav1.ConditionTrue))
		Expect(c.Message).NotTo(BeEmpty(), "the Paused condition message must be non-empty")
		Expect(c.Message).To(ContainSubstring("suspend"), "the Paused condition message must name the source: %v", c.Message)
		events := drainEvents(recorder)
		paused := eventWithReason(events, "Paused")
		Expect(paused).NotTo(BeEmpty(), "a Paused Event must be recorded: %v", events)
		Expect(paused).To(ContainSubstring("suspend"), "the Paused Event message must name the source: %v", paused)
	})

	It("spec 2: -> Paused (stall source): the Paused condition names the source; the Paused Event names it; the Stalled condition is True", func() {
		ns := nsFor("p2g-s2")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2gReconciler(recorder)
		stallAfter := int32(3)
		loop := createP2gLoop(ns, "p2g-stall", func(l *coxv1alpha1.Loop) {
			l.Spec.Loop = coxv1alpha1.LoopSettings{
				StallAfter:  &stallAfter,
				StallAction: coxv1alpha1.StallActionPause,
			}
		})
		primeP2gProxy(loop)
		reconcile(r, ns, "p2g-stall") // bootstrap
		r.readCheckOutput = func(*corev1.Pod, string) (string, bool) { return p2gOutputRepeated, true }
		runStallFailures(r, ns, 3)

		l := getLoop(ns, "p2g-stall")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePaused), "stallAction=Pause -> Paused")
		Expect(l.Status.PausedReason).To(Equal(coxv1alpha1.PausedReasonStall))
		sc := condition(l, string(coxv1alpha1.StalledCondition))
		Expect(sc).NotTo(BeNil(), "the Stalled condition must be True (the detector's record)")
		Expect(sc.Status).To(Equal(metav1.ConditionTrue))
		Expect(sc.Message).NotTo(BeEmpty())
		pc := condition(l, coxv1alpha1.PausedCondition)
		Expect(pc).NotTo(BeNil(), "the Paused condition must be set")
		Expect(pc.Status).To(Equal(metav1.ConditionTrue))
		Expect(pc.Message).To(ContainSubstring("stall"), "the Paused condition message must name the source: %v", pc.Message)
		events := drainEvents(recorder)
		paused := eventWithReason(events, "Paused")
		Expect(paused).NotTo(BeEmpty(), "a Paused Event must be recorded: %v", events)
		Expect(paused).To(ContainSubstring("stall"), "the Paused Event message must name the source: %v", paused)
	})

	It("spec 3: -> Paused (budget source): the Paused condition names the source; the Paused Event names it; the BudgetExceeded condition is True", func() {
		ns := nsFor("p2g-s3")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2gReconciler(recorder)
		maxTokens := int64(200)
		loop := createP2gLoop(ns, "p2g-s3", func(l *coxv1alpha1.Loop) {
			l.Spec.Budget = &coxv1alpha1.BudgetConfig{
				MaxTokens:  &maxTokens,
				OnExceeded: coxv1alpha1.BudgetExceededActionPause,
			}
		})
		seedPhase(loop, coxv1alpha1.LoopPhaseImplementing)
		primeP2gProxy(loop)
		// Back the token count up to the cap (a same-boot delta would do the
		// same, but the seed is the deterministic P2d fixture shape): the
		// reading confirms the count, and the decision fires on this
		// reconcile.
		seedBudgetP2g(getLoop(ns, "p2g-s3"), func(b *coxv1alpha1.BudgetStatus) {
			b.PromptTokens = 250
		})
		reconcile(r, ns, "p2g-s3") // bootstrap + the budget decision (250 >= 200 -> Pause)

		l := getLoop(ns, "p2g-s3")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePaused), "onExceeded=Pause -> Paused")
		Expect(l.Status.PausedReason).To(Equal(coxv1alpha1.PausedReasonBudget))
		bc := condition(l, coxv1alpha1.BudgetExceededCondition)
		Expect(bc).NotTo(BeNil(), "the BudgetExceeded condition must be True")
		Expect(bc.Status).To(Equal(metav1.ConditionTrue))
		Expect(bc.Message).NotTo(BeEmpty())
		pc := condition(l, coxv1alpha1.PausedCondition)
		Expect(pc).NotTo(BeNil(), "the Paused condition must be set")
		Expect(pc.Status).To(Equal(metav1.ConditionTrue))
		Expect(pc.Message).To(ContainSubstring("budget"), "the Paused condition message must name the source: %v", pc.Message)
		events := drainEvents(recorder)
		paused := eventWithReason(events, "Paused")
		Expect(paused).NotTo(BeEmpty(), "a Paused Event must be recorded: %v", events)
		Expect(paused).To(ContainSubstring("budget"), "the Paused Event message must name the source: %v", paused)
	})

	It("spec 4: resume (suspend=false): the Paused condition is False/Resumed; the Resumed Event names the phase + reason", func() {
		ns := nsFor("p2g-s4")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2gReconciler(recorder)
		loop := createP2gLoop(ns, "p2g-s4", nil)
		primeP2gProxy(loop)
		reconcile(r, ns, "p2g-s4") // bootstrap
		seedPhase(getLoop(ns, "p2g-s4"), coxv1alpha1.LoopPhaseImplementing)
		reconcile(r, ns, "p2g-s4")

		By("pausing (suspend=true) and re-reconciling")
		setSuspend(ns, "p2g-s4", true)
		reconcile(r, ns, "p2g-s4")
		Expect(getLoop(ns, "p2g-s4").Status.Phase).To(Equal(coxv1alpha1.LoopPhasePaused))

		By("resuming (suspend=false) and re-reconciling")
		setSuspend(ns, "p2g-s4", false)
		reconcile(r, ns, "p2g-s4")

		l := getLoop(ns, "p2g-s4")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing), "back to the exact pausedFrom phase")
		c := condition(l, coxv1alpha1.PausedCondition)
		Expect(c).NotTo(BeNil(), "the Paused condition must be set")
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Reason).To(Equal("Resumed"))
		Expect(c.Message).NotTo(BeEmpty())
		events := drainEvents(recorder)
		resumed := eventWithReason(events, "Resumed")
		Expect(resumed).NotTo(BeEmpty(), "a Resumed Event must be recorded: %v", events)
		Expect(resumed).To(ContainSubstring("Implementing"), "the Resumed Event message must name the phase: %v", resumed)
		Expect(resumed).To(ContainSubstring("Suspend"), "the Resumed Event message must name the reason: %v", resumed)
	})

	It("spec 5: -> Failed:Stalled (stall Fail): the Failed condition is reason Stalled; the Stalled condition is True; the Stalled Event fires", func() {
		ns := nsFor("p2g-s5")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2gReconciler(recorder)
		stallAfter := int32(3)
		loop := createP2gLoop(ns, "p2g-stall", func(l *coxv1alpha1.Loop) {
			l.Spec.Loop = coxv1alpha1.LoopSettings{
				StallAfter:  &stallAfter,
				StallAction: coxv1alpha1.StallActionFail,
			}
		})
		primeP2gProxy(loop)
		reconcile(r, ns, "p2g-stall") // bootstrap
		r.readCheckOutput = func(*corev1.Pod, string) (string, bool) { return p2gOutputRepeated, true }
		runStallFailures(r, ns, 3)

		l := getLoop(ns, "p2g-stall")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseFailed), "stallAction=Fail -> Failed")
		fc := condition(l, string(coxv1alpha1.LoopPhaseFailed))
		Expect(fc).NotTo(BeNil(), "the Failed condition must be set")
		Expect(fc.Status).To(Equal(metav1.ConditionTrue))
		Expect(fc.Reason).To(Equal("Stalled"))
		Expect(fc.Message).NotTo(BeEmpty())
		sc := condition(l, string(coxv1alpha1.StalledCondition))
		Expect(sc).NotTo(BeNil(), "the Stalled condition must be True")
		Expect(sc.Status).To(Equal(metav1.ConditionTrue))
		Expect(sc.Message).NotTo(BeEmpty())
		events := drainEvents(recorder)
		stalled := eventWithReason(events, "Stalled")
		Expect(stalled).NotTo(BeEmpty(), "a Stalled Event must be recorded (the detector's record): %v", events)
	})

	It("spec 6: -> Failed:BudgetExceeded (budget Fail): the Failed condition is reason BudgetExceeded; the BudgetExceeded condition is True; the BudgetExceeded Event fires", func() {
		ns := nsFor("p2g-s6")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2gReconciler(recorder)
		maxTokens := int64(200)
		loop := createP2gLoop(ns, "p2g-s6", func(l *coxv1alpha1.Loop) {
			l.Spec.Budget = &coxv1alpha1.BudgetConfig{
				MaxTokens:  &maxTokens,
				OnExceeded: coxv1alpha1.BudgetExceededActionFail,
			}
		})
		seedPhase(loop, coxv1alpha1.LoopPhasePlanning)
		primeP2gProxy(loop)
		seedBudgetP2g(getLoop(ns, "p2g-s6"), func(b *coxv1alpha1.BudgetStatus) {
			b.PromptTokens = 250
		})
		reconcile(r, ns, "p2g-s6") // the budget decision (250 >= 200 -> Fail)

		l := getLoop(ns, "p2g-s6")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseFailed), "onExceeded=Fail -> Failed")
		fc := condition(l, string(coxv1alpha1.LoopPhaseFailed))
		Expect(fc).NotTo(BeNil(), "the Failed condition must be set")
		Expect(fc.Reason).To(Equal("BudgetExceeded"))
		Expect(fc.Message).NotTo(BeEmpty())
		bc := condition(l, coxv1alpha1.BudgetExceededCondition)
		Expect(bc).NotTo(BeNil())
		Expect(bc.Status).To(Equal(metav1.ConditionTrue))
		Expect(bc.Message).NotTo(BeEmpty())
		events := drainEvents(recorder)
		over := eventWithReason(events, "BudgetExceeded")
		Expect(over).NotTo(BeEmpty(), "a BudgetExceeded Event must be recorded: %v", events)
	})

	It("spec 7: stall Continue (no phase change): the Stalled condition is True; the Stalled Event fires; the phase is unchanged", func() {
		ns := nsFor("p2g-s7")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2gReconciler(recorder)
		stallAfter := int32(3)
		loop := createP2gLoop(ns, "p2g-stall", func(l *coxv1alpha1.Loop) {
			l.Spec.Loop = coxv1alpha1.LoopSettings{
				StallAfter:  &stallAfter,
				StallAction: coxv1alpha1.StallActionContinue,
			}
		})
		primeP2gProxy(loop)
		reconcile(r, ns, "p2g-stall") // bootstrap
		r.readCheckOutput = func(*corev1.Pod, string) (string, bool) { return p2gOutputRepeated, true }
		fired := runStallFailures(r, ns, 3)

		l := getLoop(ns, "p2g-stall")
		Expect(fired).To(BeTrue(), "the detector fires at N=3")
		Expect(l.Status.Phase).ToNot(Equal(coxv1alpha1.LoopPhaseFailed), "stallAction=Continue keeps the loop (no phase change)")
		Expect(l.Status.Phase).ToNot(Equal(coxv1alpha1.LoopPhasePaused))
		sc := condition(l, string(coxv1alpha1.StalledCondition))
		Expect(sc).NotTo(BeNil(), "the Stalled condition records the state even though the phase is unchanged")
		Expect(sc.Status).To(Equal(metav1.ConditionTrue))
		Expect(sc.Message).NotTo(BeEmpty())
		events := drainEvents(recorder)
		stalled := eventWithReason(events, "Stalled")
		Expect(stalled).NotTo(BeEmpty(), "a Stalled Event must be recorded: %v", events)
	})

	It("spec 8: ClearedOnResume (a raised-cap budget resume): the BudgetExceeded condition is False/ClearedOnResume; the Event fires", func() {
		ns := nsFor("p2g-s8")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2gReconciler(recorder)
		maxTokens := int64(200)
		loop := createP2gLoop(ns, "p2g-s8", func(l *coxv1alpha1.Loop) {
			l.Spec.Budget = &coxv1alpha1.BudgetConfig{
				MaxTokens:  &maxTokens,
				OnExceeded: coxv1alpha1.BudgetExceededActionPause,
			}
		})
		primeP2gProxy(loop)
		reconcile(r, ns, "p2g-s8") // bootstrap

		// Simulate a Budget pause (the P2f fixture shape): phase Paused,
		// exceeded, the counts over the cap.
		l := getLoop(ns, "p2g-s8")
		l.Status.Phase = coxv1alpha1.LoopPhasePaused
		l.Status.DesiredPhase = coxv1alpha1.LoopPhasePaused
		l.Status.PausedFrom = coxv1alpha1.LoopPhaseImplementing
		l.Status.PausedReason = coxv1alpha1.PausedReasonBudget
		if l.Status.Budget == nil {
			l.Status.Budget = &coxv1alpha1.BudgetStatus{}
		}
		l.Status.Budget.PromptTokens = 250
		l.Status.Budget.Exceeded = true
		l.Status.Budget.ExceededReason = coxv1alpha1.BudgetExceededTokens
		Expect(k8sClient.Status().Update(ctx, l)).To(Succeed())
		reconcile(r, ns, "p2g-s8") // re-create the sandbox after the phase change
		reconcile(r, ns, "p2g-s8")

		By("raising the cap (an I43 same-Loop update) and resuming via the annotation")
		l = getLoop(ns, "p2g-s8")
		l.Spec.Budget.MaxTokens = p2gInt64Ptr(500)
		Expect(k8sClient.Update(ctx, l)).To(Succeed())
		setResumeAnnotation(ns, "p2g-s8")
		reconcile(r, ns, "p2g-s8")

		l = getLoop(ns, "p2g-s8")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing), "the Loop proceeds (the re-evaluation cleared the exceedance)")
		Expect(l.Status.Budget.Exceeded).To(BeFalse(), "raising the cap clears the exceedance (250 < 500)")
		bc := condition(l, coxv1alpha1.BudgetExceededCondition)
		Expect(bc).NotTo(BeNil(), "the BudgetExceeded condition must be set")
		Expect(bc.Status).To(Equal(metav1.ConditionFalse))
		Expect(bc.Reason).To(Equal("ClearedOnResume"))
		Expect(bc.Message).NotTo(BeEmpty())
		events := drainEvents(recorder)
		cleared := eventWithReason(events, "BudgetExceeded")
		Expect(cleared).NotTo(BeEmpty(), "a Normal Event must fire on the clear: %v", events)
	})

	It("spec 9: MeteringReset (a boot-ID change): the Warning Event fires; the bootIDChanged field is true; no condition", func() {
		ns := nsFor("p2g-s9")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2gReconciler(recorder)
		maxTokens := int64(10_000)
		loop := createP2gLoop(ns, "p2g-s9", func(l *coxv1alpha1.Loop) {
			l.Spec.Budget = &coxv1alpha1.BudgetConfig{
				MaxTokens:  &maxTokens,
				OnExceeded: coxv1alpha1.BudgetExceededActionFail,
			}
		})
		seedPhase(loop, coxv1alpha1.LoopPhasePlanning)
		primeP2gProxy(loop)
		// Seed the stored state (the P2d spec 10 shape): lastBootID B1.
		seedBudgetP2g(getLoop(ns, "p2g-s9"), func(b *coxv1alpha1.BudgetStatus) {
			b.LastBootID = "B1"
			b.LastPromptTokens = 100
			b.PromptTokens = 100
		})
		// A reading at B2 (a DIFFERENT boot — the pod was recreated): the
		// MeteringReset Warning fires and bootIDChanged is set. The cap is far
		// above the reading, so no decision.
		r.readProxyUsage = func(context.Context, *coxv1alpha1.Loop) (proxy.Reading, error) {
			return proxy.Reading{BootID: "B2", PromptTokens: 50, Requests: 1}, nil
		}
		reconcile(r, ns, "p2g-s9")

		l := getLoop(ns, "p2g-s9")
		Expect(l.Status.Budget.BootIDChanged).To(BeTrue(), "a genuine new boot is sticky bootIDChanged")
		events := drainEvents(recorder)
		reset := eventWithReason(events, meteringResetReason)
		Expect(reset).NotTo(BeEmpty(), "a Warning MeteringReset Event must fire: %v", events)
		Expect(reset).To(HavePrefix("Reason: "+meteringResetReason+"; "), "the event reason must be %s: %v", meteringResetReason, reset)
		// The named no-condition anomaly (MeteringReset): an Event, NO condition
		// (the status.budget.bootIDChanged field is the record).
		Expect(l.Status.Budget.BootIDChanged).To(BeTrue())
	})

	It("spec 10: every transition in the inventory has a condition + an Event with a NON-EMPTY message (the structural sweep)", func() {
		// The structural sweep (PLAN.md's auditability: all auditability comes
		// from status + Kubernetes events, never logs alone). Table-driven over
		// the inventory: for each transition, re-drive it and assert (a) the
		// condition is set with the right type/reason/status and a non-empty
		// message, and (b) an Event with the right reason and a non-empty
		// message is recorded.
		type transition struct {
			name        string
			setup       func(ns string, r *LoopReconciler, recorder *record.FakeRecorder)
			condType    string
			condStatus  metav1.ConditionStatus
			condReason  string // "" when the reason is not pinned
			eventReason string
			// noCondition is true for the named no-condition anomaly
			// (MeteringReset: the Event fires, but no condition — the
			// bootIDChanged field is the record).
			noCondition bool
		}

		// driveSuspendPause drives the suspend entry (spec.suspend=true).
		driveSuspendPause := func(ns string, r *LoopReconciler, _ *record.FakeRecorder) {
			createP2gLoop(ns, "p2g-s10", nil)
			primeP2gProxy(getLoop(ns, "p2g-s10"))
			reconcile(r, ns, "p2g-s10")
			seedPhase(getLoop(ns, "p2g-s10"), coxv1alpha1.LoopPhaseImplementing)
			reconcile(r, ns, "p2g-s10")
			setSuspend(ns, "p2g-s10", true)
			reconcile(r, ns, "p2g-s10")
		}

		// driveStallFire drives the stall gate to a fire with the given
		// action.
		driveStallFire := func(action coxv1alpha1.StallAction, ns string, r *LoopReconciler, _ *record.FakeRecorder) {
			stallAfter := int32(3)
			createP2gLoop(ns, "p2g-stall", func(l *coxv1alpha1.Loop) {
				l.Spec.Loop = coxv1alpha1.LoopSettings{
					StallAfter:  &stallAfter,
					StallAction: action,
				}
			})
			primeP2gProxy(getLoop(ns, "p2g-stall"))
			reconcile(r, ns, "p2g-stall")
			r.readCheckOutput = func(*corev1.Pod, string) (string, bool) { return p2gOutputRepeated, true }
			runStallFailures(r, ns, 3)
		}

		// driveBudgetFire drives the budget decision to a fire with the given
		// action (the P2d fixture shape: the count is seeded at the cap).
		driveBudgetFire := func(action coxv1alpha1.BudgetExceededAction, ns string, r *LoopReconciler, _ *record.FakeRecorder) {
			maxTokens := int64(200)
			createP2gLoop(ns, "p2g-s10", func(l *coxv1alpha1.Loop) {
				l.Spec.Budget = &coxv1alpha1.BudgetConfig{
					MaxTokens:  &maxTokens,
					OnExceeded: action,
				}
			})
			seedPhase(getLoop(ns, "p2g-s10"), coxv1alpha1.LoopPhaseImplementing)
			primeP2gProxy(getLoop(ns, "p2g-s10"))
			seedBudgetP2g(getLoop(ns, "p2g-s10"), func(b *coxv1alpha1.BudgetStatus) {
				b.PromptTokens = 250
			})
			reconcile(r, ns, "p2g-s10")
		}

		// driveClearedOnResume drives a Budget pause to a raised-cap resume
		// (the P2f spec 8(b) shape, re-driven for the sweep): the re-evaluation
		// clears the exceedance (ClearedOnResume + the Event) and the resume
		// proceeds.
		driveClearedOnResume := func(ns string, r *LoopReconciler, _ *record.FakeRecorder) {
			maxTokens := int64(200)
			createP2gLoop(ns, "p2g-s10", func(l *coxv1alpha1.Loop) {
				l.Spec.Budget = &coxv1alpha1.BudgetConfig{
					MaxTokens:  &maxTokens,
					OnExceeded: coxv1alpha1.BudgetExceededActionPause,
				}
			})
			primeP2gProxy(getLoop(ns, "p2g-s10"))
			reconcile(r, ns, "p2g-s10") // bootstrap

			l := getLoop(ns, "p2g-s10")
			l.Status.Phase = coxv1alpha1.LoopPhasePaused
			l.Status.DesiredPhase = coxv1alpha1.LoopPhasePaused
			l.Status.PausedFrom = coxv1alpha1.LoopPhaseImplementing
			l.Status.PausedReason = coxv1alpha1.PausedReasonBudget
			if l.Status.Budget == nil {
				l.Status.Budget = &coxv1alpha1.BudgetStatus{}
			}
			l.Status.Budget.PromptTokens = 250
			l.Status.Budget.Exceeded = true
			l.Status.Budget.ExceededReason = coxv1alpha1.BudgetExceededTokens
			Expect(k8sClient.Status().Update(ctx, l)).To(Succeed())
			reconcile(r, ns, "p2g-s10") // re-create the sandbox
			reconcile(r, ns, "p2g-s10")

			// Raise the cap (an I43 same-Loop update) and resume via the
			// annotation.
			l = getLoop(ns, "p2g-s10")
			l.Spec.Budget.MaxTokens = p2gInt64Ptr(500)
			Expect(k8sClient.Update(ctx, l)).To(Succeed())
			l = getLoop(ns, "p2g-s10")
			if l.Annotations == nil {
				l.Annotations = map[string]string{}
			}
			l.Annotations[resumeAnnotation] = unstructuredTrue
			Expect(k8sClient.Update(ctx, l)).To(Succeed())
			reconcile(r, ns, "p2g-s10")
		}

		// driveMeteringReset drives a boot-ID change (the P2d spec 10 shape,
		// re-driven for the sweep): the MeteringReset Warning fires and
		// bootIDChanged is set.
		driveMeteringReset := func(ns string, r *LoopReconciler, _ *record.FakeRecorder) {
			maxTokens := int64(10_000)
			createP2gLoop(ns, "p2g-s10", func(l *coxv1alpha1.Loop) {
				l.Spec.Budget = &coxv1alpha1.BudgetConfig{
					MaxTokens:  &maxTokens,
					OnExceeded: coxv1alpha1.BudgetExceededActionFail,
				}
			})
			seedPhase(getLoop(ns, "p2g-s10"), coxv1alpha1.LoopPhasePlanning)
			primeP2gProxy(getLoop(ns, "p2g-s10"))
			seedBudgetP2g(getLoop(ns, "p2g-s10"), func(b *coxv1alpha1.BudgetStatus) {
				b.LastBootID = "B1"
				b.LastPromptTokens = 100
				b.PromptTokens = 100
			})
			r.readProxyUsage = func(context.Context, *coxv1alpha1.Loop) (proxy.Reading, error) {
				return proxy.Reading{BootID: "B2", PromptTokens: 50, Requests: 1}, nil
			}
			reconcile(r, ns, "p2g-s10")
		}

		cases := []transition{
			{
				name:        "-> Paused (suspend)",
				setup:       driveSuspendPause,
				condType:    coxv1alpha1.PausedCondition,
				condStatus:  metav1.ConditionTrue,
				condReason:  "Paused",
				eventReason: "Paused",
			},
			{
				name:        "-> Paused (stall)",
				setup:       func(ns string, r *LoopReconciler, rec *record.FakeRecorder) { driveStallFire(coxv1alpha1.StallActionPause, ns, r, rec) },
				condType:    coxv1alpha1.PausedCondition,
				condStatus:  metav1.ConditionTrue,
				eventReason: "Paused",
			},
			{
				name:        "-> Paused (budget)",
				setup:       func(ns string, r *LoopReconciler, rec *record.FakeRecorder) { driveBudgetFire(coxv1alpha1.BudgetExceededActionPause, ns, r, rec) },
				condType:    coxv1alpha1.PausedCondition,
				condStatus:  metav1.ConditionTrue,
				eventReason: "Paused",
			},
			{
				name:        "Paused -> phase (resume)",
				setup:       func(ns string, r *LoopReconciler, rec *record.FakeRecorder) { driveSuspendPause(ns, r, rec); setSuspend(ns, "p2g-s10", false); reconcile(r, ns, "p2g-s10") },
				condType:    coxv1alpha1.PausedCondition,
				condStatus:  metav1.ConditionFalse,
				condReason:  "Resumed",
				eventReason: "Resumed",
			},
			{
				name:        "-> Failed:Stalled",
				setup:       func(ns string, r *LoopReconciler, rec *record.FakeRecorder) { driveStallFire(coxv1alpha1.StallActionFail, ns, r, rec) },
				condType:    string(coxv1alpha1.LoopPhaseFailed),
				condStatus:  metav1.ConditionTrue,
				condReason:  "Stalled",
				eventReason: "Stalled",
			},
			{
				name:        "-> Failed:BudgetExceeded",
				setup:       func(ns string, r *LoopReconciler, rec *record.FakeRecorder) { driveBudgetFire(coxv1alpha1.BudgetExceededActionFail, ns, r, rec) },
				condType:    string(coxv1alpha1.LoopPhaseFailed),
				condStatus:  metav1.ConditionTrue,
				condReason:  "BudgetExceeded",
				eventReason: "BudgetExceeded",
			},
			{
				name:        "Stalled (Continue, no phase change)",
				setup:       func(ns string, r *LoopReconciler, rec *record.FakeRecorder) { driveStallFire(coxv1alpha1.StallActionContinue, ns, r, rec) },
				condType:    string(coxv1alpha1.StalledCondition),
				condStatus:  metav1.ConditionTrue,
				eventReason: "Stalled",
			},
			{
				name:        "ClearedOnResume (a raised-cap budget resume)",
				setup:       driveClearedOnResume,
				condType:    coxv1alpha1.BudgetExceededCondition,
				condStatus:  metav1.ConditionFalse,
				condReason:  "ClearedOnResume",
				eventReason: "BudgetExceeded",
			},
			{
				name:        "MeteringReset (the named no-condition anomaly)",
				setup:       driveMeteringReset,
				noCondition: true,
				eventReason: meteringResetReason,
			},
		}

		for i, tc := range cases {
			By(tc.name)
			ns := nsFor("p2g-s10-" + string(rune('a'+i)))
			recorder := record.NewFakeRecorder(64)
			r := newP2gReconciler(recorder)
			tc.setup(ns, r, recorder)
			defer deleteNS(ctx, ns)

			events := drainEvents(recorder)
			// (b) the Event with the right reason and a NON-EMPTY message.
			ev := eventWithReason(events, tc.eventReason)
			Expect(ev).NotTo(BeEmpty(), "%s: an Event with reason %s must be recorded: %v", tc.name, tc.eventReason, events)
			Expect(ev).To(HavePrefix("Reason: "+tc.eventReason+"; "),
				"%s: the event must carry reason %s: %v", tc.name, tc.eventReason, ev)
			msg := ev[len("Reason: "+tc.eventReason+"; "):]
			Expect(msg).NotTo(BeEmpty(), "%s: the %s Event message must be non-empty: %v", tc.name, tc.eventReason, ev)

			if tc.noCondition {
				// The named no-condition anomaly: no condition is expected (the
				// bootIDChanged field is the record, spec 9 pins it).
				continue
			}
			// (a) the condition with the right type/status/reason and a
			// NON-EMPTY message.
			loop := getLoop(ns, "p2g-s10")
			c := condition(loop, tc.condType)
			Expect(c).NotTo(BeNil(), "%s: the %s condition must be set", tc.name, tc.condType)
			Expect(c.Status).To(Equal(tc.condStatus), "%s: the %s condition status must be %s", tc.name, tc.condType, tc.condStatus)
			if tc.condReason != "" {
				Expect(c.Reason).To(Equal(tc.condReason), "%s: the %s condition reason must be %s", tc.name, tc.condType, tc.condReason)
			}
			Expect(c.Message).NotTo(BeEmpty(), "%s: the %s condition message must be non-empty", tc.name, tc.condType)
		}
	})
})

// p2gInt64Ptr returns a *int64 pointing at v (the P2g budget cap values).
func p2gInt64Ptr(v int64) *int64 { return &v }
