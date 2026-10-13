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

// B1 (D59) — the VolumeSnapshot watch is CRD-GATED (I88): it is registered
// only if the RESTMapper resolves the VolumeSnapshot GVK at setup. On a
// cluster without the snapshot CRDs (a cluster that has not installed the
// external-snapshotter), an unconditional watch would make the manager fail
// to start ("no matches for kind"), and a cached List on the absent type
// would wedge the manager's cache forever. This envtest environment loads
// the Loop, AgentPolicy, and Sandbox CRDs but NOT the VolumeSnapshot CRDs
// (testdata/crds-no-snapshot) and asserts:
//
//   1. the RESTMapper does NOT resolve the VolumeSnapshot GVK (the CRD is
//      absent) — the precondition for skipping the watch;
//   2. SetupWithManager succeeds (the watch is gated on the RESTMapper, so
//      the missing CRDs do not break manager setup);
//   3. the manager starts and syncs its cache without a broken-volumeSnapshot
//      source error (the watch was not registered against the absent kind);
//   4. a Loop still reconciles (the operator's reconcile-side reads are
//      gated on snapshotCRDPresent, so no cached List on the absent type is
//      issued and the Loop's phase machine still runs).
//
// Mutation (the test must FAIL when the gate regresses): make the
// VolumeSnapshot watch unconditional (register it regardless of the
// RESTMapper). Then the manager's cache for the absent kind errors at source
// creation, and the cache-sync assertion below fails.

package controller

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// b1CaptureBaseLen records the length of the shared capturedEntries slice at
// the start of each It (the kapt test's package-level captureLogger appends
// to this same slice); the It reads only the entries logged after this length.
var b1CaptureBaseLen int

