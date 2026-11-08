-- Git projects, and the ansible and script job types that run from them.

-- Arguments passed to a script (script jobs), and ansible-playbook options.
ALTER TABLE job_templates
    ADD COLUMN arguments   text[] NOT NULL DEFAULT '{}',
    ADD COLUMN job_tags    text NOT NULL DEFAULT '',
    ADD COLUMN skip_tags   text NOT NULL DEFAULT '',
    ADD COLUMN verbosity   integer NOT NULL DEFAULT 0 CHECK (verbosity BETWEEN 0 AND 5),
    -- Only container jobs run without a project.
    ADD CONSTRAINT job_templates_container_has_no_project CHECK (job_type <> 'container' OR project_id IS NULL),
    DROP CONSTRAINT job_templates_allow_overrides_check,
    ADD CONSTRAINT job_templates_allow_overrides_check
        CHECK (allow_overrides <@ ARRAY['inventory', 'extraVars', 'limit', 'command', 'tags', 'skipTags']::text[]);

-- The commit a job's project was checked out at.
ALTER TABLE jobs
    ADD COLUMN scm_revision text NOT NULL DEFAULT '';

-- Ansible Vault passwords.
ALTER TABLE credentials
    DROP CONSTRAINT credentials_kind_check,
    ADD CONSTRAINT credentials_kind_check
        CHECK (kind IN ('ssh', 'username_password', 'api_token', 'registry', 'scm', 'vault', 'generic'));

-- Per-host results are aggregated from these.
CREATE INDEX job_events_host_idx ON job_events (job_id, host_name) WHERE host_name IS NOT NULL;
CREATE INDEX job_events_stats_idx ON job_events (job_id) WHERE event_type = 'playbook_stats';
