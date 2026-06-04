# Image URL to use all building/pushing image targets
IMG ?= controller:latest
VERSION ?= dev
LDFLAGS := -X main.version=$(VERSION)

# IMAGE_VARIANT selects which config/images/defaults.<variant>.env file is
# embedded into the operator binary and which images the E2E suite expects to
# be loaded into Kind. "dev" (default) tracks the mutable `:dev` aliases on
# the engine/metadata GHCR packages so a regression on `:dev` surfaces in CI
# before a partner pulling `:dev` sees it; "latest" pins to release-* build
# tags. "dev" stays the implicit default until the engine/metadata `:latest`
# GHCR aliases (and the auto-PR that bumps `defaults.latest.env`) are in
# place; once they land, flip the default back to "latest". The operator
# binary, the gateway pod template the operator stamps out, the image-load
# step, and the test process all derive their defaults from the same
# variant — set IMAGE_VARIANT consistently across `build`,
# `prepare-test-e2e`, and `test-e2e`.
IMAGE_VARIANT ?= dev

ifeq ($(IMAGE_VARIANT),latest)
GO_BUILD_TAGS_BASE := latest
else ifeq ($(IMAGE_VARIANT),dev)
GO_BUILD_TAGS_BASE :=
else
$(error Unsupported IMAGE_VARIANT=$(IMAGE_VARIANT); expected "latest" or "dev")
endif

# Comma-separated tag list passed to `go build -tags` / `go test -tags` /
# `ginkgo --tags`. Empty when no extra tags apply.
GO_BUILD_TAGS := $(GO_BUILD_TAGS_BASE)

