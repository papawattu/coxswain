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

package controller

// I45 (R16): ephemeral containers (kubectl debug) must not be addable to
// coxswain component pods (agent, model-proxy, egress-proxy) — KubeArmor does
// not police ephemeral containers, so one added to a fenced pod escapes the
// fence. The ValidatingAdmissionPolicy + binding in
// config/admission/validating_admission_policy.yaml (wired into
// config/default) denies the pods/ephemeralcontainers UPDATE.
//
// The envtest apiserver enforces ValidatingAdmissionPolicies (GA in k8s 1.34),
// so this spec applies the policy + binding to the test cluster, creates a
// pod per coxswain component (carrying the operator's component label) and
// an unlabelled pod, and asserts the pods/ephemeralcontainers update is
// DENIED for each component pod and ALLOWED for the unlabelled one. The
// subresource update is issued with DryRun=All: the admission chain (including
// the VAP) is evaluated, but no write persists, so the spec needs no
// kubelet.
//
// I43 gate norm (R16): this spec FAILS when the gate is disabled — remove the
// applyI45AdmissionPolicy call in the BeforeEach below and the "denied"
// assertions fail because the update succeeds. See the PR's gate-failure
// section for the recorded run.

import (
	"context"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/papawattu/coxswain/internal/policy"
)

const (
	// i45PolicyName / i45BindingName mirror the object names in
	// config/admission/validating_admission_policy.yaml. They are the
	// objects this spec applies to the envtest cluster; the YAML file's
	// values are pinned against the same policy constants by
	// config/admission/policy_yaml_test.go.
	i45PolicyName  = "coxswain-deny-ephemeral-containers"
	i45BindingName = "coxswain-deny-ephemeral-containers"
)

