// Copyright 2026 papawattu.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

// I224 (R49): spec.files entries must be canonical absolute paths. An allow
// is a grant, so the CRD CEL rule (api/v1alpha1/agentpolicy_types.go) rejects
// relative paths, spaces, "//", and "." / ".." segments, while keeping a
// trailing "/" allowed (D113: it means the directory, recursively). These
// specs assert the CRD's admission-time XValidation on AgentPolicy and
// ClusterAgentPolicy.

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

const i224RejectedFilesMessage = "files entries must be absolute canonical paths"

var (
	i224RejectedFiles = []string{"relative", "/a/../b", "/a//b", "/a/./b", "/a b"}
	i224AcceptedFiles = []string{"/etc/hosts", "/etc/ssl/certs/", "/workspace"}
)

var _ = Describe("I224: AgentPolicy files entries must be canonical absolute paths", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("rejects a namespaced AgentPolicy create with a non-canonical files entry and accepts the canonical ones", func() {
		ns := "i224-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) })

		for _, files := range i224RejectedFiles {
			pol := &coxv1alpha1.AgentPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "i224-rejected-" + nowSuffix(), Namespace: ns},
				Spec:       coxv1alpha1.AgentPolicySpec{Files: []string{files}},
			}
			err := k8sClient.Create(ctx, pol)
			Expect(err).To(HaveOccurred(), "create with files %q must be rejected", files)
			Expect(err.Error()).To(ContainSubstring(i224RejectedFilesMessage),
				"the rejection error must carry the CRD rule's message, got: %v", err)
		}

		pol := &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "i224-accepted", Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Files: i224AcceptedFiles},
		}
		Expect(k8sClient.Create(ctx, pol)).To(Succeed(),
			"canonical absolute paths (including a trailing /, D113) must be accepted")
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, pol) })
	})

	It("rejects a ClusterAgentPolicy create with a non-canonical files entry and accepts the canonical ones", func() {
		for _, files := range i224RejectedFiles {
			pol := &coxv1alpha1.ClusterAgentPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "i224-rejected-" + nowSuffix()},
				Spec:       coxv1alpha1.AgentPolicySpec{Files: []string{files}},
			}
			err := k8sClient.Create(ctx, pol)
			Expect(err).To(HaveOccurred(), "create with files %q must be rejected", files)
			Expect(err.Error()).To(ContainSubstring(i224RejectedFilesMessage),
				"the rejection error must carry the CRD rule's message, got: %v", err)
		}

		pol := &coxv1alpha1.ClusterAgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "i224-accepted"},
			Spec:       coxv1alpha1.AgentPolicySpec{Files: i224AcceptedFiles},
		}
		Expect(k8sClient.Create(ctx, pol)).To(Succeed(),
			"canonical absolute paths (including a trailing /, D113) must be accepted")
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, pol) })
	})
})
