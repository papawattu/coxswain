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
	LoopPhaseSucceeded        LoopPhase = "Succeeded"
	LoopPhaseFailed           LoopPhase = "Failed"
	LoopPhasePaused           LoopPhase = "Paused"
	LoopPhaseCleaningUp       LoopPhase = "CleaningUp"
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
	// +listType=atomic
	// +optional
	LastCheckResults []*int32 `json:"lastCheckResults,omitempty"`
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
}

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
