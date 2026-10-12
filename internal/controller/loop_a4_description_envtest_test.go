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
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

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

	It("carries the agent plan into the scratch volume via import-agent (outside the git tree; the push reads it from there)", func() {
		ns := freshNS("a4-desc-plan")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "desc7"
		r := s6Succeeded(name, ns, "http://gitea.samples.svc:3000/samples/desc7.git")
		s6Reconcile(r, ns, name)
		job := s6GetJob(ns, name)
		inits, push := s6JobContainers(job)

		// import-agent (the container that mounts the agent PVC read-only and
		// the scratch volume writable) is the ONLY container that carries the
		// plan: it copies ${SRC}/.coxswain/PLAN.md into the scratch volume
		// OUTSIDE the git tree (${DEST}/plan.md). The copy is regular-file-only
		// and never follows a symlink (a symlinked PLAN.md is agent-controlled
		// and could point anywhere on the PVC), and is capped at read time
		// (head -c to the copy cap — the plan cap plus slack).
		importScript := deliverContainerScript(GinkgoT(), inits[deliverImport])
		Expect(importScript).To(ContainSubstring("PLAN_SRC=${SRC}/.coxswain/PLAN.md"),
			"import-agent must name the agent plan on the workspace PVC")
		Expect(importScript).To(ContainSubstring("PLAN_DST=${DEST}/plan.md"),
			"import-agent must carry the plan into the scratch volume OUTSIDE the git tree (${DEST}/plan.md)")
		Expect(importScript).To(ContainSubstring("if [ -f \"${PLAN_SRC}\" ] && [ ! -L \"${PLAN_SRC}\" ]"),
			"import-agent must copy the plan only if it is a regular file and NOT a symlink (agent-controlled; never follow links)")
		Expect(importScript).To(ContainSubstring("head -c \"${PLAN_MAX}\" \"${PLAN_SRC}\" > \"${PLAN_DST}\""),
			"import-agent must cap the plan at read time (head -c)")
		Expect(importScript).To(ContainSubstring(fmt.Sprintf("PLAN_MAX=%d", deliverPlanCopyMaxBytes)),
			"import-agent's copy cap must be the plan cap plus slack (the binding display cap stays the python's PLAN_MAX)")

		// The push container reads the plan from the scratch-volume copy (the
		// sibling of the checkout, ${SRC}/plan.md) — NEVER from the checkout
		// itself: .coxswain is excluded from the agent commit (I80/D51), so
		// the file can never exist in the pinned checkout, and the push
		// container does not mount the agent PVC.
		pushScript := deliverContainerScript(GinkgoT(), push)
		Expect(pushScript).To(ContainSubstring("PLAN_FILE=${SRC}/plan.md"),
			"the push script must read the plan from the scratch-volume copy import-agent carried (${SRC}/plan.md)")
		Expect(pushScript).NotTo(ContainSubstring("PLAN_FILE=${SRC}/.coxswain/PLAN.md"),
			"the push script's PLAN_FILE must not point at the scratch checkout (.coxswain is excluded from the agent commit and the push container has no agent PVC mount)")
	})

	// R27/P1 (the new ordering): a NON-forbidden ensure failure of the
	// description ConfigMap must NOT error the Succeeded reconcile (the
	// reviewer's P2: "a transient read failure must not block the Job" — the
	// reconcile completes and requeues), and the deliver Job must NOT be
	// created while the ConfigMap is missing (the R27/P1 race fix): the
	// push container mounts the volume OPTIONAL and falls back to the
	// minimal line when the ConfigMap is absent, so a deliver Job created
	// before the ConfigMap exists silently degrades to the minimal
	// description. A non-forbidden ensure failure means the description
	// cannot be carried into the PR body this pass, so the Job is HELD
	// (requeued) until the ensure succeeds (the next reconcile retries).
	It("holds the deliver Job (no Job, requeue) when the description ConfigMap ensure fails (non-Forbidden) and the ConfigMap is missing", func() {
		ns := freshNS("a4-desc-nocm")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "desc5"
		// The Succeeded reconcile must be driven through the WRAPPED client
		// (the description ConfigMap's CREATE returns a NON-forbidden error —
		// a transient API failure, not a Forbidden error) so the Succeeded
		// reconcile HELDS the deliver Job (the ConfigMap is missing) and the
		// Job is never created. A plain s6Succeeded would use the real client
		// (the Succeeded reconcile would ensure the ConfigMap first, then
		// create the Job — the normal path) and the Job would already exist
		// before the spec can assert it is missing.
		wrapped := &deliverCMEnsureFails{Client: k8sClient, err: transientAPIError}
		r := &LoopReconciler{Client: wrapped, Scheme: k8sClient.Scheme(), apiReader: k8sClient, Recorder: record.NewFakeRecorder(64)}
		s5aEnsureSandbox(ns, name)
		loop := s6Loop(name, ns, "http://gitea.samples.svc:3000/samples/desc5.git")
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		s6Reconcile(r, ns, name) // Pending -> Planning
		loop = &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, loop)).To(Succeed())
		loop.Status.BaseCommit = s5aBaseCommit
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		s5aClaimPod(ns, name, "Planning", "")
		s6Reconcile(r, ns, name)
		s5aEnsureSandbox(ns, name)
		s5aClaimPod(ns, name, "Implementing", s6HeadCommit)
		s6Reconcile(r, ns, name)
		s5aVerifyPod(ctx, ns, name, name+"-verify-1", 0)
		s6Reconcile(r, ns, name) // Verifying -> Succeeded: the wrapped client's
		// Create on the deliver description ConfigMap returns a NON-forbidden
		// error, so the deliver Job is HELD (the ConfigMap is missing) and
		// never created (the R27/P1 ordering: the Job is created only after
		// the ConfigMap exists).
		cmName := deliverDescriptionConfigName(name)
		// The ConfigMap is missing on the Succeeded reconcile (the wrapped
		// client's Create returned a non-forbidden error; the Job is held).
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: cmName}, &corev1.ConfigMap{})).ToNot(Succeed(),
			"the description ConfigMap must be missing on the Succeeded reconcile (the wrapped client's Create returned a non-forbidden error)")
		// The deliver Job was NOT created: the Succeeded reconcile HELD it
		// (the ConfigMap is missing) and never created it (the R27/P1
		// ordering: the Job is created only after the ConfigMap exists).
		jobName := deliverJobName(name)
		jobErr := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: jobName}, &batchv1.Job{})
		Expect(apierrors.IsNotFound(jobErr)).To(BeTrue(), "the deliver Job must NOT be created on the Succeeded reconcile when the description ConfigMap is missing (the R27/P1 ordering: the Job is held, not created, while the ConfigMap is missing)")
	})

	// R27/P1: the deliver Job must be created only AFTER the description
	// ConfigMap exists on the NORMAL (non-forbidden) path: a Succeeded
	// reconcile with a healthy client ensures the ConfigMap first (Create/
	// Get succeeded), then creates the Job. The spec asserts the ConfigMap
	// exists AND the Job exists after the Succeeded reconcile, and that the
	// ConfigMap is owned by the Loop (the operator's own object, not a
	// foreign one) and carries the facts body (rendered from the
	// Loop's spec/status).
	It("creates the deliver Job only after the description ConfigMap exists on the normal path (the R27/P1 ordering)", func() {
		ns := freshNS("a4-desc-order")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "desc8"
		r := s6Succeeded(name, ns, "http://gitea.samples.svc:3000/samples/desc8.git")
		s6Reconcile(r, ns, name)
		// Both exist after the Succeeded reconcile: the ConfigMap is ensured
		// FIRST (Create/Get succeeded), the Job is created AFTER.
		cmName := deliverDescriptionConfigName(name)
		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: cmName}, cm)).To(Succeed(),
			"the description ConfigMap must exist on the normal Succeeded path")
		job := s6GetJob(ns, name)
		Expect(job).ToNot(BeNil(), "the deliver Job must be created on the normal Succeeded path (after the ConfigMap is ensured)")
		// The ConfigMap is owned by the Loop (the operator's own object) and
		// carries the facts body.
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, loop)).To(Succeed())
		Expect(metav1.IsControlledBy(cm, loop)).To(BeTrue(), "the ConfigMap must be owned by the Loop")
		wantBody := renderDeliverDescriptionBody(deliverDescriptionForLoop(loop))
		Expect(cm.Data[deliverDescriptionDataKey]).To(Equal(wantBody), "the ConfigMap must carry the facts body (rendered from the Loop's spec/status)")
	})

	// R26/P1: a FORBIDDEN error from the description ConfigMap ensure (the
	// operator lacks the configmaps RBAC for the Loop's namespace — a
	// misinstalled/broken cluster) must NOT be silent: it is logged at error
	// level AND surfaced as a Warning Event (DeliveryDescriptionForbidden), so
	// the operator owner can see that every PR is degrading to the minimal
	// description. It must NOT error the reconcile (the push container falls
	// back to the minimal line; the delivery proceeds).
	It("surfaces a Forbidden description-ConfigMap ensure as a Warning Event (not silent) and does not error the reconcile", func() {
		ns := freshNS("a4-desc-forbidden")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "desc6"
		// Drive to Succeeded with the REAL client (the S5a/S6 helper pins the
		// verify evidence + the Succeeded reconcile creates the deliver
		// description ConfigMap via the real client).
		_ = s6Succeeded(name, ns, "http://gitea.samples.svc:3000/samples/desc6.git")

		// Delete the operator's deliver description ConfigMap (via the real
		// client): the next reconcile through the wrapped client will try to
		// CREATE it, and the wrapped client models a FORBIDDEN ConfigMap write
		// (the operator's ServiceAccount lacks the configmaps RBAC — the R25
		// kind bug, which the R26 RBAC marker fixes).
		cmName := deliverDescriptionConfigName(name)
		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: cmName}, cm)).To(Succeed())
		Expect(k8sClient.Delete(ctx, cm)).To(Succeed())

		// A reconciler with a wrapped client (FORBIDDEN ConfigMap writes) and a
		// fake recorder. The Succeeded reconcile must: (1) NOT error (the
		// best-effort ensure logs the Forbidden and proceeds), (2) surface the
		// Forbidden as a Warning Event (DeliveryDescriptionForbidden, not
		// silent), and (3) still ensure the deliver Job (the delivery is
		// expected; the minimal-line fallback degrades the description, not
		// the delivery).
		recorder := record.NewFakeRecorder(64)
		wrapped := &forbiddenConfigMapWrites{Client: k8sClient}
		r := &LoopReconciler{Client: wrapped, Scheme: k8sClient.Scheme(), apiReader: k8sClient, Recorder: recorder}
		_, reconcileErr := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		Expect(reconcileErr).NotTo(HaveOccurred(), "the Succeeded reconcile must NOT error when the description ConfigMap ensure is Forbidden (best-effort; the minimal-line fallback)")

		// The deliver Job is present (the delivery is expected): the Forbidden
		// ConfigMap ensure never blocked the deliver Job.
		job := s6GetJob(ns, name)
		Expect(job).ToNot(BeNil(), "the deliver Job must be ensured even when the description ConfigMap ensure is Forbidden (best-effort; the minimal-line fallback)")

		// A Warning Event was recorded (the Forbidden is NOT silent):
		// DeliveryDescriptionForbidden.
		found := false
		for i := 0; i < 64; i++ {
			select {
			case ev := <-recorder.Events:
				if ev != "" && strings.Contains(ev, "DeliveryDescriptionForbidden") {
					found = true
				}
			default:
				i = 64 // no more events to drain
			}
		}
		Expect(found).To(BeTrue(), "a Forbidden description-ConfigMap ensure must be surfaced as a Warning Event (DeliveryDescriptionForbidden), not left silent")
	})
})

