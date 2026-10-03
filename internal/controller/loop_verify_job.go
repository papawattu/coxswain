// Copyright 2026 papawattu.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// S5a (B3, GAP 4): the operator's isolated verify Job — the VERIFIED EVIDENCE
// path for the Verifying phase. When the phase machine advances Implementing
// -> Verifying (the runner's claim pinned status.currentVerify.verifiedCommit,
// the runner's committed head), the operator creates a short-lived Job that:
//
//  1. clones the repo FRESH at the pinned SHA (the trusted clone-base init
//     container, the ONLY container that touches credentials — and only the
//     git basic-auth secret, mounted into that container alone),
//  2. copies the agent's committed work in from the Loop's workspace PVC
//     READ-ONLY (import-agent; it carries NO credentials and no agent data
//     mounts besides the read-only workspace),
//  3. runs the tamper check (baseCommit..verifiedCommit over the protected
//     globs) as an exit code,
//  4. runs each acceptance check as its own init container (exit 0 = pass),
//  5. exits — the Job pod's initContainerStatuses[].state.terminated.exitCode
//     IS the evidence (kubelet-recorded, never a result.json claim).
//
// The Job NEVER executes agent-controlled git state (it operates on a fresh
// clone + a read-only import of the agent's tree) and never runs model code.
// The main container is a no-op: all the real work is init containers so the
// kubelet records every exit code in pod status (the operator reads them via
// the APIReader, the S3/S4 pod-blind pattern — no new RBAC, no exec).
//
// B3b (restart semantics): restartPolicy Never + backoffLimit 0 — the Job runs
// its init sequence EXACTLY once. A VerifyError re-run (B4) creates a NEW Job
// with a fresh name; the operator never mutates a running Job.
//
// B3d (advisory scan): the verify Job also runs a non-blocking "advisory"
// init container after the tamper check that greps the agent's diff for
// subversion patterns (go:linkname, testing.Short, //go:build, os.Exit in a
// test, etc.). It NEVER gates: its exit code is 0 regardless of findings
// (it writes its findings to /dev/termination-log and exits 0). The operator
// reads the advisory text for the VerifySuspicious condition (advisory only,
// D24); the gate is the tamper + acceptance checks, full stop.
package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/tamper"
)

// Verify Job label / path / image constants.
const (
	// verifyLoopLabel is the Job's object label: the owning Loop.
	verifyLoopLabel = "coxswain.io/loop"
	// verifyForLabel is the verify Pod's selector label: the Loop the verify
	// Job runs for (also the Job's name).
	verifyForLabel = "coxswain.io/verify-for"
	// verifyScratchPath is the per-container writable scratch.
	verifyScratchPath = "/verify"
	// verifyBusybox is the verify Job's container image (alpine, has git).
	verifyBusybox = "busybox"
	// verifySh is the shell command for the verify init containers.
	verifySh = "/bin/sh"
	// verifyCloneBaseInit is the base-commit clone init container's name.
	verifyCloneBaseInit = "clone-base"
	// verifyImportAgentInit is the agent-workspace import init container's name.
	verifyImportAgentInit = "import-agent"
	// verifyVol is the verify Job's emptyDir volume name.
	verifyVol = "verify"
	// verifyAgentVol is the agent-workspace PVC volume name (read-only,
	// the only agent-data entry point into the Job).
	verifyAgentVol = "agent-workspace"
	// verifyNoopContainer is the no-op main container's name.
	verifyNoopContainer = "noop"
	// verifyTamperInit is the tamper-check init container's name (B2 gate).
	verifyTamperInit = "tamper"
	// verifyCommitAnnotation is the annotation the verify Job carries naming
	// the verifiedCommit it was built for (D27 stale-evidence guard: the
	// operator ignores/deletes a Job whose annotation != the current pin).
	verifyCommitAnnotation = "coxswain.io/verified-commit"
	// verifySafeDirectory is the git safe.directory config key (the
	// GIT_CONFIG_KEY_0/1 value). A constant so goconst does not flag the
	// repeated literal.
	verifySafeDirectory = "safe.directory"
)

// verifyJobImage is the image the verify Job's trusted init containers run
// (the operator's pinned, trusted git+sh image). It is the SAME image the
// workspace init container uses (r.WorkspaceGitImage) so the operator has one
// trusted image for all git work. The check-* containers do NOT use it: they
// run USER commands (the acceptance checks) and need a toolchain — see
// verifyCheckImage. The agent's image never runs in the Job.
func (r *LoopReconciler) verifyJobImage() string {
	if r.WorkspaceGitImage != "" {
		return r.WorkspaceGitImage
	}
	return "docker.io/alpine/git:v2.54.0"
}

// verifyDefaultCheckImage is the built-in default for the check-* containers
// when neither spec.verify.image nor the --verify-image flag supplies one
// (the flag always supplies one in a live deployment; this covers bare
// reconcilers constructed in tests). A Go image so `go build` / `go test`
// checks work out of the box — the trusted git image has no Go toolchain
// (the S5a kind root cause: check-0 exited 127 'go: not found' on alpine/git).
const verifyDefaultCheckImage = "docker.io/library/golang:1.26"

