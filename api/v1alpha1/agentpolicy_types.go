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
	// exec is the commands the agent may run (e.g. "git", "go", "node"). An
	// empty list means the agent may run no commands beyond the platform
	// minimum (its own entrypoint). +optional
	Exec []string `json:"exec,omitempty"`

	// network is the host:port endpoints the agent may reach (e.g.
	// "proxy.golang.org:443"). localhost is always allowed (the model proxy
	// sidecar); the model endpoint is reached only by the proxy, never the
	// agent (D29). An empty list means the agent may egress only to localhost.
	// +optional
	Network []string `json:"network,omitempty"`

	// files is the paths the agent may access (e.g. "/workspace"). The agent's
	// own workspace is always accessible; a path here grants access elsewhere.
	// +optional
	Files []string `json:"files,omitempty"`
}

// AgentPolicyStatus defines the observed state of an AgentPolicy.
type AgentPolicyStatus struct {
	// generation is the spec generation this status was computed from (so the
	// operator can tell a status is stale). +optional
	Generation int64 `json:"generation,omitempty"`

	// observedGeneration is the last processed generation. +optional
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
