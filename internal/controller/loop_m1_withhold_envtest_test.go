// M1 (D140, docs/PLAN-PHASE8.md via #485): a Loop the Judge flags (verdict
// false, verdict nil = Unknown, or status.judge.truncated) writes NO
// shared-memory lessons. On a qualifying terminal outcome whose Judge record
// flags the Loop, the operator creates NO memory-writer Job and records
// MemoryWriteDone with the LessonsWithheld reason (the durable record:
// cox describe shows why no write happened) + an Event naming the withhold.
// A Judge record that does NOT flag the Loop (verdict true, no truncation)
// leaves the memory write untouched (the M1 step does not withhold a clean
// Loop's lessons). This is the RED spec (step 1 of 2): with the
// judgeWithholdsLessons stub, the false case records MemoryWriteSkipped
// (the Job is never created, but the condition reason and the Event are
// wrong) and the true case creates the Job — so both specs must fail.
package controller

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

var _ = Describe("M1 (D140): a Loop the Judge flags writes no shared-memory lessons", func() {
	var ns string
	var tv, fv bool

	BeforeEach(func() {
		tv, fv = true, false
		ns = "m1mw-" + nowSuffix()
		Expect(k8sClient.Create(context.Background(), &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(context.Background(), &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
	})

	// m1MwSucceededForced drives a fresh Loop through the standard claim
	// path (Planning -> Implementing with the lessonsAvailable gate open),
	// then forces the qualifying terminal outcome the memory-writer gate
	// sees: Phase/DesiredPhase Succeeded + a Succeeded condition
	// (VerifyPassed) + status.progress.LessonsAvailable = true + the Judge
	// record (judge "false" = a flagged verdict; judge "true" = a clean
	// verdict). The forced terminal state is stable (a terminal phase is not
	// re-derived), so the following reconcile runs the M1 step against it.
	m1MwSucceededForced := func(name, judge string) *LoopReconciler {
		ctx := context.Background()
		r := &LoopReconciler{
			CNIProber: testCNIProber(),
			Client:    k8sClient,
			Scheme:    k8sClient.Scheme(),
			apiReader: k8sClient,
			Recorder:  record.NewFakeRecorder(20),
		}
		s5aEnsureSandbox(ns, name)
		loop := s6Loop(name, ns, inClusterRepoURL)
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		s6Reconcile(r, ns, name)
		loop = p5bMustLoop(ns, name)
		loop.Status.BaseCommit = s5aBaseCommit
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		// Planning claim.
		s5aClaimPod(ns, name, "Planning", "")
		s6Reconcile(r, ns, name)
		s5aEnsureSandbox(ns, name) // the annotation recycle deleted the Sandbox
		// Implementing claim with the headCommit + lessonsAvailable=true (the
		// P5b gate input, so the M1 step is reached at all).
		s5aClaimPodWithLessonsAvailable(ns, name, "Implementing", s6HeadCommit, true)
		s6Reconcile(r, ns, name)
		d61ReadyVerifyProxy(r, ns, name)
		s5aVerifyPod(ctx, ns, name, name+"-verify-1", 0)
		s6Reconcile(r, ns, name)
		// Force the qualifying terminal outcome (Succeeded) + the Judge
		// record (the flag under test).
		loop = p5bMustLoop(ns, name)
		loop.Status.Phase = coxv1alpha1.LoopPhaseSucceeded
		loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseSucceeded
		setCondition(loop, string(coxv1alpha1.LoopPhaseSucceeded), metav1.ConditionTrue, "VerifyPassed", "acceptance checks passed")
		loop.Status.Progress = &coxv1alpha1.ProgressStatus{
			Phase:            coxv1alpha1.LoopPhaseSucceeded,
			LessonsAvailable: true,
		}
		switch judge {
		case "false":
			loop.Status.Judge = &coxv1alpha1.JudgeStatus{Verdict: &fv, Score: 0, Reason: "gaming"}
		case "true":
			loop.Status.Judge = &coxv1alpha1.JudgeStatus{Verdict: &tv, Score: 100, Reason: "ok"}
		}
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		return r
	}

	It("m1-mw-false: a flagged Loop (verdict false) gets NO memory-writer Job and records LessonsWithheld", func() {
		name := "m1-mw-false"
		r := m1MwSucceededForced(name, "false")
		recorder := r.Recorder.(*record.FakeRecorder)

		// Reconcile: the operator sees the qualifying terminal outcome whose
		// Judge record flags the Loop. D140: it writes NO lessons.
		s6Reconcile(r, ns, name)

		// No memory-writer Job is created.
		job := &batchv1.Job{}
		err := k8sClient.Get(context.Background(), types.NamespacedName{
			Name: memoryWriterJobName(p5bMustLoop(ns, name)), Namespace: ns,
		}, job)
		Expect(err).To(HaveOccurred(), "no memory-writer Job for a Loop the Judge flagged (verdict false — D140)")

		// The MemoryWriteDone condition carries the LessonsWithheld reason
		// (the durable record: cox describe shows why no write happened).
		loop := p5bMustLoop(ns, name)
		cond := meta.FindStatusCondition(loop.Status.Conditions, memoryWriterConditionType)
		Expect(cond).NotTo(BeNil(), "the MemoryWriteDone condition must be set")
		Expect(cond.Reason).To(Equal(lessonsWithheldReason))

		// The Event names the withhold (the audit trail): one of the events
		// on the fake recorder contains the LessonsWithheld reason.
		var seen []string
		for {
			select {
			case ev := <-recorder.Events:
				seen = append(seen, ev)
			default:
			}
			break
		}
		found := false
		for _, ev := range seen {
			if strings.Contains(ev, "LessonsWithheld") {
				found = true
				break
			}
		}
		Expect(found).To(BeTrue(), "a LessonsWithheld Event must be recorded (the withhold is in the audit trail): %v", seen)
	})

	It("m1-mw-true: a clean Loop (verdict true, not truncated) is NOT withheld by the M1 step", func() {
		name := "m1-mw-true"
		r := m1MwSucceededForced(name, "true")

		// Reconcile: the M1 step sees a Judge record that does not flag the
		// Loop — it must not withhold the write.
		s6Reconcile(r, ns, name)

		// The MemoryWriteDone condition is either absent or its reason is
		// not LessonsWithheld.
		loop := p5bMustLoop(ns, name)
		cond := meta.FindStatusCondition(loop.Status.Conditions, memoryWriterConditionType)
		if cond != nil {
			Expect(cond.Reason).NotTo(Equal(lessonsWithheldReason),
				"a Loop the Judge did not flag must not be recorded as LessonsWithheld")
		}
	})
})
