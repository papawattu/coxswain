// Copyright 2026 papawattu.
//
// fix/watch-kubearmorpolicy (PR #129) — the KubeArmorPolicy watch is CRD-GATED:
// it is registered only if the RESTMapper resolves engine.KubeArmorGVK at
// setup. On a cluster without KubeArmor (a --allow-unenforced dev setup, the
// Calico cluster), the KubeArmorPolicy CRD is absent (install.yaml does not
// ship it; KubeArmor installs it) and an unconditional watch would make the
// manager fail to start ("no matches for kind"), breaking cmd/main.go's
// promise that a missing KubeArmor CRD is tolerated (no-op) under the
// --allow-unenforced escape hatch.
//
// This envtest environment loads the Loop, AgentPolicy, and Sandbox CRDs but
// NOT the KubeArmorPolicy CRD (testdata/crds-no-kubearmor). It asserts:
//   1. the RESTMapper does NOT resolve engine.KubeArmorGVK (the CRD is
//      absent) — the precondition for skipping the watch;
//   2. SetupWithManager succeeds (the watch is gated on the RESTMapper, so
//      the missing CRD does not break manager setup);
//   3. the manager starts and syncs its cache without a "no matches for
//      kind" error (the watch was not registered against the absent kind).
//
// Mutation (the test must FAIL when the fix regresses): make the
// KubeArmorPolicy watch unconditional (register it regardless of the
// RESTMapper). Then the manager fails to start on this environment (the
// unstructured watch on an absent kind errors at source creation), and the
// cache-sync / manager-start assertions below fail.

package controller