var _ = Describe("I45: ValidatingAdmissionPolicy denies ephemeral containers on component pods", func() {
	var (
		ctx context.Context
		ns  string
	)

	// i45CELExpression is the matchCondition expression. It is built from the
	// policy package constants so the test and the Go label values cannot
	// desync; the YAML manifest's literal copy is pinned separately by
	// config/admission/policy_yaml_test.go.
	i45CELExpression := "object.metadata.labels['" + policy.ComponentLabelKey + "'] == '" + policy.ComponentAgentLabel + "' || " +
		"object.metadata.labels['" + policy.ComponentLabelKey + "'] == '" + policy.ComponentProxyLabel + "' || " +
		"object.metadata.labels['" + policy.ComponentLabelKey + "'] == '" + policy.ComponentEgressProxyLabel + "'"

	// applyI45AdmissionPolicy applies the ValidatingAdmissionPolicy + binding
	// to the envtest cluster (the same objects config/admission installs in
	// a real cluster). The envtest apiserver enforces the policy ONLY when
	// started with --feature-gates=ValidatingAdmissionPolicy=true (v1 is GA in
	// 1.34, but the envtest suite's apiserver flags are set in
	// suite_test.go; see the I45 note there).
	applyI45AdmissionPolicy := func() {
		failurePolicy := admissionv1.Fail
		matchConstraints := &admissionv1.MatchResources{
			ResourceRules: []admissionv1.NamedRuleWithOperations{{
				RuleWithOperations: admissionv1.RuleWithOperations{
					Operations: []admissionv1.OperationType{admissionv1.Update},
					Rule: admissionv1.Rule{
						APIGroups:   []string{""},
						APIVersions: []string{"v1"},
						Resources:   []string{"pods/ephemeralcontainers"},
					},
				},
			}},
		}
		vap := &admissionv1.ValidatingAdmissionPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: i45PolicyName},
			Spec: admissionv1.ValidatingAdmissionPolicySpec{
				FailurePolicy:    &failurePolicy,
				MatchConstraints: matchConstraints,
				MatchConditions:  []admissionv1.MatchCondition{{Name: "coxswainComponentPod", Expression: i45CELExpression}},
				Validations: []admissionv1.Validation{{
					Expression: "false",
					Message:    "adding ephemeral containers to coxswain component pods is denied (KubeArmor fence)",
				}},
			},
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, vap)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &admissionv1.ValidatingAdmissionPolicy{ObjectMeta: metav1.ObjectMeta{Name: i45PolicyName}})
		})

		vapb := &admissionv1.ValidatingAdmissionPolicyBinding{
			ObjectMeta: metav1.ObjectMeta{Name: i45BindingName},
			Spec: admissionv1.ValidatingAdmissionPolicyBindingSpec{
				PolicyName:        i45PolicyName,
				ValidationActions: []admissionv1.ValidationAction{admissionv1.Deny},
			},
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, vapb)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &admissionv1.ValidatingAdmissionPolicyBinding{ObjectMeta: metav1.ObjectMeta{Name: i45BindingName}})
		})
	}

	// newPod creates a pod with the given component label ("" for no label)
	// and returns it. The pod never needs to be scheduled: the spec only
	// updates spec.ephemeralContainers (DryRun=All), which the apiserver
	// validates without the kubelet.
	newPod := func(component string) *corev1.Pod {
		labels := map[string]string{}
		if component != "" {
			labels[policy.ComponentLabelKey] = component
		}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "i45-" + component + "-",
				Namespace:    ns,
				Labels:       labels,
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "main", Image: "busybox:1.36", Command: []string{"sh", "-c", "sleep 3600"}},
				},
			},
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, pod)).To(Succeed())
		return pod
	}

	// addEphemeral attempts a pods/ephemeralcontainers update on the pod
	// (the subresource write kubectl debug performs) and returns whether it
	// was allowed. DryRun=All means the write does not persist; only the
	// admission verdict matters.
	addEphemeral := func(pod *corev1.Pod) bool {
		target := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:       pod.Namespace,
				Name:            pod.Name,
				ResourceVersion: pod.ResourceVersion,
			},
			Spec: corev1.PodSpec{
				Containers: pod.Spec.Containers,
				EphemeralContainers: []corev1.EphemeralContainer{{
					EphemeralContainerCommon: corev1.EphemeralContainerCommon{
						Name:    "dbg",
						Image:   "busybox:1.36",
						Command: []string{"sh", "-c", "true"},
					},
				}},
			},
		}
		err := k8sClient.SubResource("ephemeralcontainers").Update(ctx, pod,
			dryRunAllUpdate{SubResourceBody: target},
		)
		if err == nil {
			return true
		}
		if apierrors.IsInvalid(err) {
			return false
		}
		// Log the actual error so we can see if it's the VAP denial or something else.
		F("unexpected error on pods/ephemeralcontainers update of " + pod.Name + ": " + err.Error())
		return false
	}

	BeforeEach(func() {
		ctx = context.Background()
		ns = "i45-eph-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) })
		// The gate under test: apply the policy + binding. (I43 norm: removing
		// this makes the "denied" assertions below fail.)
		applyI45AdmissionPolicy()
	})

	// The VAP denies by label VALUE, so each of the three component values
	// gets its own pod. The unlabelled pod must stay ALLOWED.
	components := []struct{ name, value string }{
		{"agent", policy.ComponentAgentLabel},
		{"model-proxy", policy.ComponentProxyLabel},
		{"egress-proxy", policy.ComponentEgressProxyLabel},
	}

	for _, c := range components {
		c := c
		It("denies an ephemeral container on a pod labelled component="+c.value, func() {
			pod := newPod(c.value)
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, pod, &client.DeleteOptions{GracePeriodSeconds: ptrInt64(0)}) })
			allowed := addEphemeral(pod)
			Expect(allowed).To(BeFalse(),
				"adding an ephemeral container to a component=%s pod must be DENIED by the ValidatingAdmissionPolicy", c.value)
		})
	}

	It("allows an ephemeral container on an unlabelled pod", func() {
		pod := newPod("")
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, pod, &client.DeleteOptions{GracePeriodSeconds: ptrInt64(0)}) })
		allowed := addEphemeral(pod)
		Expect(allowed).To(BeTrue(),
			"an unlabelled pod must stay allowed (the VAP matches on the component label)")
	})
})

func ptrInt64(v int64) *int64 { return &v }

// dryRunAllUpdate is a SubResourceUpdateOption that sets DryRun=All and the
// subresource body (the ephemeral-container-bearing pod spec) in one option.
type dryRunAllUpdate struct {
	SubResourceBody client.Object
}

func (o dryRunAllUpdate) ApplyToSubResourceUpdate(dst *client.SubResourceUpdateOptions) {
	dst.UpdateOptions.DryRun = []string{"All"}
	dst.SubResourceBody = o.SubResourceBody
}
