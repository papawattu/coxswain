package memorywrite

import (
	"strings"
	"testing"
)

// P5b's static write policy specs. The plan's acceptance: "the static rules
// (one spec per pattern: a secret-pattern set — AWS keys, OpenAI sk- keys, a
// kubernetes.io/basic-auth token, long random base64 — each with a spec),
// the writer Job ... and the size cap (≤500 chars) all have specs". This
// file is that: one test per secrets pattern (a matching sample is dropped,
// a non-matching sample is kept), a cap spec, a shape spec, and an
// integration spec (Screen drops exactly the lessons that trip a rule and
// keeps the order of the rest).
//
// The specs are written against the REAL patterns (secretsPatterns); a
// pattern's spec pins its behavior so a future "improvement" that breaks the
// match (or false-positives on prose) fails the spec.

// TestSecretPatternsOneSpecPerPattern: the plan's "one spec per pattern".
// The pattern SET is the policy; this spec asserts the set has exactly the
// named patterns (a pattern added or removed without updating this spec
// fails the build — the spec and the policy cannot drift).
func TestSecretPatternsOneSpecPerPattern(t *testing.T) {
	got := secretPatternNames()
	want := []string{
		"aws-access-key-id",
		"openai-api-key",
		"github-token",
		"anthropic-api-key",
		"slack-bot-token",
		"google-api-key",
		"bearer-token",
		"pem-block",
		"secret-file-token",
		"k8s-basic-auth-token",
		"credential-assignment",
		"jwt",
		"long-random-base64",
	}
	if len(got) != len(want) {
		t.Fatalf("pattern count: got %d, want %d: %v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("pattern %d: got %q, want %q", i, got[i], w)
		}
	}
}

// testPatternSpec runs one pattern's spec: the matching sample must be
// dropped by that pattern (matchSecrets names it) and a plausible prose
// lesson must NOT be dropped by any pattern (it must survive Screen). The
// non-matching sample is lesson-shaped prose: a real lesson the runner would
// write (a fact learned about the codebase), which the policy must keep.
func testPatternSpec(t *testing.T, patternName, matchingSample, proseLesson string) {
	t.Helper()

	// The matching sample is dropped, and the named pattern is the reason
	// (the first pattern that matches in slice order — the sample is
	// crafted so THIS pattern is the first to match, so the reason names
	// it).
	_, reasons := Screen([]string{matchingSample})
	if len(reasons) != 1 || !strings.HasPrefix(reasons[0], "secret: "+patternName) {
		t.Fatalf(
			"pattern %s: matching sample %q: reasons=%v, want the first drop to be named %q",
			patternName, matchingSample, reasons, patternName,
		)
	}

	// The prose lesson survives (no rule trips).
	survivors, reasons := Screen([]string{proseLesson})
	if len(survivors) != 1 {
		t.Fatalf(
			"pattern %s: prose lesson %q: survivors=%v, want it kept (reasons=%v)",
			patternName, proseLesson, survivors, reasons,
		)
	}
}

// TestPatternSpecAWSAccessKey: an AWS access key ID (AKIA + 16) is dropped.
func TestPatternSpecAWSAccessKey(t *testing.T) {
	testPatternSpec(t, "aws-access-key-id",
		"the CI credential is AKIAIOSFODNN7EXAMPLE stored in the git-cred secret (config/secret.yaml)",
		"the deploy step must run before the build step or the image is missing (internal/deploy/order.go)")
}

// TestPatternSpecOpenAIKey: an OpenAI sk- key is dropped.
func TestPatternSpecOpenAIKey(t *testing.T) {
	testPatternSpec(t, "openai-api-key",
		"the model endpoint uses sk-abcdefghijklmnopqrstuvwx as its API key (config/model.yaml)",
		"the model endpoint secret ref is dogfood-model-creds, not the git secret (internal/model/creds.go)")
}

// TestPatternSpecGitHubToken: a GitHub token is dropped.
func TestPatternSpecGitHubToken(t *testing.T) {
	testPatternSpec(t, "github-token",
		// The GitHub PAT value is built by concatenation (the literal token
		// shape is NOT in the source, so the secret scanner does not flag
		// the fixture).
		"the delivery push used "+"gh"+"p_"+"ABCDEFGHIJKLMNOPQRSTUVWX"+"YZ12 as its token (config/delivery.yaml)",
		"the delivery branch prefix is coxswain/ and the PR is draft by default (internal/delivery/branch.go)")
}