var _ = Describe("VolumeSnapshot watch is CRD-gated (B1 D59 / I88)", func() {
	var (
		noSnapEnv *envtest.Environment
		mgr       manager.Manager
	)

	BeforeEach(func() {
		// Snapshot the length of the shared captured-entries slice so the It
		// can read only the entries logged during its own manager run (the
		// kapt test's captureLogger appends to this same slice).
		capturedMu.Lock()
		b1CaptureBaseLen = len(capturedEntries)
		capturedMu.Unlock()

		noSnapEnv = &envtest.Environment{
			// Loop + AgentPolicy + Sandbox, but NOT the VolumeSnapshot CRDs.
			CRDDirectoryPaths: []string{
				filepath.Join("..", "..", "internal", "controller", "testdata", "crds-no-snapshot"),
			},
			ErrorIfCRDPathMissing: true,
		}
		if getFirstFoundEnvTestBinaryDir() != "" {
			noSnapEnv.BinaryAssetsDirectory = getFirstFoundEnvTestBinaryDir()
		}
	})

	AfterEach(func() {
		_ = noSnapEnv.Stop()
	})

	It("does not resolve the VolumeSnapshot GVK, and the manager still starts without registering the watch", func() {
		cfg, err := noSnapEnv.Start()
		Expect(err).NotTo(HaveOccurred(), "the envtest apiserver must start WITHOUT the VolumeSnapshot CRDs")

		_ = coxv1alpha1.AddToScheme(scheme.Scheme)
		_ = sandboxv1beta1.AddToScheme(scheme.Scheme)

		mgr, err = manager.New(cfg, manager.Options{
			Scheme:                 scheme.Scheme,
			Metrics:                metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress: "0",
		})
		Expect(err).NotTo(HaveOccurred())

		// Precondition: the manager's RESTMapper must NOT resolve the
		// VolumeSnapshot GVK (the CRD is absent). This is the exact check the
		// gated watch uses.
		_, snapErr := mgr.GetRESTMapper().RESTMapping(volumeSnapshotGVK.GroupKind())
		Expect(snapErr).To(HaveOccurred(),
			"the VolumeSnapshot GVK must NOT resolve in this environment (the CRD is absent); the gate relies on this")

		r := &LoopReconciler{
			Client: mgr.GetClient(),
			Scheme: mgr.GetScheme(),
		}
		// Wire the manager's RESTMapper so snapshotCRDPresent (the reconcile-
		// side gate) reads the REAL RESTMapper (not the nil-mapper default
		// that treats the CRD as present).
		r.RESTMapper = mgr.GetRESTMapper()
		Expect(r.SetupWithManager(mgr)).To(Succeed(),
			"SetupWithManager must succeed WITHOUT the VolumeSnapshot CRDs (the watch is CRD-gated)")

		// Start the manager. The main cache must sync (the VolumeSnapshot
		// source is not registered when the CRD is absent).
		ctx, cancel := context.WithCancel(context.Background())
		startErr := make(chan error, 1)
		go func() {
			startErr <- mgr.Start(ctx)
		}()
		Eventually(func() bool {
			return mgr.GetCache().WaitForCacheSync(ctx)
		}, "20s", "500ms").Should(BeTrue(),
			"the manager must start and sync its cache (the CRD-gated watch is not registered when the CRD is absent)")

		// A Loop still reconciles: create one and reconcile it. The
		// reconcile-side snapshot reads are gated on snapshotCRDPresent
		// (which reads the REAL RESTMapper via r.RESTMapper), so no cached
		// List on the absent VolumeSnapshot type is issued, and the phase
		// machine still runs (the operator boots the Loop, sets its
		// conditions — including CheckpointsAvailable=False for the missing
		// CRD — and advances it).
		ns := "b1-nocrd-reconcile-" + nowSuffix()
		Expect(mgr.GetClient().Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
		Expect(mgr.GetClient().Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: "b1nocrd", Namespace: ns},
			Spec:       b1LoopSpec(),
		})).To(Succeed())
		// A few reconciles (the phase machine needs a few passes to reach
		// Planning).
		req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "b1nocrd", Namespace: ns}}
		for range 3 {
			_, _ = r.Reconcile(ctx, req)
			time.Sleep(200 * time.Millisecond)
		}
		loop := &coxv1alpha1.Loop{}
		Expect(mgr.GetClient().Get(ctx, types.NamespacedName{Name: "b1nocrd", Namespace: ns}, loop)).To(Succeed())
		Expect(loop.Status.Phase).NotTo(Equal(coxv1alpha1.LoopPhasePending),
			"the Loop must advance past Pending (the operator reconciles it even without the snapshot CRDs)")

		// The CheckpointsAvailable condition is False with the named
		// VolumeSnapshotCRDMissing reason (the VISIBLE fallback, I90).
		c := b1CheckpointCondition(loop)
		Expect(c).NotTo(BeNil(), "the CheckpointsAvailable condition must be set (a VISIBLE fallback, I90)")
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Reason).To(Equal(CheckpointReasonCRDMissing))

		// Give any broken VolumeSnapshot source (the mutation) a moment to
		// log its "if kind is a CRD, it should be installed before calling
		// Start" error.
		time.Sleep(3 * time.Second)
		cancel()
		select {
		case err := <-startErr:
			Expect(err).ToNot(HaveOccurred(), "mgr.Start must not return a broken-source error")
		case <-time.After(10 * time.Second):
			Fail("mgr.Start did not return after the context was cancelled")
		}
		// The load-bearing assertion: NO "if kind is a CRD, it should be
		// installed before calling Start" error was logged for the
		// VolumeSnapshot kind. (The shared capturedEntries slice is written
		// by the kapt test's package-level captureLogger, which captures ALL
		// controller-runtime logs in this process.)
		// The load-bearing assertion: NO "if kind is a CRD, it should be
		// installed before calling Start" error was logged for the
		// VolumeSnapshot kind. (The shared capturedEntries slice is written
		// by the kapt test's package-level captureLogger, which captures ALL
		// controller-runtime logs in this process.)
		capturedMu.Lock()
		entries := capturedEntries
		capturedMu.Unlock()
		// Read only the entries logged AFTER the BeforeEach snapshot (the
		// manager run), to avoid asserting on entries logged by other tests.
		newEntries := entries[b1CaptureBaseLen:]
		for _, e := range newEntries {
			Expect(e).NotTo(ContainSubstring("if kind is a CRD, it should be installed before calling Start"),
				"a broken VolumeSnapshot source (unconditional watch) must not be created when the CRD is absent")
		}
		// THE gate's positive signal: the operator's CRD-gated log line was
		// emitted ("VolumeSnapshot CRD not present ... the per-Loop checkpoint
		// watch is not registered"). An unconditional watch (the mutation)
		// does not log this (it always registers), so its absence is the
		// definitive signal that the gate skipped the watch. The log line is
		// emitted during SetupWithManager (mgr.GetRESTMapper() is used), so it
		// is in the entries logged after the BeforeEach snapshot.
		var sawGateLog bool
		for _, e := range newEntries {
			if strings.Contains(e, "VolumeSnapshot CRD not present") && strings.Contains(e, "checkpoint watch is not registered") {
				// The captureLogger records the logr message (msg string); the
				// kv values (the reason, etc.) are separate, so assert on the
				// message text only.
				sawGateLog = true
				break
			}
		}
		Expect(sawGateLog).To(BeTrue(),
			"the CRD-gated watch log line (\"VolumeSnapshot CRD not present ... checkpoint watch is not registered\") must be emitted when the CRD is absent; an unconditional watch (the mutation) does not emit it")
	})
})
