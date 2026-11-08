# Dispatch Ops

A Kubernetes-native automation platform for running Ansible playbooks, scripts
and container commands as jobs.

**Status:** early development. Ansible playbooks, scripts and container
commands run on Kubernetes with live logs; playbooks and scripts come from
git projects, and Ansible jobs report per-host results. Users sign in with
single sign-on (OIDC) or local accounts, and roles granted to users and
teams control who can see, use, change and launch what; every change is
audited. Templates can run on cron schedules, jobs can be relaunched (or
rerun on just their failed hosts), and job events can notify webhooks,
Slack or email. Old jobs are deleted after a retention period, and both
server processes export Prometheus metrics. The UI manages inventories
(hosts, groups and variables), credentials, projects, execution environments
and job templates, lists jobs, launches templates, manages their schedules,
notifications and access, streams job
output, shows per-host results, and manages users, teams, roles,
notification targets and API tokens. An operator installs and upgrades it on
Kubernetes.

## Layout

| Path | What it is |
|---|---|
| `api/openapi.yaml` | The API contract. The Go server and the TS client are generated from it. |
| `cmd/dispatch-ops` | The binary: `serve`, `task-manager`, `migrate`, `user create`, `user grant-admin`, `gen-key` |
| `cmd/dispatch-runner` | The runner: the entrypoint of every execution environment image |
| `cmd/dispatch-operator` | The Kubernetes operator |
| `internal/api` | HTTP handlers (one file per resource) and request pipeline (`router.go`) |
| `internal/api/apigen` | Generated server code. Do not edit by hand. |
| `internal/auth` | Password hashing, tokens, request principal |
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
| `images/` | `runner-base` and the starter execution environments `ee-shell`, `ee-python` and `ee-ansible` |
| `deploy/dev/` | kind cluster config and the `DispatchOps` for development |
| `deploy/helm/dispatch-ops-operator` | Helm chart that installs the operator and the CRD (`files/` is generated) |
| `deploy/examples/` | A production-style `DispatchOps` |
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

Then register the image and create a template, from the UI (*Execution
environments*, then *Job templates* → *New job template*) or with an API
token:

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

To run the whole system in the kind cluster through the operator instead:

```sh
make kind-up kind-load  # cluster, job namespace and execution environments
make kind-operator      # build the server and operator images, helm install the operator
make kind-install       # apply deploy/dev/dispatchops.yaml and wait until it's ready
```

`make kind-install` prints the administrator's password and how to reach the
UI with `kubectl port-forward`. Rerun `make kind-operator` after changing the
server or the operator. `make kind-uninstall` deletes the installation but
keeps its database.

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

### Changing the DispatchOps resource

Edit `internal/operator/api/v1alpha1/types.go` (kubebuilder markers set
validation and defaults) and run `make manifests`, which regenerates the
deepcopy code and the CRD in the Helm chart. `make generate` does it too.

### Changing the API

1. Edit `api/openapi.yaml`.
2. Run `make generate` to regenerate `internal/api/apigen` and
   `web/src/lib/api/schema.d.ts`.
3. Implement the new methods on `api.Handlers`. The build fails until every
   operation is implemented.

Requests are validated against the spec before they reach a handler. An
operation's `security` block decides whether it needs a signed-in caller.
`security: []` makes it public. What the caller may do is checked by the
handler, with the helpers in `internal/api/access.go`; changes are recorded
with `h.audit` (`internal/api/audit.go`).

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

- **Browser:** sign in with single sign-on (OIDC, below) or a local
  username and password (`POST /api/v1/auth/login`). Either sets an
  HTTP-only `dispatch_session` cookie. Cross-site state-changing requests
  are rejected by Go's `http.CrossOriginProtection`.
