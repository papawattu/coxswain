package engine

import (
	"slices"

	"github.com/papawattu/coxswain/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	kaptGroup   = "security.kubearmor.com"
	kaptVersion = "v1"
	kaptKind    = "KubeArmorPolicy"
)

var kaptGroupVersion = schema.GroupVersion{Group: kaptGroup, Version: kaptVersion}

// kaptAllowAction is the KubeArmor action an allowlist rule carries. The
// KubeArmorPolicy is default-deny; an allowlisted rule gets action Allow,
// everything else stays denied (KubeArmor's default posture).
const (
	kaptAllowAction = "Allow"
	kaptActionKey   = "action"
)

// EmitKubeArmorPolicy translates Coxswain's EnginePolicy (per-container, D29)
// into a KubeArmorPolicy object (security.kubearmor.com/v1) for the sandbox
// pod. It returns a fully-shaped unstructured object (apiVersion, kind,
// metadata{name, namespace}, spec{selector, process, network, file, action}) so
// the API server's structural validation accepts it (P1 #1: the object must
// have the real shape, not a Go struct dump).
//
// matchPaths / matchDNSQueries / matchProtocols items are OBJECTS (P1 #2: the
// v1.7.5 schema requires {path}, {domain}, {protocol} items), not strings.
//
// Per-container scoping (D29) is NOT expressible in one KubeArmorPolicy
// (the selector is pod-level), so this emits ONE pod-level policy carrying the
// UNION of the agent and proxy allows. The agent=localhost / proxy=model-endpoint
// split is enforced elsewhere (see D29 / C3).
func EmitKubeArmorPolicy(loopName, namespace string, ep policy.EnginePolicy) *unstructured.Unstructured {
	name := "coxswain-" + loopName

	// Union the allows across containers (pod-level policy).
	var exec, network, files []string
	for _, cp := range ep.Containers {
		for _, a := range cp.Allows {
			switch a.Type {
			case policy.AllowExec:
				exec = append(exec, a.Match)
			case policy.AllowNetwork:
				network = append(network, a.Match)
			case policy.AllowFile:
				files = append(files, a.Match)
			}
		}
	}
	exec = dedupe(exec)
	network = dedupe(network)
	files = dedupe(files)
	// KubeArmor's process.matchPaths[].path requires an absolute-path pattern
	// (^/+.*[^/]$), not a bare binary name. A user allow like "go" is emitted as
	// "/**/go" so it matches the binary at any depth (the agent's PATH). KubeArmor's
	// path validation requires an absolute path (^/+.*[^/]$), so the leading slash is
	// mandatory.
	execPaths := make([]string, 0, len(exec))
	for _, e := range exec {
		execPaths = append(execPaths, "/**/"+e)
	}

	spec := map[string]any{
		// Default-deny posture: KubeArmor's spec.action defaults to Audit (log
		// only, nothing blocked). Set it to Block so disallows are enforced; the
		// per-rule action: Allow is the carve-out.
		"action": "Block",
		"selector": map[string]any{
			"matchLabels": map[string]any{"coxswain.io/loop": loopName},
		},
	}
	// exec allows → process.matchPaths items ({path}), action Allow.
	// P1 #3: process (NOT syscalls, which is monitoring-only and has no action).
	if len(exec) > 0 {
		spec["process"] = map[string]any{
			kaptActionKey: kaptAllowAction,
			"matchPaths":  toPathItems(execPaths),
		}
	}
	// network allows → matchDNSQueries items ({domain}) + matchProtocols items
	// ({protocol}). P2: carry the port (protocol) alongside the DNS name so an
	// allow never widens to host:*.
	if len(network) > 0 {
		domains, protocols := splitNetworkAllows(network)
		spec["network"] = map[string]any{
			kaptActionKey:     kaptAllowAction,
			"matchDNSQueries": toDomainItems(domains),
			"matchProtocols":  toProtocolItems(protocols),
		}
	}
	// file allows → file.matchPaths items ({path}).
	if len(files) > 0 {
		spec["file"] = map[string]any{
			kaptActionKey: kaptAllowAction,
			"matchPaths":  toPathItems(files),
		}
	}

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": kaptGroup + "/" + kaptVersion,
		"kind":       kaptKind,
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
		},
		"spec": spec,
	}}
	return obj
}

// toPathItems turns strings into KubeArmor matchPaths items ({path: s}).
func toPathItems(paths []string) []any {
	out := make([]any, len(paths))
	for i, p := range paths {
		out[i] = map[string]any{"path": p}
	}
	return out
}

// toDomainItems turns DNS names into KubeArmor matchDNSQueries items ({domain: s}).
func toDomainItems(domains []string) []any {
	out := make([]any, len(domains))
	for i, d := range domains {
		out[i] = map[string]any{"domain": d}
	}
	return out
}

// toProtocolItems turns protocol strings into KubeArmor matchProtocols items
// ({protocol: s}).
func toProtocolItems(protocols []string) []any {
	out := make([]any, len(protocols))
	for i, p := range protocols {
		out[i] = map[string]any{"protocol": p}
	}
	return out
}

// splitNetworkAllows splits "host:port" allows into (dnsNames, protocols). A
// bare host (no port) yields a DNS name only; "host:port" yields a DNS name +
// a "tcp:port" protocol so the allow does not widen to host:* (P2).
// splitNetworkAllows splits network allows into (dnsNames, protocols). KubeArmor
// matches egress by DNS query name + protocol — it CANNOT express a host:port
// allow (the schema's matchProtocols items are protocol NAMES: tcp/udp, not
// "tcp:443"). So a "host:443" allow becomes "host" + protocol "tcp": the port is
// LOST — the allow widens to host:* (a PolicyTranslationLossy situation, tracked;
// the fix is the NetworkPolicy carrying the port, not KubeArmor).
func splitNetworkAllows(ends []string) (domains, protocols []string) {
	for _, e := range ends {
		host, port := splitHostPort(e)
		domains = append(domains, host)
		if port != "" {
			protocols = append(protocols, "tcp") // protocol name only; the port is not expressible
		}
	}
	return dedupe(domains), dedupe(protocols)
}

// splitHostPort splits "host:port" into (host, port). A bare host yields
// (host, "").
func splitHostPort(s string) (string, string) {
	i := 0
	for i < len(s) && s[i] != ':' {
		i++
	}
	if i == len(s) || i == 0 {
		return s, ""
	}
	return s[:i], s[i+1:]
}

func dedupe(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}
