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

// S1 step 3b (D135, ADR-0005 D17.2): the operator's read of the scan init's
// termination message and the VerifySuspicious condition it records. Pure
// unit specs (no envtest): parseScanReport is a decoder, and recordScanEvidence
// is driven by hand-built pods (the pod is the evidence, the S3/S4
// pod-blind pattern — a fake pod carries the same status the kubelet would
// record). The decision branches (hits, idempotency, unavailable, clean) are
// in TestRecordScanEvidenceDecisions; the no-decision cases (no scan status,
// running, nil pod) are in TestRecordScanEvidenceNoDecision. The split keeps
// each test function under the gocyclo bound.
package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// scanPod builds a verify pod whose `scan` init is in the given state:
// terminated=false -> a Running (waiting) state; terminated=true with msg ->
// a Terminated state with that message ("terminated" is a sentinel for a
// terminated container with no message; nil = no scan init status at all).
func scanPod(t *testing.T, terminated bool, msg string) *corev1.Pod {
	t.Helper()
	var ics corev1.ContainerStatus
	ics.Name = verifyScanInit
	switch {
	case !terminated:
		ics.State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "Running"}}
		pod := &corev1.Pod{Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{ics}}}
		return pod
	default:
		m := msg
		if m == "terminated" {
			m = ""
		}
		ics.State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: m}}
	}
	return &corev1.Pod{Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{ics}}}
}

// TestParseScanReport covers the decoder's contract: ok only for valid JSON
// with v == 1 (a version mismatch or garbage is "scan unavailable", never
// "clean").
func TestParseScanReport(t *testing.T) {
	tests := []struct {
		name string
		msg  string
		ok   bool
		v    int
		hits int
	}{
		{"valid v1 with hits", `{"v":1,"files":3,"hits":[{"path":"a.go","pattern":"os.Exit in init","line":7}],"hitsDropped":0,"errors":[],"errorsDropped":0}`, true, 1, 1},
		{"valid v1 clean", `{"v":1,"files":3,"hits":[],"hitsDropped":0,"errors":[],"errorsDropped":0}`, true, 1, 0},
		{"v != 1 is not ok", `{"v":2,"files":3}`, false, 2, 0},
		{"v missing is not ok", `{"files":3}`, false, 0, 0},
		{"garbage is not ok", `not json`, false, 0, 0},
		{"empty is not ok", ``, false, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rep, ok := parseScanReport(tt.msg)
			if ok != tt.ok {
				t.Fatalf("parseScanReport(%q) ok = %v, want %v", tt.msg, ok, tt.ok)
			}
			if !ok {
				return
			}
			if rep.V != tt.v {
				t.Errorf("v = %d, want %d", rep.V, tt.v)
			}
			if len(rep.Hits) != tt.hits {
				t.Errorf("len(hits) = %d, want %d", len(rep.Hits), tt.hits)
			}
		})
	}
}

// TestRecordScanEvidenceNoDecision covers the "no decision yet" cases:
// no scan status at all, a running scan, and a nil pod. In every case the
// function returns false and changes nothing (no condition, no Event).
func TestRecordScanEvidenceNoDecision(t *testing.T) {
	cond := func(t *testing.T, loop *coxv1alpha1.Loop) *metav1.Condition {
		t.Helper()
		for i := range loop.Status.Conditions {
			if loop.Status.Conditions[i].Type == coxv1alpha1.VerifySuspiciousCondition {
				return &loop.Status.Conditions[i]
			}
		}
		return nil
	}

	t.Run("no scan status: no decision", func(t *testing.T) {
		rec := record.NewFakeRecorder(16)
		r := &LoopReconciler{Recorder: rec}
		loop := &coxv1alpha1.Loop{}
		// A pod whose only init is the tamper init (no scan status at all).
		pod := &corev1.Pod{Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{
			{Name: verifyTamperInit},
		}}}
		if r.recordScanEvidence(loop, pod) {
			t.Fatal("recordScanEvidence() = true, want false (no scan status)")
		}
		if got := cond(t, loop); got != nil {
			t.Fatalf("condition set: %+v, want none", got)
		}
		if n := len(rec.Events); n != 0 {
			t.Fatalf("events = %v, want none", rec.Events)
		}
	})

	t.Run("scan running: no decision", func(t *testing.T) {
		rec := record.NewFakeRecorder(16)
		r := &LoopReconciler{Recorder: rec}
		loop := &coxv1alpha1.Loop{}
		pod := scanPod(t, false, "")
		if r.recordScanEvidence(loop, pod) {
			t.Fatal("recordScanEvidence() = true, want false (scan still running)")
		}
		if got := cond(t, loop); got != nil {
			t.Fatalf("condition set: %+v, want none", got)
		}
	})

	t.Run("nil pod: no decision", func(t *testing.T) {
		rec := record.NewFakeRecorder(16)
		r := &LoopReconciler{Recorder: rec}
		loop := &coxv1alpha1.Loop{}
		if r.recordScanEvidence(loop, nil) {
			t.Fatal("recordScanEvidence(nil pod) = true, want false")
		}
		if got := cond(t, loop); got != nil {
			t.Fatalf("condition set: %+v, want none", got)
		}
	})
}

