// Phase 8 J5b (PLAN-PHASE8.md, D132): the Judge's own model proxy. When
// spec.verify.judge.modelEndpoint names an endpoint of its own, the Judge's
// call goes through a second model-proxy pod + Service (<loop>-jproxy)
// carrying the Judge endpoint's Secret, metered into the Loop's totals.
//
// LABELS (the trap): the Loop's own model proxy pod carries
// proxyLabels(loopName) = {component: model-proxy, coxswain.io/proxy-for:
// <loop>}, and the Loop's proxy Service SELECTS those labels. The Judge
// proxy must therefore carry its OWN label set (judgeProxyLabels) — the
// component value judge-proxy (in policy.ProxyComponentSelector, so the
// operator's scoped pod cache can read it) and its own -for label. A Judge
// proxy that carried proxyLabels would be selected by the Loop's own proxy
// Service and route the AGENT's model calls to the Judge's endpoint.
package controller

import (
	"strings"

	corev1 "k8s.io/api/core/v1"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"

	"github.com/papawattu/coxswain/internal/policy"
)

// judgeProxyName is the Judge proxy's pod and Service name (D132). The
// suffix is -jproxy (not -judge-proxy): a 55-char Loop name + "-jproxy"
// (7) is 62, which passes the 63-char Service-name limit, while
// "-judge-proxy" (12) would not.
func judgeProxyName(loopName string) string { return loopName + "-jproxy" }

// judgeProxyForLabel is the Judge proxy's -for label key (the judgeForLabel
// scheme, coxswain.io/<component>-for — a DISTINCT key from
// coxswain.io/proxy-for so no selector on the Loop's own proxy labels can
// match the Judge proxy, and vice versa).
const judgeProxyForLabel = "coxswain.io/judge-proxy-for"

// judgeOwnEndpoint: the Loop's Judge is enabled and names its own endpoint
// (D132). Without an own endpoint the Judge's call goes through the Loop's
// own model proxy (D123/J5a).
func judgeOwnEndpoint(loop *coxv1alpha1.Loop) bool {
	j := loop.Spec.Verify.Judge
	return j != nil && j.Enabled && j.ModelEndpoint != ""
}

// judgeProxyLabels is the Judge proxy pod/Service label set (J5b, D132). It
// is DISJOINT from proxyLabels: the component value is judge-proxy (not
// model-proxy) and the -for key is judge-proxy-for (not proxy-for). If it
// carried proxyLabels, the Loop's own proxy Service (which selects
// proxyLabels) would select this pod too and the agent's model calls could
// be routed to the Judge's endpoint.
func judgeProxyLabels(loopName string) map[string]string {
	return map[string]string{
		policy.ComponentLabelKey: policy.ComponentJudgeProxyLabel,
		judgeProxyForLabel:       loopName,
	}
}

// buildJudgeProxyPod is the model-proxy pod for the Judge's endpoint (J5b,
// D132): the model-proxy pod shape (buildModelProxyPod, shared with the
// Loop's own proxy) with the Judge proxy's name and labels, the Judge's
// endpoint (normalised like modelEndpointValue: a bare host:port gets
// "http://" prefixed — httputil.NewSingleHostReverseProxy requires a full
// URL) and the Judge's credential Secret. The pod never carries the agent's
// endpoint or Secret anywhere.
func (r *LoopReconciler) buildJudgeProxyPod(loop *coxv1alpha1.Loop) *corev1.Pod {
	return r.buildModelProxyPod(loop,
		judgeProxyName(loop.Name),
		loop.Namespace,
		judgeProxyLabels(loop.Name),
		judgeEndpointValue(loop),
		loop.Spec.Verify.Judge.EndpointSecretRef,
		r.proxyImage(),
	)
}

// judgeEndpointValue normalises the Judge's own endpoint to a full URL for
// the Judge proxy's MODEL_ENDPOINT env var, the modelEndpointValue rule
// applied to spec.verify.judge.modelEndpoint: a bare host:port (no scheme)
// gets "http://" prefixed.
func judgeEndpointValue(loop *coxv1alpha1.Loop) string {
	eps := loop.Spec.Verify.Judge.ModelEndpoint
	if eps == "" {
		return ""
	}
	if strings.HasPrefix(eps, "http://") || strings.HasPrefix(eps, "https://") {
		return eps
	}
	return "http://" + eps
}
