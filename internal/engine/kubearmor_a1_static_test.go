// Copyright 2026 papawattu.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on the License.
// See the License for the specific language governing permissions and
// limitations under the License.

package engine

import (
	"context"
	"testing"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// a1Enforcer builds a KubeArmorEnforcer with the given nodes, agent pods, and
// KubeArmorPolicy objects, for the A1 static check tests.
var a1True = true

const (
	a1AgentNS       = "kubearmor"
	a1AgentLabelKey = "kubearmor-app"
	a1AgentLabelVal = "kubearmor"
	a1AgentOwner    = "kubearmor-agent"
)

func a1Enforcer(t *testing.T, nodes []corev1.Node, agentPods []corev1.Pod, kapt *unstructured.Unstructured) *KubeArmorEnforcer {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 to scheme: %v", err)
	}
	if err := coxv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add coxswain to scheme: %v", err)
	}
	var objs []client.Object
	for i := range nodes {
		objs = append(objs, &nodes[i])
	}
	for i := range agentPods {
		objs = append(objs, &agentPods[i])
	}
	if kapt != nil {
		objs = append(objs, kapt)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &KubeArmorEnforcer{Client: cl}
}

func bpfNode(name string) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{"kubearmor.io/enforcer": "bpf"},
		},
	}
}

func readyAgentPod(nodeName, name string) corev1.Pod {
	controller := true
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: a1AgentNS,
			Labels:    map[string]string{a1AgentLabelKey: a1AgentLabelVal},
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "DaemonSet", Name: a1AgentOwner, Controller: &controller},
			},
		},
		Spec:   corev1.PodSpec{NodeName: nodeName},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
}

func notReadyAgentPod(nodeName, name string) corev1.Pod {
	controller := true
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: a1AgentNS,
			Labels:    map[string]string{a1AgentLabelKey: a1AgentLabelVal},
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "DaemonSet", Name: a1AgentOwner, Controller: &controller},
			},
		},
		Spec:   corev1.PodSpec{NodeName: nodeName},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}},
	}
}

func a1Kapt() *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(KubeArmorGVK)
	obj.SetName("coxswain-l1")
	obj.SetNamespace("default")
	obj.SetOwnerReferences([]metav1.OwnerReference{
		{
			APIVersion: "coxswain.io/v1alpha1",
			Kind:       "Loop",
			Name:       "l1",
			UID:        "test-uid",
			Controller: &a1True,
		},
	})
	obj.Object["spec"] = map[string]any{
		"selector": map[string]any{
			"matchLabels": map[string]any{"coxswain.io/loop": "l1"},
		},
	}
	return obj
}

func a1Loop() *coxv1alpha1.Loop {
	return &coxv1alpha1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: "l1", Namespace: "default", UID: "test-uid"},
	}
}

func TestA1AllFactsTrue(t *testing.T) {
	nodes := []corev1.Node{bpfNode("node1")}
	pods := []corev1.Pod{readyAgentPod("node1", "ka-agent-1")}
	kapt := a1Kapt()
	e := a1Enforcer(t, nodes, pods, kapt)
	loop := a1Loop()
	enforcing, reason := e.Enforcing(context.Background(), loop)
	if !enforcing {
		t.Fatalf("expected enforcing=true, got false (reason=%s)", reason)
	}
	if reason != "" {
		t.Fatalf("expected empty reason, got %q", reason)
	}
}

func TestA1NoBPFNode(t *testing.T) {
	nodes := []corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node1"}}}
	pods := []corev1.Pod{readyAgentPod("node1", "ka-agent-1")}
	kapt := a1Kapt()
	e := a1Enforcer(t, nodes, pods, kapt)
	loop := a1Loop()
	enforcing, reason := e.Enforcing(context.Background(), loop)
	if enforcing {
		t.Fatalf("expected enforcing=false (no BPF node), got true")
	}
	if reason != ReasonNodeNotEnforcing {
		t.Fatalf("expected reason=%s, got %s", ReasonNodeNotEnforcing, reason)
	}
}

func TestA1AgentPodNotReady(t *testing.T) {
	nodes := []corev1.Node{bpfNode("node1")}
	pods := []corev1.Pod{notReadyAgentPod("node1", "ka-agent-1")}
	kapt := a1Kapt()
	e := a1Enforcer(t, nodes, pods, kapt)
	loop := a1Loop()
	enforcing, reason := e.Enforcing(context.Background(), loop)
	if enforcing {
		t.Fatalf("expected enforcing=false (agent not Ready), got true")
	}
	if reason != ReasonNodeNotEnforcing {
		t.Fatalf("expected reason=%s, got %s", ReasonNodeNotEnforcing, reason)
	}
}

func TestA1AgentPodMissingOnNode(t *testing.T) {
	nodes := []corev1.Node{bpfNode("node1")}
	pods := []corev1.Pod{}
	kapt := a1Kapt()
	e := a1Enforcer(t, nodes, pods, kapt)
	loop := a1Loop()
	enforcing, reason := e.Enforcing(context.Background(), loop)
	if enforcing {
		t.Fatalf("expected enforcing=false (no agent pod on node), got true")
	}
	if reason != ReasonNodeNotEnforcing {
		t.Fatalf("expected reason=%s, got %s", ReasonNodeNotEnforcing, reason)
	}
}

