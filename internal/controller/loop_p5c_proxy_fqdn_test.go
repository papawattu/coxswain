package controller

import "testing"

func TestSvcNamespaceFromFQDN(t *testing.T) {
	cases := []struct {
		host string
		ns   string
		ok   bool
	}{
		{"p5c-standin-llm.dogfood-dogfood3.svc.cluster.local", "dogfood-dogfood3", true},
		{"p5c-standin-llm.dogfood-dogfood3.svc", "dogfood-dogfood3", true},
		{"vllm.prod.svc.cluster.local", "prod", true},
		{"github.com", "", false},
		{"192.168.1.20", "", false},
		{"singlelabel", "", false},
		{"a.b.c.d.svc.cluster.local", "", false},
		{"my-svc.my-namespace.svc", "my-namespace", true},
	}
	for _, c := range cases {
		got, ok := svcNamespaceFromFQDN(c.host)
		if ok != c.ok || got != c.ns {
			t.Errorf("svcNamespaceFromFQDN(%q) = (%q, %v), want (%q, %v)", c.host, got, ok, c.ns, c.ok)
		}
	}
}
