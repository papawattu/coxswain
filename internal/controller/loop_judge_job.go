// Phase 7 J1c2 (PLAN-PHASE7.md slice J1, ADR-0011): the Judge Job BUILDER.
//
// The Judge is a second, ADVISORY model surface (the first is the Memory
// Curator, ADR-0002): after the acceptance checks pass and before Succeeded
// is recorded, the operator creates a short-lived Job owned by the Loop that
// asks a model — through the Loop's model proxy (D123) — "does this change
// meet the goal without gaming the acceptance?" and RECORDS the fixed-shape
// answer. It never interprets the prose (ADR-0002's boundary); the reply is
// the fixed JSON object pkg/judge validates.
//
// This file builds the Job and its NetworkPolicy (pure builders + unit
// tests). It is NOT wired into Reconcile yet (the reconcile path is J1c3).
//
// The Job shape copies the two proven siblings (ADR-0011 "Where it runs"):
//
//   - the memory-writer Job (loop_memory_writer_job.go): the operator's own
//     image, the read-only workspace PVC mount, the termination-message path
//     on an emptyDir, the model-proxy env (the curator's base URL), and the
//     NetworkPolicy that reaches ONLY the model proxy and DNS (the fence —
//     the Judge must not phone home a verdict the operator could not see);
//   - the verify Job (loop_verify_job.go): the verify base image (it has git)
//     for the diff init, the workspace PVC mounted read-only at /agent-src,
//     and the fail-closed git refusal (the tamper script's pattern: a
//     failing git diff exits non-zero, never reads as clean).
//
// Flow:
//
//  1. init container `judge-diff` (the verify base image, workspace PVC
//     READ-ONLY): writes the uncapped diff to /judge/full.patch, checks the
//     git exit status explicitly (a failing git diff exits 2 — the tamper
//     script's fail-closed pattern), caps the file to judgeDiffCapBytes+1
//     bytes (262145, the truncation marker cmd/judge's MaxDiffBytes+1 read
//     detects) into /judge/diff.patch, and removes the uncapped file.
//  2. container `judge` (the operator's own image,
//     /usr/local/bin/coxswain-judge): reads the env (goal, plan, checks,
//     diff path, result path), calls the Loop's model proxy with its own
//     max_tokens cap (spec.verify.judge.maxTokens), and writes the
//     fixed-shape verdict to the termination-message path (the operator
//     reads the kubelet-recorded termination message via pod get — no log
//     dependency, the verify / memory-writer channel).
//
// Hardening: non-root, read-only rootfs, no privilege escalation, all caps
// dropped, seccomp runtime default — and NO service-account token
// (automountServiceAccountToken false; the Job makes no API calls, the
// F66/D12 pattern). One Judge Job per verified iteration (the Job name
// carries the iteration, the verify Job's I65 pattern: a recreated Job has
// a new UID).
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// judgeForLabel is the Judge Job's / its pod's selector label: the Loop the
// Judge Job runs for (the memory-writer Job's memory-write-for label
// scheme, coxswain.io/<component>-for — a distinct key so the Judge's
// NetworkPolicy podSelector never matches the memory-writer's pods).
const judgeForLabel = "coxswain.io/judge-for"

// judgeComponentValue is the coxswain.io/component label value the Judge
// Job carries (distinct from "memory-writer" and "verify": the same label
// scheme, a distinct component).
const judgeComponentValue = "judge"

// managedByCoxswain is the managed-by label value (the controller that
// manages the object). Shared across the Job builders (goconst: it appears
// in the memory-writer, verify, and Judge Job label sets).
const managedByCoxswain = "coxswain-controller"

// gitConfigCountKey, gitConfigKey0, gitConfigValue0 are the env var names
// for git's GIT_CONFIG_* mechanism (the verify Job's verifyGitSafeEnv uses
// them too — goconst: shared across the Job builders).
const (
	gitConfigCountKey  = "GIT_CONFIG_COUNT"
	gitConfigKey0      = "GIT_CONFIG_KEY_0"
	gitConfigValue0Key = "GIT_CONFIG_VALUE_0"
)

// termmsgVol is the termination-message emptyDir volume's name (the
// memory-writer Job uses the same name — goconst: shared across the Job
// builders).
const termmsgVol = "termmsg"

// judgeDiffInit is the diff init container's name.
const judgeDiffInit = "judge-diff"

// judgeContainer is the judge container's name.
const judgeContainer = "judge"

// judgeVol is the /judge emptyDir volume's name (the diff.patch scratch).
const judgeVol = "judge"

// judgeAgentVol is the agent-workspace PVC volume's name (the same name the
// verify Job uses for its read-only import mount — one PVC, read-only, the
// only agent-data entry point).
const judgeAgentVol = "agent-workspace"

// judgeDiffPath is where the init writes the capped unified diff
// (ADR-0011 "Input": baseCommit..verifiedCommit, capped at 256 KiB).
const judgeDiffPath = "/judge/diff.patch"

// judgeDiffCapBytes is the diff cap: 256 KiB = 262144 bytes. The init
// writes a file of at most judgeDiffCapBytes+1 (262145) bytes: the extra
// byte is the truncation marker that cmd/judge's MaxDiffBytes+1 read
// detects (a file of exactly 262145 bytes is truncated; a file of 262144
// is the full diff). It is its own constant (the verify/memory-writer
// siblings use no head -c cap, so the literal is defined once here).
const judgeDiffCapBytes = 262144

// judgeDiffFullPath is where the init writes the UNCAPPED diff before capping
// (a temp file on the /judge emptyDir). It is removed after the cap is
// written so only the capped patch is left for the judge container.
const judgeDiffFullPath = "/judge/full.patch"

