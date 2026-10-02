# Image URL to use all building/pushing image targets
IMG ?= controller:latest
# YEAR defines the year value used for substituting the YEAR placeholder in the boilerplate header.
YEAR ?= $(shell date +%Y)

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	"$(CONTROLLER_GEN)" rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt",year=$(YEAR) paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out
	$(MAKE) runner-test

# I3: the runner is a separate Go module (runner/go.mod); the root `go list ./...`
# skips it. Test it explicitly so `make test` covers both modules.
.PHONY: runner-test
runner-test: ## Run the runner module's tests (separate go.mod).
	cd runner && go vet ./... && go test ./... -coverprofile cover-runner.out

# S1: the sample apps are separate Go/Python modules (examples/<app>); the root
# `go test ./...` and golangci-lint skip them. Verify each app's scoped checks
# fail on the seed and pass after the reference patch. No cluster needed.
.PHONY: samples-check
samples-check: ## S1: seed checks fail, reference patches pass
	bash hack/samples-check.sh

# S2 (samples plan §2/§7): the in-cluster git server for the sample apps, on
# coxswain-dev ONLY. The driver script (hack/samples-git.sh) pins the kubectl
# context to kind-coxswain-dev — these targets never touch any other cluster.
# Never deletes clusters, never touches KubeArmor or the operator.
.PHONY: samples-up
samples-up: ## S2: apply config/samples-git (Gitea) to coxswain-dev and wait for Ready
	@bash hack/samples-git.sh up

.PHONY: samples-seed
samples-seed: ## S2: create the 3 sample repos on the in-cluster Gitea (one 'initial' commit each, from examples/<app>)
	@bash hack/samples-git.sh seed

.PHONY: samples-accept
samples-accept: ## S2: acceptance — a throwaway pod clones each seeded repo with the samples credential and asserts the 'initial' commit + no github.com remote
	@bash hack/samples-git.sh accept

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	"$(GOLANGCI_LINT)" run
	cd runner && "$(GOLANGCI_LINT)" run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	"$(GOLANGCI_LINT)" run --fix
	cd runner && "$(GOLANGCI_LINT)" run --fix

# TODO(user): To use a different vendor for e2e tests, modify the setup under 'tests/e2e'.
# The default setup assumes Kind is pre-installed and builds/loads the Manager Docker image locally.
# kubectl kuberc is disabled by default for test isolation; enable with:
# - KUBECTL_KUBERC=true
# CertManager is installed by default; skip with:
# - CERT_MANAGER_INSTALL_SKIP=true
KIND_CLUSTER ?= coxswain-test-e2e
# D5: pin the dev/CI k8s version to 1.34 across kind + envtest. The production
# floor stays >=1.37 for agent-sandbox; dev runs one lower on purpose so the
# envtest suite is the canary for API drift, not the prod cluster.
KIND_NODE_IMAGE ?= kindest/node:v1.34.0

# I23: agent-sandbox is installed from its *release* manifest (the source
# k8s/controller.yaml is a ko:// placeholder, which is what made Phase 0 think
# no controller image existed). The version is pinned here, in exactly one
# place, and the release manifest is derived from it.
AGENT_SANDBOX_VERSION ?= v1.0.4
KUBEARMOR_VERSION ?= v1.7.5
# D38: the enforcing-CNI profile. Calico is pinned; the cluster and pool
# CIDRs must keep matching config/dev (POD_CIDR 10.244.0.0/16, service CIDR
# 10.96.0.0/12 — kind's default, left as-is). The Calico manifest URL is
# derived from the version, exactly like AGENT_SANDBOX_MANIFEST.
CALICO_VERSION ?= v3.30.1
CALICO_MANIFEST ?= https://raw.githubusercontent.com/projectcalico/calico/$(CALICO_VERSION)/manifests/calico.yaml
CALICO_CLUSTER ?= coxswain-calico
CALICO_IP_POOL ?= 10.244.0.0/16
# KubeArmor install posture flags: block for file/network/capabilities (the exec
# allowlist's block-vs-audit is gated on defaultFilePosture, NOT spec.action) +
# process visibility (needed for the process rules to be visible/evaluated).
# These are passed to `karmor install` so the posture is in the KubeArmorConfig
# BEFORE the node agent starts — see the finding below why it must NOT be a
# post-install edit + agent restart.
KUBEARMOR_POSTURE_FLAGS ?= -b all --viz process,file,network
AGENT_SANDBOX_MANIFEST ?= https://github.com/kubernetes-sigs/agent-sandbox/releases/download/$(AGENT_SANDBOX_VERSION)/sandbox.yaml
# The controller image the release manifest references (pre-loaded into the
# kind node so an offline host doesn't depend on the node reaching
# registry.k8s.io).
AGENT_SANDBOX_CONTROLLER_IMAGE ?= registry.k8s.io/agent-sandbox/agent-sandbox-controller:$(AGENT_SANDBOX_VERSION)

