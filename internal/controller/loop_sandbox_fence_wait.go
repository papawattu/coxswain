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

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/cni"
)

const (
	sandboxFenceNotReadyCondition = coxv1alpha1.SandboxFenceNotReadyCondition
	sandboxFenceNotReadyReason    = "FenceNotReady"
)

// sandboxFenceWait returns the sandbox's fence-wait init container, and false
// when the sandbox must not have one (see fenceWaitContainer). It runs as the
// sandbox's non-root UID/GID (agentPodSpec, the phase-init container).
func (r *LoopReconciler) sandboxFenceWait() (corev1.Container, bool) {
	return r.fenceWaitContainer(65532, 65532)
}

// fenceWaitContainer returns a fence-wait init container running as uid/gid
// under the hardened security context every coxswain pod uses, and false when
// the pod must not have one: under --allow-unenforced-network the operator
// runs Loops on a CNI that may never fence, so fence-wait would hold every pod
// in Init forever. The owner opted out of the network guarantee there;
// fence-wait has nothing to wait for.
func (r *LoopReconciler) fenceWaitContainer(uid, gid int64) (corev1.Container, bool) {
	if r.AllowUnenforcedNetwork {
		return corev1.Container{}, false
	}
	image := r.FenceWaitImage
	if image == "" {
		image = cni.PinnedProbeImage
	}
	falseP, trueP := false, true
	return cni.NewFenceWaitContainer(image, corev1.PullIfNotPresent, r.FenceWaitBudget, &corev1.SecurityContext{
		AllowPrivilegeEscalation: &falseP,
		RunAsNonRoot:             &trueP,
		RunAsUser:                &uid,
		RunAsGroup:               &gid,
		ReadOnlyRootFilesystem:   &trueP,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{allCaps}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}), true
}

// prependProxyFenceWait makes fence-wait the FIRST init container of a proxy
// pod (model, Judge, egress and tool proxies), running as the proxy's own
// uid/gid. A new proxy pod is unfenced for kube-router's programming window
// (0.05-1.2s after its first container starts) just like the sandbox: the
// model and tool proxies hold a credential, and every proxy's NetworkPolicy
// is what keeps it from reaching in-cluster and private targets (the egress
// and tool proxies allow 0.0.0.0/0 minus the private carve-outs). Each proxy's
// policy refuses the node's kubelet (DNS plus a model peer on its port, or
// the carve-outs, which cover RFC 1918 node IPs), so fence-wait can pass.
//
// A budget that runs out holds the proxy in Init (fail-closed): the proxy is
// never Ready, and whatever waits on it (the D35a model-proxy gate, the
// verify Job's egress gate) keeps holding. For the model proxy the hold says
// so (modelProxyNotReady).
func (r *LoopReconciler) prependProxyFenceWait(spec *corev1.PodSpec, uid, gid int64) {
	fw, ok := r.fenceWaitContainer(uid, gid)
	if !ok {
		return
	}
	spec.InitContainers = append([]corev1.Container{fw}, spec.InitContainers...)
}

// fenceWaitTimedOut reports whether a pod's (the sandbox's, or a proxy's) fence-wait last
// exited with FenceWaitTimeoutExit (the current or the last terminated state:
// under the sandbox's restart policy the kubelet re-runs a failed init
// container, so the timeout is usually in LastTerminationState while the next
// attempt runs) and the agent has not started since.
func fenceWaitTimedOut(pod *corev1.Pod) (bool, string) {
	for i := range pod.Status.InitContainerStatuses {
		cs := &pod.Status.InitContainerStatuses[i]
		if cs.Name != cni.FenceWaitContainerName {
			continue
		}
		term := cs.State.Terminated
		if term == nil || term.ExitCode == 0 {
			if cs.State.Running == nil && cs.State.Waiting == nil {
				return false, ""
			}
			// Running or waiting for a retry: the last attempt decides.
			term = cs.LastTerminationState.Terminated
		}
		if term == nil || term.ExitCode != cni.FenceWaitTimeoutExit {
			return false, ""
		}
		return true, term.Message
	}
	return false, ""
}

// applySandboxFenceNotReady surfaces a sandbox whose fence-wait ran out of
// budget: condition SandboxFenceNotReady (True, reason FenceNotReady) plus ONE
// Warning Event on the transition; cleared (False, FenceUp) once fence-wait
// passes or the pod is gone. It decides nothing about the Loop: the agent
// cannot start while fence-wait fails, so the sandbox already fails closed.
// SandboxNotStarting cannot report this case, because fence-wait itself has
// started. Returns whether the Loop's status changed.
func (r *LoopReconciler) applySandboxFenceNotReady(ctx context.Context, loop *coxv1alpha1.Loop) bool {
	reader := r.apiReader
	if reader == nil {
		if r.Client == nil {
			return false
		}
		reader = r
	}
	key := client.ObjectKey{Namespace: loop.Namespace, Name: sandboxName(loop.Name)}
	sb := &sandboxv1beta1.Sandbox{}
	pod := &corev1.Pod{}
	if err := reader.Get(ctx, key, sb); err != nil {
		return r.clearSandboxFenceNotReady(loop, client.IgnoreNotFound(err) == nil)
	}
	if err := reader.Get(ctx, key, pod); err != nil {
		return r.clearSandboxFenceNotReady(loop, client.IgnoreNotFound(err) == nil)
	}
	if !metav1.IsControlledBy(pod, sb) {
		return false // a stale pod from a previous Sandbox: no decision
	}
	timedOut, detail := fenceWaitTimedOut(pod)
	if !timedOut {
		return r.clearSandboxFenceNotReady(loop, true)
	}
	if conditionTruePresent(loop, sandboxFenceNotReadyCondition) {
		return false // already surfaced: a fixed message, so no status write per pass
	}
	msg := fmt.Sprintf("the sandbox pod's fence-wait ran out of budget: the node's kubelet stayed reachable under the Loop's NetworkPolicy, so the agent is held; check that the CNI enforces NetworkPolicy for pod %s", pod.Name)
	setCondition(loop, sandboxFenceNotReadyCondition, metav1.ConditionTrue, sandboxFenceNotReadyReason, msg)
	if r.Recorder != nil {
		r.Recorder.Eventf(loop, corev1.EventTypeWarning, sandboxFenceNotReadyReason,
			"the sandbox pod %s's fence-wait ran out of budget (%s); the agent is held until the NetworkPolicy takes effect",
			pod.Name, detail)
	}
	return true
}

// clearSandboxFenceNotReady clears a True SandboxFenceNotReady when ok (the
// pod is gone, or its fence-wait no longer reports a timeout). A failed read
// (ok false) is no decision (I49).
func (r *LoopReconciler) clearSandboxFenceNotReady(loop *coxv1alpha1.Loop, ok bool) bool {
	if !ok || !conditionTruePresent(loop, sandboxFenceNotReadyCondition) {
		return false
	}
	setCondition(loop, sandboxFenceNotReadyCondition, metav1.ConditionFalse, "FenceUp",
		"the sandbox pod's fence-wait no longer reports a timeout")
	return true
}
