/*
Copyright 2026 papawattu.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an " IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.See the License for the specific language governing permissions and
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
	// repo is the git URL to clone, e.g. https://github.com/papawattu/pixme.git
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Format=uri
	Repo string `json:"repo"`

	// ref is the branch, tag, or commit to check out. Defaults to the repo's
	// default branch when empty.
	// +optional
	// +kubebuilder:default=""
	Ref string `json:"ref,omitempty"`

	// gitCredentialSecret is the name of a Secret in the Loop's namespace
	// holding git credentials for pushing the Loop's branch.
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
}

// LoopSettings holds the loop-level knobs.
type LoopSettings struct {
	// maxIterations caps the plan→implement→verify cycles.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=10
	MaxIterations int `json:"maxIterations,omitempty"`
}

// LoopSpec defines the desired state of a Loop.
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

	// loop holds iteration and phase-timeout settings.
	// +optional
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
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Iteration",type=integer,JSONPath=`.status.iteration`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

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
