# Dispatch Ops

A Kubernetes-native automation platform for running Ansible playbooks,
scripts and container commands as jobs.

Each job runs in its own pod, streams live output to the browser, and is
governed by role-based access control. Playbooks and scripts come from git;
inventories are edited in the UI or synced from a source; secrets are
encrypted at rest and never returned by the API. An operator installs and
upgrades the whole thing on Kubernetes.

**Status:** minimum viable product. The features below work end to end; the
`DispatchOps` resource is `v1alpha1` and may still change.

To work on Dispatch Ops itself, see [DEVELOP.md](DEVELOP.md).

- [What it does](#what-it-does)
- [How it works](#how-it-works)
- [Running in production](#running-in-production)
- [Installing on Kubernetes](#installing-on-kubernetes)
- [Using Dispatch Ops](#using-dispatch-ops)
- [Configuration reference](#configuration-reference)
- [License](#license)

## What it does

**Run automation as jobs**

- Three job types: Ansible playbooks, scripts from a git repository, and any
  container command.
- Live job output in the browser, and per-host results for Ansible jobs
  (each play, task and host result, plus Ansible's recap).
- Relaunch any job with the launch options it ran with, or rerun an Ansible
  job on just the hosts that failed.
- Job templates hold everything a job needs; launchers may override only the
  fields the template allows (limit, tags, inventory and so on).
- Cron schedules, in any time zone, for templates and inventory syncs.
- Timeouts, cancellation, and clear errors when an image can't be pulled or a
  pod dies.

**Manage what jobs use**

- **Projects:** git repositories at a branch, tag or commit, cloned fresh for
  every job. Roles and collections listed in the project's requirements files
  are installed before the playbook runs.
- **Inventories:** hosts, groups and variables, edited by hand or synced from
  a project (a static file, an inventory script, or an inventory plugin
  configuration).
- **Credentials:** SSH keys, usernames and passwords, vault passwords, API
  tokens and generic key/value secrets. Encrypted with AES-256-GCM and never
  returned once saved.
- **Execution environments:** the container images jobs run in. Three
  starter images are provided, and you can extend them or bring your own.

**Control who does what**

- Single sign-on with any OIDC provider, with provider groups mapped to
  teams. Local accounts for automation and break-glass access.
- Roles granted to users and teams on individual resources or whole resource
  types: `read`, `use`, `execute`, `admin`, plus `system_admin` and
  `auditor`.
- API tokens for scripts and CI, optionally read-only.
- An audit log of every change, launch, cancel and sign-in.

**Operate it**

- Notifications on job start, success, failure and cancel, to webhooks
  (optionally HMAC-signed), Slack, Google Chat or email.
- Prometheus metrics from the API server and the task manager.
- Retention periods for finished jobs and audit log entries.
- A Kubernetes operator that installs, migrates and upgrades the platform
  from a single `DispatchOps` resource.
- Everything the UI does is available through a documented REST API
  (`api/openapi.yaml`).

## How it works

```
                 HTTPS + SSE
  +-----------+ ------------> +-----------------------+
  |  Browser  |               |  API server (2+)      |<---- API clients, CI
  +-----------+               |  REST API + web UI    |      (API tokens)
                              +-----------+-----------+
                                          |
                                          v
                              +-----------------------+
                              |  PostgreSQL           |
                              |  state, job queue,    |
                              |  job output           |
                              +-----------+-----------+
                                          ^
                                          |
                              +-----------+-----------+       Kubernetes API
                              |  Task manager         | ----> (creates Jobs,
                              |  (one active leader)  |        watches pods)
                              +-----------------------+
                                                                    |
                                                                    v
                              +-----------------------+   +--------------------+
                              |  API server           |<--|  Job pod           |
                              |  runner endpoints     |   |  execution env.    |
                              +-----------------------+   |  dispatch-runner   |
                                                          +---------+----------+
                                                                    |
                                                                    v
                                                     git servers, managed hosts,
                                                     cloud and network APIs
```

### Components

| Component | Runs as | Does |
|---|---|---|
| **Operator** | Deployment, installed with Helm | Reconciles each `DispatchOps` resource: deploys the API server and task manager, optionally a PostgreSQL, generates secrets and certificates, runs database migrations before every upgrade |
| **API server** | Deployment, scales horizontally | REST API, authentication, access control, the web UI, and the endpoints job pods report to. Fans live output out to browsers |
| **Task manager** | Deployment, one active leader elected with a Kubernetes Lease (extra replicas are standbys) | Takes queued jobs within the concurrency limit, creates a Kubernetes Job for each, watches them, enforces timeouts, fires schedules, sends notifications, applies retention |
| **Runner** | The entrypoint of every execution environment image | Fetches its job's spec, clones the project, writes inventory, variables and credentials, runs the command, streams output and reports the result |
| **PostgreSQL** | External, or a single instance the operator runs | All state. Also the job queue (`FOR UPDATE SKIP LOCKED`) and change notifications (`LISTEN/NOTIFY`), so no message broker is needed |

### How a job runs

1. Launching a template (UI, API, schedule or relaunch) stores a `pending`
   job with a snapshot of the resolved configuration. Later changes to the
   template, project or inventory don't affect it.
2. The task manager claims it when there is room under the concurrency
   limit. It creates a Secret with a one-time job token, an image pull Secret
   if the execution environment needs a registry credential, and a
   Kubernetes Job. The job becomes `waiting`. The token reaches the runner as
   the `DISPATCH_JOB_TOKEN` environment variable, never as a file.
3. The pod starts the execution environment image, whose entrypoint is
   `dispatch-runner`. The runner fetches the job spec from the API (the job
   becomes `running`), clones the project, and writes the inventory, extra
   variables and credentials. Credentials go on a memory-backed volume.
4. The runner streams output to the API in ordered batches. Any API replica
   fans it out to browsers over server-sent events.
5. The runner sends a heartbeat every 10 seconds and learns about
   cancellation from the reply. On cancel or timeout it sends SIGTERM, then
   SIGKILL after 10 seconds.
6. It reports the final status. The job's notifications are queued in the
   same transaction, and the task manager sends them.

The task manager fails a job, with an explanation, if its image can't be
pulled within 3 minutes, its pod doesn't start within the start timeout, its
pod dies without reporting (for example OOMKilled), its heartbeats stop for
90 seconds, or it outlives its timeout or a cancel by 2 minutes. It deletes
finished Kubernetes Jobs after a TTL (1 hour by default).

## Running in production

### What you need

| | Requirement |
|---|---|
| **Kubernetes** | A cluster you can install a CRD and the operator into. Job pods run in a namespace you choose. A network plugin that enforces NetworkPolicy (Calico, Cilium and so on) for the isolation described under [Network paths](#network-paths). |
| **PostgreSQL** | 15 or later. Use an external, backed-up database for production. The operator can run one for you (PostgreSQL 17, digest-pinned, on a 10 Gi PersistentVolume by default, as a non-root user with a read-only root filesystem, serving TLS), but it has no backups or replication. |
| **Container registry** | Access to `ghcr.io`, where releases publish the server, operator, runner and starter execution environment images (`ghcr.io/haught/dispatch-ops/<name>`, amd64 and arm64) that the Helm chart uses by default. Without it, mirror them, or build your own (see [Building images and releases](DEVELOP.md#building-images-and-releases)), into a registry your cluster can pull from and set `image.repository` and `serverImage.repository`. Your own execution environments go in a registry too. |
| **Ingress** | An ingress controller and a DNS name for the UI and API. The API server speaks HTTPS to the ingress controller too. |
| **TLS certificate** | Optional. Without one, the operator generates a self-signed certificate for the API server. cert-manager works well. |
| **OIDC provider** | Optional. Any provider that supports the authorization code flow with PKCE (Keycloak, Entra ID, Okta, Google and so on). |
| **Prometheus** | Optional. Pods carry scrape annotations; a PodMonitor works with the Prometheus Operator. |
| **SMTP server** | Optional, for email notifications. |

### Network paths

| From | To | Why |
|---|---|---|
| Users, API clients | Ingress, then API Service (HTTPS 8443) | UI and API |
| API server, task manager | PostgreSQL | All state |
| API server | OIDC provider | Single sign-on (contacted at first sign-in, not at startup) |
| Task manager | Kubernetes API | Create and watch Jobs, Secrets, the leader Lease |
| Task manager | Webhook, Slack, Google Chat, SMTP endpoints | Notifications (the API server too, for *Test*) |
| Job pods | API Service (`https://<name>-api.<namespace>.svc:8443`) | Fetch the job spec, report output and status |
| Job pods | Git servers, Ansible Galaxy or your collection mirror | Clone projects, install requirements |
| Job pods | Managed hosts, cloud and network APIs | Whatever your automation does |
| Nodes | Container registry | Pull execution environment images |
| Prometheus | Pods, port 9090 (`metrics`) | Metrics, plain HTTP, not exposed by the Service |

The operator creates NetworkPolicies (turn them off with
`spec.networkPolicy.enabled: false`). They take effect only where the
cluster's network plugin enforces NetworkPolicy:

- `<name>-postgres`: the managed database accepts connections only from the
  API server, task manager and migration pods.
- `<name>-api`, `<name>-task-manager`: anyone may reach the API port, and
  any pod but a job pod the unauthenticated metrics port. The chart adds the
  same for the operator's metrics (`networkPolicy.enabled`).
- `<name>-jobs`, in the job namespace: job pods accept no connections, and
  may connect only to the API server, DNS (port 53), and any address outside
  169.254.0.0/16, fe80::/10 and fd00:ec2::254 (the cloud metadata services).
  To keep jobs away from the rest of the cluster too, add its pod and
  service CIDRs (and any internal networks jobs shouldn't reach) to
  `spec.networkPolicy.jobEgressExcept`.

Behind an ingress controller or load balancer, set `spec.api.trustedProxies`
to its addresses or CIDRs, so sign-in throttling, which counts failed
attempts per client address, sees the client's address from
`X-Forwarded-For` rather than the proxy's. List only proxies that overwrite
that header; a client could otherwise choose its own address. It is unset
by default.

Each job may store at most 256 MiB of output, so one runaway job can't fill
the database. Past the cap the job keeps running, its log says the output was
cut, and only its Ansible recap is still kept. Change it with
`spec.api.maxJobOutput` (e.g. `1Gi`, or `0` for no cap).

The API server sends a strict `Content-Security-Policy` on every response,
`Cache-Control: no-store` on API responses, and `Strict-Transport-Security`
(two years, including subdomains) on the HTTPS connections it terminates
itself. Browsers only see the ingress controller's TLS, so have the ingress
send HSTS too (for ingress-nginx, its default with TLS enabled).

### Operator permissions

What the operator may do decides how much an attacker gains by compromising
it, or by creating a `DispatchOps`:

- **Watching all namespaces** (`watchNamespaces` empty, the default), the
  operator has a ClusterRole that can read and write every Secret, create
  Deployments, StatefulSets and Jobs, and write Roles and RoleBindings in
  every namespace. That is effectively cluster-admin: whoever controls the
  operator's pod or token controls the cluster. Use this mode on clusters
  where only cluster admins administer Dispatch Ops.
- **Watching some namespaces** (`watchNamespaces` set), the chart gives it a
  Role in each of them and in each of `allowedJobNamespaces`, and nothing
  cluster-wide but the `DispatchOps` CRD. Prefer this in production.
- **Job namespaces.** The task manager may read Secrets and create Jobs in
  its job namespace, and the operator grants it that. So a `DispatchOps` may
  only use its own namespace, or one that is opted in: listed in the chart's
  `allowedJobNamespaces`, or, when the operator watches all namespaces,
  labeled `dispatch-ops.io/allow-jobs-from=<the DispatchOps's namespace>`
  (which takes rights to label namespaces). Otherwise the installation
  reports `TaskManagerReady=False` (`JobNamespaceNotAllowed`) and the task
  manager isn't deployed. Removing the opt-in removes its Role and
  RoleBinding within five minutes.
- **Creating a `DispatchOps`** lets you choose the server image, and so run
  any code with the task manager's permissions in its namespace and job
  namespace (including reading their Secrets). Grant it only to those who
  could have those permissions anyway.
- The operator never takes over an object it didn't create: if one of the
  names it would use is taken, it reports an error instead.

A separate job namespace is safer than the default (the installation's own
namespace): `kubectl create namespace dispatch-jobs`, opt it in, and set
`spec.taskManager.jobNamespace`. The task manager's Secret access is only
in its job namespace, so it then can't read the installation's other
Secrets, such as the OIDC client secret, the administrator's initial
password, the database password, or anything else kept in that namespace.
(It holds the encryption key and database URL either way, since it needs
them.) It isn't the default because the namespace has to be created and
opted in first. The task manager gets only the public half of the API's
certificate, never its private key.

### Sizing

- **API server:** stateless. Two replicas (the default) for availability;
  add more for many concurrent users or live log streams.
- **Task manager:** only the leader works, so a second replica only adds a
  standby for faster failover.
- **Jobs:** each job is a pod. `spec.taskManager.maxConcurrentJobs` (default
  10) caps how many wait or run at once; the rest queue. Size the job
  namespace's nodes or quotas for that many execution environment pods.
  Each requests 100m CPU and 256Mi of memory and is limited to 2 CPUs, 2Gi
  of memory and a 4Gi working directory; change that with
  `spec.taskManager.jobResources` and `jobWorkSizeLimit`.
- **Database:** job output is the bulk of the data. Retention
  (`spec.retention.jobs`, 90 days by default) keeps it bounded.

## Installing on Kubernetes

### 1. Install the operator

Each [release](https://github.com/haught/dispatch-ops/releases) publishes the
chart as an OCI artifact on GHCR and in a Helm repository. Install it from
either, choosing a version from the releases page:

```sh
helm install dispatch-ops-operator \
  oci://ghcr.io/haught/dispatch-ops/charts/dispatch-ops-operator --version <version> \
  --namespace dispatch-ops-system --create-namespace
```

```sh
helm repo add dispatch-ops https://haught.github.io/dispatch-ops
helm install dispatch-ops-operator dispatch-ops/dispatch-ops-operator --version <version> \
  --namespace dispatch-ops-system --create-namespace
```

The chart runs the images of its own version from
`ghcr.io/haught/dispatch-ops`. To use a registry mirror, add
`--set image.repository=registry.example.com/dispatch-ops/operator --set serverImage.repository=registry.example.com/dispatch-ops/server`.

The chart installs the `DispatchOps` CRD (group `dispatch-ops.io`, short name
`dops`). The CRD is kept when the chart is uninstalled, so installations
survive. `serverImage` sets the server image that installations without
`spec.image` run, `watchNamespaces` limits the operator to some
namespaces, and `allowedJobNamespaces` lists the namespaces installations
may run jobs in besides their own. See
[Operator permissions](#operator-permissions) for what these mean for
security; in production, set both:

```sh
  --set 'watchNamespaces={dispatch-ops}' --set 'allowedJobNamespaces={dispatch-jobs}'
```

### 2. Create an installation

The smallest installation:

```yaml
apiVersion: dispatch-ops.io/v1alpha1
kind: DispatchOps
metadata:
  name: dispatch-ops
  namespace: dispatch-ops
spec: {}
```

`spec: {}` runs a PostgreSQL the operator manages, two API replicas and one
task manager running jobs in the same namespace. That is fine for a trial;
for production, start from
[deploy/examples/dispatchops.yaml](deploy/examples/dispatchops.yaml), which
uses an external database, an encryption key you provide, an Ingress, a
cert-manager certificate, single sign-on, a separate job namespace and
retention periods. Create the Secrets and namespaces it names first, and
opt the job namespace in (see [Operator permissions](#operator-permissions)).
`kubectl explain dispatchops.spec` documents every field.

`kubectl get dops` shows whether an installation is ready and which image it
runs. Its conditions (`DatabaseReady`, `Migrated`, `APIReady`,
`TaskManagerReady`, `Ready`) say what it is waiting for.

### Or let the chart create the installation

The chart can also create the installation and its Secrets from the
`installation` section of its values:

```yaml
# lab-values.yaml
installation:
  enabled: true
  namespace: dispatch-ops
  externalURL: https://dispatch.lab.example
  logLevel: info
  database:
    existingSecret: dispatch-ops-database       # holds the database URL as "url"
  encryptionKey:
    existingSecret: dispatch-ops-encryption-key # holds the key as "key"
  oidc:
    issuer: https://idp.lab.example/realms/lab
    clientID: dispatch-ops
    existingSecret: dispatch-ops-oidc
  spec:                                         # any other DispatchOps fields
    api:
      ingress:
        host: dispatch.lab.example
```

```sh
helm upgrade --install dispatch-ops-operator \
  oci://ghcr.io/haught/dispatch-ops/charts/dispatch-ops-operator --version <version> \
  --namespace dispatch-ops-system --create-namespace -f lab-values.yaml
```

Instead of `existingSecret`, `database.url`, `encryptionKey.key` and
`oidc.clientSecret` take the values themselves and the chart creates the
Secrets. Prefer `existingSecret`: Helm stores every release's values in the
release history (Secrets named `sh.helm.release.v1.*`), so a value passed to
the chart stays readable there, by anyone who can read Secrets in the
chart's namespace, after you change or remove it. If you do pass values,
keep them in a separate values file out of version control. The
installation's namespace must exist. Helm can't create a `DispatchOps` in the
same install that first creates its CRD, so on a new cluster run
`helm upgrade --install` twice; the chart's notes say when. Uninstalling the
chart keeps the installation and its Secrets unless `installation.keep` is
false.

### 3. Sign in

The first migration creates a local system administrator
(`spec.admin.username`, default `admin`) with the password in the
`<name>-admin-password` Secret, unless a user of that name already exists.
Changing the Secret later doesn't change the password.

```sh
kubectl -n dispatch-ops get secret dispatch-ops-admin-password \
  -o jsonpath='{.data.password}' | base64 -d
```

After signing in, change the password (or set up single sign-on and another
administrator) and delete the Secret, so the initial password doesn't stay
readable in the cluster:

```sh
kubectl -n dispatch-ops delete secret dispatch-ops-admin-password
```

The operator doesn't make it again once the first migration has created the
administrator, and later upgrades migrate without it. Nothing else depends
on it.

To make a single sign-on user an administrator after their first sign-in,
run `dispatch-ops user grant-admin <username>` in an API pod, or grant
`system_admin` to a team mapped from a provider group.

### What the operator creates

For an installation named `<name>`:

| Object | |
|---|---|
| `<name>-api` Deployment and Service | The API server and UI, HTTPS on port 8443 (`https`) |
| `<name>-task-manager` Deployment, ServiceAccount, Role and RoleBinding | The task manager, with the permissions it needs in the job namespace (where the Role and RoleBinding are named `<namespace>-<name>-task-manager` if it isn't the installation's) |
| `<name>-postgres` StatefulSet and Service | The managed database (unless `spec.database.external`) |
| `<name>-migrate-<hash>` Job | Database migrations for one image, then the administrator if missing |
| `<name>` Ingress | If `spec.api.ingress` is set |
| `<name>-api`, `<name>-task-manager`, `<name>-postgres`, `<name>-jobs` NetworkPolicies | See [Network paths](#network-paths) |
| `<name>-encryption-key`, `<name>-admin-password`, `<name>-postgres`, `<name>-postgres-tls`, `<name>-api-tls` Secrets | Generated unless the spec names your own |

Edit the `DispatchOps`, not these objects: the operator reverts changes to
what it created. Changes to Secrets the operator reads (for example the OIDC
client secret) take effect when the pods restart (`kubectl rollout
restart`), except the API certificate, which is reloaded.

### Upgrades

Changing `spec.image` (or upgrading the chart, for installations without
one) first runs the new image's migrations in a Job. The API server and task
manager keep running the old image until the migrations succeed, then roll
out. If migrations fail, the installation stays on the old image with
`Migrated=False`; fix the cause and delete the Job to retry.

Migrations only go forward, so going back to an older image after an upgrade
is not supported. Back up the database before upgrading.

### Backups and the encryption key

- **Back up the encryption key Secret.** Without it, stored credentials and
  notification secrets can't be decrypted. To rotate it, set the new key and
  list the old one in the Secret's `oldKeys` (comma-separated). Values are
  re-encrypted with the new key only when they are next saved, so keep the
  old key listed as long as anything encrypted with it remains.
- **Back up the database.** The managed database has none; use
  `spec.database.external` where that matters.
- Deleting a `DispatchOps` deletes what the operator runs but keeps the
  database volume and the generated Secrets, so recreating it with the same
  name picks up the same data.

### TLS

The API server always serves HTTPS on port 8443. By default the operator
generates a self-signed ECDSA P-384 certificate, valid for one year, in the
`<name>-api-tls` Secret. It covers the Service's names, `localhost`, and the
ingress and `externalURL` hosts. The operator renews it 30 days before it
expires, and reissues it when those hosts change; the API server picks up
the new one without a restart. (The managed database's `<name>-postgres-tls`
certificate is renewed the same way, and its pod restarts to serve it.) A
certificate Secret you provide is never touched. Every installation gets its
own key; none is built into the image.

A renewed self-signed certificate is a new CA too: anything outside the
cluster that pinned the old one (an ingress controller verifying the
backend, or an API client) has to trust the new one.

Job pods trust the certificate (the task manager hands them the Secret's
`ca.crt`). An nginx Ingress gets
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

The API server reloads the certificate when it is renewed, with no restart.
New jobs trust the Secret's `ca.crt`, or its `tls.crt` and the system's roots
when it has no `ca.crt` (as with a public CA). The API pods don't start until
the Secret exists.

`spec.api.tls.enabled: false` serves plain HTTP on port 8080 instead. Use it
only where something else, such as a service mesh, encrypts traffic to the
pods.

### Monitoring

Metrics are on by default (`spec.metrics.enabled`). Both Deployments' pods
serve them on a port named `metrics` (9090, plain HTTP) and carry
`prometheus.io/scrape` annotations; the API Service doesn't expose it. With
the Prometheus Operator, use a PodMonitor selecting
`app.kubernetes.io/name: dispatch-ops` with `port: metrics`.

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
| `dispatch_jobs{status}`, `dispatch_job_queue_oldest_pending_seconds`, `dispatch_notifications_pending` | From the database, reported by the leader only |

Counters are per process; sum them across replicas. Useful alerts:

- `dispatch_job_queue_oldest_pending_seconds` growing: jobs aren't starting.
- `sum(dispatch_taskmanager_leader) == 0`: no task manager is running.
- A rising rate of `dispatch_notifications_total{result="failed"}`.

Health probes are `/healthz` (liveness) and `/readyz` (checks the database).

### Data retention

The task manager deletes finished jobs, with their output and notifications,
`spec.retention.jobs` after they finish (default 90 days; `0` keeps them),
and audit log entries older than `spec.retention.auditLog` (default `0`,
kept forever). It checks hourly. Periods take days (`90d`) or Go durations
(`2160h`).

## Using Dispatch Ops

### Authentication

- **Browser:** sign in with single sign-on or a local username and password.
  Either sets an HTTP-only session cookie. Cross-site state-changing requests
  are rejected.
- **Scripts and CI:** create an API token on the *API tokens* page and send
  `Authorization: Bearer dop_...`. A token can do what its user can; a
  read-only token (`"scope": "read"`) only GET requests. Tokens can only be
  created from a browser session, never with another token. For a service
  account, create a local user and have an administrator create its tokens
  (on the user's page, or `POST /api/v1/users/{id}/tokens`).
- **Job pods:** each job gets a one-time token that only works for that
  job's runner endpoints and stops working when the job ends.
- Only a SHA-256 hash of each token is stored. Passwords use argon2id.
- Deactivating a user ends their sessions; their tokens stop working until
  they are reactivated.

### Single sign-on (OIDC)

Set `spec.externalURL` and `spec.oidc` (issuer, client ID, client secret),
and register `<externalURL>/api/v1/auth/oidc/callback` as the client's
redirect URI. The sign-in page then offers a single sign-on button. The flow
is the authorization code flow with PKCE.

A user is created at their first sign-in, keyed on the issuer and subject.
Their username comes from the username claim (default `preferred_username`,
then `email`, then the subject), and their display name and email from `name`
and `email`; all three are refreshed at each sign-in. A username already used
by another account is refused.

Teams list provider groups. At each sign-in a user joins the teams that list
one of the groups in their groups claim (default `groups`), and leaves the
teams they joined that way if they no longer have it. Members added by an
administrator are left alone. Ask the provider to include the claim (often
with the `groups` scope); nested claims such as Keycloak's
`realm_access.roles` are addressed with dots. If the ID token lacks the
claim, the userinfo endpoint is asked.

The provider is contacted on the first sign-in, not at startup, so the API
keeps serving local users, tokens and jobs while it is unreachable. Signing
out ends the Dispatch Ops session only, not the provider's.

### Access control

Roles are granted to users or teams on the *Access*, *Users* and *Teams*
pages (or `/api/v1/role-grants`):

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
  new ones. Changing what its jobs run, or where (see
  [below](#roles-that-amount-to-credential-access)), also needs `use` on all
  the credentials it keeps. Overriding the inventory at launch needs `use`
  on it.
- A job can be seen by whoever launched it and by anyone with a role on its
  template. It can be canceled by whoever launched it and by those with
  `execute` on its template.
- An inventory's source is set, changed and synced by those with `admin` on
  the inventory (and `use` on the project and credentials they add, or on
  all its credentials when they change its project, path or execution
  environment). Its sync jobs can be seen by anyone with a role on the
  inventory, and canceled by its admins.
- Everyone can use execution environments; only system administrators
  manage them, and users and teams.
- `dispatch-ops user grant-admin <username>` makes an existing user a system
  administrator, for example to recover access.

#### Roles that amount to credential access

A job holds its credentials in plain form while it runs, and whatever the
job runs can read them. So any role that decides what a job runs, or which
hosts it connects to, is as good as the credentials the job runs with.
Grant these roles only to people you would also give the credentials to.

| Role or permission | Can reach | Because |
|---|---|---|
| `admin` on a job template | Its credentials, once they hold `use` on them | It decides the command, playbook, arguments, extra vars, project, inventory and image. Changing any of these, or allowing `command`, `extraVars` or `inventory` to be overridden, needs `use` on all the template's credentials. Other changes (name, limit, tags, schedule) don't. |
| `execute` on a template that allows overriding `command` | Its credentials | The command is the launcher's. |
| `execute` on a template that allows overriding `extraVars` | Possibly its credentials | Extra vars named `ansible_*` and any with Jinja (`{{`, `{%`, `{#`) are refused at launch, so they can't change how Ansible connects or read files. Ordinary variables still steer the playbook, which may be enough to send a secret somewhere, depending on the playbook. |
| `execute` on a template that allows overriding `inventory`, plus `use` on an inventory they control | Its credentials | Host and group variables in their inventory (`ansible_ssh_common_args`, `ansible_host`, Jinja) decide where and how Ansible connects. |
| `admin` on an inventory | The credentials of every template that uses it, including other teams' templates that hold only `use` on it | The same host and group variables. |
| Push access to a project's repository, or to the repository of an inventory source | The credentials of every template and inventory source that uses the project | The repository is the playbook, script or inventory plugin that runs. |
| `admin` on a project | The credentials of every template and inventory source that uses the project | It can change `scmUrl` and `scmRef`, so it picks the code that runs. Moving the project to another server (a different scheme, host or port) while keeping its `scm` credential needs `use` on that credential. |
| `admin` on a credential | Its secrets, only by entering new ones | Changing where the secrets are sent (a registry's `server`, an API token's `url`) needs every secret entered again in the same change. |
| `system_admin` | Everything | It manages execution environments, the images every job runs in. |

Other safeguards: a `limit`, and a host name (which *Relaunch on failed
hosts* puts in one), can't name a file (`@path`), which Ansible would
otherwise read and print into the job log; and the project's `scm` username and
password are given only to the project's own server, not to submodules or
`requirements.yml` sources elsewhere.

### Audit log

Every change made through the API, every launch and cancel, and every
sign-in is recorded with who made it, how they authenticated and when:
creates with the new resource, deletes with the old one, and updates with
the fields that changed. Secrets are never recorded; a credential update
lists which inputs changed. System administrators and auditors can read it
on the *Audit log* page or at `GET /api/v1/audit-log`.

### Job types

| Type | Runs | Needs |
|---|---|---|
| `ansible` | `ansible-playbook` on the template's playbook (`entrypoint`) | A project and a playbook; an EE with ansible-core and git |
| `script` | The `entrypoint` script with the template's `arguments` | A project and a script; an EE with git |
| `container` | The template's `command` | A command |
| `inventory_sync` | `ansible-inventory` on an inventory's source (not a template type; see [Inventory sources](#inventory-sources)) | An inventory source; an EE with ansible-core and git |

Ansible and script jobs run in the project checkout, so a project's
`ansible.cfg` applies. Scripts with a `#!` line run with that interpreter;
others with `/bin/sh`.

### Projects

A project is a git repository (`scmUrl`) and a branch, tag or commit
(`scmRef`, default `main`). Each job clones it fresh (shallow) and records the
commit it ran. URLs may be `https://`, `http://`, `ssh://`, `git://`,
`file://` or scp-like `git@host:path`.

A project may have an `scm` credential: a username and password or token for
HTTP(S), or an SSH private key (decrypted in the pod if it has a passphrase).
SSH host keys are accepted on first use, since job pods start without known
hosts, unless the credential's optional `known_hosts` input lists the
server's keys (in `ssh-keyscan` / `known_hosts` format): then git only
connects to a server presenting one of them. A username and password are given only to the project URL's own
server (scheme, host and port): a submodule or a `requirements.yml` source
on another server gets none. The credential's files are deleted before the
playbook or script starts.

### Ansible jobs

The runner calls `ansible-playbook` with the job's inventory, extra vars,
`--limit`, `--tags`, `--skip-tags` and verbosity, and with the template's
credentials:

| Credential kind | Becomes |
|---|---|
| `ssh` | `-u`, `--private-key`, `--become-password-file` |
| `username_password` | `-u`, `--connection-password-file` (needs `sshpass`), `--become-password-file` |
| `vault` | `--vault-id <id>@file`, or `--vault-password-file` without a vault ID |
| `generic` | Environment variables named after its inputs, as for every job |
| others | `DISPATCH_CREDENTIALS` / `DISPATCH_CREDENTIALS_DIR`, as for every job |

A template takes at most one machine credential (`ssh` or
`username_password`) and one vault password per vault ID. Host key checking
is off unless the machine credential has a `known_hosts` input: then
Ansible connects only to hosts whose key it lists
(`ANSIBLE_HOST_KEY_CHECKING=True`, `StrictHostKeyChecking=yes`). Without
it, whoever can intercept a first connection can capture a connection or
become password. Changing `known_hosts` needs the credential's secrets
given again, like its other destination fields. The job's inventory and
extra vars are written to the in-memory secrets volume, not to disk.

The runner adds a callback plugin, `dispatch_events`, through
`ANSIBLE_CALLBACK_PLUGINS` (this replaces a `callback_plugins` setting in the
project's `ansible.cfg`; Ansible's default directories stay on the path). It
reports each play, task and host result as a structured event, without the
output of `no_log` tasks. The job page shows a per-host summary and each
host's task results.

#### Roles and collections

Before the playbook starts, the runner installs what the project's
requirements files list, in this order:

| File | Command |
|---|---|
| `roles/requirements.yml` | `ansible-galaxy role install -r ...` |
| `collections/requirements.yml` | `ansible-galaxy collection install -r ...` |
| `requirements.yml` | `ansible-galaxy install -r ...` (roles and collections) |

Each may also end in `.yaml`. They install into the job's workspace, which
`ANSIBLE_COLLECTIONS_PATH` and `ANSIBLE_ROLES_PATH` put ahead of Ansible's
default directories (this replaces `collections_path` and `roles_path` in the
project's `ansible.cfg`; playbook-adjacent `collections/` and `roles/`
directories still work). The installs use the project's `scm` credential, so
git sources on the same server as the project need no extra setup (sources
on other servers must be public, or reached over SSH with the key). The
output goes to the job log, the job fails if an install fails, and the
timeout covers the installs. Collections baked into the EE image are still
found, so an image can make a requirements file unnecessary.

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
| *generic credential inputs* | Each input of a `generic` credential, under its own name (for example `AKIPS_PASSWORD`) |

Generic credential names that the job's tools depend on are reserved and
refused when the credential is saved: `PATH`, `HOME`, `USER`, `SHELL`, `PWD`,
`LANG`, `TMPDIR`, `HOSTNAME`, and names starting with `DISPATCH_`,
`ANSIBLE_`, `LD_`, `LC_`, `PYTHON`, `GIT_`, `SSH_` or `KUBERNETES_`. If two
generic credentials on a job set the same name, the later one wins and the
job log says so. This lets tools that read their settings from the
environment, such as Ansible inventory plugins, use a generic credential
directly.

### Inventory sources

An inventory can be synced from a project instead of edited by hand: a static
inventory file or directory, an inventory script, or the YAML configuration
of an inventory plugin. Set it on the inventory's page with a project, a
path in it, an execution environment and optional credentials.

A sync (*Sync now*, a schedule, or relaunching an earlier sync) is an
`inventory_sync` job. It checks the project out, installs its requirements
files as for Ansible jobs, and runs

```
ansible-inventory -i <sourcePath> --list --export
```

in the checkout. A source Ansible can't parse fails the job instead of
emptying the inventory. The result replaces the inventory's hosts and groups
in one transaction: hosts and groups keep their IDs when their names stay,
those no longer reported are deleted, and group variables stay on their
groups. The `all` group's variables are ignored; the inventory's own
variables are yours to edit. At most one sync of an inventory runs at a
time.

While an inventory has a source, its hosts and groups can't be changed by
hand. Removing the source keeps them, editable again.

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

### Execution environments

An execution environment is an image whose entrypoint is `dispatch-runner`
and that runs as a numeric non-root user. The job's own commands run as that
same user, so the runner keeps its job token from them: it gets the token
in its environment and, before starting anything, makes itself
non-dumpable, which hides its environment and memory from other processes of
the same user. So the entrypoint must be `dispatch-runner` itself, or a
wrapper that `exec`s it; a wrapper that starts it as a child keeps a
readable copy of the token. Images built on a runner from before this
change look for a token file job pods no longer mount, and fail with *The
job pod ended before the runner started ... rebuild the image on the current
runner-base*; rebuild them on the current starter or `runner` image. Register it by name and image on
the *Execution environments* page, with a registry credential if it is
private. The starter images, published with each release as
`ghcr.io/haught/dispatch-ops/<name>:<version>`, are:

- `ee-shell`: bash, curl, jq, git and ssh.
- `ee-python`: Python in a virtualenv on `PATH`, with requests and PyYAML.
- `ee-ansible`: ansible-core with `ansible.posix`, `community.general` and
  `ansible.utils`, the requests and netaddr libraries inventory plugins often
  need, plus git, ssh and sshpass.

There are three ways to get what your jobs need, simplest first:

1. **List collections in the project.** Put them in the project's
   `requirements.yml` (see [Roles and collections](#roles-and-collections)).
   No image of your own is needed, but each job takes longer to start.
2. **Extend a starter image.** Start a repository from
   [examples/ee-template](examples/ee-template). It holds a Dockerfile,
   `packages.txt` (Debian packages), `requirements.txt` (Python packages),
   `requirements.yml` (collections and roles), and a GitHub Actions workflow
   that builds, tests and pushes the image. The starter images include
   `dispatch-ee-install`, which puts those files in the right places:

   ```dockerfile
   FROM ghcr.io/haught/dispatch-ops/ee-ansible:1.4
   USER root
   COPY packages.txt requirements.txt requirements.yml /tmp/ee/
   RUN dispatch-ee-install /tmp/ee && rm -rf /tmp/ee
   USER 1000:1000
   ```

3. **Bring your own base image.** Copy the runner in from the `runner` image,
   which holds only the static binary:

   ```dockerfile
   FROM registry.example.com/hardened/python:3.13
   COPY --from=ghcr.io/haught/dispatch-ops/runner:1.4 /dispatch-runner /usr/local/bin/dispatch-runner
   USER 1000:1000
   ENTRYPOINT ["/usr/local/bin/dispatch-runner"]
   ```

Check an image before you register it:

```sh
docker run --rm <image> selftest
```

It verifies that the image runs as a non-root user and has git, ssh and, for
Ansible images, the Ansible tools. It lists the collections, prints the
runner protocol, and exits non-zero when the image can't run jobs. The
template's workflow runs it on every build.

The API rejects runners too old for a job, with an explanation on the job.
Ansible and script jobs need runner protocol 2 or later, and inventory syncs
protocol 3. Rebuilding a custom image on a newer starter image picks up the
new runner; the template's Renovate config opens that pull request for you.

### Schedules

A schedule launches a job template on a cron expression, read in an IANA
time zone (default UTC): five fields (`30 2 * * 1-5`) or `@daily`,
`@hourly`, `@every 6h` and the like, at most once a minute. A schedule may
carry launch overrides, which the template must allow. Add schedules on a
template's page; the form previews the next run times.

A schedule that was due several times while no task manager ran launches
once, then carries on. If a launch fails (say the template no longer allows
its overrides), the schedule keeps running and shows the error. Seeing
schedules needs a role on the template; creating one needs `execute` (and
`use` on an inventory it overrides); changing or deleting one needs `admin`
on the template, or having created it.

A schedule can instead sync an inventory from its source. Those are managed
on the inventory's page by its admins. A run while the last sync is still
going launches nothing and says why.

### Relaunching

The *Relaunch* button launches the job's template again, as it is now, with
the launch overrides the job was started with. The relaunch uses the
template's current project, credentials, execution environment and command,
so it can't bring back anything a template admin has since changed. It needs
`execute` on the template, and the overrides must still be allowed and pass
the same checks as a launch (for example, `use` on an overridden inventory).
If they don't, launch the template instead. Jobs from before overrides were
recorded are relaunched without overrides. Only system administrators can
relaunch a job whose template was deleted; it runs with its original
configuration. *Relaunch on failed hosts* reruns a finished Ansible job with
its limit set to the hosts that failed or were unreachable.

### Notifications

System administrators set up notification targets on the *Notifications*
page; template admins then choose, on the template's page, which targets its
jobs notify and on which events: `started`, `succeeded`, `failed` (which
includes `error`) and `canceled`.

| Kind | Config | Secrets |
|---|---|---|
| `webhook` | `url` | `authorization` (sent as the `Authorization` header), `signingSecret` |
| `slack` | | `webhookUrl` (an incoming webhook) |
| `googlechat` | | `webhookUrl` (a space's incoming webhook) |
| `email` | `host`, `port` (587, or 465 with `tls`), `security` (`starttls`, `tls` or `none`), `username`, `from`, `to` | `password` |

Secrets are encrypted like credentials and never returned. A webhook gets a
POST of `{"event": ..., "job": {...}}` with `X-Dispatch-Event` and
`X-Dispatch-Delivery` headers; with a signing secret, also
`X-Dispatch-Timestamp` and `X-Dispatch-Signature: sha256=<hex>`, the
HMAC-SHA256 of `<timestamp>.<body>`. Check it, and reject old timestamps.
Google Chat gets a card with the job's status and an *Open job* button, and
a job's messages share one thread in the space.

Failures are retried after 30 seconds, 2, 10 and 30 minutes; 4xx responses
and SMTP 5xx replies are not retried. A job's page shows what was sent.
*Test* on a target sends a test message at once. Links to jobs need
`spec.externalURL`.

## Configuration reference

The operator sets these environment variables on the server pods from the
`DispatchOps` spec. You only set them yourself when running the server image
without the operator.

| Variable | Default | |
|---|---|---|
| `DISPATCH_DATABASE_URL` | (required) | PostgreSQL URL |
| `DISPATCH_ENCRYPTION_KEY` | (required) | Encrypts credentials. 32 bytes, base64; `dispatch-ops gen-key` makes one |
| `DISPATCH_OLD_ENCRYPTION_KEYS` | | Comma-separated previous keys, still used to decrypt during a rotation |
| `DISPATCH_LISTEN_ADDR` | `:8443`, or `:8080` without TLS | |
| `DISPATCH_TLS` | `true` | Serve HTTPS. Set `false` only where something else encrypts traffic to the server |
| `DISPATCH_TLS_CERT_FILE`, `DISPATCH_TLS_KEY_FILE` | | A PEM certificate (chain) and key, reloaded when the files change. Without them, the server makes a self-signed ECDSA P-384 certificate at startup, held only in memory |
| `DISPATCH_SECURE_COOKIES` | `true` | Secure, `__Host-` cookies (`__Host-dispatch_session`), which a sibling subdomain or a plain-HTTP response can't plant. Set `false` only for plain-HTTP development; the cookies are then `dispatch_session` and `dispatch_oidc`. Upgrading from a version with plain cookie names signs everyone out once |
| `DISPATCH_SESSION_TTL` | `12h` | Browser session lifetime |
| `DISPATCH_JOB_MAX_OUTPUT` | `256Mi` | Output one job may store (its events' text and data). Past it, the job keeps running and a note says the output was cut; only its Ansible recap is still kept. `0` for no cap |
| `DISPATCH_LOGIN_THROTTLE_WINDOW` | `15m` | Period over which failed password sign-ins are counted |
| `DISPATCH_LOGIN_MAX_FAILURES_PER_USER` | `10` | Failed sign-ins a username may have in the window before further attempts get `429`; a success resets it. `0` turns this limit off |
| `DISPATCH_LOGIN_MAX_FAILURES_PER_IP` | `100` | Failed sign-ins one client address may have in the window. `0` turns this limit off |
| `DISPATCH_TRUSTED_PROXIES` | | Comma- or space-separated addresses or CIDRs of reverse proxies (ingress controller, load balancer) whose `X-Forwarded-For` is believed. Set it behind a proxy, or every client shares the proxy's address for sign-in throttling |
| `DISPATCH_AUTO_MIGRATE` | `false` | Apply migrations when `serve` starts |
| `DISPATCH_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `DISPATCH_LOG_FORMAT` | `json` | `json` or `text` |
| `DISPATCH_EXTERNAL_URL` | | The server's URL as browsers reach it, for example `https://dispatch.example.com`. Needed for single sign-on and for links in notifications |
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
| `DISPATCH_RUNNER_API_TLS_DIR` | | A directory holding the API's certificate Secret (`ca.crt` or `tls.crt`). Job pods trust it, besides the system's roots |
| `DISPATCH_JOB_NAMESPACE` | `dispatch-jobs` | Namespace for job pods (and the leader Lease) |
| `DISPATCH_KUBE_CONTEXT` | | kubeconfig context; empty uses the in-cluster config if available |
| `DISPATCH_MAX_CONCURRENT_JOBS` | `10` | Jobs waiting or running at once |
| `DISPATCH_JOB_START_TIMEOUT` | `10m` | How long a pod may take to start its runner |
| `DISPATCH_JOB_TTL` | `1h` | How long finished Kubernetes Jobs are kept |
| `DISPATCH_JOB_CPU_REQUEST`, `DISPATCH_JOB_MEMORY_REQUEST` | `100m`, `256Mi` | Job pods' requests; `0` for none |
| `DISPATCH_JOB_CPU_LIMIT`, `DISPATCH_JOB_MEMORY_LIMIT` | `2`, `2Gi` | Job pods' limits; `0` for none |
| `DISPATCH_JOB_WORK_SIZE_LIMIT` | `4Gi` | Cap on a job pod's working directory; `0` for none |
| `DISPATCH_LEADER_ELECTION` | `true` | Elect one active task manager with a Lease |
| `DISPATCH_JOB_RETENTION` | `90d` | Delete finished jobs this long after they finish; `0` keeps them |
| `DISPATCH_AUDIT_RETENTION` | `0` | Delete audit log entries this old; `0` keeps them |

## License

GNU General Public License v2.0. See [LICENSE](LICENSE).
