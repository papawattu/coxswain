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
// gate mutation (drop one Event from its transition site) is run in a scratch
// worktree (I49 norm) and recorded in .samples/p2g/mutations.md.
//
// The event matcher: the FakeRecorder formats each event as
// "<type> <reason> <message>" (e.g. "Normal Paused paused: source suspend...").
// eventWithReason returns the message part for the given reason.
package controller

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	ctrl "sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/cni"
	"github.com/papawattu/coxswain/internal/proxy"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
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
	// stallDetectedEvent is the Warning Event reason the stall detector fires
	// (the P2e name; NOT "Stalled" — that is the condition's reason).
	stallDetectedEvent = "StallDetected"
	// p2gProxyPodIP is the proxy pod's PodIP the D35a gate reads (a valid
	// in-cluster address; envtest has no kubelet to assign one).
	p2gProxyPodIP = "127.0.0.1"
	// p2gManagedByLabel / p2gLoopLabel / p2gComponentLabel are the proxy pod's
	// label keys (the D33 shape; shared with the P2d/P2e fixtures).
	p2gManagedByLabel = "app.kubernetes.io/managed-by"
	p2gLoopLabel      = "app.kubernetes.io/loop"
	p2gComponentLabel = "app.kubernetes.io/component"
	// p2gJobNameLabel is the Job pod's job-name label key (the S5a constant).
	p2gJobNameLabel = "job-name"
)

// P2gDebug is the per-spec debug sink the P2g specs print on failure (the
// reconcile's V(1) log lines, which are otherwise invisible in the test
// output).
type P2gDebug struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func newP2gDebug() *P2gDebug { return &P2gDebug{} }

func (d *P2gDebug) Printf(format string, args ...any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	fmt.Fprintf(&d.buf, format, args...)
}

func (d *P2gDebug) String() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.buf.String()
}

