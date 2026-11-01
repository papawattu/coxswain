// D76 + D102 (R30, owner decisions 2026-11-01): the Secret opt-in gate is
// fail-closed by default. main.go registers --secret-opt-in through
// registerSecretOptInFlag (loop_secrets_flag.go, a non-test source so it
// compiles into the operator binary) and wires the parsed value into the
// LoopReconciler's SecretOptInRequired field; a missed or wrong wiring
// (default flipped to false, or the flag never registered) would silently
// disable the gate in production. This test pins the default constant and
// drives the registration on a scratch FlagSet, so the reviewer's mutation
// (flip the default to false) fails it.
package main

import (
	"flag"
	"testing"
)

func TestSecretOptInDefaultIsFailClosed(t *testing.T) {
	// The compile-time default main wires into the flag.
	if !defaultSecretOptIn {
		t.Fatal("defaultSecretOptIn = false, want true (fail-closed): " +
			"the D76/D102 Secret opt-in gate must be ON by default; " +
			"Loops that name a Secret must be held until it is opted in")
	}

	// The registration: parse a scratch FlagSet built through the same
	// function main uses — the parsed default must be true.
	fs := flag.NewFlagSet("main", flag.ContinueOnError)
	optIn := registerSecretOptInFlag(fs)
	_ = fs.Parse([]string{})
	if !*optIn {
		t.Error("--secret-opt-in parsed default = false, want true")
	}
}
