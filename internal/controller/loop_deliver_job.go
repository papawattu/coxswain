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

// S6: the deliver Job — the delivery evidence path for a SUCCESSFUL,
// VERIFIED Loop (phase Succeeded + spec.delivery.mode == PullRequest).
//
// The deliver Job reuses the operator's WORKSPACE git credential (basic
// auth) for git, and — for a GitHub delivery — the SAME Secret's password
// as the API Bearer token (the Secret is basic auth with username
// "x-access-token" and password = the token; the API call sends the
// password as a Bearer; Gitea-compatible providers use the same basic pair
// for both git and API). The container layout:
//
//   - clone-base (CREDS): fetch the base ref's commit from the remote into
//     the scratch volume — the PR base's evidence (the base branch's
//     commit) and the remote mirror the push container pushes from.
//   - import-agent (NO CREDS, agent PVC read-only, NO network): fetch the
//     pinned verifiedCommit from the agent's workspace PVC over the file
//     protocol (protocol.file.allow=always, core.hooksPath=/dev/null — the
//     agent's repo could carry hooks the operator must never run), and
//     ASSERT the resolved commit == the pinned verifiedCommit (a mismatch
//     fails the Job — D27 evidence integrity: the deliver Job pushes ONLY
//     the commit the operator verified).
//   - push (CREDS, NO agent PVC): push the imported commit to
//     refs/heads/<prefix><loop> (NEVER --force; refusing a push whose
//     target ref equals baseBranch or main/master), create the pull request
//     via the provider API (provider inferred from the repo host:
//     github.com -> GitHub, everything else -> Gitea-compatible; idempotent
//     — an open PR for the branch is reused), and write
//     {branch, commit, prNumber, prURL} to /dev/termination-log.
//
// Idempotency (one Job per verifiedCommit): the Job is named <loop>-deliver
// and stamped with the coxswain.io/verified-commit annotation (D27-style).
// A Job stamped for a different verifiedCommit is deleted (Background
// propagation) and the reconcile requeues (the name is still taken until
// the async delete lands); a Job for the same one is left alone and its
// outcome is mapped. The PR create is idempotent on the provider side (the
// push container reuses an open PR for the branch).
//
// Outcome read-back: the operator reads the push container's termination
// message via its APIReader (pod-blind, like the S3/S4 read-backs) and
// validates it STRICTLY (parseDeliverTermination: sized; branch == the
// delivery branch; commit == the pinned verifiedCommit; prNumber positive;
// prURL well-formed on the repo host — or github.com for a GitHub delivery —
// with path /pulls/<prNumber>). A valid message is written to
// status.delivery + the Delivered=True condition; a failed init/push
// container is Delivered=False reason DeliveryFailed — the Job is NOT
// retried (backoffLimit 0, one Job per verifiedCommit); the operator
// re-runs delivery by clearing status.delivery + the condition.
//
// Network: the deliver Job's pod gets its OWN NetworkPolicy
// (<loop>-deliver-np, deliverNetpolName): DNS + the egress proxy (I42) when the repo host
// is external (the push + the provider API traverse the proxy —
// NetworkPolicy cannot name hostnames; the proxy's SNI allowlist carries
// the repo host and, for GitHub, api.github.com). An in-cluster repo keeps
// the direct repo-peer rule (no proxy hop).
package controller

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// deliver Job name/label constants.
const (
	deliverComponent = "deliver"

	// deliverForLabel is the deliver pod's selector label: the Loop the
	// deliver Job was built for. A DISTINCT key from coxswain.io/loop, which
	// the AGENT pod template also carries: a pod-blind client that falls
	// back to r.Client.List would otherwise see the sandbox pod too and
	// refuse the read as "multiple deliver pods" (the S4 mutation spec's
	// failure mode — the S6 read must stay pod-blind). The Job stamps it on
	// the pod template (deliverJobLabels); the deliver pod NetworkPolicy
	// selects on it (deliverJobPodSelector).
	deliverForLabel = "coxswain.io/deliver-for"

	// deliver container names (kubelet records every exit code; the push
	// container is the MAIN container, so its termination message rides in
	// containerStatuses[push]).
	deliverCloneBase = "clone-base"
	deliverImport    = "import-agent"
	deliverPush      = "push"

	// deliver volumes + paths.
	deliverScratchPath = "/deliver"
	deliverScratchVol  = "deliver-scratch"
	deliverAgentVol    = "agent-workspace"
	deliverAgentSrc    = "/agent-src"
)

// deliver termination-message limits (strict validation, operator read).
const (
	deliverTermMsgMaxBytes = 4096
	deliverPRURLMaxBytes   = 2048
)

// deliverProvider selects the PR API provider from the repo host
// (github.com -> GitHub; everything else -> Gitea-compatible).
type deliverProvider int

const (
	deliverProviderGitHub deliverProvider = iota
	deliverProviderGitea
)

// draftTitlePrefixFor returns the PR-title prefix for a draft delivery:
// Gitea's PR API ignores the "draft" field (Gitea has no draft PRs), so a
// Gitea draft delivery is marked in the PR title instead ("WIP: "); GitHub
// honours the field and the prefix is empty.
func draftTitlePrefixFor(draft bool, prov deliverProvider) string {
	if draft && prov == deliverProviderGitea {
		return "WIP: "
	}
	return ""
}

func deliverProviderForRepo(repo string) (deliverProvider, string) {
	u, err := url.Parse(repo)
	if err != nil || u.Host == "" {
		return deliverProviderGitea, ""
	}
	host := strings.ToLower(u.Hostname())
	if host == githubHost {
		return deliverProviderGitHub, host
	}
	return deliverProviderGitea, host
}

