package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

// configMapGVR is the (core) ConfigMap resource. The learn-summary ConfigMap
// (D119 = (b)) is read through the same dynamic client the CLI already has.
var configMapGVR = schema.GroupVersionResource{
	Group:    "",
	Version:  "v1",
	Resource: "configmaps",
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
	_, err := d.c.Resource(loopGVR).Namespace(d.ns).Update(ctx, u, metav1.UpdateOptions{}, statusSubresource)
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

// learnSummaryReader is the real LearnSummaryReader: it GETs the per-Loop
// learn-summary ConfigMap (<loop>-learn-summary) the operator keeps for a
// learn-mode Loop (D119 = (b), R51 I236) with the dynamic client. The
// records.jsonl data key holds one alerts.Record JSON line per distinct
// allow; the dropped key holds the decimal count of records refused at the
// bound.
type learnSummaryReader struct {
	ns string
	c  dynamic.Interface
}

// ReadLearnSummary GETs the learn-summary ConfigMap and returns its records
// (the records.jsonl data key) and the dropped count (the dropped data key;
// 0 when the key is absent or unparsable). A missing ConfigMap returns an
// error explaining what writes the ConfigMap and how to pass an operator log
// instead. The ConfigMap must carry a controller ownerReference to the Loop
// itself (the operator sets it, D119 step 2) so a ConfigMap planted by
// someone else in the namespace is refused (I251).
func (l *learnSummaryReader) ReadLearnSummary(ctx context.Context, namespace, loop string) (string, int64, error) {
	cm, err := l.c.Resource(configMapGVR).Namespace(namespace).Get(ctx, loop+"-learn-summary", metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return "", 0, fmt.Errorf("no learn summary for Loop %s/%s (ConfigMap %s-learn-summary not found): the operator writes it only for a learn-mode Loop (every referenced AgentPolicy auditOnly) once its sandbox has run; or pass -f <operator log>", namespace, loop, loop)
		}
		return "", 0, err
	}
	loopObj, err := l.c.Resource(loopGVR).Namespace(namespace).Get(ctx, loop, metav1.GetOptions{})
	if err != nil {
		return "", 0, fmt.Errorf("could not verify the owner of ConfigMap %s/%s-learn-summary: %w", namespace, loop, err)
	}
	loopUID, _, _ := unstructured.NestedString(loopObj.Object, "metadata", "uid")
	if !learnSummaryOwnedBy(cm, loopUID, loop) {
		return "", 0, fmt.Errorf("ConfigMap %s/%s-learn-summary is not owned by Loop %s/%s; refusing to use it (it was not written by the operator)", namespace, loop, namespace, loop)
	}
	obj := cm
	records, _, _ := unstructured.NestedString(obj.Object, "data", "records.jsonl")
	var dropped int64
	if s, ok2, _ := unstructured.NestedString(obj.Object, "data", "dropped"); ok2 {
		if n, err2 := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err2 == nil {
			dropped = n
		}
	}
	return records, dropped, nil
}

// learnSummaryOwnedBy reports whether the ConfigMap (unstructured) carries an
// ownerReference with controller: true, kind "Loop", name == loopName and a
// uid matching the Loop's uid. Every field must match — the name alone does
// not prove the operator wrote the map (I251).
func learnSummaryOwnedBy(cm runtime.Object, loopUID, loopName string) bool {
	u, ok := cm.(*unstructured.Unstructured)
	if !ok {
		return false
	}
	refs, found, _ := unstructured.NestedSlice(u.Object, "metadata", "ownerReferences")
	if !found {
		return false
	}
	for _, r := range refs {
		ref, ok := r.(map[string]any)
		if !ok {
			continue
		}
		kind, _ := ref["kind"].(string)
		name, _ := ref["name"].(string)
		uid, _ := ref["uid"].(string)
		ctl, _ := ref["controller"].(bool)
		if ctl && kind == "Loop" && name == loopName && uid == loopUID {
			return true
		}
	}
	return false
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
		Loops:        &dynamicLoopClient{ns: ns, c: dc},
		Logs:         &kubectlLogStreamer{ns: ns},
		Workspace:    &peekWorkspaceReader{ns: ns, Loops: &dynamicLoopClient{ns: ns, c: dc}},
		LearnSummary: &learnSummaryReader{ns: ns, c: dc},
	}, nil
}
