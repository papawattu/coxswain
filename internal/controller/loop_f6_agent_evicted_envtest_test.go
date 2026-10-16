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

	// f6PlantSandboxAndPod creates the Sandbox CR + a failed pod (the F6
	// failure signature). The pod status is set via the status subresource
	// (Create does NOT set status in envtest).
	f6PlantSandboxAndPod := func(ns, name string) {
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

	findCondition := func(conds []metav1.Condition, condType string) *metav1.Condition {
		for i := range conds {
			if conds[i].Type == condType {
				return &conds[i]
			}
		}
		return nil
	}

	It("surfaces an evicted pod with no claim (the live F6 repro)", func() {
		const ns, name = "f6-evict-ns", "f6-evict"
		r := f6NewReconciler(ns, name)
		f6PlantSandboxAndPod(ns, name)
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
		cond := findCondition(loop.Status.Conditions, cov.AgentFailedCondition)
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
		f6PlantSandboxAndPod(ns, name)
		By("sandboxFailure detects the evicted pod (new UID)")
		f, err := r.sandboxFailure(ctx, loop)
		Expect(err).ToNot(HaveOccurred())
		Expect(f).ToNot(BeNil())
		By("applySandboxFailure fails the Loop (the bound is exhausted)")
		changed := r.applySandboxFailure(ctx, loop, f)
		Expect(changed).To(BeTrue())
		Expect(loop.Status.Phase).To(Equal(cov.LoopPhaseFailed))
		cond := findCondition(loop.Status.Conditions, string(cov.LoopPhaseFailed))
		Expect(cond).ToNot(BeNil(), "the Failed condition must be set")
		Expect(cond.Reason).To(Equal(sandboxInfraFailedCondition))
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
})

// f6FakeEnforcer is a no-op engine.Enforcer for the F6 spec.
type f6FakeEnforcer struct{}

func (f6FakeEnforcer) Apply(context.Context, *cov.Loop, policy.EffectivePolicy) error { return nil }
func (f6FakeEnforcer) Enforcing(context.Context, *cov.Loop) (bool, string)            { return true, "Enforcing" }
func (f6FakeEnforcer) KaptSpecHash(*cov.Loop, policy.EffectivePolicy) string          { return "" }
func (f6FakeEnforcer) BPFNodeAffinity() *corev1.Affinity                              { return nil }
