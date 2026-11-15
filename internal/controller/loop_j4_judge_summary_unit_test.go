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

// Phase 8 J4 (D131, D138): the Judge's one-word signal in `kubectl get loop`
// (status.judgeSummary, a top-level LoopStatus field — the reviewer's design
// note in the PR: S1 will set `Suspicious` on Loops with no Judge, so a
// nested status.judge.summary would read as an Unknown verdict). The table
// covers the D138 ordering (most serious wins: False > Truncated > Unknown >
// True) plus the S1 slot (Suspicious, set by S1's diff scan, not here).
// TestJudgeSummarySetByRecorders pins the two writer sites: recordJudgeUnavailable
// and applyJudgeVerdict each stamp the column from status.judge.
package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// TestJudgeSummary is the table test for judgeSummary's D138 decision table:
// the most serious signal wins (Suspicious from the S1 scan, then False,
// Truncated, Unknown, True); a VerifySuspicious True (D135) beats every Judge
// signal, INCLUDING the absence of a Judge; no Judge and no scan hit -> empty
// (the column prints blank, the same as before J4).
func TestJudgeSummary(t *testing.T) {
	tv := true
	fv := false

	tests := []struct {
		name  string
		judge *coxv1alpha1.JudgeStatus
		// suspicious: the VerifySuspicious condition is True (D135).
		suspicious bool
		want       string
	}{
		{"no judge -> empty", nil, false, ""},
		{"verdict true -> True", &coxv1alpha1.JudgeStatus{Verdict: &tv}, false, "True"},
		{"verdict false -> False", &coxv1alpha1.JudgeStatus{Verdict: &fv}, false, "False"},
		{"verdict nil -> Unknown", &coxv1alpha1.JudgeStatus{}, false, "Unknown"},
		{"verdict true + truncated -> Truncated (D138)", &coxv1alpha1.JudgeStatus{Verdict: &tv, Truncated: true}, false, "Truncated"},
		{"verdict nil + truncated -> Truncated (D138)", &coxv1alpha1.JudgeStatus{Truncated: true}, false, "Truncated"},
		{"verdict false + truncated -> False (D138: False > Truncated)", &coxv1alpha1.JudgeStatus{Verdict: &fv, Truncated: true}, false, "False"},
		{"VerifySuspicious True, no judge -> Suspicious (D135 beats the absence of a Judge)", nil, true, "Suspicious"},
		{"VerifySuspicious True + verdict false -> Suspicious (D135: Suspicious beats False)", &coxv1alpha1.JudgeStatus{Verdict: &fv}, true, "Suspicious"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loop := &coxv1alpha1.Loop{Status: coxv1alpha1.LoopStatus{Judge: tt.judge}}
			if tt.suspicious {
				loop.Status.Conditions = append(loop.Status.Conditions, metav1.Condition{
					Type:   coxv1alpha1.VerifySuspiciousCondition,
					Status: metav1.ConditionTrue,
					Reason: string(coxv1alpha1.ReasonSubversionPatternInDiff),
				})
			}
			if got := judgeSummary(loop); got != tt.want {
				t.Fatalf("judgeSummary() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestJudgeSummarySetByRecorders (J4) pins the writer sites: the two recorders
// stamp status.judgeSummary from the status.judge they just wrote
// (recordJudgeUnavailable -> Unknown; a parsable verdict -> the verdict's
// word). The Recorder is nil: the recorders must not touch it on this path.
// applyJudgeVerdict's msg is the Judge container's termination message (the
// judgeVerdict shape, loop_judge_job.go).
func TestJudgeSummarySetByRecorders(t *testing.T) {
	t.Run("recordJudgeUnavailable stamps Unknown", func(t *testing.T) {
		r := &LoopReconciler{}
		loop := &coxv1alpha1.Loop{}
		r.recordJudgeUnavailable(loop, "x")
		if loop.Status.JudgeSummary != "Unknown" {
			t.Fatalf("recordJudgeUnavailable: status.judgeSummary = %q, want %q", loop.Status.JudgeSummary, "Unknown")
		}
	})

	t.Run("applyJudgeVerdict stamps the verdict's word", func(t *testing.T) {
		r := &LoopReconciler{}
		loop := &coxv1alpha1.Loop{}
		r.applyJudgeVerdict(loop, `{"verdict":false,"score":10,"reason":"hardcodes the test inputs","truncated":false}`)
		if loop.Status.JudgeSummary != "False" {
			t.Fatalf("applyJudgeVerdict: status.judgeSummary = %q, want %q", loop.Status.JudgeSummary, "False")
		}
	})
}
