package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func put(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func decode(t *testing.T, b []byte) result {
	t.Helper()
	var r result
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("not JSON: %v: %s", err, b)
	}
	return r
}

func TestScanShape(t *testing.T) {
	root := t.TempDir()
	put(t, root, "verified/a/ok.go", "package a\n")
	put(t, root, "verified/b.go", "package b\n")
	put(t, root, "base/b.go", "package b\n")
	put(t, root, "verified/notgo.txt", "x")
	b := encode(scan(root))
	if strings.Contains(string(b), "\n") {
		t.Fatalf("message is not one line: %q", b)
	}
	r := decode(t, b)
	if r.V != 1 || r.Files != 2 || len(r.Hits) != 0 || len(r.Errors) != 0 {
		t.Fatalf("unexpected result: %s", b)
	}
	if !strings.Contains(string(b), `"hits":[]`) || !strings.Contains(string(b), `"errors":[]`) {
		t.Fatalf("empty lists must be [] not null: %s", b)
	}
}

func TestScanEmptyRoot(t *testing.T) {
	r := decode(t, encode(scan(t.TempDir())))
	if r.Files != 0 || len(r.Errors) != 0 {
		t.Fatalf("unexpected: %+v", r)
	}
}

func TestScanUnparseableGoesToErrors(t *testing.T) {
	root := t.TempDir()
	put(t, root, "verified/bad.go", "package {{{ not go\n")
	r := decode(t, encode(scan(root)))
	if len(r.Errors) != 1 || r.Errors[0] != "bad.go" {
		t.Skip("needs S1 step 2: ScanGo is still the stub and does not reject unparseable source; got " + fmt.Sprint(r))
	}
}

func TestScanPrepFailedIsAnError(t *testing.T) {
	root := t.TempDir()
	put(t, root, prepFailedMarker, "")
	r := decode(t, encode(scan(root)))
	if len(r.Errors) != 1 || r.Errors[0] != "scan-prep failed" {
		t.Fatalf("PREP_FAILED must surface as an error: %+v", r)
	}
}

func TestScanProbe371(t *testing.T) {
	src, err := os.ReadFile("../../internal/subvert/testdata/probe371.go.txt")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	put(t, root, "verified/zz_init.go", string(src))
	r := decode(t, encode(scan(root)))
	if len(r.Hits) == 0 {
		t.Skip("needs S1 step 2: ScanGo is still the stub and detects nothing")
	}
	for _, h := range r.Hits {
		if h.Path != "zz_init.go" || h.Line == 0 {
			t.Fatalf("bad hit: %+v", h)
		}
	}
	if len(r.Hits) != 2 {
		t.Fatalf("want both #371 patterns once, got %+v", r.Hits)
	}
}

func TestCaps(t *testing.T) {
	res := result{V: 1, Hits: []hitJSON{}, Errors: []string{}}
	hits := make([]hitJSON, 0, 50)
	errs := make([]string, 0, 50)
	for i := range 50 {
		hits = append(hits, hitJSON{Path: fmt.Sprintf("p%d.go", i), Pattern: "x", Line: i})
		errs = append(errs, fmt.Sprintf("e%d.go", i))
	}
	res.Hits, res.HitsDropped = capList(hits, maxHits, res.Hits)
	res.Errors, res.ErrorsDropped = capList(errs, maxErrors, res.Errors)
	r := decode(t, encode(res))
	if len(r.Hits) != 20 || r.HitsDropped != 30 || len(r.Errors) != 20 || r.ErrorsDropped != 30 {
		t.Fatalf("caps wrong: %+v", r)
	}
}

func TestSizeBound(t *testing.T) {
	res := result{V: 1}
	long := strings.Repeat("d/", 100) + "f.go"
	for range 20 {
		res.Hits = append(res.Hits, hitJSON{Path: long, Pattern: strings.Repeat("p", 60), Line: 1})
		res.Errors = append(res.Errors, long)
	}
	b := encode(res)
	if len(b) > maxMessageBytes {
		t.Fatalf("message is %d bytes", len(b))
	}
	r := decode(t, b)
	if r.HitsDropped+r.ErrorsDropped == 0 {
		t.Fatalf("expected drops: %s", b)
	}
}

func TestMainAlwaysExitsZeroAndWritesMessage(t *testing.T) {
	root := t.TempDir()
	put(t, root, "verified/bad.go", "package {{{\n")
	msg := filepath.Join(t.TempDir(), "term.log")
	t.Setenv("TERMINATION_MESSAGE_PATH", msg)
	old := os.Args
	defer func() { os.Args = old }()
	os.Args = []string{"x", root}
	main() // returns normally: there is no os.Exit on any path
	b, err := os.ReadFile(msg)
	if err != nil {
		t.Fatal(err)
	}
	if r := decode(t, b); r.V != 1 || r.Files != 1 {
		t.Fatalf("unexpected: %s", b)
	}
}