PROXY_IMG ?= coxswain-proxy:standin
EGRESS_IMG ?= coxswain-egress-proxy:standin
# S3b (GAP 1): the runner agent image (golang:1.26 + compiled runner + git +
# python3). The operator selects it via --runner-image; when spec.agent.image
# is empty or equals it, the agent container runs the runner's entrypoint.
RUNNER_IMG ?= coxswain-runner:dev

.PHONY: proxy-build
proxy-build: ## Build the proxy stand-in image (coxswain-proxy:standin).
	$(CONTAINER_TOOL) build -t $(PROXY_IMG) -f cmd/proxy-standin/Dockerfile .

.PHONY: egress-proxy-build
egress-proxy-build: ## Build the egress proxy image (coxswain-egress-proxy:standin).
	$(CONTAINER_TOOL) build -t $(EGRESS_IMG) -f cmd/egress-proxy/Dockerfile .

.PHONY: runner-build
runner-build: ## S3: build the runner agent image ($(RUNNER_IMG)) from cmd/runner/Dockerfile.
	$(CONTAINER_TOOL) build -t $(RUNNER_IMG) -f cmd/runner/Dockerfile .

.PHONY: runner-load
runner-load: ## S3: load $(RUNNER_IMG) into the kind node $(KIND_CLUSTER) (dev/kind only).
	$(KIND) load docker-image "$(RUNNER_IMG)" --name $(KIND_CLUSTER)

.PHONY: egress-proxy-e2e
egress-proxy-e2e: ## Run the I42a egress proxy kind e2e (real proxy, TLS + plain HTTP).
	@bash test/e2e/egress-proxy.sh

.PHONY: i42-e2e
i42-e2e: ## Run the full I42 acceptance kind e2e (pinned to --context kind-coxswain-dev).
	@K8S_CONTEXT=kind-coxswain-dev KIND_CLUSTER_NAME=coxswain-dev bash test/e2e/i42-e2e.sh

.PHONY: verify-cni
verify-cni: ## D38 preflight: check the CURRENT cluster's CNI polices pod -> host-network egress (K8S_CONTEXT=<ctx> to target another context; default = current kubectl context). Works on a cluster WITHOUT coxswain installed; creates only a temp namespace + NetworkPolicy + probe pod and always deletes them. Exits non-zero on FAIL.
	@bash test/e2e/verify-cni.sh

.PHONY: d38-cni-e2e
d38-cni-e2e: kind-calico-up ## D38: run the enforcing-CNI network e2e (pinned to --context kind-coxswain-calico).
	@K8S_CONTEXT=kind-coxswain-calico KIND_CLUSTER_NAME=$(CALICO_CLUSTER) CALICO_IP_POOL=$(CALICO_IP_POOL) bash test/e2e/d38-cni-e2e.sh