// judgeDiffScript builds the judge-diff init's sh script. It writes the
// baseCommit..verifiedCommit unified diff to judgeDiffFullPath on the /judge
// emptyDir (the UNCAPPED diff), checks the git exit status EXPLICITLY (a
// failing git diff exits 2 — the tamper script's pattern, fail closed),
// then caps the file to judgeDiffCapBytes+1 bytes (262145) into judgeDiffPath,
// and removes the uncapped file. The cap is judgeDiffCapBytes+1 (262145) —
// NOT 262144 — because cmd/judge reads MaxDiffBytes+1 (one byte more than
// 256 KiB) as the truncation marker: a file of exactly 262145 bytes is
// detected as truncated by cmd/judge's io.LimitReader + len check; a file
// of 262144 bytes is NOT (it is exactly MaxDiffBytes, which cmd/judge reads
// as "the full diff fit").
//
// Fail closed (the tamper script's pattern, loop_verify_job.go
// tamperCheckScript): the diff is written to a FILE (not a pipe), so a
// failing git diff takes its OWN exit status (checked explicitly) — the
// redirect is on the git command, not on head. A failing git diff exits 2
// (distinct from any other non-zero: the operator can tell a git failure
// from a write error) and the /judge/diff.patch file is never written
// (the redirect is inside the if-branch, so a git failure leaves no patch).
//
// POSIX sh only (the image's /bin/sh is busybox sh on alpine/git): no
// pipefail (a bashism — the file-based approach makes it unnecessary), no
// associative arrays. The diff is written to a file on the /judge emptyDir
// (writable: the readOnly rootfs makes / read-only), and head -c reads from
// that file (no pipe, no variable — the file is the buffer).
func judgeDiffScript(baseCommit, verifyCommit string) string {
	// The cap is judgeDiffCapBytes+1 (262145): one byte more than 256 KiB,
	// the truncation marker cmd/judge's MaxDiffBytes+1 read detects.
	cap := judgeDiffCapBytes + 1
	return fmt.Sprintf(`
set -e
# The Judge's input: the unified diff baseCommit..verifiedCommit, capped at
# %d bytes (judgeDiffCapBytes+1 = 262145, the truncation marker cmd/judge's
# MaxDiffBytes+1 read detects, ADR-0011 "Input"). The workspace is mounted
# read-only at /agent-src (the verify Job's tamper init operates on the same
# repo path). safe.directory: the workspace is root-owned while the container
# runs non-root (the verify Job's verifyGitSafeEnv covers the same bug).
# The diff is written to a FILE (not a pipe): a failing git diff takes its
# own exit status (checked explicitly, exit %d — the tamper script's pattern),
# and the redirect is inside the if-branch so a git failure leaves no patch.
git -c safe.directory='*' -C /agent-src diff --no-ext-diff --no-color --binary %s %s > %s || {
  echo "judge-diff: git diff failed; refusing to hand the Judge a diff"
  exit %d
}
# Cap the diff at %d bytes: head -c on the UNCAPPED file (a file read, not a
# pipe — no pipefail needed, no bashism). The extra byte (judgeDiffCapBytes+1
# = %d) is the truncation marker cmd/judge's MaxDiffBytes+1 read detects.
head -c %d %s > %s
# Remove the uncapped file: only the capped patch is left for the judge
# container (the /judge emptyDir is shared between the init and the judge
# container; a leftover full.patch would be read by the judge container if
# it looked for it — it doesn't, but rm -f is cheap insurance).
rm -f %s
echo "judge-diff ok: $(wc -c < %s) bytes (capped)"
exit 0
`, cap, judgeGitErrorExit, shellQuote(baseCommit), shellQuote(verifyCommit), judgeDiffFullPath, judgeGitErrorExit,
		cap, cap, cap, judgeDiffFullPath, judgeDiffPath,
		judgeDiffFullPath, judgeDiffPath)
}

// judgeGitErrorExit is the judge-diff init's exit status when git itself
// fails (distinct from any other non-zero: the operator reads the init's
// exit code from the pod status, the verify Job's evidence pattern).
const judgeGitErrorExit = 2

// judgeResultPath is the judge container's termination-message path. The
// kubelet records the last 4 KB of the container's terminated message there
// (the operator reads the fixed-shape verdict from the pod's container
// status — no pod-log read, the verify / memory-writer channel). It lives
// on the /tmp emptyDir (writable: the readOnly rootfs makes / read-only).
const judgeResultPath = "/tmp/judge-verdict.txt"

// judgeJobName returns the Judge Job's name: <loop>-judge-<iteration>. It
// reuses the verify Job's iteration mapping (verifyJobName) so a Judge Job
// and its verify Job for the same iteration line up in the pod list, and a
// re-verify / re-implement creates a NEW Judge Job (never mutating the
// prior one — the I65 pattern: a recreated Job has a new UID).
func judgeJobName(loop *coxv1alpha1.Loop) string {
	// verifyJobName: <loop>-verify-<iteration>. Reuse its iteration mapping
	// (status.iteration, 0-based; <= 0 maps to 1) by replacing the
	// "-verify-" component with "-judge-".
	return replaceSuffix(verifyJobName(loop), "-verify-", "-judge-")
}

// judgeEnabled reports whether this Loop opted into the advisory Judge
// (ADR-0011, J1c3): spec.verify.judge is present AND .enabled is true. A
// nil judge config (never defaulted) or an explicit enabled: false both
// disable it — the Judge is opt-in and never runs without the flag.
// judgeSettleStep is the Reconcile-facing wrapper around ensureJudgeAndSettle:
// it runs the Judge hold decision and folds any change into the caller's
// changed flag and requeue flag so Reconcile gains no branch (it sits at its
// gocyclo budget, as the memory-writer and F62 alert-relay steps do). It is
// a no-op when the Loop is not held for the Judge.
func (r *LoopReconciler) judgeSettleStep(ctx context.Context, loop *coxv1alpha1.Loop, changed, requeue *bool) error {
	judgeRequeue, err := r.ensureJudgeAndSettle(ctx, loop, changed)
	*requeue = *requeue || judgeRequeue
	return err
}

func judgeEnabled(loop *coxv1alpha1.Loop) bool {
	return loop.Spec.Verify.Judge != nil &&
		loop.Spec.Verify.Judge.Enabled
}

