package controller

// Unit tests for the C2/D101 placement lines inside the proxy pod builders
// (R43 M3b/M3c): buildEgressProxyPod and buildToolProxyPod each called
// r.applyLoopPlacement(&pod.Spec), and the focused C2 envtest did not catch
// deleting either line. Every Loop pod is pinned, so each builder must pin
// the pod to the reconciler's node pool (NodeSelector + Tolerations), and a
// zero-value reconciler must leave both empty (the F93b set-and-clear
// contract). buildProxyPod (model proxy) is in the same table for coverage
// of all three builders.

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

func TestC2ProxyPlacement(t *testing.T) {
	pinned := &LoopReconciler{
		LoopNodeSelector: map[string]string{"coxswain.io/pool": "loops"},
		LoopTolerations: []corev1.Toleration{{
			Key:      "coxswain.io/pool",
			Operator: corev1.TolerationOpEqual,
			Value:    "loops",
			Effect:   corev1.TaintEffectNoSchedule,
		}},
	}
	zero := &LoopReconciler{}

	cases := []struct {
		name  string
		build func(r *LoopReconciler) *corev1.Pod
	}{
		{
			name: "buildProxyPod",
			build: func(r *LoopReconciler) *corev1.Pod {
				return r.buildProxyPod(&coxv1alpha1.Loop{}, "loop", "default", "img")
			},
		},
		{
			name: "buildEgressProxyPod",
			build: func(r *LoopReconciler) *corev1.Pod {
				return r.buildEgressProxyPod("loop", "default", "img", []string{"github.com:443"}, "h", "10.244.0.0/16", "10.96.0.0/12")
			},
		},
		{
			name: "buildToolProxyPod",
			build: func(r *LoopReconciler) *corev1.Pod {
				return r.buildToolProxyPod("loop", "default", "img", coxv1alpha1.ToolSpec{Name: "t", Upstream: "https://example.com"}, "h", "10.244.0.0/16", "10.96.0.0/12")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name+" pinned", func(t *testing.T) {
			pod := tc.build(pinned)
			if !reflect.DeepEqual(pod.Spec.NodeSelector, pinned.LoopNodeSelector) {
				t.Errorf("NodeSelector = %v, want %v", pod.Spec.NodeSelector, pinned.LoopNodeSelector)
			}
			if !reflect.DeepEqual(pod.Spec.Tolerations, pinned.LoopTolerations) {
				t.Errorf("Tolerations = %v, want %v", pod.Spec.Tolerations, pinned.LoopTolerations)
			}
		})
		t.Run(tc.name+" zero reconciler", func(t *testing.T) {
			pod := tc.build(zero)
			if len(pod.Spec.NodeSelector) != 0 {
				t.Errorf("NodeSelector = %v, want empty", pod.Spec.NodeSelector)
			}
			if len(pod.Spec.Tolerations) != 0 {
				t.Errorf("Tolerations = %v, want empty", pod.Spec.Tolerations)
			}
		})
	}
}
