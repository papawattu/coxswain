package controller

// I34 (REVIEW-PHASE1-R10, P1): spec.agent.env must be literal-only (no
// valueFrom, so a Loop author cannot inject a Secret into the agent) and must
// not use platform-owned COX_* names (the operator sets COX_MODEL_BASE_URL in
// C2; a Loop must not be able to point the agent past the proxy).
//
// The typed API (AgentEnvVar{Name, Value}) cannot express valueFrom at all;
// this envtest posts an unstructured Loop WITH a valueFrom (and with a COX_*
// name) and asserts the CRD rejects it at admission.

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

var _ = Describe("I34: spec.agent.env is literal-only, no COX_* names", func() {
	ctx := context.Background()

	agentGVK := schema.GroupVersionKind{Group: "coxswain.wattu.com", Version: "v1alpha1", Kind: loopKind}

	// baseLoop is a minimal unstructured Loop with an agent block.
	baseLoop := func(name, ns string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "coxswain.wattu.com/v1alpha1",
			"kind":       "Loop",
			"metadata":   map[string]any{unstructuredName: name, unstructuredNs: ns},
			"spec": map[string]any{
				"goal": "do the thing",
				"workspace": map[string]any{
					"repo": "https://example.com/repo.git",
					"ref":  loopRef,
				},
				"verify": map[string]any{"acceptanceChecks": []any{"go test ./..."}},
			},
		}}
		u.SetGroupVersionKind(agentGVK)
		return u
	}

	It("does not honor an agent.env valueFrom (the CRD is literal-only)", func() {
		ns := "i34-valuefrom-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		u := baseLoop("env-secret", ns)
		Expect(unstructured.SetNestedField(u.Object, map[string]any{
			unstructuredEnv: []any{
				map[string]any{
					unstructuredName: "GITHUB_TOKEN",
					"valueFrom": map[string]any{
						"secretKeyRef": map[string]any{"name": "push-token", "key": "token"},
					},
				},
			},
		}, "spec", "agent")).To(Succeed())

		// The structural CRD prunes the valueFrom (it is not in the AgentEnvVar
		// schema), so no Secret is mounted into the agent — the unsafe form is
		// not honored. Assert the stored object has no valueFrom field.
		Expect(k8sClient.Create(ctx, u)).To(Succeed())
		stored := &unstructured.Unstructured{}
		stored.SetGroupVersionKind(agentGVK)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "env-secret", Namespace: ns}, stored)).To(Succeed())

		envItems, found, err := unstructured.NestedSlice(stored.Object, "spec", "agent", unstructuredEnv)
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(envItems).NotTo(BeEmpty())
		env0, ok := envItems[0].(map[string]any)
		Expect(ok).To(BeTrue())
		_, hasValueFrom := env0["valueFrom"]
		Expect(hasValueFrom).To(BeFalse(),
			"the CRD must not carry a valueFrom on agent.env (I34: literal-only, no Secret injection)")
	})

	It("rejects an agent.env with a COX_* name at admission", func() {
		ns := "i34-coxname-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		u := baseLoop("env-coxname", ns)
		Expect(unstructured.SetNestedField(u.Object, map[string]any{
			unstructuredEnv: []any{
				map[string]any{unstructuredName: "COX_MODEL_BASE_URL", unstructuredValue: "http://evil.example"},
			},
		}, "spec", "agent")).To(Succeed())

		err := k8sClient.Create(ctx, u)
		Expect(err).To(HaveOccurred(), "an agent.env with a COX_* name must be rejected at admission (I34)")
	})

	// P3 (R13): duplicate env names are now rejected (the Env list is a map
	// keyed on name) instead of silently letting the kubelet keep the last one.
	It("rejects an agent.env with a duplicate name at admission (P3)", func() {
		ns := "i34-dupname-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		u := baseLoop("env-dupname", ns)
		Expect(unstructured.SetNestedField(u.Object, map[string]any{
			unstructuredEnv: []any{
				map[string]any{unstructuredName: "FOO", unstructuredValue: "a"},
				map[string]any{unstructuredName: "FOO", unstructuredValue: "b"},
			},
		}, "spec", "agent")).To(Succeed())

		err := k8sClient.Create(ctx, u)
		Expect(err).To(HaveOccurred(), "a duplicate agent.env name must be rejected at admission (P3: map list keyed on name)")
	})
})
