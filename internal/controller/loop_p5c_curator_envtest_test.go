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
	"encoding/json"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	coxs "github.com/papawattu/coxswain/api/v1alpha1"
)

var _ = Describe("P5c memory curator (Phase 5, ADR-0002)", func() {
	var (
		ns string
	)

	BeforeEach(func() {
		ns = "p5c-" + nowSuffix()
		Expect(k8sClient.Create(context.Background(), &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
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
		// A Loop with no model (no modelEndpoint): the memory-writer Job does
		// NOT get the curator env vars (the curator is disabled; the writer
		// fails closed and drops every survivor as "curator unavailable" —
		// D70: no lesson is written without a true curator verdict).
		//
		// This spec tests buildMemoryWriterJob directly (not the full
		// reconcile path) to isolate the env var logic.
		name := "p5c-nomodel"
		loop := &coxs.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxs.LoopSpec{
				Goal:  "a test goal",
				Agent: coxs.AgentConfig{
					// No ModelEndpoint (no model): the curator is disabled.
				},
			},
		}
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
		job := r.buildMemoryWriterJob(loop, "memory-pvc", "succeeded")
		c := job.Spec.Template.Spec.Containers[0]
		envNames := map[string]string{}
		for _, e := range c.Env {
			envNames[e.Name] = e.Value
		}
		_, hasCurator := envNames["COX_CURATOR_BASE_URL"]
		Expect(hasCurator).To(BeFalse(),
			"the Job must NOT carry COX_CURATOR_BASE_URL when there is no model endpoint (the curator is disabled; the writer fails closed)")
	})

	It("sets the curator env vars when the Loop has a model endpoint and NO key Secret (D70 fail-closed)", func() {
		// P1-1 (D70): a Loop whose model needs no key (no endpointSecretRef,
		// e.g. a homelab vLLM) STILL gets the curator env vars. The curator
		// gate must not fail open: the writer drops every survivor as
		// "curator unavailable" only when the curator is genuinely absent
		// (no model endpoint), not when there is simply no key Secret.
		name := "p5c-nokey"
		loop := &coxs.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxs.LoopSpec{
				Goal: "a test goal",
				Agent: coxs.AgentConfig{
					ModelEndpoint: s3ModelEndpoint,
					Model:         "test-model",
					// NO EndpointSecretRef (the model is keyless).
				},
			},
		}
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
		job := r.buildMemoryWriterJob(loop, "memory-pvc", "succeeded")
		c := job.Spec.Template.Spec.Containers[0]
		envNames := map[string]string{}
		for _, e := range c.Env {
			envNames[e.Name] = e.Value
		}
		Expect(envNames).To(HaveKey("COX_CURATOR_BASE_URL"),
			"the Job must carry COX_CURATOR_BASE_URL when there is a model endpoint, even without a key Secret (D70: the curator gate must not fail open)")
		Expect(envNames["COX_CURATOR_BASE_URL"]).To(ContainSubstring(name),
			"the curator base URL must reference the Loop's model proxy")
		Expect(envNames).To(HaveKey("COX_MODEL"),
			"the Job must carry COX_MODEL (the model name for the curator call)")
	})

	It("allows egress to the model proxy pod in the memory-writer NetworkPolicy", func() {
		// The P5c NetworkPolicy allows egress to the model proxy pod (the
		// curator call) while denying all other egress.
		name := "p5c-netpol"
		_ = s6SucceededLessonsAvailable(name, ns)

		np := &networkingv1.NetworkPolicy{}
		npName := name + "-memory-write-netpol"
		Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: npName, Namespace: ns}, np)).To(Succeed())

		// The desired policy (F16 + P5c): NO ingress rules (deny-all ingress)
		// and EXACTLY TWO egress rules — the proxy-allow (the curator call)
		// + the DNS egress rule (the curator URL is a DNS name that must be
		// resolved — P1-3). A blank egress rule {} would match everything —
		// ALLOW all — the opposite of the ADR-0007 fence.
		Expect(np.Spec.Ingress).To(BeEmpty(),
			"the NetworkPolicy must have NO ingress rules (deny-all ingress)")
		Expect(np.Spec.Egress).To(HaveLen(2),
			"the NetworkPolicy must have EXACTLY TWO egress rules (the proxy-allow + DNS; a blank rule {} would allow all)")
		// The proxy-allow rule: to the model proxy pod (same namespace,
		// label coxswain.io/proxy-for: <loop>).
		allowRule := np.Spec.Egress[0]
		Expect(allowRule.To).To(HaveLen(1))
		peer := allowRule.To[0]
		Expect(peer.NamespaceSelector).NotTo(BeNil())
		Expect(peer.NamespaceSelector.MatchLabels).To(HaveKeyWithValue(
			"kubernetes.io/metadata.name", ns))
		Expect(peer.PodSelector).NotTo(BeNil())
		Expect(peer.PodSelector.MatchLabels).To(HaveKeyWithValue(
			"coxswain.io/proxy-for", name))
		// The DNS egress rule: UDP+TCP 53 to kube-system k8s-app=kube-dns
		// (P1-3: the curator URL is a DNS name that must be resolved).
		dnsRule := np.Spec.Egress[1]
		Expect(dnsRule.To).To(HaveLen(1))
		Expect(dnsRule.To[0].NamespaceSelector).ToNot(BeNil())
		Expect(dnsRule.To[0].NamespaceSelector.MatchLabels).To(HaveKeyWithValue(
			"kubernetes.io/metadata.name", "kube-system"))
		Expect(dnsRule.Ports).To(HaveLen(2), "DNS is UDP 53 + TCP 53")
	})

	It("records the curator's verdict on the MemoryWriteDone condition (the audit trail)", func() {
		// The curator's verdict (the one-line reason) is recorded in the
		// bounded summary (the Job's termination message). The operator
		// reads the summary and records it on the MemoryWriteDone condition
		// AND emits a Normal Event (item 7, the Phase 5 done-when
		// PLAN.md:85: `kubectl cox describe` shows which lessons were
		// written and why). A FakeRecorder makes the Event text assertable.
		name := "p5c-audit"
		_ = s6SucceededLessonsAvailable(name, ns)

		job := p5bGetMemoryJob(ns, name)
		// Simulate the Job completing with a curator drop in the summary:
		// the summary shows a lesson dropped by the curator (the reason is
		// "curator: too generic").
		p5bFabricateJobPod(ns, job, corev1.PodSucceeded)
		p5bSetJobPodTerminationMessage(ns, name,
			"lesson-001 aaaa11111111 dropped curator: too generic\n"+
				"lesson-002 bbbb22222222 written eligible\n"+
				"summary: total=2 written=1 dropped=1")

		// A reconciler with a FakeRecorder (the Event is emitted in this
		// reconcile).
		recorder := record.NewFakeRecorder(64)
		rec := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient, Recorder: recorder}
		s6Reconcile(rec, ns, name)

		// The MemoryWriteDone condition is set (the write completed) and its
		// message carries the audit trail (the per-lesson verdicts: the
		// sha-prefix, written|dropped, and the reason — the curator's
		// verdict).
		loop := p5bMustLoop(ns, name)
		cond := meta.FindStatusCondition(loop.Status.Conditions, memoryWriterConditionType)
		Expect(cond).NotTo(BeNil(), "the MemoryWriteDone condition must be set")
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(memoryWrittenReason))
		Expect(cond.Message).To(ContainSubstring("audit:"),
			"the condition message must carry the audit trail (item 7)")
		Expect(cond.Message).To(ContainSubstring("lesson-001 aaaa11111111 dropped curator: too generic"),
			"the condition must record the dropped lesson's sha-prefix + curator verdict")
		Expect(cond.Message).To(ContainSubstring("lesson-002 bbbb22222222 written eligible"),
			"the condition must record the written lesson")
		Expect(cond.Message).To(ContainSubstring("summary: total=2 written=1 dropped=1"),
			"the condition must record the counts line")

		// The Normal Event carries the same audit text (the transient signal;
		// the condition is the durable record).
		events := drainEvents(recorder)
		auditEventSeen := false
		for _, e := range events {
			if strings.Contains(e, "Normal MemoryWritten") && strings.Contains(e, "memory write audit") {
				auditEventSeen = true
			}
		}
		Expect(auditEventSeen).To(BeTrue(),
			"a Normal MemoryWritten audit Event must be emitted (item 7)")
	})

	It("emits the audit Event exactly once per terminal write (a re-reconcile does not re-emit it)", func() {
		// Item 7 (no Event spam): the audit Event fires only when the
		// MemoryWriteDone condition's reason transitions. A second reconcile
		// of the same terminal outcome must NOT re-emit the Event (the
		// condition is unchanged — setCondition is a no-op, the reason did
		// not change).
		name := "p5c-audit-once"
		_ = s6SucceededLessonsAvailable(name, ns)
		job := p5bGetMemoryJob(ns, name)
		p5bFabricateJobPod(ns, job, corev1.PodSucceeded)
		p5bSetJobPodTerminationMessage(ns, name,
			"lesson-001 aaaa11111111 written eligible\nsummary: total=1 written=1 dropped=0")
		recorder := record.NewFakeRecorder(64)
		rec := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient, Recorder: recorder}
		s6Reconcile(rec, ns, name)
		// First reconcile: exactly one audit Event.
		events1 := drainEvents(recorder)
		Expect(events1).To(HaveLen(1), "the first terminal reconcile emits exactly one audit Event")
		Expect(events1[0]).To(ContainSubstring("Normal MemoryWritten"))
		Expect(events1[0]).To(ContainSubstring("memory write audit"))
		// Second reconcile: the condition is unchanged — NO new Event.
		s6Reconcile(rec, ns, name)
		events2 := drainEvents(recorder)
		Expect(events2).To(BeEmpty(), "a re-reconcile of the same terminal outcome must not re-emit the audit Event")
	})
})

