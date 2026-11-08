-- Core domain schema. API endpoints for these arrive in milestone 2+.

CREATE TABLE inventories (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name         text NOT NULL UNIQUE CHECK (name <> ''),
    description  text NOT NULL DEFAULT '',
    variables    jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(variables) = 'object'),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER inventories_set_updated_at BEFORE UPDATE ON inventories
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE inventory_groups (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    inventory_id  uuid NOT NULL REFERENCES inventories (id) ON DELETE CASCADE,
    name          text NOT NULL CHECK (name <> ''),
    description   text NOT NULL DEFAULT '',
    variables     jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(variables) = 'object'),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (inventory_id, name),
    UNIQUE (inventory_id, id)
);
CREATE TRIGGER inventory_groups_set_updated_at BEFORE UPDATE ON inventory_groups
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Nested groups (Ansible "children"). Both sides must be in the same inventory.
CREATE TABLE inventory_group_children (
    inventory_id  uuid NOT NULL,
    parent_id     uuid NOT NULL,
    child_id      uuid NOT NULL,
    PRIMARY KEY (parent_id, child_id),
    CHECK (parent_id <> child_id),
    FOREIGN KEY (inventory_id, parent_id) REFERENCES inventory_groups (inventory_id, id) ON DELETE CASCADE,
    FOREIGN KEY (inventory_id, child_id) REFERENCES inventory_groups (inventory_id, id) ON DELETE CASCADE
);

CREATE TABLE hosts (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    inventory_id  uuid NOT NULL REFERENCES inventories (id) ON DELETE CASCADE,
    name          text NOT NULL CHECK (name <> ''),
    description   text NOT NULL DEFAULT '',
    variables     jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(variables) = 'object'),
    enabled       boolean NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (inventory_id, name),
    UNIQUE (inventory_id, id)
);
CREATE TRIGGER hosts_set_updated_at BEFORE UPDATE ON hosts
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Host and group must be in the same inventory.
CREATE TABLE inventory_group_hosts (
    inventory_id  uuid NOT NULL,
    group_id      uuid NOT NULL,
    host_id       uuid NOT NULL,
    PRIMARY KEY (group_id, host_id),
    FOREIGN KEY (inventory_id, group_id) REFERENCES inventory_groups (inventory_id, id) ON DELETE CASCADE,
    FOREIGN KEY (inventory_id, host_id) REFERENCES hosts (inventory_id, id) ON DELETE CASCADE
);
CREATE INDEX inventory_group_hosts_host_idx ON inventory_group_hosts (host_id);

