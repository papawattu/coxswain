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

// S1 (D135, ADR-0005 D17.2), step 3c: the envtest the plan requires —
// "the condition, the Event and the draft follow a hit". The 3b unit specs
// call recordScanEvidence directly, so a mutation removing its CALL in
// applyVerifyOutcome breaks no test; these specs close that by driving a
// scan report THROUGH the reconciler (drive to Verifying, fake the verify
// pod's scan init termination message, reconcile, assert the condition, the
// Warning Event, judgeSummary and deliverDraft).
//
// The helper s5aVerifyPodWithScanMessage (loop_verify_job_envtest_test.go)
// fakes the verify pod with every gate and check passing and the scan's
// termination message set to the given report — the scan is advisory (exit
// 0 either way), so the pod's other statuses never change what these specs
// assert: the scan's message is the only evidence under test.
package controller

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// s1ScanHitReport is the scan's v==1 report with one subversion pattern hit
// (the shape the scanner emits; path/pattern/line are the fields
// recordScanEvidence's True case names in the condition message).
const s1ScanHitReport = `{"v":1,"files":1,"hits":[{"path":"zz_init.go","pattern":"os.Exit in init","line":10}],"errors":[]}`

// s1ScanCleanReport is the scan's v==1 report with no hits (the quiet
// steady state: a clean scan never re-Events).
const s1ScanCleanReport = `{"v":1,"files":1,"hits":[],"errors":[]}`

// s1ScanSpec is s5aLoopSpec with delivery enabled (mode PullRequest; a
// pinned verifiedCommit and a repo are present once the Loop is at
// Verifying) and draft=false — so a clean Loop would deliver a NON-draft PR
// and only the VerifySuspicious True (D135) can flip deliverDraft to true.
// The draft=false spec is the positive control for the hit case: without it
// deliverDraft's true would be the default, not the scan's decision.
func s1ScanSpec() coxv1alpha1.LoopSpec {
	spec := s5aLoopSpec()
	spec.Delivery = &coxv1alpha1.DeliveryConfig{
		Mode:  coxv1alpha1.DeliveryModePullRequest,
		Draft: func() *bool { b := false; return &b }(),
	}
	return spec
}

// s1ScanCondition returns the Loop's VerifySuspicious condition (nil when
// absent).
func s1ScanCondition(loop *coxv1alpha1.Loop) *metav1.Condition {
	for i := range loop.Status.Conditions {
		if loop.Status.Conditions[i].Type == coxv1alpha1.VerifySuspiciousCondition {
			return &loop.Status.Conditions[i]
		}
	}
	return nil
}

