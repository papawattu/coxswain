package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	cxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var _ = Describe("D35: proxy readiness gate + ProxyConflict", func() {
	var ctx context.Context
	var r *LoopReconciler

	// Local consts (the shared consts from the main suite are not available
	// in this file because it's a separate Describe).
	const (
		d35TestRepo      = "https://github.com/papawattu/coxswain.git"
		d35TestKey       = "key"
		d35ModelEndpoint = "fake-model:8000"
		d35RunnerImage   = "docker.io/library/golang:1.26"
		d35TestModel     = "test-model"
		d35SecretName    = "d35-creds"
		d35LoopName      = "d35-gate"
		d35Namespace     = "d35-gate-ns"
	)

	// buildD35Loop creates a Loop with endpointSecretRef + modelEndpoint set.
	buildD35Loop := func(name, ns string) *cxv1alpha1.Loop {
		return &cxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: cxv1alpha1.LoopSpec{
				Goal: "d35 gate test",
				Workspace: cxv1alpha1.Workspace{
					Repo: d35TestRepo,
					Ref:  loopRef,
				},
				Agent: cxv1alpha1.AgentConfig{
					Image:             d35RunnerImage,
					Model:             d35TestModel,
					EndpointSecretRef: d35SecretName,
					ModelEndpoint:     d35ModelEndpoint,
				},
			},
		}
	}

	reconcileLoop := func(name, ns string) {
		req := reconcile.Request{
			NamespacedName: types.NamespacedName{Name: name, Namespace: ns},
		}
		_, err := r.Reconcile(ctx, req)
		Expect(err).ToNot(HaveOccurred())
	}

	BeforeEach(func() {
		ctx = context.Background()
		r = &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_ = k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: d35Namespace},
		})
		_ = k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: d35SecretName, Namespace: d35Namespace},
			StringData: map[string]string{
				"MODEL_API_KEY": "d35-test-key",
				modelBaseURL:    "http://" + d35ModelEndpoint,
			},
		})
	})

	It("D35a: sandbox is Suspended until the proxy pod is Ready", func() {
		loop := buildD35Loop(d35LoopName, d35Namespace)
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		// First reconcile: sandbox is created, proxy is created.
		// The proxy is NOT Ready yet (fresh pod), so the sandbox must be
		// Suspended.
		reconcileLoop(d35LoopName, d35Namespace)

		// The sandbox should exist and be Suspended (proxy not Ready yet).
		sb := &sandboxv1beta1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: d35LoopName + "-sandbox", Namespace: d35Namespace,
		}, sb)).To(Succeed())
		Expect(sb.Spec.OperatingMode).To(Equal(sandboxv1beta1.SandboxOperatingModeSuspended),
			"the sandbox must be Suspended until the proxy pod is Ready")

		// The proxy pod should exist.
		proxyPod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: d35LoopName + "-proxy", Namespace: d35Namespace,
		}, proxyPod)).To(Succeed())

		// The proxy pod is not Ready (no containers ready in the fake
		// envtest environment). Simulate it becoming Ready by setting
		// the pod condition + container status.
		readyCond := corev1.PodCondition{
			Type:    corev1.PodReady,
			Status:  corev1.ConditionTrue,
			Reason:  "ContainersReady",
			Message: "all containers ready",
		}
		proxyPod.Status.Conditions = append(proxyPod.Status.Conditions, readyCond)
		if len(proxyPod.Status.ContainerStatuses) == 0 {
			proxyPod.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name:  "proxy",
				Ready: true,
			}}
		} else {
			for i := range proxyPod.Status.ContainerStatuses {
				proxyPod.Status.ContainerStatuses[i].Ready = true
			}
		}
		Expect(k8sClient.Status().Update(ctx, proxyPod)).To(Succeed())

		// Second reconcile: the proxy is now Ready, so the sandbox should
		// flip to Running.
		reconcileLoop(d35LoopName, d35Namespace)

		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: d35LoopName + "-sandbox", Namespace: d35Namespace,
		}, sb)).To(Succeed())
		Expect(sb.Spec.OperatingMode).To(Equal(sandboxv1beta1.SandboxOperatingModeRunning),
			"the sandbox must be Running once the proxy pod is Ready")
	})

	It("D35a: a Loop without endpointSecretRef runs Running (no gate)", func() {
		// A Loop with no model endpoint has no proxy, so the D35 gate does
		// not apply. The sandbox should be Running (not Suspended).
		loop := &cxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: "d35-nogate", Namespace: d35Namespace},
			Spec: cxv1alpha1.LoopSpec{
				Goal: "no gate test",
				Workspace: cxv1alpha1.Workspace{
					Repo: d35TestRepo,
					Ref:  loopRef,
				},
				Agent: cxv1alpha1.AgentConfig{
					Image: d35RunnerImage,
					Model: d35TestModel,
					// No endpointSecretRef, no modelEndpoint.
				},
			},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		reconcileLoop("d35-nogate", d35Namespace)

		sb := &sandboxv1beta1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: "d35-nogate-sandbox", Namespace: d35Namespace,
		}, sb)).To(Succeed())
		Expect(sb.Spec.OperatingMode).To(Equal(sandboxv1beta1.SandboxOperatingModeRunning),
			"a Loop with no model endpoint must run Running (no D35 gate)")
	})

	It("D35b: ProxyConflict condition when a foreign pod has the proxy name", func() {
		// Use a different namespace + loop name to avoid collision with D35a.
		ns := "d35-conflict-ns"
		_ = k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})
		_ = k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: d35SecretName, Namespace: ns},
			StringData: map[string]string{
				"MODEL_API_KEY": "d35-test-key",
				modelBaseURL:    "http://" + d35ModelEndpoint,
			},
		})
		conflictLoopName := "d35-conflict"
		loop := buildD35Loop(conflictLoopName, ns)
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		// Pre-create a FOREIGN pod with the name <loop>-proxy that is NOT
		// controlled by the Loop. It has the proxy labels (so it would
		// receive agent traffic via the NetworkPolicy) but no owner ref
		// to the Loop.
		foreignPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      conflictLoopName + "-proxy",
				Namespace: ns,
				Labels: map[string]string{
					"app.kubernetes.io/component": "model-proxy",
					"coxswain.io/proxy-for":       conflictLoopName,
				},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:  "foreign",
					Image: "docker.io/library/busybox:1.36",
				}},
			},
		}
		Expect(k8sClient.Create(ctx, foreignPod)).To(Succeed())

		// Reconcile: the controller should detect the foreign pod and set
		// ProxyConflict=False (the proxy is not controlled by this Loop).
		reconcileLoop(conflictLoopName, ns)

		// The foreign pod must still exist (not deleted — I2 never-take-over).
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: conflictLoopName + "-proxy", Namespace: ns,
		}, foreignPod)).To(Succeed())

		// The Loop must have a ProxyConflict condition.
		var conflict *metav1.Condition
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: conflictLoopName, Namespace: ns,
		}, loop)).To(Succeed())
		for i := range loop.Status.Conditions {
			if loop.Status.Conditions[i].Type == "ProxyConflict" {
				conflict = &loop.Status.Conditions[i]
				break
			}
		}
		Expect(conflict).ToNot(BeNil(), "ProxyConflict condition must be set when a foreign pod has the proxy name")
		Expect(conflict.Status).To(Equal(metav1.ConditionTrue),
			"ProxyConflict must be True when a foreign pod occupies the proxy name")
		Expect(conflict.Reason).To(Equal("ForeignProxy"))
	})
})
