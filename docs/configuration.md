# Configuration reference

On Kubernetes you configure Dispatch Ops through the `DispatchOps` resource;
`kubectl explain dispatchops.spec` documents every field, and
[deploy/examples/dispatchops.yaml](../deploy/examples/dispatchops.yaml) shows
a production-style installation.

The operator turns that spec into the environment variables below on the
server pods. You only set them yourself when running the server image
without the operator.

## API server and shared settings

| Variable | Default | |
|---|---|---|
| `DISPATCH_DATABASE_URL` | (required) | PostgreSQL URL |
| `DISPATCH_ENCRYPTION_KEY` | (required) | Encrypts credentials. 32 bytes, base64; `dispatch-ops gen-key` makes one |
| `DISPATCH_OLD_ENCRYPTION_KEYS` | | Comma-separated previous keys, still used to decrypt during a rotation |
| `DISPATCH_LISTEN_ADDR` | `:8443`, or `:8080` without TLS | |
| `DISPATCH_TLS` | `true` | Serve HTTPS. Set `false` only where something else encrypts traffic to the server |
| `DISPATCH_TLS_CERT_FILE`, `DISPATCH_TLS_KEY_FILE` | | A PEM certificate (chain) and key, reloaded when the files change. Without them, the server makes a self-signed ECDSA P-384 certificate at startup, held only in memory |
| `DISPATCH_SECURE_COOKIES` | `true` | Secure, `__Host-` cookies (`__Host-dispatch_session`), which a sibling subdomain or a plain-HTTP response can't plant. Set `false` only for plain-HTTP development; the cookies are then `dispatch_session` and `dispatch_oidc` |
| `DISPATCH_SESSION_TTL` | `12h` | Browser session lifetime |
| `DISPATCH_JOB_MAX_OUTPUT` | `256Mi` | Output one job may store (its events' text and data). Past it, the job keeps running and a note says the output was cut; only its Ansible recap is still kept. `0` for no cap |
| `DISPATCH_LOGIN_THROTTLE_WINDOW` | `15m` | Period over which failed password sign-ins are counted |
| `DISPATCH_LOGIN_MAX_FAILURES_PER_USER` | `10` | Failed sign-ins a username may have from one client address in the window before further attempts for it from there get `429`; a success from that address resets it. Other addresses are unaffected, so a stranger can't lock the account's owner out this way. `0` turns this limit off |
| `DISPATCH_LOGIN_MAX_FAILURES_PER_USER_TOTAL` | `100` | Failed sign-ins a username may have from all addresses together in the window before attempts for it get `429`, except from addresses it has signed in from with its password in the last 90 days. It is set high so that locking an account out takes many addresses. `0` turns this limit off |
| `DISPATCH_LOGIN_MAX_FAILURES_PER_IP` | `100` | Failed sign-ins one client address may have in the window, for any usernames. IPv6 clients are counted by their /64. `0` turns this limit off |
| `DISPATCH_TRUSTED_PROXIES` | | Comma- or space-separated addresses or CIDRs of reverse proxies (ingress controller, load balancer) whose `X-Forwarded-For` is believed. Set it behind a proxy, or every client shares the proxy's address for sign-in throttling (the server logs a warning at startup while it is empty). List only the proxies themselves, never a range that includes job pods; see [Network paths and isolation](administration.md#network-paths-and-isolation) for ranges that survive pod restarts |
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

### Sign-in throttling and account lockout

The limit on a username's failures from all addresses
(`DISPATCH_LOGIN_MAX_FAILURES_PER_USER_TOTAL`) stops a password being
guessed from many addresses, but anyone with enough addresses could reach it
by failing against the account. IPv6 makes that cheap: each /64 counts as
one address, and a single /56 holds 256 of them. So the total doesn't apply
to an address the username has signed in from with its password in the last
90 days (an IPv4 address, or an IPv6 /64): the account's owner can still
sign in from the addresses they use, while attempts from anywhere else are
refused until the failures age out of the window. The per-address limits
(`DISPATCH_LOGIN_MAX_FAILURES_PER_USER` and
`DISPATCH_LOGIN_MAX_FAILURES_PER_IP`) still apply to known addresses.

The owner is still locked out of password sign-in from a new address, such
as a first sign-in from home. Single sign-on works as usual, and API tokens
keep working, so an administrator caught this way can still sign in with
SSO, or act through the API with a token. If the lockouts matter more than
the guessing they stop, raise the limit, or set it to `0` to turn it off and
rely on the per-address limits. The `DispatchOps` resource doesn't
expose the throttle settings, so under the operator they keep their
defaults.

## Task manager only

| Variable | Default | |
|---|---|---|
| `DISPATCH_RUNNER_API_URL` | (required) | The API URL as job pods reach it |
| `DISPATCH_RUNNER_API_TLS_DIR` | | A directory holding the API's certificate Secret (`ca.crt` or `tls.crt`). Job pods trust it, besides the system's roots |
| `DISPATCH_JOB_NAMESPACE` | `dispatch-ops-jobs` | Namespace for job pods (and the leader Lease). It must serve only this installation |
| `DISPATCH_INSTALLATION` | | Labels the Kubernetes objects the task manager creates (`dispatch-ops.io/installation`) and names its leader Lease; it deletes only Kubernetes Jobs with its own label or whose job is in its database, never one labeled for another installation whose job isn't. The operator sets it to the `DispatchOps`'s UID |
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

## Command line

The server binary, `dispatch-ops`, has these commands:

| Command | |
|---|---|
| `serve` | Run the API server and web UI |
| `task-manager` | Run the task manager, which runs jobs on Kubernetes |
| `migrate up` / `down` / `version` | Apply pending migrations, roll back the latest one, or show the current version |
| `migrate force <version>` | Mark a version as applied after fixing a failed migration |
| `user create` | Create a local user (`dispatch-ops user create -h` for its flags) |
| `user grant-admin <username>` | Make an existing user a system administrator |
| `gen-key` | Print a new random credential encryption key |
| `version` | Print the version |
