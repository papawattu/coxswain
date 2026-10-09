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

// I72: the deliver Job's clone-base fetches the base ref's commit over the
// network. When delivery traverses the Loop's egress proxy (an external repo
// host — deliverNeedsProxyHosts non-empty), the proxy pod must be Ready
// (owned by the Loop and PodReady) BEFORE the deliver Job is created;
// otherwise clone-base fails in milliseconds (the proxy is not listening)
// and delivery goes terminally Delivered=False/DeliveryFailed (backoffLimit
// 0, one Job per verifiedCommit — the operator does not retry). The gate is
// the same pattern as the sandbox's I42b gate (loop_controller.go): a pod
// absent, not owned by the Loop, or not Ready holds the deliver step
// (requeue, no Job, no failure). The gate applies ONLY when delivery goes
// via the egress proxy; an in-cluster repo (repoPeer covers the push) keeps
// the direct path and needs no proxy.
//
// The envtest specs (I49 test norm: a decision that reads pod status gets a
// spec for each in-progress state, asserting no decision while in progress):
//
//   - pod absent -> hold: no deliver Job, Delivered not failed
//   - pod present but Pending (phase Pending, no Ready condition) -> hold
//   - pod present, phase Running but Ready=False -> hold
//   - a pod owned by another object -> hold (not controlled by the Loop)
//   - pod owned by the Loop and Ready -> the deliver Job is created
//   - an in-cluster (Gitea .svc) delivery -> no gate: the Job is created
//     while the egress proxy pod is absent (the direct path)
//
// Mutation (recorded in the PR): with the gate removed, the absent-pod and
// the in-progress-state specs must fail (the Job is created while the proxy
// is not Ready). The Ready spec is unaffected (it passes with or without
// the gate — it is the positive control).

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

