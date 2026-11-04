// D76 + D102 (R30, owner decisions 2026-11-01): every Secret a Loop names
// must be opted in before the operator mounts or presents it.
//
// The defect (R30 D76, a confused deputy): a Loop author names any Secret in
// the namespace, and the operator mounts it (the git-credential init clone,
// the tool proxies) and presents it to a host the SAME author chose
// (spec.agent.endpointSecretRef, mounted into the model proxy). Being able
// to create a Loop therefore meant being able to read every Secret in the
// namespace, with no cluster-side gate between the two. The owner's fix has
// two independent halves:
//
//   - D102: no cluster-wide Secret access, per-namespace opt-in. The
//     operator's ClusterRole has NO secrets rule at all. A namespace is
//     enabled by an admin binding the shipped coxswain-secret-reader
//     ClusterRole (get on secrets only) to the operator's ServiceAccount in
//     THAT namespace. The operator's Get on a named Secret then succeeds in
//     an enabled namespace and is Forbidden in a disabled one — the
//     NamespaceNotEnabled reason below.
//   - D76(a): a per-Secret opt-in label. Each named Secret must exist and
//     carry coxswain.io/usable: "true". The operator reads METADATA only
//     (a PartialObjectMetadata with the Secret GVK) — never the data (the
//     condition message and the Events name the Secret and the spec field,
//     never the value).
//
// The gate runs BEFORE ANY object that mounts or uses a Secret is created:
// the sandbox (the init-clone credential mount), the model/egress/tool
// proxies (the endpoint + tool-credential mounts), and the verify/deliver
// Jobs. While any named Secret is not usable, the Loop is HELD (fail-closed,
// the same pattern as the agentPoliciesValid / modelConfigInvalid holds in
// validateStep): no sandbox, no verify Job, no deliver Job, no proxy
// objects. The hold releases on the next reconcile after the admin labels
// the Secret or adds the RoleBinding — no operator restart. Loops that name
// no Secret are unaffected (SecretsUsable=True reason NoSecrets, so the
// condition's absence never means "unchecked").
//
// The reads go through the manager's API reader (r.apiReader,
// mgr.GetAPIReader()), NOT the cached client: a cached Get on a Secret would
// start a cluster-wide list/watch informer for the Secret type, which the
// operator has no RBAC for (D102). The API reader hits the API server
// directly per Get.

package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ctrl "sigs.k8s.io/controller-runtime"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// secretUsableLabel is the per-Secret opt-in label (D76(a)): a named Secret
// must carry coxswain.io/usable: "true" for the operator to use it.
const secretUsableLabel = "coxswain.io/usable"

// secretHoldRequeue is the window the held Loop requeues on while its named
// Secret is not usable (label the Secret or add the RoleBinding, and the
// next requeue proceeds).
const secretHoldRequeue = 60 * time.Second

// schemaSecretGVK is the Secret's GVK for the metadata-only reads (the
// PartialObjectMetadata carries it so the API reader can resolve the
// resource without a full Secret read).
var schemaSecretGVK = schema.FromAPIVersionAndKind("", "Secret")

// namedSecret is one Secret a Loop names, with the spec field that names it
// (the field name goes in the condition message so the author knows which
// reference to fix; the Secret DATA never does).
type namedSecret struct {
	name  string
	field string
}

