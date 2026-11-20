DELETE FROM jobs;
DROP TABLE job_events;
DROP FUNCTION ensure_job_partitions(date, date);
DROP FUNCTION job_partition_day(text);
DROP FUNCTION job_id_floor(timestamptz);
ALTER TABLE jobs ALTER COLUMN id SET DEFAULT gen_random_uuid();

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
CREATE INDEX job_events_host_idx ON job_events (job_id, host_name) WHERE host_name IS NOT NULL;
CREATE INDEX job_events_stats_idx ON job_events (job_id) WHERE event_type = 'playbook_stats';
