# Dispatch Ops

A Kubernetes-native automation platform for running Ansible playbooks, scripts
and container commands as jobs. See [PLAN.md](PLAN.md) for the design and
roadmap.

**Status:** milestone 2 (run something). Container jobs run on Kubernetes
with live logs. Inventories, credentials, execution environments and job
templates are managed through the API; the UI lists jobs and templates,
launches templates and streams job output.

## Layout

| Path | What it is |
|---|---|
| `api/openapi.yaml` | The API contract. The Go server and the TS client are generated from it. |
| `cmd/dispatch-ops` | The binary: `serve`, `task-manager`, `migrate`, `user create`, `gen-key` |
| `cmd/dispatch-runner` | The runner: the entrypoint of every execution environment image |
| `internal/api` | HTTP handlers (one file per resource) and request pipeline (`router.go`) |
| `internal/api/apigen` | Generated server code. Do not edit by hand. |
| `internal/auth` | Password hashing, tokens, request principal |
| `internal/credentials` | Credential kinds, input validation, encryption of secret inputs |
| `internal/secrets` | AES-256-GCM keyring |
| `internal/store` | PostgreSQL queries, the job queue, `LISTEN/NOTIFY` |
| `internal/jobstream` | Wakes live job streams (SSE) when a job changes |
| `internal/taskmanager` | Runs queued jobs as Kubernetes Jobs and watches over them |
| `internal/runner` | Runner logic; `runnerapi` is its generated API client |
| `internal/db` | Connection pool and embedded SQL migrations |
| `internal/server` | Top-level HTTP handler: API, runner API, health probes, embedded UI |
| `images/` | `runner-base` and the starter `ee-shell` execution environment |
| `deploy/dev/` | kind cluster config for development |
| `web/` | SvelteKit 3 / Svelte 5 single-page app, embedded into the binary |

## Development

Requires Go 1.27+, Node 25+, Docker (Docker Desktop on macOS) and kubectl.
kind is run through `go run`, so it doesn't need installing.

```sh
make setup-env   # create .env from .env.example
make deps        # Go modules + npm packages
make db-up       # Postgres on localhost:5433
make migrate     # apply migrations
make admin       # create user "admin"; prints a generated password
make web         # build the UI (embedded by the Go build)
make run         # http://localhost:8080
```

To run jobs, also:

```sh
make kind-up       # kind cluster "dispatch-ops" with the dispatch-jobs namespace
make kind-load     # build runner-base and ee-shell, load them into kind
make task-manager  # in another terminal, next to `make run`
```

Job pods reach the API on the host at `http://host.docker.internal:8080`
(`DISPATCH_RUNNER_API_URL`). If your `.env` predates milestone 2, copy the
new variables from `.env.example`.

Then register the image and create a template, e.g. with an API token:

```sh
curl -s localhost:8080/api/v1/execution-environments -H "Authorization: Bearer $TOKEN" \
  -d '{"name": "ee-shell", "image": "dispatch-ops/ee-shell:dev"}'
curl -s localhost:8080/api/v1/job-templates -H "Authorization: Bearer $TOKEN" \
  -d '{"name": "hello", "jobType": "container", "executionEnvironmentId": "<ee id>",
       "command": ["bash", "-c", "echo hello from $(hostname)"]}'
```

and launch it from the *Job templates* page.

For UI work, run `make run` and `make web-dev` together, then open
http://localhost:5173. Vite hot-reloads the UI and proxies `/api` to the Go
server.

`make help` lists all targets.

### Changing the API

1. Edit `api/openapi.yaml`.
2. Run `make generate` to regenerate `internal/api/apigen` and
   `web/src/lib/api/schema.d.ts`.
3. Implement the new methods on `api.Handlers`. The build fails until every
   operation is implemented.

Requests are validated against the spec before they reach a handler. An
operation's `security` block decides whether it needs a signed-in caller.
`security: []` makes it public.

### Tests

```sh
make test      # unit tests; database tests are skipped
make test-db   # all tests, against dispatch_test_* databases in the dev Postgres
make check     # go vet, gofmt, svelte-check, prettier
```

Each package with database tests gets its own database (see
`internal/testdb`), because `go test` runs packages in parallel. The runner is
tested end to end against the real API in `internal/api/jobs_test.go`; the
task manager against a fake Kubernetes client.

## Authentication

- **Browser:** `POST /api/v1/auth/login` sets an HTTP-only `dispatch_session`
  cookie. Cross-site state-changing requests are rejected by Go's
  `http.CrossOriginProtection`.
- **Scripts/CI:** create an API token on the *API tokens* page and send
  `Authorization: Bearer dop_…`. API tokens cannot create more tokens.
- Only a SHA-256 hash of each token is stored. Passwords use argon2id.

