DROP TRIGGER jobs_queue_notifications ON jobs;
DROP FUNCTION queue_job_notifications();
DROP TABLE notification_deliveries;
DROP TABLE job_template_notifications;
DROP TABLE notification_targets;

ALTER TABLE schedules DROP CONSTRAINT schedules_last_job_fkey;
DROP INDEX jobs_finished_idx;
ALTER TABLE jobs DROP COLUMN relaunch_of, DROP COLUMN schedule_id;
DROP TABLE schedules;
