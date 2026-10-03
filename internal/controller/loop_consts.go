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
)
