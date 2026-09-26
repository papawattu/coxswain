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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// I18: Workspace.Repo must reject empty/invalid strings and accept HTTPS,
// SSH, and scp-style remotes. The CRD pattern does the work; this test proves
// the admission behaviour end to end in envtest.
var _ = Describe("Workspace.Repo validation", func() {
	newNS := func() (context.Context, context.CancelFunc, string) {
		ctx, cancel := context.WithCancel(context.Background())
		ns := "repo-test-" + fmt.Sprint(time.Now().UnixNano())
		Expect(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
		return ctx, cancel, ns
	}
	teardown := func(cancel context.CancelFunc, ns string) {
		_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		cancel()
	}

	It("rejects an empty repo", func() {
		ctx, cancel, ns := newNS()
		defer teardown(cancel, ns)
		u := mkUnstructuredLoop("empty-repo", ns, "")
		Expect(k8sClient.Create(ctx, u)).ToNot(Succeed(),
			"an empty repo must be rejected by the CRD (MinLength=1)")
	})

	It("rejects a non-URL repo", func() {
		ctx, cancel, ns := newNS()
		defer teardown(cancel, ns)
		u := mkUnstructuredLoop("bad-repo", ns, "not a url")
		Expect(k8sClient.Create(ctx, u)).ToNot(Succeed(),
			"a non-URL repo must be rejected by the CRD pattern")
	})

	It("accepts an HTTPS repo", func() {
		ctx, cancel, ns := newNS()
		defer teardown(cancel, ns)
		u := mkUnstructuredLoop("https-repo", ns, loopRepo)
		Expect(k8sClient.Create(ctx, u)).To(Succeed(),
			"an HTTPS repo must be accepted")
		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(u.GroupVersionKind())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "https-repo"}, got)).
			To(Succeed(), "the accepted Loop should exist")
	})

	It("accepts a scp-style SSH repo", func() {
		ctx, cancel, ns := newNS()
		defer teardown(cancel, ns)
		u := mkUnstructuredLoop("ssh-repo", ns, "git@github.com:papawattu/pixme.git")
		Expect(k8sClient.Create(ctx, u)).To(Succeed(),
			"a scp-style SSH repo must be accepted")
	})
})
