-- Schedules, relaunches and notifications.

-- Launches a job template on a cron schedule.
CREATE TABLE schedules (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name             text NOT NULL CHECK (name <> ''),
    description      text NOT NULL DEFAULT '',
    job_template_id  uuid NOT NULL REFERENCES job_templates (id) ON DELETE CASCADE,
    -- Five-field cron expression or descriptor (@daily, @every 2h, ...).
    cron             text NOT NULL CHECK (cron <> ''),
    -- IANA time zone the cron expression is read in.
    timezone         text NOT NULL DEFAULT 'UTC',
    enabled          boolean NOT NULL DEFAULT true,
    -- Launch overrides, as in a launch request.
    overrides        jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(overrides) = 'object'),
    -- NULL while disabled.
    next_run_at      timestamptz,
    last_run_at      timestamptz,
    last_job_id      uuid,
    -- Why the last run could not launch a job, if it couldn't.
    last_error       text NOT NULL DEFAULT '',
    created_by       uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (job_template_id, name),
    CHECK (enabled = (next_run_at IS NOT NULL))
);
CREATE TRIGGER schedules_set_updated_at BEFORE UPDATE ON schedules
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE INDEX schedules_due_idx ON schedules (next_run_at) WHERE next_run_at IS NOT NULL;

ALTER TABLE jobs
    ADD COLUMN schedule_id uuid REFERENCES schedules (id) ON DELETE SET NULL,
    ADD COLUMN relaunch_of uuid REFERENCES jobs (id) ON DELETE SET NULL;
CREATE INDEX jobs_schedule_idx ON jobs (schedule_id) WHERE schedule_id IS NOT NULL;
CREATE INDEX jobs_relaunch_of_idx ON jobs (relaunch_of) WHERE relaunch_of IS NOT NULL;
-- Retention deletes finished jobs by age.
CREATE INDEX jobs_finished_idx ON jobs (finished_at) WHERE finished_at IS NOT NULL;

ALTER TABLE schedules
    ADD CONSTRAINT schedules_last_job_fkey FOREIGN KEY (last_job_id) REFERENCES jobs (id) ON DELETE SET NULL;

-- Where job notifications are sent. Secret values (tokens, passwords) are
-- encrypted by the application, like credentials.
CREATE TABLE notification_targets (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name           text NOT NULL UNIQUE CHECK (name <> ''),
    description    text NOT NULL DEFAULT '',
    kind           text NOT NULL CHECK (kind IN ('webhook', 'slack', 'email')),
    config         jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(config) = 'object'),
    secret_values  bytea NOT NULL,
    secret_fields  text[] NOT NULL DEFAULT '{}',
    key_id         text NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER notification_targets_set_updated_at BEFORE UPDATE ON notification_targets
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Which targets a template's jobs notify, and on which events.
CREATE TABLE job_template_notifications (
    job_template_id  uuid NOT NULL REFERENCES job_templates (id) ON DELETE CASCADE,
    target_id        uuid NOT NULL REFERENCES notification_targets (id) ON DELETE CASCADE,
    events           text[] NOT NULL
                     CHECK (cardinality(events) > 0
                        AND events <@ ARRAY['started', 'succeeded', 'failed', 'canceled']::text[]),
    PRIMARY KEY (job_template_id, target_id)
);
CREATE INDEX job_template_notifications_target_idx ON job_template_notifications (target_id);

-- The outbox of notifications to send. Rows are added by the trigger below
-- in the same transaction as the job's status change, so no event is lost
-- whichever process changed it; the task manager sends them.
CREATE TABLE notification_deliveries (
    id               bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    job_id           uuid NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    target_id        uuid NOT NULL REFERENCES notification_targets (id) ON DELETE CASCADE,
    event            text NOT NULL CHECK (event IN ('started', 'succeeded', 'failed', 'canceled')),
    status           text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'sent', 'failed')),
    attempts         integer NOT NULL DEFAULT 0,
    next_attempt_at  timestamptz NOT NULL DEFAULT now(),
    last_error       text NOT NULL DEFAULT '',
    created_at       timestamptz NOT NULL DEFAULT now(),
    sent_at          timestamptz
);
CREATE INDEX notification_deliveries_due_idx ON notification_deliveries (next_attempt_at) WHERE status = 'pending';
CREATE INDEX notification_deliveries_job_idx ON notification_deliveries (job_id);
CREATE INDEX notification_deliveries_target_idx ON notification_deliveries (target_id);

CREATE FUNCTION queue_job_notifications() RETURNS trigger AS $$
DECLARE
    ev text;
BEGIN
    ev := CASE
        WHEN NEW.status = 'running' THEN 'started'
        WHEN NEW.status = 'successful' THEN 'succeeded'
        WHEN NEW.status IN ('failed', 'error') THEN 'failed'
        WHEN NEW.status = 'canceled' THEN 'canceled'
    END;
    IF ev IS NOT NULL AND NEW.job_template_id IS NOT NULL THEN
        INSERT INTO notification_deliveries (job_id, target_id, event)
        SELECT NEW.id, n.target_id, ev
        FROM job_template_notifications n
        WHERE n.job_template_id = NEW.job_template_id AND ev = ANY (n.events);
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER jobs_queue_notifications AFTER UPDATE OF status ON jobs
    FOR EACH ROW
    WHEN (OLD.status IS DISTINCT FROM NEW.status)
    EXECUTE FUNCTION queue_job_notifications();
