package controller

import (
	"context"
	"errors"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// F73 (P3, pi3 .samples/prereview-229.md N2/N3/N5): the memory-writer outcome
// decision read the Job's pod BEFORE the Job's own conditions, so
//
//	(a) a Job the batch controller failed (DeadlineExceeded when
//	    activeDeadlineSeconds kills a pod that never became terminal,
//	    BackoffLimitExceeded, …) with a pod still Pending/Running hot-looped at
//	    the requeue interval and never recorded MemoryWriteDone;
//	(b) a pods/list error short-circuited before any Job condition was read, so
//	    a finished Job was never decided while the list kept failing;
//	(c) the Warning Event was emitted unconditionally (only the metric was gated
//	    on the condition transition), so every re-reconcile re-emitted it.
//
// The decision now reads the Job's Failed condition first, a pod read error
// falls through to the Job's conditions, and both the metric and the Event are
// gated on the transition. envtest has no batch controller, so the specs stamp
// the Job conditions themselves (the same conditions the batch controller
// stamps); the pod is fabricated with p5bFabricateJobPod (which the memory-writer
// specs already use).
var _ = Describe("F73 memory-writer outcome decision", func() {
	var ns string

	BeforeEach(func() {
		ns = "f73-" + nowSuffix()
		Expect(k8sClient.Create(context.Background(), &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(context.Background(), &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
	})

	It("decides MemoryWriteFailed from a Failed/DeadlineExceeded Job while its pod is still Running, and emits ONE Warning Event", func() {
		// F73(a): the pod never reaches a terminal phase (the deadline killed a
		// pod stuck in CreateContainerConfigError, so the pod object is still
		// there), but the Job IS over. Deciding from the pod first requeues for
		// ever: the assertion below (a recorded condition) is what the pre-fix
		// code never reaches.
		name := "f73-mw-deadline-running"
		r := s6SucceededLessonsAvailable(name, ns)   // drives to Succeeded + creates the writer Job
		res := p5bMemoryWriterReconcile(r, ns, name) // creates the Job (the lessons gate is open)
		Expect(res.RequeueAfter).To(BeNumerically(">", 0), "the memory-writer Job's create must requeue")
		job := p5bGetMemoryJob(ns, name)

		f73StampJobFailed(ns, job.Name, batchv1.JobReasonDeadlineExceeded)
		// The pod is still RUNNING: the deadline killed a pod that never became
		// terminal (the hung-writer-pod case the F53 deadline exists for).
		p5bFabricateJobPod(ns, job, corev1.PodRunning)

		rec := record.NewFakeRecorder(64)
		r.Recorder = rec

		s6Reconcile(r, ns, name)
		// The second reconcile is the Event-gate assertion (F73(c)): the outcome
		// is already recorded, so nothing may be emitted again.
		s6Reconcile(r, ns, name)

		loop := p5bMustLoop(ns, name)
		cond := meta.FindStatusCondition(loop.Status.Conditions, memoryWriterConditionType)
		Expect(cond).NotTo(BeNil(), "a Failed Job must be decided even while its pod is not terminal")
		Expect(cond.Reason).To(Equal(memoryWriteFailedReason))
		Expect(cond.Message).To(ContainSubstring("activeDeadlineSeconds"),
			"the deadline message names the deadline")
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded),
			"a failed memory write does not fail the Loop (ADR-0004)")

		events := f73DrainWarningEvents(rec)
		Expect(events).To(HaveLen(1), "exactly one Warning Event across two reconciles; got %v", events)
		Expect(events[0]).To(ContainSubstring("activeDeadlineSeconds"),
			"the Event names the deadline too, not just the condition")
	})

	It("decides from the Job's conditions when the pod list fails (a read error is not a decision)", func() {
		// F73(b): a pods/list error must not short-circuit the Job-condition
		// read. Before the fix the read error requeued and the finished Job was
		// never recorded for as long as the list kept failing.
		name := "f73-mw-listfail"
		r := s6SucceededLessonsAvailable(name, ns)
		res := p5bMemoryWriterReconcile(r, ns, name)
		Expect(res.RequeueAfter).To(BeNumerically(">", 0), "the memory-writer Job's create must requeue")
		job := p5bGetMemoryJob(ns, name)

		f73StampJobFailed(ns, job.Name, batchv1.JobReasonBackoffLimitExceeded)
		// The pod read (a labelled List through the reconciler's apiReader)
		// fails; every other read is unaffected.
		r.apiReader = f73FailingPodListReader{Client: k8sClient}

		s6Reconcile(r, ns, name)

		loop := p5bMustLoop(ns, name)
		cond := meta.FindStatusCondition(loop.Status.Conditions, memoryWriterConditionType)
		Expect(cond).NotTo(BeNil(), "a terminal Job must be decided even when the pod list fails")
		Expect(cond.Reason).To(Equal(memoryWriteFailedReason))
		Expect(cond.Message).To(ContainSubstring(batchv1.JobReasonBackoffLimitExceeded),
			"the message names the Job's reason")
	})

	It("makes no decision for a Complete Job whose pod is still Pending (the I49 control)", func() {
		// The other half of the same gate: reading the Job's conditions first
		// must not decide a Job that has not finished. A Complete Job whose pod
		// is not terminal yet (envtest has no pod lifecycle) is the control:
		// the pre-fix code requeues here and so does this one, so this spec
		// passes both before and after the fix — it is the guard against
		// over-deciding, not a detector of F73.
		name := "f73-mw-complete-pending"
		r := s6SucceededLessonsAvailable(name, ns)
		res := p5bMemoryWriterReconcile(r, ns, name)
		Expect(res.RequeueAfter).To(BeNumerically(">", 0), "the memory-writer Job's create must requeue")
		job := p5bGetMemoryJob(ns, name)

		f73StampJobComplete(ns, job.Name)
		p5bFabricateJobPod(ns, job, corev1.PodPending)

		s6Reconcile(r, ns, name)
		s6Reconcile(r, ns, name)

		loop := p5bMustLoop(ns, name)
		Expect(meta.FindStatusCondition(loop.Status.Conditions, memoryWriterConditionType)).To(BeNil(),
			"a Complete Job whose pod is still Pending makes no decision")
	})
})

// f73StampJobFailed stamps the Job's Failed condition (the batch controller's
// terminal record; envtest has no batch controller).
func f73StampJobFailed(ns, jobName, reason string) {
	ctx := context.Background()
	job := &batchv1.Job{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: jobName, Namespace: ns}, job)).To(Succeed())
	now := metav1.Now()
	job.Status.StartTime = &now
	job.Status.Failed = 1
	job.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, Reason: "FailureTarget"},
		{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: reason,
			Message: "Job has reached the specified backoff limit"},
	}
	Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
}

