package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/pkg/memkey"
)

// P5b (Phase 5 shared memory, PLAN-PHASE5.md slice P5b): the memory-writer
// Job. The operator, on a Loop that has terminated in a qualifying outcome
// (Succeeded, or Failed:Stalled — the plan's eligibility rule), creates the
// memory-writer Job (the operator-controlled memory-write entrypoint — the
// Job mounts the workspace PVC READ-ONLY and the memory PVC READ-WRITE,
// reads the runner's proposed lessons, screens them with the operator's
// static policy, and writes the eligible lessons to the memory PVC) + its
// deny-all NetworkPolicy. The operator reads the Job's termination phase and
// records the outcome on the MemoryWriteDone condition.
//
// The plan's P5b acceptance: "the memory-writer Job's container uses the
// workspace git image's base, runs the memory-writer binary (the new binary
// — NOT the operator's main); the Job's pod is created in the Loop's
// namespace under the coxswain-memory-writer ServiceAccount (the operator
// creates the SA, not the Job's pod's own ServiceAccount — the Job's pod
// runs under the dedicated SA, the SA is created by the operator, the Job
// references the SA). The Job's container mounts the memory PVC
// read-write, the workspace PVC read-only, runs the memory-write entrypoint,
// and reports a bounded summary via its termination message. A successful
// Job records MemoryWritten; a failed Job records MemoryWriteFailed + a
// Warning Event (the Loop stays Succeeded — a failed memory write does not
// fail the Loop, ADR-0004). A Loop that ends in a non-qualifying terminal
// reason (e.g. Failed:BudgetExceeded) or a non-terminal phase records the
// MemoryWriteSkipped condition (no Job is created). The claim's
// lessonsAvailable boolean (the writer Job's gate input — the plan: "the
// claim carries only a lessonsAvailable boolean (present on disk)") is
// persisted into status.progress and the gate reads it: a Succeeded Loop
// whose claim did NOT carry lessonsAvailable (false — or absent on a
// pre-P5b claim) gets NO writer Job (the retroactive-write fix: no writer
// Jobs for old terminal Loops that never produced lessons). A writer Job
// that hits its activeDeadlineSeconds (a hung writer pod — the batch
// controller's Failed/DeadlineExceeded condition) records MemoryWriteFailed
// (the hung-writer-pod fix: the Job fails, the operator records the
// failure). The NetworkPolicy denies ALL ingress/egress. The Job's
// activeDeadlineSeconds (300s) bounds a hung pod; its backoffLimit (0) means
// no retry (a failed write is not retried — the operator records
// MemoryWriteFailed). The Job's container is hardened (non-root, read-only
// rootfs, drop all caps, seccomp — the deliver/verify Job's container
// pattern). The memory PVC is the ONLY writable mount (the readOnly rootfs
// makes the PVC the sole write target); the workspace PVC is mounted
// READ-ONLY (the Job reads the lessons file but never writes the workspace).
//
// The envtest fabricates the Job's pod (envtest has no batch controller):
// the pod is owned by the Job (ownerRef) with the Job's name label, and its
// phase is the Job's outcome (Succeeded/Failed). The Job's termination
// message is the bounded summary (written=N; dropped=N; reasons=...). The
// specs here cover: Job creation on a qualifying terminal phase (Succeeded),
// the Job's container spec (image, entrypoint, SA, mounts, hardening), the
// NetworkPolicy (deny-all), the operator-created ServiceAccount, the
// outcome recording (MemoryWritten on a Succeeded Job; MemoryWriteFailed on
// a Failed Job; the Loop stays Succeeded — ADR-0004), the eligibility
// trigger (a non-qualifying terminal reason — Failed:BudgetExceeded —
// records MemoryWriteSkipped, no Job), the lessonsAvailable gate (a
// Succeeded Loop whose claim did not carry lessonsAvailable gets NO Job —
// the retroactive-write fix), the deadline (a writer Job that hits its
// activeDeadlineSeconds records MemoryWriteFailed — the hung-writer-pod
// fix), and the non-terminal no-op (a non-terminal phase records nothing).

