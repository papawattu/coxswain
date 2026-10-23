package cli

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runMemoryScriptsTest runs the REAL generated list and write scripts (only
// /memory is substituted with a temp dir) and checks the round trip,
// including no-clobber of an existing file.
func runMemoryScriptsTest(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	sub := func(s string) string { return strings.ReplaceAll(s, "/memory", dir) }

	n1, b1 := lessonFile("fix internal/a.go build")
	n2, b2 := lessonFile("error: foo failed in pkg/b")
	if err := os.WriteFile(filepath.Join(dir, n1), []byte("PRE-EXISTING\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"-c", sub(memoryWriteScript), "sh",
		n1, base64.StdEncoding.EncodeToString(b1),
		n2, base64.StdEncoding.EncodeToString(b2)}
	if out, err := exec.Command("/bin/sh", args...).CombinedOutput(); err != nil {
		t.Fatalf("write script: %v\n%s", err, out)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, n1)); string(got) != "PRE-EXISTING\n" {
		t.Fatalf("write script clobbered an existing file: %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, n2)); string(got) != string(b2) {
		t.Fatalf("new file content = %q", got)
	}
	if ents, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(ents) != 0 {
		t.Fatalf("temp files left: %v", ents)
	}
	// An unrelated file is not listed.
	_ = os.WriteFile(filepath.Join(dir, "notes.md"), []byte("x"), 0o644)
	out, err := exec.Command("/bin/sh", "-c", sub(memoryListScript)).Output()
	if err != nil {
		t.Fatalf("list script: %v", err)
	}
	got, perr := parseMemoryList(string(out))
	if perr != nil || len(got) != 2 || string(got[n2]) != string(b2) || string(got[n1]) != "PRE-EXISTING\n" {
		t.Fatalf("list round trip: %v %v", got, perr)
	}
}
