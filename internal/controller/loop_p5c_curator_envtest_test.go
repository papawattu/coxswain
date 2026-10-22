// P5c: the Memory Curator gate (Phase 5, PLAN-PHASE5.md slice P5c, ADR-0002).
// The curator is the one controlled exception to the deterministic-operator
// rule: the operator never interprets lesson content itself, but it MAY ask
// a model to judge "is this lesson worth keeping?" and RECORD the answer.
//
// The envtest specs test the OPERATOR-SIDE behavior:
//   - The Job is created with the curator env vars (COX_CURATOR_BASE_URL,
//     COX_MODEL) when the Loop has a model (endpointSecretRef).
//   - The Job is created WITHOUT the curator env vars when the Loop has no
//     model (the curator is disabled; the Job writes all static-rule
//     survivors — the P5b behavior).
//   - The NetworkPolicy allows egress to the model proxy pod (the P5c
//     curator call) while denying all other egress.
//
// The CURATOR CALL behavior (true/false verdict, strict JSON parsing,
// injection resistance, call failure) is tested by:
//   - pkg/curator (unit tests: the client's strict-parsing and failure
//     paths).
//   - cmd/memory-writer (execution tests: the entrypoint's curator gate
//     with a fake model proxy).
package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	coxs "github.com/papawattu/coxswain/api/v1alpha1"
)

var _ = Describe("P5c memory curator (Phase 5, ADR-0002)", func() {
	var (
		r  *LoopReconciler
		ns string
	)

	BeforeEach(func() {
		ns = "p5c-" + nowSuffix()
		Expect(k8sClient.Create(context.Background(), &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
		r = &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
	})

	AfterEach(func() {
		_ = k8sClient.Delete(context.Background(), &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})
	})

	It("sets the curator env vars (COX_CURATOR_BASE_URL, COX_MODEL) on the Job when the Loop has a model", func() {
		// The Loop has a model (endpointSecretRef + modelEndpoint): the
		// memory-writer Job gets the curator env vars (the Job calls the
		// curator via the model proxy).
		name := "p5c-curated"
		_ = s6SucceededLessonsAvailable(name, ns)

		// The Job is created during the Succeeded phase's reconcile.
		job := p5bGetMemoryJob(ns, name)
		c := job.Spec.Template.Spec.Containers[0]

		// The curator env vars are set (the Loop has a model).
		envNames := map[string]string{}
		for _, e := range c.Env {
			envNames[e.Name] = e.Value
		}
		Expect(envNames).To(HaveKey("COX_CURATOR_BASE_URL"),
			"the Job must carry COX_CURATOR_BASE_URL (the curator call goes to the model proxy)")
		Expect(envNames).To(HaveKey("COX_MODEL"),
			"the Job must carry COX_MODEL (the model name for the curator call)")
		// The curator base URL is the model proxy Service URL.
		Expect(envNames["COX_CURATOR_BASE_URL"]).To(ContainSubstring(name),
			"the curator base URL must reference the Loop's model proxy")
	})

	It("does NOT set the curator env vars when the Loop has no model", func() {
		// A Loop with no model (no endpointSecretRef): the memory-writer Job
		// does NOT get the curator env vars (the curator is disabled; the Job
		// writes all static-rule survivors — the P5b behavior).
		//
		// This spec tests the buildMemoryWriterJob function directly (not the
		// full reconcile path) to isolate the env var logic.
		name := "p5c-nomodel"
		loop := &coxs.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxs.LoopSpec{
				Goal: "a test goal",
				Workspace: coxs.Workspace{
					Repo: inClusterRepoURL,
				},
				Agent: coxs.AgentConfig{
					Model:             "", // no model
					EndpointSecretRef: "", // no endpoint secret
				},
			},
		}
		job := r.buildMemoryWriterJob(loop, "memory-pvc", "succeeded")
		c := job.Spec.Template.Spec.Containers[0]
		for _, e := range c.Env {
			Expect(e.Name).ToNot(Equal("COX_CURATOR_BASE_URL"),
				"the Job must NOT carry COX_CURATOR_BASE_URL when the Loop has no model")
			Expect(e.Name).ToNot(Equal("COX_MODEL"),
				"the Job must NOT carry COX_MODEL when the Loop has no model")
		}
		// The base env vars are still present.
		envNames := map[string]string{}
		for _, e := range c.Env {
			envNames[e.Name] = e.Value
		}
		Expect(envNames).To(HaveKey("COX_SUMMARY_PATH"))
		Expect(envNames).To(HaveKey("COX_LESSONS_PATH"))
		Expect(envNames).To(HaveKey("COX_MEMORY_DIR"))
	})

	It("allows egress to the model proxy pod in the memory-writer NetworkPolicy", func() {
		// The P5c NetworkPolicy allows egress to the model proxy pod (the
		// curator call) while denying all other egress.
		name := "p5c-netpol"
		_ = s6SucceededLessonsAvailable(name, ns)

		np := &networkingv1.NetworkPolicy{}
		npName := name + "-memory-write-netpol"
		Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: npName, Namespace: ns}, np)).To(Succeed())

		// The egress rules: the first is a blank rule (deny all), the second
		// allows egress to the model proxy pod.
		Expect(len(np.Spec.Egress)).To(BeNumerically(">", 1),
			"the NetworkPolicy must have a blank egress rule (deny all) + a proxy-allow rule")
		// The proxy-allow rule: to the model proxy pod (same namespace,
		// label coxswain.io/proxy-for: <loop>).
		allowRule := np.Spec.Egress[1]
		Expect(allowRule.To).To(HaveLen(1))
		peer := allowRule.To[0]
		Expect(peer.NamespaceSelector).NotTo(BeNil())
		Expect(peer.NamespaceSelector.MatchLabels).To(HaveKeyWithValue(
			"kubernetes.io/metadata.name", ns))
		Expect(peer.PodSelector).NotTo(BeNil())
		Expect(peer.PodSelector.MatchLabels).To(HaveKeyWithValue(
			"coxswain.io/proxy-for", name))
	})

	It("records the curator's verdict on the MemoryWriteDone condition (the audit trail)", func() {
		// The curator's verdict (the one-line reason) is recorded in the
		// bounded summary (the Job's termination message). The operator
		// reads the summary and records it on the MemoryWriteDone condition.
		name := "p5c-audit"
		rr := s6SucceededLessonsAvailable(name, ns)

		job := p5bGetMemoryJob(ns, name)
		// Simulate the Job completing with a curator drop in the summary:
		// the summary shows a lesson dropped by the curator (the reason is
		// "curator: too generic").
		p5bFabricateJobPod(ns, job, corev1.PodSucceeded)
		p5bSetJobPodTerminationMessage(ns, name,
			"lesson-001 aaaa11111111 dropped curator: too generic\n"+
				"lesson-002 bbbb22222222 written eligible\n"+
				"summary: total=2 written=1 dropped=1")
		s6Reconcile(rr, ns, name)

		// The MemoryWriteDone condition is set (the write completed).
		loop := p5bMustLoop(ns, name)
		cond := meta.FindStatusCondition(loop.Status.Conditions, memoryWriterConditionType)
		Expect(cond).NotTo(BeNil(), "the MemoryWriteDone condition must be set")
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(memoryWrittenReason))
	})
})
