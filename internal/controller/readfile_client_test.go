package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// readfileClient wraps a client.Client to also expose ReadFile, which is NOT
// on the client.Client interface. The envtest suite installs it as k8sClient
// so that SetupWithManager's type assertion
//
//	mgr.GetClient().(interface{ ReadFile(...) })
//
// succeeds and the S3 baseCommit read-back seam is wired from the real
// controller-runtime client (the same path a live deployment uses). The
// method delegates to the underlying concrete client's ReadFile; in envtest
// there is no kubelet so a real read would fail, but the S3 specs that
// exercise the seam override r.readFile directly (and the gate-disabled
// check leaves it nil).
type readfileClient struct {
	client.Client
}

// NewReadfileClient wraps c. The wrap is only meaningful when c is the
// concrete controller-runtime client (client.New / fake.NewClientBuilder),
// which does implement ReadFile.
func NewReadfileClient(c client.Client) *readfileClient {
	return &readfileClient{Client: c}
}

// ReadFile implements the sub-interface that SetupWithManager asserts on.
func (r *readfileClient) ReadFile(ctx context.Context, pod *corev1.Pod, path string) ([]byte, error) {
	inner, ok := r.Client.(interface {
		ReadFile(ctx context.Context, pod *corev1.Pod, path string) ([]byte, error)
	})
	if !ok {
		return nil, errReadFileUnwired
	}
	return inner.ReadFile(ctx, pod, path)
}

type errReadFileUnwiredImpl struct{}

func (errReadFileUnwiredImpl) Error() string {
	return "readfileClient: underlying client does not expose ReadFile"
}

var errReadFileUnwired = errReadFileUnwiredImpl{}
