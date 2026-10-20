// Package memkey is the single source of truth for the shared memory PVC's
// name (Phase 5, P5a, PLAN-PHASE5.md). Both the operator (which creates the
// PVC) and the CLI (cox memory, which finds it) must compute the SAME name
// for a (namespace, git URL), so the keying lives here — a tiny shared
// package — instead of being duplicated in the two modules' call sites.
package memkey

import (
	"crypto/sha256"
	"encoding/hex"
)

// KeyHashLen is the length (hex chars) of the URL hash in the PVC name. 12
// hex chars (48 bits) is collision-resistant enough for a per-namespace key
// while keeping the PVC name short (memory- + 12 = 20 chars, well under
// DNS_LABEL 63).
const KeyHashLen = 12

// namePrefix is the memory PVC name prefix (memory-<hash>).
const namePrefix = "memory-"

// keyLen caps the PVC name's total length so a long URL hash cannot overflow
// a DNS label (63 chars). 48 chars (48 bytes) is a DNS label on its own, so
// memory-<48> = 55 chars; the keyLen cap guarantees the hash part stays ≤48
// hex chars (it is exactly KeyHashLen by construction — the cap is
// defensive, not load-bearing).
const keyLen = 48

// Key returns the memory key for a git URL: the first KeyHashLen hex chars of
// the URL's SHA-256. The URL is keyed EXACTLY as given (the spec.workspace.
// repo string): normalising it (trailing .git, scheme, host case) would make
// two Loops that the operator sees as DIFFERENT repos share a PVC they
// shouldn't (the key must match the operator's spec, not a canonical form).
// An empty URL returns "" (the caller must not call PVCName with "" — no
// memory for a repo-less Loop).
func Key(gitURL string) string {
	if gitURL == "" {
		return ""
	}
	h := sha256.Sum256([]byte(gitURL))
	summary := hex.EncodeToString(h[:])
	if len(summary) > keyLen {
		summary = summary[:keyLen]
	}
	return summary[:KeyHashLen]
}

// PVCName returns the memory PVC's name for a git URL: memory-<Key(gitURL)>.
// The name is per (namespace, git URL) because a PVC is mountable only by
// pods in its own namespace (D66): the name is namespace-scoped by the PVC
// object itself, so the same name in two namespaces is two PVCs. An empty
// URL returns "" (no memory for a repo-less Loop).
func PVCName(gitURL string) string {
	k := Key(gitURL)
	if k == "" {
		return ""
	}
	return namePrefix + k
}

// MemoryDirName is the memory dir's name in the workspace's .coxswain/ dir
// (<workspace>/.coxswain/memory/): the operator's copy-in init container
// materialises the PVC contents there, and the runner reads it into its
// prompt (ADR-0004: the .coxswain dir is operator-owned).
const MemoryDirName = "memory"