import (
	"context"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/engine"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// The KubeArmorPolicy watch is CRD-gated (PR #129, P1). This Describe block
// starts its OWN envtest environment (no KubeArmorPolicy CRD) in BeforeEach so
// it can prove the manager starts without the watch. It is part of the existing
// TestControllers suite (suite_test.go); the suite's main environment (which
// DOES load the KubeArmorPolicy CRD) is independent of this test's environment.

var _ = Describe("KubeArmorPolicy watch is CRD-gated (PR #129, P1)", func() {
	var (
		noKAEnv *envtest.Environment
		mgr     manager.Manager
	)

	BeforeEach(func() {
		noKAEnv = &envtest.Environment{
			// Loop + AgentPolicy + Sandbox, but NOT the KubeArmorPolicy CRD.
			// The controller's Owns(Sandbox) still needs the Sandbox CRD; the
			// KubeArmorPolicy watch must be the ONLY thing gated on its CRD.
			CRDDirectoryPaths: []string{
				filepath.Join("..", "..", "internal", "controller", "testdata", "crds-no-kubearmor"),
			},
			ErrorIfCRDPathMissing: true,
		}
		if getFirstFoundEnvTestBinaryDir() != "" {
			noKAEnv.BinaryAssetsDirectory = getFirstFoundEnvTestBinaryDir()
		}
	})

	AfterEach(func() {
		_ = noKAEnv.Stop()
	})

	It("does not resolve the KubeArmorPolicy GVK, and the manager still starts without registering the watch", func() {
		cfg, err := noKAEnv.Start()
		Expect(err).NotTo(HaveOccurred(), "the envtest apiserver must start WITHOUT the KubeArmorPolicy CRD")

		_ = coxv1alpha1.AddToScheme(scheme.Scheme)
		_ = sandboxv1beta1.AddToScheme(scheme.Scheme)

		mgr, err = manager.New(cfg, manager.Options{
			Scheme:                 scheme.Scheme,
			Metrics:                metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress: "0",
		})
		Expect(err).NotTo(HaveOccurred())

		// Precondition: the manager's RESTMapper must NOT resolve the
		// KubeArmorPolicy GVK (the CRD is absent). This is the exact check the
		// gated watch uses; if it unexpectedly resolves, the CRD leaked into
		// this environment and the test is not exercising the gate.
		_, kaptErr := mgr.GetRESTMapper().RESTMapping(engine.KubeArmorGVK.GroupKind())
		Expect(kaptErr).To(HaveOccurred(),
			"the KubeArmorPolicy GVK must NOT resolve in this environment (the CRD is absent); the gate relies on this")

		r := &LoopReconciler{
			Client: mgr.GetClient(),
			Scheme: mgr.GetScheme(),
		}
		// The Enforcer is nil (no enforcement); the gate fails closed without
		// it (PolicyEnforced=False / NodeNotEnforcing), which is the no-op
		// behaviour cmd/main.go promises under --allow-unenforced.
		Expect(r.SetupWithManager(mgr)).To(Succeed(),
			"SetupWithManager must succeed WITHOUT the KubeArmorPolicy CRD (the watch is CRD-gated)")

		// Start the manager and confirm its cache syncs without a "no matches
		// for kind" error. The unstructured watch on an ABSENT kind would fail
		// at source creation (a discovery-backed informer cannot list/watch a
		// kind with no REST mapping) — so a successful cache sync proves the
		// watch was NOT registered against the absent kind.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		startErr := make(chan error, 1)
		go func() {
			startErr <- mgr.Start(ctx)
		}()
		Eventually(func() bool {
			return mgr.GetCache().WaitForCacheSync(ctx)
		}, "20s", "500ms").Should(BeTrue(),
			"the manager must start and sync its cache WITHOUT the KubeArmorPolicy CRD (the watch is CRD-gated; no 'no matches for kind')")
		cancel()
		// mgr.Start returns after the context is cancelled (or with an error if
		// a source failed to start). A "no matches for kind" error is the P1
		// regression.
		select {
		case err := <-startErr:
			Expect(err).NotTo(HaveOccurred(), "the manager must not fail to start on a cluster WITHOUT the KubeArmorPolicy CRD")
		default:
			// The manager is still running (Start hasn't returned yet); the
			// cache-sync assertion already proved it started. The context
			// cancel will make Start return; no error expected.
		}
	})
})

// P2 (PR #129): the KubeArmorPolicy watch's map function
// (kubeArmorPolicyToLoopRequests) is unit-tested in isolation. It maps a
// KubeArmorPolicy (unstructured) to the Loop it belongs to, so a tamper
// re-reconciles that Loop at once. The operator names the per-Loop Kapt
// "coxswain-<loopName>"; a Kapt whose name is not prefixed "coxswain-" (a
// foreign or unrelated policy) maps to nothing.
var _ = Describe("kubeArmorPolicyToLoopRequests maps a Kapt to its Loop (PR #129, P2)", func() {
	makeKapt := func(ns, name string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(engine.KubeArmorGVK)
		u.SetNamespace(ns)
		u.SetName(name)
		return u
	}
	it := func() {
		r := &LoopReconciler{}
		// 1. A coxswain-prefixed Kapt maps to the Loop with the same name in
		//    the Kapt's namespace (coxswain-foo -> {ns, foo}).
		reqs := r.kubeArmorPolicyToLoopRequests(context.Background(), makeKapt("ns1", "coxswain-foo"))
		Expect(reqs).To(HaveLen(1), "a coxswain-foo Kapt must map to one Loop")
		Expect(reqs[0].Name).To(Equal("foo"), "the Loop name is the Kapt name minus the coxswain- prefix")
		Expect(reqs[0].Namespace).To(Equal("ns1"), "the Loop is in the Kapt's namespace")

		// 2. A non-prefixed name (a foreign or unrelated policy) maps to
		//    nothing (the operator does not own it; it must not re-reconcile
		//    a Loop).
		Expect(r.kubeArmorPolicyToLoopRequests(context.Background(), makeKapt("ns1", "foo"))).To(BeEmpty(),
			"a non-coxswain-prefixed Kapt (a foreign policy) must map to no Loop")

		// 3. The bare prefix "coxswain-" (an empty loop name) maps to nothing
		//    (there is no Loop named "").
		Expect(r.kubeArmorPolicyToLoopRequests(context.Background(), makeKapt("ns1", "coxswain-"))).To(BeEmpty(),
			"the bare coxswain- prefix (empty loop name) must map to no Loop")

		// 4. An object that is not an unstructured KubeArmorPolicy maps to
		//    nothing (defensive: the type assertion must not panic).
		Expect(r.kubeArmorPolicyToLoopRequests(context.Background(), &corev1.Pod{})).To(BeEmpty(),
			"a non-unstructured object must map to no Loop")
	}
	It("maps coxswain-<loop> to {ns, <loop>}; non-prefixed and bare-prefix map to none", it)
})
