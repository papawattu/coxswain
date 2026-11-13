// Phase 8 J5a (PLAN-PHASE8.md, D132): an optional spec.verify.judge.model.
// When set, the Judge asks the Loop's model proxy for that model; empty
// means the Loop's spec.agent.model (D123's default).
package controller

import (
	"testing"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// TestJudgeModelEnv covers the COX_JUDGE_MODEL choice in judgeEnvValueMap
// (J5a, D132): the Judge's own model when named, else the Loop's model,
// and the unchanged no-endpoint behaviour (no model, no call).
func TestJudgeModelEnv(t *testing.T) {
	r := &LoopReconciler{}
	tests := []struct {
		name   string
		adjust func(*coxv1alpha1.Loop)
		// wantModel is "" when COX_JUDGE_MODEL must be absent.
		wantModel string
		noKey     bool
	}{
		{
			name:      "no judge model means the Loop's model",
			adjust:    nil,
			wantModel: testModel,
		},
		{
			name: "the Judge's own model is named",
			adjust: func(loop *coxv1alpha1.Loop) {
				loop.Spec.Verify.Judge.Model = "judge-model-b"
			},
			wantModel: "judge-model-b",
		},
		{
			name: "no endpoint means no model key (no call)",
			adjust: func(loop *coxv1alpha1.Loop) {
				loop.Spec.Agent.ModelEndpoint = ""
				loop.Spec.Verify.Judge.Model = "judge-model-b"
			},
			wantModel: "",
			noKey:     true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loop := judgeTestLoop("j5a-loop")
			if tt.adjust != nil {
				tt.adjust(loop)
			}
			env := r.judgeEnvValueMap(loop)
			got, ok := env["COX_JUDGE_MODEL"]
			if tt.noKey {
				if ok {
					t.Fatalf("COX_JUDGE_MODEL: got %q, want absent (no endpoint, no call)", got)
				}
				return
			}
			if !ok {
				t.Fatalf("COX_JUDGE_MODEL: absent, want %q", tt.wantModel)
			}
			if got != tt.wantModel {
				t.Fatalf("COX_JUDGE_MODEL: got %q, want %q", got, tt.wantModel)
			}
		})
	}
}