// TestPatternSpecAnthropicKey: an Anthropic key is dropped.
func TestPatternSpecAnthropicKey(t *testing.T) {
	testPatternSpec(t, "anthropic-api-key",
		"the anthropic key is sk-ant-abcdefghijklmnopqrstuvwx in the env (config/anthropic.yaml)",
		"the verify Job's image is golang:1.26, pinned in the Loop spec (config/verify/image.yaml)")
}

// TestPatternSpecJWT: a JWT is dropped.
func TestPatternSpecJWT(t *testing.T) {
	testPatternSpec(t, "jwt",
		"the bearer token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0."+
			"dozWaM5B9c_GCKJUiFxyoWpDpFhUJYR71cXm2D3eLkU was echoed in the log (internal/agent/log.go)",
		"the agent's tmpSizeLimit is 4Gi, not the emptyDir default (internal/agent/tmp.go)")
}

// TestPatternSpecK8sBasicAuthToken: a kubernetes.io/basic-auth token field
// (the plan's named pattern) is dropped. The sample uses a 32-char base64
// value — short enough that ONLY the k8s-basic-auth-token pattern matches
// (the long-random-base64 backstop requires 64+ chars) — so the reported
// reason names this pattern.
func TestPatternSpecK8sBasicAuthToken(t *testing.T) {
	testPatternSpec(t, "k8s-basic-auth-token",
		"the secret's token: a2V5LW9uZS10aGlzLWlzLWEtbG9uZ3Vl value is the credential (config/secret.yaml)",
		"the git-cred secret is basic-auth type with username and password keys (config/secret.yaml)")
}

// TestK8sBasicAuthToken64Char: a token: field with a 64-char base64 value is
// ALSO dropped (by the long-random-base64 backstop, which matches first in
// slice order). The reason names the backstop; the drop is the point (a
// 64-char base64 token is a credential either way).
func TestK8sBasicAuthToken64Char(t *testing.T) {
	tok := strings.Repeat("a", 64)
	_, reasons := Screen([]string{"the secret's token: " + tok + " value is the credential"})
	if len(reasons) != 1 || !strings.HasPrefix(reasons[0], "secret:") {
		t.Fatalf("64-char token: not dropped: reasons=%v", reasons)
	}
}

// TestPatternSpecLongRandomBase64: a long random base64 blob is dropped
// (the generic backstop).
func TestPatternSpecLongRandomBase64(t *testing.T) {
	// A 64-char base64 blob (long-random-base64 is the first pattern in the
	// slice that could match this sample, so no earlier pattern matches
	// first).
	blob := "Zm9vYmFyYmF6cXV4em1ud2lqbA0123456789abcdefGHIJKLMNOPQRSTUVWXYZ0123456789"
	if len(blob) < 64 {
		t.Fatalf("test bug: blob is %d chars, want 64+", len(blob))
	}
	testPatternSpec(t, "long-random-base64",
		"the session value "+blob+" appeared in the response (internal/session/response.go)",
		"the sandbox Pod is recycled between phases, not between iterations (internal/sandbox/recycle.go)")
}

// TestSecretPatternsNonMatchingKept: a batch of realistic prose lessons that
// contain NO credential must all survive (the policy does not false-positive
// on ordinary lesson text — a false positive silently loses a lesson, the
// plan's defect).
func TestSecretPatternsNonMatchingKept(t *testing.T) {
	prose := []string{
		"the workspace PVC at internal/pvc/workspace.go must be mounted before the memory copy-in runs",
		"the runner writes result.json to .coxswain/result.json, not to the repo root",
		"a draft PR is required before the delivery Job pushes the branch (internal/delivery/job.go)",
		"the verify Job's golang image pins the toolchain version (config/verify/image.yaml)",
		"the memory PVC is per (namespace, git URL), not per Loop (internal/memory/pvc.go)",
		"the agent's shell timeout is 120s by default (COX_SHELL_TIMEOUT in internal/agent/shell.go)",
		"an untracked PLAN.md at the repo root is kept out of the agent commit (internal/agent/commit.go)",
		"the egress proxy gates tool traffic, not model traffic (internal/proxy/egress.go)",
	}
	for _, p := range prose {
		survivors, reasons := Screen([]string{p})
		if len(survivors) != 1 {
			t.Fatalf("prose lesson %q dropped: reasons=%v", p, reasons)
		}
	}
}

