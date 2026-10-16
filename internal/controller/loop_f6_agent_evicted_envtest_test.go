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
// This spec covers the two key outcomes: the evicted-pod surface+retry,
// and the I49 in-progress keep (a Running pod is never a failure).

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
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"

	cov "github.com/papawattu/coxswain/api/v1alpha1"
	cni "github.com/papawattu/coxswain/internal/cni"
	policy "github.com/papawattu/coxswain/internal/policy"
)

var _ = Describe("F6: a sandbox pod that failed without a claim is surfaced and retried (bounded)", func() {
	var f6VSCOnce sync.Once
	f6EnsureVSC := func() {
		f6VSCOnce.Do(func() {
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

	// f6Fixture creates the ns + Loop + primes the reconciler (bootstrap:
	// Pending -> Planning -> ...). The claim seam serves a PLANNING claim so
	// the prime advances. The CNI seam serves a POLICE result (the D38 gate
	// is not active). The checkpoint seam reports the VolumeSnapshot CRD
	// present + a known driver + a matching VSC.
	//
	// The Sandbox CR is planted DIRECTLY (the envtest has no sandbox
	// controller to create it; the operator's ensureSandbox creates it via
	// controllerutil.CreateOrUpdate, but in the bare test reconciler the
	// embedded client's Get does not see the CreateOrUpdate's write — the
	// F6 check's r.Get returns NotFound. Planting it directly is the
	// correct fixture: the F6 check reads the live Sandbox + pod, and the
	// operator's ensureSandbox is not the subject of this spec).
	f6Fixture := func(ns, name string) *LoopReconciler {
		f6EnsureVSC()
		if err := k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		}); err != nil && !strings.Contains(err.Error(), "already exists") {
			Expect(err).ToNot(HaveOccurred())
		}
		rec := record.NewFakeRecorder(128)
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
			readPhaseClaim: func(context.Context, *cov.Loop) (*PhaseClaim, error) {
				return &PhaseClaim{
					ObservedPhase: cov.LoopPhasePlanning,
					Status:        claimSuccess,
					PlanAttempt:   0,
				}, nil
			},
		}
		loop := &cov.Loop{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
			Spec: cov.LoopSpec{
				Goal:      "F6 test",
				Agent:     cov.AgentConfig{Image: "example.com/cox-agent:v1"},
				Workspace: cov.Workspace{Repo: "https://example.com/repo.git"},
			},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		// Plant the Sandbox CR directly (the F6 check reads it).
		sb := &sandboxv1beta1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: ns, Name: name + "-sandbox", UID: types.UID(name + "-sb-uid"),
			},
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
		// Prime: bootstrap (Pending -> Planning -> ...). The claim seam
		// serves a PLANNING claim so the prime advances.
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		Expect(err).ToNot(HaveOccurred())
		return r
	}

	// f6FailedPod creates the sandbox pod with the F6 failure signature
	// (evicted, the live repro).
	f6FailedPod := func(ns, name string) *corev1.Pod {
		msg := "Usage of EmptyDir volume tmp exceeds the limit 100Mi"
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: ns, Name: name + "-sandbox", UID: types.UID(name + "-uid"),
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:  "agent",
					Image: "example.com/cox-agent:v1",
				}},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodFailed,
				ContainerStatuses: []corev1.ContainerStatus{{
					Name: "agent",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode: 137, Reason: "Evicted", Message: msg,
						},
					},
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode: 137, Reason: "Evicted", Message: msg,
						},
					},
				}},
			},
		}
	}

	findCondition := func(conds []metav1.Condition, condType string) *metav1.Condition {
		for i := range conds {
			if conds[i].Type == condType {
				return &conds[i]
			}
		}
		return nil
	}

	It("surfaces an evicted pod with no claim (the live F6 repro) + retries the sandbox", func() {
		const ns, name = "f6-evict-ns", "f6-evict"
		r := f6Fixture(ns, name)
		By("the sandbox pod is evicted (the live F6 repro: /tmp emptyDir overrun)")
		pod := f6FailedPod(ns, name)
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		By("the operator SURFACES the failure (I90): AgentFailed condition True + a Warning Event")
		res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		Expect(err).ToNot(HaveOccurred())
		_ = res
		loop := &cov.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, loop)).To(Succeed())
		cond := findCondition(loop.Status.Conditions, cov.AgentFailedCondition)
		Expect(cond).ToNot(BeNil(), "AgentFailed condition must be set")
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal("Evicted"))
		Expect(cond.Message).To(ContainSubstring("EmptyDir volume tmp"))
		// The attempt is counted (status.agent.infraAttempts == 1, the I65
		// P1-A per-Sandbox-UID guard).
		Expect(loop.Status.Agent).ToNot(BeNil())
		Expect(loop.Status.Agent.InfraAttempts).To(Equal(int32(1)))
		By("the Loop is NOT yet Failed (the bound is not exhausted)")
		Expect(loop.Status.Phase).ToNot(Equal(cov.LoopPhaseFailed))
		// The Warning Event is emitted.
		events := r.Recorder.(*record.FakeRecorder).Events
		found := false
		select {
		case e := <-events:
			found = strings.Contains(e, "SandboxRetry") || strings.Contains(e, "SandboxFailed")
		case <-time.After(1 * time.Second):
		}
		for i := 0; i < len(events); i++ {
			if e := <-events; strings.Contains(e, "SandboxRetry") || strings.Contains(e, "SandboxFailed") {
				found = true
			}
		}
		Expect(found).To(BeTrue(), "a Warning Event (SandboxRetry or SandboxFailed) must be emitted")
	})

	It("makes NO decision on an in-progress agent (I49: a Running pod is never a failure)", func() {
		const ns, name = "f6-running-ns", "f6-running"
		r := f6Fixture(ns, name)
		By("the sandbox pod is RUNNING (the agent is still doing the work)")
		pod := f6FailedPod(ns, name)
		pod.Status.Phase = corev1.PodRunning
		pod.Status.ContainerStatuses[0].State = corev1.ContainerState{
			Running: &corev1.ContainerStateRunning{},
		}
		pod.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		Expect(err).ToNot(HaveOccurred())
		By("NO AgentFailed condition is set (I49: in-progress is never a failure)")
		loop := &cov.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, loop)).To(Succeed())
		cond := findCondition(loop.Status.Conditions, cov.AgentFailedCondition)
		if cond != nil {
			Expect(cond.Status).ToNot(Equal(metav1.ConditionTrue))
		}
	})
})

// f6FakeEnforcer is a no-op engine.Enforcer for the F6 spec (the eBPF gate
// is not the subject; it just needs to not wedge the reconcile).
type f6FakeEnforcer struct{}

func (f6FakeEnforcer) Apply(context.Context, *cov.Loop, policy.EffectivePolicy) error { return nil }
func (f6FakeEnforcer) Enforcing(context.Context, *cov.Loop) (bool, string) { return true, "Enforcing" }
func (f6FakeEnforcer) KaptSpecHash(*cov.Loop, policy.EffectivePolicy) string { return "" }
func (f6FakeEnforcer) BPFNodeAffinity() *corev1.Affinity { return nil }
