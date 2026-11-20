-- A host result's task: the seq of the task's task_start event, which holds
-- the task's name, action and play (once, not in every host's result).
ALTER TABLE job_events ADD COLUMN task_seq bigint;