// loopHoldingForJudge (ADR-0011, J1c3) reports whether the Loop passed
// verify but is held at phase Verifying (condition Verifying=True, reason
// Judging) until the Judge Job is terminal — the hold is carried ONLY by
// this condition's reason (the operator is its single writer: applyVerifySucceeded
// sets it, ensureJudgeAndSettle releases it), the same scheme as the
// delivery hold (loopHoldingForDelivery, P5f I98 D69 option i).
func loopHoldingForJudge(loop *coxv1alpha1.Loop) bool {
	if loop.Status.Phase != coxv1alpha1.LoopPhaseVerifying {
		return false
	}
	for i := range loop.Status.Conditions {
		c := &loop.Status.Conditions[i]
		if c.Type == string(coxv1alpha1.LoopPhaseVerifying) {
			return c.Status == metav1.ConditionTrue && c.Reason == coxv1alpha1.ReasonVerifyingJudging
		}
	}
	return false
}

// loopHeldAfterVerify reports whether the Loop passed verify but is held at
// phase Verifying by either the Judge hold (reason Judging, ADR-0011) or the
// delivery hold (reason Delivering, P5f). It replaces loopHoldingForDelivery
// at the call sites that gate verify-Job recreation, budget, and pause: a
// Judge-held Loop is past verify, so the verify Job is not recreated, the
// budget does not fire, and the sandbox is not treated as running. The
// delivery-specific gates (deliveryPhaseActive, settleDeliveryHold) remain
// Delivering-only.
func loopHeldAfterVerify(loop *coxv1alpha1.Loop) bool {
	return loopHoldingForDelivery(loop) || loopHoldingForJudge(loop)
}

// replaceSuffix replaces a (guaranteed) substring in s and returns the
// result. A tiny helper: judgeJobName's one use keeps judgeJobName a one-
// liner; an absent suffix (a future verifyJobName shape change) leaves the
// name unchanged — judgeJobName's unit test pins the exact name so the
// drift is loud.
func replaceSuffix(s, old, new string) string {
	before, after, found := strings.Cut(s, old)
	if !found {
		return s
	}
	return before + new + after
}

// judgeJobLabels are the labels the Judge Job carries (the memory-writer
// Job's label scheme, loop_memory_writer_job.go memoryWriterJobLabels, with
// the Judge's own component value and -for label). The podSelector for the
// Judge Job's NetworkPolicy is the judgeForLabel.
func judgeJobLabels(loopName string) map[string]string {
	return map[string]string{
		labelPartOf:             partOfCoxswain,
		verifyLoopLabel:         loopName,
		judgeForLabel:           loopName,
		"coxswain.io/component": judgeComponentValue,
		managedByLabel:          managedByCoxswain,
	}
}

// judgeNetPolName is the Judge Job's NetworkPolicy name
// (<loop>-judge-netpol), mirroring the memory-writer's
// <loop>-memory-write-netpol.
func judgeNetPolName(loopName string) string {
	return fmt.Sprintf("%s-judge-netpol", loopName)
}

// judgeEnvValueMap returns the Judge container's env vars as a map (the
// unit tests assert exact values; buildJudgeJob assembles the slice in a
// stable order). The map is built from the Loop's spec + status, so a
// caller without a full Loop (a nil field) is an explicit error, not a
// silent zero.
//
// COX_JUDGE_CHECKS is JSON: the last verify's check names (check-0,
// check-1, … — the verify Job's init container names) and their exit codes
// (status.verify.lastCheckResults, one per acceptance check in order, the
// D14 gate values the operator already read; ADR-0011 "Input").
//
// COX_JUDGE_PLAN is the plan summary in status, if any: the operator has
// no separate plan-text field — status carries the plan HASH
// (PlanApproved's message) and the attempt count, so the "plan summary" is
// composed from those (the hash is the plan's identity, the attempt the
// plan's version). An empty string when neither is set (a Loop that has
// never planned — the Judge's input is then goal + diff + checks only).
func (r *LoopReconciler) judgeEnvValueMap(loop *coxv1alpha1.Loop) map[string]string {
	env := map[string]string{
		"COX_JUDGE_DIFF_PATH":   judgeDiffPath,
		"COX_JUDGE_RESULT_PATH": judgeResultPath,
		"COX_JUDGE_GOAL":        loop.Spec.Goal,
		"COX_JUDGE_PLAN":        planSummary(loop),
	}
	if loop.Spec.Agent.ModelEndpoint != "" {
		// The Judge's call goes through the Loop's model proxy (the same
		// URL the curator uses — loop_memory_writer_job.go's
		// COX_CURATOR_BASE_URL; D123: one metered path, no new
		// credential). COX_JUDGE_MODEL is the Judge's own model name
		// when spec.verify.judge.model names one (D132: a separate
		// judge on the same endpoint), else the Loop's model name (the
		// same value the curator's COX_MODEL carries; D123's default).
		env["COX_JUDGE_BASE_URL"] = r.proxyServiceURL(loop.Name, loop.Namespace)
		model := loop.Spec.Agent.Model
		if loop.Spec.Verify.Judge != nil && loop.Spec.Verify.Judge.Model != "" {
			model = loop.Spec.Verify.Judge.Model
		}
		env["COX_JUDGE_MODEL"] = model
	}
	if loop.Spec.Verify.Judge != nil && loop.Spec.Verify.Judge.MaxTokens > 0 {
		// The Judge's own token cap (spec.verify.judge.maxTokens, ADR-0011
		// "Model": the Judge sets the model request's max_tokens to this
		// cap). The admission XValidation (J1a) guarantees an enabled
		// Judge carries it; a disabled / absent Judge leaves the env unset
		// (the reader treats an empty cap as the reader's error — a
		// malformed call, never a silent default).
		env["COX_JUDGE_MAX_TOKENS"] = fmt.Sprintf("%d", loop.Spec.Verify.Judge.MaxTokens)
	}
	env["COX_JUDGE_CHECKS"] = judgeChecksJSON(loop)
	return env
}

// planSummary composes the plan-summary string the Judge is given
// (COX_JUDGE_PLAN). The operator's status carries no plan text (the agent
// writes PLAN.md into its own workspace; the operator records the plan's
// HASH on the PlanApproved condition's message and the attempt count on
// status.planAttempt). The summary is the hash (when approved) + the
// attempt count, or "" when neither is set. ADR-0011 "Input" gives the
// Judge "the goal, PLAN.md, and the diff" — the operator-composed version
// of PLAN.md's identity, not its prose (ADR-0002's boundary: the operator
// records, it does not interpret).
func planSummary(loop *coxv1alpha1.Loop) string {
	hash := approvedPlanHash(loop)
	switch {
	case hash != "":
		return fmt.Sprintf("plan hash %s (attempt %d)", hash, loop.Status.PlanAttempt)
	case loop.Status.PlanAttempt > 0:
		return fmt.Sprintf("plan attempt %d (no approved hash recorded)", loop.Status.PlanAttempt)
	default:
		return ""
	}
}

