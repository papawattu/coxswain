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

package alerts

import (
	"context"
	"fmt"
	"io"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/egress"
	"github.com/papawattu/coxswain/internal/toolproxy"
)

// Defaults for the engine's pods. They match the defaults of
// engine.KubeArmorEnforcerConfig (AgentNamespace/AgentPodLabel): the operator
// passes its own --kubearmor-* flags here, and these are used when a flag is
// left empty.
const (
	DefaultAgentNamespace = "kubearmor"
	DefaultAgentPodLabel  = "kubearmor-app=kubearmor"
)

// The operator's per-Loop pod labels. Every one of them names the Loop, so any
// of them attributes a pod to its Loop (which label a pod carries depends on
// what the pod is: the sandbox carries LoopLabel, each proxy carries its own).
const (
	// LoopLabel is on the sandbox pod (agentPodLabels in the controller).
	LoopLabel = "coxswain.io/loop"
	// EgressProxyLabel is on the Loop's egress proxy pod.
	EgressProxyLabel = "coxswain.io/egress-proxy-for"
	// ModelProxyLabel is on the Loop's model proxy pod.
	ModelProxyLabel = "coxswain.io/proxy-for"
	// ToolProxyLabel is on the Loop's tool proxy pod.
	ToolProxyLabel = "coxswain.io/tool-proxy-for"
)

// DefaultLoopLabels returns the operator's per-Loop pod labels, in the order
// the attributor tries them.
func DefaultLoopLabels() []string {
	return []string{LoopLabel, EgressProxyLabel, ModelProxyLabel, ToolProxyLabel}
}

// LogSource follows pod container logs through the Kubernetes API (pods/log).
// It is the operator's only pod-log reader; the grant is get/list on
// pods/log (see the RBAC marker on the Loop controller).
type LogSource struct {
	Client kubernetes.Interface
}

// Follow opens a following stream on the pod container's log. since (a
// re-attach) becomes the API's sinceTime, so the relay resumes exactly at the
// last accepted alert instead of at "now" — losing that window is the I32
// defect.
func (s *LogSource) Follow(ctx context.Context, namespace, pod, container string, since *time.Time) (io.ReadCloser, error) {
	if s == nil || s.Client == nil {
		return nil, ErrNotConfigured
	}
	opts := &corev1.PodLogOptions{Follow: true}
	if container != "" {
		opts.Container = container
	}
	if since != nil {
		opts.SinceTime = &metav1.Time{Time: *since}
	}
	return s.Client.CoreV1().Pods(namespace).GetLogs(pod, opts).Stream(ctx)
}

// PodDiscoverer lists the pods the relay cares about: the engine's agent pods,
// the operator's own proxy pods, and the sandbox pods (whose existence, with an
// attached engine stream, is what allows a count of 0).
//
// The reader is the manager's UNCACHED reader: the manager's pod cache is
// label-scoped to the proxy components, so a cached List would silently miss
// the engine's agent pods and the sandbox pods.
type PodDiscoverer struct {
	// Reader lists pods (the operator's APIReader).
	Reader client.Reader
	// AgentNamespace and AgentPodLabel locate the engine's agent pods. Empty
	// values use the Default* constants above.
	AgentNamespace string
	AgentPodLabel  string
}

