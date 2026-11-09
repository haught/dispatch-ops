# Developing Dispatch Ops

This guide is for people working on Dispatch Ops itself. For what the product
does and how to run it in production, see [README.md](README.md).

- [Quickstart](#quickstart)
- [The development loop](#the-development-loop)
- [Dev container](#dev-container)
- [Repository layout](#repository-layout)
- [Changing the API](#changing-the-api)
- [Changing the DispatchOps resource](#changing-the-dispatchops-resource)
- [Tests and checks](#tests-and-checks)
- [Execution environments in development](#execution-environments-in-development)
- [Using the API from the command line](#using-the-api-from-the-command-line)
- [Building images and releases](#building-images-and-releases)
- [Make targets](#make-targets)

## Quickstart

Everything runs in a local [kind](https://kind.sigs.k8s.io) cluster: the
operator, the API server, the task manager and the job pods.
[Tilt](https://tilt.dev) rebuilds and redeploys whatever a change affects.
The database is a Postgres container you run yourself, outside the cluster.

### 1. Get the tools

The easiest way is the dev container: open the repository in VS Code and
choose **Dev Containers: Reopen in Container**. It has every tool below and
uses your host's Docker daemon, so Docker must be running on the host.

To work on the host instead, install:

| Tool | Version |
|---|---|
| Go | 1.27+ |
| Node.js | 25+ |
| Docker | Docker Desktop on macOS |
| kubectl, Helm 3, kind, Tilt | recent |
| socat | any (Tilt uses it to forward the API port) |
| make, psql | any |
| ansible-core | optional, for the playbook test in `make test-db` |

### 2. Start Postgres

The Makefile expects a container named `postgresql` with the superuser
`postgres`, publishing port 5432:

```sh
docker run -d --name postgresql --restart unless-stopped \
  -e POSTGRES_PASSWORD=postgres -p 5432:5432 \
  -v dispatch-ops-pg:/var/lib/postgresql/data \
  postgres:17
```

If you already have a Postgres container under another name, pass
`PG_CONTAINER=<name>` (and `PG_SUPERUSER=<user>` if it isn't `postgres`) to
`make db-init` in step 5.

### 3. Create your `.env`

```sh
make setup-env
```

This copies `.env.example` to `.env`. The defaults work with the container
from step 2: the database URL points at `localhost:5432` with the user and
password `dispatch`, and there is a development-only encryption key. Leave
them as they are for now. Inside the dev container the Makefile rewrites
`localhost` to `host.docker.internal` for you.

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

This creates the `dispatch-ops` kind cluster (first run only) and runs
`tilt up`. The first run builds the UI, the server, the operator and the
execution environment images, so give it several minutes.

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
https://localhost:8443 and accept the browser's warning about the
self-signed certificate (the operator generated it; you only see the warning
once). Sign in as `admin` with that password.

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
- A Secret with `DISPATCH_DATABASE_URL`, with `localhost` changed to
  `host.docker.internal`, where pods reach your Postgres.
- A Secret with `DISPATCH_ENCRYPTION_KEY` (and `DISPATCH_OLD_ENCRYPTION_KEYS`
  if set).
- `spec.externalURL`, `spec.sessionTTL` and, when `DISPATCH_OIDC_ISSUER` is
  set, `spec.oidc` with a Secret for the client secret.

Edit the `DispatchOps` (or `.env`), not the Deployments: the operator reverts
changes to the objects it created.

Keep `DISPATCH_ENCRYPTION_KEY` for as long as you keep the database. If it
changes, stored credentials can no longer be decrypted.

### Single sign-on in development

Set `DISPATCH_OIDC_ISSUER`, `DISPATCH_OIDC_CLIENT_ID` and
`DISPATCH_OIDC_CLIENT_SECRET` in `.env`, and register
`https://localhost:8443/api/v1/auth/oidc/callback` as the client's redirect
URI. Tilt re-applies the installation when `.env` changes. See
[Single sign-on](README.md#single-sign-on-oidc) for how users and groups are
mapped.

### Running the binary on the host

The CLI targets run on the host against `DISPATCH_DATABASE_URL`:

```sh
make migrate        # apply pending migrations
make migrate-down   # roll back the latest migration
make admin          # create a local "admin" system administrator
make build          # build the UI and bin/dispatch-ops
```

You rarely need `make migrate` during normal work, because the operator runs
migrations on every server image change. `make migrate-down` is useful while
iterating on a new migration.

## Dev container

`.devcontainer/` has everything the quickstart needs: Go, Node, the Docker
CLI, kubectl, Helm, kind, Tilt, psql and ansible-core. It uses the host's
Docker daemon through its socket and starts nothing by itself; use the same
`make` targets as on the host.

Inside it:

- The Makefile points `DISPATCH_DATABASE_URL` at `host.docker.internal`
  instead of `localhost` (it checks `DISPATCH_DEVCONTAINER`).
- `make kind-up` also joins the container to the `kind` Docker network and
  writes a kubeconfig with the cluster's internal address. After rebuilding
  the container, run `make kind-join` to do that again for an existing
  cluster. The `Tiltfile` refuses to start if the kubectl context isn't
  `kind-dispatch-ops`, and tells you to run it.
- Ports 8443 (Dispatch Ops), 10350 (Tilt) and 5173 (Vite) are forwarded to
  the host.

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
- `make test-db` also runs a real playbook if `ansible-playbook` is on your
  `PATH` (the dev container has it; on the host, `pip install ansible-core`
  in a virtualenv). Otherwise that test is skipped.

## Execution environments in development

`make images` builds `runner-base`, `runner` and the starter EEs as
`dispatch-ops/<name>:dev`. `make kind-load` builds them and loads them into
the kind cluster; Tilt runs it whenever the runner or `images/` changes.

To work on your own EE against the runner in this checkout, put it (or a
symlink to its repository) in `images/local/<name>/`, which is gitignored.
`make kind-load` then builds it on top of the local starter EEs and loads it
as `dispatch-ops/local-<name>:dev`. See
[images/local/README.md](images/local/README.md).

Check any image with:

```sh
docker run --rm <image> selftest
```

It verifies that the image runs as a non-root user, has git and ssh, and, for
Ansible images, the Ansible tools. It lists the collections, prints the runner
protocol, and exits non-zero when the image can't run jobs.

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

```sh
make docker            # server (dispatch-ops:<version>) and operator (dispatch-ops-operator:<version>)
make runner            # static dispatch-runner binaries for linux amd64 and arm64, in bin/
make publish-images IMAGE_REPO=ghcr.io/<org>/dispatch-ops PUBLISH_TAGS="1.4.0 1.4 latest"
```

`make publish-images` builds and pushes `runner`, `runner-base` and the
starter EEs for amd64 and arm64. The version comes from `git describe`.

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
| `setup-env` | Create `.env` from `.env.example` |
| `deps` | Go modules and npm packages |
| `db-init` | Create the role and the `dispatch` and `dispatch_test` databases |
| `dev` / `dev-down` | Start the kind cluster and Tilt / remove what Tilt deployed |
| `kind-up` / `kind-down` / `kind-join` | Create or delete the cluster; rejoin it from a rebuilt dev container |
| `admin-password` | Print the generated `admin` password |
| `web-dev` | Vite dev server with hot reload |
| `generate` / `manifests` | Regenerate code from the OpenAPI spec / the CRD |
| `test` / `test-db` / `check` / `helm-lint` | Tests and linters |
| `images` / `kind-load` | Build the EE images / and load them into kind |
| `migrate` / `migrate-down` / `admin` | Database and user commands on the host |
| `build` / `docker` / `runner` / `publish-images` | Binaries and images |
