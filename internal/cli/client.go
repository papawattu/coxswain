package cli

import (
	"context"
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// loopGVR is the Loop resource's GroupVersionResource (the CRD group in
// api/v1alpha1).
var loopGVR = schema.GroupVersionResource{
	Group:    "coxswain.wattu.com",
	Version:  "v1alpha1",
	Resource: "loops",
}

// buildConfig builds a rest.Config from the kubeconfig path and context
// (empty = the ambient context).
func buildConfig(kubeconfigPath, kubeContext string) (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfigPath != "" {
		rules.ExplicitPath = kubeconfigPath
	}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: kubeContext}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
}

// dynamicLoopClient is the real LoopClient: a client-go dynamic client for the
// Loop CRD. It uses *unstructured.Unstructured so it does not need a generated
// typed clientset (the CLI is dep-light — CLI-PLAN.md).
type dynamicLoopClient struct {
	ns string
	c  dynamic.Interface
}

// Get returns the named Loop as a *unstructured.Unstructured (or a
// not-found error).
func (d *dynamicLoopClient) Get(ctx context.Context, name string) (runtime.Object, error) {
	return d.c.Resource(loopGVR).Namespace(d.ns).Get(ctx, name, metav1.GetOptions{})
}

// Create applies a NEW Loop object (the main resource; the dynamic client's
// Create issues a POST). A new object must NOT go through Update, which PUTs
// to the STATUS subresource and 404s for a Loop that does not exist yet —
// the B6 run 12's `cox fork` failure: forkLoop built a new Loop object and
// applied it via Loops.Update, so the operator never saw it.
func (d *dynamicLoopClient) Create(ctx context.Context, obj runtime.Object) error {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("cli: expected *unstructured.Unstructured, got %T", obj)
	}
	if u.GetKind() == "" {
		u.SetKind("Loop")
	}
	if u.GetAPIVersion() == "" {
		u.SetAPIVersion("coxswain.wattu.com/v1alpha1")
	}
	_, err := d.c.Resource(loopGVR).Namespace(d.ns).Create(ctx, u, metav1.CreateOptions{})
	return err
}

// Patch applies a JSON merge patch to the named Loop. The subresource is ""
// for the main resource (spec.suspend, metadata.annotations) and "status"
// for the status subresource (the approve/reject conditions — F87).
func (d *dynamicLoopClient) Patch(ctx context.Context, name, subresource string, patch []byte) error {
	_, err := d.c.Resource(loopGVR).Namespace(d.ns).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{}, subresource)
	return err
}

// Update writes the whole object to the status subresource. Nothing in the CLI
// uses it for approvals any more (F87 moved cox approve/reject to a merge patch
// of status.conditions, see approval.go); it is kept because the LoopClient
// interface still offers it and `fork`'s spec must keep proving it does NOT go
// through Update.
//
// The pre-F87 history of this method: a main-resource Update does not persist
// status (the server resets it), and a bare-status body to the status
// subresource is rejected ("must be of type Loop: Kind is missing"), so a
// whole-object PUT to /status was the only working write — a kind experiment
// recorded that it returns 200 and persists the condition. That experiment is
// retired: a merge patch of status.conditions is the write path.
func (d *dynamicLoopClient) Update(ctx context.Context, obj runtime.Object) error {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("cli: expected *unstructured.Unstructured, got %T", obj)
	}
	// Ensure the object carries its kind (a status-subresource PUT validates the
	// full object shape).
	if u.GetKind() == "" {
		u.SetKind("Loop")
	}
	if u.GetAPIVersion() == "" {
		u.SetAPIVersion("coxswain.wattu.com/v1alpha1")
	}
	_, err := d.c.Resource(loopGVR).Namespace(d.ns).Update(ctx, u, metav1.UpdateOptions{}, "status")
	return err
}

// mergePatchType is the JSON merge patch content type.
const mergePatchType = "application/merge-patch+json"

var _ = mergePatchType

// mustJSON marshals a value to a JSON byte slice (panic on error — the
// commands build well-formed patches).
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// NewDeps builds the real client-go dependencies from the CLI's --namespace /
// --context / --kubeconfig flags. It is called by the two mains after cobra
// parses the persistent flags.
func NewDeps() (Deps, error) {
	cfg, err := buildConfig(Kubeconfig, Context)
	if err != nil {
		return Deps{}, err
	}
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return Deps{}, err
	}
	ns := Namespace
	if ns == "" {
		cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{CurrentContext: Context})
		if n, _, err := cc.Namespace(); err == nil && n != "" {
			ns = n
		} else {
			ns = "default"
		}
	}
	return Deps{
		Loops:     &dynamicLoopClient{ns: ns, c: dc},
		Logs:      &kubectlLogStreamer{ns: ns},
		Workspace: &peekWorkspaceReader{ns: ns, Loops: &dynamicLoopClient{ns: ns, c: dc}},
	}, nil
}
