-- Job output, partitioned by day so that retention drops whole days of it
-- instead of deleting rows one by one.
--
-- Job IDs become UUIDv7, whose first 48 bits are the creation time in
-- milliseconds, and job output is range-partitioned on job_id: one partition
-- per UTC day of job creation. A job's output is in a single partition, and
-- any query by job_id reads only that partition.
--
-- Existing jobs have random (v4) IDs, which belong to no day; they are
-- deleted, with their output.

DELETE FROM jobs;

ALTER TABLE jobs ALTER COLUMN id SET DEFAULT uuidv7();

-- The lowest UUIDv7 that could be generated at ts.
CREATE FUNCTION job_id_floor(ts timestamptz) RETURNS uuid
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
    SELECT (lpad(to_hex(floor(extract(epoch FROM ts) * 1000)::bigint), 12, '0') || repeat('0', 20))::uuid
$$;

-- The UTC day a partition named <table>_pYYYYMMDD holds jobs created on.
CREATE FUNCTION job_partition_day(name text) RETURNS date
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
    SELECT to_date(substring(name FROM '_p(\d{8})$'), 'YYYYMMDD')
$$;

-- Creates the partitions for jobs created from from_day to to_day (UTC),
-- if missing. The task manager calls it regularly to stay a week ahead.
CREATE FUNCTION ensure_job_partitions(from_day date, to_day date) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
    d date;
    parent text;
    part text;
BEGIN
    -- Creating a partition briefly locks its parent; don't queue behind a
    -- long query (and make everything else queue behind us). It is retried.
    PERFORM set_config('lock_timeout', '5s', true);
    FOR d IN SELECT generate_series(from_day, to_day, interval '1 day')::date LOOP
        FOREACH parent IN ARRAY ARRAY['job_events'] LOOP
            part := parent || '_p' || to_char(d, 'YYYYMMDD');
            IF to_regclass(quote_ident(part)) IS NULL THEN
                EXECUTE format('CREATE TABLE IF NOT EXISTS %I PARTITION OF %I FOR VALUES FROM (%L) TO (%L)',
                    part, parent,
                    job_id_floor(d::timestamp AT TIME ZONE 'UTC'),
                    job_id_floor((d + 1)::timestamp AT TIME ZONE 'UTC'));
            END IF;
        END LOOP;
    END LOOP;
END;
$$;

-- Output and structured events from the runner. seq is assigned by the
-- runner, in order.
DROP TABLE job_events;
CREATE TABLE job_events (
    job_id      uuid NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    seq         bigint NOT NULL CHECK (seq >= 0),
    created_at  timestamptz NOT NULL DEFAULT now(),
    event_type  text NOT NULL DEFAULT 'stdout',
    host_name   text,
    stdout      text NOT NULL DEFAULT '',
    data        jsonb,
    PRIMARY KEY (job_id, seq)
) PARTITION BY RANGE (job_id);
-- A host's events, for its task results.
CREATE INDEX job_events_host_idx ON job_events (job_id, host_name) WHERE host_name IS NOT NULL;
CREATE INDEX job_events_stats_idx ON job_events (job_id) WHERE event_type = 'playbook_stats';

SELECT ensure_job_partitions((now() AT TIME ZONE 'UTC')::date - 1, (now() AT TIME ZONE 'UTC')::date + 7);
