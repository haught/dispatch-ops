-- How much output a job has stored, so one job can't fill the database
-- (see DISPATCH_JOB_MAX_OUTPUT). Bytes of each stored event's stdout, host
-- name and data. Once the cap is reached the job keeps running, but only
-- its recap is stored, and output_truncated is set.
ALTER TABLE jobs
    ADD COLUMN output_bytes bigint NOT NULL DEFAULT 0,
    ADD COLUMN output_truncated boolean NOT NULL DEFAULT false;
