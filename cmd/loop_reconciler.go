// D76/D102 (R45 review, PR #380 P2): the LoopReconciler construction lives
// here (not inline in main) so main's flag-to-reconciler wiring is covered by
// a test. main builds newLoopReconcilerOptions from the parsed flags, then
// calls newLoopReconciler, which maps every option onto the reconciler. A
// missed or wrong wiring (e.g. SecretOptInRequired no longer fed from
// --secret-opt-in) fails TestNewLoopReconcilerWiresSecretOptIn (
// loop_reconciler_test.go), which builds the reconciler from DEFAULT flags
// through the same path main uses.
package main

import (
	"github.com/papawattu/coxswain/internal/alerts"
	"github.com/papawattu/coxswain/internal/cni"
	"github.com/papawattu/coxswain/internal/controller"
	"github.com/papawattu/coxswain/internal/engine"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// newLoopReconcilerOptions carries the flag-derived configuration the
// LoopReconciler is built from. Every field is a parsed operator flag (or
// an operator-derived value such as the operator namespace).
type newLoopReconcilerOptions struct {
	// SecretOptInRequired is the D76/D102 Secret opt-in gate (--secret-opt-in,
	// fail-closed by default: true).
	SecretOptInRequired bool
	// AllowUnenforced is the eBPF escape hatch (--allow-unenforced).
	AllowUnenforced bool
	// AllowUnenforcedNetwork is the CNI escape hatch (--allow-unenforced-network).
	AllowUnenforcedNetwork bool
	// MemoryStorageClass is the memory PVC's StorageClass (--memory-storage-class;
	// empty = the cluster default).
	MemoryStorageClass string
	// MemoryWriterImage is the P5b memory-writer Job image
	// (--memory-writer-image; empty = the workspace git image).
	MemoryWriterImage string
	// ClusterDomain is the cluster's service DNS domain (--cluster-domain;
	// empty = cluster.local).
	ClusterDomain string
	// WorkspaceGitImage is the workspace init container image
	// (--workspace-git-image).
	WorkspaceGitImage string
	// RunnerImage is the runner image the operator recognises
	// (--runner-image).
	RunnerImage string
	// VerifyImage is the verify Job's default check image (--verify-image).
	VerifyImage string
	// ToolProxyImage is the tool proxy pods' image (--tool-proxy-image;
	// empty = the reconciler's dev stand-in).
	ToolProxyImage string
	// EgressProxyImage is the egress proxy sidecar image
	// (--egress-proxy-image; empty = the reconciler's dev stand-in).
	EgressProxyImage string
	// ProxyImage is the model proxy pods' image (--proxy-image; empty =
	// the reconciler's coxswain-proxy:metering).
	ProxyImage string
	// OperatorNamespace is the operator's namespace
	// (--operator-namespace, default $POD_NAMESPACE).
	OperatorNamespace string
}

// newLoopReconciler builds the LoopReconciler from the manager's client and
// scheme, the manager's event recorder, the flag-derived options, and the
// operator-side seams main wires up first (the KubeArmor enforcer, the CNI
// prober, the CNI re-gate source, the alert relay holder). main calls this
// and then SetupWithManager.
func newLoopReconciler(c client.Client, scheme *runtime.Scheme,
	recorder record.EventRecorder, o newLoopReconcilerOptions,
	kaEnforcer *engine.KubeArmorEnforcer,
	cniProber cni.CNIProber,
	cniRegateSource source.TypedSource[reconcile.Request],
	audit *alerts.Holder,
) *controller.LoopReconciler {
	return &controller.LoopReconciler{
		Client:                 c,
		Scheme:                 scheme,
		Enforcer:               kaEnforcer,
		AllowUnenforced:        o.AllowUnenforced,
		AllowUnenforcedNetwork: o.AllowUnenforcedNetwork,
		MemoryStorageClass:     o.MemoryStorageClass,
		// D76/D102: the Secret opt-in gate (--secret-opt-in, fail-closed by
		// default). The cmd test asserts the default-flag mapping so a
		// missed wiring fails a test, not a production install.
		SecretOptInRequired: o.SecretOptInRequired,
		MemoryWriterImage:   o.MemoryWriterImage,
		CNIProber:           cniProber,
		CNIRegateSource:     cniRegateSource,
		Audit:               audit,
		ClusterDomain:       o.ClusterDomain,
		WorkspaceGitImage:   o.WorkspaceGitImage,
		RunnerImage:         o.RunnerImage,
		VerifyImage:         o.VerifyImage,
		ToolProxyImage:      o.ToolProxyImage,
		EgressProxyImage:    o.EgressProxyImage,
		// P2b (ADR-0009): the metering model proxy image (default empty ->
		// the reconciler's coxswain-proxy:metering) and the operator's
		// namespace (the proxy pod's usage-port ingress source).
		ProxyImage:        o.ProxyImage,
		OperatorNamespace: o.OperatorNamespace,
		// D38: the NetworkEnforced condition-change Event (the manager's
		// recorder posts it as a Kubernetes Event; the re-gate Event lives
		// in the probe Runnable).
		// mgr.GetEventRecorderFor is deprecated in controller-runtime v0.25
		// in favour of mgr.GetEventRecorder (new events.k8s.io/v1 API) — but
		// that returns the new events.EventRecorder (AnnotatedEventf) whose
		// method set differs from LoopReconciler.Recorder (client-go
		// record.EventRecorder). Keep the old (v1 Event, still fully
		// supported) API until the recorder is ported.
		Recorder: recorder,
	}
}
