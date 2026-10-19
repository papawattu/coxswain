package controller

// loop_f6_agent_evicted_envtest_test.go is the F6 (dogfood finding) envtest
// spec. F6 is the P1: the operator notices a sandbox pod that failed
// WITHOUT a phase claim — the agent container terminated (OOMKilled,
// Evicted, or a non-zero exit) before it could write a claim — and acts on
// it. Observed live on the dogfood chartcheck: the agent was OOM-killed for
// the /tmp emptyDir sizeLimit overrun, the pod went Error, and the Loop sat
// stuck in Planning forever: no condition, no Event, a claim reader that
// re-queued forever. The operator never noticed, never surfaced, never
// retried.
//
// The fix (internal/controller/loop_s4_phase.go, advancePhaseFromClaim's
// F6 check) mirrors the I65 VerifyInfraFailed infra-retry pattern onto the
// sandbox pod. The F6 check runs at the TOP of advancePhaseFromClaim,
// BEFORE the claim read and the Verifying hold — so it fires in ANY
// non-terminal phase.
//
// This spec tests the F6 functions directly (sandboxFailure + applySandbox
// Failure) rather than through the full Reconcile (which hangs on unrelated
// steps in the bare envtest). The kind proof (dogfood/dogfood-chartcheck)
// covers the full reconcile path.

import (
	"context"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"

	cov "github.com/papawattu/coxswain/api/v1alpha1"
	cni "github.com/papawattu/coxswain/internal/cni"
	policy "github.com/papawattu/coxswain/internal/policy"
)

