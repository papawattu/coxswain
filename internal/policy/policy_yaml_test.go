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

package policy

// I45 (R16): the ValidatingAdmissionPolicy in
// config/admission/validating_admission_policy.yaml denies adding ephemeral
// containers to coxswain component pods. Its CEL matchCondition hard-codes the
// label key and the three component values. This test reads the YAML file by
// relative path and pins those literals against the package constants, so
// renaming a constant in Go cannot silently desync the manifest.

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const policyYAMLRelativePath = "../../config/admission/validating_admission_policy.yaml"

// vapSpec mirrors the ValidatingAdmissionPolicySpec fields this test pins.
// (admissionregistration.k8s.io/v1: the policy-wide matchConditions gate which
// requests are evaluated; validations is the CEL validation list.)
type vapSpec struct {
	FailurePolicy    *string           `yaml:"failurePolicy"`
	MatchConstraints *matchConstraints `yaml:"matchConstraints"`
	MatchConditions  []matchCondition  `yaml:"matchConditions"`
	Validations      []validationRule  `yaml:"validations"`
}

type matchConstraints struct {
	ResourceRules []resourceRule `yaml:"resourceRules"`
}

type resourceRule struct {
	Operations []string `yaml:"operations"`
	APIGroups  []string `yaml:"apiGroups"`
	APIVersion []string `yaml:"apiVersions"`
	Resources  []string `yaml:"resources"`
}

type validationRule struct {
	Expression string `yaml:"expression"`
	Message    string `yaml:"message"`
}

type matchCondition struct {
	Name       string `yaml:"name"`
	Expression string `yaml:"expression"`
}

func loadVAPSpec(t *testing.T) vapSpec {
	t.Helper()
	data, err := os.ReadFile(policyYAMLRelativePath)
	if err != nil {
		t.Fatalf("reading config/admission/validating_admission_policy.yaml: %v", err)
	}
	// The file is multi-doc (ValidatingAdmissionPolicy + its binding).
	// Split on the document separator and parse each doc individually.
	for doc := range strings.SplitSeq(string(data), "\n---") {
		var m struct {
			Kind string  `yaml:"kind"`
			Spec vapSpec `yaml:"spec"`
		}
		if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
			continue
		}
		if m.Kind == "ValidatingAdmissionPolicy" {
			return m.Spec
		}
	}
	t.Fatalf("no ValidatingAdmissionPolicy document found in %s", policyYAMLRelativePath)
	return vapSpec{}
}

func TestPolicyYAMLPinnedAgainstConstants(t *testing.T) {
	spec := loadVAPSpec(t)

	// FailurePolicy must be Fail (fail-closed).
	if spec.FailurePolicy == nil || *spec.FailurePolicy != "Fail" {
		t.Errorf("failurePolicy = %v, want \"Fail\" (fail-closed)", spec.FailurePolicy)
	}

	// MatchConstraints scope: pods/ephemeralcontainers, UPDATE only.
	if spec.MatchConstraints == nil {
		t.Fatalf("matchConstraints is nil")
	}
	if len(spec.MatchConstraints.ResourceRules) != 1 {
		t.Fatalf("len(matchConstraints.resourceRules) = %d, want 1", len(spec.MatchConstraints.ResourceRules))
	}
	rule := spec.MatchConstraints.ResourceRules[0]
	if !equalStrings(rule.Resources, []string{"pods/ephemeralcontainers"}) {
		t.Errorf("matchConstraints.resourceRules[0].resources = %v, want [pods/ephemeralcontainers]", rule.Resources)
	}
	if !equalStrings(rule.Operations, []string{"UPDATE"}) {
		t.Errorf("matchConstraints.resourceRules[0].operations = %v, want [UPDATE]", rule.Operations)
	}

	// matchConditions: exactly one, selecting coxswain component pods.
	if len(spec.MatchConditions) != 1 {
		t.Fatalf("len(matchConditions) = %d, want 1", len(spec.MatchConditions))
	}
	expr := spec.MatchConditions[0].Expression

	// The matchCondition expression must reference the label key and all
	// three component values (as quoted CEL string literals).
	for _, c := range []string{
		ComponentLabelKey,
		ComponentAgentLabel,
		ComponentProxyLabel,
		ComponentEgressProxyLabel,
		ComponentToolProxyLabel,
	} {
		if !strings.Contains(expr, "'"+c+"'") {
			t.Errorf("matchCondition expression does not reference constant %q (as a quoted literal):\n%s", c, expr)
		}
	}

	// Validations: exactly one, denying all matching updates.
	if len(spec.Validations) != 1 {
		t.Fatalf("len(validations) = %d, want 1", len(spec.Validations))
	}
	v := spec.Validations[0]
	if v.Expression != "false" {
		t.Errorf("validations[0].expression = %q, want \"false\" (deny)", v.Expression)
	}

	// The message should mention the KubeArmor fence rationale.
	if !strings.Contains(v.Message, "KubeArmor fence") {
		t.Errorf("validations[0].message should mention the KubeArmor fence rationale:\n%s", v.Message)
	}
}

// vapbSpec mirrors the ValidatingAdmissionPolicyBindingSpec fields this test pins.
type vapbSpec struct {
	PolicyName        string   `yaml:"policyName"`
	ValidationActions []string `yaml:"validationActions"`
}

func loadVAPBSpec(t *testing.T) vapbSpec {
	t.Helper()
	data, err := os.ReadFile(policyYAMLRelativePath)
	if err != nil {
		t.Fatalf("reading config/admission/validating_admission_policy.yaml: %v", err)
	}
	for doc := range strings.SplitSeq(string(data), "\n---") {
		var m struct {
			Kind string   `yaml:"kind"`
			Spec vapbSpec `yaml:"spec"`
		}
		if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
			continue
		}
		if m.Kind == "ValidatingAdmissionPolicyBinding" {
			return m.Spec
		}
	}
	t.Fatalf("no ValidatingAdmissionPolicyBinding document found in %s", policyYAMLRelativePath)
	return vapbSpec{}
}

// TestPolicyYAMLBindingIsDeny pins the binding's validationActions to [Deny]
// and checks that its policyName matches the policy document's metadata.name.
// Flipping the binding to [Warn] or [Audit] would disable enforcement, which
// the envtest (which loads the shipped YAML) would also catch, but this unit
// test fails fast with a clear message.
func TestPolicyYAMLBindingIsDeny(t *testing.T) {
	b := loadVAPBSpec(t)

	if len(b.ValidationActions) != 1 || b.ValidationActions[0] != "Deny" {
		t.Errorf("binding validationActions = %v, want [Deny]", b.ValidationActions)
	}

	// The binding must reference the policy by name. We read the policy name
	// from the same file to keep them in sync.
	data, err := os.ReadFile(policyYAMLRelativePath)
	if err != nil {
		t.Fatalf("reading config/admission/validating_admission_policy.yaml: %v", err)
	}
	policyName := ""
	for doc := range strings.SplitSeq(string(data), "\n---") {
		var m struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
			continue
		}
		if m.Kind == "ValidatingAdmissionPolicy" {
			policyName = m.Metadata.Name
			break
		}
	}
	if policyName == "" {
		t.Fatalf("could not find ValidatingAdmissionPolicy metadata.name in %s", policyYAMLRelativePath)
	}
	if b.PolicyName != policyName {
		t.Errorf("binding policyName = %q, want %q (must match the policy document's metadata.name)", b.PolicyName, policyName)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