// loopNamedSecrets collects the Secret names a Loop references, deduplicated
// by name with the FIRST field naming it kept (the message names the field
// the reference came from; a name shared by two fields is one Secret, one
// label check). The three references (R30 D76):
//
//   - spec.workspace.gitCredentialSecret (the init-clone credential);
//   - spec.agent.endpointSecretRef (the model proxy's endpoint credential);
//   - each referenced AgentPolicy's tools' credentialSecretRef (the tool
//     proxy's credential).
func loopNamedSecrets(ctx context.Context, r *LoopReconciler, loop *coxv1alpha1.Loop) []namedSecret {
	seen := make(map[string]bool)
	var named []namedSecret
	add := func(name, field string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		named = append(named, namedSecret{name: name, field: field})
	}
	add(loop.Spec.Workspace.GitCredentialSecret, "spec.workspace.gitCredentialSecret")
	add(loop.Spec.Agent.EndpointSecretRef, "spec.agent.endpointSecretRef")
	// The tool-credential references live on the referenced policies (P6a:
	// resolved through resolvePolicyRefs so a ClusterAgentPolicy ref's tools
	// are named the same way; the PolicyValid gate holds the Loop for a
	// resolvable ref, so a resolution failure here is the gate's problem, not
	// this one — it holds upstream).
	if res, rerr := r.resolvePolicyRefs(ctx, loop); rerr == nil {
		for i, ref := range loop.Spec.PolicyRefs {
			for _, tool := range res.specs[i].Tools {
				// The label names the RESOLVED kind's lowerCamel prefix (a
				// cluster-only ref is a clusterAgentPolicy.<ref>, not an
				// agentPolicy.<ref>) — matching the lowerCamel field-path shape
				// of every other label in this function (reviewer's P3-1: the
				// kind must not be a capitalised Go type name in a label).
				kindPrefix := "agentPolicy"
				if res.kinds[i] == "ClusterAgentPolicy" {
					kindPrefix = "clusterAgentPolicy"
				}
				add(tool.CredentialSecretRef.Name, kindPrefix+"."+ref+".spec.tools[].credentialSecretRef")
			}
		}
	}
	slices.SortFunc(named, func(a, b namedSecret) int { return strings.Compare(a.name, b.name) })
	return named
}

// secretUsability is the outcome of the D76(a) label check on ONE named
// Secret (metadata only, through the API reader).
type secretUsability struct {
	name  string
	field string
	// usable is true when the Secret exists and carries the opt-in label.
	usable bool
	// reason is the SecretsUsable reason to surface when usable is false
	// (NamespaceNotEnabled / SecretNotFound / SecretNotOptedIn).
	reason string
	// message names the Secret and the spec field (NEVER the data).
	message string
}

// checkSecretUsable reads the named Secret's METADATA only (the
// PartialObjectMetadata with the Secret GVK — a full Secret read would
// return the data, which the operator neither needs nor may surface)
// through the API reader and applies the D76(a) label rule.
func checkSecretUsable(ctx context.Context, reader client.Reader, ns string, named namedSecret) secretUsability {
	meta := &metav1.PartialObjectMetadata{}
	meta.SetGroupVersionKind(schemaSecretGVK)
	err := reader.Get(ctx, types.NamespacedName{Namespace: ns, Name: named.name}, meta)
	switch {
	case apierrors.IsNotFound(err):
		return secretUsability{
			name:   named.name,
			field:  named.field,
			reason: coxv1alpha1.SecretsUsableReasonSecretNotFound,
			message: fmt.Sprintf("the Secret %s named by %s does not exist in namespace %s; create it and label it %s=true",
				named.name, named.field, ns, secretUsableLabel),
		}
	case apierrors.IsForbidden(err):
		return secretUsability{
			name:   named.name,
			field:  named.field,
			reason: coxv1alpha1.SecretsUsableReasonNamespaceNotEnabled,
			message: fmt.Sprintf("the operator may not read Secrets in namespace %s (get on secrets is Forbidden); the D102 per-namespace opt-in is missing — bind the coxswain-secret-reader ClusterRole to the operator's ServiceAccount in this namespace before the Loop can use %s (named by %s)",
				ns, named.name, named.field),
		}
	case err != nil:
		// A transient read error (not a NotFound / Forbidden) is treated as
		// not opted in (fail closed): the Loop holds with the read error
		// named, and the next requeue retries. It is NOT surfaced as a
		// distinct reason — the condition reasons are the four owner-named
		// values, and a transient error must not masquerade as an opt-in
		// state.
		return secretUsability{
			name:   named.name,
			field:  named.field,
			reason: coxv1alpha1.SecretsUsableReasonSecretNotOptedIn,
			message: fmt.Sprintf("the Secret %s named by %s could not be read through the API reader: %v (the Loop is held until the read succeeds and the Secret is opted in)",
				named.name, named.field, err),
		}
	default:
		if meta.GetLabels()[secretUsableLabel] == "true" {
			return secretUsability{name: named.name, field: named.field, usable: true}
		}
		return secretUsability{
			name:   named.name,
			field:  named.field,
			reason: coxv1alpha1.SecretsUsableReasonSecretNotOptedIn,
			message: fmt.Sprintf("the Secret %s named by %s exists but is not opted in (label %s must be \"true\"); label it before the Loop can use it",
				named.name, named.field, secretUsableLabel),
		}
	}
}

