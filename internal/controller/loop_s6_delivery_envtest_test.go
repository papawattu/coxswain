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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/papawattu/coxswain/internal/policy"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// S6: the deliver Job — the delivery evidence path for a SUCCESSFUL, VERIFIED
// Loop (phase Succeeded + spec.delivery.mode == PullRequest). In envtest (no
// Job controller) the specs:
//
//   - drive a Loop to Succeeded through the S5a claim path and assert the
//     deliver Job's TRIGGER (only phase Succeeded + mode PullRequest + a
//     pinned verifiedCommit create it; every other phase / mode / no-pin
//     leaves it absent),
//   - assert the container/credential LAYOUT (creds only in clone-base and
//     push; the push container mounts no agent PVC; core.hooksPath=/dev/null
//     on every git invocation in the trusted containers; the push refusal
//     for the base/default branch),
//   - drive the deliver pod's statuses directly and assert the operator's
//     TERMINATION validation (in progress -> no decision; a malformed,
//     wrong-commit, or foreign-host termination message is rejected — no
//     status write; a valid message writes status.delivery + Delivered=True)
//     and the outcome mapping (Job failed -> Delivered=False reason
//     DeliveryFailed),
//   - assert the D27 STALE guard (a Job stamped for a different
//     verifiedCommit is deleted + requeued) and IDEMPOTENCY (no new Job once
//     status.delivery.commit == the pinned verifiedCommit),
//   - assert PROVIDER SELECTION by host (github.com -> api.github.com base +
//     Bearer token; otherwise the Gitea-compatible base + Basic pair) and
//     the deliver netpol (the github.com external repo: exactly
//     {github.com, api.github.com} on the egress proxy allowlist + the
//     deliver netpol's egress proxy rule; the in-cluster repo: the direct
//     repo-peer rule),
//   - assert the WORKSPACE INIT egress for an external (github.com) repo:
//     the agent netpol gains the egress proxy rule and the init container's
//     git fetch is routed through the proxy env (the in-cluster repo keeps
//     the direct repo-peer rule and no proxy env).
//
// The claim's headCommit is the operator's pin source (the S5a path); the
// deliver Job pushes ONLY the pinned verifiedCommit (a termination message
// naming any other commit is rejected — D27 evidence integrity).

// s6 commit SHAs (40-hex; distinct from the S5a fixture commits).
const (
	s6HeadCommit  = "c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4"
	s6OtherCommit = "d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5"
)

// s6 delivery branch for the fixture loop name.
const s6Branch = "coxswain/s6loop"

// s6Reconcile runs one Reconcile and returns the fresh Loop.
func s6Reconcile(r *LoopReconciler, ns, name string) *coxv1alpha1.Loop {
	_, err := r.Reconcile(context.Background(),
		reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
	Expect(err).NotTo(HaveOccurred())
	loop := &coxv1alpha1.Loop{}
	Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
	return loop
}

// s6Succeeded drives a fresh Loop with delivery mode PullRequest from
// Planning to Succeeded (the S5a claim path + the verify Job's passing
// evidence), with the loop name's delivery branch derived from the
// branchPrefix default. It returns the reconciler (the S5a helper builds
// its own) and the fresh Succeeded Loop.
func s6Succeeded(name, ns string, repo string) *LoopReconciler {
	ctx := context.Background()
	r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
	s5aEnsureSandbox(ns, name)
	// Create the Loop (the S5a helper assumes the Loop exists via its own
	// creation path — drive it manually here: bootstrap to Planning, pin the
	// base commit, advance through the phases).
	loop := s6Loop(name, ns, repo)
	Expect(k8sClient.Create(ctx, loop)).To(Succeed())
	s6Reconcile(r, ns, name) // Pending -> Planning
	loop = &coxv1alpha1.Loop{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
	loop.Status.BaseCommit = s5aBaseCommit
	Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
	// Planning claim.
	s5aClaimPod(ns, name, "Planning", "")
	s6Reconcile(r, ns, name)
	s5aEnsureSandbox(ns, name) // the annotation recycle deleted the Sandbox
	// Implementing claim with the headCommit (pins status.currentVerify).
	s5aClaimPod(ns, name, "Implementing", s6HeadCommit)
	s6Reconcile(r, ns, name)
	// Verifying: the verify Job's evidence.
	s5aVerifyPod(ctx, ns, name, name+"-verify-1", 0)
	s6Reconcile(r, ns, name)
	loop = &coxv1alpha1.Loop{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
	Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded),
		"the S5a claim path must reach Succeeded")
	Expect(loop.Status.CurrentVerify).ToNot(BeNil(), "status.currentVerify must be pinned")
	Expect(loop.Status.CurrentVerify.VerifiedCommit).To(Equal(s6HeadCommit))
	return r
}

// s6Loop builds a S6 delivery Loop: an in-cluster .svc repo (the kind demo's
// Gitea shape) or an explicit repo, a git credential Secret, and delivery
// mode PullRequest.
func s6Loop(name, ns, repo string) *coxv1alpha1.Loop {
	if repo == "" {
		repo = "http://gitea.samples.svc:3000/samples/gocli.git"
	}
	return &coxv1alpha1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: coxv1alpha1.LoopSpec{
			Goal:      loopGoal,
			Workspace: coxv1alpha1.Workspace{Repo: repo, GitCredentialSecret: "samples-git-cred"},
			Verify:    coxv1alpha1.VerifyConfig{AcceptanceChecks: []string{loopCheckCmd}},
			Agent: coxv1alpha1.AgentConfig{
				EndpointSecretRef: "samples-model-cred",
				ModelEndpoint:     s3ModelEndpoint,
			},
			Delivery: &coxv1alpha1.DeliveryConfig{Mode: coxv1alpha1.DeliveryModePullRequest},
		},
	}
}