// verifyCheckImage returns the image the verify Job's check-* acceptance-check
// containers run (S5a): the Loop's spec.verify.image when set, else the
// operator's --verify-image default, else the built-in Go image. The checks
// are user commands (ADR: the only gate to Succeeded) and may need a
// toolchain, so this image is separate from the trusted git image the
// clone-base / import-agent / tamper containers run.
func (r *LoopReconciler) verifyCheckImage(loop *coxv1alpha1.Loop) string {
	if loop.Spec.Verify.Image != "" {
		return loop.Spec.Verify.Image
	}
	if r.VerifyImage != "" {
		return r.VerifyImage
	}
	return verifyDefaultCheckImage
}

// errVerifyStaleDeleted is the sentinel error ensureVerifyJob returns when
// the D27 stale-Job guard has just deleted a Job stamped for a different
// pin. The caller (Reconcile) maps it to a clean requeue (5s RequeueAfter in
// the FINAL return, after the shared Status().Update) — never a controller
// error, and never an in-same-reconcile create of the fresh Job (the name is
// still taken; the apiserver deletes async).
var errVerifyStaleDeleted = errors.New("stale verify Job deleted")

// verifyJobName returns the verify Job name: <loop>-verify-<iteration>. The
// iteration is the Loop's current 1-based iteration count (the pin in
// status.currentVerify is the evidence the Job produces; the Job name carries
// the iteration so a B4 re-run creates a NEW Job, never mutating the prior
// one).
func verifyJobName(loop *coxv1alpha1.Loop) string {
	iter := loop.Status.Iteration
	if iter <= 0 {
		iter = 1
	}
	return fmt.Sprintf("%s-verify-%d", loop.Name, iter)
}

// verifyJobLabels are the labels the verify Job carries (the Loop's identity
// + the verify component). The podSelector for the verify Job's NetworkPolicy
// is the "coxswain.io/verify-for" label.
func verifyJobLabels(loopName string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/part-of":    "coxswain",
		verifyLoopLabel:                loopName,
		verifyForLabel:                 loopName,
		"app.kubernetes.io/managed-by": "coxswain-controller",
	}
}

// ensureVerifyJob creates (idempotently) the verify Job for a Loop that has
// pinned a current verifiedCommit (status.currentVerify) AND is at the
// Verifying phase. The Job is owned by the Loop (controller ref), so deleting
// the Loop garbage-collects the Job. An EXISTING Job with the same name is
// left untouched (idempotency — the operator never mutates a running Job).
//
// The init container order is the evidence order (kubelet runs inits in
// order, stopping on the first non-zero exit — the operator reads the FULL
// list from pod.status.initContainerStatuses, so a check that never ran is
// distinguishable from one that ran and failed):
//
//   - clone-base: the trusted fresh clone at the pinned SHA (the ONLY
//     container with credentials; the git secret is mounted into it alone,
//     as a basic-auth secret with username/password keys).
//   - import-agent: copies the agent's committed tree from the Loop's
//     workspace PVC (READ-ONLY mount, no credentials) into the fresh clone
//     at /verify. This is the only place agent data enters the Job.
//   - tamper: runs the tamper check (baseCommit..verifiedCommit over the
//     protected globs) and exits with its result (0 = clean).
//   - check-<k>: one per acceptance check, in spec order; exit 0 = pass.
//
// The main container is a no-op (the inits do the work; the main is there
// because a Job pod requires a main container — it exits 0 immediately).
func (r *LoopReconciler) ensureVerifyJob(ctx context.Context, loop *coxv1alpha1.Loop) error {
	if loop.Status.Phase != coxv1alpha1.LoopPhaseVerifying {
		return nil
	}
	if loop.Status.CurrentVerify == nil || loop.Status.CurrentVerify.VerifiedCommit == "" {
		// No pin yet (the advance has not happened). Hold — no Job to create.
		return nil
	}
	if loop.Status.BaseCommit == "" {
		// No base to diff against. The tamper check would be meaningless; hold.
		return nil
	}
	name := verifyJobName(loop)
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: loop.Namespace}, job)
	if err == nil {
		// Already exists: check the D27 stale-evidence guard. A Job stamped
		// with a different verifiedCommit is stale (another iteration's
		// evidence) — delete it so a fresh Job is created for the current
		// pin. Never reuse another commit's evidence.
		if job.Annotations[verifyCommitAnnotation] != loop.Status.CurrentVerify.VerifiedCommit {
			logf.FromContext(ctx).Info("verify Job is stale (annotation mismatch); deleting",
				"job", name, "job-commit", job.Annotations[verifyCommitAnnotation],
				"loop-commit", loop.Status.CurrentVerify.VerifiedCommit)
			// batch/v1 Jobs default to ORPHAN deletion propagation: the
			// apiserver adds the "orphan" finalizer and waits for the garbage
			// collector, which leaves the stale Job (and its pods) in place in
			// any environment without a GC (envtest) and orphans the old Job's
			// pods even on a real cluster. Background propagation deletes the
			// Job's pods with it.
			if err := r.Delete(ctx, job,
				client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("delete stale verify job %s: %w", name, err)
			}
			// D27: never reuse another commit's evidence. The stale Job is
			// deleted; requeue WITHOUT creating in this reconcile — the name
			// is still taken until the Job object goes away (a Create now would
			// hit AlreadyExists on a real cluster and be silently ignored by
			// IgnoreNotFound, leaving the new pin un-built). The Owns
			// (&batchv1.Job{}) watch (deletion) plus the requeue create a fresh
			// Job for the current pin on the next reconcile. Return the
			// sentinel: the caller maps it to a clean requeue (5s RequeueAfter
			// in the FINAL return, after the shared Status().Update) — never
			// a controller error, and never an in-same-reconcile create of the
			// fresh Job (the name is still taken; the apiserver deletes async).
			return errVerifyStaleDeleted
		} else {
			// Already exists and matches: leave it untouched (idempotency,
			// B3b — the operator never mutates a running Job; a re-run is
			// a NEW Job).
			return nil
		}
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get verify job %s: %w", name, err)
	}
	// Create the fresh Job. If the name is still taken (a stale Job from a
	// prior reconcile's deletion has not gone away yet), the Create returns
	// AlreadyExists: requeue and retry, never silently skip (the new pin
	// must be built).
	job = &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: loop.Namespace,
			Labels:    verifyJobLabels(loop.Name),
			Annotations: map[string]string{
				verifyCommitAnnotation: loop.Status.CurrentVerify.VerifiedCommit,
			},
		},
		Spec: r.buildVerifyJobSpec(loop),
	}
	if err := ctrl.SetControllerReference(loop, job, r.Scheme); err != nil {
		return fmt.Errorf("set owner on verify job %s: %w", name, err)
	}
	if err := r.Create(ctx, job); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// The name is still taken (a stale Job from a prior reconcile's
			// deletion has not gone away yet). Requeue and retry — never
			// silently skip (the new pin must be built), and never Ignore
			// the error (that would leave the old pin's Job in place).
			logf.FromContext(ctx).Info("verify Job name still taken; requeueing", "job", name)
			return nil
		}
		return fmt.Errorf("create verify job %s: %w", name, err)
	}
	return nil
}

