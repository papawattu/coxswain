// Copyright 2026 papawattu.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package engine

import (
	"context"
	"strings"
	"testing"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/policy"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// J5b review fix 1 (PR #491 review P1): the KubeArmor wiring for the Judge
// proxy is pinned by four Apply specs on a fake client. The previous
// TestEmitJudgeProxyKubeArmorPolicyShape test only pinned the emitter;
// it did not prove that Apply creates the policy, that the else branch
// cleans it up, or that a foreign policy is left alone.
//
// Spec (a): own-endpoint Loop → coxswain-<loop>-jproxy exists with the
//          judge-proxy selector.
// Spec (b): Judge Enabled=false but ModelEndpoint set → no jproxy policy.
// Spec (c): a Loop that had it, then loses its own endpoint → re-Apply
//           deletes it.
// Spec (d): a foreign coxswain-<loop>-jproxy is left alone (never taken over).
//
// K1 mutation: `if judgeOwnEndpoint(loop) {` → `if false && judgeOwnEndpoint(loop) {`
//   kills spec (a) and (c).
// K2 mutation: `return j != nil && j.Enabled && j.ModelEndpoint != ""` →
//   `return j != nil && j.ModelEndpoint != ""`
//   kills spec (b).

const (
	jproxyTestNS       = "jproxy-ns"
	jproxyTestLoopName = "jproxy-loop"
	jproxyKaptName     = "coxswain-" + jproxyTestLoopName + "-jproxy"
)

// newJproxyTestClient builds a fake client with the coxswain + corev1 scheme
// (the KubeArmorPolicy GVK is not registered — unstructured objects work
// directly through the fake client).
func newJproxyTestClient(t *testing.T) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	if err := coxv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add coxswain scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).Build()
}

// newJproxyEnforcer builds a KubeArmorEnforcer with the fake client and the
// required FQDN functions (nil-check guard: Apply returns ErrNoProxyFQDNs
// when they are absent).
func newJproxyEnforcer(t *testing.T, cl client.Client) *KubeArmorEnforcer {
	t.Helper()
	fqdn := func(loopName, ns string) string { return "coxswain-" + loopName + "-proxy." + ns + ".svc" }
	egressFqdn := func(loopName, ns string) string { return "coxswain-" + loopName + "-egress-proxy." + ns + ".svc" }
	toolFqdn := func(loopName, ns, toolName string) string {
		return "coxswain-" + loopName + "-tool-" + toolName + "." + ns + ".svc"
	}
	return &KubeArmorEnforcer{
		Client:          cl,
		ProxyFQDN:       fqdn,
		EgressProxyFQDN: egressFqdn,
		ToolProxyFQDN:   toolFqdn,
	}
}

// jproxyLoopWithOwnEndpoint returns a Loop with the Judge enabled and
// ModelEndpoint set (the own-endpoint predicate is true).
func jproxyLoopWithOwnEndpoint() *coxv1alpha1.Loop {
	return &coxv1alpha1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: jproxyTestLoopName, Namespace: jproxyTestNS},
		Spec: coxv1alpha1.LoopSpec{
			Agent: coxv1alpha1.AgentConfig{
				Image:             "busybox:1.36",
				Model:             "local-model",
				EndpointSecretRef: "model-secret",
				ModelEndpoint:     "model.test:8443",
			},
			Verify: coxv1alpha1.VerifyConfig{
				Judge: &coxv1alpha1.JudgeConfig{
					Enabled:       true,
					MaxTokens:     4096,
					Model:         "judge-model",
					ModelEndpoint: "judge.test:9000",
				},
			},
		},
	}
}

// jproxyLoopNoOwnEndpoint returns a Loop with the Judge DISABLED but
// ModelEndpoint set (the own-endpoint predicate must be false; kills K2).
func jproxyLoopNoOwnEndpoint() *coxv1alpha1.Loop {
	loop := jproxyLoopWithOwnEndpoint()
	loop.Spec.Verify.Judge.Enabled = false
	return loop
}

func jproxyTestPolicy() policy.EffectivePolicy {
	return policy.EffectivePolicy{
		Exec:    []string{"git"},
		Network: []string{"proxy.golang.org:443"},
		Files:   []string{"/data"},
	}
}

// getJproxyKapt fetches the coxswain-<loop>-jproxy KubeArmorPolicy or returns nil.
func getJproxyKapt(t *testing.T, cl client.Client) *unstructured.Unstructured {
	t.Helper()
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(KubeArmorGVK)
	err := cl.Get(context.Background(), client.ObjectKey{Namespace: jproxyTestNS, Name: jproxyKaptName}, obj)
	if err != nil {
		return nil
	}
	return obj
}

