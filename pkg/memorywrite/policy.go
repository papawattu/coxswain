// Package memorywrite is P5b's static write policy (PLAN-PHASE5.md slice P5b):
// the ONLY code path that writes a lesson to the shared memory PVC, gated by
// static rules. ADR-0002: the Memory Curator is NOT a second LLM — the
// lessons the runner proposes are filtered by static rules (one spec per
// rule), and the operator's writer Job persists only the survivors. A lesson
// that trips ANY rule is dropped (the Loop succeeds either way — memory
// writes are best-effort, ADR-0004).
//
// The rules are static by construction: no model call, no operator flag
// toggle; the rules live here in Go and are the policy. The set is:
//
//  1. Secret patterns (secretsRegexes): a lesson matching ANY pattern is
//     dropped — lessons are read back into every future prompt on the repo,
//     so a token written into memory would be echoed to the model and,
//     worse, is durable (a lesson outlives the credential that produced it).
//  2. Size cap (MaxLessonChars): a lesson longer than the cap is dropped
//     (memory is a prompt-inclusion channel; a runaway lesson blows the
//     model's context).
//  3. Shape: a lesson must be non-blank and single-line (one file per
//     lesson, flat memory dir; a multi-line lesson would corrupt the
//     one-file-per-lesson layout the reader assumes).
//
// Dropped lessons are never written and never surface as a Loop failure;
// Screen returns which lessons (by index) were dropped so the operator can
// log them (debug visibility, not a Loop condition).
package memorywrite

import (
	"regexp"
	"strings"
)

// MaxLessonChars is P5b's size cap: a lesson is bounded to 500 characters.
// The runner's memory reader caps each file at memoryFileMaxBytes (1000) and
// drops the whole section above memoryPromptMaxBytes (10000); a 500-char
// lesson keeps even a few dozen lessons well under those caps.
const MaxLessonChars = 500

