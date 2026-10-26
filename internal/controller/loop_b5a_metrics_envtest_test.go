// B5a: envtest specs that drive Reconcile to verify that the Prometheus
// metrics are bumped at the REAL call sites (not just by calling the bump
// helpers directly). These specs create a Loop, drive it to Succeeded (or
// Failed), and check that the relevant metrics were bumped.
//
// Mutation: remove the call to bumpLoopCompletion from applyVerifyOutcome
// must fail the Succeeded spec (the metric stays at its baseline).
// Mutation: remove the call to bumpVerifyResult from applyVerifyOutcome
// must fail the Succeeded spec (the metric stays at its baseline).

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// readMetricLabelEnvtest reads the value of a named metric for a specific
// label combination from the process-wide controller-runtime metrics registry.
// Returns (value, found).
func readMetricLabelEnvtest(name string, labels map[string]string) (float64, bool) {
	mfs, err := metrics.Registry.Gather()
	if err != nil {
		return 0, false
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			match := true
			for _, lp := range m.GetLabel() {
				if v, ok := labels[lp.GetName()]; ok && v != lp.GetValue() {
					match = false
					break
				}
			}
			if !match {
				continue
			}
			allPresent := true
			for _, lp := range m.GetLabel() {
				if _, ok := labels[lp.GetName()]; !ok {
					allPresent = false
					break
				}
			}
			if !allPresent {
				continue
			}
			if c := m.GetCounter(); c != nil {
				return c.GetValue(), true
			}
			if g := m.GetGauge(); g != nil {
				return g.GetValue(), true
			}
			return 0, true
		}
	}
	return 0, false
}

var _ = Describe("B5a metrics envtest", func() {
	ctx := context.Background()

	It("bumps loops_completed and verify_results when a Loop reaches Succeeded", func() {
		ns := "b5a-metrics-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		// Reset the metrics to a known baseline.
		loopsCompletedTotal.Reset()
		verifyResultsTotal.Reset()

		// Create the Loop.
		name := "b5a-loop"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s5aLoopSpec(),
		})).To(Succeed())

		// Drive the Loop to Verifying (via the s5a helper).
		r := s5aDriveToVerifying(ns, name)

		// The verify Job pod (envtest stand-in: no Job controller, so the spec
		// creates the pod the Job WOULD create, labelled job-name).
		jobName := verifyJobName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
		job := &batchv1.Job{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: jobName, Namespace: ns}, job); err != nil {
			job = plantVerifyJob(ctx, ns, name, jobName)
		}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name + "-verify-pod",
				Namespace: ns,
				Labels:    map[string]string{s5aJobNameLabel: jobName, verifyForLabel: name},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: batchv1.SchemeGroupVersion.String(),
					Kind:       i65JobKind,
					Name:       jobName,
					UID:        job.UID,
				}},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: verifyNoopContainer, Image: verifyBusybox}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		// Tamper clean (0) + check-0 pass (0).
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{
			{Name: s5aCloneBase, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: s5aImportAgent, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: s5aTamper, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: s5aCheck0, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
		}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		// Reconcile: this should detect the verify success and transition to Succeeded.
		l := s5aReconcile(r, ns, name)
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded))

		// Check the metrics.
		succCount, found := readMetricLabelEnvtest("coxswain_loops_completed_total",
			map[string]string{"result": "succeeded", "reason": "verify_passed"})
		Expect(found).To(BeTrue(), "coxswain_loops_completed_total{result=succeeded,reason=verify_passed} should be found")
		Expect(succCount).To(Equal(1.0), "coxswain_loops_completed_total{result=succeeded,reason=verify_passed} should be 1")

		passCount, found := readMetricLabelEnvtest("coxswain_verify_results_total",
			map[string]string{"result": "pass"})
		Expect(found).To(BeTrue(), "coxswain_verify_results_total{result=pass} should be found")
		Expect(passCount).To(Equal(1.0), "coxswain_verify_results_total{result=pass} should be 1")
	})
})
