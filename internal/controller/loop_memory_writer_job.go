// P5b (Phase 5 shared memory, PLAN-PHASE5.md slice P5b): the memory-writer
// Job. The operator's sole writer of the shared memory PVC (the runner
// proposes the lessons into a workspace file; the operator-created Job reads
// the file, screens it with the operator's static policy, and persists the
// survivors as one file per lesson on the memory PVC). The Job runs in the
// Loop's namespace (it mounts the memory PVC — a PVC is mountable only by
// pods in its own namespace, D66).
//
// The flow (operator-driven, terminal-triggered — reviewer decision
// p5b-trigger-decision.md; NOT an operator exec into the sandbox pod):
//
//  1. The runner (a successful Implementing) writes the proposed lessons to
//     <workspace>/.coxswain/lessons.json on the workspace PVC (the
//     workspace-file transport, R28 P2-7).
//  2. When the Loop terminates in a qualifying outcome (Succeeded, or Failed
//     with reason Stalled — the plan's eligibility rule), the operator
//     creates the memory-writer Job. The Job is OPERATOR-CONTROLLED code
//     (the memory-write subcommand, cmd/memory-writer): it mounts the
//     workspace PVC READ-ONLY and the memory PVC READ-WRITE, reads
//     /workspace/.coxswain/lessons.json, applies the static write policy
//     (pkg/memorywrite.Screen — secrets regex, ≤500 chars, single-line,
//     non-blank, the size cap), writes ONLY the eligible lessons to
//     /memory/lesson-<n>.txt, and reports a BOUNDED summary (counts + per-
//     lesson sha256-prefix/written|dropped/rule — never the lesson text)
//     via its termination message.
//  3. The operator reads the Job's termination phase + the bounded summary
//     and records the outcome on the MemoryWriteDone condition (and a
//     Warning Event on failure). The Job's NetworkPolicy denies ALL egress
//     (the ADR-0007 fence: a lesson must not be exfiltrated during the
//     write).
//
// A failed memory write does NOT fail the Loop (ADR-0004): the Loop still
// Succeeds (the memory is best-effort; the work is what the Loop is for).
// The condition is the durable record (cox describe shows it; the review
// can see a memory write was attempted and why it failed).
//
// The operator pod mounts NO Loop PVCs (R28 plan review P1-2); reading the
// lessons file by the operator directly is impossible without a Job. The
// Job is the operator's trusted extension of its own process (the operator's
// static policy runs IN the Job's entrypoint — the Job is never
// model-authored code).
package controller

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/pkg/memkey"
)

// memoryWriterJobName returns the memory-writer Job name:
// <loop>-memory-write-<iteration>. The iteration is the Loop's current
// 1-based iteration count (the Job name carries the iteration so a re-plan /
// re-implement creates a NEW Job, never mutating the prior one — the verify
// Job's I65 pattern: a recreated Job has a new UID and its pod is matched by
// the Job's UID, not just its name).
func memoryWriterJobName(loop *coxv1alpha1.Loop) string {
	return fmt.Sprintf("%s-memory-write-%d", loop.Name, max(1, loop.Status.Iteration))
}

// memoryWriteForLabelKey is the label the memory-writer Job and its NetworkPolicy
// carry (the "memory-write-for" label — the operator identifies the Job and its
// pod by this label). The key is a constant (goconst: it appears in more than
// one place).
const memoryWriteForLabelKey = "coxswain.io/memory-write-for"

// memoryWriterJobLabels are the labels the memory-writer Job carries (the
// Loop's identity + the memory-writer component). The podSelector for the
// Job's NetworkPolicy is the memory-write-for label.
func memoryWriterJobLabels(loopName string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/part-of": partOfCoxswain,
		verifyLoopLabel:             loopName,
		"coxswain.io/component":     "memory-writer",
		memoryWriteForLabelKey:      loopName,
		managedByLabel:              "coxswain-controller",
	}
}

// memoryWriterNetPolName is the memory-writer Job's NetworkPolicy name
// (<loop>-memory-write-netpol). The NetworkPolicy denies ALL ingress and ALL
// egress for the Job's pod (the writer Job makes no network calls; the
// deny-all is the ADR-0007 fence).
func memoryWriterNetPolName(loopName string) string {
	return fmt.Sprintf("%s-memory-write-netpol", loopName)
}

// memoryWriterConditionType is the Loop condition the memory-writer outcome
// is recorded on (the durable record: cox describe shows whether a memory
// write was attempted and why it failed — a failed write does NOT fail the
// Loop, ADR-0004, but it IS surfaced).
const memoryWriterConditionType = "MemoryWriteDone"