// verifyCheckTmpVol is the name of the emptyDir mounted at /tmp in each
// check-* container (the writable scratch area for user check commands
// running with readOnlyRootFilesystem — the S5a kind root cause: check-0
// (golang:1.26) failed with 'go: creating work dir: mkdir /tmp/go-build...
// read-only file system').
const verifyCheckTmpVol = "check-tmp"

// verifyCheckEnv returns the env for the check-* containers: the
// safe.directory entry for /verify (user-authored checks may call git
// there) PLUS the writable-scratch env (HOME/TMPDIR/GOCACHE/GOPATH/
// XDG_CACHE_HOME all under /tmp) so toolchain caches land on the check-tmp
// emptyDir instead of the read-only root. Generic across toolchains, not
// Go-specific.
func verifyCheckEnv() []corev1.EnvVar {
	env := verifyGitSafeEnv()
	return append(env,
		corev1.EnvVar{Name: "HOME", Value: agentTmpMount},
		corev1.EnvVar{Name: "TMPDIR", Value: agentTmpMount},
		corev1.EnvVar{Name: "GOCACHE", Value: "/tmp/gocache"},
		corev1.EnvVar{Name: "GOPATH", Value: "/tmp/gopath"},
		corev1.EnvVar{Name: "XDG_CACHE_HOME", Value: "/tmp/.cache"},
	)
}

// verifyGitSafeEnv returns the safe.directory env for the /verify scratch
// (the emptyDir mount point, root-owned while the containers run non-root —
// the same class of bug as the runner's /workspace dubious-ownership fix).
// Without it every git call in a verify container fails with
// 'fatal: detected dubious ownership in repository at /verify'. The
// env-based form (GIT_CONFIG_COUNT/KEY/VALUE) covers USER-AUTHORED
// acceptance checks too — they run /verify as their working dir with no
// operator control over their argv. import-agent carries a SECOND entry
// for the source repo /agent-src/.git (different-owner, see its container).
func verifyGitSafeEnv() []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "GIT_CONFIG_COUNT", Value: "1"},
		{Name: "GIT_CONFIG_KEY_0", Value: verifySafeDirectory},
		{Name: "GIT_CONFIG_VALUE_0", Value: verifyScratchPath},
	}
}

