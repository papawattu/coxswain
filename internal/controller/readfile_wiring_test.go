package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// exposingClient is a client.Client wrapper that ALSO exposes ReadFile —
// the shape the live manager client has (client.New returns a concrete
// client with ReadFile). It proves SetupWithManager's type assertion wires
// the seam when the client exposes the method.
type exposingClient struct {
	client.Client
}

func (e *exposingClient) ReadFile(ctx context.Context, pod *corev1.Pod, path string) ([]byte, error) {
	return []byte("1cedc3c58775f92ccac9bd2ec52defe58a019f09\n"), nil
}

// The concrete controller-runtime client implements ReadFile, which is NOT
// on the client.Client interface. SetupWithManager's type assertion must
// recover it when the client exposes the method, and must NOT wire the seam
// when it does not (the dead-seam failure observed live on kind 2026-10-02:
// the assertion never succeeded against the manager's client, the seam
// stayed nil, and status.baseCommit was silently empty forever even though
// the init container wrote the file).
func TestBaseCommitSeamWiring(t *testing.T) {
	sc := runtime.NewScheme()
	utilruntime.Must(coxv1alpha1.AddToScheme(sc))
	utilruntime.Must(scheme.AddToScheme(sc))

	// (1) A client that exposes ReadFile MUST pass the assertion and wire
	// the seam.
	e := &exposingClient{Client: fake.NewClientBuilder().WithScheme(sc).Build()}
	rc, ok := client.Client(e).(interface {
		ReadFile(ctx context.Context, pod *corev1.Pod, path string) ([]byte, error)
	})
	if !ok {
		t.Fatal("a client that exposes ReadFile did not pass the SetupWithManager type assertion")
	}
	data, err := rc.ReadFile(context.Background(),
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"}},
		"/workspace/.coxswain/base-commit")
	if err != nil {
		t.Fatalf("ReadFile via the wired seam: %v", err)
	}
	if string(data) != "1cedc3c58775f92ccac9bd2ec52defe58a019f09\n" {
		t.Fatalf("seam read = %q", string(data))
	}

	// (2) The readfileClient wrap (the envtest suite's k8sClient) delegates
	// to the underlying client's ReadFile when it has one, and errors
	// (not silently skips) when it does not.
	w := NewReadfileClient(e)
	_, err = w.ReadFile(context.Background(),
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"}},
		"/workspace/.coxswain/base-commit")
	if err != nil {
		t.Fatalf("readfileClient delegation: %v", err)
	}
	plain := fake.NewClientBuilder().WithScheme(sc).Build()
	w2 := NewReadfileClient(plain)
	if _, err := w2.ReadFile(context.Background(),
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"}},
		"/workspace/.coxswain/base-commit"); err == nil {
		t.Fatal("readfileClient over a client WITHOUT ReadFile must error, not silently skip")
	}
}
