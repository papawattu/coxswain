package controller

// Shared production string constants (goconst): values that repeat across the
// controller builders (agent / verify / deliver / egress-proxy) and would
// otherwise trip goconst's min-occurrences threshold. Kept out of the
// individual files so the constant is defined once.
const (
	// partOfCoxswain is the common "app.kubernetes.io/part-of" / managed-by
	// label value.
	partOfCoxswain = "coxswain"

	// workspaceCredsMount is the read-only mount path for the Git credential
	// secret (username/password items) the workspace-init and verify builders
	// reference in their credential-helper scripts.
	workspaceCredsMount = "/workspace-creds"

	// gitBasicAuthHeader is the git -c flag that injects the Basic-auth
	// credential into a single git command (the workspace-init fetch and the
	// verify import both build it).
	gitBasicAuthHeader = ` -c http.extraHeader="Authorization: Basic $AUTH"`

	// githubHost is the GitHub delivery host (the deliver provider + the
	// egress-proxy SNI allowlist + api.github.com all key off it).
	githubHost = "github.com"

	// runnerShellPath is the ABSOLUTE path of the runner's shell: the runner
	// executes every tool call via exec.Command("sh", "-c", …) (runner/runner.go)
	// and the PATH in the sandbox image resolves "sh" to /bin/sh (the verify
	// scripts use #!/bin/sh; verifySh in loop_verify_job.go). D46 gate input.
	runnerShellPath = "/bin/sh"

	// runnerShellBase is the shell's binary name (for messages: "via sh -c").
	runnerShellBase = "sh"
)

// missingShellInExecList (D46) reports whether a NON-EMPTY union of referenced
// AgentPolicy exec lists omits the runner's shell (runnerShellPath). An empty
// list is a clean pass: with no exec allows the KubeArmor policy carries no
// process rule, so exec is unrestricted (the default-deny minimum the demo
// runs). A list that exists but cannot run the runner's tool calls wedges the
// Loop under an enforcing Block policy, so it is the rejection case.
func missingShellInExecList(exec []string) bool {
	if len(exec) == 0 {
		return false
	}
	for _, e := range exec {
		if e == runnerShellPath {
			return false
		}
	}
	return true
}