// judgeChecksJSON builds the COX_JUDGE_CHECKS env value: a JSON array of
// the last verify's check names + exit codes (ADR-0011 "Input": "the check
// names and their exit codes (the D14 gate values the operator already
// read, one per check container)"). The names are the ACCEPTANCE CHECK
// COMMANDS — spec.verify.acceptanceChecks[i] (the user's own shell command,
// e.g. "go test ./...") — NOT the container names (check-0, check-1, …).
// The exit codes are status.verify.lastCheckResults (one per acceptance
// check, in order — the -1 sentinel means "did not run / no exit code yet",
// recordCheckResults) matched by index to the check container.
//
// The command (not the container name) is what the Judge needs: the model
// reads the command to know WHAT the check actually verifies. A real
// homelab model reply judged an honest change false when it saw only
// "check-0: exit 0" and true when it saw the command (the reviewer's
// scratchpad judge-capture evidence: reply-honest.json vs reply2-honest.json).
// The container name is opaque to the Judge — it carries no information
// about what the check does; the command is the check's own identity.
//
// An empty array "[]" when no verify has been recorded (the Judge runs
// after the checks pass, so a real call always has the codes; the empty
// array is the reader's signal that the evidence was unavailable, not a
// "no checks" claim).
func judgeChecksJSON(loop *coxv1alpha1.Loop) string {
	type judgeCheck struct {
		Name     string `json:"name"`
		ExitCode *int32 `json:"exitCode,omitempty"`
	}
	checks := []judgeCheck{}
	if loop.Status.Verify != nil {
		for i, code := range loop.Status.Verify.LastCheckResults {
			name := fmt.Sprintf("check-%d", i) // fallback: container name
			if i < len(loop.Spec.Verify.AcceptanceChecks) {
				name = loop.Spec.Verify.AcceptanceChecks[i] // the command
			}
			checks = append(checks, judgeCheck{Name: name, ExitCode: code})
		}
	}
	b, err := json.Marshal(checks)
	if err != nil {
		// json.Marshal of []judgeCheck{} cannot fail; a failure here is a
		// bug (a malformed struct tag), not an input problem. The env
		// value falls back to the empty array so the Job still builds (the
		// reader reads an empty checks list as "no checks listed", the
		// prompt's own empty-case wording).
		return "[]"
	}
	return string(b)
}

// judgeJobDeadlineSeconds is the Judge Job's activeDeadlineSeconds (the
// hung-pod fix, the memory-writer Job's pattern): a stuck judge pod must
// FAIL the Job after this deadline (the batch controller fails it with
// reason DeadlineExceeded) so the operator records the Judge as unavailable
// (D124: JudgeVerdict=Unknown, the Loop proceeds). 300s: the diff init is
// fast (a local git diff, no fetch), and the model call's own client
// timeout (pkg/judge, 60s + a 7s retry ladder) is far under it — a pod
// that has not terminated by 300s is hung, not slow.
const judgeJobDeadlineSeconds = int64(300)

// judgeJobBackoffLimit is the Judge Job's backoffLimit: the Job runs ONCE
// (the memory-writer's 0: a retry would re-run the same diff + the same
// model call — the verdict is the verdict, a retry does not change it, and
// a non-zero backoffLimit would let the batch controller restart the pod
// after a failure and mask the unavailable outcome).
const judgeJobBackoffLimit = int32(0)

