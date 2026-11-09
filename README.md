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

- [Features](#features)
- [How it works](#how-it-works)
- [Requirements](#requirements)
- [Quick start](#quick-start)
- [Going to production](#going-to-production)
- [Upgrading](#upgrading)
- [Documentation](#documentation)
- [License](#license)

## Features

- **Three kinds of job:** Ansible playbooks, scripts from a git repository,
  and any container command, each in its own pod.
- **Live output** in the browser, with per-host results for Ansible jobs.
- **Job templates** that hold everything a job needs, with the fields a
  launcher may override; relaunch a job, or rerun it on just the hosts that
  failed.
- **Schedules** on cron expressions, in any time zone.
- **Projects** from git, with Ansible roles and collections installed from
  the project's requirements files.
- **Inventories** edited by hand or synced from a static file, a script or an
  inventory plugin.
- **Credentials** (SSH keys, passwords, vault passwords, API tokens, generic
  secrets), encrypted at rest and never shown again once saved.
- **Execution environments:** the container images jobs run in. Three
  starter images are provided; extend them or bring your own.
- **Single sign-on** with any OIDC provider, with groups mapped to teams, plus
  local accounts and API tokens.
- **Role-based access control** per resource or resource type, and an
  **audit log** of every change, launch and sign-in.
- **Notifications** to webhooks, Slack, Google Chat or email.
- **Prometheus metrics**, retention periods, and a documented REST API
  ([api/openapi.yaml](api/openapi.yaml)) for everything the UI does.

## How it works

```
  Browser, API clients
          |
          v
  +----------------+       +--------------+       +----------------+
  |  API server    |<----->|  PostgreSQL  |<----->|  Task manager  |
  |  REST API + UI |       |  state, queue|       |  (leader)      |
  +----------------+       +--------------+       +-------+--------+
          ^                                               |
          |  output, status                               | creates Kubernetes Jobs
          |                                               v
          |                                       +----------------+
          +---------------------------------------|  Job pod       |
                                                  |  runner + EE   |
                                                  +-------+--------+
                                                          |
                                                          v
                                          git, managed hosts, cloud APIs
```

The **API server** serves the web UI and REST API. The **task manager**
takes queued jobs and runs each as a Kubernetes Job. Inside the job's pod,
the **runner** clones the project, runs the playbook, script or command, and
streams output back to the API. **PostgreSQL** holds all state and doubles
as the job queue, so there is no message broker. The **operator** deploys
and upgrades all of this from a single `DispatchOps` resource.

## Requirements

- **Kubernetes**, where you can install a CRD and an operator. A network
  plugin that enforces NetworkPolicy (Calico, Cilium and so on) is
  recommended, to isolate job pods.
- **Helm 3**, to install the operator.
- **PostgreSQL 15 or later.** For a trial, the operator can run one for you;
  for production, use your own backed-up database.
- **Access to `ghcr.io`**, where the images are published (or a mirror).
- Optional: an ingress controller and DNS name, an OIDC provider,
  cert-manager, Prometheus, and an SMTP server.

## Quick start

### 1. Install the operator

Pick a version from the [releases page](https://github.com/haught/dispatch-ops/releases):

```sh
helm install dispatch-ops-operator \
  oci://ghcr.io/haught/dispatch-ops/charts/dispatch-ops-operator --version <version> \
  --namespace dispatch-ops-system --create-namespace
```

The chart is also in a Helm repository:
`helm repo add dispatch-ops https://haught.github.io/dispatch-ops`.

### 2. Create an installation

```sh
kubectl create namespace dispatch-ops
kubectl apply -f - <<'EOF'
apiVersion: dispatch-ops.io/v1alpha1
kind: DispatchOps
metadata:
  name: dispatch-ops
  namespace: dispatch-ops
spec: {}
EOF
```

`spec: {}` runs a PostgreSQL the operator manages, two API server replicas
and a task manager that runs jobs in the same namespace. That is fine for
trying it out; see [Going to production](#going-to-production) for more.

Wait until it is ready:

```sh
kubectl -n dispatch-ops get dops
```

### 3. Sign in

The operator creates a local administrator, `admin`, with a generated
password:

```sh
kubectl -n dispatch-ops get secret dispatch-ops-admin-password \
  -o jsonpath='{.data.password}' | base64 -d
```

Without an ingress, forward the API port and open https://localhost:8443
(the certificate is self-signed, so the browser will warn you):

```sh
kubectl -n dispatch-ops port-forward svc/dispatch-ops-api 8443:8443
```

After signing in, change the password and delete the Secret, so it doesn't
stay readable in the cluster:

```sh
kubectl -n dispatch-ops delete secret dispatch-ops-admin-password
```

### 4. Run your first job

1. Under **Execution environments**, add one named `ee-shell` with the image
   `ghcr.io/haught/dispatch-ops/ee-shell:<version>`.
2. Under **Job templates**, create a template with the job type `container`,
   the `ee-shell` execution environment, and a command such as
   `bash -c 'echo hello from $(hostname)'`.
3. Launch it. The job page streams the output as the pod runs.

From here, add a project (a git repository), an inventory and credentials to
run Ansible playbooks. The [user guide](docs/user-guide.md) covers each.

## Going to production

Start from [deploy/examples/dispatchops.yaml](deploy/examples/dispatchops.yaml),
a production-style installation, and create the Secrets and namespaces it
names first. In short:

- **Use an external, backed-up database** (`spec.database.external`).
- **Provide and back up the encryption key** (`spec.encryptionKeySecret`).
  Without it, stored credentials can't be decrypted.
- **Limit the operator** to the namespaces it manages, and **run jobs in a
  separate namespace**. Install the chart with
  `--set 'watchNamespaces={dispatch-ops}' --set 'allowedJobNamespaces={dispatch-jobs}'`
  and set `spec.taskManager.jobNamespace`.
- **Expose it through an ingress** (`spec.api.ingress`) with a proper
  certificate, and set `spec.externalURL` and `spec.api.trustedProxies`.
- **Set up single sign-on** (`spec.oidc`) and map provider groups to teams.
- **Review who can change what jobs run.** Some roles are as good as the
  credentials a job uses; see
  [Roles that amount to credential access](docs/user-guide.md#roles-that-amount-to-credential-access).

The [administration guide](docs/administration.md) explains each of these,
along with network isolation, TLS, sizing, monitoring and retention. The
chart can also create the installation for you from its values; see
[Installing with the chart's values](docs/administration.md#installing-with-the-charts-values).

## Upgrading

Upgrade the chart (or change `spec.image`). The operator runs the new
version's database migrations first, and only rolls out the new version once
they succeed. Migrations only go forward, so **back up the database before
upgrading**. See [Upgrades](docs/administration.md#upgrades).

## Documentation

| | |
|---|---|
| [Administration guide](docs/administration.md) | Architecture, production setup, security, TLS, upgrades, backups, monitoring |
| [User guide](docs/user-guide.md) | Signing in, access control, projects, inventories, credentials, execution environments, schedules, notifications |
| [Configuration reference](docs/configuration.md) | Environment variables and command-line commands |
| [REST API](api/openapi.yaml) | The OpenAPI specification |
| [Execution environment template](examples/ee-template) | Starting point for building your own job images |
| [DEVELOP.md](DEVELOP.md) | Working on Dispatch Ops itself |

## License

GNU General Public License v2.0. See [LICENSE](LICENSE).
