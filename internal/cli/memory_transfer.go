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
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/papawattu/coxswain/pkg/memkey"
	"github.com/papawattu/coxswain/pkg/memorywrite"
)

// memShell is the pods' shell.
const memShell = "/bin/sh"

const (
	// maxImportFiles / maxLessonBytes bound an imported tarball (lessons are
	// <=500 chars, the memory cap is ~100; these are generous ceilings).
	maxImportFiles = 1000
	maxLessonBytes = 4096
	// maxTarballBytes bounds the file read before parsing.
	maxTarballBytes = 16 << 20
	// writeBatch is how many lessons one write pod carries (each is passed as
	// a pod argument; batching keeps the pod spec and argv far below their
	// limits for any tarball up to maxImportFiles).
	writeBatch = 50
)

// storageClassRe is a DNS-1123 subdomain (what a StorageClass name must be).
var storageClassRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

func validStorageClass(sc string) error {
	if sc == "" {
		return nil
	}
	if len(sc) > 253 || !storageClassRe.MatchString(sc) {
		return fmt.Errorf("--storage-class %q is not a valid StorageClass name (a DNS-1123 subdomain)", sc)
	}
	return nil
}

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
	var inPath, storageClass string
	c := &cobra.Command{
		Use:   "import <repo-url> -f file.tar.gz",
		Short: "Write a memory tarball into the repo's memory PVC (idempotent; manual)",
		Long: "cox memory import writes the lessons in a tarball made by `cox memory export`\n" +
			"into the memory PVC for <repo-url> in the current namespace, creating the PVC\n" +
			"(RWX, 1Gi, default StorageClass) when it is absent. A lesson already present\n" +
			"(same SHA-256) is skipped, so re-importing changes nothing.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return memoryImportFromFile(cmd.Context(), currentDeps, namespaceOf(), args[0], inPath, storageClass, cmd.OutOrStdout())
		},
	}
	c.Flags().StringVarP(&inPath, "file", "f", "", "the tarball to import (required)")
	c.Flags().StringVar(&storageClass, "storage-class", "", "StorageClass for the memory PVC if import has to create it (default: the cluster default; use an RWX-capable class, the operator's --memory-storage-class)")
	_ = c.MarkFlagRequired("file")
	return c
}

func memoryStoreOf(dc Deps, ns, storageClass string) MemoryStore {
	if dc.MemoryStore != nil {
		return dc.MemoryStore
	}
	return &kubectlMemoryStore{pod: &peekMemoryReader{ns: ns}, ns: ns, storageClass: storageClass}
}

func memoryExportToFile(ctx context.Context, dc Deps, ns, repoURL, path string, out io.Writer) error {
	var buf bytes.Buffer
	n, err := memoryExport(ctx, memoryStoreOf(dc, ns, ""), repoURL, &buf)
	if err != nil {
		return err
	}
	if err := writeFileExcl(path, buf.Bytes()); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Exported %d lesson(s) to %s\n", n, path)
	return err
}

