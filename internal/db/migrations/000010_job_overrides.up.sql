-- The launch overrides a job was started with, as in a launch request, so a
-- relaunch can launch the job's template again with the same overrides
-- (and the same checks) instead of copying the resolved config. NULL for
-- jobs from before this column, and for jobs no template launched.
ALTER TABLE jobs ADD COLUMN overrides jsonb CHECK (overrides IS NULL OR jsonb_typeof(overrides) = 'object');
