// F93a (R46, .samples/f93/F93.md + the addendum): bound ANY unschedulable
// Loop pod. A pod the scheduler explicitly rejects (PodScheduled=False,
// Unschedulable) wedged Loops with no decision: the sandbox case sat
// SandboxNotStarting for 20h+ (the F48b unmountable-volume bound only
// counts mount Events, so it never trips on a FailedScheduling pod), and
// with C2's placement flags a Loop with no matching node holds at Planning
// with ModelProxyNotReady=True forever (the proxy pods are the first to go
// unschedulable; the sandbox is not even created).
//
// These specs cover the three shapes:
//  1. the model proxy pod unschedulable (the C2 failure mode): the
//     PodUnschedulable condition appears within one reconcile, the clock
//     annotation is stamped, and — with the clock advanced past the bound —
//     the Loop fails with the PodUnschedulable reason (Failed condition,
//     phase Failed, the sandbox is not even created yet).
//  2. the sandbox pod unschedulable (the original F93a observation): the
//     same surface + bound, and the SandboxNotStarting condition is also
//     set (the two surfaces are independent; F93a's bound fires on the
//     scheduler's verdict, not on the F48 mount-Event bound).
//  3. the bound does NOT trip within the window (a pod stuck for less than
//     the bound stays held, not failed) and a pod that becomes schedulable
//     again clears the condition + resets the clock (the clear path).
//
// The specs must FAIL when the F93a bound is disabled (a mutation that
// removes the applyUnschedulableLoopPods call from Reconcile, or that
// returns false from the bound's exhaustion check — the gate-spec norm,
// R16 I43).
package controller

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
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

