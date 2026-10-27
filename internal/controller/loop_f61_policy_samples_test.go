/*
Copyright 2026.

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
	"os"
	"path"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// F61 (R31 row 21): ADR-0007 says reference AgentPolicies ship under
// config/samples/policies/ and that the sample must ship with a minimal
// AgentPolicy. These specs hold the shipped files to that promise: they parse
// strictly into the Go type, the APISERVER admits them (the CRD's markers and
// CEL rules are the real validation — a parse-only check would pass an
// in-cluster network target the CRD rejects), every file is listed in the
// samples kustomization (and every listed policy exists), and default-deny's
// own "no allows" claim is true.
var _ = Describe("F61: the shipped sample AgentPolicies", Ordered, func() {
	const policiesDir = "config/samples/policies"
	const namespace = "f61-policy-samples"

	repoPath := func(parts ...string) string {
		return filepath.Join(append([]string{"..", ".."}, parts...)...)
	}

	// policyFiles lists the shipped policy documents (the test validates every
	// file present, so a future third reference policy is covered).
	policyFiles := func() []string {
		entries, err := os.ReadDir(repoPath(policiesDir))
		Expect(err).NotTo(HaveOccurred())
		var files []string
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
				continue
			}
			files = append(files, filepath.Join(repoPath(policiesDir), entry.Name()))
		}
		return files
	}

	BeforeAll(func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		if err := k8sClient.Create(ctx, ns); err != nil {
			Expect(apierrors.IsAlreadyExists(err)).To(BeTrue(), "creating the %s namespace: %v", namespace, err)
		}
	})

	AfterAll(func() {
		// The namespace (and with it the policies) is test-owned; remove it so
		// the specs leave nothing behind (IgnoreNotFound for a re-run).
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, ns))).To(Succeed())
	})

	It("admits every shipped policy (strict parse + apiserver validation)", func() {
		files := policyFiles()
		Expect(files).NotTo(BeEmpty(), "no reference policies under %s", repoPath(policiesDir))

		names := make([]string, 0, len(files))
		for _, file := range files {
			names = append(names, filepath.Base(file))
		}
		Expect(names).To(ContainElements("default-deny.yaml", "go-toolchain.yaml"),
			"the two reference policies ADR-0007 promises must ship")

		for _, file := range files {
			data, err := os.ReadFile(file)
			Expect(err).NotTo(HaveOccurred())

			policy := &coxv1alpha1.AgentPolicy{}
			// Strict: a typo'd field is an error here, not silently dropped.
			Expect(yaml.UnmarshalStrict(data, policy)).To(Succeed(), "%s", file)
			Expect(policy.Kind).To(Equal("AgentPolicy"), "%s", file)
			Expect(policy.APIVersion).To(Equal(coxv1alpha1.GroupVersion.String()), "%s", file)

			base := strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
			Expect(policy.Name).To(Equal(base), "%s: the policy name should match the file name", file)
			policy.Namespace = namespace

			// Create is the validation: the apiserver enforces the CRD markers
			// and the CEL rules (e.g. no .svc / localhost network targets).
			Expect(k8sClient.Create(ctx, policy)).To(Succeed(), "%s must be admitted by the AgentPolicy CRD", file)

			stored := &coxv1alpha1.AgentPolicy{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: policy.Name}, stored)).To(Succeed(), "%s", file)
			Expect(stored.Spec).To(Equal(policy.Spec), "%s: the stored spec must round-trip", file)
		}
	})

	It("lists every shipped policy in the samples kustomization, and every listed policy exists", func() {
		data, err := os.ReadFile(repoPath("config", "samples", "kustomization.yaml"))
		Expect(err).NotTo(HaveOccurred())

		var kustomization struct {
			Resources []string `yaml:"resources"`
		}
		Expect(yaml.UnmarshalStrict(data, &kustomization)).To(Succeed())

		var listed []string
		for _, resource := range kustomization.Resources {
			if strings.HasPrefix(resource, "policies/") {
				listed = append(listed, resource)
			}
		}

		// Direction 1: every shipped policy file is listed. A file that is not
		// in the kustomization is invisible to `kubectl apply -k
		// config/samples`, which is how the docs tell users to apply them.
		files := policyFiles()
		shipped := make([]string, 0, len(files))
		for _, file := range files {
			// path.Join, not filepath.Join: `listed` holds the kustomization's
			// own strings, which are always slash-separated. filepath.Join would
			// build a back-slash path on a Windows host and mismatch for a
			// reason that has nothing to do with the assertion.
			shipped = append(shipped, path.Join("policies", filepath.Base(file)))
		}
		Expect(listed).To(ConsistOf(shipped),
			"the kustomization's policies/ entries must be exactly the shipped policy files")

		// Direction 2: every listed policy path exists (no dangling entry that
		// would make `kubectl apply -k` fail).
		for _, resource := range listed {
			_, err := os.Stat(repoPath("config", "samples", resource))
			Expect(err).NotTo(HaveOccurred(), "%s is listed in the kustomization but does not exist", resource)
		}
	})

	It("keeps default-deny's no-allows claim true", func() {
		data, err := os.ReadFile(repoPath(policiesDir, "default-deny.yaml"))
		Expect(err).NotTo(HaveOccurred())

		policy := &coxv1alpha1.AgentPolicy{}
		Expect(yaml.UnmarshalStrict(data, policy)).To(Succeed())
		Expect(policy.Spec.Exec).To(BeEmpty(), "default-deny must allow no exec")
		Expect(policy.Spec.Network).To(BeEmpty(), "default-deny must allow no network")
		Expect(policy.Spec.Files).To(BeEmpty(), "default-deny must allow no files")
		Expect(policy.Spec.Tools).To(BeEmpty(), "default-deny must allow no tools")
	})
})