var _ = Describe("P5b memory-writer Job (Phase 5, R28 P2-7 transport)", func() {
	var ns string

	BeforeEach(func() {
		ns = "p5b-" + nowSuffix()
		Expect(k8sClient.Create(context.Background(), &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(context.Background(), &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
	})

	// p5bSucceededWithLessons drives a fresh Loop to Succeeded (the
	// standard s6Succeeded path) and then RE-PLANTS the claim pod with the
	// field on a Planning-phase claim and re-drives the phase machine (the
	// terminal phase is stable — a terminal-phase reconcile does NOT
	// re-derive the phase from a claim — so the test resets the Loop to
	// Planning and re-drives to Succeeded). The field rides the PLANNING
	// claim (the runner writes .coxswain/lessons.json across its run and
	// the operator's gate reads the claim's field (persisted into
	// status.progress) at Job-create time on the terminal phase — the
	// field's TIMING (which claim carried it) is not part of the gate's
	// contract; the field's VALUE (the runner's honest no-lessons report
	// for the run) is). The IMPLEMENTING claim is re-planted with the
	// field as well (the LATEST claim's field is the gate's input — an
	// absent field on a later claim would flip the field to false, so the
	// test carries the field on the Implementing claim too). The mutation
	// the specs isolate is the gate (drop the gate: the no-lessons spec
	// FAILS — the Job WOULD be created), not the claim that carried the
	// field.

	It("creates the memory-writer Job for a Succeeded Loop (trigger) and records MemoryWritten on a Succeeded Job", func() {
		name := "p5b-mw"
		r := s6SucceededLessonsAvailable(name, ns) // the standard path + lessonsAvailable claim

		// Reconcile once more: the operator sees the Succeeded phase + the
		// claim's lessonsAvailable=true (the S5a claim path drove the Loop
		// through the phase machine; the lessonsAvailable field rides the
		// claim) and creates the memory-writer Job + its NetworkPolicy.
		s6Reconcile(r, ns, name)

		// The memory-writer Job is created.
		job := p5bGetMemoryJob(ns, name)
		Expect(job.Labels["coxswain.io/memory-write-for"]).To(Equal(name))
		Expect(job.Labels["app.kubernetes.io/part-of"]).To(Equal("coxswain"))

		// The Job mounts the workspace PVC READ-ONLY + the memory PVC
		// READ-WRITE.
		c := job.Spec.Template.Spec.Containers[0]
		Expect(c.Name).To(Equal("memory-write"))
		mounts := map[string]corev1.VolumeMount{}
		for _, m := range c.VolumeMounts {
			mounts[m.Name] = m
		}
		wsMount, ok := mounts["workspace"]
		Expect(ok).To(BeTrue(), "the workspace PVC must be mounted")
		Expect(wsMount.ReadOnly).To(BeTrue(), "the workspace PVC must be read-only")
		Expect(wsMount.MountPath).To(Equal("/workspace"))
		memMount, ok := mounts["memory"]
		Expect(ok).To(BeTrue(), "the memory PVC must be mounted")
		Expect(memMount.ReadOnly).To(BeFalse(), "the memory PVC must be read-write")
		Expect(memMount.MountPath).To(Equal("/memory"))

		// The workspace volume source is the Loop's workspace PVC
		// (read-only); the memory volume source is the P5a memory PVC
		// (memory-<url-hash>).
		volumes := map[string]corev1.Volume{}
		for _, v := range job.Spec.Template.Spec.Volumes {
			volumes[v.Name] = v
		}
		wsVol := volumes["workspace"]
		Expect(wsVol.PersistentVolumeClaim.ClaimName).To(Equal(workspacePVCName(name)))
		memVol := volumes["memory"]
		Expect(memVol.PersistentVolumeClaim.ClaimName).To(Equal(memkey.PVCName(inClusterRepoURL)))

		// The Job's container runs under the dedicated ServiceAccount
		// (coxswain-memory-writer) — the operator creates it in the Loop's
		// namespace; the Job references it.
		Expect(job.Spec.Template.Spec.ServiceAccountName).To(Equal("coxswain-memory-writer"))

		// The Job's container is hardened (non-root, read-only rootfs, drop
		// all caps, seccomp — the deliver/verify Job's container pattern).
		Expect(c.SecurityContext.RunAsNonRoot).NotTo(BeNil())
		Expect(*c.SecurityContext.RunAsNonRoot).To(BeTrue())
		Expect(c.SecurityContext.RunAsUser).NotTo(BeNil())
		// The runAsUser is non-zero (the container does not run as root).
		Expect(*c.SecurityContext.RunAsUser).NotTo(BeZero())
		Expect(job.Spec.Template.Spec.SecurityContext.RunAsNonRoot).NotTo(BeNil())
		Expect(*job.Spec.Template.Spec.SecurityContext.RunAsNonRoot).To(BeTrue())
		Expect(job.Spec.Template.Spec.SecurityContext.RunAsUser).NotTo(BeNil())
		Expect(*job.Spec.Template.Spec.SecurityContext.RunAsUser).NotTo(BeZero())
		Expect(job.Spec.Template.Spec.SecurityContext.SeccompProfile).NotTo(BeNil())
		Expect(c.SecurityContext.SeccompProfile).NotTo(BeNil())
		Expect(c.SecurityContext.Capabilities).NotTo(BeNil())
		Expect(c.SecurityContext.Capabilities.Drop).To(ContainElement(corev1.Capability("ALL")))
		Expect(job.Spec.Template.Spec.SecurityContext.SeccompProfile.Type).To(Equal(corev1.SeccompProfileTypeRuntimeDefault))

		// The container's command is the memory-write entrypoint (the
		// operator-controlled binary — the Job's pod runs the memory-write
		// subcommand, NOT the operator's main; the entrypoint is
		// /usr/local/bin/coxswain-memory-write, the binary's path).
		Expect(c.Command).NotTo(BeEmpty(), "the memory-write container must carry an explicit command (the entrypoint)")
		// The entrypoint is the operator binary's memory-write subcommand
		// (the Job's container runs the operator image's memory-write
		// entrypoint — the entrypoint is configured by the operator's
		// memoryWriterEntrypoint; the command's last arg is the outcome
		// (the plan: "the Job's container runs the memory-write entrypoint
		// with the Loop's terminal outcome as its argument")).
		// The container's command carries the outcome arg (the Loop's
		// terminal outcome — "succeeded" for a Succeeded Loop).
		Expect(c.Command[len(c.Command)-1]).To(Equal("succeeded"),
			"the memory-write entrypoint's last arg is the Loop's terminal outcome")

		// The Job's activeDeadlineSeconds (the hung-writer-pod fix): a
		// writer pod stuck in CreateContainerConfigError (or any other
		// non-terminating state) must FAIL the Job after the deadline (the
		// batch controller fails it with reason DeadlineExceeded); the
		// operator records MemoryWriteFailed on a Failed Job (the spec
		// below covers the DeadlineExceeded outcome specifically).
		Expect(job.Spec.ActiveDeadlineSeconds).NotTo(BeNil(),
			"the writer Job must carry activeDeadlineSeconds (a stuck pod must fail it)")
		Expect(*job.Spec.ActiveDeadlineSeconds).To(BeNumerically(">", 0),
			"the writer Job's activeDeadlineSeconds must be positive")
		// The Job's backoffLimit (0): a failed write is NOT retried (the
		// operator records MemoryWriteFailed; a retry would re-read the
		// same lessons file and re-write the same lessons — the write is
		// idempotent but the retry is not useful; the bounded summary is
		// the operator's record, not the Job's retry).
		Expect(job.Spec.BackoffLimit).NotTo(BeNil(),
			"the writer Job must carry backoffLimit (a failed write is not retried)")
		Expect(*job.Spec.BackoffLimit).To(BeZero(),
			"the writer Job's backoffLimit is 0 (no retry — the operator records MemoryWriteFailed)")

		// The desired NetworkPolicy is created for the Job (F16 + P5c:
		// deny-all ingress — NO Ingress rules; egress denies all EXCEPT the
		// model proxy pod, because s6Loop gives the Loop a model
		// (endpointSecretRef), so the P5c curator call is the ONLY egress
		// rule. A blank rule {} would match everything — ALLOW all, the
		// opposite of the ADR-0007 fence).
		np := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(context.Background(), types.NamespacedName{
			Name: memoryWriterNetPolName(name), Namespace: ns,
		}, np)).To(Succeed())
		Expect(np.Spec.PolicyTypes).To(ConsistOf(
			networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress))
		// Deny ALL ingress: no Ingress rules (F16: an empty ingress rule {}
		// would match everything — allow all, the opposite of the ADR-0007
		// fence).
		Expect(np.Spec.Ingress).To(BeEmpty(),
			"the NetworkPolicy must have NO ingress rules (deny-all ingress)")
		// Egress: EXACTLY TWO rules — the proxy-allow (the P5c curator call)
		// + the DNS egress rule (the curator URL is a DNS name that must be
		// resolved — P1-3). A blank (extra) egress rule {} would match
		// everything — ALLOW all — and defeat the fence.
		Expect(np.Spec.Egress).To(HaveLen(2),
			"the NetworkPolicy must have EXACTLY TWO egress rules (the proxy-allow + DNS)")
		Expect(np.Spec.Egress[0].To).To(HaveLen(1))
		peer := np.Spec.Egress[0].To[0]
		Expect(peer.NamespaceSelector).NotTo(BeNil())
		Expect(peer.NamespaceSelector.MatchLabels).To(HaveKeyWithValue(
			"kubernetes.io/metadata.name", ns))
		Expect(peer.PodSelector).NotTo(BeNil())
		Expect(peer.PodSelector.MatchLabels).To(HaveKeyWithValue(
			"coxswain.io/proxy-for", name))

		// Fabricate the Job's pod (envtest has no batch controller) and mark
		// it Succeeded (the Job's termination message is the bounded
		// summary: written=N; dropped=N; reasons=...).
		p5bFabricateJobPod(ns, job, corev1.PodSucceeded)
		p5bSetJobPodTerminationMessage(ns, name, "written=2; dropped=1; reasons=too-verbose")

		// Reconcile again: the operator reads the Job's outcome and records
		// MemoryWritten on the MemoryWriteDone condition.
		s6Reconcile(r, ns, name)
		loop := p5bMustLoop(ns, name)
		cond := meta.FindStatusCondition(loop.Status.Conditions, memoryWriterConditionType)
		Expect(cond).NotTo(BeNil(), "the MemoryWriteDone condition must be set")
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(memoryWrittenReason))
		// The condition's message is a fixed string (the operator does not
		// include the Job's termination message in the condition; the
		// termination message is the bounded summary on the Job's pod).
		Expect(cond.Message).To(ContainSubstring("memory write completed"),
			"the MemoryWritten condition carries the completion message")

		// The Loop is still Succeeded (the memory write is best-effort —
		// ADR-0004: a successful write does not change the phase).
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded))

		// The MemoryWritten condition is visible (the kubectl describe path
		// reads the Loop's conditions — the condition is on the Loop's
		// status, not the Job's).
		Expect(cond.LastTransitionTime).NotTo(BeZero())
	})

	It("creates the memory-writer Job for a Failed:Stalled Loop (the eligibility trigger) and records the outcome", func() {
		// The plan's eligibility trigger: a lesson is eligible only on
		// Succeeded or Failed:Stalled. A Failed Loop with the Stalled reason
		// (the runner gave up — the iteration budget was exhausted or the
		// runner reported a stall) is eligible: the operator creates the
		// memory-writer Job (the runner may have written lessons before it
		// gave up). The outcome arg is "failed-stalled" (the Loop's terminal
		// outcome — the Job's entrypoint's last arg).
		// Drive a Loop to Failed:Stalled with lessonsAvailable=true.
		name := "p5b-mw-stall"
		r := s6FailedStalledLessonsAvailable(name, ns)

		// Reconcile: the operator sees the Failed:Stalled outcome + the
		// claim's lessonsAvailable=true and creates the memory-writer Job
		// (the eligibility trigger: Succeeded OR Failed:Stalled).
		s6Reconcile(r, ns, name)

		// The Job is created with the outcome arg "failed-stalled".
		job := p5bGetMemoryJob(ns, name)
		c := job.Spec.Template.Spec.Containers[0]
		Expect(c.Command[len(c.Command)-1]).To(Equal("failed-stalled"),
			"the memory-write entrypoint's last arg is the Loop's terminal outcome (failed-stalled)")

		// Fabricate the Job's pod + mark it Succeeded + reconcile: the
		// operator records MemoryWritten (the write is best-effort — the
		// Loop is still Failed; the MemoryWritten condition is set).
		p5bFabricateJobPod(ns, job, corev1.PodSucceeded)
		p5bSetJobPodTerminationMessage(ns, name, "written=1; dropped=0; reasons=")
		s6Reconcile(r, ns, name)
		loop := p5bMustLoop(ns, name)
		cond := meta.FindStatusCondition(loop.Status.Conditions, memoryWriterConditionType)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal(memoryWrittenReason))
		// The Loop is still Failed (the memory write is best-effort —
		// ADR-0004: a successful write does not change the phase).
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseFailed))
	})

	It("ensures the memory-writer Job's dedicated ServiceAccount in the Loop's namespace (the operator creates it; the Job's pod runs under it)", func() {
		// The plan: "the Job's pod is created in the Loop's namespace under
		// the coxswain-memory-writer ServiceAccount (the operator creates
		// the SA, not the Job's pod's own ServiceAccount — the Job's pod
		// runs under the dedicated SA, the SA is created by the operator,
		// the Job references the SA). The SA does NOT exist in the Loop's
		// namespace by default (the install's RBAC creates it in the
		// operator's namespace, coxswain-system). The operator ENSURES the
		// SA in the Loop's namespace (automountServiceAccountToken: false,
		// NO RoleBinding — the Job's binary makes no API calls) so the
		// Job's pod can be created (a Job that references a missing SA
		// cannot create its pod). The SA is NOT owned by the Loop (it is
		// shared per namespace — a second Loop in the same namespace
		// reuses it; deleting one Loop must not delete the SA another
		// Loop's Job is using).
		name := "p5b-mw-sa"
		_ = s6SucceededLessonsAvailable(name, ns) // the standard path + lessonsAvailable claim

		// The SA IS in the Loop's namespace (the operator created it when
		// the Job was created during the Succeeded phase's reconcile).
		sa := &corev1.ServiceAccount{}
		Expect(k8sClient.Get(context.Background(), types.NamespacedName{
			Name: "coxswain-memory-writer", Namespace: ns,
		}, sa)).To(Succeed(), "the operator must ensure the memory-writer SA in the Loop's namespace")

		// The SA is hardened: automountServiceAccountToken: false (the Job's
		// binary makes no API calls — the token is not mounted).
		Expect(sa.AutomountServiceAccountToken).NotTo(BeNil())
		Expect(*sa.AutomountServiceAccountToken).To(BeFalse(),
			"the memory-writer SA must set automountServiceAccountToken: false (the Job's binary makes no API calls)")

		// The SA is NOT owned by the Loop (it is shared per namespace —
		// deleting the Loop must not delete the SA another Loop's Job is
		// using).
		Expect(sa.OwnerReferences).To(BeEmpty(),
			"the memory-writer SA must NOT be owned by the Loop (it is shared per namespace)")

		// The Job's pod runs under the SA (the Job references it).
		job := p5bGetMemoryJob(ns, name)
		Expect(job.Spec.Template.Spec.ServiceAccountName).To(Equal("coxswain-memory-writer"))

		// A second Loop in the SAME namespace reuses the SA (the SA is
		// shared per namespace — the operator does NOT re-create it; the
		// ensure is idempotent). The second Loop's Job references the same
		// SA (the SA's name is fixed: coxswain-memory-writer).
		name2 := "p5b-mw-sa2"
		r2 := s6SucceededLessonsAvailable(name2, ns)
		res2 := p5bMemoryWriterReconcile(r2, ns, name2)
		Expect(res2.RequeueAfter).To(BeNumerically(">", 0), "the second memory-writer Job's create must requeue")
		job2 := p5bGetMemoryJob(ns, name2)
		Expect(job2.Spec.Template.Spec.ServiceAccountName).To(Equal("coxswain-memory-writer"))

		// The SA is still there (the second Loop's Job did NOT delete or
		// re-create it — the ensure is idempotent; the SA is shared).
		saAfter := &corev1.ServiceAccount{}
		Expect(k8sClient.Get(context.Background(), types.NamespacedName{
			Name: "coxswain-memory-writer", Namespace: ns,
		}, saAfter)).To(Succeed(), "the SA must still be in the Loop's namespace (shared per namespace)")
		// The SA's UID is unchanged (the same SA — not re-created).
		Expect(saAfter.UID).To(Equal(sa.UID),
			"the memory-writer SA must be the SAME SA (shared per namespace — not re-created)")
	})

	It("writes nothing for a Failed:BudgetExceeded Loop (the eligibility trigger: only Succeeded or Failed:Stalled qualifies)", func() {
		// The plan's eligibility trigger: a lesson is eligible only on
		// Succeeded or Failed:Stalled. ANY other terminal reason (e.g.
		// Failed:BudgetExceeded — the budget gate stopped the Loop before it
		// could iterate) is ineligible: the operator records the
		// MemoryWriteSkipped condition and does NOT create the Job.
		name := "p5b-mw-budget"
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
		s5aEnsureSandbox(ns, name)
		loop := s6Loop(name, ns, inClusterRepoURL)
		Expect(k8sClient.Create(context.Background(), loop)).To(Succeed())
		s6Reconcile(r, ns, name)
		loop = p5bMustLoop(ns, name)
		loop.Status.BaseCommit = s5aBaseCommit
		Expect(k8sClient.Status().Update(context.Background(), loop)).To(Succeed())
		// Force the Loop to Failed with a BUDGET reason (BudgetExceeded — a
		// non-Stalled terminal reason). The phase machine is authoritative;
		// a terminal Failed state is stable (the S4 phase machine does NOT
		// re-derive from a terminal Failed).
		loop = p5bMustLoop(ns, name)
		loop.Status.Phase = coxv1alpha1.LoopPhaseFailed
		loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseFailed
		setCondition(loop, string(coxv1alpha1.LoopPhaseFailed), metav1.ConditionTrue, "BudgetExceeded", "the iteration budget was exhausted (non-Stalled reason)")
		Expect(k8sClient.Status().Update(context.Background(), loop)).To(Succeed())

		// Reconcile: the operator sees the Failed:BudgetExceeded outcome and
		// records the MemoryWriteSkipped condition (no Job is created — the
		// eligibility trigger is Succeeded or Failed:Stalled ONLY).
		s6Reconcile(r, ns, name)

		// No memory-writer Job is created.
		job := &batchv1.Job{}
		err := k8sClient.Get(context.Background(), types.NamespacedName{
			Name: memoryWriterJobName(p5bMustLoop(ns, name)), Namespace: ns,
		}, job)
		Expect(err).To(HaveOccurred(), "no memory-writer Job for a Failed:BudgetExceeded Loop (the eligibility trigger)")

		// The MemoryWriteSkipped condition is recorded (the drop is in the
		// audit trail with the rule that failed — the eligibility rule).
		loop = p5bMustLoop(ns, name)
		cond := meta.FindStatusCondition(loop.Status.Conditions, memoryWriterConditionType)
		Expect(cond).NotTo(BeNil(), "the MemoryWriteDone condition must be set")
		Expect(cond.Reason).To(Equal(memoryWriteSkippedReason))
	})

	It("records MemoryWriteFailed on a Failed Job (and the Loop stays Succeeded — ADR-0004)", func() {
		// Drive a Loop to Succeeded, create the Job (as in the first spec),
		// then mark the Job's pod FAILED. The operator records
		// MemoryWriteFailed + a Warning Event. The Loop stays Succeeded (a
		// failed memory write does NOT fail the Loop — ADR-0004).
		name := "p5b-mw-fail"
		r := s6SucceededLessonsAvailable(name, ns)   // the standard path + lessonsAvailable claim
		res := p5bMemoryWriterReconcile(r, ns, name) // creates the Job
		Expect(res.RequeueAfter).To(BeNumerically(">", 0), "the memory-writer Job's create must requeue")
		job := p5bGetMemoryJob(ns, name)

		// Mark the Job's pod FAILED (the write failed — e.g. the memory PVC
		// was not writable, the policy screen dropped all lessons, the
		// binary crashed). The termination message is the bounded summary
		// (written=0; dropped=0; reasons=error: <msg>).
		p5bFabricateJobPod(ns, job, corev1.PodFailed)
		p5bSetJobPodTerminationMessage(ns, name, "written=0; dropped=0; reasons=error: the memory PVC was not writable")
		s6Reconcile(r, ns, name)
		loop := p5bMustLoop(ns, name)
		cond := meta.FindStatusCondition(loop.Status.Conditions, memoryWriterConditionType)
		Expect(cond).NotTo(BeNil(), "the MemoryWriteDone condition must be set")
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(memoryWriteFailedReason))
		// The condition's message is a fixed string (the operator does not
		// include the Job's termination message in the condition).
		Expect(cond.Message).To(ContainSubstring("memory write failed"),
			"the MemoryWriteFailed condition carries the failure message")

		// The Loop is still Succeeded (a failed memory write does NOT fail
		// the Loop — ADR-0004).
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded))
	})

	It("creates NO memory-writer Job for a Succeeded Loop whose claim did not carry lessonsAvailable (false or absent) (the retroactive-write fix)", func() {
		// The plan: "the claim carries only a lessonsAvailable boolean (present
		// on disk)" — the memory-writer Job is created ONLY when the Loop
		// terminated with the claim's lessonsAvailable true. A Succeeded Loop
		// whose runner proposed no lessons (the field is false — or absent on a
		// pre-P5b claim, which is also false) gets NO writer Job: the operator
		// records MemoryWriteSkipped instead (no retroactive writes for old
		// terminal Loops that never produced lessons — the kind finding: the
		// dogfood3/5/7 writer pods hung in CreateContainerConfigError).
		//
		// The test drives the Loop to Succeeded with the S5a claim path
		// (the standard s6Succeeded path — the pre-P5b claim shape: the
		// lessonsAvailable field is ABSENT on every claim — the gate sees
		// the absent field as false). To isolate the GATE (the mutation:
		// drop the gate -> the Job WOULD be created), the test sets the
		// Loop's status.progress.LessonsAvailable to false EXPLICITLY (the
		// field's value is the runner's report for the run — the gate reads
		// the field's value, not the field's presence; the field is a
		// claim-derived field, and the test's direct status write is the
		// way of setting the field's value to false WITHOUT re-driving the
		// phase machine (the terminal phase is stable — a direct status
		// write persists)).
		name := "p5b-mw-noless"
		r := s6Succeeded(name, ns, inClusterRepoURL) // the standard path

		// The Loop's progress record carries lessonsAvailable=false (the
		// field's value is the runner's honest no-lessons report for the
		// run — the test's direct status write sets the field's value to
		// false WITHOUT re-driving the phase machine; the gate reads the
		// field's value, not the field's presence).
		loop := p5bMustLoop(ns, name)
		if loop.Status.Progress == nil {
			loop.Status.Progress = &coxv1alpha1.ProgressStatus{}
		}
		loop.Status.Progress.LessonsAvailable = false
		Expect(k8sClient.Status().Update(context.Background(), loop)).To(Succeed())
		loop = p5bMustLoop(ns, name)
		Expect(loop.Status.Progress).NotTo(BeNil(), "the Succeeded claim must record progress")
		Expect(loop.Status.Progress.LessonsAvailable).To(BeFalse(),
			"the claim must not report lessons (the field is false — the runner wrote no lessons file)")

		// Reconcile: the operator sees the Succeeded phase + the claim's
		// no-lessons report (the field's value is false) and does NOT
		// create the writer Job.
		p5bMemoryWriterReconcile(r, ns, name)

		// No memory-writer Job is created (the lessonsAvailable gate).
		job := &batchv1.Job{}
		err := k8sClient.Get(context.Background(), types.NamespacedName{
			Name: memoryWriterJobName(p5bMustLoop(ns, name)), Namespace: ns,
		}, job)
		Expect(err).To(HaveOccurred(),
			"no memory-writer Job for a Succeeded Loop with lessonsAvailable false (the retroactive-write fix)")

		// The MemoryWriteSkipped condition is recorded (the audit trail shows
		// the drop and its rule — the lessonsAvailable gate).
		loop = p5bMustLoop(ns, name)
		cond := meta.FindStatusCondition(loop.Status.Conditions, memoryWriterConditionType)
		Expect(cond).NotTo(BeNil(), "the MemoryWriteDone condition must be set")
		Expect(cond.Reason).To(Equal(memoryWriteSkippedReason))

		// The Loop stays Succeeded (the skip does not change the phase).
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded))
	})

	It("creates the memory-writer Job when the claim carries lessonsAvailable=true (the gate opens)", func() {
		// The inverse of the no-lessons spec: the SAME Succeeded Loop with the
		// claim's lessonsAvailable=true (the runner wrote the lessons file)
		// gets the writer Job. Together the two specs isolate the field under
		// test (the mutation: dropping the gate makes the no-lessons spec
		// FAIL — the Job WOULD be created).
		//
		// The test drives the Loop to Succeeded with the S5a claim path +
		// the lessonsAvailable=true field on the Implementing claim (the
		// runner wrote the lessons file during the Implementing phase — the
		// field rides the claim into progress via the S4 phase machine's
		// recordPhaseClaim, so progress.LessonsAvailable is true when the
		// Loop reaches Succeeded). The gate reads the LATEST claim's field
		// (the Implementing claim's, not the Planning claim's), so the test
		// carries the field on the Implementing claim.
		name := "p5b-mw-lesst"
		r := s6SucceededLessonsAvailable(name, ns)
		loop := p5bMustLoop(ns, name)
		Expect(loop.Status.Progress).NotTo(BeNil(), "the Succeeded claim must record progress")
		Expect(loop.Status.Progress.LessonsAvailable).To(BeTrue(),
			"the claim must report lessons (the field is true — the runner wrote the lessons file)")

		// Reconcile: the operator sees the Succeeded phase + the claim's
		// lessonsAvailable=true and creates the writer Job (the gate opens).
		res := p5bMemoryWriterReconcile(r, ns, name)
		Expect(res.RequeueAfter).To(BeNumerically(">", 0), "the memory-writer Job's create must requeue")

		// The Job IS created (the gate opens when lessonsAvailable is true).
		job := p5bGetMemoryJob(ns, name)
		Expect(job.Spec.Template.Spec.ServiceAccountName).To(Equal("coxswain-memory-writer"))
		Expect(job.Spec.Template.Spec.Containers[0].Command[len(job.Spec.Template.Spec.Containers[0].Command)-1]).To(Equal("succeeded"))
	})

	It("records MemoryWriteFailed when the writer Job hits its activeDeadlineSeconds (DeadlineExceeded)", func() {
		// The hung-writer-pod fix (reviewer P1 round 4): a writer pod stuck in
		// CreateContainerConfigError must FAIL the Job (the batch controller
		// stamps Failed/DeadlineExceeded when activeDeadlineSeconds elapses)
		// and the operator must record MemoryWriteFailed. envtest has no batch
		// controller, so the spec stamps the Job's Failed/DeadlineExceeded
		// condition itself (the same condition the batch controller would
		// stamp) and marks the Job's pod Failed (the phase the deadline kill
		// leaves).
		name := "p5b-mw-deadline"
		r := s6SucceededLessonsAvailable(name, ns)   // the standard path + lessonsAvailable claim
		res := p5bMemoryWriterReconcile(r, ns, name) // creates the Job (the
		// gate opens: the claim's lessonsAvailable is true — the spec
		// isolates the deadline handling on a Job that exists).
		Expect(res.RequeueAfter).To(BeNumerically(">", 0), "the memory-writer Job's create must requeue")
		job := p5bGetMemoryJob(ns, name)

		// The Job carries the deadline (the hung-writer-pod fix): a stuck pod
		// fails the Job after the deadline, it never runs forever.
		Expect(job.Spec.ActiveDeadlineSeconds).NotTo(BeNil(),
			"the writer Job must carry activeDeadlineSeconds (a stuck pod must fail it)")
		Expect(*job.Spec.ActiveDeadlineSeconds).To(Equal(int64(300)),
			"the writer Job's activeDeadlineSeconds is the 300s constant")
		Expect(job.Spec.BackoffLimit).NotTo(BeNil(),
			"the writer Job must carry backoffLimit (a failed write is not retried)")
		Expect(*job.Spec.BackoffLimit).To(BeZero(),
			"the writer Job's backoffLimit is 0 (no retry — the operator records MemoryWriteFailed)")

		// envtest has no batch controller: mark the Job's pod Failed (the
		// phase the deadline kill leaves) + the Job's Failed/DeadlineExceeded
		// condition (the batch controller stamps it when activeDeadlineSeconds
		// elapses without the Job completing). The operator reads the Job's
		// condition to determine the deadline message.
		now := metav1.Now()
		job.Status.StartTime = &now
		job.Status.Failed = 1
		job.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, Reason: "FailureTarget", Message: ""},
			{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: batchv1.JobReasonDeadlineExceeded, Message: "Job has exceeded the deadline"},
		}
		Expect(k8sClient.Status().Update(context.Background(), job)).To(Succeed())
		p5bFabricateJobPod(ns, job, corev1.PodFailed)
		p5bFabricateJobPod(ns, job, corev1.PodFailed)
		p5bSetJobPodTerminationMessage(ns, name, "written=0; dropped=0; reasons=deadline: the Job hit its activeDeadlineSeconds")
		s6Reconcile(r, ns, name)

		// The MemoryWriteFailed condition is recorded, and the message names
		// the deadline (the review sees the hang, not a generic failure).
		loop := p5bMustLoop(ns, name)
		cond := meta.FindStatusCondition(loop.Status.Conditions, memoryWriterConditionType)
		Expect(cond).NotTo(BeNil(), "the MemoryWriteDone condition must be set")
		Expect(cond.Reason).To(Equal(memoryWriteFailedReason))
		Expect(cond.Message).To(ContainSubstring("activeDeadlineSeconds"),
			"the MemoryWriteFailed message must name the deadline (the hung-writer-pod fix)")
		// The Loop is still Succeeded (a failed memory write does NOT fail the
		// Loop — ADR-0004).
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded))
	})

	// F31: a Job that is terminal with NO pod (pod GC, eviction, an admission
	// reject) is decided from the Job's own conditions; a Job with neither
	// condition and no pod is still in progress (no decision).
	for _, tc := range []struct {
		name, cond, reason string
		wantReason         string // "" = no decision
	}{
		{"p5b-mw-nopod-failed", string(batchv1.JobFailed), "BackoffLimitExceeded", memoryWriteFailedReason},
		{"p5b-mw-nopod-complete", string(batchv1.JobComplete), "", memoryWrittenReason},
		{"p5b-mw-nopod-running", "", "", ""},
	} {
		It("decides a no-pod Job from its conditions: "+tc.name, func() {
			r := s6SucceededLessonsAvailable(tc.name, ns)
			p5bMemoryWriterReconcile(r, ns, tc.name) // creates the Job
			job := p5bGetMemoryJob(ns, tc.name)
			if tc.cond != "" {
				now := metav1.Now()
				job.Status.StartTime = &now
				job.Status.Conditions = []batchv1.JobCondition{
					{Type: batchv1.JobConditionType(tc.cond), Status: corev1.ConditionTrue, Reason: tc.reason},
				}
				if tc.cond == string(batchv1.JobFailed) {
					job.Status.Conditions = append([]batchv1.JobCondition{{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, Reason: tc.reason}}, job.Status.Conditions...)
					job.Status.Failed = 1
				} else {
					job.Status.CompletionTime = &now
					job.Status.Succeeded = 1
					job.Status.Conditions = append([]batchv1.JobCondition{{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue}}, job.Status.Conditions...)
				}
				Expect(k8sClient.Status().Update(context.Background(), job)).To(Succeed())
			}
			// No pod is fabricated: the pod is absent.
			s6Reconcile(r, ns, tc.name)
			s6Reconcile(r, ns, tc.name)
			loop := p5bMustLoop(ns, tc.name)
			cond := meta.FindStatusCondition(loop.Status.Conditions, memoryWriterConditionType)
			if tc.wantReason == "" {
				Expect(cond).To(BeNil(), "an in-progress Job with no pod makes no decision")
				return
			}
			Expect(cond).NotTo(BeNil(), "a terminal Job with no pod must be decided")
			Expect(cond.Reason).To(Equal(tc.wantReason))
			if tc.reason != "" {
				Expect(cond.Message).To(ContainSubstring(tc.reason), "the message names the Job's reason")
			}
		})
	}

	It("does nothing on a non-terminal phase (the eligibility rule)", func() {
		// A non-terminal phase (e.g. Implementing) is ineligible: the operator
		// records NOTHING (no condition, no Job) — the Loop is still running
		// and the write is not yet eligible. The MemoryWriteDone condition is
		// only meaningful once the Loop has terminated.
		// Drive a Loop to Implementing (the S4 phase machine: Planning ->
		// Implementing on the Implementing claim).
		name := "p5b-mw-nonterm"
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
		s5aEnsureSandbox(ns, name)
		loop := s6Loop(name, ns, inClusterRepoURL)
		Expect(k8sClient.Create(context.Background(), loop)).To(Succeed())
		s6Reconcile(r, ns, name) // Pending -> Planning
		loop = p5bMustLoop(ns, name)
		loop.Status.BaseCommit = s5aBaseCommit
		Expect(k8sClient.Status().Update(context.Background(), loop)).To(Succeed())
		// Planning claim (the runner executed Planning).
		s5aClaimPod(ns, name, "Planning", "")
		s6Reconcile(r, ns, name)
		s5aEnsureSandbox(ns, name) // the annotation recycle deleted the Sandbox
		// Implementing claim (the runner executed Implementing) — the Loop is
		// now Verifying (the phase machine advanced Implementing -> Verifying
		// because the claim carried a valid headCommit; Verifying is a
		// non-terminal phase).
		s5aClaimPodWithLessonsAvailable(ns, name, "Implementing", s6HeadCommit, false)
		s6Reconcile(r, ns, name)
		loop = p5bMustLoop(ns, name)
		// The Loop is Verifying (a non-terminal phase — the memory write
		// is not yet eligible). Verifying is the phase the machine enters
		// after an Implementing claim with a valid headCommit; the spec's
		// intent (a non-terminal phase) is satisfied by Verifying.
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"the Implementing claim must advance the Loop to Verifying (a non-terminal phase)")

		// Reconcile again: the operator sees the non-terminal phase and does
		// NOTHING (no condition, no Job).
		p5bMemoryWriterReconcile(r, ns, name)

		// No memory-writer Job is created (non-terminal phase).
		job := &batchv1.Job{}
		err := k8sClient.Get(context.Background(), types.NamespacedName{
			Name: memoryWriterJobName(p5bMustLoop(ns, name)), Namespace: ns,
		}, job)
		Expect(err).To(HaveOccurred(), "no memory-writer Job for a non-terminal phase")

		// No MemoryWriteDone condition is recorded (the condition is only
		// meaningful once the Loop has terminated — a non-terminal phase
		// records nothing).
		loop = p5bMustLoop(ns, name)
		cond := meta.FindStatusCondition(loop.Status.Conditions, memoryWriterConditionType)
		Expect(cond).To(BeNil(), "no MemoryWriteDone condition on a non-terminal phase")
	})

	It("updates a drifted (allow-all) existing NetworkPolicy to deny-all (F16)", func() {
		// The P5b build created the memory-writer NetworkPolicy with
		// Ingress: []NetworkPolicyIngressRule{{}} and
		// Egress: []NetworkPolicyEgressRule{{}} (which ALLOWS all traffic).
		// ensureMemoryWriterNetworkPolicy must UPDATE the drifted policy to
		// the desired spec (no rules, both PolicyTypes) — not skip it.
		name := "p5b-drifted"

		// Pre-create the drifted (allow-all) policy BEFORE the reconcile
		// that would create it. The policy is named <loop>-memory-write-netpol.
		drifted := &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name:      memoryWriterNetPolName(name),
				Namespace: ns,
				Labels:    memoryWriterJobLabels(name),
			},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{
					MatchLabels: map[string]string{memoryWriteForLabelKey: name},
				},
				PolicyTypes: []networkingv1.PolicyType{
					networkingv1.PolicyTypeIngress,
					networkingv1.PolicyTypeEgress,
				},
				// Drifted: the P5b build's allow-all rules.
				Ingress: []networkingv1.NetworkPolicyIngressRule{{}},
				Egress:  []networkingv1.NetworkPolicyEgressRule{{}},
			},
		}
		Expect(k8sClient.Create(context.Background(), drifted)).To(Succeed())

		// Drive the Loop to Succeeded with lessonsAvailable (the standard
		// P5b path). The reconcile will call ensureMemoryWriterNetworkPolicy
		// and see the existing (drifted) policy.
		_ = s6SucceededLessonsAvailable(name, ns)

		// After reconcile, the policy must be updated to the desired spec
		// (the whole spec, not just rule counts — the F16 drift check uses
		// equality.Semantic.DeepEqual on the desired Spec): the drifted
		// allow-all rules are gone, and the desired policy (NO ingress rules;
		// EXACTLY ONE egress rule — the proxy-allow, because s6Loop gives the
		// Loop a model) is in place.
		np := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(context.Background(), types.NamespacedName{
			Name: memoryWriterNetPolName(name), Namespace: ns,
		}, np)).To(Succeed())
		Expect(np.Spec.Ingress).To(BeEmpty(),
			"the drifted allow-all ingress rule {} must be removed (deny-all ingress)")
		// The drifted allow-all egress rule {} must be GONE (it would match
		// everything — ALLOW all) and replaced by the desired two egress
		// rules (the proxy-allow + the DNS egress rule — P1-3).
		Expect(np.Spec.Egress).To(HaveLen(2),
			"the drifted allow-all egress rule must be replaced by the desired proxy-allow + DNS rules")
		Expect(np.Spec.Egress[0].To).To(HaveLen(1))
		peer := np.Spec.Egress[0].To[0]
		Expect(peer.NamespaceSelector).NotTo(BeNil())
		Expect(peer.NamespaceSelector.MatchLabels).To(HaveKeyWithValue(
			"kubernetes.io/metadata.name", ns))
		Expect(peer.PodSelector).NotTo(BeNil())
		Expect(peer.PodSelector.MatchLabels).To(HaveKeyWithValue(
			"coxswain.io/proxy-for", name))
		Expect(np.Spec.PolicyTypes).To(ConsistOf(
			networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress))
	})
})