// f73StampJobComplete stamps the Job's Complete condition.
func f73StampJobComplete(ns, jobName string) {
	ctx := context.Background()
	job := &batchv1.Job{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: jobName, Namespace: ns}, job)).To(Succeed())
	now := metav1.Now()
	job.Status.StartTime = &now
	job.Status.CompletionTime = &now
	job.Status.Succeeded = 1
	job.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, Reason: "CompletionsReached"},
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, Reason: "CompletionsReached"},
	}
	Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
}

// f73DrainWarningEvents returns the Warning Events the recorder has emitted so
// far, without blocking (the FakeRecorder buffers into a channel).
func f73DrainWarningEvents(rec *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			if strings.HasPrefix(e, corev1.EventTypeWarning) {
				out = append(out, e)
			}
		default:
			return out
		}
	}
}

// f73FailingPodListReader is the reconciler's apiReader with the
// memory-writer pod list failing (F73(b)): the operator lists the Job's pod
// with the memory-write-for label, and only that List errors — the deliver and
// verify pod lists carry their own labels, so they keep working, and every
// non-pod read (the Job, the Loop, the status update) goes to the real client.
type f73FailingPodListReader struct{ client.Client }

func (f f73FailingPodListReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*corev1.PodList); ok {
		lo := &client.ListOptions{}
		for _, o := range opts {
			o.ApplyToList(lo)
		}
		if lo.LabelSelector != nil && strings.Contains(lo.LabelSelector.String(), memoryWriteForLabelKey) {
			return errors.New("injected memory-writer pod list failure")
		}
	}
	return f.Client.List(ctx, list, opts...)
}