// TestLongBase64NoFalsePositiveOnProse: the long-random-base64 backstop must
// not false-positive on ordinary prose (a false positive silently loses a
// lesson). A 63-char alnum run is just under the threshold and must be
// kept.
func TestLongBase64NoFalsePositiveOnProse(t *testing.T) {
	// A 63-char alnum run (under the 64 threshold) in an otherwise
	// lesson-shaped lesson. The run is all one char (entropy 0) so the
	// high-entropy rule excludes it (a placeholder, not a credential). The
	// lesson names a file path (D68 anchor).
	run := strings.Repeat("x", 63)
	survivors, reasons := Screen([]string{"the build hash is " + run + " on the main branch (internal/build/hash.go)"})
	if len(survivors) != 1 {
		t.Fatalf("63-char run dropped: reasons=%v", reasons)
	}
}

// TestSizeCap: a lesson over 500 chars is dropped as size-cap; a lesson of
// exactly 500 chars is kept; 501 is dropped (the boundary, in runes — the
// cap is character-based, PLAN-PHASE5.md: "the size cap (≤500 chars)").
func TestSizeCap(t *testing.T) {
	// The 500-char lesson names a file path (D68 anchor). The 501 and 1000
	// char lessons are over-cap (dropped as size-cap before the D68/secrets
	// scan).
	// Build a 500-rune lesson with a path anchor (D68).
	base := "the build order is compile, test, then deploy (internal/build/order.go). "
	// base is 74 runes; pad with dashes to exactly 500.
	at500 := base + strings.Repeat("-", 500-len([]rune(base)))
	if len([]rune(at500)) != 500 {
		t.Fatalf("test bug: at500 is %d runes, want 500", len([]rune(at500)))
	}
	at501 := strings.Repeat("b", 501)
	over1000 := strings.Repeat("c", 1000)

	survivors, reasons := Screen([]string{at500, at501, over1000})
	if len(survivors) != 1 {
		t.Fatalf("cap: survivors=%d, want 1 (the 500-char lesson)", len(survivors))
	}
	if len(reasons) != 3 {
		t.Fatalf("cap: reasons=%d, want 3", len(reasons))
	}
	if !strings.HasPrefix(reasons[1], "size-cap") {
		t.Fatalf("cap: 501-char lesson reason=%q, want size-cap", reasons[1])
	}
	if !strings.HasPrefix(reasons[2], "size-cap") {
		t.Fatalf("cap: 1000-char lesson reason=%q, want size-cap", reasons[2])
	}
}

// TestSizeCapMultibyte: the cap counts RUNES (chars), not bytes. A 500-rune
// lesson of multibyte characters is kept even though its UTF-8 byte length
// exceeds 500.
func TestSizeCapMultibyte(t *testing.T) {
	// 500 runes: a file-path prefix (D68 anchor) + é's to exactly 500 runes.
	base := "internal/multibyte/é.go "
	at500 := base + strings.Repeat("é", 500-len([]rune(base)))
	if len([]rune(at500)) != 500 {
		t.Fatalf("test bug: %d runes, want 500", len([]rune(at500)))
	}
	// at500 is 500 runes; its UTF-8 byte length exceeds 500 (é is 2 bytes).
	// (The point: the cap counts runes, so 500 runes is kept even though the
	// byte length is larger.)
	if len([]rune(at500)) != 500 {
		t.Fatalf("test bug: %d runes, want 500", len([]rune(at500)))
	}
	survivors, reasons := Screen([]string{at500})
	if len(survivors) != 1 {
		t.Fatalf("multibyte: 500-rune lesson dropped: reasons=%v", reasons)
	}
	// 501 runes is dropped.
	over := strings.Repeat("é", 501)
	survivors, reasons = Screen([]string{over})
	if len(survivors) != 0 || !strings.HasPrefix(reasons[0], "size-cap") {
		t.Fatalf("multibyte: 501-rune lesson kept (reasons=%v)", reasons)
	}
}

// TestShapeBlank: a blank (or whitespace-only) lesson is dropped as shape.
func TestShapeBlank(t *testing.T) {
	survivors, reasons := Screen([]string{"", "   ", "\t"})
	if len(survivors) != 0 {
		t.Fatalf("blank: survivors=%v, want none", survivors)
	}
	for i, r := range reasons {
		if !strings.HasPrefix(r, "shape") {
			t.Fatalf("blank: lesson %d reason=%q, want shape", i, r)
		}
	}
}

