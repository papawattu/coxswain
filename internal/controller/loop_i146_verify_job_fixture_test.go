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

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// plantVerifyJob creates a verify-Job fixture the way the operator's own
// ensureVerifyJob creates one: named verifyJobName, labelled
// verifyJobLabels, and owned by the Loop through a controller ownerReference
// (R39 I146).
//
// The ownerReference is an obligation on every fixture, not a detail. The
// operator refuses to trust, adopt or delete a verify Job that is not the
// Loop's (foreignVerifyJob, loop_verify_job.go), and the refusal happens
// BEFORE any decision, so a fixture created without the reference exercises
// only the "foreign" branch: the Loop holds at Verifying and decides nothing.
// A spec that asserts "no decision" then passes vacuously, and one that
// expects a decision fails on a phase assertion far from the cause (that is
// how nine specs failed at once in #283). Keeping the reference in one helper
// means a new fixture inherits it instead of re-deriving it from a comment.
//
// refine callbacks set the fields a particular fixture needs before the
// create (the pin annotation the D27 guard compares, a pod template for the
// batch controller's shape). The Job is returned created, so its UID is set
// for a stand-in pod's ownerReference.
//
// The default pod template is the minimum the API server accepts
// (spec.template.spec.containers and restartPolicy are required): several of
// the migrated sites build their Job only when the operator has NOT created
// one, so their hand-built shape had never been submitted. A fixture that
// does fire now creates a valid Job instead of failing on admission.
func plantVerifyJob(ctx context.Context, ns, name, jobName string, refine ...func(*batchv1.Job)) *batchv1.Job {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: ns,
			Labels:    verifyJobLabels(name),
		},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers:    []corev1.Container{{Name: verifyNoopContainer, Image: verifyBusybox}},
		}}},
	}
	for _, f := range refine {
		f(job)
	}
	ExpectWithOffset(1, controllerutil.SetControllerReference(p5fGet(ns, name), job, k8sClient.Scheme())).To(Succeed())
	ExpectWithOffset(1, k8sClient.Create(ctx, job)).To(Succeed())
	return job
}

var _ = Describe("I146: the verify-Job fixtures are Loop-owned", func() {
	ctx := context.Background()

	// The spec is the guard on the helper's precondition: drop the
	// SetControllerReference line from plantVerifyJob and this fails. The
	// positive control is the bare Job below — it is what the operator refuses,
	// so the assertion above is not vacuous.
	It("plantVerifyJob's Job is the Loop's, not a foreign Job the operator would refuse", func() {
		ns := "i146-owned-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		})
		const name = "i146-owned"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s5aLoopSpec(),
		})).To(Succeed())

		loop := p5fGet(ns, name)
		jobName := verifyJobName(loop)
		planted := plantVerifyJob(ctx, ns, name, jobName)
		Expect(planted.UID).NotTo(BeEmpty(), "the fixture Job was created")
		Expect(metav1.IsControlledBy(planted, loop)).To(BeTrue(), "the fixture Job is Loop-owned")
		Expect(foreignVerifyJob(planted, loop)).To(BeFalse(), "the operator trusts the fixture Job")

		// Positive control: the bare shape the sixteen fixtures used to build (a
		// labelled Job with no controller reference) is exactly what the guard
		// refuses — so the assertion above is not vacuous.
		bare := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
			Name: verifyJobName(loop) + "-bare", Namespace: ns, Labels: verifyJobLabels(name),
		}}
		Expect(foreignVerifyJob(bare, loop)).To(BeTrue(), "a Job with no controller reference is foreign")
	})
})
