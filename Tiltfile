# Development loop: everything runs in the kind cluster, through the operator.
# Start it with `make dev` (or `make kind-up` then `tilt up`); the Tilt UI is
# at http://localhost:10350 and the app at http://localhost:8080.
#
# Changing a file rebuilds only what depends on it:
#   web/**              -> web-build -> server-compile -> server image
#   cmd/dispatch-ops, internal/**
#                       -> server-compile -> server image
#   server image        -> DispatchOps spec.image; the operator migrates the
#                          database, then rolls the API and task manager
#   operator code, chart -> operator-compile -> operator image -> rollout
#   runner code, images/ -> ees: rebuilds runner-base and the EEs, loads them
#                          into kind for the next job
#
# Configuration (database, encryption key, OIDC) comes from .env.

CONTEXT = 'kind-dispatch-ops'
NAMESPACE = 'dispatch-ops'
OPERATOR_NAMESPACE = 'dispatch-ops-system'
JOB_NAMESPACE = 'dispatch-jobs'
CHART = 'deploy/helm/dispatch-ops-operator'

if k8s_context() != CONTEXT:
    fail('kubectl context is %r, not %r: run `make kind-up` (or `make kind-join` in the dev container)' % (k8s_context(), CONTEXT))

# --- .env ----------------------------------------------------------------------

def read_dotenv(path):
    env = {}
    for line in str(read_file(path, '')).splitlines():
        line = line.strip()
        if not line or line.startswith('#') or '=' not in line:
            continue
        key, value = line.split('=', 1)
        value = value.strip()
        if len(value) >= 2 and value[0] == value[-1] and value[0] in '"\'':
            value = value[1:-1]
        env[key.strip()] = value
    return env

env = read_dotenv('.env')

def require(key):
    if not env.get(key):
        fail('%s is not set in .env (see .env.example)' % key)
    return env[key]

# Pods reach ports published on the host at host.docker.internal.
database_url = require('DISPATCH_DATABASE_URL')
for local_host in ['@localhost:', '@127.0.0.1:']:
    database_url = database_url.replace(local_host, '@host.docker.internal:')

version = str(local('git describe --tags --always --dirty 2>/dev/null || echo dev', quiet=True, echo_off=True)).strip()
go_build = 'CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=%s"' % version

# --- Namespaces ----------------------------------------------------------------

def namespace(name):
    return {'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': name}}

k8s_yaml(encode_yaml_stream([namespace(n) for n in [OPERATOR_NAMESPACE, NAMESPACE, JOB_NAMESPACE]]))
k8s_resource(
    new_name='namespaces',
    objects=['%s:namespace' % n for n in [OPERATOR_NAMESPACE, NAMESPACE, JOB_NAMESPACE]],
    labels=['cluster'],
)

# --- Builds --------------------------------------------------------------------

local_resource(
    'web-build',
    cmd='cd web && npm run build',
    deps=['web/src', 'web/static', 'web/package.json', 'web/package-lock.json', 'web/vite.config.ts', 'web/tsconfig.json'],
    labels=['build'],
)

# The UI is embedded in the server binary, so a UI build is followed by a Go
# build (web/build is a dependency).
local_resource(
    'server-compile',
    cmd='%s -o .tilt-build/server/dispatch-ops ./cmd/dispatch-ops' % go_build,
    deps=['cmd/dispatch-ops', 'internal', 'web/build', 'web/web.go', 'go.mod', 'go.sum'],
    ignore=['**/*_test.go'],
    resource_deps=['web-build'],
    labels=['build'],
)

# The operator's own packages (`go list -deps ./cmd/dispatch-operator`).
local_resource(
    'operator-compile',
    cmd='%s -o .tilt-build/operator/dispatch-operator ./cmd/dispatch-operator' % go_build,
    deps=['cmd/dispatch-operator', 'internal/operator', 'go.mod', 'go.sum'],
    ignore=['**/*_test.go'],
    labels=['build'],
)

docker_build('dispatch-ops', '.tilt-build/server', dockerfile='deploy/dev/tilt/Dockerfile.server')
docker_build('dispatch-ops-operator', '.tilt-build/operator', dockerfile='deploy/dev/tilt/Dockerfile.operator')

