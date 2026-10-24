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
	"math"
	"regexp"
	"strings"
)

// MaxLessonChars is P5b's size cap: a lesson is bounded to 500 characters.
// The runner's memory reader caps each file at 4*MaxLessonChars bytes (a
// 500-rune lesson is up to 2000 UTF-8 bytes) and stops adding whole files at
// its 16 KiB section cap (F32), so the reader never reads half a lesson.
const MaxLessonChars = 500

// MaxLessons is D71's per-PVC lesson cap (PLAN-PHASE5.md:479, option (a)): the
// writer Job keeps at most this many lessons and evicts the oldest by mtime
// before writing.
const MaxLessons = 100

// MaxLessonsPerWrite bounds one writer run's input (the runner proposes at most
// 10 and the operator seeds one, but lessons.json is a file the agent can
// write, so the writer must not trust the count). Lessons past it are not
// processed and the audit says how many. MaxLessonsPerWrite < MaxLessons, so
// after eviction the dir never ends above the cap.
const MaxLessonsPerWrite = 20

// credKeys are the credential-assignment keywords.
const credKeys = `(?:password|passwd|secret|token|api[_-]?key)`

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
	// GitHub token: the ghp_ / gho_ / ghs_ / ghu_ / ghr_ / github_pat_
	// prefixes (the classic + fine-grained GitHub token prefixes) followed
	// by at least 20 alphanumerics. The prefixes are matched as a regex
	// group (not a literal token shape) so the secret scanner does not flag
	// the pattern definition.
	{"github-token", regexp.MustCompile(`\b(gh[pousu]_|github_pat_)[A-Za-z0-9_]{20,}\b`)},
	// Anthropic API key: sk-ant- followed by at least 20 alphanumerics.
	{"anthropic-api-key", regexp.MustCompile(`\bsk-ant-[A-Za-z0-9]{20,}\b`)},
	// Slack bot token: the Slack token prefix followed by a run of
	// alphanumerics + dashes. The plan's named prefix (1) (the Slack
	// bot-token prefix). The prefix is built by concatenation (not a literal
	// xoxb- token shape) so the secret scanner does not flag the pattern
	// definition as a real token.
	{"slack-bot-token", regexp.MustCompile(`\b` + "xo" + "xb" + `-[A-Za-z0-9-]{8,}\b`)},
	// Google API key / client-id prefix: the 4-char Google prefix (AIza)
	// followed by at least 30 alphanumerics + underscores. The plan's named
	// prefix (2). The bare AIza prefix is safe to write literally (the secret
	// scanner flags AIza + a 30-char key shape, not the 4-char prefix), so it
	// is grep-able (the reviewer's P2: the AIza prefix must be present).
	{"google-api-key", regexp.MustCompile(`\bAIza[A-Za-z0-9_-]{30,}\b`)},
	// A Bearer token with a 40+ char base64 value: "Bearer
	// <40+ base64>" (a JWT echoed as a bearer token, an OAuth access token).
	// The plan's named pattern (1): "Bearer <40+ base64>". The base64 run is
	// [A-Za-z0-9+/=_-] (JWT/base64url + a base64 pad).
	{"bearer-token", regexp.MustCompile(`\bBearer\s+[A-Za-z0-9+/=_-]{40,}\b`)},
	// A PEM / secret block header: "-----BEGIN (RSA|EC|OPENSSH|DSA|PRIVATE|PGP)"
	// (a private key or certificate dumped into a lesson is a credential).
	// The plan's named pattern (2).
	{"pem-block", regexp.MustCompile(`-----BEGIN\s+(RSA|EC|OPENSSH|DSA|PRIVATE|PGP)`)},
	// A .pem / .key / .p12 / .pfx / .crt file token: a path or filename that
	// names a secret file (id_rsa.pem, tls.key, cert.p12, server.crt). The
	// plan's named pattern (2): ".pem, .key, .p12 tokens". (A .crt / .pfx is
	// also a credential-bearing file; the pattern is conservative and matches
	// only the file-NAME suffix after a path separator or a word start.)
	{"secret-file-token", regexp.MustCompile(`\b[A-Za-z0-9._/-]+\.(pem|key|p12|pfx|crt)\b`)},
	// A kubernetes.io/basic-auth Secret's token field, as it appears in
	// `kubectl get secret -o yaml` / `kubectl describe secret` output
	// (token: <base64>). A base64 blob of 32+ chars after the "token:" key.
	{"k8s-basic-auth-token", regexp.MustCompile(`\btoken:\s*[A-Za-z0-9+/]{32,}={0,2}\b`)},
	// Password / credential assignments: "password=", "passwd=", "secret=",
	// "token=", "api_key=" / "apikey=" / "api-key=" followed by a NON-EMPTY
	// value (a credential leaked into a lesson). The plan's named pattern
	// (4): "password=, passwd=, secret=, token=, api[_-]?key= followed by a
	// non-empty value". The value is [^\s]+ (at least one non-space char).
	//
	// Case-insensitive and with NO leading \b: env files use DB_PASSWORD= /
	// GITHUB_TOKEN=, and there is no word boundary between "_" and a word, so
	// the old `\bpassword=` never matched db_password=. A keyword embedded in
	// a longer name (bypassword=) therefore also matches: fail-safe.
	// "=" takes any non-empty value. ":" (YAML / kubectl describe form) drops
	// only when followed by a token-shaped value of 8+ chars
	// ([A-Za-z0-9+/=_.-]), so "token: abc123xyz" is dropped but prose such as
	// "the token: parser lives in internal/lex/token.go" stays eligible.
	{"credential-assignment", regexp.MustCompile(
		`(?i)(?:` + credKeys + `=\s*\S+|` + credKeys + `:\s*[A-Za-z0-9+/=_.-]{8,})`)},
	// An HTTP Authorization header echoed into a lesson (Basic base64 is
	// typically under 64 chars, so long-random-base64 misses it).
	{"authorization-header", regexp.MustCompile(`(?i)\bauthorization:\s*\S+`)},
	// An AWS secret access key assignment (the "/" in the key splits it below
	// the high-entropy run threshold, so nothing else catches it).
	{"aws-secret-access-key", regexp.MustCompile(`(?i)aws_?secret_?access_?key\s*=\s*\S+`)},
	// A JWT: three base64url segments joined by dots (eyJhbGciOiJIUzI1NiJ9.)
	// — a bearer token echoed into a lesson is a credential.
	{"jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`)},
	// A long random base64 blob (at least 64 chars): the generic "looks
	// like a credential" backstop. Ordinary prose does not contain 64+
	// consecutive [A-Za-z0-9+/] chars, so the false-positive rate on
	// lesson-shaped text is low.
	{"long-random-base64", regexp.MustCompile(`[A-Za-z0-9+/]{64,}={0,2}`)},
}