var _ = Describe("F6: a sandbox pod that failed without a claim is surfaced and retried (bounded)", func() {
	var vscOnce sync.Once
	f6EnsureVSC := func() {
		vscOnce.Do(func() {
			vsc := &unstructured.Unstructured{}
			vsc.SetAPIVersion("snapshot.storage.k8s.io/v1")
			vsc.SetKind("VolumeSnapshotClass")
			vsc.SetName("f6-test-vsc")
			_ = unstructured.SetNestedField(vsc.Object, "test.driver", "driver")
			_ = unstructured.SetNestedField(vsc.Object, "Delete", "deletionPolicy")
			if err := k8sClient.Create(ctx, vsc); err != nil && !strings.Contains(err.Error(), "already exists") {
				Expect(err).ToNot(HaveOccurred(), "plant the f6-test-vsc VolumeSnapshotClass")
			}
		})
	}

	// f6NewReconciler creates a bare reconciler for the F6 functions.
	f6NewReconciler := func(ns, name string) *LoopReconciler {
		f6EnsureVSC()
		if err := k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		}); err != nil && !strings.Contains(err.Error(), "already exists") {
			Expect(err).ToNot(HaveOccurred())
		}
		rec := record.NewFakeRecorder(256)
		r := &LoopReconciler{
			Client:             k8sClient,
			Scheme:             k8sClient.Scheme(),
			Enforcer:           &f6FakeEnforcer{},
			CNIProber:          &cni.FakeProber{NextResult: cni.CNIProbeResult{Reason: cni.ReasonCNIEnforced}},
			Recorder:           rec,
			SandboxImage:       "example.com/cox-sandbox:v1",
			ProxyImage:         "example.com/cox-proxy:v1",
			EgressProxyImage:   "example.com/egress-proxy:v1",
			ToolProxyImage:     "example.com/tool-proxy:v1",
			WorkspaceGitImage:  "example.com/git:v1",
			snapshotCRDPresent: func(context.Context) bool { return true },
			workspaceDriverSeam: func(context.Context, *cov.Loop) (string, bool) {
				return "test.driver", true
			},
		}
		loop := &cov.Loop{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID(name + "-loop-uid")},
			Spec: cov.LoopSpec{
				Goal:      "F6 test",
				Agent:     cov.AgentConfig{Image: "example.com/cox-agent:v1"},
				Workspace: cov.Workspace{Repo: "https://example.com/repo.git"},
			},
			Status: cov.LoopStatus{
				Phase: cov.LoopPhasePlanning,
			},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		return r
	}

	It("surfaces an evicted pod with no claim (the live F6 repro)", func() {
		const ns, name = "f6-evict-ns", "f6-evict"
		r := f6NewReconciler(ns, name)
		f6PlantSandboxAndPodFn(ns, name)
		By("sandboxFailure detects the evicted pod")
		loop := &cov.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, loop)).To(Succeed())
		f, err := r.sandboxFailure(ctx, loop)
		Expect(err).ToNot(HaveOccurred())
		Expect(f).ToNot(BeNil(), "sandboxFailure must detect the evicted pod")
		Expect(f.reason).To(Equal("Evicted"))
		Expect(f.message).To(ContainSubstring("EmptyDir volume tmp"))
		By("applySandboxFailure surfaces the failure (I90): condition + Event + attempt count")
		changed := r.applySandboxFailure(ctx, loop, f)
		Expect(changed).To(BeTrue())
		cond := findConditionFn(loop.Status.Conditions, cov.AgentFailedCondition)
		Expect(cond).ToNot(BeNil(), "AgentFailed condition must be set")
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal("Evicted"))
		Expect(cond.Message).To(ContainSubstring("EmptyDir volume tmp"))
		Expect(loop.Status.Agent).ToNot(BeNil())
		Expect(loop.Status.Agent.InfraAttempts).To(Equal(int32(1)))
		By("a Warning Event is emitted")
		var events []string
		for {
			select {
			case e := <-r.Recorder.(*record.FakeRecorder).Events:
				events = append(events, e)
			case <-time.After(2 * time.Second):
				// Drain complete (the FakeRecorder is non-blocking; no more events).
				goto drainDone
			}
		}
	drainDone:
		found := false
		for _, e := range events {
			if strings.Contains(e, "SandboxRetry") || strings.Contains(e, "SandboxFailed") {
				found = true
			}
		}
		Expect(found).To(BeTrue(), "a Warning Event must be emitted")
		By("the Sandbox is deleted (the retry)")
		sb := &sandboxv1beta1.Sandbox{}
		err = k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name + "-sandbox"}, sb)
		Expect(err).To(HaveOccurred(), "the Sandbox must be deleted (the retry)")
	})

	It("fails the Loop after sandboxInfraRetries retries (the bound is exhausted)", func() {
		const ns, name = "f6-exhaust-ns", "f6-exhaust"
		r := f6NewReconciler(ns, name)
		loop := &cov.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, loop)).To(Succeed())
		// Simulate 2 prior retries (attempts 1 and 2).
		loop.Status.Agent = &cov.AgentStatus{
			InfraAttempts:   2,
			InfraSandboxUID: "prior-uid",
		}
		// Plant a NEW sandbox + failed pod (a different UID).
		f6PlantSandboxAndPodFn(ns, name)
		By("sandboxFailure detects the evicted pod (new UID)")
		f, err := r.sandboxFailure(ctx, loop)
		Expect(err).ToNot(HaveOccurred())
		Expect(f).ToNot(BeNil())
		By("applySandboxFailure fails the Loop (the bound is exhausted)")
		changed := r.applySandboxFailure(ctx, loop, f)
		Expect(changed).To(BeTrue())
		Expect(loop.Status.Phase).To(Equal(cov.LoopPhaseFailed))
		cond := findConditionFn(loop.Status.Conditions, string(cov.LoopPhaseFailed))
		Expect(cond).ToNot(BeNil(), "the Failed condition must be set")
		Expect(cond.Reason).To(Equal(sandboxInfraFailedCondition))
	})

	// The F6 review follow-up (P1): the condition is CLEARED to
	// False/Recovered once a fresh claim arrives (the agent recovered from a
	// prior sandbox failure). The dogfood kind run observed AgentFailed=True
	// sticking on a healthy Loop at AwaitingApproval — the condition was set
	// by the F6 check but never cleared once the fresh pod claimed.
	It("clears the AgentFailed condition to False/Recovered on a fresh claim (the recovery)", func() {
		const ns, name = "f6-recover-ns", "f6-recover"
		r := f6NewReconciler(ns, name)
		// Plant the failing sandbox + Evicted pod (the original F6 scenario).
		f6PlantSandboxAndPodFn(ns, name)
		loop := &cov.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, loop)).To(Succeed())
		// The F6 check runs on the real path: it SURFACES the failure (True +
		// Evicted + the kubelet message) and deletes the failing sandbox (the
		// retry). The condition is in-memory here — the kind run is the proof
		// the persistence lands; this spec proves the clear path on the NEXT
		// fresh claim against the SAME loop object the reconcile mutates.
		f, err := r.sandboxFailure(ctx, loop)
		Expect(err).ToNot(HaveOccurred())
		Expect(f).ToNot(BeNil(), "the evicted pod must be detected")
		Expect(r.applySandboxFailure(ctx, loop, f)).To(BeTrue(), "the F6 failure must be surfaced")
		cond := findConditionFn(loop.Status.Conditions, cov.AgentFailedCondition)
		Expect(cond).ToNot(BeNil(), "AgentFailed must be set True by the F6 check")
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))

		// Fresh pod + a fresh claim: the recreation ran, the recovered agent
		// wrote its claim. The fresh pod is planted directly (the operator's
		// ensureSandbox recreate is the kind run's job) with a terminated
		// agent that carries the claim in its termination message.
		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name + "-sandbox"}, pod)).To(Succeed())
		pod.Status = corev1.PodStatus{
			Phase: corev1.PodSucceeded,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "agent",
				State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{
						ExitCode: 0,
						Reason:   "Completed",
						Message:  `{"observedPhase":"Planning","status":"success"}`,
					},
				},
			}},
		}
		Expect(k8sClient.SubResource("status").Update(ctx, pod)).To(Succeed())
		// The Sandbox is deleted (the retry): recreate it so the F6 check
		// (sandboxFailure reads the Sandbox) finds the fresh pod + sandbox.
		sb := &sandboxv1beta1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name + "-sandbox", UID: types.UID(name + "-sb-uid-2")},
			Spec: sandboxv1beta1.SandboxSpec{
				SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
					PodTemplate: sandboxv1beta1.PodTemplate{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "agent", Image: "example.com/cox-agent:v1"}},
						},
					},
				},
				OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
			},
		}
		Expect(k8sClient.Create(ctx, sb)).To(Succeed())

		// Mutation: remove the clearRecoveredCondition call in
		// advancePhaseFromClaim (loop_s4_phase.go) and this spec FAILS
		// (AgentFailed stays True).
		_, _, _ = r.advancePhaseFromClaim(ctx, loop)
		cond = findConditionFn(loop.Status.Conditions, cov.AgentFailedCondition)
		Expect(cond).ToNot(BeNil(), "the AgentFailed condition must still be present (the audit record)")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse), "AgentFailed must be cleared to False on a fresh claim")
		Expect(cond.Reason).To(Equal("Recovered"))
	})

	// The F6 review follow-up (P2): the condition carries the POD-LEVEL
	// reason + the kubelet message (pod.status.reason / the pod condition
	// message) when set, falling back to the container's termination reason.
	// The kind repro: the container terminated with reason "Error" (exit
	// 137) while the pod condition carried reason "Evicted" + the kubelet
	// message "Usage of EmptyDir volume tmp exceeds the limit 100Mi" — the
	// doc promise is the kubelet's word, not the coarse container reason.
	It("uses the pod-level reason + kubelet message over the container's (the pod status)", func() {
		const ns, name = "f6-podstatus-ns", "f6-podstatus"
		r := f6NewReconciler(ns, name)
		f6PlantSandboxAndPodPodStatusFn(ns, name)
		loop := &cov.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, loop)).To(Succeed())
		// Mutation: drop the pod.Status.Message read in sandboxFailure
		// (loop_s4_phase.go — podStatusMessage) and this spec FAILS (the
		// message is empty: the failure conditions carry no message and the
		// DisruptionTarget message is the bare "Evicted", not the EmptyDir
		// text).
		f, err := r.sandboxFailure(ctx, loop)
		Expect(err).ToNot(HaveOccurred())
		Expect(f).ToNot(BeNil(), "the failed pod must be detected")
		Expect(f.reason).To(Equal("Evicted"), "the pod-level reason must win over the container's Error")
		Expect(f.message).To(ContainSubstring("Usage of EmptyDir volume"), "the kubelet's pod.status.message must be carried")
		// And the condition carries it.
		r.applySandboxFailure(ctx, loop, f)
		cond := findConditionFn(loop.Status.Conditions, cov.AgentFailedCondition)
		Expect(cond).ToNot(BeNil())
		Expect(cond.Reason).To(Equal("Evicted"))
		Expect(cond.Message).To(ContainSubstring("Usage of EmptyDir volume"))
	})

	// The evict-and-exit race (reviewer, 13:04): the agent exits 0 with a
	// valid claim at about the same moment the kubelet evicts the pod for
	// the EmptyDir overrun. The pod is Failed (Evicted) AND the claim is
	// valid. The claim WINS (ADR-0004: the claim channel is the only thing
	// that advances): sandboxFailure returns nil, and advancePhaseFromClaim
	// advances the phase machine instead of surfacing an AgentFailed failure.
	It("does NOT treat an evicted pod as an infra failure when the agent already terminated with a valid claim (the claim wins, the race is defined)", func() {
		const ns, name = "f6-race-ns", "f6-race"
		r := f6NewReconciler(ns, name)
		f6PlantSandboxAndPodPodStatusFn(ns, name) // the real kubelet Evicted shape
		// The agent's valid claim (exit 0) — the race: it terminated with a
		// valid claim at about the same moment the pod went Failed/Evicted.
		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name + "-sandbox"}, pod)).To(Succeed())
		pod.Status.ContainerStatuses[0].State.Terminated = &corev1.ContainerStateTerminated{
			ExitCode: 0,
			Reason:   "Completed",
			Message:  `{"observedPhase":"Planning","status":"success","headCommit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`,
		}
		Expect(k8sClient.SubResource("status").Update(ctx, pod)).To(Succeed())
		loop := &cov.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, loop)).To(Succeed())
		// Mutation: drop the hasValidSandboxClaim claim-wins check in
		// sandboxFailure (loop_s4_phase.go) and this spec FAILS at the
		// sandboxFailure assertion (the evicted pod surfaces as an AgentFailed
		// failure instead of being skipped).
		f, err := r.sandboxFailure(ctx, loop)
		Expect(err).ToNot(HaveOccurred())
		Expect(f).To(BeNil(), "a valid claim wins over the Failed/Evicted pod (no infra failure)")
		// And the advance path proceeds: no AgentFailed condition.
		Expect(findConditionFn(loop.Status.Conditions, cov.AgentFailedCondition)).To(BeNil(), "no AgentFailed condition (the claim won)")
		// The race is a defined behaviour: the claim channel is the ONLY thing
		// that advances (ADR-0004), so the advance path (advancePhaseFromClaim
		// with a valid Planning claim on a Planning Loop) must run and advance
		// to Implementing, not surface an infra failure.
		loopIn := &cov.Loop{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID(name + "-uid")},
			Spec:       loop.Spec,
			Status:     cov.LoopStatus{Phase: cov.LoopPhasePlanning},
		}
		_, changed, advancedFrom := r.advancePhaseFromClaim(ctx, loopIn)
		Expect(advancedFrom).To(Equal(cov.LoopPhasePlanning), "the claim advanced the phase (the race is a defined behaviour)")
		Expect(changed).To(BeTrue())
		Expect(loopIn.Status.Phase).To(Equal(cov.LoopPhaseImplementing))
		Expect(findConditionFn(loopIn.Status.Conditions, cov.AgentFailedCondition)).To(BeNil(), "no AgentFailed condition after the advance")
	})

	// The fallback: a failed pod WITHOUT a pod-level condition (no
	// status.reason, no condition) keeps the container's termination reason.
	It("falls back to the container's termination reason when the pod status carries no reason", func() {
		const ns, name = "f6-fallback-ns", "f6-fallback"
		r := f6NewReconciler(ns, name)
		f6PlantSandboxAndPodFn(ns, name) // the existing fixture: no pod condition
		loop := &cov.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, loop)).To(Succeed())
		f, err := r.sandboxFailure(ctx, loop)
		Expect(err).ToNot(HaveOccurred())
		Expect(f).ToNot(BeNil())
		Expect(f.reason).To(Equal("Evicted"), "the container's termination reason (Evicted) is the fallback")
		Expect(f.message).To(ContainSubstring("EmptyDir volume tmp"))
	})

	It("makes NO decision on an in-progress agent (I49: a Running pod is never a failure)", func() {
		const ns, name = "f6-running-ns", "f6-running"
		r := f6NewReconciler(ns, name)
		// Plant the sandbox + a RUNNING pod.
		sb := &sandboxv1beta1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name + "-sandbox", UID: types.UID(name + "-sb-uid")},
			Spec: sandboxv1beta1.SandboxSpec{
				SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
					PodTemplate: sandboxv1beta1.PodTemplate{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "agent", Image: "example.com/cox-agent:v1"}},
						},
					},
				},
				OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
			},
		}
		Expect(k8sClient.Create(ctx, sb)).To(Succeed())
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: ns, Name: name + "-sandbox", UID: types.UID(name + "-pod-uid"),
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "agent", Image: "example.com/cox-agent:v1"}},
			},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		pod.Status = corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "agent",
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
		By("sandboxFailure returns nil (no failure — the pod is Running)")
		loop := &cov.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, loop)).To(Succeed())
		f, err := r.sandboxFailure(ctx, loop)
		Expect(err).ToNot(HaveOccurred())
		Expect(f).To(BeNil(), "a Running pod is never a failure (I49)")
	})

	It("F14 repro: a stale Failed pod (not controlled by the current Sandbox) is NOT counted as a new attempt", func() {
		const ns, name = "f6-f14-ns", "f6-f14"
		r := f6NewReconciler(ns, name)
		// Step 1: Create Sandbox A (UID: A) + a Failed pod controlled by A.
		sbAUID := types.UID("f14-sandbox-a-uid")
		sbA := &sandboxv1beta1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name + "-sandbox", UID: sbAUID},
			Spec: sandboxv1beta1.SandboxSpec{
				SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
					PodTemplate: sandboxv1beta1.PodTemplate{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "agent", Image: "example.com/cox-agent:v1"}},
						},
					},
				},
				OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
			},
		}
		Expect(k8sClient.Create(ctx, sbA)).To(Succeed())
		// The API server generates the UID (it ignores the one we set).
		// Read it back.
		sbAActual := &sandboxv1beta1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name + "-sandbox"}, sbAActual)).To(Succeed())
		sbAUID = sbAActual.UID
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: ns, Name: name + "-sandbox", UID: types.UID(name + "-pod-uid"),
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: sandboxv1beta1.GroupVersion.String(),
					Kind:       "Sandbox",
					Name:       name + "-sandbox",
					UID:        sbAUID,
					Controller: &[]bool{true}[0],
				}},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "agent", Image: "example.com/cox-agent:v1"}},
			},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		pod.Status = corev1.PodStatus{
			Phase: corev1.PodFailed,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "agent",
				State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "Error"},
				},
				LastTerminationState: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "Error"},
				},
			}},
		}
		Expect(k8sClient.SubResource("status").Update(ctx, pod)).To(Succeed())
		// Step 2: Run applySandboxFailure (counts attempt 1, deletes Sandbox A).
		loop := &cov.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, loop)).To(Succeed())
		f, err := r.sandboxFailure(ctx, loop)
		Expect(err).ToNot(HaveOccurred())
		Expect(f).ToNot(BeNil(), "sandboxFailure must detect the failed pod")
		Expect(string(f.sandboxUID)).To(Equal(string(sbAUID)), "the failure must be attributed to Sandbox A")
		r.applySandboxFailure(ctx, loop, f)
		// Step 3: Create Sandbox B (new UID), keep the old pod present.
		// The old pod is still Failed and still present (the GC has not run).
		// It is controlled by Sandbox A (deleted), NOT by Sandbox B.
		sbBUID := types.UID("f14-sandbox-b-uid")
		sbB := &sandboxv1beta1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name + "-sandbox", UID: sbBUID},
			Spec: sandboxv1beta1.SandboxSpec{
				SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
					PodTemplate: sandboxv1beta1.PodTemplate{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "agent", Image: "example.com/cox-agent:v1"}},
						},
					},
				},
				OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
			},
		}
		Expect(k8sClient.Create(ctx, sbB)).To(Succeed())
		// Step 4: Re-reconcile N times. The old pod is still present (Failed,
		// controlled by the deleted Sandbox A). sandboxFailure reads it by
		// name and attributes its failure to Sandbox B's UID. The fix (owner
		// check) must make sandboxFailure return nil (no decision) for a pod
		// not controlled by the current Sandbox.
		for i := 0; i < 3; i++ {
			loop = &cov.Loop{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, loop)).To(Succeed())
			f, err = r.sandboxFailure(ctx, loop)
			Expect(err).ToNot(HaveOccurred())
			// The fix: sandboxFailure must return nil for a pod not controlled
			// by the current Sandbox (the stale pod from the deleted Sandbox A).
			Expect(f).To(BeNil(),
				"F14: sandboxFailure must NOT return a failure for a pod not controlled by the current Sandbox (reconcile %d)", i+1)
		}
		// Assert InfraAttempts == 1 (only the original failure was counted).
		loop = &cov.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, loop)).To(Succeed())
		if loop.Status.Agent != nil {
			Expect(loop.Status.Agent.InfraAttempts).To(Equal(int32(1)),
				"F14: a stale Failed pod (not controlled by the current Sandbox) must not be counted as a new attempt")
		}
	})
})