// deliverRepoOwnerName returns the owner and repo name a spec.workspace.repo
// URL names: the last two path segments (<owner>/<repo>), with a .git suffix
// stripped from the name. The provider API paths (/repos/<owner>/<name>) and
// the PR page paths (/<owner>/<name>/pull[s]/<n>) are derived from it. A repo
// URL without exactly those two segments returns empty values.
func deliverRepoOwnerName(repo string) (owner, name string) {
	u, err := url.Parse(repo)
	if err != nil {
		return "", ""
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(segs) < 2 {
		return "", ""
	}
	name = strings.TrimSuffix(segs[len(segs)-1], ".git")
	owner = segs[len(segs)-2]
	return owner, name
}

// deliverJobName is the deliver Job's name: <loop>-deliver.
// deliverJobName returns the deliver Job name for a Loop (D20: shared
// derivedName helper — '<loop>-deliver', truncated+hashed only for the
// near-max names that would exceed the 63-char DNS-1035 budget).
func deliverJobName(loopName string) string { return derivedName(loopName, "-deliver") }

// ensureDeliver runs the S6 deliver step for one reconcile: ensure the deliver
// Job (create or stale-delete), ensure the deliver pod's NetworkPolicy, and
// read the push container's termination message back into status.delivery. It
// returns (requeue, error): requeue is true when the stale-Job guard deleted a
// Job this reconcile and another reconcile is needed to observe the result —
// the caller maps it to a 5s RequeueAfter (like the verify stale guard), no
// retry loop, no controller error. Extracted from Reconcile so the top-level
// reconcile stays within the gocyclo budget.
func (r *LoopReconciler) ensureDeliver(ctx context.Context, loop *coxv1alpha1.Loop) (bool, error) {
	requeue := false
	if ok, err := r.ensureDeliverJob(ctx, loop); err != nil {
		return false, err
	} else if ok {
		requeue = true
	}
	if err := r.ensureDeliverNetPolicies(ctx, loop); err != nil {
		return false, err
	}
	if err := r.ensureDeliverReadback(ctx, loop); err != nil {
		return false, err
	}
	return requeue, nil
}

// deliverNetpolName is the deliver Job pod's NetworkPolicy name: <loop>-
// deliver-np (D20: the short suffix keeps the name within the 63-char
// DNS-1035 budget for a 55-char (the max valid) Loop name).
func deliverNetpolName(loopName string) string { return derivedName(loopName, "-deliver-np") }

// deliverBranchName is the delivery branch: <prefix><loop-name>.
func deliverBranchName(prefix, loopName string) string { return prefix + loopName }

// deliverBaseBranch resolves spec.delivery.baseBranch (default
// spec.workspace.ref; default "main" when the workspace ref is empty).
func deliverBaseBranch(loop *coxv1alpha1.Loop) string {
	if loop.Spec.Delivery != nil && loop.Spec.Delivery.BaseBranch != "" {
		return loop.Spec.Delivery.BaseBranch
	}
	if loop.Spec.Workspace.Ref != "" {
		return loop.Spec.Workspace.Ref
	}
	return "main"
}

// deliverBranchPrefix resolves spec.delivery.branchPrefix (default
// "coxswain/").
func deliverBranchPrefix(loop *coxv1alpha1.Loop) string {
	if loop.Spec.Delivery != nil && loop.Spec.Delivery.BranchPrefix != "" {
		return loop.Spec.Delivery.BranchPrefix
	}
	return "coxswain/"
}

// deliverDraft resolves spec.delivery.draft (default true).
func deliverDraft(loop *coxv1alpha1.Loop) bool {
	if loop.Spec.Delivery != nil && loop.Spec.Delivery.Draft != nil {
		return *loop.Spec.Delivery.Draft
	}
	return true
}

// deliveryRequested reports whether this Loop wants delivery and has not
// recorded it yet: phase Succeeded (terminal; no re-tasking) + mode
// PullRequest + a pinned verifiedCommit (delivery is bound to the commit
// the operator verified) + no recorded delivery FOR THIS COMMIT (one Job
// per verifiedCommit; a recorded outcome for another commit is stale
// evidence, not idempotency).
//
// Idempotency is commit-scoped (status.delivery.commit == the pinned
// verifiedCommit -> skip), not "status.delivery != nil -> skip": a Loop
// that re-verifies after a recorded delivery pins a NEW verifiedCommit and
// MUST re-deliver (the stale guard deletes the old Job); the old recorded
// outcome must not suppress the new one. The operator clears
// status.delivery + the condition to re-run delivery for the SAME commit.
func deliveryRequested(loop *coxv1alpha1.Loop) (bool, string) {
	if loop.Status.Phase != coxv1alpha1.LoopPhaseSucceeded {
		return false, "phase is not Succeeded"
	}
	if loop.Spec.Delivery == nil || loop.Spec.Delivery.Mode != coxv1alpha1.DeliveryModePullRequest {
		return false, "delivery mode is not PullRequest"
	}
	if loop.Status.CurrentVerify == nil || loop.Status.CurrentVerify.VerifiedCommit == "" {
		return false, "no verifiedCommit pinned (status.currentVerify)"
	}
	if loop.Spec.Workspace.Repo == "" {
		return false, "spec.workspace.repo is empty"
	}
	if loop.Status.Delivery != nil && loop.Status.Delivery.Commit == loop.Status.CurrentVerify.VerifiedCommit {
		return false, "delivery already recorded for this verifiedCommit (status.delivery.commit)"
	}
	return true, ""
}

// deliveryExpected is deliveryRequested minus the "already recorded" check
// (the deliver Job's NetworkPolicy is expected while the Job exists — phase
// Succeeded + mode PullRequest + a pinned commit, whether or not
// status.delivery is set; it is cleaned up only when the Loop is no longer
// Succeeded or the mode flips off).
func deliveryExpected(loop *coxv1alpha1.Loop) bool {
	if loop.Status.Phase != coxv1alpha1.LoopPhaseSucceeded {
		return false
	}
	if loop.Spec.Delivery == nil || loop.Spec.Delivery.Mode != coxv1alpha1.DeliveryModePullRequest {
		return false
	}
	return loop.Status.CurrentVerify != nil && loop.Status.CurrentVerify.VerifiedCommit != "" &&
		loop.Spec.Workspace.Repo != ""
}

// deliverOutcome is the validated delivery outcome (the operator's record
// for status.delivery + the Delivered condition).
type deliverOutcome struct {
	Branch   string
	Commit   string
	PRNumber int64
	PRURL    string
}

// ensureDeliverJob builds (idempotently) the deliver Job for a Loop that
// wants delivery, and maps its outcome: a just-created or in-progress Job
// requeues; a completed Job requeues (the termination message is read +
// validated on the next pass); a failed Job is terminal (Delivered=False
// reason DeliveryFailed — no retry). A Job stamped for a DIFFERENT
// verifiedCommit is deleted (Background propagation) and the reconcile
// requeues (D27 stale guard, like the verify Job's errVerifyStaleDeleted).
func (r *LoopReconciler) ensureDeliverJob(ctx context.Context, loop *coxv1alpha1.Loop) (bool, error) {
	if ok, _ := deliveryRequested(loop); !ok {
		return false, nil
	}
	jobName := deliverJobName(loop.Name)
	verified := loop.Status.CurrentVerify.VerifiedCommit

	existing := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: loop.Namespace}, existing)
	switch {
	case apierrors.IsNotFound(err):
		job := r.buildDeliverJob(loop)
		if err := controllerutil.SetControllerReference(loop, job, r.Scheme); err != nil {
			return false, fmt.Errorf("set owner ref on deliver Job %s: %w", jobName, err)
		}
		if err := r.Create(ctx, job); err != nil {
			return false, fmt.Errorf("create deliver Job %s: %w", jobName, err)
		}
		logf.FromContext(ctx).Info("created deliver Job", "job", jobName, "loop", loop.Name,
			"verifiedCommit", verified, "branch", deliverBranchName(deliverBranchPrefix(loop), loop.Name))
		setCondition(loop, coxv1alpha1.DeliveredCondition, metav1.ConditionFalse,
			coxv1alpha1.ReasonDeliveryInProgress, "deliver Job in progress")
		return true, nil
	case err != nil:
		return false, fmt.Errorf("get deliver Job %s: %w", jobName, err)
	default:
		// D27 stale guard: a Job stamped for a different verifiedCommit is
		// stale evidence — delete it (the annotation pins what it was built
		// for) and requeue (the name is still taken until the async delete
		// lands; the next pass recreates).
		if existing.Annotations[verifyCommitAnnotation] != verified {
			prop := metav1.DeletePropagationBackground
			if err := r.Delete(ctx, existing, &client.DeleteOptions{PropagationPolicy: &prop}); err != nil && !apierrors.IsNotFound(err) {
				return false, fmt.Errorf("delete stale deliver Job %s: %w", jobName, err)
			}
			logf.FromContext(ctx).Info("deleted stale deliver Job (verifiedCommit mismatch)",
				"job", jobName, "stamped", existing.Annotations[verifyCommitAnnotation], "current", verified)
			return true, nil
		}
		return r.deliverJobOutcome(loop, existing)
	}
}