# Helm chart configuration
HELM_CHART_DIR ?= helm/firebolt-operator
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
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole, CRDs, CRD JSON schemas, and description-slimmed chart CRDs.
	"$(CONTROLLER_GEN)" rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases
	python3 $(CURDIR)/scripts/patch-crd-template-metadata.py config/crd/bases/*.yaml
	mkdir -p $(HELM_CRD_CHART_DIR)/json-schema
	cd config/crd/bases && python3 $(CURDIR)/scripts/openapi2jsonschema.py *.yaml
	mv config/crd/bases/*.json $(HELM_CRD_CHART_DIR)/json-schema/
	# Ship description-slimmed CRDs in both Helm charts so each release Secret
	# stays under Kubernetes' 1 MiB cap. MUST run after patch-crd-template-metadata.py
	# (which keys off descriptions); config/crd/bases stays full-fat and canonical.
	python3 $(CURDIR)/scripts/strip-crd-descriptions.py --out-dir $(HELM_CRD_CHART_DIR)/templates config/crd/bases/*.yaml
	python3 $(CURDIR)/scripts/strip-crd-descriptions.py --out-dir $(HELM_CHART_DIR)/crds config/crd/bases/*.yaml

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt" paths="./..."

# envtest's embedded kube-apiserver does not honour SIGTERM on macOS; it only
# exits when envtest's SIGKILL fallback fires after the stop timeout. Shrink
# the timeout on Darwin so AfterSuite reaps it in ~5s instead of waiting the
# upstream default 20s. The resulting "timeout waiting for process" error is
# tolerated in suite_test.go (Darwin-only). Linux/CI is untouched.
# See controller-runtime#1571 / #2560.
ENVTEST_STOP_TIMEOUT_ENV :=
ifeq ($(shell uname -s),Darwin)
ENVTEST_STOP_TIMEOUT_ENV := KUBEBUILDER_CONTROLPLANE_STOP_TIMEOUT=5s
endif

.PHONY: test
test: manifests generate setup-envtest ## Run tests.
	$(ENVTEST_STOP_TIMEOUT_ENV) KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" go test -tags "$(GO_BUILD_TAGS)" $$(go list ./... | grep -v /e2e) -coverprofile cover.out

RAPID_CHECKS ?= 25

.PHONY: test-property
test-property: manifests generate setup-envtest ## Run the outer-Reconcile rapid harness (Phase 9). Override RAPID_CHECKS for deeper runs.
	$(ENVTEST_STOP_TIMEOUT_ENV) KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" \
		go test -tags outerharness -run TestEngineOuterStateMachine -count=1 \
		./internal/controller/... -args -rapid.checks=$(RAPID_CHECKS)

.PHONY: test-webhook-integration
test-webhook-integration: manifests generate setup-envtest ## Run the webhook-on integration suite (envtest + manager exposing the operator's admission webhooks).
	$(ENVTEST_STOP_TIMEOUT_ENV) KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" \
		go test -tags webhook_integration -count=1 ./internal/controller/...

KIND_CLUSTER ?= operator-test-e2e

# Local Docker registry that kind nodes mirror through (avoids per-node
# duplication of multi-GB images on multi-node clusters). Override
# REGISTRY_PORT / REGISTRY_NAME if 5001 / kind-registry collide with another
# tool. The same defaults are baked into scripts/setup-local-registry.sh.
REGISTRY_NAME ?= kind-registry
REGISTRY_PORT ?= 5001

.PHONY: setup-local-registry
setup-local-registry: ## Start the local Docker registry that kind nodes mirror through.
	@REGISTRY_NAME=$(REGISTRY_NAME) REGISTRY_PORT=$(REGISTRY_PORT) ./scripts/setup-local-registry.sh

.PHONY: cleanup-local-registry
cleanup-local-registry: ## Stop and remove the local Docker registry container (cached images are lost).
	@docker rm -f $(REGISTRY_NAME) >/dev/null 2>&1 || true
	@echo "Removed local registry '$(REGISTRY_NAME)' (if it existed). Re-run 'make setup-local-registry' to recreate."

.PHONY: flush-local-registry
flush-local-registry: cleanup-local-registry setup-local-registry ## Recreate the local registry from scratch (drops cached images).

.PHONY: setup-kind
setup-kind: setup-local-registry ## Create a Kind cluster if it does not exist (also starts the local registry).
	@REGISTRY_NAME=$(REGISTRY_NAME) REGISTRY_PORT=$(REGISTRY_PORT) ./scripts/setup-kind-cluster.sh $(KIND_CLUSTER)

.PHONY: load-test-images
load-test-images: ## Publish required Docker images to the local registry (via mirror, kind nodes pull on demand).
	IMAGE_VARIANT=$(IMAGE_VARIANT) REGISTRY_NAME=$(REGISTRY_NAME) REGISTRY_PORT=$(REGISTRY_PORT) ./scripts/load-e2e-images.sh $(KIND_CLUSTER)

.PHONY: prepare-test-e2e
prepare-test-e2e: manifests generate setup-kind load-test-images ## Full setup: create cluster as needed, publish images

GINKGO_FOCUS ?=

# GINKGO_PROCS controls how many Ginkgo processes run specs in parallel.
# Default: half of the host's online CPUs, with a floor of 1. Override on the
# command line (e.g. GINKGO_PROCS=1 for serial debugging).
GINKGO_PROCS ?= $(shell n=$$(nproc 2>/dev/null || getconf _NPROCESSORS_ONLN 2>/dev/null || echo 2); p=$$((n / 2)); [ $$p -lt 1 ] && p=1; echo $$p)

# GINKGO_TAGS is the full --tags value passed to ginkgo: always "e2e", plus
# the variant-specific build tag (currently only "latest") so the embedded
# defaults match the images that load-e2e-images.sh just pushed into Kind.
# The "dev" variant carries no extra tag — it is the implicit default.
ifeq ($(GO_BUILD_TAGS_BASE),)
GINKGO_TAGS := e2e
else
GINKGO_TAGS := e2e,$(GO_BUILD_TAGS_BASE)
endif

.PHONY: test-e2e
test-e2e: ginkgo ## Run E2E tests against an existing Kind cluster (run prepare-test-e2e first)
	KIND=$(KIND) KIND_CLUSTER=$(KIND_CLUSTER) \
		REGISTRY_HOST_ENDPOINT="localhost:$(REGISTRY_PORT)" \
		"$(GINKGO)" run \
		--tags=$(GINKGO_TAGS) \
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
	"$(GOLANGCI_LINT)" run $(if $(GO_BUILD_TAGS),--build-tags=$(GO_BUILD_TAGS),)

##@ Formal Verification

TLA2TOOLS ?= $(LOCALBIN)/tla2tools.jar
TLA2TOOLS_VERSION ?= v1.8.0
TLA2TOOLS_URL ?= https://github.com/tlaplus/tlaplus/releases/download/$(TLA2TOOLS_VERSION)/tla2tools.jar

$(TLA2TOOLS): $(LOCALBIN)
	wget -q -O "$(TLA2TOOLS)" "$(TLA2TOOLS_URL)"

.PHONY: tla2tools
tla2tools: $(TLA2TOOLS) ## Download tla2tools.jar locally if necessary.

.PHONY: formal-check
formal-check: tla2tools ## Run TLC model checker on all TLA+ specs.
	java -cp "$(TLA2TOOLS)" tlc2.TLC -workers auto -config formal/FireboltEngine.cfg formal/FireboltEngine.tla
	java -cp "$(TLA2TOOLS)" tlc2.TLC -workers auto -config formal/FireboltInstance.cfg formal/FireboltInstance.tla

.PHONY: formal-dump
formal-dump: tla2tools ## Dump the TLC state graphs for both specs to formal/*.dot.
	java -cp "$(TLA2TOOLS)" tlc2.TLC -workers auto \
		-config formal/FireboltEngine.cfg \
		-dump dot,actionlabels formal/FireboltEngine.dot \
		formal/FireboltEngine.tla
	java -cp "$(TLA2TOOLS)" tlc2.TLC -workers auto \
		-config formal/FireboltInstance.cfg \
		-dump dot,actionlabels formal/FireboltInstance.dot \
		formal/FireboltInstance.tla

.PHONY: formal-gen
formal-gen: formal-dump ## Regenerate the TLA+ state-cover test fixtures from the TLC state graphs.
	python3 scripts/gen-tla-state-tests.py \
		--dot formal/FireboltEngine.dot \
		--out internal/controller/engine_tla_states_data_test.go
	python3 scripts/gen-tla-instance-state-tests.py \
		--dot formal/FireboltInstance.dot \
		--out internal/controller/instance_tla_states_data_test.go

.PHONY: formal-verify
formal-verify: formal-gen ## CI guard: regenerate the fixtures and fail if any generated file changed.
	@for f in internal/controller/engine_tla_states_data_test.go internal/controller/instance_tla_states_data_test.go; do \
		if ! git diff --quiet -- "$$f"; then \
			echo "ERROR: TLA+ state-cover fixture $$f is out of date. Run 'make formal-gen' and commit the result." >&2; \
			git --no-pager diff -- "$$f"; \
			exit 1; \
		fi; \
	done

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	"$(GOLANGCI_LINT)" run --fix $(if $(GO_BUILD_TAGS),--build-tags=$(GO_BUILD_TAGS),)

.PHONY: docs-check
docs-check: ## Validate Mintlify navigation (path depth and lost pages)
	$(MAKE) -C docs check

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	"$(GOLANGCI_LINT)" config verify

##@ Build

.PHONY: build
build: manifests generate ## Build manager binary.
	# Always target Linux (for Kind/K8s); GOARCH from host matches the cluster node arch (same as Dockerfile.ci TARGETARCH).
	CGO_ENABLED=0 GOOS=linux GOARCH=$(shell go env GOARCH) go build -tags "$(GO_BUILD_TAGS)" -ldflags "$(LDFLAGS)" -o bin/manager cmd/main.go

.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	DOCKER_BUILDKIT=1 $(CONTAINER_TOOL) build \
		--build-arg VERSION=$(VERSION) \
		--build-arg IMAGE_VARIANT=$(IMAGE_VARIANT) \
		--secret id=gitconfig,src=$(HOME)/.gitconfig \
		-f Dockerfile.ci -t ${IMG} .

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
		--set fullnameOverride=firebolt-operator \
		--set image.repository=$(LOCAL_IMG_REPO) \
		--set image.tag=$(LOCAL_IMG_TAG) \
		--set image.pullPolicy=Never \
		--set metrics.secure=false \
		--set leaderElection.enabled=false \
		--set logging.development=true \
		--set-string podAnnotations.deploy-timestamp="$(shell date +%s)"
	$(KUBECTL) rollout status deployment/firebolt-operator -n default --timeout=30s

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

.PHONY: helm-test
HELM_TEST_BASIC_NS ?= helm-verify-basic
HELM_TEST_FULL_NS ?= helm-verify-full
HELM_TEST_CONTEXT ?= kind-$(KIND_CLUSTER)
helm-test: ## Run Helm quickstart validation scripts against current cluster/operator.
	@ctx="$$( $(KUBECTL) config current-context 2>/dev/null || true )"; \
	if [ "$$ctx" != "$(HELM_TEST_CONTEXT)" ]; then \
		echo "Refusing to run helm-test on kube context '$$ctx' (expected '$(HELM_TEST_CONTEXT)')." >&2; \
		echo "Switch context or override HELM_TEST_CONTEXT / KIND_CLUSTER explicitly." >&2; \
		exit 1; \
	fi
	./scripts/ci/verify-quickstart-basic.sh "$(HELM_TEST_BASIC_NS)"
	./scripts/ci/verify-quickstart-full.sh "$(HELM_TEST_FULL_NS)"

.PHONY: helm-package
helm-package: ## Package the Helm charts into dist/.
	mkdir -p dist
	helm package $(HELM_CHART_DIR) --destination dist/
	helm package $(HELM_CRD_CHART_DIR) --destination dist/

.PHONY: helm-push
helm-push: helm-package ## Package and push the Helm charts to ECR.
	helm push dist/firebolt-operator-[0-9]*.tgz $(HELM_REGISTRY)
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
