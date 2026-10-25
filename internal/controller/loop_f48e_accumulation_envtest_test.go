package controller

// F48e: the sandbox infra-attempt count must ACCUMULATE across the operator's
// own Sandbox recycles and trip at the bound (pi3's retry-bound audit §2/§6).
//
// Nothing in the tree did this before this file. Every sandbox-bound spec
// either SEEDS the count (loop_f48b_mount_envtest_test.go,
// loop_f48_pending_envtest_test.go) or calls sandboxFailure /
// applySandboxFailure / ensureSandbox DIRECTLY (the F6 suite says so in its own
// header) — which is exactly the combination that let the shipped P1 through:
// #266's kind run showed the operator re-emitting "attempt 1 of 3" on every
// recreate (.samples/f48b/08-operator-log-f48b.log, 11 x attempt 1 of 3 and
// 0 x attempt 2 of 3) while every existing spec stayed green, because none of
// them let Reconcile do the delete-and-recreate loop the real operator does.
//
// So spec A below starts at zero, drives Reconcile through
// sandboxInfraRetries+1 cycles (the operator deletes the Sandbox, the next
// reconcile recreates it, the pods are planted the way the sandbox controller
// and the kubelet would) and asserts the persisted count, the run key, the
// per-attempt retry Event and the terminal Failed:SandboxFailed. Spec B pins
// the other half of the UID guard: a stale pod is neither counted again nor
// treated as a reset.

import (
	"context"
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cov "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/cni"
)

