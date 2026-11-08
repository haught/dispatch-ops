# Dispatch Ops

A Kubernetes-native automation platform for running Ansible playbooks, scripts
and container commands as jobs.

**Status:** early development. Ansible playbooks, scripts and container
commands run on Kubernetes with live logs; playbooks and scripts come from
git projects, and Ansible jobs report per-host results. Inventories,
credentials, execution environments, projects and job templates are managed
through the API; the UI lists jobs and templates, launches templates, streams
job output and shows per-host results.

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
| `internal/runner` | Runner logic; `runnerapi` is its generated API client, `callback` the Ansible callback plugin it embeds |
| `internal/db` | Connection pool and embedded SQL migrations |
| `internal/server` | Top-level HTTP handler: API, runner API, health probes, embedded UI |
| `images/` | `runner-base` and the starter execution environments `ee-shell`, `ee-python` and `ee-ansible` |
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
make kind-load     # build runner-base and the starter EEs, load them into kind
make task-manager  # in another terminal, next to `make run`
```

Job pods reach the API on the host at `http://host.docker.internal:8080`
(`DISPATCH_RUNNER_API_URL`). If your `.env` is older than
`.env.example`, copy the variables it is missing.

Then register the image and create a template, e.g. with an API token:

```sh
curl -s localhost:8080/api/v1/execution-environments -H "Authorization: Bearer $TOKEN" \
  -d '{"name": "ee-shell", "image": "dispatch-ops/ee-shell:dev"}'
curl -s localhost:8080/api/v1/job-templates -H "Authorization: Bearer $TOKEN" \
  -d '{"name": "hello", "jobType": "container", "executionEnvironmentId": "<ee id>",
       "command": ["bash", "-c", "echo hello from $(hostname)"]}'
```

and launch it from the *Job templates* page. For a playbook, register
`dispatch-ops/ee-ansible:dev` and a project, then make an `ansible` template:

```sh
curl -s localhost:8080/api/v1/projects -H "Authorization: Bearer $TOKEN" \
  -d '{"name": "ops", "scmUrl": "https://github.com/acme/ops.git", "scmRef": "main"}'
curl -s localhost:8080/api/v1/job-templates -H "Authorization: Bearer $TOKEN" \
  -d '{"name": "site", "jobType": "ansible", "executionEnvironmentId": "<ee id>",
       "projectId": "<project id>", "entrypoint": "site.yml", "inventoryId": "<inventory id>",
       "credentialIds": ["<ssh credential id>"], "allowOverrides": ["limit", "tags"]}'
```

`make test-db` runs a real playbook too if `ansible-playbook` is on your
`PATH` (e.g. `pip install ansible-core` in a virtualenv); otherwise that test
is skipped.

For UI work, run `make run` and `make web-dev` together, then open
http://localhost:5173. Vite hot-reloads the UI and proxies `/api` to the Go
server.

`make help` lists all targets.

### Dev container

`.devcontainer/` has everything above (Go, Node, the Docker CLI and compose,
kubectl, psql, ansible-core) and uses the host's Docker daemon through its
socket. It starts nothing by itself; use the same `make` targets. Inside it:

- `make db-up` starts Postgres on the host; the Makefile points
  `DISPATCH_DATABASE_URL` at `host.docker.internal` instead of `localhost`.
- `make kind-up` also joins the container to the `kind` network and writes a
  kubeconfig with the cluster's internal address. After rebuilding the
  container, run `make kind-join` to do that again for an existing cluster.
- Port 8080 is published on the host, so job pods still reach the API at
  `http://host.docker.internal:8080`.
- `web/node_modules` is a Docker volume, so the container's Linux packages
  don't replace the host's. Run `make deps` once after creating the container.

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

OIDC sign-in and role-based access control are not implemented yet: every
signed-in user can manage every resource and launch any template.

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

## Job types

| Type | Runs | Needs |
|---|---|---|
| `ansible` | `ansible-playbook` on the template's playbook (`entrypoint`) | `projectId`, `entrypoint`; an EE with ansible-core and git |
| `script` | The `entrypoint` script with the template's `arguments` | `projectId`, `entrypoint`; an EE with git |
| `container` | The template's `command` | `command` |

Ansible and script jobs run in the project checkout (so a project's
`ansible.cfg` applies). Scripts with a `#!` line run with that interpreter;
others with `/bin/sh`.

### Projects

A project is a git repository (`scmUrl`) and a branch, tag or commit
(`scmRef`, default `main`). Each job clones it fresh (shallow) and records
the commit it ran (`scmRevision`). URLs may be `https://`, `http://`,
`ssh://`, `git://`, `file://` or scp-like `git@host:path`. A project may have
an `scm` credential: a username and password/token for HTTP(S), or an SSH
private key (decrypted in the pod if it has a passphrase). SSH host keys are
accepted on first use, since job pods start without known hosts. The
credential's files are deleted before the playbook or script starts.

A job keeps the URL and ref it was launched with; later project changes
don't affect it.

### Ansible

The runner calls `ansible-playbook` with the job's inventory, extra vars
(`-e @file`), `--limit`, `--tags`, `--skip-tags` and verbosity, and with the
template's credentials:

| Credential kind | Becomes |
|---|---|
| `ssh` | `-u`, `--private-key`, `--become-password-file` |
| `username_password` | `-u`, `--connection-password-file` (needs `sshpass`), `--become-password-file` |
| `vault` | `--vault-id <id>@file`, or `--vault-password-file` without a vault ID |
| others | `DISPATCH_CREDENTIALS` / `DISPATCH_CREDENTIALS_DIR`, as for every job |

A template takes at most one machine credential (`ssh` or
`username_password`) and one vault password per vault ID. Host key checking
is off (`ANSIBLE_HOST_KEY_CHECKING=False`).

The runner adds a callback plugin, `dispatch_events`, by setting
`ANSIBLE_CALLBACK_PLUGINS` (this replaces a `callback_plugins` setting in the
project's `ansible.cfg`; Ansible's default directories stay on the path). It
reports each play, task and host result as a structured job event, without
the output of `no_log` tasks. `GET /api/v1/jobs/{id}/hosts` sums them up per
host (Ansible's own recap once the playbook ends), and
`GET /api/v1/jobs/{id}/events?hostName=…` lists one host's task results. The
job page shows both.

### What the command sees

| Variable | |
|---|---|
| `DISPATCH_JOB_ID` | The job ID |
| `DISPATCH_PROJECT_DIR` | The project checkout (ansible and script jobs) |
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
support with an explanation on the job. Ansible and script jobs need
protocol 2 or later, so images built with an older runner keep running
container jobs but must be rebuilt for the new job types.

The starter EEs are `ee-shell` (bash, curl, jq, git, ssh), `ee-python`
(Python in a virtualenv on `PATH`, with requests and PyYAML) and `ee-ansible`
(ansible-core with `ansible.posix`, `community.general` and `ansible.utils`,
plus git, ssh and sshpass).

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
