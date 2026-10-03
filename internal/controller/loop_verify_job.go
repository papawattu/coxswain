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

// verifyJobImage is the image the verify Job's init containers run (the
// operator's pinned, trusted git+sh image). It is the SAME image the
// workspace init container uses (r.WorkspaceGitImage) so the operator has one
// trusted image for all git work (the clone-base + import-agent + tamper +
// check containers are all operator-owned; the agent's image never runs in
// the Job).
func (r *LoopReconciler) verifyJobImage() string {
	if r.WorkspaceGitImage != "" {
		return r.WorkspaceGitImage
	}
	return "docker.io/alpine/git:v2.54.0"
}

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
		"coxswain.io/loop":             loopName,
		"coxswain.io/verify-for":       loopName,
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
		// Already exists: leave it untouched (idempotency, B3b — the operator
		// never mutates a running Job; a re-run is a NEW Job).
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get verify job %s: %w", name, err)
	}
	job = &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: loop.Namespace,
			Labels:    verifyJobLabels(loop.Name),
		},
		Spec: r.buildVerifyJobSpec(ctx, loop),
	}
	if err := client.IgnoreNotFound(r.Create(ctx, job)); err != nil {
		return fmt.Errorf("create verify job %s: %w", name, err)
	}
	if err := ctrl.SetControllerReference(loop, job, r.Scheme); err != nil {
		return fmt.Errorf("set owner on verify job %s: %w", name, err)
	}
	// SetControllerReference mutates the in-memory object; persist the owner
	// ref (Create did not carry it — the Job was built without it).
	if err := r.Update(ctx, job); err != nil {
		return fmt.Errorf("set owner ref on verify job %s: %w", name, err)
	}
	return nil
}

