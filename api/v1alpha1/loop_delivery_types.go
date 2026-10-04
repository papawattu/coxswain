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

// S6: delivery. A SUCCESSFUL, VERIFIED Loop (phase Succeeded) can be
// configured to push the verified commit to the workspace repo and open a
// pull request, so the operator's PR pipeline (comment reviews, required
// conversation resolution) is the human gate — the agent never merges and
// never reaches the default branch.
//
// Delivery target: there is deliberately NO separate target-URL field.
// Delivery pushes only to spec.workspace.repo (the repo the Loop cloned and
// the checks ran against); the PR base is spec.delivery.baseBranch
// (default spec.workspace.ref), and the branch is
// spec.delivery.branchPrefix + loop name. The provider (GitHub vs
// Gitea-compatible) is inferred from the repo host: host == "github.com"
// selects the GitHub API (a Gitea-compatible PR API is the fallback for
// every other host).
//
// Credentials: the git clone/push uses the EXISTING workspace.gitCredentialSecret
// (basic auth, exactly as S3's clone-base does). For GitHub, the same Secret
// is reused for the API — the Secret is basic auth with username
// "x-access-token" and password = the token; the API call sends the password
// as a Bearer token. Gitea uses the same basic pair for both git and API.
// The Secret stays operator-side (ADR-0006): it is mounted into the
// deliver Job's clone-base and push containers only, NEVER into
// import-agent (which runs the pinned, trusted work from the agent's
// evidence PVC with no network and no credentials).
package v1alpha1

// DeliveryMode selects how a successful, verified Loop is handed to the
// operator.
// +kubebuilder:validation:Enum=None;PullRequest
type DeliveryMode string

const (
	// DeliveryModeNone: no delivery; the verified commit stays on the agent's
	// workspace PVC. Default (an unset field decodes as "" == None).
	DeliveryModeNone DeliveryMode = "None"
	// DeliveryModePullRequest: push the verified commit to
	// spec.workspace.repo on branch <branchPrefix><loop-name> and open a
	// (by default draft) pull request against spec.delivery.baseBranch.
	DeliveryModePullRequest DeliveryMode = "PullRequest"
)

// DeliveryConfig is the operator's delivery intent (S6).
// +kubebuilder:validation:Optional
type DeliveryConfig struct {
	// Mode selects the delivery path. Default None (a successful Loop
	// without delivery config is exactly today's behavior: the verified
	// commit lives on the agent's workspace PVC).
	// +kubebuilder:validation:Enum=None;PullRequest
	// +kubebuilder:default=None
	// +optional
	Mode DeliveryMode `json:"mode,omitempty"`

	// BranchPrefix is prepended to the Loop name to form the delivery
	// branch name (<prefix><loop-name>). The operator sets the prefix that
	// identifies the Loop's branches; the name part is derived from the
	// Loop. Must be a git branch prefix made of [A-Za-z0-9._/-]; the
	// result must not collide with baseBranch or a default branch (the
	// deliver Job refuses to push on those — see DeliverStatus).
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9._/-]+$`
	// +kubebuilder:default="coxswain/"
	// +optional
	BranchPrefix string `json:"branchPrefix,omitempty"`

	// BaseBranch is the PR base branch (the branch the delivery branch is
	// compared against when the PR is opened). Default: spec.workspace.ref
	// (the ref the workspace clone started from), so a Loop with no
	// baseBranch override opens its PR against the ref it cloned.
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9._/-]+$`
	// +optional
	BaseBranch string `json:"baseBranch,omitempty"`

	// Draft controls whether the opened PR is a draft. Default true: the
	// builder marks it ready explicitly (via the GraphQL
	// markPullRequestReadyForReview path, since the gh CLI `pr ready`
	// command requires the workflow scope the token lacks). How draft is
	// represented is provider-specific: on GitHub the PR is opened as a
	// real draft PR; on Gitea the create-PR API ignores the draft flag, so
	// the draft is marked by prefixing the PR title with "WIP: " (Gitea's
	// draft convention) — the PR is NOT draft in the API's own state, the
	// title prefix is the only marker.
	// +kubebuilder:default=true
	// +optional
	Draft *bool `json:"draft,omitempty"`
}

// DeliverStatus records the operator's delivery outcome (S6). Written by
// the operator ONLY (the runner and the agent never write status; the
// deliver Job reports through its push container's termination message,
// which the operator reads strictly and records here). The status is
// meaningful only for Loops that reached phase Succeeded with
// spec.delivery.mode == PullRequest: the Job is built for exactly one
// verifiedCommit (stamped in an annotation, D27-style), and there is no
// retry loop — one deliver Job per verifiedCommit, backoffLimit 0.
// +kubebuilder:validation:Optional
type DeliverStatus struct {
	// Branch is the delivery branch the verified commit was pushed to
	// (== deliveryBranchName: <spec.delivery.branchPrefix><loop-name>).
	// +optional
	Branch string `json:"branch,omitempty"`
	// Commit is the commit the branch points at. Must equal
	// status.currentVerify.verifiedCommit — the operator asserts this
	// before writing the status (a termination message that reports any
	// other commit is rejected).
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{40}$`
	// +optional
	Commit string `json:"commit,omitempty"`
	// PRNumber is the pull request number the provider assigned.
	// +optional
	PRNumber int64 `json:"prNumber,omitempty"`
	// PRURL is the pull request URL. The operator validates its host
	// before recording it: it must be the workspace repo's host (or, for a
	// GitHub delivery, github.com) — a termination message that reports a
	// PR on a foreign host is rejected.
	// +optional
	PRURL string `json:"prURL,omitempty"`
}