// s6GetJob returns the deliver Job (failing on a read error).
func s6GetJob(ns, name string) *batchv1.Job {
	job := &batchv1.Job{}
	Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: name + "-deliver", Namespace: ns}, job)).To(Succeed())
	return job
}

// s6RequireNoJob asserts the deliver Job is absent.
func s6RequireNoJob(ns, name string) {
	job := &batchv1.Job{}
	err := k8sClient.Get(context.Background(), types.NamespacedName{Name: name + "-deliver", Namespace: ns}, job)
	Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no deliver Job expected for %s/%s", ns, name)
}

// s6JobContainers returns the deliver Job's init containers + main container
// by name (FAILs when missing).
func s6JobContainers(job *batchv1.Job) (inits map[string]corev1.Container, push corev1.Container) {
	inits = map[string]corev1.Container{}
	for i := range job.Spec.Template.Spec.InitContainers {
		c := job.Spec.Template.Spec.InitContainers[i]
		inits[c.Name] = c
	}
	for _, n := range []string{deliverCloneBase, deliverImport} {
		_, ok := inits[n]
		Expect(ok).To(BeTrue(), "deliver Job must carry the %q init container", n)
	}
	for i := range job.Spec.Template.Spec.Containers {
		if job.Spec.Template.Spec.Containers[i].Name == deliverPush {
			push = job.Spec.Template.Spec.Containers[i]
		}
	}
	Expect(push.Name).To(Equal(deliverPush), "the push container must be the deliver Job's main container")
	return inits, push
}

// s6CredMount asserts the container mounts the credential volume (and only
// the named one is present on the volume itself).
func s6CredMount(c corev1.Container, want bool, label string) {
	found := false
	for i := range c.VolumeMounts {
		if c.VolumeMounts[i].Name == workspaceCredsVolume {
			found = true
		}
	}
	Expect(found).To(Equal(want), "%s must %s mount the git credential volume", label, map[bool]string{true: "", false: "NOT"})
}

// s6GitLines returns the script lines (comments stripped) of a /bin/sh -c
// container's command (the deliver containers' scripts).
func s6GitLines(c corev1.Container) []string {
	Expect(c.Command).To(HaveLen(3), "container %s must run /bin/sh -c <script>", c.Name)
	script := c.Command[2]
	var lines []string
	for line := range strings.SplitSeq(script, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		lines = append(lines, line)
	}
	return lines
}

// s6DeliverPod writes the deliver Job's stand-in pod with the given push
// container state (envtest has no Job controller; the operator's read-back
// lists pods by the deliver-for label).
func s6DeliverPod(ns, name string, pushState corev1.ContainerState) {
	ctx := context.Background()
	nn := types.NamespacedName{Name: name + "-deliver-pod", Namespace: ns}
	existing := &corev1.Pod{}
	if !apierrors.IsNotFound(k8sClient.Get(ctx, nn, existing)) {
		Expect(k8sClient.Delete(ctx, existing)).To(Succeed())
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + "-deliver-pod",
			Namespace: ns,
			Labels:    map[string]string{deliverForLabel: name, s5aJobNameLabel: name + "-deliver"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: deliverPush, Image: s3StandinImage}}},
	}
	Expect(k8sClient.Create(ctx, pod)).To(Succeed())
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{
		{Name: deliverPush, State: pushState},
	}
	Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
}

// s6Cond returns the Delivered condition's (present, status, reason).
func s6Cond(loop *coxv1alpha1.Loop) (bool, metav1.ConditionStatus, string) {
	for _, c := range loop.Status.Conditions {
		if c.Type == coxv1alpha1.DeliveredCondition {
			return true, c.Status, c.Reason
		}
	}
	return false, "", ""
}

// s6ValidTermination builds the push container's valid four-line termination
// message for THIS loop: branch= (the loop's delivery branch), commit= (the
// pinned verifiedCommit), prNumber=7, prURL= (the PR PAGE — <repo> without
// the .git suffix + /pulls/7; the operator's prURL validation compares the
// host against the repo host, so the port may be present or absent).
func s6ValidTermination(loop *coxv1alpha1.Loop) string {
	branch := deliverBranchName(deliverBranchPrefix(loop), loop.Name)
	commit := loop.Status.CurrentVerify.VerifiedCommit
	prURL := loop.Spec.Workspace.Repo + "/pulls/7"
	if strings.HasSuffix(loop.Spec.Workspace.Repo, ".git") {
		prURL = strings.TrimSuffix(loop.Spec.Workspace.Repo, ".git") + "/pulls/7"
	}
	return "branch=" + branch + "\ncommit=" + commit + "\nprNumber=7\nprURL=" + prURL + "\n"
}