// TestRecordScanEvidenceDecisions covers recordScanEvidence's decision
// branches (D135): the hits case (True + Event + Suspicious summary), the
// idempotent second pass, the two Unknown cases (garbage message, errors
// only), the clean False case (no Event), and the nil-Recorder guard.
func TestRecordScanEvidenceDecisions(t *testing.T) {
	const hitsReport = `{"v":1,"files":2,"hits":[{"path":"main.go","pattern":"os.Exit in init","line":9},{"path":"main.go","pattern":"zzz_canary","line":41},{"path":"util.go","pattern":"//go:linkname","line":3},{"path":"x.go","pattern":"testing.Testing()","line":50}],"hitsDropped":1,"errors":[],"errorsDropped":0}`
	const cleanReport = `{"v":1,"files":4,"hits":[],"hitsDropped":0,"errors":[],"errorsDropped":0}`

	cond := func(t *testing.T, loop *coxv1alpha1.Loop) *metav1.Condition {
		t.Helper()
		for i := range loop.Status.Conditions {
			if loop.Status.Conditions[i].Type == coxv1alpha1.VerifySuspiciousCondition {
				return &loop.Status.Conditions[i]
			}
		}
		return nil
	}
	fresh := func() (*LoopReconciler, *corev1.Pod, *coxv1alpha1.Loop, *record.FakeRecorder) {
		rec := record.NewFakeRecorder(16)
		r := &LoopReconciler{Recorder: rec}
		loop := &coxv1alpha1.Loop{Status: coxv1alpha1.LoopStatus{
			CurrentVerify: &coxv1alpha1.CurrentVerifyStatus{VerifiedCommit: "abcdef1234567890"},
		}}
		return r, scanPod(t, true, hitsReport), loop, rec
	}

	t.Run("hits: True + one Event + Suspicious summary (judge nil)", func(t *testing.T) {
		r, pod, loop, rec := fresh()
		if !r.recordScanEvidence(loop, pod) {
			t.Fatal("recordScanEvidence() = false, want true")
		}
		c := cond(t, loop)
		if c == nil {
			t.Fatal("no VerifySuspicious condition")
		}
		if c.Status != metav1.ConditionTrue || c.Reason != string(coxv1alpha1.ReasonSubversionPatternInDiff) {
			t.Fatalf("condition = %s/%s, want True/SubversionPatternInDiff", c.Status, c.Reason)
		}
		// 5 patterns (4 hits + 1 dropped), the 7-char commit, the first 3 hits.
		want := "5 pattern(s) in the diff at abcdef1: os.Exit in init in main.go:9, zzz_canary in main.go:41, //go:linkname in util.go:3, …"
		if c.Message != want {
			t.Fatalf("message = %q, want %q", c.Message, want)
		}
		if n := len(rec.Events); n != 1 {
			t.Fatalf("events = %v, want exactly one", rec.Events)
		}
		// D135: a hit with NO Judge still reads Suspicious (judgeSummary's S1
		// slot beats the absence of status.judge).
		if loop.Status.JudgeSummary != "Suspicious" {
			t.Fatalf("judgeSummary = %q, want Suspicious", loop.Status.JudgeSummary)
		}
	})

	t.Run("same pod again: no second Event, no change", func(t *testing.T) {
		r, pod, loop, rec := fresh()
		if !r.recordScanEvidence(loop, pod) {
			t.Fatal("first pass: want changed")
		}
		if n := len(rec.Events); n != 1 {
			t.Fatalf("first pass events = %v, want one", rec.Events)
		}
		if r.recordScanEvidence(loop, scanPod(t, true, hitsReport)) {
			t.Fatal("second pass: changed = true, want false (steady state)")
		}
		if n := len(rec.Events); n != 1 {
			t.Fatalf("second pass events = %v, want still one (no re-Event)", rec.Events)
		}
	})

	t.Run("clean: False and no Event", func(t *testing.T) {
		rec := record.NewFakeRecorder(16)
		r := &LoopReconciler{Recorder: rec}
		loop := &coxv1alpha1.Loop{}
		if !r.recordScanEvidence(loop, scanPod(t, true, cleanReport)) {
			t.Fatal("recordScanEvidence() = false, want true")
		}
		c := cond(t, loop)
		if c == nil || c.Status != metav1.ConditionFalse || c.Reason != string(coxv1alpha1.ReasonNoSubversionPattern) {
			t.Fatalf("condition = %+v, want False/NoSubversionPattern", c)
		}
		if n := len(rec.Events); n != 0 {
			t.Fatalf("events = %v, want none (a clean scan stays quiet)", rec.Events)
		}
	})

	t.Run("no recorder: still records the condition", func(t *testing.T) {
		r := &LoopReconciler{} // nil Recorder — must not panic.
		loop := &coxv1alpha1.Loop{}
		if !r.recordScanEvidence(loop, scanPod(t, true, hitsReport)) {
			t.Fatal("recordScanEvidence() = false, want true")
		}
		c := cond(t, loop)
		if c == nil || c.Status != metav1.ConditionTrue {
			t.Fatalf("condition = %+v, want True", c)
		}
	})
}

