package controller

// F48: a sandbox pod that never starts must be visible (SandboxNotStarting),
// a kubelet-declared config error is bounded like any sandbox infra failure,
// and the Secret-name fields are validated by the CRD.

import (
	"context"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cov "github.com/papawattu/coxswain/api/v1alpha1"
)

var _ = Describe("F48: a sandbox pod that never starts", func() {
	const goal = "F48 test"

	// plant creates a Loop (Planning), its Sandbox and a controlled Pending
	// pod with the given container statuses. It returns the Loop, the pod and a
	// reconciler whose clock is pod.creationTimestamp + the given offset
	// (settable through *offset).
	plant := func(name string, statuses []corev1.ContainerStatus) (*cov.Loop, *corev1.Pod, *LoopReconciler, *time.Duration, *record.FakeRecorder) {
		ns := "f48-" + name + "-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		})
		loop := &cov.Loop{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
			Spec: cov.LoopSpec{Goal: goal, Agent: cov.AgentConfig{Image: "example.com/cox-agent:v1"},
				Workspace: cov.Workspace{Repo: "https://example.com/repo.git"}},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		loop.Status.Phase = cov.LoopPhasePlanning
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		f6PlantSandboxAndPodFn(ns, name)
		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name + "-sandbox"}, pod)).To(Succeed())
		pod.Status = corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: statuses}
		Expect(k8sClient.SubResource("status").Update(ctx, pod)).To(Succeed())
		offset := new(time.Duration)
		rec := record.NewFakeRecorder(64)
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient, Recorder: rec}
		r.now = func() metav1.Time { return metav1.NewTime(pod.CreationTimestamp.Add(*offset)) }
		return loop, pod, r, offset, rec
	}
	waiting := func(reason, msg string) []corev1.ContainerStatus {
		return []corev1.ContainerStatus{{Name: "agent", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: msg}}}}
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

	It("sets SandboxNotStarting once after the grace, not before, and clears it when the pod starts", func() {
		loop, pod, r, offset, rec := plant("f48stuck", waiting("PodInitializing", ""))

		*offset = sandboxStartGrace - time.Minute
		_, ch, _ := r.sandboxFailureStep(ctx, loop, false)
		Expect(ch).To(BeFalse(), "in progress (a slow image pull looks the same): no decision")
		Expect(conditionTruePresent(loop, cov.SandboxNotStartingCondition)).To(BeFalse())
		Expect(countEvents(rec, sandboxNotStartingEventReason)).To(BeZero())

		*offset = sandboxStartGrace + time.Minute
		stop, ch, _ := r.sandboxFailureStep(ctx, loop, false)
		Expect(stop).To(BeFalse(), "surfacing only: it never stops the phase machine")
		Expect(ch).To(BeTrue())
		Expect(conditionTruePresent(loop, cov.SandboxNotStartingCondition)).To(BeTrue())
		cond := findConditionFn(loop.Status.Conditions, cov.SandboxNotStartingCondition)
		msg := cond.Message
		*offset = sandboxStartGrace + 2*time.Minute
		_, ch, _ = r.sandboxFailureStep(ctx, loop, false)
		Expect(ch).To(BeFalse(), "a second pass after the transition changes nothing (no status write per pass)")
		Expect(findConditionFn(loop.Status.Conditions, cov.SandboxNotStartingCondition).Message).To(Equal(msg), "the message is fixed")
		Expect(countEvents(rec, sandboxNotStartingEventReason)).To(Equal(1), "exactly one Warning Event across passes")

		By("the pod starts: the condition clears")
		Expect(k8sClient.Get(ctx, podKey(pod), pod)).To(Succeed())
		pod.Status.Phase = corev1.PodRunning
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "agent", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
		Expect(k8sClient.SubResource("status").Update(ctx, pod)).To(Succeed())
		_, ch, _ = r.sandboxFailureStep(ctx, loop, false)
		Expect(ch).To(BeTrue())
		Expect(conditionTruePresent(loop, cov.SandboxNotStartingCondition)).To(BeFalse())
	})

	It("treats a CreateContainerConfigError past the grace as a bounded sandbox infra failure; before it, no decision", func() {
		loop, _, r, offset, _ := plant("f48cfg", waiting(sandboxConfigErrorReason, `secret "x" not found`))

		*offset = sandboxConfigErrorGrace - 10*time.Second
		f, err := r.sandboxFailure(ctx, loop)
		Expect(err).NotTo(HaveOccurred())
		Expect(f).To(BeNil(), "inside the grace the kubelet may still resolve it: no decision")

		*offset = sandboxConfigErrorGrace + 30*time.Second
		f, err = r.sandboxFailure(ctx, loop)
		Expect(err).NotTo(HaveOccurred())
		Expect(f).NotTo(BeNil())
		Expect(f.reason).To(Equal(sandboxConfigErrorReason))
		Expect(f.message).To(ContainSubstring(`secret "x" not found`))

		Expect(r.applySandboxFailure(ctx, loop, f)).To(BeTrue())
		Expect(loop.Status.Agent.InfraAttempts).To(Equal(int32(1)))
		cond := findConditionFn(loop.Status.Conditions, cov.AgentFailedCondition)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Message).To(Equal(`the sandbox pod's agent container has not started (waiting: CreateContainerConfigError): secret "x" not found`),
			"nothing terminated: the message must not say it did")

		By("the bound: a later pass past sandboxInfraRetries fails the Loop with the kubelet's message")
		loop.Status.Agent.InfraAttempts = sandboxInfraRetries
		f.sandboxUID = types.UID("another-sandbox")
		Expect(r.applySandboxFailure(ctx, loop, f)).To(BeTrue())
		Expect(loop.Status.Phase).To(Equal(cov.LoopPhaseFailed))
		failed := findConditionFn(loop.Status.Conditions, string(cov.LoopPhaseFailed))
		Expect(failed).NotTo(BeNil())
		Expect(failed.Reason).To(Equal(sandboxInfraFailedCondition))
		Expect(failed.Message).To(ContainSubstring(`secret "x" not found`))
	})

	It("a CreateContainerConfigError with a valid claim in LastTerminationState is not a failure (the claim wins)", func() {
		statuses := waiting(sandboxConfigErrorReason, `secret "x" not found`)
		statuses[0].LastTerminationState = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 0,
			Message:  `{"observedPhase":"Planning","status":"success"}`,
		}}
		loop, _, r, offset, _ := plant("f48claim", statuses)
		*offset = sandboxConfigErrorGrace + 30*time.Second
		f, err := r.sandboxFailure(ctx, loop)
		Expect(err).NotTo(HaveOccurred())
		Expect(f).To(BeNil(), "a valid claim means the phase finished; no infra attempt is burned")
	})

	It("F48d: the Verifying hold RETURNS the changed flag the not-starting surfacing set (it used to discard it)", func() {
		loop, _, r, offset, rec := plant("f48d-return", waiting("PodInitializing", ""))
		*offset = sandboxStartGrace + time.Minute
		loop.Status.Phase = cov.LoopPhaseVerifying
		loop.Status.DesiredPhase = cov.LoopPhaseVerifying
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())

		// The hold's return value IS what persists the condition: drive the real
		// phase function twice and require changed=true the first time (the
		// surfacing happened) and false the second (already surfaced).
		advanced, changed, next := r.advancePhaseFromClaim(ctx, loop)
		Expect(advanced).To(BeFalse(), "Verifying is evidence-gated: the claim never advances it")
		Expect(next).To(Equal(cov.LoopPhase("")))
		Expect(changed).To(BeTrue(),
			"F48d: the hold must report the change it just made, or the status write (and so the condition) is dropped")
		cond := findConditionFn(loop.Status.Conditions, cov.SandboxNotStartingCondition)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))

		advanced, changed, _ = r.advancePhaseFromClaim(ctx, loop)
		Expect(advanced).To(BeFalse())
		Expect(changed).To(BeFalse(), "a second pass with the condition already set changes nothing")
		Expect(countEvents(rec, sandboxNotStartingEventReason)).To(Equal(1),
			"exactly one Warning Event across both passes (the transition-gated Event must not re-fire)")
	})

	It("F48d: through Reconcile at Verifying the condition is written to the API", func() {
		loop, _, r, offset, _ := plant("f48d-reconcile", waiting("PodInitializing", ""))
		*offset = sandboxStartGrace + time.Minute
		loop.Status.Phase = cov.LoopPhaseVerifying
		loop.Status.DesiredPhase = cov.LoopPhaseVerifying
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		ns, name := loop.Namespace, loop.Name
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		Expect(err).NotTo(HaveOccurred())
		got := &cov.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(cov.LoopPhaseVerifying), "the hold still holds")
		cond := findConditionFn(got.Status.Conditions, cov.SandboxNotStartingCondition)
		Expect(cond).NotTo(BeNil(), "F48d: the condition must reach the API at Verifying")
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	})

	It("F48c: SandboxNotStarting clears when the pod it described is gone", func() {
		loop, pod, r, offset, _ := plant("f48c-gone", waiting("PodInitializing", ""))
		*offset = sandboxStartGrace + time.Minute
		_, ch, _ := r.sandboxFailureStep(ctx, loop, false)
		Expect(ch).To(BeTrue())
		Expect(conditionIsTrue(loop, cov.SandboxNotStartingCondition)).To(BeTrue())

		By("the pod is deleted (a fresh Sandbox gets its own grace)")
		Expect(k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0))).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, podKey(pod), &corev1.Pod{}))
		}).WithTimeout(10 * time.Second).Should(BeTrue())
		Expect(r.applySandboxNotStarting(ctx, loop)).To(BeTrue(), "the condition must clear, not freeze on a pod that is gone")
		cond := findConditionFn(loop.Status.Conditions, cov.SandboxNotStartingCondition)
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("PodGone"))
	})

	It("F48d: InvalidImageName is bounded at once (the kubelet never retries it)", func() {
		loop, _, r, offset, _ := plant("f48d-image", waiting(sandboxInvalidImageNameReason, `invalid reference format`))
		*offset = 10 * time.Second // younger than the CreateContainerConfigError grace
		f, err := r.sandboxFailure(ctx, loop)
		Expect(err).NotTo(HaveOccurred())
		Expect(f).NotTo(BeNil(), "a terminal kubelet verdict needs no grace")
		Expect(f.reason).To(Equal(sandboxInvalidImageNameReason))
		Expect(f.neverStarted).To(BeTrue())
	})

	It("a Pending pod with no kubelet reason is never a failure, however old (I49)", func() {
		loop, _, r, offset, _ := plant("f48quiet", waiting("PodInitializing", ""))
		*offset = 2 * time.Hour
		f, err := r.sandboxFailure(ctx, loop)
		Expect(err).NotTo(HaveOccurred())
		Expect(f).To(BeNil())
	})

	It("the CRD rejects a Secret name that is not a DNS subdomain, or longer than 253", func() {
		ns := "f48-crd-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		})
		for _, bad := range []string{"Not A Name", strings.Repeat("a", 300)} {
			git := &cov.Loop{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "badgit"},
				Spec: cov.LoopSpec{Goal: goal, Workspace: cov.Workspace{Repo: "https://example.com/r.git", GitCredentialSecret: bad}}}
			Expect(k8sClient.Create(ctx, git)).NotTo(Succeed(), "gitCredentialSecret %.20q must be rejected", bad)
			ep := &cov.Loop{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "badep"},
				Spec: cov.LoopSpec{Goal: goal, Workspace: cov.Workspace{Repo: "https://example.com/r.git"},
					Agent: cov.AgentConfig{EndpointSecretRef: bad, ModelEndpoint: "m.svc:8080"}}}
			Expect(k8sClient.Create(ctx, ep)).NotTo(Succeed(), "endpointSecretRef %.20q must be rejected", bad)
		}
		ok := &cov.Loop{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "okname"},
			Spec: cov.LoopSpec{Goal: goal, Workspace: cov.Workspace{Repo: "https://example.com/r.git", GitCredentialSecret: "samples-git-cred.v2"}}}
		Expect(k8sClient.Create(ctx, ok)).To(Succeed(), "a valid name is still accepted")
	})
})

func podKey(pod *corev1.Pod) types.NamespacedName {
	return types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}
}
