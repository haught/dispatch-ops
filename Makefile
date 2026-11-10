# Dispatch Ops development tasks. Run `make help` for a list.

-include .env
# .env's variables reach the environment only of the targets that run
# dispatch-ops itself (DOTENV_TARGETS), not every recipe: npm, docker and
# go test have no business with the encryption key or the database
# password. Recipes that need one otherwise take it as a make variable.
# That holds only for plain NAME=value lines: make reads .env as a
# makefile, so a line written `export NAME=value` exports NAME to every
# recipe. Leave `export` out of .env.
DOTENV_VARS := $(shell sed -nE 's/^[[:space:]]*([A-Za-z_][A-Za-z0-9_]*)[[:space:]]*=.*/\1/p' .env 2>/dev/null)
DOTENV_TARGETS := migrate migrate-down admin
$(foreach v,$(DOTENV_VARS),$(eval $(DOTENV_TARGETS): export $(v) := $$($(v))))
# The Vite dev server's proxy target, if .env sets it.
ifdef DISPATCH_API_URL
web-dev: export DISPATCH_API_URL := $(DISPATCH_API_URL)
endif

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
BIN := bin/dispatch-ops
# The test database sits next to DISPATCH_DATABASE_URL's, with a _test suffix.
TEST_DATABASE_URL := $(subst /dispatch?,/dispatch_test?,$(DISPATCH_DATABASE_URL))
# The Postgres container that `make db-init` sets up, and its superuser.
PG_CONTAINER ?= postgresql
PG_SUPERUSER ?= postgres

# Execution environment images are $(IMAGE_REPO)/<name>:$(IMAGE_TAG). Local
# dev keeps dispatch-ops/<name>:dev, the names EEs are registered by.
IMAGE_REPO ?= dispatch-ops
IMAGE_TAG ?= dev
KIND := kind
CONTROLLER_GEN := go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.23.0
CHART := deploy/helm/dispatch-ops-operator
KIND_CLUSTER := dispatch-ops
KUBE_CONTEXT := kind-$(KIND_CLUSTER)
DISPATCH_JOB_NAMESPACE ?= dispatch-ops-jobs
DEV_NAMESPACE := dispatch-ops

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: setup-env
setup-env: ## Create .env from .env.example, with a new encryption key
	@test -f .env && echo ".env already exists" || \
		{ key=$$(go run ./cmd/dispatch-ops gen-key) && \
		  sed "s|^DISPATCH_ENCRYPTION_KEY=.*|DISPATCH_ENCRYPTION_KEY=$$key|" .env.example > .env && \
		  echo "created .env with a new DISPATCH_ENCRYPTION_KEY"; }

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
manifests: ## Regenerate the DispatchOps CRD (the operator's embedded copy and the chart's crds/) and its deepcopy code
	$(CONTROLLER_GEN) object paths=./internal/operator/api/... \
		crd paths=./internal/operator/api/... output:crd:dir=internal/operator/crd
	cp internal/operator/crd/dispatch-ops.io_dispatchops.yaml $(CHART)/crds/

# --- Database ----------------------------------------------------------------

# The database runs outside the cluster, in your own Postgres container
# ($(PG_CONTAINER)) on the host's Docker; DISPATCH_DATABASE_URL in .env says
# how to reach it (host.docker.internal, from the dev container and kind).
.PHONY: db-init
db-init: ## Create DISPATCH_DATABASE_URL's role, database and _test database in your Postgres container (PG_CONTAINER)
	@url='$(DISPATCH_DATABASE_URL)'; \
	user=$$(echo "$$url" | sed -E 's#^[a-z]+://([^:@/]+).*#\1#'); \
	pass=$$(echo "$$url" | sed -nE 's#^[a-z]+://[^:@/]+:([^@]*)@.*#\1#p'); \
	db=$$(echo "$$url" | sed -E 's#^[a-z]+://[^/]*/([^?]*).*#\1#'); \
	printf '%s\n' \
		"SELECT format('CREATE ROLE %I LOGIN PASSWORD %L', :'user', :'pass') WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = :'user') \gexec" \
		"SELECT format('ALTER ROLE %I CREATEDB', :'user') \gexec" \
		"SELECT format('CREATE DATABASE %I OWNER %I', d, :'user') FROM unnest(ARRAY[:'db', :'db' || '_test']) AS d WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = d) \gexec" \
	| docker exec -i $(PG_CONTAINER) psql -q -v ON_ERROR_STOP=1 -U $(PG_SUPERUSER) -d postgres \
		-v user="$$user" -v pass="$$pass" -v db="$$db" \
	&& echo "$(PG_CONTAINER): role $$user can create databases; $$db and $${db}_test exist"

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

