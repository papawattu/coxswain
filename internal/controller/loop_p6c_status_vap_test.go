package controller

// P6c step 1 (D85(a), D114): the operator is the SOLE writer of the Loop
// status subresource — every loops/status write is denied at admission
// except the operator's own ServiceAccount
// (system:serviceaccount:coxswain-system:coxswain-controller-manager).
//
// The ValidatingAdmissionPolicy + binding in
// config/admission/loop_status_vap.yaml (wired into config/admission)
// matches loops/status UPDATE and allows it only when
// request.userInfo.username is the operator SA. The envtest apiserver
// enforces ValidatingAdmissionPolicies (GA in k8s 1.34; the suite enables
// the ValidatingAdmissionPolicy admission plugin — see suite_test.go).
//
// This spec loads the SHIPPED YAML (the I45 pattern) and drives it with two
// impersonating clients:
//
//   - a "p6c-editor" user (system:authenticated) — the editor role's writer,
//     DENIED for a status.iteration change AND for a PlanApproved-only
//     condition change (D114: no exception);
//   - the operator's own ServiceAccount (impersonated) — ALLOWED.
//
// Namespace scoping: before creating the binding the spec sets
// spec.matchResources to a NamespaceSelector on the label p6c-vap-test=true
// and creates the test namespace with that label. This confines the policy
// to THIS namespace: the other specs' status writes (which run as admin,
// via k8sClient) keep working, and the binding's own namespace
// (coxswain-system) is unaffected.
//
// Loading the shipped YAML (not a Go copy) proves the exact artefact we
// deploy: editing the username literal, dropping the validation, or flipping
// the binding to Warn would all be caught here. The subresource updates are
// issued with DryRun=All: the admission chain (including the VAP) is
// evaluated, but nothing persists, so the spec needs no kubelet.
//
// I43 gate norm (R16): this spec FAILS when the gate is disabled — remove
// the applyP6cStatusVAP call in the BeforeEach below and the "denied"
// assertions fail because the update succeeds.

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

