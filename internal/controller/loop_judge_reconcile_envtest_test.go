// Copyright 2026 papawattu.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on the License.
// See the License for the specific language governing permissions and
// limitations under the License.

// Phase 7 J1c3 (PLAN-PHASE7.md slice J1, ADR-0011): the Judge RECONCILE
// wiring's envtest specs (loop_judge_job.go ensureJudgeAndSettle).
//
// The fixture drives a fresh Loop (an in-cluster .svc repo, so no egress
// proxy is needed) from Planning to the verify-passed outcome with the Judge
// ENABLED. envtest has no Job controller: the Judge Job's conditions and pod
// statuses are stamped by the spec (the verify Job's s5a fixture pattern).
//
// Spec coverage (the handoff's a–g):
//
//   - (a) judge enabled, verify passes -> Verifying/Judging hold, the Judge
//     Job + NetworkPolicy exist, NOT Succeeded;
//   - (b) Job in progress (no conditions; pod Running; pod Pending) -> still
//     Judging, status.judge nil (one spec per in-progress state, the I49
//     norm: no decision while the Judge runs);
//   - (c) Job Complete + verdict true -> status.judge.verdict true,
//     JudgeVerdict=True, then Succeeded (delivery off);
//   - (d) verdict false + delivery on -> Verifying/Delivering,
//     status.judge.verdict false (the delivery hold takes over after the
//     Judge settles);
//   - (e) unavailable line, garbage line, Job Failed -> JudgeVerdict=Unknown
//     (reason JudgeUnavailable), a Warning Event, and the Loop proceeds;
//   - (f) judge disabled -> unchanged behaviour (the existing P5f specs stay
//     green; a direct no-hold spec here);
//   - (g) budget/pause gates do not fire while Judging (the wall clock does
//     not advance and no Budget condition is set — the loopHeldAfterVerify
//     guard at loop_p2d_budget.go).
package controller

import (
	"context"
	"encoding/json"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	networkingv1 "k8s.io/api/networking/v1"
)

// judgeDriveToJudging drives a fresh Judge-enabled Loop from Planning to the
// verify-passed Judging hold. It returns the reconciler (with a Recorder when
// events are asserted) and the Judge Job's name. The in-cluster .svc repo
// skips the egress-proxy holds (verifyNeedsProxy / deliveryNeedsProxy are
// false for .svc).
func judgeDriveToJudging(ns, name string, withDelivery bool) string {
	ctx := context.Background()
	r := &LoopReconciler{
		CNIProber:         testCNIProber(),
		Client:            k8sClient,
		Scheme:            k8sClient.Scheme(),
		apiReader:         k8sClient,
		MemoryWriterImage: "operator:judge-envtest",
		WorkspaceGitImage: "git:judge-envtest",
		Recorder:          record.NewFakeRecorder(100),
	}
	loop := &coxv1alpha1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: coxv1alpha1.LoopSpec{
			Goal:      loopGoal,
			Workspace: coxv1alpha1.Workspace{Repo: inClusterRepoURL},
			Verify: coxv1alpha1.VerifyConfig{
				AcceptanceChecks: []string{loopCheckCmd},
				Judge:            &coxv1alpha1.JudgeConfig{Enabled: true, MaxTokens: 512},
			},
			Agent: coxv1alpha1.AgentConfig{
				EndpointSecretRef: samplesModelCredSecret,
				Model:             testModel,
				ModelEndpoint:     s3ModelEndpoint,
			},
		},
	}
	if withDelivery {
		loop.Spec.Delivery = &coxv1alpha1.DeliveryConfig{Mode: coxv1alpha1.DeliveryModePullRequest}
	}
	Expect(k8sClient.Create(ctx, loop)).To(Succeed())
	// Bootstrap: Pending -> Planning.
	judgeReconcile(r, ns, name)
	// Pin the base commit (the tamper diff needs it).
	loop = judgeGet(ns, name)
	loop.Status.BaseCommit = s5aBaseCommit
	Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
	// Planning claim.
	s5aClaimPod(ns, name, "Planning", "")
	judgeReconcile(r, ns, name)
	s5aEnsureSandbox(ns, name)
	// Implementing claim WITH the headCommit (pins status.currentVerify,
	// advances to Verifying).
	s5aClaimPod(ns, name, "Implementing", s5aHeadCommit)
	judgeReconcile(r, ns, name)
	// The verify Job's passing evidence (envtest has no Job controller: the
	// fixture pod carries the terminated check inits).
	s5aVerifyPod(ctx, ns, name, name+"-verify-1", 0)
	// The advance-to-Verifying reconcile creates the verify Job; the next
	// reconcile sees the evidence and runs applyVerifyOutcome ->
	// applyVerifySucceeded -> the Judging hold (judgeEnabled) + creates the
	// Judge Job + NetworkPolicy.
	judgeReconcile(r, ns, name)
	loop = judgeReconcile(r, ns, name)
	return judgeJobName(loop)
}

