package controller

import "testing"

// TestServiceNamespaceFromHostDomainAware covers the domain-aware Service-FQDN
// extraction that modelPeer uses (via r.serviceNamespaceFromHost): the
// minimal same-cluster form (svc.ns.svc) and the full form with the
// reconciler's cluster domain, including a NON-DEFAULT cluster domain (a
// hardcoded cluster.local would fail these).
func TestServiceNamespaceFromHostDomainAware(t *testing.T) {
	cases := []struct {
		domain string
		host   string
		ns     string
		ok     bool
	}{
		// Default domain (cluster.local).
		{"", "p5c-standin-llm.dogfood-dogfood3.svc.cluster.local", "dogfood-dogfood3", true},
		{"", "p5c-standin-llm.dogfood-dogfood3.svc", "dogfood-dogfood3", true},
		{"", "vllm.prod.svc.cluster.local", "prod", true},
		// Non-default domain.
		{"corp.example", "llm.mlops.svc.corp.example", "mlops", true},
		{"corp.example", "llm.mlops.svc", "mlops", true},
		// External hosts / IPs / bare labels: fail closed.
		{"corp.example", "github.com", "", false},
		{"corp.example", "192.168.1.20", "", false},
		{"corp.example", "singlelabel", "", false},
		// A full FQDN with the WRONG domain is not a same-cluster Service.
		{"corp.example", "llm.mlops.svc.cluster.local", "", false},
		// A namespace is a DNS label: no second dot allowed.
		{"", "a.b.c.d.svc.cluster.local", "", false},
	}
	for _, c := range cases {
		r := &LoopReconciler{CNIProber: testCNIProber()}
		if c.domain != "" {
			r.ClusterDomain = c.domain
		}
		got, ok := r.serviceNamespaceFromHost(c.host)
		if ok != c.ok || got != c.ns {
			t.Errorf("serviceNamespaceFromHost(%q, domain %q) = (%q, %v), want (%q, %v)", c.host, c.domain, got, ok, c.ns, c.ok)
		}
	}
}

// TestModelPeerServiceFQDN uses modelPeer end-to-end with a non-default
// cluster domain: a full-FQDN endpoint in that domain must yield a
// namespaceSelector over the Service's namespace; an endpoint in the
// default domain must be rejected (fail closed) on that cluster.
func TestModelPeerServiceFQDNNonDefaultDomain(t *testing.T) {
	r := &LoopReconciler{CNIProber: testCNIProber(), ClusterDomain: "corp.example"}
	peer := modelPeer("http://llm.mlops.svc.corp.example:8000/v1/chat/completions", r.serviceNamespaceFromHost)
	if peer == nil || peer.NamespaceSelector == nil {
		t.Fatalf("modelPeer(non-default domain FQDN) = %+v, want namespaceSelector peer", peer)
	}
	if ns := peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]; ns != "mlops" {
		t.Errorf("namespaceSelector label = %q, want %q", ns, "mlops")
	}
	// A default-domain FQDN must not match a non-default-domain cluster.
	if p := modelPeer("http://llm.mlops.svc.cluster.local:8000/v1", r.serviceNamespaceFromHost); p != nil {
		t.Errorf("modelPeer(default-domain FQDN on corp.example cluster) = %+v, want nil (fail closed)", p)
	}
}

// TestModelPeerNonDefaultDomainFQDN isolates the domain the model endpoint
// FQDN is matched against: on a non-default cluster domain, a full-FQDN
// endpoint in THAT domain must be recognised. (With the hardcoded
// cluster.local parser — the pre-fix svcNamespaceFromFQDN behaviour — this
// fails: the FQDN is not recognised and the peer is nil, fail-closed.)
func TestModelPeerNonDefaultDomainFQDN(t *testing.T) {
	r := &LoopReconciler{CNIProber: testCNIProber(), ClusterDomain: "corp.example"}
	peer := modelPeer("http://llm.mlops.svc.corp.example:8000", r.serviceNamespaceFromHost)
	if peer == nil || peer.NamespaceSelector == nil {
		t.Fatalf("modelPeer(%q, domain corp.example) = nil, want namespaceSelector", "llm.mlops.svc.corp.example")
	}
	if ns := peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]; ns != "mlops" {
		t.Errorf("namespaceSelector label = %q, want %q", ns, "mlops")
	}
}
