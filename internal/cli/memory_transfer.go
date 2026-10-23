package cli

// P5d (PLAN-PHASE5.md, "P5d. cox memory export / import"): move a repo's
// memory dir between clusters as a tarball. EXPLICIT AND MANUAL ONLY: there is
// no sync loop, no replication, no CRDT; the only path between clusters is a
// human carrying the file (PLAN.md:83).
//
// The memory dir holds one file per lesson, named <sha256(lesson)>.txt (the
// P5b writer, cmd/memory-writer). The tarball holds those files at its top
// level. Import validates every entry (name shape, content hash == name, size
// caps) BEFORE writing anything, then skips lessons whose SHA-256 is already
// in the PVC, so a re-import changes nothing.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/papawattu/coxswain/pkg/memkey"
)

const (
	// maxImportFiles / maxLessonBytes bound an imported tarball (lessons are
	// <=500 chars, the memory cap is ~100; these are generous ceilings).
	maxImportFiles = 1000
	maxLessonBytes = 4096
)

// lessonNameRe is the only file name an export/import handles: the writer's
// <sha256>.txt.
var lessonNameRe = regexp.MustCompile(`^([0-9a-f]{64})\.txt$`)

// MemoryStore is the memory PVC's read/write boundary for export/import.
// Files are keyed by file name (<sha256>.txt) with their raw content.
type MemoryStore interface {
	// ListFiles returns every lesson file in the PVC. A missing PVC is
	// memoryNotFoundErr.
	ListFiles(ctx context.Context, pvcName string) (map[string][]byte, error)
	// WriteFiles writes the files into the PVC, creating the PVC when absent.
	// It never overwrites or deletes an existing file.
	WriteFiles(ctx context.Context, pvcName string, files map[string][]byte) error
}

func newMemoryExportCommand() *cobra.Command {
	var outPath string
	c := &cobra.Command{
		Use:   "export <repo-url> -o file.tar.gz",
		Short: "Write the repo's memory dir to a tarball (manual; no sync)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return memoryExportToFile(cmd.Context(), currentDeps, namespaceOf(), args[0], outPath, cmd.OutOrStdout())
		},
	}
	c.Flags().StringVarP(&outPath, "output", "o", "", "the tarball to write (required)")
	_ = c.MarkFlagRequired("output")
	return c
}

func newMemoryImportCommand() *cobra.Command {
	var inPath string
	c := &cobra.Command{
		Use:   "import <repo-url> -f file.tar.gz",
		Short: "Write a memory tarball into the repo's memory PVC (idempotent; manual)",
		Long: "cox memory import writes the lessons in a tarball made by `cox memory export`\n" +
			"into the memory PVC for <repo-url> in the current namespace, creating the PVC\n" +
			"(RWX, 1Gi, default StorageClass) when it is absent. A lesson already present\n" +
			"(same SHA-256) is skipped, so re-importing changes nothing.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return memoryImportFromFile(cmd.Context(), currentDeps, namespaceOf(), args[0], inPath, cmd.OutOrStdout())
		},
	}
	c.Flags().StringVarP(&inPath, "file", "f", "", "the tarball to import (required)")
	_ = c.MarkFlagRequired("file")
	return c
}

func memoryStoreOf(dc Deps, ns string) MemoryStore {
	if dc.MemoryStore != nil {
		return dc.MemoryStore
	}
	return &kubectlMemoryStore{pod: &peekMemoryReader{ns: ns}, ns: ns}
}

func memoryExportToFile(ctx context.Context, dc Deps, ns, repoURL, path string, out io.Writer) error {
	var buf bytes.Buffer
	n, err := memoryExport(ctx, memoryStoreOf(dc, ns), repoURL, &buf)
	if err != nil {
		return err
	}
	if err := writeFileExcl(path, buf.Bytes()); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Exported %d lesson(s) to %s\n", n, path)
	return err
}

func memoryImportFromFile(ctx context.Context, dc Deps, ns, repoURL, path string, out io.Writer) error {
	data, err := readFileBounded(path)
	if err != nil {
		return err
	}
	written, skipped, err := memoryImport(ctx, memoryStoreOf(dc, ns), repoURL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Imported %d lesson(s), skipped %d already present\n", written, skipped)
	return err
}

// memoryExport writes the PVC's lessons for repoURL as a .tar.gz to w and
// returns the lesson count. The archive is deterministic (sorted, zero mtime).
func memoryExport(ctx context.Context, store MemoryStore, repoURL string, w io.Writer) (int, error) {
	name := memkey.PVCName(repoURL)
	if name == "" {
		return 0, errors.New("an empty repo URL has no memory PVC")
	}
	files, err := store.ListFiles(ctx, name)
	if err != nil {
		if isNoMemoryErr(err) {
			return 0, fmt.Errorf("no memory to export: the memory PVC %s does not exist", name)
		}
		return 0, fmt.Errorf("read memory PVC %s: %w", name, err)
	}
	if len(files) == 0 {
		return 0, fmt.Errorf("no memory to export: the memory PVC %s is empty", name)
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	for _, n := range names {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(files[n])), Typeflag: tar.TypeReg}); err != nil {
			return 0, err
		}
		if _, err := tw.Write(files[n]); err != nil {
			return 0, err
		}
	}
	if err := tw.Close(); err != nil {
		return 0, err
	}
	if err := gz.Close(); err != nil {
		return 0, err
	}
	return len(names), nil
}

