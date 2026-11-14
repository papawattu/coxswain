// Phase 8 J5b (PLAN-PHASE8.md, D132): the Judge's own model proxy. When
// spec.verify.judge.modelEndpoint names an endpoint of its own, the Judge's
// call goes through a second model-proxy pod + Service (<loop>-jproxy)
// carrying the Judge endpoint's Secret, metered into the Loop's totals.
//
// LABELS (the trap): the Loop's own model proxy pod carries
// proxyLabels(loopName) = {component: model-proxy, coxswain.io/proxy-for:
// <loop>}, and the Loop's proxy Service SELECTS those labels. The Judge
// proxy must therefore carry its OWN label set (judgeProxyLabels) — the
// component value judge-proxy (in policy.ProxyComponentSelector, so the
// operator's scoped pod cache can read it) and its own -for label. A Judge
// proxy that carried proxyLabels would be selected by the Loop's own proxy
// Service and route the AGENT's model calls to the Judge's endpoint.
package controller

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"

	"github.com/papawattu/coxswain/internal/policy"
)

// judgeProxyName is the Judge proxy's pod and Service name (D132). The
// suffix is -jproxy (not -judge-proxy): a 55-char Loop name + "-jproxy"
// (7) is 62, which passes the 63-char Service-name limit, while
// "-judge-proxy" (12) would not.
func judgeProxyName(loopName string) string { return loopName + "-jproxy" }

// judgeProxyForLabel is the Judge proxy's -for label key (the judgeForLabel
// scheme, coxswain.io/<component>-for — a DISTINCT key from
// coxswain.io/proxy-for so no selector on the Loop's own proxy labels can
// match the Judge proxy, and vice versa).
const judgeProxyForLabel = "coxswain.io/judge-proxy-for"

// judgeOwnEndpoint: the Loop's Judge is enabled and names its own endpoint
// (D132). Without an own endpoint the Judge's call goes through the Loop's
// own model proxy (D123/J5a).
func judgeOwnEndpoint(loop *coxv1alpha1.Loop) bool {
	j := loop.Spec.Verify.Judge
	return j != nil && j.Enabled && j.ModelEndpoint != ""
}

// judgeProxyLabels is the Judge proxy pod/Service label set (J5b, D132). It
// is DISJOINT from proxyLabels: the component value is judge-proxy (not
// model-proxy) and the -for key is judge-proxy-for (not proxy-for). If it
// carried proxyLabels, the Loop's own proxy Service (which selects
// proxyLabels) would select this pod too and the agent's model calls could
// be routed to the Judge's endpoint.
func judgeProxyLabels(loopName string) map[string]string {
	return map[string]string{
		policy.ComponentLabelKey: policy.ComponentJudgeProxyLabel,
		judgeProxyForLabel:       loopName,
	}
}

// buildJudgeProxyPod is the model-proxy pod for the Judge's endpoint (J5b,
// D132): the model-proxy pod shape (buildModelProxyPod, shared with the
// Loop's own proxy) with the Judge proxy's name and labels, the Judge's
// endpoint (normalised like modelEndpointValue: a bare host:port gets
// "http://" prefixed — httputil.NewSingleHostReverseProxy requires a full
// URL) and the Judge's credential Secret. The pod never carries the agent's
// endpoint or Secret anywhere.
func (r *LoopReconciler) buildJudgeProxyPod(loop *coxv1alpha1.Loop) *corev1.Pod {
	return r.buildModelProxyPod(loop,
		judgeProxyName(loop.Name),
		loop.Namespace,
		judgeProxyLabels(loop.Name),
		judgeEndpointValue(loop),
		loop.Spec.Verify.Judge.EndpointSecretRef,
		r.proxyImage(),
	)
}

// judgeEndpointValue normalises the Judge's own endpoint to a full URL for
// the Judge proxy's MODEL_ENDPOINT env var, the modelEndpointValue rule
// applied to spec.verify.judge.modelEndpoint: a bare host:port (no scheme)
// gets "http://" prefixed.
func judgeEndpointValue(loop *coxv1alpha1.Loop) string {
	eps := loop.Spec.Verify.Judge.ModelEndpoint
	if eps == "" {
		return ""
	}
	if strings.HasPrefix(eps, "http://") || strings.HasPrefix(eps, "https://") {
		return eps
	}
	return "http://" + eps
}

// mutateJudgeProxyService is the Judge proxy Service (J5b D141): the Loop
// proxy Service's shape (mutateModelProxyService: port proxyPort, ClusterIP)
// with the Judge proxy's labels and selector, so it never selects the Loop's
// own proxy pod and the Loop's proxy Service never selects it.
func mutateJudgeProxyService(scheme *runtime.Scheme, loop *coxv1alpha1.Loop, svc *corev1.Service) error {
	if err := mutateModelProxyService(scheme, loop, svc); err != nil {
		return err
	}
	svc.Labels = judgeProxyLabels(loop.Name)
	svc.Spec.Selector = judgeProxyLabels(loop.Name)
	return nil
}

