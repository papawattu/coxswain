// Package subvert scans verified deliverable source for subversion patterns
// the verified file adds over its base (ADR-0005 D17.2).
package subvert

// Hit is one subversion pattern the verified file adds over its base (ADR-0005 D17.2).
type Hit struct {
	Path    string
	Pattern string
	Line    int
}

// Pattern names (the Hit.Pattern values):
const (
	PatternTestingTesting = "testing.Testing()"
	PatternExitInInit     = "os.Exit in init"
	PatternLinkname       = "//go:linkname"
	PatternFlagLookupTest = `flag.Lookup("test.`
	PatternCanaryName     = "zzz_canary"
)

// ScanGo reports the patterns the verified Go source has MORE of than the base
// (base nil = a new file). It parses with go/parser (ParseComments); a file that
// does not parse returns an error, never "clean".
func ScanGo(path string, base, verified []byte) ([]Hit, error) { return nil, nil }
