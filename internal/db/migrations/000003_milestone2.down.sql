DROP TRIGGER jobs_notify_update ON jobs;
DROP TRIGGER jobs_notify_insert ON jobs;
DROP FUNCTION notify_job_change();

ALTER TABLE credentials DROP COLUMN secret_fields;

ALTER TABLE job_templates DROP CONSTRAINT job_templates_allow_overrides_check;

ALTER TABLE jobs
    DROP CONSTRAINT jobs_config_is_object,
    DROP COLUMN cancel_requested_at,
    DROP COLUMN dispatched_at,
    DROP COLUMN runner_version,
    DROP COLUMN runner_token_hash;
