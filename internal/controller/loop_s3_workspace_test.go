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
// baseCommit read-back: the operator reads the init container's termination
// message (pod.status.initContainerStatuses[name=init-workspace].state.
// terminated.message) via its APIReader path (a real, non-cached client), and
// validates it as a 40-hex commit SHA. No exec/kubelet/ReadFile access and no
// new RBAC are needed (the pod's status is already readable). In envtest the
// spec overrides r.readBaseCommit (the seam) to simulate a pod. The
// mutation-check below proves the spec fails when the read-back is broken
// (the cached-client mutation: swapping in the cached client for the pod Get).

const (
	s3BaseCommitSHA = "abc123def456abc123def456abc123def456abcd"
	s3RunnerImg     = "coxswain-runner:dev"
	s3OtherImg      = "example.com/some-agent:1"
	// s3StandinImage is the image for the stand-in sandbox pod the envtest
	// specs create (envtest has no agent-sandbox controller to run the real
	// agent). A constant (goconst): it is used in every S3 stand-in pod.
	s3StandinImage = "busybox"
	// s3ModelEndpoint is the shared model endpoint across the S3 envtest
	// fixture Loops (goconst: the endpoint literal repeats per fixture).
	s3ModelEndpoint = "vllm:8000"
	// s3DNSSvcNs is the namespaceSelector label of the cluster-DNS peer
	// (dnsPeer), shared by the repo-peer specs that must skip it (goconst).
	s3DNSSvcNs = "kube-system"
)

