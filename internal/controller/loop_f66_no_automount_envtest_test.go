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

// F66 (P2, sec): the verify and deliver Job pods must not automount a
// ServiceAccount token.
//
// Both Jobs run agent-chosen code — the verify Job's `check-*` containers run
// the agent's own tests and `import-agent` fetches the agent's repo; the deliver
// Job fetches that repo and pushes from it — and neither makes an API call. The
// operator's every other pod already says so explicitly (the sandbox,
// loop_controller.go; the model proxy; the egress proxy; the tool proxy; the
// memory-writer Job, loop_memory_writer_job.go), but these two Jobs only set
// ServiceAccountName: "" — which selects the default SA and still mounts its
// token. A token in a container running agent code is a credential the sandbox's
// own hardening keeps out of the agent pod.
package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// f66Pin is the verified commit the deliver Job's push container is built from.
const f66Pin = "0123456789abcdef0123456789abcdef01234567"

// f66NoAutomountOK asserts the two F66 properties on a Job's pod template: no
// ServiceAccount token is automounted (explicitly, not by omission) and no
// ServiceAccount is selected by name.
func f66NoAutomountOK(spec corev1.PodSpec, which string) {
	ExpectWithOffset(1, spec.AutomountServiceAccountToken).NotTo(BeNil(),
		"the %s Job pod must state automountServiceAccountToken explicitly (nil inherits the default SA's mount)", which)
	ExpectWithOffset(1, *spec.AutomountServiceAccountToken).To(BeFalse(),
		"the %s Job runs agent-chosen code and makes no API calls: no SA token may be mounted", which)
	ExpectWithOffset(1, spec.ServiceAccountName).To(BeEmpty(),
		"the %s Job must not select a ServiceAccount by name", which)
}

var _ = Describe("F66: the verify and deliver Job pods automount no ServiceAccount token", func() {
	ctx := context.Background()

	// The verify Job is asserted on the object the operator CREATES (envtest), so
	// the spec covers ensureVerifyJob's wiring and not just the builder.
	It("sets automountServiceAccountToken false on the verify Job the operator creates", func() {
		ns := "f66-verify-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "f66verify"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s5aLoopSpec(),
		})).To(Succeed())

		r := &LoopReconciler{CNIProber: testCNIProber(), Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
		s5aDriveToVerifyingWithReconciler(r, ns, name)

		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: verifyJobName(&coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		})}, job)).To(Succeed(), "the operator created the verify Job")
		f66NoAutomountOK(job.Spec.Template.Spec, "verify")
	})

	// The deliver Job has no cheap envtest path to a created object (it needs a
	// Succeeded Loop and a PR), so this asserts the spec the operator creates
	// verbatim (`Spec: r.buildDeliverJobSpec(loop)`, loop_deliver_job.go).
	It("sets automountServiceAccountToken false on the deliver Job spec", func() {
		// The shared verify fixture (an IN-CLUSTER repo, so verifyNeedsProxy is
		// false and no proxy/config seam is read) and a zero-valued reconciler —
		// the shape the F53 template specs use for the same builders.
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: "f66deliver", Namespace: "f66-ns", UID: types.UID("f66-uid")},
			Spec:       s5aLoopSpec(),
		}
		loop.Status.CurrentVerify = &coxv1alpha1.CurrentVerifyStatus{VerifiedCommit: f66Pin}

		r := &LoopReconciler{}
		f66NoAutomountOK(r.buildDeliverJobSpec(loop).Template.Spec, "deliver")
	})
})
