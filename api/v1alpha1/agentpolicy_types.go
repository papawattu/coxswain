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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AgentPolicySpec is the desired state of an AgentPolicy (ADR-0007 Q2): a
// default-deny, additive-allow policy for the agents a Loop runs. Rules are
// only ever allows; a Loop's effective policy is the union of the allows across
// every AgentPolicy it references (spec.policyRefs[]).
type AgentPolicySpec struct {
	// exec is the commands the agent may run, as ABSOLUTE binary paths (e.g.
	// "/usr/bin/git", "/usr/local/go/bin/go"). An empty list means the agent may
	// run no commands beyond the platform minimum (its own entrypoint).
	//
	// D46 (R20, owner decision (c), 2026-10-03): exec fencing applies to the
	// OPERATOR-OWNED PROXIES (model proxy, egress proxy), NOT to the agent
	// container, in the MVP. The agent's runner executes every tool call via
	// "/bin/sh -c", and a shell command spawns an open-ended set of binaries
	// (the shell's external commands, the Go toolchain's compile/link helpers),
	// so an exact-path allow-list cannot describe an agent shell — a restrictive
	// agent exec list wedges the Loop under an enforcing Block policy. What
	// protects the system instead: the network fence (egress proxy +
	// NetworkPolicy), credential isolation (ADR-0006) and the verify Job
	// (ADR-0005); exec inside the agent sandbox is deliberately unrestricted.
	// A referenced exec list that omits the runner's shell (/bin/sh) is
	// rejected by the controller (PolicyValid=False, reason
	// ExecListMissingShell) before the sandbox is created — the Loop fails fast
	// instead of wedging. Per-tool agent proxies (no general shell) are the
	// D41 follow-on that makes agent exec fencing meaningful again.
	//
	// Absolute paths are required (P1, ADR-0007 Q3): the agent has three writable
	// mounts (/workspace, /scratch, /tmp), so a bare command name ("git") is
	// spoofable — the agent could write its own /tmp/git that does anything and
	// claim it is the allowed "git". The eBPF engine matches the binary by
	// absolute path, so the allow must name a real binary outside the writable
	// mounts.
	//
	// The XValidation rejects bare names (no leading /) and anything at or under
	// the writable mounts (/workspace, /scratch, /tmp).
	//
	// P1 (R15): non-canonical paths that resolve to a writable mount after
	// normalization ("//tmp/git", "/usr/../tmp/git") are NOT caught by this CEL
	// rule — adding a !contains('..') check would push the CRD over the CEL cost
	// budget (2.0x over). They ARE caught by the controller in
	// effectivePolicyHash (isNonCanonicalPath), which rejects the Loop before it
	// reaches the eBPF engine. The CRD is the first line of defence (catches the
	// common case at admission); the controller is the second (catches everything).
	// +optional
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=512
	// +kubebuilder:validation:XValidation:rule="self.all(e, e.matches('^(/[A-Za-z0-9._+-]+)+$') && !e.contains('/./') && !e.contains('/../') && !e.endsWith('/.') && !e.endsWith('/..') && !e.startsWith('/workspace/') && e != '/workspace' && !e.startsWith('/scratch/') && e != '/scratch' && !e.startsWith('/tmp/') && e != '/tmp')",message="exec entries must be canonical absolute paths (no ., .., //, trailing /) at or outside the writable mounts"
	Exec []string `json:"exec,omitempty"`

	// network is the host:port endpoints the agent may reach (e.g.
	// "proxy.golang.org:443"). The model endpoint is reached only by the model
	// proxy, never the agent (D29). An empty list means the agent has no
	// external egress.
	//
	// I42e (first layer of the SSRF defence): an allow must not name an
	// in-cluster target, because the agent's egress is enforced by the
	// operator's egress proxy (I42a/I42b) — an in-cluster "external" allow is
	// the exfiltration path. This CEL rule is the cheap first layer that fires
	// at admission; it rejects, case-insensitively (lowerAscii) and
	// trailing-dot-aware, the in-cluster name suffixes the CRD can check
	// without cluster config (".svc" / ".svc.cluster.local" / ".cluster.local",
	// i.e. a Service / cluster-local FQDN) and the literal loopback hostnames
	// localhost / 127.0.0.1.
	//
	// The rule uses only the free string methods (startsWith / endsWith /
	// contains / lowerAscii): a regex (matches) cannot express the dotted
	// suffixes because CEL string literals do not process backslash escapes
	// (\. is an illegal escape), so a literal backslash can never reach the
	// regex. Consequence (documented per review P2/P3): the CRD name rule is a
	// deliberate, cheap first layer and the controller-side check
	// (policy.FindInClusterNetworkAllow, run before the egress proxy is
	// created) is the authoritative first layer. The controller catches, for
	// every object (including ones created before this rule and any future CRD
	// drift): the .svc / .svc.cluster.local / cluster.local name suffixes
	// (mirrored, case-insensitive, trailing-dot-aware — so the trailing root-dot
	// forms "…svc.", "…svc.cluster.local." that this rule admits because the
	// host text is followed by ":port", not the suffix itself, are still
	// rejected by the controller), the full loopback / unspecified / link-local
	// IP forms (127/8, 169.254/16, 0.0.0.0, [::], etc.), and IP literals inside
	// the operator's pod/service CIDR. The egress proxy's resolved-IP check
	// (I42a) is the backstop that catches rebinding / split-horizon.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=512
	// +kubebuilder:validation:XValidation:rule="self.all(e, !(e.contains('.svc:') || e.contains('.svc.cluster.local:') || e.contains('.cluster.local:') || e.lowerAscii().contains('.svc:') || e.lowerAscii().contains('.svc.cluster.local:') || e.lowerAscii().contains('.cluster.local:') || e.startsWith('localhost') || e.startsWith('127.0.0.1') || e.endsWith('.svc:') || e.endsWith('.svc.cluster.local:') || e.endsWith('.cluster.local:') || e.lowerAscii().endsWith('.svc:') || e.lowerAscii().endsWith('.svc.cluster.local:') || e.lowerAscii().endsWith('.cluster.local:') || e.endsWith('.svc.') || e.endsWith('.svc.cluster.local.') || e.endsWith('.cluster.local.') || e.lowerAscii().endsWith('.svc.') || e.lowerAscii().endsWith('.svc.cluster.local.') || e.lowerAscii().endsWith('.cluster.local.')))",message="network allows must not name in-cluster targets (.svc / .svc.cluster.local / .cluster.local / localhost / 127.0.0.1): the agent's external egress is enforced by the egress proxy, and an in-cluster target is an SSRF path"
	Network []string `json:"network,omitempty"`

	// files is the paths the agent may access (e.g. "/workspace"). The agent's
	// own workspace is always accessible; a path here grants access elsewhere.
	// +optional
	Files []string `json:"files,omitempty"`
}

// AgentPolicyStatus defines the observed state of an AgentPolicy.
type AgentPolicyStatus struct {
	// generation is the spec generation this status was computed from (so the
	// operator can tell a status is stale).
	// +optional
	Generation int64 `json:"generation,omitempty"`

	// observedGeneration is the last processed generation.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// AgentPolicy is the Schema for the agentpolicies API (ADR-0007 Q2). It is a
// namespaced, default-deny, additive-allow policy selected by a Loop via
// spec.policyRefs[]. Who may create an AgentPolicy is a different RBAC role from
// who may create a Loop, so a Loop author cannot grant themselves more.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Generation",type=integer,JSONPath=`.status.observedGeneration`
type AgentPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentPolicySpec   `json:"spec,omitempty"`
	Status AgentPolicyStatus `json:"status,omitempty"`
}

// AgentPolicyList contains a list of AgentPolicy.
//
// +kubebuilder:object:root=true
type AgentPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentPolicy `json:"items"`
}
