package toolproxy

import (
	"reflect"
	"testing"
)

// The rule engine is segment-prefix, table-driven. A rule matches when the
// method is in its methods AND the normalised path is a segment prefix of the
// rule's prefixes.
func TestRuleMatch(t *testing.T) {
	rules := []Rule{
		{Methods: []string{methodGET}, Paths: []string{toolPrefix}},
		{Methods: []string{methodGET, methodPOST}, Paths: []string{prefixRepos}},
	}
	cases := []struct {
		name   string
		method string
		path   string
		want   bool
	}{
		{"allowed method+prefix", methodGET, toolPath, true},
		{"allowed prefix subpath", methodGET, "/repos/acme/x/y", true},
		{"exact prefix is a match", methodGET, toolPrefix, true},
		{"wrong method", methodDELETE, toolPath, false},
		{"disallowed prefix", methodGET, "/orgs/acme/repo", false},
		{"segment boundary: /repos/acme/ does NOT cover /repos/acmer", methodGET, segmentBoundary, false},
		{"exact /repos is covered by its own rule", methodGET, prefixRepos, true},
		{"exact /repos does not cover /repos2", methodGET, "/repos2", false},
		{"POST is allowed for /repos", methodPOST, prefixRepos, true},
		{"method case-insensitive (lowercase get)", "get", toolPath, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ruleMatch(rules, tc.method, tc.path); got != tc.want {
				t.Fatalf("ruleMatch(%q, %q) = %v, want %v", tc.method, tc.path, got, tc.want)
			}
		})
	}
}

// Empty rule set → everything is 403 (default-deny).
func TestEmptyRulesDenyAll(t *testing.T) {
	if ruleMatch(nil, methodGET, toolPath) {
		t.Fatal("empty rule set must not allow anything")
	}
	if ruleMatch([]Rule{}, methodGET, "/") {
		t.Fatal("empty rule set must not allow anything")
	}
}

// Segment-prefix semantics stated in the package doc, pinned here.
func TestSegmentPrefixSemantics(t *testing.T) {
	if !matchSegmentPrefix(toolPrefix, "/repos/acme/x/y") {
		t.Fatal("/repos/acme/ must cover /repos/acme/x/y")
	}
	if matchSegmentPrefix(toolPrefix, segmentBoundary) {
		t.Fatal("/repos/acme/ must NOT cover /repos/acmer (a different segment)")
	}
}

func TestParseRules(t *testing.T) {
	rules := ParseRules(`[{"methods":["GET"],"paths":["/a/"]},{"methods":["POST"],"paths":["/b"]}]`)
	if len(rules) != 2 {
		t.Fatalf("got %d rules, want 2", len(rules))
	}
	if !reflect.DeepEqual(rules[0], Rule{Methods: []string{methodGET}, Paths: []string{"/a/"}}) {
		t.Fatalf("bad rule 0: %+v", rules[0])
	}
	// A bad/empty value yields an empty (default-deny) rule set.
	if got := ParseRules(`not-json`); len(got) != 0 {
		t.Fatalf("bad JSON must yield no rules, got %+v", got)
	}
	if got := ParseRules(""); len(got) != 0 {
		t.Fatalf("empty value must yield no rules, got %+v", got)
	}
}

// Path normalisation is applied BEFORE the rule check. Rejected encodings
// (backslash, NUL, root-traversal) → 400 (the handler); cleaned forms land on
// the same verdict as their canonical path.
func TestNormalisePath(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
		err  bool
	}{
		{"plain", toolPath, toolPath, false},
		{"single slash collapse", "/a//b///c", "/a/b/c", false},
		{"trailing slash cleaned", "/a/b/", "/a/b", false},
		{"root stays root", "/", "/", false},
		{"dot segment dropped", "/a/./b", "/a/b", false},
		{"non-escaping dotdot rejected", "/repos/acme/../repo", "", true},
		{"percent-encoded dotdot rejected", traversalPCT, "", true},
		{"percent-encoded slash decoded", "/repos/%2fetc", "/repos/etc", false},
		{"bare path gets leading slash", "repos/acme", "/repos/acme", false},
		{"root traversal rejected", traversalDotDot, "", true},
		{"top-level dotdot rejected", "/../etc", "", true},
		{"encoded root traversal rejected", "/repos/acme%2f..%2f..%2fetc", "", true},
		{"backslash rejected", "/repos/\\../x", "", true},
		{"forward-backslash rejected", "/repos\\acme", "", true},
		{"NUL rejected", "/repos/a\000b", "", true},
		{"bad percent escape rejected", "/repos/%zz", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalisePath(tc.raw)
			if tc.err {
				if err == nil {
					t.Fatalf("normalisePath(%q) = %q, want error", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalisePath(%q) error: %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("normalisePath(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// The table-driven bypass cases from the plan: these encoded forms land on
// the SAME verdict as their canonical form — never on an allowed rule's
// prefix by accident. The rule here allows /repos/acme/; the canonical
// forms of these bypass attempts are /etc (a 403, no dial).
func TestNormalisedBeforeMatching(t *testing.T) {
	rules := []Rule{{Methods: []string{methodGET}, Paths: []string{toolPrefix}}}
	bypass := []struct {
		raw  string
		want bool
	}{
		{traversalDotDot, false},
		{traversalPCT, false},
		{"/repos/acme//../../etc", false},
		{"/repos/acme/repo/../../../../etc", false},
		{segmentBoundary, false},
	}
	for _, tc := range bypass {
		norm, err := normalisePath(tc.raw)
		if err != nil {
			// Rejected (400) is also a non-match: both outcomes keep the
			// request off the allowed prefix.
			if ruleMatch(rules, methodGET, norm) {
				t.Fatalf("rejected path %q must not match", tc.raw)
			}
			continue
		}
		if got := ruleMatch(rules, methodGET, norm); got != tc.want {
			t.Fatalf("ruleMatch on normalised(%q)=%q = %v, want %v", tc.raw, norm, got, tc.want)
		}
	}
	// A canonical allowed path still matches after normalisation.
	if !ruleMatch(rules, methodGET, mustNormalise(t, toolPath)) {
		t.Fatal("/repos/acme/repo must match /repos/acme/")
	}
}

func mustNormalise(t *testing.T, raw string) string {
	t.Helper()
	got, err := normalisePath(raw)
	if err != nil {
		t.Fatalf("normalisePath(%q): %v", raw, err)
	}
	return got
}
