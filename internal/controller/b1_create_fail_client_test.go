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

package controller

// b1CreateFailClient makes the FIRST Create of a VolumeSnapshot return an
// injected error (the B1 "first create fails" spec: the reconciler's create
// path gets a real non-AlreadyExists error, so the CreateFailed branch runs).
// Every other call passes through to the inner client. It implements
// client.Client by delegation (interceptor.NewClient needs a WithWatch, which
// the envtest plain client does not provide — the conflictOnceClient idiom).

import (
	"context"
	"sync"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// b1Err is a plain error (not an apierrors.StatusError), so the operator's
// AlreadyExists/NotFound checks treat it as a generic failure.
type b1Err struct{}

func (b1Err) Error() string {
	return "injected: first VolumeSnapshot create fails (b1CreateFailClient)"
}

var errB1InjectedCreate = b1Err{}

type b1CreateFailClient struct {
	inner client.Client
	mu    sync.Mutex
	left  int
}

// B1CreateFailClient wraps inner so the next n VolumeSnapshot Creates fail.
func B1CreateFailClient(inner client.Client, n int) client.Client {
	return &b1CreateFailClient{inner: inner, left: n}
}

func (c *b1CreateFailClient) shouldFail(obj client.Object) bool {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return false
	}
	return u.GetAPIVersion() == snapshotGV && u.GetKind() == volumeSnapshotKind
}

func (c *b1CreateFailClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if c.shouldFail(obj) {
		c.mu.Lock()
		if c.left > 0 {
			c.left--
			c.mu.Unlock()
			return errB1InjectedCreate
		}
		c.mu.Unlock()
	}
	return c.inner.Create(ctx, obj, opts...)
}

// The remaining client.Client methods delegate (pass-through).

func (c *b1CreateFailClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return c.inner.Get(ctx, key, obj, opts...)
}

func (c *b1CreateFailClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return c.inner.List(ctx, list, opts...)
}

func (c *b1CreateFailClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	return c.inner.Delete(ctx, obj, opts...)
}

func (c *b1CreateFailClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	return c.inner.Update(ctx, obj, opts...)
}

func (c *b1CreateFailClient) Patch(ctx context.Context, obj client.Object, data client.Patch, opts ...client.PatchOption) error {
	return c.inner.Patch(ctx, obj, data, opts...)
}

func (c *b1CreateFailClient) DeleteAllOf(ctx context.Context, obj client.Object, opts ...client.DeleteAllOfOption) error {
	return c.inner.DeleteAllOf(ctx, obj, opts...)
}

func (c *b1CreateFailClient) Apply(ctx context.Context, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
	return c.inner.Apply(ctx, obj, opts...)
}

func (c *b1CreateFailClient) Status() client.SubResourceWriter {
	return c.inner.Status()
}

// Watch delegates (the envtest client provides it); it also satisfies
// client.WithWatch, so this wrapper can be used anywhere a plain Client is.
func (c *b1CreateFailClient) Watch(ctx context.Context, obj client.ObjectList, opts ...client.ListOption) (watch.Interface, error) {
	inner, ok := c.inner.(interface {
		Watch(ctx context.Context, obj client.ObjectList, opts ...client.ListOption) (watch.Interface, error)
	})
	if !ok {
		panic("b1CreateFailClient: inner client does not support Watch")
	}
	return inner.Watch(ctx, obj, opts...)
}

func (c *b1CreateFailClient) SubResource(subResource string) client.SubResourceClient {
	return c.inner.SubResource(subResource)
}

func (c *b1CreateFailClient) GroupVersionKindFor(obj runtime.Object) (schema.GroupVersionKind, error) {
	return c.inner.GroupVersionKindFor(obj)
}

func (c *b1CreateFailClient) Scheme() *runtime.Scheme {
	return c.inner.Scheme()
}

func (c *b1CreateFailClient) RESTMapper() meta.RESTMapper {
	return c.inner.RESTMapper()
}

func (c *b1CreateFailClient) IsObjectNamespaced(obj runtime.Object) (bool, error) {
	return c.inner.IsObjectNamespaced(obj)
}

// compile-time check.
var _ client.WithWatch = (*b1CreateFailClient)(nil)
