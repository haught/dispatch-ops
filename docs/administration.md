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
| Task manager | Kubernetes API | Create, start, watch and delete Jobs, watch pods, create job Secrets (no other access to Secrets), hold its leader Lease |
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
- `<name>-jobs`, in the job namespace: job pods (every pod there labeled
  `app.kubernetes.io/managed-by=dispatch-ops` and
  `app.kubernetes.io/component=job`, whatever its
  `dispatch-ops.io/installation`, since the namespace serves only this
  installation) accept no connections, and may connect only to the API
  server, the cluster DNS (port 53), and any address outside the cloud
  metadata and credential services: 169.254.0.0/16 (including EKS Pod
  Identity's 169.254.170.23), 100.100.100.200 (Alibaba Cloud),
  168.63.129.16 (Azure WireServer), fe80::/10, fd00:ec2::254 and
  fd00:ec2::23 (AWS, EKS Pod Identity) and fd20:ce::254 (Google Cloud). To
  keep jobs away from the rest of the cluster too, add its pod and service
  CIDRs (and any internal networks jobs shouldn't reach) to
  `spec.networkPolicy.jobEgressExcept`.
- DNS is allowed only to the cluster DNS pods: by default `k8s-app=kube-dns`
  pods in `kube-system` (CoreDNS on most distributions), so port 53 isn't a
  way around `jobEgressExcept`. Elsewhere, set the chart's `jobDNS` values
  (`namespaceSelector`, `podSelector`, `cidrs`), or job pods can't resolve
  names once `jobEgressExcept` covers the pod or service CIDR:
  - **OpenShift**: `namespaceSelector: kubernetes.io/metadata.name=openshift-dns`
    and `podSelector: dns.operator.openshift.io/daemonset-dns=default`. Its
    DNS pods listen on port 5353 behind the Service's 53; the policy allows
    5353 to the selected pods too, since a NetworkPolicy sees the pod's port.
  - **NodeLocal DNSCache**: add its address, `169.254.20.10/32`, to `cidrs`.
    In iptables mode it also answers on the kube-dns Service's ClusterIP
    (`kubectl -n kube-system get svc kube-dns`), intercepting the traffic on
    the node before it reaches a pod, so add that IP as a `/32` too.

  A `podSelector` with an empty `namespaceSelector` selects those pods in
  every namespace. With both selectors empty and no CIDRs, job pods may
  query DNS anywhere.

Behind an ingress controller or load balancer, set `spec.api.trustedProxies`
to its addresses, so sign-in throttling, which counts failed attempts per
client address, sees the client's address from `X-Forwarded-For` rather
than the proxy's. List only proxies that overwrite that header; a client
could otherwise choose its own address. Give the ingress controller pods'
exact IPs, or the narrowest CIDR that holds only them, never the cluster's
whole pod CIDR (such as `10.0.0.0/8`): job pods can reach the API server,
and from a trusted address they could forge `X-Forwarded-For`. It is unset
by default, and the API server logs a warning at startup while it is.

Pod IPs change whenever the controller's pods are rescheduled, so exact IPs
go stale. In practice, give the controller a range no job pod can get and
list that: run it on dedicated nodes, tainted so other pods stay off, and
list those nodes' pod CIDRs; or assign it a dedicated pod IP pool (such as a
Calico `IPPool` or Cilium pool for its namespace). The controller must also
see real client addresses: behind a cloud load balancer, set
`externalTrafficPolicy: Local` on its Service, or enable PROXY protocol
between the load balancer and the controller. Otherwise every client
arrives with a node's or the load balancer's address.

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
  Role in each of them and in each job namespace in
  `allowedJobNamespaces`. Cluster-wide
  it gets only the `DispatchOps` CRD and a ClusterRole,
  `<release>-namespaces`, with `get` on exactly those namespaces (by
  `resourceNames`), to read their Pod Security label. Prefer this in
  production.
- **Job namespaces.** The task manager creates Jobs, pods and Secrets in
  its job namespace, and the operator grants it that. A pod can mount any
  Secret in its namespace, so a `DispatchOps` may only use its own
  namespace, or one that is opted in for its namespace: mapped to it in the
  chart's `allowedJobNamespaces`
  (`{dispatch-jobs: [dispatch-ops]}`; the operator's
  `--allowed-job-namespaces=dispatch-jobs=dispatch-ops`, with `|` between
  several namespaces), or, when the operator watches all namespaces,
  labeled `dispatch-ops.io/allow-jobs-from=<the DispatchOps's namespace>`
  (which takes rights to label namespaces). A `DispatchOps` in any other
  namespace may not use it. Otherwise the installation reports
  `TaskManagerReady=False` (`JobNamespaceNotAllowed`) and the task manager
  isn't deployed. Removing the opt-in (the label, or the entry) takes the
  installation out of the namespace within five minutes (see below), but
  only what the operator can tell is the installation's goes with it:
  objects its task manager created without the installation's labels
  (Secrets, Jobs, pods) may remain. The namespace then keeps a tombstone
  (see [Released namespaces](#released-namespaces)); to hand it to
  someone else, delete and recreate it. A job namespace in
  `allowedJobNamespaces` must not also be in `watchNamespaces`: the chart
  fails, and the operator refuses to start, rather than let one
  installation's task manager create pods next to another's encryption key
  and database Secrets. An entry without the namespaces allowed to use it
  is refused too.
- **One installation per namespace.** A namespace is never shared:
  installations sharing one could read each other's job tokens and
  credentials. An installation holds each namespace it uses with a claim,
  a ConfigMap named `dispatch-ops-claim` that the operator creates there:
  in its own namespace at its first reconcile, before anything else
  (encryption key and database included), and in its job namespace before
  granting its task manager anything there. A claim is only ever created,
  never taken over, so of two installations (even of two operator
  releases) claiming a namespace at once, exactly one gets it. The
  operator never hands a job namespace to a `DispatchOps` when:
  - another installation holds it (`Ready=False`, `JobNamespaceInUse`);
  - it hosts a `DispatchOps`, claimed or not, migrated or not (also
    `JobNamespaceInUse`: its job pods could mount that installation's
    Secrets);
  - another installation's task manager Role or RoleBinding, job
    NetworkPolicy, Jobs or job pods are there (by their
    `dispatch-ops.io/owner-uid` or `dispatch-ops.io/installation` label,
    or their owner), say left by a deleted installation (also
    `JobNamespaceInUse`; the operator's log names that installation's UID,
    and an administrator may delete them);
  - it was another installation's job namespace, or a deleted
    installation's own namespace, and still has its tombstone
    (`JobNamespaceReleased`; see
    [Released namespaces](#released-namespaces)).

  A second `DispatchOps` in a namespace is refused with
  `Ready=False` (`NamespaceInUse`). A refused installation is refused
  outright: nothing is made for it, and its task manager and whatever it
  had in job namespaces are removed. Status messages name the holder only
  when it is in the same namespace, and never give its UID. The holder
  keeps a namespace, however old the others are, until it has left it
  (see below), and even then the others don't get it until an
  administrator has cleared it (see
  [Released namespaces](#released-namespaces)). A change to one `DispatchOps`'s job namespace, or a
  `DispatchOps` created in someone's job namespace, reconciles the others
  concerned at once. Give each installation its own job namespace. A claim on a namespace whose installation is gone for
  good (deleted while the operator was down, say) can be deleted by an
  administrator; only the operator and the namespace's own users can
  write ConfigMaps there, and the task manager can't.
- **Leaving a job namespace.** When an installation loses or leaves a job
  namespace (it is refused, the namespace stops allowing it, its
  `spec.taskManager.jobNamespace` changes, or its `DispatchOps` is
  deleted), the operator deletes its own Jobs there (in the background, so
  their pods and job Secrets go too; jobs still running fail), then the
  task manager's Role and RoleBinding, and the job NetworkPolicy, once
  its job pods have stopped, and last it turns its claim into a tombstone
  (see [Released namespaces](#released-namespaces)). Deleting a
  `DispatchOps` waits for that, then releases the claim on its own
  namespace, to a tombstone too. The operator records the job namespaces
  an installation holds claims in in its status
  (`status.jobNamespaces`), before it claims one, so they are found
  whatever the spec or the opt-in says by then, and even when the
  `DispatchOps`'s own namespace is being deleted. Tenants can't write the
  status unless granted `update` or `patch` on `dispatchops/status`:
  never grant it (nor `dispatchops/*`, or `*` resources), or they could
  make the operator forget a job namespace, and leave their Jobs running
  there. The operator looks for an installation's Jobs and job pods only
  in the namespaces it holds claims in, so a pod labeled for it elsewhere
  can't hold up its deletion.
- **Pod Security.** The operator runs jobs only in namespaces that enforce
  the restricted (or baseline) Pod Security Standard. Label each job
  namespace, including a `DispatchOps`'s own when it runs its jobs there:
  `kubectl label namespace <namespace> pod-security.kubernetes.io/enforce=restricted`
  (job pods comply). Without it, the installation reports
  `TaskManagerReady=False` (`JobNamespacePodSecurity`) and the
  installation leaves the namespace as above, within five minutes of the
  label's removal. The chart's
  `allowJobNamespacesWithoutPodSecurity: true` lifts this, at the cost
  below. The operator needs `get` on those namespaces; with
  `watchNamespaces` set, the chart grants it for the watched and allowed
  job namespaces only.
- **Creating or editing a `DispatchOps`** lets you:
  - choose the server image, and so run any code with the task manager's
    permissions in its namespace and job namespace: create pods there, as
    any of its service accounts, mounting any of its Secrets (where Pod
    Security isn't enforced, privileged pods with the node's filesystem,
    which is root on the node);
  - create an Ingress with any host and annotations (`spec.api.ingress`),
    which your ingress controller may act on (snippets, auth bypasses,
    taking over another hostname);
  - create a `LoadBalancer` or `NodePort` Service (`spec.api.serviceType`),
    exposing the API outside the cluster.

  Grant it only to those who could have those permissions anyway, and
  never `dispatchops/status` (see above).
- The operator never takes over an object it didn't create: if one of the
  names it would use is taken, it reports an error instead. That goes for
  the Secrets it generates too: it labels each with the `DispatchOps`'s
  UID (`dispatch-ops.io/owner-uid`), and on a first install it won't use
  an existing Secret under one of their names without that label
  (`Ready=False`, `ForeignSecret`): someone else (a task manager whose job
  namespace this was, say) could have left it, with values they know.
  Delete it, and the operator generates its own; or, if you made it for
  this installation, label it as the message says. The
  `dispatch-ops.io/generated` annotation alone proves nothing.

### Released namespaces

A namespace isn't handed from one installation to another automatically.
When an installation leaves a job namespace, or its `DispatchOps` is
deleted, the operator doesn't delete its claim, but turns it into a
tombstone: the `dispatch-ops-claim` ConfigMap stays, labeled
`dispatch-ops.io/released` with the role the namespace had, and the
UID of the installation that left in `dispatch-ops.io/released-uid`.

- A **former job namespace** (`dispatch-ops.io/released=job-namespace`):
  the task manager ran the tenant's image there, which could create
  Secrets of any name (such as another installation's generated ones,
  with values it knows), and Jobs whose pods run as any service account
  there, or carry any labels (such as another installation's API's, to
  pass its database's NetworkPolicy), without the installation's labels
  that the operator cleans up by. So it is refused as another
  installation's job namespace (`Ready=False`, `JobNamespaceReleased`)
  and as a `DispatchOps`'s namespace (`NamespaceReleased`). Only the
  installation that left it may take it back (after its opt-in was
  removed for a while, say); one created again under the same name is
  another installation.
- A **deleted installation's namespace**
  (`dispatch-ops.io/released=control-plane`): its generated Secrets (the
  encryption key among them) and its database volume
  (`data-<name>-postgres-0`) are kept there, for job pods to mount. So it
  is refused as anyone's job namespace (`JobNamespaceReleased`). It may
  host a `DispatchOps` again: one of the same name takes over the kept
  Secrets and database, and the tombstone goes (see
  [Backups and the encryption key](#backups-and-the-encryption-key)).

To use such a namespace for something else, an administrator must
delete the tombstone after making sure the namespace holds nothing left by
the previous installation (its Secrets, Jobs, pods and, for a deleted
installation, PersistentVolumeClaims). Better, delete and recreate the
namespace:

```sh
kubectl get configmaps -A -l dispatch-ops.io/released        # the tombstones
kubectl delete namespace dispatch-jobs && kubectl create namespace dispatch-jobs
# or, once it is clean:
kubectl delete configmap -n dispatch-jobs dispatch-ops-claim
```

Whoever can write ConfigMaps in a namespace can delete its tombstone:
in a job namespace, the administrators; in a `DispatchOps`'s own
namespace, its tenants too, who only expose their own kept data by it.

### Optional hardening: admission policy for task managers

The task manager's Role can't say which Jobs it may create: their pod
template and labels are the tenant's image's choice. A
ValidatingAdmissionPolicy (Kubernetes 1.30 or later) can narrow that, so
a task manager's Jobs carry an installation label (the operator deletes
an installation's Jobs by it when it leaves a job namespace) and their
pods run as the namespace's default service account (which the task
manager's job pods do, with no token mounted), not as another of the
namespace's service accounts:

```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata:
  name: dispatch-ops-task-manager-jobs
spec:
  failurePolicy: Fail
  matchConstraints:
    resourceRules:
      - apiGroups: [batch]
        apiVersions: [v1]
        operations: [CREATE, UPDATE]
        resources: [jobs]
  # Requests by task manager service accounts (<name>-task-manager).
  matchConditions:
    - name: task-manager
      expression: >-
        request.userInfo.username.startsWith('system:serviceaccount:') &&
        request.userInfo.username.endsWith('-task-manager')
  validations:
    - expression: >-
        has(object.metadata.labels) &&
        'dispatch-ops.io/installation' in object.metadata.labels &&
        object.metadata.labels['dispatch-ops.io/installation'] != '' &&
        has(object.spec.template.metadata) && has(object.spec.template.metadata.labels) &&
        'dispatch-ops.io/installation' in object.spec.template.metadata.labels &&
        object.spec.template.metadata.labels['dispatch-ops.io/installation'] ==
        object.metadata.labels['dispatch-ops.io/installation']
      message: A task manager's Jobs and their pods must carry its dispatch-ops.io/installation label.
    - expression: >-
        (!has(object.spec.template.spec.serviceAccountName) ||
        object.spec.template.spec.serviceAccountName in ['', 'default']) &&
        (!has(object.spec.template.spec.serviceAccount) ||
        object.spec.template.spec.serviceAccount in ['', 'default'])
      message: A task manager's job pods run as the namespace's default service account.
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicyBinding
metadata:
  name: dispatch-ops-task-manager-jobs
spec:
  policyName: dispatch-ops-task-manager-jobs
  validationActions: [Deny]
```

It matches service accounts by name, so adapt `matchConditions` if other
service accounts in your cluster end in `-task-manager`. It can't check
that the label is the task manager's own installation (its value is the
`DispatchOps`'s UID, which the request doesn't carry), nor what Secrets
the task manager creates, so it narrows what a task manager can leave
behind in a job namespace without replacing the tombstones above.

### Separate job namespaces

A separate job namespace is safer than the default (the installation's own
namespace): `kubectl create namespace dispatch-jobs`, label it for Pod
Security, opt it in, and set `spec.taskManager.jobNamespace`. The task
manager's Role allows only `create` on Secrets: through the Kubernetes API
it can't read, list, change or delete any Secret, its own job Secrets
included (each is owned by its Job and deleted with it). But it creates
pods, and a pod can mount any Secret in its namespace. In a namespace of its own, the
installation's other Secrets (the OIDC client secret, the administrator's
initial password, the database password, the API's private key) are out of
the reach of a compromised task manager. (It holds the encryption key and
database URL either way, since it needs them.) It isn't the default because
the namespace has to be created and opted in first. The task manager gets
only the public half of the API's certificate, never its private key.

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
| `<name>-task-manager` Deployment, ServiceAccount, Role and RoleBinding | The task manager, with the permissions it needs in the job namespace (where the Role and RoleBinding are named `<namespace>-<name>-task-manager-<hash>` if it isn't the installation's). Its leader Lease, `dispatch-ops-task-manager-<hash>`, is per installation, and the Role lets it update no other |
| `<name>-postgres` StatefulSet and Service | The managed database (unless `spec.database.external`) |
| `<name>-migrate-<hash>` Job | Database migrations for one image, then the administrator if missing |
| `<name>` Ingress | If `spec.api.ingress` is set |
| `<name>-api`, `<name>-task-manager`, `<name>-postgres`, `<name>-jobs` NetworkPolicies | See [Network paths and isolation](#network-paths-and-isolation). (A `<name>-jobs-unlabeled` NetworkPolicy from an older operator is deleted: `<name>-jobs` covers those pods.) |
| `<name>-encryption-key`, `<name>-admin-password`, `<name>-postgres`, `<name>-postgres-ca`, `<name>-postgres-tls`, `<name>-api-tls` Secrets | Generated unless the spec names your own, labeled `dispatch-ops.io/owner-uid=<the DispatchOps's UID>` |
| `dispatch-ops-claim` ConfigMap | The installation's claim on its namespace, and on its job namespace; a tombstone once it has left (see [Released namespaces](#released-namespaces)) |

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

The servers verify the managed database's certificate: its URL (the
`<name>-postgres` Secret) uses `sslmode=verify-full` with the database's CA
as the root (`sslrootcert`). The operator generates that CA, valid for ten
years, in `<name>-postgres-ca`, and issues the database's one-year
certificate (`<name>-postgres-tls`) from it. The API server, task manager
and migration pods mount only the CA's certificate, never its key. Renewing
the database's certificate keeps the CA, so the servers, which read it only
when they start, keep their connections; only the database restarts. The CA
itself is renewed a year before it expires, and then the servers restart
too. A self-signed database certificate from an older operator is replaced
by one from the CA, once. URLs an older operator generated
(`sslmode=disable` or `require`) are switched over by the operator.

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

Before upgrading to an operator that requires Pod Security on job
namespaces, label them (see [Operator permissions](#operator-permissions));
until then the task manager isn't deployed. The task manager treats a
Kubernetes Job as its own if its job is in the installation's database,
whatever its `dispatch-ops.io/installation` label (one from before that
label, or from before its `DispatchOps` was created again with a new UID),
or if it has the installation's label; it never deletes one labeled for
another installation whose job isn't in its database. `<name>-jobs`
covers every job pod in the job namespace, labeled or not.

The task manager's Role lets it only create Secrets, each owned by its
Job from the start. A task manager built before that (development builds
before the first release created a job's Secrets first, then patched their
owner) can't set the owner under that Role: while it runs against a newer
operator, as during a rollout, the job fails and its Secrets (its token,
and its registry credentials if any) aren't deleted with its Job. If an
installation ran such a build, delete them once no job runs:
`kubectl delete secret -n <job namespace> -l app.kubernetes.io/managed-by=dispatch-ops,dispatch-ops.io/installation=<DispatchOps UID>`.

Custom execution environments carry their own copy of the runner. The API
rejects a runner too old for a job, with an explanation on the job; rebuild
the image on a current starter image (see
[Execution environments](user-guide.md#execution-environments)).

## Backups and the encryption key

- **Back up the encryption key Secret.** Without it, stored credentials and
  notification secrets can't be decrypted. If the generated
  `<name>-encryption-key` Secret is deleted, the operator doesn't make a new
  key (which would leave everything stored unreadable): the installation
  reports `Degraded=True` and `Ready=False` (`EncryptionKeyMissing`) until
  you restore the Secret, or create it yourself with a new key to start
  over without the stored credentials. To rotate it, set the new key and
  list the old one in the Secret's `oldKeys` (comma-separated). Values are
  re-encrypted with the new key only when they are next saved, so keep the
  old key listed as long as anything encrypted with it remains.
- **Back up the database.** The managed database has none; use
  `spec.database.external` where that matters.
- Deleting a `DispatchOps` deletes what the operator runs, its Kubernetes
  Jobs included (jobs still running fail), but keeps the database volume
  and the generated Secrets, so recreating it with the same name in the
  same namespace picks up the same data: the namespace's tombstone (see
  [Released namespaces](#released-namespaces)) records which installation
  left them, and the new one takes over the Secrets that one generated
  (relabeling them with its own UID) and removes the tombstone. (If it
  ran its jobs in a namespace of their own, that one's tombstone must be
  cleared by an administrator first: the new `DispatchOps` is another
  installation.) A `DispatchOps` of another name there doesn't, and its
  arrival removes the tombstone, so a later one of the first name is a
  first install, refused (`ForeignSecret`) while the old Secrets remain. To start over
  instead, delete the kept Secrets and the `data-<name>-postgres-0`
  PersistentVolumeClaim (or the namespace) first.

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
