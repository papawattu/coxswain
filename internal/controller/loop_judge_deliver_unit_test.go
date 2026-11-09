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

// J1e (ADR-0011 "Effect (D122, advisory)"): a false Judge verdict holds the
// PR as a draft whatever spec.delivery.draft says; a true verdict, a nil
// verdict (Unknown, D124), or no status.judge leaves spec.delivery.draft
// exactly as it is (default true). The test is a plain unit test (no
// envtest, no cluster): deliverDraft is a pure function of the Loop's
// spec/status, and the table below covers every combination of
// spec.delivery.draft (unset / false) with status.judge (nil / verdict nil /
// verdict true / verdict false).
package controller

import (
	"testing"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// TestDeliverDraftJudgeVerdict (J1e, ADR-0011 D122) is the table test for
// deliverDraft's Judge branch. It asserts every (draft, judge) combination:
//
//   - no judge and draft unset  -> true (the default);
//   - no judge and draft false   -> false (spec says ready);
//   - verdict false and draft false -> true (the advisory forces a draft);
//   - verdict true and draft false  -> false (a pass does not force a draft);
//   - verdict nil and draft false   -> false (Unknown leaves draft as-is).
//
// The "verdict false forces a draft" case is D122's only effect on delivery:
// the Judge can never be the only gate (it cannot make a ready PR non-draft,
// and it never changes Succeeded or iterates — the draft marker is the
// advisory's whole reach).
func TestDeliverDraftJudgeVerdict(t *testing.T) {
	tv := true
	fv := false
	noJudge := coxv1alpha1.JudgeStatus{}

	tests := []struct {
		name  string
		draft *bool // nil = spec.delivery.draft unset
		judge *coxv1alpha1.JudgeStatus
		want  bool
	}{
		{"no judge, draft unset -> true", nil, nil, true},
		{"no judge, draft false -> false", new(bool), nil, false},
		{"verdict false, draft false -> true (the advisory forces a draft)", new(bool), &coxv1alpha1.JudgeStatus{Verdict: &fv}, true},
		{"verdict false, draft unset -> true", nil, &coxv1alpha1.JudgeStatus{Verdict: &fv}, true},
		{"verdict true, draft false -> false", new(bool), &coxv1alpha1.JudgeStatus{Verdict: &tv}, false},
		{"verdict true, draft unset -> true", nil, &coxv1alpha1.JudgeStatus{Verdict: &tv}, true},
		{"verdict nil, draft false -> false", new(bool), &noJudge, false},
		{"verdict nil, draft unset -> true", nil, &noJudge, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loop := &coxv1alpha1.Loop{
				Spec: coxv1alpha1.LoopSpec{
					Delivery: &coxv1alpha1.DeliveryConfig{Draft: tt.draft},
				},
				Status: coxv1alpha1.LoopStatus{Judge: tt.judge},
			}
			if got := deliverDraft(loop); got != tt.want {
				t.Errorf("deliverDraft() = %v, want %v (draft=%v judge=%v)", got, tt.want, tt.draft, tt.judge)
			}
		})
	}
}

// The table's *bool draft values use new(bool) (a pointer to false) for the
// "draft false" cases (nil = unset, the "draft unset" cases).
