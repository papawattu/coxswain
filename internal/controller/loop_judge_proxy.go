// Phase 8 J5b (PLAN-PHASE8.md, D132): the Judge's own model proxy. When
// spec.verify.judge.modelEndpoint names an endpoint of its own, the Judge's
// call goes through a second model-proxy pod + Service (<loop>-jproxy)
// carrying the Judge endpoint's Secret, metered into the Loop's totals.
// STEP 1 of 3: name + predicate + builder stubs; the RED specs are in
// loop_j5b_judge_proxy_unit_test.go and loop_judge_api_envtest_test.go.
package controller

import (
	corev1 "k8s.io/api/core/v1"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// judgeProxyName is the Judge proxy's pod and Service name (D132). The
// suffix is -jproxy (not -judge-proxy): a 55-char Loop name + "-jproxy"
// (7) is 62, which passes the 63-char Service-name limit, while
// "-judge-proxy" (12) would not.
func judgeProxyName(loopName string) string { return "" }

// judgeOwnEndpoint: the Loop's Judge is enabled and names its own endpoint.
func judgeOwnEndpoint(loop *coxv1alpha1.Loop) bool { return false }

// buildJudgeProxyPod is the model-proxy pod for the Judge's endpoint.
func (r *LoopReconciler) buildJudgeProxyPod(loop *coxv1alpha1.Loop) *corev1.Pod { return nil }