var _ = Describe("I72: deliver Job held until the egress proxy is Ready (envtest)", func() {
	ctx := context.Background()

	freshNS := func(prefix string) string {
		ns := prefix + "-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		return ns
	}

	// i72AgentPolicy creates an AgentPolicy with one external network allow
	// in ns and returns it (the Loop references it by name — the egress
	// proxy is only created/needed when the effective policy has network
	// allows, I42b).
	i72AgentPolicy := func(ns, name string) string {
		ap := &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Network: []string{i42eGitHubHost}},
		}
		Expect(k8sClient.Create(ctx, ap)).To(Succeed())
		return name
	}

	// i72Succeeded drives a fresh Loop with delivery mode PullRequest to
	// Succeeded (the S5a claim path) and returns the reconciler. repo is the
	// workspace repo (github.com -> external -> the egress proxy is needed
	// for the deliver Job's clone-base/push). The AgentPolicy (with the
	// external network allow) is created first and referenced by the Loop.
	i72Succeeded := func(ns, name, repo, policyName string) *LoopReconciler {
		ctx := context.Background()
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
		s5aEnsureSandbox(ns, name)
		loop := s6Loop(name, ns, repo)
		loop.Spec.PolicyRefs = []string{policyName}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		s6Reconcile(r, ns, name) // Pending -> Planning
		loop = &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		loop.Status.BaseCommit = s5aBaseCommit
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		s5aClaimPod(ns, name, "Planning", "")
		s6Reconcile(r, ns, name)
		s5aEnsureSandbox(ns, name) // the annotation recycle deleted the Sandbox
		s5aClaimPod(ns, name, "Implementing", s6HeadCommit)
		s6Reconcile(r, ns, name)
		s5aVerifyPod(ctx, ns, name, name+"-verify-1", 0)
		s6Reconcile(r, ns, name)
		loop = &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded),
			"I72: the Loop must reach Succeeded")
		return r
	}

	// i72EgressPod creates the egress proxy pod for the Loop (name+
	// "-egress-proxy") using buildEgressProxyPod (the operator's own builder,
	// so the spec-hash annotation matches the desired spec and the operator's
	// ensureEgressProxy does not delete it for drift), with the given phase
	// and Ready status (the operator-owned pod the gate reads). ready=true
	// sets the PodReady condition to True; ready=false sets it to False.
	i72EgressPod := func(ns, loopName string, phase corev1.PodPhase, ready bool) {
		ctx := context.Background()
		nn := types.NamespacedName{Name: loopName + "-egress-proxy", Namespace: ns}
		existing := &corev1.Pod{}
		if !apierrors.IsNotFound(k8sClient.Get(ctx, nn, existing)) {
			Expect(k8sClient.Delete(ctx, existing)).To(Succeed())
		}
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: loopName, Namespace: ns}, loop)).To(Succeed())
		pod := buildEgressProxyPod(loopName, ns, "coxswain-egress-proxy:standin",
			[]string{i42eGitHubHost}, "testhash", "10.244.0.0/16", "10.96.0.0/12")
		pod.Annotations = map[string]string{proxySpecHashAnnotation: egressProxyPodSpecHash(pod)}
		if err := controllerutil.SetControllerReference(loop, pod, k8sClient.Scheme()); err != nil {
			Expect(err).NotTo(HaveOccurred(), "I72: set owner ref on the egress proxy pod")
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		pod.Status.Phase = phase
		if ready {
			pod.Status.Conditions = []corev1.PodCondition{{
				Type: corev1.PodReady, Status: corev1.ConditionTrue,
			}}
		} else {
			pod.Status.Conditions = []corev1.PodCondition{{
				Type: corev1.PodReady, Status: corev1.ConditionFalse,
			}}
		}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	}

	// i72ForeignEgressPod creates an egress proxy pod for the Loop that is
	// NOT owned by the Loop (no owner ref — the operator treats it as
	// EgressProxyConflict and does not delete it, and the I72 gate holds
	// because the pod is not controlled by the Loop, same pattern as
	// D35a/I42b). The spec-hash annotation is set so the operator's
	// ensureEgressProxy does not delete it for drift.
	i72ForeignEgressPod := func(ns, loopName string) {
		ctx := context.Background()
		nn := types.NamespacedName{Name: loopName + "-egress-proxy", Namespace: ns}
		existing := &corev1.Pod{}
		if !apierrors.IsNotFound(k8sClient.Get(ctx, nn, existing)) {
			Expect(k8sClient.Delete(ctx, existing)).To(Succeed())
		}
		pod := buildEgressProxyPod(loopName, ns, "coxswain-egress-proxy:standin",
			[]string{i42eGitHubHost}, "testhash", "10.244.0.0/16", "10.96.0.0/12")
		// No owner ref (the pod is foreign). The spec-hash annotation is set
		// to the hash of this pod's own spec so the operator does not delete
		// it for drift (the operator's ensureEgressProxy skips deletion when
		// the pod is not controlled by the Loop, but the annotation keeps the
		// pod stable across reconciles).
		pod.Annotations = map[string]string{proxySpecHashAnnotation: egressProxyPodSpecHash(pod)}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		pod.Status.Phase = corev1.PodRunning
		pod.Status.Conditions = []corev1.PodCondition{{
			Type: corev1.PodReady, Status: corev1.ConditionTrue,
		}}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	}

	// i72AssertHold asserts the deliver step is held: no deliver Job exists
	// and the Delivered condition is NOT failed (absent or InProgress — the
	// hold must not write a failure).
	i72AssertHold := func(ns, name string, detail string) {
		ctx := context.Background()
		job := &batchv1.Job{}
		err := k8sClient.Get(ctx, types.NamespacedName{Name: name + "-deliver", Namespace: ns}, job)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(),
			"I72: no deliver Job expected while the egress proxy is not Ready (%s)", detail)
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		ok, status, reason := s6Cond(loop)
		if ok {
			Expect(status != metav1.ConditionFalse || reason != coxv1alpha1.ReasonDeliveryFailed).To(BeTrue(),
				"I72: the hold must not write Delivered=False/DeliveryFailed (%s); got %s/%s", detail, status, reason)
		}
	}

	It("holds the deliver Job while the egress proxy pod is absent (external repo)", func() {
		ns := freshNS("i72-absent")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()
		name := "i72a"
		policy := i72AgentPolicy(ns, "i72a-policy")
		r := i72Succeeded(ns, name, "https://github.com/o/r.git", policy)
		// The egress proxy pod is absent (the operator creates it, but in
		// envtest the spec does not run a full reconcile that would create
		// it — assert the hold before it exists).
		s6Reconcile(r, ns, name)
		i72AssertHold(ns, name, "pod absent")
	})

	It("holds the deliver Job while the egress proxy pod is Pending (external repo)", func() {
		ns := freshNS("i72-pending")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()
		name := "i72b"
		policy := i72AgentPolicy(ns, "i72b-policy")
		r := i72Succeeded(ns, name, "https://github.com/o/r.git", policy)
		i72EgressPod(ns, name, corev1.PodPending, false)
		s6Reconcile(r, ns, name)
		i72AssertHold(ns, name, "pod Pending, not Ready")
	})

	It("holds the deliver Job while the egress proxy pod is Running but not Ready (external repo)", func() {
		ns := freshNS("i72-running")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()
		name := "i72c"
		policy := i72AgentPolicy(ns, "i72c-policy")
		r := i72Succeeded(ns, name, "https://github.com/o/r.git", policy)
		i72EgressPod(ns, name, corev1.PodRunning, false)
		s6Reconcile(r, ns, name)
		i72AssertHold(ns, name, "pod Running, Ready=False")
	})

	It("holds the deliver Job when the egress proxy pod is owned by a foreign object (external repo)", func() {
		ns := freshNS("i72-foreign")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()
		name := "i72d"
		policy := i72AgentPolicy(ns, "i72d-policy")
		r := i72Succeeded(ns, name, "https://github.com/o/r.git", policy)
		i72ForeignEgressPod(ns, name)
		s6Reconcile(r, ns, name)
		i72AssertHold(ns, name, "pod owned by a foreign object")
	})

	It("creates the deliver Job once the egress proxy pod is owned by the Loop and Ready (external repo)", func() {
		ns := freshNS("i72-ready")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()
		name := "i72e"
		policy := i72AgentPolicy(ns, "i72e-policy")
		r := i72Succeeded(ns, name, "https://github.com/o/r.git", policy)
		// The operator creates its own egress proxy pod (ensureEgressProxy)
		// with the effective policy hash. Drive reconcile until the pod is
		// stable (no spec drift) and owned by the Loop, then mark it Ready
		// (envtest has no kubelet). The I72 gate passes and the deliver Job
		// is created on the next reconcile.
		var podUID types.UID
		var loop *coxv1alpha1.Loop
		for range 30 {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
			Expect(err).NotTo(HaveOccurred())
			loop = &coxv1alpha1.Loop{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
			pod := &corev1.Pod{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: egressProxyPodName(name), Namespace: ns}, pod); err == nil &&
				metav1.IsControlledBy(pod, loop) {
				if podUID != "" && pod.UID == podUID {
					break // stable across two reconciles
				}
				podUID = pod.UID
			}
			time.Sleep(100 * time.Millisecond)
		}
		Expect(podUID).ToNot(Equal(types.UID("")),
			"I72: the operator's egress proxy pod must exist and be owned by the Loop")
		// Mark the operator's pod Ready (envtest has no kubelet).
		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: egressProxyPodName(name), Namespace: ns}, pod)).To(Succeed())
		Expect(pod.UID).To(Equal(podUID), "I72: the pod must be the stable one")
		pod.Status.Phase = corev1.PodRunning
		pod.Status.Conditions = []corev1.PodCondition{{
			Type: corev1.PodReady, Status: corev1.ConditionTrue,
		}}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
		// One more reconcile: the I72 gate passes (pod owned + Ready) and the
		// deliver Job is created.
		s6Reconcile(r, ns, name)
		job := s6GetJob(ns, name)
		Expect(job.Annotations[verifyCommitAnnotation]).To(Equal(s6HeadCommit),
			"I72: the deliver Job is created once the egress proxy is Ready (the D27 stamp)")
		// The Job's own outcome mapping sets InProgress on creation.
		loop = &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		ok, status, reason := s6Cond(loop)
		Expect(ok).To(BeTrue(), "I72: the Delivered condition must be set once the Job is created")
		Expect(status).To(Equal(metav1.ConditionFalse))
		Expect(reason).To(Equal(coxv1alpha1.ReasonDeliveryInProgress))
	})

	It("does NOT gate an in-cluster (Gitea .svc) delivery: the deliver Job is created while the egress proxy pod is absent (direct path)", func() {
		ns := freshNS("i72-incluster")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()
		name := "i72f"
		policy := i72AgentPolicy(ns, "i72f-policy")
		r := i72Succeeded(ns, name, inClusterRepoURL, policy)
		// No egress proxy pod (absent). The in-cluster delivery uses the
		// direct repo-peer path (no proxy hop), so the gate does not apply.
		s6Reconcile(r, ns, name)
		job := s6GetJob(ns, name)
		Expect(job.Annotations[verifyCommitAnnotation]).To(Equal(s6HeadCommit),
			"I72: the in-cluster deliver Job is NOT gated on the egress proxy (direct path)")
	})

	It("keeps a Failed deliver Job as DeliveryFailed while the egress proxy pod is not Ready (CREATE-path-only gate)", func() {
		ns := freshNS("i72-failed")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()
		name := "i72g"
		policy := i72AgentPolicy(ns, "i72g-policy")
		r := i72Succeeded(ns, name, "https://github.com/o/r.git", policy)
		// The I72 gate is on the CREATE path only: an existing deliver Job's
		// outcome is mapped as before (Failed -> DeliveryFailed), NOT held as
		// InProgress. This spec proves a terminal delivery outcome is not reset
		// by a proxy that is not Ready. To reach that state we must first get
		// the deliver Job created (the gate passes when the proxy is Ready),
		// then mark it Failed, then mark the proxy not Ready, and assert the
		// Failed outcome persists (not reset to InProgress by the gate).
		//
		// Phase 1: mark the egress proxy pod Ready so the I72 gate passes and
		// the deliver Job is created. The operator creates its own egress proxy
		// pod (ensureEgressProxy); in envtest it may be deleted for spec drift
		// before it is stable. Mark the current pod Ready, then reconcile until
		// the deliver Job is created (the gate passes once a pod is owned +
		// Ready; if the operator recreates the pod, re-mark it Ready and
		// re-reconcile).
		egressPodName := egressProxyPodName(name)
		var loop *coxv1alpha1.Loop
		var job *batchv1.Job
		for range 30 {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
			Expect(err).NotTo(HaveOccurred())
			// Is the deliver Job created yet?
			job = &batchv1.Job{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name + "-deliver", Namespace: ns}, job); err == nil {
				break // the Job exists — the gate passed and the Job was created
			}
			// The Job is not yet created: mark the current egress proxy pod
			// Ready (envtest has no kubelet) so the gate can pass on the next
			// reconcile. If the operator recreated the pod (spec drift), the
			// new pod is marked Ready here too.
			loop = &coxv1alpha1.Loop{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
			egressPod := &corev1.Pod{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: egressPodName, Namespace: ns}, egressPod); err == nil &&
				metav1.IsControlledBy(egressPod, loop) {
				egressPod.Status.Phase = corev1.PodRunning
				egressPod.Status.Conditions = []corev1.PodCondition{{
					Type: corev1.PodReady, Status: corev1.ConditionTrue,
				}}
				Expect(k8sClient.Status().Update(ctx, egressPod)).To(Succeed())
			}
			time.Sleep(100 * time.Millisecond)
		}
		job = &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-deliver", Namespace: ns}, job)).To(Succeed(),
			"I72: the deliver Job must be created once the egress proxy is Ready")
		Expect(job.Annotations[verifyCommitAnnotation]).To(Equal(s6HeadCommit),
			"I72: the deliver Job must be stamped for the verified commit")
		// Phase 2: mark the Job Failed (a terminal delivery outcome).
		job.Status.Failed = 1
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
		// Phase 3: mark the egress proxy pod NOT Ready (the I72 gate would hold
		// a fresh Job, but the existing Job's outcome must be mapped as before).
		egressPod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: egressProxyPodName(name), Namespace: ns}, egressPod)).To(Succeed())
		egressPod.Status.Phase = corev1.PodPending
		egressPod.Status.Conditions = []corev1.PodCondition{{
			Type: corev1.PodReady, Status: corev1.ConditionFalse,
		}}
		Expect(k8sClient.Status().Update(ctx, egressPod)).To(Succeed())
		// The reconcile must map the existing Failed Job to DeliveryFailed, NOT
		// hold it as InProgress (the gate is CREATE-path-only).
		loop = s6Reconcile(r, ns, name)
		ok, status, reason := s6Cond(loop)
		Expect(ok).To(BeTrue(), "I72: the Delivered condition must be set")
		Expect(status).To(Equal(metav1.ConditionFalse))
		Expect(reason).To(Equal(coxv1alpha1.ReasonDeliveryFailed),
			"I72: a Failed deliver Job must stay DeliveryFailed even while the egress proxy is not Ready (the gate is CREATE-path-only, not an outcome override)")
		// The Job is NOT deleted/recreated (no retry: one Job per verifiedCommit).
		jobAfter := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-deliver", Namespace: ns}, jobAfter)).To(Succeed(),
			"I72: the Failed deliver Job must persist (not deleted by the gate)")
		Expect(jobAfter.Status.Failed).To(BeNumerically(">", 0),
			"I72: the Failed deliver Job's status must be untouched (the gate does not retry)")
	})
})