// deliverJobOutcome maps a same-commit deliver Job to (requeue, error):
// in progress -> Delivered=False InProgress + requeue; succeeded -> requeue
// (the termination message is read + validated on the next pass); failed ->
// Delivered=False reason DeliveryFailed + requeue (terminal: the Job is NOT
// retried — backoffLimit 0, one Job per verifiedCommit; the operator
// re-runs delivery by clearing status.delivery + the condition).
func (r *LoopReconciler) deliverJobOutcome(loop *coxv1alpha1.Loop, job *batchv1.Job) (bool, error) {
	switch {
	case job.Status.Succeeded > 0:
		setCondition(loop, coxv1alpha1.DeliveredCondition, metav1.ConditionFalse,
			coxv1alpha1.ReasonDeliveryInProgress, "deliver Job succeeded; reading the push container's termination message")
		return true, nil
	case job.Status.Failed > 0:
		setCondition(loop, coxv1alpha1.DeliveredCondition, metav1.ConditionFalse,
			coxv1alpha1.ReasonDeliveryFailed,
			fmt.Sprintf("deliver Job %s failed (no retry: one Job per verifiedCommit; clear status.delivery + the condition to re-run delivery)",
				deliverJobName(loop.Name)))
		return true, nil
	default:
		setCondition(loop, coxv1alpha1.DeliveredCondition, metav1.ConditionFalse,
			coxv1alpha1.ReasonDeliveryInProgress, "deliver Job in progress")
		return true, nil
	}
}

// buildDeliverJob builds the deliver Job (owned by the Loop, annotated with
// the verifiedCommit it was built for — D27 stale guard).
func (r *LoopReconciler) buildDeliverJob(loop *coxv1alpha1.Loop) *batchv1.Job {
	verified := loop.Status.CurrentVerify.VerifiedCommit
	jobName := deliverJobName(loop.Name)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: loop.Namespace,
			Labels:    deliverJobLabels(loop.Name),
			Annotations: map[string]string{
				verifyCommitAnnotation: verified,
			},
		},
		Spec: r.buildDeliverJobSpec(loop),
	}
}

// deliverJobLabels is the deliver Job's label set (the deliver pod
// NetworkPolicy selects on this; the Job stamps it on the pod template).
func deliverJobLabels(loopName string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by": partOfCoxswain,
		"app.kubernetes.io/component":  deliverComponent,
		deliverForLabel:                loopName,
	}
}

// buildDeliverJobSpec builds the deliver Job's spec: the init containers in
// order (clone-base, import-agent) + the push container (the MAIN container
// — its termination message rides in containerStatuses[push], read by the
// operator via the APIReader). backoffLimit 0 (one Job per verifiedCommit;
// a failure is not retried), RestartPolicy Never.
func (r *LoopReconciler) buildDeliverJobSpec(loop *coxv1alpha1.Loop) batchv1.JobSpec {
	verified := loop.Status.CurrentVerify.VerifiedCommit
	branch := deliverBranchName(deliverBranchPrefix(loop), loop.Name)
	base := deliverBaseBranch(loop)

	inits := []corev1.Container{
		r.deliverCloneBaseContainer(loop),
		r.deliverImportAgentContainer(loop),
	}
	main := r.deliverPushContainer(loop, verified, branch, base)

	volumes := []corev1.Volume{
		{Name: deliverScratchVol, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		// The agent's workspace PVC read-only (import-agent fetches the
		// pinned commit from it). Mounted into import-agent ONLY (the push
		// container never mounts the agent PVC).
		{Name: deliverAgentVol, VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
			ClaimName: workspacePVCName(loop.Name), ReadOnly: true,
		}}},
	}
	if loop.Spec.Workspace.GitCredentialSecret != "" {
		volumes = append(volumes, corev1.Volume{
			Name: workspaceCredsVolume,
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: loop.Spec.Workspace.GitCredentialSecret,
				Items:      deliverCredItems(),
			}},
		})
	}

	backoff := int32(0)
	never := corev1.RestartPolicyNever
	return batchv1.JobSpec{
		BackoffLimit: &backoff,
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: deliverJobLabels(loop.Name)},
			Spec: corev1.PodSpec{
				RestartPolicy:      never,
				ServiceAccountName: "",
				Volumes:            volumes,
				InitContainers:     inits,
				Containers:         []corev1.Container{main},
			},
		},
	}
}

// deliverCredItems restricts the git-cred Secret mount to the two keys
// (username, password) — the same shape as the workspace creds mount.
func deliverCredItems() []corev1.KeyToPath {
	return []corev1.KeyToPath{
		{Key: workspaceCredsUsernameKey, Path: workspaceCredsUsernameKey},
		{Key: workspaceCredsPasswordKey, Path: workspaceCredsPasswordKey},
	}
}

// deliverCredLines returns the git containers' basic-auth header lines (the
// workspace init container's pattern: the header is built from the mounted
// files and passed via a per-command -c flag — nothing is persisted; the
// inline 'credential.helper' forms are not usable: the 'store' helper writes
// a lock a read-only mount refuses, and a '!sh -c' helper is not parsed by
// busybox ash).
func deliverCredLines(creds bool) (string, string) {
	if !creds {
		return "", ""
	}
	authLine := "AUTH=$(printf '%s:%s' \"$(cat /workspace-creds/" + workspaceCredsUsernameKey + ")\" \"$(cat /workspace-creds/" + workspaceCredsPasswordKey + ")\" | base64 -w 0)\n"
	fetchCred := gitBasicAuthHeader
	return authLine, fetchCred
}

// deliverSafeDir is the per-command safe.directory flag for the scratch
// path (the volume is owned by an arbitrary uid while the container runs as
// 65532 — without it git refuses the repo as 'detected dubious ownership').
func deliverSafeDir() string { return "-c safe.directory=" + deliverScratchPath }

