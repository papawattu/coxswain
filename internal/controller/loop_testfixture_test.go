package controller

import coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"

// testRepoURL is the fixture workspace repo used across the controller tests.
// It's a placeholder that the tests never actually clone (envtest has no
// git), so one shared constant avoids goconst churn.
const testRepoURL = "https://example.com/repo.git"

// testWorkspace returns a LoopSpec workspace pointing at the shared fixture
// repo, so the individual test files don't each repeat the literal.
func testWorkspace() coxv1alpha1.Workspace {
	return coxv1alpha1.Workspace{Repo: testRepoURL}
}
