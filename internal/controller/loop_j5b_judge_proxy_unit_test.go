// Phase 8 J5b (PLAN-PHASE8.md, D132, STEP 1 of 3): RED specs for the Judge's
// own model proxy. The Judge endpoint of its own gets a second model-proxy
// pod + Service, <loop>-jproxy (55-char Loop name + 7 = 62 <= 63 Service-name
// limit; "-judge-proxy" would not fit), carrying the Judge endpoint's Secret
// and metered into the Loop's totals (ADR-0009).
package controller

import (
	"strings"
	"testing"
)

func TestJudgeProxyName(t *testing.T) {
	if got := judgeProxyName("loop-b"); got != "loop-b-jproxy" {
		t.Fatalf("judgeProxyName: got %q, want loop-b-jproxy", got)
	}
	// A 55-char Loop name (the sandbox-name limit, D20) must still give a
	// Service name of at most 63 chars.
	name := strings.Repeat("a", 55)
	if got := judgeProxyName(name); len(got) > 63 {
		t.Fatalf("judgeProxyName with a 55-char Loop name: %q is %d chars, want <= 63", got, len(got))
	}
	if got := judgeProxyName(name); got != name+"-jproxy" {
		t.Fatalf("judgeProxyName with a 55-char Loop name: got %q, want %s-jproxy", got, name)
	}
}

func TestJudgeOwnEndpoint(t *testing.T) {
	// No Judge at all.
	none := judgeTestLoop("loop-b")
	none.Spec.Verify.Judge = nil
	if judgeOwnEndpoint(none) {
		t.Fatal("judgeOwnEndpoint: want false when there is no Judge")
	}
	// Judge without its own endpoint: the Loop's proxy serves it (J5a).
	loop := judgeTestLoop("loop-b")
	loop.Spec.Verify.Judge.ModelEndpoint = ""
	if judgeOwnEndpoint(loop) {
		t.Fatal("judgeOwnEndpoint: want false when Judge.ModelEndpoint is empty (the Loop's proxy serves the Judge)")
	}
	// Judge with its own endpoint.
	loop.Spec.Verify.Judge.ModelEndpoint = "judge-vllm.judge-ns.svc:8001"
	loop.Spec.Verify.Judge.EndpointSecretRef = "judge-key"
	if !judgeOwnEndpoint(loop) {
		t.Fatal("judgeOwnEndpoint: want true when the Judge names its own endpoint")
	}
}

// TestJudgeEnvOwnEndpoint: with its own endpoint the Judge's call goes to the
// Judge's own proxy; without one it keeps the Loop's proxy URL (D123/J5a).
func TestJudgeEnvOwnEndpoint(t *testing.T) {
	r := &LoopReconciler{}
	loop := judgeTestLoop("loop-b")
	loop.Spec.Verify.Judge.ModelEndpoint = "judge-vllm.judge-ns.svc:8001"
	loop.Spec.Verify.Judge.EndpointSecretRef = "judge-key"
	env := r.judgeEnvValueMap(loop)
	if got := env["COX_JUDGE_BASE_URL"]; got != "http://loop-b-jproxy.judge-ns.svc.cluster.local:8080" {
		t.Fatalf("own endpoint: COX_JUDGE_BASE_URL: got %q, want http://loop-b-jproxy.judge-ns.svc.cluster.local:8080", got)
	}
	loop.Spec.Verify.Judge.ModelEndpoint = ""
	loop.Spec.Verify.Judge.EndpointSecretRef = ""
	env = r.judgeEnvValueMap(loop)
	if got := env["COX_JUDGE_BASE_URL"]; got != "http://loop-b-proxy.judge-ns.svc.cluster.local:8080" {
		t.Fatalf("no own endpoint: COX_JUDGE_BASE_URL: got %q, want http://loop-b-proxy.judge-ns.svc.cluster.local:8080", got)
	}
}

func TestBuildJudgeProxyPod(t *testing.T) {
	r := &LoopReconciler{}
	loop := judgeTestLoop("loop-b")
	loop.Spec.Agent.EndpointSecretRef = "agent-key"
	loop.Spec.Verify.Judge.ModelEndpoint = "judge-vllm.judge-ns.svc:8001"
	loop.Spec.Verify.Judge.EndpointSecretRef = "judge-key"

	pod := r.buildJudgeProxyPod(loop)
	if pod == nil {
		t.Fatal("buildJudgeProxyPod: got nil, want a pod")
	}
	if pod.Name != "loop-b-jproxy" {
		t.Fatalf("pod name: got %q, want loop-b-jproxy", pod.Name)
	}
	foundEndpoint := false
	for _, c := range pod.Spec.Containers {
		for _, e := range c.Env {
			if e.Name == "MODEL_ENDPOINT" {
				foundEndpoint = true
				if e.Value != "http://judge-vllm.judge-ns.svc:8001" {
					t.Fatalf("MODEL_ENDPOINT: got %q, want http://judge-vllm.judge-ns.svc:8001", e.Value)
				}
			}
		}
	}
	if !foundEndpoint {
		t.Fatal("buildJudgeProxyPod: no MODEL_ENDPOINT env var on the proxy container")
	}
	// The Secret volume carries the Judge's key, not the Loop agent's.
	foundSecret := false
	for _, v := range pod.Spec.Volumes {
		if v.Secret != nil {
			if v.Secret.SecretName != "judge-key" {
				t.Fatalf("Secret volume: SecretName got %q, want judge-key", v.Secret.SecretName)
			}
			foundSecret = true
		}
	}
	if !foundSecret {
		t.Fatal("buildJudgeProxyPod: no Secret volume, want one named judge-key")
	}
	for _, v := range pod.Spec.Volumes {
		if v.Secret != nil && v.Secret.SecretName == loop.Spec.Agent.EndpointSecretRef {
			t.Fatalf("the Judge proxy must NOT mount the agent's Secret %q", loop.Spec.Agent.EndpointSecretRef)
		}
	}
	// The Loop's own proxy is unchanged: it still carries the AGENT's
	// endpoint and Secret (D123).
	agentPod := r.buildProxyPod(loop, "loop-b", "judge-ns", "img")
	if agentPod == nil {
		t.Fatal("buildProxyPod: got nil, want the Loop's proxy pod")
	}
	agentEndpoint := false
	for _, c := range agentPod.Spec.Containers {
		for _, e := range c.Env {
			if e.Name == "MODEL_ENDPOINT" && e.Value == "http://vllm.svc:8000" {
				agentEndpoint = true
			}
		}
	}
	if !agentEndpoint {
		t.Fatal("the Loop's proxy pod must still carry the agent's MODEL_ENDPOINT (http://vllm.svc:8000)")
	}
	agentSecret := false
	for _, v := range agentPod.Spec.Volumes {
		if v.Secret != nil && v.Secret.SecretName == "agent-key" {
			agentSecret = true
		}
	}
	if !agentSecret {
		t.Fatal("the Loop's proxy pod must still mount the agent's Secret (agent-key)")
	}
}
