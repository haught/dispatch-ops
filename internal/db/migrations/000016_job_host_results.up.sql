-- Each host's task results in an Ansible job, kept as the job's events
-- arrive (and replaced by Ansible's recap at the end), so they can be read
-- without going through the job's output, and stay complete when the
-- output is cut at the cap. Partitioned like job_events.
CREATE TABLE job_host_results (
    job_id       uuid NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    host_name    text NOT NULL,
    -- Tasks that succeeded, including changed ones and ignored failures.
    ok           integer NOT NULL DEFAULT 0,
    changed      integer NOT NULL DEFAULT 0,
    failed       integer NOT NULL DEFAULT 0,
    unreachable  integer NOT NULL DEFAULT 0,
    skipped      integer NOT NULL DEFAULT 0,
    rescued      integer NOT NULL DEFAULT 0,
    -- Failures with ignore_errors.
    ignored      integer NOT NULL DEFAULT 0,
    -- The host's overall result, the worst of its task results.
    status       text GENERATED ALWAYS AS (CASE
                     WHEN unreachable > 0 THEN 'unreachable'
                     WHEN failed > 0 THEN 'failed'
                     WHEN changed > 0 THEN 'changed'
                     WHEN ok > 0 THEN 'ok'
                     ELSE 'skipped' END) VIRTUAL,
    PRIMARY KEY (job_id, host_name)
) PARTITION BY RANGE (job_id);

ALTER TABLE jobs
    -- The highest seq stored (or dropped at the output cap). The runner
    -- sends events in order, so any at or below it are a retry.
    ADD COLUMN last_event_seq bigint NOT NULL DEFAULT 0,
    -- The seq of the last event that changed job_host_results, for live
    -- streams to tell browsers when to refresh host results.
    ADD COLUMN host_results_seq bigint NOT NULL DEFAULT 0,
    -- Rows in job_host_results (see maxJobHosts in the store).
    ADD COLUMN host_count integer NOT NULL DEFAULT 0;

-- The recap is no longer stored as an event.
DELETE FROM job_events WHERE event_type = 'playbook_stats';
DROP INDEX job_events_stats_idx;

CREATE OR REPLACE FUNCTION ensure_job_partitions(from_day date, to_day date) RETURNS void
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
        FOREACH parent IN ARRAY ARRAY['job_events', 'job_host_results'] LOOP
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

-- The same days as job_events.
SELECT ensure_job_partitions(
    coalesce(min(job_partition_day(c.relname)), (now() AT TIME ZONE 'UTC')::date - 1),
    (now() AT TIME ZONE 'UTC')::date + 7)
FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
WHERE i.inhparent = 'job_events'::regclass;
