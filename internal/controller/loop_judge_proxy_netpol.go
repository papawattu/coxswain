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

package controller

// J5b step 3b (PLAN-PHASE8.md, D132): the Judge proxy's network and admission
// fences. When the Judge names its own endpoint (judgeOwnEndpoint) it calls
// the model through its OWN proxy pod (<loop>-jproxy, judgeProxyLabels)
// instead of the Loop's model proxy, so the jproxy needs its own
// NetworkPolicy, and the Judge Job's NetworkPolicy egress must target the
// jproxy INSTEAD of the Loop's proxy (rule 2): pointing it at the Loop's
// proxy would leave the Judge's call fenced out of its actual path.
//
// The jproxy NetPol is modelled on the Loop proxy's (loop_controller.go
// ensureNetworkPolicy, loop_i43_proxy_netpol_test.go pins its shape):
// INGRESS only from the Judge Job's pods on proxyPort (8080 — the Judge's
// only network call is a POST to this proxy) and from the operator on the
// usage port (the same peer the Loop proxy's usage ingress uses, P2b/ADR-0009
// metering), EGRESS only to the Judge's endpoint (modelPeer +
// modelEndpointPort applied to spec.verify.judge.modelEndpoint, exactly as
// the Loop proxy does for spec.agent.modelEndpoint) plus cluster DNS. The
// agent's endpoint is NEVER an egress peer of the jproxy.

import (
	"context"
	"fmt"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// judgeProxyNetPolName is the Judge proxy's NetworkPolicy name
// (<loop>-jproxy-netpol).
func judgeProxyNetPolName(loopName string) string {
	return loopName + "-jproxy-netpol"
}

// buildJudgeProxyNetPol is the desired spec for the Judge proxy's
// NetworkPolicy (J5b step 3b). Ingress: the Judge Job's pods (judgeJobLabels)
// on proxyPort, and the operator on the usage port when r.OperatorNamespace
// is set (the SAME peer the Loop proxy's usage ingress uses — the operator
// namespace + the controller-manager pod label; fail-closed: with no operator
// namespace the rule is omitted, the usage endpoint is unreachable rather
// than wide-open). Egress: the Judge's endpoint (modelPeer +
// modelEndpointPort on spec.verify.judge.modelEndpoint, the Loop proxy's
// modelEndpoint rule applied to the Judge's endpoint) + cluster DNS. The
// agent's endpoint is not an egress peer.
func (r *LoopReconciler) buildJudgeProxyNetPol(loop *coxv1alpha1.Loop) *networkingv1.NetworkPolicy {
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      judgeProxyNetPolName(loop.Name),
			Namespace: loop.Namespace,
		},
		Spec: networkingv1.NetworkPolicySpec{
			// The jproxy's podSelector is judgeProxyLabels — DISJOINT from
			// proxyLabels (component judge-proxy, key judge-proxy-for), so no
			// selector for the Loop's own proxy pod matches the jproxy and
			// vice versa (the J5b label trap, loop_judge_proxy.go).
			PodSelector: metav1.LabelSelector{MatchLabels: judgeProxyLabels(loop.Name)},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					// The only admitted caller is the Judge Job's pods (their only
					// network call is the model POST to this proxy — the memory-writer /
					// Judge peer pattern the Loop proxy's 8080 ingress uses, using the
					// minimal -for label: the judge pod carries coxswain.io/judge-for=
					// <loop> (judgeJobLabels), which the minimal peer selects). The
					// AGENT is NOT a peer: the agent reaches the model through the Loop's
					// own proxy, never the jproxy.
					From: []networkingv1.NetworkPolicyPeer{{
						PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{judgeForLabel: loop.Name}},
					}},
					Ports: []networkingv1.NetworkPolicyPort{{Port: intstrPtr32(proxyPort), Protocol: new(corev1.ProtocolTCP)}},
				},
			},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					To:    []networkingv1.NetworkPolicyPeer{dnsPeer()},
					Ports: dnsPorts(),
				},
			},
		},
	}

	// The operator reads the jproxy's cumulative usage from the usage port
	// (J5b step 3a meters the Judge proxy into the Loop's totals through the
	// same /coxswain/usage endpoint as the Loop proxy): the same operator
	// peer the Loop proxy's usage ingress admits. Fail-closed: omitted when
	// the operator namespace is unset (tests).
	if r.OperatorNamespace != "" {
		operatorPeer := networkingv1.NetworkPolicyPeer{
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{kubeNSLabelKey: r.OperatorNamespace},
			},
			PodSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"control-plane": controllerMgrLabel},
			},
		}
		np.Spec.Ingress = append(np.Spec.Ingress, networkingv1.NetworkPolicyIngressRule{
			From:  []networkingv1.NetworkPolicyPeer{operatorPeer},
			Ports: []networkingv1.NetworkPolicyPort{{Port: intstrPtr32(proxyUsagePort), Protocol: new(corev1.ProtocolTCP)}},
		})
	}

	// The Judge's endpoint egress rule (modelPeer + modelEndpointPort on
	// spec.verify.judge.modelEndpoint — the Loop proxy's modelEndpoint rule
	// applied to the Judge's endpoint). No rule when the endpoint cannot be
	// expressed as a peer (fail-closed: the jproxy egress stays DNS-only and
	// the Judge's call fails, not wide-open).
	if ep := loop.Spec.Verify.Judge.ModelEndpoint; ep != "" {
		if peer := modelPeer(ep, r.serviceNamespaceFromHost); peer != nil {
			port := intstrPtr32(int32(modelEndpointPort(ep)))
			np.Spec.Egress = append(np.Spec.Egress, networkingv1.NetworkPolicyEgressRule{
				To:    []networkingv1.NetworkPolicyPeer{*peer},
				Ports: []networkingv1.NetworkPolicyPort{{Port: port, Protocol: new(corev1.ProtocolTCP)}},
			})
		}
	}
	return np
}