// deliverSafeDirs is the per-command safe.directory flags for BOTH the
// scratch path and the agent-src .git (import-agent needs both).
func deliverSafeDirs() string {
	return "-c safe.directory=" + deliverScratchPath + " -c safe.directory=" + deliverAgentSrc + "/.git"
}

// deliverCloneBaseContainer fetches the BASE ref's commit from the remote
// into the scratch volume (the PR base's evidence + the remote mirror the
// push pushes from). Carries the git credential (basic auth).
func (r *LoopReconciler) deliverCloneBaseContainer(loop *coxv1alpha1.Loop) corev1.Container {
	base := deliverBaseBranch(loop)
	repo := loop.Spec.Workspace.Repo
	creds := loop.Spec.Workspace.GitCredentialSecret != ""
	mounts := []corev1.VolumeMount{
		{Name: deliverScratchVol, MountPath: deliverScratchPath},
	}
	if creds {
		mounts = append(mounts, corev1.VolumeMount{Name: workspaceCredsVolume, MountPath: "/workspace-creds", ReadOnly: true})
	}
	authLine, fetchCred := deliverCredLines(creds)
	env := r.deliverProxyEnv(loop)
	script := `#!/bin/sh
set -eu
export GIT_TERMINAL_PROMPT=0
DEST=` + deliverScratchPath + `
REPO=` + shellQuote(repo) + `
BASE=` + shellQuote(base) + `
# The volume is a MOUNT POINT (cannot be rm -rf'd); wipe its contents.
if [ -d "${DEST}/.git" ]; then rm -rf "${DEST}/.git"; fi
mkdir -p "${DEST}"
git ` + deliverSafeDir() + ` -c core.hooksPath=/dev/null init "${DEST}"
git -C "${DEST}" ` + deliverSafeDir() + ` -c core.hooksPath=/dev/null remote add origin "${REPO}"
` + authLine + `  git ` + deliverSafeDir() + ` -c core.hooksPath=/dev/null -C "${DEST}"` + fetchCred + ` fetch origin "${BASE}"
git -C "${DEST}" ` + deliverSafeDir() + ` -c core.hooksPath=/dev/null checkout --detach FETCH_HEAD
echo "deliver clone-base: base ${BASE} at $(git -C "${DEST}" ` + deliverSafeDir() + ` -c core.hooksPath=/dev/null rev-parse HEAD)"
`
	return corev1.Container{
		Name:         deliverCloneBase,
		Image:        r.workspaceGitImage(),
		Env:          env,
		Command:      []string{verifySh, "-c", script},
		VolumeMounts: mounts,
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                &[]int64{deliverNonRootUID}[0],
			RunAsGroup:               &[]int64{deliverNonRootUID}[0],
			RunAsNonRoot:             &deliverTrue,
			AllowPrivilegeEscalation: &deliverFalse,
			ReadOnlyRootFilesystem:   &deliverTrue,
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{allCaps}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
	}
}

// deliverImportAgentContainer fetches the pinned verifiedCommit from the
// agent's workspace PVC (read-only) into the scratch volume's EXISTING base
// clone (the clone-base init left /deliver checked out at the base). NO
// credential, NO network (file:// fetch), hooks disabled (core.hooksPath=
// /dev/null — the agent's repo could carry hooks the operator must never
// run), and the resolved commit is ASSERTED == the pinned verifiedCommit (a
// mismatch fails the Job — D27 evidence integrity: the deliver Job pushes
// ONLY the commit the operator verified). The fetch also imports the agent
// commit's parent chain (base..pinned), so the push container's PR has the
// base's objects. (The S6 first-pass script wiped + re-inited the scratch
// .git here, leaving clone-base's checked-out files as untracked — the
// pinned checkout aborted on them; kind run gocli-task1, 2026-10-03.)
func (r *LoopReconciler) deliverImportAgentContainer(loop *coxv1alpha1.Loop) corev1.Container {
	verified := loop.Status.CurrentVerify.VerifiedCommit
	script := `#!/bin/sh
set -eu
export GIT_TERMINAL_PROMPT=0
SRC=` + deliverAgentSrc + `
DEST=` + deliverScratchPath + `
PINNED=` + shellQuote(verified) + `
# The base clone (clone-base's work) must exist: import into it, never wipe
# or re-init (wiping .git leaves the checked-out base files untracked and
# the pinned checkout aborts on them).
if [ ! -d "${DEST}/.git" ]; then
  echo "deliver import-agent: ${DEST} is not a git repo (clone-base must run first); refusing"
  exit 1
fi
# Fetch the pinned commit from the agent's .git over the file protocol INTO
# the existing base clone. protocol.file.allow=always is REQUIRED (modern
# git refuses file:// by default); core.hooksPath=/dev/null disables any
# hook in the imported repo; GIT_TERMINAL_PROMPT=0 is belt-and-suspenders
# (no credential, no network — a prompt would hang).
git -c protocol.file.allow=always -c core.hooksPath=/dev/null ` + deliverSafeDirs() + ` -C "${DEST}" fetch "file://${SRC}/.git" "${PINNED}"
# Assert the fetched commit == the pinned commit (a mismatch means the
# evidence is stale or the agent's .git moved — fail the Job, never push).
GOT=$(git -c core.hooksPath=/dev/null ` + deliverSafeDirs() + ` -C "${DEST}" rev-parse "${PINNED}")
if [ "$GOT" != "${PINNED}" ]; then
  echo "deliver import-agent: pinned ${PINNED} != fetched ${GOT}; refusing"
  exit 1
fi
git -c core.hooksPath=/dev/null ` + deliverSafeDirs() + ` -C "${DEST}" checkout -q --detach "${PINNED}"
echo "deliver import-agent: imported ${PINNED} from the agent workspace PVC"
`
	return corev1.Container{
		Name:    deliverImport,
		Image:   r.workspaceGitImage(),
		Command: []string{verifySh, "-c", script},
		VolumeMounts: []corev1.VolumeMount{
			{Name: deliverScratchVol, MountPath: deliverScratchPath},
			// The agent workspace PVC read-only (the exact bytes the verify
			// Job trusted into status.currentVerify).
			{Name: deliverAgentVol, MountPath: deliverAgentSrc, ReadOnly: true},
		},
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                &[]int64{deliverNonRootUID}[0],
			RunAsGroup:               &[]int64{deliverNonRootUID}[0],
			RunAsNonRoot:             &deliverTrue,
			AllowPrivilegeEscalation: &deliverFalse,
			ReadOnlyRootFilesystem:   &deliverTrue,
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{allCaps}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
	}
}

