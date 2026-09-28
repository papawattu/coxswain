package engine

import (
	"slices"
	"testing"

	"github.com/papawattu/coxswain/internal/policy"
)

const (
	testExecGit = "git"
	testExecGo  = "go"
)

func epForTest() policy.EnginePolicy {
	return policy.Translate(policy.EffectivePolicy{
		Exec:    []string{testExecGit, testExecGo},
		Network: []string{"proxy.golang.org:443"},
		Files:   []string{"/data"},
	}, "l1-proxy.ns.svc")
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

// P1 #3: exec allows are emitted under process.matchPaths (path-only objects),
// action Allow — NOT syscalls.
// P1 (R16): the item carries ONLY the ABSOLUTE path for the real binary — no
// execname key at all. KubeArmor v1.7.5's BPF-LSM keys the process rule on the
// exec'd file's dentry name when the item sets execname and IGNORES path, so
// execname+path matches any file named <basename> anywhere (the same spoof hole
// as /**/<name>). A path-only item matches the exec's absolute path, so a
// spoofed copy (e.g. /tmp/go) is denied.
func TestEmitKubeArmorPolicyExecPathIsAbsoluteNotSpoofable(t *testing.T) {
	obj := EmitKubeArmorPolicy("l1", "ns", policy.Translate(policy.EffectivePolicy{
		Exec: []string{"go"},
	}, "l1-proxy.ns.svc"))
	spec := obj.Object["spec"].(map[string]any)
	proc, ok := spec["process"].(map[string]any)
	if !ok {
		t.Fatal("no process block")
	}
	items, _ := proc["matchPaths"].([]any)
	if len(items) == 0 {
		t.Fatal("no process.matchPaths")
	}
	first := items[0].(map[string]any)
	// The path must be an absolute location for the real go binary, not /**/go.
	if first["path"] != "/usr/local/go/bin/go" {
		t.Errorf("exec allow %q must be emitted as the absolute path /usr/local/go/bin/go (not the spoofable /**/go), got %q", "go", first["path"])
	}
	// No execname key: with execname set, v1.7.5's rule key is the execname and
	// path is ignored, so any file named "go" anywhere would satisfy the allow.
	if _, has := first["execname"]; has {
		t.Errorf("exec allow item must NOT set execname (it overrides path in v1.7.5's BPF-LSM rule keying and re-opens the spoof hole), got %q", first["execname"])
	}
}

// The policy must be default-deny at the spec level: spec.action = Block, with the
// per-rule action: Allow as the carve-out. Without it KubeArmor defaults to Audit
// (log only) and nothing is actually blocked.
func TestEmitKubeArmorPolicyTopLevelActionIsBlock(t *testing.T) {
	obj := EmitKubeArmorPolicy("l1", "ns", policy.Translate(policy.EffectivePolicy{
		Exec: []string{"go"},
	}, "l1-proxy.ns.svc"))
	spec := obj.Object["spec"].(map[string]any)
	if spec["action"] != "Block" {
		t.Fatalf("spec.action must be Block (default-deny), got %v (KubeArmor defaults to Audit without it)", spec["action"])
	}
}

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
		// P1 #2: items are objects, not strings. P1 (R16): path-only items.
		m := it.(map[string]any)
		paths = append(paths, m["path"].(string))
		if _, has := m["execname"]; has {
			t.Fatalf("process.matchPaths item %v must not carry execname (it overrides path in v1.7.5's rule keying)", m)
		}
	}
	// Absolute paths for the real binaries (not /**/git, /**/go).
	if !slices.Contains(paths, "/usr/bin/git") || !slices.Contains(paths, "/usr/local/go/bin/go") {
		t.Fatalf("process.matchPaths must include /usr/bin/git and /usr/local/go/bin/go, got %v", paths)
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
	domains := make([]string, 0, len(dns))
	for _, it := range dns {
		domains = append(domains, it.(map[string]any)["domain"].(string))
	}
	if !slices.Contains(domains, "proxy.golang.org") {
		t.Fatalf("matchDNSQueries must include proxy.golang.org (port stripped from the name), got %v", domains)
	}
	prots := net["matchProtocols"].([]any)
	protocols := make([]string, 0, len(prots))
	for _, it := range prots {
		protocols = append(protocols, it.(map[string]any)["protocol"].(string))
	}
	if !slices.Contains(protocols, "tcp") {
		t.Fatalf("matchProtocols must carry the protocol name (tcp), got %v", protocols)
	}
}