# Execution environments keep fixed :dev tags (EEs are registered by image
# name in the app), so they are built and loaded outside Tilt's image
# tracking. The next job pod uses the reloaded image.
local_resource(
    'ees',
    cmd='make kind-load',
    deps=['cmd/dispatch-runner', 'internal/runner', 'images'],
    ignore=['**/*_test.go'],
    labels=['build'],
)

# --- Operator ------------------------------------------------------------------

k8s_yaml(helm(CHART, name='dispatch-ops-operator', namespace=OPERATOR_NAMESPACE, set=['logLevel=debug']))
k8s_resource(
    'dispatch-ops-operator',
    objects=[
        'dispatchops.dispatch-ops.io:customresourcedefinition',
        'dispatch-ops-operator:serviceaccount',
        'dispatch-ops-operator:clusterrole',
        'dispatch-ops-operator:clusterrolebinding',
    ],
    resource_deps=['namespaces', 'operator-compile'],
    labels=['cluster'],
)

# --- Dispatch Ops ----------------------------------------------------------------

def secret(name, data):
    return {
        'apiVersion': 'v1', 'kind': 'Secret', 'type': 'Opaque',
        'metadata': {'name': name, 'namespace': NAMESPACE},
        'stringData': data,
    }

secrets = [secret('dispatch-ops-dev-db', {'url': database_url})]

encryption = {'key': require('DISPATCH_ENCRYPTION_KEY')}
if env.get('DISPATCH_OLD_ENCRYPTION_KEYS'):
    encryption['oldKeys'] = env['DISPATCH_OLD_ENCRYPTION_KEYS']
secrets.append(secret('dispatch-ops-dev-encryption-key', encryption))

install = decode_yaml(read_file('deploy/dev/dispatchops.yaml'))
spec = install['spec']
# Tilt replaces this with the image it built.
spec['image'] = 'dispatch-ops'
if env.get('DISPATCH_EXTERNAL_URL'):
    spec['externalURL'] = env['DISPATCH_EXTERNAL_URL']
if env.get('DISPATCH_SESSION_TTL'):
    spec['sessionTTL'] = env['DISPATCH_SESSION_TTL']
if env.get('DISPATCH_OIDC_ISSUER'):
    oidc = {'issuer': env['DISPATCH_OIDC_ISSUER'], 'clientID': require('DISPATCH_OIDC_CLIENT_ID')}
    if env.get('DISPATCH_OIDC_CLIENT_SECRET'):
        secrets.append(secret('dispatch-ops-dev-oidc', {'clientSecret': env['DISPATCH_OIDC_CLIENT_SECRET']}))
        oidc['clientSecret'] = {'name': 'dispatch-ops-dev-oidc', 'key': 'clientSecret'}
    if env.get('DISPATCH_OIDC_SCOPES'):
        oidc['scopes'] = [s.strip() for s in env['DISPATCH_OIDC_SCOPES'].split(',') if s.strip()]
    for key, field in [('DISPATCH_OIDC_USERNAME_CLAIM', 'usernameClaim'), ('DISPATCH_OIDC_GROUPS_CLAIM', 'groupsClaim'), ('DISPATCH_OIDC_DISPLAY_NAME', 'displayName')]:
        if env.get(key):
            oidc[field] = env[key]
    spec['oidc'] = oidc

k8s_yaml(encode_yaml_stream(secrets + [install]))
k8s_kind('DispatchOps', image_json_path='{.spec.image}', pod_readiness='wait')
k8s_resource(
    'dispatch-ops',
    objects=['%s:secret' % s['metadata']['name'] for s in secrets],
    resource_deps=['dispatch-ops-operator', 'server-compile'],
    labels=['cluster'],
)

# kubectl port-forward ends when its pod goes away, as on every rollout, so
# restart it.
local_resource(
    'api-port-forward',
    serve_cmd='while true; do kubectl --context %s -n %s port-forward svc/dispatch-ops-api 8080:8080; sleep 2; done' % (CONTEXT, NAMESPACE),
    resource_deps=['dispatch-ops'],
    links=[link('http://localhost:8080', 'Dispatch Ops')],
    labels=['cluster'],
)