// f6FakeEnforcer is a no-op engine.Enforcer for the F6 spec.
type f6FakeEnforcer struct{}

func (f6FakeEnforcer) Apply(context.Context, *cov.Loop, policy.EffectivePolicy) error { return nil }
func (f6FakeEnforcer) Enforcing(context.Context, *cov.Loop) (bool, string)            { return true, "Enforcing" }
func (f6FakeEnforcer) KaptSpecHash(*cov.Loop, policy.EffectivePolicy) string          { return "" }
func (f6FakeEnforcer) BPFNodeAffinity() *corev1.Affinity                              { return nil }

var _ = Describe("F6 wiring: advancePhaseFromClaim invokes sandbox failure handling", func() {
	It("surfaces an Evicted pod via the advancePhaseFromClaim path (the wiring)", func() {
		const ns, name = "f6-wiring-ns", "f6-wiring"
		// Ensure the VolumeSnapshotClass (the suite setup does this, but this
		// spec runs in the same suite).
		vsc := &unstructured.Unstructured{}
		vsc.SetAPIVersion("snapshot.storage.k8s.io/v1")
		vsc.SetKind("VolumeSnapshotClass")
		vsc.SetName("f6-test-vsc")
		_ = unstructured.SetNestedField(vsc.Object, "test.driver", "driver")
		_ = unstructured.SetNestedField(vsc.Object, "Delete", "deletionPolicy")
		_ = k8sClient.Create(ctx, vsc) // ignore already-exists

		Expect(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())

		rec := record.NewFakeRecorder(64)
		r := &LoopReconciler{
			Client:             k8sClient,
			Scheme:             k8sClient.Scheme(),
			Recorder:           rec,
			Enforcer:           &f6FakeEnforcer{},
			CNIProber:          &cni.FakeProber{NextResult: cni.CNIProbeResult{Reason: cni.ReasonCNIEnforced}},
			SandboxImage:       "example.com/cox-sandbox:v1",
			ProxyImage:         "example.com/cox-proxy:v1",
			EgressProxyImage:   "example.com/egress-proxy:v1",
			ToolProxyImage:     "example.com/tool-proxy:v1",
			WorkspaceGitImage:  "example.com/git:v1",
			snapshotCRDPresent: func(context.Context) bool { return true },
			workspaceDriverSeam: func(context.Context, *cov.Loop) (string, bool) {
				return "test.driver", true
			},
			// No claim (the F6 scenario: the pod failed BEFORE writing a claim).
			readPhaseClaim: func(context.Context, *cov.Loop) (*PhaseClaim, error) {
				return nil, nil
			},
		}

		loop := &cov.Loop{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID(name + "-uid")},
			Spec: cov.LoopSpec{
				Goal:      "F6 wiring",
				Agent:     cov.AgentConfig{Image: "example.com/cox-agent:v1"},
				Workspace: cov.Workspace{Repo: "https://example.com/repo.git"},
			},
			Status: cov.LoopStatus{
				Phase: cov.LoopPhasePlanning,
			},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())

		// Plant the Sandbox + Evicted pod.
		f6PlantSandboxAndPodFn(ns, name)

		// Re-read the loop (fresh resourceVersion).
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, loop)).To(Succeed())

		// Call advancePhaseFromClaim directly (the wiring test: does the
		// reconcile path invoke the F6 failure handling?).
		// Mutation: remove the sandboxFailure call at loop_s4_phase.go:601
		// (the F6 check) and this spec FAILS (no AgentFailed condition).
		pending, changed, _ := r.advancePhaseFromClaim(ctx, loop)
		_ = pending
		Expect(changed).To(BeTrue(), "advancePhaseFromClaim must surface the F6 failure (the wiring)")

		// The AgentFailed condition is set on the in-memory loop.
		cond := findConditionFn(loop.Status.Conditions, cov.AgentFailedCondition)
		Expect(cond).ToNot(BeNil(), "the F6 wiring must set the AgentFailed condition")
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal("Evicted"))

		// The attempt count is set.
		Expect(loop.Status.Agent).ToNot(BeNil())
		Expect(loop.Status.Agent.InfraAttempts).To(Equal(int32(1)))
	})
})