// forbiddenConfigMapWrites wraps a client.Client and makes ConfigMap writes
// (Create/Update/Delete) return a FORBIDDEN error (the operator's ServiceAccount
// lacks the configmaps RBAC — the R25 kind bug, which the R26 RBAC marker fixes).
// Reads and all other resources pass through (so the Succeeded reconcile and the
// deliver Job are unaffected). Used by the R26 Forbidden spec to prove the
// ensure is surfaced (logged at error + a Warning Event) and non-fatal.
type forbiddenConfigMapWrites struct {
	client.Client
}

func forbiddenErr() error {
	return apierrors.NewForbidden(corev1.Resource("configmaps"), "forbidden by RBAC (test model)", fmt.Errorf("configmaps forbidden"))
}

func (c *forbiddenConfigMapWrites) isDeliverDescriptionCM(obj client.Object) bool {
	cm, ok := obj.(*corev1.ConfigMap)
	return ok && strings.HasSuffix(cm.Name, "-deliver-description")
}

func (c *forbiddenConfigMapWrites) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if c.isDeliverDescriptionCM(obj) {
		return forbiddenErr()
	}
	return c.Client.Create(ctx, obj, opts...)
}

func (c *forbiddenConfigMapWrites) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if c.isDeliverDescriptionCM(obj) {
		return forbiddenErr()
	}
	return c.Client.Update(ctx, obj, opts...)
}

func (c *forbiddenConfigMapWrites) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if c.isDeliverDescriptionCM(obj) {
		return forbiddenErr()
	}
	return c.Client.Delete(ctx, obj, opts...)
}

// deliverCMEnsureFails wraps a client.Client and makes the deliver
// description ConfigMap's CREATE (the non-forbidden path: the ConfigMap is
// missing, the ensure tries to create it) return a NON-forbidden error (a
// transient API failure — not a Forbidden error). Reads and all other
// resources pass through. Used by the R27/P1 non-forbidden ensure-failure
// spec to prove the Job is held (not created) while the ConfigMap is missing
// on a non-forbidden failure.
const transientAPIError = "transient API error: the apiserver is briefly unavailable (test model)"

type deliverCMEnsureFails struct {
	client.Client
	err string
}

func (c *deliverCMEnsureFails) isDeliverDescriptionCM(obj client.Object) bool {
	cm, ok := obj.(*corev1.ConfigMap)
	return ok && strings.HasSuffix(cm.Name, "-deliver-description")
}

func (c *deliverCMEnsureFails) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if c.isDeliverDescriptionCM(obj) {
		return fmt.Errorf("%s", c.err)
	}
	return c.Client.Create(ctx, obj, opts...)
}
