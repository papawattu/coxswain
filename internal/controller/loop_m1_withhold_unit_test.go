// M1 (D140, docs/PLAN-PHASE8.md via #485): a Loop the Judge flags (verdict
// false, verdict nil = Unknown, or status.judge.truncated) writes NO
// shared-memory lessons. judgeWithholdsLessons is the operator's one
// decision: it returns why the Loop's lessons are withheld ("" = not
// withheld), reading only status.judge (the fixed-shape record, ADR-0011).
// This is the RED spec (step 1 of 2): the stub always returns "", so the
// flagged cases fail until the decision is implemented.
package controller

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

func TestJudgeWithholdsLessons(t *testing.T) {
	tv, fv := true, false
	cases := []struct {
		name  string
		judge *coxv1alpha1.JudgeStatus
		// suspicious: the VerifySuspicious condition is True (D135).
		suspicious bool
		wantEmpty  bool
		contains   string // required substring in the reason ("" = no check)
	}{
		{
			name:      "no Judge record: not withheld",
			judge:     nil,
			wantEmpty: true,
		},
		{
			name:      "verdict true: not withheld",
			judge:     &coxv1alpha1.JudgeStatus{Verdict: &tv, Score: 87, Reason: "meets the goal without gaming"},
			wantEmpty: true,
		},
		{
			name:      "verdict false: withheld, the reason names the false verdict",
			judge:     &coxv1alpha1.JudgeStatus{Verdict: &fv, Score: 12, Reason: "the acceptance was gamed"},
			wantEmpty: false,
			contains:  "false",
		},
		{
			name:      "verdict nil (Unknown): withheld, the reason names the Unknown verdict",
			judge:     &coxv1alpha1.JudgeStatus{Score: 0},
			wantEmpty: false,
			contains:  "Unknown",
		},
		{
			name:      "verdict true but truncated: withheld, the reason names the truncation",
			judge:     &coxv1alpha1.JudgeStatus{Verdict: &tv, Score: 87, Reason: "ok", Truncated: true},
			wantEmpty: false,
			contains:  "truncated",
		},
		{
			name:       "VerifySuspicious True, no judge: withheld, the reason names the diff scan (D135)",
			judge:      nil,
			suspicious: true,
			wantEmpty:  false,
			contains:   "diff scan",
		},
		{
			name:       "VerifySuspicious True + verdict true: withheld (D135 beats a clean verdict)",
			judge:      &coxv1alpha1.JudgeStatus{Verdict: &tv, Score: 87, Reason: "ok"},
			suspicious: true,
			wantEmpty:  false,
			contains:   "diff scan",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loop := &coxv1alpha1.Loop{}
			loop.Status.Judge = tc.judge
			if tc.suspicious {
				loop.Status.Conditions = append(loop.Status.Conditions, metav1.Condition{
					Type:   coxv1alpha1.VerifySuspiciousCondition,
					Status: metav1.ConditionTrue,
					Reason: string(coxv1alpha1.ReasonSubversionPatternInDiff),
				})
			}
			got := judgeWithholdsLessons(loop)
			if tc.wantEmpty {
				if got != "" {
					t.Fatalf("judgeWithholdsLessons() = %q, want \"\" (not withheld)", got)
				}
				return
			}
			if got == "" {
				t.Fatalf("judgeWithholdsLessons() = \"\", want a non-empty reason (the Loop's lessons must be withheld)")
			}
			if !strings.Contains(got, tc.contains) {
				t.Fatalf("judgeWithholdsLessons() = %q, want a reason containing %q", got, tc.contains)
			}
		})
	}
}