.PHONY: kind-calico-up
kind-calico-up: ## D38: create the coxswain-calico kind cluster (Calico $(CALICO_VERSION) as the enforcing CNI, kindnet disabled), install agent-sandbox + the operator (dev overlay). Idempotent. NEVER installs KubeArmor here (ADR-0007 F2).
	@command -v $(KIND) >/dev/null 2>&1 || { \
		echo "Kind is not installed. Please install Kind manually."; \
		exit 1; \
	}
	@INOT_INST=$$(cat /proc/sys/fs/inotify/max_user_instances 2>/dev/null || echo 0); INOT_WATCH=$$(cat /proc/sys/fs/inotify/max_user_watches 2>/dev/null || echo 0); echo "   preflight: fs.inotify.max_user_instances=$$INOT_INST (need >=512)  fs.inotify.max_user_watches=$$INOT_WATCH (need >=524288)"; [ "$$INOT_INST" -ge 512 ] && [ "$$INOT_WATCH" -ge 524288 ] || { echo "FATAL: host fs.inotify limits too low for a second kind cluster (instances=$$INOT_INST, watches=$$INOT_WATCH). Run: sudo sysctl -w fs.inotify.max_user_instances=1024 fs.inotify.max_user_watches=524288 (persist in /etc/sysctl.d/99-kind-inotify.conf)"; exit 1; }
	@echo "=== D38 enforcing-CNI profile: cluster $(CALICO_CLUSTER), Calico $(CALICO_VERSION), pool $(CALICO_IP_POOL) ==="
	@echo "NOTE: KubeArmor is deliberately NOT installed on $(CALICO_CLUSTER) (ADR-0007 F2: a second BPF-LSM agent on this host's kernel risks wedging the BPF subsystem; coxswain-dev already carries the BPF-LSM load). coxswain-dev is never touched."
	@if $(KIND) get clusters | grep -qxF "$(CALICO_CLUSTER)"; then \
		echo "Kind cluster '$(CALICO_CLUSTER)' already exists. Skipping creation."; \
		else \
			echo "Creating Kind cluster '$(CALICO_CLUSTER)' on $(KIND_NODE_IMAGE) with hack/kind-calico.yaml (kindnet disabled)..."; \
			$(KIND) create cluster --name $(CALICO_CLUSTER) --image $(KIND_NODE_IMAGE) --config hack/kind-calico.yaml || \
			{ echo "FATAL: cluster creation failed (partial state left; delete with: kind delete cluster --name $(CALICO_CLUSTER))"; exit 1; }; \
		fi
	@CTX=kind-$(CALICO_CLUSTER); \
		if kubectl --context $$CTX get nodes >/dev/null 2>&1; then \
			KINDNET=$$(kubectl --context $$CTX -n kube-system get pod -l k8s-app=kindnet --no-headers 2>/dev/null | wc -l | tr -d ' '); \
			if [ "$$KINDNET" != "0" ]; then echo "FATAL: cluster $(CALICO_CLUSTER) is running kindnet — it is NOT the enforcing-CNI profile (disableDefaultCNI). Delete it (kind delete cluster --name $(CALICO_CLUSTER)) and rerun with the hack/kind-calico.yaml profile."; exit 1; fi; \
			echo "   profile verified (no kindnet pods; kindnet is disabled via networking.disableDefaultCNI)."; \
		fi
	@echo "Installing Calico $(CALICO_VERSION) (enforcing CNI; kindnet is disabled so Calico is the only CNI)"
	@curl -fsSL "$(CALICO_MANIFEST)" -o /tmp/calico-$(CALICO_VERSION).yaml
	@kubectl --context kind-$(CALICO_CLUSTER) apply -f /tmp/calico-$(CALICO_VERSION).yaml
	@echo "Waiting for the Calico nodes (calico-node) to be Ready..."
	@for i in $$(seq 1 30); do \
		n=$$(kubectl --context kind-$(CALICO_CLUSTER) -n kube-system get pod -l k8s-app=calico-node --no-headers 2>/dev/null | grep -c ' 1/1 ' || true); \
		[ "$$n" -ge 1 ] && break; \
		sleep 5; \
	done
	@kubectl --context kind-$(CALICO_CLUSTER) -n kube-system get pod -l k8s-app=calico-node || { echo "FATAL: calico-node not Ready after 150s"; exit 1; }
	@echo "Verifying the Calico IP pool CIDR matches config/dev POD_CIDR ($(CALICO_IP_POOL))..."
	@POOL_CIDR=$$(kubectl --context kind-$(CALICO_CLUSTER) get ippool -o json 2>/dev/null | python3 -c "import sys,yaml,json; d=json.load(sys.stdin); print(d['items'][0]['spec']['cidr'])" 2>/dev/null); \
		echo "   default-ipv4-ippool CIDR=$$POOL_CIDR (expected prefix $(CALICO_IP_POOL))"; \
		case "$$POOL_CIDR" in $(CALICO_IP_POOL)*) : ;; *) echo "WARNING: pool CIDR $$POOL_CIDR does not match $(CALICO_IP_POOL); the dev overlay's POD_CIDR carve-outs may not match. (Calico auto-creates default-ipv4-ippool at 10.244.0.0/16 by default, which is what config/dev expects.)";; esac
	@echo "Installing agent-sandbox $(AGENT_SANDBOX_VERSION) from the release manifest..."
	@curl -fsSL "$(AGENT_SANDBOX_MANIFEST)" | kubectl --context kind-$(CALICO_CLUSTER) apply -f -
	@echo "Waiting for the agent-sandbox controller to be ready..."
	@kubectl --context kind-$(CALICO_CLUSTER) rollout status deploy/agent-sandbox-controller -n agent-sandbox-system --timeout=180s
	@echo "Deploying the operator (config/calico profile: --allow-unenforced ONLY; the D38 gate is LIVE, no --allow-unenforced-network) plus the config/cni-probe ns+RBAC on $(CALICO_CLUSTER)..."
	@echo "   Building the controller image coxswain-controller:d38 and the egress/proxy stand-ins..."
	@$(CONTAINER_TOOL) build -t coxswain-controller:d38 -f Dockerfile . || { echo "FATAL: controller docker-build failed"; exit 1; }
	@$(MAKE) egress-proxy-build
	@$(MAKE) proxy-build
	@echo "   Loading images into the kind node $(CALICO_CLUSTER)..."
	# golang:1.26 = the e2e agent image; busybox:1.36 = the i42-e2e throwaway
	# pod; python:3-alpine = the operator's CNI self-test probe pod
	# (--cni-probe-image default; the operator pulls it by name, so it must be
	# pre-loaded for offline hosts).
	@for img in coxswain-controller:d38 $(EGRESS_IMG) $(PROXY_IMG) golang:1.26 busybox:1.36 python:3-alpine; do \
		echo "     kind load: $$img"; \
		$(KIND) load docker-image "$$img" --name $(CALICO_CLUSTER) || { echo "FATAL: kind load $$img failed"; exit 1; }; \
	done
	@CTX=kind-$(CALICO_CLUSTER); \
		$(MAKE) kustomize >/dev/null 2>&1; \
		TMP_OVERLAY=$$(mktemp -d); \
		cp -r config "$$TMP_OVERLAY/config"; \
		(cd "$$TMP_OVERLAY/config/manager" && "$(LOCALBIN)/kustomize" edit set image controller=coxswain-controller:d38); \
		# D38: the enforcing-CNI profile deploys the config/calico overlay \
		# (config/default + --allow-unenforced ONLY; the D38 network gate is \
		# LIVE — no --allow-unenforced-network). --allow-unenforced bypasses \
		# the D30 KubeArmor gate, which on this cluster (Calico, no KubeArmor, \
		# ADR-0007 F2) would otherwise hold every Loop Suspended regardless of \
		# D38 (Loops run with PolicyEnforced=False/EnforcementDisabled). The \
		# config/cni-probe ns+RBAC is standalone (see \
		# config/cni-probe/kustomization.yaml) and must be applied separately \
		# so the operator's CNI self-test probe can run. \
		(cd "$$TMP_OVERLAY" && "$(LOCALBIN)/kustomize" build config/calico | kubectl --context $$CTX apply -f -) || { echo "FATAL: controller deploy failed"; exit 1; }; \
		(cd "$$TMP_OVERLAY" && "$(LOCALBIN)/kustomize" build config/cni-probe | kubectl --context $$CTX apply -f -) || { echo "FATAL: cni-probe ns/RBAC deploy failed"; exit 1; }
	@rm -rf "$${TMP_OVERLAY:-}"
	@echo "kind-calico-up complete (cluster ready, images loaded; make d38-cni-e2e runs the assertions)."


