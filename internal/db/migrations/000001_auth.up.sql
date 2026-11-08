-- Keeps updated_at current on any table that has the column.
CREATE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TABLE users (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    username       text NOT NULL CHECK (username <> ''),
    display_name   text NOT NULL DEFAULT '',
    email          text NOT NULL DEFAULT '',
    auth_source    text NOT NULL DEFAULT 'local' CHECK (auth_source IN ('local', 'oidc')),
    -- argon2id PHC string; NULL for OIDC users, who never sign in with a password.
    password_hash  text,
    oidc_subject   text,
    is_admin       boolean NOT NULL DEFAULT false,
    is_active      boolean NOT NULL DEFAULT true,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    last_login_at  timestamptz,
    CHECK (auth_source <> 'local' OR password_hash IS NOT NULL),
    CHECK (auth_source <> 'oidc' OR oidc_subject IS NOT NULL)
);
CREATE UNIQUE INDEX users_username_key ON users (lower(username));
CREATE UNIQUE INDEX users_oidc_subject_key ON users (oidc_subject) WHERE oidc_subject IS NOT NULL;
CREATE TRIGGER users_set_updated_at BEFORE UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Browser sessions and API tokens. Only a SHA-256 hash of the secret is stored.
CREATE TABLE auth_tokens (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind          text NOT NULL CHECK (kind IN ('session', 'api')),
    name          text NOT NULL DEFAULT '',
    token_hash    bytea NOT NULL UNIQUE,
    token_prefix  text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz,
    last_used_at  timestamptz,
    CHECK (kind <> 'session' OR expires_at IS NOT NULL)
);
CREATE INDEX auth_tokens_user_kind_idx ON auth_tokens (user_id, kind);
CREATE INDEX auth_tokens_expires_idx ON auth_tokens (expires_at) WHERE expires_at IS NOT NULL;