// memoryWriterConditionReasons are the condition reasons the memory-writer
// outcome maps to:
//
//   - MemoryWritten: the Job succeeded (the eligible lessons are on the
//     memory PVC — the Job ran and wrote them, whether or not any was
//     eligible; a zero-eligible write is a valid "nothing to write").
//   - MemoryWriteFailed: the Job failed (a failed write — the Loop still
//     Succeeds, ADR-0004, but the condition + a Warning Event surface it).
//   - MemoryWriteSkipped: the Loop did not end in a qualifying outcome
//     (a non-terminal phase, or a terminal reason other than Succeeded /
//     Failed:Stalled — the eligibility rule; the Job is not created) or the
//     Loop's claim did not carry lessonsAvailable=true (the runner proposed
//     no lessons into the workspace file — the plan's gate: no retroactive
//     writes for terminal Loops that never produced lessons).
const (
	memoryWrittenReason      = "MemoryWritten"
	memoryWriteFailedReason  = "MemoryWriteFailed"
	memoryWriteSkippedReason = "MemoryWriteSkipped"
)

// memoryWriterOutcomeArg returns the terminal outcome arg the operator passes
// to the memory-writer Job's entrypoint (the Job applies the eligibility
// rule: a lesson is eligible only on a qualifying terminal outcome). It
// returns ("", false) when the Loop's outcome is NOT qualifying (the
// operator skips the Job — the MemoryWriteSkipped condition). A qualifying
// outcome is Succeeded or Failed:Stalled (the plan's eligibility rule).
func (r *LoopReconciler) memoryWriterOutcomeArg(loop *coxv1alpha1.Loop) (string, bool) {
	switch loop.Status.Phase {
	case coxv1alpha1.LoopPhaseSucceeded:
		return "succeeded", true
	case coxv1alpha1.LoopPhaseFailed:
		// Failed:Stalled only (a Failed with another reason — e.g.
		// SandboxFailed, BudgetExceeded — is NOT qualifying: the plan
		// requires the Stalled reason).
		if loopFailedReasonStalled(loop) {
			return "failed-stalled", true
		}
		return "", false
	default:
		// A non-terminal phase is ineligible (the plan: "any other terminal
		// reason, or a non-terminal phase, is ineligible").
		return "", false
	}
}

// loopFailedReasonStalled reports whether the Loop's Failed condition's
// reason is "Stalled" (the stall detector's Fail action sets the
// LoopPhaseFailed condition with reason "Stalled" —
// internal/controller/loop_p2e_stall.go).
func loopFailedReasonStalled(loop *coxv1alpha1.Loop) bool {
	for i := range loop.Status.Conditions {
		if loop.Status.Conditions[i].Type == string(coxv1alpha1.LoopPhaseFailed) &&
			loop.Status.Conditions[i].Status == metav1.ConditionTrue &&
			loop.Status.Conditions[i].Reason == "Stalled" {
			return true
		}
	}
	return false
}

// memoryWriterImage returns the image the memory-writer Job's container runs
// (the operator-controlled memory-write entrypoint). It is the operator's
// own image (the memory-writer subcommand is built into the operator's
// binary — cmd/memory-writer). When the operator has not been given a
// memory-writer image (envtests / minimal setups), fall back to the
// workspace git image (the Job still builds; a real run uses the operator
// image). The image must contain the memory-write entrypoint (a
// /usr/local/bin/coxswain-memory-write binary, or the operator binary with a
// memory-write subcommand — the entrypoint is configured by the
// memoryWriterEntrypoint).
func (r *LoopReconciler) memoryWriterImage() string {
	if r.MemoryWriterImage != "" {
		return r.MemoryWriterImage
	}
	return r.workspaceGitImage()
}