// deliverPushAPIAuthExpr is the API auth setup line(s) for the push script's
// PR-call section (emitted AFTER the git push — the only place $AUTH may be
// referenced is after its assignment; see the script's comment). GitHub:
// the Secret's PASSWORD as a Bearer token (read directly — no $AUTH); Gitea:
// the basic pair as a Basic header ($AUTH, assigned before the push).
func deliverPushAPIAuthExpr(creds bool, prov deliverProvider) string {
	switch {
	case creds && prov == deliverProviderGitHub:
		return "API_AUTH=\"Authorization: Bearer $(cat /workspace-creds/" + workspaceCredsPasswordKey + ")\"\n"
	case creds:
		return "API_AUTH=\"Authorization: Basic $AUTH\"\n"
	default:
		return "API_AUTH=\"\"\n"
	}
}

// deliverPushContainer pushes the imported commit to the delivery branch
// (NEVER --force; refusing a push whose target ref equals baseBranch or a
// default branch — main/master, or the repo's default branch) and creates
// the pull request via the provider API (idempotent: an open PR for the
// branch is reused). Asserts the scratch clone's HEAD == the pinned
// verifiedCommit and pushes the pinned SHA explicitly (never "HEAD" — a
// regression that leaves HEAD elsewhere is refused, not silently
// delivered). Writes
// {branch, commit, prNumber, prURL} to /dev/termination-log (prURL is the
// provider's own html_url). Carries the
// credential; does NOT mount the agent PVC.
//
// The API auth: for a GitHub delivery the Secret's PASSWORD is the Bearer
// token (the Secret is basic auth with username "x-access-token" and
// password = the token); for a Gitea-compatible delivery the same basic
// pair is sent as a Basic header.
func (r *LoopReconciler) deliverPushContainer(loop *coxv1alpha1.Loop, verified, branch, base string) corev1.Container {
	repo := loop.Spec.Workspace.Repo
	creds := loop.Spec.Workspace.GitCredentialSecret != ""
	prov, _ := deliverProviderForRepo(repo)
	apiBase := deliverAPIBase(repo, prov)
	draft := deliverDraft(loop)
	draftTitlePrefix := draftTitlePrefixFor(draft, prov)

	mounts := []corev1.VolumeMount{
		{Name: deliverScratchVol, MountPath: deliverScratchPath},
	}
	if creds {
		mounts = append(mounts, corev1.VolumeMount{Name: workspaceCredsVolume, MountPath: "/workspace-creds", ReadOnly: true})
	}
	gitCredFlag := ""
	if creds {
		gitCredFlag = gitBasicAuthHeader
	}
	apiAuthExpr := deliverPushAPIAuthExpr(creds, prov)
	env := r.deliverProxyEnv(loop)

	script := `#!/bin/sh
set -eu
export GIT_TERMINAL_PROMPT=0
SRC=` + deliverScratchPath + `
REPO=` + shellQuote(repo) + `
BRANCH=` + shellQuote(branch) + `
BASE=` + shellQuote(base) + `
PINNED=` + shellQuote(verified) + `
API_BASE=` + shellQuote(apiBase) + `
DRAFT=` + strconv.FormatBool(draft) + `
TITLE_PREFIX=` + shellQuote(draftTitlePrefix) + `
PATH_PART=${REPO#*://}
REMAIN=${PATH_PART#*/}
OWNER=${REMAIN%%/*}
REPO_NAME=${REMAIN#*/}
case "${REPO_NAME}" in
  *.git) REPO_NAME=${REPO_NAME%.git} ;;
esac
# --- the credential (basic pair as base64 — the git push and the provider
# API both use it: the API auth is set up HERE, before any reference to it;
# the s6b/s6c kind runs proved a reference above the assignment dies under
# set -u with 'AUTH: parameter not set').
` + (func() string {
		if creds {
			return "AUTH=$(printf '%s:%s' \"$(cat /workspace-creds/" + workspaceCredsUsernameKey + ")\" \"$(cat /workspace-creds/" + workspaceCredsPasswordKey + ")\" | base64 -w 0)\n"
		}
		return ""
	})() + apiAuthExpr + `
# --- refusal: the delivery branch must NOT equal the base branch or a
# default branch (main/master, or the repo's ACTUAL default branch — read
# from GET /repos/{owner}/{repo}, the same API + auth as the PR calls).
# Pushing onto a default branch would deliver the agent's code straight to
# the operator's mainline — the PR gate is the point of delivery. The
# main/master check is belt-and-suspenders (works without the API).
if [ "${BRANCH}" = "${BASE}" ] || [ "${BRANCH}" = "main" ] || [ "${BRANCH}" = "master" ]; then
  echo "deliver push: refusing to push branch ${BRANCH} (equals the base branch or a default branch)"
  exit 1
fi
DEFAULT_BRANCH=$(curl -sfS -H "$API_AUTH" "${API_BASE}/repos/${OWNER}/${REPO_NAME}" | sed -n 's/.*"default_branch"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p') || DEFAULT_BRANCH=""
if [ -n "${DEFAULT_BRANCH}" ] && [ "${BRANCH}" = "${DEFAULT_BRANCH}" ]; then
  echo "deliver push: refusing to push branch ${BRANCH} (equals the repo's default branch ${DEFAULT_BRANCH})"
  exit 1
fi
# --- the push pushes the PINNED verifiedCommit, not whatever HEAD happens
# to be (a regression that leaves HEAD elsewhere must not deliver another
# commit): assert the scratch clone's HEAD == the pinned commit, then push
# the pinned SHA explicitly (core.hooksPath=/dev/null — the operator's own
# import, never the agent's repo's hooks).
HEAD_SHA=$(git -C "${SRC}" ` + deliverSafeDir() + ` -c core.hooksPath=/dev/null rev-parse HEAD)
if [ "${HEAD_SHA}" != "${PINNED}" ]; then
  echo "deliver push: refusing to push (HEAD ${HEAD_SHA} != the pinned verifiedCommit ${PINNED})"
  exit 1
fi
git -C "${SRC}" ` + deliverSafeDir() + ` -c core.hooksPath=/dev/null` + gitCredFlag + ` push origin "${PINNED}:refs/heads/${BRANCH}"
# --- create the PR (idempotent: reuse an open PR for the branch).
# Look for an existing open PR for the branch (reuse it — idempotent).
# The lookup and the create both return the PR's html_url: the operator's
# trust boundary is the provider's OWN URL (never one the script assembles).
EXISTING=$(curl -sfS -H "$API_AUTH" "${API_BASE}/repos/${OWNER}/${REPO_NAME}/pulls?state=open&head=${OWNER}:${BRANCH}") || EXISTING=""
PR_NUM=$(printf '%s' "${EXISTING}" | tr -d '\n' | sed -n 's/.*"number"[[:space:]]*:[[:space:]]*\([0-9]*\).*/\1/p' | head -1)
PR_URL=$(printf '%s' "${EXISTING}" | tr -d '\n' | sed -n 's/.*"html_url"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)
if [ -n "${PR_NUM}" ]; then
  # An open PR for the branch: reuse it (idempotent).
  [ -n "${PR_URL}" ] || { echo "deliver push: existing PR ${PR_NUM} has no html_url"; exit 1; }
else
  PAYLOAD=$(printf '{"title":"%scoxswain: %s","head":"%s","base":"%s","body":"Delivered by coxswain from verified commit %s.","draft":%s}' \
    "${TITLE_PREFIX}" "${BRANCH}" "${BRANCH}" "${BASE}" "${PINNED}" "${DRAFT}")
  # Gitea's PR create can reject a non-existent base branch; the push above
  # already pushed the delivery branch, and the base branch exists on the
  # remote (clone-base fetched it) — a failed create is a hard failure.
  CREATED=$(curl -sfS -X POST -H "$API_AUTH" -H "Content-Type: application/json" -d "${PAYLOAD}" "${API_BASE}/repos/${OWNER}/${REPO_NAME}/pulls") || { echo "deliver push: PR create failed (see API response)"; exit 1; }
  PR_NUM=$(printf '%s' "${CREATED}" | sed -n 's/.*"number"[[:space:]]*:[[:space:]]*\([0-9]*\).*/\1/p')
  PR_URL=$(printf '%s' "${CREATED}" | sed -n 's/.*"html_url"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
fi
[ -n "${PR_NUM}" ] || { echo "deliver push: no PR number returned (API unreachable or refused)"; exit 1; }
[ -n "${PR_URL}" ] || { echo "deliver push: no PR html_url returned"; exit 1; }
# Write the result to the termination log (the operator reads it via the
# APIReader — kubelet-recorded, not a claim). prURL is the PR PAGE the
# provider returned (html_url, not the API base): the operator validates it
# strictly (allowed host + the exact per-provider PR path for THIS repo).
{
  echo "branch=${BRANCH}"
  echo "commit=${PINNED}"
  echo "prNumber=${PR_NUM}"
  echo "prURL=${PR_URL}"
} > /dev/termination-log
`
	return corev1.Container{
		Name:         deliverPush,
		Image:        r.deliverPushImage(),
		Env:          env,
		Command:      []string{verifySh, "-c", script},
		VolumeMounts: mounts,
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                &[]int64{deliverNonRootUID}[0],
			RunAsGroup:               &[]int64{deliverNonRootUID}[0],
			RunAsNonRoot:             &deliverTrue,
			AllowPrivilegeEscalation: &deliverFalse,
			ReadOnlyRootFilesystem:   &deliverTrue,
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{allCaps}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
	}
}

