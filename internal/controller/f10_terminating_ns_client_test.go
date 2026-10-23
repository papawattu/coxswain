package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// f10TerminatingNSClient wraps an inner client so that List and Delete of
// VolumeSnapshots return a terminating-namespace Forbidden (simulating the
// live F10 repro: a namespace being terminated rejects creates and fails
// reads). Everything else passes through. The F10 fix must treat this
// Forbidden as "done" (the children go away with the namespace) and remove
// the checkpoint finalizer, so the namespace can complete its delete.
//
// This is the controlled equivalent of the live dogfood4-df4 repro: envtest
// cannot put a namespace into terminating state and make the apiserver
// reject creates the way a real terminating namespace does, so the spec drives
// the REAL Reconcile (the deleting-Loop path) with a client whose VolumeSnapshot
// list/delete return the Forbidden.
type f10TerminatingNSClient struct {
	inner client.Client
}

// terminatingNSForbidden is a StatusError the apiserver returns when a
// request is rejected because the namespace is terminating. apierrors.IsForbidden
// recognizes it (a *apierrors.StatusError with StatusReasonForbidden), so the
// operator's isForbidden check treats the terminating-ns list/delete as
// "done" (the children go away with the namespace) and removes the finalizer.
var terminatingNSForbidden = apierrors.NewForbidden(
	schema.GroupResource{Group: "snapshot.storage.k8s.io", Resource: "volumesnapshots"}, "",
	fmt.Errorf("unable to create new content in namespace \"dogfood-dogfood4\" because it is being terminated"))

// isVolumeSnapshotList reports whether the list is an unstructured
// VolumeSnapshot list (the gcLoopCheckpoints list is unstructured).
func isVolumeSnapshotList(list client.ObjectList) bool {
	u, ok := list.(*unstructured.UnstructuredList)
	if !ok {
		return false
	}
	return u.GetKind() == "VolumeSnapshotList" || u.GetKind() == "VolumeSnapshot"
}

// isVolumeSnapshot reports whether the object is a VolumeSnapshot
// (unstructured, the gcLoopCheckpoints delete path).
func isVolumeSnapshotObj(obj client.Object) bool {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return false
	}
	return u.GetKind() == "VolumeSnapshot"
}

func (c *f10TerminatingNSClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if isVolumeSnapshotList(list) {
		return terminatingNSForbidden
	}
	return c.inner.List(ctx, list, opts...)
}

func (c *f10TerminatingNSClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if isVolumeSnapshotObj(obj) {
		return terminatingNSForbidden
	}
	return c.inner.Delete(ctx, obj, opts...)
}

func (c *f10TerminatingNSClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return c.inner.Get(ctx, key, obj, opts...)
}

func (c *f10TerminatingNSClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	return c.inner.Create(ctx, obj, opts...)
}

func (c *f10TerminatingNSClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	return c.inner.Update(ctx, obj, opts...)
}

func (c *f10TerminatingNSClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	return c.inner.Patch(ctx, obj, patch, opts...)
}

func (c *f10TerminatingNSClient) DeleteAllOf(ctx context.Context, obj client.Object, opts ...client.DeleteAllOfOption) error {
	return c.inner.DeleteAllOf(ctx, obj, opts...)
}

func (c *f10TerminatingNSClient) Apply(ctx context.Context, cfg runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
	return c.inner.Apply(ctx, cfg, opts...)
}

func (c *f10TerminatingNSClient) Status() client.SubResourceWriter {
	return c.inner.Status()
}

func (c *f10TerminatingNSClient) Watch(ctx context.Context, obj client.ObjectList, opts ...client.ListOption) (watch.Interface, error) {
	inner, ok := c.inner.(interface {
		Watch(ctx context.Context, obj client.ObjectList, opts ...client.ListOption) (watch.Interface, error)
	})
	if !ok {
		panic("f10TerminatingNSClient: inner client does not support Watch")
	}
	return inner.Watch(ctx, obj, opts...)
}

func (c *f10TerminatingNSClient) SubResource(subResource string) client.SubResourceClient {
	return c.inner.SubResource(subResource)
}

func (c *f10TerminatingNSClient) GroupVersionKindFor(obj runtime.Object) (schema.GroupVersionKind, error) {
	return c.inner.GroupVersionKindFor(obj)
}

func (c *f10TerminatingNSClient) Scheme() *runtime.Scheme {
	return c.inner.Scheme()
}