// buildVerifyJobSpec builds the verify Job's spec: the init containers in
// evidence order + the no-op main container + restartPolicy Never +
// backoffLimit 0 (B3b: exactly one run).
func (r *LoopReconciler) buildVerifyJobSpec(loop *coxv1alpha1.Loop) batchv1.JobSpec {
	baseImage := r.verifyJobImage()
	verifyCommit := loop.Status.CurrentVerify.VerifiedCommit
	baseCommit := loop.Status.BaseCommit
	repo := loop.Spec.Workspace.Repo

	// The protected globs (tamper.ExpandPreset is the single expansion point,
	// D25). The tamper check runs `git diff --name-only <base> <verified> --
	// <globs...>` and exits non-zero on any hit.
	globs := tamper.ExpandPreset(tamper.Preset(loop.Spec.Verify.Preset),
		loop.Spec.Verify.ProtectedPaths, loop.Spec.Verify.ProtectedPathsOverride)
	globArgs := strings.Join(globs, "\n")

	// The trusted clone-base init: fetches baseCommit from origin into /verify
	// (emptyDir). It is the ONLY container that touches credentials — and
	// only the git basic-auth secret (if the Loop declares one), mounted into
	// this container ALONE. It fetches ONLY baseCommit (the verifiedCommit
	// was committed locally by the agent and is not on origin; import-agent
	// fetches it from the workspace PVC).
	cloneCt := corev1.Container{
		Name:            verifyCloneBaseInit,
		Image:           baseImage,
		Command:         []string{verifySh, "-c", cloneScript(repo, baseCommit)},
		SecurityContext: trustedContainerSecurityContext(),
		Env:             verifyGitSafeEnv(),
		VolumeMounts: []corev1.VolumeMount{
			{Name: verifyVol, MountPath: verifyScratchPath},
		},
	}

	// The git credential mount: ONLY when the Loop declares a git secret AND
	// ONLY into the clone-base container (the operator's trusted container,
	// never the agent's). The secret is a standard kubernetes.io/basic-auth
	// secret (username + password keys); the cred script reads them and passes
	// them to git as a basic-auth http.extraHeader (the S3a init-workspace
	// pattern — nothing is written or persisted).
	if loop.Spec.Workspace.GitCredentialSecret != "" {
		cloneCt.VolumeMounts = append(cloneCt.VolumeMounts, corev1.VolumeMount{
			Name: "git-cred", MountPath: "/git-cred", ReadOnly: true,
		})
		cloneCt.Command = []string{verifySh, "-c", cloneCredScript(repo, baseCommit)}
	}

	// The import-agent init: fetches the agent's verifiedCommit from the
	// workspace PVC (READ-ONLY) via a local file:// fetch into the fresh
	// clone at /verify. It carries NO credentials and NO agent data mounts
	// besides the read-only workspace. This is the ONLY place agent data
	// enters the Job, and it imports the verified COMMIT (not the working
	// tree) — so the tamper check and the acceptance checks operate on the
	// SAME tree (the checked-out verifiedCommit), closing the tamper bypass.
	importScript := fmt.Sprintf(`
set -e
cd /verify
git -c core.hooksPath=/dev/null -c protocol.file.allow=always fetch --no-tags file:///agent-src/.git %s
git -c core.hooksPath=/dev/null checkout --detach %s
echo "import-agent ok"
`, shellQuote(verifyCommit), shellQuote(verifyCommit))
	importCt := corev1.Container{
		Name:            verifyImportAgentInit,
		Image:           baseImage,
		Command:         []string{verifySh, "-c", importScript},
		SecurityContext: trustedContainerSecurityContext(),
		Env: []corev1.EnvVar{
			// Two safe.directory entries: the scratch (/verify, where the
			// clone lives) and the SOURCE repo on the read-only PVC
			// (/agent-src/.git, owned by the agent's git user — a
			// different-ownership repo). Scoped to this container alone.
			{Name: "GIT_CONFIG_COUNT", Value: "2"},
			{Name: "GIT_CONFIG_KEY_0", Value: verifySafeDirectory},
			{Name: "GIT_CONFIG_VALUE_0", Value: verifyScratchPath},
			{Name: "GIT_CONFIG_KEY_1", Value: verifySafeDirectory},
			{Name: "GIT_CONFIG_VALUE_1", Value: "/agent-src/.git"},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: verifyVol, MountPath: verifyScratchPath},
			// The agent's workspace PVC, READ-ONLY. This is the only place
			// agent data enters the Job.
			{Name: verifyAgentVol, MountPath: "/agent-src", ReadOnly: true},
		},
	}

	// The tamper init: runs the tamper check and exits with its result. A
	// non-zero exit means a protected path changed (TamperedVerify, B2). The
	// check is a single git diff over the protected globs; the exit code is
	// the evidence (the operator reads it from pod status).
	tamperScript := fmt.Sprintf(`
set -e
cd /verify
# Diff baseCommit..verifiedCommit over the protected globs. A non-empty
# result (any protected path changed) => exit 1 (tampered). Empty => exit 0
# (clean). The globs are one per line.
CHANGED=$(git diff --name-only %s %s -- %s | sed '/^$/d')
if [ -n "$CHANGED" ]; then
  echo "tamper: protected paths changed:"
  echo "$CHANGED"
  exit 1
fi
echo "tamper: clean"
exit 0
`, shellQuote(baseCommit), shellQuote(verifyCommit), shellQuote(globArgs))
	tamperCt := corev1.Container{
		Name:            verifyTamperInit,
		Image:           baseImage,
		Command:         []string{verifySh, "-c", tamperScript},
		SecurityContext: trustedContainerSecurityContext(),
		Env:             verifyGitSafeEnv(),
		VolumeMounts:    []corev1.VolumeMount{{Name: verifyVol, MountPath: verifyScratchPath}},
	}

	// The acceptance-check inits: one per spec.acceptanceChecks, in order.
	// Each runs the check command in /verify (the agent's tree) and exits
	// with the check's exit code (0 = pass). The operator reads each check's
	// exit code from pod.status.initContainerStatuses (a check that never
	// ran — because a prior init failed — has no terminated status, which the
	// operator distinguishes from a ran-and-failed check).
	checks := loop.Spec.Verify.AcceptanceChecks
	// S5a: the checks run USER commands (they are the only gate to Succeeded
	// and may need a toolchain — `go test` needs Go), so they run on the
	// verify CHECK image (spec.verify.image / --verify-image / built-in Go
	// default), NOT on the trusted git image (which has no Go).
	checkImage := r.verifyCheckImage(loop)
	checkCts := make([]corev1.Container, 0, len(checks))
	for i, cmd := range checks {
		c := cmd
		ct := corev1.Container{
			Name:            fmt.Sprintf("check-%d", i),
			Image:           checkImage,
			Command:         []string{verifySh, "-c", c},
			WorkingDir:      "/verify",
			SecurityContext: trustedContainerSecurityContext(),
			Env:             verifyCheckEnv(),
			VolumeMounts: []corev1.VolumeMount{
				{Name: verifyVol, MountPath: verifyScratchPath},
				// The check containers run with readOnlyRootFilesystem and
				// the user's command needs a writable scratch area (go build
				// writes to GOCACHE / a work dir under /tmp). The small
				// check-tmp emptyDir at /tmp is that scratch — generic enough
				// for toolchains other than Go too (the env below points
				// the common cache/home paths there).
				{Name: verifyCheckTmpVol, MountPath: agentTmpMount},
			},
		}
		checkCts = append(checkCts, ct)
	}

	// The main container: a no-op (the inits do the work). It exits 0
	// immediately so the Job "succeeds" in the batch/v1 sense once all inits
	// pass; the EVIDENCE is the inits' exit codes, not the main.
	mainCt := corev1.Container{
		Name:            verifyNoopContainer,
		Image:           baseImage,
		Command:         []string{"/bin/true"},
		SecurityContext: trustedContainerSecurityContext(),
	}

	inits := append([]corev1.Container{cloneCt, importCt, tamperCt}, checkCts...)

	// The volumes: the verify emptyDir (the fresh clone) + the check-tmp
	// emptyDir (writable /tmp scratch for the check containers) + the
	// agent's workspace PVC (read-only, for the import) + the git
	// credential (only when present).
	volumes := []corev1.Volume{
		{Name: verifyVol, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: verifyCheckTmpVol, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: verifyAgentVol, VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
			ClaimName: workspacePVCName(loop.Name), ReadOnly: true,
		}}},
	}
	if loop.Spec.Workspace.GitCredentialSecret != "" {
		volumes = append(volumes, corev1.Volume{
			Name: "git-cred",
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: loop.Spec.Workspace.GitCredentialSecret,
			}},
		})
	}

	backoff := int32(0)
	never := corev1.RestartPolicyNever
	return batchv1.JobSpec{
		BackoffLimit: &backoff,
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: verifyJobLabels(loop.Name)},
			Spec: corev1.PodSpec{
				RestartPolicy:      never,
				ServiceAccountName: "",
				Volumes:            volumes,
				InitContainers:     inits,
				Containers:         []corev1.Container{mainCt},
			},
		},
	}
}