.PHONY: dev
dev: kind-up ## Run everything in the kind cluster with Tilt, rebuilding on changes (Tilt UI: localhost:10350)
	tilt up

.PHONY: dev-down
dev-down: ## Remove what `make dev` deployed (the cluster, namespaces and database are kept)
	tilt down

.PHONY: admin-password
admin-password: ## Print the password the operator generated for "admin" (used only if the database had no admin)
	@kubectl --context $(KUBE_CONTEXT) -n $(DEV_NAMESPACE) get secret dispatch-ops-admin-password -o jsonpath='{.data.password}' | base64 -d; echo

.PHONY: web-dev
web-dev: ## Run the Vite dev server with hot reload (proxies /api to the in-cluster API on :8443)
	cd web && npm run dev

# --- Dev cluster and execution environments ----------------------------------

.PHONY: kind-up
kind-up: ## Create the kind cluster and the job namespace, and connect the dev container to it
	@$(KIND) get clusters 2>/dev/null | grep -qx $(KIND_CLUSTER) || $(KIND) create cluster --config deploy/dev/kind.yaml
	@$(MAKE) --no-print-directory kind-join
	@kubectl --context $(KUBE_CONTEXT) get namespace $(DISPATCH_JOB_NAMESPACE) >/dev/null 2>&1 \
		|| kubectl --context $(KUBE_CONTEXT) create namespace $(DISPATCH_JOB_NAMESPACE)
	@# The operator runs jobs only where Pod Security is enforced.
	@kubectl --context $(KUBE_CONTEXT) label --overwrite namespace $(DISPATCH_JOB_NAMESPACE) \
		pod-security.kubernetes.io/enforce=restricted >/dev/null

.PHONY: kind-join
kind-join: ## Join the kind network and point kubectl at the cluster (after rebuilding the dev container)
	@docker inspect -f '{{range $$k, $$v := .NetworkSettings.Networks}}{{$$k}} {{end}}' $$(hostname) | grep -qw kind \
		|| docker network connect kind $$(hostname)
	@$(KIND) export kubeconfig --name $(KIND_CLUSTER) --internal

.PHONY: kind-down
kind-down: ## Delete the kind cluster
	$(KIND) delete cluster --name $(KIND_CLUSTER)