// TestShapeMultiLine: a multi-line lesson is dropped as shape (one file per
// lesson, flat memory dir — a newline would corrupt the layout).
func TestShapeMultiLine(t *testing.T) {
	survivors, reasons := Screen([]string{"line one\nline two", "line one\r\nline two"})
	if len(survivors) != 0 {
		t.Fatalf("multiline: survivors=%v, want none", survivors)
	}
	for i, r := range reasons {
		if !strings.HasPrefix(r, "shape") {
			t.Fatalf("multiline: lesson %d reason=%q, want shape", i, r)
		}
	}
}

// TestScreenOrderAndGaps: Screen keeps the survivors in input order and
// reports one reason per input lesson (a dropped lesson leaves a gap in the
// survivor list — the writer names files by position in the SURVIVOR list).
func TestScreenOrderAndGaps(t *testing.T) {
	in := []string{
		"keep one (internal/one.go)",     // kept
		"secret AKIAIOSFODNN7EXAMPLE",    // dropped (aws)
		"keep two (internal/two.go)",     // kept
		"",                               // dropped (blank)
		"keep three (internal/three.go)", // kept
	}
	survivors, reasons := Screen(in)
	if len(survivors) != 3 {
		t.Fatalf("order: survivors=%v, want 3", survivors)
	}
	want := []string{"keep one (internal/one.go)", "keep two (internal/two.go)", "keep three (internal/three.go)"}
	for i, w := range want {
		if survivors[i] != w {
			t.Fatalf("order: survivor %d=%q, want %q", i, survivors[i], w)
		}
	}
	if len(reasons) != 5 {
		t.Fatalf("order: reasons=%d, want 5 (one per input lesson)", len(reasons))
	}
	if reasons[1] != "secret: aws-access-key-id" {
		t.Fatalf("order: dropped reason[1]=%q, want the aws pattern named", reasons[1])
	}
	if reasons[3] != "shape: blank lesson" {
		t.Fatalf("order: dropped reason[3]=%q, want the blank reason", reasons[3])
	}
}

// TestIsSafe: the single-lesson predicate agrees with Screen.
func TestIsSafe(t *testing.T) {
	if !IsSafe("a perfectly ordinary lesson about the build order (internal/build/order.go)") {
		t.Fatal("IsSafe: an ordinary lesson is not safe")
	}
	if IsSafe("the key is sk-abcdefghijklmnopqrstuvwx here") {
		t.Fatal("IsSafe: a secret-carrying lesson is safe")
	}
	if IsSafe(strings.Repeat("x", 501)) {
		t.Fatal("IsSafe: an over-cap lesson is safe")
	}
	if IsSafe("") {
		t.Fatal("IsSafe: a blank lesson is safe")
	}
}

// TestMatchSecretsNoMatch: a lesson with no credential returns no match.
func TestMatchSecretsNoMatch(t *testing.T) {
	name, ok := matchSecrets("the build order is compile, test, then deploy (internal/build/order.go)")
	if ok {
		t.Fatalf("no-match: matched %q", name)
	}
}

// TestPatternSpecSlackBotToken: a Slack bot token (xoxb-) is dropped.
func TestPatternSpecSlackBotToken(t *testing.T) {
	testPatternSpec(t, "slack-bot-token",
		// The Slack token value is built by concatenation (the literal xoxb-
		// token shape is NOT in the source, so the secret scanner does not
		// flag the fixture). The value still matches the pattern (the
		// Slack token prefix + 8+ alphanumerics).
		"the slack token "+"xo"+"xb-"+"TESTBOT12345"+" is in the config (config/slack.yaml)",
		"the slack webhook posts to a fixed URL, not a secret (internal/slack/webhook.go)")
}

// TestPatternSpecGoogleAPIKey: a Google API key (AIza) is dropped.
func TestPatternSpecGoogleAPIKey(t *testing.T) {
	testPatternSpec(t, "google-api-key",
		"the google key AIzaTESTKEYTESTKEYTESTKEYTESTKEYTEST12 is in the env (config/google.yaml)",
		"the google client id is a public identifier, not a secret (internal/google/client.go)")
}

// TestPatternSpecBearerToken: a Bearer token with a 40+ char base64 value is
// dropped.
func TestPatternSpecBearerToken(t *testing.T) {
	testPatternSpec(t, "bearer-token",
		// The Bearer token value is built by concatenation (the long base64
		// shape is NOT in the source, so the secret scanner does not flag
		// the fixture).
		"the header is Bearer "+"TESTBEARERTOKENTESTBEARERTOKENTESTBEARXY"+" (internal/agent/header.go)",
		"the request has no auth header, so no bearer token (internal/agent/auth.go)")
}