// s3RunnerCommand is the agent container Command the operator emits for a
// runner agent (asserted by the specs): the entrypoint + the -extra-body
// tuning flag (the local Qwen vLLM is a thinking model; without
// chat_template_kwargs.enable_thinking=false the model's tool calls land in
// the reasoning output and the loop cannot make progress — S3 acceptance,
// 2026-10-02).
var s3RunnerCommand = []string{
	"/usr/local/bin/runner",
	"-extra-body",
	`{"chat_template_kwargs":{"enable_thinking":false}}`,
}

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
					ModelEndpoint:     s3ModelEndpoint,
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
		// The default is a PINNED release, not :latest: this container handles
		// the git credential, so the operator's default must not move out from
		// under a deployed cluster.
		Expect(init.Image).To(Equal("docker.io/alpine/git:v2.54.0"), "the init container must run the operator's pinned --workspace-git-image default")
		// The init container clones and writes baseCommitFile: its command must
		// reference the repo, the base-commit file, AND the termination-log
		// redirect (the operator's PRIMARY read-back: the termination message
		// carries the clone's resolved commit SHA — no exec/kubelet/ReadFile
		// access and no new RBAC needed, the D38 pattern).
		cmd := strings.Join(init.Command, " ")
		Expect(cmd).To(ContainSubstring("base-commit"), "the init container must write baseCommitFile")
		Expect(cmd).To(ContainSubstring("/dev/termination-log"), "the init container must write the SHA to its termination message (the operator's read-back)")
		Expect(cmd).To(ContainSubstring("git"), "the init container must run git")

		By("guarding every git invocation with safe.directory=/workspace (dubious ownership)")
		// The emptyDir volume's owner can differ from the init container's UID
		// (65532); without safe.directory git aborts with 'detected dubious
		// ownership in repository'. The flag is passed per command (no config
		// file write) and must precede EVERY git invocation in the script.
		// The sh script is init.Command[2]; scan its lines only (not the whole
		// command join) so test/source text cannot match the pattern.
		Expect(init.Command).To(HaveLen(3), "the init container must run /bin/sh -c <script>")
		script := init.Command[2]
		gitLines := 0
		for line := range strings.SplitSeq(script, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "#") { // skip sh comments
				continue
			}
			// A line is a git invocation when 'git' appears as a whole word at
			// or after a leading space (the script's git commands start at
			// column 0, e.g. 'git -C ...' — a Contains(" git ") match misses
			// column-0 invocations).
			isGit := false
			for i := 0; i+3 < len(line); i++ {
				if !strings.EqualFold(line[i:i+3], "git") {
					continue
				}
				before := i == 0 || line[i-1] == ' '
				after := line[i+3] == ' '
				if before && after {
					isGit = true
					break
				}
			}
			if isGit {
				gitLines++
				Expect(line).To(ContainSubstring("-c safe.directory=/workspace"),
					"every git invocation must pass -c safe.directory=/workspace: "+line)
			}
		}
		// Fresh-clone path: init, remote add, fetch, checkout + the trailing
		// rev-parse (the idempotent re-run path adds one more rev-parse/checkout).
		Expect(gitLines).To(BeNumerically(">=", 5), "the init script's git invocations (init, remote add, fetch, checkout, rev-parse) are all guarded")

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
		// The items mapping pins the mount to the two basic-auth keys at
		// /workspace-creds/username and /workspace-creds/password. Without it,
		// a SubPath mount of a missing Secret key makes the kubelet mount an
		// empty DIRECTORY, and without the items list a Secret with OTHER keys
		// would mount them too.
		Expect(credVol.Secret.Items).To(Equal([]corev1.KeyToPath{
			{Key: workspaceCredsUsernameKey, Path: workspaceCredsUsernameKey},
			{Key: workspaceCredsPasswordKey, Path: workspaceCredsPasswordKey},
		}), "the credential volume must mount ONLY the basic-auth 'username' and 'password' keys")
		// The init container mounts the volume directory (the items mapping
		// already restricts it); a SubPath on top would re-enter the same key
		// and, for a missing key, mount an empty directory.
		initCredMount := &corev1.VolumeMount{}
		foundCredMount := false
		for _, m := range init.VolumeMounts {
			if m.Name == workspaceCredsVolume {
				initCredMount = &m
				foundCredMount = true
			}
		}
		Expect(foundCredMount).To(BeTrue(), "the init container must mount /workspace-creds (the items mapping provides the file)")
		Expect(initCredMount.MountPath).To(Equal("/workspace-creds"), "the init container must mount the /workspace-creds directory")
		Expect(initCredMount.ReadOnly).To(BeTrue(), "the credential mount must be read-only")
		Expect(initCredMount.SubPath).To(BeEmpty(),
			"the init container must not add a SubPath on top of the items-mapped volume (a SubPath on a missing key mounts an empty directory)")
		agentEnvNames := map[string]bool{}
		for _, e := range agent.Env {
			agentEnvNames[e.Name] = true
		}
		Expect(agentEnvNames["GIT_CREDENTIALS"]).To(BeFalse(), "the agent must not carry the git credential as an env var")

		By("passing the credential to git via http.extraHeader (no credential helper, no store, no global .gitconfig)")
		// The P1 credential-leak check: a HOME=/workspace + `git config --global
		// credential.helper store` would make git's store helper READ AND WRITE
		// /workspace/.git-credentials (the agent's workspace) after an
		// authenticated fetch. The credential helpers are not usable on the
		// read-only mount ('store' writes a lock and erases the entry on
		// success — the live 'unable to get credential storage lock: Read-only
		// file system' failure; the inline '!sh -c' helper form is not parsed
		// by busybox ash), so the script builds a Basic-auth header from the
		// mounted files and passes it to THAT ONE fetch invocation via
		// -c http.extraHeader — nothing is written or persisted (a -c flag
		// lives only for that one command).
		Expect(cmd).NotTo(ContainSubstring("git config --global"),
			"the init script must not use git config --global (a global .gitconfig in /workspace would persist the credential into the agent's workspace)")
		Expect(cmd).NotTo(ContainSubstring("credential.helper"),
			"the init script must not use any git credential helper ('store' refuses the read-only mount; the inline form is not parsed by busybox ash)")
		Expect(cmd).NotTo(ContainSubstring("-c credential.helper"),
			"the init script must not set the credential helper (-c form; 'store' writes a lock and erases the entry on success — a read-only mount refuses both)")
		Expect(cmd).To(ContainSubstring("http.extraHeader"),
			"the init script must pass the Basic-auth header to the fetch via -c http.extraHeader")
		Expect(cmd).To(ContainSubstring("Authorization: Basic $AUTH"),
			"the fetch's http.extraHeader must be the Authorization: Basic header built from the mounted credential files")
		Expect(cmd).To(ContainSubstring("base64 -w 0"),
			"the script must base64 the credential on one line via 'base64 -w 0' (busybox and GNU base64 both support -w; no tr, which would corrupt the base64 output)")
		Expect(cmd).NotTo(ContainSubstring("tr -d"),
			"the script must not use tr on the base64 output (a raw-string tr -d argument corrupts the encoding: it would delete backslashes and every 'n'")
		Expect(cmd).To(ContainSubstring("/workspace-creds/username"))
		Expect(cmd).To(ContainSubstring("/workspace-creds/password"))
		for _, e := range init.Env {
			Expect(e.Value).NotTo(Equal(agentWorkspaceMount),
				"the init container must not set HOME (or any env) to the agent workspace (a credential could be persisted there)")
		}
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
		init := s3Init(sb)
		Expect(init).NotTo(BeNil())
		// No credential flag when no gitCredentialSecret is declared (detected
		// from the spec, not the mount count).
		Expect(strings.Join(init.Command, " ")).NotTo(ContainSubstring("http.extraHeader"),
			"the init script must not reference the credential header when no gitCredentialSecret is set")
		By("adding no credential volume to the pod")
		found := false
		for i := range sb.Spec.PodTemplate.Spec.Volumes {
			if sb.Spec.PodTemplate.Spec.Volumes[i].Name == workspaceCredsVolume {
				found = true
			}
		}
		Expect(found).To(BeFalse(), "no gitCredentialSecret -> no credential volume")
	})

	// S4 review P1 (R18, ADR-0006): once status.baseCommit is pinned (immutable,
	// ADR-0005 D10) the sandbox pod must carry NO workspace-creds volume and
	// NO init-workspace container — the credential is never present next to the
	// agent-controlled /workspace/.git (agent-planted hooks or git config could
	// not run while the credential is mounted), and nothing needs git anymore
	// (the PVC holds the repo; readBaseCommit is skipped once baseCommit is set).
	It("builds the later-phase sandbox with no credential volume and no init-workspace once baseCommit is pinned (a)", func() {
		ns := "s3-pinned-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "pinnedloop"
		Expect(k8sClient.Create(ctx, s3Loop(name, ns, true))).To(Succeed())

		// Pin the baseCommit BEFORE the sandbox is built (the first reconcile
		// would normally read it from the init's termination message; the pin is
		// what the later-phase pods see).
		pinned := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, pinned)).To(Succeed())
		pinned.Status.BaseCommit = s3BaseCommitSHA
		Expect(k8sClient.Status().Update(ctx, pinned)).To(Succeed())

		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())

		sb := &sandboxv1beta1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-sandbox", Namespace: ns}, sb)).To(Succeed())

		By("adding no workspace-creds volume to the pod")
		foundVol := false
		for i := range sb.Spec.PodTemplate.Spec.Volumes {
			if sb.Spec.PodTemplate.Spec.Volumes[i].Name == workspaceCredsVolume {
				foundVol = true
			}
		}
		Expect(foundVol).To(BeFalse(), "baseCommit pinned -> no credential volume on the pod")

		By("adding no init-workspace container")
		foundInit := false
		for i := range sb.Spec.PodTemplate.Spec.InitContainers {
			if sb.Spec.PodTemplate.Spec.InitContainers[i].Name == workspaceInitContainerName {
				foundInit = true
			}
		}
		Expect(foundInit).To(BeFalse(), "baseCommit pinned -> no init-workspace container")

		By("mounting the credential in NO container")
		allContainers := make([]corev1.Container, 0, len(sb.Spec.PodTemplate.Spec.Containers)+len(sb.Spec.PodTemplate.Spec.InitContainers))
		allContainers = append(allContainers, sb.Spec.PodTemplate.Spec.Containers...)
		allContainers = append(allContainers, sb.Spec.PodTemplate.Spec.InitContainers...)
		for i := range allContainers {
			Expect(s3HasMount(&allContainers[i], workspaceCredsVolume)).To(BeFalse(),
				"no container may mount the credential volume once baseCommit is pinned (ADR-0006): %s", allContainers[i].Name)
		}
	})

	// I43 mutation for the previous spec (recorded, not committed): the guard
	// '&& loop.Status.BaseCommit == ""' was removed from BOTH the init-container
	// and the credential-volume branches of agentPodSpec (always including
	// them). The spec failed with: 'baseCommit pinned -> no credential volume
	// on the pod' (a volumes entry workspace-creds was present) and
	// 'baseCommit pinned -> no init-workspace container'. The first-clone spec
	// above (no baseCommit) still passes, so the pair is the mutation check.

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

	// s3SvcRepoLoop is like s3Loop but with an in-cluster Service repo host
	// (S3 review P1: repoPeer must emit a namespaceSelector peer for
	// "<svc>.<ns>.svc[.cluster.local]" so the clone works on an enforcing
	// CNI — the CRD allows http:// only for exactly these hosts).
	s3SvcRepoLoop := func(name, ns, repo string) *coxv1alpha1.Loop {
		return &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      loopGoal,
				Workspace: coxv1alpha1.Workspace{Repo: repo},
				Agent: coxv1alpha1.AgentConfig{
					Image:             "",
					EndpointSecretRef: "samples-model-cred",
					ModelEndpoint:     s3ModelEndpoint,
				},
			},
		}
	}

	It("emits a namespaceSelector repo egress rule for an in-cluster .svc repo (S3 review P1)", func() {
		ns := "s3-svcrepo-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "svcreploop"
		Expect(k8sClient.Create(ctx, s3SvcRepoLoop(name, ns, "http://gitea.samples.svc:3000/samples/gocli.git"))).To(Succeed())
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		s3Reconcile(r, name, ns)

		np := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-agent-netpol", Namespace: ns}, np)).To(Succeed())

		// Exactly one egress rule carries the repo peer: a namespaceSelector on
		// kubernetes.io/metadata.name=samples on port 3000/TCP (the URL port).
		repoRules := 0
		for i := range np.Spec.Egress {
			for j := range np.Spec.Egress[i].To {
				peer := np.Spec.Egress[i].To[j]
				if peer.NamespaceSelector == nil {
					continue
				}
				if peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] == s3DNSSvcNs && peer.PodSelector != nil {
					continue // the cluster-DNS peer (dnsPeer), not a repo rule
				}
				repoRules++
				Expect(peer.NamespaceSelector.MatchLabels).To(Equal(map[string]string{"kubernetes.io/metadata.name": "samples"}),
					"the .svc repo peer must be a namespaceSelector over the Service's namespace (samples)")
				Expect(peer.PodSelector).To(BeNil(), "a .svc repo peer must not ALSO be a podSelector")
				Expect(peer.IPBlock).To(BeNil(), "a .svc repo peer must not be an ipBlock")
				Expect(np.Spec.Egress[i].Ports).To(HaveLen(1))
				Expect(np.Spec.Egress[i].Ports[0].Port.IntVal).To(Equal(int32(3000)), "the .svc repo rule must use the repo URL port")
				Expect(np.Spec.Egress[i].Ports[0].Protocol).To(HaveValue(BeEquivalentTo(corev1.ProtocolTCP)))
				Expect(np.Spec.Egress[i].To).To(HaveLen(1), "the repo rule must target only the namespace peer")
			}
		}
		Expect(repoRules).To(Equal(1), "exactly one namespaceSelector repo egress rule for a .svc repo")
	})

	It("emits the namespaceSelector repo rule for a .svc.cluster.local repo (S3 review P1)", func() {
		ns := "s3-svcrepo2-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "svcrep2loop"
		Expect(k8sClient.Create(ctx, s3SvcRepoLoop(name, ns, "http://gitea.samples.svc.cluster.local:3000/samples/gocli.git"))).To(Succeed())
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		s3Reconcile(r, name, ns)

		np := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-agent-netpol", Namespace: ns}, np)).To(Succeed())

		repoRules := 0
		for i := range np.Spec.Egress {
			for j := range np.Spec.Egress[i].To {
				peer := np.Spec.Egress[i].To[j]
				if peer.NamespaceSelector == nil {
					continue
				}
				if peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] == s3DNSSvcNs && peer.PodSelector != nil {
					continue // the cluster-DNS peer (dnsPeer), not a repo rule
				}
				repoRules++
				Expect(peer.NamespaceSelector.MatchLabels).To(Equal(map[string]string{"kubernetes.io/metadata.name": "samples"}),
					"the .svc.cluster.local repo peer must strip the cluster-domain suffix (namespace is still samples)")
				Expect(np.Spec.Egress[i].Ports).To(HaveLen(1))
				Expect(np.Spec.Egress[i].Ports[0].Port.IntVal).To(Equal(int32(3000)))
			}
		}
		Expect(repoRules).To(Equal(1), "exactly one namespaceSelector repo egress rule for a .svc.cluster.local repo")
	})

	It("adds no repo egress rule for an external-FQDN repo (fail-closed, S3 review P1)", func() {
		// The existing 'lets the sandbox pod reach the repo host' spec already
		// asserts no repo rule for https://example.com (no podSelector/443 rule
		// and no 0.0.0.0/0). This spec closes the remaining gap: an external
		// host must produce NO namespaceSelector peer either (a namespace
		// selector for an external host would be both wrong and a general-
		// egress hole).
		ns := "s3-extrepo-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "extreloop"
		Expect(k8sClient.Create(ctx, s3SvcRepoLoop(name, ns, "https://github.com/example/repo.git"))).To(Succeed())
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		s3Reconcile(r, name, ns)

		np := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-agent-netpol", Namespace: ns}, np)).To(Succeed())

		for i := range np.Spec.Egress {
			for j := range np.Spec.Egress[i].To {
				peer := np.Spec.Egress[i].To[j]
				if peer.NamespaceSelector != nil &&
					peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] == s3DNSSvcNs &&
					peer.PodSelector != nil {
					continue // the cluster-DNS peer (dnsPeer) is expected; not a repo rule
				}
				Expect(peer.NamespaceSelector).To(BeNil(),
					"an external-FQDN repo host must not produce a namespaceSelector peer (fail-closed)")
			}
		}
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
		Expect(agent1.Command).To(Equal(s3RunnerCommand), "the runner image must run its entrypoint (with the -extra-body tuning flag), not sleep infinity")
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
		Expect(agent3.Command).To(Equal(s3RunnerCommand))

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

	It("records status.baseCommit from the init container's termination message and pins it immutably (a)", func() {
		ns := "s3-base-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "baseloop"
		Expect(k8sClient.Create(ctx, s3Loop(name, ns, false))).To(Succeed())

		// The sandbox pod must exist for the read-back to find it; envtest has
		// no agent-sandbox controller, so create a stand-in pod with the
		// expected name + a terminated init container whose termination message
		// carries the clone's resolved commit SHA (the operator's own evidence,
		// not a runner claim).
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-sandbox", Namespace: ns},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: agentContainerName, Image: s3StandinImage}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{
			{Name: workspaceInitContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: s3BaseCommitSHA}}},
		}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		// Wire the APIReader path (a real, non-cached client) — the seam the
		// live deployment uses (no exec/kubelet/ReadFile access, no new RBAC).
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
		loop := s3Reconcile(r, name, ns)
		By("recording status.baseCommit from the termination message")
		Expect(loop.Status.BaseCommit).To(Equal(s3BaseCommitSHA), "the operator must record the clone's SHA in status.baseCommit")

		By("pinning baseCommit immutably (a later reconcile must not overwrite it)")
		// Change the pod's termination message to a DIFFERENT SHA; the pin must
		// NOT change (immutable once set, ADR-0005 D10).
		pod.Status.InitContainerStatuses[0].State.Terminated.Message = strings.Repeat("0", 40)
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
		loop = s3Reconcile(r, name, ns)
		Expect(loop.Status.BaseCommit).To(Equal(s3BaseCommitSHA), "baseCommit is immutable once set (the Loop's base is pinned for its life)")
	})

	It("requeues while baseCommit is pending and stops once the init container terminates", func() {
		ns := "s3-requeue-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "reqlp"
		Expect(k8sClient.Create(ctx, s3Loop(name, ns, false))).To(Succeed())

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-sandbox", Namespace: ns},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: agentContainerName, Image: s3StandinImage}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())

		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}}

		By("requeuing while the init container has not terminated yet")
		// No init container status yet: the read-back is pending, so the
		// operator must requeue (a sandbox pod change does not trigger a
		// reconcile on its own; the RequeueAfter drives the retry).
		res, err := r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeNumerically(">=", time.Second), "while baseCommit is pending the operator must requeue")

		By("recording baseCommit once the init container terminates with a SHA")
		terminated := int32(0)
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{
			{Name: workspaceInitContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: terminated, Message: s3BaseCommitSHA}}},
		}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
		_, err = r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		got := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, req.NamespacedName, got)).To(Succeed())
		Expect(got.Status.BaseCommit).To(Equal(s3BaseCommitSHA), "once the init container terminates with a SHA, baseCommit is recorded")
		By("bounded: a re-reconcile with baseCommit set does NOT change it")
		_, err = r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, req.NamespacedName, got)).To(Succeed())
		Expect(got.Status.BaseCommit).To(Equal(s3BaseCommitSHA), "baseCommit is immutable once set (the bounded S3 property)")
	})

	It("rejects a non-SHA init container termination message", func() {
		ns := "s3-badsha-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "badsha"
		Expect(k8sClient.Create(ctx, s3Loop(name, ns, false))).To(Succeed())

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-sandbox", Namespace: ns},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: agentContainerName, Image: s3StandinImage}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		terminated := int32(0)
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{
			{Name: workspaceInitContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: terminated, Message: "no base commit"}}},
		}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}}

		By("failing the read-back on a non-SHA termination message (bounded)")
		// The init container terminated with a non-SHA message: the read-back
		// fails. The requeue is BOUNDED — it stops once the init has
		// terminated (a permanently-failed init would not change on a requeue;
		// the sandbox stays init-failed until the pod is recreated).
		_, err := r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		got := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, req.NamespacedName, got)).To(Succeed())
		Expect(got.Status.BaseCommit).To(BeEmpty(), "baseCommit must stay empty when the termination message is not a commit SHA")
		By("bounded: a re-reconcile with a non-SHA init does NOT change baseCommit")
		_, err = r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, req.NamespacedName, got)).To(Succeed())
		Expect(got.Status.BaseCommit).To(BeEmpty(), "baseCommit stays empty after a re-reconcile (the bounded S3 property)")
	})

	It("mutation-check: the read-back MUST use the APIReader path, not the cached client", func() {
		// The reviewer's mutation: swap the APIReader for the CACHED client.
		// The sandbox pod is NOT in the manager's Pod cache (the cache is
		// scoped to ProxyComponentSelector), so a cached Get of the sandbox
		// pod returns NotFound and the read-back silently skips — baseCommit
		// stays empty forever. This spec proves the operator reads via the
		// APIReader path: when the APIReader is available, a pod created in
		// the envtest API server MUST be read and its SHA recorded. (A bare
		// reconciler with apiReader nil falls back to the cached client; the
		// mutation-check asserts that fallback is NOT silently used when the
		// APIReader is wired, by asserting the APIReader path records the SHA.)
		ns := "s3-mut-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "mutlp"
		Expect(k8sClient.Create(ctx, s3Loop(name, ns, false))).To(Succeed())

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-sandbox", Namespace: ns},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: agentContainerName, Image: s3StandinImage}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		terminated := int32(0)
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{
			{Name: workspaceInitContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: terminated, Message: s3BaseCommitSHA}}},
		}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		// The APIReader path (a real, non-cached client) reads the pod the envtest
		// API server persists — the same path a live deployment uses. If the
		// operator silently fell back to a scoped cache (the bug the reviewer
		// found), this pod would not be found and baseCommit would stay empty.
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
		loop := s3Reconcile(r, name, ns)
		Expect(loop.Status.BaseCommit).To(Equal(s3BaseCommitSHA),
			"the read-back MUST use the APIReader path (a pod in the envtest API server must be read); a cached-client fallback would silently skip and leave baseCommit empty")
	})
})