// ensureMemoryWriterJobAndNetPolicy creates (idempotently) the memory-writer
// Job + its NetworkPolicy for a Loop that has terminated in a qualifying
// outcome (Succeeded, or Failed:Stalled — the plan's eligibility rule). The
// Job is owned by the Loop (controller ref, so deleting the Loop
// garbage-collects the Job). An EXISTING Job with the same name is left
// untouched (idempotency — the operator never mutates a running Job; a
// re-run is a NEW Job, the verify Job's I65 pattern).
//
// The Job runs the operator-controlled memory-write entrypoint (the
// memory-writer subcommand, cmd/memory-writer): it mounts the workspace PVC
// READ-ONLY and the memory PVC READ-WRITE, reads the runner's proposed
// lessons, applies the static write policy, writes the eligible lessons to
// the memory PVC, and reports a bounded summary via its termination message.
// The container is hardened (non-root, read-only rootfs, drop all caps,
// seccomp — the deliver/verify Job's container pattern); the memory PVC is
// the ONLY writable mount (the readOnly rootfs makes the PVC the sole write
// target), and the workspace PVC is mounted READ-ONLY (the Job reads the
// lessons file but never writes the workspace).
//
// The NetworkPolicy denies ALL ingress and ALL egress (the writer Job makes
// no network calls — the deny-all is the ADR-0007 fence: a lesson must not
// be exfiltrated during the write).
//
// Returns (requeue, err). requeue is true when the Job was just created or
// is not yet terminal (the operator must requeue to read its outcome);
// false when the outcome is already recorded or the Loop's outcome is not
// qualifying (the Job is not created).
func (r *LoopReconciler) ensureMemoryWriterJobAndNetPolicy(ctx context.Context, loop *coxv1alpha1.Loop) (bool, error) {
	// B2 restore hold: while the restore hold is active (the Restore condition
	// is False, reason StoppingWorkloads/StoppingSandbox/DeletingPVC), do NOT
	// create the memory-writer Job. The Job mounts the workspace PVC (read-only)
	// and the memory PVC (read-write); a new Job would re-mount the workspace
	// PVC (or a Terminating PVC) and hold it via kubernetes.io/pvc-protection,
	// stalling the restore. The restore must complete (the hold clears with the
	// True/Restored marker) before the Job is created. (The memory write is
	// best-effort — ADR-0004 — so deferring it while the restore is in
	// progress is safe: the write happens once the restore completes and the
	// Loop is still at its terminal phase.)
	if r.b2RestoreHoldSuspended(loop) {
		return false, nil
	}

	// The memory write is gated on a TERMINAL phase (Succeeded or Failed).
	// On a non-terminal phase the operator does NOTHING (no condition, no
	// Job) — the Loop is still running and the write is not yet eligible.
	// Setting a condition on a non-terminal phase would leave a stale
	// condition that the later terminal reconcile must clear (the
	// MemoryWriteDone condition is only meaningful once the Loop has
	// terminated).
	switch loop.Status.Phase {
	case coxv1alpha1.LoopPhaseSucceeded, coxv1alpha1.LoopPhaseFailed:
		// A terminal phase: proceed (the eligibility check below decides the
		// outcome).
	default:
		// A non-terminal phase: the memory write is not yet eligible. Do
		// nothing (no condition, no Job).
		return false, nil
	}

	// The eligibility gate: a qualifying terminal outcome only (Succeeded,
	// or Failed with the Stalled reason). A non-qualifying terminal reason
	// (Failed with a non-Stalled reason) is the MemoryWriteSkipped
	// condition (the plan: "any other terminal reason, or a non-terminal
	// phase, is ineligible").
	outcomeArg, eligible := r.memoryWriterOutcomeArg(loop)
	if !eligible {
		setMemoryWriterCondition(loop, memoryWriteSkippedReason,
			"memory write skipped: the Loop ended in a non-qualifying terminal reason (not Succeeded or Failed:Stalled)")
		return false, nil
	}

	// The lessonsAvailable gate (the plan: "the claim carries only a
	// lessonsAvailable boolean (present on disk)"): a qualifying terminal
	// Loop gets a writer Job ONLY when its claim reported lessonsAvailable
	// true. Every other terminal Loop — the runner proposed no lessons
	// (the claim's lessonsAvailable is false), or the field is absent
	// (no progress record yet — a terminal phase the phase machine reached
	// without a claim that carried the field) — gets NO writer Job. The
	// strict form (a nil progress is not a gate input) prevents retroactive
	// writes for OLD terminal Loops that never produced lessons (the kind
	// finding: the dogfood3/5/7 writer pods hung in
	// CreateContainerConfigError).
	if loop.Status.Progress == nil || !loop.Status.Progress.LessonsAvailable {
		reason := "the Loop's claim did not report lessons (lessonsAvailable is false or absent — the runner wrote no lessons file)"
		if loop.Status.Progress == nil {
			reason = "the Loop has no progress record carrying a lessonsAvailable claim (no claim reported lessons)"
		}
		setMemoryWriterCondition(loop, memoryWriteSkippedReason,
			"memory write skipped: "+reason)
		return false, nil
	}

	// The memory PVC name (P5a: memory-<url-hash>, keyed by (namespace,
	// git URL)). A repo-less Loop has no memory PVC; the write is skipped
	// (the eligibility outcome is recorded as skipped — the Job cannot
	// mount a PVC that does not exist).
	pvcName := memkey.PVCName(loop.Spec.Workspace.Repo)
	if pvcName == "" {
		setMemoryWriterCondition(loop, memoryWriteSkippedReason,
			"memory write skipped: no workspace repo (no memory PVC to write to)")
		return false, nil
	}

	// Idempotency: an existing Job with the same name is left untouched
	// (the operator never mutates a running Job; a re-run is a NEW Job, the
	// verify Job's I65 pattern). Read the Job; on NotFound, create it.
	jobName := memoryWriterJobName(loop)
	existing := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: loop.Namespace}, existing)
	if err == nil {
		// The Job exists: read its outcome (the termination phase + the
		// bounded summary) and record it. Requeue when the Job's pod is not
		// yet terminal.
		return r.recordMemoryWriterOutcome(ctx, loop, existing), nil
	}
	if !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("get memory-writer job %s: %w", jobName, err)
	}

	// The memory-writer Job runs in the LOOP's namespace under the dedicated
	// ServiceAccount (coxswain-memory-writer). The SA does NOT exist in the
	// Loop's namespace by default (the install's RBAC creates it in the
	// operator's namespace, coxswain-system). The operator ENSURES the SA in
	// the Loop's namespace (automountServiceAccountToken: false, NO
	// RoleBinding — the Job's binary makes no API calls) so the Job's pod can
	// be created (a Job that references a missing SA cannot create its pod).
	// The SA is NOT owned by the Loop (it is shared per namespace — a second
	// Loop in the same namespace reuses it; deleting one Loop must not delete
	// the SA another Loop's Job is using).
	if err := r.ensureMemoryWriterServiceAccount(ctx, loop.Namespace); err != nil {
		return false, fmt.Errorf("ensure memory-writer serviceaccount in %s: %w", loop.Namespace, err)
	}

	// Create the Job (the operator-controlled memory-write entrypoint) + the
	// NetworkPolicy (deny-all). The Job is owned by the Loop.
	job := r.buildMemoryWriterJob(loop, pvcName, outcomeArg)
	if err := ctrl.SetControllerReference(loop, job, r.Scheme); err != nil {
		return false, fmt.Errorf("set owner on memory-writer job %s: %w", jobName, err)
	}
	if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return false, fmt.Errorf("create memory-writer job %s: %w", jobName, err)
	}
	if err := r.ensureMemoryWriterNetworkPolicy(ctx, loop); err != nil {
		return false, fmt.Errorf("create memory-writer netpol %s: %w", memoryWriterNetPolName(loop.Name), err)
	}
	logf.FromContext(ctx).Info("created memory-writer Job", "job", jobName, "outcome", outcomeArg)
	// The Job was just created: requeue to read its outcome (the Job's pod
	// is not yet terminal).
	return true, nil
}

