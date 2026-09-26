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
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// D20: agent-sandbox creates a Service named after the Sandbox, and Service
// names are DNS-1035 labels (lowercase, start with a letter, <= 63, no dots).
// Loop names are DNS-1123 subdomains (may contain dots, start with a digit,
// up to 253). So a Loop named `fix.auth` or `1-bug` produces a Sandbox whose
// Service can't be created. The CRD CEL validation on metadata.name rejects
// such Loop names at admission (max 55 chars = 63 - len("-sandbox")), and
// sandboxName is then just name + "-sandbox".
var _ = Describe("Loop name validation (D20)", func() {
	newNS := func() (context.Context, context.CancelFunc, string) {
		ctx, cancel := context.WithCancel(context.Background())
		ns := "d20-test-" + fmt.Sprint(time.Now().UnixNano())
		Expect(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
		return ctx, cancel, ns
	}
	teardown := func(cancel context.CancelFunc, ns string) {
		_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		cancel()
	}

	It("rejects a Loop name with a dot (fix.auth)", func() {
		ctx, cancel, ns := newNS()
		defer teardown(cancel, ns)
		u := mkUnstructuredLoop("fix.auth", ns, loopRepo)
		err := k8sClient.Create(ctx, u)
		Expect(err).To(HaveOccurred(), "a dotted Loop name must be rejected")
		Expect(err.Error()).To(ContainSubstring("DNS-1035"),
			"the error should be the CRD validation message, not a later failure")
	})

	It("rejects a Loop name starting with a digit (1-bug)", func() {
		ctx, cancel, ns := newNS()
		defer teardown(cancel, ns)
		u := mkUnstructuredLoop("1-bug", ns, loopRepo)
		err := k8sClient.Create(ctx, u)
		Expect(err).To(HaveOccurred(), "a digit-leading Loop name must be rejected")
		Expect(err.Error()).To(ContainSubstring("DNS-1035"))
	})

	It("rejects a 56-character Loop name", func() {
		ctx, cancel, ns := newNS()
		defer teardown(cancel, ns)
		// 56 lowercase a's: valid DNS-1035 chars but too long (63 - 8 = 55 max).
		name := strings.Repeat("a", 56)
		u := mkUnstructuredLoop(name, ns, loopRepo)
		err := k8sClient.Create(ctx, u)
		Expect(err).To(HaveOccurred(), "a 56-char Loop name must be rejected")
		Expect(err.Error()).To(ContainSubstring("DNS-1035"))
	})

	It("accepts a 55-char DNS-1035 Loop name and its Sandbox is <name>-sandbox", func() {
		ctx, cancel, ns := newNS()
		defer teardown(cancel, ns)
		// 55 lowercase a's.
		name := strings.Repeat("a", 55)
		u := mkUnstructuredLoop(name, ns, loopRepo)
		Expect(k8sClient.Create(ctx, u)).To(Succeed(), "a 55-char DNS-1035 name must be accepted")
		// Reconcile and assert the Sandbox name is exactly <name>-sandbox.
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}
		_, err := r.Reconcile(ctx, req)
		Expect(err).ToNot(HaveOccurred())
		sbs := &sandboxv1beta1.SandboxList{}
		Expect(k8sClient.List(ctx, sbs, client.InNamespace(ns))).To(Succeed())
		Expect(sbs.Items).To(HaveLen(1), "exactly one sandbox for the Loop")
		Expect(sbs.Items[0].Name).To(Equal(name+"-sandbox"),
			"sandboxName must be <name>-sandbox (no hash truncation for valid names)")
	})
})
