// D76 + D102 (R30, owner decisions 2026-11-01): the Secret opt-in gate is
// fail-closed by default. The operator's flag registration lives in
// registerOperatorFlags (main.go), which main calls on flag.CommandLine; a
// missed or wrong wiring (default flipped to false, or the flag never
// registered) would silently disable the gate in production. This file is a
// NON-test source so it compiles into the operator binary: the test below
// pins the default constant and drives the registration on its own FlagSet,
// so the reviewer's mutation (flip the default to false) fails it.
package main

import "flag"

// registerSecretOptInFlag registers --secret-opt-in on the given FlagSet. It
// is the single source of truth for the flag's name and default: main
// registers it on flag.CommandLine (wiring *optIn into the reconciler's
// SecretOptInRequired field), and the test registers it on a scratch
// FlagSet to assert the parsed default is true — a flag dropped from main or
// a flipped default fails TestSecretOptInDefaultIsFailClosed.
func registerSecretOptInFlag(fs *flag.FlagSet) *bool {
	return fs.Bool("secret-opt-in", defaultSecretOptIn,
		"Hold Loops whose named Secrets are not opted in (label coxswain.io/usable=\"true\"; D76/D102). "+
			"Defaults to true (fail-closed); --secret-opt-in=false is a dev/kind escape hatch, never for production.")
}
