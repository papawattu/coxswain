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

// A4 (PLAN-ALPHA.md A4): the deliver description ConfigMap's envtest specs.
// The operator ensures a deliver description ConfigMap (<loop>-deliver-
// description) carrying the FACTS body (the operator built it from the Loop's
// spec/status only) while the deliver Job is expected, and cleans it up when
// delivery is no longer expected (the mirror of the deliver netpol lifecycle,
// I42c-style: a foreign ConfigMap of the same name is left alone). The push
// container is the ONLY container that mounts it (read-only, optional).
//
// These specs assert:
//  1. the ConfigMap exists for a Succeeded + PullRequest Loop, is owned by
//     the Loop, carries the description.md key with the operator's facts
//     body (rendered from spec/status), and is updated (not just created)
//     when the facts change (re-reconcile with a changed status: the body
//     reflects the new facts);
//  2. the push container mounts the ConfigMap volume read-only (and the
//     clone-base / import-agent containers do NOT — the description is
//     assembled in the push container's PR payload only);
//  3. the ConfigMap is cleaned up when delivery is no longer expected
//     (a foreign ConfigMap of the same name is left alone).
package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

var _ = Describe("A4: the deliver description ConfigMap (envtest)", func() {
	ctx := context.Background()

	freshNS := func(prefix string) string {
		ns := prefix + "-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		return ns
	}

	It("ensures the ConfigMap for a Succeeded + PullRequest Loop (the facts body, owned by the Loop)", func() {
		ns := freshNS("a4-desc")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "desc1"
		// An IN-CLUSTER Gitea repo (the d41/p2e pattern): the deliver Job takes
		// the direct repo-peer path (no egress proxy gate), so the deliver Job
		// and the deliver description ConfigMap are both ensured on the
		// Succeeded reconcile (s6Succeeded drives the Loop to Succeeded with
		// the pinned verifiedCommit).
		r := s6Succeeded(name, ns, "http://gitea.samples.svc:3000/samples/desc1.git")
		s6Reconcile(r, ns, name)

		cmName := deliverDescriptionConfigName(name)
		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: cmName}, cm)).To(Succeed(),
			"the deliver description ConfigMap must exist for a Succeeded + PullRequest Loop")
		loopOwner := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, loopOwner)).To(Succeed())
		Expect(metav1.IsControlledBy(cm, loopOwner)).To(BeTrue(), "the ConfigMap must be owned by the Loop")
		Expect(cm.Data).To(HaveKey(deliverDescriptionDataKey), "the ConfigMap must carry the description.md key")

		// The body is the operator's facts, rendered from the Loop's
		// spec/status (the facts section is the operator's text).
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, loop)).To(Succeed())
		wantBody := renderDeliverDescriptionBody(deliverDescriptionForLoop(loop))
		Expect(cm.Data[deliverDescriptionDataKey]).To(Equal(wantBody),
			"the ConfigMap's description.md must be the operator's facts body (rendered from the Loop's spec/status)")
		// The body carries the facts (the operator's text), and the push
		// container's plan section (the untrusted agent plan) is NOT in the
		// ConfigMap (the plan is fenced in by the push script, from the agent
		// workspace — never carried in the operator's ConfigMap).
		Expect(wantBody).To(ContainSubstring("# coxswain delivery"))
		Expect(cm.Data[deliverDescriptionDataKey]).ToNot(ContainSubstring("## Plan (written by the agent, untrusted)"),
			"the operator's ConfigMap must not carry the untrusted agent plan (the plan is fenced in by the push script)")
	})

	It("updates the ConfigMap when the facts change (a re-reconcile with a changed status)", func() {
		ns := freshNS("a4-desc-upd")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "desc2"
		r := s6Succeeded(name, ns, "http://gitea.samples.svc:3000/samples/desc2.git")
		s6Reconcile(r, ns, name)

		cmName := deliverDescriptionConfigName(name)
		// Change a fact: bump the iteration count (status.iteration) and
		// re-reconcile. The body must reflect the new iteration count (the
		// ConfigMap is UPDATED, not just created — the I42c-style same-Loop
		// update: the object is updated when the input changes).
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, loop)).To(Succeed())
		loop.Status.Iteration++
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		r2 := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
		s6Reconcile(r2, ns, name)

		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: cmName}, cm)).To(Succeed())
		updated := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, updated)).To(Succeed())
		wantBody := renderDeliverDescriptionBody(deliverDescriptionForLoop(updated))
		Expect(cm.Data[deliverDescriptionDataKey]).To(Equal(wantBody),
			"the ConfigMap's description.md must be UPDATED when the facts change (the iteration count)")
		Expect(wantBody).To(ContainSubstring("- **Iterations:** "+""), "the body must name the iteration count")
	})

	It("mounts the ConfigMap into the push container only (read-only, optional)", func() {
		ns := freshNS("a4-desc-mount")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "desc3"
		r := s6Succeeded(name, ns, "http://gitea.samples.svc:3000/samples/desc3.git")
		s6Reconcile(r, ns, name)
		job := s6GetJob(ns, name)
		inits, push := s6JobContainers(job)

		// The push container mounts the deliver description volume read-only.
		pushMounted := false
		for i := range push.VolumeMounts {
			if push.VolumeMounts[i].Name == deliverDescriptionVol {
				pushMounted = true
				Expect(push.VolumeMounts[i].ReadOnly).To(BeTrue(), "the push container must mount the deliver description ConfigMap READ-ONLY")
			}
		}
		Expect(pushMounted).To(BeTrue(), "the push container must mount the deliver description ConfigMap volume (it assembles the PR payload from the facts body)")

		// The clone-base and import-agent containers do NOT mount it (the
		// description is assembled in the push container's PR payload only).
		for _, c := range []corev1.Container{inits[deliverCloneBase], inits[deliverImport]} {
			for i := range c.VolumeMounts {
				Expect(c.VolumeMounts[i].Name).ToNot(Equal(deliverDescriptionVol),
					"the %s container must NOT mount the deliver description ConfigMap (the description is the push container's)", c.Name)
			}
		}

		// The volume is optional (a transient ConfigMap read failure must not
		// block the Job; the push script falls back to the minimal line).
		var optionalSet bool
		for _, v := range job.Spec.Template.Spec.Volumes {
			if v.Name == deliverDescriptionVol {
				Expect(v.ConfigMap).ToNot(BeNil(), "the deliver description volume must be a ConfigMap volume")
				Expect(v.ConfigMap.Optional).ToNot(BeNil(), "the deliver description volume must be OPTIONAL (a missing ConfigMap must not block the Job)")
				Expect(*v.ConfigMap.Optional).To(BeTrue())
				optionalSet = true
			}
		}
		Expect(optionalSet).To(BeTrue(), "the deliver Job must declare the deliver description volume")
	})

	It("cleans up the ConfigMap when delivery is no longer expected (a foreign one is left alone)", func() {
		ns := freshNS("a4-desc-cleanup")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "desc4"
		r := s6Succeeded(name, ns, "http://gitea.samples.svc:3000/samples/desc4.git")
		s6Reconcile(r, ns, name)
		cmName := deliverDescriptionConfigName(name)
		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: cmName}, cm)).To(Succeed(), "the ConfigMap must exist while delivery is expected")

		// A foreign ConfigMap of the same name (not owned by the Loop): the
		// operator's cleanup must leave it alone (the I42c-style foreign-
		// object guard — the operator only deletes objects it owns).
		foreign := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "foreign-" + cmName, Namespace: ns, Labels: map[string]string{"app.kubernetes.io/managed-by": "foreign"}},
			Data:       map[string]string{"description.md": "not the operator's"},
		}
		Expect(k8sClient.Create(ctx, foreign)).To(Succeed())

		// The foreign ConfigMap (a different name) must still exist after the
		// operator's reconcile (the operator only deletes its OWN ConfigMap).
		r2 := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
		s6Reconcile(r2, ns, name)
		still := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: foreign.Name}, still)).To(Succeed(),
			"a foreign ConfigMap must not be deleted by the operator's cleanup")

		// Now make delivery NOT expected (flip the delivery mode off) and
		// re-reconcile: the operator's OWN ConfigMap is cleaned up, but the
		// foreign one is left alone.
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, loop)).To(Succeed())
		mode := coxv1alpha1.DeliveryModeNone
		loop.Spec.Delivery = &coxv1alpha1.DeliveryConfig{Mode: mode}
		Expect(k8sClient.Update(ctx, loop)).To(Succeed())
		r3 := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
		s6Reconcile(r3, ns, name)

		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: cmName}, &corev1.ConfigMap{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the operator's deliver description ConfigMap must be deleted when delivery is no longer expected")
		// The foreign ConfigMap is still there (the operator left it alone).
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: foreign.Name}, &corev1.ConfigMap{})).To(Succeed(),
			"the foreign ConfigMap must be left alone by the operator's cleanup")
	})
})