func TestEmitKubeArmorPolicyUnionsContainers(t *testing.T) {
	agent := policy.Translate(policy.EffectivePolicy{Exec: []string{"/usr/bin/git"}}, "loop-x-proxy.ns-x.svc")
	proxy := policy.Translate(policy.EffectivePolicy{}, "")
	ep := policy.EnginePolicy{Containers: []policy.ContainerPolicy{agent.Containers[0], proxy.Containers[0]}}
	obj := EmitKubeArmorPolicy("loop-x", "ns-x", ep)
	spec := obj.Object["spec"].(map[string]any)
	items := spec["process"].(map[string]any)["matchPaths"].([]any)
	paths := make([]string, 0, len(items))
	for _, it := range items {
		paths = append(paths, it.(map[string]any)["path"].(string))
	}
	if len(paths) != 1 || paths[0] != "/usr/bin/git" {
		t.Fatalf("union across containers must keep the agent's exec allow (/usr/bin/git), got %v", paths)
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
func TestNetworkLossy(t *testing.T) {
	// host:PORT allows are lossy (the port is dropped).
	lossy := NetworkLossy([]string{"pypi.org:443", "localhost:8080"})
	if len(lossy) != 2 {
		t.Fatalf("expected 2 lossy allows, got %d: %v", len(lossy), lossy)
	}
	if lossy[0] != "pypi.org:443" || lossy[1] != "localhost:8080" {
		t.Errorf("unexpected lossy allows: %v", lossy)
	}

	// Bare hosts are NOT lossy.
	lossy = NetworkLossy([]string{"pypi.org", "localhost"})
	if len(lossy) != 0 {
		t.Errorf("expected 0 lossy allows for bare hosts, got %d: %v", len(lossy), lossy)
	}

	// Mixed: only the host:port ones are lossy.
	lossy = NetworkLossy([]string{"pypi.org", "github.com:443"})
	if len(lossy) != 1 || lossy[0] != "github.com:443" {
		t.Errorf("expected [github.com:443], got %v", lossy)
	}

	// Empty input.
	lossy = NetworkLossy(nil)
	if len(lossy) != 0 {
		t.Errorf("expected 0 lossy allows for nil input, got %d", len(lossy))
	}
}

// TestEmitKubeArmorPolicyDNSAllowVerifiesUDPAndTCP verifies that the
// platform-minimum DNS allow (dns/udp+tcp) produces matchProtocols [udp, tcp]
// in the KubeArmorPolicy network block (D33: the agent needs both UDP and TCP
// DNS to resolve the proxy Service FQDN).
func TestEmitKubeArmorPolicyDNSAllowUDPAndTCP(t *testing.T) {
	obj := EmitKubeArmorPolicy("l1", "ns", policy.Translate(policy.EffectivePolicy{}, "l1-proxy.ns.svc"))
	spec := obj.Object["spec"].(map[string]any)
	net := spec["network"].(map[string]any)
	protocols := net["matchProtocols"].([]any)
	got := map[string]bool{}
	for _, item := range protocols {
		p := item.(map[string]any)
		got[p["protocol"].(string)] = true
	}
	if !got["udp"] {
		t.Errorf("matchProtocols must include udp (for DNS), got %v", protocols)
	}
	if !got["tcp"] {
		t.Errorf("matchProtocols must include tcp (for DNS over TCP), got %v", protocols)
	}
	// The proxy Service FQDN must be a matchDNSQueries item.
	domains := net["matchDNSQueries"].([]any)
	found := false
	for _, item := range domains {
		d := item.(map[string]any)
		if d["domain"] == "l1-proxy.ns.svc" {
			found = true
		}
	}
	if !found {
		t.Errorf("matchDNSQueries must include l1-proxy.ns.svc (the proxy Service FQDN), got %v", domains)
	}
}