// judgeReconcile runs one full Reconcile and returns the fresh Loop.
func judgeReconcile(r *LoopReconciler, ns, name string) *coxv1alpha1.Loop {
	_, err := r.Reconcile(context.Background(),
		reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
	Expect(err).NotTo(HaveOccurred())
	return judgeGet(ns, name)
}

func judgeGet(ns, name string) *coxv1alpha1.Loop {
	loop := &coxv1alpha1.Loop{}
	Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
	return loop
}

// judgePod creates the stand-in Judge Job pod (the s5aVerifyPod pattern:
// the pod's ownerReference carries the Job's UID — the I65 match). The
// judge container's state is given explicitly; "terminated" with a message
// models the Judge writing its verdict to the termination-message path.
func judgePod(ctx context.Context, ns, name string, job *batchv1.Job, state corev1.ContainerState, phase corev1.PodPhase) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + "-judge-pod",
			Namespace: ns,
			Labels:    map[string]string{s5aJobNameLabel: job.Name, judgeForLabel: name},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: batchv1.SchemeGroupVersion.String(),
				Kind:       i65JobKind,
				Name:       job.Name,
				UID:        job.UID,
			}},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: judgeContainerName, Image: "operator:judge-envtest"}},
		},
	}
	Expect(k8sClient.Create(ctx, pod)).To(Succeed())
	pod.Status.Phase = phase
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: judgeContainerName, State: state}}
	Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
}

// judgeGetJob returns the Judge Job by name (failing on a read error).
func judgeGetJob(ns, jobName string) *batchv1.Job {
	job := &batchv1.Job{}
	Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: jobName, Namespace: ns}, job)).To(Succeed())
	return job
}

// judgeCompleteJob stamps the Judge Job as Complete (the envtest Job status
// validation requires SuccessCriteriaMet=True before Complete=True, plus
// startTime and completionTime — the f73 fixture's pattern).
func judgeCompleteJob(ctx context.Context, job *batchv1.Job) {
	now := metav1.Now()
	job.Status.StartTime = &now
	job.Status.CompletionTime = &now
	job.Status.Succeeded = 1
	job.Status.Conditions = append(job.Status.Conditions,
		batchv1.JobCondition{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, Reason: "CompletionsReached"},
		batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, Reason: "CompletionsReached"})
	Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
}

// judgeFailedJob stamps the Judge Job as Failed (DeadlineExceeded: the
// FailureTarget condition is required before Failed=True — the same envtest
// Job status validation).
func judgeFailedJob(ctx context.Context, job *batchv1.Job) {
	now := metav1.Now()
	job.Status.StartTime = &now
	job.Status.Conditions = append(job.Status.Conditions,
		batchv1.JobCondition{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, Reason: "DeadlineExceeded"},
		batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "DeadlineExceeded"})
	Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
}

// judgeVerdictCond reads the JudgeVerdict condition (nil when absent).
func judgeVerdictCond(loop *coxv1alpha1.Loop) *metav1.Condition {
	for i := range loop.Status.Conditions {
		if loop.Status.Conditions[i].Type == coxv1alpha1.JudgeVerdictCondition {
			c := loop.Status.Conditions[i]
			return &c
		}
	}
	return nil
}

