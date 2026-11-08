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
KIND_CLUSTER := dispatch-ops
KUBE_CONTEXT := kind-$(KIND_CLUSTER)
DISPATCH_JOB_NAMESPACE ?= dispatch-jobs

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
generate: ## Regenerate Go server and TS client from api/openapi.yaml
	go generate ./internal/api/apigen/
	cd web && npm run gen:api

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
	@kubectl --context $(KUBE_CONTEXT) get namespace $(DISPATCH_JOB_NAMESPACE) >/dev/null 2>&1 \
		|| kubectl --context $(KUBE_CONTEXT) create namespace $(DISPATCH_JOB_NAMESPACE)

.PHONY: kind-down
kind-down: ## Delete the kind cluster
	$(KIND) delete cluster --name $(KIND_CLUSTER)

.PHONY: images
images: ## Build the runner-base and ee-shell images
	docker build -f images/runner-base/Dockerfile --build-arg VERSION=$(VERSION) -t dispatch-ops/runner-base:$(IMAGE_TAG) .
	docker build --build-arg RUNNER_BASE=dispatch-ops/runner-base:$(IMAGE_TAG) -t dispatch-ops/ee-shell:$(IMAGE_TAG) images/ee-shell

.PHONY: kind-load
kind-load: images ## Build the images and load them into the kind cluster
	$(KIND) load docker-image --name $(KIND_CLUSTER) dispatch-ops/runner-base:$(IMAGE_TAG) dispatch-ops/ee-shell:$(IMAGE_TAG)

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
docker: ## Build the container image
	docker build --build-arg VERSION=$(VERSION) -t dispatch-ops:$(VERSION) .
