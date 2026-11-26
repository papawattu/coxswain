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

	corev1 "k8s.io/api/core/v1"
)

// The Loop pool (--loop-node-selector) scopes A1 fact 1. KubeArmor's node
// probe labels every BPF-LSM node, but every pod coxswain puts a policy on
// (sandbox, proxies, Jobs) is pinned to the pool, so only BPF-LSM nodes in
// the pool need an agent.

func poolBPFNode(name string) corev1.Node {
	n := bpfNode(name)
	n.Labels["coxswain.io/sandbox"] = "true"
	return n
}

var a1Pool = map[string]string{"coxswain.io/sandbox": "true"}

func TestA1PoolIgnoresBPFNodeOutsidePool(t *testing.T) {
	nodes := []corev1.Node{poolBPFNode("cox1"), bpfNode("pi0")}
	pods := []corev1.Pod{readyAgentPod("cox1", "ka-agent-cox1")}
	e := a1Enforcer(t, nodes, pods, a1Kapt())
	e.PoolNodeSelector = a1Pool
	enforcing, reason := e.Enforcing(context.Background(), a1Loop())
	if !enforcing {
		t.Fatalf("a BPF-LSM node outside the Loop pool (no agent) must not hold the Loop: enforcing=false reason=%s", reason)
	}
}

func TestA1PoolStillNeedsAnAgentOnEveryPoolNode(t *testing.T) {
	nodes := []corev1.Node{poolBPFNode("cox1"), poolBPFNode("cox2"), bpfNode("pi0")}
	pods := []corev1.Pod{readyAgentPod("cox1", "ka-agent-cox1"), readyAgentPod("pi0", "ka-agent-pi0")}
	e := a1Enforcer(t, nodes, pods, a1Kapt())
	e.PoolNodeSelector = a1Pool
	enforcing, reason := e.Enforcing(context.Background(), a1Loop())
	if enforcing || reason != ReasonNodeNotEnforcing {
		t.Fatalf("pool node cox2 has no agent: want enforcing=false reason=%s, got %v/%s", ReasonNodeNotEnforcing, enforcing, reason)
	}
}

func TestA1PoolWithNoBPFNodeIsNotEnforcing(t *testing.T) {
	pool := bpfNode("cox1")
	delete(pool.Labels, "kubearmor.io/enforcer")
	pool.Labels["coxswain.io/sandbox"] = "true"
	nodes := []corev1.Node{pool, bpfNode("pi0")}
	pods := []corev1.Pod{readyAgentPod("cox1", "ka-agent-cox1"), readyAgentPod("pi0", "ka-agent-pi0")}
	e := a1Enforcer(t, nodes, pods, a1Kapt())
	e.PoolNodeSelector = a1Pool
	enforcing, reason := e.Enforcing(context.Background(), a1Loop())
	if enforcing || reason != ReasonNodeNotEnforcing {
		t.Fatalf("no BPF-LSM node in the pool: want enforcing=false reason=%s, got %v/%s", ReasonNodeNotEnforcing, enforcing, reason)
	}
}

func TestA1NoPoolChecksEveryBPFNode(t *testing.T) {
	nodes := []corev1.Node{poolBPFNode("cox1"), bpfNode("pi0")}
	pods := []corev1.Pod{readyAgentPod("cox1", "ka-agent-cox1")}
	e := a1Enforcer(t, nodes, pods, a1Kapt())
	enforcing, reason := e.Enforcing(context.Background(), a1Loop())
	if enforcing || reason != ReasonNodeNotEnforcing {
		t.Fatalf("no pool set: pi0 (BPF-LSM, no agent) must hold the Loop: got %v/%s", enforcing, reason)
	}
}
