# Developing Dispatch Ops

This guide is for people working on Dispatch Ops itself. For what the product
does and how to install it, see [README.md](README.md); for running and using
it, see [docs/](docs/).

Development happens in the repository's dev container, which has every tool
the project needs. On your machine you only need:

- Docker Desktop. Pods in the dev cluster reach your database at
  `host.docker.internal`, which Docker Desktop provides.
- VS Code with the Dev Containers extension (or the
  [`devcontainer` CLI](https://github.com/devcontainers/cli)).

Every command in this guide runs in the dev container's terminal.

- [Quickstart](#quickstart)
- [The development loop](#the-development-loop)
- [How the dev container works](#how-the-dev-container-works)
- [Repository layout](#repository-layout)
- [Changing the API](#changing-the-api)
- [Changing the DispatchOps resource](#changing-the-dispatchops-resource)
- [Tests and checks](#tests-and-checks)
- [Execution environments in development](#execution-environments-in-development)
- [Using the API from the command line](#using-the-api-from-the-command-line)
- [Building images and releases](#building-images-and-releases)
- [Make targets](#make-targets)
- [Documentation](#documentation)

## Quickstart

Everything runs in a local [kind](https://kind.sigs.k8s.io) cluster: the
operator, the API server, the task manager and the job pods.
[Tilt](https://tilt.dev) rebuilds and redeploys whatever a change affects.
The database is a Postgres container you run yourself, outside the cluster.

### 1. Open the dev container

Open the repository in VS Code and choose **Dev Containers: Reopen in
Container**. The first build takes a few minutes. Docker must be running on
your machine: the dev container uses its Docker daemon to run Postgres, the
kind cluster and image builds.

### 2. Start Postgres

The Makefile expects a container named `postgresql` with the superuser
`postgres`, publishing port 5432:

```sh
docker run -d --name postgresql --restart unless-stopped \
  -e POSTGRES_PASSWORD=postgres -p 5432:5432 \
  -v dispatch-ops-pg:/var/lib/postgresql/data \
  postgres:17
```

It runs on your machine's Docker, next to the dev container, so it survives
rebuilding the dev container. The dev container and the kind cluster both
reach it at `host.docker.internal:5432`.

If you already have a Postgres container under another name, pass
`PG_CONTAINER=<name>` (and `PG_SUPERUSER=<user>` if it isn't `postgres`) to
`make db-init` in step 5.

### 3. Create your `.env`

```sh
make setup-env
```

This copies `.env.example` to `.env` and fills in a new random encryption
key. The defaults work with the container from step 2: the database URL
points at `host.docker.internal:5432` with the user and password `dispatch`.
Leave them as they are for now.

### 4. Install dependencies

```sh
make deps
```

Downloads Go modules and runs `npm ci` in `web/`.

### 5. Create the databases

```sh
make db-init
```

Creates the `dispatch` role and the `dispatch` and `dispatch_test` databases
in the `postgresql` container. Run it once; it is safe to run again.

### 6. Start the cluster and the app

```sh
make dev
```

This creates the `dispatch-ops` kind cluster (first run only), connects the
dev container to it, and runs `tilt up`. The first run builds the UI, the
server, the operator and the execution environment images, so give it
several minutes.

Open the Tilt UI at http://localhost:10350 and wait until every resource is
green. `api-port-forward` comes up last.

`tilt up` stays in the foreground. Pressing Ctrl+C stops Tilt but leaves the
app running in the cluster; run `make dev` again to pick up where you left
off.

### 7. Sign in

```sh
make admin-password
```

prints the password the operator generated for the user `admin`. Open
https://localhost:8443 (VS Code forwards the port from the dev container) and
accept the browser's warning about the self-signed certificate. Sign in as
`admin` with that password.

`make admin-password` reads a Secret in the cluster. The admin user is only
created when the database has no user called `admin`, so if you recreated the
cluster but kept the database, the old password still applies.

### 8. Run your first job

Tilt has already built the starter execution environments and loaded them
into the cluster as `dispatch-ops/ee-shell:dev`, `dispatch-ops/ee-python:dev`
and `dispatch-ops/ee-ansible:dev`.

1. Go to **Execution environments** and add one named `ee-shell` with the
   image `dispatch-ops/ee-shell:dev`.
2. Go to **Job templates**, then **New job template**. Choose the job type
   `container`, the `ee-shell` execution environment, and a command such as
   `bash -c 'echo hello from $(hostname)'`.
3. Launch the template. The job page streams the output as the pod runs.

You now have a working environment. Edit code and Tilt redeploys it (see
below).

### 9. Stop

```sh
make dev-down    # tilt down: removes what Tilt deployed, keeps the cluster
make kind-down   # deletes the kind cluster
```

Neither touches the database. To start over with an empty database, drop and
recreate it, or remove the `postgresql` container and its volume and repeat
steps 2 and 5.

## The development loop

Tilt watches the tree and rebuilds only what a change affects:

| You change | Tilt runs | Then |
|---|---|---|
| `web/` | `npm run build`, then the Go build (the UI is embedded) | as below |
| `cmd/dispatch-ops`, `internal/` | Go build, server image | the operator migrates the database, then rolls the API server and task manager |
| `cmd/dispatch-operator`, `internal/operator`, the chart | Go build, operator image | the operator rolls |
| `cmd/dispatch-runner`, `internal/runner`, `images/` | `make kind-load` | the next job uses the new EE images |
| `deploy/dev/dispatchops.yaml`, `.env` | nothing to build | re-applies the `DispatchOps` |

A server change takes about 15 to 30 seconds to go live. The Tilt UI shows
build status and logs for every part.

### Fast UI work

For UI changes, run this next to `make dev`:

```sh
make web-dev
```

and open http://localhost:5173. Vite hot-reloads the UI and proxies `/api` to
the API server in the cluster.

### How the dev installation is configured

The operator runs the `DispatchOps` in `deploy/dev/dispatchops.yaml`: one API
replica, one task manager running jobs in the `dispatch-jobs` namespace, and
the external database from `.env`. The `Tiltfile` fills in the rest before
applying it:

- `spec.image`: the server image Tilt just built.
- A Secret with `DISPATCH_DATABASE_URL`.
- A Secret with `DISPATCH_ENCRYPTION_KEY` (and `DISPATCH_OLD_ENCRYPTION_KEYS`
  if set).
- `spec.externalURL`, `spec.sessionTTL` and, when `DISPATCH_OIDC_ISSUER` is
  set, `spec.oidc` with a Secret for the client secret.

Edit the `DispatchOps` (or `.env`), not the Deployments: the operator reverts
changes to the objects it created.

Keep `DISPATCH_ENCRYPTION_KEY` for as long as you keep the database. If it
changes, stored credentials can no longer be decrypted. To change it, move
the old key to `DISPATCH_OLD_ENCRYPTION_KEYS` and set a new one.
`go run ./cmd/dispatch-ops gen-key` prints a new key.

### Single sign-on in development

Set `DISPATCH_OIDC_ISSUER`, `DISPATCH_OIDC_CLIENT_ID` and
`DISPATCH_OIDC_CLIENT_SECRET` in `.env`, and register
`https://localhost:8443/api/v1/auth/oidc/callback` as the client's redirect
URI. Tilt re-applies the installation when `.env` changes. See
[Single sign-on](docs/user-guide.md#single-sign-on-oidc) for how users and
groups are mapped.

### Running the CLI directly

These targets run the server binary in the dev container against
`DISPATCH_DATABASE_URL`:

```sh
make migrate        # apply pending migrations
make migrate-down   # roll back the latest migration
make admin          # create a local "admin" system administrator
make build          # build the UI and bin/dispatch-ops
```

You rarely need `make migrate` during normal work, because the operator runs
migrations on every server image change. `make migrate-down` is useful while
iterating on a new migration.

## How the dev container works

[.devcontainer/](.devcontainer/) has Go, Node, the Docker CLI, kubectl, Helm,
kind, Tilt, psql, socat and ansible-core. It starts nothing by itself.

- **Docker** is your machine's daemon, through its socket (`DOCKER_HOST`).
  Containers you start, including Postgres and the kind cluster's node, run
  next to the dev container, not inside it.
- **`host.docker.internal`** reaches ports those containers publish on your
  machine. The dev container gets the name from `--add-host`; Docker Desktop
  provides it to kind's pods.
- **The kind cluster:** `make kind-up` connects the dev container to the
  `kind` Docker network and writes a kubeconfig with the cluster's internal
  address. A rebuilt dev container loses that connection: run
  `make kind-join` (or `make kind-up`, which does it too). The `Tiltfile`
  refuses to start if the kubectl context isn't `kind-dispatch-ops`.
- **Ports** 8443 (Dispatch Ops), 10350 (Tilt) and 5173 (Vite) are forwarded
  to your machine.

### Personal settings

To add mounts, features or extensions for yourself, copy
`.devcontainer/devcontainer.json` to `.devcontainer/local/devcontainer.json`,
which is gitignored, and change its build to
`"build": { "dockerfile": "../Dockerfile" }`. **Dev Containers: Reopen in
Container** then offers both configurations. For example, mount a checkout
of your own execution environment from elsewhere on your machine:

```jsonc
"mounts": [
  "source=${localEnv:HOME}/src/my-ee,target=${containerWorkspaceFolder}/images/local/my-ee,type=bind"
]
```

## Repository layout

| Path | What it is |
|---|---|
| `api/openapi.yaml` | The API contract. The Go server and the TypeScript client are generated from it. |
| `cmd/dispatch-ops` | The server binary: `serve`, `task-manager`, `migrate`, `user create`, `user grant-admin`, `gen-key` |
| `cmd/dispatch-runner` | The runner: the entrypoint of every execution environment image |
| `cmd/dispatch-operator` | The Kubernetes operator |
| `internal/api` | HTTP handlers (one file per resource) and the request pipeline (`router.go`) |
| `internal/api/apigen` | Generated server code. Do not edit by hand. |
| `internal/auth` | Password hashing, tokens, the request principal |
| `internal/rbac` | Roles, and which roles allow what |
| `internal/oidc` | Single sign-on with an OIDC provider |
| `internal/credentials` | Credential kinds, input validation, encryption of secret inputs |
| `internal/secrets` | AES-256-GCM keyring |
| `internal/store` | PostgreSQL queries, the job queue, `LISTEN/NOTIFY` |
| `internal/jobstream` | Wakes live job streams (SSE) when a job changes |
| `internal/taskmanager` | Runs queued jobs as Kubernetes Jobs and watches over them |
| `internal/runner` | Runner logic; `runnerapi` is its generated API client, `callback` the Ansible callback plugin it embeds |
| `internal/db` | Connection pool and embedded SQL migrations |
| `internal/server` | Top-level HTTP handler: API, runner API, health probes, embedded UI |
| `internal/operator` | The operator's reconciler and the objects it creates; `api/v1alpha1` is the `DispatchOps` resource |
| `internal/testdb` | Per-package test databases |
| `images/` | `runner-base` and the starter execution environments `ee-shell`, `ee-python` and `ee-ansible`; your own EEs go in the gitignored `images/local/` |
| `examples/ee-template` | Starting point for a repository holding a custom execution environment |
| `deploy/dev/` | kind cluster config, the dev `DispatchOps`, and the Dockerfiles Tilt builds from |
| `deploy/helm/dispatch-ops-operator` | Helm chart that installs the operator and the CRD (`files/` is generated) |
| `deploy/examples/` | A production-style `DispatchOps` |
| `docs/` | Administration guide, user guide and configuration reference |
| `web/` | SvelteKit / Svelte 5 single-page app, embedded into the server binary |
| `Tiltfile` | The development loop |

## Changing the API

1. Edit `api/openapi.yaml`.
2. Run `make generate` to regenerate `internal/api/apigen` and
   `web/src/lib/api/schema.d.ts`.
3. Implement the new methods on `api.Handlers`. The build fails until every
   operation is implemented.

Things the pipeline does for you, and things you must do yourself:

- Requests are validated against the spec before they reach a handler.
- An operation's `security` block decides whether it needs a signed-in
  caller. `security: []` makes it public.
- What the caller may do is checked by the handler, with the helpers in
  `internal/api/access.go`.
- Record changes with `h.audit` (`internal/api/audit.go`). Never put secrets
  in audit entries.

## Changing the DispatchOps resource

Edit `internal/operator/api/v1alpha1/types.go` (kubebuilder markers set
validation and defaults) and run `make manifests`. It regenerates the
deepcopy code and the CRD in the Helm chart's `files/`. `make generate` does
it too.

## Tests and checks

```sh
make test      # unit tests; database tests are skipped
make test-db   # all tests, against dispatch_test* databases in your Postgres
make check     # go vet, gofmt, svelte-check, prettier
make helm-lint # lint the operator chart
```

- `make test-db` sets `DISPATCH_TEST_DATABASE_URL` from your database URL
  with a `_test` suffix (`make db-init` created that database and allowed the
  role to create more).
- Each package with database tests gets its own database (see
  `internal/testdb`), because `go test` runs packages in parallel.
- The runner is tested end to end against the real API in
  `internal/api/jobs_test.go`, and the task manager against a fake Kubernetes
  client.
- `make test-db` also runs a real playbook with the dev container's
  `ansible-playbook`.

## Execution environments in development

`make images` builds `runner-base`, `runner` and the starter EEs as
`dispatch-ops/<name>:dev`. `make kind-load` builds them and loads them into
the kind cluster; Tilt runs it whenever the runner or `images/` changes.

To work on your own EE against the runner in this checkout, clone its
repository into `images/local/<name>/`, which is gitignored (or mount it
there, see [Personal settings](#personal-settings)).
`make kind-load` then builds it on top of the local starter EEs and loads it
as `dispatch-ops/local-<name>:dev`. See
[images/local/README.md](images/local/README.md).

Check any image with:

```sh
docker run --rm <image> selftest
```

See [Execution environments](docs/user-guide.md#execution-environments) for
what an image must provide.

### Runner protocol

The runner reports a protocol version and the API rejects runners it doesn't
support, with an explanation on the job. Ansible and script jobs need
protocol 2 or later, and inventory syncs need protocol 3. Bump the protocol
when the runner needs something from the API that older servers don't have,
or when a new job type needs a newer runner.

## Using the API from the command line

Create an API token on the **API tokens** page, then:

```sh
export TOKEN=dop_...

# A container job
curl -sk https://localhost:8443/api/v1/execution-environments -H "Authorization: Bearer $TOKEN" \
  -d '{"name": "ee-shell", "image": "dispatch-ops/ee-shell:dev"}'
curl -sk https://localhost:8443/api/v1/job-templates -H "Authorization: Bearer $TOKEN" \
  -d '{"name": "hello", "jobType": "container", "executionEnvironmentId": "<ee id>",
       "command": ["bash", "-c", "echo hello from $(hostname)"]}'

# A playbook: register dispatch-ops/ee-ansible:dev and a project first
curl -sk https://localhost:8443/api/v1/projects -H "Authorization: Bearer $TOKEN" \
  -d '{"name": "ops", "scmUrl": "https://github.com/acme/ops.git", "scmRef": "main"}'
curl -sk https://localhost:8443/api/v1/job-templates -H "Authorization: Bearer $TOKEN" \
  -d '{"name": "site", "jobType": "ansible", "executionEnvironmentId": "<ee id>",
       "projectId": "<project id>", "entrypoint": "site.yml", "inventoryId": "<inventory id>",
       "credentialIds": ["<ssh credential id>"], "allowOverrides": ["limit", "tags"]}'

# Launch it
curl -sk -X POST https://localhost:8443/api/v1/job-templates/<template id>/launch \
  -H "Authorization: Bearer $TOKEN"
```

`-k` skips certificate verification for the dev cluster's self-signed
certificate. The full API is in `api/openapi.yaml`.

## Building images and releases

### Cutting a release

Push a version tag:

```sh
git tag v1.4.0
git push origin v1.4.0
```

[.github/workflows/release.yml](.github/workflows/release.yml) then publishes:

| What | Where |
|---|---|
| Images `server`, `operator`, `runner`, `runner-base`, `ee-shell`, `ee-python`, `ee-ansible`, for amd64 and arm64, with provenance and SBOM attestations | `ghcr.io/haught/dispatch-ops/<name>:1.4.0`, `:1.4` and `:latest` |
| The Helm chart, with `version` and `appVersion` set to `1.4.0` | `oci://ghcr.io/haught/dispatch-ops/charts/dispatch-ops-operator`, and the Helm repository `https://haught.github.io/dispatch-ops` (`index.yaml` on the `gh-pages` branch) |
| A GitHub Release with generated notes, the `dispatch-runner` binaries (linux amd64 and arm64), the chart `.tgz` and `SHA256SUMS` | [Releases](https://github.com/haught/dispatch-ops/releases) |

A prerelease tag such as `v1.4.0-rc.1` publishes only the `1.4.0-rc.1` image
tag, leaves `latest` and `1.4` alone, and marks the GitHub Release as a
prerelease. Helm only shows prerelease charts with `--devel`.

Pull requests and pushes to `main` run
[.github/workflows/ci.yml](.github/workflows/ci.yml): Go vet, gofmt and tests
(with a PostgreSQL service, so the DB tests run), the UI's type check, lint
and build, a check that generated code is up to date, the Helm lint, and an
amd64 build of every image with the starter EEs' `selftest`. Nothing is
published from `main`.

The in-tree `Chart.yaml` keeps `version: 0.1.0` and `appVersion: dev`; the
release sets both when it packages the chart.

One-time setup, after the first release:

- GHCR packages owned by a personal account may start out private. On each
  package's settings page (server, operator, runner, runner-base, the EEs and
  `charts/dispatch-ops-operator`), check that it is linked to this repository
  and set its visibility to public.
- In the repository's Settings → Pages, deploy from the `gh-pages` branch
  (root), which the first release creates.

### Building locally

[docker-bake.hcl](docker-bake.hcl) defines every image. The EEs build on the
`runner-base` of the same bake, so nothing has to be pushed first.

```sh
docker buildx bake --load                 # every image as dispatch-ops/<name>:dev
docker buildx bake ees --load             # runner-base and the starter EEs
docker buildx bake --print                # what would be built, with tags
make publish-images IMAGE_REPO=registry.example.com/dispatch-ops PUBLISH_TAGS="1.4.0 1.4 latest"
make dist CHART_VERSION=1.4.0             # the release assets, in dist/
```

`make publish-images` pushes every image for amd64 and arm64 to your own
registry, for mirrors or forks. The version built into the binaries comes
from `git describe`. The Go and UI builds run on the build platform and
cross-compile, so arm64 images don't build under emulation.

`make docker` builds the server and operator alone, as
`dispatch-ops:<version>` and `dispatch-ops-operator:<version>`, and
`make images` the EEs for the dev cluster.

The server image serves health probes at `/healthz` (liveness) and `/readyz`
(checks the database). To try it outside Kubernetes:

```sh
docker run --rm -e DISPATCH_DATABASE_URL=... -e DISPATCH_ENCRYPTION_KEY=... \
  -p 8443:8443 dispatch-ops:<version>
```

## Make targets

`make help` lists every target. The ones you will use most:

| Target | |
|---|---|
| `setup-env` | Create `.env` from `.env.example`, with a new encryption key |
| `deps` | Go modules and npm packages |
| `db-init` | Create the role and the `dispatch` and `dispatch_test` databases |
| `dev` / `dev-down` | Start the kind cluster and Tilt / remove what Tilt deployed |
| `kind-up` / `kind-down` / `kind-join` | Create the cluster and connect to it / delete it / reconnect after a dev container rebuild |
| `admin-password` | Print the generated `admin` password |
| `web-dev` | Vite dev server with hot reload |
| `generate` / `manifests` | Regenerate code from the OpenAPI spec / the CRD |
| `test` / `test-db` / `check` / `helm-lint` | Tests and linters |
| `images` / `kind-load` | Build the EE images / and load them into kind |
| `migrate` / `migrate-down` / `admin` | Database and user commands, run directly |
| `build` / `docker` / `runner` / `publish-images` | Binaries and images |
| `chart` / `dist` | Package the Helm chart / all release assets, in `dist/` |

## Documentation

Keep the documentation describing the product as it is now, for its
audience. It is not a changelog: don't describe what changed or how things
used to work (release notes cover that), and update or remove text a change
makes wrong.

| File | Audience | Holds |
|---|---|---|
| [README.md](README.md) | Anyone evaluating or installing Dispatch Ops | What it is, requirements, quick start, the production checklist. Keep it short and link to `docs/` for detail |
| [docs/administration.md](docs/administration.md) | Cluster and platform administrators | Architecture, security model, TLS, upgrades, backups, monitoring |
| [docs/user-guide.md](docs/user-guide.md) | People using the UI or API | Access control, job types, projects, inventories, execution environments, schedules, notifications |
| [docs/configuration.md](docs/configuration.md) | Anyone running the server without the operator | Environment variables and CLI commands |
| `DEVELOP.md` | Contributors | Development setup, repository layout, tests, releases |

Field-level detail of the `DispatchOps` resource belongs in the kubebuilder
comments in `types.go` (shown by `kubectl explain`), and API detail in
`api/openapi.yaml`.
