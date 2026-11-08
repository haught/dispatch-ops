# Dispatch Ops development tasks. Run `make help` for a list.

-include .env
export

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
BIN := bin/dispatch-ops
TEST_DATABASE_URL := postgres://dispatch:dispatch@localhost:5433/dispatch_test?sslmode=disable

IMAGE_TAG ?= dev
KIND_VERSION := v0.33.0
KIND := go run sigs.k8s.io/kind@$(KIND_VERSION)
CONTROLLER_GEN := go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.23.0
CHART := deploy/helm/dispatch-ops-operator
KIND_CLUSTER := dispatch-ops
KUBE_CONTEXT := kind-$(KIND_CLUSTER)
DISPATCH_JOB_NAMESPACE ?= dispatch-jobs

# In the dev container (.devcontainer), Docker runs on the host, so Postgres's
# published port is on host.docker.internal rather than localhost.
ifdef DISPATCH_DEVCONTAINER
DISPATCH_DATABASE_URL := $(subst @localhost:,@host.docker.internal:,$(DISPATCH_DATABASE_URL))
TEST_DATABASE_URL := $(subst @localhost:,@host.docker.internal:,$(TEST_DATABASE_URL))
endif

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: setup-env
setup-env: ## Create .env from .env.example
	@test -f .env && echo ".env already exists" || cp .env.example .env

.PHONY: deps
deps: ## Install Go and npm dependencies
	go mod download
	cd web && npm ci

# --- Code generation ---------------------------------------------------------

.PHONY: generate
generate: manifests ## Regenerate Go server and TS client from api/openapi.yaml, and the CRD
	go generate ./internal/api/apigen/ ./internal/runner/runnerapi/
	cd web && npm run gen:api

.PHONY: manifests
manifests: ## Regenerate the DispatchOps CRD and its deepcopy code from internal/operator/api
	$(CONTROLLER_GEN) object paths=./internal/operator/api/... \
		crd paths=./internal/operator/api/... output:crd:dir=$(CHART)/files

# --- Database ----------------------------------------------------------------

.PHONY: db-up
db-up: ## Start Postgres in Docker (host port 5433)
	docker compose up -d --wait postgres

.PHONY: db-down
db-down: ## Stop Postgres (data is kept)
	docker compose down

.PHONY: migrate
migrate: ## Apply pending migrations
	go run ./cmd/dispatch-ops migrate up

.PHONY: migrate-down
migrate-down: ## Roll back the latest migration
	go run ./cmd/dispatch-ops migrate down

.PHONY: admin
admin: ## Create an "admin" user with a generated password
	go run ./cmd/dispatch-ops user create -username admin -display-name Administrator -admin

# --- Run ---------------------------------------------------------------------

.PHONY: run
run: ## Run the API server (serves the last UI build)
	go run -ldflags "-X main.version=$(VERSION)" ./cmd/dispatch-ops serve

.PHONY: task-manager
task-manager: ## Run the task manager (runs jobs on the kind cluster; see kind-up)
	go run -ldflags "-X main.version=$(VERSION)" ./cmd/dispatch-ops task-manager

.PHONY: web-dev
web-dev: ## Run the Vite dev server with hot reload (proxies /api to :8080)
	cd web && npm run dev

# --- Dev cluster and execution environments ----------------------------------

.PHONY: kind-up
kind-up: ## Create the kind cluster and the job namespace
	@$(KIND) get clusters 2>/dev/null | grep -qx $(KIND_CLUSTER) || $(KIND) create cluster --config deploy/dev/kind.yaml
ifdef DISPATCH_DEVCONTAINER
	@$(MAKE) --no-print-directory kind-join
endif
	@kubectl --context $(KUBE_CONTEXT) get namespace $(DISPATCH_JOB_NAMESPACE) >/dev/null 2>&1 \
		|| kubectl --context $(KUBE_CONTEXT) create namespace $(DISPATCH_JOB_NAMESPACE)

.PHONY: kind-join
kind-join: ## Dev container only: join the kind network and point kubectl at it (run after a rebuild)
	@docker inspect -f '{{range $$k, $$v := .NetworkSettings.Networks}}{{$$k}} {{end}}' $$(hostname) | grep -qw kind \
		|| docker network connect kind $$(hostname)
	@$(KIND) export kubeconfig --name $(KIND_CLUSTER) --internal

.PHONY: kind-down
kind-down: ## Delete the kind cluster
	$(KIND) delete cluster --name $(KIND_CLUSTER)

EES := ee-shell ee-python ee-ansible

.PHONY: images
images: ## Build the runner-base image and the starter EEs (ee-shell, ee-python, ee-ansible)
	docker build -f images/runner-base/Dockerfile --build-arg VERSION=$(VERSION) -t dispatch-ops/runner-base:$(IMAGE_TAG) .
	for ee in $(EES); do \
		docker build --build-arg RUNNER_BASE=dispatch-ops/runner-base:$(IMAGE_TAG) -t dispatch-ops/$$ee:$(IMAGE_TAG) images/$$ee || exit 1; \
	done