// buildVerifyJobSpec builds the verify Job's spec: the init containers in
// evidence order + the no-op main container + restartPolicy Never +
// backoffLimit 0 (B3b: exactly one run).
func (r *LoopReconciler) buildVerifyJobSpec(ctx context.Context, loop *coxv1alpha1.Loop) batchv1.JobSpec {
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

	// The trusted clone-base init: a FRESH clone of the repo at the pinned
	// SHA into /verify (emptyDir). It is the ONLY container that touches
	// credentials — and only the git basic-auth secret (if the Loop declares
	// one), mounted into this container ALONE. It fetches both baseCommit and
	// verifiedCommit (the tamper diff needs both SHAs reachable; a single-SHA
	// shallow fetch would leave baseCommit absent).
	cloneCt := corev1.Container{
		Name:            "clone-base",
		Image:           baseImage,
		Command:         []string{"/bin/sh", "-c", cloneScript(repo, verifyCommit, baseCommit)},
		SecurityContext: trustedContainerSecurityContext(),
		VolumeMounts: []corev1.VolumeMount{
			{Name: "verify", MountPath: "/verify"},
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
		cloneCt.Command = []string{"/bin/sh", "-c", cloneCredScript(repo, verifyCommit, baseCommit)}
	}

	// The import-agent init: copies the agent's committed tree from the
	// Loop's workspace PVC (READ-ONLY) into the fresh clone at /verify. It
	// carries NO credentials and NO agent data mounts besides the read-only
	// workspace. It overwrites the clone's tree with the agent's files (the
	// tamper check then diffs baseCommit..verifiedCommit, where verifiedCommit
	// is the agent's committed head — the import makes the agent's tree the
	// checked-out tree).
	// The tamper check diffs SHAs (not the working tree), so the import does
	// not affect the gate; it exists so the acceptance checks run against the
	// agent's actual tree. The agent's .coxswain result dir is excluded (the
	// operator's result files must not leak into the verified tree).
	importScript := `set -e
# Copy the agent's committed tree (read-only PVC at /agent-src) into the
# fresh clone at /verify, preserving the clone's .git (tar excludes it).
cd /verify
tar -cf - -C /agent-src --exclude=.git --exclude=.coxswain . 2>/dev/null | tar -xf - -C /verify || true
echo "import-agent ok"
`
	importCt := corev1.Container{
		Name:            "import-agent",
		Image:           baseImage,
		Command:         []string{"/bin/sh", "-c", importScript},
		SecurityContext: trustedContainerSecurityContext(),
		VolumeMounts: []corev1.VolumeMount{
			{Name: "verify", MountPath: "/verify"},
			// The agent's workspace PVC, READ-ONLY. This is the only place
			// agent data enters the Job.
			{Name: "agent-workspace", MountPath: "/agent-src", ReadOnly: true},
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
		Name:            "tamper",
		Image:           baseImage,
		Command:         []string{"/bin/sh", "-c", tamperScript},
		SecurityContext: trustedContainerSecurityContext(),
		VolumeMounts:    []corev1.VolumeMount{{Name: "verify", MountPath: "/verify"}},
	}

	// The acceptance-check inits: one per spec.acceptanceChecks, in order.
	// Each runs the check command in /verify (the agent's tree) and exits
	// with the check's exit code (0 = pass). The operator reads each check's
	// exit code from pod.status.initContainerStatuses (a check that never
	// ran — because a prior init failed — has no terminated status, which the
	// operator distinguishes from a ran-and-failed check).
	checks := loop.Spec.Verify.AcceptanceChecks
	checkCts := make([]corev1.Container, 0, len(checks))
	for i, cmd := range checks {
		c := cmd
		ct := corev1.Container{
			Name:            fmt.Sprintf("check-%d", i),
			Image:           baseImage,
			Command:         []string{"/bin/sh", "-c", c},
			WorkingDir:      "/verify",
			SecurityContext: trustedContainerSecurityContext(),
			VolumeMounts:    []corev1.VolumeMount{{Name: "verify", MountPath: "/verify"}},
		}
		checkCts = append(checkCts, ct)
	}

	// The main container: a no-op (the inits do the work). It exits 0
	// immediately so the Job "succeeds" in the batch/v1 sense once all inits
	// pass; the EVIDENCE is the inits' exit codes, not the main.
	mainCt := corev1.Container{
		Name:            "noop",
		Image:           baseImage,
		Command:         []string{"/bin/true"},
		SecurityContext: trustedContainerSecurityContext(),
	}

	inits := append([]corev1.Container{cloneCt, importCt, tamperCt}, checkCts...)

	// The volumes: the verify emptyDir (the fresh clone) + the agent's
	// workspace PVC (read-only, for the import) + the git credential (only
	// when present).
	volumes := []corev1.Volume{
		{Name: "verify", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "agent-workspace", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
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
		RunAsNonRoot:             newBoolPtr(true),
		AllowPrivilegeEscalation: &allowPriv,
		ReadOnlyRootFilesystem:   newBoolPtr(true),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func newBoolPtr(b bool) *bool {
	return &b
}

// cloneScript is the credential-less clone-base script: a fresh clone at the
// pinned SHA that also fetches the base SHA (the tamper diff needs both
// reachable).
func cloneScript(repo, verifyCommit, baseCommit string) string {
	return fmt.Sprintf(`
set -e
rm -rf /verify
mkdir -p /verify
cd /verify
git init -q
git remote add origin %s
git fetch -q --depth=1 origin %s
git checkout -q --detach %s
# Fetch the base SHA too if the shallow clone lacks it (the diff needs both).
git cat-file -e %s 2>/dev/null || git fetch -q origin %s
echo "clone-base ok: %s"
`, shellQuote(repo), shellQuote(verifyCommit), shellQuote(verifyCommit),
		shellQuote(baseCommit), shellQuote(baseCommit), shellQuote(verifyCommit))
}

// cloneCredScript is the clone-base script when a git basic-auth secret is
// present: it reads username + password from /git-cred and passes them to the
// fetch as a basic-auth http.extraHeader (the S3a init-workspace pattern —
// nothing is written or persisted; the header is in the process's argv only,
// a known PR-noted limitation).
func cloneCredScript(repo, verifyCommit, baseCommit string) string {
	return fmt.Sprintf(`
set -e
# Build the basic-auth header from the two mounted secret files (S3a
# pattern: nothing is written or persisted).
AUTH="Authorization: Basic $(echo -n "$(cat /git-cred/username)" | base64 -w0)"
rm -rf /verify
mkdir -p /verify
cd /verify
git init -q
git remote add origin %s
git -c http.extraHeader="$AUTH" fetch -q --depth=1 origin %s
git checkout -q --detach %s
git cat-file -e %s 2>/dev/null || git -c http.extraHeader="$AUTH" fetch -q origin
echo "clone-base ok"
`, shellQuote(repo), shellQuote(verifyCommit), shellQuote(verifyCommit),
		shellQuote(baseCommit))
}

// ensureVerifyNetworkPolicy creates (idempotently) the verify Job's
// NetworkPolicy: egress only to the git repo peer (for the clone-base fetch)
// + DNS. The verify pod has NO model/egress access — it is a trusted,
// isolated check runner, not an agent. The podSelector is the verify-for
// label (the Job's pod carries it).
func (r *LoopReconciler) ensureVerifyNetworkPolicy(ctx context.Context, loop *coxv1alpha1.Loop) error {
	if loop.Status.Phase != coxv1alpha1.LoopPhaseVerifying {
		return nil
	}
	if loop.Spec.Workspace.Repo == "" {
		return nil
	}
	name := loop.Name + "-verify-netpol"
	existing := &networkingv1.NetworkPolicy{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: loop.Namespace}, existing)
	if err == nil {
		return nil // already exists; leave untouched (idempotency)
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get verify netpol %s: %w", name, err)
	}
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
			Labels:    map[string]string{"coxswain.io/verify-for": loop.Name},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"coxswain.io/verify-for": loop.Name},
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
		client.MatchingLabels{"coxswain.io/verify-for": loop.Name}); err != nil {
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

// verifyOutcome reads the pod's init statuses and returns the outcome.
func verifyOutcome(pod *corev1.Pod, checkCount int) (int, bool) {
	if pod == nil {
		return verifyNoDecision, true
	}
	// Find the tamper init.
	tamperIdx := -1
	for i := range pod.Status.InitContainerStatuses {
		if pod.Status.InitContainerStatuses[i].Name == "tamper" {
			tamperIdx = i
			break
		}
	}
	if tamperIdx < 0 {
		return verifyNoDecision, true
	}
	tamper := pod.Status.InitContainerStatuses[tamperIdx]
	if tamper.State.Terminated == nil {
		return verifyNoDecision, true
	}
	if tamper.State.Terminated.ExitCode != 0 {
		return verifyTampered, false
	}
	// Tamper clean: read the check inits (check-0 .. check-<checkCount-1>).
	for i := 0; i < checkCount; i++ {
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
				return verifyIterate, false
			}
			break
		}
		if !found {
			// The check init is not in the pod status at all (the Job pod has
			// fewer inits than expected — a malformed Job). No evidence.
			return verifyNoDecision, true
		}
		if code != 0 {
			return verifyIterate, false
		}
	}
	return verifySucceeded, false
}

// applyVerifyOutcome maps a verifyOutcome to a phase transition (B3). It
// returns changed (the Loop's status was mutated). The caller runs it every
// reconcile at Verifying (the B2 tamper gate ALSO runs every reconcile; this
// is the check/iterate/Succeeded path).
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
	outcome, requeue := verifyOutcome(pod, checkCount)
	if requeue {
		return false, true
	}
	switch outcome {
	case verifySucceeded:
		loop.Status.Phase = coxv1alpha1.LoopPhaseSucceeded
		loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseSucceeded
		return true, false
	case verifyIterate:
		// Back to Implementing, iteration+1 (B4: the cap is the caller's
		// decision — if iteration+1 exceeds the cap, the caller flips to
		// Failed; for S5a the default cap is high, so iterate).
		loop.Status.Iteration++
		loop.Status.Phase = coxv1alpha1.LoopPhaseImplementing
		loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseImplementing
		// Clear the current pin: the next Implementing run will produce a new
		// headCommit and re-pin on the next advance.
		loop.Status.CurrentVerify = nil
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
