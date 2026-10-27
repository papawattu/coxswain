/*
Copyright 2026 papawattu.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// F68 (P2, sec): spec.workspace.ref and spec.verify.acceptanceChecks are
// validated at admission. Both values are interpolated into the verify/deliver
// scripts (shell-quoted since F37), so the CRD bounds are defence in depth:
//
//   - ref is a character whitelist ([A-Za-z0-9._/@+] and '-', at most 255
//     characters, empty allowed = the default branch) and it may not START
//     with '-', because a dash-leading argument is parsed by git as an OPTION:
//     a ref of `-f` makes `git fetch origin <ref>` take a different fetch
//     (measured for `--upload-pack` in .samples/f68/01-ref-option-parse.txt);
//   - acceptanceChecks: at most 16, each 1..4096 characters and not blank. The
//     checks run serially in one verify Job whose deadline is 900s (F53, per
//     Job not per check).
//
// Every spec creates the Loop through the API server, so the CRD validation is
// what decides — a marker dropped from the Go type would accept the object and
// fail here. Each rejection asserts the REASON (422 Invalid naming the field),
// not just "some error", so a create that failed for another cause cannot pass.
var _ = Describe("F68: ref and acceptanceChecks validation", func() {
	newNS := func() (context.Context, context.CancelFunc, string) {
		ctx, cancel := context.WithCancel(context.Background())
		ns := "f68-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
		return ctx, cancel, ns
	}
	teardown := func(cancel context.CancelFunc, ns string) {
		_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		cancel()
	}

	// f68Loop builds the shared unstructured Loop with ref/checks overridden.
	// A nil ref or checks keeps the fixture's default (main / one check); an
	// empty ref or an empty slice SETS that value.
	f68Loop := func(name, ns string, ref *string, checks []string) *unstructured.Unstructured {
		u := mkUnstructuredLoop(name, ns, loopRepo)
		if ref != nil {
			ws, _, err := unstructured.NestedMap(u.Object, unstructuredSpec, unstructuredWs)
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
			ws[unstructuredRef] = *ref
			ExpectWithOffset(1, unstructured.SetNestedMap(u.Object, ws, unstructuredSpec, unstructuredWs)).To(Succeed())
		}
		if checks != nil {
			items := make([]any, 0, len(checks))
			for _, c := range checks {
				items = append(items, c)
			}
			ver, _, err := unstructured.NestedMap(u.Object, unstructuredSpec, unstructuredVer)
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
			ver[unstructuredAcck] = items
			ExpectWithOffset(1, unstructured.SetNestedMap(u.Object, ver, unstructuredSpec, unstructuredVer)).To(Succeed())
		}
		return u
	}

	// f68Rejected asserts the apiserver refused the create BECAUSE the field
	// fails validation: a 422 Invalid whose message names that field (R25 I84).
	f68Rejected := func(ctx context.Context, u *unstructured.Unstructured, wantField, what string) {
		err := k8sClient.Create(ctx, u)
		ExpectWithOffset(1, err).To(HaveOccurred(), "%s must be rejected", what)
		ExpectWithOffset(1, apierrors.IsInvalid(err)).To(BeTrue(),
			"%s must be rejected as INVALID (not by some other failure); got %v", what, err)
		ExpectWithOffset(1, err.Error()).To(ContainSubstring(wantField),
			"the rejection must name %s; got %v", wantField, err)
	}

	It("rejects a ref with a shell metacharacter or a git-forbidden character", func() {
		ctx, cancel, ns := newNS()
		defer teardown(cancel, ns)
		for i, ref := range []string{
			"release 1",               // space
			"main;id",                 // command separator
			"main$(id)",               // command substitution
			"main`id`",                // backtick substitution
			"main~1",                  // git forbids '~' in a ref
			"a|b",                     // pipe
			"refs/heads/x:y",          // git forbids ':' in a ref
			"--upload-pack=/bin/true", // '=' is not in the whitelist
			strings.Repeat("a", 256),  // MaxLength=255
		} {
			r := ref
			name := "f68-bad-ref-" + string(rune('a'+i))
			f68Rejected(ctx, f68Loop(name, ns, &r, nil), "spec.workspace.ref", "ref "+r)
		}
	})

	// The dash rule is a separate rule from the character whitelist, so it needs
	// values the whitelist ALONE would accept. The isolating mutation is to flip
	// the first class to [A-Za-z0-9._/@+-]: these are then admitted and this
	// spec fails while every other ref spec stays green.
	It("rejects a dash-leading ref that the character whitelist alone would allow", func() {
		ctx, cancel, ns := newNS()
		defer teardown(cancel, ns)
		for i, ref := range []string{"-f", "--all", "--tags", "--force", "--dry-run"} {
			r := ref
			name := "f68-dash-ref-" + string(rune('a'+i))
			f68Rejected(ctx, f68Loop(name, ns, &r, nil), "spec.workspace.ref",
				"ref "+r+" (a dash-leading argument is parsed by git as an OPTION)")
		}
	})

	It("accepts the ref shapes a Loop really uses, including empty (the default branch)", func() {
		ctx, cancel, ns := newNS()
		defer teardown(cancel, ns)
		for i, ref := range []string{
			"", // unset / templated-empty: the repo's default branch (the field doc's promise)
			"main",
			"feature/x-1.2",
			"v1.0.0+build",
			"release@2",
			"0123456789abcdef0123456789abcdef01234567", // a pinned commit
			strings.Repeat("a", 255),                   // exactly MaxLength
		} {
			r := ref
			name := "f68-ok-ref-" + string(rune('a'+i))
			Expect(k8sClient.Create(ctx, f68Loop(name, ns, &r, nil))).To(Succeed(),
				"ref %q is valid (or empty = the default branch) and must be accepted", r)
			got := &unstructured.Unstructured{}
			got.SetGroupVersionKind(f68Loop(name, ns, nil, nil).GroupVersionKind())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, got)).To(Succeed())
		}
	})

	It("rejects a blank or whitespace-only acceptance check", func() {
		ctx, cancel, ns := newNS()
		defer teardown(cancel, ns)
		for i, checks := range [][]string{
			{""},
			{"   "},
			{"\t\n "},
			{"go test ./...", ""},
		} {
			name := "f68-blank-check-" + string(rune('a'+i))
			f68Rejected(ctx, f68Loop(name, ns, nil, checks), "spec.verify.acceptanceChecks",
				"checks with a blank entry")
		}
	})

	It("rejects an oversize acceptance check and accepts one at the limit", func() {
		ctx, cancel, ns := newNS()
		defer teardown(cancel, ns)
		over := strings.Repeat("a", 4097)
		f68Rejected(ctx, f68Loop("f68-long-check", ns, nil, []string{over}),
			"spec.verify.acceptanceChecks", "a check longer than maxLength=4096")
		atLimit := strings.Repeat("a", 4096)
		Expect(k8sClient.Create(ctx, f68Loop("f68-limit-check", ns, nil, []string{atLimit}))).To(Succeed(),
			"a check at exactly 4096 characters must be accepted (the positive control for the bound)")
	})

	It("rejects more than 16 acceptance checks and accepts 16", func() {
		ctx, cancel, ns := newNS()
		defer teardown(cancel, ns)
		many := func(n int) []string {
			out := make([]string, 0, n)
			for range n {
				out = append(out, "true")
			}
			return out
		}
		f68Rejected(ctx, f68Loop("f68-many-checks", ns, nil, many(17)),
			"spec.verify.acceptanceChecks", "17 checks (maxItems=16)")
		Expect(k8sClient.Create(ctx, f68Loop("f68-16-checks", ns, nil, many(16)))).To(Succeed(),
			"16 checks are within the bound (the positive control)")
	})

	It("accepts a Loop whose checks carry shell metacharacters (quoting is shellQuote's job, F37)", func() {
		ctx, cancel, ns := newNS()
		defer teardown(cancel, ns)
		Expect(k8sClient.Create(ctx, f68Loop("f68-shell-checks", ns, nil, []string{
			`grep -c "$(printf x)" notes.tmp`,
			`sh -c 'exit 0' && echo "ok; done"`,
			`go test ./... -run '^TestA$'`,
		}))).To(Succeed(), "the F68 bounds are blank/length only: a check is a shell command and keeps its metacharacters")
	})

	// The bounds are also enforced on UPDATE, which is what the upgrade notes
	// describe: CRD validation ratcheting (1.30+) excuses a field an update does
	// NOT change, but a change to it must be valid — so an out-of-bounds value
	// cannot be written, and the fixing patch can.
	It("enforces the bounds on an update that changes the field", func() {
		ctx, cancel, ns := newNS()
		defer teardown(cancel, ns)
		name := "f68-update-ref"
		ref := "main"
		Expect(k8sClient.Create(ctx, f68Loop(name, ns, &ref, nil))).To(Succeed())

		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(f68Loop(name, ns, nil, nil).GroupVersionKind())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, got)).To(Succeed())
		ws, _, err := unstructured.NestedMap(got.Object, unstructuredSpec, unstructuredWs)
		Expect(err).NotTo(HaveOccurred())
		ws[unstructuredRef] = "bad ref"
		Expect(unstructured.SetNestedMap(got.Object, ws, unstructuredSpec, unstructuredWs)).To(Succeed())

		err = k8sClient.Update(ctx, got)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(),
			"an update that CHANGES ref to an out-of-bounds value must be rejected as invalid; got %v", err)
		Expect(err.Error()).To(ContainSubstring("spec.workspace.ref"))

		// The fixing patch (empty = the default branch) is valid.
		ws[unstructuredRef] = ""
		Expect(unstructured.SetNestedMap(got.Object, ws, unstructuredSpec, unstructuredWs)).To(Succeed())
		Expect(k8sClient.Update(ctx, got)).To(Succeed(),
			"the remediation the upgrade notes give (patch the field to a valid value) must be accepted")
	})

	It("still accepts the plain fixture Loop", func() {
		ctx, cancel, ns := newNS()
		defer teardown(cancel, ns)
		Expect(k8sClient.Create(ctx, mkUnstructuredLoop("f68-control", ns, loopRepo))).To(Succeed(),
			"the shared fixture (ref main, one check) must still be admitted")
	})

	// F4 (the upgrade hazard, docs/UPGRADE.md): a Loop whose STORED spec no
	// longer passes admission cannot have its finalizer removed, so on delete it
	// sits in Terminating. That must be visible (an Event naming the reason and
	// the doc), and it must not spin on the 5s terminating-namespace timer: the
	// fix needs a human, so the requeue is the slow one.
	It("a deleting Loop whose finalizer-removal Update is rejected as Invalid surfaces an Event and requeues slowly (F68)", func() {
		ns := "f68-invalid-fin-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()
		b1CreateVSC()

		name := "f68invfin"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b1LoopSpec(),
		})).To(Succeed())

		r := b1NewReconciler(true, false)
		recorder := record.NewFakeRecorder(64)
		r.Recorder = recorder

		// The finalizer is added with the real client (the refusal under test is
		// the REMOVAL on delete).
		b1Reconcile(r, ns, name)
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		Expect(k8sClient.Delete(ctx, loop)).To(Succeed())
		for range 5 {
			got := &coxv1alpha1.Loop{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, got); err == nil {
				Expect(got.DeletionTimestamp).NotTo(BeZero(), "the deleting Loop is held by the finalizer")
				break
			}
			time.Sleep(50 * time.Millisecond)
		}

		// Now every Loop Update is refused the way the apiserver refuses an
		// object whose stored spec fails the tightened CRD bounds.
		r.Client = &f68InvalidUpdateClient{Client: k8sClient}
		r.apiReader = k8sClient

		res, err := r.Reconcile(context.Background(),
			reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		Expect(err).ToNot(HaveOccurred(),
			"an Invalid finalizer-removal Update is not a reconcile error: it is surfaced and retried on a timer")
		Expect(res.RequeueAfter).To(Equal(finalizerRemovalInvalidSpecRequeue),
			"the Invalid case requeues on the slow timer (a human must fix the spec), not the 5s terminating-namespace one")

		var event string
		for len(recorder.Events) > 0 {
			if e := <-recorder.Events; strings.Contains(e, "FinalizerRemovalBlocked") {
				event = e
				break
			}
		}
		Expect(event).ToNot(BeEmpty(), "the refusal must be surfaced as a FinalizerRemovalBlocked Event (R26 I90)")
		Expect(event).To(ContainSubstring("CRD admission"),
			"the Event must say WHY the update was refused, not just that it was")
		Expect(event).To(ContainSubstring("docs/UPGRADE.md"),
			"the Event must point at the remediation the upgrade notes carry")

		got := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, got)).To(Succeed())
		Expect(got.Finalizers).To(ContainElement(loopCheckpointFinalizer),
			"the finalizer stays when the Update is refused (the requeue retries after the spec is fixed)")
	})
})

// f68InvalidUpdateClient delegates to the envtest client but refuses every Loop
// Update with an admission Invalid — the F68 upgrade shape, where the API
// server validates the whole stored object and the spec no longer passes.
type f68InvalidUpdateClient struct {
	client.Client
}

var _ client.Client = &f68InvalidUpdateClient{}

func (c *f68InvalidUpdateClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if loop, ok := obj.(*coxv1alpha1.Loop); ok {
		return apierrors.NewInvalid(
			coxv1alpha1.GroupVersion.WithKind("Loop").GroupKind(), loop.Name,
			field.ErrorList{field.Invalid(field.NewPath("spec", "workspace", "ref"),
				"(stored before the F68 bounds)", "must match the ref pattern")})
	}
	return c.Client.Update(ctx, obj, opts...)
}