// p5bMemoryWriterReconcile drives a manual reconcile of the Loop and returns
// both the reconcile result (the requeue contract: the Job's create returns
// requeue=true — the operator must come back to read the Job's outcome) and
// the resulting Loop. (envtest has no watch/cache: the requeue is the ONLY
// signal that the next reconcile should happen; the spec drives it
// explicitly, asserting the requeue-true contract in place of the wait. The
// s6Reconcile helper is the loop-phase helper — it does not surface the
// requeue, which the memory-writer create-time contract needs.)
func p5bMemoryWriterReconcile(r *LoopReconciler, ns, name string) reconcile.Result {
	res, err := r.Reconcile(context.Background(),
		reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
	Expect(err).NotTo(HaveOccurred())
	return res
}

// p5bGetMemoryJob returns the memory-writer Job (failing on a read error).
func p5bGetMemoryJob(ns, loopName string) *batchv1.Job {
	job := &batchv1.Job{}
	Expect(k8sClient.Get(context.Background(), types.NamespacedName{
		Name: memoryWriterJobName(p5bMustLoop(ns, loopName)), Namespace: ns,
	}, job)).To(Succeed())
	return job
}

// p5bMustLoop returns the Loop (failing on a read error).
func p5bMustLoop(ns, name string) *coxv1alpha1.Loop {
	loop := &coxv1alpha1.Loop{}
	Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
	return loop
}

// p5bFabricateJobPod creates the Job's pod (envtest has no batch controller
// — the operator's recordMemoryWriterOutcome reads the Job's pod via the
// Job's name label + the Job's UID ownerRef, so the pod must carry both).
// The pod's phase is the Job's outcome (Succeeded/Failed).
func p5bFabricateJobPod(ns string, job *batchv1.Job, phase corev1.PodPhase) {
	ctx := context.Background()
	// Delete any existing pod with the same name (envtest has no batch
	// controller; the operator's recordMemoryWriterOutcome reads the pod via
	// the memory-write-for label + the Job's UID ownerRef). The pod must carry
	// both.
	existing := &corev1.Pod{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: job.Name + "-pod", Namespace: ns}, existing); err == nil {
		_ = k8sClient.Delete(ctx, existing)
	}
	// The operator's readMemoryWriterJobPod matches the pod by the
	// memory-write-for label (memoryWriteForLabelKey) + the Job's UID
	// ownerRef. The pod must carry both.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      job.Name + "-pod",
			Namespace: ns,
			Labels: map[string]string{
				memoryWriteForLabelKey: job.Labels[memoryWriteForLabelKey],
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: batchv1.SchemeGroupVersion.String(),
				Kind:       i65JobKind,
				Name:       job.Name,
				UID:        job.UID,
			}},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: "coxswain-memory-writer",
			Containers:         []corev1.Container{{Name: "memory-write", Image: "coxswain-memory-writer:latest"}},
		},
	}
	Expect(k8sClient.Create(ctx, pod)).To(Succeed())
	pod.Status.Phase = phase
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "memory-write",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 0,
		}},
	}}
	Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
}