var _ = Describe("P6c: only the coxswain operator writes Loop status (D85(a)/D114)", func() {
	const (
		// p6cTestNSLabel scopes the binding to the test namespace only, so
		// the other specs' admin status writes are unaffected.
		p6cTestNSLabelKey   = "p6c-vap-test"
		p6cTestNSLabelValue = "true"

		// p6cOperatorSA is the ONLY username the shipped policy allows on
		// loops/status writes.
		p6cOperatorSA = "system:serviceaccount:coxswain-system:coxswain-controller-manager"
		// p6cEditorUser is a plain authenticated user (the editor role's
		// writer) that must be denied.
		p6cEditorUser = "p6c-editor"
	)

	var (
		ctx       context.Context
		ns        string
		editorCl  client.Client
		managerCl client.Client
	)

	// impersonatingClient returns a controller-runtime client that
	// impersonates the given user via the rest.Config.Impersonate field.
	// The shared suite's k8sClient is admin (system:masters); these
	// impersonating clients exercise the admission chain as the given user.
	impersonatingClient := func(user string, groups []string) client.Client {
		c := *cfg
		c.Impersonate = rest.ImpersonationConfig{UserName: user, Groups: groups}
		cl, err := client.New(&c, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred(), "building the impersonating client for %s", user)
		return cl
	}

	// applyP6cStatusVAP reads the shipped YAML, decodes the two documents
	// (ValidatingAdmissionPolicy + its binding) with sigs.k8s.io/yaml, and
	// creates them in the envtest cluster. Before creating the binding it
	// sets spec.matchResources to a NamespaceSelector on the test label so
	// the policy applies only to the test namespace (the other specs'
	// admin status writes must not be denied).
	applyP6cStatusVAP := func() {
		data, err := os.ReadFile(filepath.Join("..", "..", "config", "admission", "loop_status_vap.yaml"))
		Expect(err).NotTo(HaveOccurred(), "reading the shipped policy YAML")

		var vap *admissionv1.ValidatingAdmissionPolicy
		var vapb *admissionv1.ValidatingAdmissionPolicyBinding
		for doc := range strings.SplitSeq(string(data), "\n---") {
			var untyped struct {
				Kind string `yaml:"kind"`
			}
			if err := yaml.Unmarshal([]byte(doc), &untyped); err != nil {
				continue
			}
			switch untyped.Kind {
			case "ValidatingAdmissionPolicy":
				vap = &admissionv1.ValidatingAdmissionPolicy{}
				Expect(yaml.Unmarshal([]byte(doc), vap)).To(Succeed())
			case "ValidatingAdmissionPolicyBinding":
				vapb = &admissionv1.ValidatingAdmissionPolicyBinding{}
				Expect(yaml.Unmarshal([]byte(doc), vapb)).To(Succeed())
			}
		}
		Expect(vap).NotTo(BeNil(), "shipped YAML has a ValidatingAdmissionPolicy document")
		Expect(vapb).NotTo(BeNil(), "shipped YAML has a ValidatingAdmissionPolicyBinding document")

		// Apply the kustomize namePrefix the way the Makefile does: the
		// namePrefix (coxswain-) transforms metadata.name but NOT
		// spec.policyName, so the binding's policyName is already the
		// prefixed name in the shipped YAML (I85).
		prefix := "coxswain-"
		if !strings.HasPrefix(vap.Name, prefix) {
			vap.Name = prefix + vap.Name
		}
		if !strings.HasPrefix(vapb.Name, prefix) {
			vapb.Name = prefix + vapb.Name
		}

		// Scope the binding to the test namespace only.
		vapb.Spec.MatchResources = &admissionv1.MatchResources{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{p6cTestNSLabelKey: p6cTestNSLabelValue}},
		}

		// Ensure the binding's namespace exists (coxswain-system, set in the
		// shipped YAML).
		_ = k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: vapb.Namespace}})

		Expect(k8sClient.Create(ctx, vap)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &admissionv1.ValidatingAdmissionPolicy{ObjectMeta: metav1.ObjectMeta{Name: vap.Name}})
		})
		Expect(k8sClient.Create(ctx, vapb)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &admissionv1.ValidatingAdmissionPolicyBinding{ObjectMeta: metav1.ObjectMeta{Name: vapb.Name, Namespace: vapb.Namespace}})
		})
	}

	// setPlanApproved returns a copy of r with the PlanApproved condition
	// set to True (the D114 "no exception" case: approval moves to the
	// one-shot annotation on the Loop OBJECT since D85, so a PlanApproved-
	// only status change from the editor must be denied).
	setPlanApproved := func(r *coxv1alpha1.Loop) *coxv1alpha1.Loop {
		body := r.DeepCopy()
		body.ResourceVersion = r.ResourceVersion
		cond := metav1.Condition{
			Type:               coxv1alpha1.PlanApprovedCondition,
			Status:             metav1.ConditionTrue,
			Reason:             "Approved",
			Message:            "Approved: " + strings.Repeat("a", 64),
			LastTransitionTime: metav1.Now(),
		}
		if meta.SetStatusCondition(&body.Status.Conditions, cond) {
			return body
		}
		body.Status.Conditions = []metav1.Condition{cond}
		return body
	}

	BeforeEach(func() {
		ctx = context.Background()
		// The test namespace carries the label the binding's NamespaceSelector
		// matches on (see applyP6cStatusVAP).
		ns = "p6c-status-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:   ns,
			Labels: map[string]string{p6cTestNSLabelKey: p6cTestNSLabelValue},
		}})).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) })

		// The Loop whose status subresource the specs exercise.
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: "p6c-loop", Namespace: ns},
			Spec:       coxv1alpha1.LoopSpec{Goal: loopGoal, Workspace: testWorkspace()},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		// The DeferCleanup's DELETE goes through k8sClient (admin, no
		// interceptor) so the finalizer can run and the namespace deletion is
		// not blocked by the VAP-scoped namespace.
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: "p6c-loop", Namespace: ns}})
		})

		// The gate under test: apply the shipped policy + binding. (I43 norm:
		// removing this makes the "denied" assertions below fail.)
		applyP6cStatusVAP()

		editorCl = impersonatingClient(p6cEditorUser, []string{"system:authenticated"})
		managerCl = impersonatingClient(p6cOperatorSA,
			[]string{"system:serviceaccounts", "system:authenticated"})
	})

	// statusUpdate runs the given body through Status().Update (DryRun=All)
	// with the given client and returns the admission error (nil when
	// allowed). The dry run keeps the write from persisting while the full
	// admission chain (including the VAP) still runs.
	statusUpdate := func(cl client.Client, r *coxv1alpha1.Loop, mutate func(*coxv1alpha1.Loop) *coxv1alpha1.Loop) error {
		dryRunAll := client.DryRunAll
		return cl.Status().Update(ctx, mutate(r), &dryRunAll)
	}

	It("denies the editor user's status.iteration write (only the operator writes status)", func() {
		loop := &coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: "p6c-loop", Namespace: ns}}
		// A freshly-applied VAP takes a moment to become active in the API
		// server. Poll until the update is DENIED (~15s to absorb the
		// admission-plugin cache lag).
		var denied bool
		Eventually(func() bool {
			err := statusUpdate(editorCl, loop, func(b *coxv1alpha1.Loop) *coxv1alpha1.Loop {
				b.Status.Iteration = 1
				return b
			})
			denied = err != nil
			return denied
		}, "15s", "500ms").Should(BeTrue(),
			"the editor user must be DENIED when writing status.iteration")

		// The denial must carry the VAP's message, not a plain RBAC denial.
		err := statusUpdate(editorCl, loop, func(b *coxv1alpha1.Loop) *coxv1alpha1.Loop {
			b.Status.Iteration = 1
			return b
		})
		Expect(err).NotTo(BeNil())
		Expect(err.Error()).To(ContainSubstring("written only by the coxswain operator"),
			"the denial message must be the VAP's, not a plain RBAC denial")
	})

	It("allows the operator's own ServiceAccount to write status", func() {
		loop := &coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: "p6c-loop", Namespace: ns}}
		var allowed bool
		// First confirm the policy is ACTIVE (the editor is denied) so we
		// are not asserting "allowed" merely because the VAP has not loaded
		// yet.
		Eventually(func() bool {
			err := statusUpdate(editorCl, loop, func(b *coxv1alpha1.Loop) *coxv1alpha1.Loop {
				b.Status.Iteration = 1
				return b
			})
			return err != nil
		}, "15s", "500ms").Should(BeTrue(),
			"the editor must be denied before the operator-allow assertion")

		// Then assert the operator SA is allowed.
		Consistently(func() bool {
			err := statusUpdate(managerCl, loop, func(b *coxv1alpha1.Loop) *coxv1alpha1.Loop {
				b.Status.Iteration = 1
				return b
			})
			allowed = err == nil
			return allowed
		}, "5s", "500ms").Should(BeTrue(),
			"the operator's own ServiceAccount must be ALLOWED to write status")
	})

	It("denies a PlanApproved-only status change from the editor user (D114: no exception)", func() {
		loop := &coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: "p6c-loop", Namespace: ns}}
		var denied bool
		Eventually(func() bool {
			err := statusUpdate(editorCl, loop, setPlanApproved)
			denied = err != nil
			return denied
		}, "15s", "500ms").Should(BeTrue(),
			"the editor must be DENIED even for a PlanApproved-only change (D114)")
	})
})
