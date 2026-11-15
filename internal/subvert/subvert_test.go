package subvert

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

// expectPatterns is the order-free set of Pattern names a row expects.
func expectPatterns(hits []Hit) []string {
	got := make([]string, 0, len(hits))
	for _, h := range hits {
		got = append(got, h.Pattern)
	}
	slices.Sort(got)
	return got
}

func wantPatterns(names ...string) []string {
	slices.Sort(names)
	return names
}

func TestScanGo(t *testing.T) {
	t.Parallel()

	tm := t.TempDir()

	// writeVerified writes the source as a .go file under a unique dir and
	// returns the path.
	write := func(name, src string) string {
		p := filepath.Join(tm, name)
		if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return p
	}

	probe, err := os.ReadFile(filepath.Join("testdata", "probe371.go.txt"))
	if err != nil {
		t.Fatalf("read probe371 fixture: %v", err)
	}
	probePath := write("probe371.go", string(probe))

	exitInInit := "package main\n\nimport \"os\"\n\nfunc init() { os.Exit(0) }\n"

	rows := []struct {
		name     string
		verified string
		base     string // empty and baseSet false => base nil (new file)
		baseSet  bool
		wantErr  bool
		want     []string // order-free Pattern names
	}{
		{
			name:     "clean: a func that calls fmt.Println",
			verified: "package main\n\nimport \"fmt\"\n\nfunc f() { fmt.Println(\"hi\") }\n",
			want:     []string{},
		},
		{
			name:     "testing.Testing()",
			verified: "package main\n\nimport \"testing\"\n\nfunc f() bool { return testing.Testing() }\n",
			want:     wantPatterns(PatternTestingTesting),
		},
		{
			name:     "os.Exit inside init",
			verified: "package main\n\nimport \"os\"\n\nfunc init() { os.Exit(0) }\n",
			want:     wantPatterns(PatternExitInInit),
		},
		{
			name:     "os.Exit outside init",
			verified: "package main\n\nimport \"os\"\n\nfunc main() { os.Exit(1) }\n",
			want:     []string{},
		},
		{
			name:     "os.Exit in a func literal inside init",
			verified: "package main\n\nimport \"os\"\n\nfunc init() { f := func() { os.Exit(0) }; f() }\n",
			want:     wantPatterns(PatternExitInInit),
		},
		{
			name:     "linkname comment",
			verified: "package main\n\n//go:linkname x testing.y\n\nfunc main() {}\n",
			want:     wantPatterns(PatternLinkname),
		},
		{
			name: `flag.Lookup("test.v")`,
			verified: `package main

import "flag"

func main() { flag.Lookup("test.v") }
`,
			want: wantPatterns(PatternFlagLookupTest),
		},
		{
			name: `flag.Lookup("v") is not suspicious`,
			verified: `package main

import "flag"

func main() { flag.Lookup("v") }
`,
			want: []string{},
		},
		{
			name: "canary name in a string literal",
			verified: `package main

var p = "zzz_canary_test.go"

func main() { _ = p }
`,
			want: wantPatterns(PatternCanaryName),
		},
		{
			name:     "aliased testing",
			verified: "package main\n\nimport t \"testing\"\n\nfunc f() bool { return t.Testing() }\n",
			want:     wantPatterns(PatternTestingTesting),
		},
		{
			name:     "aliased os in init",
			verified: "package main\n\nimport o \"os\"\n\nfunc init() { o.Exit(0) }\n",
			want:     wantPatterns(PatternExitInInit),
		},
		{
			name:     "dot-imported testing",
			verified: "package main\n\nimport . \"testing\"\n\nfunc f() bool { return Testing() }\n",
			want:     wantPatterns(PatternTestingTesting),
		},
		{
			name:     "a local type with a Testing method is not a hit",
			verified: "package main\n\ntype x struct{}\n\nfunc (x) Testing() bool { return true }\n\nfunc f() bool { var v x; return v.Testing() }\n",
			want:     []string{},
		},
		{
			name:     "canary name in a comment",
			verified: "package main\n\n// see zzz_canary_test.go\nfunc main() {}\n",
			want:     wantPatterns(PatternCanaryName),
		},
		{
			name:     "probe371 fixture (base nil)",
			verified: probePath,
			want:     wantPatterns(PatternExitInInit, PatternCanaryName),
		},
		{
			name:     "os.Exit in init already in base",
			verified: exitInInit,
			base:     exitInInit,
			baseSet:  true,
			want:     []string{},
		},
		{
			name:     "second os.Exit in init added over base",
			verified: "package main\n\nimport \"os\"\n\nfunc init() { os.Exit(0); os.Exit(1) }\n",
			base:     exitInInit,
			baseSet:  true,
			want:     wantPatterns(PatternExitInInit),
		},
		{
			name:     "unparseable verified",
			verified: "package x\nfunc {\n",
			wantErr:  true,
		},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			var vpath string
			var verified []byte
			if row.verified == probePath {
				vpath = probePath
				b, err := os.ReadFile(probePath)
				if err != nil {
					t.Fatalf("read probe371: %v", err)
				}
				verified = b
			} else {
				vpath = write("verified-"+row.name+".go", row.verified)
				verified = []byte(row.verified)
			}

			var base []byte
			if row.baseSet {
				base = []byte(row.base)
			}

			hits, err := ScanGo(vpath, base, verified)
			if row.wantErr {
				if err == nil {
					t.Fatalf("ScanGo: err = nil, want error for unparseable source")
				}
				return
			}
			if err != nil {
				t.Fatalf("ScanGo: %v", err)
			}

			if got, want := expectPatterns(hits), row.want; !reflect.DeepEqual(got, want) {
				t.Fatalf("ScanGo patterns = %v, want %v", got, want)
			}

			for _, h := range hits {
				if h.Line <= 0 {
					t.Errorf("hit %s: Line = %d, want > 0", h.Pattern, h.Line)
				}
				if h.Path != vpath {
					t.Errorf("hit %s: Path = %q, want %q", h.Pattern, h.Path, vpath)
				}
			}
		})
	}
}