// buildJudgeJob returns the Judge Job's spec (J1c2: the builder only — no
// Reconcile wiring). It is the memory-writer Job's shape (single hardened
// container, operator's own image, read-only workspace PVC, the
// termination-message path on an emptyDir, the model-proxy env) plus the
// verify Job's diff init (the verify base image, the workspace PVC
// read-only at /agent-src, a fail-closed git diff).
//
// The judge container runs /usr/local/bin/coxswain-judge (the operator's
// own binary, the memory-writer's entrypoint pattern — the Job is
// OPERATOR-CONTROLLED code, never model-authored code; the diff the Judge
// reads is agent-AUTHORED data, handed to it as text on the /judge
// emptyDir, and the Judge executes no agent code — ADR-0011 "Where it
// runs").
func (r *LoopReconciler) buildJudgeJob(loop *coxv1alpha1.Loop) (*batchv1.Job, error) {
	// The Judge reads the agent's diff from status.currentVerify
	// (verifiedCommit) + status.baseCommit: without a pinned verifiedCommit
	// there is nothing to judge (the Job is created after the checks pass,
	// so a real run has the pin; a missing pin is the reader's error, not a
	// Job the reader creates on an empty diff).
	verifiedCommit := ""
	if loop.Status.CurrentVerify != nil {
		verifiedCommit = loop.Status.CurrentVerify.VerifiedCommit
	}
	if loop.Status.BaseCommit == "" || verifiedCommit == "" {
		return nil, fmt.Errorf("build judge job %s: the Loop has no baseCommit / verifiedCommit pin (the Judge's diff has no range)", loop.Name)
	}

	// The judge container's env (the map form: buildJudgeJob assembles the
	// slice in a stable order, the unit tests assert exact values).
	envMap := r.judgeEnvValueMap(loop)
	env := make([]corev1.EnvVar, 0, len(envMap))
	for _, name := range []string{
		"COX_JUDGE_BASE_URL", "COX_JUDGE_MODEL", "COX_JUDGE_MAX_TOKENS",
		"COX_JUDGE_GOAL", "COX_JUDGE_PLAN", "COX_JUDGE_CHECKS",
		"COX_JUDGE_DIFF_PATH", "COX_JUDGE_RESULT_PATH",
	} {
		if v, ok := envMap[name]; ok {
			env = append(env, corev1.EnvVar{Name: name, Value: v})
		}
	}

	deadline := judgeJobDeadlineSeconds
	backoff := judgeJobBackoffLimit
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      judgeJobName(loop),
			Namespace: loop.Namespace,
			Labels:    judgeJobLabels(loop.Name),
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:          &backoff,
			ActiveDeadlineSeconds: &deadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: judgeJobLabels(loop.Name),
				},
				Spec: corev1.PodSpec{
					// Pod-level security (the memory-writer Job's pattern):
					// non-root (65532 — the operator image's USER), seccomp
					// runtime default. The container's own SecurityContext
					// carries the additional hardening (read-only rootfs,
					// no privilege escalation, caps dropped).
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: &memoryWriterTrue,
						RunAsUser:    &memoryWriterNonRootUID,
						RunAsGroup:   &memoryWriterNonRootGID,
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					RestartPolicy: corev1.RestartPolicyNever,
					// The Judge Job's pod makes NO API calls (the diff init
					// reads the workspace mount, the judge container reads
					// the env + the diff + the model proxy). NO
					// ServiceAccount is named and
					// automountServiceAccountToken is false (the F66/D12
					// pattern: no token is mounted, so the Job CANNOT make
					// API calls even if the default SA's RBAC were broader).
					AutomountServiceAccountToken: &memoryWriterFalse,
					// The init container: the verify base image (it has git),
					// the workspace PVC READ-ONLY at /agent-src (the verify
					// Job's tamper init operates on the same repo path), and
					// the /judge emptyDir (writable: the readOnly rootfs
					// makes / read-only, so the diff patch lives on the
					// emptyDir).
					InitContainers: []corev1.Container{{
						Name:            judgeDiffInit,
						Image:           r.verifyJobImage(),
						Command:         []string{verifySh, "-c", judgeDiffScript(loop.Status.BaseCommit, verifiedCommit)},
						WorkingDir:      "/",
						SecurityContext: trustedContainerSecurityContext(),
						Resources:       verifyTrustedResources(),
						Env: []corev1.EnvVar{
							// safe.directory for the read-only workspace
							// (the verify Job's verifyGitSafeEnv pattern —
							// the workspace is root-owned while the
							// container runs non-root; the script also
							// passes -c safe.directory='*', so the env is
							// belt-and-braces, not the load-bearing gate).
							{Name: gitConfigCountKey, Value: "1"},
							{Name: gitConfigKey0, Value: verifySafeDirectory},
							{Name: gitConfigValue0Key, Value: deliverAgentSrc},
						},
						VolumeMounts: []corev1.VolumeMount{
							// The workspace PVC, READ-ONLY (the only
							// agent-data entry point into the Job; the
							// diff init reads it, the judge container does
							// NOT mount it — it sees the agent's work only
							// as the diff text on /judge).
							{Name: judgeAgentVol, MountPath: "/agent-src", ReadOnly: true},
							// The /judge emptyDir (the diff.patch scratch).
							{Name: judgeVol, MountPath: "/judge"},
						},
					}},
					Containers: []corev1.Container{{
						Name:  judgeContainer,
						Image: r.memoryWriterImage(),
						// The operator's judge binary, run DIRECTLY (the
						// memory-writer's pattern: the distroless image has
						// no /bin/sh — the binary's first hop is the env,
						// not a shell). COX_JUDGE_RESULT_PATH points the
						// binary at the termination-message path so it
						// writes the fixed-shape verdict there itself (the
						// operator reads it from the pod's container
						// status — no log dependency).
						Command:         []string{"/usr/local/bin/coxswain-judge"},
						ImagePullPolicy: corev1.PullIfNotPresent,
						WorkingDir:      "/",
						SecurityContext: &corev1.SecurityContext{
							RunAsNonRoot:             &memoryWriterTrue,
							RunAsUser:                &memoryWriterNonRootUID,
							RunAsGroup:               &memoryWriterNonRootGID,
							ReadOnlyRootFilesystem:   &memoryWriterTrue,
							AllowPrivilegeEscalation: &memoryWriterFalse,
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{allCaps}},
							SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
						},
						TerminationMessagePath:   judgeResultPath,
						TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
						Env:                      env,
						Resources:                verifyTrustedResources(),
						VolumeMounts: []corev1.VolumeMount{
							// The /judge emptyDir (the diff the Judge reads;
							// READ-ONLY: the Judge writes its verdict to
							// /tmp, never back to the diff).
							{Name: judgeVol, MountPath: "/judge", ReadOnly: true},
							// An emptyDir for the termination message (the
							// readOnly rootfs makes / read-only; the
							// verdict file lives on the emptyDir).
							{Name: termmsgVol, MountPath: "/tmp"},
						},
					}},
					Volumes: []corev1.Volume{
						// The workspace PVC (the Loop's existing workspace
						// PVC — the diff init reads the agent's .git from
						// it). READ-ONLY (the Job never writes the
						// workspace).
						{Name: judgeAgentVol, VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
								ClaimName: workspacePVCName(loop.Name),
								ReadOnly:  true,
							},
						}},
						// The /judge emptyDir (the diff patch).
						{Name: judgeVol, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						// The termination message's emptyDir.
						{Name: termmsgVol, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					},
				},
			},
		},
	}
	// C2 + D101: the Judge Job's pod template carries the CURRENT Loop node
	// placement (Jobs are immutable and short-lived: a flag change re-places
	// NEW Judge Jobs only — the memory-writer / verify Jobs' pattern).
	r.applyLoopPlacement(&job.Spec.Template.Spec)
	return job, nil
}

