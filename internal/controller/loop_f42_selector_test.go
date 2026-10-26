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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// F42 review note 2: the agent netpol only fences the pod it selects. For a
// Loop with no endpoint, the operator-rendered sandbox pod template must carry
// every label in the agent netpol's podSelector (a selector/label mismatch is
// exactly the unfenced-agent defect class).
var _ = Describe("F42: the agent netpol selects the rendered sandbox pod", func() {
	It("the sandbox pod template labels contain the agent netpol podSelector", func() {
		ns := "f42-selector-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "noendpoint"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       coxv1alpha1.LoopSpec{Goal: loopGoal, Workspace: testWorkspace()},
		})).To(Succeed())
		r := &LoopReconciler{CNIProber: testCNIProber(), Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())

		np := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-agent-netpol", Namespace: ns}, np)).To(Succeed())
		sb := &sandboxv1beta1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-sandbox", Namespace: ns}, sb)).To(Succeed())

		Expect(np.Spec.PodSelector.MatchLabels).NotTo(BeEmpty())
		for k, v := range np.Spec.PodSelector.MatchLabels {
			Expect(sb.Spec.PodTemplate.ObjectMeta.Labels).To(HaveKeyWithValue(k, v),
				"the agent netpol selector %s=%s must match the sandbox pod template", k, v)
		}
	})
})
