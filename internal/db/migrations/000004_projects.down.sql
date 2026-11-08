DROP INDEX job_events_stats_idx;
DROP INDEX job_events_host_idx;

DELETE FROM credentials WHERE kind = 'vault';
ALTER TABLE credentials
    DROP CONSTRAINT credentials_kind_check,
    ADD CONSTRAINT credentials_kind_check
        CHECK (kind IN ('ssh', 'username_password', 'api_token', 'registry', 'scm', 'generic'));

ALTER TABLE jobs DROP COLUMN scm_revision;

UPDATE job_templates SET allow_overrides = array_remove(array_remove(allow_overrides, 'tags'), 'skipTags');
ALTER TABLE job_templates
    DROP CONSTRAINT job_templates_allow_overrides_check,
    ADD CONSTRAINT job_templates_allow_overrides_check
        CHECK (allow_overrides <@ ARRAY['inventory', 'extraVars', 'limit', 'command']::text[]),
    DROP CONSTRAINT job_templates_container_has_no_project,
    DROP COLUMN verbosity,
    DROP COLUMN skip_tags,
    DROP COLUMN job_tags,
    DROP COLUMN arguments;