// Discover implements Discoverer. A list that fails does not hide the others:
// the peers that WERE listed are returned with the error, so a permissions
// problem on one source does not blind the other two (the relay logs it).
func (d *PodDiscoverer) Discover(ctx context.Context) ([]Peer, error) {
	if d == nil || d.Reader == nil {
		return nil, ErrNotConfigured
	}
	var peers []Peer
	var firstErr error
	note := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	agentNS := firstNonEmpty(d.AgentNamespace, DefaultAgentNamespace)
	agentLabel := firstNonEmpty(d.AgentPodLabel, DefaultAgentPodLabel)
	if sel, err := labels.Parse(agentLabel); err == nil {
		list := &corev1.PodList{}
		if err := d.Reader.List(ctx, list, client.InNamespace(agentNS), client.MatchingLabelsSelector{Selector: sel}); err != nil {
			note(fmt.Errorf("list the engine's agent pods in %s: %w", agentNS, err))
		} else {
			for i := range list.Items {
				peers = append(peers, Peer{
					Namespace: list.Items[i].Namespace,
					Pod:       list.Items[i].Name,
					Kind:      PeerEngine,
					Source:    SourceKubeArmor,
				})
			}
		}
	} else {
		note(fmt.Errorf("the engine's agent pod label %q is not a label selector: %w", agentLabel, err))
	}
	for _, proxy := range []struct{ label, source string }{
		{EgressProxyLabel, egress.Source},
		{ToolProxyLabel, toolproxy.Source},
	} {
		sel, err := existsSelector(proxy.label)
		if err != nil {
			note(err)
			continue
		}
		list := &corev1.PodList{}
		if err := d.Reader.List(ctx, list, client.MatchingLabelsSelector{Selector: sel}); err != nil {
			note(fmt.Errorf("list the %s pods (%s): %w", proxy.source, proxy.label, err))
			continue
		}
		for i := range list.Items {
			peers = append(peers, Peer{
				Namespace: list.Items[i].Namespace,
				Pod:       list.Items[i].Name,
				Kind:      PeerProxy,
				Source:    proxy.source,
			})
		}
	}
	if sel, err := existsSelector(LoopLabel); err == nil {
		list := &corev1.PodList{}
		if err := d.Reader.List(ctx, list, client.MatchingLabelsSelector{Selector: sel}); err != nil {
			note(fmt.Errorf("list the sandbox pods (%s): %w", LoopLabel, err))
		} else {
			for i := range list.Items {
				peers = append(peers, Peer{
					Namespace: list.Items[i].Namespace,
					Pod:       list.Items[i].Name,
					Kind:      PeerWorkload,
				})
			}
		}
	} else {
		note(err)
	}
	return peers, firstErr
}

// PodAttributor resolves a pod to its Loop through the operator's own labels,
// then the Loop object.
type PodAttributor struct {
	// Reader reads pods and Loops (the operator's APIReader: the manager's pod
	// cache is label-scoped to the proxy components).
	Reader client.Reader
	// LoopLabels are the per-Loop labels tried in order (DefaultLoopLabels).
	LoopLabels []string
}

// PodLoop returns the Loop a pod belongs to.
func (a *PodAttributor) PodLoop(ctx context.Context, namespace, pod string) (string, string, bool) {
	if a == nil || a.Reader == nil || pod == "" {
		return "", "", false
	}
	p := &corev1.Pod{}
	if err := a.Reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: pod}, p); err != nil {
		return "", "", false
	}
	keys := a.LoopLabels
	if len(keys) == 0 {
		keys = DefaultLoopLabels()
	}
	for _, key := range keys {
		if name := p.Labels[key]; name != "" {
			return namespace, name, true
		}
	}
	return "", "", false
}

// LoopUID returns the Loop's UID (the count key).
func (a *PodAttributor) LoopUID(ctx context.Context, namespace, name string) (types.UID, bool) {
	if a == nil || a.Reader == nil || name == "" {
		return "", false
	}
	loop := &coxv1alpha1.Loop{}
	if err := a.Reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, loop); err != nil {
		return "", false
	}
	return loop.UID, true
}

// existsSelector builds the "label is present" selector the proxy and sandbox
// lists use (their values are Loop names, which vary per pod).
func existsSelector(key string) (labels.Selector, error) {
	req, err := labels.NewRequirement(key, selection.Exists, nil)
	if err != nil {
		return nil, fmt.Errorf("build the %q exists selector: %w", key, err)
	}
	return labels.NewSelector().Add(*req), nil
}
