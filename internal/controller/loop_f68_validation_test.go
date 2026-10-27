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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// F68 (P2, sec): spec.workspace.ref and spec.verify.acceptanceChecks are
// validated at admission. Both values are interpolated into the verify/deliver
// scripts (shell-quoted since F37), so the CRD bounds are defence in depth:
//
//   - ref must be a git ref name — and must not START with '-', because a
//     dash-leading argument is parsed by git as an OPTION: a ref of
//     `--upload-pack=/bin/true` makes `git fetch origin <ref>` run that program
//     (measured: .samples/f68/01-ref-option-parse.txt);
//   - acceptanceChecks: at most 16, each 1..4096 characters and not blank. The
//     checks run serially in one verify Job whose deadline is 900s (F53, per
//     Job not per check).
//
// Every spec creates the Loop through the API server, so the CRD validation is
// what decides — a marker dropped from the Go type would accept the object and
// fail here.
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
	// empty slice SETS an empty list.
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

	It("rejects a ref git could not check out or that carries a shell metacharacter", func() {
		ctx, cancel, ns := newNS()
		defer teardown(cancel, ns)
		for i, ref := range []string{
			"release 1",              // space
			"main;id",                // command separator
			"main$(id)",              // command substitution
			"main`id`",               // backtick substitution
			"main~1",                 // git forbids '~' in a ref
			"a|b",                    // pipe
			"refs/heads/x:y",         // git forbids ':' in a ref
			strings.Repeat("a", 256), // MaxLength=255
		} {
			r := ref
			Expect(k8sClient.Create(ctx, f68Loop("f68-bad-ref-"+strings.Repeat("x", i%3)+string(rune('a'+i)), ns, &r, nil))).
				ToNot(Succeed(), "ref %q must be rejected by the CRD pattern/length", r)
		}
	})

	It("rejects a dash-leading ref, which git would parse as an OPTION", func() {
		ctx, cancel, ns := newNS()
		defer teardown(cancel, ns)
		ref := "--upload-pack=/bin/true"
		Expect(k8sClient.Create(ctx, f68Loop("f68-dash-ref", ns, &ref, nil))).ToNot(Succeed(),
			"a dash-leading ref makes `git fetch origin <ref>` run the program it names")
	})

	It("accepts the ref shapes a Loop really uses", func() {
		ctx, cancel, ns := newNS()
		defer teardown(cancel, ns)
		for i, ref := range []string{
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
				"ref %q is a valid git ref name and must be accepted", r)
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
			Expect(k8sClient.Create(ctx, f68Loop("f68-blank-check-"+string(rune('a'+i)), ns, nil, checks))).
				ToNot(Succeed(), "a blank check %q must be rejected", checks)
		}
	})

	It("rejects an oversize acceptance check and accepts one at the limit", func() {
		ctx, cancel, ns := newNS()
		defer teardown(cancel, ns)
		over := strings.Repeat("a", 4097)
		Expect(k8sClient.Create(ctx, f68Loop("f68-long-check", ns, nil, []string{over}))).ToNot(Succeed(),
			"a check longer than maxLength=4096 must be rejected")
		atLimit := strings.Repeat("a", 4096)
		Expect(k8sClient.Create(ctx, f68Loop("f68-limit-check", ns, nil, []string{atLimit}))).To(Succeed(),
			"a check at exactly 4096 characters must be accepted (the positive control for the bound)")
	})

	It("rejects more than 16 acceptance checks and accepts 16", func() {
		ctx, cancel, ns := newNS()
		defer teardown(cancel, ns)
		many := func(n int) []string {
			out := make([]string, 0, n)
			for i := 0; i < n; i++ {
				out = append(out, "true")
			}
			return out
		}
		Expect(k8sClient.Create(ctx, f68Loop("f68-many-checks", ns, nil, many(17)))).ToNot(Succeed(),
			"17 checks exceed maxItems=16")
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

	It("still accepts the plain fixture Loop", func() {
		ctx, cancel, ns := newNS()
		defer teardown(cancel, ns)
		Expect(k8sClient.Create(ctx, mkUnstructuredLoop("f68-control", ns, loopRepo))).To(Succeed(),
			"the shared fixture (ref main, one check) must still be admitted")
	})
})
