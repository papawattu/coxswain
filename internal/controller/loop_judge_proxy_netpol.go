// Phase 8 J5b step 3b (pi3) implements these; step 3a carries stubs so the
// Judge proxy lifecycle compiles. The reviewer keeps pi3's version.
package controller

import (
	"context"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

func (r *LoopReconciler) ensureJudgeProxyNetworkPolicy(_ context.Context, _ *coxv1alpha1.Loop) error {
	return nil
}

func (r *LoopReconciler) deleteJudgeProxyNetworkPolicy(_ context.Context, _ *coxv1alpha1.Loop) error {
	return nil
}
