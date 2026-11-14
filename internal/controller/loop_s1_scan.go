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

// S1 step 3b (D135, ADR-0005 D17.2): the operator's read of the diff scan's
// evidence and the VerifySuspicious condition it records.
//
// The verify Job's trusted `scan` init container (S1-3a) greps the
// base→verified diff of non-protected files for subversion patterns and
// reports the result as its termination MESSAGE (a versioned JSON report —
// the scan is trusted code, so the message is evidence like any other init's
// termination data, per D15). The container exits 0 either way: the scan is
// advisory (D21) and can never block the checks. This file is the operator's
// side of that contract: it parses the message, decides the condition's
// status and reason, and — only when the decision CHANGES — emits the
// Warning Event (a True/Unknown scan outcome is noteworthy; a clean False is
// the quiet steady state). The condition is advisory: deliverDraft,
// judgeSummary and judgeWithholdsLessons each read it (D135).
package controller

import (
	"encoding/json"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// verifyScanInit (the scan init's name) is declared with the verify Job's
// other init names in loop_verify_job.go (S1-3a).

// scanReport is the v==1 JSON report the scan init writes to its termination
// message (the operator's only read of the scan's verdict).
type scanReport struct {
	V             int       `json:"v"`
	Files         int       `json:"files"`
	Hits          []scanHit `json:"hits"`
	HitsDropped   int       `json:"hitsDropped"`
	Errors        []string  `json:"errors"`
	ErrorsDropped int       `json:"errorsDropped"`
}

// scanHit is one subversion pattern the verified file adds over its base.
type scanHit struct {
	Path    string `json:"path"`
	Pattern string `json:"pattern"`
	Line    int    `json:"line"`
}

// parseScanReport decodes the scan init's termination message. ok is true ONLY
// for valid JSON with v == 1: a version the operator does not understand is
// "scan unavailable" (the ADR's garbled-message case), never "clean".
func parseScanReport(msg string) (scanReport, bool) {
	var rep scanReport
	if err := json.Unmarshal([]byte(msg), &rep); err != nil {
		return scanReport{}, false
	}
	return rep, rep.V == 1
}

// recordScanEvidence reads the verify Job pod's `scan` init termination
// message and records the VerifySuspicious condition (D135, ADR-0005 D17.2):
//
//   - no pod, no scan status, or the scan has not terminated -> false and
//     nothing changes (no decision yet);
//   - an unreadable or version-mismatched report -> Unknown/ScanUnavailable
//     (a Warning Event);
//   - at least one pattern (or dropped hits) -> True/SubversionPatternInDiff
//     (a Warning Event, the hit list);
//   - a report with no hits but file errors -> Unknown/ScanUnavailable
//     (a Warning Event; the scan did not see the whole diff);
//   - otherwise False/NoSubversionPattern (no Event — the quiet steady state).
//
// The Warning Event fires only when the condition's status or reason CHANGES
// (a steady-state reconcile never re-Events, and a clean scan never does).
// It returns whether anything changed (the caller's evidenceChanged fold).
func (r *LoopReconciler) recordScanEvidence(loop *coxv1alpha1.Loop, pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	var terminated *corev1.ContainerStateTerminated
	for i := range pod.Status.InitContainerStatuses {
		ics := &pod.Status.InitContainerStatuses[i]
		if ics.Name == verifyScanInit {
			terminated = ics.State.Terminated
			break
		}
	}
	if terminated == nil {
		return false
	}

	commit := ""
	if loop.Status.CurrentVerify != nil {
		commit = loop.Status.CurrentVerify.VerifiedCommit
	}

	// The decision: status + reason of the condition this pass would record.
	// (The message alone changing never re-Events — the Warning Event fires
	// on a status or reason CHANGE, R26 I90.)
	cond := metav1.ConditionFalse
	reason := string(coxv1alpha1.ReasonNoSubversionPattern)
	message := "no subversion pattern in the diff"
	if rep, ok := parseScanReport(terminated.Message); ok {
		switch {
		case len(rep.Hits)+rep.HitsDropped > 0:
			cond = metav1.ConditionTrue
			reason = string(coxv1alpha1.ReasonSubversionPatternInDiff)
			message = scanHitMessage(rep, commit)
		case len(rep.Errors) > 0:
			cond = metav1.ConditionUnknown
			reason = string(coxv1alpha1.ReasonScanUnavailable)
			message = "scan incomplete: " + strconv.Itoa(len(rep.Errors)) + " file(s) did not parse"
		}
	} else {
		cond = metav1.ConditionUnknown
		reason = string(coxv1alpha1.ReasonScanUnavailable)
		message = "scan unavailable: no readable report"
	}

	changed := false
	var existing *metav1.Condition
	for i := range loop.Status.Conditions {
		if loop.Status.Conditions[i].Type == coxv1alpha1.VerifySuspiciousCondition {
			existing = &loop.Status.Conditions[i]
			break
		}
	}
	if existing == nil || existing.Status != cond || existing.Reason != reason {
		changed = true
	}

	if changed {
		setCondition(loop, coxv1alpha1.VerifySuspiciousCondition, cond, reason, message)
		// R26 I90: surface the non-steady-state outcome (R25 I84: the True and
		// Unknown cases are the noteworthy ones; a clean False stays quiet).
		if r.Recorder != nil && cond != metav1.ConditionFalse {
			r.Recorder.Eventf(loop, corev1.EventTypeWarning, reason, "%s", message)
		}
	}

	loop.Status.JudgeSummary = judgeSummary(loop)
	return changed
}

// scanHitMessage renders the True case's condition message: the hit count,
// the verified commit (7 chars, so the condition's reason/message stay
// storable even when the pin is empty), and the first three hits.
func scanHitMessage(rep scanReport, commit string) string {
	total := len(rep.Hits) + rep.HitsDropped
	var b strings.Builder
	b.WriteString(strconv.Itoa(total))
	b.WriteString(" pattern(s) in the diff at ")
	b.WriteString(commit7(commit))
	b.WriteString(": ")
	for i, h := range rep.Hits {
		if i > 0 {
			b.WriteString(", ")
		}
		if i == 3 {
			b.WriteString("…")
			break
		}
		b.WriteString(h.Pattern)
		b.WriteString(" in ")
		b.WriteString(h.Path)
		b.WriteString(":")
		b.WriteString(strconv.Itoa(h.Line))
	}
	return b.String()
}

// commit7 is the verified commit's first 7 characters ("" when the pin is
// empty — the scan's message must not depend on a pin that was not recorded).
func commit7(commit string) string {
	if len(commit) >= 7 {
		return commit[:7]
	}
	return commit
}

// hasConditionTrue reports whether the Loop carries a condition of condType
// with Status True (the advisory flag read: deliverDraft, judgeSummary and
// judgeWithholdsLessons each ask this of VerifySuspicious, D135).
func hasConditionTrue(loop *coxv1alpha1.Loop, condType string) bool {
	if loop == nil {
		return false
	}
	for i := range loop.Status.Conditions {
		if loop.Status.Conditions[i].Type == condType {
			return loop.Status.Conditions[i].Status == metav1.ConditionTrue
		}
	}
	return false
}
