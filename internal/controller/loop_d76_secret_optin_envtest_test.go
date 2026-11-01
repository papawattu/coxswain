// D76 + D102 (R30, owner decisions 2026-11-01): Secrets must be opted in
// before a Loop can use them (TDD — red first; the specs were written
// against the absent gate and failed with the SecretsUsable condition never
// appearing and the sandbox being created anyway).
//
// The specs (envtest, same Loop across the holds — the same-Loop update
// rule):
//   - a labelled Secret → SecretsUsable=True/OptedIn and the Loop proceeds
//     (the sandbox is created);
//   - an unlabelled Secret → False/SecretNotOptedIn, NO sandbox; then label
//     it, re-reconcile (same Loop) and the Loop proceeds (the hold releases
//     without a restart);
//   - a missing Secret → False/SecretNotFound;
//   - each of the THREE references is checked (gitCredentialSecret,
//     endpointSecretRef, the AgentPolicy tool's credentialSecretRef);
//   - a Forbidden read → False/NamespaceNotEnabled (the interceptor client
//     returns Forbidden for the Secret Get; envtest runs as admin so the
//     real RBAC would not fire);
//   - a Loop that names no Secret → True/NoSecrets (the condition is set,
//     never absent — documented in the API type's comment).

package controller

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	schema "k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/agent-sandbox/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// d76ForbiddenClient wraps a client.Client so a Get whose target is a
// PartialObjectMetadata in the given namespace returns Forbidden (the
// D102 namespace opt-in missing). Every other call passes through to the
// inner client. It implements client.WithWatch by delegation (the envtest
// plain client provides Watch), so interceptor.NewClient wraps it (the
// same idiom as b1CreateFailClient: interceptor.NewClient needs a
// WithWatch, which the envtest plain client does not provide).
type d76ForbiddenClient struct {
	inner client.Client
	ns    string
}

func (c *d76ForbiddenClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*metav1.PartialObjectMetadata); ok && key.Namespace == c.ns {
		return apierrors.NewForbidden(corev1.Resource("secrets"), key.Name,
			errors.New("secrets \"d76-git-cred\" is forbidden: cannot get resource \"secrets\" in API group \"\" in the namespace"))
	}
	return c.inner.Get(ctx, key, obj, opts...)
}
func (c *d76ForbiddenClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return c.inner.List(ctx, list, opts...)
}
func (c *d76ForbiddenClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	return c.inner.Create(ctx, obj, opts...)
}
func (c *d76ForbiddenClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	return c.inner.Delete(ctx, obj, opts...)
}
func (c *d76ForbiddenClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	return c.inner.Update(ctx, obj, opts...)
}
func (c *d76ForbiddenClient) Patch(ctx context.Context, obj client.Object, data client.Patch, opts ...client.PatchOption) error {
	return c.inner.Patch(ctx, obj, data, opts...)
}
func (c *d76ForbiddenClient) DeleteAllOf(ctx context.Context, obj client.Object, opts ...client.DeleteAllOfOption) error {
	return c.inner.DeleteAllOf(ctx, obj, opts...)
}
func (c *d76ForbiddenClient) Apply(ctx context.Context, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
	return c.inner.Apply(ctx, obj, opts...)
}
func (c *d76ForbiddenClient) Watch(ctx context.Context, obj client.ObjectList, opts ...client.ListOption) (watch.Interface, error) {
	inner, ok := c.inner.(interface {
		Watch(ctx context.Context, obj client.ObjectList, opts ...client.ListOption) (watch.Interface, error)
	})
	if !ok {
		panic("d76ForbiddenClient: inner client does not support Watch")
	}
	return inner.Watch(ctx, obj, opts...)
}
func (c *d76ForbiddenClient) SubResource(subResource string) client.SubResourceClient {
	return c.inner.SubResource(subResource)
}
func (c *d76ForbiddenClient) Status() client.SubResourceWriter {
	return c.inner.Status()
}
func (c *d76ForbiddenClient) GroupVersionKindFor(obj runtime.Object) (schema.GroupVersionKind, error) {
	return c.inner.GroupVersionKindFor(obj)
}
func (c *d76ForbiddenClient) Scheme() *runtime.Scheme {
	return c.inner.Scheme()
}
func (c *d76ForbiddenClient) RESTMapper() meta.RESTMapper {
	return c.inner.RESTMapper()
}
func (c *d76ForbiddenClient) IsObjectNamespaced(obj runtime.Object) (bool, error) {
	return c.inner.IsObjectNamespaced(obj)
}

