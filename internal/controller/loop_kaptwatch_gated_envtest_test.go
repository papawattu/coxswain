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
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/go-logr/logr"
	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/engine"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// captureLogger is a minimal logr.LogSink that records every log message (as
// a string) into a shared slice. Used by the CRD-gated watch test to detect
// whether a broken KubeArmorPolicy source logs its "if kind is a CRD, it
// should be installed before calling Start" error (the controller-runtime
// source for an unstructured watch on an absent kind logs this).
type captureLogger struct {
	mu      *sync.Mutex
	entries *[]string
}

func (c *captureLogger) Init(info logr.RuntimeInfo) {}
func (c *captureLogger) Enabled(level int) bool       { return true }
func (c *captureLogger) Info(level int, msg string, kv ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	*c.entries = append(*c.entries, msg)
}
func (c *captureLogger) Error(err error, msg string, kv ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	*c.entries = append(*c.entries, msg+" | "+err.Error())
}
func (c *captureLogger) VLevel(level int) logr.LogSink { return c }
func (c *captureLogger) WithValues(kv ...any) logr.LogSink { return c }
func (c *captureLogger) WithName(name string) logr.LogSink { return c }

// capturedEntries holds the log entries captured by the CRD-gated watch test's
// capturing logger. Set in BeforeEach, read in the It, restored in AfterEach.
// capLogger is the capturing logger (a delegating logr.Logger) installed via
// init() so that controller-runtime's one-shot rootLog.Fulfill (called by
// logf.SetLogger) is satisfied with capLogger from package load. This is
// essential: the Ginkgo suite's BeforeAll ALSO calls logf.SetLogger(zap...),
// but logf.SetLogger's Fulfill is one-shot — once rootLog is fulfilled (by
// init() here), the suite's SetLogger is a no-op, and ALL controller-runtime
// logging (including the KubeArmorPolicy source's "if kind is a CRD" error)
// goes to capLogger, where the CRD-gated watch test can observe it. The cost:
// the Ginkgo suite's log output goes to capLogger instead of GinkgoWriter
// (less visible output), but no spec asserts on log output, so this is safe.
var (
	capturedEntries []string
	capturedMu      sync.Mutex
	capLogger       logr.Logger
)

func init() {
	capLogger = logr.New(&captureLogger{entries: &capturedEntries, mu: &capturedMu})
	logf.SetLogger(capLogger)
}

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
		// Clear the captured log entries (the capturing logger was installed in
		// init(); this gives the test a clean slate). The main suite's envtest
		// manager may have logged since the last test; we only care about what
		// is logged during THIS test's manager run.
		capturedMu.Lock()
		capturedEntries = nil
		capturedMu.Unlock()
	})

	AfterEach(func() {
		_ = noKAEnv.Stop()
		// Drain the captured entries (the capturing logger is package-level and
		// persists; clearing here bounds memory).
		capturedMu.Lock()
		capturedEntries = nil
		capturedMu.Unlock()
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

		// Start the manager. The load-bearing assertion is about the
		// KubeArmorPolicy source: when the watch is unconditionally registered
		// (the mutation) on an ABSENT kind, the source cannot get an informer
		// and logs the controller-runtime error "if kind is a CRD, it should be
		// installed before calling Start" (controller-runtime
		// pkg/internal/source/kind.go) — and keeps retrying. The CRD gate
		// prevents that source from being created, so no such error is logged
		// (captured into capturedEntries by the capturing logger set in
		// BeforeEach). The main cache still syncs either way (the
		// KubeArmorPolicy source is a separate informer), so the log is the
		// definitive signal of the broken source.
		ctx, cancel := context.WithCancel(context.Background())
		startErr := make(chan error, 1)
		go func() {
			startErr <- mgr.Start(ctx)
		}()
		// Wait for the main cache to sync (the manager is running).
		Eventually(func() bool {
			return mgr.GetCache().WaitForCacheSync(ctx)
		}, "20s", "500ms").Should(BeTrue(),
			"the manager must start and sync its cache (the CRD-gated watch is not registered when the CRD is absent)")
		// Give the KubeArmorPolicy source (if unconditionally registered, i.e.
		// the mutation) a moment to log its "if kind is a CRD, it should be
		// installed before calling Start" error.
		time.Sleep(3 * time.Second)
		cancel()
		// mgr.Start returns after the context is cancelled; assert no broken
		// source error. (On a real cluster the broken source keeps retrying;
		// the log assertion below is the load-bearing check.)
		select {
		case err := <-startErr:
			Expect(err).To(BeNil(), "mgr.Start must not return a broken-source error")
		case <-time.After(10 * time.Second):
			Fail("mgr.Start did not return after the context was cancelled")
		}
		// The load-bearing assertion: NO "if kind is a CRD, it should be
		// installed before calling Start" error was logged. The unconditional
		// watch (the mutation) would log it for the absent KubeArmorPolicy kind.
		// The capturing logger (installed in init()) captures all
		// controller-runtime logs; we check the entries captured during this
		// test's manager run.
		capturedMu.Lock()
		entries := capturedEntries
		capturedMu.Unlock()
		for _, e := range entries {
			Expect(e).NotTo(ContainSubstring("if kind is a CRD, it should be installed before calling Start"),
				"a broken KubeArmorPolicy source (unconditional watch) must not be created when the CRD is absent")
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