// deliverAPIBase is the provider API base: for GitHub, the GitHub REST API
// (https://api.github.com — the PR create is POST
// /repos/<owner>/<repo>/pulls, the repo is GET /repos/<owner>/<repo>);
// for a Gitea-compatible provider, the repo host's Gitea API
// (<scheme>://<host>/api/v1 — the Gitea PR REST is the same shape,
// /repos/<owner>/<repo>/pulls; the repo is GET /repos/<owner>/<repo>).
func deliverAPIBase(repo string, prov deliverProvider) string {
	if prov == deliverProviderGitHub {
		return "https://api.github.com"
	}
	u, err := url.Parse(repo)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host + "/api/v1"
}

// deliverPRPathSegment is the PR page path segment per provider: GitHub
// pages PRs at /<owner>/<repo>/pull/<n> (singular) and Gitea at
// /<owner>/<repo>/pulls/<n> (plural).
func deliverPRPathSegment(prov deliverProvider) string {
	if prov == deliverProviderGitHub {
		return "pull"
	}
	return "pulls"
}

// deliverNonRootUID / deliverTrue / deliverFalse are the hardened profile
// values (the verify Job's trustedContainerSecurityContext profile).
const deliverNonRootUID = 65532

var (
	deliverTrue  = true
	deliverFalse = false
)

// parseDeliverTermination validates the push container's termination
// message STRICTLY (the operator's trust boundary for the delivery
// evidence). The message is the push container's stdout written to
// /dev/termination-log (the four lines: branch=, commit=, prNumber=,
// prURL=). It is kubelet-recorded (not a claim), but the operator asserts:
//
//   - the message is sized (<= deliverTermMsgMaxBytes).
//   - branch == the delivery branch (<prefix><loop>).
//   - commit == the pinned verifiedCommit (a termination message that names
//     any other commit is rejected: the deliver Job pushes ONLY the
//     verified commit).
//   - prNumber is a positive integer.
//   - prURL is a well-formed URL whose host is allowed (the repo host, or
//     github.com for a GitHub delivery) and whose path is EXACTLY the
//     provider's PR page path for this repo: /<owner>/<repo>/pulls/<n>
//     (Gitea) or /<owner>/<repo>/pull/<n> (GitHub), with owner/repo derived
//     from spec.workspace.repo. The PR page comes from the provider's own
//     html_url (never assembled by the push script): a URL that names any
//     other repo, or the wrong path segment, is rejected.
//
// A malformed or foreign message is rejected (ok=false, no error): the
// operator re-reads (the pod may be in the middle of writing) and requeues.
func parseDeliverTermination(msg string, loop *coxv1alpha1.Loop) (deliverOutcome, bool) {
	if len(msg) > deliverTermMsgMaxBytes {
		return deliverOutcome{}, false
	}
	verified := loop.Status.CurrentVerify.VerifiedCommit
	wantBranch := deliverBranchName(deliverBranchPrefix(loop), loop.Name)
	prov, repoHost := deliverProviderForRepo(loop.Spec.Workspace.Repo)

	fields := map[string]string{}
	for line := range strings.SplitSeq(msg, "\n") {
		line = strings.TrimSpace(line)
		if i := strings.IndexByte(line, '='); i > 0 {
			fields[strings.TrimSpace(line[:i])] = strings.TrimSpace(line[i+1:])
		}
	}
	branch := fields["branch"]
	commit := fields["commit"]
	prNumStr := fields["prNumber"]
	prURL := fields["prURL"]
	if branch == "" || commit == "" || prNumStr == "" || prURL == "" {
		return deliverOutcome{}, false
	}
	// commit MUST be the pinned verifiedCommit (D27 evidence integrity).
	if commit != verified {
		return deliverOutcome{}, false
	}
	// branch MUST be the delivery branch.
	if branch != wantBranch {
		return deliverOutcome{}, false
	}
	// prNumber MUST be a positive integer.
	prNum, err := strconv.ParseInt(prNumStr, 10, 64)
	if err != nil || prNum <= 0 {
		return deliverOutcome{}, false
	}
	// prURL MUST be well-formed, sized, on an allowed host, and its path
	// MUST be exactly the provider's PR page path for THIS repo.
	if len(prURL) > deliverPRURLMaxBytes {
		return deliverOutcome{}, false
	}
	u, err := url.Parse(prURL)
	if err != nil || u.Host == "" {
		return deliverOutcome{}, false
	}
	host := strings.ToLower(u.Hostname())
	if prov == deliverProviderGitHub {
		if host != githubHost {
			return deliverOutcome{}, false
		}
	} else if host != repoHost {
		return deliverOutcome{}, false
	}
	// The path is EXACTLY /<owner>/<repo>/pull[s]/<prNumber> for this repo
	// (owner/repo from spec.workspace.repo; the segment per provider —
	// GitHub /pull/<n>, Gitea /pulls/<n>): a URL that names any other repo
	// or the wrong segment is rejected.
	owner, repoName := deliverRepoOwnerName(loop.Spec.Workspace.Repo)
	if owner == "" || repoName == "" {
		return deliverOutcome{}, false
	}
	wantPath := "/" + owner + "/" + repoName + "/" + deliverPRPathSegment(prov) + "/" + prNumStr
	if u.Path != wantPath {
		return deliverOutcome{}, false
	}
	return deliverOutcome{Branch: branch, Commit: commit, PRNumber: prNum, PRURL: prURL}, true
}