// judgeNetworkPolicy returns the Judge Job's NetworkPolicy: the pod may
// reach ONLY the Loop's model proxy and DNS (the memory-writer Job's fence,
// loop_memory_writer_job.go ensureMemoryWriterNetworkPolicy — the Judge
// must not phone home a verdict the operator could not see).
//
// The podSelector is the judgeForLabel (the Job stamps it on the pod
// template). A Loop with no model endpoint has NO proxy and a fully denied
// NetworkPolicy (the memory-writer's P5b fence — a judge container with no
// proxy URL fails its call and is recorded unavailable, D124: the deny-all
// is the fence, the call's failure is the surface).
func judgeNetworkPolicy(loop *coxv1alpha1.Loop) *networkingv1.NetworkPolicy {
	desired := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      judgeNetPolName(loop.Name),
			Namespace: loop.Namespace,
			Labels:    judgeJobLabels(loop.Name),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{judgeForLabel: loop.Name},
			},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			// Deny ALL ingress: no Ingress rules (F16: an empty rule {}
			// would MATCH everything — allow all, the opposite of the
			// fence). Egress is denied the same way, EXCEPT the model proxy
			// pod + DNS when the Loop has a model (added below).
		},
	}
	if loop.Spec.Agent.ModelEndpoint != "" {
		// The model-proxy egress rule (the Judge's /v1/chat/completions POST
		// to the proxy; the proxy pod carries coxswain.io/proxy-for: <loop>)
		// + the DNS egress rule (the proxy URL is a DNS name — proxyServiceURL
		// — so the judge pod needs DNS egress to resolve it, the
		// memory-writer's D34 rule).
		desired.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{
			{
				To: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{kubeNSLabelKey: loop.Namespace},
					},
					PodSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{netpolProxyForLabel: loop.Name},
					},
				}},
				Ports: []networkingv1.NetworkPolicyPort{{Port: intstrPtr32(proxyPort)}},
			},
			{
				To:    []networkingv1.NetworkPolicyPeer{dnsPeer()},
				Ports: dnsPorts(),
			},
		}
	}
	return desired
}

// J1c3 (ADR-0011): the Judge Job RECONCILE WIRING.
//
// ensureJudgeAndSettle is the Reconcile-side counterpart of buildJudgeJob
// (J1c2): when the Loop is held at Verifying for the Judge (reason Judging,
// loopHoldingForJudge) it ensures the Judge NetworkPolicy + Job (both owned
// by the Loop) and decides the hold:
//
//   - Job in progress (neither Complete nor Failed) -> requeue, NO decision
//     (the I49 norm: a still-running Judge is not a verdict);
//   - Job Complete -> read the judge container's termination message and
//     parse the fixed-shape verdict ({"verdict","score","reason",
//     "truncated"}): record it on status.judge + the JudgeVerdict condition
//     (True/False, reason Judged) + a Normal Event;
//   - {"unavailable",...} OR unparsable OR Job Failed (incl.
//     DeadlineExceeded) OR a buildJudgeJob error -> status.judge {Verdict
//     nil, Reason=cause} + JudgeVerdict=Unknown (reason JudgeUnavailable) +
//     a Warning Event (D124: a fallback must surface its failure before it
//     degrades — the verdict is never read as a pass or a fail);
//   - then RELEASE the hold: delivery enabled -> Verifying=True reason
//     Delivering (the existing P5f hold, so deliverDraft sees status.judge);
//     otherwise Succeeded exactly as applyVerifySucceeded's non-delivery
//     branch (phase + desiredPhase + bumpLoopCompletion).
//
// The hold is released even when the Judge is disabled mid-hold
// (spec.verify.judge cleared after the hold was set): no Job is created and
// the Loop proceeds with no verdict.

// ensureJudgeAndSettle decides the Judge hold (ADR-0011, J1c3). It acts ONLY
// while loopHoldingForJudge (phase Verifying, Verifying=True reason Judging);
// every other state is a no-op (the delivery hold and the non-held paths are
// untouched). It returns (requeue, err): a requeue is the in-progress signal
// (the Job's final reconcile is triggered by the Owns(&batchv1.Job{}) watch
// plus this 5s RequeueAfter).
func (r *LoopReconciler) ensureJudgeAndSettle(ctx context.Context, loop *coxv1alpha1.Loop, changed *bool) (bool, error) {
	if !loopHoldingForJudge(loop) {
		return false, nil
	}
	// Disabled mid-hold (spec.verify.judge cleared after the hold was set):
	// release with no Job and no verdict (the Loop proceeds exactly as the
	// judge-off path).
	if !judgeEnabled(loop) {
		r.releaseJudgeHold(loop, changed)
		return false, nil
	}
	// The NetworkPolicy BEFORE the Job (the memory-writer's P3-6 ordering:
	// the judge pod never runs without its fence).
	if err := r.ensureJudgeNetworkPolicy(ctx, loop); err != nil {
		return false, fmt.Errorf("ensure judge netpol: %w", err)
	}
	job, err := r.ensureJudgeJob(ctx, loop)
	if err != nil {
		// A build error (e.g. missing verify inputs) is a Judge failure, not
		// a controller error (D124: the Loop proceeds — the failure is
		// surfaced, the verdict is Unknown, never a pass).
		r.recordJudgeUnavailable(loop, "judge job build failed: "+err.Error())
		*changed = true
		r.releaseJudgeHold(loop, changed)
		return false, nil
	}
	// Job Failed (incl. DeadlineExceeded): the Judge's verdict is Unknown
	// (D124) and the Loop proceeds. Decided before the pod read — the
	// DeadlineExceeded case leaves a pod that may never reach a terminal
	// phase (the memory-writer's F73(a) ordering).
	if jobTerminalCondition(job, batchv1.JobFailed) != nil {
		cond := jobTerminalCondition(job, batchv1.JobFailed)
		r.recordJudgeUnavailable(loop, "judge job failed: "+cond.Reason)
		*changed = true
		r.releaseJudgeHold(loop, changed)
		return false, nil
	}
	// Job in progress (the Complete condition was checked after Failed; no
	// conditions at all is also in progress): NO decision, requeue.
	if jobTerminalCondition(job, batchv1.JobComplete) == nil {
		return true, nil
	}
	// Job Complete: read the judge container's termination message (the
	// kubelet-recorded verdict; the memory-writer's readMemoryWriterJobPod
	// pattern) and decide it.
	msg, err := r.judgeTerminationMessage(ctx, loop, job)
	if err != nil {
		// A pod read error is not a decision (the memory-writer's F73(b)):
		// the Job's own conditions are authoritative and Complete is set —
		// but the verdict is unread, so it is unavailable (D124).
		r.recordJudgeUnavailable(loop, "judge verdict unreadable: "+err.Error())
		*changed = true
		r.releaseJudgeHold(loop, changed)
		return false, nil
	}
	r.applyJudgeVerdict(loop, msg)
	*changed = true
	r.releaseJudgeHold(loop, changed)
	return false, nil
}

