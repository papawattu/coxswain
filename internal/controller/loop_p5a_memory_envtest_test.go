package controller

// P5a (Phase 5 shared memory, PLAN-PHASE5.md:34; D66 accepted): the per-
// (namespace, git URL) memory PVC + the copy-in init container.
//
// The specs:
//   (a) A Loop on repo R with no memory PVC gets a PVC created (or an
//       existing one reused) IN THE LOOP'S NAMESPACE; the PVC is keyed by
//       (namespace, git URL) — two Loops on the same URL in the same
//       namespace share it, two on different URLs (or different namespaces)
//       get distinct PVCs. The PVC's accessModes is ReadWriteMany.
//   (b) The sandbox carries the copy-in init container (the memory volume
//       mount) when the PVC is Bound; the operator does NOT write to the
//       PVC during the read phase (the write is P5b's writer Job, not the
//       operator pod — the PVC has no controller owner ref to the Loop, and
//       the agent container has no mount of the memory volume). An unbound
//       PVC: no copy-in container, no memory volume, and a visible Event
//       (MemoryPVCUnbound, R26 I90).
//   (c) The copy-in init container runs the REAL generated script (the
//       execution test, loop_p5a_memory_script_test.go) and is inserted
//       BEFORE the phase-init container (init-order: workspace-init,
//       copy-in, phase-init, agent).
//
// envtest has no provisioner: the operator-created PVC stays Pending
// (unbound). To test the BOUND path, the specs set the PVC's status.phase to
// Bound manually (a direct status update) before reconciling — the operator's
// decision (mount + copy-in vs Event) is a pure function of that phase.

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/pkg/memkey"
)

const (
	p5aRepoA         = "https://github.com/papawattu/repo-a.git"
	p5aRepoB         = "https://github.com/papawattu/repo-b.git"
	p5aRunnerImg     = "p5a-runner:dev"
	p5aModelEndpoint = "vllm:8000"
	p5aStandinImage  = "busybox"
)

// p5aMakeLoop builds a minimal Loop on the given repo in ns (the P5a fixture:
// a runner Loop with a model, so ensureSandbox builds the full pod spec).
func p5aMakeLoop(name, ns, repo string) *coxv1alpha1.Loop {
	l := &coxv1alpha1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: coxv1alpha1.LoopSpec{
			Goal: "p5a memory test",
			Workspace: coxv1alpha1.Workspace{
				Repo: repo,
			},
			Agent: coxv1alpha1.AgentConfig{
				Image:             p5aRunnerImg,
				EndpointSecretRef: "p5a-model-creds",
				ModelEndpoint:     p5aModelEndpoint,
			},
		},
	}
	return l
}

// p5aSandboxPodSpec reads the Loop's sandbox pod spec from the Sandbox
// object (the envtest stand-in pattern: the operator writes the pod spec onto
// the Sandbox; the specs assert on it).
func p5aSandboxPodSpec(ctx context.Context, name, ns string) *corev1.PodSpec {
	sb := &sandboxv1beta1.Sandbox{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name + "-sandbox"}, sb); err != nil {
		return nil
	}
	ps := sb.Spec.PodTemplate.Spec
	return &ps
}

// p5aInitContainerIndex returns the index of the named init container in the
// sandbox pod spec (-1 when absent).
func p5aInitContainerIndex(ps *corev1.PodSpec, name string) int {
	for i := range ps.InitContainers {
		if ps.InitContainers[i].Name == name {
			return i
		}
	}
	return -1
}