// s1ScanEvents drains the FakeRecorder's channel (the M1 lesson: a bare
// break in the case branch would stop after one event — the break must sit
// on the select itself so the drain loops until the channel is empty).
func s1ScanEvents(recorder *record.FakeRecorder) []string {
	out := []string{}
	for {
		select {
		case e := <-recorder.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

// s1ScanEventCount counts the drained events containing needle.
func s1ScanEventCount(events []string, needle string) int {
	n := 0
	for _, e := range events {
		if strings.Contains(e, needle) {
			n++
		}
	}
	return n
}

var _ = Describe("S1 (D135): the diff scan flags a Loop through the reconciler", func() {
	ctx := context.Background()

	// s1ScanDrive drives a fresh Loop to Verifying through the SPEC's
	// reconciler (the claim path: bootstrap -> Planning -> Implementing with
	// the 40-hex headCommit), then drains the drive's own Events so each
	// spec asserts only ITS reconcile's events.
	s1ScanDrive := func(ns, name string, r *LoopReconciler) string {
		s5aEnsureSandbox(ns, name)
		s5aReconcile(r, ns, name) // bootstrap: Pending -> Planning
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		loop.Status.BaseCommit = s5aBaseCommit
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		s5aClaimPod(ns, name, "Planning", "")
		s5aReconcile(r, ns, name)
		s5aEnsureSandbox(ns, name) // the annotation-based recycle deleted it
		s5aClaimPod(ns, name, "Implementing", s5aHeadCommit)
		s5aReconcile(r, ns, name)
		d61ReadyVerifyProxy(r, ns, name)                    // the verify Job is now created
		_ = s1ScanEvents(r.Recorder.(*record.FakeRecorder)) // drain the drive's events
		return verifyJobName(loop)
	}

	It("s1-hit: a pattern hit flags the Loop through the reconciler", func() {
		ns := "s1-hit-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "scanloop"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s1ScanSpec(),
		})).To(Succeed())

		recorder := record.NewFakeRecorder(64)
		r := &LoopReconciler{CNIProber: testCNIProber(), Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient, Recorder: recorder}
		jobName := s1ScanDrive(ns, name, r)

		// The verify pod: all checks pass; the scan reports one hit.
		s5aVerifyPodWithScanMessage(ctx, ns, name, jobName, s1ScanHitReport)

		_ = s5aReconcile(r, ns, name)
		// The re-read Loop (the plan's assertion target: a fresh Get, not the
		// reconcile's in-memory copy).
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())

		By("recording the verify decision (the scan is advisory and never blocks the checks)")
		Expect(loop.Status.Verify).NotTo(BeNil())
		Expect(loop.Status.Verify.Iteration).NotTo(BeNil())

		By("flagging VerifySuspicious=True reason SubversionPatternInDiff naming the hit")
		cond := s1ScanCondition(loop)
		Expect(cond).NotTo(BeNil(), "VerifySuspicious must be recorded through the reconciler")
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(string(coxv1alpha1.ReasonSubversionPatternInDiff)))
		Expect(cond.Message).To(ContainSubstring("zz_init.go:10"), "the message names the hit's path:line")

		By("summarizing the Judge signal as Suspicious (no Judge ran)")
		Expect(loop.Status.JudgeSummary).To(Equal("Suspicious"))

		By("emitting ONE Warning event containing SubversionPatternInDiff")
		events := s1ScanEvents(recorder)
		Expect(s1ScanEventCount(events, string(coxv1alpha1.ReasonSubversionPatternInDiff))).To(Equal(1),
			"exactly one Warning event for the True scan outcome; got: %v", events)
		Expect(s1ScanEventCount(events, "Warning")).To(BeNumerically(">", 0))

		By("forcing a draft PR whatever spec.delivery.draft says (D135)")
		Expect(deliverDraft(loop)).To(BeTrue(),
			"spec.delivery.draft=false is overridden by the VerifySuspicious True")
	})

	It("s1-clean: a clean scan is the quiet steady state", func() {
		ns := "s1-clean-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "scanloop"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s1ScanSpec(),
		})).To(Succeed())

		recorder := record.NewFakeRecorder(64)
		r := &LoopReconciler{CNIProber: testCNIProber(), Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient, Recorder: recorder}
		jobName := s1ScanDrive(ns, name, r)

		// The verify pod: all checks pass; the scan reports clean.
		s5aVerifyPodWithScanMessage(ctx, ns, name, jobName, s1ScanCleanReport)

		_ = s5aReconcile(r, ns, name)
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())

		By("recording the verify decision (the clean scan passed through the reconciler)")
		Expect(loop.Status.Verify).NotTo(BeNil())
		Expect(loop.Status.Verify.Iteration).NotTo(BeNil())

		By("recording VerifySuspicious=False reason NoSubversionPattern")
		cond := s1ScanCondition(loop)
		Expect(cond).NotTo(BeNil(), "a clean scan still records the condition (the steady state is False, not absent)")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(string(coxv1alpha1.ReasonNoSubversionPattern)))

		By("leaving judgeSummary empty (no Judge ran and nothing is suspicious)")
		Expect(loop.Status.JudgeSummary).To(BeEmpty())

		By("emitting NO SubversionPatternInDiff event (a clean scan never re-Events)")
		events := s1ScanEvents(recorder)
		Expect(s1ScanEventCount(events, string(coxv1alpha1.ReasonSubversionPatternInDiff))).To(Equal(0),
			"a clean scan is quiet; got: %v", events)

		By("leaving spec.delivery.draft exactly as set (false: a non-draft PR)")
		Expect(deliverDraft(loop)).To(BeFalse())
	})

	It("s1-garbled: an unreadable scan report is Unknown, not clean", func() {
		ns := "s1-garb-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "scanloop"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s1ScanSpec(),
		})).To(Succeed())

		recorder := record.NewFakeRecorder(64)
		r := &LoopReconciler{CNIProber: testCNIProber(), Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient, Recorder: recorder}
		jobName := s1ScanDrive(ns, name, r)

		// The verify pod: all checks pass; the scan's message is not JSON.
		s5aVerifyPodWithScanMessage(ctx, ns, name, jobName, "not json")

		_ = s5aReconcile(r, ns, name)
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())

		By("recording the verify decision (a garbled scan never blocks the checks)")
		Expect(loop.Status.Verify).NotTo(BeNil())
		Expect(loop.Status.Verify.Iteration).NotTo(BeNil())

		By("recording VerifySuspicious=Unknown reason ScanUnavailable")
		cond := s1ScanCondition(loop)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionUnknown), "a garbled message is never treated as clean")
		Expect(cond.Reason).To(Equal(string(coxv1alpha1.ReasonScanUnavailable)))

		By("emitting ONE Warning ScanUnavailable event (R26 I90: surface the degradation)")
		events := s1ScanEvents(recorder)
		Expect(s1ScanEventCount(events, string(coxv1alpha1.ReasonScanUnavailable))).To(Equal(1),
			"exactly one Warning event for the Unknown scan outcome; got: %v", events)
		Expect(s1ScanEventCount(events, "Warning")).To(BeNumerically(">", 0))
	})
})