.PHONY: kind-load
kind-load: images ## Build the images and load them into the kind cluster
	$(KIND) load docker-image --name $(KIND_CLUSTER) dispatch-ops/runner-base:$(IMAGE_TAG) $(foreach ee,$(EES),dispatch-ops/$(ee):$(IMAGE_TAG))

# --- Operator on the dev cluster ---------------------------------------------

DEV_NAMESPACE := dispatch-ops

.PHONY: kind-operator
kind-operator: ## Build the server and operator images and install the operator on the kind cluster
	docker build --build-arg VERSION=$(VERSION) -t dispatch-ops:$(IMAGE_TAG) .
	docker build --build-arg VERSION=$(VERSION) --target operator -t dispatch-ops-operator:$(IMAGE_TAG) .
	$(KIND) load docker-image --name $(KIND_CLUSTER) dispatch-ops:$(IMAGE_TAG) dispatch-ops-operator:$(IMAGE_TAG)
	helm upgrade --install dispatch-ops-operator $(CHART) --kube-context $(KUBE_CONTEXT) \
		--namespace dispatch-ops-system --create-namespace \
		--set image.tag=$(IMAGE_TAG) --set serverImage.tag=$(IMAGE_TAG) --set image.pullPolicy=Never
	kubectl --context $(KUBE_CONTEXT) -n dispatch-ops-system rollout restart deployment/dispatch-ops-operator
	kubectl --context $(KUBE_CONTEXT) -n dispatch-ops-system rollout status deployment/dispatch-ops-operator --timeout=3m

.PHONY: kind-install
kind-install: ## Create the dev DispatchOps on the kind cluster (after kind-operator and kind-load)
	@kubectl --context $(KUBE_CONTEXT) get namespace $(DEV_NAMESPACE) >/dev/null 2>&1 \
		|| kubectl --context $(KUBE_CONTEXT) create namespace $(DEV_NAMESPACE)
	kubectl --context $(KUBE_CONTEXT) apply -f deploy/dev/dispatchops.yaml
	kubectl --context $(KUBE_CONTEXT) -n $(DEV_NAMESPACE) wait dispatchops/dispatch-ops --for=condition=Ready --timeout=5m
	@echo "admin password: $$(kubectl --context $(KUBE_CONTEXT) -n $(DEV_NAMESPACE) get secret dispatch-ops-admin-password -o jsonpath='{.data.password}' | base64 -d)"
	@echo "UI: kubectl --context $(KUBE_CONTEXT) -n $(DEV_NAMESPACE) port-forward svc/dispatch-ops-api 8090:8080, then http://localhost:8090"

.PHONY: kind-uninstall
kind-uninstall: ## Delete the dev DispatchOps (its database volume and secrets are kept)
	kubectl --context $(KUBE_CONTEXT) -n $(DEV_NAMESPACE) delete dispatchops/dispatch-ops --ignore-not-found

.PHONY: runner
runner: ## Build static dispatch-runner binaries (linux amd64/arm64) for custom EE images
	for arch in amd64 arm64; do \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" \
			-o bin/dispatch-runner-linux-$$arch ./cmd/dispatch-runner || exit 1; \
	done

# --- Build & test ------------------------------------------------------------

.PHONY: web
web: ## Build the UI into web/build (embedded by the Go build)
	cd web && npm run build

.PHONY: build
build: web ## Build the UI and the dispatch-ops binary
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/dispatch-ops

.PHONY: test
test: ## Run Go tests (DB tests are skipped unless DISPATCH_TEST_DATABASE_URL is set)
	go test ./...

.PHONY: test-db
test-db: ## Run Go tests including DB tests, against a dispatch_test database in the dev Postgres
	@docker compose exec -T postgres psql -U dispatch -d dispatch -tAc \
		"SELECT 1 FROM pg_database WHERE datname = 'dispatch_test'" | grep -q 1 \
		|| docker compose exec -T postgres createdb -U dispatch dispatch_test
	DISPATCH_TEST_DATABASE_URL="$(TEST_DATABASE_URL)" go test -count=1 ./...

.PHONY: check
check: ## Vet/format-check Go and type-check/lint the UI
	go vet ./...
	@out="$$(gofmt -l cmd internal web/web.go)"; test -z "$$out" || { echo "gofmt needed:"; echo "$$out"; exit 1; }
	cd web && npm run check && npm run lint

.PHONY: docker
docker: ## Build the server and operator container images
	docker build --build-arg VERSION=$(VERSION) -t dispatch-ops:$(VERSION) .
	docker build --build-arg VERSION=$(VERSION) --target operator -t dispatch-ops-operator:$(VERSION) .

.PHONY: helm-lint
helm-lint: ## Lint the operator's Helm chart
	helm lint $(CHART)
