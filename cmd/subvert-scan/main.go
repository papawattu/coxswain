// Command subvert-scan is the S1 diff scan's entrypoint (ADR-0005 D17.2). It
// runs as the verify Job's `scan` init container on the operator's own image.
// scan-prep has copied the changed, non-protected .go files into
// <root>/verified/<path> (and the base version, when one exists, into
// <root>/base/<path>); this binary runs subvert.ScanGo over each and writes ONE
// line of JSON to the termination message path:
//
//	{"v":1,"files":N,"hits":[{"path":..,"pattern":..,"line":..}],"errors":[..path..]}
//
// plus "hitsDropped" / "errorsDropped" when a cap cut a list. It ALWAYS exits 0:
// D17.2 says the scan never blocks the checks. A file that cannot be scanned
// goes into "errors", never into a silent clean.
//
// Usage: coxswain-subvert-scan <root>
// Env:   TERMINATION_MESSAGE_PATH  where the line is written (default
//
//	/tmp/termination.log); the line is also printed to stdout.
package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/papawattu/coxswain/internal/subvert"
)

const (
	defaultMessagePath = "/tmp/termination.log"
	maxHits            = 20
	maxErrors          = 20
	// maxMessageBytes keeps the line well under the kubelet's 4096-byte
	// termination message limit.
	maxMessageBytes = 3500
	// prepFailedMarker is scan-prep's "a git step failed" file in the root.
	prepFailedMarker = "PREP_FAILED"
)

type hitJSON struct {
	Path    string `json:"path"`
	Pattern string `json:"pattern"`
	Line    int    `json:"line"`
}

type result struct {
	V             int       `json:"v"`
	Files         int       `json:"files"`
	Hits          []hitJSON `json:"hits"`
	HitsDropped   int       `json:"hitsDropped,omitempty"`
	Errors        []string  `json:"errors"`
	ErrorsDropped int       `json:"errorsDropped,omitempty"`
}

// scan walks <root>/verified and returns the result. Paths are relative to it.
func scan(root string) result {
	res := result{V: 1, Hits: []hitJSON{}, Errors: []string{}}
	var hits []hitJSON
	var errs []string
	if _, err := os.Stat(filepath.Join(root, prepFailedMarker)); err == nil {
		errs = append(errs, "scan-prep failed")
	}
	vroot := filepath.Join(root, "verified")
	var files []string
	walkErr := filepath.WalkDir(vroot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == vroot && os.IsNotExist(err) {
				return nil // no changed .go files
			}
			errs = append(errs, "walk "+relOr(vroot, p))
			return nil
		}
		if !d.IsDir() && strings.HasSuffix(p, ".go") {
			files = append(files, p)
		}
		return nil
	})
	if walkErr != nil {
		errs = append(errs, "walk verified")
	}
	slices.Sort(files)
	for _, p := range files {
		rel := relOr(vroot, p)
		res.Files++
		verified, err := os.ReadFile(p)
		if err != nil {
			errs = append(errs, rel)
			continue
		}
		var base []byte // nil = a new file
		if b, err := os.ReadFile(filepath.Join(root, "base", rel)); err == nil {
			if b == nil {
				b = []byte{}
			}
			base = b
		} else if !os.IsNotExist(err) {
			errs = append(errs, rel)
			continue
		}
		hs, err := subvert.ScanGo(rel, base, verified)
		if err != nil {
			errs = append(errs, rel)
			continue
		}
		for _, h := range hs {
			hits = append(hits, hitJSON{Path: h.Path, Pattern: h.Pattern, Line: h.Line})
		}
	}
	res.Hits, res.HitsDropped = capList(hits, maxHits, res.Hits)
	res.Errors, res.ErrorsDropped = capList(errs, maxErrors, res.Errors)
	return res
}

func capList[T any](in []T, limit int, empty []T) ([]T, int) {
	if len(in) == 0 {
		return empty, 0
	}
	if len(in) > limit {
		return in[:limit], len(in) - limit
	}
	return in, 0
}

func relOr(root, p string) string {
	if r, err := filepath.Rel(root, p); err == nil {
		return filepath.ToSlash(r)
	}
	return p
}

// encode marshals the result under maxMessageBytes by dropping trailing
// entries (counted in the *Dropped fields) until it fits.
func encode(res result) []byte {
	for {
		b, err := json.Marshal(res)
		if err != nil {
			return []byte(`{"v":1,"files":0,"hits":[],"errors":["encode"]}`)
		}
		if len(b) <= maxMessageBytes {
			return b
		}
		switch {
		case len(res.Errors) > 0 && len(res.Errors) >= len(res.Hits):
			res.Errors, res.ErrorsDropped = res.Errors[:len(res.Errors)-1], res.ErrorsDropped+1
		case len(res.Hits) > 0:
			res.Hits, res.HitsDropped = res.Hits[:len(res.Hits)-1], res.HitsDropped+1
		default:
			return []byte(`{"v":1,"files":0,"hits":[],"errors":["too large"]}`)
		}
	}
}

func main() {
	root := "/scan"
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	line := encode(scan(root))
	fmt.Println(string(line))
	path := os.Getenv("TERMINATION_MESSAGE_PATH")
	if path == "" {
		path = defaultMessagePath
	}
	if err := os.WriteFile(path, append(line, '\n'), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "subvert-scan: cannot write the termination message:", err)
	}
	// Always exit 0 (D17.2).
}
