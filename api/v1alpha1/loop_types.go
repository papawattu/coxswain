/*
Copyright 2026 papawattu.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// LoopPhase is the phase of a Loop's state machine.
// The full transition table is defined in docs/PLAN.md (Phase 1 implements it).
// +kubebuilder:validation:Enum=Pending;Planning;AwaitingApproval;Implementing;Verifying;Succeeded;Failed;Paused;CleaningUp
type LoopPhase string

const (
	LoopPhasePending          LoopPhase = "Pending"
	LoopPhasePlanning         LoopPhase = "Planning"
	LoopPhaseAwaitingApproval LoopPhase = "AwaitingApproval"
	LoopPhaseImplementing     LoopPhase = "Implementing"
	LoopPhaseVerifying        LoopPhase = "Verifying"
	// LoopPhaseSucceeded is the terminal success phase: a Verify result of
	// "pass". No more phase-machine work happens for this Loop (the one-shot
	// fork is done; no re-tasking - ADR-0003). The agent's workspace artifact
	// is the product; with spec.delivery.mode PullRequest, the deliver Job
	// then pushes the verified commit and opens the PR (S6) and records it in
	// status.delivery + the Delivered condition.
	LoopPhaseSucceeded  LoopPhase = "Succeeded"
	LoopPhaseFailed     LoopPhase = "Failed"
	LoopPhasePaused     LoopPhase = "Paused"
	LoopPhaseCleaningUp LoopPhase = "CleaningUp"
)

// DeliveredCondition is the S6 delivery condition type (phase Succeeded +
// spec.delivery.mode == PullRequest only). True + reason Delivered = the
// verified commit is pushed and the PR is open (see
// status.delivery). False + reason DeliveryFailed = the deliver Job
// definitively failed (init/push container failed); the deliver Job is not
// retried (one Job per verifiedCommit, backoffLimit 0) - the operator
// re-runs delivery by clearing status.delivery + the condition. False +
// reason InProgress = the deliver Job has not terminated yet. No condition
// (absent) = delivery not requested (mode None / unset).
const DeliveredCondition = "Delivered"

// Delivery condition reasons.
const (
	// ReasonDelivered: the verified commit is pushed and the PR is open.
	ReasonDelivered = "Delivered"
	// ReasonDeliveryInProgress: the deliver Job has not terminated yet.
	ReasonDeliveryInProgress = "InProgress"
	// ReasonDeliveryFailed: the deliver Job definitively failed (an init or
	// the push container exited non-zero, or the push container reported a
	// rejection such as "refusing to push to the base branch").
	ReasonDeliveryFailed = "DeliveryFailed"
)

// CheckpointPolicy is how hard a Loop requires volume-snapshot checkpoints
// (D59, PLAN-BETA B1; PLAN.md Phase 3 says checkpoints are mandatory, but
// D59's owner decision makes that opt-in so installs on clusters without a
// snapshot CSI — including default kind — are not blocked).
type CheckpointPolicy string

const (
	// CheckpointPolicyBestEffort is the default: take a VolumeSnapshot of the
	// workspace PVC after each successful verify when the cluster can (the
	// VolumeSnapshot CRD exists and the PVC's CSI driver has a
	// VolumeSnapshotClass). When it cannot (no CRD, or no VolumeSnapshotClass
	// for the PVC's CSI driver), the operator sets
	// CheckpointsAvailable=False with the named reason and carries on with the
	// A2 PVC-resume (the workspace PVC itself survives controller/sandbox
	// restarts). The condition is a VISIBLE fallback (I90: fallbacks surface
	// their failure before they degrade), never a silent one.
	CheckpointPolicyBestEffort CheckpointPolicy = "BestEffort"
	// CheckpointPolicyRequired fails the Loop at start (terminal
	// Failed:CheckpointUnavailable) when checkpoints are not possible (no
	// VolumeSnapshot CRD, or no VolumeSnapshotClass for the PVC's CSI driver).
	// This is PLAN.md Phase 3's mandatory-checkpoint semantics, applied only
	// to the Loops that ask for it.
	CheckpointPolicyRequired CheckpointPolicy = "Required"
)

// Workspace defines where a Loop's code comes from and how it is
// authenticated. The sandbox clones repo at ref on start and works on a
// single branch per Loop (docs/CONTEXT.md: "Workspace").
type Workspace struct {
	// repo is the git URL to clone. Accepts HTTPS (`https://…`), plain HTTP
	// (`http://…`, for in-cluster git servers such as a kind Gitea dev
	// fixture), SSH (`ssh://…`), and scp-style (`user@host:path`) remotes —
	// the init container's git client handles all four. Must be non-empty and
	// match the pattern.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^(https://|http://|ssh://|[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:).+`
	Repo string `json:"repo"`

	// ref is the branch, tag, or commit to check out. Defaults to the repo's
	// default branch when empty.
	// +optional
	Ref string `json:"ref,omitempty"`

	// gitCredentialSecret is the name of a Secret in the Loop's namespace
	// holding git credentials. It MUST be a standard kubernetes.io/basic-auth
	// Secret carrying the keys 'username' and 'password' (the same shape as the
	// samples 'samples-git-cred'); the workspace init container mounts ONLY
	// those two keys and passes them to the fetch as a Basic-auth
	// http.extraHeader (nothing is written or persisted).
	// +optional
	GitCredentialSecret string `json:"gitCredentialSecret,omitempty"`
}

// VerifyConfig declares the acceptance checks — the only gate to Succeeded.
// In Phase 0 these are recorded but not executed (execution lands in Phase 1).
type VerifyConfig struct {
	// acceptanceChecks are user-authored shell commands run in the workspace.
	// Exit code 0 is a pass. These are the ONLY gate that can flip a Loop to
	// Succeeded; their source files are protected paths.
	// +listType=atomic
	// +optional
	AcceptanceChecks []string `json:"acceptanceChecks,omitempty"`

	// preset selects the per-language protected-path glob set used by the
	// TamperedVerify glob diff (ADR-0005 D16). The operator cannot infer the
	// repo's language, so the preset is explicit. "go" (the default) expands to
	// **/*_test.go, **/testdata/**, go.mod, go.sum; "none" uses only
	// protectedPaths; "protectedPathsOverride" or preset none replaces the set
	// entirely.
	// +kubebuilder:validation:Enum=go;none
	// +kubebuilder:default=go
	// +optional
	Preset string `json:"preset,omitempty"`

	// protectedPaths are glob patterns of paths the agent must not change
	// (ADR-0005 D10). They ADD to the preset's glob set; a change to any of
	// them between baseCommit and verifiedCommit is TamperedVerify (terminal).
	// A check that calls `make` should list `Makefile` here explicitly.
	// +listType=atomic
	// +optional
	ProtectedPaths []string `json:"protectedPaths,omitempty"`

	// protectedPathsOverride, when true, replaces the preset's glob set with
	// only protectedPaths (ADR-0005 D16). Equivalent to preset: none.
	// +optional
	ProtectedPathsOverride bool `json:"protectedPathsOverride,omitempty"`

	// image is the container image the check-* acceptance-check containers
	// run (S5a: the acceptance checks are user commands that may need a
	// toolchain — `go test` needs a Go image, not the operator's git image).
	// It is separate from the trusted git image the clone-base / import-agent /
	// tamper containers run, which never runs user commands. The image MUST
	// carry a POSIX shell (the checks run via /bin/sh -c), safe.directory
	// support is carried by env, not the image. When empty, the operator uses
	// its default check image (the controller's --verify-image flag when set,
	// else a built-in Go image so `go build` / `go test` checks work out of
	// the box).
	// +optional
	Image string `json:"image,omitempty"`
}

// VerifyStatus is the operator's record of the last verify run's evidence
// (ADR-0005 D14). The values are kubelet-recorded container exit codes from
// the verify Job's pod, never result.json claims.
//
// Integrity invariant (ADR-0005 D12): only the manager role may write
// loops/status; it holds this verify evidence. The scaffolded loop_editor_role
// and loop_admin_role grant loops/status `get` only, so today nothing but the
// operator can set these fields. Keep it that way.
type VerifyStatus struct {
	// verifiedCommit is the SHA the evidence was measured at (ADR-0005 D11). The
	// operator records it from the branch head it resolved itself at Verifying
	// start. Evidence that names a different verifiedCommit (e.g. left over from a
	// previous iteration's Job) is treated as no evidence (D24).
	// +optional
	VerifiedCommit string `json:"verifiedCommit,omitempty"`

	// jobName is the verify Job the evidence came from (D24). Recording it (and
	// the attempt) means evidence from a previous Job cannot be reused for the
	// current verifiedCommit.
	// +optional
	JobName string `json:"jobName,omitempty"`

	// iteration is the Loop iteration the verify ran at (recorded when the
	// verify outcome is applied). The checkpoint for this verify is named
	// checkpoint-<loop>-iter-<iteration>. The operator records it so the
	// checkpoint step can key off the recorded outcome (not the current
	// iteration, which may have been incremented on the iterate path).
	// nil means no completed verify has been recorded yet (iteration 0 is a
	// valid recorded value, so an int with omitempty would be ambiguous).
	// +optional
	Iteration *int32 `json:"iteration,omitempty"`

	// tamperExitCode is the tamper-check init container's exit code. A NON-NIL
	// value means the operator copied it from a TERMINATED tamper init container
	// (0 = clean, non-zero = a protected path differs between baseCommit and
	// verifiedCommit). nil means no evidence (never ran, Job crashed before the
	// tamper container finished, or status was lost) — and nil is NEVER treated
	// as clean (D24 fail-closed). Only a value from a terminated container for
	// the current verifiedCommit counts.
	// +optional
	TamperExitCode *int32 `json:"tamperExitCode,omitempty"`

	// lastCheckResults carries the per-check exit codes from the last verify run
	// (one entry per acceptance check, in order) to feed forward (B3). A nil entry
	// means that check did not run (I14 NotRun); a non-nil 0 means it ran and
	// passed. B3 distinguishes the two.
	//
	// The operator records a still-running or never-run check as the -1 sentinel
	// (the API server rejects a nil element): -1 means "no exit code yet" — the
	// check is still running, or it was not in the pod status at all — and is NOT
	// a pass. The B1 milestone check treats any non-zero entry (including -1) as
	// "the verify did not pass", so a -1 entry never flips a checkpoint into a
	// milestone.
	// +listType=atomic
	// +optional
	LastCheckResults []*int32 `json:"lastCheckResults,omitempty"`

	// infraAttempts is the number of verify Job attempts the operator has made
	// for this verifiedCommit after a verify INFRASTRUCTURE failure (I65):
	// a trusted init (clone-base / import-agent) terminated non-zero, or the
	// Job went Failed with no check ever running. It is a per-verified-commit
	// attempt count, exactly like tamperExitCode and lastCheckResults: it
	// rides the pin's evidence record and is NOT an iteration (an infra failure
	// says nothing about the agent's work and must not burn a
	// maxIterations cycle). The operator recreates the Job a bounded number of
	// times (the verifyInfraRetries cap, the controller's constant) and fails
	// the Loop with the VerifyInfraFailed reason once the bound is exhausted.
	//
	// I65 P1-A: it counts at most ONE attempt per Job UID (see infraJobUID).
	// The operator's infra-retry deletes the failed Job (background
	// propagation) and recreates it with the SAME NAME on the next reconcile;
	// on a real cluster the old Job's pod lingers until the GC removes it,
	// and without the UID guard every reconcile would re-read that stale
	// failed pod and burn another attempt (one flake burning every retry
	// before a single retried Job runs).
	// +kubebuilder:validation:Minimum=0
	// +optional
	InfraAttempts int32 `json:"infraAttempts,omitempty"`

	// infraJobUID is the UID of the verify Job whose infra-failure was most
	// recently counted into infraAttempts (I65 P1-A). It is the "one attempt
	// per Job UID" guard: the operator increments infraAttempts only when the
	// failing Job's UID differs from the one already recorded here. A
	// recreated Job has a new UID (the operator's infra-retry deletes the
	// failed Job and ensureVerifyJob creates a fresh one), so the first
	// infra-failure on the new Job is counted (the stale pod of the old Job
	// does not re-count), while a re-read of the SAME failing Job (the pod
	// lingering, the reconcile re-running) does not double-count. An empty
	// value means no attempt has been counted yet.
	// +optional
	InfraJobUID string `json:"infraJobUID,omitempty"`
}

// CurrentVerifyStatus is the operator's pin of the current iteration's verified
// commit (D11/D27). The operator writes verifiedCommit on entering Verifying
// (resolved from the branch head it read itself, never from the agent), and the
// verify evidence must name the same commit to be accepted. A mismatch or an
// empty pin makes the evidence Unknown (fail-closed).
type CurrentVerifyStatus struct {
	// verifiedCommit is the SHA the operator resolved from the Loop branch head
	// at Verifying start and pinned for this iteration (D11). The verify Job
	// checks out exactly this SHA, and the evidence's verifiedCommit must equal
	// it for the evidence to count.
	// +optional
	VerifiedCommit string `json:"verifiedCommit,omitempty"`
}

// PolicyStatus is the operator's record of the effective AgentPolicy for the
// agent (ADR-0007 Q2). It is set by the operator only; the agent has no write
// path to it.
type PolicyStatus struct {
	// effectiveHash is the SHA-256 of the canonical effective allows (the union
	// of the allows across every AgentPolicy the Loop references, with the
	// platform-minimum endpoints). It lets the decision audit show what the agent
	// was allowed to do in each iteration (D32). Empty when no policy is applied.
	// +optional
	EffectiveHash string `json:"effectiveHash,omitempty"`

	// kaptSpecHash is the SHA-256 of the canonical JSON of the rendered
	// KubeArmorPolicy spec (the agent Kapt). The operator records it when it
	// applies the policy; the static enforcement check (A1) recomputes it from
	// the live policy and compares, so a tampered or stale spec is detected
	// (D52 fact 3). Empty when no policy is applied.
	// +optional
	KaptSpecHash string `json:"kaptSpecHash,omitempty"`

	// blockedCount is the number of actions the engine has blocked for this Loop
	// (ADR-0007 Q5 / C8). It is derived by the operator from the engine's alert
	// stream; a relay outage must surface as "unknown", not zero (I32). +optional
	BlockedCount *int64 `json:"blockedCount,omitempty"`
}

// LoopSettings holds the loop-level knobs.
type LoopSettings struct {
	// maxIterations caps the plan→implement→verify cycles.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=10
	MaxIterations int `json:"maxIterations,omitempty"`

	// stallAfter is the number of CONSECUTIVE verify-failure iterations (with
	// identical normalised failing output, P2e) before the stall detector
	// fires. nil or 0 reads as the PLAN.md default of 3 (the CRD does not
	// default it; the operator's effectiveStallConfig does — P2c).
	// +kubebuilder:validation:Minimum=1
	// +optional
	StallAfter *int32 `json:"stallAfter,omitempty"`

	// stallAction is the action taken when the stall detector fires.
	// +kubebuilder:validation:Enum=Fail;Pause;Continue
	// +kubebuilder:default=Fail
	// +optional
	StallAction StallAction `json:"stallAction,omitempty"`
}

// StallAction is the action the stall detector takes when it fires (P2c/P2e).
// +kubebuilder:validation:Enum=Fail;Pause;Continue
type StallAction string

const (
	// StallActionFail fails the Loop (phase Failed, reason Stalled).
	StallActionFail StallAction = "Fail"
	// StallActionPause pauses the Loop (phase Paused, pausedReason Stall).
	StallActionPause StallAction = "Pause"
	// StallActionContinue keeps the Loop running (the Stalled condition is
	// True, the phase is unchanged).
	StallActionContinue StallAction = "Continue"
)

// BudgetExceededAction is the action taken when any spec.budget cap is hit
// (P2c/P2d).
// +kubebuilder:validation:Enum=Pause;Fail
type BudgetExceededAction string

const (
	// BudgetExceededActionPause pauses the Loop (phase Paused,
	// pausedReason Budget).
	BudgetExceededActionPause BudgetExceededAction = "Pause"
	// BudgetExceededActionFail fails the Loop (phase Failed, reason
	// BudgetExceeded).
	BudgetExceededActionFail BudgetExceededAction = "Fail"
)

// DecimalString is a decimal number as a string (e.g. "0.0021"), used for
// budget money and price values where a float would lose or add precision.
type DecimalString string

// ModelPrices is the per-Loop override of the cluster-wide model prices
// (P2c, item 11). Precedence for the cost derivation (P2d): the Loop's
// modelPrices > the coxswain-model-prices ConfigMap > (no prices → the cost
// cap is inert, fail-closed).
type ModelPrices struct {
	// promptUsdPerMtok is the prompt price in USD per million tokens
	// (decimal string, e.g. "0.30").
	// +kubebuilder:validation:Pattern=`^\d+(\.\d+)?$`
	// +kubebuilder:validation:MinLength=1
	// +optional
	PromptUsdPerMtok DecimalString `json:"promptUsdPerMtok,omitempty"`

	// completionUsdPerMtok is the completion price in USD per million tokens
	// (decimal string, e.g. "1.20").
	// +kubebuilder:validation:Pattern=`^\d+(\.\d+)?$`
	// +kubebuilder:validation:MinLength=1
	// +optional
	CompletionUsdPerMtok DecimalString `json:"completionUsdPerMtok,omitempty"`
}

// BudgetConfig is the Loop's token/time/cost budget (P2c). All caps are
// optional; the operator's budget decision (P2d) only evaluates caps that
// are set.
type BudgetConfig struct {
	// maxTokens is the total prompt + completion tokens across the Loop's
	// life. nil means no token cap.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxTokens *int64 `json:"maxTokens,omitempty"`

	// maxWallClock is the accumulated ACTIVE time cap as a Go duration
	// string (e.g. "1h30m"). It does not count time spent Paused (the
	// budget clock stops while the Loop is paused; see P2d's
	// status.budget.activeSeconds accumulation). nil means no wall-clock
	// cap. The pattern admits only Go duration parts (no commas, no signs,
	// no bare numbers — the operator seam's ParseMaxWallClock is the final
	// gate and rejects sub-second caps).
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h))+$`
	// +optional
	MaxWallClock string `json:"maxWallClock,omitempty"`

	// maxCostUsd is the derived-cost cap in USD as a decimal string (e.g.
	// "0.50"). The cost is derived (P2d) from the token counts and the
	// prices, not measured. nil means no cost cap.
	// +kubebuilder:validation:Pattern=`^\d+(\.\d+)?$`
	// +kubebuilder:validation:MinLength=1
	// +optional
	MaxCostUsd DecimalString `json:"maxCostUsd,omitempty"`

	// onExceeded is the action when ANY cap is hit.
	// +kubebuilder:validation:Enum=Pause;Fail
	// +kubebuilder:default=Pause
	// +optional
	OnExceeded BudgetExceededAction `json:"onExceeded,omitempty"`

	// modelPrices is the per-Loop override of the cluster-wide
	// coxswain-model-prices ConfigMap (item 11). Precedence: this field
	// > the ConfigMap > (no prices → the cost cap is inert, fail-closed).
	// +optional
	ModelPrices *ModelPrices `json:"modelPrices,omitempty"`
}

// LoopSpec defines the desired state of a Loop.
// AgentConfig selects the agent image and model for a Loop's sandbox.
// The agent holds no credentials (ADR-0006): endpointSecretRef holds the model
// base URL + API key and is mounted only into the model-proxy sidecar; the
// agent container is hardened (no SA token automount, runAsNonRoot, drop all
// caps, seccomp, read-only rootfs) and talks to the model over localhost.
//
// +kubebuilder:validation:XValidation:rule="has(self.endpointSecretRef) == has(self.modelEndpoint)",message="endpointSecretRef and modelEndpoint must be set together (a Loop with a model Secret must also name the model endpoint)"
type AgentConfig struct {
	// image is the agent container image (e.g. the reference conformance runner
	// or an adapter for an external agent). Required when agent is set.
	// +optional
	Image string `json:"image,omitempty"`

	// model is the model name/identifier the proxy uses when calling the endpoint.
	// +optional
	Model string `json:"model,omitempty"`

	// endpointSecretRef is the name of a Secret in the Loop's namespace holding
	// the model base URL + API key. It is mounted only into the proxy pod
	// (D33), never the agent container.
	//
	// Immutable (P2, R15 review on PR #12): the proxy is a bare Pod and
	// spec.volumes is immutable, so changing this field after creation would
	// make CreateOrUpdate's Update fail. The operator rejects the change at
	// admission; a new secret requires a new Loop.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="endpointSecretRef is immutable (the proxy pod's spec.volumes cannot change)"
	EndpointSecretRef string `json:"endpointSecretRef,omitempty"`

	// modelEndpoint is the model server's in-cluster address (host:port,
	// e.g. "vllm:8000"). It is NOT secret — only the API key is. D34 uses it
	// to build the proxy pod's NetworkPolicy egress rule (NetworkPolicy
	// cannot match DNS names, only pod/namespace selectors or IP blocks).
	// Immutable for the same reason as endpointSecretRef: it changes the
	// proxy NetworkPolicy, and the D33 spec-hash contract keeps things
	// simple by not allowing post-creation changes.
	//
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == oldSelf || self == ''",message="modelEndpoint is immutable"
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern="^[a-z0-9]([a-z0-9.-]*[a-z0-9])?:[0-9]{1,5}$"
	// +kubebuilder:validation:XValidation:rule="self == '' || int(self.split(':')[1]) > 0 && int(self.split(':')[1]) <= 65535",message="modelEndpoint port must be 1-65535"
	ModelEndpoint string `json:"modelEndpoint,omitempty"`

	// env carries literal-only environment variables for the agent container
	// (I34: a valueFrom/secretKeyRef form is not expressible here, so a Loop
	// author cannot inject a Secret into the agent). Names are limited to
	// avoid the platform-owned COX_* namespace (the operator sets
	// COX_MODEL_BASE_URL in C2; a Loop must not point the agent past the proxy).
	// +kubebuilder:validation:MaxItems=64
	// P3 (R13): a map list keyed on name so the API server rejects duplicate env
	// names (an atomic list silently let [{FOO,a},{FOO,b}] through and the
	// kubelet kept the last one).
	// +listType=map
	// +listMapKey=name
	// +optional
	Env []AgentEnvVar `json:"env,omitempty"`
}

// AgentEnvVar is a literal-only environment variable for the agent container
// (I34, REVIEW-PHASE1-R10). It deliberately does NOT carry the
// corev1.EnvVar's valueFrom field, so it cannot reference a Secret or ConfigMap
// — a Loop author cannot thereby inject a credential into the untrusted agent
// (ADR-0006). Names must not start with the platform-owned COX_ prefix (the
// operator sets COX_MODEL_BASE_URL in C2; a Loop must not point the agent past
// the proxy). Enforced by a CEL rule because Kubernetes structural schemas use
// RE2 (no negative lookahead) and prune unknown fields rather than rejecting
// them.
type AgentEnvVar struct {
	// name of the environment variable.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:XValidation:rule="!self.startsWith('COX_')",message="agent env names must not use the platform-owned COX_ prefix (I34)"
	Name string `json:"name"`

	// value is the literal value.
	// +kubebuilder:validation:MaxLength=16384
	// +optional
	Value string `json:"value,omitempty"`
}

// A Loop is one-shot and single-goal (docs/adr/0003).
type LoopSpec struct {
	// goal is the natural-language objective for this Loop.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Goal string `json:"goal"`

	// workspace is where the code comes from and how it is authenticated.
	// +kubebuilder:validation:Required
	Workspace Workspace `json:"workspace"`

	// verify declares the acceptance checks (the only gate to Succeeded).
	// +optional
	Verify VerifyConfig `json:"verify,omitempty"`

	// agent selects the agent image and model the sandbox runs. The agent holds
	// no credentials (ADR-0006): the model key in endpointSecretRef is mounted
	// only into the proxy sidecar, and the agent talks to the model through
	// localhost. The agent container is hardened by the operator (C1).
	// +optional
	Agent AgentConfig `json:"agent,omitempty"`

	// Delivery configures what happens to a SUCCESSFUL, VERIFIED Loop
	// (phase Succeeded): mode None (default) leaves the verified commit on
	// the agent's workspace PVC; mode PullRequest pushes the verified commit
	// to spec.workspace.repo on branch <branchPrefix><loop-name> and opens a
	// pull request (draft by default). S6.
	// +optional
	Delivery *DeliveryConfig `json:"delivery,omitempty"`

	// policyRefs is the list of AgentPolicy names (in the Loop's namespace) whose
	// allows the agent may use. The effective policy is the union of their allows
	// (ADR-0007 Q2). With no policyRefs the agent runs default-deny (the platform
	// minimum only). +optional
	PolicyRefs []string `json:"policyRefs,omitempty"`

	// loop holds iteration and phase-timeout settings.
	// +optional
	// +kubebuilder:default={}
	Loop LoopSettings `json:"loop,omitempty"`

	// suspend, when true, pauses the Loop at its current phase.
	// +optional
	Suspend bool `json:"suspend,omitempty"`

	// budget caps the Loop's token, active-time and derived-cost totals
	// (P2c); P2d applies onExceeded when any cap is hit. nil means no
	// budget caps.
	// +optional
	Budget *BudgetConfig `json:"budget,omitempty"`

	// checkpoints selects the checkpoint policy (D59, PLAN-BETA B1). An
	// empty value reads as BestEffort (the CRD default): after each
	// successful verify the operator takes a VolumeSnapshot of the workspace
	// PVC named checkpoint-<loop>-iter-<N> when the cluster can, and
	// otherwise sets CheckpointsAvailable=False and carries on with the A2
	// PVC-resume. Required makes checkpoints mandatory: without a
	// VolumeSnapshot CRD or a VolumeSnapshotClass for the PVC's CSI driver
	// the Loop fails at start (terminal Failed:CheckpointUnavailable).
	// +kubebuilder:validation:Enum=BestEffort;Required
	// +kubebuilder:default=BestEffort
	// +optional
	Checkpoints CheckpointPolicy `json:"checkpoints,omitempty"`

	// restoreFrom is the ROLLBACK target (B2, PLAN-BETA / PLAN.md Phase 3):
	// "iter-N" names the checkpoint checkpoint-<loop>-iter-<N> the operator
	// restores the workspace PVC from. The checkpoint must EXIST (a recorded
	// status.checkpoints entry whose VolumeSnapshot is present) and be
	// ReadyToUse (status.boundSnapshotVolumeReadyToUse of the snapshot is
	// true); an unknown or not-ready iter is rejected VISIBLY (a
	// RestoreRejected condition plus an Event, never a silent ignore). The
	// operator deletes and recreates the workspace PVC with
	// spec.dataSource = the VolumeSnapshot, then resumes the A2 path from
	// the restored content. Immutable (PVC dataSource is immutable): a
	// change is rejected at admission; a different rollback target is a
	// fork.
	// +kubebuilder:validation:Pattern=`^iter-[0-9]+$`
	// +optional
	RestoreFrom string `json:"restoreFrom,omitempty"`

	// forkOf seeds this Loop from ANOTHER Loop's checkpoint (B2, PLAN-BETA /
	// PLAN.md Phase 3; ADR-0003: a fork, not a re-task). forkOf.loop is the
	// source Loop's name (any namespace; the operator reads the source
	// snapshot by name in THIS Loop's namespace) and forkOf.iteration names
	// the source checkpoint iteration. At start the operator restores this
	// Loop's OWN workspace PVC from the source snapshot
	// checkpoint-<loop>-iter-<N> (PVC spec.dataSource = the VolumeSnapshot);
	// the fork then runs a normal one-shot Loop on that content: its own
	// branch, its own deliver, its own PR. A fork is a one-shot Loop
	// (ADR-0003: forks, not re-tasks). The source Loop may be deleted after
	// the fork's restore: a restored PVC owns its content, not the source
	// snapshot, so the fork is independent (the source's snapshot GC is
	// documented and tested — the source's finalizer still deletes its
	// snapshots in CleaningUp; the fork's PVC survives that delete because
	// the PVC content was materialised at restore).
	// Immutable (a fork target is a seeding decision made at creation).
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="forkOf is immutable (it seeds the workspace PVC from the source snapshot at creation)"
	ForkOf *ForkOf `json:"forkOf,omitempty"`
}

// ForkOf names the checkpoint a new Loop is seeded from (B2). The fork
// restores its own workspace PVC from the source snapshot and then runs
// independently (own branch, own deliver, own PR; ADR-0003 one-shot).
type ForkOf struct {
	// loop is the source Loop's name (the checkpoint name is
	// checkpoint-<loop>-iter-<iteration>). Same namespace as the fork.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Loop string `json:"loop"`

	// iteration is the source checkpoint's iteration (the
	// checkpoint-<loop>-iter-<iteration> name component). Iteration 0 is
	// a valid checkpoint (the first verify; the same lesson as B1's iter-0).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=0
	Iteration int `json:"iteration"`
}

// CheckpointStatus is one VolumeSnapshot the operator took of the Loop's
// workspace PVC after a successful verify (B1, PLAN-BETA). The snapshot
// outlives the sandbox pod and is the durability record behind
// status.checkpoint (the latest).
type CheckpointStatus struct {
	// iteration is the 1-based iteration the checkpoint was taken at (the
	// iteration whose verify just succeeded).
	Iteration int `json:"iteration"`

	// name is the VolumeSnapshot's name: checkpoint-<loop>-iter-<iteration>
	// (B1). It is unique per Loop (iteration numbers are never reused), so
	// the name is the operator's record of what exists on the cluster.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// milestone is true when the checkpoint is retained beyond the
	// last-3 window: a milestone is a checkpoint of a SUCCESSFUL verify
	// (PLAN.md Phase 3: "the last 3 plus all milestone (successful-verify)
	// checkpoints"). In B1 every checkpoint is taken after a successful
	// verify, so every checkpoint is a milestone and nothing is GC'd — the
	// milestone flag is explicit in the API so B2's restore/rollback/fork
	// can distinguish a plain recent checkpoint from one the user is likely
	// to want back.
	Milestone bool `json:"milestone"`

	// at is the operator's record of when the snapshot was taken (the
	// operator's clock, set when the snapshot object is recorded).
	At metav1.Time `json:"at"`
}

// LoopStatus defines the observed state of a Loop.
type LoopStatus struct {
	// phase is the current phase of the state machine.
	// +kubebuilder:default=Pending
	// +optional
	Phase LoopPhase `json:"phase,omitempty"`

	// iteration is the 1-based count of plan→implement→verify cycles run.
	// +kubebuilder:default=0
	// +optional
	Iteration int `json:"iteration,omitempty"`

	// desiredPhase is the phase the operator has asked the runner to be in.
	// It will also be copied to .coxswain/desired-phase for the runner to read
	// (Phase 1 runner/exec wiring slice, not yet implemented).
	// The operator is the sole writer of Loop status (ADR-0004); the runner
	// never sets this — it only reports observedPhase in result.json.
	// +optional
	DesiredPhase LoopPhase `json:"desiredPhase,omitempty"`

	// observedPhase is the operator's record of the phase the runner reported
	// in result.json. It is the input to the transition table (B1). The runner
	// never writes this field directly (ADR-0004).
	// +optional
	ObservedPhase LoopPhase `json:"observedPhase,omitempty"`

	// baseCommit is the SHA resolved from spec.workspace.ref at Loop start and
	// pinned for the Loop's life (ADR-0005 D10). It is one of the two operator-
	// pinned SHAs the TamperedVerify glob diff compares; the agent has no write
	// path to it. Immutable once set.
	// +optional
	BaseCommit string `json:"baseCommit,omitempty"`

	// verify carries the operator's record of the last verify run's evidence
	// (ADR-0005 D14): the tamper-check container's exit code (0 = clean) and the
	// per-check exit codes. These are kubelet-recorded, not result.json claims;
	// in envtest (no Job controller) the B-slice tests set them directly.
	// +optional
	Verify *VerifyStatus `json:"verify,omitempty"`

	// currentVerify is the operator's pin of the CURRENT iteration's verified
	// commit, written by the operator on entering Verifying (D11) and used to
	// bind the verify evidence to the commit being verified. It is distinct from
	// verify.verifiedCommit (the commit the *evidence* names): a mismatch between
	// the two means the evidence is stale (from a previous iteration's Job or a
	// force-pushed branch) and must be treated as no evidence (D27 fail-closed).
	// +optional
	CurrentVerify *CurrentVerifyStatus `json:"currentVerify,omitempty"`

	// Delivery records the delivery outcome (S6) once the deliver Job
	// completes: the branch, the pushed commit (== the verifiedCommit), and
	// the opened PR's number and URL. nil until delivery has run. Set for
	// phase Succeeded Loops with spec.delivery.mode == PullRequest (one
	// deliver Job per verifiedCommit; the operator re-reads the push
	// container's termination message each reconcile until it is valid or
	// the Job definitively failed). Written by the operator only.
	// +optional
	Delivery *DeliverStatus `json:"delivery,omitempty"`

	// conditions represent the current state of the Loop resource.
	// Each condition has a unique type and reflects the status of a specific
	// aspect of the resource. Status is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// observedGeneration is the spec generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// policy is the operator's record of the effective AgentPolicy for the
	// agent (ADR-0007 Q2): the union of the allows across every AgentPolicy the
	// Loop references (spec.policyRefs[]). effectiveHash is the SHA-256 of the
	// canonical effective allows, so the decision audit shows what the agent was
	// allowed to do (D32); it is set by the operator, never the agent. +optional
	Policy *PolicyStatus `json:"policy,omitempty"`

	// budget is the operator's record of the Loop's accumulated budget
	// consumption (P2c; P2d accumulates it from the model proxy endpoint).
	// nil until the first read.
	// +optional
	Budget *BudgetStatus `json:"budget,omitempty"`

	// stallHistory is the ring of the last 10 verify-failure iterations
	// (one StallEntry per failed verify Job, atomic list, P2c/P2e). The
	// stall detector (P2e) needs only the recent consecutive run.
	// +listType=atomic
	// +optional
	StallHistory []StallEntry `json:"stallHistory,omitempty"`

	// pausedFrom is the phase the Loop left on entering Paused (P2f);
	// cleared on resume.
	// +optional
	PausedFrom LoopPhase `json:"pausedFrom,omitempty"`

	// pausedReason is WHY the Loop is paused (P2c, item 4): spec.suspend
	// resumes only a Suspend pause; a Stall or Budget pause resumes via the
	// coxswain.io/resume annotation (P2f). Cleared on resume.
	// +optional
	PausedReason PausedReason `json:"pausedReason,omitempty"`

	// checkpoint is the operator's record of the LATEST checkpoint taken
	// after a successful verify (B1, PLAN-BETA). nil until the first
	// successful verify has taken one (or is being taken). It is the restore
	// point B2's PVC-recovery path reads. Same shape as the entries of
	// status.checkpoints (the latest entry).
	// +optional
	Checkpoint *CheckpointStatus `json:"checkpoint,omitempty"`

	// checkpoints is the status list of every checkpoint the operator has
	// taken for this Loop (B1, PLAN-BETA). It carries the retention record:
	// the last 3 plus every milestone (successful-verify) checkpoint; the
	// rest are GC'd (their VolumeSnapshots deleted). Written by the operator
	// only; the agent has no write path (ADR-0004).
	// +listType=atomic
	// +optional
	Checkpoints []CheckpointStatus `json:"checkpoints,omitempty"`

	// progress is the operator's structured progress record (R19 OS1),
	// populated from the runner's ADR-0004 claim (the agent container's
	// termination message) plus the operator's own pin. It is a CLAIM, not a
	// gate input (ADR-0005): size-limited and strict-parsed on read, and no
	// gate reads it. Written only when the operator has actually read a claim;
	// nil before the first claim. +optional
	Progress *ProgressStatus `json:"progress,omitempty"`
}

// ProgressStatus is the operator's structured progress record (R19 OS1). The
// fields the runner CANNOT set for itself (lastActivityTime, the generation/
// commit pins) are the operator's own; the rest are copied from the claim
// unchanged (size-limited, strict-parsed, never a gate input — ADR-0005).
type ProgressStatus struct {
	// phase is the phase the claim named (the runner's reported
	// observedPhase, validated against the operator's enum).
	// +optional
	Phase LoopPhase `json:"phase,omitempty"`

	// lastActivityTime is the operator's record of when the last claim arrived
	// (the agent container's finish time, kubelet-recorded — not the runner's
	// clock).
	// +optional
	LastActivityTime *metav1.Time `json:"lastActivityTime,omitempty"`

	// iteration is the iteration the claim named (the runner's record of the
	// .coxswain/iteration it read; the operator's own status.iteration is the
	// authoritative count).
	// +optional
	Iteration int `json:"iteration,omitempty"`

	// lastResultStatus is the claim's status (success/blocked) for the last
	// phase run.
	// +optional
	LastResultStatus string `json:"lastResultStatus,omitempty"`

	// blockedReason is the claim's reason when lastResultStatus is blocked
	// (size-limited on read).
	// +optional
	BlockedReason string `json:"blockedReason,omitempty"`

	// observedGeneration is the spec generation the pin the operator read the
	// claim against (the operator's pin, not the claim's — a claim can never
	// name a generation for itself).
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// baseCommit is the commit pin the operator read the claim against (the
	// status.baseCommit pin, D10 — the claim cannot set it for itself).
	// +optional
	BaseCommit string `json:"baseCommit,omitempty"`
}

// BudgetExceededReason names the cap that was hit (P2c/P2d); empty until
// status.budget.exceeded is true.
// +kubebuilder:validation:Enum=Tokens;WallClock;Cost
type BudgetExceededReason string

const (
	// BudgetExceededTokens: the maxTokens cap was hit.
	BudgetExceededTokens BudgetExceededReason = "Tokens"
	// BudgetExceededWallClock: the maxWallClock cap was hit.
	BudgetExceededWallClock BudgetExceededReason = "WallClock"
	// BudgetExceededCost: the maxCostUsd cap was hit.
	BudgetExceededCost BudgetExceededReason = "Cost"
)

// PausedReason is WHY a Loop is paused (P2c, item 4). It makes resume
// well-defined: spec.suspend=false resumes ONLY a Suspend pause; a Stall or
// Budget pause resumes via the coxswain.io/resume annotation (P2f). The
// owner's escape for an un-raisable budget is a fork (ADR-0003).
// +kubebuilder:validation:Enum=Suspend;Stall;Budget
type PausedReason string

const (
	// PausedReasonSuspend: the operator set spec.suspend=true.
	PausedReasonSuspend PausedReason = "Suspend"
	// PausedReasonStall: the stall detector fired with stallAction=Pause.
	PausedReasonStall PausedReason = "Stall"
	// PausedReasonBudget: a budget cap was hit with onExceeded=Pause.
	PausedReasonBudget PausedReason = "Budget"
)

// BudgetStatus is the operator's record of the Loop's accumulated budget
// consumption (P2c; the counts are accumulated by P2d from the model proxy
// endpoint, ADR-0009). The last* fields carry the last-READ CUMULATIVE
// counter values the operator's delta logic consumes (item 2): when the
// proxy pod is recreated the counters reset, and bootIDChanged flags it.
type BudgetStatus struct {
	// promptTokens is the accumulated prompt-token total across boot-ID
	// changes.
	// +optional
	PromptTokens int64 `json:"promptTokens,omitempty"`

	// completionTokens is the accumulated completion-token total across
	// boot-ID changes.
	// +optional
	CompletionTokens int64 `json:"completionTokens,omitempty"`

	// requests is the accumulated number of metered model requests.
	// +optional
	Requests int64 `json:"requests,omitempty"`

	// unmeteredRequests is the CUMULATIVE count of requests the proxy could
	// not meter (item 3: a count, not a last-request flag).
	// +optional
	UnmeteredRequests int64 `json:"unmeteredRequests,omitempty"`

	// costUsd is the derived cost in USD as a decimal string (P2d: derived
	// from the token counts and the prices, never measured).
	// +optional
	CostUsd string `json:"costUsd,omitempty"`

	// activeSeconds is the accumulated ACTIVE time in seconds (item 10).
	// Time spent Paused is not counted: the accumulation stops while the
	// Loop is paused (P2d) and lastActiveStamp is reset to now on resume
	// (P2f) so the first post-resume reconcile does not add the pause's
	// duration.
	// +optional
	ActiveSeconds int64 `json:"activeSeconds,omitempty"`

	// exceeded is true when any spec.budget cap is hit. It is sticky until
	// re-evaluated on resume (item 5 — P2f's resume re-evaluates the
	// current caps against the current counts; raising a cap clears it).
	// +optional
	Exceeded bool `json:"exceeded,omitempty"`

	// exceededReason names the cap that was hit (Tokens|WallClock|Cost);
	// empty until exceeded is true.
	// +optional
	ExceededReason BudgetExceededReason `json:"exceededReason,omitempty"`

	// lastBootID is the proxy bootID the last-read counters came from
	// (item 2). Empty before the first read.
	// +optional
	LastBootID string `json:"lastBootID,omitempty"`

	// lastPromptTokens is the last-READ CUMULATIVE prompt-token counter
	// from the proxy (item 2) — the operator's delta logic consumes it.
	// +optional
	LastPromptTokens int64 `json:"lastPromptTokens,omitempty"`

	// lastCompletionTokens is the last-READ CUMULATIVE completion-token
	// counter from the proxy (item 2).
	// +optional
	LastCompletionTokens int64 `json:"lastCompletionTokens,omitempty"`

	// lastRequests is the last-READ CUMULATIVE metered-request counter from
	// the proxy (item 2).
	// +optional
	LastRequests int64 `json:"lastRequests,omitempty"`

	// lastUnmeteredRequests is the last-READ CUMULATIVE unmetered-request
	// counter from the proxy (item 2).
	// +optional
	LastUnmeteredRequests int64 `json:"lastUnmeteredRequests,omitempty"`

	// bootIDChanged is true (sticky) when a proxy pod recreate wiped the
	// counters between reads (the honest-limit marker, P2a): the
	// accumulated totals jumped back relative to the last-read values.
	// +optional
	BootIDChanged bool `json:"bootIDChanged,omitempty"`

	// lastActiveStamp is the last reconcile's active-time accumulation point
	// (item E). The operator sets it to now on every non-paused reconcile
	// and resets it to now on resume (P2f), so the first post-resume
	// reconcile does not add the pause's duration to activeSeconds.
	// +optional
	LastActiveStamp *metav1.Time `json:"lastActiveStamp,omitempty"`
}

// StallEntry is one verify-failure iteration's record in status.stallHistory
// (P2c/P2e). jobName is the dedup key (item 6): a re-read of the same verify
// Job appends no entry.
type StallEntry struct {
	// iteration is the 1-based iteration number of the failing verify run.
	Iteration int `json:"iteration"`

	// jobName is the verify Job's name (<loop>-verify-<iteration>) — the
	// dedup key (item 6).
	// +kubebuilder:validation:MinLength=1
	JobName string `json:"jobName"`

	// hash is the SHA-256 hex of the normalised failing output (P2e).
	Hash string `json:"hash"`

	// normalisationVersion is the version of the normalisation the hash was
	// computed with (the P2e constant). A change resets the consecutive run
	// (a v1 and a v2 hash of the same output must not be conflated).
	// +kubebuilder:validation:MinLength=1
	NormalisationVersion string `json:"normalisationVersion"`

	// check is the failing check's name.
	Check string `json:"check"`

	// at is the verify Job pod's finish time (kubelet-recorded).
	At metav1.Time `json:"at"`
}

// Loop condition types (the DeliveredCondition pattern).
const (
	// StalledCondition is True when the stall detector has fired (P2e).
	StalledCondition = "Stalled"
	// BudgetExceededCondition is True when status.budget.exceeded is true
	// (P2d).
	BudgetExceededCondition = "BudgetExceeded"
	// PausedCondition is True while the phase is Paused; the message names
	// the pausedReason (P2f).
	PausedCondition = "Paused"
	// ModelAvailableCondition is True while the model backend is reachable
	// (I89). It is set to False (reason=ModelUnavailable) when the runner
	// claim carries ModelUnavailable in its blocked reason — the model
	// backend timed out after bounded retries. It is set back to True when
	// a subsequent claim succeeds (the model recovered).
	ModelAvailableCondition = "ModelAvailable"
	// CheckpointsAvailableCondition (B1, D59) records whether volume-snapshot
	// checkpoints are possible for this Loop. True + reason Available = the
	// VolumeSnapshot CRD exists and the workspace PVC's CSI driver has a
	// VolumeSnapshotClass (checkpoints are taken after each successful
	// verify). False + a named reason (VolumeSnapshotCRDMissing / NoSnapshotClassForDriver,
	// or SnapshotCreateFailed) = checkpoints are NOT being taken; the Loop
	// carries on with the A2 PVC-resume under BestEffort (a VISIBLE fallback,
	// I90), while under Required the Loop is already terminal
	// Failed:CheckpointUnavailable (the gate runs at start, before any
	// phase work). Absent = the check has not run yet (the PVC's
	// provisioner/driver is not known until the PVC has a storageClass).
	CheckpointsAvailableCondition = "CheckpointsAvailable"
	// RestoreCondition (B2, PLAN-BETA) records the workspace PVC's restore
	// state for spec.restoreFrom (rollback) and spec.forkOf (fork seeding).
	// True + reason Restored = the workspace PVC was recreated from the
	// named checkpoint and the Loop resumed (A2 path). False + a named
	// reason = the restore was rejected or has not succeeded yet:
	// RestoreSourceMissing (the named checkpoint's VolumeSnapshot does not
	// exist), RestoreSourceNotReady (the snapshot is not ReadyToUse — the
	// restore is DEFERRED, retried each reconcile, and is NOT a terminal
	// rejection), RestoreRejected (the spec is malformed or the restore
	// failed). Absent = no restore was requested and the PVC was never lost.
	// A PVC lost with no usable checkpoint carries on with the A2 PVC-resume
	// — that is surfaced by the Restore condition False reason
	// NoUsableCheckpoint + the WorkspaceRestoreUnavailable Event (I90).
	RestoreCondition = "Restore"
)

// B2 restore condition reasons (the RestoreCondition's named reasons;
// I90: fallbacks and rejections are named and visible, never silent).
const (
	// RestoreReasonRestored: the workspace PVC was restored from the named
	// checkpoint (the restore completed this Loop's life).
	RestoreReasonRestored = "Restored"
	// RestoreReasonSourceMissing: the named checkpoint's VolumeSnapshot does
	// not exist (the checkpoint was GC'd, or the name/iter is unknown).
	// Terminal: the operator does not retry (the object is gone).
	RestoreReasonSourceMissing = "RestoreSourceMissing"
	// RestoreReasonSourceNotReady: the named checkpoint's VolumeSnapshot
	// exists but is not ReadyToUse yet (the snapshot-controller has not
	// finished). NOT terminal: the operator defers the restore and retries
	// each reconcile (the in-progress state — no restore decision, I49).
	RestoreReasonSourceNotReady = "RestoreSourceNotReady"
	// RestoreReasonRejected: the restore spec is malformed or a restore
	// attempt failed (visible, never a silent ignore).
	RestoreReasonRejected = "RestoreRejected"
	// RestoreReasonNoUsableCheckpoint: the workspace PVC was LOST and no
	// usable checkpoint exists (the latest recorded checkpoint's
	// VolumeSnapshot is absent — GC'd or never taken). The Loop falls back
	// to the A2 PVC-resume (the PVC is recreated EMPTY) — a VISIBLE
	// fallback, not a silent ignore (I90).
	RestoreReasonNoUsableCheckpoint = "NoUsableCheckpoint"
	// RestoreReasonStoppingSandbox: the ROLLBACK restore is in progress —
	// the sandbox was just suspended (a mounted PVC is held by
	// kubernetes.io/pvc-protection until the pod is gone; the A2 kill path
	// stops it the same way). NOT terminal: the operator deletes the PVC on
	// the next reconcile, once the sandbox is stopped, and recreates it from
	// the snapshot (in-progress state, I49).
	RestoreReasonStoppingSandbox = "StoppingSandbox"
	// RestoreReasonStoppingWorkloads: the ROLLBACK restore is in progress —
	// the Loop-owned verify and deliver Jobs are being deleted (the delete is
	// async — the Job controller deletes the Pods; the PVC is held by
	// kubernetes.io/pvc-protection while any Loop-owned pod mounts it; the
	// sandbox is stopped once the Jobs are gone). NOT terminal (in-progress
	// state, I49).
	RestoreReasonStoppingWorkloads = "StoppingWorkloads"
	// RestoreReasonDeletingPVC: the ROLLBACK restore is in progress — the
	// sandbox is stopped and the workspace PVC delete has been issued (the
	// delete is async; the PVC may still be present with a deletionTimestamp)
	// and the PVC is recreated from the snapshot once it is gone. NOT
	// terminal (in-progress state, I49).
	RestoreReasonDeletingPVC = "DeletingPVC"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Iteration",type=integer,JSONPath=`.status.iteration`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// D20: the Loop name names the Sandbox (and its Service, if one is enabled),
// so it must be a DNS-1035 label (lowercase, start with a letter, no dots) of
// at most 55 chars (63 - len("-sandbox")). The per-Sandbox Service is opt-in
// (spec.service: true, D22) and Coxswain does not set it, but a stricter name
// costs nothing and is still required if a Service is ever enabled (e.g. a
// runner health endpoint). Loop names are normally DNS-1123 subdomains
// (dots/digits/253 chars allowed), so this is enforced at admission rather
// than left to a later Service-create failure.
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z]([-a-z0-9]*[a-z0-9])?$') && size(self.metadata.name) <= 55",message="Loop name must be a DNS-1035 label of at most 55 characters (it names the Sandbox, and its Service if one is enabled)"
// S3 (review P1): a plain http:// repo URL may only point at an in-cluster
// host. Plain http sends any git credentials in cleartext (a
// spec.workspace.gitCredentialSecret would leak over the wire), so an
// http://host not ending in the in-cluster service suffixes is rejected at
// admission. https:// and ssh:// are unrestricted. The host is the first
// segment of the path after "http://", up to the first "/"; a port suffix is
// stripped. The in-cluster suffixes are ".svc" (the cluster-internal
// service-domain short form) and ".svc.cluster.local" (the FQDN form); a
// custom clusterDomain would use the latter shape.
// +kubebuilder:validation:XValidation:rule="!has(self.spec.workspace.repo) || !self.spec.workspace.repo.startsWith('http://') || (self.spec.workspace.repo.split('http://')[1].split('/')[0].split(':')[0].endsWith('.svc') || self.spec.workspace.repo.split('http://')[1].split('/')[0].split(':')[0].endsWith('.svc.cluster.local'))",message="workspace.repo: plain http:// is only allowed for in-cluster hosts (ending in .svc or .svc.cluster.local); use https:// for external hosts (git credentials would be sent in cleartext)"

// Loop is the Schema for the loops API.
type Loop struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Loop
	// +required
	Spec LoopSpec `json:"spec"`

	// status defines the observed state of Loop
	// +optional
	Status LoopStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// LoopList contains a list of Loop
type LoopList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Loop `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Loop{}, &LoopList{})
		return nil
	})
}