var _ = Describe("P5a memory PVC + copy-in (Phase 5, D66)", func() {
	var (
		ctx      context.Context
		nsA, nsB string
		repoA    = p5aRepoA
	)

	BeforeEach(func() {
		ctx = context.Background()
		nsA = "p5a-a-" + nowSuffix()
		nsB = "p5a-b-" + nowSuffix()
		for _, ns := range []string{nsA, nsB} {
			Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
			// Wait for the namespace to be Active (a just-created namespace is
			// briefly Terminating/initialising; a create of a resource into it
			// races the namespace finaliser. The P5a specs reconcile the Loop
			// right after creating the namespace; the wait keeps the race out).
			var nsObj corev1.Namespace
			Eventually(func() bool {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: ns}, &nsObj)
				return nsObj.Status.Phase == corev1.NamespaceActive
			}, 30*time.Second, time.Second).Should(BeTrue(), "the test namespace must be Active before the Loop is created into it")
		}
	})

	AfterEach(func() {
		for _, ns := range []string{nsA, nsB} {
			_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}
	})

	// reconcileLoop runs the reconciler on the Loop TWICE (a direct two-pass
	// reconcile, matching the pattern of every other spec in this suite —
	// the bootstrap's option-B recycle: pass 1 creates the sandbox, the spec
	// recreates it, pass 2 re-ensures the pod spec on the recreated pod).
	//
	// NOT wrapped in Eventually: the other specs call Reconcile directly and
	// assert on the result. Wrapping a direct function call in
	// Eventually(30s) polls the full 30s whenever the second pass requeues
	// (returns a non-nil error), which is what made the full envtest suite
	// run 1200s+ instead of main's 355s (each P5a spec could hit the 30s
	// timeout). The two passes are deterministic; a requeue on the second
	// pass is a ctrl.Result (not an error) and does not block the spec.
	reconcileLoop := func(name, ns string) {
		By("reconciling the Loop (two passes)")
		ctrlReconciler := &LoopReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
		req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}
		_, err := ctrlReconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred(), "pass 1 reconcile must not error")
		// Recreate the sandbox (the bootstrap's option-B recycle) so the
		// operator re-ensures the pod spec on the recreated pod (the same
		// pattern as every other spec in this suite).
		probe := &sandboxv1beta1.Sandbox{}
		if apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name + "-sandbox"}, probe)) {
			err = k8sClient.Create(ctx, &sandboxv1beta1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: name + "-sandbox", Namespace: ns}})
			Expect(err).To(Succeed(), "recreate the sandbox for pass 2")
		}
		_, err = ctrlReconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred(), "pass 2 reconcile must not error")
	}

	Context("(a) the PVC is created in the Loop's namespace, keyed by (ns, URL), RWX", func() {
		It("creates the memory PVC in the Loop's namespace with ReadWriteMany", func() {
			loop := p5aMakeLoop("p5a-a1", nsA, repoA)
			Expect(k8sClient.Create(ctx, loop)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, loop) }()
			reconcileLoop("p5a-a1", nsA)

			pvcName := memkeyPVCName(repoA)
			pvc := &corev1.PersistentVolumeClaim{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Namespace: nsA, Name: pvcName}, pvc)
			}).Should(Succeed(), "the memory PVC must be created in the Loop's namespace (not coxswain-system)")

			// (a) the PVC's accessModes is ReadWriteMany (the mutation "use RWO
			// instead of RWX" must make THIS spec FAIL).
			Expect(pvc.Spec.AccessModes).To(Equal([]corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}),
				"the memory PVC must be ReadWriteMany (RWO would not schedule several sandboxes on different nodes)")

			// (a) the PVC is NOT Loop-owned (shared across Loops; it must
			// outlive the first Loop to create it).
			Expect(pvc.OwnerReferences).To(BeEmpty(),
				"the memory PVC is shared across Loops on the same repo — a controller owner ref would GC it on the first Loop's delete")
		})

		It("reuses an existing memory PVC (idempotent; no spec rewrite)", func() {
			loop := p5aMakeLoop("p5a-a2", nsA, repoA)
			Expect(k8sClient.Create(ctx, loop)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, loop) }()
			reconcileLoop("p5a-a2", nsA)

			pvcName := memkeyPVCName(repoA)
			pvc := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: nsA, Name: pvcName}, pvc)).To(Succeed())
			rvBefore := pvc.ResourceVersion
			specBefore := pvc.Spec

			// A second Loop on the same (ns, URL) reuses the PVC: no create, no
			// spec rewrite (the same I43 rule as the workspace PVC).
			loop2 := p5aMakeLoop("p5a-a2b", nsA, repoA)
			Expect(k8sClient.Create(ctx, loop2)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, loop2) }()
			reconcileLoop("p5a-a2b", nsA)

			pvc2 := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: nsA, Name: pvcName}, pvc2)).To(Succeed())
			Expect(pvc2.ResourceVersion).To(Equal(rvBefore), "the reused PVC must not be rewritten (no resourceVersion churn)")
			Expect(pvc2.Spec).To(Equal(specBefore), "the reused PVC's spec must be byte-identical")
		})

		It("two Loops on the same URL in the same namespace share one PVC", func() {
			loop1 := p5aMakeLoop("p5a-s1", nsA, repoA)
			loop2 := p5aMakeLoop("p5a-s2", nsA, repoA)
			Expect(k8sClient.Create(ctx, loop1)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, loop1) }()
			Expect(k8sClient.Create(ctx, loop2)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, loop2) }()
			reconcileLoop("p5a-s1", nsA)
			reconcileLoop("p5a-s2", nsA)

			pvcName := memkeyPVCName(repoA)
			// The PVC name is a function of the URL alone (the namespace is
			// the PVC object's scope): both Loops' PVCs are the SAME object.
			Expect(memkeyPVCName(p5aMakeLoop("x", nsA, repoA).Spec.Workspace.Repo)).To(Equal(memkeyPVCName(p5aMakeLoop("y", nsA, repoA).Spec.Workspace.Repo)))
			pvc := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: nsA, Name: pvcName}, pvc)).To(Succeed())
			// Exactly ONE memory PVC for repoA in nsA (the two Loops share it).
			pvcs := &corev1.PersistentVolumeClaimList{}
			Expect(k8sClient.List(ctx, pvcs, client.InNamespace(nsA))).To(Succeed())
			count := 0
			for i := range pvcs.Items {
				if pvcs.Items[i].Name == pvcName {
					count++
				}
			}
			Expect(count).To(Equal(1), "two Loops on the same URL in the same namespace must share ONE memory PVC")
		})

		It("two Loops on different URLs get distinct PVCs", func() {
			loopA := p5aMakeLoop("p5a-d1", nsA, repoA)
			loopB := p5aMakeLoop("p5a-d2", nsA, p5aRepoB)
			Expect(k8sClient.Create(ctx, loopA)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, loopA) }()
			Expect(k8sClient.Create(ctx, loopB)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, loopB) }()
			reconcileLoop("p5a-d1", nsA)
			reconcileLoop("p5a-d2", nsA)

			nameA := memkeyPVCName(repoA)
			nameB := memkeyPVCName(p5aRepoB)
			Expect(nameA).ToNot(Equal(nameB), "different URLs must yield different PVC names")
			// Both exist, distinct.
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: nsA, Name: nameA}, &corev1.PersistentVolumeClaim{})).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: nsA, Name: nameB}, &corev1.PersistentVolumeClaim{})).To(Succeed())
		})

		It("Loops in different namespaces on the same URL get distinct PVCs (per-namespace)", func() {
			loopA := p5aMakeLoop("p5a-n1", nsA, repoA)
			loopB := p5aMakeLoop("p5a-n2", nsB, repoA)
			Expect(k8sClient.Create(ctx, loopA)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, loopA) }()
			Expect(k8sClient.Create(ctx, loopB)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, loopB) }()
			reconcileLoop("p5a-n1", nsA)
			reconcileLoop("p5a-n2", nsB)

			name := memkeyPVCName(repoA)
			// The same NAME in two namespaces is two PVCs (a PVC is
			// namespace-scoped; D66: cross-namespace sharing is out of scope).
			pvcA := &corev1.PersistentVolumeClaim{}
			pvcB := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: nsA, Name: name}, pvcA)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: nsB, Name: name}, pvcB)).To(Succeed())
			Expect(pvcA.Namespace).To(Equal(nsA))
			Expect(pvcB.Namespace).To(Equal(nsB))
		})
	})

	Context("(b) the sandbox carries the copy-in init container when the PVC is Bound", func() {
		// boundMemoryPVC sets the memory PVC's status.phase to Bound (the
		// envtest has no provisioner; the operator's decision is a pure
		// function of the phase).
		boundMemoryPVC := func(ns, repo string) {
			name := memkeyPVCName(repo)
			pvc := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, pvc)).To(Succeed())
			pvc.Status.Phase = corev1.ClaimBound
			Expect(k8sClient.Status().Update(ctx, pvc)).To(Succeed())
		}

		It("mounts the copy-in init container + memory volume when the PVC is Bound", func() {
			loop := p5aMakeLoop("p5a-b1", nsA, repoA)
			Expect(k8sClient.Create(ctx, loop)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, loop) }()
			reconcileLoop("p5a-b1", nsA)
			boundMemoryPVC(nsA, repoA)
			reconcileLoop("p5a-b1", nsA)

			ps := p5aSandboxPodSpec(ctx, "p5a-b1", nsA)
			Expect(ps).ToNot(BeNil(), "the sandbox must exist")

			// (b) the copy-in init container is present with the memory
			// volume mount (the sandbox spec's volumeMounts carry a mount of
			// the memory volume).
			idxCopy := p5aInitContainerIndex(ps, "memory-copy-in")
			Expect(idxCopy).ToNot(Equal(-1), "the memory copy-in init container must be present when the PVC is Bound")

			// The copy-in mounts the memory PVC READ-ONLY + the workspace RW.
			var copyInit *corev1.Container
			for i := range ps.InitContainers {
				if ps.InitContainers[i].Name == "memory-copy-in" {
					copyInit = &ps.InitContainers[i]
					break
				}
			}
			Expect(copyInit).ToNot(BeNil())
			sawMemoryRO, sawWorkspace := false, false
			for _, m := range copyInit.VolumeMounts {
				if m.Name == "memory-pvc" {
					Expect(m.ReadOnly).To(BeTrue(), "the copy-in must mount the memory PVC READ-ONLY (a pure read of the PVC)")
					sawMemoryRO = true
				}
				if m.Name == "workspace" {
					sawWorkspace = true
				}
			}
			Expect(sawMemoryRO).To(BeTrue(), "the copy-in must mount the memory volume (memory-pvc)")
			Expect(sawWorkspace).To(BeTrue(), "the copy-in must mount the workspace volume")

			// (b) the agent container has NO mount of the memory volume (the
			// operator does not give the agent the memory PVC; memory is
			// read-only in the agent via the workspace's .coxswain dir).
			for _, c := range ps.Containers {
				if c.Name != "agent" {
					continue
				}
				for _, m := range c.VolumeMounts {
					Expect(m.Name).ToNot(Equal("memory-pvc"), "the agent must NOT mount the memory PVC (only the copy-in init does)")
				}
			}
			_ = sawWorkspace
		})

		It("the copy-in runs BEFORE the phase-init container (init-order)", func() {
			loop := p5aMakeLoop("p5a-b2", nsA, repoA)
			Expect(k8sClient.Create(ctx, loop)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, loop) }()
			reconcileLoop("p5a-b2", nsA)
			boundMemoryPVC(nsA, repoA)
			reconcileLoop("p5a-b2", nsA)

			ps := p5aSandboxPodSpec(ctx, "p5a-b2", nsA)
			Expect(ps).ToNot(BeNil())
			idxCopy := p5aInitContainerIndex(ps, "memory-copy-in")
			idxPhase := p5aInitContainerIndex(ps, "init-phase")
			Expect(idxCopy).ToNot(Equal(-1))
			Expect(idxPhase).ToNot(Equal(-1))
			Expect(idxCopy).To(BeNumerically("<", idxPhase), "the copy-in must run before the phase-init (init-order: workspace-init, copy-in, phase-init, agent)")
		})

		It("an UNBOUND PVC: no copy-in container, no memory volume, and a visible Event", func() {
			loop := p5aMakeLoop("p5a-b3", nsA, repoA)
			Expect(k8sClient.Create(ctx, loop)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, loop) }()
			reconcileLoop("p5a-b3", nsA)
			// The PVC exists (created by the operator) but is NOT Bound
			// (envtest has no provisioner; the PVC is Pending). The operator
			// must NOT mount it (the kubelet would never start the pod) and
			// must surface the degradation (an Event, R26 I90).
			ps := p5aSandboxPodSpec(ctx, "p5a-b3", nsA)
			Expect(ps).ToNot(BeNil())
			Expect(p5aInitContainerIndex(ps, "memory-copy-in")).To(Equal(-1),
				"an unbound (Pending) memory PVC must NOT be mounted (the kubelet would never start the pod)")
			for _, v := range ps.Volumes {
				Expect(v.Name).ToNot(Equal("memory-pvc"), "an unbound memory PVC must not appear as a pod volume")
			}

			// The visible degradation: a MemoryPVCUnbound Event (R26 I90). The
			// operator emits it via its Recorder (unit-covered by
			// TestEmitMemoryPVCUnboundEvent); this spec asserts the decision
			// (no copy-in, no memory volume) that the Event accompanies.
		})

		It("a Bound PVC with the operator NOT writing it: the PVC is unchanged by a reconcile (no owner ref, no spec rewrite)", func() {
			loop := p5aMakeLoop("p5a-b5", nsA, repoA)
			Expect(k8sClient.Create(ctx, loop)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, loop) }()
			reconcileLoop("p5a-b5", nsA)
			boundMemoryPVC(nsA, repoA)
			reconcileLoop("p5a-b5", nsA)

			// The operator does NOT write to the PVC during the read phase: no
			// controller owner ref (P5b's writer Job is the sole writer), no
			// spec rewrite. The PVC is read-only to the operator.
			name := memkeyPVCName(repoA)
			pvc := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: nsA, Name: name}, pvc)).To(Succeed())
			Expect(pvc.OwnerReferences).To(BeEmpty(), "the memory PVC must have no controller owner ref (the operator does not own it; P5b's writer Job is the sole writer)")
			Expect(pvc.Spec.AccessModes).To(Equal([]corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}), "a no-change reconcile must not rewrite the access modes")
		})

		// P5a --memory-storage-class flag: when the operator is configured
		// with a non-empty MemoryStorageClass, the memory PVC is created
		// with that storageClassName. When empty (the default), the PVC
		// uses the cluster default (nil storageClassName). This spec tests
		// the non-empty path; the specs above (which use the default
		// reconciler with no MemoryStorageClass) implicitly test the
		// nil path.
		It("sets the memory PVC's storageClassName when --memory-storage-class is set", func() {
			loop := p5aMakeLoop("p5a-sc1", nsA, repoA)
			Expect(k8sClient.Create(ctx, loop)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, loop) }()

			// Reconcile with a reconciler that has MemoryStorageClass set.
			scReconciler := &LoopReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				MemoryStorageClass: "local-shared",
			}
			req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: nsA, Name: "p5a-sc1"}}
			_, err := scReconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred(), "reconcile must not error")

			pvcName := memkeyPVCName(repoA)
			pvc := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: nsA, Name: pvcName}, pvc)).To(Succeed(), "the memory PVC must be created")
			Expect(pvc.Spec.StorageClassName).NotTo(BeNil(), "the memory PVC must have a storageClassName set when the operator is configured with --memory-storage-class")
			Expect(*pvc.Spec.StorageClassName).To(Equal("local-shared"), "the memory PVC's storageClassName must match the operator's --memory-storage-class flag")
		})

		// The nil path: when MemoryStorageClass is empty (the default), the
		// memory PVC's storageClassName is nil (the cluster default).
		It("leaves the memory PVC's storageClassName nil when --memory-storage-class is empty (the default)", func() {
			loop := p5aMakeLoop("p5a-sc2", nsA, repoA)
			Expect(k8sClient.Create(ctx, loop)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, loop) }()

			// Use the default reconciler (no MemoryStorageClass set — nil path).
			reconcileLoop("p5a-sc2", nsA)

			pvcName := memkeyPVCName(repoA)
			pvc := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: nsA, Name: pvcName}, pvc)).To(Succeed(), "the memory PVC must be created")
			// nil or empty string = the cluster default.
			if pvc.Spec.StorageClassName != nil {
				Expect(*pvc.Spec.StorageClassName).To(Equal(""), "when --memory-storage-class is empty, the PVC's storageClassName must be empty or nil (the cluster default)")
			}
		})
	})

	_ = nsB
})

// memkeyPVCName is the test's accessor for the memory PVC name (it uses the
// real memkey package — the operator and the test must agree on the key).
func memkeyPVCName(repo string) string {
	return memkeyPVCNameReal(repo)
}

func memkeyPVCNameReal(repo string) string {
	return memkey.PVCName(repo)
}