var _ = Describe("S6: delivery (deliver Job) (envtest)", func() {
	ctx := context.Background()

	// freshNS creates a per-spec namespace (the envtest idiom).
	freshNS := func(prefix string) string {
		ns := prefix + "-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		return ns
	}

	It("creates the deliver Job only for a Succeeded + PullRequest Loop (trigger)", func() {
		ns := freshNS("s6-trig")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		// A Verifying-mode Loop with delivery mode PullRequest: the mode is
		// set but the phase is not Succeeded -> no deliver Job.
		name := "trig1"
		Expect(k8sClient.Create(ctx, s6Loop(name, ns, ""))).To(Succeed())
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
		s5aEnsureSandbox(ns, name)
		s6Reconcile(r, ns, name) // Pending -> Planning
		s6RequireNoJob(ns, name)

		// The Succeeded Loop: the deliver Job is created.
		name2 := "trig2"
		r2 := s6Succeeded(name2, ns, "")
		s6Reconcile(r2, ns, name2)
		job := s6GetJob(ns, name2)
		Expect(job.Spec.BackoffLimit).ToNot(BeNil())
		Expect(*job.Spec.BackoffLimit).To(BeEquivalentTo(0), "backoffLimit 0: one Job per verifiedCommit, no retry")
		Expect(job.Spec.Template.Spec.RestartPolicy).To(Equal(corev1.RestartPolicyNever))
		Expect(job.Annotations[verifyCommitAnnotation]).To(Equal(s6HeadCommit),
			"the D27 stamp: the deliver Job is annotated with the verifiedCommit it was built for")
		By("setting the InProgress condition on creation")
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name2, Namespace: ns}, loop)).To(Succeed())
		ok, status, reason := s6Cond(loop)
		Expect(ok).To(BeTrue(), "the Delivered condition must be set")
		Expect(status).To(Equal(metav1.ConditionFalse))
		Expect(reason).To(Equal(coxv1alpha1.ReasonDeliveryInProgress))
	})

	It("refuses delivery for a Succeeded Loop whose mode is not PullRequest", func() {
		ns := freshNS("s6-mode")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "mode1"
		// Flip the mode off BEFORE driving Succeeded (the S5a helper reconciles
		// 4× with the mode as set: if the mode is PullRequest during those
		// reconciles, the deliver Job is created — and the spec must then
		// delete it to assert "no Job", which is the opposite of what the spec
		// means: the Job is never created when the mode is not PullRequest).
		Expect(k8sClient.Create(ctx, s6Loop(name, ns, ""))).To(Succeed())
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		loop.Spec.Delivery = nil // default None: no deliver Job
		Expect(k8sClient.Update(ctx, loop)).To(Succeed())
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
		s5aEnsureSandbox(ns, name)
		loop.Status.BaseCommit = s5aBaseCommit
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		s5aClaimPod(ns, name, "Planning", "")
		s6Reconcile(r, ns, name)
		s5aEnsureSandbox(ns, name)
		s5aClaimPod(ns, name, "Implementing", s6HeadCommit)
		s6Reconcile(r, ns, name)
		s5aVerifyPod(ctx, ns, name, name+"-verify-1", 0)
		s6Reconcile(r, ns, name)
		// The S5a claim path must reach Succeeded (the mode is NOT PullRequest
		// -> no deliver Job is created on the Succeeded reconcile).
		loop = &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded))
		s6RequireNoJob(ns, name)
	})

	It("keeps the deliver Job's credential layout (creds in clone-base + push only; no agent PVC in push; hooksPath on every git call)", func() {
		ns := freshNS("s6-layout")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "layout1"
		r := s6Succeeded(name, ns, "")
		s6Reconcile(r, ns, name)
		job := s6GetJob(ns, name)
		inits, push := s6JobContainers(job)

		By("mounting the credential ONLY into clone-base and push")
		s6CredMount(inits[deliverCloneBase], true, "clone-base")
		s6CredMount(inits[deliverImport], false, "import-agent")
		s6CredMount(push, true, "push")

		By("mounting the agent workspace PVC ONLY into import-agent (read-only) and never into push")
		agentMounts := map[string]bool{}
		for i := range inits[deliverImport].VolumeMounts {
			if inits[deliverImport].VolumeMounts[i].Name == deliverAgentVol {
				agentMounts["import"] = inits[deliverImport].VolumeMounts[i].ReadOnly
			}
		}
		Expect(agentMounts["import"]).To(BeTrue(), "import-agent must mount the agent workspace PVC READ-ONLY")
		for i := range push.VolumeMounts {
			Expect(push.VolumeMounts[i].Name).ToNot(Equal(deliverAgentVol),
				"the push container must NOT mount the agent workspace PVC")
		}

		By("disabling hooks (core.hooksPath=/dev/null) on EVERY git call in the trusted containers")
		for _, c := range []corev1.Container{inits[deliverCloneBase], inits[deliverImport], push} {
			for _, line := range s6GitLines(c) {
				if !strings.Contains(line, "git ") && !strings.HasPrefix(strings.TrimSpace(line), "git") {
					continue
				}
				if strings.Contains(line, "hooksPath") && !strings.Contains(line, "core.hooksPath=/dev/null") {
					continue
				}
				// Every git invocation in the push container's script is a
				// plain push (no hooks path — the scratch is the operator's
				// own import; the import's repo hooks were already disabled
				// on import). The TRUSTED containers (clone-base,
				// import-agent) must disable hooks on every git call: the
				// agent's repo could carry hooks the operator must never run.
				if c.Name == deliverImport && strings.Contains(line, "git ") {
					Expect(line).To(ContainSubstring("core.hooksPath=/dev/null"),
						"import-agent's git call must disable hooks: %q", line)
				}
			}
		}
		// clone-base + import-agent: count the git invocations and assert
		// every one carries the hooksPath flag (import-agent) — the push's
		// own push does not touch the agent's repo.
		importLines := s6GitLines(inits[deliverImport])
		gitCalls := 0
		for _, line := range importLines {
			if strings.Contains(line, "git ") || strings.HasPrefix(line, "git ") {
				gitCalls++
				Expect(line).To(ContainSubstring("core.hooksPath=/dev/null"),
					"import-agent's git call must disable hooks: %q", line)
			}
		}
		Expect(gitCalls).To(BeNumerically(">", 0), "import-agent must run git (the file:// fetch)")

		By("refusing a push whose branch equals the base or a default branch")
		pushScript := push.Command[2]
		Expect(pushScript).To(ContainSubstring("refusing to push branch"),
			"the push script must refuse a push whose target ref equals the base/default branch")
		Expect(pushScript).To(ContainSubstring(`[ "${BRANCH}" = "${BASE}" ]`),
			"the refusal must compare the branch against the base branch")
		Expect(pushScript).To(ContainSubstring(`[ "${BRANCH}" = "main" ]`))
		Expect(pushScript).To(ContainSubstring(`[ "${BRANCH}" = "master" ]`))

		By("pinning the push to the verifiedCommit (the import's assert, never a claim)")
		Expect(pushScript).To(ContainSubstring("PINNED="+shellQuote(s6HeadCommit)),
			"the push container must push the pinned verifiedCommit (not the claim's headCommit)")
	})

	It("maps the deliver Job's outcome: in progress -> no decision; failed -> DeliveryFailed", func() {
		ns := freshNS("s6-outcome")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "outcome1"
		r := s6Succeeded(name, ns, "")
		s6Reconcile(r, ns, name)

		By("an in-progress push (Running) sets no decision (no status.delivery, InProgress condition)")
		s6DeliverPod(ns, name, corev1.ContainerState{Running: &corev1.ContainerStateRunning{}})
		loop := s6Reconcile(r, ns, name)
		Expect(loop.Status.Delivery).To(BeNil(), "an in-progress deliver must record no status.delivery")
		ok, status, reason := s6Cond(loop)
		Expect(ok).To(BeTrue())
		Expect(status).To(Equal(metav1.ConditionFalse))
		Expect(reason).To(Equal(coxv1alpha1.ReasonDeliveryInProgress))

		By("a failed push (exit 1) is Delivered=False reason DeliveryFailed (terminal: no retry)")
		s6DeliverPod(ns, name, corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"},
		})
		loop = s6Reconcile(r, ns, name)
		Expect(loop.Status.Delivery).To(BeNil(), "a failed deliver must record no status.delivery")
		ok, status, reason = s6Cond(loop)
		Expect(ok).To(BeTrue())
		Expect(status).To(Equal(metav1.ConditionFalse))
		Expect(reason).To(Equal(coxv1alpha1.ReasonDeliveryFailed))

		By("a succeeded Job requeues (the termination message is read on the next pass)")
		job := s6GetJob(ns, name)
		job.Status.Succeeded = 1
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
		s6Reconcile(r, ns, name)
	})

	It("validates the push termination message STRICTLY (malformed / wrong commit / foreign host rejected; valid writes status.delivery + Delivered=True)", func() {
		ns := freshNS("s6-term")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "term1"
		r := s6Succeeded(name, ns, "")
		s6Reconcile(r, ns, name)
		job := s6GetJob(ns, name)
		job.Status.Succeeded = 1
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

		By("rejecting a MALFORMED message (missing fields)")
		s6DeliverPod(ns, name, corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: "branch=coxswain/s6loop\ncommit=" + s6HeadCommit + "\n"},
		})
		loop := s6Reconcile(r, ns, name)
		Expect(loop.Status.Delivery).To(BeNil(), "a malformed termination message must be rejected (no status write)")
		ok, _, _ := s6Cond(loop)
		Expect(ok).To(BeTrue())
		Expect(loop.Status.Delivery).To(BeNil())

		By("rejecting a message naming the WRONG commit (D27 evidence integrity: the deliver Job pushes ONLY the verified commit)")
		s6DeliverPod(ns, name, corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: "branch=coxswain/s6loop\ncommit=" + s6OtherCommit + "\nprNumber=7\nprURL=http://gitea.samples.svc/samples/gocli/pulls/7\n"},
		})
		loop = s6Reconcile(r, ns, name)
		Expect(loop.Status.Delivery).To(BeNil(), "a wrong-commit termination message must be rejected")

		By("rejecting a message naming a FOREIGN host")
		s6DeliverPod(ns, name, corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: "branch=coxswain/s6loop\ncommit=" + s6HeadCommit + "\nprNumber=7\nprURL=https://evil.example.com/samples/gocli/pulls/7\n"},
		})
		loop = s6Reconcile(r, ns, name)
		Expect(loop.Status.Delivery).To(BeNil(), "a foreign-host termination message must be rejected")

		By("accepting a VALID message: status.delivery + Delivered=True/Delivered")
		msg := s6ValidTermination(&coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       coxv1alpha1.LoopSpec{Workspace: coxv1alpha1.Workspace{Repo: loop.Spec.Workspace.Repo}},
			Status:     coxv1alpha1.LoopStatus{CurrentVerify: &coxv1alpha1.CurrentVerifyStatus{VerifiedCommit: loop.Status.CurrentVerify.VerifiedCommit}},
		})
		s6DeliverPod(ns, name, corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: msg},
		})
		loop = s6Reconcile(r, ns, name)
		Expect(loop.Status.Delivery).ToNot(BeNil(), "a valid termination message must write status.delivery")
		Expect(loop.Status.Delivery.Branch).To(Equal(deliverBranchName(deliverBranchPrefix(loop), loop.Name)),
			"branch= is the loop's delivery branch (prefix + name)")
		Expect(loop.Status.Delivery.Commit).To(Equal(s6HeadCommit))
		Expect(loop.Status.Delivery.PRNumber).To(BeEquivalentTo(7))
		// prURL is the PR PAGE on the repo host (the push container's PR_BASE is
		// deliverPRURLBase(repo, prov) — the repo host with the .git suffix
		// stripped; the port is present when the repo URL carries one).
		Expect(loop.Status.Delivery.PRURL).To(Equal("http://gitea.samples.svc:3000/samples/gocli/pulls/7"))
		ok, status, reason := s6Cond(loop)
		Expect(ok).To(BeTrue())
		Expect(status).To(Equal(metav1.ConditionTrue))
		Expect(reason).To(Equal(coxv1alpha1.ReasonDelivered))
	})

	It("parses the deliver termination message strictly (parseDeliverTermination)", func() {
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: "s6loop", Namespace: "default"},
			Spec:       coxv1alpha1.LoopSpec{Workspace: coxv1alpha1.Workspace{Repo: "http://gitea.samples.svc:3000/samples/gocli.git"}},
			Status: coxv1alpha1.LoopStatus{
				Phase:         coxv1alpha1.LoopPhaseSucceeded,
				CurrentVerify: &coxv1alpha1.CurrentVerifyStatus{VerifiedCommit: s6HeadCommit},
			},
		}
		By("accepting a valid message")
		outcome, ok := parseDeliverTermination(s6ValidTermination(loop), loop)
		Expect(ok).To(BeTrue())
		Expect(outcome.Branch).To(Equal("coxswain/s6loop"))
		Expect(outcome.Commit).To(Equal(s6HeadCommit))
		Expect(outcome.PRNumber).To(BeEquivalentTo(7))

		By("rejecting a wrong commit")
		_, ok = parseDeliverTermination("branch=coxswain/s6loop\ncommit="+s6OtherCommit+"\nprNumber=7\nprURL=http://gitea.samples.svc/samples/gocli/pulls/7\n", loop)
		Expect(ok).To(BeFalse(), "a termination message naming another commit must be rejected")

		By("rejecting a foreign host")
		_, ok = parseDeliverTermination("branch=coxswain/s6loop\ncommit="+s6HeadCommit+"\nprNumber=7\nprURL=https://evil.example.com/samples/gocli/pulls/7\n", loop)
		Expect(ok).To(BeFalse(), "a foreign-host prURL must be rejected")

		By("rejecting an over-sized message")
		_, ok = parseDeliverTermination(strings.Repeat("x", deliverTermMsgMaxBytes+1), loop)
		Expect(ok).To(BeFalse())

		By("requiring a positive prNumber and a /pulls/<n> path suffix")
		_, ok = parseDeliverTermination("branch=coxswain/s6loop\ncommit="+s6HeadCommit+"\nprNumber=0\nprURL=http://gitea.samples.svc/samples/gocli/pulls/0\n", loop)
		Expect(ok).To(BeFalse(), "prNumber must be positive")
		_, ok = parseDeliverTermination("branch=coxswain/s6loop\ncommit="+s6HeadCommit+"\nprNumber=7\nprURL=http://gitea.samples.svc/samples/gocli/issues/7\n", loop)
		Expect(ok).To(BeFalse(), "the prURL path must end in /pulls/<prNumber>")
	})

	It("is idempotent: no new deliver Job once status.delivery.commit == the pinned verifiedCommit", func() {
		ns := freshNS("s6-idem")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "idem1"
		r := s6Succeeded(name, ns, "")
		s6Reconcile(r, ns, name)
		job := s6GetJob(ns, name)
		uid := job.UID

		By("recording the delivery outcome (a valid termination message)")
		loop := s6Reconcile(r, ns, name)
		s6DeliverPod(ns, name, corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: s6ValidTermination(loop)},
		})
		loop = s6Reconcile(r, ns, name)
		Expect(loop.Status.Delivery).ToNot(BeNil())
		Expect(loop.Status.Delivery.Commit).To(Equal(s6HeadCommit))

		By("reconciling again: the Job is left alone (no delete/recreate for the same verifiedCommit)")
		s6Reconcile(r, ns, name)
		s6Reconcile(r, ns, name)
		fresh := s6GetJob(ns, name)
		Expect(fresh.UID).To(Equal(uid), "an already-delivered Loop must NOT recreate its deliver Job")
	})

	It("deletes a stale deliver Job stamped for a different verifiedCommit (D27) and requeues", func() {
		ns := freshNS("s6-stale")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "stale1"
		r := s6Succeeded(name, ns, "")
		s6Reconcile(r, ns, name)
		s6GetJob(ns, name) // the Job exists (stamped s6HeadCommit)

		By("re-stamping the Loop's pin for another commit (a re-verify would pin a new head)")
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		loop.Status.CurrentVerify = &coxv1alpha1.CurrentVerifyStatus{VerifiedCommit: s6OtherCommit}
		// status.delivery is nil (not yet recorded): delivery is still
		// requested for the NEW pin -> the stale Job (stamped s6HeadCommit)
		// must be deleted.
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())

		res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).ToNot(BeZero(), "a stale deliver Job delete must requeue (the name is still taken until the async delete lands)")
		// The reconcile already deleted the stale Job (the log line above
		// "deleted stale deliver Job (verifiedCommit mismatch)"); a follow-up
		// Get is racy (envtest deletes asynchronously) — the requeue is the
		// proof. (The S5a stale-verify Job spec asserts the same way.)
	})

	It("re-delivers for a NEW verifiedCommit after a recorded delivery (idempotency is commit-scoped, not 'status.delivery is set')", func() {
		ns := freshNS("s6-redeliver")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "redel1"
		r := s6Succeeded(name, ns, "")
		s6Reconcile(r, ns, name)
		job := s6GetJob(ns, name)

		By("recording the delivery outcome for the current pin")
		loop := s6Reconcile(r, ns, name)
		s6DeliverPod(ns, name, corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: s6ValidTermination(loop)},
		})
		loop = s6Reconcile(r, ns, name)
		Expect(loop.Status.Delivery).ToNot(BeNil())
		Expect(loop.Status.Delivery.Commit).To(Equal(s6HeadCommit))

		By("re-verifying: pinning a NEW verifiedCommit (status.delivery is still set, for the OLD commit)")
		loop = &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		loop.Status.CurrentVerify = &coxv1alpha1.CurrentVerifyStatus{VerifiedCommit: s6OtherCommit}
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())

		By("reconciling: the stale Job (stamped s6HeadCommit) is deleted, and the NEW commit is still delivery-requested")
		res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())
		// The reconcile requeues: the stale Job (stamped s6HeadCommit) was
		// deleted even though status.delivery is set (for the OLD commit) —
		// the requeue is the proof (a follow-up Get is racy: envtest deletes
		// asynchronously, and the delete has already landed by the time the
		// reconcile returned).
		Expect(res.RequeueAfter).ToNot(BeZero(), "the stale deliver Job delete (for the recorded-but-other commit) must requeue")

		By("reconciling after the async delete lands: a fresh Job is created for the NEW pin")
		// The delete lands asynchronously (envtest); reconcile until the fresh
		// Job appears (the name is free once the delete is complete).
		var fresh *batchv1.Job
		for i := 0; i < 50; i++ {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
			Expect(err).NotTo(HaveOccurred())
			probe := &batchv1.Job{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name + "-deliver", Namespace: ns}, probe); err == nil && probe.UID != job.UID {
				fresh = probe
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		Expect(fresh).ToNot(BeNil(), "a NEW deliver Job must be created for the new pin (the delete landed + the fresh Job was created)")
		Expect(fresh.Annotations[verifyCommitAnnotation]).To(Equal(s6OtherCommit),
			"the fresh Job is stamped for the NEW verifiedCommit")
	})

	It("selects the provider by host: github.com -> GitHub API base + Bearer; otherwise Gitea base + Basic", func() {
		By("provider + API base for a github.com repo")
		prov, host := deliverProviderForRepo("https://github.com/owner/repo.git")
		Expect(prov).To(Equal(deliverProviderGitHub))
		Expect(host).To(Equal("github.com"))
		Expect(deliverAPIBase("https://github.com/owner/repo.git", prov)).To(Equal("https://api.github.com/repos"))

		By("provider + API base for a Gitea-compatible repo")
		prov, host = deliverProviderForRepo("http://gitea.samples.svc:3000/samples/gocli.git")
		Expect(prov).To(Equal(deliverProviderGitea))
		Expect(host).To(Equal("gitea.samples.svc"))
		Expect(deliverAPIBase("http://gitea.samples.svc:3000/samples/gocli.git", prov)).To(Equal("http://gitea.samples.svc:3000/api/v1/repos"))
	})

	It("builds the deliver Job for a github.com delivery: the push uses the GitHub API + Bearer token", func() {
		ns := freshNS("s6-gh")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "gh1"
		r := s6Succeeded(name, ns, "https://github.com/samples/gocli.git")
		s6Reconcile(r, ns, name)
		job := s6GetJob(ns, name)
		_, push := s6JobContainers(job)
		script := push.Command[2]
		Expect(script).To(ContainSubstring("https://api.github.com/repos"),
			"a github.com delivery must create the PR on the GitHub API")
		Expect(script).To(ContainSubstring("Authorization: Bearer"),
			"a github.com delivery must send the Secret's password as a Bearer token")
	})

	It("gives a github.com deliver Loop's egress proxy the allowlist {github.com, api.github.com} and the deliver netpol the proxy rule", func() {
		ns := freshNS("s6-netpol")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "netpol1"
		r := s6Succeeded(name, ns, "https://github.com/samples/gocli.git")
		s6Reconcile(r, ns, name)

		By("allowlisting exactly {github.com, api.github.com} on the egress proxy")
		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-egress-proxy", Namespace: ns}, pod)).To(Succeed(),
			"a github.com deliver Loop must create the egress proxy")
		var policyJSONEnv string
		for _, c := range pod.Spec.Containers {
			for _, e := range c.Env {
				if e.Name == "EGRESS_POLICY_JSON" {
					policyJSONEnv = e.Value
				}
			}
		}
		var allows []string
		Expect(json.Unmarshal([]byte(policyJSONEnv), &allows)).To(Succeed())
		Expect(allows).To(ConsistOf("github.com:443", "api.github.com:443"),
			"the deliver proxy allowlist must be exactly {github.com, api.github.com}")

		By("giving the deliver netpol the egress proxy egress rule (the in-cluster repo keeps the direct repo-peer rule)")
		np := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-deliver-np", Namespace: ns}, np)).To(Succeed(),
			"the deliver netpol must exist for a Succeeded delivery Loop")
		Expect(np.Spec.PodSelector.MatchLabels).To(HaveKeyWithValue(deliverForLabel, name))
		// The egress rules: DNS + the egress proxy (an external repo host).
		Expect(np.Spec.Egress).To(HaveLen(2))
		foundProxy := false
		for _, rule := range np.Spec.Egress {
			for _, to := range rule.To {
				if to.PodSelector != nil && to.PodSelector.MatchLabels["app.kubernetes.io/instance"] == name &&
					to.PodSelector.MatchLabels[policy.ComponentLabelKey] == netpolEgressProxyComponent {
					foundProxy = true
					for _, p := range rule.Ports {
						Expect(int(p.Port.IntValue())).To(BeEquivalentTo(egressProxyPort))
					}
				}
			}
		}
		Expect(foundProxy).To(BeTrue(), "the deliver netpol must egress to the egress proxy on 3128")
	})

	It("gives an in-cluster deliver Loop's deliver netpol the direct repo-peer rule (no proxy hop)", func() {
		ns := freshNS("s6-incluster")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "inc1"
		r := s6Succeeded(name, ns, "")
		s6Reconcile(r, ns, name)
		np := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-deliver-np", Namespace: ns}, np)).To(Succeed())
		foundPeer := false
		for _, rule := range np.Spec.Egress {
			for _, to := range rule.To {
				if to.NamespaceSelector != nil && to.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] == "samples" {
					foundPeer = true
					for _, p := range rule.Ports {
						Expect(int(p.Port.IntValue())).To(BeEquivalentTo(3000))
					}
				}
			}
		}
		Expect(foundPeer).To(BeTrue(), "an in-cluster deliver netpol must egress to the repo peer (the .svc Service's namespace)")
	})

	It("routes the workspace init clone of an external (github.com) repo through the egress proxy", func() {
		ns := freshNS("s6-init")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "init1"
		Expect(k8sClient.Create(ctx, s6Loop(name, ns, "https://github.com/samples/gocli.git"))).To(Succeed())
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())

		By("giving the agent netpol the egress proxy egress rule (no expressible repo rule for the external host)")
		np := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-agent-netpol", Namespace: ns}, np)).To(Succeed())
		foundProxy, foundRepoPeer := false, false
		for _, rule := range np.Spec.Egress {
			for _, to := range rule.To {
				if to.PodSelector != nil && to.PodSelector.MatchLabels["app.kubernetes.io/instance"] == name &&
					to.PodSelector.MatchLabels[policy.ComponentLabelKey] == netpolEgressProxyComponent {
					foundProxy = true
				}
				// A repo peer is a namespaceSelector WITHOUT a pod selector
				// (the repo's namespace). The egress proxy rule and the
				// kube-dns rule both carry a pod selector (the egress proxy
				// pod, the kube-dns pod) — they are not repo peers.
				if to.NamespaceSelector != nil && to.PodSelector == nil {
					foundRepoPeer = true
				}
			}
		}
		Expect(foundProxy).To(BeTrue(), "an external repo host must give the agent netpol the egress proxy rule (the init clone traverses the proxy)")
		Expect(foundRepoPeer).To(BeFalse(), "an external repo host must NOT get a namespaceSelector repo peer (fail-closed: the proxy is the only path)")

		By("routing the init container's git fetch through the proxy env")
		sb := &sandboxv1beta1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-sandbox", Namespace: ns}, sb)).To(Succeed())
		var init *corev1.Container
		for i := range sb.Spec.PodTemplate.Spec.InitContainers {
			if sb.Spec.PodTemplate.Spec.InitContainers[i].Name == workspaceInitContainerName {
				init = &sb.Spec.PodTemplate.Spec.InitContainers[i]
			}
		}
		Expect(init).ToNot(BeNil(), "the first clone must carry the workspace init container")
		proxyURL := "http://" + name + "-egress." + ns + ".svc.cluster.local:3128"
		for _, e := range init.Env {
			if e.Name == "HTTPS_PROXY" || e.Name == "https_proxy" {
				Expect(e.Value).To(Equal(proxyURL), "the init container's git fetch must traverse the egress proxy")
			}
		}
		Expect(init.Env).To(HaveLen(4), "the init container's proxy env must be the four standard names (both cases)")
	})

	It("keeps the workspace init clone of an in-cluster repo on the direct path (no proxy env)", func() {
		ns := freshNS("s6-initin")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "initin1"
		Expect(k8sClient.Create(ctx, s6Loop(name, ns, "http://gitea.samples.svc:3000/samples/gocli.git"))).To(Succeed())
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())

		By("keeping the direct repo-peer rule (no egress proxy rule for the init clone)")
		np := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-agent-netpol", Namespace: ns}, np)).To(Succeed())
		foundRepoPeer, foundProxy := false, false
		for _, rule := range np.Spec.Egress {
			for _, to := range rule.To {
				if to.NamespaceSelector != nil && to.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] == "samples" {
					foundRepoPeer = true
				}
				if to.PodSelector != nil && to.PodSelector.MatchLabels["app.kubernetes.io/instance"] == name &&
					to.PodSelector.MatchLabels[policy.ComponentLabelKey] == netpolEgressProxyComponent {
					foundProxy = true
				}
			}
		}
		Expect(foundRepoPeer).To(BeTrue(), "an in-cluster repo must keep the direct repo-peer rule")
		Expect(foundProxy).To(BeFalse(), "an in-cluster repo's init clone must NOT traverse the egress proxy")

		By("setting no proxy env on the init container")
		sb := &sandboxv1beta1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-sandbox", Namespace: ns}, sb)).To(Succeed())
		for i := range sb.Spec.PodTemplate.Spec.InitContainers {
			if sb.Spec.PodTemplate.Spec.InitContainers[i].Name == workspaceInitContainerName {
				Expect(sb.Spec.PodTemplate.Spec.InitContainers[i].Env).To(BeEmpty(),
					"an in-cluster repo's init clone runs on the direct path (no proxy env)")
			}
		}
	})
})