// TestRecordScanEvidenceUnavailable covers the two Unknown decision cases of
// recordScanEvidence (D135): an unreadable report (garbage message) and a
// report with file errors (the scan did not see the whole diff).
func TestRecordScanEvidenceUnavailable(t *testing.T) {
	const errorsReport = `{"v":1,"files":4,"hits":[],"hitsDropped":0,"errors":["broken.go: parse error"],"errorsDropped":1}`

	cond := func(t *testing.T, loop *coxv1alpha1.Loop) *metav1.Condition {
		t.Helper()
		for i := range loop.Status.Conditions {
			if loop.Status.Conditions[i].Type == coxv1alpha1.VerifySuspiciousCondition {
				return &loop.Status.Conditions[i]
			}
		}
		return nil
	}

	t.Run("garbage message: Unknown", func(t *testing.T) {
		rec := record.NewFakeRecorder(16)
		r := &LoopReconciler{Recorder: rec}
		loop := &coxv1alpha1.Loop{}
		if !r.recordScanEvidence(loop, scanPod(t, true, `not a report`)) {
			t.Fatal("recordScanEvidence() = false, want true")
		}
		c := cond(t, loop)
		if c == nil || c.Status != metav1.ConditionUnknown || c.Reason != string(coxv1alpha1.ReasonScanUnavailable) {
			t.Fatalf("condition = %+v, want Unknown/ScanUnavailable", c)
		}
		if c.Message != "scan unavailable: no readable report" {
			t.Fatalf("message = %q", c.Message)
		}
		if n := len(rec.Events); n != 1 {
			t.Fatalf("events = %v, want one Warning", rec.Events)
		}
	})

	t.Run("errors only: Unknown (incomplete)", func(t *testing.T) {
		rec := record.NewFakeRecorder(16)
		r := &LoopReconciler{Recorder: rec}
		loop := &coxv1alpha1.Loop{}
		if !r.recordScanEvidence(loop, scanPod(t, true, errorsReport)) {
			t.Fatal("recordScanEvidence() = false, want true")
		}
		c := cond(t, loop)
		if c == nil || c.Status != metav1.ConditionUnknown || c.Reason != string(coxv1alpha1.ReasonScanUnavailable) {
			t.Fatalf("condition = %+v, want Unknown/ScanUnavailable", c)
		}
		if c.Message != "scan incomplete: 1 file(s) did not parse" {
			t.Fatalf("message = %q", c.Message)
		}
		if n := len(rec.Events); n != 1 {
			t.Fatalf("events = %v, want one Warning", rec.Events)
		}
	})
}
