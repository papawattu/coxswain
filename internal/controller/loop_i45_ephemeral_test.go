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
// so this spec loads the SHIPPED YAML (config/admission/
// validating_admission_policy.yaml) into the test cluster, creates a pod per
// coxswain component (carrying the operator's component label) and an
// unlabelled pod, and asserts the pods/ephemeralcontainers update is DENIED
// for each component pod and ALLOWED for the unlabelled one. The subresource
// update is issued with DryRun=All: the admission chain (including the VAP)
// is evaluated, but no write persists, so the spec needs no kubelet.
//
// Loading the shipped YAML (not a Go copy) means this spec proves the exact
// artefact we deploy: dropping the has() guard, changing the CEL expression,
// or flipping the binding to Warn would all be caught here. The constants pin
// in internal/policy/policy_yaml_test.go keeps the manifest's literals in sync
// with the Go label values.
//
// I43 gate norm (R16): this spec FAILS when the gate is disabled — remove the
// applyI45AdmissionPolicy call in the BeforeEach below and the "denied"
// assertions fail because the update succeeds. See the PR's gate-failure
// section for the recorded run.

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/papawattu/coxswain/internal/policy"
)

var _ = Describe("I45: ValidatingAdmissionPolicy denies ephemeral containers on component pods", func() {
	var (
		ctx context.Context
		ns  string
	)

	// applyI45AdmissionPolicy reads the shipped YAML, decodes the two documents
	// (ValidatingAdmissionPolicy + its binding) with sigs.k8s.io/yaml, and
	// creates them in the envtest cluster. The envtest apiserver enforces the
	// policy exactly as the shipped artefact declares it.
	applyI45AdmissionPolicy := func() {
		data, err := os.ReadFile(filepath.Join("..", "..", "config", "admission", "validating_admission_policy.yaml"))
		Expect(err).NotTo(HaveOccurred(), "reading the shipped policy YAML")

		var vap *admissionv1.ValidatingAdmissionPolicy
		var vapb *admissionv1.ValidatingAdmissionPolicyBinding
		for doc := range strings.SplitSeq(string(data), "\n---") {
			var untyped struct {
				Kind string `yaml:"kind"`
			}
			if err := yaml.Unmarshal([]byte(doc), &untyped); err != nil {
				continue
			}
			switch untyped.Kind {
			case "ValidatingAdmissionPolicy":
				vap = &admissionv1.ValidatingAdmissionPolicy{}
				Expect(yaml.Unmarshal([]byte(doc), vap)).To(Succeed())
			case "ValidatingAdmissionPolicyBinding":
				vapb = &admissionv1.ValidatingAdmissionPolicyBinding{}
				Expect(yaml.Unmarshal([]byte(doc), vapb)).To(Succeed())
			}
		}
		Expect(vap).NotTo(BeNil(), "shipped YAML has a ValidatingAdmissionPolicy document")
		Expect(vapb).NotTo(BeNil(), "shipped YAML has a ValidatingAdmissionPolicyBinding document")

		ExpectWithOffset(1, k8sClient.Create(ctx, vap)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &admissionv1.ValidatingAdmissionPolicy{ObjectMeta: metav1.ObjectMeta{Name: vap.Name}})
		})
		ExpectWithOffset(1, k8sClient.Create(ctx, vapb)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &admissionv1.ValidatingAdmissionPolicyBinding{ObjectMeta: metav1.ObjectMeta{Name: vapb.Name}})
		})
		// NOTE: the apiserver needs a moment to load a freshly-applied VAP into
		// the ValidatingAdmissionPolicy plugin's cache. We do NOT wait here —
		// the denied specs poll with Eventually (up to ~15s) until the update
		// is actually denied, and the unlabelled spec confirms the policy is
		// live before asserting allow. This avoids a one-shot update racing the
		// plugin cache.
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
						Command: []string{"sh", "-c", unstructuredTrue},
					},
				}},
			},
		}
		err := k8sClient.SubResource("ephemeralcontainers").Update(ctx, pod,
			dryRunAllUpdate{SubResourceBody: target},
		)
		// Return whether the update was allowed. The subresource update goes
		// through the full admission chain (including the VAP); DryRun=All means
		// nothing persists, so the spec needs no kubelet. On a transient error
		// (e.g. a 409 before the apiserver settles) just report "not allowed" —
		// the caller's Eventually retries.
		return err == nil
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
		{agentContainerName, policy.ComponentAgentLabel},
		{"model-proxy", policy.ComponentProxyLabel},
		{"egress-proxy", policy.ComponentEgressProxyLabel},
	}

	for _, c := range components {
		It("denies an ephemeral container on a pod labelled component="+c.value, func() {
			pod := newPod(c.value)
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, pod, &client.DeleteOptions{GracePeriodSeconds: new(int64)}) })
			// A freshly-applied VAP takes a moment to become active in the API
			// server. Poll until the update is DENIED (a labelled component pod
			// must stay denied — so retrying on a transient allow is safe: it
			// will keep being denied once the policy is live). ~15s to absorb
			// the admission-plugin cache lag.
			var allowed bool
			Eventually(func() bool {
				allowed = addEphemeral(pod)
				return !allowed
			}, "15s", "500ms").Should(BeTrue(),
				"adding an ephemeral container to a component=%s pod must be DENIED by the ValidatingAdmissionPolicy", c.value)
		})
	}

	It("allows an ephemeral container on an unlabelled pod", func() {
		pod := newPod("")
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, pod, &client.DeleteOptions{GracePeriodSeconds: new(int64)}) })
		// First confirm the policy is actually ACTIVE (a labelled pod is denied)
		// so we are not asserting "allowed" merely because the VAP has not
		// loaded yet. Then assert the unlabelled pod stays allowed.
		labelled := newPod(policy.ComponentAgentLabel)
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, labelled, &client.DeleteOptions{GracePeriodSeconds: new(int64)}) })
		Eventually(func() bool { return !addEphemeral(labelled) }, "15s", "500ms").Should(BeTrue(),
			"a labelled component pod must be denied before the unlabelled assertion")
		var allowed bool
		Consistently(func() bool {
			allowed = addEphemeral(pod)
			return allowed
		}, "5s", "500ms").Should(BeTrue(),
			"an unlabelled pod must stay allowed (the VAP matches on the component label)")
	})
})

// dryRunAllUpdate is a SubResourceUpdateOption that sets DryRun=All and the
// subresource body (the ephemeral-container-bearing pod spec) in one option.
type dryRunAllUpdate struct {
	SubResourceBody client.Object
}

func (o dryRunAllUpdate) ApplyToSubResourceUpdate(dst *client.SubResourceUpdateOptions) {
	dst.DryRun = []string{"All"}
	dst.SubResourceBody = o.SubResourceBody
}
