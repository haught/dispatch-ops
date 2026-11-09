# Administration guide

How Dispatch Ops runs on Kubernetes, and what to know when operating it in
production. For a first installation, start with the
[quick start in the README](../README.md#quick-start).

- [Architecture](#architecture)
- [Requirements](#requirements)
- [Network paths and isolation](#network-paths-and-isolation)
- [Operator permissions](#operator-permissions)
- [Installing with the chart's values](#installing-with-the-charts-values)
- [What the operator creates](#what-the-operator-creates)
- [Sizing](#sizing)
- [TLS](#tls)
- [Upgrades](#upgrades)
- [Backups and the encryption key](#backups-and-the-encryption-key)
- [Monitoring](#monitoring)
- [Data retention](#data-retention)

## Architecture

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

## Requirements

| | Requirement |
|---|---|
| **Kubernetes** | A cluster you can install a CRD and the operator into. Job pods run in a namespace you choose. A network plugin that enforces NetworkPolicy (Calico, Cilium and so on) for the isolation described under [Network paths and isolation](#network-paths-and-isolation). |
| **PostgreSQL** | 15 or later. Use an external, backed-up database for production. The operator can run one for you (PostgreSQL 17, digest-pinned, on a 10 Gi PersistentVolume by default, as a non-root user with a read-only root filesystem, serving TLS), but it has no backups or replication. |
| **Container registry** | Access to `ghcr.io`, where releases publish the server, operator, runner and starter execution environment images (`ghcr.io/haught/dispatch-ops/<name>`, amd64 and arm64) that the Helm chart uses by default. Without it, mirror them, or build your own (see [Building images and releases](../DEVELOP.md#building-images-and-releases)), into a registry your cluster can pull from and set `image.repository` and `serverImage.repository`. Your own execution environments go in a registry too. |
| **Ingress** | An ingress controller and a DNS name for the UI and API. The API server speaks HTTPS to the ingress controller too. |
| **TLS certificate** | Optional. Without one, the operator generates a self-signed certificate for the API server. cert-manager works well. |
| **OIDC provider** | Optional. Any provider that supports the authorization code flow with PKCE (Keycloak, Entra ID, Okta, Google and so on). |
| **Prometheus** | Optional. Pods carry scrape annotations; a PodMonitor works with the Prometheus Operator. |
| **SMTP server** | Optional, for email notifications. |

## Network paths and isolation

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

## Operator permissions

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

## Installing with the chart's values

Instead of applying a `DispatchOps` yourself, the chart can create the
installation and its Secrets from the `installation` section of its values:

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
keep them in a separate values file out of version control.

The installation's namespace must exist. Helm can't create a `DispatchOps`
in the same install that first creates its CRD, so on a new cluster run
`helm upgrade --install` twice; the chart's notes say when. Uninstalling the
chart keeps the installation and its Secrets unless `installation.keep` is
false.

## What the operator creates

For an installation named `<name>`:

| Object | |
|---|---|
| `<name>-api` Deployment and Service | The API server and UI, HTTPS on port 8443 (`https`) |
| `<name>-task-manager` Deployment, ServiceAccount, Role and RoleBinding | The task manager, with the permissions it needs in the job namespace (where the Role and RoleBinding are named `<namespace>-<name>-task-manager` if it isn't the installation's) |
| `<name>-postgres` StatefulSet and Service | The managed database (unless `spec.database.external`) |
| `<name>-migrate-<hash>` Job | Database migrations for one image, then the administrator if missing |
| `<name>` Ingress | If `spec.api.ingress` is set |
| `<name>-api`, `<name>-task-manager`, `<name>-postgres`, `<name>-jobs` NetworkPolicies | See [Network paths and isolation](#network-paths-and-isolation) |
| `<name>-encryption-key`, `<name>-admin-password`, `<name>-postgres`, `<name>-postgres-tls`, `<name>-api-tls` Secrets | Generated unless the spec names your own |

Edit the `DispatchOps`, not these objects: the operator reverts changes to
what it created. Changes to Secrets the operator reads (for example the OIDC
client secret) take effect when the pods restart (`kubectl rollout
restart`), except the API certificate, which is reloaded.

`kubectl get dops` shows whether an installation is ready and which image it
runs. Its conditions (`DatabaseReady`, `Migrated`, `APIReady`,
`TaskManagerReady`, `Ready`) say what it is waiting for.
`kubectl explain dispatchops.spec` documents every field.

The first migration creates a local system administrator
(`spec.admin.username`, default `admin`) with the password in the
`<name>-admin-password` Secret, unless a user of that name already exists.
Changing the Secret later doesn't change the password. Once the
administrator exists the operator doesn't recreate the Secret, and nothing
else depends on it, so delete it after the first sign-in.

## Sizing

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

## TLS

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

## Upgrades

Changing `spec.image` (or upgrading the chart, for installations without
one) first runs the new image's migrations in a Job. The API server and task
manager keep running the old image until the migrations succeed, then roll
out. If migrations fail, the installation stays on the old image with
`Migrated=False`; fix the cause and delete the Job to retry.

Migrations only go forward, so going back to an older image after an upgrade
is not supported. Back up the database before upgrading.

Custom execution environments carry their own copy of the runner. The API
rejects a runner too old for a job, with an explanation on the job; rebuild
the image on a current starter image (see
[Execution environments](user-guide.md#execution-environments)).

## Backups and the encryption key

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

## Monitoring

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

## Data retention

The task manager deletes finished jobs, with their output and notifications,
`spec.retention.jobs` after they finish (default 90 days; `0` keeps them),
and audit log entries older than `spec.retention.auditLog` (default `0`,
kept forever). It checks hourly. Periods take days (`90d`) or Go durations
(`2160h`).