// ensureJudgeProxy creates the Judge proxy NetworkPolicy, Service and pod
// (J5b D141) right before the Judge Job, when the Judge has its own endpoint.
// The pod follows ensureProxy's pattern: Get/create/delete, never Update a Pod
// spec, with the spec-hash annotation. It returns requeue=true when it
// deleted a drifted pod (the Job waits for the recreate).
func (r *LoopReconciler) ensureJudgeProxy(ctx context.Context, loop *coxv1alpha1.Loop) (bool, error) {
	if !judgeOwnEndpoint(loop) {
		return false, nil
	}
	log := logf.FromContext(ctx)
	ns, name := loop.Namespace, judgeProxyName(loop.Name)
	if err := r.ensureJudgeProxyNetworkPolicy(ctx, loop); err != nil {
		return false, fmt.Errorf("ensure judge proxy netpol: %w", err)
	}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		return mutateJudgeProxyService(r.Scheme, loop, svc)
	}); err != nil {
		return false, fmt.Errorf("ensure judge proxy service %s/%s: %w", ns, name, err)
	}
	desired := r.buildJudgeProxyPod(loop)
	if err := controllerutil.SetControllerReference(loop, desired, r.Scheme); err != nil {
		return false, fmt.Errorf("set owner ref on judge proxy pod %s/%s: %w", ns, name, err)
	}
	hash := proxyPodSpecHash(desired)
	desired.Annotations = map[string]string{proxySpecHashAnnotation: hash}

	existing := &corev1.Pod{}
	err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, existing)
	switch {
	case apierrors.IsNotFound(err):
		if cerr := r.Create(ctx, desired); cerr != nil {
			return false, fmt.Errorf("create judge proxy pod %s/%s: %w", ns, name, cerr)
		}
		log.Info("Created Judge proxy pod", "proxy", name, "loop", loop.Name)
		return false, nil
	case err != nil:
		return false, fmt.Errorf("get judge proxy pod %s/%s: %w", ns, name, err)
	case !metav1.IsControlledBy(existing, loop):
		return false, fmt.Errorf("judge proxy pod %s/%s exists but is not controlled by this Loop", ns, name)
	case existing.Annotations[proxySpecHashAnnotation] != hash:
		// Spec drift (an operator upgrade): a final read so the recreate's
		// boot-ID change loses nothing recorded, then delete; the Owns(Pod)
		// watch re-reconciles and creates the new pod.
		if fr, rerr := r.resolveJudgeProxyUsageRead(ctx, loop); rerr == nil && fr != nil {
			r.applyJudgeUsageReading(ctx, loop, *fr)
		}
		if derr := r.Delete(ctx, existing); derr != nil && !apierrors.IsNotFound(derr) {
			return false, fmt.Errorf("delete drifted judge proxy pod %s/%s: %w", ns, name, derr)
		}
		return true, nil
	}
	return false, nil
}

// settleJudgeProxy ends the Judge proxy's life once the verdict (or
// JudgeUnavailable) is recorded (J5b D141): the FINAL usage reading BEFORE the
// delete, folded into status.budget; then delete the pod, Service and
// NetworkPolicy. A failed final read never blocks the Loop: it deletes anyway
// and surfaces the loss (I90) — a Warning Event JudgeUsageUnread and a note on
// the JudgeVerdict condition, never a silent zero. A no-op when no Judge proxy
// exists and the Judge has no own endpoint.
func (r *LoopReconciler) settleJudgeProxy(ctx context.Context, loop *coxv1alpha1.Loop) error {
	ns, name := loop.Namespace, judgeProxyName(loop.Name)
	pod := &corev1.Pod{}
	podErr := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, pod)
	if podErr != nil && !apierrors.IsNotFound(podErr) {
		return fmt.Errorf("get judge proxy pod %s/%s: %w", ns, name, podErr)
	}
	podFound := podErr == nil
	if podFound || judgeOwnEndpoint(loop) {
		if fr, rerr := r.resolveJudgeProxyUsageRead(ctx, loop); rerr == nil && fr != nil {
			r.applyJudgeUsageReading(ctx, loop, *fr)
		} else if rerr != nil {
			r.noteJudgeUsageUnread(loop, rerr)
		}
	}
	if podFound && metav1.IsControlledBy(pod, loop) {
		if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete judge proxy pod %s/%s: %w", ns, name, err)
		}
	}
	svc := &corev1.Service{}
	switch err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, svc); {
	case err == nil:
		if metav1.IsControlledBy(svc, loop) {
			if derr := r.Delete(ctx, svc); derr != nil && !apierrors.IsNotFound(derr) {
				return fmt.Errorf("delete judge proxy service %s/%s: %w", ns, name, derr)
			}
		}
	case !apierrors.IsNotFound(err):
		return fmt.Errorf("get judge proxy service %s/%s: %w", ns, name, err)
	}
	if err := r.deleteJudgeProxyNetworkPolicy(ctx, loop); err != nil {
		return fmt.Errorf("delete judge proxy netpol: %w", err)
	}
	return nil
}

// noteJudgeUsageUnread surfaces a failed FINAL Judge usage read: a Warning
// Event and a note appended to the JudgeVerdict condition's message.
func (r *LoopReconciler) noteJudgeUsageUnread(loop *coxv1alpha1.Loop, err error) {
	note := fmt.Sprintf(" (judge usage unread: %v; its tokens are not in status.budget)", err)
	// A failed settle retries and would append the note again: once only.
	if c := apimeta.FindStatusCondition(loop.Status.Conditions, coxv1alpha1.JudgeVerdictCondition); c != nil &&
		!strings.Contains(c.Message, "judge usage unread") {
		setCondition(loop, coxv1alpha1.JudgeVerdictCondition, c.Status, c.Reason, c.Message+note)
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(loop, corev1.EventTypeWarning, judgeUsageUnreadReason,
			"the Judge proxy's final usage reading failed (%v); its tokens are not counted toward the budget", err)
	}
}