EES := ee-shell ee-python ee-ansible
# Your own EEs: each images/local/<name>/ with a Dockerfile (a checkout of
# your EE repo, cloned there) builds as $(IMAGE_REPO)/local-<name>,
# on top of the starter EEs built here. images/local is gitignored; see
# images/local/README.md.
LOCAL_EES := $(notdir $(patsubst %/Dockerfile,%,$(wildcard images/local/*/Dockerfile)))
# What `make publish-images` pushes: each tag, for each platform.
PUBLISH_TAGS ?= $(VERSION)
PLATFORMS ?= linux/amd64,linux/arm64
comma := ,
empty :=
space := $(empty) $(empty)

.PHONY: images
images: ## Build the runner images, the starter EEs and any EEs in images/local
	docker build -f images/runner-base/Dockerfile --build-arg VERSION=$(VERSION) --target runner -t $(IMAGE_REPO)/runner:$(IMAGE_TAG) .
	docker build -f images/runner-base/Dockerfile --build-arg VERSION=$(VERSION) -t $(IMAGE_REPO)/runner-base:$(IMAGE_TAG) .
	for ee in $(EES); do \
		docker build --build-arg RUNNER_BASE=$(IMAGE_REPO)/runner-base:$(IMAGE_TAG) -t $(IMAGE_REPO)/$$ee:$(IMAGE_TAG) images/$$ee || exit 1; \
	done
	for ee in $(LOCAL_EES); do \
		docker build --build-arg IMAGE_REPO=$(IMAGE_REPO) --build-arg IMAGE_TAG=$(IMAGE_TAG) \
			-t $(IMAGE_REPO)/local-$$ee:$(IMAGE_TAG) images/local/$$ee/ || exit 1; \
	done

.PHONY: kind-load
kind-load: images ## Build the images and load them into the kind cluster
	$(KIND) load docker-image --name $(KIND_CLUSTER) $(IMAGE_REPO)/runner-base:$(IMAGE_TAG) \
		$(foreach ee,$(EES),$(IMAGE_REPO)/$(ee):$(IMAGE_TAG)) \
		$(foreach ee,$(LOCAL_EES),$(IMAGE_REPO)/local-$(ee):$(IMAGE_TAG))

.PHONY: publish-images
publish-images: ## Push every image (docker-bake.hcl), multi-platform (IMAGE_REPO=ghcr.io/<org>/dispatch-ops PUBLISH_TAGS="1.4.0 1.4 latest")
	@if [ "$(IMAGE_REPO)" = dispatch-ops ]; then \
		echo "Set IMAGE_REPO to the registry to push to, e.g. IMAGE_REPO=ghcr.io/<org>/dispatch-ops"; exit 1; fi
	REGISTRY=$(IMAGE_REPO) TAGS="$(subst $(space),$(comma),$(strip $(PUBLISH_TAGS)))" \
		VERSION=$(VERSION) REVISION=$$(git rev-parse HEAD) PLATFORMS=$(PLATFORMS) \
		docker buildx bake --push

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
test-db: ## Run Go tests including DB tests, against dispatch_test* databases (see db-init)
	@echo 'DISPATCH_TEST_DATABASE_URL=<DISPATCH_DATABASE_URL, _test database> go test -count=1 ./...'
	@DISPATCH_TEST_DATABASE_URL='$(TEST_DATABASE_URL)' go test -count=1 ./...

.PHONY: check
check: ## Vet/format-check Go and type-check/lint the UI
	go vet ./...
	@out="$$(gofmt -l cmd internal web/web.go)"; test -z "$$out" || { echo "gofmt needed:"; echo "$$out"; exit 1; }
	cd web && npm run check && npm run lint

.PHONY: docker
docker: ## Build the server and operator container images
	docker build --build-arg VERSION=$(VERSION) -t dispatch-ops:$(VERSION) .
	docker build --build-arg VERSION=$(VERSION) --target operator -t dispatch-ops-operator:$(VERSION) .

# helm lint and template run in this namespace, as the README installs the chart.
CHART_NAMESPACE := dispatch-ops-system

.PHONY: helm-lint
helm-lint: ## Lint the operator's Helm chart, and check its CRD matches the operator's
	@cmp -s internal/operator/crd/dispatch-ops.io_dispatchops.yaml $(CHART)/crds/dispatch-ops.io_dispatchops.yaml \
		|| { echo 'the chart'"'"'s crds/ differ from the operator'"'"'s CRD: run make manifests'; exit 1; }
	helm lint $(CHART) -n $(CHART_NAMESPACE)
	helm lint $(CHART) -n $(CHART_NAMESPACE) -f $(CHART)/ci/installation-values.yaml
	helm lint $(CHART) -n $(CHART_NAMESPACE) -f $(CHART)/ci/namespaced-values.yaml
	helm lint $(CHART) -n $(CHART_NAMESPACE) --set watchAllNamespaces=true
	helm lint $(CHART) -n $(CHART_NAMESPACE) --set jobNamespaceSuffix= --set namespaces.create=false --set installation.enabled=false
	@# Cluster-wide mode only by name, never by an empty list.
	@! helm template $(CHART) -n $(CHART_NAMESPACE) --set 'watchNamespaces=null' >/dev/null 2>&1 \
		|| { echo 'the chart rendered an empty watchNamespaces'; exit 1; }
	@# A watched namespace must never be a job namespace too: the chart refuses.
	@! helm template $(CHART) -n $(CHART_NAMESPACE) --set 'watchNamespaces={team-a,jobs-a}' --set 'allowedJobNamespaces.jobs-a={team-a}' \
		>/dev/null 2>&1 || { echo 'the chart rendered a job namespace that is also watched'; exit 1; }
	@! helm template $(CHART) -n $(CHART_NAMESPACE) --set 'watchNamespaces={a,a-jobs}' >/dev/null 2>&1 \
		|| { echo 'the chart rendered a default job namespace that is also watched'; exit 1; }
	@! helm template $(CHART) -n $(CHART_NAMESPACE) --set 'allowedJobNamespaces={jobs-a}' >/dev/null 2>&1 \
		|| { echo 'the chart rendered allowedJobNamespaces without the namespaces allowed to use them'; exit 1; }
	@# The operator's namespace must host no installation or jobs.
	@! helm template $(CHART) -n dispatch-ops >/dev/null 2>&1 \
		|| { echo 'the chart rendered the operator'"'"'s namespace as a watched namespace'; exit 1; }

# --- Release -----------------------------------------------------------------

# The chart version for `make chart`: the git version without its leading v.
CHART_VERSION ?= $(patsubst v%,%,$(VERSION))

.PHONY: chart
chart: ## Package the operator's Helm chart into dist/ as CHART_VERSION (also its appVersion: the image tag)
	helm package $(CHART) --version $(CHART_VERSION) --app-version $(CHART_VERSION) -d dist

.PHONY: dist
dist: chart ## Release assets in dist/: dispatch-runner binaries, the chart and SHA256SUMS
	for arch in amd64 arm64; do \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" \
			-o dist/dispatch-runner-linux-$$arch ./cmd/dispatch-runner || exit 1; \
	done
	cd dist && sha256sum dispatch-runner-linux-* dispatch-ops-operator-$(CHART_VERSION).tgz > SHA256SUMS