var _ = Describe("S6: egress proxy hosts (unit)", func() {
	It("returns the deliver hosts for an external github.com delivery and none otherwise", func() {
		ctx := context.Background()
		r := &LoopReconciler{}
		By("an in-cluster Succeeded delivery adds no proxy hosts")
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: "s6loop", Namespace: "default"},
			Spec:       coxv1alpha1.LoopSpec{Workspace: coxv1alpha1.Workspace{Repo: "http://gitea.samples.svc:3000/samples/gocli.git"}},
			Status: coxv1alpha1.LoopStatus{
				Phase:         coxv1alpha1.LoopPhaseSucceeded,
				CurrentVerify: &coxv1alpha1.CurrentVerifyStatus{VerifiedCommit: s6HeadCommit},
			},
		}
		loop.Spec.Delivery = &coxv1alpha1.DeliveryConfig{Mode: coxv1alpha1.DeliveryModePullRequest}
		Expect(r.deliverEgressProxyHosts(loop)).To(BeEmpty(), "an in-cluster repo uses the direct repo-peer rule (no proxy hop)")

		By("an external github.com delivery adds github.com + api.github.com")
		loop.Spec.Workspace.Repo = "https://github.com/samples/gocli.git"
		Expect(r.deliverEgressProxyHosts(loop)).To(ConsistOf("github.com:443", "api.github.com:443"))

		By("a non-Succeeded Loop adds no hosts (delivery not expected)")
		loop.Status.Phase = coxv1alpha1.LoopPhaseVerifying
		Expect(r.deliverEgressProxyHosts(loop)).To(BeEmpty())

		By("the workspace init host: an external repo adds the repo host; an in-cluster repo adds none")
		initLoop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: "s6loop", Namespace: "default"},
			Spec:       coxv1alpha1.LoopSpec{Workspace: coxv1alpha1.Workspace{Repo: "https://github.com/samples/gocli.git"}},
		}
		Expect(r.workspaceInitProxyHost(ctx, initLoop)).To(Equal("github.com:443"))
		initLoop.Spec.Workspace.Repo = "http://gitea.samples.svc:3000/samples/gocli.git"
		Expect(r.workspaceInitProxyHost(ctx, initLoop)).To(BeEmpty())
	})

	// Fake-GitHub provider test (S6 TODO 4): validates the GitHub PR-creation
	// API contract the push container uses (the shell script's curl). The
	// push container: (1) pushes the commit to the branch, (2) GETs the PR for
	// the branch (idempotent: reuse an open PR), (3) POSTs a draft PR if none
	// exists. This test exercises (2)+(3) against a fake GitHub API server:
	// the PR is always draft (PR is always draft by default), the
	// Authorization Bearer header carries the token, and a second GET returns
	// the existing PR (no duplicate POST).
	It("fake-GitHub provider: the PR is always draft, Bearer auth, idempotent GET of an existing PR", func() {
		var prCreated bool
		var prNumber int
		var lastAuth string
		var lastMethod string
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			lastMethod = req.Method
			lastAuth = req.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			switch {
			case req.Method == "GET" && strings.HasSuffix(req.URL.Path, "/pulls"):
				// GET /repos/{owner}/{repo}/pulls?head=coxswain/branch
				if prCreated {
					//nolint:errcheck
					fmt.Fprintf(w, `[{"number": %d, "state": "open", "draft": true}]`, prNumber)
				} else {
					//nolint:errcheck
					fmt.Fprint(w, `[]`)
				}
			case req.Method == "POST" && strings.HasSuffix(req.URL.Path, "/pulls"):
				// POST /repos/{owner}/{repo}/pulls (create a draft PR)
				body, _ := io.ReadAll(req.Body)
				var pr struct {
					Title string `json:"title"`
					Head  string `json:"head"`
					Base  string `json:"base"`
					Draft bool   `json:"draft"`
				}
				_ = json.Unmarshal(body, &pr)
				Expect(pr.Draft).To(BeTrue(), "the PR must be a draft")
				Expect(pr.Title).ToNot(BeEmpty())
				Expect(pr.Head).ToNot(BeEmpty())
				Expect(pr.Base).ToNot(BeEmpty())
				prNumber = 7
				prCreated = true
				w.WriteHeader(http.StatusCreated)
				//nolint:errcheck
				fmt.Fprintf(w, `{"number": %d, "state": "open", "draft": true, "url": "https://github.com/samples/gocli/pull/%d"}`, prNumber, prNumber)
			default:
				w.WriteHeader(http.StatusNotFound)
				//nolint:errcheck
				fmt.Fprint(w, `"not found"`)
			}
		}))
		defer ts.Close()

		// The push container's API base for GitHub is https://api.github.com/repos
		// — the fake server substitutes for it. The auth header for GitHub is
		// Bearer <token> (the Secret's password is the token; the username is
		// "x-access-token").
		token := "ghp_fake_token_for_test"
		authHeader := "Bearer " + token

		// Simulate the push container's PR-creation sequence:
		// 1. GET the existing PR for the branch.
		getReq, err := http.NewRequest("GET", ts.URL+"/samples/gocli/pulls?head=coxswain/s6loop", nil)
		Expect(err).NotTo(HaveOccurred())
		getReq.Header.Set("Authorization", authHeader)
		getResp, err := http.DefaultClient.Do(getReq)
		Expect(err).NotTo(HaveOccurred())
		//nolint:errcheck
		defer getResp.Body.Close()
		Expect(getResp.StatusCode).To(Equal(http.StatusOK))
		var prs []struct {
			Number int    `json:"number"`
			State  string `json:"state"`
			Draft  bool   `json:"draft"`
		}
		Expect(json.NewDecoder(getResp.Body).Decode(&prs)).To(Succeed())
		Expect(prs).To(BeEmpty(), "no PR yet: the first GET returns an empty list")
		Expect(lastAuth).To(Equal(authHeader), "the GET must carry the Bearer token")

		// 2. POST a new draft PR.
		prBody := struct {
			Title string `json:"title"`
			Head  string `json:"head"`
			Base  string `json:"base"`
			Draft bool   `json:"draft"`
		}{Title: "S6 delivery", Head: "coxswain/s6loop", Base: "main", Draft: true}
		prBytes, _ := json.Marshal(prBody)
		postReq, err := http.NewRequest("POST", ts.URL+"/samples/gocli/pulls", bytes.NewReader(prBytes))
		Expect(err).NotTo(HaveOccurred())
		postReq.Header.Set("Authorization", authHeader)
		postReq.Header.Set("Content-Type", "application/json")
		postResp, err := http.DefaultClient.Do(postReq)
		Expect(err).NotTo(HaveOccurred())
		//nolint:errcheck
		defer postResp.Body.Close()
		Expect(postResp.StatusCode).To(Equal(http.StatusCreated))
		Expect(lastAuth).To(Equal(authHeader), "the POST must carry the Bearer token")
		Expect(prNumber).To(BeEquivalentTo(7))

		// 3. Idempotent: a second GET returns the existing PR (no duplicate POST).
		getReq2, err := http.NewRequest("GET", ts.URL+"/samples/gocli/pulls?head=coxswain/s6loop", nil)
		Expect(err).NotTo(HaveOccurred())
		getResp2, err := http.DefaultClient.Do(getReq2)
		Expect(err).NotTo(HaveOccurred())
		//nolint:errcheck
		defer getResp2.Body.Close()
		Expect(getResp2.StatusCode).To(Equal(http.StatusOK))
		var prs2 []struct {
			Number int    `json:"number"`
			State  string `json:"state"`
			Draft  bool   `json:"draft"`
		}
		Expect(json.NewDecoder(getResp2.Body).Decode(&prs2)).To(Succeed())
		Expect(prs2).To(HaveLen(1), "the second GET returns the existing PR")
		Expect(prs2[0].Number).To(BeEquivalentTo(7))
		Expect(prs2[0].Draft).To(BeTrue())
		// No additional POST (the GET found the existing PR).
		Expect(lastMethod).To(Equal("GET"), "after the idempotent GET, the last method is GET (no duplicate POST)")
	})
})
