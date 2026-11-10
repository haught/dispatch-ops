# User guide

How to sign in, control access, and set up and run jobs. Everything here can
be done in the web UI or through the REST API ([api/openapi.yaml](../api/openapi.yaml)).

- [Signing in](#signing-in)
- [Single sign-on (OIDC)](#single-sign-on-oidc)
- [Access control](#access-control)
- [Audit log](#audit-log)
- [Job types](#job-types)
- [Projects](#projects)
- [Ansible jobs](#ansible-jobs)
- [What the command sees](#what-the-command-sees)
- [Inventory sources](#inventory-sources)
- [Execution environments](#execution-environments)
- [Schedules](#schedules)
- [Relaunching](#relaunching)
- [Notifications](#notifications)

## Signing in

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

For example, launching a job template with a token:

```sh
curl -X POST https://dispatch.example.com/api/v1/job-templates/<template id>/launch \
  -H "Authorization: Bearer $TOKEN"
```

## Single sign-on (OIDC)

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

To make a single sign-on user an administrator after their first sign-in,
run `dispatch-ops user grant-admin <username>` in an API pod, or grant
`system_admin` to a team mapped from a provider group.

## Access control

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
- A job can be seen by anyone with a role on its template; launching it
  grants nothing extra, so a launcher who loses every role on the template
  can no longer see its jobs. It can be canceled by those with `execute` on
  its template, and by whoever launched it while they can still see it.
- An inventory's source is set, changed and synced by those with `admin` on
  the inventory (and `use` on the project and credentials they add, or on
  all its credentials when they change its project, path or execution
  environment). Its sync jobs can be seen by anyone with a role on the
  inventory, and canceled by its admins.
- Everyone can use execution environments; only system administrators
  manage them, and users and teams.
- `dispatch-ops user grant-admin <username>` makes an existing user a system
  administrator, for example to recover access.

### Roles that amount to credential access

A job holds its credentials in plain form while it runs, and whatever the
job runs can read them. So any role that decides what a job runs, or which
hosts it connects to, is as good as the credentials the job runs with.
Grant these roles only to people you would also give the credentials to.

| Role or permission | Can reach | Because |
|---|---|---|
| `admin` on a job template | Its credentials, once they hold `use` on them | It decides the command, playbook, arguments, extra vars, project, inventory and image, the verbosity, tags, skipped tags and limit (tags and a limit can skip a playbook's guard tasks or plays), and which credentials the job gets (one credential's variables, like `VAULT_ADDR`, can decide where another's secrets, like `VAULT_TOKEN`, are sent). Changing any of these, adding or removing a credential, or allowing `command`, `extraVars`, `inventory`, `tags`, `skipTags` or `limit` to be overridden, needs `use` on all the credentials the template will have (not on those it removes). Other changes (name, description, timeout, schedule) don't. |
| `execute` on a template that allows overriding `command` | Its credentials | The command is the launcher's. |
| `execute` on a template that allows overriding `extraVars` | Possibly its credentials | Extra vars named `ansible_*` and any with Jinja (`{{`, `{%`, `{#`) are refused at launch, so they can't change how Ansible connects or read files. Ordinary variables still steer the playbook, which may be enough to send a secret somewhere, depending on the playbook. |
| `execute` on a template that allows overriding `inventory`, plus `use` on an inventory they control | Its credentials | Host and group variables in their inventory (`ansible_ssh_common_args`, `ansible_host`, Jinja) decide where and how Ansible connects. |
| `admin` on an inventory | The credentials of every template that uses it, including other teams' templates that hold only `use` on it | The same host and group variables. Changing its source's project, path, image or credentials needs `use` on all the credentials the source will have. |
| Push access to a project's repository, or to the repository of an inventory source | The credentials of every template and inventory source that uses the project | The repository is the playbook, script or inventory plugin that runs. |
| `admin` on a project | The credentials of every template and inventory source that uses the project | It can change `scmUrl` and `scmRef`, so it picks the code that runs. Moving the project to another server (a different scheme, host or port) while keeping its `scm` credential needs `use` on that credential. |
| `admin` on a credential | Its secrets, only by entering new ones | Changing where the secrets are sent (a registry's `server`, an API token's `url`) needs every secret entered again in the same change. |
| `system_admin` | Everything | It manages execution environments, the images every job runs in. |

Other safeguards: a job whose generic credentials set the same
environment variable (compared ignoring case) is refused, and so is a
template or inventory source that would get such credentials, rather than
one silently overriding the other; a `limit`, and a host name (which *Relaunch on failed
hosts* puts in one), can't name a file (`@path`), which Ansible would
otherwise read and print into the job log; and the project's `scm` username
and password are given only to the project's own server, not to submodules
or `requirements.yml` sources elsewhere.

## Audit log

Every change made through the API, every launch and cancel, and every
sign-in is recorded with who made it, how they authenticated and when:
creates with the new resource, deletes with the old one, and updates with
the fields that changed. Secrets are never recorded; a credential update
lists which inputs changed. The server also records, with no actor, when a
sign-in limit starts refusing attempts (`login_throttled`, once per limit
per throttle window, with the client address and, for a real account, the
user), and when a schedule turns itself off because its owner may no longer
launch it (an `update` of the schedule with the reason in `lastError`). System administrators and auditors can read it
on the *Audit log* page or at `GET /api/v1/audit-log`.

## Job types

| Type | Runs | Needs |
|---|---|---|
| `ansible` | `ansible-playbook` on the template's playbook (`entrypoint`) | A project and a playbook; an EE with ansible-core and git |
| `script` | The `entrypoint` script with the template's `arguments` | A project and a script; an EE with git |
| `container` | The template's `command` | A command |
| `inventory_sync` | `ansible-inventory` on an inventory's source (not a template type; see [Inventory sources](#inventory-sources)) | An inventory source; an EE with ansible-core and git |

Ansible and script jobs run in the project checkout, so a project's
`ansible.cfg` applies. Scripts with a `#!` line run with that interpreter;
others with `/bin/sh`.

A job template holds everything a job needs. Its `allowOverrides` list says
which fields (such as `limit`, `tags`, `extraVars` or `inventory`) a launcher
may change at launch; in the UI, these are the fields ticked **Prompt on
launch**.

**Launch** opens a dialog summarizing what the job will run with, where the
prompted fields can be changed for that launch only. They start as the
template's values, and only those that differ are sent. Extra variables merge
over the template's at the top level, so removing one there doesn't unset it.

## Projects

A project is a git repository (`scmUrl`) and a branch, tag or commit
(`scmRef`, default `main`). Each job clones it fresh (shallow) and records the
commit it ran. URLs may be `https://`, `http://`, `ssh://`, `git://`,
`file://` or scp-like `git@host:path`.

A project may have an `scm` credential: a username and password or token for
HTTP(S), or an SSH private key (decrypted in the pod if it has a passphrase).
SSH host keys are accepted on first use, since job pods start without known
hosts, unless the credential's optional `known_hosts` input lists the
server's keys (in `ssh-keyscan` / `known_hosts` format): then git only
connects to a server presenting one of them. A username and password are
given only to the project URL's own server (scheme, host and port): a
submodule or a `requirements.yml` source on another server gets none. The
credential's files are deleted before the playbook or script starts.

## Ansible jobs

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

### Roles and collections

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

## What the command sees

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
`ANSIBLE_`, `LD_`, `LC_`, `PYTHON`, `GIT_`, `SSH_` or `KUBERNETES_`. Two
generic credentials on a template or inventory source can't set the same
name (compared ignoring case), since one would decide where the other's
secrets go; if they come to (a credential edited later), the job fails
before it runs, naming both. This lets tools that read their settings from the
environment, such as Ansible inventory plugins, use a generic credential
directly.

## Inventory sources

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

## Execution environments

An execution environment (EE) is the container image a job runs in. Register
it by name and image on the *Execution environments* page, with a registry
credential if it is private. The starter images, published with each release
as `ghcr.io/haught/dispatch-ops/<name>:<version>`, are:

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
   [examples/ee-template](../examples/ee-template). It holds a Dockerfile,
   `packages.txt` (Debian packages), `requirements.txt` (Python packages),
   `requirements.yml` (collections and roles), and a GitHub Actions workflow
   that builds, tests and pushes the image. The starter images include
   `dispatch-ee-install`, which puts those files in the right places:

   ```dockerfile
   FROM ghcr.io/haught/dispatch-ops/ee-ansible:<version>
   USER root
   COPY packages.txt requirements.txt requirements.yml /tmp/ee/
   RUN dispatch-ee-install /tmp/ee && rm -rf /tmp/ee
   USER 1000:1000
   ```

3. **Bring your own base image.** Copy the runner in from the `runner` image,
   which holds only the static binary:

   ```dockerfile
   FROM registry.example.com/hardened/python:3.13
   COPY --from=ghcr.io/haught/dispatch-ops/runner:<version> /dispatch-runner /usr/local/bin/dispatch-runner
   USER 1000:1000
   ENTRYPOINT ["/usr/local/bin/dispatch-runner"]
   ```

An EE must run as a numeric non-root user, and its entrypoint must be
`dispatch-runner` itself or a wrapper that `exec`s it. The job's commands run
as the same user as the runner; the runner keeps its job token from them by
making itself non-dumpable before starting anything, so a wrapper that
starts it as a child process would leave a readable copy of the token.

Check an image before you register it:

```sh
docker run --rm <image> selftest
```

It verifies that the image runs as a non-root user and has git, ssh and, for
Ansible images, the Ansible tools. It lists the collections, prints the
runner protocol, and exits non-zero when the image can't run jobs. The
template's workflow runs it on every build.

The API rejects runners too old for a job, with an explanation on the job.
Rebuilding a custom image on a newer starter image picks up the new runner;
the template's Renovate config opens that pull request for you.

## Schedules

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

## Relaunching

The *Relaunch* button launches the job's template again, as it is now, with
the launch overrides the job was started with. The relaunch uses the
template's current project, credentials, execution environment and command,
so it can't bring back anything a template admin has since changed. It needs
`execute` on the template, and the overrides must still be allowed and pass
the same checks as a launch (for example, `use` on an overridden inventory).
If they don't, launch the template instead. Only system administrators can
relaunch a job whose template was deleted; it runs with its original
configuration.

*Relaunch on failed hosts* reruns a finished Ansible job with its limit set
to the hosts that failed or were unreachable.

## Notifications

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