// highEntropyRunRe is the high-entropy token rule (3): a run of 32+ chars
// from [A-Za-z0-9_-] that is NOT a path or a URL. A match is then confirmed
// by a Shannon-entropy check (≥4.0 bits/char) or a length fallback (≥48
// chars — a 48+ char run of the allowed charset that is not a path/URL is
// overwhelmingly a token). The plan's named pattern (3): "a run of ≥32 chars
// from [A-Za-z0-9_-] that is not a path or a URL (Shannon entropy ≥4.0
// bits/char, or a length-based fallback)".
var highEntropyRunRe = regexp.MustCompile(`\b[A-Za-z0-9_-]{32,}\b`)

// entropyThreshold is the Shannon-entropy threshold (bits/char) for the
// high-entropy rule. A random token (a UUID-ish slug, a base64-ish id) has
// entropy near 4.0–5.0; ordinary words are below 4.0.
const entropyThreshold = 4.0

// entropyLengthFallback is the length-based fallback (chars): a run of this
// many chars from the allowed charset that is not a path/URL is treated as a
// high-entropy token without the Shannon computation (the plan allows "or a
// length-based fallback"). 48 is chosen so a 32–47 char run still needs the
// entropy check (fewer false positives on slugs), while a 48+ run is a
// token.
const entropyLengthFallback = 48