// f6PlantSandboxAndPodPodStatusFn creates the Sandbox CR + a failed pod in
// the REAL KUBELET eviction shape: status.phase=Failed, status.reason=
// "Evicted", status.message with the kubelet's EmptyDir text, conditions
// Ready=False/ContainersNotReady, ContainersReady=False/PodFailed (no
// messages — a scan of failure conditions would return "no message" on this
// pod) and DisruptionTarget=True/TerminationByKubelet (the kubelet's
// secondary eviction word, Status=True), with the agent container terminated
// exitCode 137 reason Error (the coarser container-level word). The spec
// asserts the AgentFailed reason is Evicted and the message carries the
// kubelet's EmptyDir text — the P1 fix reads these from the pod STATUS, not
// a failure-condition scan.
func f6PlantSandboxAndPodPodStatusFn(ns, name string) {
	sb := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name + "-sandbox", UID: types.UID(name + "-sb-uid")},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "agent", Image: "example.com/cox-agent:v1"}},
					},
				},
			},
			OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
		},
	}
	Expect(k8sClient.Create(ctx, sb)).To(Succeed())
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: name + "-sandbox", UID: types.UID(name + "-pod-uid"),
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "agent", Image: "example.com/cox-agent:v1"}},
		},
	}
	Expect(k8sClient.Create(ctx, pod)).To(Succeed())
	pod.Status = corev1.PodStatus{
		Phase:   corev1.PodFailed,
		Reason:  "Evicted",                                                        // pod.status.reason: the kubelet's word
		Message: "Usage of EmptyDir volume \"tmp\" exceeds the limit \"100Mi\". ", // pod.status.message: the kubelet's message
		Conditions: []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionFalse, Reason: "ContainersNotReady"},
			{Type: corev1.ContainersReady, Status: corev1.ConditionFalse, Reason: "PodFailed"},
			{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue, Reason: "TerminationByKubelet", Message: "Evicted"},
		},
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: "agent",
			State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 137, Reason: "Error",
				},
			},
			LastTerminationState: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 137, Reason: "Error",
				},
			},
		}},
	}
	Expect(k8sClient.SubResource("status").Update(ctx, pod)).To(Succeed())
}