.PHONY: kind-up
kind-up: ## Create the kind cluster (if needed) and install agent-sandbox $(AGENT_SANDBOX_VERSION)
	@command -v $(KIND) >/dev/null 2>&1 || { \
		echo "Kind is not installed. Please install Kind manually."; \
		exit 1; \
	}
	@case "$$($(KIND) get clusters)" in \
		*"$(KIND_CLUSTER)*") \
			echo "Kind cluster '$(KIND_CLUSTER)' already exists. Skipping creation." ;; \
		*) \
			echo "Creating Kind cluster '$(KIND_CLUSTER)' on $(KIND_NODE_IMAGE)..."; \
			$(KIND) create cluster --name $(KIND_CLUSTER) --image $(KIND_NODE_IMAGE) ;; \
	esac
	@echo "Installing agent-sandbox $(AGENT_SANDBOX_VERSION) from the release manifest..."
	curl -fsSL "$(AGENT_SANDBOX_MANIFEST)" | kubectl apply -f -
	@echo "Pre-loading the controller image into the kind node (offline-host fallback)"
	@docker pull "$(AGENT_SANDBOX_CONTROLLER_IMAGE)" >/dev/null 2>&1 \
		&& $(KIND) load docker-image "$(AGENT_SANDBOX_CONTROLLER_IMAGE)" --name $(KIND_CLUSTER) \
		|| echo "(could not pre-load $(AGENT_SANDBOX_CONTROLLER_IMAGE); the node will pull it)"
	@echo "Waiting for the agent-sandbox controller to be ready..."
	kubectl rollout status deploy/agent-sandbox-controller -n agent-sandbox-system --timeout=120s
	@echo "Installing KubeArmor $(KUBEARMOR_VERSION) via the pinned karmor CLI..."
	@mkdir -p bin
	@command -v bin/karmor >/dev/null 2>&1 || \
		curl -sfL "https://github.com/kubearmor/kubearmor-client/releases/download/v1.4.9/karmor_1.4.9_linux_amd64.tar.gz" \
		| tar xz -C bin
	@bin/karmor install $(KUBEARMOR_POSTURE_FLAGS) --tag $(KUBEARMOR_VERSION) || \
		{ echo "KubeArmor install failed (needs --tag $(KUBEARMOR_VERSION), the v-prefix is mandatory for Docker Hub tags)"; exit 1; }
	@echo "KubeArmor $(KUBEARMOR_VERSION) installed (BPF-LSM enforcer)."
	@echo "Verifying the KubeArmor default posture is block (BPF-LSM exec/whitelist enforcement)."
	@echo "The posture is set via karmor install flags ($(KUBEARMOR_POSTURE_FLAGS)), NOT by editing the config + restarting the agent: KubeArmor v1.7.5 gates the exec allowlist's block-vs-audit on defaultFilePosture (NOT spec.action), so block must be in place before the agent first starts. A post-install config edit + agent rollout-restart is also host-disruptive: on kernel 6.1 a BPF-LSM agent stop can hang in bpf_trampoline teardown and wedge the node's BPF subsystem (see ADR-0007, findings)." \
		&& KA_NS=$$(kubectl get configmap -A --no-headers 2>/dev/null | awk '$$2=="kubearmor-config"{print $$1; exit}') \
		&& [ -n "$$KA_NS" ] \
		&& FP=$$(kubectl -n "$$KA_NS" get configmap kubearmor-config -o jsonpath='{.data.defaultFilePosture}' 2>/dev/null) \
		&& VP=$$(kubectl -n "$$KA_NS" get configmap kubearmor-config -o jsonpath='{.data.visibility}' 2>/dev/null) \
		&& echo "   defaultFilePosture=$$FP visibility=$$VP" \
		&& [ "$$FP" = "block" ] \
		&& case "$$VP" in *process*) true;; *) echo "process visibility missing"; false;; esac \
		|| { echo "Posture is not block (defaultFilePosture=$$FP): a disallowed exec would be logged but allowed. Check KUBEARMOR_POSTURE_FLAGS."; exit 1; }
	@echo "Building the proxy stand-in image and loading it into the kind node..."
	$(MAKE) proxy-build
	$(KIND) load docker-image "$(PROXY_IMG)" --name $(KIND_CLUSTER)

