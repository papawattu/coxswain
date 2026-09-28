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
	// the exfiltration path. The CEL XValidation rejects the name suffixes the
	// CRD can check without cluster config ("...svc" / "...svc.cluster.local",
	// i.e. a Service FQDN); the IP-in-pod/service-CIDR and localhost cases need
	// the operator's CIDR config and are rejected controller-side (reason
	// InClusterAllow, PolicyValid=False) before the egress proxy is created.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=512
	// +kubebuilder:validation:XValidation:rule="self.all(e, !e.startsWith('localhost:') && !e.startsWith('127.0.0.1:') && !e.contains('.svc:') && !e.contains('.svc.cluster.local:'))",message="network allows must not name in-cluster targets (.svc / .svc.cluster.local / localhost / 127.0.0.1): the agent's external egress is enforced by the egress proxy, and an in-cluster target is an SSRF path"
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
