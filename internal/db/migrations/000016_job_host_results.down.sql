DROP TABLE job_host_results;
ALTER TABLE jobs DROP COLUMN last_event_seq, DROP COLUMN host_results_seq, DROP COLUMN host_count;
CREATE INDEX job_events_stats_idx ON job_events (job_id) WHERE event_type = 'playbook_stats';

CREATE OR REPLACE FUNCTION ensure_job_partitions(from_day date, to_day date) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
    d date;
    part text;
BEGIN
    PERFORM set_config('lock_timeout', '5s', true);
    FOR d IN SELECT generate_series(from_day, to_day, interval '1 day')::date LOOP
        part := 'job_events_p' || to_char(d, 'YYYYMMDD');
        IF to_regclass(quote_ident(part)) IS NULL THEN
            EXECUTE format('CREATE TABLE IF NOT EXISTS %I PARTITION OF job_events FOR VALUES FROM (%L) TO (%L)',
                part, job_id_floor(d::timestamp AT TIME ZONE 'UTC'), job_id_floor((d + 1)::timestamp AT TIME ZONE 'UTC'));
        END IF;
    END LOOP;
END;
$$;