// judgeDrainedEvents drains the FakeRecorder's buffered events.
func judgeDrainedEvents(r *LoopReconciler) []string {
	var out []string
	for {
		select {
		case e := <-r.Recorder.(*record.FakeRecorder).Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

var _ = Describe("J1c3 the Judge reconcile wiring (ADR-0011)", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	// freshNS creates a per-spec namespace (the suite pattern: a unique name
	// per spec so a delete+recreate between specs never races the namespace
	// termination).
	freshNS := func() string {
		ns := "j1c3-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		return ns
	}

	It("(a) verify passed with the Judge enabled holds at Verifying/Judging: the Job + NetPol exist and the Loop is NOT Succeeded", func() {
		ns := freshNS()
		jobName := judgeDriveToJudging(ns, "judge-a", false)
		loop := judgeGet(ns, "judge-a")
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying))
		c := judgeVerifyingCond(loop)
		Expect(c).NotTo(BeNil(), "the Verifying condition must be set")
		Expect(c.Status).To(Equal(metav1.ConditionTrue))
		Expect(c.Reason).To(Equal(coxv1alpha1.ReasonVerifyingJudging))
		Expect(loop.Status.Judge).To(BeNil(), "status.judge is nil while the Judge runs")
		// The Judge Job exists, owned by the Loop.
		job := judgeGetJob(ns, jobName)
		Expect(metav1.IsControlledBy(job, loop)).To(BeTrue())
		// The Judge NetworkPolicy exists, owned by the Loop, denying all.
		netpol := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: judgeNetPolName(loop.Name), Namespace: ns}, netpol)).To(Succeed())
		Expect(metav1.IsControlledBy(netpol, loop)).To(BeTrue())
		Expect(netpol.Spec.PolicyTypes).To(ContainElement(networkingv1.PolicyTypeEgress))
	})

	It("(b1) an in-progress Judge Job (no conditions, no pod) is not a decision: still Judging, status.judge nil", func() {
		ns := freshNS()
		jobName := judgeDriveToJudging(ns, "judge-b1", false)
		// A re-reconcile of the same hold: the Job has neither condition
		// (and no pod) -> no decision, still Judging.
		loop := judgeReconcile(judgeFreshReconciler(), ns, "judge-b1")
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying))
		Expect(judgeVerifyingCond(loop).Reason).To(Equal(coxv1alpha1.ReasonVerifyingJudging))
		Expect(loop.Status.Judge).To(BeNil())
		job := judgeGetJob(ns, jobName)
		Expect(jobTerminalCondition(job, batchv1.JobComplete)).To(BeNil())
		Expect(jobTerminalCondition(job, batchv1.JobFailed)).To(BeNil())
	})

	It("(b2) a Running judge pod is not a decision: still Judging, status.judge nil", func() {
		ns := freshNS()
		jobName := judgeDriveToJudging(ns, "judge-b2", false)
		job := judgeGetJob(ns, jobName)
		judgePod(ctx, ns, "judge-b2", job,
			corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}, corev1.PodRunning)
		// A re-reconcile of the same hold: the pod is Running (neither
		// Job condition is set) -> no decision.
		loop := judgeReconcile(judgeFreshReconciler(), ns, "judge-b2")
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying))
		Expect(judgeVerifyingCond(loop).Reason).To(Equal(coxv1alpha1.ReasonVerifyingJudging))
		Expect(loop.Status.Judge).To(BeNil())
	})

	It("(b3) a Pending judge pod (the diff init still working) is not a decision", func() {
		ns := freshNS()
		jobName := judgeDriveToJudging(ns, "judge-b3", false)
		job := judgeGetJob(ns, jobName)
		judgePod(ctx, ns, "judge-b3", job,
			corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "Init:0"}}, corev1.PodPending)
		loop := judgeReconcile(judgeFreshReconciler(), ns, "judge-b3")
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying))
		Expect(judgeVerifyingCond(loop).Reason).To(Equal(coxv1alpha1.ReasonVerifyingJudging))
		Expect(loop.Status.Judge).To(BeNil())
	})

	It("(c) a Complete Judge Job with a true verdict records status.judge + JudgeVerdict=True, then Succeeded (delivery off)", func() {
		ns := freshNS()
		jobName := judgeDriveToJudging(ns, "judge-c", false)
		job := judgeGetJob(ns, jobName)
		msg := `{"verdict":true,"score":87,"reason":"meets the goal; no gaming"}`
		judgePod(ctx, ns, "judge-c", job,
			corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: msg}},
			corev1.PodSucceeded)
		judgeCompleteJob(ctx, job)
		loop := judgeReconcile(judgeFreshReconciler(), ns, "judge-c")
		// The verdict is recorded.
		Expect(loop.Status.Judge).NotTo(BeNil())
		Expect(loop.Status.Judge.Verdict).NotTo(BeNil())
		Expect(*loop.Status.Judge.Verdict).To(BeTrue())
		Expect(loop.Status.Judge.Score).To(Equal(int32(87)))
		Expect(loop.Status.Judge.Reason).To(Equal("meets the goal; no gaming"))
		Expect(loop.Status.Judge.At).NotTo(BeNil())
		// The JudgeVerdict condition is True (reason Judged).
		vc := judgeVerdictCond(loop)
		Expect(vc).NotTo(BeNil())
		Expect(vc.Status).To(Equal(metav1.ConditionTrue))
		Expect(vc.Reason).To(Equal(judgeVerdictedReason))
		// Delivery is off: the hold releases to Succeeded.
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded))
		Expect(loop.Status.DesiredPhase).To(Equal(coxv1alpha1.LoopPhaseSucceeded))
	})

	It("(d) a false verdict with delivery on moves the hold to Delivering (status.judge false)", func() {
		ns := freshNS()
		jobName := judgeDriveToJudging(ns, "judge-d", true)
		job := judgeGetJob(ns, jobName)
		msg := `{"verdict":false,"score":31,"reason":"the check is stubbed, not fixed"}`
		judgePod(ctx, ns, "judge-d", job,
			corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: msg}},
			corev1.PodSucceeded)
		judgeCompleteJob(ctx, job)
		loop := judgeReconcile(judgeFreshReconciler(), ns, "judge-d")
		Expect(loop.Status.Judge).NotTo(BeNil())
		Expect(*loop.Status.Judge.Verdict).To(BeFalse())
		vc := judgeVerdictCond(loop)
		Expect(vc).NotTo(BeNil())
		Expect(vc.Status).To(Equal(metav1.ConditionFalse))
		// The delivery hold takes over (P5f): still Verifying, reason
		// Delivering — the deliver Job (and deliverDraft's read of
		// status.judge) runs before Succeeded.
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying))
		Expect(loopHoldingForDelivery(loop)).To(BeTrue())
		Expect(judgeVerifyingCond(loop).Reason).To(Equal(coxv1alpha1.ReasonVerifyingDelivering))
	})

	It("(e1) an unavailable line gives JudgeVerdict=Unknown/JudgeUnavailable, a Warning Event, and the Loop proceeds", func() {
		ns := freshNS()
		jobName := judgeDriveToJudging(ns, "judge-e1", false)
		r := judgeFreshReconciler()
		job := judgeGetJob(ns, jobName)
		msg := `{"unavailable":"model proxy refused the request"}`
		judgePod(ctx, ns, "judge-e1", job,
			corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: msg}},
			corev1.PodSucceeded)
		judgeCompleteJob(ctx, job)
		loop := judgeReconcile(r, ns, "judge-e1")
		Expect(loop.Status.Judge).NotTo(BeNil())
		Expect(loop.Status.Judge.Verdict).To(BeNil(), "unavailable is never a pass or a fail (D124)")
		Expect(loop.Status.Judge.Reason).To(Equal("model proxy refused the request"),
			"the unavailable line records the Judge's own cause, not a 'malformed' marker (P3a)")
		vc := judgeVerdictCond(loop)
		Expect(vc).NotTo(BeNil())
		Expect(vc.Status).To(Equal(metav1.ConditionUnknown))
		Expect(vc.Reason).To(Equal(coxv1alpha1.ReasonJudgeUnavailable))
		Expect(judgeDrainedEvents(r)).To(ContainElement(And(
			ContainSubstring(corev1.EventTypeWarning),
			ContainSubstring(coxv1alpha1.ReasonJudgeUnavailable),
		)))
		// The Loop proceeds (delivery off: Succeeded).
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded))
	})

	It("(e2) a garbage (unparsable) termination line is JudgeUnavailable, never a verdict", func() {
		ns := freshNS()
		jobName := judgeDriveToJudging(ns, "judge-e2", false)
		r := judgeFreshReconciler()
		job := judgeGetJob(ns, jobName)
		judgePod(ctx, ns, "judge-e2", job,
			corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: "not json at all"}},
			corev1.PodSucceeded)
		judgeCompleteJob(ctx, job)
		loop := judgeReconcile(r, ns, "judge-e2")
		Expect(loop.Status.Judge.Verdict).To(BeNil())
		vc := judgeVerdictCond(loop)
		Expect(vc.Status).To(Equal(metav1.ConditionUnknown))
		Expect(vc.Reason).To(Equal(coxv1alpha1.ReasonJudgeUnavailable))
		Expect(judgeDrainedEvents(r)).To(ContainElement(And(
			ContainSubstring(corev1.EventTypeWarning),
			ContainSubstring("malformed"),
		)))
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded))
	})

	It("(e4) a verdict with a score outside 0..100 is JudgeUnavailable, never a clamped record", func() {
		ns := freshNS()
		jobName := judgeDriveToJudging(ns, "judge-e4", false)
		r := judgeFreshReconciler()
		job := judgeGetJob(ns, jobName)
		judgePod(ctx, ns, "judge-e4", job,
			corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 0,
				Message:  `{"verdict":true,"score":140,"reason":"confidence out of range"}`,
			}},
			corev1.PodSucceeded)
		judgeCompleteJob(ctx, job)
		loop := judgeReconcile(r, ns, "judge-e4")
		Expect(loop.Status.Judge).NotTo(BeNil())
		Expect(loop.Status.Judge.Verdict).To(BeNil(), "a bad score is never a clamped record")
		Expect(loop.Status.Judge.Reason).To(ContainSubstring("score out of range"))
		vc := judgeVerdictCond(loop)
		Expect(vc.Status).To(Equal(metav1.ConditionUnknown))
		Expect(vc.Reason).To(Equal(coxv1alpha1.ReasonJudgeUnavailable))
		Expect(judgeDrainedEvents(r)).To(ContainElement(And(
			ContainSubstring(corev1.EventTypeWarning),
			ContainSubstring("score out of range"),
		)))
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded))
	})

	It("(e3) a Failed Judge Job (DeadlineExceeded) is JudgeUnavailable and the Loop proceeds", func() {
		ns := freshNS()
		jobName := judgeDriveToJudging(ns, "judge-e3", false)
		r := judgeFreshReconciler()
		job := judgeGetJob(ns, jobName)
		judgeFailedJob(ctx, job)
		loop := judgeReconcile(r, ns, "judge-e3")
		Expect(loop.Status.Judge).NotTo(BeNil())
		Expect(loop.Status.Judge.Verdict).To(BeNil())
		Expect(loop.Status.Judge.Reason).To(ContainSubstring("DeadlineExceeded"))
		vc := judgeVerdictCond(loop)
		Expect(vc.Status).To(Equal(metav1.ConditionUnknown))
		Expect(vc.Reason).To(Equal(coxv1alpha1.ReasonJudgeUnavailable))
		Expect(judgeDrainedEvents(r)).To(ContainElement(And(
			ContainSubstring(corev1.EventTypeWarning),
			ContainSubstring(coxv1alpha1.ReasonJudgeUnavailable),
		)))
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded))
	})

	It("(f) a verify pass with the Judge disabled behaves exactly as before (no hold, no Judge Job)", func() {
		// Drive to Verifying with the Judge DISABLED.
		ns := freshNS()
		r := &LoopReconciler{CNIProber: testCNIProber(), Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: "judge-f", Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      loopGoal,
				Workspace: coxv1alpha1.Workspace{Repo: inClusterRepoURL},
				Verify: coxv1alpha1.VerifyConfig{
					AcceptanceChecks: []string{loopCheckCmd},
				},
				Agent: coxv1alpha1.AgentConfig{EndpointSecretRef: samplesModelCredSecret, Model: testModel, ModelEndpoint: s3ModelEndpoint},
			},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		judgeReconcile(r, ns, "judge-f")
		loop = judgeGet(ns, "judge-f")
		loop.Status.BaseCommit = s5aBaseCommit
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		s5aClaimPod(ns, "judge-f", "Planning", "")
		judgeReconcile(r, ns, "judge-f")
		s5aEnsureSandbox(ns, "judge-f")
		s5aClaimPod(ns, "judge-f", "Implementing", s5aHeadCommit)
		judgeReconcile(r, ns, "judge-f")
		s5aVerifyPod(ctx, ns, "judge-f", "judge-f-verify-1", 0)
		judgeReconcile(r, ns, "judge-f")
		loop = judgeReconcile(r, ns, "judge-f")
		// Judge off + delivery off: straight to Succeeded (the J1c3 hold is
		// absent — unchanged behaviour).
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded))
		job := &batchv1.Job{}
		err := k8sClient.Get(ctx, types.NamespacedName{Name: judgeJobName(loop), Namespace: ns}, job)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no Judge Job when the Judge is disabled")
	})

	It("(g) the budget and pause gates do not fire while the Judging hold is active", func() {
		ns := freshNS()
		_ = judgeDriveToJudging(ns, "judge-g", false)
		// The wall clock: an active-stamped budget must NOT advance while
		// Judging (loopHeldAfterVerify guards the stamp at loop_p2d_budget.go)
		// and no Budget condition may be set (the budget decision is guarded
		// the same way).
		loop := judgeGet(ns, "judge-g")
		loop.Status.Phase = coxv1alpha1.LoopPhaseVerifying
		judgeVerifyingSetReason(loop, coxv1alpha1.ReasonVerifyingJudging, "the fixture's Judging hold")
		t0 := time.Now().Add(-120 * time.Second).Truncate(time.Second)
		stamp := metav1.NewTime(t0)
		loop.Status.Budget = &coxv1alpha1.BudgetStatus{ActiveSeconds: 10, LastActiveStamp: &stamp}
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		r := &LoopReconciler{CNIProber: testCNIProber(), Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
		// Budget: the wall clock must NOT advance while the Judging hold is
		// active (loopHeldAfterVerify guards the stamp at loop_p2d_budget.go)
		// and the budget decision must not fire (same guard at
		// loop_p2d_budget.go:313).
		r.applyBudget(ctx, judgeGet(ns, "judge-g"))
		loop = judgeGet(ns, "judge-g")
		Expect(loop.Status.Budget.ActiveSeconds).To(Equal(int64(10)),
			"the wall clock must not advance while the Judging hold is active (m3's guard)")
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"the budget must not advance a Judging-held Loop to Paused")
		// Pause: the Verifying-held (Judging) Loop must not be treated as
		// running (the guard at loop_p2f_paused.go:160 — the hold means the
		// sandbox is not operating, so a sandbox-liveness stall decision
		// cannot fire).
		Expect(r.loopSandboxStopped(judgeGet(ns, "judge-g"))).To(BeTrue(),
			"loopHeldAfterVerify must cover the Judging hold (m3's guard)")
	})
})

