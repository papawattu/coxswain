package controller

import (
	"testing"

	cli "github.com/papawattu/coxswain/internal/cli"
)

// TestLogSelectorLabelsMatchCLI asserts the controller's verify/deliver Job
// pod-selector labels equal the keys the `cox logs` CLI mirrors in its
// argument building (internal/cli/logstream.go verifyForLabel /
// deliverForLabel). A rename on either side fails this test, so the CLI's
// `-l coxswain.io/verify-for=<loop>` / `-l coxswain.io/deliver-for=<loop>`
// selectors stay in lockstep with the labels the Jobs actually stamp.
//
// (The CLI mirrors the keys rather than importing the controller — the CLI is
// dep-light per CLI-PLAN.md — so this cross-package equality spec is the guard
// that keeps the two in sync.)
func TestLogSelectorLabelsMatchCLI(t *testing.T) {
	if verifyForLabel != cli.VerifyForLabel {
		t.Errorf("controller verifyForLabel (%q) != cli.VerifyForLabel (%q)", verifyForLabel, cli.VerifyForLabel)
	}
	if deliverForLabel != cli.DeliverForLabel {
		t.Errorf("controller deliverForLabel (%q) != cli.DeliverForLabel (%q)", deliverForLabel, cli.DeliverForLabel)
	}
}