// memoryWriterJobDeadlineSeconds is the writer Job's activeDeadlineSeconds
// (the hung-writer-pod fix, reviewer P1 round 4): a writer pod stuck in
// CreateContainerConfigError (or any other non-terminating state) must FAIL
// the Job after this deadline (the batch controller fails it with reason
// DeadlineExceeded) so the operator records MemoryWriteFailed. Without it
// the Job runs forever, the operator keeps requeueing, and the failure is
// never surfaced (the kind finding: old writer pods sat 6h40m+ with no
// MemoryWriteFailed condition).
const memoryWriterJobDeadlineSeconds = int64(300)

// memoryWriterJobBackoffLimit is the writer Job's backoffLimit: the Job runs
// once (a failed write is not retried — the operator records
// MemoryWriteFailed; a retry would re-read the same lessons file and re-write
// the same files — idempotent but pointless, and the plan does not ask for
// retries). A non-zero backoffLimit would let the batch controller restart
// the pod after a failure and mask the failed write.
const memoryWriterJobBackoffLimit = int32(0)

// buildMemoryWriterJob returns the Job spec: a single hardened container
// (the operator-controlled memory-write entrypoint) that reads the lessons
// file from the workspace PVC (read-only at /workspace), screens it, and
// writes the eligible lessons to the memory PVC (read-write at /memory).
// The container is non-root, read-only rootfs, drop all caps, seccomp (the
// deliver/verify Job's container pattern); the memory PVC is the ONLY
// writable mount (the readOnly rootfs makes the PVC the sole write target),
// and the workspace PVC is mounted READ-ONLY.
// memoryWriterEnvVars returns the Job container's env vars. The curator
// env vars (COX_CURATOR_BASE_URL, COX_MODEL) are set only when the Loop has
// a model (endpointSecretRef); without a model there is no proxy and the
// curator is disabled (the Job writes all static-rule survivors — the P5b
// behavior).
func (r *LoopReconciler) memoryWriterEnvVars(loop *coxv1alpha1.Loop, termMsgPath string) []corev1.EnvVar {
	env := []corev1.EnvVar{
		{Name: "COX_SUMMARY_PATH", Value: termMsgPath},
		{Name: "COX_LESSONS_PATH", Value: "/workspace/.coxswain/lessons.json"},
		{Name: "COX_MEMORY_DIR", Value: "/memory"},
	}
	// P5c: the curator call goes to the Loop's model proxy (the same
	// endpoint + Secret the runner uses; the proxy injects the API key).
	// The curator is set only when the Loop has a model (endpointSecretRef);
	// without a model there is no proxy and the curator is disabled.
	if loop.Spec.Agent.EndpointSecretRef != "" {
		env = append(env,
			corev1.EnvVar{Name: "COX_CURATOR_BASE_URL", Value: r.proxyServiceURL(loop.Name, loop.Namespace)},
			corev1.EnvVar{Name: "COX_MODEL", Value: loop.Spec.Agent.Model},
		)
	}
	return env
}