// findConditionFn finds a condition by type (package-level for reuse across
// Describe blocks).
func findConditionFn(conds []metav1.Condition, condType string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == condType {
			return &conds[i]
		}
	}
	return nil
}

// f6PlantSandboxAndPodFn creates the Sandbox CR + a failed (Evicted) pod.
// The pod status is set via the status subresource (Create does NOT set
// status in envtest — the API server strips it).
func f6PlantSandboxAndPodFn(ns, name string) {
	sbUID := types.UID(name + "-sb-uid")
	sb := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name + "-sandbox", UID: sbUID},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "agent", Image: "example.com/cox-agent:v1"}},
					},
				},
			},
			OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
		},
	}
	Expect(k8sClient.Create(ctx, sb)).To(Succeed())
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: name + "-sandbox", UID: types.UID(name + "-pod-uid"),
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "agent", Image: "example.com/cox-agent:v1"}},
		},
	}
	Expect(k8sClient.Create(ctx, pod)).To(Succeed())
	// Set the status via the status subresource.
	pod.Status = corev1.PodStatus{
		Phase: corev1.PodFailed,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: "agent",
			State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 137, Reason: "Evicted",
					Message: "Usage of EmptyDir volume tmp exceeds the limit 100Mi",
				},
			},
			LastTerminationState: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 137, Reason: "Evicted",
					Message: "Usage of EmptyDir volume tmp exceeds the limit 100Mi",
				},
			},
		}},
	}
	Expect(k8sClient.SubResource("status").Update(ctx, pod)).To(Succeed())
}
