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
	@# The Helm chart carries its own copy of the CRDs, and the plugin only rewrites
	@# them when the whole chart is regenerated -- which would overwrite this
	@# project's customisations. Syncing them here keeps the chart from shipping a
	@# CRD the operator no longer matches.
	$(MAKE) --no-print-directory helm-crds

.PHONY: bucket-name
bucket-name: ## Print the bucket name a StaticSite will get. NAMESPACE=web NAME=docs [CLUSTER_UUID=...]
	@test -n "$(NAMESPACE)" || { echo "Set NAMESPACE, e.g. make bucket-name NAMESPACE=web NAME=docs"; exit 1; }
	@test -n "$(NAME)" || { echo "Set NAME, e.g. make bucket-name NAMESPACE=web NAME=docs"; exit 1; }
	@go run ./hack/tools/bucketname \
		--namespace "$(NAMESPACE)" --name "$(NAME)" \
		$(if $(CLUSTER_UUID),--cluster-uuid "$(CLUSTER_UUID)") \
		$(if $(BUCKET_NAME_TEMPLATE),--template "$(BUCKET_NAME_TEMPLATE)")

.PHONY: helm-crds
helm-crds: ## Sync the generated CRDs into the Helm chart's templates.
	go run ./hack/tools/helmsync --crd-dir config/crd/bases --chart-crd-dir dist/chart/templates/crd

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

# The integration tests talk to a real S3 API rather than a mock, because what
# they check is how a provider behaves. MINIO_IMAGE is a local, S3-compatible
# server started in Docker for the duration of the run; point S3_TEST_ENDPOINT at
# something else to run them against another provider.
MINIO_IMAGE ?= quay.io/minio/minio:latest
MINIO_CONTAINER ?= static-s3-operator-test-minio
MINIO_PORT ?= 9100
MINIO_ACCESS_KEY ?= minioadmin
MINIO_SECRET_KEY ?= minioadmin

.PHONY: test-integration
test-integration: manifests generate fmt vet ## Run integration tests against a local S3 server started in Docker.
	@$(MAKE) --no-print-directory start-minio
	@set -e; trap '$(MAKE) --no-print-directory stop-minio' EXIT; \
		S3_TEST_ENDPOINT=http://127.0.0.1:$(MINIO_PORT) \
		S3_TEST_ACCESS_KEY=$(MINIO_ACCESS_KEY) \
		S3_TEST_SECRET_KEY=$(MINIO_SECRET_KEY) \
		go test -tags=integration -count=1 ./internal/s3/... ./internal/syncer/...

.PHONY: start-minio
start-minio: ## Start a local S3 server for the integration tests.
	@if [ -n "$$(docker ps -aq -f name=^$(MINIO_CONTAINER)$$)" ]; then \
		docker rm -f $(MINIO_CONTAINER) >/dev/null; \
	fi
	@echo "Starting a local S3 server on port $(MINIO_PORT)..."
	@docker run -d --rm --name $(MINIO_CONTAINER) \
		-p $(MINIO_PORT):9000 \
		-e MINIO_ROOT_USER=$(MINIO_ACCESS_KEY) \
		-e MINIO_ROOT_PASSWORD=$(MINIO_SECRET_KEY) \
		$(MINIO_IMAGE) server /data >/dev/null
	@echo "Waiting for the S3 server to become ready..."
	@for i in $$(seq 1 60); do \
		if curl -fsS "http://127.0.0.1:$(MINIO_PORT)/minio/health/live" >/dev/null 2>&1; then \
			echo "S3 server is ready."; exit 0; \
		fi; \
		sleep 1; \
	done; \
	echo "The S3 server did not become ready in time; logs follow:"; \
	docker logs $(MINIO_CONTAINER) || true; \
	exit 1

.PHONY: stop-minio
stop-minio: ## Stop the local S3 server.
	@docker rm -f $(MINIO_CONTAINER) >/dev/null 2>&1 || true

.PHONY: schemas
schemas: manifests ## Generate standalone JSON Schemas for the CRDs, for editor completion and validation.
	go run ./hack/tools/genschema --crd-dir config/crd/bases --output-dir dist/schemas --id-prefix "$(SCHEMA_ID_PREFIX)"

SCHEMA_ID_PREFIX ?= https://raw.githubusercontent.com/rashkash103/static-s3-operator/main/dist/schemas