// parseMemoryTarball reads and validates a tarball: only regular
// <sha256>.txt entries whose content hashes to the name (a lesson is its
// hash; a mismatch is a corrupt or tampered file). Nothing is written here.
func parseMemoryTarball(r io.Reader) (map[string][]byte, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("not a gzip tarball: %w", err)
	}
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tarball: %w", err)
		}
		m := lessonNameRe.FindStringSubmatch(h.Name)
		if h.Typeflag != tar.TypeReg || m == nil {
			return nil, fmt.Errorf("tarball entry %q is not a regular <sha256>.txt lesson file", h.Name)
		}
		if len(files) >= maxImportFiles {
			return nil, fmt.Errorf("tarball has more than %d lessons", maxImportFiles)
		}
		body, err := io.ReadAll(io.LimitReader(tr, maxLessonBytes+1))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", h.Name, err)
		}
		if len(body) > maxLessonBytes {
			return nil, fmt.Errorf("tarball entry %s exceeds %d bytes", h.Name, maxLessonBytes)
		}
		sum := sha256.Sum256([]byte(strings.TrimSuffix(string(body), "\n")))
		if hex.EncodeToString(sum[:]) != m[1] {
			return nil, fmt.Errorf("tarball entry %s: content SHA-256 does not match its name", h.Name)
		}
		files[h.Name] = body
	}
	if len(files) == 0 {
		return nil, errors.New("tarball contains no lessons")
	}
	return files, nil
}

// memoryImport writes the tarball's lessons into the PVC, skipping any whose
// SHA-256 (the file name) is already present. Returns (written, skipped).
func memoryImport(ctx context.Context, store MemoryStore, repoURL string, r io.Reader) (int, int, error) {
	name := memkey.PVCName(repoURL)
	if name == "" {
		return 0, 0, errors.New("an empty repo URL has no memory PVC")
	}
	files, err := parseMemoryTarball(r)
	if err != nil {
		return 0, 0, err
	}
	existing, err := store.ListFiles(ctx, name)
	if err != nil && !isNoMemoryErr(err) {
		return 0, 0, fmt.Errorf("read memory PVC %s: %w", name, err)
	}
	toWrite := map[string][]byte{}
	skipped := 0
	for n, body := range files {
		if _, dup := existing[n]; dup {
			skipped++
			continue
		}
		toWrite[n] = body
	}
	if len(toWrite) > 0 || isNoMemoryErr(err) {
		if werr := store.WriteFiles(ctx, name, toWrite); werr != nil {
			return 0, 0, fmt.Errorf("write memory PVC %s: %w", name, werr)
		}
	}
	return len(toWrite), skipped, nil
}

// kubectlMemoryStore is the real MemoryStore: hardened pods against the
// memory PVC (read-only for ListFiles; read-write only for WriteFiles).
type kubectlMemoryStore struct {
	pod *peekMemoryReader
	ns  string
}

const memoryListScript = `for f in /memory/*.txt; do [ -f "$f" ] || continue; printf '%s ' "${f#/memory/}"; base64 "$f" | tr -d '\n'; echo; done`

// memoryWriteScript decodes <name> <base64> argument pairs into /memory,
// never overwriting an existing file (no-clobber via a temp file + ln).
const memoryWriteScript = `set -e; while [ $# -gt 0 ]; do printf '%s' "$2" | base64 -d > "/memory/$1.tmp"; if [ -e "/memory/$1" ]; then rm -f "/memory/$1.tmp"; else mv "/memory/$1.tmp" "/memory/$1"; fi; shift 2; done`

func (k *kubectlMemoryStore) ListFiles(ctx context.Context, pvcName string) (map[string][]byte, error) {
	out, _, err := k.pod.runPod(ctx, pvcName, true, []string{"/bin/sh", "-c", memoryListScript})
	if err != nil {
		return nil, err
	}
	return parseMemoryList(out)
}

// parseMemoryList parses the list script's "<name> <base64>" lines.
func parseMemoryList(out string) (map[string][]byte, error) {
	files := map[string][]byte{}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		name, b64, ok := strings.Cut(line, " ")
		if !ok || lessonNameRe.FindString(name) == "" {
			continue // not a lesson file (the writer only creates <sha256>.txt)
		}
		body, derr := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
		if derr != nil {
			return nil, fmt.Errorf("decode memory file %s: %w", name, derr)
		}
		files[name] = body
	}
	return files, nil
}

func (k *kubectlMemoryStore) WriteFiles(ctx context.Context, pvcName string, files map[string][]byte) error {
	if _, err := k.pod.peekKubectlGet(ctx, "get", "pvc", pvcName, "-o", "json"); err != nil {
		manifest := fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %s
  namespace: %s
spec:
  accessModes: [ReadWriteMany]
  resources:
    requests:
      storage: 1Gi
`, pvcName, k.ns)
		if cerr := k.pod.peekKubectlCreate(ctx, manifest, "create", "-f", "-"); cerr != nil {
			return fmt.Errorf("create memory PVC %s: %w", pvcName, cerr)
		}
	}
	if len(files) == 0 {
		return nil
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	cmd := []string{"/bin/sh", "-c", memoryWriteScript, "sh"}
	for _, n := range names {
		cmd = append(cmd, n, base64.StdEncoding.EncodeToString(files[n]))
	}
	_, _, err := k.pod.runPod(ctx, pvcName, false, cmd)
	return err
}