.PHONY: kind-smoke
kind-smoke: ## Rerun D22's evidence: create a bare Sandbox and wait for Ready=True
	@kubectl create namespace d22-smoke --dry-run=client -o yaml | kubectl apply -f - >/dev/null
	@echo "Creating a bare Sandbox in ns d22-smoke and waiting for Ready=True..."
	@printf '%s\n' 'apiVersion: agents.x-k8s.io/v1beta1' 'kind: Sandbox' 'metadata:' '  name: d22-smoke' '  namespace: d22-smoke' 'spec:' '  podTemplate:' '    spec:' '      containers:' '        - name: agent' '          image: docker.io/library/busybox:latest' '          command: ["sh", "-c", "sleep 3600"]' | kubectl apply -f -
	@for i in $$(seq 1 48); do \
		r=$$(kubectl get sandbox d22-smoke -n d22-smoke -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null); \
		if [ "$$r" = "True" ]; then echo "Sandbox d22-smoke is Ready (D22 reproduced)."; break; fi; \
		if [ $$i -eq 48 ]; then echo "Sandbox d22-smoke did not reach Ready in 240s"; exit 1; fi; \
		sleep 5; \
	done

.PHONY: crd-drift-check
crd-drift-check: ## Fail if the vendored agent-sandbox CRD drifts from the release manifest
	@echo "Checking vendored CRD against the agent-sandbox $(AGENT_SANDBOX_VERSION) release manifest..."
	@curl -fsSL "$(AGENT_SANDBOX_MANIFEST)" > /tmp/cox-agent-sandbox.yaml
	@python3 -c "import yaml,sys; rel=[d for d in yaml.safe_load_all(open('/tmp/cox-agent-sandbox.yaml')) if d and d.get('kind')=='CustomResourceDefinition' and 'sandboxes.agents.x-k8s.io' in d['metadata']['name']][0]; vend=yaml.safe_load(open('config/crd/external/agents.x-k8s.io_sandboxes.yaml')); ks=lambda d: sorted(d['spec']['versions'][0]['schema']['openAPIV3Schema']['properties']['spec']['properties'].keys()); r,v=ks(rel),ks(vend); sys.exit(1) if r!=v else print('Vendored agent-sandbox CRD matches the release manifest.')"
	@rm -f /tmp/cox-agent-sandbox.yaml

