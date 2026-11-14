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

const (
	policyYAMLRelativePath = "../../config/admission/validating_admission_policy.yaml"
	// chartVAPRelativePath is the Helm chart's copy of the VAP (the chart
	// install method, generated next to its binding template). PR #491 review
	// P1: the J5b judge-proxy component was added to the config/ file but the
	// chart copy kept the old component list and message, so a Helm-installed
	// cluster would leave the Judge proxy pod's ephemeral-container deny out
	// of scope. loadVAPSpec reads BOTH files and the pin applies to each.
	chartVAPRelativePath = "../../dist/chart/templates/admission/ephemeral-containers-vap.yaml"
)

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

// stripChartDirectives removes the Helm {{- /* … */}} comment block from the
// chart template so the remaining VAP body parses as plain YAML. It tracks
// an "inside block" state and drops all lines from the opening {{- through
// the closing */}} (inclusive); the VAP body itself carries no directives.
func stripChartDirectives(data []byte) string {
	var out strings.Builder
	inBlock := false
	for line := range strings.SplitSeq(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if !inBlock {
			if strings.HasPrefix(trimmed, "{{") {
				inBlock = true
				continue
			}
			out.WriteString(line)
			out.WriteString("\n")
		} else {
			if strings.HasSuffix(trimmed, "}}") {
				inBlock = false
			}
			// lines inside the block (including the closing */}}) are dropped
		}
	}
	return out.String()
}

func vapSpecFrom(t *testing.T, path string, strip bool) vapSpec {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if strip {
		data = []byte(stripChartDirectives(data))
	}
	// The file is multi-doc (ValidatingAdmissionPolicy + its binding) OR
	// single-doc (the chart template renders only the VAP; its binding lives
	// in the neighbouring deny-ephemeral-containers-binding.yaml).
	if strings.Contains(string(data), "\n---") {
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
	} else {
		var m struct {
			Kind string  `yaml:"kind"`
			Spec vapSpec `yaml:"spec"`
		}
		if err := yaml.Unmarshal(data, &m); err == nil && m.Kind == "ValidatingAdmissionPolicy" {
			return m.Spec
		}
	}
	t.Fatalf("no ValidatingAdmissionPolicy document found in %s", path)
	return vapSpec{}
}

func loadVAPSpec(t *testing.T) vapSpec {
	return vapSpecFrom(t, policyYAMLRelativePath, false)
}

// assertVAPSpecPinned applies the full pin to one VAP spec (the config/
// install file and the chart template must both carry it — the two install
// methods are independent, so one test asserts the same pin on both).
func assertVAPSpecPinned(t *testing.T, spec vapSpec, path string) {
	t.Helper()

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
	// component values (as quoted CEL string literals).
	for _, c := range []string{
		ComponentLabelKey,
		ComponentAgentLabel,
		ComponentProxyLabel,
		ComponentJudgeProxyLabel,
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
	// The message names the component pods; it must include the judge-proxy
	// (J5b) — the config file's message text is the reference and the chart
	// copy must match it word for word (PR #491 review P1).
	for _, c := range []string{ComponentAgentLabel, ComponentProxyLabel, ComponentJudgeProxyLabel, ComponentEgressProxyLabel, ComponentToolProxyLabel} {
		if !strings.Contains(v.Message, c) {
			t.Errorf("%s validations[0].message does not name component %q:\n%s", path, c, v.Message)
		}
	}
}

func TestPolicyYAMLPinnedAgainstConstants(t *testing.T) {
	assertVAPSpecPinned(t, loadVAPSpec(t), "config/admission/validating_admission_policy.yaml")
}

// J5b (PR #491 review P1): the CHART's VAP copy (dist/chart/templates) must
// carry the same component list and message as the config/ install file —
// the Helm install and the kustomize install are independent, so the pin must
// hold on both or a Helm-installed cluster would miss the judge-proxy.
func TestChartVAPPinnedAgainstConstants(t *testing.T) {
	assertVAPSpecPinned(t, vapSpecFrom(t, chartVAPRelativePath, true), "dist/chart/templates/admission/ephemeral-containers-vap.yaml")
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
	// from the same file to keep them in sync. The admission kustomization
	// applies `namePrefix: coxswain-`, which transforms metadata.name but NOT
	// spec.policyName (a spec field). The raw file's spec.policyName must
	// carry the PREFIXED name (coxswain-<base>) so the rendered binding
	// references the rendered VAP. The test extracts the raw VAP
	// metadata.name and applies the same prefix the kustomization uses.
	data, err := os.ReadFile(policyYAMLRelativePath)
	if err != nil {
		t.Fatalf("reading config/admission/validating_admission_policy.yaml: %v", err)
	}
	var baseName string
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
			baseName = m.Metadata.Name
			break
		}
	}
	if baseName == "" {
		t.Fatalf("could not find ValidatingAdmissionPolicy metadata.name in %s", policyYAMLRelativePath)
	}
	// The admission kustomization applies namePrefix: coxswain- (see
	// config/admission/kustomization.yaml). The rendered VAP name is
	// coxswain-<baseName>; the binding's spec.policyName must match.
	renderedPolicyName := "coxswain-" + baseName
	if b.PolicyName != renderedPolicyName {
		t.Errorf("binding policyName = %q, want %q (must match the rendered policy name: namePrefix 'coxswain-' + raw metadata.name %q)", b.PolicyName, renderedPolicyName, baseName)
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
