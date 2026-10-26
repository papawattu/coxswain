package controller

// F89b: F89 fixed an EMPTY failure reason; it could not fix a NON-EMPTY one the
// condition schema cannot store. pod.status.reason is free text in the core
// API (no enum, no pattern, no length limit — see podStatusMessage, which
// copies it verbatim), while the CRD's metav1.Condition.reason is
// minLength 1 / maxLength 1024 / ^[A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?$. A
// value like "Low on ephemeral-storage" is therefore rejected by the API
// server, and it rejects the WHOLE status write — so AgentFailed never
// persists, status.agent.infraAttempts never persists, the per-Sandbox-UID
// dedupe never engages and the bounded retry never exhausts (F89's chain).
//
// These specs run against the envtest API server with config/crd/bases
// installed, so the pattern is enforced for real: that is what makes them a
// drift detector for the copied schema (conditionReasonRe). The first two
// drive Reconcile (the tail status write is where the rejection happened); the
// third pins setCondition's contract; the fourth the NetworkEnforced
// comparator, which compares a STORED reason with a caller's raw one.

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
	"github.com/papawattu/coxswain/internal/cni"
)

var _ = Describe("F89b: a failed pod whose kubelet reason the condition schema cannot store", func() {
	const (
		goal = "F89b test"
		// unstorableReason is the kind of value the kubelet writes and the
		// condition pattern rejects (a space and a hyphen). The repo's own
		// podStatusMessage comment names this exact string.
		unstorableReason = "Low on ephemeral-storage"
	)

	// plant creates a Loop at Verifying, its Sandbox and a controlled Failed pod
	// whose pod.status.reason is unstorable. withAgent selects the failure
	// constructor: true = the agent container terminated with an empty reason
	// (the pod-level reason wins, then must be normalised); false = no container
	// status at all (the pod-level constructor F89 could never have fixed).
	plant := func(name string, withAgent bool) (*cov.Loop, *LoopReconciler, *record.FakeRecorder) {
		ns := "f89b-" + name + "-" + nowSuffix()
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
		loop.Status.Phase = cov.LoopPhaseVerifying
		loop.Status.DesiredPhase = cov.LoopPhaseVerifying
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
				Namespace: ns, Name: name + "-sandbox",
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
		status := corev1.PodStatus{
			Phase:  corev1.PodFailed,
			Reason: unstorableReason, // free text: the API does not constrain it
		}
		if withAgent {
			status.ContainerStatuses = []corev1.ContainerStatus{{
				Name: "agent",
				State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{
						ExitCode: 1, // Reason: "" — the pod-level value is the unstorable one
					},
				},
			}}
		}
		pod.Status = status
		Expect(k8sClient.SubResource("status").Update(ctx, pod)).To(Succeed())

		rec := record.NewFakeRecorder(64)
		r := &LoopReconciler{CNIProber: testCNIProber(),
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

	// assertPersisted carries the assertions both Reconcile specs share. They
	// deliberately check the VALUE of the persisted reason, not just that the
	// write succeeded: with setCondition's guard in place the write succeeds even
	// under mutation M1 (it stores Unknown instead), so an "it persisted"
	// assertion would pass vacuously (the F89 M1/M3 lesson).
	assertPersisted := func(loop *cov.Loop) {
		got := &cov.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: loop.Namespace, Name: loop.Name}, got)).To(Succeed())
		Expect(got.Status.Agent).NotTo(BeNil(), "the attempt count must be durable")
		Expect(got.Status.Agent.InfraAttempts).To(Equal(int32(1)),
			"the count the rejected write used to lose (which kept the retry bound from ever exhausting)")
		persisted := findConditionFn(got.Status.Conditions, cov.AgentFailedCondition)
		Expect(persisted).NotTo(BeNil(), "AgentFailed must be persisted, not just set in memory")
		Expect(persisted.Status).To(Equal(metav1.ConditionTrue))
		Expect(persisted.Reason).To(Equal(podFailedReason),
			"an unstorable kubelet reason falls back to the pod-level default at the source")
		Expect(persisted.Message).To(ContainSubstring(unstorableReason),
			"the kubelet's own words must survive in the message")
		Expect(persisted.Message).To(ContainSubstring("the condition schema cannot store"),
			"the message must say why the reason differs from the kubelet's")
	}

	It("through Reconcile, the terminated path persists AgentFailed(reason=PodFailed)", func() {
		loop, r, rec := plant("terminated", true)

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: loop.Namespace, Name: loop.Name}})
		Expect(err).NotTo(HaveOccurred(),
			"an unstorable condition reason made the API server reject the whole status write")
		Expect(countEvents(rec, sandboxInfraRetryEventReason)).To(Equal(1), "one retry Event")
		assertPersisted(loop)
	})

	It("through Reconcile, the no-container-status path persists AgentFailed(reason=PodFailed)", func() {
		loop, r, rec := plant("no-agent", false)

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: loop.Namespace, Name: loop.Name}})
		Expect(err).NotTo(HaveOccurred(),
			"the pod-level constructor copies pod.status.reason verbatim and F89 never covered it")
		Expect(countEvents(rec, sandboxInfraRetryEventReason)).To(Equal(1), "one retry Event")
		assertPersisted(loop)
	})

	It("setCondition substitutes a storable reason only when the CRD would reject the value", func() {
		loop := &cov.Loop{}
		setCondition(loop, "F89bUnstorable", metav1.ConditionTrue, unstorableReason, "a message")
		Expect(findConditionFn(loop.Status.Conditions, "F89bUnstorable").Reason).To(Equal(conditionReasonUnknown),
			"a value the pattern rejects must not reach the status write")

		// A storable non-empty reason passes through byte for byte: sanitising it
		// would rewrite the kubelet's evidence for no reason.
		setCondition(loop, "F89bPassThrough", metav1.ConditionTrue, "Evicted", "")
		Expect(findConditionFn(loop.Status.Conditions, "F89bPassThrough").Reason).To(Equal("Evicted"))

		// The predicate agrees with the schema on the two shapes that matter.
		Expect(storableConditionReason("Low on ephemeral-storage")).To(BeFalse())
		Expect(storableConditionReason("")).To(BeFalse())
		Expect(storableConditionReason("PodFailed")).To(BeTrue())
		Expect(storableConditionReason("CreateContainerConfigError")).To(BeTrue())
	})

	// F91b (R37 I140): the length unit. The premise in I140 ("a 500-character
	// CJK reason is 1500 bytes, so len() substitutes a value the schema would
	// have accepted") does not hold — the reason pattern is ASCII-only, so every
	// input where a rune count and a byte count differ is rejected by the
	// PATTERN, and the "cannot store" message is true for it. These rows pin
	// which term decides, so the two cannot be swapped silently:
	//   - the boundary rows are pure length (ASCII, so len == runes);
	//   - the kubelet-text and multi-byte rows assert BOTH that the predicate is
	//     false AND that the regex is what rejects them.
	DescribeTable("F91b: which term rejects a reason (length vs pattern)",
		func(reason string, wantStorable bool, wantRegexMatch bool) {
			Expect(storableConditionReason(reason)).To(Equal(wantStorable))
			Expect(conditionReasonRe.MatchString(reason)).To(Equal(wantRegexMatch),
				"the pattern is the term I140's multi-byte example trips, not the length")
		},
		Entry("empty (minLength)", "", false, false),
		Entry("the boundary (1024 ASCII)", strings.Repeat("A", 1024), true, true),
		Entry("one over the boundary (1025 ASCII)", strings.Repeat("A", 1025), false, true),
		Entry("the kubelet's own text (spaces and a dash)", "Low on ephemeral-storage", false, false),
		Entry("multi-byte (500 CJK characters = 1500 bytes)", strings.Repeat("失", 500), false, false),
	)

	It("the NetworkEnforced comparator normalises the caller's reason before comparing", func() {
		msg := "the CNI does not police pod -> host-network egress"
		// What pass 1 stores when the caller's reason is empty (a custom Prober
		// returning "": setCondition substitutes Unknown).
		loop := &cov.Loop{Status: cov.LoopStatus{Conditions: []metav1.Condition{{
			Type:    cni.NetworkEnforcedCondition,
			Status:  metav1.ConditionUnknown,
			Reason:  conditionReasonUnknown,
			Message: msg,
		}}}}
		r := &LoopReconciler{CNIProber: testCNIProber()}

		Expect(r.networkEnforcedChanged(loop, metav1.ConditionUnknown, "", msg)).To(BeFalse(),
			"a normalised reason read back against the caller's raw one would fire an Event every reconcile")

		// Positive control: the comparator still reports a real change, so the
		// spec above cannot pass by returning false unconditionally.
		loop.Status.Conditions[0].Reason = "CNIUnenforced"
		loop.Status.Conditions[0].Status = metav1.ConditionFalse
		Expect(r.networkEnforcedChanged(loop, metav1.ConditionFalse, "", msg)).To(BeTrue())

		// And the Event text names the stored reason, not an empty one.
		rec := record.NewFakeRecorder(4)
		r.Recorder = rec
		r.emitNetworkEnforcedEvent(loop, metav1.ConditionFalse, "", msg)
		events := drainEvents(rec)
		Expect(events).To(HaveLen(1))
		Expect(events[0]).To(ContainSubstring("reason=" + conditionReasonUnknown))
	})
})
