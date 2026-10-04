// Package toolproxy is the D41a tool proxy (ADR-0008): the generic HTTP tool
// proxy. It is a **reverse proxy** — the agent speaks plain HTTP, origin-form,
// in-cluster to the proxy; the proxy rule-checks, strips agent-supplied
// auth, injects the credential, and originates its own request (TLS,
// verified) to a single fixed upstream — not a forward proxy: a forward
// proxy's CONNECT tunnel to an https upstream is opaque TLS, so it could
// neither rule-check nor inject (PR #70 review, the D41a P1).
//
// Request shape: the only accepted shape is a plain-HTTP, origin-form
// request (a relative path). CONNECT and absolute-form URIs are rejected
// with 405 and never reach the network. The path is normalised (rejecting
// traversal, encoded slashes and NUL, or collapsing redundant slashes and
// dot segments) BEFORE the rule check, so no encoding can route a request
// past a rule. Matching is **segment prefix**: a rule prefix covers a path
// only when the path equals the prefix or continues with a `/` boundary —
// a segment, not a byte string, is the unit of match.
//
// Credential injection: if a credential file is configured, the proxy
// strips any agent-supplied Authorization / Proxy-Authorization and sets
// `Authorization: Bearer <file contents>`. Without one, agent headers are
// still stripped (a forged Authorization must not reach the upstream).
//
// No redirect following: a 3xx is returned to the agent as-is; the proxy
// never re-resolves or dials a Location target.
package toolproxy

import (
	"encoding/json"
	"strings"
)

// Rule is one entry of the tool's request rules: an allowed method list and
// an allowed path-prefix list. A request matches a rule when its method is
// in Methods AND its normalised path matches any prefix in Prefixes
// (segment-prefix semantics, see matchSegmentPrefix).
type Rule struct {
	Methods []string `json:"methods"`
	Paths   []string `json:"paths"`
}

// ParseRules parses TOOL_RULES_JSON (an array of {methods: [...], paths:
// [...]}). A bad/empty value yields an empty (default-deny) rule set —
// fail-closed: with no rules, everything is 403.
func ParseRules(jsonStr string) []Rule {
	var out []Rule
	if err := json.Unmarshal([]byte(jsonStr), &out); err != nil {
		return nil
	}
	return out
}

// matchSegmentPrefix reports whether path (already normalised) is covered by
// prefix. The match is on path segments, not bytes: prefix "/repos/acme/"
// covers "/repos/acme/x/y" (the path continues at a `/` boundary) but NOT
// "/repos/acmer" (a different segment that merely shares a byte prefix).
func matchSegmentPrefix(prefix, path string) bool {
	if prefix == path {
		return true
	}
	return strings.HasPrefix(path, prefix) && strings.HasSuffix(prefix, "/")
}

// ruleMatch reports whether the method and normalised path match any rule.
func ruleMatch(rules []Rule, method, path string) bool {
	method = strings.ToUpper(method)
	for _, r := range rules {
		if !methodAllowed(r.Methods, method) {
			continue
		}
		for _, p := range r.Paths {
			if matchSegmentPrefix(p, path) {
				return true
			}
		}
	}
	return false
}

func methodAllowed(methods []string, method string) bool {
	for _, m := range methods {
		if strings.EqualFold(m, method) {
			return true
		}
	}
	return false
}
