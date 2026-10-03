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

	// defaultNamespace / baseBranch are the default namespace the unit
	// fixtures target and the base-branch name (the push target). goconst:
	// each appears 3+ times across the controller test files.
	defaultNamespace = "default"
	baseBranch       = "main"

	// corePodsResource is the core Pods RBAC resource name (goconst).
	corePodsResource = "pods"
)

// s6SampleLoopObjMeta is the shared fixture ObjectMeta for a unit-level S6
// Loop (name "s6loop" in the "default" namespace).
func s6SampleLoopObjMeta() metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: "s6loop", Namespace: defaultNamespace}
}