.PHONY: setup-test-e2e
setup-test-e2e: kind-up crd-drift-check ## Set up a Kind cluster for e2e tests (with agent-sandbox installed)

# C6b exec-block e2e: install KubeArmor (pinned) and prove a disallowed exec in
# the agent container is blocked by the operator's KubeArmorPolicy. Requires the
# kind cluster + operator (make deploy with --allow-unenforced) already running.
kubearmor-e2e:
	@KUBEARMOR_VERSION=$(KUBEARMOR_VERSION) bash test/e2e/kubearmor-exec-block.sh

.PHONY: test-e2e
test-e2e: setup-test-e2e manifests generate fmt vet ## Run the e2e tests. Expected an isolated environment using Kind.
	KIND=$(KIND) KIND_CLUSTER=$(KIND_CLUSTER) go test -tags=e2e ./test/e2e/ -v -ginkgo.v
	$(MAKE) cleanup-test-e2e

.PHONY: cleanup-test-e2e
cleanup-test-e2e: ## Tear down the Kind cluster used for e2e tests
	@$(KIND) delete cluster --name $(KIND_CLUSTER)

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	"$(GOLANGCI_LINT)" config verify

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -o bin/manager cmd/main.go

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# Override BASE_IMAGE to build from another registry, e.g.
# make docker-build IMG=<img> BASE_IMAGE=docker.io/library/golang:1.26
.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build $(if $(BASE_IMAGE),--build-arg BASE_IMAGE=$(BASE_IMAGE)) -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	# copy existing Dockerfile and insert --platform=${BUILDPLATFORM} into Dockerfile.cross, and preserve the original Dockerfile
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name coxswain-builder
	$(CONTAINER_TOOL) buildx use coxswain-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) $(if $(BASE_IMAGE),--build-arg BASE_IMAGE=$(BASE_IMAGE)) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm coxswain-builder
	rm Dockerfile.cross

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/default > dist/install.yaml
	"$(KUSTOMIZE)" build config/cni-probe >> dist/install.yaml

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" apply -f -; else echo "No CRDs to install; skipping."; fi

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -; else echo "No CRDs to delete; skipping."; fi

