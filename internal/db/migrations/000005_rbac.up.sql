-- Teams, role-based access control, scoped API tokens, OIDC users and the
-- audit log.

CREATE TABLE teams (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name         text NOT NULL UNIQUE CHECK (name <> ''),
    description  text NOT NULL DEFAULT '',
    -- Values of the OIDC groups claim. A user signing in with any of them
    -- joins the team; one signing in without them leaves it again.
    oidc_groups  text[] NOT NULL DEFAULT '{}',
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER teams_set_updated_at BEFORE UPDATE ON teams
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE team_members (
    team_id     uuid NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    user_id     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- manual: added through the API. oidc: from the groups claim at sign-in;
    -- only these are removed when the claim no longer matches.
    source      text NOT NULL DEFAULT 'manual' CHECK (source IN ('manual', 'oidc')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (team_id, user_id)
);
CREATE INDEX team_members_user_idx ON team_members (user_id);

-- A role held by a user or a team. Without a resource type it is a global
-- role (system_admin, auditor). With a type but no resource ID it applies to
-- every resource of that type, and admin on a type allows creating them.
CREATE TABLE role_grants (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    role           text NOT NULL,
    resource_type  text CHECK (resource_type IN ('inventory', 'credential', 'project', 'job_template')),
    resource_id    uuid,
    user_id        uuid REFERENCES users (id) ON DELETE CASCADE,
    team_id        uuid REFERENCES teams (id) ON DELETE CASCADE,
    created_at     timestamptz NOT NULL DEFAULT now(),
    CHECK ((user_id IS NULL) <> (team_id IS NULL)),
    CHECK (resource_type IS NOT NULL OR resource_id IS NULL),
    CHECK (CASE
        WHEN resource_type IS NULL THEN role IN ('system_admin', 'auditor')
        WHEN resource_type = 'job_template' THEN role IN ('admin', 'execute', 'read')
        ELSE role IN ('admin', 'use', 'read')
    END),
    UNIQUE NULLS NOT DISTINCT (role, resource_type, resource_id, user_id, team_id)
);
CREATE INDEX role_grants_resource_idx ON role_grants (resource_type, resource_id);
CREATE INDEX role_grants_user_idx ON role_grants (user_id) WHERE user_id IS NOT NULL;
CREATE INDEX role_grants_team_idx ON role_grants (team_id) WHERE team_id IS NOT NULL;

-- Every role each user holds, directly or through a team.
CREATE VIEW user_roles AS
    SELECT g.user_id, g.role, g.resource_type, g.resource_id
    FROM role_grants g WHERE g.user_id IS NOT NULL
    UNION ALL
    SELECT m.user_id, g.role, g.resource_type, g.resource_id
    FROM role_grants g JOIN team_members m ON m.team_id = g.team_id;

-- Grants on a resource go with it. TG_ARGV[0] is the resource type.
CREATE FUNCTION delete_role_grants() RETURNS trigger AS $$
BEGIN
    DELETE FROM role_grants WHERE resource_type = TG_ARGV[0] AND resource_id = OLD.id;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER inventories_delete_role_grants AFTER DELETE ON inventories
    FOR EACH ROW EXECUTE FUNCTION delete_role_grants('inventory');
CREATE TRIGGER credentials_delete_role_grants AFTER DELETE ON credentials
    FOR EACH ROW EXECUTE FUNCTION delete_role_grants('credential');
CREATE TRIGGER projects_delete_role_grants AFTER DELETE ON projects
    FOR EACH ROW EXECUTE FUNCTION delete_role_grants('project');
CREATE TRIGGER job_templates_delete_role_grants AFTER DELETE ON job_templates
    FOR EACH ROW EXECUTE FUNCTION delete_role_grants('job_template');

-- System administrators become role grants.
INSERT INTO role_grants (role, user_id) SELECT 'system_admin', id FROM users WHERE is_admin;
ALTER TABLE users DROP COLUMN is_admin;

-- OIDC subjects are only unique per issuer.
ALTER TABLE users ADD COLUMN oidc_issuer text;
DROP INDEX users_oidc_subject_key;
CREATE UNIQUE INDEX users_oidc_subject_key ON users (oidc_issuer, oidc_subject) WHERE oidc_subject IS NOT NULL;
ALTER TABLE users ADD CONSTRAINT users_oidc_issuer_check CHECK (auth_source <> 'oidc' OR oidc_issuer IS NOT NULL);

-- Read-only API tokens may only make GET requests.
ALTER TABLE auth_tokens
    ADD COLUMN scope text NOT NULL DEFAULT 'write' CHECK (scope IN ('read', 'write'));

CREATE TABLE audit_log (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    created_at     timestamptz NOT NULL DEFAULT now(),
    actor_id       uuid REFERENCES users (id) ON DELETE SET NULL,
    -- The actor's username at the time, kept if the user is deleted.
    actor_name     text NOT NULL DEFAULT '',
    auth_method    text NOT NULL DEFAULT '',
    action         text NOT NULL,
    resource_type  text NOT NULL,
    resource_id    uuid,
    resource_name  text NOT NULL DEFAULT '',
    -- The resource's fields before and after the change. For updates, only
    -- the fields that changed.
    before         jsonb,
    after          jsonb
);
CREATE INDEX audit_log_created_idx ON audit_log (created_at DESC);
CREATE INDEX audit_log_resource_idx ON audit_log (resource_type, resource_id, created_at DESC);
CREATE INDEX audit_log_actor_idx ON audit_log (actor_id, created_at DESC);