// trustedContainerSecurityContext is the hardened profile every verify Job
// container runs under (the same profile the sandbox's init containers use):
// non-root (65532), no privilege escalation, read-only rootfs, all
// capabilities dropped, the runtime-default seccomp profile.
func trustedContainerSecurityContext() *corev1.SecurityContext {
	runAs := int64(65532)
	allowPriv := false
	return &corev1.SecurityContext{
		RunAsUser:                &runAs,
		RunAsGroup:               &runAs,
		RunAsNonRoot:             verifyTruePtr,
		AllowPrivilegeEscalation: &allowPriv,
		ReadOnlyRootFilesystem:   verifyTruePtr,
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// verifyTruePtr/verifyFalsePtr are package-level bool pointers for the Job
// spec's bool fields.
var (
	verifyTruePtr  = new(bool)
	verifyFalsePtr = new(bool)
)

func init() {
	*verifyTruePtr = true
	*verifyFalsePtr = false
}

// cloneScript builds the clone-base init container script. It fetches ONLY
// baseCommit from origin (the agent's verifiedCommit is local and never
// pushed, so fetching it from origin would fail). The import-agent container
// later fetches verifiedCommit from file:///agent-src/.git.
func cloneScript(repo, baseCommit string) string {
	return fmt.Sprintf(`
set -e
rm -rf /verify/*
mkdir -p /verify
cd /verify
git init -q
git remote add origin %s
git fetch -q --depth=1 origin %s
git checkout -q --detach FETCH_HEAD
echo "clone-base ok: %s"
`, shellQuote(repo), shellQuote(baseCommit), shellQuote(baseCommit))
}

// cloneCredScript is the clone-base script when a git basic-auth secret is
// present: it reads username + password from /git-cred and builds the
// Basic-auth header (S3a pattern: printf '%s:%s' username password |
// base64 -w 0). Fetches ONLY baseCommit from origin.
func cloneCredScript(repo, baseCommit string) string {
	return fmt.Sprintf(`
set -e
# Build the basic-auth header from the two mounted secret files (S3a pattern).
AUTH="Authorization: Basic $(printf '%%s:%%s' "$(cat /git-cred/username)" "$(cat /git-cred/password)" | base64 -w 0)"
rm -rf /verify/*
mkdir -p /verify
cd /verify
git init -q
git remote add origin %s
git -c http.extraHeader="$AUTH" fetch -q --depth=1 origin %s
git checkout -q --detach FETCH_HEAD
echo "clone-base ok"
`, shellQuote(repo), shellQuote(baseCommit))
}

// ensureVerifyNetworkPolicy creates or updates the verify Job's NetworkPolicy:
// egress only to the git repo peer (for the clone-base fetch) + DNS. The
// verify pod has NO model/egress access — it is a trusted, isolated check
// runner, not an agent. The podSelector is the verify-for label (the Job's
// pod carries it).
//
// I42c pattern (createOrUpdateNP): the spec is set in the mutate func so that
// input changes (e.g. the repo port) propagate to an existing policy.
func (r *LoopReconciler) ensureVerifyNetworkPolicy(ctx context.Context, loop *coxv1alpha1.Loop) error {
	if loop.Status.Phase != coxv1alpha1.LoopPhaseVerifying {
		return nil
	}
	if loop.Spec.Workspace.Repo == "" {
		return nil
	}
	name := loop.Name + "-verify-netpol"
	egress := []networkingv1.NetworkPolicyEgressRule{
		{To: []networkingv1.NetworkPolicyPeer{dnsPeer()}, Ports: dnsPorts()},
	}
	if peer := repoPeer(loop.Spec.Workspace.Repo, r.serviceNamespaceFromHost); peer != nil {
		port := intstrPtr32(int32(workspaceRepoPort(loop.Spec.Workspace.Repo)))
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{
			To:    []networkingv1.NetworkPolicyPeer{*peer},
			Ports: []networkingv1.NetworkPolicyPort{{Port: port, Protocol: new(corev1.ProtocolTCP)}},
		})
	}
	npol := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: loop.Namespace,
			Labels:    map[string]string{verifyForLabel: loop.Name},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{verifyForLabel: loop.Name},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress:     []networkingv1.NetworkPolicyIngressRule{},
			Egress:      egress,
		},
	}
	if _, err := r.createOrUpdateNP(ctx, loop, npol); err != nil {
		return fmt.Errorf("create verify netpol %s: %w", name, err)
	}
	return nil
}