var _ = Describe("P2g: conditions + events for every P2 transition (the auditability sweep)", func() {
	ctx := context.Background()
	// p2gDebug is the per-spec debug sink (temporary while the stall drive is
	// being debugged; the specs print it on failure).
	p2gDebug := newP2gDebug()

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
				Verify: coxv1alpha1.VerifyConfig{
					AcceptanceChecks: []string{"p2g-check-0"},
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

	// primeP2gProxy creates the model proxy the D35a gate requires. The
	// bootstrap reconcile CREATES the controller's own proxy pod (with the
	// controller owner ref — IsControlledBy needs the UID, which a hand-built
	// pod would lack and which would leave the sandbox held Suspended by the
	// D35a gate); this then marks it Ready (the gate's remaining input) and
	// creates the secret.
	primeP2gProxy := func(loop *coxv1alpha1.Loop) {
		_ = k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: p2gModelSecret, Namespace: loop.Namespace},
			StringData: map[string]string{modelAPIKey: "p2g-dummy", modelBaseURL: p2gModelEndpoint},
		})
		// Create the proxy Pod DIRECTLY (the P2d fixture shape): the operator's
		// ensureProxy would also create a ClusterIP Service and exhaust the
		// envtest ServiceIP pool; the D35a gate checks the pod, and the
		// readProxyUsage seam stands in for the pod's :9090 endpoint.
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      proxyPodName(loop.Name),
				Namespace: loop.Namespace,
				Labels: map[string]string{
					p2gManagedByLabel: partOfCoxswain,
					p2gLoopLabel:      loop.Name,
					p2gComponentLabel: p2dModelProxyComponent,
				},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: p2dProxyComponent, Image: p2dProxyComponent}},
			},
		}
		if ownerErr := controllerutil.SetControllerReference(loop, pod, k8sClient.Scheme()); ownerErr != nil {
			Expect(ownerErr).NotTo(HaveOccurred())
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		// Mark the pod Ready (the D35a gate checks the pod's conditions).
		got := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: loop.Namespace, Name: proxyPodName(loop.Name)}, got)).To(Succeed())
		now := metav1.Now()
		got.Status.Conditions = []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: now},
		}
		got.Status.PodIP = p2gProxyPodIP
		Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())
	}

	// p2gDebugLog appends to the per-spec debug sink (temporary while the
	// stall drive is being debugged).
	p2gDebugLog := func(format string, args ...any) { p2gDebug.Printf(format, args...) }

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
	_ = aP2gFailingPod // seam helper retained for direct applyStallGate tests

	// runStallFailures drives the FULL Reconcile at Verifying for iterations
	// [1..n]: each iteration a fresh verify pod (job-name <loop>-verify-<i>)
	// with check-0 failed (exit 1, the check output teed into the
	// terminationMessage — the default read path), re-seeded phase Verifying
	// + iteration i, and one reconcile. applyVerifyOutcome reads the pod (the
	// real read path) and the stall gate fires on the Nth identical failure.
	// It returns the post-fire phase ("" when the gate never fired).
	runStallFailures := func(r *LoopReconciler, ns string, n int, loopName string) string {
		var phase coxv1alpha1.LoopPhase
		for iter := 1; iter <= n; iter++ {
			jobName := loopName + "-verify-" + strconv.Itoa(iter)
			// Replace this iteration's verify pod (fresh name: the read path
			// filters to the current iteration's Job pod).
			oldPod := &corev1.Pod{}
			if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: jobName}, oldPod); err == nil {
				_ = k8sClient.Delete(ctx, oldPod)
			}
			fin := metav1.Now()
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      jobName,
					Namespace: ns,
					Labels: map[string]string{
						verifyForLabel:  loopName,
						p2gJobNameLabel: jobName,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "checks", Image: verifyBusybox}},
				},
				Status: corev1.PodStatus{
					InitContainerStatuses: []corev1.ContainerStatus{
						{Name: verifyTamperInit, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
						{Name: verifyArtifactInit, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
						{Name: s5aCheck0, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, FinishedAt: fin, Message: p2gOutputRepeated}}},
					},
				},
			}
			// The API server drops status on create: create the pod, then set
			// its status via the status subresource (the
			// loop_p2e_stall_plan_test.go shape — without this the pod has no
			// initContainerStatuses and verifyOutcome returns no-decision, so
			// the stall gate never fires).
			st := pod.Status
			pod.Status = corev1.PodStatus{}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			pod.Status = st
			Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
			By(fmt.Sprintf("debug: created verify pod %s", jobName))
			p2gDebugLog("iter %d: phase=%s before reconcile\n", iter, getLoop(ns, loopName).Status.Phase)
			// Re-seed the phase + iteration (the previous reconcile may have
			// moved the phase: the Continue action iterates back to
			// Implementing).
			loop := getLoop(ns, loopName)
			loop.Status.Phase = coxv1alpha1.LoopPhaseVerifying
			loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseVerifying
			loop.Status.Iteration = iter
			// The pin must name the current iteration's Job (the read path
			// looks up <loop>-verify-<iteration>); the base commit is already
			// set by createP2gLoop.
			loop.Status.CurrentVerify = &coxv1alpha1.CurrentVerifyStatus{VerifiedCommit: p2gHeadCommit}
			Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
			// The bootstrap reconcile (Planning) left the sandbox with a
			// stale annotation; the next reconcile's phaseBootstrap deletes it
			// and RETURNS before the verify outcome step runs. Patch the
			// annotation to the current desired phase BEFORE the reconcile so
			// no recycle is pending and ONE reconcile runs the full pass
			// (the D38 pattern — the advance is never skipped).
			sb := &sandboxv1beta1.Sandbox{}
			if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: sandboxName(loopName)}, sb); err == nil {
				if sb.Annotations == nil {
					sb.Annotations = map[string]string{}
				}
				sb.Annotations[sandboxDesiredPhaseAnnotation] = string(coxv1alpha1.LoopPhaseVerifying)
				Expect(k8sClient.Update(ctx, sb)).To(Succeed())
			}
			// Create the verify Job (the S5a envtest shape: envtest has no
			// Job controller, so the operator's ensureVerifyJob is the Job's
			// only path — the pod is then read from it via the verify-for
			// label + job-name label, as the S5a specs do).
			if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: jobName}, &batchv1.Job{}); apierrors.IsNotFound(err) {
				job := &batchv1.Job{
					ObjectMeta: metav1.ObjectMeta{
						Name:      jobName,
						Namespace: ns,
						Labels:    verifyJobLabels(loopName),
					},
					Spec: batchv1.JobSpec{
						Template: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{
									verifyForLabel:  loopName,
									p2gJobNameLabel: jobName,
								},
							},
							Spec: corev1.PodSpec{
								RestartPolicy: corev1.RestartPolicyNever,
								Containers:    []corev1.Container{{Name: verifyNoopContainer, Image: verifyBusybox}},
							},
						},
					},
				}
				Expect(controllerutil.SetControllerReference(getLoop(ns, loopName), job, k8sClient.Scheme())).To(Succeed())
				Expect(k8sClient.Create(ctx, job)).To(Succeed())
			}
			reconcile(r, ns, loopName) // ensureVerifyJob sees the Job; the pod is read from it
			l := getLoop(ns, loopName)
			p2gDebugLog("iter %d: phase=%s stallHistory=%d\n", iter, l.Status.Phase, len(l.Status.StallHistory))
			phase = l.Status.Phase
		}
		return string(phase)
	}

	// eventWithReason returns the message of a recorded event with the given
	// reason ("" when none). The FakeRecorder formats "<type> <reason> <msg>",
	// so the message follows the type+reason tokens.
	eventWithReason := func(events []string, reason string) string {
		for _, e := range events {
			fields := strings.SplitN(e, " ", 3)
			if len(fields) == 3 && fields[1] == reason {
				return fields[2]
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
		createP2gLoop(ns, "p2g-s1", func(l *coxv1alpha1.Loop) { l.Spec.Suspend = true })
		reconcile(r, ns, "p2g-s1") // bootstrap (creates the controller's proxy pod + enters Paused)

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
		createP2gLoop(ns, "p2g-stall", func(l *coxv1alpha1.Loop) {
			l.Spec.Loop = coxv1alpha1.LoopSettings{
				StallAfter:  &stallAfter,
				StallAction: coxv1alpha1.StallActionPause,
			}
		})
		reconcile(r, ns, "p2g-stall") // bootstrap (creates the controller's proxy pod)
		phase := runStallFailures(r, ns, 3, "p2g-stall")
		Expect(phase).To(Equal(string(coxv1alpha1.LoopPhasePaused)), "stallAction=Pause fires at N=3: %v\n%v", phase, p2gDebug.String())

		l := getLoop(ns, "p2g-stall")
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

	It("spec 5: -> Failed:Stalled (stall Fail): the Failed condition is reason Stalled; the Stalled condition is True; the StallDetected Event fires", func() {
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
		reconcile(r, ns, "p2g-stall") // bootstrap (creates the controller's proxy pod)
		phase := runStallFailures(r, ns, 3, "p2g-stall")
		Expect(phase).To(Equal(string(coxv1alpha1.LoopPhaseFailed)), "stallAction=Fail fires at N=3: %v\n%v", phase, p2gDebug.String())

		l := getLoop(ns, "p2g-stall")
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
		stalled := eventWithReason(events, stallDetectedEvent)
		Expect(stalled).NotTo(BeEmpty(), "a StallDetected Event must be recorded (the detector's record): %v", events)
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

	It("spec 7: stall Continue (no phase change): the Stalled condition is True; the StallDetected Event fires; the phase is unchanged", func() {
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
		reconcile(r, ns, "p2g-stall") // bootstrap (creates the controller's proxy pod)
		phase := runStallFailures(r, ns, 3, "p2g-stall")
		Expect(phase).ToNot(Equal(string(coxv1alpha1.LoopPhaseFailed)), "stallAction=Continue keeps the loop (no phase change): %v\n%v", phase, p2gDebug.String())
		Expect(phase).ToNot(Equal(string(coxv1alpha1.LoopPhasePaused)))
		l := getLoop(ns, "p2g-stall")
		sc := condition(l, string(coxv1alpha1.StalledCondition))
		Expect(sc).NotTo(BeNil(), "the Stalled condition records the state even though the phase is unchanged")
		Expect(sc.Status).To(Equal(metav1.ConditionTrue))
		Expect(sc.Message).NotTo(BeEmpty())
		events := drainEvents(recorder)
		stalled := eventWithReason(events, stallDetectedEvent)
		Expect(stalled).NotTo(BeEmpty(), "a StallDetected Event must be recorded: %v", events)
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
		cap500 := int64(500)
		l.Spec.Budget.MaxTokens = &cap500
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
		cleared := eventWithReason(events, coxv1alpha1.BudgetExceededCondition)
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
		Expect(l.Status.Phase).ToNot(Equal(coxv1alpha1.LoopPhaseFailed), "no cap is hit: no phase change")
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

		driveSuspendPause := func(ns string, r *LoopReconciler, _ *record.FakeRecorder) {
			createP2gLoop(ns, "p2g-s10", nil)
			primeP2gProxy(getLoop(ns, "p2g-s10"))
			reconcile(r, ns, "p2g-s10")
			seedPhase(getLoop(ns, "p2g-s10"), coxv1alpha1.LoopPhaseImplementing)
			reconcile(r, ns, "p2g-s10")
			setSuspend(ns, "p2g-s10", true)
			reconcile(r, ns, "p2g-s10")
		}

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
			runStallFailures(r, ns, 3, "p2g-stall")
		}

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
			cap500 := int64(500)
			l.Spec.Budget.MaxTokens = &cap500
			Expect(k8sClient.Update(ctx, l)).To(Succeed())
			l = getLoop(ns, "p2g-s10")
			if l.Annotations == nil {
				l.Annotations = map[string]string{}
			}
			l.Annotations[resumeAnnotation] = unstructuredTrue
			Expect(k8sClient.Update(ctx, l)).To(Succeed())
			reconcile(r, ns, "p2g-s10")
		}

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
				condReason:  pausedCondReasonPaused,
				eventReason: pauseEventReason,
			},
			{
				name: "-> Paused (stall)",
				setup: func(ns string, r *LoopReconciler, rec *record.FakeRecorder) {
					driveStallFire(coxv1alpha1.StallActionPause, ns, r, rec)
				},
				condType:    coxv1alpha1.PausedCondition,
				condStatus:  metav1.ConditionTrue,
				eventReason: pauseEventReason,
			},
			{
				name: "-> Paused (budget)",
				setup: func(ns string, r *LoopReconciler, rec *record.FakeRecorder) {
					driveBudgetFire(coxv1alpha1.BudgetExceededActionPause, ns, r, rec)
				},
				condType:    coxv1alpha1.PausedCondition,
				condStatus:  metav1.ConditionTrue,
				eventReason: pauseEventReason,
			},
			{
				name: "Paused -> phase (resume)",
				setup: func(ns string, r *LoopReconciler, rec *record.FakeRecorder) {
					driveSuspendPause(ns, r, rec)
					setSuspend(ns, "p2g-s10", false)
					reconcile(r, ns, "p2g-s10")
				},
				condType:    coxv1alpha1.PausedCondition,
				condStatus:  metav1.ConditionFalse,
				condReason:  "Resumed",
				eventReason: "Resumed",
			},
			{
				name: "-> Failed:Stalled",
				setup: func(ns string, r *LoopReconciler, rec *record.FakeRecorder) {
					driveStallFire(coxv1alpha1.StallActionFail, ns, r, rec)
				},
				condType:    string(coxv1alpha1.LoopPhaseFailed),
				condStatus:  metav1.ConditionTrue,
				condReason:  "Stalled",
				eventReason: stallDetectedEvent,
			},
			{
				name: "-> Failed:BudgetExceeded",
				setup: func(ns string, r *LoopReconciler, rec *record.FakeRecorder) {
					driveBudgetFire(coxv1alpha1.BudgetExceededActionFail, ns, r, rec)
				},
				condType:    string(coxv1alpha1.LoopPhaseFailed),
				condStatus:  metav1.ConditionTrue,
				condReason:  "BudgetExceeded",
				eventReason: "BudgetExceeded",
			},
			{
				name: "Stalled (Continue, no phase change)",
				setup: func(ns string, r *LoopReconciler, rec *record.FakeRecorder) {
					driveStallFire(coxv1alpha1.StallActionContinue, ns, r, rec)
				},
				condType:    string(coxv1alpha1.StalledCondition),
				condStatus:  metav1.ConditionTrue,
				eventReason: stallDetectedEvent,
			},
			{
				name:        "ClearedOnResume (a raised-cap budget resume)",
				setup:       driveClearedOnResume,
				condType:    coxv1alpha1.BudgetExceededCondition,
				condStatus:  metav1.ConditionFalse,
				condReason:  "ClearedOnResume",
				eventReason: coxv1alpha1.BudgetExceededCondition,
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

			if tc.noCondition {
				// The named no-condition anomaly: no condition is expected (the
				// bootIDChanged field is the record, spec 9 pins it).
				continue
			}
			// (a) the condition with the right type/status/reason and a
			// NON-EMPTY message.
			// The stall drives use the p2g-stall loop name (the stall fixture's
			// shape); every other drive uses p2g-s10.
			loopName := "p2g-s10"
			if strings.Contains(tc.name, "stall") || strings.Contains(tc.name, "Stalled") {
				loopName = "p2g-stall"
			}
			loop := getLoop(ns, loopName)
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