// secretsPatterns is the static secrets pattern set (one spec per pattern —
// the plan's acceptance: "one spec per pattern"). Each pattern is matched
// with regexp.MatchString (the lesson text is untrusted runner output; a
// match anywhere in the lesson drops the lesson). The set is deliberately
// conservative: patterns target well-known credential SHAPES (long random
// base64/hex, JWT, AWS, OpenAI/GitHub/Anthropic key prefixes, a
// kubernetes.io/basic-auth token field), not exact values. A pattern that
// false-positives on ordinary prose is a defect (a lesson silently lost) —
// the specs pin each pattern's behavior against both a matching and a
// non-matching sample.
//
// Each entry is (name, *regexp.Regexp); the name is the spec's label (a
// test iterates the names to assert one spec exists per pattern, I76-style
// evidence: a dropped lesson names the pattern that dropped it).
var secretsPatterns = []struct {
	name    string
	pattern *regexp.Regexp
}{
	// AWS access key ID: AKIA (classic) or ASIA (temporary) followed by
	// 16 alphanumerics (AKIA0123456789ABCDEF).
	{"aws-access-key-id", regexp.MustCompile(`\b(AKIA|ASIA)[A-Z0-9]{16}\b`)},
	// OpenAI API key: sk- followed by at least 20 alphanumerics
	// (sk-abc123def456ghi789jkl012).
	{"openai-api-key", regexp.MustCompile(`\bsk-[A-Za-z0-9]{20,}\b`)},
	// GitHub token: ghp_ / gho_ / ghs_ / ghu_ / ghr_ / github_pat_ followed
	// by at least 20 alphanumerics (ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123).
	{"github-token", regexp.MustCompile(`\b(gh[pousu]_|github_pat_)[A-Za-z0-9_]{20,}\b`)},
	// Anthropic API key: sk-ant- followed by at least 20 alphanumerics.
	{"anthropic-api-key", regexp.MustCompile(`\bsk-ant-[A-Za-z0-9]{20,}\b`)},
	// A JWT: three base64url segments joined by dots (eyJhbGciOiJIUzI1NiJ9.)
	// — a bearer token echoed into a lesson is a credential.
	{"jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`)},
	// A long random base64 blob (at least 64 chars): the generic "looks
	// like a credential" backstop (a raw base64 dump, a session cookie, an
	// OAuth code). Ordinary prose does not contain 64+ consecutive
	// [A-Za-z0-9+/] chars, so the false-positive rate on lesson-shaped text
	// is low. (The threshold is 64 — NOT 40 — because a 40-char run
	// false-positives on base64-ish slugs.)
	{"long-random-base64", regexp.MustCompile(`[A-Za-z0-9+/]{64,}={0,2}`)},
	// A kubernetes.io/basic-auth Secret's token field, as it appears in
	// `kubectl get secret -o yaml` / `kubectl describe secret` output
	// (token: <base64>). The plan's named pattern ("a kubernetes.io/
	// basic-auth token"): a base64 blob of 32+ chars after the "token:" key
	// (a 64-char value also matches the long-random-base64 backstop above;
	// a 32–63-char value matches ONLY this pattern). (git-cred's
	// .dockerconfigjson is a different shape and is not matched here — it is
	// a JSON doc, not a bare token.)
	{"k8s-basic-auth-token", regexp.MustCompile(`\btoken:\s*[A-Za-z0-9+/]{32,}={0,2}\b`)},
	// (NOTE on scan order: matchSecrets reports the FIRST pattern in slice
	// order that matches. A token: field with a 64-char base64 value
	// matches BOTH patterns; the slice order (backstop first) makes the
	// reported reason "long-random-base64" for that case. The spec for
	// k8s-basic-auth-token uses a 32-char value — matched ONLY by this
	// pattern — so the reason names it. Both are secret drops; the name is
	// for operator debug logs.)
}

// secretPatternNames returns the pattern names in order (the spec's "one
// spec per pattern": the test iterates these and asserts each is matched by
// a matching sample and not by a non-matching sample).
func secretPatternNames() []string {
	names := make([]string, 0, len(secretsPatterns))
	for _, sp := range secretsPatterns {
		names = append(names, sp.name)
	}
	return names
}

// secretPattern looks up a pattern by name (the operator's drop log names
// the pattern; nil when the name is unknown).
func secretPattern(name string) (*regexp.Regexp, bool) {
	for _, sp := range secretsPatterns {
		if sp.name == name {
			return sp.pattern, true
		}
	}
	return nil, false
}

// Screen applies the static write policy to the runner's proposed lessons and
// returns the surviving lessons (the ones the writer Job will persist) plus
// the set of dropped pattern names (one per dropped lesson, in lesson
// order; empty for a lesson that survived). The order of the survivors
// matches the order of the input (the writer names files lesson-<n>.txt by
// position in the SURVIVOR list — a dropped lesson leaves a gap, which is
// fine: the memory is flat and the reader lists files in sorted order).
//
// A lesson is dropped when it trips ANY rule; the FIRST rule that trips is
// the named reason (a secrets pattern name, "size-cap", or "shape"). A
// lesson that is blank after trimming, multi-line, or over MaxLessonChars is
// dropped by shape/cap BEFORE the secrets scan (a 5000-char blob is dropped
// as "size-cap" even if it also matches a secrets pattern — the reason is
// the cheaper, more actionable one).
func Screen(lessons []string) (survivors []string, reasons []string) {
	survivors = make([]string, 0, len(lessons))
	reasons = make([]string, 0, len(lessons))
	for _, raw := range lessons {
		l := strings.TrimSpace(raw)
		if l == "" {
			reasons = append(reasons, "shape: blank lesson")
			continue
		}
		if strings.ContainsAny(l, "\r\n") {
			reasons = append(reasons, "shape: multi-line lesson")
			continue
		}
		if len([]rune(l)) > MaxLessonChars {
			reasons = append(reasons, "size-cap: lesson exceeds 500 characters")
			continue
		}
		if name, ok := matchSecrets(l); ok {
			reasons = append(reasons, "secret: "+name)
			continue
		}
		survivors = append(survivors, l)
		reasons = append(reasons, "")
	}
	return survivors, reasons
}

// matchSecrets returns the name of the first secrets pattern that matches
// the lesson, and true; or ("", false) when no pattern matches. The scan
// order is the secretsPatterns slice order (deterministic: the first named
// pattern that matches is the reported reason).
func matchSecrets(lesson string) (string, bool) {
	for _, sp := range secretsPatterns {
		if sp.pattern.MatchString(lesson) {
			return sp.name, true
		}
	}
	return "", false
}

// IsSafe is the single-lesson predicate (the writer Job's shell script cannot
// run Go; the OPERATOR screens the lessons in Screen and passes only the
// survivors to the Job as env/args — the Job itself writes what it is given,
// and the static policy is the operator's, enforced in Screen). A lesson is
// safe when Screen would keep it.
func IsSafe(lesson string) bool {
	survivors, _ := Screen([]string{lesson})
	return len(survivors) == 1
}