// ensureJudgeProxyNetworkPolicy creates or updates the Judge proxy's
// NetworkPolicy (J5b step 3b). The caller invokes it only when
// judgeOwnEndpoint(loop) is true. The desired spec is re-asserted inside the
// mutate (the I42c lesson: a no-op mutate froze the Loop proxy's spec —
// buildJudgeProxyNetPol's inputs can change across reconciles and the live
// object must track them). A foreign netpol occupying the name is left
// untouched (createOrUpdateNP's errForeignNetpol, the I2/I42b never-take-over
// rule); the caller maps it like ensureNetworkPolicy does.
func (r *LoopReconciler) ensureJudgeProxyNetworkPolicy(ctx context.Context, loop *coxv1alpha1.Loop) error {
	_, err := r.createOrUpdateNP(ctx, loop, r.buildJudgeProxyNetPol(loop))
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil // nothing to report; the create is next pass
		}
		return fmt.Errorf("ensure Judge proxy NetworkPolicy %s/%s: %w",
			loop.Namespace, judgeProxyNetPolName(loop.Name), err)
	}
	return nil
}

// deleteJudgeProxyNetworkPolicy deletes the Judge proxy's NetworkPolicy when
// the Judge no longer has an own endpoint (J5b step 3b cleanup; the egress
// proxy netpol's cleanupEgressProxyNetpol pattern — a stale netpol is drift
// the reconciler must own). A FOREIGN netpol occupying the name is left
// alone (never take over, I2/I42b). NotFound is fine (already gone).
//
// 3b exports the pair (the brief's interface for 3a's PR, which wires the
// ensure call into the reconcile and the delete call into the else branch);
// until 3a lands the delete path has no caller in this branch, so a unit
// spec pins its foreign-leave-alone and NotFound-is-fine behaviour and
// keeps the function out of the unused linter's reach.
func (r *LoopReconciler) deleteJudgeProxyNetworkPolicy(ctx context.Context, loop *coxv1alpha1.Loop) error {
	np := &networkingv1.NetworkPolicy{}
	err := r.Get(ctx, client.ObjectKey{
		Namespace: loop.Namespace,
		Name:      judgeProxyNetPolName(loop.Name),
	}, np)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get Judge proxy NetworkPolicy %s/%s: %w",
			loop.Namespace, judgeProxyNetPolName(loop.Name), err)
	}
	if !metav1.IsControlledBy(np, loop) {
		// Foreign object: leave it alone (never take over, I2/I42b).
		return nil
	}
	if err := r.Delete(ctx, np); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete Judge proxy NetworkPolicy %s/%s: %w",
			loop.Namespace, np.Name, err)
	}
	logf.FromContext(ctx).Info("deleted Judge proxy NetworkPolicy (the Judge no longer has an own endpoint)",
		"networkPolicy", np.Name, "loop", loop.Name)
	return nil
}