// p5bSetJobPodTerminationMessage sets the Job's pod's termination message
// (the bounded summary: written=N; dropped=N; reasons=...).
func p5bSetJobPodTerminationMessage(ns, loopName, msg string) {
	ctx := context.Background()
	job := p5bGetMemoryJob(ns, loopName)
	pod := &corev1.Pod{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: job.Name + "-pod", Namespace: ns}, pod)).To(Succeed())
	pod.Status.ContainerStatuses[0].State.Terminated.Message = msg
	Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
}

// s6SucceededLessonsAvailable drives a fresh Loop to Succeeded with the
// lessonsAvailable=true field on the Implementing claim (the P5b gate
// input). It is s6Succeeded with the Implementing claim carrying the
// lessonsAvailable field (the runner wrote .coxswain/lessons.json during
// the Implementing phase — the field rides the claim into progress via
// recordPhaseClaim, so progress.LessonsAvailable is true when the Loop
// reaches Succeeded). The gate reads the LATEST claim's field, so the
// Implementing claim (the latest non-Verifying claim) is the one that
// carries it.
// s6SucceededLessonsAvailable drives a fresh Loop to Succeeded with the
// lessonsAvailable gate OPEN. It uses the S5a claim path for the phase
// machine (Planning -> Implementing -> Verifying -> Succeeded) and sets the
// Implementing claim's lessonsAvailable=true so the field rides into
// status.progress via recordPhaseClaim. The gate reads progress.LessonsAvailable.
// s6SucceededLessonsAvailable creates a Succeeded Loop with the
// lessonsAvailable gate OPEN. It sets the status directly (phase Succeeded,
// progress.LessonsAvailable=true) to isolate the memory-writer gate from the
// S5a claim propagation path (the claim path's lessonsAvailable field is
// tested separately in the S4 specs; the P5b specs test the gate logic).
func s6SucceededLessonsAvailable(name, ns string) *LoopReconciler {
	ctx := context.Background()
	r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
	s5aEnsureSandbox(ns, name)
	// Create the Loop.
	loop := s6Loop(name, ns, inClusterRepoURL)
	Expect(k8sClient.Create(ctx, loop)).To(Succeed())
	s6Reconcile(r, ns, name) // Pending -> Planning
	loop = &coxv1alpha1.Loop{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
	loop.Status.BaseCommit = s5aBaseCommit
	Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
	// Planning claim.
	s5aClaimPod(ns, name, "Planning", "")
	s6Reconcile(r, ns, name)
	s5aEnsureSandbox(ns, name) // the annotation recycle deleted the Sandbox
	// Implementing claim with lessonsAvailable=true (the P5b gate: the field
	// rides the claim into progress via recordPhaseClaim, so the memory-writer
	// Job's eligibility gate reads it when the Loop reaches Succeeded).
	s5aClaimPodWithLessonsAvailable(ns, name, "Implementing", s6HeadCommit, true)
	s6Reconcile(r, ns, name)
	// D61: ready the verify proxy.
	d61ReadyVerifyProxy(r, ns, name)
	// Verifying: the verify Job's evidence.
	s5aVerifyPod(ctx, ns, name, name+"-verify-1", 0)
	s6Reconcile(r, ns, name)
	s6ForceSucceededFromHold(ns, name)
	s6Reconcile(r, ns, name) // the memory writer reads the settled Succeeded
	loop = &coxv1alpha1.Loop{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
	Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded),
		"the S5a claim path must reach Succeeded")
	return r
}