.PHONY: verify-generated
verify-generated: manifests generate schemas build-installer ## Fail if generated files are out of date.
	go run ./hack/tools/helmsync --check
	@if [ -n "$$(git status --porcelain -- config dist api)" ]; then \
		echo "Generated files are out of date. Run 'make manifests generate schemas build-installer' and commit the result:"; \
		git --no-pager diff -- config dist api; \
		exit 1; \
	fi
	@# go.mod belongs here too. Leaving it to CI meant a missing `go mod tidy` after
	@# an import change was only caught after a push, twice.
	go mod tidy
	@if [ -n "$$(git status --porcelain -- go.mod go.sum)" ]; then \
		echo "go.mod is not tidy. Run 'go mod tidy' and commit the result:"; \
		git --no-pager diff -- go.mod go.sum; \
		exit 1; \
	fi
	@echo "Generated files are up to date and go.mod is tidy."

.PHONY: helm-chart
helm-chart: manifests generate ## Regenerate the Helm chart from the kustomize output.
	@echo "Regenerating the Helm chart. The chart's operator settings live in"
	@echo "dist/chart/templates/_operator.tpl and the values block in values.yaml;"
	@echo "re-apply these to templates/manager/manager.yaml if the plugin"
	@echo "overwrites them:"
	@echo "  - the operatorArgs include"
	@echo "  - the certManager.enabled hunks: --metrics-cert-path, the"
	@echo "    metrics-certs volume, and its mount"
	kubebuilder edit --plugins=helm/v2-alpha --force

.PHONY: helm-lint
# Under bin/ deliberately: anywhere tracked and a leftover render would show up as
# drift in verify-generated.
SCRATCH_RENDER ?= $(LOCALBIN)/helm-lint-render.yaml
helm-lint: ## Lint and render the Helm chart.
	@mkdir -p $(LOCALBIN)
	helm lint dist/chart
	helm template static-s3-operator dist/chart >/dev/null
	helm template static-s3-operator dist/chart \
		--set operator.credentials.create=true \
		--set operator.credentials.accessKeyID=test \
		--set operator.credentials.secretAccessKey=test >/dev/null
	@# The monitoring paths, which are off by default and so otherwise unrendered.
	@# certManager.enabled once added cert references to the ServiceMonitor with
	@# nothing issuing or serving the certificate.
	helm template static-s3-operator dist/chart \
		--set metrics.enabled=true \
		--set prometheus.enabled=true \
		--set certManager.enabled=true \
		--set 'metrics.readers[0].name=prometheus-k8s' \
		--set 'metrics.readers[0].namespace=monitoring' > $(SCRATCH_RENDER)
	@# A Certificate the manager never mounts, or a mount without a Certificate,
	@# both render happily and both break metrics.
	@grep -q 'kind: Certificate' $(SCRATCH_RENDER) \
		|| { echo "certManager.enabled rendered no Certificate"; exit 1; }
	@grep -q -- '--metrics-cert-path=' $(SCRATCH_RENDER) \
		|| { echo "certManager.enabled did not give the manager --metrics-cert-path"; exit 1; }
	@grep -q 'secretName: metrics-server-cert' $(SCRATCH_RENDER) \
		|| { echo "the metrics-certs volume does not reference the issued Secret"; exit 1; }
	@grep -q 'name: prometheus-k8s' $(SCRATCH_RENDER) \
		|| { echo "metrics.readers produced no ClusterRoleBinding subject"; exit 1; }
	@rm -f $(SCRATCH_RENDER)
	helm template static-s3-operator dist/chart --set metrics.enabled=false >/dev/null
	@echo "Helm chart renders cleanly."

# The e2e suite runs the whole thing in a throwaway Kind cluster: it builds the
# operator image, deploys an object store and a container registry, publishes a
# generated sample site, and fetches the result over HTTP. It needs Docker and
# Kind, and installs no cert-manager, because this operator has no webhooks.
#
# kubectl kuberc is disabled during the run so local kubectl preferences cannot
# change how the commands behave; enable it with KUBECTL_KUBERC=true.
KIND_CLUSTER ?= static-s3-operator-test-e2e

.PHONY: setup-test-e2e
setup-test-e2e: kind ## Set up a Kind cluster for e2e tests if it does not exist
	@case "$$($(KIND) get clusters)" in \
		*"$(KIND_CLUSTER)"*) \
			echo "Kind cluster '$(KIND_CLUSTER)' already exists. Skipping creation." ;; \
		*) \
			echo "Creating Kind cluster '$(KIND_CLUSTER)'..."; \
			$(KIND) create cluster --name $(KIND_CLUSTER) ;; \
	esac