var _ = Describe("F93a: the unschedulable Loop pod is bounded", func() {
	var (
		ctx = context.Background()
		ns  string
	)

	BeforeEach(func() {
		ns = "f93a-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
	})

	AfterEach(func() {
		_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	})

	// f93aReconciler builds the spec's reconciler: a wired CNI prober (CNI
	// gate passes), no Enforcer + AllowUnenforced (the D30 gate passes on the
	// flag) and a CLOCK the spec advances deterministically (the bound is a
	// pure function of the annotation + the operator's clock, so advancing
	// the clock is the deterministic way to trip it — the spec never sleeps
	// for 10 minutes).
	f93aReconciler := func(clock *time.Time) *LoopReconciler {
		return &LoopReconciler{
			CNIProber:              testCNIProber(),
			Client:                 k8sClient,
			Scheme:                 k8sClient.Scheme(),
			AllowUnenforced:        true,
			AllowUnenforcedNetwork: true,
			now:                    func() metav1.Time { return metav1.NewTime(*clock) },
		}
	}

	reconcile := func(r *LoopReconciler, name string) {
		_, err := r.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Name: name, Namespace: ns},
		})
		Expect(err).NotTo(HaveOccurred())
	}

	// f93aModelLoop creates a Loop with an endpointSecretRef (the model
	// proxy exists: the operator-owned bare proxy pod the spec wedges), no
	// policy refs (no tool proxies), and an in-cluster repo (no egress
	// proxy for the workspace init clone — the shape the C2 specs use).
	f93aModelLoop := func(name string) *coxv1alpha1.Loop {
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      loopGoal,
				Workspace: testWorkspace(),
				Agent: coxv1alpha1.AgentConfig{
					Image:             runnerImage,
					Model:             testModel,
					EndpointSecretRef: d33ModelCredsSecret,
					ModelEndpoint:     d34ModelEndpoint,
				},
			},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		return loop
	}

	f93aGetLoop := func(name string) *coxv1alpha1.Loop {
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		return loop
	}

	// setUnschedulable marks the named pod unschedulable (the scheduler's
	// own verdict: PodScheduled=False, Unschedulable, with the scheduler's
	// message). The pod must already exist (the spec asserts it does).
	setUnschedulable := func(name, nsName, msg string) {
		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: nsName}, pod)).To(Succeed())
		for i := range pod.Status.Conditions {
			c := &pod.Status.Conditions[i]
			if c.Type == corev1.PodScheduled {
				c.Status = corev1.ConditionFalse
				c.Reason = corev1.PodReasonUnschedulable
				c.Message = msg
				c.LastTransitionTime = metav1.Now()
				Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
				return
			}
		}
		// No PodScheduled condition yet (the pod was just created, not
		// scheduled): add one (the apiserver does not add it until the
		// scheduler acts, which never happens in envtest — the spec sets it
		// itself, the same way the F48 specs set mount-failure Events).
		pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
			Reason: corev1.PodReasonUnschedulable, Message: msg,
			LastTransitionTime: metav1.Now(),
		})
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	}

	// setScheduledFalse marks the named pod with PodScheduled=False and the
	// given reason (the spec uses this for M1: a pod the scheduler rejected
	// with a NON-Unschedulable reason, which the F93a read must not count).
	setScheduledFalse := func(name, nsName, reason, msg string) {
		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: nsName}, pod)).To(Succeed())
		for i := range pod.Status.Conditions {
			c := &pod.Status.Conditions[i]
			if c.Type == corev1.PodScheduled {
				c.Status = corev1.ConditionFalse
				c.Reason = reason
				c.Message = msg
				c.LastTransitionTime = metav1.Now()
				Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
				return
			}
		}
		pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
			Reason: reason, Message: msg,
			LastTransitionTime: metav1.Now(),
		})
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	}

	// proxyPodName returns the pod name the operator builds for the Loop's
	// model proxy (the proxy's name IS the Loop's name — the F93a helper
	// uses the same derived name the builder does).
	proxyPodNameFor := func(loopName string) string { return proxyPodName(loopName) }

	It("a model proxy pod that is unschedulable is surfaced, clocked, and — past the bound — fails the Loop with the PodUnschedulable reason", func() {
		clock := time.Now()
		name := "f93a-mp"
		r := f93aReconciler(&clock)
		f93aModelLoop(name)
		reconcile(r, name)

		// The model proxy pod exists (the D35a gate ensures it before the
		// hold) and is not yet unschedulable (no scheduler verdict).
		By("the model proxy pod exists and the Loop is held for it (the D35a gate, pre-F93a)")
		pod := &corev1.Pod{}
		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Name: proxyPodNameFor(name), Namespace: ns}, pod)
		}, "10s").Should(Succeed())
		Expect(pod.Status.Conditions).To(BeEmpty(),
			"the proxy pod has no conditions yet (no scheduler verdict in envtest)")
		// The D35a gate holds the sandbox Suspended (ModelProxyNotReady=True):
		// the proxy pod is not Ready, so the sandbox is not created.
		Eventually(func() bool {
			loop := f93aGetLoop(name)
			return conditionTruePresent(loop, coxv1alpha1.ModelProxyNotReadyCondition)
		}, "10s").Should(BeTrue(),
			"the D35a gate holds the Loop for the not-Ready proxy pod (ModelProxyNotReady=True)")

		By("the scheduler marks the proxy pod unschedulable (the C2 failure mode: no node matches the placement flags)")
		setUnschedulable(proxyPodNameFor(name), ns, "0/3 nodes are available: 3 node(s) didn't match Pod's node affinity/selector")
		// The first reconcile AFTER the verdict: the condition appears, the
		// clock annotation is stamped. The Loop is NOT yet failed (the clock
		// has not advanced past the bound).
		reconcile(r, name)
		loop := f93aGetLoop(name)
		Expect(conditionTruePresent(loop, unschedulableLoopPodsCondition)).To(BeTrue(),
			"the PodUnschedulable condition is True after the first reconcile past the verdict")
		Expect(loop.Annotations[loopUnschedulableSinceAnnotation]).ToNot(BeEmpty(),
			"the clock annotation is stamped on the transition to the stuck state")
		Expect(loop.Status.Phase).NotTo(Equal(coxv1alpha1.LoopPhaseFailed),
			"the Loop is not failed while the stuck state is within the bound")
		// The condition message names the pod + the scheduler's message
		// (I90: the surface must be actionable).
		var stuckCond *metav1.Condition
		for i := range loop.Status.Conditions {
			if loop.Status.Conditions[i].Type == unschedulableLoopPodsCondition {
				stuckCond = &loop.Status.Conditions[i]
			}
		}
		Expect(stuckCond).NotTo(BeNil())
		Expect(stuckCond.Message).To(ContainSubstring(proxyPodNameFor(name)),
			"the condition message names the stuck pod")
		Expect(stuckCond.Message).To(ContainSubstring("didn't match"),
			"the condition message carries the scheduler's own message")

		By("the clock advances past the bound: the Loop fails with the PodUnschedulable reason")
		clock = clock.Add(unschedulableLoopPodsBound + time.Second)
		reconcile(r, name)
		loop = f93aGetLoop(name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseFailed),
			"the Loop is Failed past the bound")
		var failedCond *metav1.Condition
		for i := range loop.Status.Conditions {
			if loop.Status.Conditions[i].Type == string(coxv1alpha1.LoopPhaseFailed) {
				failedCond = &loop.Status.Conditions[i]
			}
		}
		Expect(failedCond).NotTo(BeNil(), "the Failed condition is set on the terminal pass")
		Expect(failedCond.Reason).To(Equal("PodUnschedulable"),
			"the Failed condition's reason is PodUnschedulable (the F93a terminal)")
		Expect(failedCond.Message).To(ContainSubstring(proxyPodNameFor(name)),
			"the Failed condition names the stuck pod")
		// The surface condition is cleared (the Failed condition supersedes
		// it, the F6 exhaustion pattern).
		Expect(conditionTruePresent(loop, unschedulableLoopPodsCondition)).To(BeFalse(),
			"the PodUnschedulable surface condition is cleared on the terminal pass")
	})

	It("a sandbox pod that is unschedulable is surfaced and bounded (the original F93a observation: the F48b bound does not trip on a FailedScheduling pod)", func() {
		clock := time.Now()
		name := "f93a-sb"
		r := f93aReconciler(&clock)
		// No endpointSecretRef: the D35a gate is skipped, so the sandbox is
		// created (the shape the F48 specs use).
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      loopGoal,
				Workspace: testWorkspace(),
			},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		reconcile(r, name)

		By("the sandbox pod exists")
		sb := &sandboxv1beta1.Sandbox{}
		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Name: sandboxName(name), Namespace: ns}, sb)
		}, "10s").Should(Succeed())
		// The sandbox pod exists (the operator creates the Sandbox; the
		// agent-sandbox controller creates the pod). In envtest the
		// agent-sandbox controller is not running, so the pod is not created
		// by the controller — the spec creates it directly (the same pattern
		// the F48 specs use for the pod they then wedge).
		pod := &corev1.Pod{}
		err := k8sClient.Get(ctx, types.NamespacedName{Name: sandboxName(name), Namespace: ns}, pod)
		if apierrors.IsNotFound(err) {
			// Create the pod directly (the F48 pattern): the pod is
			// controller-owned by the Sandbox (the F14 check), so the
			// controller ref must be set for the F93a read to see it as the
			// operator's own pod.
			pod = &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName(name),
					Namespace: ns,
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion: sandboxv1beta1.GroupVersion.String(),
						Kind:       "Sandbox",
						Name:       sandboxName(name),
						UID:        sb.UID,
						Controller: func() *bool { b := true; return &b }(),
					}},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "sandbox", Image: runnerImage}},
				},
				Status: corev1.PodStatus{Phase: corev1.PodPending},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		}

		By("the scheduler marks the sandbox pod unschedulable")
		setUnschedulable(sandboxName(name), ns, "0/3 nodes are available: 3 node(s) had untolerated taint")
		reconcile(r, name)
		loop2 := f93aGetLoop(name)
		Expect(conditionTruePresent(loop2, unschedulableLoopPodsCondition)).To(BeTrue(),
			"the PodUnschedulable condition is True for the unschedulable sandbox pod")
		Expect(loop2.Annotations[loopUnschedulableSinceAnnotation]).ToNot(BeEmpty(),
			"the clock annotation is stamped")
		Expect(loop2.Status.Phase).NotTo(Equal(coxv1alpha1.LoopPhaseFailed),
			"the Loop is not failed while within the bound")

		By("a second stuck pass (nothing changed) requeues (the P2 fix: the bound is re-checked on every stuck pass)")
		rvBefore := loop2.ResourceVersion
		res, err := r.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Name: name, Namespace: ns},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeNumerically(">", 0),
			"the stuck-but-unchanged pass requeues (the 10m bound must be re-checked; a Pending pod gets no more watch events)")
		// The Loop's resourceVersion is unchanged (no status write on a
		// stuck-but-unchanged pass — the F84c discipline).
		loop3 := f93aGetLoop(name)
		Expect(loop3.ResourceVersion).To(Equal(rvBefore),
			"no status write on a stuck-but-unchanged pass (the F84c discipline)")

		By("the clock advances past the bound: the Loop fails with the PodUnschedulable reason")
		clock = clock.Add(unschedulableLoopPodsBound + time.Second)
		reconcile(r, name)
		loop4 := f93aGetLoop(name)
		Expect(loop4.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseFailed),
			"the Loop is Failed past the bound (the F48b bound does not trip on a FailedScheduling pod — F93a does)")
		var failedCond *metav1.Condition
		for i := range loop4.Status.Conditions {
			if loop4.Status.Conditions[i].Type == string(coxv1alpha1.LoopPhaseFailed) {
				failedCond = &loop4.Status.Conditions[i]
			}
		}
		Expect(failedCond).NotTo(BeNil())
		Expect(failedCond.Reason).To(Equal("PodUnschedulable"),
			"the Failed condition's reason is PodUnschedulable")
	})

	// M9: the f93aUnschedulableStep helper sets verifyRequeue on stuck and
	// changed on changed — directly, with no other gate to mask the bug.
	It("M9: f93aUnschedulableStep sets verifyRequeue on a stuck-but-unchanged pass (direct call)", func() {
		clock := time.Now()
		name := "f93a-m9"
		r := f93aReconciler(&clock)
		// No endpointSecretRef: the D35a gate is skipped, so the sandbox is
		// created (the shape the F48 specs use).
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      loopGoal,
				Workspace: testWorkspace(),
			},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		reconcile(r, name)

		By("the sandbox pod exists")
		sb := &sandboxv1beta1.Sandbox{}
		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Name: sandboxName(name), Namespace: ns}, sb)
		}, "10s").Should(Succeed())
		pod := &corev1.Pod{}
		err := k8sClient.Get(ctx, types.NamespacedName{Name: sandboxName(name), Namespace: ns}, pod)
		if apierrors.IsNotFound(err) {
			pod = &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName(name),
					Namespace: ns,
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion: sandboxv1beta1.GroupVersion.String(),
						Kind:       "Sandbox",
						Name:       sandboxName(name),
						UID:        sb.UID,
						Controller: func() *bool { b := true; return &b }(),
					}},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "sandbox", Image: runnerImage}},
				},
				Status: corev1.PodStatus{Phase: corev1.PodPending},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		}

		By("the scheduler marks the sandbox pod unschedulable: the first reconcile stamps the condition + annotation")
		setUnschedulable(sandboxName(name), ns, "0/3 nodes are available")
		reconcile(r, name)
		loop2 := f93aGetLoop(name)
		Expect(conditionTruePresent(loop2, unschedulableLoopPodsCondition)).To(BeTrue())
		Expect(loop2.Annotations[loopUnschedulableSinceAnnotation]).ToNot(BeEmpty())

		By("a direct call to f93aUnschedulableStep on a stuck-but-unchanged pass: verifyRequeue=true, changed=false")
		var changed, vr bool
		r.f93aUnschedulableStep(ctx, f93aGetLoop(name), &changed, &vr)
		Expect(vr).To(BeTrue(),
			"f93aUnschedulableStep sets verifyRequeue on a stuck pass (M9: the requeue is this step's job, not another gate's)")
		Expect(changed).To(BeFalse(),
			"f93aUnschedulableStep does NOT set changed on a stuck-but-unchanged pass (M9: no per-pass status write)")
	})

	It("the bound does NOT trip within the window, and a pod that becomes schedulable again clears the condition + resets the clock", func() {
		clock := time.Now()
		name := "f93a-clear"
		r := f93aReconciler(&clock)
		f93aModelLoop(name)
		reconcile(r, name)

		By("the model proxy pod exists and is then marked unschedulable")
		pod := &corev1.Pod{}
		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Name: proxyPodNameFor(name), Namespace: ns}, pod)
		}, "10s").Should(Succeed())
		setUnschedulable(proxyPodNameFor(name), ns, "0/3 nodes are available")
		reconcile(r, name)
		Expect(conditionTruePresent(f93aGetLoop(name), unschedulableLoopPodsCondition)).To(BeTrue())

		By("the clock advances LESS than the bound: the Loop is NOT failed")
		clock = clock.Add(unschedulableLoopPodsBound - time.Minute)
		reconcile(r, name)
		loopWithin := f93aGetLoop(name)
		Expect(loopWithin.Status.Phase).NotTo(Equal(coxv1alpha1.LoopPhaseFailed),
			"the Loop is not failed within the bound (the bound is a time bound, not a counter)")
		Expect(conditionTruePresent(loopWithin, unschedulableLoopPodsCondition)).To(BeTrue(),
			"the condition stays True within the bound")

		By("the scheduler now schedules the pod (the node pool is fixed): the condition clears and the clock resets")
		pod2 := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: proxyPodNameFor(name), Namespace: ns}, pod2)).To(Succeed())
		for i := range pod2.Status.Conditions {
			c := &pod2.Status.Conditions[i]
			if c.Type == corev1.PodScheduled {
				c.Status = corev1.ConditionTrue
				c.Reason = "Scheduled"
				c.Message = ""
				c.LastTransitionTime = metav1.Now()
				Expect(k8sClient.Status().Update(ctx, pod2)).To(Succeed())
			}
		}
		reconcile(r, name)
		loopCleared := f93aGetLoop(name)
		Expect(conditionTruePresent(loopCleared, unschedulableLoopPodsCondition)).To(BeFalse(),
			"the condition is cleared (False) once the pod is schedulable")
		Expect(loopCleared.Annotations[loopUnschedulableSinceAnnotation]).To(BeEmpty(),
			"the clock annotation is removed (the bound's clock resets)")
	})

	// M5 — the Warning Event fires exactly once (on the transition to True,
	// not on every stuck pass). A FakeRecorder captures the events.
	It("M5: the Warning Event fires exactly once on the stuck transition, not on every pass", func() {
		clock := time.Now()
		name := "f93a-ev1"
		rec := record.NewFakeRecorder(64)
		r := f93aReconcilerWithRecorder(&clock, rec)
		f93aModelLoop(name)
		reconcile(r, name)
		pod := &corev1.Pod{}
		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Name: proxyPodNameFor(name), Namespace: ns}, pod)
		}, "10s").Should(Succeed())

		By("the scheduler marks the proxy pod unschedulable: the first stuck pass fires one event")
		setUnschedulable(proxyPodNameFor(name), ns, "0/3 nodes are available")
		reconcile(r, name)
		loop := f93aGetLoop(name)
		Expect(conditionTruePresent(loop, unschedulableLoopPodsCondition)).To(BeTrue())
		// Drain the recorder buffer fully: exactly one PodUnschedulable event.
		var stuckEvents []string
	drain1:
		for {
			select {
			case ev := <-rec.Events:
				if strings.Contains(ev, unschedulableEventReason) {
					stuckEvents = append(stuckEvents, ev)
				}
			default:
				break drain1
			}
		}
		Expect(stuckEvents).To(HaveLen(1),
			"exactly one PodUnschedulable Warning Event on the transition to True")

		By("a second stuck pass (same stuck set) does NOT fire another event")
		reconcile(r, name)
		// Drain the channel fully and count PodUnschedulable events: must be 0.
		var secondPassEvents []string
	drain2:
		for {
			select {
			case ev := <-rec.Events:
				if strings.Contains(ev, unschedulableEventReason) {
					secondPassEvents = append(secondPassEvents, ev)
				}
			default:
				break drain2
			}
		}
		Expect(secondPassEvents).To(BeEmpty(),
			"no PodUnschedulable Warning Event on the second stuck pass (the event fires once, on the transition)")
	})

	// M1 — a Pending pod that is NOT unschedulable (no PodScheduled
	// condition, or PodScheduled=True, or PodScheduled=False with a
	// NON-Unschedulable reason) is not counted by the F93a read.
	It("M1: a Pending pod that is NOT unschedulable is not counted by the F93a read", func() {
		clock := time.Now()
		name := "f93a-m1"
		r := f93aReconciler(&clock)
		f93aModelLoop(name)
		reconcile(r, name)

		By("the model proxy pod is Pending with NO PodScheduled condition (not yet scheduled, not rejected)")
		pod := &corev1.Pod{}
		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Name: proxyPodNameFor(name), Namespace: ns}, pod)
		}, "10s").Should(Succeed())
		// The pod has no conditions (the scheduler has not acted in envtest).
		// F93a must NOT see this as stuck: podUnschedulable returns false
		// when no PodScheduled condition exists.
		reconcile(r, name)
		loop := f93aGetLoop(name)
		Expect(conditionTruePresent(loop, unschedulableLoopPodsCondition)).To(BeFalse(),
			"a Pending pod with no PodScheduled condition is NOT unschedulable (M1)")
		Expect(loop.Annotations[loopUnschedulableSinceAnnotation]).To(BeEmpty(),
			"the clock annotation is not stamped for a non-unschedulable pod")

		By("the pod is Pending with PodScheduled=True (scheduled but not yet running): still not counted")
		pod2 := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: proxyPodNameFor(name), Namespace: ns}, pod2)).To(Succeed())
		pod2.Status.Conditions = append(pod2.Status.Conditions, corev1.PodCondition{
			Type: corev1.PodScheduled, Status: corev1.ConditionTrue,
			Reason: "Scheduled", LastTransitionTime: metav1.Now(),
		})
		Expect(k8sClient.Status().Update(ctx, pod2)).To(Succeed())
		reconcile(r, name)
		loop = f93aGetLoop(name)
		Expect(conditionTruePresent(loop, unschedulableLoopPodsCondition)).To(BeFalse(),
			"a Pending pod with PodScheduled=True is NOT unschedulable (M1)")

		By("the pod has PodScheduled=False with Reason=SchedulingGated (not Unschedulable): still not counted (M1 discriminator)")
		setScheduledFalse(proxyPodNameFor(name), ns, "SchedulingGated", "pod is gated by a scheduling gate")
		reconcile(r, name)
		loop = f93aGetLoop(name)
		Expect(conditionTruePresent(loop, unschedulableLoopPodsCondition)).To(BeFalse(),
			"a pod with PodScheduled=False Reason=SchedulingGated is NOT unschedulable (M1: only Reason=Unschedulable counts)")
		Expect(loop.Annotations[loopUnschedulableSinceAnnotation]).To(BeEmpty(),
			"the clock annotation is not stamped for a SchedulingGated pod (M1)")
	})

	// M2 — the F93a bound is inert while the Loop is Paused. The clock
	// annotation is removed on entry to Paused so the bound measures a
	// contiguous run while the Loop is unpaused.
	It("M2: a Paused Loop's clock is reset (the annotation is removed on pause)", func() {
		clock := time.Now()
		name := "f93a-m2"
		r := f93aReconciler(&clock)
		f93aModelLoop(name)
		reconcile(r, name)
		pod := &corev1.Pod{}
		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Name: proxyPodNameFor(name), Namespace: ns}, pod)
		}, "10s").Should(Succeed())

		By("the proxy pod is unschedulable: the condition is set and the clock is stamped")
		setUnschedulable(proxyPodNameFor(name), ns, "0/3 nodes are available")
		reconcile(r, name)
		loop := f93aGetLoop(name)
		Expect(conditionTruePresent(loop, unschedulableLoopPodsCondition)).To(BeTrue())
		Expect(loop.Annotations[loopUnschedulableSinceAnnotation]).ToNot(BeEmpty(),
			"the clock annotation is stamped before the pause")

		By("the owner pauses the Loop")
		loop = f93aGetLoop(name)
		loop.Spec.Suspend = true
		Expect(k8sClient.Update(ctx, loop)).To(Succeed())

		By("a reconcile on the paused Loop: the annotation is removed")
		reconcile(r, name)
		loop = f93aGetLoop(name)
		// The paused Loop's phase is Paused (the operator sets it on the next
		// pass). The annotation is removed on entry to Paused.
		if loop.Status.Phase == coxv1alpha1.LoopPhasePaused {
			Expect(loop.Annotations[loopUnschedulableSinceAnnotation]).To(BeEmpty(),
				"the clock annotation is removed when the Loop enters Paused (M2)")
		}

		By("the owner resumes the Loop and advances the clock past the bound: the Loop does NOT fail (the clock restarted on resume)")
		loop = f93aGetLoop(name)
		loop.Spec.Suspend = false
		Expect(k8sClient.Update(ctx, loop)).To(Succeed())
		// The clock was paused for the full bound duration + extra, but the
		// annotation was removed on pause and re-stamped fresh on resume, so
		// the bound has not elapsed since the resume.
		clock = clock.Add(unschedulableLoopPodsBound + time.Minute)
		reconcile(r, name)
		loop = f93aGetLoop(name)
		Expect(loop.Status.Phase).NotTo(Equal(coxv1alpha1.LoopPhaseFailed),
			"the Loop is not failed: the bound's clock restarted on resume (M2)")
	})

	// M4 — a foreign pod (not owned by the Loop) that is unschedulable
	// is ignored by the F93a read. The spec calls loopUnschedulablePods
	// directly (no reconcile) so the operator's own proxy pod does NOT
	// exist; the only pod with the proxy's name is the foreign one.
	It("M4: a foreign pod that is unschedulable is not counted by the F93a read", func() {
		clock := time.Now()
		name := "f93a-m4"
		r := f93aReconciler(&clock)
		// Create the Loop but do NOT reconcile it: no operator-created pods
		// exist. The only pod with the proxy's name is the foreign pod the
		// spec creates.
		f93aModelLoop(name)

		By("a bare pod named proxyPodName(name) with NO ownerReferences is created and unschedulable")
		proxyName := proxyPodNameFor(name)
		foreignPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      proxyName,
				Namespace: ns,
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "foreign", Image: "busybox"}},
			},
			Status: corev1.PodStatus{Phase: corev1.PodPending},
		}
		Expect(k8sClient.Create(ctx, foreignPod)).To(Succeed())
		setUnschedulable(proxyName, ns, "0/3 nodes are available")

		By("loopUnschedulablePods returns empty (the pod is not Loop-owned)")
		stuck, err := r.loopUnschedulablePods(ctx, f93aGetLoop(name))
		Expect(err).NotTo(HaveOccurred())
		Expect(stuck).To(BeEmpty(),
			"a pod with the proxy's name but no ownerReferences is not counted (M4: IsControlledBy check)")

		By("positive control: the same pod with a controller ownerReference to the Loop IS counted")
		loop := f93aGetLoop(name)
		// Re-read the pod (setUnschedulable bumped the resourceVersion):
		// the Update below must not conflict.
		freshPod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: proxyName, Namespace: ns}, freshPod)).To(Succeed())
		Expect(controllerutil.SetControllerReference(loop, freshPod, k8sClient.Scheme())).To(Succeed())
		Expect(k8sClient.Update(ctx, freshPod)).To(Succeed())
		stuck2, err2 := r.loopUnschedulablePods(ctx, f93aGetLoop(name))
		Expect(err2).NotTo(HaveOccurred())
		Expect(stuck2).To(HaveLen(1),
			"the same pod WITH a controller ownerReference to the Loop IS counted (positive control)")
		Expect(stuck2[0].name).To(Equal(proxyName))
	})

	// M3 — the egress proxy pod is bounded. A Loop with an external repo
	// (which creates the egress proxy) has its egress pod wedged.
	It("M3: the egress proxy pod is surfaced and bounded", func() {
		clock := time.Now()
		name := "f93a-m3"
		r := f93aReconciler(&clock)
		// A Loop with an external repo (the shape that creates the egress
		// proxy) and no endpoint (no model proxy — the egress proxy is the
		// only proxy). The workspace repo is external (git-clone requires
		// the egress proxy).
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      loopGoal,
				Workspace: coxv1alpha1.Workspace{Repo: githubRepoURL, Ref: loopRef},
				Agent:     coxv1alpha1.AgentConfig{Image: runnerImage, Model: testModel},
			},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		reconcile(r, name)

		By("the egress proxy pod exists")
		egressName := egressProxyPodName(name)
		pod := &corev1.Pod{}
		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Name: egressName, Namespace: ns}, pod)
		}, "10s").Should(Succeed())

		By("the scheduler marks the egress proxy pod unschedulable")
		setUnschedulable(egressName, ns, "0/3 nodes are available: 3 node(s) didn't match Pod's node affinity/selector")
		reconcile(r, name)
		loop2 := f93aGetLoop(name)
		Expect(conditionTruePresent(loop2, unschedulableLoopPodsCondition)).To(BeTrue(),
			"the PodUnschedulable condition is True for the unschedulable egress proxy pod (M3)")
		Expect(loop2.Annotations[loopUnschedulableSinceAnnotation]).ToNot(BeEmpty(),
			"the clock annotation is stamped (M3)")
		// The condition message names the egress proxy pod.
		var stuckCond *metav1.Condition
		for i := range loop2.Status.Conditions {
			if loop2.Status.Conditions[i].Type == unschedulableLoopPodsCondition {
				stuckCond = &loop2.Status.Conditions[i]
			}
		}
		Expect(stuckCond).NotTo(BeNil())
		Expect(stuckCond.Message).To(ContainSubstring(egressName),
			"the condition message names the stuck egress proxy pod (M3)")

		By("the clock advances past the bound: the Loop fails")
		clock = clock.Add(unschedulableLoopPodsBound + time.Second)
		reconcile(r, name)
		loop3 := f93aGetLoop(name)
		Expect(loop3.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseFailed),
			"the Loop is Failed past the bound for the unschedulable egress proxy (M3)")
	})
})

// f93aReconcilerWithRecorder is f93aReconciler with a FakeRecorder (for the
// M5 event spec). The rest of the wiring is identical.
func f93aReconcilerWithRecorder(clock *time.Time, rec *record.FakeRecorder) *LoopReconciler {
	return &LoopReconciler{
		CNIProber:              testCNIProber(),
		Client:                 k8sClient,
		Scheme:                 k8sClient.Scheme(),
		AllowUnenforced:        true,
		AllowUnenforcedNetwork: true,
		Recorder:               rec,
		now:                    func() metav1.Time { return metav1.NewTime(*clock) },
	}
}
