/*
Copyright 2026 papawattu.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	// +kubebuilder:scaffold:imports
)

// These tests use Ginkgo (BDD-style Go testing framework). Refer to
// http://onsi.github.io/ginkgo/ to learn more about Ginkgo.

var (
	ctx       context.Context
	cancel    context.CancelFunc
	testEnv   *envtest.Environment
	cfg       *rest.Config
	k8sClient client.Client
	// loopCache is a cache scoped to Loops only, with the spec.policyRefs
	// field index registered. Used by the agentPolicyToLoopRequests test to
	// exercise the indexed path (R15 OK-notes).
	loopCache cache.Cache
)

func TestControllers(t *testing.T) {
	RegisterFailHandler(Fail)

	RunSpecs(t, "Controller Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	ctx, cancel = context.WithCancel(context.TODO())

	var err error
	err = coxv1alpha1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	// The Loop controller creates agent-sandbox Sandboxes; the scheme must know
	// the type for CreateOrUpdate and status assertions to work.
	err = sandboxv1beta1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	// +kubebuilder:scaffold:scheme

	// I45: the envtest apiserver must enforce ValidatingAdmissionPolicies for
	// the I45 spec (pods/ephemeralcontainers denial on coxswain component
	// pods). The feature gate is required on the apiserver side (the Go API
	// type ships in k8s 1.34, but the envtest apiserver does not enable the
	// gate by default). Enabling it here is the "gate" the I43 norm refers
	// to: remove ValidatingAdmissionPolicy=true from this list and the I45
	// spec fails (the update is allowed for all pods).
	By("bootstrapping test environment")
	testEnv = &envtest.Environment{
		// The Loop CRD plus the agent-sandbox Sandbox CRD (vendored under
		// config/crd/external). The controller creates Sandboxes, so envtest
		// must have that type registered to accept them.
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "config", "crd", "bases"),
			filepath.Join("..", "..", "config", "crd", "external"),
		},
		ErrorIfCRDPathMissing: true,
		// I45: explicitly enable the ValidatingAdmissionPolicy admission
		// plugin on the envtest apiserver. On Kubernetes 1.34 the plugin is in
		// the default-enabled list, but stating it explicitly makes the test
		// self-documenting: if a future k8s version removes it from defaults
		// (or the plugin is disabled another way) this line is where to look.
		ControlPlane: envtest.ControlPlane{
			APIServer: &envtest.APIServer{
				Args: []string{
					"--enable-admission-plugins=ValidatingAdmissionPolicy",
				},
			},
		},
	}

	// Retrieve the first found binary directory to allow running tests from IDEs
	if getFirstFoundEnvTestBinaryDir() != "" {
		testEnv.BinaryAssetsDirectory = getFirstFoundEnvTestBinaryDir()
	}

	// cfg is defined in this file globally.
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())

	// Set up a cache scoped to Loops only, with the spec.policyRefs field
	// index registered (R15 OK-notes: test the indexed path). This cache is
	// used ONLY by the agentPolicyToLoopRequests test; the k8sClient remains
	// a plain client for all other specs.
	loopCacheOpts := cache.Options{
		Scheme: scheme.Scheme,
		ByObject: map[client.Object]cache.ByObject{
			&coxv1alpha1.Loop{}: {},
		},
	}
	loopCache, err = cache.New(cfg, loopCacheOpts)
	Expect(err).NotTo(HaveOccurred())
	Expect(loopCache.IndexField(ctx, &coxv1alpha1.Loop{}, loopPolicyRefsFieldIndex,
		func(obj client.Object) []string {
			loop, ok := obj.(*coxv1alpha1.Loop)
			if !ok {
				return nil
			}
			return loop.Spec.PolicyRefs
		})).To(Succeed())
	go func() {
		_ = loopCache.Start(ctx)
	}()
	Eventually(func() bool {
		return loopCache.WaitForCacheSync(ctx)
	}, "10s").Should(BeTrue())
})

var _ = AfterSuite(func() {
	By("tearing down the test environment")
	cancel()
	err := testEnv.Stop()
	Expect(err).NotTo(HaveOccurred())
})

// getFirstFoundEnvTestBinaryDir locates the first binary in the specified path.
// ENVTEST-based tests depend on specific binaries, usually located in paths set by
// controller-runtime. When running tests directly (e.g., via an IDE) without using
// Makefile targets, the 'BinaryAssetsDirectory' must be explicitly configured.
//
// This function streamlines the process by finding the required binaries, similar to
// setting the 'KUBEBUILDER_ASSETS' environment variable. To ensure the binaries are
// properly set up, run 'make setup-envtest' beforehand.
func getFirstFoundEnvTestBinaryDir() string {
	basePath := filepath.Join("..", "..", "bin", "k8s")
	entries, err := os.ReadDir(basePath)
	if err != nil {
		logf.Log.Error(err, "Failed to read directory", "path", basePath)
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(basePath, entry.Name())
		}
	}
	return ""
}