var _ client.WithWatch = (*d76ForbiddenClient)(nil)

// d76EventSink is the broadcaster's EventSink (it posts the Event to the
// envtest cluster — the specs assert on the live Events; the broadcaster's
// default sink writes to a file the specs cannot see).
type d76EventSink struct{ client client.Client }

func (s d76EventSink) Create(e *corev1.Event) (*corev1.Event, error) {
	out := &corev1.Event{}
	err := s.client.Create(context.Background(), e)
	return out, err
}
func (s d76EventSink) Update(e *corev1.Event) (*corev1.Event, error) {
	out := &corev1.Event{}
	err := s.client.Update(context.Background(), e)
	return out, err
}
func (s d76EventSink) Patch(e *corev1.Event, data []byte) (*corev1.Event, error) {
	return e, nil
}

var _ = Describe("D76/D102 secret opt-in (SecretsUsable gate)", func() {
	var (
		ns  string
		ctx = context.Background()
	)

	BeforeEach(func() {
		ns = "d76-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
	})

	AfterEach(func() {
		_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	})

	// d76Reconciler builds the reconciler the specs drive: a REAL event
	// recorder (the broadcaster posts Events to the envtest cluster) and the
	// apiReader wired to the given reader (the gate's metadata-only reads go
	// through it; the apiReader nil-fallback would read through the cached
	// Client instead, which the Forbidden spec's interceptor must intercept).
	d76Reconciler := func(reader client.Reader) *LoopReconciler {
		broadcaster := record.NewBroadcaster()
		broadcaster.StartRecordingToSink(d76EventSink{client: k8sClient})
		return &LoopReconciler{CNIProber: testCNIProber(),
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			// The gate is ON for these specs: the shared test helpers keep the
			// zero value (off) so specs that name test Secrets that were never
			// created or labelled are unaffected; the D76 specs opt in
			// explicitly (cmd/main.go sets it true by default — asserted by
			// the cmd-level test).
			SecretOptInRequired: true,
			Recorder:            broadcaster.NewRecorder(k8sClient.Scheme(), corev1.EventSource{Component: "d76-spec"}),
			apiReader:           reader,
		}
	}

	reconcileLoop := func(r *LoopReconciler, ns, name string) {
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())
	}

	// d76Loop creates a Loop with the given spec (the workspace carries no
	// credential by default — the specs add the Secret references under
	// test) and reconciles once.
	d76Loop := func(r *LoopReconciler, name string, mutate func(*coxv1alpha1.Loop)) *coxv1alpha1.Loop {
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       coxv1alpha1.LoopSpec{Goal: loopGoal, Workspace: testWorkspace()},
		}
		if mutate != nil {
			mutate(loop)
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		reconcileLoop(r, ns, name)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		return loop
	}

	// secretsUsableCondition returns the live SecretsUsable condition.
	secretsUsableCondition := func(name string) metav1.Condition {
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		cond := meta.FindStatusCondition(loop.Status.Conditions, coxv1alpha1.SecretsUsableCondition)
		Expect(cond).NotTo(BeNil(), "the SecretsUsable condition must be set (never absent)")
		return *cond
	}

	sandboxExists := func(name string) bool {
		sb := &v1beta1.Sandbox{}
		err := k8sClient.Get(ctx, types.NamespacedName{Name: name + "-sandbox", Namespace: ns}, sb)
		if apierrors.IsNotFound(err) {
			return false
		}
		Expect(err).NotTo(HaveOccurred())
		return true
	}

	gitCredLoop := func(name string, mutate func(*coxv1alpha1.Loop)) *LoopReconciler {
		r := d76Reconciler(k8sClient)
		d76Loop(r, name, func(loop *coxv1alpha1.Loop) {
			loop.Spec.Workspace.GitCredentialSecret = "d76-git-cred"
			if mutate != nil {
				mutate(loop)
			}
		})
		return r
	}

	It("a labelled Secret: SecretsUsable=True/OptedIn and the Loop proceeds (the sandbox is created)", func() {
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "d76-git-cred", Namespace: ns,
				Labels: map[string]string{"coxswain.io/usable": "true"}},
		})).To(Succeed())
		r := gitCredLoop("d76-labelled", nil)
		cond := secretsUsableCondition("d76-labelled")
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(coxv1alpha1.SecretsUsableReasonOptedIn))
		Expect(sandboxExists("d76-labelled")).To(BeTrue(), "an opted-in Secret must not hold the Loop")
		_ = r
	})

	It("an unlabelled Secret: False/SecretNotOptedIn, no sandbox; then label it and the SAME Loop proceeds (the hold releases without a restart)", func() {
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "d76-git-cred", Namespace: ns},
		})).To(Succeed())
		r := gitCredLoop("d76-unlabelled", nil)

		cond := secretsUsableCondition("d76-unlabelled")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(coxv1alpha1.SecretsUsableReasonSecretNotOptedIn))
		Expect(cond.Message).To(ContainSubstring("d76-git-cred"))
		Expect(cond.Message).To(ContainSubstring("spec.workspace.gitCredentialSecret"))
		Expect(sandboxExists("d76-unlabelled")).To(BeFalse(), "the held Loop must have NO sandbox")

		// Label the Secret; re-reconcile the SAME Loop (the same-Loop update
		// rule) — the hold releases on the next reconcile, no restart.
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "d76-git-cred", Namespace: ns}, secret)).To(Succeed())
		secret.Labels = map[string]string{"coxswain.io/usable": "true"}
		Expect(k8sClient.Update(ctx, secret)).To(Succeed())
		reconcileLoop(r, ns, "d76-unlabelled")

		cond = secretsUsableCondition("d76-unlabelled")
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(coxv1alpha1.SecretsUsableReasonOptedIn))
		Expect(sandboxExists("d76-unlabelled")).To(BeTrue(), "after the label the same Loop must proceed (the sandbox is created)")
	})

	It("a missing Secret: False/SecretNotFound (no sandbox)", func() {
		r := gitCredLoop("d76-missing", nil) // no d76-git-cred created
		cond := secretsUsableCondition("d76-missing")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(coxv1alpha1.SecretsUsableReasonSecretNotFound))
		Expect(cond.Message).To(ContainSubstring("d76-git-cred"))
		Expect(sandboxExists("d76-missing")).To(BeFalse())
		_ = r
	})

	It("each of the THREE references is checked (gitCredentialSecret / endpointSecretRef / the AgentPolicy tool's credentialSecretRef)", func() {
		// Reference 1: spec.workspace.gitCredentialSecret (covered by the
		// labelled / unlabelled / missing specs above; here: unlabelled).
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "d76-git-cred", Namespace: ns},
		})).To(Succeed())
		gitCredLoop("d76-ref-git", nil)
		cond := secretsUsableCondition("d76-ref-git")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(coxv1alpha1.SecretsUsableReasonSecretNotOptedIn))
		Expect(cond.Message).To(ContainSubstring("spec.workspace.gitCredentialSecret"))

		// Reference 2: spec.agent.endpointSecretRef (unlabelled → held).
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "d76-model-cred", Namespace: ns},
			Data:       map[string][]byte{"apiKey": []byte("key"), "baseUrl": []byte("http://vllm:8000/v1")},
		})).To(Succeed())
		r := d76Reconciler(k8sClient)
		d76Loop(r, "d76-ref-endpoint", func(loop *coxv1alpha1.Loop) {
			loop.Spec.Agent = coxv1alpha1.AgentConfig{
				Image:             runnerImage,
				Model:             testModel,
				ModelEndpoint:     s3ModelEndpoint,
				EndpointSecretRef: "d76-model-cred",
			}
		})
		cond = secretsUsableCondition("d76-ref-endpoint")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(coxv1alpha1.SecretsUsableReasonSecretNotOptedIn))
		Expect(cond.Message).To(ContainSubstring("d76-model-cred"))
		Expect(cond.Message).To(ContainSubstring("spec.agent.endpointSecretRef"))

		// The reference-2 Secret is labelled, so it no longer blocks (the
		// reference-3 secret is what the reference-3 assertions prove).
		modelCred := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "d76-model-cred", Namespace: ns}, modelCred)).To(Succeed())
		modelCred.Labels = map[string]string{"coxswain.io/usable": "true"}
		Expect(k8sClient.Update(ctx, modelCred)).To(Succeed())

		// Reference 3: the AgentPolicy tool's credentialSecretRef (unlabelled
		// → held; the message names the AgentPolicy + field).
		ap := &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "d76-policy", Namespace: ns},
			Spec: coxv1alpha1.AgentPolicySpec{
				Tools: []coxv1alpha1.ToolSpec{{
					Name:     "d76-tool",
					Upstream: "https://api.upstream.example",
					Rules:    []coxv1alpha1.ToolRule{{Methods: []string{"GET"}}},
					CredentialSecretRef: coxv1alpha1.CredentialSecretRef{
						Name: "d76-tool-cred", Key: "token"},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, ap)).To(Succeed())
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "d76-tool-cred", Namespace: ns},
		})).To(Succeed())
		r3 := d76Reconciler(k8sClient)
		d76Loop(r3, "d76-ref-tool", func(loop *coxv1alpha1.Loop) {
			loop.Spec.PolicyRefs = []string{"d76-policy"}
			loop.Spec.Agent = coxv1alpha1.AgentConfig{
				Image:             runnerImage,
				Model:             testModel,
				ModelEndpoint:     s3ModelEndpoint,
				EndpointSecretRef: "d76-model-cred",
			}
		})
		cond = secretsUsableCondition("d76-ref-tool")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(coxv1alpha1.SecretsUsableReasonSecretNotOptedIn))
		Expect(cond.Message).To(ContainSubstring("d76-tool-cred"))
		Expect(cond.Message).To(ContainSubstring("credentialSecretRef"))
	})

	It("a Forbidden Secret read: False/NamespaceNotEnabled (the D102 namespace opt-in is missing)", func() {
		// envtest runs as admin, so the real RBAC would allow the read: the
		// Forbidden client wrapper returns Forbidden for the Secret Get
		// (the metadata-only read the gate makes through its APIReader); the
		// interceptor wrap satisfies the gate's Watch path (no-op funcs).
		sealed := interceptor.NewClient(&d76ForbiddenClient{inner: k8sClient, ns: ns}, interceptor.Funcs{})
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "d76-git-cred", Namespace: ns,
				Labels: map[string]string{"coxswain.io/usable": "true"}},
		})).To(Succeed())
		r := d76Reconciler(sealed)
		d76Loop(r, "d76-forbidden", func(loop *coxv1alpha1.Loop) {
			loop.Spec.Workspace.GitCredentialSecret = "d76-git-cred"
		})
		cond := secretsUsableCondition("d76-forbidden")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(coxv1alpha1.SecretsUsableReasonNamespaceNotEnabled))
		Expect(sandboxExists("d76-forbidden")).To(BeFalse())
		_ = r
	})

	It("a Loop that names no Secret: True/NoSecrets (the condition is set, never absent), and the Loop proceeds", func() {
		r := d76Reconciler(k8sClient)
		d76Loop(r, "d76-nosecrets", nil)
		cond := secretsUsableCondition("d76-nosecrets")
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(coxv1alpha1.SecretsUsableReasonNoSecrets))
		Expect(sandboxExists("d76-nosecrets")).To(BeTrue(), "a Loop with no Secrets is unaffected by the gate")
		_ = r
	})
})