.PHONY: test-e2e
test-e2e: setup-test-e2e manifests generate fmt vet ## Run the e2e tests. Expected an isolated environment using Kind.
	KIND=$(KIND) KIND_CLUSTER=$(KIND_CLUSTER) go test -tags=e2e ./test/e2e/ -v -ginkgo.v
	$(MAKE) cleanup-test-e2e

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
build: manifests generate fmt vet ## Build manager binary.
	go build -o bin/manager cmd/main.go

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build -t ${IMG} .

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
	- $(CONTAINER_TOOL) buildx create --name static-s3-operator-builder
	$(CONTAINER_TOOL) buildx use static-s3-operator-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm static-s3-operator-builder
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
# Prefer a kind already on PATH, and fall back to the one `make kind` downloads,
# so `make test-e2e` works on a machine that has never installed it.
KIND ?= $(shell command -v kind 2>/dev/null || echo "$(LOCALBIN)/kind")
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

## Tool Versions
KUSTOMIZE_VERSION ?= v5.8.1
CONTROLLER_TOOLS_VERSION ?= v0.21.0

#ENVTEST_VERSION is the controller-runtime version to use for setup-envtest, derived from go.mod
ENVTEST_VERSION ?= $(shell v='$(call gomodver,sigs.k8s.io/controller-runtime)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_VERSION manually (controller-runtime replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v")

#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell v='$(call gomodver,k8s.io/api)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_K8S_VERSION manually (k8s.io/api replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')

GOLANGCI_LINT_VERSION ?= v2.12.2
KIND_VERSION ?= v0.31.0
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

.PHONY: kind
kind: ## Download kind into bin/ if it is not already on PATH.
	@if command -v $(KIND) >/dev/null 2>&1; then \
		echo "Using $$($(KIND) version | head -1)"; \
	else \
		echo "kind is not on PATH; installing $(KIND_VERSION) into $(LOCALBIN)"; \
		$(MAKE) --no-print-directory $(LOCALBIN)/kind; \
	fi

$(LOCALBIN)/kind: $(LOCALBIN)
	$(call go-install-tool,$(LOCALBIN)/kind,sigs.k8s.io/kind,$(KIND_VERSION))

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

##@ Helm Deployment

## Helm binary to use for deploying the chart
HELM ?= helm
## Namespace to deploy the Helm release
HELM_NAMESPACE ?= static-s3-operator-system
## Name of the Helm release
HELM_RELEASE ?= static-s3-operator
## Path to the Helm chart directory
HELM_CHART_DIR ?= dist/chart
## Additional arguments to pass to helm commands
HELM_EXTRA_ARGS ?=

.PHONY: install-helm
install-helm: ## Install the latest version of Helm.
	@command -v $(HELM) >/dev/null 2>&1 || { \
		echo "Installing Helm..." && \
		curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-4 | bash; \
	}

.PHONY: helm-deploy
helm-deploy: install-helm ## Deploy manager to the K8s cluster via Helm. Specify an image with IMG.
	$(HELM) upgrade --install $(HELM_RELEASE) $(HELM_CHART_DIR) \
		--namespace $(HELM_NAMESPACE) \
		--create-namespace \
		--set manager.image.repository=$${IMG%:*} \
		--set manager.image.tag=$${IMG##*:} \
		--wait \
		--timeout 5m \
		$(HELM_EXTRA_ARGS)

.PHONY: helm-uninstall
helm-uninstall: ## Uninstall the Helm release from the K8s cluster.
	$(HELM) uninstall $(HELM_RELEASE) --namespace $(HELM_NAMESPACE)

.PHONY: helm-status
helm-status: ## Show Helm release status.
	$(HELM) status $(HELM_RELEASE) --namespace $(HELM_NAMESPACE)

.PHONY: helm-history
helm-history: ## Show Helm release history.
	$(HELM) history $(HELM_RELEASE) --namespace $(HELM_NAMESPACE)

.PHONY: helm-rollback
helm-rollback: ## Rollback to previous Helm release.
	$(HELM) rollback $(HELM_RELEASE) --namespace $(HELM_NAMESPACE)
