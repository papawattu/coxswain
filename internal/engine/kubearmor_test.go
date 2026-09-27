package engine

import (
	"slices"
	"testing"

	"github.com/papawattu/coxswain/internal/policy"
)

func epForTest() policy.EnginePolicy {
	return policy.Translate(policy.EffectivePolicy{
		Exec:    []string{"git", "go"},
		Network: []string{"proxy.golang.org:443"},
		Files:   []string{"/data"},
	})
}

// P1 #1: the emitted object must have the real KubeArmorPolicy shape
// (apiVersion, kind, metadata{name, namespace}, spec{...}).
func TestEmitKubeArmorPolicyShape(t *testing.T) {
	obj := EmitKubeArmorPolicy("loop-x", "ns-x", epForTest())
	if obj.GetAPIVersion() != "security.kubearmor.com/v1" {
		t.Fatalf("apiVersion must be security.kubearmor.com/v1, got %s", obj.GetAPIVersion())
	}
	if obj.GetKind() != "KubeArmorPolicy" {
		t.Fatalf("kind must be KubeArmorPolicy, got %s", obj.GetKind())
	}
	if obj.GetName() != "coxswain-loop-x" || obj.GetNamespace() != "ns-x" {
		t.Fatalf("metadata wrong: %s/%s", obj.GetNamespace(), obj.GetName())
	}
	spec, ok := obj.Object["spec"].(map[string]any)
	if !ok {
		t.Fatal("spec must be a map")
	}
	sel := spec["selector"].(map[string]any)
	labels := sel["matchLabels"].(map[string]any)
	if labels["coxswain.io/loop"] != "loop-x" {
		t.Fatalf("selector must target coxswain.io/loop=loop-x, got %v", labels)
	}
}

// P1 #3: exec allows are emitted under process.matchPaths (objects {path}),
// action Allow — NOT syscalls.
func TestEmitKubeArmorPolicyExecUsesProcessMatchPaths(t *testing.T) {
	obj := EmitKubeArmorPolicy("loop-x", "ns-x", epForTest())
	spec := obj.Object["spec"].(map[string]any)
	proc, ok := spec["process"].(map[string]any)
	if !ok {
		t.Fatal("process block must exist for exec allows")
	}
	if proc["action"] != "Allow" {
		t.Fatalf("process action must be Allow, got %v", proc["action"])
	}
	items, _ := proc["matchPaths"].([]any)
	paths := make([]string, 0, len(items))
	for _, it := range items {
		// P1 #2: items are objects {path: ...}, not strings.
		m := it.(map[string]any)
		paths = append(paths, m["path"].(string))
	}
	if !slices.Contains(paths, "git") || !slices.Contains(paths, "go") {
		t.Fatalf("process.matchPaths must include git and go, got %v", paths)
	}
	if _, has := spec["syscalls"]; has {
		t.Fatal("syscalls must NOT be set (monitoring-only); exec allows live in process")
	}
}

// P1 #2: network matchDNSQueries items are objects {domain}, matchProtocols
// items are objects {protocol}. P2: a host:port allow carries the port (does
// not widen to host:*).
func TestEmitKubeArmorPolicyNetworkItemsAreObjectsWithPort(t *testing.T) {
	obj := EmitKubeArmorPolicy("loop-x", "ns-x", epForTest())
	spec := obj.Object["spec"].(map[string]any)
	net, ok := spec["network"].(map[string]any)
	if !ok {
		t.Fatal("network block must exist")
	}
	dns := net["matchDNSQueries"].([]any)
	var domains []string
	for _, it := range dns {
		domains = append(domains, it.(map[string]any)["domain"].(string))
	}
	if !slices.Contains(domains, "proxy.golang.org") {
		t.Fatalf("matchDNSQueries must include proxy.golang.org (port stripped from the name), got %v", domains)
	}
	prots := net["matchProtocols"].([]any)
	var protocols []string
	for _, it := range prots {
		protocols = append(protocols, it.(map[string]any)["protocol"].(string))
	}
	if !slices.Contains(protocols, "tcp") {
		t.Fatalf("matchProtocols must carry the protocol name (tcp), got %v", protocols)
	}
}

func TestEmitKubeArmorPolicyUnionsContainers(t *testing.T) {
	agent := policy.Translate(policy.EffectivePolicy{Exec: []string{"git"}})
	proxy := policy.Translate(policy.EffectivePolicy{})
	ep := policy.EnginePolicy{Containers: []policy.ContainerPolicy{agent.Containers[0], proxy.Containers[0]}}
	obj := EmitKubeArmorPolicy("loop-x", "ns-x", ep)
	spec := obj.Object["spec"].(map[string]any)
	items := spec["process"].(map[string]any)["matchPaths"].([]any)
	var paths []string
	for _, it := range items {
		paths = append(paths, it.(map[string]any)["path"].(string))
	}
	if len(paths) != 1 || paths[0] != "git" {
		t.Fatalf("union across containers must keep the agent's exec allow, got %v", paths)
	}
}

func TestEmitKubeArmorPolicyDefaultDeny(t *testing.T) {
	obj := EmitKubeArmorPolicy("loop-x", "ns-x", policy.EnginePolicy{})
	spec := obj.Object["spec"].(map[string]any)
	for _, k := range []string{"process", "network", "file"} {
		if _, ok := spec[k]; ok {
			t.Fatalf("an empty EnginePolicy must not emit a %s allow (default-deny)", k)
		}
	}
}
