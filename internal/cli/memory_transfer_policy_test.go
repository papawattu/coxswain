package cli

// P5d review fixes: the write-policy screen on import, the rendered pod and
// PVC manifests, --storage-class validation, and the tarball gates.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/papawattu/coxswain/pkg/memorywrite"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// rawTarball builds a .tar.gz from explicit headers+bodies (so a spec can make
// entries the exporter never would: symlinks, dirs, duplicates, oversize).
func rawTarball(t *testing.T, entries []tar.Header, bodies [][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for i := range entries {
		h := entries[i]
		h.Size = int64(len(bodies[i]))
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(bodies[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func regEntry(name string) tar.Header {
	return tar.Header{Name: name, Mode: 0o644, Typeflag: tar.TypeReg}
}

// P2-1: a lesson the P5b static screen would drop (a secret, over 500 chars,
// no path/error anchor) is dropped on import with its rule; the others import.
// Fixtures are concatenated (the GitHub secret scanner blocks literal shapes).
func TestMemoryImportScreensLessons(t *testing.T) {
	good := "fix internal/a.go build"
	secret := "set DB_PASSWORD=" + "hunter2" + " in deploy/config.yaml before applying"
	long := "see internal/a.go " + strings.Repeat("word ", 120) // > 500 chars
	noAnchor := "always remember to run the tests before pushing"
	src := newStore(good, secret, long, noAnchor)
	tb := tarballOf(t, src)
	dst := newStore()
	w, s, dropped, err := memoryImport(context.Background(), dst, memTestRepo, bytes.NewReader(tb))
	if err != nil || w != 1 || s != 0 || len(dropped) != 3 {
		t.Fatalf("import = (%d,%d,%d dropped,%v), want (1,0,3,nil)", w, s, len(dropped), err)
	}
	gn, _ := lessonFile(good)
	if _, ok := dst.files[gn]; !ok || len(dst.files) != 1 {
		t.Fatalf("only the good lesson may be written, got %v", dst.files)
	}
	for _, d := range dropped {
		if d.Reason == "" || strings.Contains(d.Reason, "hunter2") {
			t.Fatalf("a drop needs a rule name and must not echo the lesson: %+v", d)
		}
	}
	// the secret's drop names the credential rule
	sn, _ := lessonFile(secret)
	found := false
	for _, d := range dropped {
		if d.Name == sn && strings.Contains(d.Reason, "credential-assignment") {
			found = true
		}
	}
	if !found {
		t.Fatalf("secret lesson not dropped by credential-assignment: %+v", dropped)
	}
}

// The command reports every drop and exits non-zero when any were dropped,
// still importing the rest.
func TestMemoryImportCommandReportsDropsAndFails(t *testing.T) {
	secret := "set DB_PASSWORD=" + "hunter2" + " in deploy/config.yaml before applying"
	path := filepath.Join(t.TempDir(), "m.tar.gz")
	if err := os.WriteFile(path, tarballOf(t, newStore("fix internal/a.go build", secret)), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := newStore()
	out := &strings.Builder{}
	err := memoryImportFromFile(context.Background(), Deps{MemoryStore: dst}, "default", memTestRepo, path, "", out)
	if err == nil {
		t.Fatalf("a dropped lesson must make the command fail")
	}
	if len(dst.files) != 1 || !strings.Contains(out.String(), "dropped 1 by the write policy") || !strings.Contains(out.String(), "credential-assignment") {
		t.Fatalf("output %q files %d", out.String(), len(dst.files))
	}
}

// P2-2: the rendered pod is hardened and carries no SA token; only the write
// pod mounts the PVC read-write.
func TestMemoryPodManifestHardened(t *testing.T) {
	for _, ro := range []bool{true, false} {
		m, err := memoryPodManifest("ns1", "memory-abc", ro, []string{memShell, "-c", "true"})
		if err != nil {
			t.Fatal(err)
		}
		var pod corev1.Pod
		if err := yaml.UnmarshalStrict([]byte(m), &pod); err != nil {
			t.Fatalf("manifest does not parse as a Pod: %v\n%s", err, m)
		}
		if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
			t.Errorf("ro=%v: automountServiceAccountToken must be false", ro)
		}
		if pod.Namespace != "ns1" || pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
			t.Errorf("ro=%v: namespace/restartPolicy: %s %s", ro, pod.Namespace, pod.Spec.RestartPolicy)
		}
		checkPodSecurity(t, ro, &pod)
		c := pod.Spec.Containers[0]
		if got := c.VolumeMounts[0].ReadOnly; got != ro {
			t.Errorf("ro=%v: mount readOnly=%v", ro, got)
		}
		if v := pod.Spec.Volumes[0].PersistentVolumeClaim; v == nil || v.ClaimName != "memory-abc" || v.ReadOnly != ro {
			t.Errorf("ro=%v: volume claim: %+v", ro, v)
		}
	}
}

func checkPodSecurity(t *testing.T, ro bool, pod *corev1.Pod) {
	t.Helper()
	psc := pod.Spec.SecurityContext
	if psc == nil || psc.RunAsNonRoot == nil || !*psc.RunAsNonRoot || psc.SeccompProfile == nil || psc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("ro=%v: pod securityContext: %+v", ro, psc)
	}
	c := pod.Spec.Containers[0]
	sc := c.SecurityContext
	if c.Image != memoryPodImage || strings.HasSuffix(c.Image, ":latest") || !strings.Contains(c.Image, ":") {
		t.Errorf("ro=%v: image must be pinned: %s", ro, c.Image)
	}
	if sc == nil || sc.RunAsUser == nil || *sc.RunAsUser != 65532 || !*sc.RunAsNonRoot ||
		sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem ||
		sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation ||
		sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("ro=%v: container securityContext: %+v", ro, sc)
	}
}

// P2-3: --storage-class must be a DNS-1123 name, and the created PVC is RWX 1Gi.
func TestValidStorageClass(t *testing.T) {
	for _, ok := range []string{"", "local-shared", "a", "fast.ssd-1", "a.b.c", "a-b.c-d"} {
		if err := validStorageClass(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"Bad", "a b", "x\ny: z", "a\"b", "-a", "a-", "a..b", "a.-b", "a-.b", ".a", "a.", strings.Repeat("a", 254), "a: {b: c}"} {
		if err := validStorageClass(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	// the command refuses before touching anything
	path := filepath.Join(t.TempDir(), "m.tar.gz")
	_ = os.WriteFile(path, tarballOf(t, newStore("fix internal/a.go build")), 0o600)
	dst := &fakeMemoryStore{}
	if err := memoryImportFromFile(context.Background(), Deps{MemoryStore: dst}, "default", memTestRepo, path, "bad class", &strings.Builder{}); err == nil || dst.created {
		t.Fatalf("invalid storage class: err=%v created=%v", err, dst.created)
	}
}

func TestMemoryPVCManifest(t *testing.T) {
	for _, sc := range []string{"", "local-shared"} {
		var pvc corev1.PersistentVolumeClaim
		if err := yaml.UnmarshalStrict([]byte(memoryPVCManifest("memory-abc", "ns1", sc)), &pvc); err != nil {
			t.Fatal(err)
		}
		if pvc.Name != "memory-abc" || pvc.Namespace != "ns1" ||
			len(pvc.Spec.AccessModes) != 1 || pvc.Spec.AccessModes[0] != corev1.ReadWriteMany {
			t.Errorf("sc=%q: %+v", sc, pvc.ObjectMeta)
		}
		if q := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; q.String() != "1Gi" {
			t.Errorf("sc=%q: storage %s", sc, q.String())
		}
		got := ""
		if pvc.Spec.StorageClassName != nil {
			got = *pvc.Spec.StorageClassName
		}
		if got != sc {
			t.Errorf("storageClassName = %q, want %q", got, sc)
		}
	}
}

// Tarball gates: each must have a failing spec.
func TestParseMemoryTarballGates(t *testing.T) {
	emptyName, _ := lessonFile("") // sha256("").txt
	okName, okBody := lessonFile("fix internal/a.go build")
	cases := map[string][]byte{
		"symlink named like a lesson": rawTarball(t,
			[]tar.Header{{Name: emptyName, Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}}, [][]byte{nil}),
		"directory":      rawTarball(t, []tar.Header{{Name: okName, Typeflag: tar.TypeDir, Mode: 0o755}}, [][]byte{nil}),
		"duplicate name": rawTarball(t, []tar.Header{regEntry(okName), regEntry(okName)}, [][]byte{okBody, okBody}),
		"oversize lesson": func() []byte {
			big := bytes.Repeat([]byte("a"), maxLessonBytes+1)
			sum := sha256.Sum256(big)
			return rawTarball(t, []tar.Header{regEntry(hex.EncodeToString(sum[:]) + ".txt")}, [][]byte{big})
		}(),
		"too many entries": distinctTarball(t, maxImportFiles+1),
	}
	for name, tb := range cases {
		if _, err := parseMemoryTarball(bytes.NewReader(tb)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// exactly maxImportFiles distinct entries is fine
	if _, err := parseMemoryTarball(bytes.NewReader(distinctTarball(t, maxImportFiles))); err != nil {
		t.Errorf("%d distinct entries rejected: %v", maxImportFiles, err)
	}
}

// distinctTarball makes n valid, distinct lesson entries.
func distinctTarball(t *testing.T, n int) []byte {
	t.Helper()
	hs := make([]tar.Header, n)
	bs := make([][]byte, n)
	for i := range hs {
		name, body := lessonFile(fmt.Sprintf("lesson %d in internal/f.go", i))
		hs[i], bs[i] = regEntry(name), body
	}
	return rawTarball(t, hs, bs)
}

// The 16 MiB tarball cap.
func TestReadFileBoundedCap(t *testing.T) {
	dir := t.TempDir()
	ok, big := filepath.Join(dir, "ok"), filepath.Join(dir, "big")
	for p, n := range map[string]int64{ok: maxTarballBytes, big: maxTarballBytes + 1} {
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(n); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
	if _, err := readFileBounded(ok); err != nil {
		t.Errorf("exactly the cap rejected: %v", err)
	}
	if _, err := readFileBounded(big); err == nil {
		t.Errorf("over the cap accepted")
	}
}

// Export is deterministic: same lessons, same bytes, whatever the map order.
func TestMemoryExportDeterministic(t *testing.T) {
	a := newStore("fix internal/a.go build", "error: foo failed in pkg/b", "go vet fails on pkg/b/c.go:12")
	first := tarballOf(t, a)
	for range 20 {
		if !bytes.Equal(first, tarballOf(t, a)) {
			t.Fatalf("export is not deterministic")
		}
	}
}

func TestChunkNames(t *testing.T) {
	names := make([]string, 2*writeBatch+3)
	got := chunkNames(names, writeBatch)
	if len(got) != 3 || len(got[0]) != writeBatch || len(got[2]) != 3 {
		t.Fatalf("chunks: %d (%d,..,%d)", len(got), len(got[0]), len(got[len(got)-1]))
	}
	if len(chunkNames(nil, writeBatch)) != 0 {
		t.Fatalf("no names, no chunks")
	}
}

func TestMemoryWriteCommandShape(t *testing.T) {
	n, b := lessonFile("fix internal/a.go build")
	cmd := memoryWriteCommand([]string{n}, map[string][]byte{n: b})
	if len(cmd) != 6 || cmd[0] != memShell || cmd[3] != "sh" || cmd[4] != n {
		t.Fatalf("command: %v", cmd)
	}
}

// D1: a lesson already on the PVC is "skipped", never "dropped", even if
// today's write policy would reject it (a lesson an older writer accepted):
// a no-op re-import changes nothing and exits 0.
func TestMemoryImportAlreadyPresentIsNotDropped(t *testing.T) {
	old := "use aws_secret_access_key=" + "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY" + " in cmd/aws.go" // rejected by today's rules
	if s, _ := memorywriteScreen(old); len(s) != 0 {
		t.Fatalf("test bug: %q must be rejected by the policy", old)
	}
	dst := newStore(old) // an older writer accepted it
	path := filepath.Join(t.TempDir(), "m.tar.gz")
	if err := os.WriteFile(path, tarballOf(t, dst), 0o600); err != nil {
		t.Fatal(err)
	}
	out := &strings.Builder{}
	if err := memoryImportFromFile(context.Background(), Deps{MemoryStore: dst}, "default", memTestRepo, path, "", out); err != nil {
		t.Fatalf("a no-op re-import must exit 0: %v\n%s", err, out)
	}
	if !strings.Contains(out.String(), "skipped 1 already present, dropped 0") || len(dst.writes) != 0 {
		t.Fatalf("output %q writes %v", out.String(), dst.writes)
	}
	// ...but the SAME lesson arriving as NEW is dropped.
	fresh := newStore()
	if err := os.WriteFile(path, tarballOf(t, newStore(old)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := memoryImportFromFile(context.Background(), Deps{MemoryStore: fresh}, "default", memTestRepo, path, "", &strings.Builder{}); err == nil || len(fresh.files) != 0 {
		t.Fatalf("a new unsafe lesson must be dropped: err=%v files=%d", err, len(fresh.files))
	}
}

func memorywriteScreen(l string) ([]string, []string) { return memorywrite.Screen([]string{l}) }

// fakeKubectlDir installs a fake kubectl on PATH that answers the memory
// pod flow (get pvc, create -f -, get pod, logs, delete), saving every
// create's stdin to create.<n>; `get pvc` fails until it has been created
// when pvcMissing. Returns the dir.
func fakeKubectlDir(t *testing.T, pvcMissing bool, podLog string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "podlog"), []byte(podLog), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := "0"
	if pvcMissing {
		missing = "1"
	}
	script := `#!/bin/sh
d="` + dir + `"
case " $* " in
*" get pvc "*)
  if [ "` + missing + `" = 1 ] && ! ls "$d"/create.* >/dev/null 2>&1; then exit 1; fi
  echo '{}'; exit 0 ;;
*" create -f - "*)
  n=$(ls "$d" | grep -c '^create\.'); cat > "$d/create.$n"; exit 0 ;;
*" get pod -l "*) printf podx; exit 0 ;;
*" get pod podx "*) echo '{"status":{"phase":"Succeeded"}}'; exit 0 ;;
*" logs "*) cat "$d/podlog"; exit 0 ;;
*" delete "*) exit 0 ;;
esac
echo "unexpected: $*" >&2; exit 99
`
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func readCreate(t *testing.T, dir string, n int) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("create.%d", n)))
	if err != nil {
		t.Fatalf("create.%d: %v", n, err)
	}
	return string(b)
}

// D2: the pod the REAL store actually submits (not just the renderer) is
// read-only for listing, read-write only for writing, tokenless and pinned.
func TestKubectlMemoryStoreSubmitsHardenedPods(t *testing.T) {
	n, b := lessonFile("fix internal/a.go build")
	dir := fakeKubectlDir(t, false, n+" "+base64.StdEncoding.EncodeToString(b)+"\n")
	k := &kubectlMemoryStore{pod: &peekMemoryReader{ns: "ns1"}, ns: "ns1"}

	got, err := k.ListFiles(context.Background(), "memory-abc")
	if err != nil || string(got[n]) != string(b) {
		t.Fatalf("ListFiles = %v, %v", got, err)
	}
	list := readCreate(t, dir, 0)
	if strings.Count(list, "readOnly: true") != 2 || strings.Contains(list, "readOnly: false") {
		t.Errorf("the list pod must mount the PVC read-only:\n%s", list)
	}

	if err := k.WriteFiles(context.Background(), "memory-abc", map[string][]byte{n: b}); err != nil {
		t.Fatal(err)
	}
	write := readCreate(t, dir, 1)
	if strings.Count(write, "readOnly: false") != 2 || strings.Contains(write, "readOnly: true") {
		t.Errorf("the write pod must mount the PVC read-write:\n%s", write)
	}
	if !strings.Contains(write, n) {
		t.Errorf("the write pod must carry the lesson name")
	}
	for name, m := range map[string]string{"list": list, "write": write} {
		if !strings.Contains(m, "automountServiceAccountToken: false") {
			t.Errorf("%s pod must not mount an SA token:\n%s", name, m)
		}
		if !strings.Contains(m, "image: "+memoryPodImage) {
			t.Errorf("%s pod must use the pinned image %s", name, memoryPodImage)
		}
	}
}

// The PVC import creates when absent is what is submitted (RWX, 1Gi, the
// requested class), then the write pod follows.
func TestKubectlMemoryStoreCreatesMissingPVC(t *testing.T) {
	dir := fakeKubectlDir(t, true, "")
	k := &kubectlMemoryStore{pod: &peekMemoryReader{ns: "ns1"}, ns: "ns1", storageClass: "local-shared"}
	n, b := lessonFile("fix internal/a.go build")
	if err := k.WriteFiles(context.Background(), "memory-abc", map[string][]byte{n: b}); err != nil {
		t.Fatal(err)
	}
	pvc := readCreate(t, dir, 0)
	if !strings.Contains(pvc, "kind: PersistentVolumeClaim") || !strings.Contains(pvc, "storageClassName: local-shared") || !strings.Contains(pvc, "ReadWriteMany") {
		t.Errorf("PVC submitted:\n%s", pvc)
	}
	if !strings.Contains(readCreate(t, dir, 1), "kind: Pod") {
		t.Errorf("the write pod must follow the PVC")
	}
}