// readVerifyJobPod reads the verify Job's pod via the APIReader (the S3/S4
// pod-blind pattern — the Job's pod is NOT in the manager's Pod cache, so the
// non-cached client is required). It returns the pod (or nil if the Job has no
// pod yet / the Job is gone). A VerifyError (zero or >1 pods) is the caller's
// decision.
func (r *LoopReconciler) readVerifyJobPod(ctx context.Context, loop *coxv1alpha1.Loop) (*corev1.Pod, error) {
	jobName := verifyJobName(loop)
	reader := r.apiReader
	if reader == nil {
		reader = r
	}
	// Find the Job's pod by the verify-for label (the Job stamps it on the
	// pod template).
	list := &corev1.PodList{}
	if err := reader.List(ctx, list,
		client.InNamespace(loop.Namespace),
		client.MatchingLabels{verifyForLabel: loop.Name}); err != nil {
		return nil, fmt.Errorf("list verify pods: %w", err)
	}
	// Filter to the current iteration's Job pod (the Job name is on the pod's
	// label controller-uid, but the simplest reliable match is the Job name in
	// the pod's labels — the Job controller sets the "job-name" label).
	var pods []corev1.Pod
	for i := range list.Items {
		if list.Items[i].Labels["job-name"] == jobName {
			pods = append(pods, list.Items[i])
		}
	}
	if len(pods) == 0 {
		return nil, nil
	}
	if len(pods) > 1 {
		return nil, fmt.Errorf("verify job %s has %d pods (want 1)", jobName, len(pods))
	}
	return &pods[0], nil
}