// s6FailedStalledLessonsAvailable drives a fresh Loop to Failed:Stalled with
// the lessonsAvailable gate OPEN. It uses the S5a claim path for the phase
// machine (Planning -> Implementing -> Verifying), then marks the Loop
// Failed with the Stalled reason (the stall detector's terminal outcome).
// The Loop's status carries a stallHistory entry (the stall signature the
// P5c negative lesson is composed from) and the default stallAction (Fail).
func s6FailedStalledLessonsAvailable(name, ns string) *LoopReconciler {
	ctx := context.Background()
	r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
	s5aEnsureSandbox(ns, name)
	loop := s6Loop(name, ns, inClusterRepoURL)
	Expect(k8sClient.Create(ctx, loop)).To(Succeed())
	// Drive the Loop to Failed:Stalled directly (the stall detector's
	// terminal outcome). The operator's memory-writer gate reads the
	// phase + the lessonsAvailable field. Set both directly to isolate
	// the eligibility check from the S5a claim path.
	loop = &coxv1alpha1.Loop{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
	loop.Status.Phase = coxv1alpha1.LoopPhaseFailed
	loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseFailed
	loop.Status.Progress = &coxv1alpha1.ProgressStatus{
		Phase:            coxv1alpha1.LoopPhaseFailed,
		LessonsAvailable: true,
	}
	// The stall signature (the P5c negative lesson's input: the repeated
	// verify-failure hash — the last status.stallHistory entry's hash).
	// at is required by the CRD (the verify Job pod's finish time).
	loop.Status.StallHistory = []coxv1alpha1.StallEntry{{
		Iteration:            1,
		JobName:              name + "-verify-1",
		Hash:                 "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		NormalisationVersion: "1",
		Check:                loopCheckCmd,
		At:                   metav1.Now(),
	}}
	setCondition(loop, string(coxv1alpha1.LoopPhaseFailed), metav1.ConditionTrue, "Stalled", "the runner stalled (the iteration budget was exhausted)")
	Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
	return r
}