func (r *LoopReconciler) buildMemoryWriterJob(loop *coxv1alpha1.Loop, pvcName, outcomeArg string) *batchv1.Job {
	// The Job runs once with a bounded lifetime (the hung-writer-pod fix):
	// activeDeadlineSeconds fails a stuck pod (reason DeadlineExceeded — the
	// operator records MemoryWriteFailed), backoffLimit 0 means a failed pod
	// is terminal (the operator records MemoryWriteFailed; no retry).
	deadline := memoryWriterJobDeadlineSeconds
	backoff := memoryWriterJobBackoffLimit
	// The termination message path (the bounded summary — the operator reads
	// it from the Job's pod's container status). The path is the operator's
	// constant (no user input, so it is unquoted in the entrypoint script).
	termMsgPath := "/tmp/memory-write-summary.txt"
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      memoryWriterJobName(loop),
			Namespace: loop.Namespace,
			Labels:    memoryWriterJobLabels(loop.Name),
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:          &backoff,
			ActiveDeadlineSeconds: &deadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: memoryWriterJobLabels(loop.Name),
				},
				Spec: corev1.PodSpec{
					// Pod-level security: non-root, seccomp runtime default (the
					// deliver/verify Job's pattern — the pod-level defaults apply
					// to every container; the container's own SecurityContext
					// carries the additional hardening: read-only rootfs, drop
					// all caps, no privilege escalation).
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: &memoryWriterTrue,
						RunAsUser:    &memoryWriterNonRootUID,
						RunAsGroup:   &memoryWriterNonRootGID,
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					RestartPolicy: corev1.RestartPolicyNever,
					// P5b: the memory-writer Job's DEDICATED ServiceAccount (the
					// plan: "runs under a dedicated ServiceAccount with RBAC
					// (get/list on the Loop, create on the Job's pods; no access
					// to the operator's secrets)"). The Job does NO API access
					// (it reads the lessons file from the workspace mount and
					// writes to the memory PVC — both mounted volumes, no
					// kubectl/API calls), so automountServiceAccountToken is false
					// (no token is mounted: the Job CANNOT make API calls even if
					// the SA's RBAC were broader — the "write from the operator
					// pod" mutation is prevented by the dedicated SA + the no-token
					// pod). The SA's Role (config/rbac/memory_writer_role.yaml)
					// is the SA's documented intended scope (get/list on the
					// Loop, create/get/list on the Job's pods) — NARROWER than the
					// operator's manager-role (no Secrets, no PVC writes).
					ServiceAccountName:           "coxswain-memory-writer",
					AutomountServiceAccountToken: &memoryWriterFalse,
					Containers: []corev1.Container{{
						Name:            "memory-write",
						Image:           r.memoryWriterImage(),
						ImagePullPolicy: corev1.PullIfNotPresent,
						// The operator's memory-write binary, run DIRECTLY
						// (no shell wrapper — the operator's distroless image
						// has no /bin/sh; the kind run showed a StartError
						// "exec /bin/sh: no such file or directory" when the
						// Job used /bin/sh -c). The binary's first arg is the
						// terminal outcome (the eligibility rule). COX_SUMMARY_PATH
						// points the binary at the termination-message path so
						// it writes the bounded summary there itself (the
						// operator reads it from the pod's container status);
						// COX_LESSONS_PATH / COX_MEMORY_DIR are the Job's
						// mounts (the defaults match, but the env makes the
						// paths explicit).
						Command: []string{
							"/usr/local/bin/coxswain-memory-write",
							outcomeArg,
						},
						WorkingDir: "/",
						SecurityContext: &corev1.SecurityContext{
							RunAsNonRoot:             &memoryWriterTrue,
							RunAsUser:                &memoryWriterNonRootUID,
							RunAsGroup:               &memoryWriterNonRootGID,
							ReadOnlyRootFilesystem:   &memoryWriterTrue,
							AllowPrivilegeEscalation: &memoryWriterFalse,
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
							SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
						},
						TerminationMessagePath:   termMsgPath,
						TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
						Env:                      r.memoryWriterEnvVars(loop, termMsgPath),
						VolumeMounts: []corev1.VolumeMount{
							// The workspace PVC (READ-ONLY — the Job reads the
							// lessons file but never writes the workspace).
							{Name: "workspace", MountPath: "/workspace", ReadOnly: true},
							// The memory PVC (READ-WRITE — the ONLY writable
							// mount; the Job writes the eligible lessons here).
							{Name: "memory", MountPath: "/memory"},
							// An emptyDir for the termination message (the
							// readOnly rootfs makes / read-only; the
							// termination message file lives on the
							// emptyDir).
							{Name: "termmsg", MountPath: "/tmp"},
						},
					}},
					Volumes: []corev1.Volume{
						// The workspace PVC (the Loop's existing workspace
						// PVC — the runner's proposed lessons are on it).
						{Name: "workspace", VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
								ClaimName: workspacePVCName(loop.Name),
								ReadOnly:  true,
							},
						}},
						// The memory PVC (the P5a memory PVC — the Job
						// writes the eligible lessons here).
						{Name: "memory", VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
								ClaimName: pvcName,
							},
						}},
						// The termination message's emptyDir.
						{Name: "termmsg", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					},
				},
			},
		},
	}
}