- **Scripts/CI:** create an API token on the *API tokens* page and send
  `Authorization: Bearer dop_…`. A token can do what its user can; a
  read-only one (`"scope": "read"`) only GET requests. Tokens can only be
  created from a browser session, never with another token. For a service
  account, create a local user and have an administrator create its tokens
  (`POST /api/v1/users/{id}/tokens`, or the user's page).
- Only a SHA-256 hash of each token is stored. Passwords use argon2id.
- Deactivating a user ends their sessions; their tokens stop working until
  they are reactivated.
- **Runners:** each job gets a one-time token (`doj_…`) that only works on
  `/runner/v1/jobs/<its id>/…`, and stops working when the job ends.

### Single sign-on (OIDC)

Set `DISPATCH_OIDC_ISSUER`, `DISPATCH_OIDC_CLIENT_ID`,
`DISPATCH_OIDC_CLIENT_SECRET` and `DISPATCH_EXTERNAL_URL`, and register
`<DISPATCH_EXTERNAL_URL>/api/v1/auth/oidc/callback` as the client's redirect
URI. The sign-in page then offers a single sign-on button. The flow is the
authorization code flow with PKCE.

A user is created at their first sign-in, keyed on the issuer and subject.
Their username comes from `DISPATCH_OIDC_USERNAME_CLAIM` (then `email`, then
the subject), and their display name and email from `name` and `email`; all
three are refreshed at each sign-in. A username already used by another
account is refused.

Teams list provider groups (`oidcGroups`). At each sign-in a user joins the
teams that list one of the groups in their `DISPATCH_OIDC_GROUPS_CLAIM`, and
leaves the teams they joined that way if they no longer have it. Members
added by an administrator are left alone. Ask the provider to include the
claim (often with the `groups` scope in `DISPATCH_OIDC_SCOPES`); nested
claims such as Keycloak's `realm_access.roles` are addressed with dots. If
the ID token lacks the claim, the userinfo endpoint is asked.

The provider is contacted on the first sign-in, not at startup, so the API
keeps serving local users, tokens and runners while it is unreachable.
Signing out ends the Dispatch Ops session only, not the provider's.

## Access control

Roles are granted to users or teams (`/api/v1/role-grants`, or the
*Access*, *Users* and *Teams* pages):

| Role | On | Allows |
|---|---|---|
| `admin` | an inventory, credential, project or job template | Edit and delete it, and grant roles on it. Includes the others. |
| `use` | an inventory, credential or project | Reference it from a job template (or a credential from a project) |
| `execute` | a job template | Launch it |
| `read` | any of those | See it |
| `system_admin` | everything | Anything, including users, teams and execution environments |
| `auditor` | everything | Read everything, including the audit log |

- A role on a resource type without a resource ID applies to every resource
  of that type. `admin` on a type is what allows creating them, and whoever
  creates one gets `admin` on it.
- Lists show only what the caller can read. A resource they can't read is
  reported as not found; an action their roles don't allow is forbidden.
- Changing a template's project, inventory or credentials needs `use` on the
  new ones. Overriding the inventory at launch needs `use` on it.
- A job can be seen by whoever launched it and by anyone with a role on its
  template. It can be canceled by whoever launched it and by those with
  `execute` on its template.
- Execution environments can be used by everyone; only system
  administrators manage them, and users and teams.
- `make admin` (`dispatch-ops user create -admin`) creates a local system
  administrator. `dispatch-ops user grant-admin <username>` makes an
  existing user one, e.g. to recover access, or to promote a single sign-on
  user after their first sign-in. Granting `system_admin` to a team mapped
  from a provider group does the same for everyone in it.

Upgrading from a version without access control turns existing
administrators into `system_admin` grants. Other existing users see nothing
until they are granted roles.

### Audit log

Every change made through the API, every launch and cancel, and every
sign-in is recorded with who made it, how they authenticated and when:
creates with the new resource, deletes with the old one, and updates with
the fields that changed. Secrets are never recorded; a credential update
lists which inputs were changed. System administrators and auditors can
read it on the *Audit log* page or at `GET /api/v1/audit-log`.

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

### Relaunching

`POST /api/v1/jobs/{id}/relaunch` (the *Relaunch* button) queues a new job
with the old one's configuration: the same overrides, image and project
ref, even if the template has changed since. With `{"hosts": "failed"}`, a
finished Ansible job is rerun with its `limit` set to the hosts that failed
or were unreachable. Relaunching needs `execute` on the template, plus `use`
on anything the job used that the template no longer references (only
system administrators can relaunch jobs whose template was deleted).

## Schedules

A schedule launches a job template on a cron expression, read in an IANA
time zone (default UTC): five fields (`30 2 * * 1-5`) or `@daily`,
`@hourly`, `@every 6h` and the like, at most once a minute. A schedule may
carry launch overrides, which the template must allow. Add schedules on a
template's page or with `POST /api/v1/schedules`;
`GET /api/v1/schedule-preview?cron=…&timezone=…` checks an expression and
lists its next run times.

The task manager launches due schedules (launch type `schedule`, recorded
in the audit log as launched by the schedule). A schedule that was due
several times while no task manager ran launches once, then carries on. If
a launch fails (say the template no longer allows its overrides), the
schedule keeps running and shows the error as `lastError`. Seeing schedules
needs a role on the template; creating one needs `execute` (and `use` on an
inventory it overrides); changing or deleting one needs `admin` on the
template, or having created it.

## Notifications

System administrators set up notification targets on the *Notifications*
page (`/api/v1/notification-targets`); template admins then choose, on the
template's page, which targets its jobs notify and on which events:
`started`, `succeeded`, `failed` (which includes `error`) and `canceled`.

| Kind | Config | Secrets |
|---|---|---|
| `webhook` | `url` | `authorization` (sent as the `Authorization` header), `signingSecret` |
| `slack` | | `webhookUrl` (an incoming webhook) |
| `email` | `host`, `port` (587, or 465 with `tls`), `security` (`starttls`, `tls` or `none`), `username`, `from`, `to` | `password` |

Secrets are encrypted like credentials and never returned. A webhook gets a
POST of `{"event": …, "job": {…}}` with `X-Dispatch-Event` and
`X-Dispatch-Delivery` headers; with a signing secret, also
`X-Dispatch-Timestamp` and `X-Dispatch-Signature: sha256=<hex>`, the
HMAC-SHA256 of `<timestamp>.<body>`. Check it, and reject old timestamps.

A job's status change queues its notifications in the same database
transaction, and the task manager sends them. Failures are retried after
30s, 2m, 10m and 30m; 4xx responses and SMTP 5xx replies are not retried.
A job's page (and `GET /api/v1/jobs/{id}/notifications`) shows what was
sent. *Test* on a target sends a test message from the API server at once.
Links to jobs need `DISPATCH_EXTERNAL_URL`.

## Data retention

The task manager deletes finished jobs, with their output and
notifications, `DISPATCH_JOB_RETENTION` after they finish (default 90 days;
`0` keeps them), and audit log entries older than
`DISPATCH_AUDIT_RETENTION` (default `0`, kept forever). It checks hourly,
starting a minute after it becomes leader. Retention periods take days
(`90d`) or Go durations (`2160h`).

## Metrics

The API server serves Prometheus metrics on `:9090/metrics` and the task
manager on `:9091/metrics` (`DISPATCH_METRICS_ADDR`; set it empty to turn
them off). The listener is separate from the API, so metrics aren't exposed
wherever the API is. Counters are per process; sum them across replicas.

| Metric | |
|---|---|
| `dispatch_http_requests_total{operation,method,code}`, `dispatch_http_request_duration_seconds{operation}` | API requests by OpenAPI operation ID |
| `dispatch_job_streams_active` | Open live log streams |
| `dispatch_jobs_launched_total{job_type,launch_type}` | Jobs queued |
| `dispatch_jobs_finished_total{job_type,status}`, `dispatch_job_duration_seconds{job_type,status}` | Jobs finished, and how long they ran |
| `dispatch_job_queue_wait_seconds{job_type}` | Time from launch to the runner starting |
| `dispatch_taskmanager_jobs_dispatched_total{result}`, `dispatch_taskmanager_jobs_ended_total{status}` | Kubernetes Jobs created; jobs the task manager ended (timeouts, lost pods) |
| `dispatch_schedule_runs_total{result}` | Schedule firings |
| `dispatch_notifications_total{kind,result}`, `dispatch_notification_send_duration_seconds{kind}` | Notification attempts (`sent`, `retry`, `failed`) |
| `dispatch_retention_deleted_total{kind}` | Rows deleted by retention |
| `dispatch_taskmanager_tick_duration_seconds` | Task manager passes |
| `dispatch_taskmanager_leader` | 1 on the leading task manager |
| `dispatch_jobs{status}`, `dispatch_job_queue_oldest_pending_seconds`, `dispatch_notifications_pending` | From the database, by the leader only |

Useful alerts: `dispatch_job_queue_oldest_pending_seconds` growing (jobs
aren't starting), `sum(dispatch_taskmanager_leader) == 0` (no task
manager), and a rising rate of `dispatch_notifications_total{result="failed"}`.

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
| `DISPATCH_EXTERNAL_URL` | | The server's URL as browsers reach it, e.g. `https://dispatch.example.com`. Needed for single sign-on, and for links to jobs in notifications |
| `DISPATCH_METRICS_ADDR` | `:9090` (`serve`), `:9091` (`task-manager`) | Where `/metrics` is served; empty turns metrics off |
| `DISPATCH_OIDC_ISSUER` | | OIDC provider's issuer URL. Turns on single sign-on |
| `DISPATCH_OIDC_CLIENT_ID` | | Required with the issuer |
| `DISPATCH_OIDC_CLIENT_SECRET` | | |
| `DISPATCH_OIDC_SCOPES` | `profile,email` | Scopes besides `openid`, comma- or space-separated |
| `DISPATCH_OIDC_USERNAME_CLAIM` | `preferred_username` | |
| `DISPATCH_OIDC_GROUPS_CLAIM` | `groups` | Claim listing the user's groups, which teams map to |
| `DISPATCH_OIDC_DISPLAY_NAME` | `Single sign-on` | Label of the sign-in button |

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
| `DISPATCH_JOB_RETENTION` | `90d` | Delete finished jobs this long after they finish; `0` keeps them |
| `DISPATCH_AUDIT_RETENTION` | `0` | Delete audit log entries this old; `0` keeps them |

## Installing on Kubernetes

The operator runs Dispatch Ops from `DispatchOps` resources (group
`dispatch-ops.io`, short name `dops`), one per installation. Install it with
its Helm chart:

```sh
helm install dispatch-ops-operator deploy/helm/dispatch-ops-operator \
  --namespace dispatch-ops-system --create-namespace
```

The chart installs the CRD too (kept on uninstall, so installations
survive). `serverImage` sets the server image that installations without
`spec.image` run, and `watchNamespaces` limits the operator to some
namespaces. Then create an installation:

```yaml
apiVersion: dispatch-ops.io/v1alpha1
kind: DispatchOps
metadata:
  name: dispatch-ops
  namespace: dispatch-ops
spec: {}
```

`spec: {}` runs a PostgreSQL the operator manages, two API replicas and one
task manager running jobs in the same namespace. `kubectl get dops` shows
whether it is ready and the image it runs; the conditions
(`DatabaseReady`, `Migrated`, `APIReady`, `TaskManagerReady`, `Ready`) say
what it is waiting for. `deploy/examples/dispatchops.yaml` shows the
options for production: an external database, an encryption key you
provide, an Ingress, single sign-on, a separate job namespace and
retention periods. `kubectl explain dispatchops.spec` documents every
field.

`spec.retention.jobs` and `spec.retention.auditLog` set the retention
periods (e.g. `2160h`). Metrics are on by default (`spec.metrics.enabled`):
both Deployments' pods serve them on a port named `metrics` (9090) and carry
`prometheus.io/scrape` annotations; the API Service doesn't expose it. To
scrape with the Prometheus Operator, use a PodMonitor selecting
`app.kubernetes.io/name: dispatch-ops` with `port: metrics`.

For an installation named `<name>`, the operator creates:

| Object | |
|---|---|
| `<name>-api` Deployment and Service | The API server and UI on port 8080. Jobs reach it at `http://<name>-api.<namespace>.svc:8080` |
| `<name>-task-manager` Deployment, ServiceAccount, Role and RoleBinding | The task manager, with the permissions it needs in the job namespace |
| `<name>-postgres` StatefulSet and Service | The managed database (unless `spec.database.external`) |
| `<name>-migrate-<hash>` Job | Database migrations for one image, then the administrator if missing |
| `<name>` Ingress | If `spec.api.ingress` is set |
| `<name>-encryption-key`, `<name>-admin-password`, `<name>-postgres` Secrets | Generated unless the spec names your own |

Installing or changing `spec.image` (or upgrading the chart, for
installations without one) first runs the new image's migrations in a Job;
the API server and task manager keep running the old image until the
migrations succeed, then roll out. If they fail, the installation stays on
the old image with `Migrated=False`; fix the cause and delete the Job to
retry. Migrations only go forward, so going back to an older image after an
upgrade is not supported.

The administrator (`spec.admin.username`, default `admin`) is created by the
first migration Job with the password in `<name>-admin-password`, unless a
user of that name exists. Changing the Secret later doesn't change the
password.

Deleting a `DispatchOps` deletes what the operator runs but keeps the
database volume and the generated Secrets, so recreating it with the same
name picks up the same data. Back up the encryption key Secret: without it,
stored credentials can't be decrypted. The managed database has no backups
or replication; use `spec.database.external` where that matters.

Changes to Secrets the operator reads (e.g. the OIDC client secret) take
effect when the pods restart (`kubectl rollout restart`).

## Container images

```sh
make docker
docker run --rm -e DISPATCH_DATABASE_URL=… -p 8080:8080 dispatch-ops:dev
```

builds `dispatch-ops` (the server) and `dispatch-ops-operator`. Health
probes: `/healthz` (liveness) and `/readyz` (checks the database).
