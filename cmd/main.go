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

package main

import (
	"crypto/tls"
	"flag"
	"os"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/cni"
	"github.com/papawattu/coxswain/internal/controller"
	"github.com/papawattu/coxswain/internal/engine"
	"github.com/papawattu/coxswain/internal/policy"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(coxv1alpha1.AddToScheme(scheme))
	utilruntime.Must(sandboxv1beta1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var webhookPort int
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.IntVar(&webhookPort, "webhook-port", 9443, "Port the webhook server listens on. "+
		"Defaults to 9443. Set -1 to disable the webhook server.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	var allowUnenforced bool
	flag.BoolVar(&allowUnenforced, "allow-unenforced", false,
		"Run Loops even when the eBPF engine is not enforcing (off by default; dev escape hatch). "+
			"Loops run with PolicyEnforced=False reason EnforcementDisabled.")
	// D38: the network escape hatch (separate from allow-unenforced — the two
	// are independent) and the probe configuration.
	var allowUnenforcedNetwork bool
	flag.BoolVar(&allowUnenforcedNetwork, "allow-unenforced-network", false,
		"Run Loops even when the CNI does not police pod -> host-network egress (off by default; dev escape hatch). "+
			"Loops run with NetworkEnforced=False reason EnforcementDisabled.")
	var cniCheckInterval time.Duration
	flag.DurationVar(&cniCheckInterval, "cni-check-interval", 10*time.Minute,
		"How often the leader-elected probe Runnable re-probes the CNI self-test (D38).")
	var cniProbeTimeout time.Duration
	flag.DurationVar(&cniProbeTimeout, "cni-probe-timeout", 60*time.Second,
		"Per-run timeout for one CNI probe; not-Ready / no exit / pull failure within it = ProbeUnavailable (D38).")
	var cniProbeImage string
	flag.StringVar(&cniProbeImage, "cni-probe-image", "python:3-alpine",
		"The image the CNI probe pod runs (D38). Must be pullable by the probe node.")
	var cniProbeNamespace string
	flag.StringVar(&cniProbeNamespace, "cni-probe-namespace", "coxswain-cni-probe",
		"The fixed namespace the CNI probe pod + NetworkPolicy live in (D38; created at install).")
	var clusterDomain string
	flag.StringVar(&clusterDomain, "cluster-domain", "",
		"The cluster's service DNS domain (default cluster.local). Used for the proxy Service FQDNs the agent's DNS "+
			"allowlist carries. R16 I44 item 2: a non-default-domain cluster (a DNS-domain override on the cluster's "+
			"service CIDR) is supported.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts
	webhookServerOptions := webhook.Options{
		TLSOpts: webhookTLSOpts,
		Port:    webhookPort,
	}

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	webhookServer := webhook.NewServer(webhookServerOptions)

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.25.0/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.25.0/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Cache: cache.Options{
			// Scope the Pod and Service cache to the operator's two proxy
			// components (the model proxy and the egress proxy) so the
			// operator doesn't cache every Pod and Service in the cluster
			// (P2, R15 review on PR #12; widened for the egress proxy in
			// I42b). The selector is policy.ProxyComponentSelector
			// (internal/policy) so it is tested against the label sets the
			// operator owns — a component the operator Gets through this cache
			// but that is missing here is a cache miss that turns into a
			// spurious Create (AlreadyExists) on the first reconcile of that
			// resource.
			ByObject: map[client.Object]cache.ByObject{
				&corev1.Pod{}:     {Label: policy.ProxyComponentSelector()},
				&corev1.Service{}: {Label: policy.ProxyComponentSelector()},
			},
		},
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "a54de9d2.coxswain.wattu.com",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	// R16 I44 item 1+2: the KubeArmorEnforcer is configured ONCE at
	// construction with the proxy Service FQDN functions (built from the
	// controller's proxyServiceName/egressProxyServiceName naming) and the
	// cluster domain. The enforcer must not build the FQDNs from a literal or
	// hard-code the domain, and (review #30 P2) it must not be mutated per
	// reconcile (a shared-enforcer mutation races if the controller's
	// concurrency is raised).
	kaEnforcer := &engine.KubeArmorEnforcer{
		Client:          mgr.GetClient(),
		ProxyFQDN:       controller.ProxyServiceFQDN,
		EgressProxyFQDN: controller.EgressProxyServiceFQDN,
		ClusterDomain:   clusterDomain,
		// A missing KubeArmor CRD is a loud error in production; tolerated
		// (no-op) only under the --allow-unenforced dev escape hatch (D38).
		AllowUnenforced: allowUnenforced,
	}
	// D38: the operator-side CNI self-test prober. It runs the probe pod in the
	// fixed coxswain-cni-probe namespace, reads the termination message, and
	// caches the result in the shared holder. The leader-elected probe Runnable
	// (registered below) calls Probe on an interval; the reconcile loop reads
	// the cached result via LatestResult (never runs the probe).
	cniProber := cni.NewPodProber(cni.PodProberConfig{
		Client:        mgr.GetClient(),
		Namespace:     cniProbeNamespace,
		ProbeImage:    cniProbeImage,
		ProbeTimeout:  cniProbeTimeout,
		ClusterDomain: clusterDomain,
	})
	// D38: the leader-elected probe Runnable (first probe at startup, then
	// every cniCheckInterval). It re-gates every Loop (via a K8s Event + a
	// source.Channel GenericEvent) when the result changes. The returned source
	// is wired into the Loop controller (below) via the CNIRegateSource field
	// so a result change enqueues every Loop.
	cniRegateSrc, err := cni.AddProbeRunnable(mgr, cniProber, cniCheckInterval, cniProbeTimeout)
	if err != nil {
		setupLog.Error(err, "Failed to register the CNI probe Runnable")
		os.Exit(1)
	}
	if err := (&controller.LoopReconciler{
		Client:                 mgr.GetClient(),
		Scheme:                 mgr.GetScheme(),
		Enforcer:               kaEnforcer,
		AllowUnenforced:        allowUnenforced,
		AllowUnenforcedNetwork: allowUnenforcedNetwork,
		CNIProber:              cniProber,
		CNIRegateSource:        cniRegateSrc,
		ClusterDomain:          clusterDomain,
		// D38: the NetworkEnforced condition-change Event (the manager's
		// recorder posts it as a Kubernetes Event; the re-gate Event lives
		// in the probe Runnable).
		// GetEventRecorderFor is deprecated in controller-runtime v0.25 in favour
		// of GetEventRecorder (new events.k8s.io/v1 API) — but that returns the
		// new events.EventRecorder (AnnotatedEventf) whose method set differs
		// from LoopReconciler.Recorder (client-go record.EventRecorder). Keep the
		// old (v1 Event, still fully supported) API until the recorder is ported.
		Recorder: mgr.GetEventRecorderFor("loop-controller"), //nolint:staticcheck
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "loop")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}
