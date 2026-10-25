package controller

// F48b: a sandbox pod the kubelet cannot mount its volumes for is an Event-only
// fact (the pod status says Pending + ContainerCreating, exactly like a slow
// image pull), so it needs its own grace and its own read: a matching Warning
// Event on THIS pod, past sandboxMountGrace, is a bounded sandbox infra failure.

import (
	"context"
	"errors"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cov "github.com/papawattu/coxswain/api/v1alpha1"
)

var _ = Describe("F48b: a sandbox pod whose volumes cannot be mounted", func() {
	const goal = "F48b test"

	// plant creates a Loop (Planning), its Sandbox and a controlled Pending pod
	// whose only container is Waiting/ContainerCreating — the shape the kubelet
	// shows for both "still starting" and "a volume will not mount". It returns
	// a reconciler whose clock is pod.creationTimestamp + the given offset.
	plant := func(name string) (*cov.Loop, *corev1.Pod, *LoopReconciler, *time.Duration, *record.FakeRecorder) {
		ns := "f48b-" + name + "-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		})
		loop := &cov.Loop{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
			Spec: cov.LoopSpec{Goal: goal, Agent: cov.AgentConfig{Image: "example.com/cox-agent:v1"},
				Workspace: cov.Workspace{Repo: "https://example.com/repo.git", GitCredentialSecret: "git-cred-missing"}},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		loop.Status.Phase = cov.LoopPhasePlanning
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		f6PlantSandboxAndPodFn(ns, name)
		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name + "-sandbox"}, pod)).To(Succeed())
		pod.Status = corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{{Name: "agent",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}}},
		}
		Expect(k8sClient.SubResource("status").Update(ctx, pod)).To(Succeed())
		offset := new(time.Duration)
		rec := record.NewFakeRecorder(64)
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient, Recorder: rec}
		r.now = func() metav1.Time { return metav1.NewTime(pod.CreationTimestamp.Add(*offset)) }
		return loop, pod, r, offset, rec
	}
	// plantMountEvent creates the kubelet's Event on the pod, for the given
	// involved UID (a previous attempt's Event carries the OLD UID).
	plantMountEvent := func(pod *corev1.Pod, reason, msg string, uid types.UID) {
		ev := &corev1.Event{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: pod.Namespace,
				Name:      pod.Name + "." + strings.ToLower(reason) + "-" + nowSuffix(),
			},
			InvolvedObject: corev1.ObjectReference{
				Kind: "Pod", Namespace: pod.Namespace, Name: pod.Name, UID: uid,
			},
			Reason:         reason,
			Message:        msg,
			Type:           corev1.EventTypeWarning,
			Count:          1,
			FirstTimestamp: metav1.Now(),
			LastTimestamp:  metav1.Now(),
		}
		Expect(k8sClient.Create(ctx, ev)).To(Succeed())
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

	It("bounds a FailedMount past the grace: AgentFailed(FailedMount), one retry, the Sandbox deleted", func() {
		loop, pod, r, offset, rec := plant("f48bmount")
		plantMountEvent(pod, "FailedMount",
			`MountVolume.SetUp failed for volume "git-cred" : secret "git-cred-missing" not found`, pod.UID)

		*offset = sandboxMountGrace + 10*time.Second
		f, err := r.sandboxFailure(ctx, loop)
		Expect(err).NotTo(HaveOccurred())
		Expect(f).NotTo(BeNil(), "a FailedMount Event past the grace is an infra failure")
		Expect(f.reason).To(Equal("FailedMount"))
		Expect(f.mountFailure).To(BeTrue(), "the message must not claim a container terminated")

		Expect(r.applySandboxFailure(ctx, loop, f)).To(BeTrue())
		Expect(loop.Status.Agent.InfraAttempts).To(Equal(int32(1)))
		cond := findConditionFn(loop.Status.Conditions, cov.AgentFailedCondition)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal("FailedMount"))
		Expect(cond.Message).To(ContainSubstring("cannot mount its volumes"))
		Expect(cond.Message).To(ContainSubstring(`secret "git-cred-missing" not found`), "the kubelet's own text")
		Expect(countEvents(rec, sandboxInfraRetryEventReason)).To(Equal(1), "one retry Event")

		By("the Sandbox is deleted so the next reconcile recreates it")
		sb := &sandboxv1beta1.Sandbox{}
		gerr := k8sClient.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}, sb)
		Expect(apierrors.IsNotFound(gerr)).To(BeTrue(), "the Sandbox is deleted for the recreate, got %v", gerr)
	})

	It("decides nothing when the pod's events say nothing about a mount", func() {
		loop, _, r, offset, _ := plant("f48bnoevent")
		*offset = sandboxMountGrace + 10*time.Minute
		f, err := r.sandboxFailure(ctx, loop)
		Expect(err).NotTo(HaveOccurred())
		Expect(f).To(BeNil(), "no Event: no decision (this is exactly a slow image pull, I49)")
	})

	It("decides nothing for a Warning Event that is not a mount failure", func() {
		loop, pod, r, offset, _ := plant("f48bbackoff")
		plantMountEvent(pod, "BackOff", "Back-off restarting failed container", pod.UID)
		*offset = sandboxMountGrace + 10*time.Minute
		f, err := r.sandboxFailure(ctx, loop)
		Expect(err).NotTo(HaveOccurred())
		Expect(f).To(BeNil(), "a crashloop is not a mount failure (and is a different, in-progress shape)")
	})

	It("decides nothing on the PREVIOUS attempt's mount Event (the UID filter)", func() {
		loop, pod, r, offset, _ := plant("f48bstale")
		plantMountEvent(pod, "FailedMount", "secret not found (previous sandbox)", types.UID("old-pod-uid"))
		*offset = sandboxMountGrace + 10*time.Minute
		f, err := r.sandboxFailure(ctx, loop)
		Expect(err).NotTo(HaveOccurred())
		Expect(f).To(BeNil(), "Events outlive the pod: without the UID filter the fresh attempt would be failed on the old evidence")
	})

	It("decides nothing inside sandboxMountGrace", func() {
		loop, pod, r, offset, _ := plant("f48bgrace")
		plantMountEvent(pod, "FailedMount", "secret not found", pod.UID)
		*offset = sandboxMountGrace - 10*time.Second
		f, err := r.sandboxFailure(ctx, loop)
		Expect(err).NotTo(HaveOccurred())
		Expect(f).To(BeNil(), "a mount that resolves in seconds (a slow CSI attach) must not burn an attempt")
	})

	It("fails the Loop once the retries are exhausted, naming the kubelet's evidence", func() {
		loop, pod, r, offset, rec := plant("f48bbound")
		plantMountEvent(pod, "FailedAttachVolume", "attaching volume: rpc error: deadline exceeded", pod.UID)
		*offset = sandboxMountGrace + time.Minute
		f, err := r.sandboxFailure(ctx, loop)
		Expect(err).NotTo(HaveOccurred())
		Expect(f).NotTo(BeNil())
		Expect(f.reason).To(Equal("FailedAttachVolume"))

		By("the boundary: the LAST permitted retry still retries (attempt == sandboxInfraRetries)")
		// F48g: the seeded count belongs to THIS run (the operator wrote it
		// during the episode), so the run key must match or the count restarts.
		loop.Status.Agent = &cov.AgentStatus{InfraAttempts: sandboxInfraRetries - 1, InfraEpisode: sandboxRunKey(loop)}
		Expect(r.applySandboxFailure(ctx, loop, f)).To(BeTrue())
		Expect(loop.Status.Agent.InfraAttempts).To(Equal(int32(sandboxInfraRetries)))
		Expect(loop.Status.Phase).To(Equal(cov.LoopPhasePlanning), "the last permitted attempt recreates, it does not fail")
		Expect(countEvents(rec, sandboxInfraRetryEventReason)).To(Equal(1))

		By("then the next attempt (a FRESH Sandbox UID) fails the Loop")
		loop.Status.Agent.InfraSandboxUID = "earlier-sandbox-uid"
		Expect(r.applySandboxFailure(ctx, loop, f)).To(BeTrue())
		Expect(loop.Status.Phase).To(Equal(cov.LoopPhaseFailed))
		failed := findConditionFn(loop.Status.Conditions, string(cov.LoopPhaseFailed))
		Expect(failed).NotTo(BeNil())
		Expect(failed.Reason).To(Equal(sandboxInfraFailedCondition))
		Expect(failed.Message).To(ContainSubstring("cannot mount its volumes"))
		Expect(failed.Message).To(ContainSubstring("deadline exceeded"))
		Expect(loop.Status.Agent.InfraAttempts).To(Equal(int32(sandboxInfraRetries + 1)))
		Expect(countEvents(rec, sandboxInfraFinalEventReason)).To(Equal(1))
	})

	// F48b (kind finding): the sandbox infra-attempt count used to be reset
	// whenever the live Sandbox's UID differed from status.agent.infraSandboxUID
	// — which is exactly what the operator's own retry recreate produces. On a
	// real cluster the bound therefore never tripped: the same "attempt 1 of 3"
	// was re-emitted on every recreate (.samples/f48b/08-operator-log-f48b.log).
	// The count belongs to the failure EPISODE and is cleared by a claim.
	It("keeps the retry count across the operator's own Sandbox recreate, and trips at the bound", func() {
		loop, pod, r, offset, _ := plant("f48brecreate")
		plantMountEvent(pod, "FailedMount", "secret not found", pod.UID)
		*offset = sandboxMountGrace + time.Minute
		f, err := r.sandboxFailure(ctx, loop)
		Expect(err).NotTo(HaveOccurred())
		Expect(f).NotTo(BeNil())
		Expect(r.applySandboxFailure(ctx, loop, f)).To(BeTrue())
		Expect(loop.Status.Agent.InfraAttempts).To(Equal(int32(1)))

		By("the operator's next reconcile recreates the Sandbox (a new UID) and ensures it")
		fresh := &sandboxv1beta1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Namespace: pod.Namespace, Name: pod.Name},
			Spec: sandboxv1beta1.SandboxSpec{
				SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
					PodTemplate: sandboxv1beta1.PodTemplate{
						Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "agent", Image: "example.com/cox-agent:v1"}}},
					},
				},
				OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
			},
		}
		Expect(k8sClient.Create(ctx, fresh)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}, fresh)).To(Succeed())
		Expect(string(fresh.UID)).NotTo(Equal(string(f.sandboxUID)), "the recreate has a new UID (the shape that used to reset the count)")
		_, err = r.ensureSandbox(ctx, loop, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(loop.Status.Agent.InfraAttempts).To(Equal(int32(1)), "the retry episode keeps its count across the recreate")

		By("the next failure (a new Sandbox UID) counts again and reaches the bound")
		second := &sandboxFailure{sandboxUID: fresh.UID, container: "<pod>", reason: "FailedMount",
			message: "secret not found", mountFailure: true}
		Expect(r.applySandboxFailure(ctx, loop, second)).To(BeTrue())
		Expect(loop.Status.Agent.InfraAttempts).To(Equal(int32(2)))
		third := &sandboxFailure{sandboxUID: "another-sandbox-uid", container: "<pod>", reason: "FailedMount",
			message: "secret not found", mountFailure: true}
		Expect(r.applySandboxFailure(ctx, loop, third)).To(BeTrue())
		Expect(loop.Status.Phase).To(Equal(cov.LoopPhaseFailed), "3 attempts in the episode, then Failed:SandboxFailed")
	})

	It("clears the retry count when a claim the operator accepts ends the episode", func() {
		loop, _, r, _, _ := plant("f48brecover")
		// P3-5 (review-postmerge-10): seed the episode key too — a count with no
		// key is a state the operator never writes (the claim clear only tests
		// InfraAttempts, so the omission was invisible).
		loop.Status.Agent = &cov.AgentStatus{InfraAttempts: 2, InfraSandboxUID: "some-sandbox-uid",
			InfraEpisode: sandboxRunKey(loop)}
		r.readPhaseClaim = func(_ context.Context, _ *cov.Loop) (*PhaseClaim, error) {
			return &PhaseClaim{ObservedPhase: cov.LoopPhasePlanning, Status: claimSuccess, Iteration: loop.Status.Iteration}, nil
		}
		_, changed, _ := r.advancePhaseFromClaim(ctx, loop)
		Expect(changed).To(BeTrue())
		Expect(loop.Status.Agent.InfraAttempts).To(BeZero(), "a fresh claim is the recovery: the episode's count ends")
		Expect(loop.Status.Agent.InfraSandboxUID).To(BeEmpty())
		// F48e (F3): the third field the claim clear zeroes. Without it nothing
		// asserted that the clear ends the KEY too, so a clear that left the key
		// behind (and with it, the next failure's wrong-episode read) passed.
		Expect(loop.Status.Agent.InfraEpisode).To(BeEmpty())
	})

	It("ignores a FailedMount the kubelet has superseded (a later Normal event for the same pod)", func() {
		loop, pod, r, offset, _ := plant("f48bsuperseded")
		// The mount FAILED, then succeeded on a retry: the kubelet's later
		// SuccessfulMountVolume (and Started) prove the mount is history, and
		// the pod is still Pending for a different reason (a slow image pull).
		old := metav1.NewTime(time.Now().Add(-10 * time.Minute))
		plantEventAt(pod, "FailedMount", `MountVolume.SetUp failed for volume "workspace-creds" : timeout`, pod.UID, old)
		plantEventAt(pod, "SuccessfulMountVolume", `MountVolume.SetUp succeeded for volume "workspace-creds"`, pod.UID,
			metav1.NewTime(old.Add(5*time.Minute)))
		*offset = sandboxMountGrace + 5*time.Minute
		f, err := r.sandboxFailure(ctx, loop)
		Expect(err).NotTo(HaveOccurred())
		Expect(f).To(BeNil(), "a superseded FailedMount must not burn a retry")
	})

	It("runs the trailing status write after the one-shot annotation patch (the resourceVersion is refreshed)", func() {
		loop, pod, clock, offset, rec := plant("f48brv")
		plantMountEvent(pod, "FailedMount", "secret not found", pod.UID)
		*offset = sandboxMountGrace + time.Minute
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(),
			apiReader: forbidEventsReader{Client: k8sClient}, Recorder: rec}
		r.now = clock.now
		Expect(r.sandboxFailure(ctx, loop)).To(BeNil(), noFailure())
		// markMemoryPVCUnboundNotified refreshes loop.ResourceVersion after its
		// patch; without that the trailing Status().Update below is a 409.
		setCondition(loop, cov.SandboxNotStartingCondition, metav1.ConditionTrue, "SandboxNotStarting", "test")
		Expect(r.finalizeLoopStatus(ctx, loop, true, true, false, false, "")).To(Succeed())
		persisted := &cov.Loop{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(loop), persisted)).To(Succeed())
		Expect(persisted.Annotations[sandboxEventsUnreadableAnnotation]).To(Equal(resumeAnnotationTrue))
		Expect(conditionTruePresent(persisted, cov.SandboxNotStartingCondition)).To(BeTrue(), "the pass's conditions are persisted")
	})

	It("surfaces an unreadable Events list once (no decision, no failure) instead of skipping it silently", func() {
		loop, pod, clock, offset, rec := plant("f48bforbidden")
		plantMountEvent(pod, "FailedMount", "secret not found", pod.UID)
		*offset = sandboxMountGrace + time.Minute
		// The same clock as the plant (the pod is past its grace), but an
		// apiReader whose Event List is Forbidden.
		r := &LoopReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			apiReader: forbidEventsReader{Client: k8sClient}, Recorder: rec,
		}
		r.now = clock.now

		f, err := r.sandboxFailure(ctx, loop)
		Expect(err).NotTo(HaveOccurred(), "a read error is never a controller error")
		Expect(f).To(BeNil(), "and never a failure decision (I49)")
		Expect(countEvents(rec, sandboxEventsUnreadableReason)).To(Equal(1), "the blindness is visible (R26 I90)")

		By("a second pass emits no second Event (the one-shot annotation)")
		f, err = r.sandboxFailure(ctx, loop)
		Expect(err).NotTo(HaveOccurred())
		Expect(f).To(BeNil())
		Expect(countEvents(rec, sandboxEventsUnreadableReason)).To(BeZero())
	})
})