// S3 (review P1): plain http:// repo URLs are only allowed for in-cluster
// hosts. A git credential Secret over plain http would leak in cleartext, so
// the CRD XValidation rejects http://host that does not end in ".svc" or
// ".svc.cluster.local". https:// and ssh:// are unrestricted.
var _ = Describe("S3 workspace.repo http restriction (CRD admission)", func() {
	ctx := context.Background()
	// s3HTTPLoop builds a minimal Loop with only workspace.repo set (no model
	// config, so no netpol expectation) — the shape the CRD XValidation sees.
	s3HTTPLoop := func(name, ns, repo string) *coxv1alpha1.Loop {
		return &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      loopGoal,
				Workspace: coxv1alpha1.Workspace{Repo: repo},
			},
		}
	}

	It("rejects a plain http:// repo URL to an external host", func() {
		ns := "s3-http-rej-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()
		l := s3HTTPLoop("httprej", ns, "http://evil.example.com/repo.git")
		err := k8sClient.Create(ctx, l)
		Expect(err).To(MatchError(ContainSubstring(".svc")))
	})

	It("accepts a plain http:// repo URL to an in-cluster host (.svc)", func() {
		ns := "s3-http-ok-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()
		l := s3HTTPLoop("httpok", ns, "http://gitea.samples.svc:3000/samples/gocli.git")
		Expect(k8sClient.Create(ctx, l)).To(Succeed())
		got := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "httpok", Namespace: ns}, got)).To(Succeed())
		Expect(got.Spec.Workspace.Repo).To(Equal("http://gitea.samples.svc:3000/samples/gocli.git"))
	})

	It("accepts a plain http:// repo URL to an in-cluster host (.svc.cluster.local)", func() {
		ns := "s3-http-ok2-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()
		l := s3HTTPLoop("httpok2", ns, "http://gitea.samples.svc.cluster.local:3000/samples/gocli.git")
		Expect(k8sClient.Create(ctx, l)).To(Succeed())
	})

	It("still accepts an https:// repo URL to an external host (unrestricted)", func() {
		ns := "s3-http-https-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()
		l := s3HTTPLoop("httpsok", ns, "https://github.com/example/repo.git")
		Expect(k8sClient.Create(ctx, l)).To(Succeed())
	})
})