// ensureJudgeJob returns the current Judge Job: it creates it (idempotently
// — an existing Job with the name is left untouched, the I65 pattern) when
// absent and returns the existing one otherwise. The Job is owned by the
// Loop (controller ref: deleting the Loop garbage-collects it).
func (r *LoopReconciler) ensureJudgeJob(ctx context.Context, loop *coxv1alpha1.Loop) (*batchv1.Job, error) {
	name := judgeJobName(loop)
	existing := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: loop.Namespace}, existing)
	if err == nil {
		return existing, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get judge job %s: %w", name, err)
	}
	job, err := r.buildJudgeJob(loop)
	if err != nil {
		return nil, err
	}
	if err := ctrl.SetControllerReference(loop, job, r.Scheme); err != nil {
		return nil, fmt.Errorf("set owner on judge job %s: %w", name, err)
	}
	if err := r.Create(ctx, job); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// A concurrent reconcile created it (or a stale Job's deletion
			// has not landed): re-read on the next pass, never skip.
			if gerr := r.Get(ctx, types.NamespacedName{Name: name, Namespace: loop.Namespace}, existing); gerr != nil {
				return nil, fmt.Errorf("get judge job %s after create: %w", name, gerr)
			}
			return existing, nil
		}
		return nil, fmt.Errorf("create judge job %s: %w", name, err)
	}
	return job, nil
}

// ensureJudgeNetworkPolicy creates (idempotently) the Judge Job's
// NetworkPolicy: deny all ingress and egress except the Loop's model proxy
// and DNS (the memory-writer's ensureMemoryWriterNetworkPolicy pattern —
// drift against the whole desired spec triggers an update). The NetPol is
// owned by the Loop (controller ref: the apiserver garbage-collects it on
// Loop delete).
func (r *LoopReconciler) ensureJudgeNetworkPolicy(ctx context.Context, loop *coxv1alpha1.Loop) error {
	desired := judgeNetworkPolicy(loop)
	if err := ctrl.SetControllerReference(loop, desired, r.Scheme); err != nil {
		return fmt.Errorf("set the judge netpol %s owner to the Loop: %w", desired.Name, err)
	}
	existing := &networkingv1.NetworkPolicy{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: loop.Namespace}, existing)
	if err == nil {
		if equality.Semantic.DeepEqual(existing.Spec, desired.Spec) {
			return nil // already correct
		}
		existing.Spec = desired.Spec
		if err := r.Update(ctx, existing); err != nil {
			return fmt.Errorf("update drifted judge netpol %s: %w", desired.Name, err)
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get judge netpol %s: %w", desired.Name, err)
	}
	if err := r.Create(ctx, desired); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("create judge netpol %s: %w", desired.Name, err)
	}
	return nil
}

