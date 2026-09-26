/*
Copyright 2026 papawattu.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an " IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

var _ = Describe("Loop Controller", func() {
	Context("When reconciling a resource", func() {
		const (
			resourceName      = "test-resource"
			resourceNamespace = "default"
		)

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: resourceNamespace,
		}
		loop := &coxv1alpha1.Loop{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind Loop")
			err := k8sClient.Get(ctx, typeNamespacedName, loop)
			if err != nil && errors.IsNotFound(err) {
				resource := &coxv1alpha1.Loop{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: resourceNamespace,
					},
					Spec: coxv1alpha1.LoopSpec{
						Goal: "make the failing test pass",
						Workspace: coxv1alpha1.Workspace{
							Repo: "https://github.com/papawattu/coxswain.git",
							Ref:  "main",
						},
						Verify: coxv1alpha1.VerifyConfig{
							AcceptanceChecks: []string{"go test ./..."},
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &coxv1alpha1.Loop{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			if err == nil {
				By("Cleaning up the specific resource instance Loop")
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}
		})

		It("creates a Sandbox owned by the Loop and sets phase to Pending", func() {
			By("Reconciling the created resource")
			controllerReconciler := &LoopReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("checking the Loop got a Sandbox")
			sandbox := &sandboxv1beta1.Sandbox{}
			sandboxName := types.NamespacedName{
				Name:      resourceName + "-sandbox",
				Namespace: resourceNamespace,
			}
			Expect(k8sClient.Get(ctx, sandboxName, sandbox)).To(Succeed())
			Expect(sandbox.OwnerReferences).To(HaveLen(1))
			Expect(sandbox.OwnerReferences[0].Name).To(Equal(resourceName))

			By("checking the Loop phase advanced to Pending")
			Expect(k8sClient.Get(ctx, typeNamespacedName, loop)).To(Succeed())
			Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePending))
			Expect(loop.Status.ObservedGeneration).To(Equal(loop.Generation))

			By("reconciling again is idempotent (no second sandbox)")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			var sandboxes sandboxv1beta1.SandboxList
			Expect(k8sClient.List(ctx, &sandboxes,
				client.InNamespace(resourceNamespace))).To(Succeed())
			Expect(sandboxes.Items).To(HaveLen(1))
		})
	})
})