// deliverJobPodSelector is the label selector the deliver pod NetworkPolicy
// matches (the deliver Job's labels — the Job stamps them on the pod
// template).
func deliverJobPodSelector(loopName string) metav1.LabelSelector {
	return metav1.LabelSelector{MatchLabels: deliverJobLabels(loopName)}
}

// ensureDeliverNetPolicies creates the deliver Job pod's NetworkPolicy
// (DNS + the egress proxy for an external repo host, or a direct
// repo-peer rule for an in-cluster repo), or cleans it up when delivery is
// no longer expected. Mirrors ensureVerifyNetworkPolicy (I42c: the spec is
// set in the mutate func so input changes propagate).
func (r *LoopReconciler) ensureDeliverNetPolicies(ctx context.Context, loop *coxv1alpha1.Loop) error {
	if !deliveryExpected(loop) {
		return r.cleanupDeliverNetpol(ctx, loop)
	}
	name := deliverNetpolName(loop.Name)
	egress := []networkingv1.NetworkPolicyEgressRule{
		{To: []networkingv1.NetworkPolicyPeer{dnsPeer()}, Ports: dnsPorts()},
	}
	if peer := repoPeer(loop.Spec.Workspace.Repo, r.serviceNamespaceFromHost); peer != nil {
		// An in-cluster repo: the direct repo-peer rule (no proxy hop).
		port := intstrPtr32(int32(workspaceRepoPort(loop.Spec.Workspace.Repo)))
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{
			To:    []networkingv1.NetworkPolicyPeer{*peer},
			Ports: []networkingv1.NetworkPolicyPort{{Port: port, Protocol: new(corev1.ProtocolTCP)}},
		})
	} else {
		// An external repo host: the egress proxy (I42) — the push + the
		// provider API traverse it (NetworkPolicy cannot name hostnames; the
		// proxy's SNI allowlist carries the repo host and, for GitHub,
		// api.github.com). The proxy pod carries egressProxyLabels.
		peer := networkingv1.NetworkPolicyPeer{
			PodSelector: &metav1.LabelSelector{MatchLabels: egressProxyLabels(loop.Name)},
		}
		port := intstrPtr32(egressProxyPort)
		protocol := corev1.ProtocolTCP
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{
			To:    []networkingv1.NetworkPolicyPeer{peer},
			Ports: []networkingv1.NetworkPolicyPort{{Port: port, Protocol: &protocol}},
		})
	}
	npol := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: loop.Namespace,
			Labels:    map[string]string{deliverForLabel: loop.Name},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: deliverJobPodSelector(loop.Name),
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress:     []networkingv1.NetworkPolicyIngressRule{},
			Egress:      egress,
		},
	}
	if _, err := r.createOrUpdateNP(ctx, loop, npol); err != nil {
		return fmt.Errorf("create deliver netpol %s: %w", name, err)
	}
	return nil
}

// cleanupDeliverNetpol deletes the deliver netpol when delivery is no longer
// expected (I42c: a foreign netpol of the same name is left alone).
func (r *LoopReconciler) cleanupDeliverNetpol(ctx context.Context, loop *coxv1alpha1.Loop) error {
	np := &networkingv1.NetworkPolicy{}
	err := r.Get(ctx, client.ObjectKey{Namespace: loop.Namespace, Name: deliverNetpolName(loop.Name)}, np)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get deliver NetworkPolicy %s/%s: %w", loop.Namespace, loop.Name, err)
	}
	if !metav1.IsControlledBy(np, loop) {
		return nil // foreign: leave alone
	}
	if err := r.Delete(ctx, np); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete deliver NetworkPolicy %s/%s: %w", loop.Namespace, loop.Name, err)
	}
	return nil
}

// deliverEgressProxyHosts returns the repo host (+ api.github.com for a
// GitHub delivery) for the egress proxy's SNI allowlist (I42): the deliver
// Job's git push + the provider API call traverse the proxy for a repo with
// no expressible NetworkPolicy repo rule (deliverNeedsProxyHosts: repoPeer
// nil), and the proxy must allowlist BOTH hosts. A nil (no delivery) or an
// in-cluster repo returns no hosts (the direct repo-peer rule covers it, no
// proxy hop).
// deliverProxyHost is the egress-proxy SNI allowlist entry (the I42
// host:port allow form, like the AgentPolicy network allows) for a single
// host:443 (the deliver Job's git push + the provider API both dial 443).
func deliverProxyHost(host string) string {
	return host + ":443"
}

func (r *LoopReconciler) deliverEgressProxyHosts(loop *coxv1alpha1.Loop) []string {
	host := r.deliverNeedsProxyHosts(loop)
	if host == "" {
		return nil
	}
	prov, _ := deliverProviderForRepo(loop.Spec.Workspace.Repo)
	hosts := []string{deliverProxyHost(host)}
	if prov == deliverProviderGitHub {
		hosts = append(hosts, deliverProxyHost("api.github.com"))
	}
	return hosts
}

// deliverNeedsProxyHosts is the repo host (no port) whose git push + provider
// API traverse the egress proxy when the deliver Job runs: "" when delivery
// is not expected, the repo is empty, or the in-cluster repoPeer rule covers
// the push (no proxy hop). The caller wraps it into the host:port allow form
// (deliverProxyHost, port 443 — the deliver Job dials 443).
func (r *LoopReconciler) deliverNeedsProxyHosts(loop *coxv1alpha1.Loop) string {
	if !deliveryExpected(loop) {
		return ""
	}
	if repo := loop.Spec.Workspace.Repo; repo != "" &&
		repoPeer(repo, r.serviceNamespaceFromHost) == nil {
		return workspaceRepoHost(repo)
	}
	return ""
}

