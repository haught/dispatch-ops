# Dispatch Ops development tasks. Run `make help` for a list.

-include .env
export

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
BIN := bin/dispatch-ops
TEST_DATABASE_URL := postgres://dispatch:dispatch@localhost:5433/dispatch_test?sslmode=disable

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

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

.PHONY: web-dev
web-dev: ## Run the Vite dev server with hot reload (proxies /api to :8080)
	cd web && npm run dev

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