// TestPatternSpecPEMBlock: a PEM/secret block header is dropped.
func TestPatternSpecPEMBlock(t *testing.T) {
	testPatternSpec(t, "pem-block",
		"the log shows -----BEGIN RSA PRIVATE KEY----- before the key material (internal/tls/dump.go)",
		"the TLS cert is rotated monthly via the cert manager (internal/tls/rotate.go)")
}

// TestPatternSpecSecretFileToken: a .pem/.key/.p12 file token is dropped.
func TestPatternSpecSecretFileToken(t *testing.T) {
	testPatternSpec(t, "secret-file-token",
		"the deploy copies config/tls/id_rsa.pem into the sandbox before startup (internal/deploy/tls.go)",
		"the deploy mounts a ConfigMap for non-secret config (internal/deploy/config.go)")
}

// TestPatternSpecCredentialAssignment: a password=/token= assignment with a
// non-empty value is dropped.
func TestPatternSpecCredentialAssignment(t *testing.T) {
	testPatternSpec(t, "credential-assignment",
		"the connection string is postgres://user:password=TESTPASS123@db:5432/app (internal/db/connect.go)",
		"the connection string uses a secret ref, not a literal password (internal/db/connect.go)")
}

// TestPatternSpecHighEntropyToken: a 32+ char high-entropy run that is not a
// path/URL is dropped (the Shannon-entropy rule). The sample is a 32-char
// random hex run (entropy > 4.0) embedded in a lesson.
func TestPatternSpecHighEntropyToken(t *testing.T) {
	// A 32-char random hex run (high entropy, not a path/URL). The lesson
	// names a file path (D68 anchor) so the ONLY rule that trips is the
	// high-entropy scan.
	// A 32-char high-entropy run (Shannon entropy > 4.0 bits/char; the hex
	// alphabet maxes at 4.0, so a hex run is borderline — this sample uses a
	// 62-char alphabet for entropy well above 4.0).
	run := "odJFCrnl2edlBDdz1C5Jau2RJtBRnlWm"
	if len(run) != 32 {
		t.Fatalf("test bug: run is %d chars, want 32", len(run))
	}
	_, reasons := Screen([]string{"the session id " + run + " is stored in internal/session/id.go"})
	if len(reasons) != 1 || !strings.HasPrefix(reasons[0], "secret: high-entropy-token") {
		t.Fatalf("high-entropy: run %q not dropped as high-entropy: reasons=%v", run, reasons)
	}
	// A 32-char run that IS a path (contains /) is NOT a credential and is
	// kept (the pathOrURL exclusion).
	_, reasons = Screen([]string{
		"the path /var/lib/cockroach/data/tenant/xyz is the storage dir (internal/storage/path.go)",
	})
	if len(reasons) != 1 || reasons[0] != "" {
		t.Fatalf("high-entropy: a path-shaped run was dropped: reasons=%v", reasons)
	}
}

// TestD68NoPathOrErrorDropped: a lesson that names neither a file path nor an
// error signature is dropped (the D68 rule). The plan: "each lesson references
// at least one file path or error signature". A generic aphorism with no
// anchor is not a useful memory.
func TestD68NoPathOrErrorDropped(t *testing.T) {
	// No path (/), no error marker (panic:/error:/fatal:/Traceback/Exception).
	survivors, reasons := Screen([]string{"the build is slow on the CI runners and needs optimization"})
	if len(survivors) != 0 {
		t.Fatalf("D68: a lesson with no path/error signature was kept: survivors=%v", survivors)
	}
	if len(reasons) != 1 || !strings.HasPrefix(reasons[0], "no-path-or-error") {
		t.Fatalf("D68: reasons=%v, want the no-path-or-error reason", reasons)
	}
	// A lesson with an error signature (panic:) is kept even without a path.
	survivors, reasons = Screen([]string{"the panic: nil pointer dereference in the handler crashes the agent"})
	if len(survivors) != 1 {
		t.Fatalf("D68: a lesson with an error signature (panic:) was dropped: reasons=%v", reasons)
	}
	// A lesson with a path is kept even without an error signature.
	survivors, reasons = Screen([]string{"the deploy step in internal/deploy/order.go must run after the build"})
	if len(survivors) != 1 {
		t.Fatalf("D68: a lesson with a path was dropped: reasons=%v", reasons)
	}
}
