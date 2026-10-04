package toolproxy

import (
	"fmt"
	"net/url"
	"strings"
)

// normalisePath cleans a request path into its canonical form so that rule
// matching operates on the path the request would actually address, not on
// whatever encoding the agent sent. It runs BEFORE the rule check, so no
// encoding can route a request past a rule.
//
// Rejected (error → the handler answers 400, no upstream dial):
//   - any backslash (forward- and backward-): a backslash is a traversal
//     vector on a Windows-mounted upstream or a raw-path smuggle; it has no
//     meaning in an origin-form URL.
//   - any NUL byte.
//   - traversal: a `..` segment in the DECODED path (the path is decoded
//     first, so %2e%2e counts) — `/repos/acme/../../etc`, `/..` and any
//     encoded form of them are rejected, so a `..` can never land on an
//     allowed rule's prefix by accident.
//
// Cleaned:
//   - percent-decoding of the path (so %2f is matched as `/`);
//   - collapse of `//` runs and trailing `/` (except the root);
//   - removal of `.` segments.
//
// The result always starts with "/" (a bare path becomes "/"+path).
func normalisePath(raw string) (string, error) {
	if strings.ContainsRune(raw, '\\') {
		return "", fmt.Errorf("path contains a backslash")
	}
	if strings.ContainsRune(raw, 0) {
		return "", fmt.Errorf("path contains a NUL byte")
	}
	path, err := url.PathUnescape(raw)
	if err != nil {
		return "", fmt.Errorf("bad percent-encoding: %w", err)
	}
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	segments := strings.Split(path, "/")
	out := make([]string, 0, len(segments))
	for _, seg := range segments {
		switch seg {
		case "":
			// The leading empty segment (from the leading "/") and any
			// collapsed "//" runs.
		case ".":
			// Drop.
		case "..":
			// Any decoded `..` segment is rejected: it is a traversal
			// vector and must never reach the rule matcher (a `..` that
			// "stays within the tree" is still a smuggling attempt the
			// canonical form hides).
			return "", fmt.Errorf("path traverses with a .. segment")
		default:
			out = append(out, seg)
		}
	}
	return "/" + strings.Join(out, "/"), nil
}
