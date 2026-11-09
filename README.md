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
Slack, Google Chat or email. Old jobs are deleted after a retention period, and both
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
| `images/` | `runner-base` and the starter execution environments `ee-shell`, `ee-python` and `ee-ansible`; your own EEs in the gitignored `images/local/` |
| `examples/ee-template` | Starting point for a repository holding a custom execution environment |
| `deploy/dev/` | kind cluster config and the `DispatchOps` for development |
| `deploy/helm/dispatch-ops-operator` | Helm chart that installs the operator and the CRD (`files/` is generated) |
| `deploy/examples/` | A production-style `DispatchOps` |
| `web/` | SvelteKit 3 / Svelte 5 single-page app, embedded into the binary |

## Development

Everything runs in a local kind cluster: the operator, the API server, the
task manager and job pods. [Tilt](https://tilt.dev) rebuilds and redeploys
whatever a change affects. The database is your own Postgres, outside the
cluster.

Requires Go 1.27+, Node 25+, Docker (Docker Desktop on macOS), kubectl, Helm 3,
kind and Tilt (the dev container has them all), and a Postgres you can reach
at `localhost` (by default a container named `postgresql`).

```sh
make setup-env   # create .env from .env.example; set DISPATCH_DATABASE_URL
make deps        # Go modules + npm packages
make db-init     # create the role and the dispatch + dispatch_test databases (once)
make dev         # kind cluster + `tilt up`
```

Then open https://localhost:8443 (Dispatch Ops) and http://localhost:10350
(Tilt: build status and logs for every part). The API serves HTTPS with a
self-signed certificate the operator generated, so the browser warns about it
once. On a new database, the operator
creates the user `admin`; `make admin-password` prints its password.

What a change rebuilds:

| You change | Tilt runs | Then |
|---|---|---|
| `web/` | `npm run build`, then the Go build (the UI is embedded) | as below |
| `cmd/dispatch-ops`, `internal/` | Go build, server image | the operator migrates the database, then rolls the API server and task manager |
| `cmd/dispatch-operator`, `internal/operator`, the chart | Go build, operator image | the operator rolls |
| `cmd/dispatch-runner`, `internal/runner`, `images/` | `make kind-load` | the next job uses the new EE images |
| `deploy/dev/dispatchops.yaml`, `.env` | — | re-applies the `DispatchOps` (edit the `Tiltfile` to change how) |

A server change takes some 15–30 seconds to go live. For quick UI work, run
`make web-dev` next to `make dev` and open http://localhost:5173: Vite
hot-reloads the UI and proxies `/api` to the API in the cluster.

The `DispatchOps` the operator runs is `deploy/dev/dispatchops.yaml`. The
`Tiltfile` adds the server image it built and Secrets made from `.env`: the
database URL (with `localhost` changed to `host.docker.internal`, where pods
reach your Postgres), the encryption key and the OIDC client secret. Edit the
`DispatchOps`, not the Deployments: the operator reverts changes to what it
created.

`make dev-down` (`tilt down`) removes the deployment; `make kind-down` deletes
the cluster. The database is untouched either way.

To run jobs, register an image and create a template, from the UI (*Execution
environments*, then *Job templates* → *New job template*) or with an API
token:

```sh
curl -sk https://localhost:8443/api/v1/execution-environments -H "Authorization: Bearer $TOKEN" \
  -d '{"name": "ee-shell", "image": "dispatch-ops/ee-shell:dev"}'
curl -sk https://localhost:8443/api/v1/job-templates -H "Authorization: Bearer $TOKEN" \
  -d '{"name": "hello", "jobType": "container", "executionEnvironmentId": "<ee id>",
       "command": ["bash", "-c", "echo hello from $(hostname)"]}'
```

and launch it from the *Job templates* page. For a playbook, register
`dispatch-ops/ee-ansible:dev` and a project, then make an `ansible` template:

```sh
curl -sk https://localhost:8443/api/v1/projects -H "Authorization: Bearer $TOKEN" \
  -d '{"name": "ops", "scmUrl": "https://github.com/acme/ops.git", "scmRef": "main"}'
curl -sk https://localhost:8443/api/v1/job-templates -H "Authorization: Bearer $TOKEN" \
  -d '{"name": "site", "jobType": "ansible", "executionEnvironmentId": "<ee id>",
       "projectId": "<project id>", "entrypoint": "site.yml", "inventoryId": "<inventory id>",
       "credentialIds": ["<ssh credential id>"], "allowOverrides": ["limit", "tags"]}'
```

`make test-db` runs a real playbook too if `ansible-playbook` is on your
`PATH` (e.g. `pip install ansible-core` in a virtualenv); otherwise that test
is skipped.

`make help` lists all targets.

### Dev container

`.devcontainer/` has everything above (Go, Node, the Docker CLI, kubectl,
Helm, kind, Tilt, psql, ansible-core) and uses the host's Docker daemon
through its socket. It starts nothing by itself; use the same `make` targets.
Inside it:

- The Makefile points `DISPATCH_DATABASE_URL` at `host.docker.internal`
  instead of `localhost`.
- `make kind-up` also joins the container to the `kind` network and writes a
  kubeconfig with the cluster's internal address. After rebuilding the
  container, run `make kind-join` to do that again for an existing cluster.
- Ports 8443 (Dispatch Ops), 10350 (Tilt) and 5173 (Vite) are forwarded to
  the host.
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
make test-db   # all tests, against dispatch_test_* databases in your Postgres (see db-init)
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
- An inventory's source is set, changed and synced by those with `admin` on
  the inventory (and `use` on the project and credentials they add). Its
  sync jobs can be seen by anyone with a role on the inventory, and
  canceled by its admins.
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

A schedule can instead sync an inventory from its source (`inventoryId`
instead of `jobTemplateId`, no overrides). Those are managed on the
inventory's page by its admins. A run while the last sync is still going
launches nothing and shows why in `lastError`.

## Notifications

System administrators set up notification targets on the *Notifications*
page (`/api/v1/notification-targets`); template admins then choose, on the
template's page, which targets its jobs notify and on which events:
`started`, `succeeded`, `failed` (which includes `error`) and `canceled`.

| Kind | Config | Secrets |
|---|---|---|
| `webhook` | `url` | `authorization` (sent as the `Authorization` header), `signingSecret` |
| `slack` | | `webhookUrl` (an incoming webhook) |
| `googlechat` | | `webhookUrl` (a space's incoming webhook, from *Apps & integrations → Webhooks*) |
| `email` | `host`, `port` (587, or 465 with `tls`), `security` (`starttls`, `tls` or `none`), `username`, `from`, `to` | `password` |

Secrets are encrypted like credentials and never returned. A webhook gets a
POST of `{"event": …, "job": {…}}` with `X-Dispatch-Event` and
`X-Dispatch-Delivery` headers; with a signing secret, also
`X-Dispatch-Timestamp` and `X-Dispatch-Signature: sha256=<hex>`, the
HMAC-SHA256 of `<timestamp>.<body>`. Check it, and reject old timestamps.
Google Chat gets a card with the job's status and an *Open job* button,
and a job's messages share one thread in the space.

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
| `inventory_sync` | `ansible-inventory` on an inventory's source (see [Inventory sources](#inventory-sources)); not a template type | an inventory source; an EE with ansible-core and git |

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
| `generic` | Environment variables named after its inputs, as for every job |
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

#### Roles and collections

Before the playbook starts, the runner installs what the project's
requirements files list, with `ansible-galaxy`, in this order:

| File | Command |
|---|---|
| `roles/requirements.yml` | `ansible-galaxy role install -r …` |
| `collections/requirements.yml` | `ansible-galaxy collection install -r …` |
| `requirements.yml` | `ansible-galaxy install -r …` (roles and collections) |

Each may also end in `.yaml`. They install into the job's workspace, which
`ANSIBLE_COLLECTIONS_PATH` and `ANSIBLE_ROLES_PATH` put ahead of Ansible's
default directories (this replaces `collections_path` and `roles_path` in the
project's `ansible.cfg`; playbook-adjacent `collections/` and `roles/`
directories still work). The installs use the project's `scm` credential, so
Git sources on the same server as the project need no extra setup. The
output goes to the job log, and the job fails if an install fails. The
timeout covers the installs too. Collections baked into the EE image are
still found, so an image can make a requirements file unnecessary.

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
| *generic credential inputs* | Each input of a `generic` credential, under its own name (e.g. `AKIPS_PASSWORD`) |

Generic credential names that the job's tools depend on are reserved and
refused when the credential is saved: `PATH`, `HOME`, `USER`, `SHELL`, `PWD`,
`LANG`, `TMPDIR`, `HOSTNAME`, and names starting with `DISPATCH_`, `ANSIBLE_`,
`LD_`, `LC_`, `PYTHON`, `GIT_`, `SSH_` or `KUBERNETES_`. If two generic
credentials on a job set the same name, the later one wins and the job log
says so. This lets tools that read their settings from the environment,
such as Ansible inventory plugins (`AKIPS_HOST`, …), use a generic
credential directly.

### Inventory sources

An inventory can be synced from a project instead of edited by hand, like
AWX's "sourced from a project": a static inventory file or directory, an
inventory script, or the YAML configuration of an inventory plugin. Set it
on the inventory's page, or with `PUT /api/v1/inventories/{id}/source`
(`projectId`, `sourcePath`, `executionEnvironmentId`, optional
`credentialIds`, `timeoutSeconds`, `verbosity`).

A sync (`POST /api/v1/inventories/{id}/sync`, *Sync now*, a schedule, or
relaunching an earlier sync) is an `inventory_sync` job. It checks the
project out, installs its requirements files as for Ansible jobs, and runs

```
ansible-inventory -i <sourcePath> --list --export
```

in the checkout, with `ANSIBLE_INVENTORY_UNPARSED_FAILED` and
`ANSIBLE_INVENTORY_ANY_UNPARSED_IS_FAILED` set, so a source Ansible can't
parse fails the job instead of emptying the inventory. The runner sends the
result to the API, which replaces the inventory's hosts and groups in one
transaction: hosts and groups keep their IDs when their names stay, those
no longer reported are deleted, and group variables stay on their groups.
The `all` group's variables are ignored; the inventory's own variables are
yours to edit. Host names with whitespace are skipped with a warning in the
job log. At most one sync of an inventory runs at a time.

While an inventory has a source, its hosts and groups can't be changed by
hand (409). Removing the source keeps them, editable again.

Credentials a sync can use: `generic` (each input becomes an environment
variable, which is how inventory plugins usually take their settings),
`vault` (for vaulted variable files) and `api_token` (in
`DISPATCH_CREDENTIALS`, for scripts). For example, an inventory synced with
the `haught.akips.akips_inventory` plugin:

1. A generic credential with `AKIPS_HOST`, `AKIPS_USERNAME` and
   `AKIPS_PASSWORD`.
2. A project for the repository holding `inventory_akips.yml` and a
   `collections/requirements.yml` listing `haught.akips`.
3. The inventory's source: that project, `inventory_akips.yml`, the
   `ee-ansible` execution environment, and the credential.

Inventory syncs need runner protocol 3 or later.

## Execution environments

An execution environment is an image whose entrypoint is `dispatch-runner`
and that runs as a numeric non-root user. The starter EEs are:

- `ee-shell`: bash, curl, jq, git and ssh.
- `ee-python`: Python in a virtualenv on `PATH`, with requests and PyYAML.
- `ee-ansible`: ansible-core with `ansible.posix`, `community.general` and
  `ansible.utils`, the requests and netaddr libraries inventory plugins often
  need, plus git, ssh and sshpass.

`make images` builds them as `dispatch-ops/<name>:dev`. `make publish-images
IMAGE_REPO=ghcr.io/<org>/dispatch-ops PUBLISH_TAGS="1.4.0 1.4 latest"` pushes
them for amd64 and arm64, along with `runner-base` and `runner`.

There are three ways to get what your jobs need, simplest first.

1. **Collections a project needs:** list them in the project's
   `requirements.yml` (see [Roles and collections](#roles-and-collections)). The
   runner installs them at the start of every job, so you need no image of your
   own, but each job takes longer to start.
2. **Extend a starter EE:** start a repository from
   [`examples/ee-template`](examples/ee-template). It holds a Dockerfile,
   `packages.txt` (Debian packages), `requirements.txt` (Python packages),
   `requirements.yml` (collections and roles), and a GitHub Actions workflow
   that builds, tests and pushes the image. The starter EEs include
   `dispatch-ee-install`, which installs those files into the right places:

   ```dockerfile
   FROM ghcr.io/<org>/dispatch-ops/ee-ansible:1.4
   USER root
   COPY packages.txt requirements.txt requirements.yml /tmp/ee/
   RUN dispatch-ee-install /tmp/ee && rm -rf /tmp/ee
   USER 1000:1000
   ```

   To build your EE against the runner in a checkout, link it into
   `images/local/`, which is gitignored (see
   [images/local/README.md](images/local/README.md)). `make kind-load` then
   builds and loads it as `dispatch-ops/local-<name>:dev`.
3. **Bring your own base image:** copy the runner in from the `runner` image,
   which holds only the static binary:

   ```dockerfile
   FROM registry.example.com/hardened/python:3.13
   COPY --from=ghcr.io/<org>/dispatch-ops/runner:1.4 /dispatch-runner /usr/local/bin/dispatch-runner
   USER 1000:1000
   ENTRYPOINT ["/usr/local/bin/dispatch-runner"]
   ```

   `make runner` also builds the binary for amd64 and arm64, in `bin/`.

Check an image before you register it with `docker run --rm <image>
selftest`. It verifies that the image runs as a non-root user and has git, ssh
and, for Ansible images, the Ansible tools. It lists the collections and prints
the runner protocol, and it exits non-zero when the image can't run jobs. The
template's workflow runs it on every build.

The runner reports its protocol version, and the API rejects runners it doesn't
support with an explanation on the job. Ansible and script jobs need protocol 2
or later, and inventory syncs protocol 3. Images built with an older runner keep
running the job types they know but must be rebuilt for newer ones. Rebuilding
a custom EE on a newer starter EE picks up the new runner. The template's
Renovate config opens that pull request for you.

## Configuration

| Variable | Default | |
|---|---|---|
| `DISPATCH_DATABASE_URL` | (required) | PostgreSQL URL |
| `DISPATCH_LISTEN_ADDR` | `:8443`, or `:8080` without TLS | |
| `DISPATCH_TLS` | `true` | Serve HTTPS. Set `false` only where something else encrypts traffic to the server |
| `DISPATCH_TLS_CERT_FILE`, `DISPATCH_TLS_KEY_FILE` | | A PEM certificate (chain) and key to serve, reloaded when the files change. Without them, `serve` makes a self-signed ECDSA P-384 certificate at startup, held only in memory |
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
| `DISPATCH_RUNNER_API_TLS_DIR` | | A directory holding the API's certificate Secret (`ca.crt` or `tls.crt`). Job pods trust it, besides the system's roots, to reach an `https` API URL |
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

The chart can also create the installation and its Secrets from the
`installation` section of its values, with the settings `.env` holds in
development:

```yaml
# lab-values.yaml
serverImage:
  repository: registry.lab.example/dispatch-ops
image:
  repository: registry.lab.example/dispatch-ops-operator
installation:
  enabled: true
  namespace: dispatch-ops
  externalURL: https://dispatch.lab.example     # DISPATCH_EXTERNAL_URL
  logLevel: info                                # DISPATCH_LOG_LEVEL
  database:
    existingSecret: dispatch-ops-database       # holds DISPATCH_DATABASE_URL as "url"
  encryptionKey:
    existingSecret: dispatch-ops-encryption-key # holds DISPATCH_ENCRYPTION_KEY as "key"
  oidc:
    issuer: https://idp.lab.example/realms/lab  # DISPATCH_OIDC_*
    clientID: dispatch-ops
    existingSecret: dispatch-ops-oidc
  spec:                                         # any other DispatchOps fields
    api:
      ingress:
        host: dispatch.lab.example
```

```sh
helm upgrade --install dispatch-ops-operator deploy/helm/dispatch-ops-operator \
  --namespace dispatch-ops-system --create-namespace -f lab-values.yaml
```

Instead of `existingSecret`, `database.url`, `encryptionKey.key` and
`oidc.clientSecret` take the values themselves and the chart creates the
Secrets; keep those in a separate values file out of version control. The
installation's namespace must exist. Helm can't create a `DispatchOps` in the
same install that first creates its CRD, so on a new cluster run the
`helm upgrade --install` twice; the chart's notes say when. Uninstalling the
chart keeps the installation and its Secrets unless `installation.keep` is
false.

`spec.retention.jobs` and `spec.retention.auditLog` set the retention
periods (e.g. `2160h`). Metrics are on by default (`spec.metrics.enabled`):
both Deployments' pods serve them on a port named `metrics` (9090) and carry
`prometheus.io/scrape` annotations; the API Service doesn't expose it. To
scrape with the Prometheus Operator, use a PodMonitor selecting
`app.kubernetes.io/name: dispatch-ops` with `port: metrics`.

For an installation named `<name>`, the operator creates:

| Object | |
|---|---|
| `<name>-api` Deployment and Service | The API server and UI, HTTPS on port 8443 (`https`). Jobs reach it at `https://<name>-api.<namespace>.svc:8443` |
| `<name>-task-manager` Deployment, ServiceAccount, Role and RoleBinding | The task manager, with the permissions it needs in the job namespace |
| `<name>-postgres` StatefulSet and Service | The managed database (unless `spec.database.external`) |
| `<name>-migrate-<hash>` Job | Database migrations for one image, then the administrator if missing |
| `<name>` Ingress | If `spec.api.ingress` is set |
| `<name>-encryption-key`, `<name>-admin-password`, `<name>-postgres`, `<name>-api-tls` Secrets | Generated unless the spec names your own |

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
effect when the pods restart (`kubectl rollout restart`), except the API
certificate (below).

### TLS

The API server serves HTTPS on port 8443. By default the operator generates
a self-signed ECDSA P-384 certificate, valid for 10 years, in the
`<name>-api-tls` Secret. It is valid for the Service's names, `localhost`, and
the ingress and `externalURL` hosts as they were when it was generated; delete
the Secret to have it generated again. Every installation gets its own key;
none is built into the image. Job pods trust the certificate (the task manager
hands them the Secret's `ca.crt`), and an Ingress gets
`nginx.ingress.kubernetes.io/backend-protocol: HTTPS` unless you set that
annotation yourself. Other ingress controllers may need to be told to use
HTTPS to the `https` Service port; Traefik does so by the port's name.

To serve your own certificate, such as one cert-manager keeps renewed, name
its `kubernetes.io/tls` Secret in `spec.api.tls.secretName`
(`installation.tls.existingSecret` in the chart):

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: dispatch-ops-api
  namespace: dispatch-ops
spec:
  secretName: dispatch-ops-api-tls
  privateKey:
    algorithm: ECDSA
    size: 384
  dnsNames:
    - dispatch-ops-api.dispatch-ops.svc   # job pods use this name: required
    - dispatch.example.com
  issuerRef:
    name: internal-ca
    kind: ClusterIssuer
---
apiVersion: dispatch-ops.io/v1alpha1
kind: DispatchOps
metadata:
  name: dispatch-ops
  namespace: dispatch-ops
spec:
  api:
    tls:
      secretName: dispatch-ops-api-tls
```

The API server reloads the certificate when cert-manager renews it, with no
restart, and new jobs trust the Secret's `ca.crt`, or its `tls.crt` and the
system's roots when it has no `ca.crt` (as with a public CA). The API pods
don't start until the Secret exists.

`spec.api.tls.enabled: false` serves plain HTTP on port 8080 instead; use it
only where something else, such as a service mesh, encrypts traffic to the
pods. Metrics (port 9090) are always plain HTTP.

## Container images

```sh
make docker
docker run --rm -e DISPATCH_DATABASE_URL=… -p 8443:8443 dispatch-ops:dev
```

builds `dispatch-ops` (the server) and `dispatch-ops-operator`. Health
probes: `/healthz` (liveness) and `/readyz` (checks the database). The server
serves HTTPS on 8443 with a self-signed ECDSA P-384 certificate it makes at
startup; mount your own and set `DISPATCH_TLS_CERT_FILE` and
`DISPATCH_TLS_KEY_FILE` to replace it.
