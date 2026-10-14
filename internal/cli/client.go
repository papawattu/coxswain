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

// Patch applies a JSON merge patch to the named Loop's main resource.
// (The CLI's commands patch only the main resource — spec.suspend and
// metadata.annotations. The subresource parameter is accepted for interface
// stability but is not used by the CLI's commands today.)
func (d *dynamicLoopClient) Patch(ctx context.Context, name, subresource string, patch []byte) error {
	_, err := d.c.Resource(loopGVR).Namespace(d.ns).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

// Update writes the whole object (used to set annotations / spec.suspend).
// Update writes the whole object. The Loop CRD has a status subresource, and the
// CLI's commands (approve/reject) write status.conditions. A main-resource Update
// does NOT persist status (the server resets status to the live value); a
// status-subresource PUT is rejected for a bare-status body ("must be of type Loop:
// Kind is missing"). The verified working path (the kind cluster: a full-object PUT
// to /status returns 200 and persists the condition) is a whole-object PUT to the
// status subresource. The CLI therefore sends the whole object (kind + status) to
// the status subresource.
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
