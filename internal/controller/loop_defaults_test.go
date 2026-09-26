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

// P3: a YAML Loop that omits the whole `loop:` block must still get the
// maxIterations default (10) from the CRD. Without +kubebuilder:default={}.
// on LoopSpec.Loop the CRD has no default to apply and the field stays absent.
// (Go clients happen to send loop: {} and mask this gap.)
var _ = Describe("Loop maxIterations default", func() {
	var ns string
	var ctx context.Context
	var cancel context.CancelFunc

	BeforeEach(func() {
		var c context.CancelFunc
		ctx, c = context.WithCancel(context.Background())
		cancel = c
		ns = "defaults-test-" + fmt.Sprint(time.Now().UnixNano())
		Expect(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
	})

	AfterEach(func() {
		// I19: delete the namespace with a live context, then cancel — the old
		// order cancelled first, so the delete ran on a cancelled context and
		// failed silently. Use context.Background() so cleanup is robust.
		_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		if cancel != nil {
			cancel()
		}
	})

	It("applies the maxIterations default when the loop block is omitted", func() {
		// Create the Loop via unstructured, omitting the whole loop: block, so
		// the CRD's defaulting is what must supply maxIterations.
		u := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "coxswain.wattu.com/v1alpha1",
			"kind":       "Loop",
			"metadata":   map[string]any{"name": "no-loop-block", "namespace": ns},
			"spec": map[string]any{
				"goal": "make the failing test pass",
				"workspace": map[string]any{
					"repo": testRepoURL,
					"ref":  "main",
				},
				"verify": map[string]any{
					"acceptanceChecks": []any{"go test ./..."},
				},
				// no "loop" key
			},
		}}
		Expect(k8sClient.Create(ctx, u)).To(Succeed())

		// I19: CRD defaulting is applied synchronously at create, so a plain Get
		// + Expect is enough — no Eventually needed. (The Get target needs its
		// GVK set, or the client refuses the Get with "Kind is missing".)
		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(u.GroupVersionKind())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "no-loop-block"}, got)).To(Succeed())
		v, found, err := unstructured.NestedInt64(got.Object, "spec", "loop", "maxIterations")
		Expect(err).ToNot(HaveOccurred())
		Expect(found).To(BeTrue(), "spec.loop.maxIterations must be present after defaulting")
		Expect(v).To(Equal(int64(10)), "the CRD must default spec.loop.maxIterations to 10 when the loop block is omitted")
	})
})
