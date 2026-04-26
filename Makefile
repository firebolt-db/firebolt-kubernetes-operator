# Image URL to use all building/pushing image targets
IMG ?= controller:latest
VERSION ?= dev
LDFLAGS := -X main.version=$(VERSION)

# Helm chart configuration
HELM_CHART_DIR ?= helm/firebolt-kubernetes-operator
HELM_CRD_CHART_DIR ?= helm/firebolt-operator-crds
HELM_REGISTRY ?= oci://ghcr.io/firebolt-db/helm-charts

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
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: test
test: manifests generate setup-envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

KIND_CLUSTER ?= operator-test-e2e

.PHONY: setup-kind
setup-kind: ## Create a Kind cluster if it does not exist
	@./scripts/setup-kind-cluster.sh $(KIND_CLUSTER)

.PHONY: load-test-images
load-test-images: ## Load required Docker images into the Kind cluster
	./scripts/load-e2e-images.sh $(KIND_CLUSTER)

.PHONY: prepare-test-e2e
prepare-test-e2e: manifests generate setup-kind load-test-images ## Full setup: create cluster as needed, load images

GINKGO_FOCUS ?=

# GINKGO_PROCS controls how many Ginkgo processes run specs in parallel.
# Default: half of the host's online CPUs, with a floor of 1. Override on the
# command line (e.g. GINKGO_PROCS=1 for serial debugging).
GINKGO_PROCS ?= $(shell n=$$(nproc 2>/dev/null || getconf _NPROCESSORS_ONLN 2>/dev/null || echo 2); p=$$((n / 2)); [ $$p -lt 1 ] && p=1; echo $$p)

.PHONY: test-e2e
test-e2e: ginkgo ## Run E2E tests against an existing Kind cluster (run prepare-test-e2e first)
	KIND=$(KIND) KIND_CLUSTER=$(KIND_CLUSTER) \
		"$(GINKGO)" run \
		--tags=e2e \
		-v \
		--no-color \
		--junit-report=e2e-report.xml \
		--poll-progress-after=30s \
		--poll-progress-interval=30s \
		--procs=$(GINKGO_PROCS) \
		--timeout=30m \
		$(if $(GINKGO_FOCUS),--focus="$(GINKGO_FOCUS)") \
		./test/e2e/

.PHONY: cleanup-test-e2e
cleanup-test-e2e: ## Tear down the Kind cluster used for e2e tests
	@$(KIND) delete cluster --name $(KIND_CLUSTER)

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	"$(GOLANGCI_LINT)" run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	"$(GOLANGCI_LINT)" run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	"$(GOLANGCI_LINT)" config verify

##@ Build

.PHONY: build
build: manifests generate ## Build manager binary.
	# Always target Linux (for Kind/K8s); GOARCH from host matches the cluster node arch (same as Dockerfile.ci TARGETARCH).
	CGO_ENABLED=0 GOOS=linux GOARCH=$(shell go env GOARCH) go build -ldflags "$(LDFLAGS)" -o bin/manager cmd/main.go

.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	DOCKER_BUILDKIT=1 $(CONTAINER_TOOL) build --build-arg VERSION=$(VERSION) --secret id=gitconfig,src=$(HOME)/.gitconfig -f Dockerfile.ci -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

LOCAL_IMG_REPO ?= firebolt-operator
LOCAL_IMG_TAG ?= local
LOCAL_IMG ?= $(LOCAL_IMG_REPO):$(LOCAL_IMG_TAG)

.PHONY: docker-build-local
docker-build-local: build ## Build a local Docker image from pre-built binary.
	$(CONTAINER_TOOL) build -t $(LOCAL_IMG) .

.PHONY: kind-load-operator
kind-load-operator: ## Load the operator image into the Kind cluster.
	$(KIND) load docker-image $(LOCAL_IMG) --name $(KIND_CLUSTER)

.PHONY: local-deploy
local-deploy: docker-build-local kind-load-operator manifests ## Build, load, and deploy operator to Kind (one command).
	helm upgrade --install firebolt-operator $(HELM_CHART_DIR) \
		--set fullnameOverride=firebolt-kubernetes-operator \
		--set image.repository=$(LOCAL_IMG_REPO) \
		--set image.tag=$(LOCAL_IMG_TAG) \
		--set image.pullPolicy=Never \
		--set metrics.secure=false \
		--set leaderElection.enabled=false \
		--set additionalArgs='{--enable-webhooks=false}' \
		--set-string podAnnotations.deploy-timestamp="$(shell date +%s)"

.PHONY: local-undeploy
local-undeploy: ## Remove the operator Helm release.
	helm uninstall firebolt-operator

##@ Deployment

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" apply -f -; else echo "No CRDs to install; skipping."; fi

##@ Helm

.PHONY: helm-docs
helm-docs: ## Generate Helm chart README from values.yaml comments.
	helm-docs --chart-search-root helm/ --template-files README.md.gotmpl

.PHONY: helm-lint
helm-lint: ## Lint the Helm charts.
	helm lint $(HELM_CHART_DIR)
	helm lint $(HELM_CRD_CHART_DIR)

.PHONY: helm-template
helm-template: ## Render Helm chart templates locally.
	helm template firebolt-operator $(HELM_CHART_DIR)
	helm template firebolt-crds $(HELM_CRD_CHART_DIR)

.PHONY: helm-package
helm-package: ## Package the Helm charts into dist/.
	mkdir -p dist
	helm package $(HELM_CHART_DIR) --destination dist/
	helm package $(HELM_CRD_CHART_DIR) --destination dist/

.PHONY: helm-push
helm-push: helm-package ## Package and push the Helm charts to ECR.
	helm push dist/firebolt-kubernetes-operator-*.tgz $(HELM_REGISTRY)
	helm push dist/firebolt-operator-crds-*.tgz $(HELM_REGISTRY)

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
GINKGO ?= $(LOCALBIN)/ginkgo

## Tool Versions
KUSTOMIZE_VERSION ?= v5.7.1
CONTROLLER_TOOLS_VERSION ?= v0.19.0
GINKGO_VERSION ?= $(shell v='$(call gomodver,github.com/onsi/ginkgo/v2)'; \
  [ -n "$$v" ] || { echo "Set GINKGO_VERSION manually (onsi/ginkgo/v2 not in go.mod)" >&2; exit 1; }; \
  printf '%s\n' "$$v")

#ENVTEST_VERSION is the version of controller-runtime release branch to fetch the envtest setup script (i.e. release-0.20)
ENVTEST_VERSION ?= $(shell v='$(call gomodver,sigs.k8s.io/controller-runtime)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_VERSION manually (controller-runtime replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?([0-9]+)\.([0-9]+).*/release-\1.\2/')

#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell v='$(call gomodver,k8s.io/api)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_K8S_VERSION manually (k8s.io/api replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')

GOLANGCI_LINT_VERSION ?= v2.5.0
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

.PHONY: ginkgo
ginkgo: $(GINKGO) ## Download the ginkgo CLI locally if necessary.
$(GINKGO): $(LOCALBIN)
	$(call go-install-tool,$(GINKGO),github.com/onsi/ginkgo/v2/ginkgo,$(GINKGO_VERSION))

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