// judgeVerifyingCond reads the Verifying condition (nil when absent).
func judgeVerifyingCond(loop *coxv1alpha1.Loop) *metav1.Condition {
	for i := range loop.Status.Conditions {
		if loop.Status.Conditions[i].Type == string(coxv1alpha1.LoopPhaseVerifying) {
			c := loop.Status.Conditions[i]
			return &c
		}
	}
	return nil
}

// judgeVerifyingSetReason sets (or updates) the Verifying condition to the
// given reason (the spec's fixture point — the production writer is
// applyVerifySucceeded).
func judgeVerifyingSetReason(loop *coxv1alpha1.Loop, reason, message string) {
	for i := range loop.Status.Conditions {
		if loop.Status.Conditions[i].Type == string(coxv1alpha1.LoopPhaseVerifying) {
			loop.Status.Conditions[i].Status = metav1.ConditionTrue
			loop.Status.Conditions[i].Reason = reason
			loop.Status.Conditions[i].Message = message
			return
		}
	}
	loop.Status.Conditions = append(loop.Status.Conditions, metav1.Condition{
		Type:               string(coxv1alpha1.LoopPhaseVerifying),
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	})
}

// judgeFreshReconciler builds a fresh reconciler over the same envtest client
// (a Reconcile run needs the Client/Scheme/apiReader wiring; the Recorder is
// included so the event specs can drain it).
func judgeFreshReconciler() *LoopReconciler {
	return &LoopReconciler{
		CNIProber:         testCNIProber(),
		Client:            k8sClient,
		Scheme:            k8sClient.Scheme(),
		apiReader:         k8sClient,
		MemoryWriterImage: "operator:judge-envtest",
		WorkspaceGitImage: "git:judge-envtest",
		Recorder:          record.NewFakeRecorder(100),
	}
}

// judgeUnusedJSON keeps the json import honest when the specs drop a marshaling
// helper (the unused-import guard for the fixed-shape contract).
var _ = json.Marshal