var _ = Describe("F48e: the sandbox infra count accumulates across the operator's own recycles", func() {
	ctx := context.Background()

	f48eNS := func(prefix string) string {
		ns := prefix + "-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		})
		return ns
	}

	// f48eReconciler is the I74 fixture, copied rather than called: both it and
	// i74PrimeProxy are closures inside the I74 Describe (loop_i74_sandbox_
	// terminal_test.go:110-150). Every gate is satisfied so a reconcile reaches
	// the phase machine instead of a hold (D30 AllowUnenforced, D38 CNI
	// enforced, D35a proxy Ready+owned). The duplication is this suite's
	// established pattern (the P2f fixture is the same copy).
	f48eReconciler := func(rec *record.FakeRecorder) *LoopReconciler {
		r := &LoopReconciler{
			Client:          k8sClient,
			Scheme:          k8sClient.Scheme(),
			apiReader:       k8sClient,
			AllowUnenforced: true,
			CNIProber:       cni.NewFakeProber(),
			Recorder:        rec,
			readPhaseClaim:  func(context.Context, *cov.Loop) (*PhaseClaim, error) { return nil, nil },
		}
		r.CNIProber.(*cni.FakeProber).SetResult(cni.CNIProbeResult{Reason: cni.ReasonCNIEnforced})
		return r
	}

	// f48ePrimeProxy creates the model secret the D33 proxy needs and lets the
	// operator build the real proxy pod, then marks it Ready (the D35a gate's
	// peer) — the I74 fixture's i74PrimeProxy.
	f48ePrimeProxy := func(r *LoopReconciler, loop *cov.Loop) {
		_ = k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: p2fModelSecret, Namespace: loop.Namespace},
			StringData: map[string]string{modelAPIKey: "f48e-dummy", modelBaseURL: p2fModelEndpoint},
		})
		Expect(r.ensureProxy(ctx, loop)).To(Succeed())
		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: proxyPodName(loop.Name), Namespace: loop.Namespace}, pod)).To(Succeed(),
			"F48e: the operator's ensureProxy must have created the proxy pod")
		now := metav1.Now()
		pod.Status.Conditions = []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: now},
		}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	}

	f48eCreateLoop := func(ns, name string) *cov.Loop {
		loop := &cov.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: cov.LoopSpec{
				Goal:      "F48e accumulation spec",
				Workspace: cov.Workspace{Repo: testRepoURL},
				Agent: cov.AgentConfig{
					Image:             runnerImage,
					Model:             testModel,
					EndpointSecretRef: p2fModelSecret,
					ModelEndpoint:     p2fModelEndpoint,
				},
			},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		return loop
	}

	f48eReconcile := func(r *LoopReconciler, ns, name string) {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		Expect(err).NotTo(HaveOccurred(), "F48e: reconcile %s/%s", ns, name)
	}

	f48eGet := func(ns, name string) *cov.Loop {
		got := &cov.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, got)).To(Succeed())
		return got
	}

	f48eSandbox := func(ns, name string) *sandboxv1beta1.Sandbox {
		sb := &sandboxv1beta1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: sandboxName(name)}, sb)).To(Succeed())
		return sb
	}

	// f48ePlantFailedPod models the two controllers envtest does not run: the
	// sandbox controller (which creates the pod for the Sandbox the operator
	// just made) and the kubelet (which fails it). The ownerReference UID is
	// what sandboxFailure's F14 guard requires (metav1.IsControlledBy against
	// the CURRENT Sandbox); a pod with no ownerReference would reconcile
	// forever and count nothing, and would look green if the spec only
	// asserted "no error". The container is named "agent"
	// (agentContainerNameS4) so the decision names the container rather than
	// falling back to the weaker pod-level branch.
	//
	// The previous cycle's pod is deleted first: the pod name is reused (the
	// Sandbox's name) and envtest runs no garbage collector, while on a real
	// cluster the Sandbox delete's background propagation removes the pod
	// (DeleteSandboxForRetry). Deleting it is the faithful model.
	f48ePlantFailedPod := func(ns, name string) *sandboxv1beta1.Sandbox {
		sb := f48eSandbox(ns, name)
		old := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: sandboxName(name)}}
		if err := k8sClient.Delete(ctx, old); err != nil && !apierrors.IsNotFound(err) {
			Expect(err).NotTo(HaveOccurred(), "F48e: drop the previous cycle's pod (the GC's job on a real cluster)")
		}
		controller := true
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: ns, Name: sandboxName(name),
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: sandboxv1beta1.GroupVersion.String(), Kind: "Sandbox",
					Name: sb.Name, UID: sb.UID, Controller: &controller,
				}},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "agent", Image: "example.com/cox-agent:v1"}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		// A storable, non-empty reason (F89/F89b own the empty/unstorable
		// shapes). Create does not set status in envtest, so it goes through
		// the status subresource.
		pod.Status = corev1.PodStatus{
			Phase: corev1.PodFailed,
			ContainerStatuses: []corev1.ContainerStatus{{Name: "agent",
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}}}},
		}
		Expect(k8sClient.SubResource("status").Update(ctx, pod)).To(Succeed())
		return sb
	}

	// f48eRetryAttempts counts the per-attempt retry Events
	// ("... (attempt N of M); recreating the sandbox ...") into a map, so an
	// assertion on the count carries its own positive control (the attempts
	// that must be present, not just an absence).
	f48eRetryAttempts := func(rec *record.FakeRecorder) map[int]int {
		got := map[int]int{}
		for _, e := range drainEvents(rec) {
			i := strings.Index(e, "attempt ")
			if i < 0 {
				continue
			}
			var n int
			if _, err := fmt.Sscanf(e[i:], "attempt %d of", &n); err == nil {
				got[n]++
			}
		}
		return got
	}

	It("A: accumulates across the operator's own Sandbox recycles and trips at the bound", func() {
		ns := f48eNS("f48e-accum")
		const name = "f48e-a"
		rec := record.NewFakeRecorder(64)
		r := f48eReconciler(rec)
		f48eCreateLoop(ns, name)
		f48ePrimeProxy(r, f48eGet(ns, name))

		f48eReconcile(r, ns, name) // bootstrap: the operator CREATES Sandbox #1
		Expect(string(f48eSandbox(ns, name).UID)).NotTo(BeEmpty())

		// The run key the operator will record, captured before the loop: the
		// exhausted branch moves desiredPhase to Failed AFTER the key compare,
		// so sandboxRunKey(read after the terminal pass) would read
		// "<iter>/Failed" and the assertion would fail on a CORRECT
		// implementation.
		bootstrap := f48eGet(ns, name)
		runKey := sandboxRunKey(bootstrap)

		var seen []types.UID
		var got *cov.Loop
		for attempt := int32(1); attempt <= sandboxInfraRetries+1; attempt++ {
			sb := f48ePlantFailedPod(ns, name) // the pod belongs to THIS Sandbox
			seen = append(seen, sb.UID)
			if attempt > 1 {
				Expect(sb.UID).NotTo(Equal(seen[len(seen)-2]),
					"the retry really recreated the Sandbox (a new UID)")
			}
			f48eReconcile(r, ns, name) // read the failure, count it, delete the Sandbox (or fail the Loop)

			got = f48eGet(ns, name) // read the count back from the API, never an in-memory value
			Expect(got.Status.Agent).NotTo(BeNil())
			Expect(got.Status.Agent.InfraAttempts).To(Equal(attempt),
				"the count must ACCUMULATE across the operator's own recycles")
			Expect(got.Status.Agent.InfraSandboxUID).To(Equal(string(sb.UID)),
				"the operator counted the failure it read from the Sandbox it then deleted")
			Expect(got.Status.Agent.InfraEpisode).To(Equal(runKey),
				"every counted failure belongs to the SAME run (the key never moves)")
			if attempt <= sandboxInfraRetries {
				// No run boundary: the episode is the only thing that can explain
				// the count. (These hold only while the Loop is still alive.)
				Expect(got.Status.Iteration).To(Equal(bootstrap.Status.Iteration))
				Expect(got.Status.DesiredPhase).To(Equal(bootstrap.Status.DesiredPhase))
				f48eReconcile(r, ns, name) // the pod is gone: recreates the NEXT Sandbox, no decision
				Expect(sandboxName(name)).NotTo(BeEmpty())
			}
		}

		Expect(seen).To(HaveLen(int(sandboxInfraRetries + 1)))
		Expect(uniqueSandboxUIDs(seen)).To(HaveLen(len(seen)),
			"three distinct Sandbox UIDs: the operator recreated it each time")
		Expect(got.Status.Phase).To(Equal(cov.LoopPhaseFailed))
		failed := findConditionFn(got.Status.Conditions, string(cov.LoopPhaseFailed))
		Expect(failed).NotTo(BeNil())
		Expect(failed.Reason).To(Equal(sandboxInfraFailedCondition))

		By("the retry Event names each attempt once (the F48b defect re-emitted 'attempt 1 of 3' forever)")
		Expect(f48eRetryAttempts(rec)).To(Equal(map[int]int{1: 1, 2: 1}))
	})

	It("B: a stale pod is neither counted again nor treated as a reset", func() {
		ns := f48eNS("f48e-stale")
		const name = "f48e-b"
		rec := record.NewFakeRecorder(64)
		r := f48eReconciler(rec)
		f48eCreateLoop(ns, name)
		f48ePrimeProxy(r, f48eGet(ns, name))

		f48eReconcile(r, ns, name) // bootstrap: Sandbox #1
		first := f48ePlantFailedPod(ns, name)
		f48eReconcile(r, ns, name) // counted: attempt 1, then the Sandbox is deleted
		Expect(f48eGet(ns, name).Status.Agent.InfraAttempts).To(Equal(int32(1)))

		By("the previous cycle's pod is still present when the operator recreates the Sandbox")
		// envtest runs no GC, so the Failed pod owned by Sandbox #1 survives the
		// delete; on a real cluster it lingers too (background propagation),
		// which is the window the F14 guard exists for.
		Expect(string(first.UID)).NotTo(BeEmpty())
		f48eReconcile(r, ns, name) // recreates Sandbox #2 with the stale pod readable
		stale := f48eGet(ns, name)
		Expect(stale.Status.Agent.InfraAttempts).To(Equal(int32(1)),
			"a pod not controlled by the CURRENT Sandbox is no decision: not counted again")
		Expect(stale.Status.Agent.InfraSandboxUID).To(Equal(string(first.UID)))
		Expect(stale.Status.Phase).NotTo(Equal(cov.LoopPhaseFailed))

		By("a fresh failure from the new Sandbox is attempt 2")
		f48ePlantFailedPod(ns, name) // deletes the stale pod, plants one owned by Sandbox #2
		f48eReconcile(r, ns, name)
		second := f48eGet(ns, name)
		Expect(second.Status.Agent.InfraAttempts).To(Equal(int32(2)),
			"the UID guard skips a re-read; it does not reset the count")
		Expect(second.Status.Agent.InfraEpisode).To(Equal(sandboxRunKey(second)))
	})
})

// uniqueSandboxUIDs returns the distinct UIDs in order-independent form (a
// recreate assertion for spec A).
func uniqueSandboxUIDs(uids []types.UID) []types.UID {
	seen := map[types.UID]struct{}{}
	var out []types.UID
	for _, u := range uids {
		if _, ok := seen[u]; ok {
			continue
		}
		seen[u] = struct{}{}
		out = append(out, u)
	}
	return out
}
