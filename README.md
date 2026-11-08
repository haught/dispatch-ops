# Dispatch Ops

A Kubernetes-native automation platform for running Ansible playbooks, scripts
and container commands as jobs. See [PLAN.md](PLAN.md) for the design and
roadmap.

**Status:** milestone 1 (skeleton). Includes local sign-in, API tokens, the
database schema and the UI shell. Jobs do not run yet.

## Layout

| Path | What it is |
|---|---|
| `api/openapi.yaml` | The API contract. The Go server and the TS client are generated from it. |
| `cmd/dispatch-ops` | The binary: `serve`, `migrate`, `user create` |
| `internal/api` | HTTP handlers (`handlers.go`) and request pipeline (`router.go`) |
| `internal/api/apigen` | Generated server code. Do not edit by hand. |
| `internal/auth` | Password hashing, tokens, request principal |
| `internal/store` | PostgreSQL queries |
| `internal/db` | Connection pool and embedded SQL migrations |
| `internal/server` | Top-level HTTP handler: API, health probes, embedded UI |
| `web/` | SvelteKit 3 / Svelte 5 single-page app, embedded into the binary |

## Development

Requires Go 1.27+, Node 25+, and Docker.

```sh
make setup-env   # create .env from .env.example
make deps        # Go modules + npm packages
make db-up       # Postgres on localhost:5433
make migrate     # apply migrations
make admin       # create user "admin"; prints a generated password
make web         # build the UI (embedded by the Go build)
make run         # http://localhost:8080
```

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
make test-db   # all tests, against a dispatch_test database in the dev Postgres
make check     # go vet, gofmt, svelte-check, prettier
```

## Authentication

- **Browser:** `POST /api/v1/auth/login` sets an HTTP-only `dispatch_session`
  cookie. Cross-site state-changing requests are rejected by Go's
  `http.CrossOriginProtection`.
- **Scripts/CI:** create an API token on the *API tokens* page and send
  `Authorization: Bearer dop_…`. API tokens cannot create more tokens.
- Only a SHA-256 hash of each token is stored. Passwords use argon2id.

OIDC sign-in and RBAC arrive in milestone 4.

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

## Container image

```sh
make docker
docker run --rm -e DISPATCH_DATABASE_URL=… -p 8080:8080 dispatch-ops:dev
```

Health probes: `/healthz` (liveness) and `/readyz` (checks the database).