// ensureMemoryWriterNetworkPolicy creates (idempotently) the memory-writer
// Job's NetworkPolicy: denies ALL ingress and ALL egress EXCEPT the model
// proxy pod (the P5c curator call goes to the Loop's model proxy Service; the
// proxy pod is identified by the coxswain.io/proxy-for label). The writer
// Job's only network call is the curator's /v1/chat/completions POST to the
// proxy; all other egress is denied (the ADR-0007 fence: a lesson must not
// be exfiltrated during the write). The podSelector is the
// "coxswain.io/memory-write-for" label (the Job stamps it on the pod
// template).
//
// F16: the desired spec carries NO Ingress rules and at most ONE Egress rule
// (the proxy-allow). An empty rule {} MATCHES everything (allow all) — the
// opposite of the fence — so a blank Ingress or Egress rule must never be
// emitted. Drift is detected against the WHOLE desired spec (semantic
// equality, not rule counts) so a drifted rule set is updated in place.
func (r *LoopReconciler) ensureMemoryWriterNetworkPolicy(ctx context.Context, loop *coxv1alpha1.Loop) error {
	name := memoryWriterNetPolName(loop.Name)
	desired := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: loop.Namespace,
			Labels:    memoryWriterJobLabels(loop.Name),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{memoryWriteForLabelKey: loop.Name},
			},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			// Deny ALL ingress: no Ingress rules (F16: an empty ingress rule {}
			// would MATCH everything — allow all, the opposite of the ADR-0007
			// fence). Egress is denied the same way, EXCEPT the model proxy
			// pod when the Loop has a model: the P5c curator call goes to the
			// Loop's model proxy Service, and the proxy pod carries the label
			// coxswain.io/proxy-for: <loop>. The proxy-allow rule is the ONLY
			// egress rule (F16: a blank egress rule {} would also allow all).
		},
	}
	// The proxy egress rule (the P5c curator call) is the ONLY egress rule,
	// added AFTER the struct literal (an if cannot appear inside a struct
	// literal). A Loop with no model (no endpointSecretRef) has NO proxy and
	// a fully denied NetworkPolicy (the P5b fence).
	if loop.Spec.Agent.EndpointSecretRef != "" {
		desired.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{{
			To: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{kubeNSLabelKey: loop.Namespace},
				},
				PodSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"coxswain.io/proxy-for": loop.Name},
				},
			}},
		}}
	}

	existing := &networkingv1.NetworkPolicy{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: loop.Namespace}, existing)
	if err == nil {
		// The policy exists: check for drift against the WHOLE desired spec
		// (the P5b build created it with Ingress/Egress [{}] which ALLOWS all
		// traffic; rule counts alone would miss a drifted rule set, so compare
		// semantically — F16). If drifted, update to the desired spec.
		if equality.Semantic.DeepEqual(existing.Spec, desired.Spec) {
			return nil // already correct
		}
		existing.Spec = desired.Spec
		if err := r.Update(ctx, existing); err != nil {
			return fmt.Errorf("update drifted memory-writer netpol %s: %w", name, err)
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get memory-writer netpol %s: %w", name, err)
	}
	// The NetPol is owned by the Loop (controller reference) so the apiserver
	// garbage-collects it when the Loop is deleted — the same way the
	// memory-writer Job is GC'd (the Job is SetControllerReference'd in
	// buildMemoryWriterJob). Without the owner, the NetPol leaks on Loop
	// delete (a standalone object the B1 finalizer does not clean up; the
	// kind acceptance left p5b-df3c/df4-memory-write-netpol behind).
	if err := controllerutil.SetControllerReference(loop, desired, r.Scheme); err != nil {
		return fmt.Errorf("set the memory-writer netpol %s owner to the Loop: %w", name, err)
	}
	if err := r.Create(ctx, desired); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create memory-writer netpol %s: %w", name, err)
	}
	return nil
}

// memoryWriterServiceAccountName is the dedicated ServiceAccount the
// memory-writer Job's pod runs under (the plan: "a dedicated ServiceAccount
// with the stated RBAC" — the Job makes no API calls; the SA's RBAC is the
// Job's intended scope, but automountServiceAccountToken: false means no
// token is mounted, so the Job CANNOT make API calls even if the SA's RBAC
// were broader). The SA is created in the LOOP's namespace by the operator
// (ensureMemoryWriterServiceAccount) — NOT owned by the Loop (it is shared
// per namespace: a second Loop in the same namespace reuses it; deleting one
// Loop must not delete the SA another Loop's Job is using). There is NO
// RoleBinding in the Loop's namespace (the Job needs no API access; the
// install's Role/RoleBinding in the operator's namespace is unused by the
// Job and is dropped — the Job's no-token pod is the security boundary).
const memoryWriterServiceAccountName = "coxswain-memory-writer"