// applySecretsGate runs the D76(a) + D102 gate: before ANY object that mounts
// or uses a Secret is created, every Secret the Loop names must exist and
// carry coxswain.io/usable: "true". It is the caller's hold (validateStep):
// it returns (result, true) while the Loop must be held (the caller returns
// the result — no sandbox, no verify/deliver, no proxies), (zero, false)
// when the Loop may proceed.
//
// The condition is ALWAYS set (never absent): True/OptedIn (every named
// Secret opted in), True/NoSecrets (the Loop names none — the absence would
// otherwise be uninterpretable), or False with the first failing named
// Secret's reason (NamespaceNotEnabled / SecretNotFound /
// SecretNotOptedIn — first in sorted-name order, so the message is stable).
// A Warning Event fires on each transition to False (the same
// lastStatus==true comparison the other gate Events use).
func (r *LoopReconciler) applySecretsGate(ctx context.Context, loop *coxv1alpha1.Loop) (ctrl.Result, bool) {
	if r.SecretOptInRequired {
		if res, done := r.applySecretsGateEnabled(ctx, loop); done {
			return res, true
		}
	}
	return ctrl.Result{}, false
}

// applySecretsGateEnabled runs the gate's check (the SecretOptInRequired
// field is checked by the caller; it is split so the Reconcile-side call
// site stays within the gocyclo budget and the specs can drive the gate
// directly).
func (r *LoopReconciler) applySecretsGateEnabled(ctx context.Context, loop *coxv1alpha1.Loop) (ctrl.Result, bool) {
	if !r.SecretOptInRequired {
		return ctrl.Result{}, false
	}
	named := loopNamedSecrets(ctx, r, loop)
	if len(named) == 0 {
		setCondition(loop, coxv1alpha1.SecretsUsableCondition, metav1.ConditionTrue,
			coxv1alpha1.SecretsUsableReasonNoSecrets, "the Loop names no Secret")
		return ctrl.Result{}, false
	}

	// A bare reconciler (a test that wires only Client) has no apiReader:
	// fall back to the plain client ONLY for the test seams that wrap it
	// (the Forbidden spec uses an interceptor client). On a live install the
	// apiReader is always set (cmd/main.go wires mgr.GetAPIReader()).
	reader := client.Reader(r.Client)
	if r.apiReader != nil {
		reader = r.apiReader
	}

	var blocker *secretUsability
	for _, n := range named {
		u := checkSecretUsable(ctx, reader, loop.Namespace, n)
		if !u.usable {
			blocker = &u
			break // first failing (sorted-name order): the message names it
		}
	}

	lastStatus := metav1.ConditionUnknown
	for i := range loop.Status.Conditions {
		if loop.Status.Conditions[i].Type == coxv1alpha1.SecretsUsableCondition {
			lastStatus = loop.Status.Conditions[i].Status
		}
	}

	if blocker == nil {
		setCondition(loop, coxv1alpha1.SecretsUsableCondition, metav1.ConditionTrue,
			coxv1alpha1.SecretsUsableReasonOptedIn,
			fmt.Sprintf("all %d Secret(s) the Loop names are opted in (label %s=true)", len(named), secretUsableLabel))
		if err := r.Status().Update(ctx, loop); err != nil {
			return ctrl.Result{}, true // the write failed; hold and retry
		}
		return ctrl.Result{}, false
	}

	setCondition(loop, coxv1alpha1.SecretsUsableCondition, metav1.ConditionFalse, blocker.reason, blocker.message)
	if err := r.Status().Update(ctx, loop); err != nil {
		return ctrl.Result{}, true // the write failed; hold and retry
	}
	if lastStatus == metav1.ConditionTrue {
		// The transition True -> False (a labelled Secret became unlabelled,
		// or a binding was removed): a Warning Event, per the gate's event
		// rule. A False -> False message change does NOT re-emit.
		if r.Recorder != nil {
			r.Recorder.Eventf(loop, corev1.EventTypeWarning, string(coxv1alpha1.SecretsUsableReason(blocker.reason)), "%s", blocker.message)
		}
	}
	// Held: no sandbox, no verify Job, no deliver Job, no proxy objects. The
	// hold releases on the next reconcile after the admin labels the Secret
	// or adds the RoleBinding (no restart; the requeue below bounds the wait
	// for a label-only change, which fires no watch on the Loop).
	return ctrl.Result{RequeueAfter: secretHoldRequeue}, true
}