# deploy = the base install (fail-closed: no --allow-unenforced). The D30 gate
# holds the sandbox Suspended until the eBPF engine proves enforcement (the I32
# relay, not yet wired). This is what a production install (dist/install.yaml)
# ships.
#
# deploy-dev = base + --allow-unenforced, via the config/dev kustomize overlay.
# For local kind dev only, so Loops run before the enforcement-evidence seam is
# wired (they run with PolicyEnforced=False reason EnforcementDisabled, loudly).
# Never use for production.

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config (fail-closed; no --allow-unenforced).
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" apply -f -
	"$(KUSTOMIZE)" build config/cni-probe | "$(KUBECTL)" apply -f -

.PHONY: deploy-dev
deploy-dev: manifests kustomize ## Dev/kind only: deploy the controller with --allow-unenforced (Loops run before the I32 enforcement-evidence relay is wired). Not for production.
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/dev | "$(KUBECTL)" apply -f -
	"$(KUSTOMIZE)" build config/cni-probe | "$(KUBECTL)" apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -
	"$(KUSTOMIZE)" build config/cni-probe | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

## Tool Binaries
KUBECTL ?= kubectl
KIND ?= kind
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

## Tool Versions
KUSTOMIZE_VERSION ?= v5.8.1
CONTROLLER_TOOLS_VERSION ?= v0.22.0

#ENVTEST_VERSION is the controller-runtime version to use for setup-envtest, derived from go.mod
ENVTEST_VERSION ?= $(shell v='$(call gomodver,sigs.k8s.io/controller-runtime)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_VERSION manually (controller-runtime replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v")

 #ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST
 # binaries (i.e. 1.31). Pinned explicitly (D5) so envtest does not silently track
 # the k8s.io/* client-lib version in go.mod (which would drift the apiserver).
ENVTEST_K8S_VERSION ?= 1.34.0

GOLANGCI_LINT_VERSION ?= v2.13.1
.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST in the local bin directory.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@"$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))
	@test -f .custom-gcl.yml && { \
		echo "Building custom golangci-lint with plugins..." && \
		$(GOLANGCI_LINT) custom --destination $(LOCALBIN) --name golangci-lint-custom && \
		mv -f $(LOCALBIN)/golangci-lint-custom $(GOLANGCI_LINT); \
	} || true

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] && [ "$$(readlink -- "$(1)" 2>/dev/null)" = "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f "$(1)" ;\
GOBIN="$(LOCALBIN)" go install $${package} ;\
mv "$(LOCALBIN)/$$(basename "$(1)")" "$(1)-$(3)" ;\
} ;\
ln -sf "$$(realpath "$(1)-$(3)")" "$(1)"
endef

define gomodver
$(shell go list -m -f '{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}' $(1) 2>/dev/null)
endef