// ensureMemoryWriterServiceAccount creates (idempotently) the dedicated
// ServiceAccount in the given namespace (the Loop's namespace) so the
// memory-writer Job's pod can be created (a Job that references a missing SA
// cannot create its pod). The SA has automountServiceAccountToken: false
// (no token mounted — the Job makes no API calls). It is NOT owned by any
// Loop (shared per namespace; no ownerReference, so deleting a Loop does not
// GC it). Returns an error only on a genuine failure (not AlreadyExists).
func (r *LoopReconciler) ensureMemoryWriterServiceAccount(ctx context.Context, namespace string) error {
	sa := &corev1.ServiceAccount{}
	err := r.Get(ctx, types.NamespacedName{Name: memoryWriterServiceAccountName, Namespace: namespace}, sa)
	if err == nil {
		return nil // the SA exists (a previous Loop in this namespace created it)
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get memory-writer serviceaccount %s/%s: %w", namespace, memoryWriterServiceAccountName, err)
	}
	// The SA does not exist: create it. automountServiceAccountToken: false
	// (no token mounted — the Job makes no API calls). No ownerReference
	// (shared per namespace — deleting one Loop must not delete the SA
	// another Loop's Job is using).
	noAutomount := false
	saToCreate := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      memoryWriterServiceAccountName,
			Namespace: namespace,
			Labels: map[string]string{
				labelPartOf:                   partOfCoxswain,
				"app.kubernetes.io/component": "memory-writer",
			},
		},
		AutomountServiceAccountToken: &noAutomount,
	}
	if err := r.Create(ctx, saToCreate); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create memory-writer serviceaccount %s/%s: %w", namespace, memoryWriterServiceAccountName, err)
	}
	return nil
}

// recordMemoryWriterOutcome reads the memory-writer Job's pod's terminal
// phase + the bounded summary (the termination message) and records the
// outcome on the Loop's MemoryWriteDone condition. Returns (requeue): true
// when the Job's pod is not yet terminal (the operator must requeue to read
// the outcome); false when a terminal outcome was recorded (Succeeded ->
// MemoryWritten, Failed -> MemoryWriteFailed).
//
// A Succeeded Job is MemoryWritten (the write completed — whether or not
// any lesson was eligible; a zero-eligible write is a valid "nothing to
// write", the Job's own eligibility decision). A Failed Job is
// MemoryWriteFailed (a Warning Event surfaces the failure; the Loop still
// Succeeds, ADR-0004).
func (r *LoopReconciler) recordMemoryWriterOutcome(ctx context.Context, loop *coxv1alpha1.Loop, job *batchv1.Job) bool {
	pod, err := r.readMemoryWriterJobPod(ctx, loop, job)
	if err != nil {
		// A pod read error (the pod is gone, the read failed): requeue (the
		// Job may be creating the pod; a transient error is not a terminal
		// outcome).
		return true
	}
	if pod == nil {
		// The Job has no pod yet (the batch controller has not created it):
		// requeue.
		return true
	}
	// The Job's terminal state (the batch Job's conditions: Complete or
	// Failed). The pod's phase is the authoritative signal (the Job's
	// conditions are updated by the batch controller after the pod
	// terminates). The Job's own conditions are the direct record of a
	// deadline fail: the activeDeadlineSeconds kill (reason DeadlineExceeded
	// — the hung-writer-pod fix) is a Job-level condition the batch
	// controller stamps when the pod never became terminal; the condition's
	// message names the deadline so the review sees the hang.
	deadlineExceeded := jobHasConditionReason(job, batchv1.JobFailed, batchv1.JobReasonDeadlineExceeded)
	podPhase := pod.Status.Phase
	switch podPhase {
	case corev1.PodSucceeded:
		// The Job's pod succeeded: the write completed (whether or not any
		// lesson was eligible). Record MemoryWritten.
		setMemoryWriterCondition(loop, memoryWrittenReason,
			"memory write completed (the Job's pod succeeded; the eligible lessons are on the memory PVC)")
		return false
	case corev1.PodFailed:
		// The Job's pod failed: a write failure. Record MemoryWriteFailed +
		// a Warning Event (the Loop still Succeeds, ADR-0004). When the
		// failure is the activeDeadlineSeconds deadline (reason
		// DeadlineExceeded — the hung-writer-pod fix: a pod stuck in
		// CreateContainerConfigError etc. that the deadline killed), the
		// condition's message names the deadline so the review sees the
		// hang, not just a generic failure.
		msg := "memory write failed (the Job's pod failed; the Loop still Succeeds — ADR-0004 a failed memory write does not fail the Loop)"
		if deadlineExceeded {
			msg = "memory write failed (the Job hit its activeDeadlineSeconds deadline — the pod never became terminal; the Loop still Succeeds — ADR-0004 a failed memory write does not fail the Loop)"
		}
		setMemoryWriterCondition(loop, memoryWriteFailedReason, msg)
		if r.Recorder != nil {
			r.Recorder.Eventf(loop, corev1.EventTypeWarning, memoryWriteFailedReason,
				"memory write failed: the memory-writer Job's pod failed (%s)", memoryWriterJobName(loop))
		}
		return false
	default:
		// The pod is not yet terminal (Pending / Running): requeue.
		return true
	}
}