func memoryImportFromFile(ctx context.Context, dc Deps, ns, repoURL, path, storageClass string, out io.Writer) error {
	if err := validStorageClass(storageClass); err != nil {
		return err
	}
	data, err := readFileBounded(path)
	if err != nil {
		return err
	}
	written, skipped, dropped, err := memoryImport(ctx, memoryStoreOf(dc, ns, storageClass), repoURL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintf(out, "Imported %d lesson(s), skipped %d already present, dropped %d by the write policy\n", written, skipped, len(dropped)); err != nil {
		return err
	}
	for _, d := range dropped {
		if _, err = fmt.Fprintf(out, "  dropped %s: %s\n", d.Name, d.Reason); err != nil {
			return err
		}
	}
	if len(dropped) > 0 {
		return fmt.Errorf("%d lesson(s) were dropped by the memory write policy (the rest were imported)", len(dropped))
	}
	return nil
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
	slices.Sort(names)
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
	entries := 0
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
		// Count RAW entries (not unique names): a tarball repeating one name
		// must not make the importer hash it unboundedly often.
		entries++
		if entries > maxImportFiles {
			return nil, fmt.Errorf("tarball has more than %d entries", maxImportFiles)
		}
		if _, dup := files[h.Name]; dup {
			return nil, fmt.Errorf("tarball entry %s appears more than once", h.Name)
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

// droppedLesson is an imported lesson the P5b static write policy refused.
type droppedLesson struct {
	Name   string
	Reason string // pkg/memorywrite's reason (a rule name), never the lesson text
}

// memoryImport writes the tarball's lessons into the PVC. Every lesson goes
// through the same static screen the P5b writer applies (pkg/memorywrite:
// shape, size cap, path/error anchor, secrets patterns): memory is injected
// into every later Loop's prompt, so a tarball (from another cluster, or edited
// by hand) must not carry in what a Loop could not have written. A lesson that
// fails is dropped and reported; the rest are imported. A lesson already in the
// PVC (same SHA-256) is skipped. Returns (written, skipped, dropped).
func memoryImport(ctx context.Context, store MemoryStore, repoURL string, r io.Reader) (int, int, []droppedLesson, error) {
	name := memkey.PVCName(repoURL)
	if name == "" {
		return 0, 0, nil, errors.New("an empty repo URL has no memory PVC")
	}
	files, err := parseMemoryTarball(r)
	if err != nil {
		return 0, 0, nil, err
	}
	// The static screen, per lesson, in a stable order.
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	slices.Sort(names)
	var dropped []droppedLesson
	screened := map[string][]byte{}
	for _, n := range names {
		text := strings.TrimSuffix(string(files[n]), "\n")
		survivors, reasons := memorywrite.Screen([]string{text})
		if len(survivors) != 1 {
			reason := "dropped by the write policy"
			if len(reasons) > 0 && reasons[0] != "" {
				reason = reasons[0]
			}
			dropped = append(dropped, droppedLesson{Name: n, Reason: reason})
			continue
		}
		screened[n] = files[n]
	}
	existing, err := store.ListFiles(ctx, name)
	if err != nil && !isNoMemoryErr(err) {
		return 0, 0, nil, fmt.Errorf("read memory PVC %s: %w", name, err)
	}
	toWrite := map[string][]byte{}
	skipped := 0
	for n, body := range screened {
		if _, dup := existing[n]; dup {
			skipped++
			continue
		}
		toWrite[n] = body
	}
	if len(toWrite) > 0 || (isNoMemoryErr(err) && len(screened) > 0) {
		if werr := store.WriteFiles(ctx, name, toWrite); werr != nil {
			return 0, 0, dropped, fmt.Errorf("write memory PVC %s: %w", name, werr)
		}
	}
	return len(toWrite), skipped, dropped, nil
}

// kubectlMemoryStore is the real MemoryStore: hardened pods against the
// memory PVC (read-only for ListFiles; read-write only for WriteFiles).
type kubectlMemoryStore struct {
	pod          *peekMemoryReader
	ns           string
	storageClass string // "" = the cluster default
}

const memoryListScript = `for f in /memory/*.txt; do [ -f "$f" ] || continue; printf '%s ' "${f#/memory/}"; base64 "$f" | tr -d '\n'; echo; done`

// memoryWriteScript decodes <name> <base64> argument pairs into /memory,
// never overwriting an existing file (no-clobber via a temp file + ln).
const memoryWriteScript = `set -e; while [ $# -gt 0 ]; do printf '%s' "$2" | base64 -d > "/memory/$1.tmp"; if [ -e "/memory/$1" ]; then rm -f "/memory/$1.tmp"; else mv "/memory/$1.tmp" "/memory/$1"; fi; shift 2; done`

func (k *kubectlMemoryStore) ListFiles(ctx context.Context, pvcName string) (map[string][]byte, error) {
	out, _, err := k.pod.runPod(ctx, pvcName, true, []string{memShell, "-c", memoryListScript})
	if err != nil {
		return nil, err
	}
	return parseMemoryList(out)
}

// parseMemoryList parses the list script's "<name> <base64>" lines.
func parseMemoryList(out string) (map[string][]byte, error) {
	files := map[string][]byte{}
	for line := range strings.SplitSeq(out, "\n") {
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

// memoryPVCManifest renders the PVC import creates when it is absent (the
// operator's shape: RWX, 1Gi). storageClass "" = the cluster default.
func memoryPVCManifest(pvcName, ns, storageClass string) string {
	scLine := ""
	if storageClass != "" {
		scLine = "  storageClassName: " + storageClass + "\n"
	}
	return fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %s
  namespace: %s
spec:
  accessModes: [ReadWriteMany]
%s  resources:
    requests:
      storage: 1Gi
`, pvcName, ns, scLine)
}

// memoryWriteCommand is one write pod's command: the script plus <name>
// <base64> argument pairs.
func memoryWriteCommand(names []string, files map[string][]byte) []string {
	cmd := []string{memShell, "-c", memoryWriteScript, "sh"}
	for _, n := range names {
		cmd = append(cmd, n, base64.StdEncoding.EncodeToString(files[n]))
	}
	return cmd
}

func (k *kubectlMemoryStore) WriteFiles(ctx context.Context, pvcName string, files map[string][]byte) error {
	if err := validStorageClass(k.storageClass); err != nil {
		return err
	}
	if _, err := k.pod.peekKubectlGet(ctx, "get", "pvc", pvcName, "-o", "json"); err != nil {
		if cerr := k.pod.peekKubectlCreate(ctx, memoryPVCManifest(pvcName, k.ns, k.storageClass), "create", "-f", "-"); cerr != nil {
			// Another importer (or the operator) may have created it between
			// our get and create: that is fine, the PVC exists.
			if _, gerr := k.pod.peekKubectlGet(ctx, "get", "pvc", pvcName, "-o", "json"); gerr != nil {
				return fmt.Errorf("create memory PVC %s: %w", pvcName, cerr)
			}
		}
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, batch := range chunkNames(names, writeBatch) {
		if _, _, err := k.pod.runPod(ctx, pvcName, false, memoryWriteCommand(batch, files)); err != nil {
			return err
		}
	}
	return nil
}

// chunkNames splits names into batches of at most size.
func chunkNames(names []string, size int) [][]string {
	var out [][]string
	for start := 0; start < len(names); start += size {
		out = append(out, names[start:min(start+size, len(names))])
	}
	return out
}
