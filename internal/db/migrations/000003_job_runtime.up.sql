-- Columns and notifications used to run jobs.

ALTER TABLE jobs
    -- SHA-256 of the one-time token the job's runner authenticates with.
    -- Cleared when the job finishes, which revokes the token.
    ADD COLUMN runner_token_hash bytea UNIQUE,
    ADD COLUMN runner_version text NOT NULL DEFAULT '',
    -- When the task manager created the Kubernetes Job.
    ADD COLUMN dispatched_at timestamptz,
    ADD COLUMN cancel_requested_at timestamptz,
    ADD CONSTRAINT jobs_config_is_object CHECK (jsonb_typeof(config) = 'object');

-- Names of the secret fields that have a value, so they can be listed
-- without decrypting anything.
ALTER TABLE credentials
    ADD COLUMN secret_fields text[] NOT NULL DEFAULT '{}';

ALTER TABLE job_templates
    ADD CONSTRAINT job_templates_allow_overrides_check
    CHECK (allow_overrides <@ ARRAY['inventory', 'extraVars', 'limit', 'command']::text[]);

-- Tells the task manager about new and canceled jobs, and API servers about
-- status changes to push to browsers. The payload is the job ID.
CREATE FUNCTION notify_job_change() RETURNS trigger AS $$
BEGIN
    PERFORM pg_notify('dispatch_jobs', NEW.id::text);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER jobs_notify_insert AFTER INSERT ON jobs
    FOR EACH ROW EXECUTE FUNCTION notify_job_change();
CREATE TRIGGER jobs_notify_update AFTER UPDATE OF status, cancel_requested ON jobs
    FOR EACH ROW
    WHEN (OLD.status IS DISTINCT FROM NEW.status OR OLD.cancel_requested IS DISTINCT FROM NEW.cancel_requested)
    EXECUTE FUNCTION notify_job_change();
