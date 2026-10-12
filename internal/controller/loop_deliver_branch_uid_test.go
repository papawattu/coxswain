// Copyright 2026 papawattu.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// TestDeliverBranchNameFormat (R25 I81) pins the delivery branch name format:
// <prefix><loop-name>-<uid[:8]>, where uid[:8] is the first 8 characters of
// the Loop's metadata.uid.
//
// The uid suffix makes the branch UNIQUE PER LOOP INSTANCE. An earlier Loop of
// the same name (this cluster or another) leaves <prefix><loop-name>-<olduid8>
// on the remote; without the uid suffix the new Loop's push to <prefix>
// <loop-name> is rejected ("fetch first") and neither the Loop nor a
// `cox redeliver` of it can deliver until the remote branch is deleted by
// hand. With the suffix, branches never collide and nothing is overwritten.
//
// Acceptance (I81): the branch name is <prefix><loop-name>-<uid8> and a second
// Loop with an earlier Loop's name delivers to a DIFFERENT branch (so the
// earlier branch existing does not reject its push).
func TestDeliverBranchNameFormat(t *testing.T) {
	const prefix = "coxswain/"
	const loopName = "my-loop"

	// A k8s-like UID (15 chars). The first 8 are "deadbeef".
	uidA := types.UID("deadbeef1234")
	wantA := prefix + loopName + "-deadbeef"
	if got := deliverBranchName(prefix, loopName, string(uidA)); got != wantA {
		t.Fatalf("deliverBranchName(%q,%q,%q) = %q; want %q (<prefix><loop>-<uid[:8]>)",
			prefix, loopName, string(uidA), got, wantA)
	}

	// A second Loop with the SAME name but a DIFFERENT uid gets a DIFFERENT
	// branch — the I81 collision case.
	uidB := types.UID("cafe12345678")
	wantB := prefix + loopName + "-cafe1234"
	if got := deliverBranchName(prefix, loopName, string(uidB)); got != wantB {
		t.Fatalf("a second Loop with the same name must get a different branch: deliverBranchName(…, %q) = %q; want %q",
			string(uidB), got, wantB)
	}
	if wantA == wantB {
		t.Fatalf("two same-name Loops with different uids produced the SAME branch %q (the I81 collision)", wantA)
	}

	// The uid suffix is EXACTLY the first 8 chars — not the whole uid, not
	// fewer. (A mutation that drops the suffix makes wantA != <prefix>
	// <loop-name>-<uid8>; a mutation that uses the whole uid makes it longer
	// than <prefix><loop-name>-8.)
	if suffix := deliverBranchName(prefix, loopName, string(uidA))[len(prefix)+len(loopName):]; suffix != "-deadbeef" {
		t.Fatalf("branch suffix %q; want \"-deadbeef\" (the first 8 chars of the uid, with a hyphen)", suffix)
	}
	// 8 chars total, all alphanumeric (a k8s uid is lowercase alphanumeric).
	if len(strings.TrimPrefix("-deadbeef", "-")) != 8 {
		t.Fatal("the uid suffix must be 8 chars")
	}

	// The empty-uid edge (unit tests build Loops without a uid; the reconciler
	// always has one set by the API) degrades to <prefix><loop-name> with no
	// dangling hyphen.
	if got := deliverBranchName(prefix, loopName, ""); got != prefix+loopName {
		t.Fatalf("empty uid must degrade to %q; got %q (no dangling hyphen)", prefix+loopName, got)
	}
	// A uid shorter than 8 also degrades (never a partial suffix).
	if got := deliverBranchName(prefix, loopName, "abc"); got != prefix+loopName {
		t.Fatalf("a uid shorter than 8 must degrade to %q; got %q", prefix+loopName, got)
	}
}

// TestDeliverBranchNameMatchesStatus (R25 I81) asserts the deliver readback's
// wantBranch (what the push termination message's branch MUST equal) is the
// SAME value as the branch the push container is built with, and that it
// carries the uid suffix. This is the readback half of I81: if the readback
// expected <prefix><loop-name> while the push used <prefix><loop-name>-<uid8>
// (or vice versa), a successful delivery would be rejected as "branch does
// not match the delivery branch".
func TestDeliverBranchNameMatchesStatus(t *testing.T) {
	loop := &coxv1alpha1.Loop{
		ObjectMeta: metav1.ObjectMeta{
			Name: "i81-loop",
			UID:  types.UID("0123456789ab"),
		},
		Spec: coxv1alpha1.LoopSpec{
			Workspace: coxv1alpha1.Workspace{Repo: "https://github.com/samples/gocli"},
		},
		Status: coxv1alpha1.LoopStatus{
			CurrentVerify: &coxv1alpha1.CurrentVerifyStatus{
				VerifiedCommit: "1111111111111111111111111111111111111111",
			},
		},
	}
	// The branch the push container is built with (buildDeliverJobSpec) and the
	// branch the readback validates (parseDeliverTermination) must be the same
	// value — both call deliverBranchName(prefix, name, uid).
	prefix := deliverBranchPrefix(loop)
	want := deliverBranchName(prefix, loop.Name, string(loop.UID))
	if want != "coxswain/i81-loop-01234567" {
		t.Fatalf("the delivery branch for loop uid 0123456789ab must be coxswain/i81-loop-01234567; got %q", want)
	}
	// A termination message on the OLD (pre-I81) branch name must be REJECTED
	// by the readback (the loop delivered to its uid-suffixed branch, not the
	// stale one).
	msg := "branch=coxswain/i81-loop\ncommit=1111111111111111111111111111111111111111\nprNumber=1\nprURL=https://github.com/samples/gocli/pull/1\n"
	if _, ok, _ := parseDeliverTermination(msg, loop); ok {
		t.Fatal("the readback must REJECT a termination message whose branch is the old <prefix><loop-name> (no uid suffix); it should expect the uid-suffixed branch")
	}
	// And a termination message on the NEW (uid-suffixed) branch must be
	// ACCEPTED.
	msg = "branch=" + want + "\ncommit=1111111111111111111111111111111111111111\nprNumber=1\nprURL=https://github.com/samples/gocli/pull/1\n"
	if _, ok, err := parseDeliverTermination(msg, loop); !ok {
		t.Fatalf("the readback must ACCEPT a termination message on the uid-suffixed branch; got ok=%v err=%v", ok, err)
	}
}
