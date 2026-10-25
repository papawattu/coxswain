package controller

// F89: ContainerStateTerminated.Reason is OPTIONAL in the Kubernetes API. When
// the kubelet records neither a pod status reason nor a container termination
// reason, sandboxFailure used to hand an EMPTY reason to setCondition, and the
// CRD requires a condition reason (`minLength: 1`): the API server rejected the
// WHOLE status write, so AgentFailed never persisted and neither did
// status.agent.infraAttempts (mutated by applySandboxFailure just before the
// shared write). With the count lost, the per-Sandbox-UID dedupe never engages
// and the bounded retry never exhausts: the Loop recreates the sandbox forever.
//
// These specs run against the envtest API server with config/crd/bases
// installed, so the CRD's `minLength: 1` on reason is enforced — a fake client
// would accept the write and the wedge would be invisible. The first spec drives
// Reconcile itself (the trailing finalizeLoopStatus → Status().Update is where
// the rejection happened); the second pins the decision the fix makes and its
// Event; the third covers the setCondition guard.

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cov "github.com/papawattu/coxswain/api/v1alpha1"
)

var _ = Describe("F89: a failed pod whose termination records no reason", func() {
	const goal = "F89 test"

	// plant creates a Loop at the given phase, its Sandbox and a controlled
	// Failed pod whose agent container is Terminated{ExitCode: 1, Reason: ""}
	// and whose pod status carries NO reason and NO DisruptionTarget condition
	// — the shape the API allows and the code must survive. It returns the Loop,
	// a reconciler with no claim (the F6 scenario: the pod failed without one)
	// and the recorder.
	plant := func(name string, phase cov.LoopPhase) (*cov.Loop, *LoopReconciler, *record.FakeRecorder) {
		ns := "f89-" + name + "-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		})
		loop := &cov.Loop{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
			Spec: cov.LoopSpec{
				Goal:      goal,
				Agent:     cov.AgentConfig{Image: "example.com/cox-agent:v1"},
				Workspace: cov.Workspace{Repo: "https://example.com/repo.git"},
			},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		loop.Status.Phase = phase
		if phase == cov.LoopPhaseVerifying {
			loop.Status.DesiredPhase = phase
		}
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())

		sb := &sandboxv1beta1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name + "-sandbox"},
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
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name + "-sandbox"}, sb)).To(Succeed())
		controller := true
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: ns, Name: name + "-sandbox", UID: types.UID(name + "-pod-uid"),
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: sandboxv1beta1.GroupVersion.String(),
					Kind:       "Sandbox",
					Name:       name + "-sandbox",
					UID:        sb.UID,
					Controller: &controller,
				}},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "agent", Image: "example.com/cox-agent:v1"}},
			},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		pod.Status = corev1.PodStatus{
			Phase: corev1.PodFailed, // no Reason, no Message, no DisruptionTarget
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "agent",
				State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{
						ExitCode: 1, // Reason: "" — the kubelet recorded none
					},
				},
			}},
		}
		Expect(k8sClient.SubResource("status").Update(ctx, pod)).To(Succeed())

		rec := record.NewFakeRecorder(64)
		r := &LoopReconciler{
			Client:    k8sClient,
			Scheme:    k8sClient.Scheme(),
			apiReader: k8sClient,
			Recorder:  rec,
			// No claim: the pod failed BEFORE writing one (the F6 scenario).
			readPhaseClaim: func(context.Context, *cov.Loop) (*PhaseClaim, error) { return nil, nil },
		}
		return loop, r, rec
	}

	countEvents := func(rec *record.FakeRecorder, reason string) int {
		n := 0
		for _, e := range drainEvents(rec) {
			if strings.Contains(e, reason) {
				n++
			}
		}
		return n
	}

	It("through Reconcile, persists AgentFailed(reason=PodFailed) and the attempt count", func() {
		loop, r, rec := plant("reconcile", cov.LoopPhaseVerifying)
		ns, name := loop.Namespace, loop.Name

		// Reconcile's tail (finalizeLoopStatus → Status().Update,
		// loop_controller.go:1086) is the write the CRD rejected before F89.
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		Expect(err).NotTo(HaveOccurred(),
			"an empty condition reason made the API server reject the whole status write")
		Expect(countEvents(rec, sandboxInfraRetryEventReason)).To(Equal(1), "one retry Event")

		got := &cov.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, got)).To(Succeed())
		Expect(got.Status.Agent).NotTo(BeNil(), "the attempt count must be durable")
		Expect(got.Status.Agent.InfraAttempts).To(Equal(int32(1)),
			"the count the rejected write used to lose (which kept the retry bound from ever exhausting)")
		persisted := findConditionFn(got.Status.Conditions, cov.AgentFailedCondition)
		Expect(persisted).NotTo(BeNil(), "AgentFailed must be persisted, not just set in memory")
		Expect(persisted.Status).To(Equal(metav1.ConditionTrue))
		Expect(persisted.Reason).To(Equal(podFailedReason),
			"an empty kubelet reason falls back to the pod-level default (F89)")
		Expect(persisted.Message).NotTo(BeEmpty())
	})

	It("sandboxFailure defaults the empty reason before the condition is built", func() {
		loop, r, rec := plant("helper", cov.LoopPhasePlanning)

		f, err := r.sandboxFailure(ctx, loop)
		Expect(err).NotTo(HaveOccurred())
		Expect(f).NotTo(BeNil(), "a Failed pod with no claim is a sandbox failure")
		Expect(f.reason).To(Equal(podFailedReason),
			"the fallback belongs at the source, not only in setCondition")

		Expect(r.applySandboxFailure(ctx, loop, f)).To(BeTrue())
		cond := findConditionFn(loop.Status.Conditions, cov.AgentFailedCondition)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(podFailedReason))
		Expect(cond.Message).To(ContainSubstring("terminated without a phase claim"))
		Expect(loop.Status.Agent).NotTo(BeNil())
		Expect(loop.Status.Agent.InfraAttempts).To(Equal(int32(1)))
		Expect(countEvents(rec, sandboxInfraRetryEventReason)).To(Equal(1), "one retry Event")
	})

	It("setCondition substitutes a self-describing reason for an empty one", func() {
		loop := &cov.Loop{}
		setCondition(loop, "F89Guard", metav1.ConditionTrue, "", "a message")
		guard := findConditionFn(loop.Status.Conditions, "F89Guard")
		Expect(guard).NotTo(BeNil())
		Expect(guard.Reason).To(Equal(conditionReasonUnknown),
			"the guard's value must be self-describing in the object (not a plausible cause)")

		// A non-empty reason passes through byte for byte: the CRD pattern
		// accepts every kubelet reason in use, and sanitising would rewrite the
		// kubelet's own evidence.
		setCondition(loop, "F89PassThrough", metav1.ConditionTrue, "OOMKilled", "")
		Expect(findConditionFn(loop.Status.Conditions, "F89PassThrough").Reason).To(Equal("OOMKilled"))
	})
})