// judgeTerminationMessage reads the judge container's termination message
// from the Judge Job's pod (the I65 pattern: match the pod to the CURRENT
// Job's UID, not just its name — a recreated Job has a new UID; skip
// terminating pods). It returns the trimmed termination message (the fixed
// JSON line the Judge wrote to the termination-message path).
func (r *LoopReconciler) judgeTerminationMessage(ctx context.Context, loop *coxv1alpha1.Loop, job *batchv1.Job) (string, error) {
	reader := r.apiReader
	if reader == nil {
		reader = r
	}
	jobUID := job.UID
	list := &corev1.PodList{}
	if err := reader.List(ctx, list,
		client.InNamespace(loop.Namespace),
		client.MatchingLabels{judgeForLabel: loop.Name}); err != nil {
		return "", fmt.Errorf("list judge pods: %w", err)
	}
	var pod *corev1.Pod
	for i := range list.Items {
		p := &list.Items[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		if podOwnedByJobUID(p, jobUID) {
			pod = p
		}
	}
	if pod == nil {
		return "", fmt.Errorf("no live judge pod for job %s (the pod may have been GC'd)", job.Name)
	}
	// The judge container's termination message: the fixed JSON line the
	// Judge wrote to its termination-message path (the memory-writer's
	// memoryWriterSummary pattern).
	for i := range pod.Status.ContainerStatuses {
		cs := &pod.Status.ContainerStatuses[i]
		if cs.Name == judgeContainerName && cs.State.Terminated != nil {
			return strings.TrimSpace(cs.State.Terminated.Message), nil
		}
	}
	return "", fmt.Errorf("the judge container %s has no terminated state on pod %s", judgeContainerName, pod.Name)
}

// judgeVerdict is the fixed-shape reply the Judge Job reports via its
// termination message (pkg/judge's output contract, ADR-0011): the operator
// validates the shape and records the fields, never interprets prose.
type judgeVerdict struct {
	// Unavailable is the marker key in the {"unavailable",...} form: the
	// Judge (cmd/judge) writes it when the model call failed — the operator
	// records JudgeVerdict=Unknown (D124), never a pass or a fail.
	Unavailable string `json:"unavailable,omitempty"`
	// Verdict is the Judge's boolean answer (required in the verdict form).
	Verdict *bool `json:"verdict,omitempty"`
	// Score is the Judge's confidence score, 0-100.
	Score int32 `json:"score,omitempty"`
	// Reason is the Judge's one-line reason, at most 200 characters. A
	// longer reason is a malformed reply (unavailable, not a truncation).
	Reason string `json:"reason,omitempty"`
	// Truncated is true when the diff exceeded the 256 KiB cap and was
	// truncated to its first 256 KiB.
	Truncated bool `json:"truncated,omitempty"`
}

// applyJudgeVerdict is the decision table for the Judge Job's completion:
// a parsable verdict form is recorded (True/False, reason Judged, Normal
// Event); everything else — the unavailable marker, an unparsable line, a
// missing boolean, an out-of-range score, or an over-long reason — is
// unavailable (D124: Unknown, reason JudgeUnavailable, Warning Event; the
// Loop proceeds).
func (r *LoopReconciler) applyJudgeVerdict(loop *coxv1alpha1.Loop, msg string) {
	var v judgeVerdict
	if err := json.Unmarshal([]byte(msg), &v); err != nil || v.Verdict == nil {
		// A line that does not parse (or lacks the boolean verdict) is an
		// unavailable line: the Judge's own failure marker is recorded with
		// its cause, any other garbage is recorded as malformed.
		if err == nil && v.Verdict == nil && v.Unavailable != "" {
			r.recordJudgeUnavailable(loop, v.Unavailable)
			return
		}
		r.recordJudgeUnavailable(loop, "malformed judge verdict: "+msg)
		return
	}
	if v.Score < 0 || v.Score > 100 {
		// A score outside 0..100 is a malformed reply (unavailable, not a
		// clamped record — the CRD's Min=0/Max=100 would reject the status
		// write otherwise, and clamping would record a confidence the Judge
		// did not report).
		r.recordJudgeUnavailable(loop, "malformed judge verdict: score out of range")
		return
	}
	if len(v.Reason) > judgeReasonMaxChars {
		// A reason over the 200-character bound is a malformed reply
		// (unavailable), not a truncated record (the CRD's MaxLength=200
		// would reject the status write otherwise).
		r.recordJudgeUnavailable(loop, "malformed judge verdict: reason exceeds 200 characters")
		return
	}
	status := &coxv1alpha1.JudgeStatus{
		Verdict:   v.Verdict,
		Score:     v.Score,
		Reason:    v.Reason,
		Truncated: v.Truncated,
		At:        ptrJudgeTime(),
	}
	loop.Status.Judge = status
	loop.Status.JudgeSummary = judgeSummary(loop)
	cond := metav1.ConditionFalse
	if *v.Verdict {
		cond = metav1.ConditionTrue
	}
	setCondition(loop, coxv1alpha1.JudgeVerdictCondition, cond, judgeVerdictedReason,
		"judge verdict recorded (advisory, ADR-0011): verdict "+boolString(*v.Verdict)+", score "+int32String(v.Score))
	if r.Recorder != nil {
		r.Recorder.Eventf(loop, corev1.EventTypeNormal, judgeVerdictedReason,
			"judge verdict: %s (score %d): %s", boolString(*v.Verdict), v.Score, v.Reason)
	}
}

// recordJudgeUnavailable records the Judge's unavailability (D124):
// status.judge {Verdict nil, Reason=cause, At} + JudgeVerdict=Unknown
// (reason JudgeUnavailable) + a Warning Event. The Loop proceeds exactly as
// if the Judge had not run — Unknown is never read as a pass or a fail.
func (r *LoopReconciler) recordJudgeUnavailable(loop *coxv1alpha1.Loop, cause string) {
	loop.Status.Judge = &coxv1alpha1.JudgeStatus{
		Reason: truncateJudgeReason(cause),
		At:     ptrJudgeTime(),
	}
	loop.Status.JudgeSummary = judgeSummary(loop)
	setCondition(loop, coxv1alpha1.JudgeVerdictCondition, metav1.ConditionUnknown,
		coxv1alpha1.ReasonJudgeUnavailable, "judge unavailable: "+cause)
	if r.Recorder != nil {
		r.Recorder.Eventf(loop, corev1.EventTypeWarning, coxv1alpha1.ReasonJudgeUnavailable,
			"judge unavailable, the Loop proceeds: %s", cause)
	}
}

// releaseJudgeHold releases the Judging hold (Verifying=True reason Judging).
// Delivery enabled -> the Loop moves to the existing delivery hold
// (Verifying=True reason Delivering) so the deliver Job runs and deliverDraft
// sees status.judge; otherwise Succeeded exactly as applyVerifySucceeded's
// non-delivery branch (phase + desiredPhase + bumpLoopCompletion).
func (r *LoopReconciler) releaseJudgeHold(loop *coxv1alpha1.Loop, changed *bool) {
	if deliveryEnabled(loop) {
		// The delivery hold takes over (P5f): the Delivered condition is
		// terminal-checked by settleDeliveryHold; status.judge rides along
		// (deliverDraft reads it for the draft + the PR description).
		setCondition(loop, string(coxv1alpha1.LoopPhaseVerifying), metav1.ConditionTrue,
			coxv1alpha1.ReasonVerifyingDelivering,
			"verify passed; judge settled; holding Succeeded until delivery (Delivered condition) is terminal")
		*changed = true
		return
	}
	setCondition(loop, string(coxv1alpha1.LoopPhaseVerifying), metav1.ConditionFalse,
		judgeVerdictedReason,
		"verify passed and the judge is settled (no delivery): the Loop is Succeeded")
	loop.Status.Phase = coxv1alpha1.LoopPhaseSucceeded
	loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseSucceeded
	bumpLoopCompletion("succeeded", "verify_passed")
	*changed = true
}

// judgeContainerName is the Judge Job's judge container's name (the
// termination-message read keys on it).
const judgeContainerName = "judge"

// judgeReasonMaxChars is the Judge verdict's one-line reason bound (the
// status.judge.reason MaxLength, loop_types.go). A longer reason is a
// malformed reply (unavailable).
const judgeReasonMaxChars = 200

// judgeSummary is the one-word Judge signal for `kubectl get loop` (D131,
// D138): the most serious of the Judge's signals wins (the S1 diff scan sets
// `Suspicious` itself, then False > Truncated > Unknown > True); empty when
// there is no Judge. The JudgeVerdict condition keeps the detail.
func judgeSummary(loop *coxv1alpha1.Loop) string {
	if loop == nil || loop.Status.Judge == nil {
		return ""
	}
	j := loop.Status.Judge
	switch {
	case j.Verdict != nil && !*j.Verdict:
		return "False"
	case j.Truncated:
		return "Truncated"
	case j.Verdict == nil:
		return "Unknown"
	default:
		return "True"
	}
}

// judgeVerdictedReason is the reason on the JudgeVerdict condition when the
// verdict was recorded (True or False). Distinct from the hold reason
// "Judging" (the Verifying condition while the Job runs) and from
// JudgeUnavailable (the Unknown case, D124).
const judgeVerdictedReason = "Judged"

func ptrJudgeTime() *metav1.Time {
	t := metav1.NewTime(time.Now())
	return &t
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func int32String(i int32) string {
	return fmt.Sprintf("%d", i)
}

// truncateJudgeReason bounds the unavailable-cause text to the 200-character
// reason field (the cause can be a long build error; the record must fit).
func truncateJudgeReason(s string) string {
	if len(s) <= judgeReasonMaxChars {
		return s
	}
	return s[:judgeReasonMaxChars-1] + "…"
}
