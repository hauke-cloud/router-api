# Makefile for router-api

CHART_DIR    ?= deployments/helm/router-api
IMAGE        ?= ghcr.io/hauke-cloud/router-api
IMAGE_TAG    ?= dev
PLATFORMS    ?= linux/amd64,linux/arm64

# One image carries all three managers; the chart picks the binary.
BINARIES := core infrastructure-hetzner config-vyos

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

PKG     := github.com/hauke-cloud/router-api/internal/version
LDFLAGS := -w -s -X $(PKG).version=$(VERSION) -X $(PKG).commit=$(COMMIT) -X $(PKG).date=$(DATE)

# Container engine. podman and docker are interchangeable here; docker only
# counts if its daemon answers, a docker CLI without one is common.
CONTAINER_ENGINE ?= $(shell docker info >/dev/null 2>&1 && echo docker || command -v podman 2>/dev/null)

# Pinned so that a local run and a CI run report the same findings.
GOLANGCI_LINT_VERSION ?= v2.14.0
GOLANGCI_LINT         ?= go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

# The CRDs and the deepcopy functions are generated from the types in api/.
CONTROLLER_GEN_VERSION ?= v0.22.0
CONTROLLER_GEN         ?= go run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)

# envtest runs the integration tests against a real kube-apiserver and etcd.
ENVTEST_K8S_VERSION ?= 1.37
SETUP_ENVTEST       ?= go run sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.25

# The generated CRDs live inside the package that embeds them. There is
# exactly one copy on purpose: each manager installs the CRDs of its own API
# group, so a second copy in the chart could only ever be out of date.
CRD_DIR := internal/crd/assets

.DEFAULT_GOAL := help

##@ General

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} \
	  /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 } \
	  /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Development

.PHONY: build
build: ## Build the three manager binaries
	@for binary in $(BINARIES); do \
	  echo "building bin/$$binary"; \
	  CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/$$binary ./cmd/$$binary || exit 1; \
	done

.PHONY: envtest-assets
envtest-assets: ## Download kube-apiserver and etcd for the integration tests
	@$(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path >/dev/null

.PHONY: test
test: ## Run unit and integration tests with the race detector
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" \
	  go test -race -count=1 ./...

.PHONY: cover
cover: ## Run tests and write a coverage profile
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" \
	  go test -race -count=1 -coverprofile=coverage.out -covermode=atomic ./...
	go tool cover -func=coverage.out | tail -1

.PHONY: fmt
fmt: ## Format the code
	gofmt -s -w .

.PHONY: fmt-check
fmt-check: ## Fail if any file is unformatted
	@# `gofmt -l` only lists offenders and still exits 0, so the check has to
	@# be inverted to be able to fail.
	@unformatted="$$(gofmt -s -l . | grep -v '^vendor/' || true)"; \
	if [ -n "$$unformatted" ]; then \
	  echo "not gofmt-clean:"; echo "$$unformatted"; gofmt -s -d .; exit 1; \
	fi

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: lint
lint: ## Run golangci-lint
	$(GOLANGCI_LINT) run

.PHONY: ci-lint
ci-lint: fmt-check vet lint generate-check helm-lint tag-guard ## Every lint CI runs, in one target

.PHONY: tidy
tidy: ## Tidy and verify go.mod
	go mod tidy
	go mod verify

.PHONY: check
check: fmt ci-lint test ## Everything CI runs on the Go code

.PHONY: clean
clean: ## Remove build artefacts
	rm -rf bin coverage.out

##@ Code generation

.PHONY: generate
generate: ## Regenerate the deepcopy functions and the embedded CRDs
	$(CONTROLLER_GEN) object:headerFile=hack/boilerplate.go.txt paths=./api/...
	$(CONTROLLER_GEN) crd:generateEmbeddedObjectMeta=false paths=./api/... \
	  output:crd:artifacts:config=$(CRD_DIR)

.PHONY: generate-check
generate-check: ## Fail if generated code is behind the Go types
	@# The CRDs ship inside the binaries, so a stale one is not a documentation
	@# problem -- it is a field the API server silently drops at runtime.
	@tmp="$$(mktemp -d)"; \
	$(CONTROLLER_GEN) crd:generateEmbeddedObjectMeta=false paths=./api/... \
	  output:crd:artifacts:config="$$tmp"; \
	if ! diff -ruN $(CRD_DIR) "$$tmp"; then \
	  rm -rf "$$tmp"; \
	  echo; echo "the embedded CRDs are out of date; run 'make generate'"; exit 1; \
	fi; \
	rm -rf "$$tmp"

##@ Container image

.PHONY: image
image: ## Build the container image for the host platform
	$(CONTAINER_ENGINE) build \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg COMMIT=$(COMMIT) \
	  --build-arg DATE=$(DATE) \
	  -t $(IMAGE):$(IMAGE_TAG) .

##@ Helm chart

# The shared CI action packages the chart but never lints or renders it, so a
# broken template would otherwise only surface at install time.
.PHONY: helm-lint
helm-lint: ## Lint the chart and render it with every option exercised
	helm lint $(CHART_DIR)
	helm template router-api $(CHART_DIR) --namespace router-api > /dev/null
	helm template router-api $(CHART_DIR) --namespace router-api \
	  --set metrics.serviceMonitor.enabled=true \
	  --set watchNamespace=routers \
	  --set crds.install=false \
	  --set managers.config-vyos.enabled=false \
	  --set 'managers.core.extraArgs[0]=-log-level=debug' > /dev/null

# CI rewrites every "tag:" line in values.yaml to the release version.
.PHONY: tag-guard
tag-guard: ## Fail unless values.yaml has exactly one "tag:" line
	@count="$$(grep -c 'tag:' $(CHART_DIR)/values.yaml)"; \
	  [ "$$count" -eq 1 ] || { echo "values.yaml must contain exactly one 'tag:' line, found $$count" >&2; exit 1; }

.PHONY: helm-install
helm-install: ## Install the chart into the current cluster
	helm upgrade --install router-api $(CHART_DIR) --namespace router-api --create-namespace

##@ Tests against real software

# Needs podman and the image from the OCI/vyos repository. Rootless is enough.
VYOS_IMAGE ?= ghcr.io/hauke-cloud/vyos:dev

.PHONY: test-vyos
test-vyos: ## Run the config provider against a real VyOS container
	VYOS_IMAGE=$(VYOS_IMAGE) go test -count=1 -timeout 15m -v ./test/vyos/

# Creates servers at Hetzner Cloud and deletes them again. See test/e2e for
# the environment it needs.
.PHONY: e2e
e2e: ## Run router-api against Hetzner Cloud for real (billed resources)
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" \
	  go test -count=1 -timeout 90m -v ./test/e2e/
