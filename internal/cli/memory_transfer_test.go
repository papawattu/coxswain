package cli

// P5d specs: export/import of the memory dir as a tarball. The fake store is
// an in-memory PVC; export/import run the real tarball + dedup logic.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

type fakeMemoryStore struct {
	pvcExists bool
	files     map[string][]byte
	writes    []string // every file name passed to WriteFiles, in order
	created   bool
}

func (f *fakeMemoryStore) ListFiles(_ context.Context, _ string) (map[string][]byte, error) {
	if !f.pvcExists {
		return nil, memoryNotFoundErr{}
	}
	out := map[string][]byte{}
	for k, v := range f.files {
		out[k] = v
	}
	return out, nil
}

func (f *fakeMemoryStore) WriteFiles(_ context.Context, _ string, files map[string][]byte) error {
	if !f.pvcExists {
		f.pvcExists, f.created = true, true
	}
	if f.files == nil {
		f.files = map[string][]byte{}
	}
	for n, b := range files {
		f.writes = append(f.writes, n)
		f.files[n] = b
	}
	return nil
}

func lessonFile(text string) (string, []byte) {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:]) + ".txt", []byte(text + "\n")
}

func newStore(lessons ...string) *fakeMemoryStore {
	s := &fakeMemoryStore{pvcExists: true, files: map[string][]byte{}}
	for _, l := range lessons {
		n, b := lessonFile(l)
		s.files[n] = b
	}
	return s
}

func shaSet(files map[string][]byte) []string {
	var out []string
	for n, b := range files {
		sum := sha256.Sum256(b)
		out = append(out, n+":"+hex.EncodeToString(sum[:]))
	}
	sort.Strings(out)
	return out
}

func tarballOf(t *testing.T, s *fakeMemoryStore) []byte {
	t.Helper()
	var buf bytes.Buffer
	if _, err := memoryExport(context.Background(), s, memTestRepo, &buf); err != nil {
		t.Fatalf("export: %v", err)
	}
	return buf.Bytes()
}

// Export writes a tarball whose contents are byte-identical to the PVC's.
func TestMemoryExportMatchesPVC(t *testing.T) {
	src := newStore("fix internal/a.go build", "error: foo failed in pkg/b")
	tb := tarballOf(t, src)
	got, err := parseMemoryTarball(bytes.NewReader(tb))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if strings.Join(shaSet(got), ",") != strings.Join(shaSet(src.files), ",") {
		t.Fatalf("tarball contents differ from the PVC:\n%v\n%v", shaSet(got), shaSet(src.files))
	}
}

func TestMemoryExportMissingOrEmptyIsError(t *testing.T) {
	for _, s := range []*fakeMemoryStore{{}, newStore()} {
		var buf bytes.Buffer
		if _, err := memoryExport(context.Background(), s, memTestRepo, &buf); err == nil {
			t.Fatalf("exporting a missing/empty PVC must be an error")
		}
	}
}

// Import into an empty PVC restores the same SHA-256 set.
func TestMemoryImportRestores(t *testing.T) {
	src := newStore("fix internal/a.go build", "error: foo failed in pkg/b")
	tb := tarballOf(t, src)
	dst := newStore()
	w, s, err := memoryImport(context.Background(), dst, memTestRepo, bytes.NewReader(tb))
	if err != nil || w != 2 || s != 0 {
		t.Fatalf("import = (%d,%d,%v), want (2,0,nil)", w, s, err)
	}
	if strings.Join(shaSet(dst.files), ",") != strings.Join(shaSet(src.files), ",") {
		t.Fatalf("restored set differs")
	}
}

// Import into a missing PVC creates it.
func TestMemoryImportCreatesMissingPVC(t *testing.T) {
	tb := tarballOf(t, newStore("fix internal/a.go build"))
	dst := &fakeMemoryStore{}
	if _, _, err := memoryImport(context.Background(), dst, memTestRepo, bytes.NewReader(tb)); err != nil || !dst.created {
		t.Fatalf("import into missing PVC: err=%v created=%v", err, dst.created)
	}
}

