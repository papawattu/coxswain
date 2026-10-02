package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	restclient "k8s.io/client-go/rest"
)

// newReadFileViaREST (the live-deployment fallback for the baseCommit
// read-back seam) must issue the kubelet Pod-read subrequest with the
// container and path query parameters. Without them the API server returns
// the pod object (or an error) instead of the file content, and
// status.baseCommit is never populated (observed live on kind 2026-10-02:
// the init container wrote the file and the pod was Ready, but the read
// silently went to the wrong endpoint and baseCommit stayed empty).
func TestNewReadFileViaRESTQueryParams(t *testing.T) {
	var gotQuery url.Values
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("1cedc3c58775f92ccac9bd2ec52defe58a019f09"))
	}))
	t.Cleanup(srv.Close)

	cfg := &restclient.Config{Host: srv.URL, Transport: srv.Client().Transport}
	readFile := newReadFileViaREST(cfg)

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "s3-accept", Name: "s3-accept-sandbox"}}
	data, err := readFile(context.Background(), pod, "/workspace/.coxswain/base-commit")
	if err != nil {
		t.Fatalf("readFile: %v", err)
	}
	if string(data) != "1cedc3c58775f92ccac9bd2ec52defe58a019f09" {
		t.Fatalf("readFile returned %q, want the file content", data)
	}
	const wantURLPath = "/api/v1/namespaces/s3-accept/pods/s3-accept-sandbox"
	if gotPath != wantURLPath {
		t.Fatalf("request path = %q, want %q", gotPath, wantURLPath)
	}
	const wantContainer = "agent"
	const wantPath = "/workspace/.coxswain/base-commit"
	if got := gotQuery.Get("container"); got != wantContainer {
		t.Errorf("container query param = %q, want %q (without it the kubelet read is not scoped to the agent container)", got, wantContainer)
	}
	if got := gotQuery.Get("path"); got != wantPath {
		t.Errorf("path query param = %q, want %q (without it the request targets the pod object, not the file)", got, wantPath)
	}
}