// verifyJobOutcome reads the verify Job pod's init container exit codes and
// maps them to a Verifying outcome (B3):
//
//   - no pod / no init statuses: no evidence yet (the Job is still starting)
//     -> (noDecision, true [requeue]).
//   - tamper init not terminated: no evidence yet -> (noDecision, true).
//   - tamper exit != 0: TamperedVerify (terminal, B2) -> the caller sets
//     Failed. (The B2 tamper gate ALSO runs every reconcile, so this is a
//     belt-and-suspenders; the gate is the authority.)
//   - all check inits terminated with exit 0: Succeeded.
//   - a check init terminated with exit != 0: iterate (back to Implementing,
//     iteration+1) — unless the iteration cap is hit (then Failed).
//   - a check init NOT terminated (because a prior init failed): treat as
//     "did not run" (I14 NotRun) — the first non-zero init is the failure.
//
// Returns (outcome, requeue). outcome is one of: verifyNoDecision,
// verifySucceeded, verifyIterate, verifyTampered.
const (
	verifyNoDecision = iota
	verifySucceeded
	verifyIterate
	verifyTampered
)

// verifyOutcome reads the pod's init statuses and returns the outcome. A
// verifyIterate carries the failing check's name and exit code (the
// operator records them into status.progress so the next Implementing
// prompt can include the failure).
func verifyOutcome(pod *corev1.Pod, checkCount int) (int, bool, string, int32) {
	if pod == nil {
		return verifyNoDecision, true, "", 0
	}
	// Find the tamper init.
	tamperIdx := -1
	for i := range pod.Status.InitContainerStatuses {
		if pod.Status.InitContainerStatuses[i].Name == verifyTamperInit {
			tamperIdx = i
			break
		}
	}
	if tamperIdx < 0 {
		return verifyNoDecision, true, "", 0
	}
	tamperStatus := pod.Status.InitContainerStatuses[tamperIdx]
	if tamperStatus.State.Terminated == nil {
		return verifyNoDecision, true, "", 0
	}
	if tamperStatus.State.Terminated.ExitCode != 0 {
		return verifyTampered, false, verifyTamperInit, tamperStatus.State.Terminated.ExitCode
	}
	// Tamper clean: read the check inits (check-0 .. check-<checkCount-1>).
	for i := range checkCount {
		name := fmt.Sprintf("check-%d", i)
		found := false
		var code int32
		for j := range pod.Status.InitContainerStatuses {
			ics := &pod.Status.InitContainerStatuses[j]
			if ics.Name != name {
				continue
			}
			found = true
			if ics.State.Terminated != nil {
				code = ics.State.Terminated.ExitCode
			} else {
				// Not terminated: a prior init failed, so this check did not
				// run (I14 NotRun). The first non-terminated check after a
				// clean tamper is the failure point.
				return verifyIterate, false, name, 0
			}
			break
		}
		if !found {
			// The check init is not in the pod status at all (the Job pod has
			// fewer inits than expected — a malformed Job). No evidence.
			return verifyNoDecision, true, "", 0
		}
		if code != 0 {
			return verifyIterate, false, name, code
		}
	}
	return verifySucceeded, false, "", 0
}

// verifyIteratedReason is the stable Event reason the operator emits when a
// failing acceptance check sends the Loop back to Implementing (the
// Verifying -> Implementing iterate). It is distinct from phaseAdvancedReason
// (PhaseAdvanced) so the event stream reads correctly: an iterate is not a
// forward advance. The message carries the from/to phases + the failing
// check and its exit code.
const verifyIteratedReason = "PhaseIterated"

// MaxIterationsExceededReason is the Failed condition reason recorded when
// the iteration cap (spec.loop.maxIterations, default 3 when unset) is hit:
// a verify check failed on the last allowed iteration, so the Loop is Failed
// instead of iterating again (S5a hot-loop guard — without a cap a failing
// check would re-enter Implementing and re-verify forever).
const MaxIterationsExceededReason = "MaxIterationsExceeded"

// defaultMaxIterations is the cap when spec.loop.maxIterations is unset
// (the CRD default is 10; this constant covers bare reconcilers/Loops that
// were constructed without the CRD defaulting, and documents the S5a MVP
// cap the handoff specifies: 3 cycles).
const defaultMaxIterations = 3