// noFailure is a readability helper for the nil-failure assertions.
func noFailure() string { return "no failure decision (I49)" }

// plantEventAt creates a kubelet Event for the pod with an explicit last
// timestamp (the superseded-FailedMount spec needs an ordering).
func plantEventAt(pod *corev1.Pod, reason, msg string, uid types.UID, last metav1.Time) {
	ev := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Namespace: pod.Namespace,
			Name: pod.Name + "." + strings.ToLower(reason) + "-" + nowSuffix()},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: pod.Namespace, Name: pod.Name, UID: uid},
		Reason:         reason,
		Message:        msg,
		Type:           corev1.EventTypeNormal,
		Count:          1,
		FirstTimestamp: last,
		LastTimestamp:  last,
	}
	if !sandboxMountFailureReasons[reason] {
		ev.Type = corev1.EventTypeNormal
	} else {
		ev.Type = corev1.EventTypeWarning
	}
	Expect(k8sClient.Create(context.Background(), ev)).To(Succeed())
}

// forbidEventsReader is a client.Reader whose Event List is Forbidden (a
// missing events get;list rule), for the F48b unreadable-Events spec.
type forbidEventsReader struct{ client.Client }

func (f forbidEventsReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*corev1.EventList); ok {
		return apierrors.NewForbidden(schema.GroupResource{Resource: "events"}, "", errors.New("events is forbidden"))
	}
	return f.Client.List(ctx, list, opts...)
}