// Idempotent import: a lesson already in the PVC is skipped; a re-import
// writes nothing and leaves the SHA-256 set unchanged. (Mutation "skip the
// dedup on import" must FAIL this spec.)
func TestMemoryImportIdempotent(t *testing.T) {
	tb := tarballOf(t, newStore("fix internal/a.go build", "error: foo failed in pkg/b"))
	dst := newStore("fix internal/a.go build") // one already present
	w, s, err := memoryImport(context.Background(), dst, memTestRepo, bytes.NewReader(tb))
	if err != nil || w != 1 || s != 1 {
		t.Fatalf("first import = (%d,%d,%v), want (1,1,nil)", w, s, err)
	}
	before := strings.Join(shaSet(dst.files), ",")
	dst.writes = nil
	w, s, err = memoryImport(context.Background(), dst, memTestRepo, bytes.NewReader(tb))
	if err != nil || w != 0 || s != 2 {
		t.Fatalf("re-import = (%d,%d,%v), want (0,2,nil)", w, s, err)
	}
	if len(dst.writes) != 0 {
		t.Fatalf("re-import wrote %v; a duplicate must be skipped", dst.writes)
	}
	if strings.Join(shaSet(dst.files), ",") != before {
		t.Fatalf("re-import changed the SHA-256 set")
	}
}

// A tampered entry (content does not hash to its name) rejects the whole
// import before anything is written.
func TestMemoryImportRejectsHashMismatch(t *testing.T) {
	name, _ := lessonFile("fix internal/a.go build")
	bad := &fakeMemoryStore{pvcExists: true, files: map[string][]byte{name: []byte("different text\n")}}
	tb := tarballOf(t, bad)
	dst := newStore()
	if _, _, err := memoryImport(context.Background(), dst, memTestRepo, bytes.NewReader(tb)); err == nil {
		t.Fatalf("a hash-mismatched lesson must be rejected")
	}
	if len(dst.writes) != 0 {
		t.Fatalf("nothing may be written on a rejected import")
	}
}

// A path-traversal or odd-named entry is rejected.
func TestMemoryImportRejectsBadNames(t *testing.T) {
	bad := &fakeMemoryStore{pvcExists: true, files: map[string][]byte{"../evil.txt": []byte("x")}}
	tb := tarballOf(t, bad)
	if _, _, err := memoryImport(context.Background(), newStore(), memTestRepo, bytes.NewReader(tb)); err == nil {
		t.Fatalf("a non-<sha256>.txt entry must be rejected")
	}
}

func TestMemoryImportRejectsNonTarball(t *testing.T) {
	if _, _, err := memoryImport(context.Background(), newStore(), memTestRepo, strings.NewReader("junk")); err == nil {
		t.Fatalf("junk must be rejected")
	}
}

// End to end through the file-based commands: export, wipe, import.
func TestMemoryExportImportFiles(t *testing.T) {
	src := newStore("fix internal/a.go build", "error: foo failed in pkg/b")
	path := filepath.Join(t.TempDir(), "m.tar.gz")
	out := &strings.Builder{}
	if err := memoryExportToFile(context.Background(), Deps{MemoryStore: src}, "default", memTestRepo, path, out); err != nil {
		t.Fatalf("export: %v", err)
	}
	if err := memoryExportToFile(context.Background(), Deps{MemoryStore: src}, "default", memTestRepo, path, out); err == nil {
		t.Fatalf("export must not overwrite an existing file")
	}
	dst := &fakeMemoryStore{} // PVC deleted
	if err := memoryImportFromFile(context.Background(), Deps{MemoryStore: dst}, "default", memTestRepo, path, out); err != nil {
		t.Fatalf("import: %v", err)
	}
	if strings.Join(shaSet(dst.files), ",") != strings.Join(shaSet(src.files), ",") {
		t.Fatalf("restored set differs")
	}
	if !strings.Contains(out.String(), "Imported 2 lesson(s), skipped 0") {
		t.Fatalf("output: %s", out.String())
	}
}

// The real script pair, run against a temp dir with only the path substituted.
func TestMemoryScriptsExecute(t *testing.T) {
	runMemoryScriptsTest(t)
}
