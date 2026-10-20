package memkey

import "testing"

// TestKeyDeterministicAndStable: the key is a function of the URL alone and
// is stable (the same URL always yields the same key, so the operator and
// the CLI — which must agree on the PVC name — compute the same value).
func TestKeyDeterministicAndStable(t *testing.T) {
	const url = "https://github.com/papawattu/coxswain.git"
	a := Key(url)
	b := Key(url)
	if a != b {
		t.Fatalf("Key(%q) not deterministic: %q vs %q", url, a, b)
	}
	if len(a) != KeyHashLen {
		t.Fatalf("Key length = %d, want %d (%q)", len(a), KeyHashLen, a)
	}
	for _, c := range a {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			t.Fatalf("Key(%q) = %q is not lowercase hex", url, a)
		}
	}
}

// TestKeyDistinctURLsDistinctKeys: two different URLs yield different keys
// (a collision here would make two Loops on different repos share a PVC).
func TestKeyDistinctURLsDistinctKeys(t *testing.T) {
	urls := []string{
		"https://github.com/papawattu/coxswain.git",
		"https://github.com/papawattu/coxswain.git#ref",
		"https://github.com/papawattu/other.git",
		"https://github.com/papawattu/coxswain",
		"http://github.com/papawattu/coxswain.git",
	}
	seen := make(map[string]string, len(urls))
	for _, u := range urls {
		k := Key(u)
		if prev, ok := seen[k]; ok {
			t.Fatalf("Key collision: %q and %q both -> %q", prev, u, k)
		}
		seen[k] = u
	}
}

// TestKeyNoNormalization: the key is over the URL EXACTLY as given. A trailing
// .git, a scheme change, or a ref fragment all change the key (the operator
// keys on spec.workspace.repo verbatim; normalising would silently merge
// repos the operator treats as distinct).
func TestKeyNoNormalization(t *testing.T) {
	base := Key("https://github.com/papawattu/coxswain")
	if base == Key("https://github.com/papawattu/coxswain.git") {
		t.Fatal("Key did not distinguish a trailing .git")
	}
	if base == Key("http://github.com/papawattu/coxswain") {
		t.Fatal("Key did not distinguish a scheme change")
	}
	if base == Key("https://github.com/papawattu/coxswain#main") {
		t.Fatal("Key did not distinguish a URL fragment")
	}
}

// TestKeyEmpty: an empty URL returns "" (no memory for a repo-less Loop; the
// caller must not create a PVC for "").
func TestKeyEmpty(t *testing.T) {
	if Key("") != "" {
		t.Fatal("Key(\"\") must be \"\"")
	}
}

// TestPVCName: the PVC name is memory-<key>, and the empty URL yields "" (no
// name, so the caller creates nothing).
func TestPVCName(t *testing.T) {
	const url = "https://github.com/papawattu/coxswain.git"
	want := "memory-" + Key(url)
	if got := PVCName(url); got != want {
		t.Fatalf("PVCName(%q) = %q, want %q", url, got, want)
	}
	if PVCName("") != "" {
		t.Fatal("PVCName(\"\") must be \"\"")
	}
	// The full name is a valid DNS label (≤63 chars, [a-z0-9-]).
	if len(want) > 63 {
		t.Fatalf("PVCName length %d exceeds 63", len(want))
	}
}