// readMemoryWriterJobPod reads the memory-writer Job's pod via the APIReader
// (the Job's pod is NOT in the manager's Pod cache; the non-cached client is
// required — the verify Job's readVerifyJobPod pattern). It matches the pod
// by the Job's UID (the I65 pattern: a recreated Job has a new UID, and a
// name-only match would read a stale pod from a prior Job). Returns (nil,
// nil) when the Job has no pod yet.
func (r *LoopReconciler) readMemoryWriterJobPod(ctx context.Context, loop *coxv1alpha1.Loop, job *batchv1.Job) (*corev1.Pod, error) {
	reader := r.apiReader
	if reader == nil {
		reader = r
	}
	jobUID := job.UID
	// Find the Job's pod by the memory-write-for label (the Job stamps it on
	// the pod template).
	list := &corev1.PodList{}
	if err := reader.List(ctx, list,
		client.InNamespace(loop.Namespace),
		client.MatchingLabels{memoryWriteForLabelKey: loop.Name}); err != nil {
		return nil, fmt.Errorf("list memory-writer pods: %w", err)
	}
	// Filter to the current Job's pod (the I65 pattern: match by the Job's
	// UID, not just the name — a recreated Job has a new UID).
	var pod *corev1.Pod
	for i := range list.Items {
		p := &list.Items[i]
		if p.DeletionTimestamp != nil {
			continue // a terminating pod is being removed by the GC; skip it.
		}
		if podOwnedByJobUID(p, jobUID) {
			pod = p
		}
	}
	return pod, nil
}

// podOwnedByJobUID reports whether the pod's ownerReferences name the Job's
// UID (the batch Job controller stamps the owning Job's UID on each pod's
// ownerReferences). This is the I65 pattern: a recreated Job has a new UID,
// and matching by UID (not just name) selects only the pod the current Job
// created.
func podOwnedByJobUID(pod *corev1.Pod, jobUID types.UID) bool {
	for i := range pod.OwnerReferences {
		if pod.OwnerReferences[i].UID == jobUID && pod.OwnerReferences[i].Kind == "Job" {
			return true
		}
	}
	return false
}

// jobHasConditionReason reports whether the Job's status carries the given
// condition type (ConditionTrue) with the given reason (the deadline-fail
// detection: Failed/DeadlineExceeded — the batch controller stamps it when
// activeDeadlineSeconds elapses without the Job completing). An absent
// condition is false (the envtest specs stamp the condition themselves
// because envtest has no batch controller).
func jobHasConditionReason(job *batchv1.Job, condType batchv1.JobConditionType, reason string) bool {
	for i := range job.Status.Conditions {
		c := &job.Status.Conditions[i]
		if c.Type == condType && c.Status == corev1.ConditionTrue && c.Reason == reason {
			return true
		}
	}
	return false
}

// setMemoryWriterCondition records the memory-writer outcome on the Loop's
// MemoryWriteDone condition (the durable record: cox describe shows whether
// a memory write was attempted and why it failed — a failed write does NOT
// fail the Loop, ADR-0004, but it IS surfaced). The condition is a
// status-only change (the operator updates it via Status().Update; it does
// NOT change the phase machine — ADR-0005: the claim channel is the only
// thing that advances).
func setMemoryWriterCondition(loop *coxv1alpha1.Loop, reason, message string) {
	status := metav1.ConditionTrue
	setCondition(loop, memoryWriterConditionType, status, reason, message)
}

// memoryWriterTrue / memoryWriterFalse are the bool values the memory-writer
// Job's container SecurityContext references (a pointer to a package-level
// bool is the deliver/verify Job's pattern; the &deliverTrue style avoids the
// modernize linter's newexpr complaint on ptrBool(x)).
var (
	memoryWriterTrue  = true
	memoryWriterFalse = false
)

// memoryWriterNonRootUID / memoryWriterNonRootGID are the writer Job
// container's runAsUser/runAsGroup (65532 — the nonroot UID the operator
// image is built with, USER 65532:65532; the deliver/verify Jobs and the
// sandbox set the same UID on the same image). RunAsNonRoot alone rejects
// the image at pod admission ("image will run as root") because the image
// manifest carries no USER; the explicit UID/GID make the run user
// explicit.
var (
	memoryWriterNonRootUID = int64(65532)
	memoryWriterNonRootGID = int64(65532)
)

// reconcileP5bMemoryWriter is the Reconcile's P5b step (the memory-writer Job +
// its NetworkPolicy). It calls ensureMemoryWriterJobAndNetPolicy, accumulates
// the requeue into *requeue (out param, so Reconcile's cyclomatic complexity
// stays low), and returns an error that propagates to Reconcile (a failed
// create/record is a reconcile error, not a best-effort skip). The caller
// (Reconcile) returns early on a non-nil error.
func (r *LoopReconciler) reconcileP5bMemoryWriter(ctx context.Context, loop *coxv1alpha1.Loop, requeue *bool) error {
	memRequeue, err := r.ensureMemoryWriterJobAndNetPolicy(ctx, loop)
	if err != nil {
		return err
	}
	*requeue = *requeue || memRequeue
	return nil
}

// runTamperGate runs the B2 tamper gate (applyTamperGate) unconditionally and
// folds its result into *changed. The gate must run every reconcile — a
// short-circuiting changed = changed || r.applyTamperGate(...) would skip the
// security gate when changed was already true this reconcile (the red B2
// tamper spec). Extracted from Reconcile to keep its cyclomatic complexity
// under the gocyclo threshold.
func (r *LoopReconciler) runTamperGate(loop *coxv1alpha1.Loop, changed *bool) {
	if r.applyTamperGate(loop) {
		*changed = true
	}
}