func TestA1PolicyNotYetCreated(t *testing.T) {
	nodes := []corev1.Node{bpfNode("node1")}
	pods := []corev1.Pod{readyAgentPod("node1", "ka-agent-1")}
	e := a1Enforcer(t, nodes, pods, nil)
	loop := a1Loop()
	enforcing, reason := e.Enforcing(context.Background(), loop)
	if enforcing {
		t.Fatalf("expected enforcing=false (policy not created), got true")
	}
	if reason != ReasonEnforcementUnverified {
		t.Fatalf("expected reason=%s, got %s", ReasonEnforcementUnverified, reason)
	}
}

func TestA1PolicyNotControlledByLoop(t *testing.T) {
	nodes := []corev1.Node{bpfNode("node1")}
	pods := []corev1.Pod{readyAgentPod("node1", "ka-agent-1")}
	kapt := a1Kapt()
	kapt.SetOwnerReferences([]metav1.OwnerReference{
		{APIVersion: "apps/v1", Kind: "Deployment", Name: "other", UID: "other-uid", Controller: &a1True},
	})
	e := a1Enforcer(t, nodes, pods, kapt)
	loop := a1Loop()
	enforcing, reason := e.Enforcing(context.Background(), loop)
	if enforcing {
		t.Fatalf("expected enforcing=false (policy not controlled by loop), got true")
	}
	if reason != ReasonEnforcementUnverified {
		t.Fatalf("expected reason=%s, got %s", ReasonEnforcementUnverified, reason)
	}
}

func TestA1PolicySelectorMismatch(t *testing.T) {
	nodes := []corev1.Node{bpfNode("node1")}
	pods := []corev1.Pod{readyAgentPod("node1", "ka-agent-1")}
	kapt := a1Kapt()
	kapt.Object["spec"] = map[string]any{
		"selector": map[string]any{
			"matchLabels": map[string]any{"coxswain.io/loop": "different-loop"},
		},
	}
	e := a1Enforcer(t, nodes, pods, kapt)
	loop := a1Loop()
	enforcing, reason := e.Enforcing(context.Background(), loop)
	if enforcing {
		t.Fatalf("expected enforcing=false (selector mismatch), got true")
	}
	if reason != ReasonEnforcementUnverified {
		t.Fatalf("expected reason=%s, got %s", ReasonEnforcementUnverified, reason)
	}
}

func TestA1AgentPodNotDaemonSet(t *testing.T) {
	nodes := []corev1.Node{bpfNode("node1")}
	controller := true
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ka-agent-1",
			Namespace: a1AgentNS,
			Labels:    map[string]string{a1AgentLabelKey: a1AgentLabelVal},
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "Deployment", Name: a1AgentOwner, Controller: &controller},
			},
		},
		Spec:   corev1.PodSpec{NodeName: "node1"},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
	kapt := a1Kapt()
	e := a1Enforcer(t, nodes, []corev1.Pod{pod}, kapt)
	loop := a1Loop()
	enforcing, reason := e.Enforcing(context.Background(), loop)
	if enforcing {
		t.Fatalf("expected enforcing=false (agent not DaemonSet-owned), got true")
	}
	if reason != ReasonNodeNotEnforcing {
		t.Fatalf("expected reason=%s, got %s", ReasonNodeNotEnforcing, reason)
	}
}

func TestA1MultipleNodesAllReady(t *testing.T) {
	nodes := []corev1.Node{bpfNode("node1"), bpfNode("node2")}
	pods := []corev1.Pod{
		readyAgentPod("node1", "ka-agent-1"),
		readyAgentPod("node2", "ka-agent-2"),
	}
	kapt := a1Kapt()
	e := a1Enforcer(t, nodes, pods, kapt)
	loop := a1Loop()
	enforcing, reason := e.Enforcing(context.Background(), loop)
	if !enforcing {
		t.Fatalf("expected enforcing=true (both nodes Ready), got false (reason=%s)", reason)
	}
}

func TestA1MultipleNodesOneNotReady(t *testing.T) {
	nodes := []corev1.Node{bpfNode("node1"), bpfNode("node2")}
	pods := []corev1.Pod{
		readyAgentPod("node1", "ka-agent-1"),
		notReadyAgentPod("node2", "ka-agent-2"),
	}
	kapt := a1Kapt()
	e := a1Enforcer(t, nodes, pods, kapt)
	loop := a1Loop()
	enforcing, reason := e.Enforcing(context.Background(), loop)
	if enforcing {
		t.Fatalf("expected enforcing=false (one node not Ready), got true")
	}
	if reason != ReasonNodeNotEnforcing {
		t.Fatalf("expected reason=%s, got %s", ReasonNodeNotEnforcing, reason)
	}
}

func TestA1GateOpensFromSuspended(t *testing.T) {
	// The acceptance scenario: the gate opens from Suspended (no sandbox pod)
	// when all facts hold. The Enforcing method reads nodes and agent pods, not
	// the sandbox pod, so it can report True even with no pod.
	nodes := []corev1.Node{bpfNode("node1")}
	pods := []corev1.Pod{readyAgentPod("node1", "ka-agent-1")}
	kapt := a1Kapt()
	e := a1Enforcer(t, nodes, pods, kapt)
	loop := a1Loop()
	enforcing, reason := e.Enforcing(context.Background(), loop)
	if !enforcing {
		t.Fatalf("expected enforcing=true (gate opens from Suspended), got false (reason=%s)", reason)
	}
}
