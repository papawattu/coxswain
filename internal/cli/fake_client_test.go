package cli

import (
	"context"
	"sync"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// fakeLoopClient is the test LoopClient (the TDD seam's fake, CLI-PLAN.md). It
// holds one Loop object and records Patches/Updates so tests assert on the
// calls.
type fakeLoopClient struct {
	mu sync.Mutex

	loop *unstructured.Unstructured // the object Get returns (nil = not found)

	// patches records the (subresource, patch-bytes) of every Patch call.
	patches []patchCall
	// updates records the whole object of every Update call.
	updates []runtime.Object

	// patchResult / getResult are injected errors (nil = success).
	patchResult error
	getResult   error
}

type patchCall struct {
	name        string
	subresource string
	patch       []byte
}

func newFakeLoopClient() *fakeLoopClient {
	return &fakeLoopClient{}
}

// setLoop sets the object Get returns.
func (f *fakeLoopClient) setLoop(obj *unstructured.Unstructured) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loop = obj
}

func (f *fakeLoopClient) Get(_ context.Context, name string) (runtime.Object, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getResult != nil {
		return nil, f.getResult
	}
	if f.loop == nil {
		return nil, &notFoundError{name: name}
	}
	return f.loop, nil
}

func (f *fakeLoopClient) Patch(_ context.Context, name, subresource string, patch []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.patchResult != nil {
		return f.patchResult
	}
	f.patches = append(f.patches, patchCall{name: name, subresource: subresource, patch: patch})
	return nil
}

func (f *fakeLoopClient) Update(_ context.Context, obj runtime.Object) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, obj)
	return nil
}

// lastPatch returns the most recent Patch call (nil if none).
func (f *fakeLoopClient) lastPatch() *patchCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.patches) == 0 {
		return nil
	}
	return &f.patches[len(f.patches)-1]
}

// notFoundError is a minimal not-found error for the fake client.
type notFoundError struct{ name string }

func (e *notFoundError) Error() string { return "loop \"" + e.name + "\" not found" }
