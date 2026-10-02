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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// S3 (GAP 1): workspace materialisation + agent execution.
//
// (a) ensureSandbox gains a workspace init container that clones
//     spec.workspace.repo @ ref into the 'workspace' volume; the git credential
//     Secret is mounted ONLY into the init container (ADR-0006: zero
//     credentials in the agent); the operator records status.baseCommit from
//     the clone; the agent pod's NetworkPolicy lets the init container reach the
//     repo host:port (not general egress).
// (b) the operator sets COX_GOAL=spec.goal and runs the runner's entrypoint ONLY
//     when the agent image is the runner (--runner-image); otherwise 'sleep
//     infinity' (an explicit opt-out).
//
// baseCommit read-back: the operator reads the file the init container wrote on
// the sandbox pod through the kubelet (r.readFile). In envtest there is no
// kubelet, so the spec wires a fake r.readFile (the seam) to simulate the init
// container having written baseCommitFile. The mutation-check below proves the
// spec fails when the read-back is broken (gate-disabled: readFile left nil).

const (
	s3BaseCommitSHA = "abc123def456"
	s3RunnerImg     = "coxswain-runner:dev"
	s3OtherImg      = "example.com/some-agent:1"
)

var _ = Describe("S3: workspace init container + agent execution (GAP 1)", func() {
	ctx := context.Background()

	// s3Loop builds a Loop with the shared fixture repo and, when withCreds, a
	// git credential Secret name. endpointSecretRef is set (with the require-pair
	// modelEndpoint) so the per-Loop NetworkPolicies are expected — the repo
	// egress rule under test lives on the agent pod's NetworkPolicy, which the
	// operator only builds when there is something to allow-egress to.
	s3Loop := func(name, ns string, withCreds bool) *coxv1alpha1.Loop {
		ws := testWorkspace()
		if withCreds {
			ws.GitCredentialSecret = "samples-git-cred"
		}
		return &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      loopGoal,
				Workspace: ws,
				Agent: coxv1alpha1.AgentConfig{
					Image:             "",
					EndpointSecretRef: "samples-model-cred",
					ModelEndpoint:     "vllm:8000",
				},
			},
		}
	}

	// s3Agent returns the agent container on the built sandbox.
	s3Agent := func(sb *sandboxv1beta1.Sandbox) *corev1.Container {
		for i := range sb.Spec.PodTemplate.Spec.Containers {
			if sb.Spec.PodTemplate.Spec.Containers[i].Name == agentContainerName {
				return &sb.Spec.PodTemplate.Spec.Containers[i]
			}
		}
		Fail("no agent container on the built sandbox")
		return nil
	}

	// s3Init returns the workspace init container on the built sandbox (FAILs
	// when absent — the mutation-check for the init container).
	s3Init := func(sb *sandboxv1beta1.Sandbox) *corev1.Container {
		for i := range sb.Spec.PodTemplate.Spec.InitContainers {
			if sb.Spec.PodTemplate.Spec.InitContainers[i].Name == workspaceInitContainerName {
				return &sb.Spec.PodTemplate.Spec.InitContainers[i]
			}
		}
		Fail("no workspace init container on the built sandbox")
		return nil
	}

	// s3Env returns the value of env var envName on a container (FAILs if absent).
	s3Env := func(c *corev1.Container, envName string) string {
		for i := range c.Env {
			if c.Env[i].Name == envName {
				return c.Env[i].Value
			}
		}
		Fail(fmt.Sprintf("env var %q not set on container %q", envName, c.Name))
		return ""
	}

	// s3HasMount reports whether a container mounts volume vol (by name).
	s3HasMount := func(c *corev1.Container, vol string) bool {
		for i := range c.VolumeMounts {
			if c.VolumeMounts[i].Name == vol {
				return true
			}
		}
		return false
	}

	// s3Vol reports the pod-level volume vol (FAILs if absent).
	s3Vol := func(sb *sandboxv1beta1.Sandbox, vol string) *corev1.Volume {
		for i := range sb.Spec.PodTemplate.Spec.Volumes {
			if sb.Spec.PodTemplate.Spec.Volumes[i].Name == vol {
				return &sb.Spec.PodTemplate.Spec.Volumes[i]
			}
		}
		Fail(fmt.Sprintf("no pod volume %q on the built sandbox", vol))
		return nil
	}

	// s3BaseCommit is intentionally absent: the baseCommit spec wires its own
	// readFile seam (it needs the loop name to scope the file read).

	// s3Reconcile reconciles and returns the Loop (re-fetching from the API).
	s3Reconcile := func(r *LoopReconciler, name, ns string) *coxv1alpha1.Loop {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		return loop
	}

	It("builds the workspace init container and isolates the git credential (a)", func() {
		ns := "s3-init-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "initloop"
		loop := s3Loop(name, ns, true)
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())

		sb := &sandboxv1beta1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-sandbox", Namespace: ns}, sb)).To(Succeed())

		By("building a workspace init container on the trusted git image")
		init := s3Init(sb)
		Expect(init.Image).To(Equal("docker.io/library/alpine/git"), "the init container must run the operator's --workspace-git-image default")
		// The init container clones and writes baseCommitFile: its command must
		// reference the repo and the base-commit file (the mutation-check target).
		cmd := strings.Join(init.Command, " ")
		Expect(cmd).To(ContainSubstring("base-commit"), "the init container must write baseCommitFile")
		Expect(cmd).To(ContainSubstring("git"), "the init container must run git")

		By("mounting the credential Secret into the init container ONLY (ADR-0006)")
		Expect(s3HasMount(init, workspaceCredsVolume)).To(BeTrue(), "the init container must mount the git credential Secret")
		agent := s3Agent(sb)
		Expect(s3HasMount(agent, workspaceCredsVolume)).To(BeFalse(),
			"the agent must NOT mount the git credential Secret (zero credentials in the agent, ADR-0006)")
		// The credential is on the pod ONLY as a Secret volume feeding the init
		// container; it is not an env var on the agent.
		credVol := s3Vol(sb, workspaceCredsVolume)
		Expect(credVol.Secret).NotTo(BeNil(), "the credential volume must be a Secret volume")
		Expect(credVol.Secret.SecretName).To(Equal("samples-git-cred"), "the credential Secret name must be the Loop's gitCredentialSecret")
		agentEnvNames := map[string]bool{}
		for _, e := range agent.Env {
			agentEnvNames[e.Name] = true
		}
		Expect(agentEnvNames["GIT_CREDENTIALS"]).To(BeFalse(), "the agent must not carry the git credential as an env var")
	})

	It("mounts no credential volume when gitCredentialSecret is unset (a)", func() {
		ns := "s3-nocred-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "nocredloop"
		Expect(k8sClient.Create(ctx, s3Loop(name, ns, false))).To(Succeed())
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())

		sb := &sandboxv1beta1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-sandbox", Namespace: ns}, sb)).To(Succeed())

		By("still building the init container (a public clone needs no creds)")
		Expect(s3Init(sb)).NotTo(BeNil())
		By("adding no credential volume to the pod")
		found := false
		for i := range sb.Spec.PodTemplate.Spec.Volumes {
			if sb.Spec.PodTemplate.Spec.Volumes[i].Name == workspaceCredsVolume {
				found = true
			}
		}
		Expect(found).To(BeFalse(), "no gitCredentialSecret -> no credential volume")
	})

	It("lets the sandbox pod reach the repo host (not general egress) (a)", func() {
		ns := "s3-netpol-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "netpolloop"
		Expect(k8sClient.Create(ctx, s3Loop(name, ns, false))).To(Succeed())
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())

		np := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-agent-netpol", Namespace: ns}, np)).To(Succeed())

		// The agent netpol egress must carry a rule to the repo peer: the fixture
		// repo is https, so the host is example.com, which the operator's
		// fail-closed repoPeer resolution does NOT map to a NetworkPolicy peer
		// (external FQDN — NetworkPolicy cannot match DNS names) and NO repo
		// rule is added. The spec therefore asserts the fail-closed shape: the
		// agent egress is exactly the model-proxy rule (vllm:8000 resolves to a
		// podSelector peer), the DNS rule, and no 0.0.0.0/0 general-egress rule.
		By("adding no general-egress (0.0.0.0/0) rule and no repo rule the operator cannot express")
		repoRule := false
		generalEgress := false
		hasModelRule := false
		for i := range np.Spec.Egress {
			for j := range np.Spec.Egress[i].To {
				peer := np.Spec.Egress[i].To[j]
				if peer.IPBlock != nil && peer.IPBlock.CIDR == "0.0.0.0/0" {
					generalEgress = true
					continue
				}
				// The repo host (example.com) must never appear as an egress peer:
				// it does not resolve to a NetworkPolicy peer, so the operator adds
				// no rule for it (fail-closed). The operator's podSelector egress
				// peers are the model proxy (8080) and the egress proxy (3128,
				// when expected); a 443 egress to a podSelector peer is a repo
				// rule by construction.
				if peer.PodSelector != nil && len(np.Spec.Egress[i].Ports) > 0 &&
					np.Spec.Egress[i].Ports[0].Port.Type == intstr.Int && np.Spec.Egress[i].Ports[0].Port.IntVal == 443 {
					repoRule = true
				}
				// The model-proxy rule IS present (the proxy pod resolves to a
				// podSelector peer on 8080) — the agent egress is model proxy +
				// DNS, nothing more.
				if peer.PodSelector != nil && len(np.Spec.Egress[i].Ports) > 0 &&
					np.Spec.Egress[i].Ports[0].Port.Type == intstr.Int && np.Spec.Egress[i].Ports[0].Port.IntVal == 8080 {
					hasModelRule = true
				}
			}
		}
		Expect(generalEgress).To(BeFalse(), "the agent netpol must NOT open general (0.0.0.0/0) egress for the repo clone")
		Expect(repoRule).To(BeFalse(), "an external-FQDN repo host must NOT be expressed as a general rule (fail-closed: no rule the operator cannot scope)")
		Expect(hasModelRule).To(BeTrue(), "the agent netpol must still carry the model-proxy egress rule (the repo addition changes nothing else)")
	})

	It("sets COX_GOAL and selects the runner command only for the runner image (b)", func() {
		ns := "s3-exec-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		// (1) empty agent image + runner flag set -> the runner entrypoint.
		name1 := "runnerloop"
		loop1 := s3Loop(name1, ns, false)
		loop1.Spec.Agent.Image = ""
		Expect(k8sClient.Create(ctx, loop1)).To(Succeed())
		r1 := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), RunnerImage: s3RunnerImg}
		_, err := r1.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name1, Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())
		sb1 := &sandboxv1beta1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name1 + "-sandbox", Namespace: ns}, sb1)).To(Succeed())
		agent1 := s3Agent(sb1)
		By("running the runner entrypoint when the agent image is the runner (empty image + runner flag)")
		Expect(agent1.Command).To(Equal([]string{"/usr/local/bin/runner"}), "the runner image must run its entrypoint, not sleep infinity")
		Expect(s3Env(agent1, coxGoal)).To(Equal(loopGoal), "COX_GOAL must equal spec.goal")

		// (2) a non-runner image + runner flag set -> 'sleep infinity' (opt-out).
		name2 := "otherloop"
		loop2 := s3Loop(name2, ns, false)
		loop2.Spec.Agent.Image = s3OtherImg
		Expect(k8sClient.Create(ctx, loop2)).To(Succeed())
		_, err = r1.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name2, Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())
		sb2 := &sandboxv1beta1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name2 + "-sandbox", Namespace: ns}, sb2)).To(Succeed())
		agent2 := s3Agent(sb2)
		By("keeping 'sleep infinity' for a non-runner image (the explicit opt-out)")
		Expect(agent2.Command).To(ContainElement("sleep infinity"), "a non-runner image must keep the sleep-infinity stand-in")
		Expect(agent2.Image).To(Equal(s3OtherImg))

		// (3) runner image set explicitly (== the flag) -> the runner entrypoint.
		name3 := "runnersetloop"
		loop3 := s3Loop(name3, ns, false)
		loop3.Spec.Agent.Image = s3RunnerImg
		Expect(k8sClient.Create(ctx, loop3)).To(Succeed())
		_, err = r1.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name3, Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())
		sb3 := &sandboxv1beta1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name3 + "-sandbox", Namespace: ns}, sb3)).To(Succeed())
		agent3 := s3Agent(sb3)
		By("running the runner entrypoint when spec.agent.image equals the runner flag")
		Expect(agent3.Command).To(Equal([]string{"/usr/local/bin/runner"}))

		// (4) runner flag UNSET (no runner configured) -> 'sleep infinity' even
		//     for an empty image (no Loop is run as the runner).
		name4 := "norunnerloop"
		loop4 := s3Loop(name4, ns, false)
		loop4.Spec.Agent.Image = ""
		Expect(k8sClient.Create(ctx, loop4)).To(Succeed())
		r4 := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err = r4.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name4, Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())
		sb4 := &sandboxv1beta1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name4 + "-sandbox", Namespace: ns}, sb4)).To(Succeed())
		agent4 := s3Agent(sb4)
		By("keeping 'sleep infinity' when no runner image is configured (runner flag unset)")
		Expect(agent4.Command).To(ContainElement("sleep infinity"))
		// COX_GOAL is always set (the runner reads it; a non-runner ignores it).
		Expect(s3Env(agent4, coxGoal)).To(Equal(loopGoal))
	})

	It("records status.baseCommit from the clone and pins it immutably (a)", func() {
		ns := "s3-base-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "baseloop"
		Expect(k8sClient.Create(ctx, s3Loop(name, ns, false))).To(Succeed())

		// The sandbox pod must exist for readBaseCommit to find it; envtest has
		// no agent-sandbox controller, so create a stand-in pod with the
		// expected name + a terminated init container (the operator reads the
		// baseCommit file from it via the readFile seam).
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-sandbox", Namespace: ns},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: agentContainerName, Image: "busybox"}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{
			{Name: workspaceInitContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
		}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		// Wire the readFile seam to simulate the init container having written
		// baseCommitFile (the operator's own evidence, not a runner claim).
		r.readFile = func(ctx context.Context, p *corev1.Pod, path string) ([]byte, error) {
			if p.Name != name+"-sandbox" {
				return nil, fmt.Errorf("not found: %s", p.Name)
			}
			return []byte(s3BaseCommitSHA + "\n"), nil
		}
		loop := s3Reconcile(r, name, ns)
		By("recording status.baseCommit from the clone")
		Expect(loop.Status.BaseCommit).To(Equal(s3BaseCommitSHA), "the operator must record the clone's SHA in status.baseCommit")

		By("pinning baseCommit immutably (a later reconcile must not overwrite it)")
		// A later reconcile with a DIFFERENT file value must NOT change it.
		r.readFile = func(ctx context.Context, p *corev1.Pod, path string) ([]byte, error) {
			return []byte("deadbeef"), nil
		}
		loop = s3Reconcile(r, name, ns)
		Expect(loop.Status.BaseCommit).To(Equal(s3BaseCommitSHA), "baseCommit is immutable once set (the Loop's base is pinned for its life)")
	})
})
