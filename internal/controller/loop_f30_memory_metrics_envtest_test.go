// F30 (PLAN.md Phase 7 "memory-write rate"): envtest specs for
// coxswain_memory_writes_total{outcome} and coxswain_memory_lessons_total
// {verdict}. Each counter moves once per Loop, on the transition that sets
// the MemoryWriteDone condition, and never on an in-progress Job or on a
// re-reconcile of an already-recorded outcome.
package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// f30Summary is a writer summary (cmd/memory-writer summaryLines format):
// 2 kept (one with a curator-keep reason), 1 curator drop, 1 curator
// unavailable, 2 static drops (a rule and a duplicate).
const f30Summary = `lesson-001 aaaaaaaaaaaa written eligible
lesson-002 bbbbbbbbbbbb written curator-keep: a real gotcha
lesson-003 cccccccccccc dropped curator: restates the goal
lesson-004 dddddddddddd dropped curator unavailable
lesson-005 eeeeeeeeeeee dropped secrets-pattern
lesson-006 ffffffffffff dropped duplicate
summary: total=6 written=2 dropped=4`

// f30Snapshot is the six series this slice moves.
type f30Snapshot struct{ written, failed, kept, static, curator float64 }

// f30Counter reads one labelled counter series from the manager's registry
// (0 when the series does not exist yet).
func f30Counter(name, label, val string) float64 {
	mfs, err := metrics.Registry.Gather()
	Expect(err).NotTo(HaveOccurred())
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == label && lp.GetValue() == val {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

func f30Read() f30Snapshot {
	g := func(name, label, val string) float64 {
		return f30Counter(name, label, val)
	}
	return f30Snapshot{
		written: g("coxswain_memory_writes_total", "outcome", "written"),
		failed:  g("coxswain_memory_writes_total", "outcome", "failed"),
		kept:    g("coxswain_memory_lessons_total", "verdict", "kept"),
		static:  g("coxswain_memory_lessons_total", "verdict", "static-drop"),
		curator: g("coxswain_memory_lessons_total", "verdict", "curator-drop"),
	}
}

var _ = Describe("F30 memory-write metrics", func() {
	var ns string

	BeforeEach(func() {
		ns = "f30-" + nowSuffix()
		Expect(k8sClient.Create(context.Background(), &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(context.Background(), &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
	})

	// f30Touch changes an input the memory writer does not read (a Loop
	// annotation) so the next reconcile is a genuine re-reconcile.
	f30Touch := func(name string) {
		loop := p5bMustLoop(ns, name)
		if loop.Annotations == nil {
			loop.Annotations = map[string]string{}
		}
		loop.Annotations["f30/touch"] = nowSuffix()
		Expect(k8sClient.Update(context.Background(), loop)).To(Succeed())
	}

	It("bumps written and the summary's verdict counts once, and not again on a re-reconcile", func() {
		name := "f30-written"
		r := s6SucceededLessonsAvailable(name, ns)
		p5bMemoryWriterReconcile(r, ns, name) // creates the Job
		job := p5bGetMemoryJob(ns, name)
		p5bFabricateJobPod(ns, job, corev1.PodSucceeded)
		p5bSetJobPodTerminationMessage(ns, name, f30Summary)

		before := f30Read()
		s6Reconcile(r, ns, name) // records MemoryWritten
		after := f30Read()
		Expect(after.written).To(Equal(before.written+1), "written bumps once")
		Expect(after.failed).To(Equal(before.failed))
		Expect(after.kept).To(Equal(before.kept+2), "kept = the two written lines")
		Expect(after.curator).To(Equal(before.curator+2), "curator drop + curator unavailable")
		Expect(after.static).To(Equal(before.static+2), "a rule drop + a duplicate")

		// Re-reconcile twice, with an unrelated input changed in between.
		s6Reconcile(r, ns, name)
		f30Touch(name)
		s6Reconcile(r, ns, name)
		Expect(f30Read()).To(Equal(after), "no counter moves on a re-reconcile")
	})

	It("bumps failed once, no lessons, and not again on a re-reconcile", func() {
		name := "f30-failed"
		r := s6SucceededLessonsAvailable(name, ns)
		p5bMemoryWriterReconcile(r, ns, name)
		job := p5bGetMemoryJob(ns, name)
		p5bFabricateJobPod(ns, job, corev1.PodFailed)
		p5bSetJobPodTerminationMessage(ns, name, f30Summary) // must be ignored on failure

		before := f30Read()
		s6Reconcile(r, ns, name)
		after := f30Read()
		Expect(after.failed).To(Equal(before.failed + 1))
		Expect(after.written).To(Equal(before.written))
		Expect(after.kept).To(Equal(before.kept))
		Expect(after.static).To(Equal(before.static))
		Expect(after.curator).To(Equal(before.curator))

		s6Reconcile(r, ns, name)
		f30Touch(name)
		s6Reconcile(r, ns, name)
		Expect(f30Read()).To(Equal(after), "no counter moves on a re-reconcile")
	})

	It("bumps nothing while the Job is in progress (no pod, Pending, Running)", func() {
		name := "f30-progress"
		r := s6SucceededLessonsAvailable(name, ns)
		p5bMemoryWriterReconcile(r, ns, name) // creates the Job; no pod yet
		before := f30Read()

		s6Reconcile(r, ns, name) // Job, no pod
		Expect(f30Read()).To(Equal(before), "Job with no pod")

		job := p5bGetMemoryJob(ns, name)
		for _, phase := range []corev1.PodPhase{corev1.PodPending, corev1.PodRunning} {
			p5bFabricateJobPod(ns, job, phase)
			p5bSetJobPodTerminationMessage(ns, name, f30Summary)
			s6Reconcile(r, ns, name)
			Expect(f30Read()).To(Equal(before), "pod phase %s", phase)
		}
	})
})