// deliverProxyEnv returns the egress-proxy env vars the deliver Job's
// clone-base + push containers need to reach an EXTERNAL repo host: HTTPS_PROXY
// /https_proxy and HTTP_PROXY/http_proxy point at the Loop's egress-proxy
// Service (the same URL the workspace init clone uses), and NO_PROXY/no_proxy
// carry the in-cluster Service suffixes so the in-cluster repo-peer path (a
// Gitea <svc>.<ns>.svc clone/push) never traverses the proxy. An in-cluster
// repo (repoPeer non-nil) returns nil — no proxy env, the direct repo-peer
// rule covers it. This mirrors the workspace init container's I42d proxy env
// so the deliver Job's git + the provider API route identically.
func (r *LoopReconciler) deliverProxyEnv(loop *coxv1alpha1.Loop) []corev1.EnvVar {
	if r.deliverNeedsProxyHosts(loop) == "" {
		return nil // in-cluster repo or no delivery: direct path, no proxy
	}
	proxyURL := r.egressProxyServiceURL(loop.Name, loop.Namespace)
	// NO_PROXY covers the in-cluster Service suffixes: the deliver Job must
	// not send in-cluster egress (e.g. a Gitea repo peer, the agent workspace
	// PVC, or the model proxy) through the egress proxy. The egress proxy
	// Service's own host is covered too (a client must not proxy to the proxy
	// itself). The model proxy Service (egressNOProxy) is appended for
	// symmetry with the agent's env.
	noProxy := strings.Join([]string{
		EgressProxyServiceFQDN(loop.Name, loop.Namespace),
		EgressProxyServiceFQDN(loop.Name, loop.Namespace) + "." + r.clusterDomain(),
		proxyServiceName(loop.Name) + "." + loop.Namespace + ".svc",
		"localhost",
		"127.0.0.1",
	}, ",")
	return []corev1.EnvVar{
		{Name: envHTTPSProxy, Value: proxyURL},
		{Name: envHttpsProxy, Value: proxyURL},
		{Name: envHTTPProxy, Value: proxyURL},
		{Name: envHttpProxy, Value: proxyURL},
		{Name: envNoProxy, Value: noProxy},
		{Name: envNoProxyLower, Value: noProxy},
	}
}

// deliverReadbackChanged is the shared "read the deliver pod's push state via
// the APIReader" step (pod-blind, like the S3/S4 read-backs). It returns
// (changed, error): changed is true when this call set a condition or wrote
// status.delivery (the caller's shared Status().Update persists it); error is
// non-nil only for an unexpected read state (multiple deliver pods).
//
// The read is a no-op when there is no pod yet, the push container has not
// terminated, or the outcome for this commit is already recorded. A non-zero
// push (or a failed init) is Delivered=False reason DeliveryFailed (terminal —
// the Job is not retried). A malformed or foreign termination message is
// rejected (no status write; the next re-read requeues — the pod may be
// mid-write).
func (r *LoopReconciler) deliverReadbackChanged(ctx context.Context, loop *coxv1alpha1.Loop) (bool, error) {
	if !deliveryExpected(loop) {
		return false, nil
	}
	// Idempotent: a recorded delivery FOR THIS COMMIT is terminal (the read
	// is done; a new commit re-delivers via the stale guard).
	if loop.Status.Delivery != nil && loop.Status.Delivery.Commit == loop.Status.CurrentVerify.VerifiedCommit {
		return false, nil
	}
	// Find the deliver Job's pod (pod-blind: list by the deliver-for label).
	reader := r.apiReader
	if reader == nil {
		reader = r
	}
	list := &corev1.PodList{}
	if err := reader.List(ctx, list,
		client.InNamespace(loop.Namespace),
		client.MatchingLabels{deliverForLabel: loop.Name}); err != nil {
		return false, fmt.Errorf("list deliver pods for %s: %w", loop.Name, err)
	}
	if len(list.Items) == 0 {
		return false, nil // no pod yet (the Job was just created)
	}
	if len(list.Items) > 1 {
		return false, fmt.Errorf("multiple deliver pods for %s (got %d); refusing to read", loop.Name, len(list.Items))
	}
	pod := list.Items[0]
	// The push container is the MAIN container: its state rides in
	// containerStatuses (not initContainerStatuses).
	var push *corev1.ContainerStatus
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == deliverPush {
			push = &pod.Status.ContainerStatuses[i]
			break
		}
	}
	if push == nil || push.State.Terminated == nil {
		return false, nil // push has not terminated yet
	}
	terminated := push.State.Terminated
	if terminated.ExitCode != 0 {
		// A non-zero push (or a failed init, which the kubelet records on the
		// pod) is a terminal delivery failure: no retry (one Job per
		// verifiedCommit, backoffLimit 0).
		setCondition(loop, coxv1alpha1.DeliveredCondition, metav1.ConditionFalse,
			coxv1alpha1.ReasonDeliveryFailed,
			fmt.Sprintf("deliver push container exited %d: %s (no retry; clear status.delivery + the condition to re-run delivery)",
				terminated.ExitCode, strings.TrimSpace(terminated.Reason)))
		return true, nil
	}
	// Validate the termination message STRICTLY.
	outcome, ok := parseDeliverTermination(terminated.Message, loop)
	if !ok {
		// Malformed or foreign message: reject (no status write). The next
		// reconcile re-reads (the pod may be mid-write); a persistently
		// malformed message leaves the Loop InProgress (the operator can
		// inspect the pod's logs).
		logf.FromContext(ctx).Info("deliver termination message rejected (malformed or foreign)",
			"loop", loop.Name, "bytes", len(terminated.Message))
		return false, nil
	}
	// Write status.delivery + the Delivered=True condition.
	loop.Status.Delivery = &coxv1alpha1.DeliverStatus{
		Branch:   outcome.Branch,
		Commit:   outcome.Commit,
		PRNumber: outcome.PRNumber,
		PRURL:    outcome.PRURL,
	}
	setCondition(loop, coxv1alpha1.DeliveredCondition, metav1.ConditionTrue,
		coxv1alpha1.ReasonDelivered,
		fmt.Sprintf("delivered %s to branch %s; PR %d open", outcome.Commit, outcome.Branch, outcome.PRNumber))
	if r.Recorder != nil {
		r.Recorder.Eventf(loop, corev1.EventTypeNormal, "Delivered",
			"delivered %s to branch %s (PR %d)", outcome.Commit, outcome.Branch, outcome.PRNumber)
	}
	return true, nil
}

// ensureDeliverReadback reads the deliver pod's push termination message
// (deliverReadbackChanged) and persists the outcome via its OWN
// Status().Update (not the Reconcile's shared Update: a delivery outcome
// must never be skipped by an unrelated status write — a commit-scoped
// status.delivery + Delivered=True condition that is lost leaves the Loop
// InProgress with a pushed branch and an unrecorded PR).
func (r *LoopReconciler) ensureDeliverReadback(ctx context.Context, loop *coxv1alpha1.Loop) error {
	changed, err := r.deliverReadbackChanged(ctx, loop)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	if err := r.Status().Update(ctx, loop); err != nil {
		return fmt.Errorf("persist delivery outcome for %s: %w", loop.Name, err)
	}
	return nil
}