// spec (a): an own-endpoint Loop → Apply creates coxswain-<loop>-jproxy with
// the judge-proxy selector (component=judge-proxy + judge-proxy-for=<loop>).
func TestApplyJudgeProxyKaptCreatedForOwnEndpoint(t *testing.T) {
	cl := newJproxyTestClient(t)
	e := newJproxyEnforcer(t, cl)
	loop := jproxyLoopWithOwnEndpoint()

	if err := e.Apply(context.Background(), loop, jproxyTestPolicy()); err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}

	kapt := getJproxyKapt(t, cl)
	if kapt == nil {
		t.Fatal("spec (a): coxswain-<loop>-jproxy KubeArmorPolicy not created after Apply with own-endpoint Loop")
	}

	// Check the selector.
	spec, found, _ := unstructured.NestedMap(kapt.Object, KaptSpecKey)
	if !found {
		t.Fatal("spec (a): spec key missing on the Judge proxy KubeArmorPolicy")
	}
	sel, found, _ := unstructured.NestedMap(spec, KaptSelectorKey)
	if !found {
		t.Fatal("spec (a): spec.selector missing")
	}
	labels, found, _ := unstructured.NestedMap(sel, KaptMatchLabelsKey)
	if !found {
		t.Fatal("spec (a): spec.selector.matchLabels missing")
	}
	if labels[policy.ComponentLabelKey] != policy.ComponentJudgeProxyLabel {
		t.Errorf("spec (a): matchLabels[component] = %v, want %q", labels[policy.ComponentLabelKey], policy.ComponentJudgeProxyLabel)
	}
	if labels["coxswain.io/judge-proxy-for"] != jproxyTestLoopName {
		t.Errorf("spec (a): matchLabels[judge-proxy-for] = %v, want %q", labels["coxswain.io/judge-proxy-for"], jproxyTestLoopName)
	}
	// The selector must NOT carry coxswain.io/loop (disjoint guard).
	if _, has := labels["coxswain.io/loop"]; has {
		t.Errorf("spec (a): selector must NOT carry coxswain.io/loop: %v", labels)
	}
}

// spec (b): Judge Enabled=false but ModelEndpoint set → no jproxy policy.
// This kills K2 (removing j.Enabled && from judgeOwnEndpoint).
func TestApplyJudgeProxyKaptNotCreatedWhenJudgeDisabled(t *testing.T) {
	cl := newJproxyTestClient(t)
	e := newJproxyEnforcer(t, cl)
	loop := jproxyLoopNoOwnEndpoint()

	if err := e.Apply(context.Background(), loop, jproxyTestPolicy()); err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}

	kapt := getJproxyKapt(t, cl)
	if kapt != nil {
		t.Fatal("spec (b): coxswain-<loop>-jproxy KubeArmorPolicy must NOT be created when Judge.Enabled=false (even with ModelEndpoint set)")
	}
}

// spec (c): a Loop that had the jproxy policy, then loses its own endpoint
// (Judge disabled) → re-Apply deletes the policy.
func TestApplyJudgeProxyKaptDeletedWhenEndpointRemoved(t *testing.T) {
	cl := newJproxyTestClient(t)
	e := newJproxyEnforcer(t, cl)

	// First Apply with own endpoint (creates the policy).
	loop1 := jproxyLoopWithOwnEndpoint()
	if err := e.Apply(context.Background(), loop1, jproxyTestPolicy()); err != nil {
		t.Fatalf("first Apply returned error: %v", err)
	}
	kapt := getJproxyKapt(t, cl)
	if kapt == nil {
		t.Fatal("spec (c): setup failed — policy not created by first Apply")
	}

	// Re-Apply with the Judge disabled (must delete the policy).
	loop2 := jproxyLoopNoOwnEndpoint()
	if err := e.Apply(context.Background(), loop2, jproxyTestPolicy()); err != nil {
		t.Fatalf("second Apply returned error: %v", err)
	}

	kapt = getJproxyKapt(t, cl)
	if kapt != nil {
		t.Fatal("spec (c): coxswain-<loop>-jproxy KubeArmorPolicy must be deleted on re-Apply after the Judge lost its own endpoint")
	}
}

// spec (d): a foreign coxswain-<loop>-jproxy is left alone (never taken over).
// createOrUpdateKapt returns ErrForeignKapt when the live object is not
// controlled by the Loop.
func TestApplyJudgeProxyKaptForeignLeftAlone(t *testing.T) {
	cl := newJproxyTestClient(t)

	// Create a foreign (Loop-unowned) KubeArmorPolicy with the jproxy name.
	foreign := &unstructured.Unstructured{}
	foreign.SetGroupVersionKind(KubeArmorGVK)
	foreign.SetName(jproxyKaptName)
	foreign.SetNamespace(jproxyTestNS)
	foreign.SetLabels(map[string]string{"external": "true"})
	foreign.SetOwnerReferences([]metav1.OwnerReference{
		{APIVersion: "v1", Kind: "ConfigMap", Name: "someone-else", UID: "foreign-uid"},
	})
	if err := unstructured.SetNestedMap(foreign.Object, map[string]any{
		"action": "Block",
		"selector": map[string]any{
			"matchLabels": map[string]any{"external": "true"},
		},
	}, "spec"); err != nil {
		t.Fatalf("set foreign spec: %v", err)
	}
	if err := cl.Create(context.Background(), foreign); err != nil {
		t.Fatalf("create foreign KubeArmorPolicy: %v", err)
	}

	e := newJproxyEnforcer(t, cl)
	loop := jproxyLoopWithOwnEndpoint()

	// Apply should return ErrForeignKapt (wrapped).
	err := e.Apply(context.Background(), loop, jproxyTestPolicy())
	if err == nil {
		t.Fatal("spec (d): Apply must return an error when a foreign jproxy KubeArmorPolicy occupies the name")
	}
	if !strings.Contains(err.Error(), "foreign") {
		t.Errorf("spec (d): error should name the foreign conflict, got: %v", err)
	}

	// The foreign object is untouched (still has the external label).
	after := &unstructured.Unstructured{}
	after.SetGroupVersionKind(KubeArmorGVK)
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: jproxyTestNS, Name: jproxyKaptName}, after); err != nil {
		t.Fatalf("get foreign KubeArmorPolicy after Apply: %v", err)
	}
	if after.GetLabels()["external"] != "true" {
		t.Error("spec (d): foreign KubeArmorPolicy was modified — external label removed")
	}
}