func (c *f10TerminatingNSClient) RESTMapper() meta.RESTMapper {
	return c.inner.RESTMapper()
}

func (c *f10TerminatingNSClient) IsObjectNamespaced(obj runtime.Object) (bool, error) {
	return c.inner.IsObjectNamespaced(obj)
}

// f10TerminatingNSUpdateClient wraps an inner client so that BOTH the
// VolumeSnapshot list/delete AND the Loop Update (the finalizer removal)
// return a terminating-namespace Forbidden. This simulates the FULL live
// F10 repro: a namespace being terminated rejects creates AND reads AND
// updates. The F10 fix must:
//   - tolerate the VolumeSnapshot Forbidden (the GC is "done")
//   - tolerate the finalizer-removal Update Forbidden (surface it as an Event
//     and requeue with a targeted RequeueAfter, NOT the error backoff cap)
type f10TerminatingNSUpdateClient struct {
	inner client.Client
}

var loopForbidden = apierrors.NewForbidden(
	schema.GroupResource{Group: "coxswain.wattu.com", Resource: "loops"}, "",
	fmt.Errorf("unable to update in namespace \"dogfood-dogfood4\" because it is being terminated"))

func (c *f10TerminatingNSUpdateClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if isVolumeSnapshotList(list) {
		return terminatingNSForbidden
	}
	return c.inner.List(ctx, list, opts...)
}

func (c *f10TerminatingNSUpdateClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if isVolumeSnapshotObj(obj) {
		return terminatingNSForbidden
	}
	return c.inner.Delete(ctx, obj, opts...)
}

func (c *f10TerminatingNSUpdateClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	// Reject the Loop Update ONLY when the Loop is being deleted (its
	// DeletionTimestamp is set) — the finalizer-removal Update. The
	// finalizer-ADDING Update (the Loop is not yet deleted) passes through.
	if loop, ok := obj.(*coxv1alpha1.Loop); ok && !loop.DeletionTimestamp.IsZero() {
		return loopForbidden
	}
	return c.inner.Update(ctx, obj, opts...)
}

func (c *f10TerminatingNSUpdateClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return c.inner.Get(ctx, key, obj, opts...)
}

func (c *f10TerminatingNSUpdateClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	return c.inner.Create(ctx, obj, opts...)
}

func (c *f10TerminatingNSUpdateClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	return c.inner.Patch(ctx, obj, patch, opts...)
}

func (c *f10TerminatingNSUpdateClient) DeleteAllOf(ctx context.Context, obj client.Object, opts ...client.DeleteAllOfOption) error {
	return c.inner.DeleteAllOf(ctx, obj, opts...)
}

func (c *f10TerminatingNSUpdateClient) Apply(ctx context.Context, cfg runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
	return c.inner.Apply(ctx, cfg, opts...)
}

func (c *f10TerminatingNSUpdateClient) Status() client.SubResourceWriter {
	return c.inner.Status()
}

func (c *f10TerminatingNSUpdateClient) SubResource(subResource string) client.SubResourceClient {
	return c.inner.SubResource(subResource)
}

func (c *f10TerminatingNSUpdateClient) GroupVersionKindFor(obj runtime.Object) (schema.GroupVersionKind, error) {
	return c.inner.GroupVersionKindFor(obj)
}

func (c *f10TerminatingNSUpdateClient) IsObjectNamespaced(obj runtime.Object) (bool, error) {
	return c.inner.IsObjectNamespaced(obj)
}

func (c *f10TerminatingNSUpdateClient) RESTMapper() meta.RESTMapper {
	return c.inner.RESTMapper()
}

func (c *f10TerminatingNSUpdateClient) Scheme() *runtime.Scheme {
	return c.inner.Scheme()
}

// f25NoMatchClient makes every VolumeSnapshot list fail with the RESTMapper's
// NoKindMatchError, which is what a client returns for a kind whose CRD is not
// installed (F25: hermes has no snapshot CRDs). Everything else passes through.
type f25NoMatchClient struct {
	client.Client
}

func (c *f25NoMatchClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if isVolumeSnapshotList(list) {
		return &meta.NoKindMatchError{
			GroupKind:        schema.GroupKind{Group: "snapshot.storage.k8s.io", Kind: "VolumeSnapshot"},
			SearchedVersions: []string{"v1"},
		}
	}
	return c.Client.List(ctx, list, opts...)
}