-- Secrets are encrypted by the application. public_inputs holds the
-- non-secret fields (e.g. username) so they can be shown in the UI.
CREATE TABLE credentials (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name            text NOT NULL UNIQUE CHECK (name <> ''),
    description     text NOT NULL DEFAULT '',
    kind            text NOT NULL CHECK (kind IN ('ssh', 'username_password', 'api_token', 'registry', 'scm', 'generic')),
    public_inputs   jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(public_inputs) = 'object'),
    secret_inputs   bytea NOT NULL,
    -- Identifies the encryption key, to support key rotation.
    key_id          text NOT NULL,
    created_by      uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER credentials_set_updated_at BEFORE UPDATE ON credentials
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- A git repository holding playbooks and scripts.
CREATE TABLE projects (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name           text NOT NULL UNIQUE CHECK (name <> ''),
    description    text NOT NULL DEFAULT '',
    scm_url        text NOT NULL CHECK (scm_url <> ''),
    scm_ref        text NOT NULL DEFAULT 'main',
    credential_id  uuid REFERENCES credentials (id) ON DELETE SET NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER projects_set_updated_at BEFORE UPDATE ON projects
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Registered job images. Each is built with the dispatch-ops runner as its entrypoint.
CREATE TABLE execution_environments (
    id                      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name                    text NOT NULL UNIQUE CHECK (name <> ''),
    description             text NOT NULL DEFAULT '',
    image                   text NOT NULL CHECK (image <> ''),
    pull_policy             text NOT NULL DEFAULT 'IfNotPresent' CHECK (pull_policy IN ('Always', 'IfNotPresent', 'Never')),
    registry_credential_id  uuid REFERENCES credentials (id) ON DELETE SET NULL,
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER execution_environments_set_updated_at BEFORE UPDATE ON execution_environments
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE job_templates (
    id                        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name                      text NOT NULL UNIQUE CHECK (name <> ''),
    description               text NOT NULL DEFAULT '',
    job_type                  text NOT NULL CHECK (job_type IN ('ansible', 'script', 'container')),
    execution_environment_id  uuid NOT NULL REFERENCES execution_environments (id) ON DELETE RESTRICT,
    project_id                uuid REFERENCES projects (id) ON DELETE RESTRICT,
    inventory_id              uuid REFERENCES inventories (id) ON DELETE RESTRICT,
    -- Playbook or script path within the project (ansible, script).
    entrypoint                text NOT NULL DEFAULT '',
    -- Command and arguments (container).
    command                   text[] NOT NULL DEFAULT '{}',
    extra_vars                jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(extra_vars) = 'object'),
    limit_pattern             text NOT NULL DEFAULT '',
    timeout_seconds           integer NOT NULL DEFAULT 3600 CHECK (timeout_seconds > 0),
    -- Fields a launcher may override, e.g. {inventory, extra_vars, limit}.
    allow_overrides           text[] NOT NULL DEFAULT '{}',
    created_at                timestamptz NOT NULL DEFAULT now(),
    updated_at                timestamptz NOT NULL DEFAULT now(),
    CHECK (job_type = 'container' OR (project_id IS NOT NULL AND entrypoint <> '')),
    CHECK (job_type <> 'container' OR cardinality(command) > 0)
);
CREATE TRIGGER job_templates_set_updated_at BEFORE UPDATE ON job_templates
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE job_template_credentials (
    job_template_id  uuid NOT NULL REFERENCES job_templates (id) ON DELETE CASCADE,
    credential_id    uuid NOT NULL REFERENCES credentials (id) ON DELETE RESTRICT,
    PRIMARY KEY (job_template_id, credential_id)
);

CREATE TABLE jobs (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    job_template_id    uuid REFERENCES job_templates (id) ON DELETE SET NULL,
    name               text NOT NULL,
    status             text NOT NULL DEFAULT 'pending'
                       CHECK (status IN ('pending', 'waiting', 'running', 'successful', 'failed', 'error', 'canceled')),
    launch_type        text NOT NULL CHECK (launch_type IN ('manual', 'api', 'schedule', 'relaunch')),
    launched_by        uuid REFERENCES users (id) ON DELETE SET NULL,
    -- Snapshot of the fully resolved template + overrides at launch time.
    config             jsonb NOT NULL,
    cancel_requested   boolean NOT NULL DEFAULT false,
    exit_code          integer,
    failure_reason     text NOT NULL DEFAULT '',
    k8s_namespace      text,
    k8s_job_name       text,
    created_at         timestamptz NOT NULL DEFAULT now(),
    started_at         timestamptz,
    finished_at        timestamptz,
    last_heartbeat_at  timestamptz
);
-- The task manager's queue: oldest pending job first.
CREATE INDEX jobs_pending_idx ON jobs (created_at) WHERE status = 'pending';
CREATE INDEX jobs_active_idx ON jobs (status) WHERE status IN ('waiting', 'running');
CREATE INDEX jobs_template_idx ON jobs (job_template_id, created_at DESC);
CREATE INDEX jobs_created_idx ON jobs (created_at DESC);

-- Output and structured events from the runner. seq is assigned by the runner
-- so retried batches are idempotent.
CREATE TABLE job_events (
    job_id      uuid NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    seq         bigint NOT NULL CHECK (seq >= 0),
    created_at  timestamptz NOT NULL DEFAULT now(),
    event_type  text NOT NULL DEFAULT 'stdout',
    host_name   text,
    stdout      text NOT NULL DEFAULT '',
    data        jsonb,
    PRIMARY KEY (job_id, seq)
);