var _ = Describe("P5c negative lesson (Phase 5, PLAN-PHASE5.md slice P5c)", func() {
	var (
		r  *LoopReconciler
		ns string
	)

	BeforeEach(func() {
		ns = "p5c-neg-" + nowSuffix()
		Expect(k8sClient.Create(context.Background(), &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
		r = &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(context.Background(), &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
	})

	It("seeds the negative lesson (COX_SEEDED_LESSONS) on a terminal Failed:Stalled Loop with stallAction=Fail (the default)", func() {
		// A terminal Failed:Stalled stall (stallAction=Fail — the default):
		// the operator seeds a negative lesson (the stall signature — the
		// repeated verify-failure hash, NOT the agent's text). The seed is
		// passed to the Job via COX_SEEDED_LESSONS (a JSON array) so the Job
		// prepends it to the runner's lessons (the seed flows through the
		// static rules + the curator exactly like the runner's lessons).
		name := "p5c-stall-fail"
		rr := s6FailedStalledLessonsAvailable(name, ns)
		// The reconcile that creates the memory-writer Job runs in the
		// terminal phase's handler (the Failed:Stalled trigger).
		s6Reconcile(rr, ns, name)
		job := p5bGetMemoryJob(ns, name)
		c := job.Spec.Template.Spec.Containers[0]
		envNames := map[string]string{}
		for _, e := range c.Env {
			envNames[e.Name] = e.Value
		}
		// The seed env var is set (the Loop is a terminal Failed:Stalled stall
		// with stallAction=Fail — the default on).
		Expect(envNames).To(HaveKey("COX_SEEDED_LESSONS"),
			"the Job must carry COX_SEEDED_LESSONS on a terminal Failed:Stalled stall (stallAction=Fail)")
		var seeded []string
		Expect(json.Unmarshal([]byte(envNames["COX_SEEDED_LESSONS"]), &seeded)).To(Succeed(),
			"COX_SEEDED_LESSONS must be a JSON array of single-line strings")
		Expect(seeded).To(HaveLen(1), "the seed must be a single lesson (the stall signature)")
		// The seed text is the stall signature (the repeated verify-failure
		// hash) — NOT the agent's text. It references the verify-result file
		// path (the static write policy admits it: a file-path reference).
		Expect(seeded[0]).To(ContainSubstring("aaaaaaaaaaaa"),
			"the seed must carry the stall signature (the repeated verify-failure hash, 12-char prefix)")
		Expect(seeded[0]).To(ContainSubstring(".coxswain/verify-result.json"),
			"the seed must reference the verify-result file path (the static write policy admits it)")
		Expect(seeded[0]).To(HavePrefix("stall: repeated verify failure"),
			"the seed must be operator-composed (the 'stall: repeated verify failure' marker)")
	})

	It("does NOT seed the negative lesson (no COX_SEEDED_LESSONS) on a terminal Failed:Stalled Loop with stallAction=Continue", func() {
		// A terminal Failed:Stalled stall with stallAction=Continue: the
		// operator does NOT seed a negative lesson (the plan: "default on for
		// Fail/Pause, off for Continue"). The Job's COX_SEEDED_LESSONS is
		// absent (the Job sees no seed).
		name := "p5c-stall-continue"
		_ = s6FailedStalledLessonsAvailable(name, ns)
		// Override the Loop's stallAction to Continue (the plan: off for
		// Continue — even a terminal Failed:Stalled Loop under a Continue
		// stall does not seed).
		loop := p5bMustLoop(ns, name)
		loop.Spec.Loop.StallAction = coxs.StallActionContinue
		Expect(k8sClient.Update(context.Background(), loop)).To(Succeed())
		s6Reconcile(r, ns, name)
		job := p5bGetMemoryJob(ns, name)
		c := job.Spec.Template.Spec.Containers[0]
		for _, e := range c.Env {
			Expect(e.Name).ToNot(Equal("COX_SEEDED_LESSONS"),
				"the Job must NOT carry COX_SEEDED_LESSONS on a Continue stall (the plan: off for Continue)")
		}
	})
})
