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
// (<loop>-deliver-netpol): DNS + the egress proxy (I42) when the repo host
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
	deliverJobSuffix = "-deliver"
	deliverForLabel  = "coxswain.io/loop"

	deliverComponent = "deliver"

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

func deliverProviderForRepo(repo string) (deliverProvider, string) {
	u, err := url.Parse(repo)
	if err != nil || u.Host == "" {
		return deliverProviderGitea, ""
	}
	host := strings.ToLower(u.Hostname())
	if host == "github.com" {
		return deliverProviderGitHub, host
	}
	return deliverProviderGitea, host
}

// deliverJobName is the deliver Job's name: <loop>-deliver.
func deliverJobName(loopName string) string { return loopName + deliverJobSuffix }

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
// the operator verified) + no recorded status.delivery (one Job per
// verifiedCommit; a recorded outcome is terminal until the operator clears
// it).
func deliveryRequested(loop *coxv1alpha1.Loop) (bool, string) {
	if loop.Status.Phase != coxv1alpha1.LoopPhaseSucceeded {
		return false, "phase is not Succeeded"
	}
	if loop.Spec.Delivery == nil || loop.Spec.Delivery.Mode != coxv1alpha1.DeliveryModePullRequest {
		return false, "delivery mode is not PullRequest"
	}
	if loop.Status.Delivery != nil {
		return false, "delivery already recorded (status.delivery is set)"
	}
	if loop.Status.CurrentVerify == nil || loop.Status.CurrentVerify.VerifiedCommit == "" {
		return false, "no verifiedCommit pinned (status.currentVerify)"
	}
	if loop.Spec.Workspace.Repo == "" {
		return false, "spec.workspace.repo is empty"
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
		"app.kubernetes.io/managed-by": "coxswain",
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
	fetchCred := ` -c http.extraHeader="Authorization: Basic $AUTH"`
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
	script := `#!/bin/sh
set -eu
export GIT_TERMINAL_PROMPT=0
DEST=` + deliverScratchPath + `
REPO=` + shellQuote(repo) + `
BASE=` + shellQuote(base) + `
# The volume is a MOUNT POINT (cannot be rm -rf'd); wipe its contents.
if [ -d "${DEST}/.git" ]; then rm -rf "${DEST}/.git"; fi
mkdir -p "${DEST}"
git ` + deliverSafeDir() + ` init "${DEST}"
git -C "${DEST}" ` + deliverSafeDir() + ` remote add origin "${REPO}"
` + authLine + `  git ` + deliverSafeDir() + ` -C "${DEST}"` + fetchCred + ` fetch origin "${BASE}"
git -C "${DEST}" ` + deliverSafeDir() + ` checkout --detach FETCH_HEAD
echo "deliver clone-base: base ${BASE} at $(git -C "${DEST}" ` + deliverSafeDir() + ` rev-parse HEAD)"
`
	return corev1.Container{
		Name:         deliverCloneBase,
		Image:        r.workspaceGitImage(),
		Command:      []string{"/bin/sh", "-c", script},
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
// agent's workspace PVC (read-only) into the scratch volume. NO credential,
// NO network (file:// fetch), hooks disabled (core.hooksPath=/dev/null —
// the agent's repo could carry hooks the operator must never run), and the
// resolved commit is ASSERTED == the pinned verifiedCommit (a mismatch
// fails the Job — D27 evidence integrity: the deliver Job pushes ONLY the
// commit the operator verified).
func (r *LoopReconciler) deliverImportAgentContainer(loop *coxv1alpha1.Loop) corev1.Container {
	verified := loop.Status.CurrentVerify.VerifiedCommit
	script := `#!/bin/sh
set -eu
export GIT_TERMINAL_PROMPT=0
SRC=` + deliverAgentSrc + `
DEST=` + deliverScratchPath + `
PINNED=` + shellQuote(verified) + `
# Wipe the scratch (the volume is a mount point: wipe the .git contents,
# keep the dir).
if [ -d "${DEST}/.git" ]; then rm -rf "${DEST}/.git"; fi
mkdir -p "${DEST}"
# Fetch the pinned commit from the agent's .git over the file protocol.
# protocol.file.allow=always is REQUIRED (modern git refuses file:// by
# default); core.hooksPath=/dev/null disables any hook in the imported repo;
# GIT_TERMINAL_PROMPT=0 is belt-and-suspenders (no credential, no network —
# a prompt would hang).
git -c protocol.file.allow=always -c core.hooksPath=/dev/null ` + deliverSafeDirs() + ` init "${DEST}"
git -c protocol.file.allow=always -c core.hooksPath=/dev/null ` + deliverSafeDirs() + ` -C "${DEST}" fetch "file://${SRC}/.git" "${PINNED}"
# Assert the fetched commit == the pinned commit (a mismatch means the
# evidence is stale or the agent's .git moved — fail the Job, never push).
GOT=$(git -c core.hooksPath=/dev/null ` + deliverSafeDirs() + ` -C "${DEST}" rev-parse "${PINNED}")
if [ "$GOT" != "${PINNED}" ]; then
  echo "deliver import-agent: pinned ${PINNED} != fetched ${GOT}; refusing"
  exit 1
fi
git -c core.hooksPath=/dev/null ` + deliverSafeDirs() + ` -C "${DEST}" checkout --detach "${PINNED}"
echo "deliver import-agent: imported ${PINNED} from the agent workspace PVC"
`
	return corev1.Container{
		Name: deliverImport,
		Image: r.workspaceGitImage(),
		Command:      []string{"/bin/sh", "-c", script},
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

// deliverPushContainer pushes the imported commit to the delivery branch
// (NEVER --force; refusing a push whose target ref equals baseBranch or a
// default branch — main/master) and creates the pull request via the
// provider API (idempotent: an open PR for the branch is reused). Writes
// {branch, commit, prNumber, prURL} to /dev/termination-log. Carries the
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

	mounts := []corev1.VolumeMount{
		{Name: deliverScratchVol, MountPath: deliverScratchPath},
	}
	if creds {
		mounts = append(mounts, corev1.VolumeMount{Name: workspaceCredsVolume, MountPath: "/workspace-creds", ReadOnly: true})
	}

	var apiAuthLine string
	switch {
	case creds && prov == deliverProviderGitHub:
		apiAuthLine = "API_AUTH=\"Authorization: Bearer $(cat /workspace-creds/" + workspaceCredsPasswordKey + ")\""
	case creds:
		apiAuthLine = "API_AUTH=\"Authorization: Basic $AUTH\""
	default:
		apiAuthLine = "API_AUTH=\"\""
	}
	gitCredFlag := ""
	if creds {
		gitCredFlag = ` -c http.extraHeader="Authorization: Basic $AUTH"`
	}

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
` + apiAuthLine + `
# --- refusal: the delivery branch must NOT equal the base branch or a
# default branch (main/master). Pushing onto a default branch would deliver
# the agent's code straight to the operator's mainline — the PR gate is the
# point of delivery.
if [ "${BRANCH}" = "${BASE}" ] || [ "${BRANCH}" = "main" ] || [ "${BRANCH}" = "master" ]; then
  echo "deliver push: refusing to push branch ${BRANCH} (equals the base branch or a default branch)"
  exit 1
fi
# --- the API auth (GitHub: the Secret's PASSWORD as a Bearer token — the
# Secret is basic auth with username "x-access-token" and password = the
# token; Gitea-compatible: the same basic pair as a Basic header).
` + (func() string { if creds { return "AUTH=$(printf '%s:%s' \"$(cat /workspace-creds/" + workspaceCredsUsernameKey + ")\" \"$(cat /workspace-creds/" + workspaceCredsPasswordKey + ")\" | base64 -w 0)\n" }; return "" })() + `
git -C "${SRC}" ` + deliverSafeDir() + `` + gitCredFlag + ` push origin "HEAD:refs/heads/${BRANCH}"
# --- create the PR (idempotent: reuse an open PR for the branch).
# owner/name come from the repo URL (<host>/<owner>/<name>[.git]).
PATH_PART=${REPO#*://}
REMAIN=${PATH_PART#*/}
OWNER=${REMAIN%%/*}
REPO_NAME=${REMAIN#*/}
case "${REPO_NAME}" in
  *.git) REPO_NAME=${REPO_NAME%.git} ;;
esac
# Look for an existing open PR for the branch (reuse it — idempotent).
EXISTING=$(curl -sfS -H "$API_AUTH" "${API_BASE}/${OWNER}/${REPO_NAME}/pulls?state=open&head=${OWNER}:${BRANCH}" | tr -d '\n' | sed -n 's/.*"number"[[:space:]]*:[[:space:]]*\([0-9]*\).*/\1/p' | head -1) || EXISTING=""
if [ -n "${EXISTING}" ]; then
  PR_NUM=${EXISTING}
else
  PAYLOAD=$(printf '{"title":"coxswain: %s","head":"%s","base":"%s","body":"Delivered by coxswain from verified commit %s.","draft":%s}' \
    "${BRANCH}" "${BRANCH}" "${BASE}" "${PINNED}" "${DRAFT}")
  PR_NUM=$(curl -sfS -X POST -H "$API_AUTH" -H "Content-Type: application/json" -d "${PAYLOAD}" "${API_BASE}/${OWNER}/${REPO_NAME}/pulls" | sed -n 's/.*"number"[[:space:]]*:[[:space:]]*\([0-9]*\).*/\1/p')
fi
[ -n "${PR_NUM}" ] || { echo "deliver push: no PR number returned (API unreachable or refused)"; exit 1; }
# Write the result to the termination log (the operator reads it via the
# APIReader — kubelet-recorded, not a claim).
{
  echo "branch=${BRANCH}"
  echo "commit=${PINNED}"
  echo "prNumber=${PR_NUM}"
  echo "prURL=${API_BASE}/${OWNER}/${REPO_NAME}/pulls/${PR_NUM}"
} > /dev/termination-log
`
	return corev1.Container{
		Name:         deliverPush,
		Image:        r.workspaceGitImage(),
		Command:      []string{"/bin/sh", "-c", script},
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
// (https://api.github.com/repos — the PR create is POST
// /repos/<owner>/<repo>/pulls); for a Gitea-compatible provider, the repo
// host's Gitea API (<scheme>://<host>/api/v1/repos — the Gitea PR REST is
// the same shape, /repos/<owner>/<repo>/pulls).
func deliverAPIBase(repo string, prov deliverProvider) string {
	if prov == deliverProviderGitHub {
		return "https://api.github.com/repos"
	}
	u, err := url.Parse(repo)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host + "/api/v1/repos"
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
//   - prURL is a well-formed URL whose host is the repo host (or, for a
//     GitHub delivery, github.com — the PR URL's host is github.com, not
//     api.github.com) and whose path ends in /pulls/<prNumber>.
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
	for _, line := range strings.Split(msg, "\n") {
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
	// prURL MUST be well-formed, sized, on an allowed host, with a path
	// ending in /pulls/<prNumber>.
	if len(prURL) > deliverPRURLMaxBytes {
		return deliverOutcome{}, false
	}
	u, err := url.Parse(prURL)
	if err != nil || u.Host == "" {
		return deliverOutcome{}, false
	}
	host := strings.ToLower(u.Hostname())
	if prov == deliverProviderGitHub {
		if host != "github.com" {
			return deliverOutcome{}, false
		}
	} else if host != repoHost {
		return deliverOutcome{}, false
	}
	if !strings.HasSuffix(u.Path, fmt.Sprintf("/pulls/%d", prNum)) {
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
	name := loop.Name + "-deliver-netpol"
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
	err := r.Get(ctx, client.ObjectKey{Namespace: loop.Namespace, Name: loop.Name + "-deliver-netpol"}, np)
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
// Job's git push + the provider API call traverse the proxy for an external
// repo host, and the proxy must allowlist BOTH hosts. A nil (no external
// repo) or in-cluster repo returns no hosts (the direct repo-peer rule
// covers it, no proxy hop).
func (r *LoopReconciler) deliverEgressProxyHosts(loop *coxv1alpha1.Loop) []string {
	if !deliveryExpected(loop) {
		return nil
	}
	prov, host := deliverProviderForRepo(loop.Spec.Workspace.Repo)
	if peer := repoPeer(loop.Spec.Workspace.Repo, r.serviceNamespaceFromHost); peer != nil {
		// In-cluster: no proxy hop.
		return nil
	}
	if host == "" {
		return nil
	}
	hosts := []string{host}
	if prov == deliverProviderGitHub {
		hosts = append(hosts, "api.github.com")
	}
	return hosts
}

// ensureDeliverReadback reads the deliver Job pod's push container
// termination message via the APIReader (pod-blind, like the S3/S4
// read-backs), validates it (parseDeliverTermination), and — on a valid
// message — writes status.delivery + the Delivered=True condition. It is
// the operator's delivery read-back; the push container is the Job's MAIN
// container, so its termination message rides in containerStatuses[push].
//
// The read is a NO-OP when: delivery is not expected, there is no Job, the
// Job has not succeeded, or the push container has not terminated. A
// malformed or foreign message is rejected (no status write; the next
// re-read requeues — the pod may be mid-write). A terminated non-zero push
// (or a failed init) is Delivered=False reason DeliveryFailed (terminal —
// the Job is not retried).
func (r *LoopReconciler) ensureDeliverReadback(ctx context.Context, loop *coxv1alpha1.Loop) error {
	if !deliveryExpected(loop) || loop.Status.Delivery != nil {
		return nil
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
		return fmt.Errorf("list deliver pods for %s: %w", loop.Name, err)
	}
	if len(list.Items) == 0 {
		return nil // no pod yet (the Job was just created)
	}
	if len(list.Items) > 1 {
		return fmt.Errorf("multiple deliver pods for %s (got %d); refusing to read", loop.Name, len(list.Items))
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
		return nil // push has not terminated yet
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
		return nil
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
		return nil
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
	return nil
}
