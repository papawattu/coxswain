package controller

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Shared test fixture literals (goconst): the in-cluster Gitea repo URL, the
// sample-app Git/model credential secret names, and the default namespace the
// unit fixtures target. Defined once here so the repeated literals across the
// controller test files (S3/S4/S6/D34/etc.) reference a single constant and
// goconst's min-occurrences threshold is not tripped.
const (
	// inClusterRepoURL is the in-cluster Gitea sample-app repo (S3/S6/D34...).
	inClusterRepoURL = "http://gitea.samples.svc:3000/samples/gocli.git"

	// samplesGitCredSecret / samplesModelCredSecret are the sample-app
	// Git-credential and model-endpoint credential secret names.
	samplesGitCredSecret   = "samples-git-cred"
	samplesModelCredSecret = "samples-model-cred"

	// nsSamples is the namespace the in-cluster Gitea sample repos live in
	// (the .svc Service's namespace for the namespaceSelector repo-peer rule).
	nsSamples = "samples"

	// githubRepoURL is the GitHub sample-app repo URL (S6 tests: the
	// github.com delivery provider + the fake-GitHub API specs).
	githubRepoURL = "https://github.com/samples/gocli.git"

	// defaultNamespace / baseBranch are the default namespace the unit
	// fixtures target and the base-branch name (the push target). goconst:
	// each appears 3+ times across the controller test files.
	defaultNamespace = "default"
	baseBranch       = "main"

	// testCredSecretName is the git-credential Secret name the deliver
	// script test fixture references (goconst: 3+ occurrences).
	testCredSecretName = "test-cred"

	// corePodsResource is the core Pods RBAC resource name (goconst).
	corePodsResource = "pods"

	// testGiteaExampleRepoURL is the Gitea-compatible repo URL the push-script
	// execution tests target (goconst: 3+ occurrences across the test file).
	testGiteaExampleRepoURL = "http://gitea.example:3000/samples/gocli.git"

	// testInitialBranch is the base-branch name the push-script execution
	// tests use for the agent repo (goconst: 3+ occurrences).
	testInitialBranch = "initial"
)

// s6SampleLoopObjMeta is the shared fixture ObjectMeta for a unit-level S6
// Loop (name "s6loop" in the "default" namespace).
func s6SampleLoopObjMeta() metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: "s6loop", Namespace: defaultNamespace}
}