- **Runners:** each job gets a one-time token (`doj_…`) that only works on
  `/runner/v1/jobs/<its id>/…`, and stops working when the job ends.

OIDC sign-in and RBAC arrive in milestone 4. Until then, every signed-in user
can manage every resource and launch any template.

## How a job runs

1. `POST /api/v1/job-templates/{id}/launch` stores a `pending` job with a
   snapshot of the resolved configuration.
2. The task manager (one active replica, elected with a Kubernetes Lease)
   claims it within `DISPATCH_MAX_CONCURRENT_JOBS`, creates a token Secret,
   an image pull Secret if the EE has a registry credential, and a Kubernetes
   Job. The job becomes `waiting`.
3. The pod starts the EE image, whose entrypoint is `dispatch-runner`. It
   fetches the spec (once; the job becomes `running`), writes the inventory,
   extra vars and credentials (credentials on a memory-backed volume) and runs
   the command in its own process group.
4. Output goes to `POST /runner/v1/jobs/{id}/events` in ordered,
   sequence-numbered batches; any API replica fans it out to browsers over
   SSE (`GET /api/v1/jobs/{id}/stream`).
5. The runner heartbeats every 10s and learns about cancellation from the
   reply; on cancel or timeout it sends SIGTERM, then SIGKILL after 10s.
6. It reports the final status. Finished Kubernetes Jobs are kept for
   `DISPATCH_JOB_TTL`.

The task manager fails a job with an explanation if its image can't be
pulled (after 3 minutes), its pod doesn't start within
`DISPATCH_JOB_START_TIMEOUT`, its pod dies without reporting (e.g.
OOMKilled), its heartbeats stop for 90s, or it outlives its timeout or a
cancel by 2 minutes. It also deletes Kubernetes Jobs whose job has finished.

### What the command sees

| Variable | |
|---|---|
| `DISPATCH_JOB_ID` | The job ID |
| `DISPATCH_INVENTORY` | Ansible JSON inventory file (only if the job has an inventory) |
| `DISPATCH_VARS` | Extra vars, as a JSON file |
| `DISPATCH_LIMIT` | Host pattern limit (may be empty) |
| `DISPATCH_CREDENTIALS` | All credentials as JSON: `[{name, kind, inputs}]` |
| `DISPATCH_CREDENTIALS_DIR` | One directory per credential (named after it), one file per input |

## Execution environments

An execution environment is an image whose entrypoint is `dispatch-runner`
and that runs as a numeric non-root user. Build one `FROM
dispatch-ops/runner-base` (see `images/runner-base/Dockerfile`), or copy a
static binary from `make runner` into any image:

```dockerfile
COPY dispatch-runner-linux-amd64 /usr/local/bin/dispatch-runner
USER 1000:1000
ENTRYPOINT ["/usr/local/bin/dispatch-runner"]
```

The runner reports its protocol version; the API rejects runners it doesn't
support with an explanation on the job.

## Configuration

| Variable | Default | |
|---|---|---|
| `DISPATCH_DATABASE_URL` | (required) | PostgreSQL URL |
| `DISPATCH_LISTEN_ADDR` | `:8080` | |
| `DISPATCH_SECURE_COOKIES` | `true` | Set `false` only for plain-HTTP development |
| `DISPATCH_SESSION_TTL` | `12h` | Browser session lifetime |
| `DISPATCH_AUTO_MIGRATE` | `false` | Apply migrations when `serve` starts |
| `DISPATCH_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `DISPATCH_LOG_FORMAT` | `json` | `json` or `text` |
| `DISPATCH_ENCRYPTION_KEY` | (required) | Encrypts credentials. 32 bytes, base64; `dispatch-ops gen-key` makes one |
| `DISPATCH_OLD_ENCRYPTION_KEYS` | | Comma-separated previous keys, still used to decrypt during a rotation |

Task manager only:

| Variable | Default | |
|---|---|---|
| `DISPATCH_RUNNER_API_URL` | (required) | The API URL as job pods reach it |
| `DISPATCH_JOB_NAMESPACE` | `dispatch-jobs` | Namespace for job pods (and the leader Lease) |
| `DISPATCH_KUBE_CONTEXT` | | kubeconfig context; empty uses the in-cluster config if available |
| `DISPATCH_MAX_CONCURRENT_JOBS` | `10` | Jobs waiting or running at once |
| `DISPATCH_JOB_START_TIMEOUT` | `10m` | How long a pod may take to start its runner |
| `DISPATCH_JOB_TTL` | `1h` | How long finished Kubernetes Jobs are kept |
| `DISPATCH_LEADER_ELECTION` | `true` | Elect one active task manager with a Lease |

## Container image

```sh
make docker
docker run --rm -e DISPATCH_DATABASE_URL=… -p 8080:8080 dispatch-ops:dev
```

Health probes: `/healthz` (liveness) and `/readyz` (checks the database).
