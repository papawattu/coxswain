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
AGENT_SANDBOX_MANIFEST ?= https://github.com/kubernetes-sigs/agent-sandbox/releases/download/$(AGENT_SANDBOX_VERSION)/sandbox.yaml
# The controller image the release manifest references (pre-loaded into the
# kind node so an offline host doesn't depend on the node reaching
# registry.k8s.io).
AGENT_SANDBOX_CONTROLLER_IMAGE ?= registry.k8s.io/agent-sandbox/agent-sandbox-controller:$(AGENT_SANDBOX_VERSION)

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

# Build the D33/D34 proxy stand-in image. Used by the kind e2e and by
# make kind-up / deploy-dev. The image is loaded into the kind cluster
# via `kind load docker-image coxswain-proxy:standin`.
PROXY_IMG ?= coxswain-proxy:standin
.PHONY: proxy-build
proxy-build: ## Build the proxy stand-in image (coxswain-proxy:standin).
	$(CONTAINER_TOOL) build -t $(PROXY_IMG) cmd/proxy-standin/

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

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -

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
