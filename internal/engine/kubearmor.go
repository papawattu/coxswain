package engine

import (
	"slices"
	"strings"

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

// kaptActionValue is the spec-level default-deny action (KubeArmor defaults to
// Audit without it; Block is enforced). Shared by the C6b agent policy and the
// I42f proxy policies.
const kaptActionValue = "Block"

// KubeArmorPolicy spec-level keys. The policy is emitted as a fully-shaped
// unstructured object (the API server's structural validation requires the
// real shape), and the three emitters (the C6b agent policy and the I42f
// proxy policies) build the same nested keys; constants here so the emitters
// and the unstructured-map shape cannot drift (and goconst does not flag the
// shared keys). Exported so the controller's envtest helpers read the
// policy's spec through the same keys.
const (
	KaptSpecKey        = "spec"
	KaptSelectorKey    = "selector"
	KaptProcessKey     = "process"
	KaptMatchLabelsKey = "matchLabels"
	KaptMatchPathsKey  = "matchPaths"
	KaptMatchDNSKey    = "matchDNSQueries"
	KaptMatchProtoKey  = "matchProtocols"
	kaptAPIVersionKey  = "apiVersion"
	kaptKindKey        = "kind"
	kaptMetadataKey    = "metadata"
	kaptNameKey        = "name"
	kaptNamespaceKey   = "namespace"
	KaptFileKey        = "file"
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

	// exec allows -> process.matchPaths items ({path}). Each item carries ONLY
	// path: an ABSOLUTE path pattern for the real binary. KubeArmor v1.7.5's
	// BPF-LSM keys the process rule on the exec'd file's dentry name when the
	// item sets execname, and IGNORES path (enforcer/bpflsm/rulesHandling.go:
	// the rule key is execname if present, else path) — so an execname (or the
	// old /**/<name> form) matches any file with that name at ANY depth, and a
	// same-named binary dropped into a writable dir (e.g. /tmp/go) would
	// satisfy the allow. Path-only items match the exec's absolute path, so a
	// spoofed copy elsewhere is denied (reproduced: path-only item blocks
	// /tmp/go with permission denied, execname+path item allows it).
	spec := map[string]any{
		// Default-deny posture: KubeArmor's spec.action defaults to Audit (log
		// only, nothing blocked). Set it to Block so disallows are enforced; the
		// per-rule action: Allow is the carve-out.
		kaptActionKey: kaptActionValue,
		KaptSelectorKey: map[string]any{
			KaptMatchLabelsKey: map[string]any{"coxswain.io/loop": loopName},
		},
	}
	// exec allows -> process.matchPaths items ({path} ONLY), action Allow.
	// P1 #3: process (NOT syscalls, which is monitoring-only and has no action).
	// P1 (R16): path-only — execname overrides path in v1.7.5's BPF-LSM rule
	// keying, so an execname+path item is exactly as spoofable as /**/<name>.
	if len(exec) > 0 {
		items := make([]any, 0, len(exec))
		for _, e := range exec {
			items = append(items, map[string]any{
				"path": execAbsPath(e),
			})
		}
		spec["process"] = map[string]any{
			kaptActionKey:     kaptAllowAction,
			KaptMatchPathsKey: items,
		}
	}
	// network allows → matchDNSQueries items ({domain}) + matchProtocols items
	// ({protocol}). P2: carry the port (protocol) alongside the DNS name so an
	// allow never widens to host:*.
	if len(network) > 0 {
		domains, protocols := splitNetworkAllows(network)
		spec["network"] = map[string]any{
			kaptActionKey:     kaptAllowAction,
			KaptMatchDNSKey:   toDomainItems(domains),
			KaptMatchProtoKey: toProtocolItems(protocols),
		}
	}
	// file allows → file.matchPaths items ({path}).
	if len(files) > 0 {
		spec[KaptFileKey] = map[string]any{
			kaptActionKey:     kaptAllowAction,
			KaptMatchPathsKey: toPathItems(files),
		}
	}

	obj := &unstructured.Unstructured{Object: map[string]any{
		kaptAPIVersionKey: kaptGroup + "/" + kaptVersion,
		kaptKindKey:       kaptKind,
		kaptMetadataKey: map[string]any{
			kaptNameKey:      name,
			kaptNamespaceKey: namespace,
		},
		KaptSpecKey: spec,
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

// execAbsPath returns the absolute path pattern for an exec allow. A bare
// binary name (e.g. "go") is resolved to its canonical location in the sandbox
// image so the match is pinned to the real binary and a same-named binary in a
// writable dir (e.g. /workspace/go) does NOT satisfy the allow (the R15 P1
// spoofing fix). An allow that already carries a path is used as-is.
//
// The name table is a STOPGAP for legacy bare-name allows: PR #7 (C6a) makes
// AgentPolicy.exec take absolute paths validated at admission (rejecting bare
// names), and once that lands this function only passes validated paths through
// — the table should then be deleted, not extended.
func execAbsPath(e string) string {
	if strings.Contains(e, "/") {
		return e
	}
	if p, ok := defaultExecPaths[e]; ok {
		return p
	}
	// Unknown binary: fall back to the conventional /usr/bin location. Still
	// absolute, so a spoofed copy elsewhere does not match.
	return "/usr/bin/" + e
}

// defaultExecPaths maps common binaries to their canonical location in the
// default Go dev sandbox image (docker.io/library/golang:1.26). These are the
// locations the exec allow actually needs to reach; the agent's PATH includes
// them. If a future agent image differs, the allow should carry the full path.
var defaultExecPaths = map[string]string{
	"go":     "/usr/local/go/bin/go",
	"git":    "/usr/bin/git",
	"sh":     "/bin/sh",
	"bash":   "/bin/bash",
	"curl":   "/bin/curl",
	"wget":   "/bin/wget",
	"python": "/usr/bin/python",
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
// LOST — the allow widens to host:*, and KubeArmor alone cannot keep the
// source rule's precision. That is the condition for the PolicyTranslationLossy
// Loop status flag (D33 follow-up): whenever the effective policy carries a
// host:PORT network allow, the operator must record it; the NetworkPolicy
// (D34, post-C6) carries the host:port precision where KubeArmor cannot.
// splitNetworkAllows splits network allows into (dnsNames, protocols).
// KubeArmor matches egress by DNS query name + protocol — it CANNOT express
// a host:port allow (the schema's matchProtocols items are protocol NAMES:
// tcp/udp, not "tcp:443"). So a "host:443" allow becomes "host" + protocol
// "tcp": the port is LOST — the allow widens to host:*, and KubeArmor alone
// cannot keep the source rule's precision. That is the condition for the
// PolicyTranslationLossy Loop status flag (D33 follow-up).
//
// Special formats:
//   - "dns/udp+tcp" (the platform-minimum DNS allow): produces protocols
//     ["udp", "tcp"] with no domain (it's a protocol allow, not a query).
//   - "host:port": produces domain "host" + protocol "tcp" (port is lost).
//   - "host" (bare): produces domain "host" only.
func splitNetworkAllows(ends []string) (domains, protocols []string) {
	for _, e := range ends {
		// Platform-minimum DNS allow: "dns/udp+tcp" -> protocols [udp, tcp].
		if e == "dns/udp+tcp" {
			protocols = append(protocols, "udp", "tcp")
			continue
		}
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

// TranslationResult reports what the engine's translation dropped. The
// controller uses this to set PolicyTranslationLossy on the Loop when the
// translation is lossy (I41: a host:PORT allow that loses its port must be
// reported, not silently widened).
type TranslationResult struct {
	// DroppedNetworkPorts lists the host:port allows that lost their port
	// during translation (e.g. "pypi.org:443" -> "pypi.org" + tcp). Empty
	// means the translation was not lossy.
	DroppedNetworkPorts []string
}

// NetworkLossy returns the host:port network allows that the KubeArmor
// translation cannot express at that precision (the port is dropped). A bare
// host (no port) is not lossy. This is the single source of truth for the
// PolicyTranslationLossy condition (I41).
func NetworkLossy(ends []string) []string {
	var lossy []string
	for _, e := range ends {
		_, port := splitHostPort(e)
		if port != "" {
			lossy = append(lossy, e)
		}
	}
	return lossy
}