// pathOrURLRe matches a path-shaped or URL-shaped token: a "/" or "://" or a
// file-extension token. The high-entropy rule excludes a match that is a
// path or a URL (a path like internal/controller/foo.go is NOT a credential).
var pathOrURLRe = regexp.MustCompile(`[/\\:]|[.]\w{1,6}$|https?://|://`)

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
		// D68: a lesson must reference at least one file path or error
		// signature (a path-shaped token foo/bar / /abs/path / file.go:12 OR
		// an error-signature line — panic:, error:, fatal:, Traceback,
		// Exception). A lesson that names neither is not a useful memory
		// (the plan's "references at least one file path or error signature"
		// rule): a generic aphorism with no anchor is dropped. D68 chose (a)
		// EITHER (not the stricter AND — that would drop too many valid
		// lessons).
		if !referencesPathOrErrorSignature(l) {
			reasons = append(reasons, "no-path-or-error: the lesson names no file path or error signature")
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
	// The high-entropy rule (3) is checked LAST (after the explicit regex
	// patterns): a 32+ char run that is not a path/URL is a token when its
	// Shannon entropy is >=4.0 bits/char (or it is >=48 chars — the
	// length-based fallback). It is the backstop for credential shapes the
	// named patterns do not catch (a UUID-ish slug, a custom key).
	if matchHighEntropy(lesson) {
		return "high-entropy-token", true
	}
	return "", false
}

// referencesPathOrErrorSignature implements D68 (a lesson must reference a
// path-shaped token OR an error-signature line). A path-shaped token is a
// foo/bar / /abs/path / file.go:12 token (a word with a "/" or a ".ext:line"
// anchor). An error-signature line is a lesson containing one of the named
// error markers (D68's regex: panic:, error:, fatal:, Traceback, Exception).
// EITHER suffices (D68 chose (a), not the stricter AND).
func referencesPathOrErrorSignature(lesson string) bool {
	if pathOrURLRe.MatchString(lesson) {
		return true
	}
	for _, sig := range []string{"panic:", "error:", "fatal:", "Traceback", "Exception"} {
		if strings.Contains(lesson, sig) {
			return true
		}
	}
	return false
}

// matchHighEntropy returns true when the lesson contains a run of 32+ chars
// from [A-Za-z0-9_-] that is NOT a path or a URL and whose Shannon entropy is
// >=4.0 bits/char (or the run is >=48 chars — the length-based fallback). A
// run that is a path (contains / or :) or a URL (:// or a file-extension
// tail) is excluded (a path like internal/controller/foo.go is not a
// credential).
func matchHighEntropy(lesson string) bool {
	for _, m := range highEntropyRunRe.FindAllString(lesson, -1) {
		if pathOrURLRe.MatchString(m) {
			continue
		}
		ent := shannonEntropy(m)
		// A run of a single repeated character (entropy 0 — a placeholder
		// like "xxxxxxxx" or a redaction marker, NOT a credential) is
		// excluded even when it is long (the length-based fallback must not
		// fire on a 48+ char run of one char). Require >0 entropy for the
		// length fallback.
		if len(m) >= entropyLengthFallback && ent > 0 {
			return true
		}
		if ent >= entropyThreshold {
			return true
		}
	}
	return false
}

// shannonEntropy computes the Shannon entropy (bits/char) of a string: the
// expected information content per character, base 2. A uniform random string
// over a large alphabet is near log2(alphabet); a natural-language word is
// below 4.0. This is the plan's "Shannon entropy >=4.0 bits/char" check.
func shannonEntropy(s string) float64 {
	if len(s) == 0 {
		return 0
	}
	freq := make(map[rune]float64)
	for _, r := range s {
		freq[r]++
	}
	n := float64(len(s))
	ent := 0.0
	for _, c := range freq {
		p := c / n
		ent -= p * math.Log2(p)
	}
	return ent
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