// applyVerifyOutcome maps a verifyOutcome to a phase transition (B3). It
// returns changed (the Loop's status was mutated). The caller runs it every
// reconcile at Verifying (the B2 tamper gate ALSO runs every reconcile; this
// is the check/iterate/Succeeded path).
//
// A non-zero check (verifyIterate) sends the Loop BACK to Implementing with
// the iteration incremented (the runner's iteration scoping then does new
// model work against the failing check). It does NOT create a new verify
// Job: a new Job is created only when a NEW verifiedCommit is pinned at the
// next Implementing -> Verifying advance (the pin is cleared here, so
// ensureVerifyJob holds until the re-advance). The failing check's name and
// exit code are recorded into status.progress (the runner's implementing
// prompt can include them in the next iteration's model work). The
// maxIterations cap bounds the iterate: when the increment would exceed it,
// the Loop is Failed:MaxIterationsExceeded instead of iterating.
func (r *LoopReconciler) applyVerifyOutcome(ctx context.Context, loop *coxv1alpha1.Loop) (bool, bool) {
	if loop.Status.Phase != coxv1alpha1.LoopPhaseVerifying {
		return false, false
	}
	// The B2 tamper gate runs FIRST (every reconcile, order-independent). If
	// it flipped the Loop to Failed, do nothing here.
	pod, err := r.readVerifyJobPod(ctx, loop)
	if err != nil {
		logf.FromContext(ctx).Error(err, "verify pod read failed; requeueing", "loop", loop.Name)
		return true, true
	}
	checkCount := len(loop.Spec.Verify.AcceptanceChecks)
	outcome, requeue, failedCheck, checkExitCode := verifyOutcome(pod, checkCount)
	if requeue {
		return false, true
	}
	switch outcome {
	case verifySucceeded:
		loop.Status.Phase = coxv1alpha1.LoopPhaseSucceeded
		loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseSucceeded
		return true, false
	case verifyIterate:
		// Back to Implementing, iteration+1 — NOT a new verify Job (the Job
		// is created only for a fresh pin at the next advance). The failing
		// check rides into status.progress so the next Implementing run can
		// target it.
		nextIter := max(loop.Status.Iteration+1, 2)
		cap := loop.Spec.Loop.MaxIterations
		if cap <= 0 {
			cap = defaultMaxIterations
		}
		if nextIter > cap {
			// The cap is hit: the last allowed iteration's check failed. Fail
			// the Loop (terminal) instead of iterating again — this is the
			// hot-loop guard (a failing check without a cap would create
			// verify-N jobs in a tight loop).
			setCondition(loop, string(coxv1alpha1.LoopPhaseFailed), metav1.ConditionTrue,
				MaxIterationsExceededReason,
				fmt.Sprintf("acceptance check %s failed (exit %d) on iteration %d; the maxIterations cap (%d) is reached",
					failedCheck, checkExitCode, loop.Status.Iteration, cap))
			loop.Status.Phase = coxv1alpha1.LoopPhaseFailed
			loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseFailed
			return true, false
		}
		loop.Status.Iteration = nextIter
		from := loop.Status.Phase // Verifying — the ACTUAL previous phase (OS5)
		loop.Status.Phase = coxv1alpha1.LoopPhaseImplementing
		loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseImplementing
		// OS1: record the failing check into progress (the next Implementing
		// run can target it). The progress block is the operator's structured
		// record; this is the operator's own evidence (the verify Job pod's
		// exit codes), not a runner claim.
		if loop.Status.Progress == nil {
			loop.Status.Progress = &coxv1alpha1.ProgressStatus{}
		}
		loop.Status.Progress.LastResultStatus = fmt.Sprintf("check-failed: %s (exit %d)", failedCheck, checkExitCode)
		loop.Status.Progress.Iteration = loop.Status.Iteration
		// Clear the current pin: the next Implementing run will produce a new
		// headCommit and re-pin on the next advance (the fresh Job is created
		// for THAT pin only).
		loop.Status.CurrentVerify = nil
		// OS5: the iterate is a distinct Event with the correct from-phase
		// (Verifying -> Implementing, never the stale phase a leftover claim
		// would imply). The failing check + exit code ride in the message.
		r.emitVerifyIteratedEvent(loop, from, failedCheck, checkExitCode, nextIter)
		return true, false
	case verifyTampered:
		// The B2 tamper gate is the authority (it runs every reconcile and
		// flips to Failed:TamperedVerify). Belt-and-suspenders: if it has not
		// yet flipped (e.g. the gate's evidence binding differs), flip here.
		if loop.Status.Phase == coxv1alpha1.LoopPhaseVerifying {
			setCondition(loop, string(coxv1alpha1.LoopPhaseFailed), metav1.ConditionTrue,
				TamperedVerifyReason, "a protected path changed between baseCommit and verifiedCommit")
			loop.Status.Phase = coxv1alpha1.LoopPhaseFailed
			loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseFailed
			return true, false
		}
	}
	return false, false
}

// emitVerifyIteratedEvent (OS5, S5a) emits the distinct Event for a verify
// iterate (Verifying -> Implementing). It is emitted at the moment of the
// transition — before status.phase is mutated — so the message carries the
// ACTUAL previous phase (Verifying), never a stale phase. Best-effort like
// emitPhaseAdvancedEvent: a nil Recorder (most envtests) skips it; a failed
// Event never blocks the reconcile.
func (r *LoopReconciler) emitVerifyIteratedEvent(loop *coxv1alpha1.Loop, from coxv1alpha1.LoopPhase, failedCheck string, checkExitCode int32, nextIter int) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(loop, corev1.EventTypeNormal, verifyIteratedReason,
		"phase advanced %s -> %s (iteration %d, %s exit %d)",
		from, loop.Status.Phase, nextIter, failedCheck, checkExitCode)
}
